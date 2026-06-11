package mirror

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pascalinthecloud/terrastrata/internal/cache"
)

// fakeRegistry is an httptest server implementing the upstream provider registry
// protocol for a single provider (hashicorp/null @ 3.2.0, linux_amd64). It counts
// hits so tests can assert the cache prevents repeat upstream calls.
type fakeRegistry struct {
	server   *httptest.Server
	zipBytes []byte
	zipSum   string
	// servedShasum is what the download endpoint reports; defaults to the true
	// sum but tests can override it to exercise mismatch / empty-checksum paths.
	servedShasum string
	hits         atomic.Int64
	// zipHits counts only the archive-download endpoint, so coalescing tests can
	// assert a burst of concurrent requests produced exactly one upstream fetch.
	zipHits atomic.Int64
	// zipDelay holds the /zip handler briefly to widen the window in which
	// concurrent requests overlap, making coalescing observable.
	zipDelay time.Duration
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	t.Helper()
	zip := []byte("PK\x03\x04 this is a fake provider zip payload")
	sum := sha256.Sum256(zip)
	fr := &fakeRegistry{zipBytes: zip, zipSum: hex.EncodeToString(sum[:])}
	fr.servedShasum = fr.zipSum

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/providers/hashicorp/null/versions", func(w http.ResponseWriter, _ *http.Request) {
		fr.hits.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"versions": []map[string]any{
				{"version": "3.2.0", "platforms": []map[string]string{{"os": "linux", "arch": "amd64"}}},
				{"version": "3.1.0", "platforms": []map[string]string{{"os": "linux", "arch": "amd64"}}},
			},
		})
	})
	mux.HandleFunc("GET /v1/providers/hashicorp/null/3.2.0/download/linux/amd64", func(w http.ResponseWriter, _ *http.Request) {
		fr.hits.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"os":           "linux",
			"arch":         "amd64",
			"filename":     "terraform-provider-null_3.2.0_linux_amd64.zip",
			"download_url": fr.server.URL + "/zip",
			"shasum":       fr.servedShasum,
		})
	})
	mux.HandleFunc("GET /zip", func(w http.ResponseWriter, _ *http.Request) {
		fr.hits.Add(1)
		fr.zipHits.Add(1)
		if fr.zipDelay > 0 {
			time.Sleep(fr.zipDelay)
		}
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(fr.zipBytes)
	})

	fr.server = httptest.NewServer(mux)
	t.Cleanup(fr.server.Close)
	return fr
}

func newTestHandler(t *testing.T, base string) *Handler {
	t.Helper()
	return newTestHandlerTTL(t, base, 0)
}

func newTestHandlerTTL(t *testing.T, base string, ttl time.Duration) *Handler {
	t.Helper()
	c, err := cache.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	u := NewUpstream(base, "terrastrata-test", 5*time.Second)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h, err := NewHandler(Options{
		Cache:      c,
		Upstream:   u,
		Metrics:    NopMetrics{},
		StagingDir: t.TempDir(),
		IndexTTL:   ttl,
		Logger:     log,
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return h
}

func doGet(t *testing.T, srv *httptest.Server, path string) *http.Response {
	t.Helper()
	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return resp
}

func TestEndToEndCachingFlow(t *testing.T) {
	reg := newFakeRegistry(t)
	h := newTestHandler(t, reg.server.URL)

	mux := http.NewServeMux()
	h.Routes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// 1. Versions index — MISS then HIT.
	const versionsPath = "/registry.terraform.io/hashicorp/null/index.json"
	resp := doGet(t, ts, versionsPath)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("versions status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Cache"); got != "MISS" {
		t.Errorf("first versions X-Cache = %q, want MISS", got)
	}
	var vidx VersionsIndex
	decode(t, resp, &vidx)
	if _, ok := vidx.Versions["3.2.0"]; !ok {
		t.Errorf("versions index missing 3.2.0: %+v", vidx.Versions)
	}

	hitsAfterVersions := reg.hits.Load()
	resp = doGet(t, ts, versionsPath)
	if got := resp.Header.Get("X-Cache"); got != "HIT" {
		t.Errorf("second versions X-Cache = %q, want HIT", got)
	}
	resp.Body.Close()
	if reg.hits.Load() != hitsAfterVersions {
		t.Error("cached versions request still hit upstream")
	}

	// 2. Archives index — MISS, verify URL rewrite + hash.
	const archivesPath = "/registry.terraform.io/hashicorp/null/3.2.0.json"
	resp = doGet(t, ts, archivesPath)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("archives status = %d", resp.StatusCode)
	}
	var aidx ArchivesIndex
	decode(t, resp, &aidx)
	arch, ok := aidx.Archives["linux_amd64"]
	if !ok {
		t.Fatalf("archives missing linux_amd64: %+v", aidx.Archives)
	}
	wantURL := "3.2.0/download/linux_amd64/terraform-provider-null_3.2.0_linux_amd64.zip"
	if arch.URL != wantURL {
		t.Errorf("archive URL = %q, want %q", arch.URL, wantURL)
	}
	if len(arch.Hashes) != 1 || arch.Hashes[0] != "zh:"+reg.zipSum {
		t.Errorf("archive hashes = %v, want [zh:%s]", arch.Hashes, reg.zipSum)
	}

	// 3. Zip — MISS then HIT, bytes intact, checksum verified by handler.
	zipPath := "/registry.terraform.io/hashicorp/null/" + wantURL
	resp = doGet(t, ts, zipPath)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("zip status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/zip" {
		t.Errorf("zip Content-Type = %q", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != string(reg.zipBytes) {
		t.Errorf("zip bytes mismatch")
	}

	hitsAfterZip := reg.hits.Load()
	resp = doGet(t, ts, zipPath)
	if got := resp.Header.Get("X-Cache"); got != "HIT" {
		t.Errorf("second zip X-Cache = %q, want HIT", got)
	}
	resp.Body.Close()
	if reg.hits.Load() != hitsAfterZip {
		t.Error("cached zip request still hit upstream")
	}
}

func TestConcurrentColdZipRequestsCoalesce(t *testing.T) {
	reg := newFakeRegistry(t)
	reg.zipDelay = 50 * time.Millisecond // widen the overlap window
	h := newTestHandler(t, reg.server.URL)

	mux := http.NewServeMux()
	h.Routes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	const zipPath = "/registry.terraform.io/hashicorp/null/3.2.0/download/linux_amd64/terraform-provider-null_3.2.0_linux_amd64.zip"

	// Fire a burst of identical cold requests, released together.
	const n = 25
	var wg sync.WaitGroup
	start := make(chan struct{})
	bodies := make([][]byte, n)
	statuses := make([]int, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			resp, err := http.Get(ts.URL + zipPath)
			if err != nil {
				t.Errorf("GET: %v", err)
				return
			}
			defer resp.Body.Close()
			statuses[i] = resp.StatusCode
			bodies[i], _ = io.ReadAll(resp.Body)
		}()
	}
	close(start)
	wg.Wait()

	// Every caller got the correct bytes...
	for i := range n {
		if statuses[i] != http.StatusOK {
			t.Errorf("request %d status = %d, want 200", i, statuses[i])
		}
		if string(bodies[i]) != string(reg.zipBytes) {
			t.Errorf("request %d body mismatch", i)
		}
	}
	// ...but the burst hit the upstream archive exactly once.
	if got := reg.zipHits.Load(); got != 1 {
		t.Errorf("upstream zip fetches = %d, want 1 (requests coalesced)", got)
	}
}

func TestUnknownProviderReturns404(t *testing.T) {
	reg := newFakeRegistry(t)
	h := newTestHandler(t, reg.server.URL)
	mux := http.NewServeMux()
	h.Routes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp := doGet(t, ts, "/registry.terraform.io/hashicorp/doesnotexist/index.json")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestInvalidPathReturns400(t *testing.T) {
	reg := newFakeRegistry(t)
	h := newTestHandler(t, reg.server.URL)
	mux := http.NewServeMux()
	h.Routes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// A version segment that fails validation.
	resp := doGet(t, ts, "/registry.terraform.io/hashicorp/null/not-a-version.json")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestZipChecksumMismatchIsRejectedAndNotCached(t *testing.T) {
	reg := newFakeRegistry(t)
	reg.servedShasum = "deadbeef" // does not match the real zip
	h := newTestHandler(t, reg.server.URL)
	mux := http.NewServeMux()
	h.Routes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	zipPath := "/registry.terraform.io/hashicorp/null/3.2.0/download/linux_amd64/terraform-provider-null_3.2.0_linux_amd64.zip"
	resp := doGet(t, ts, zipPath)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 on checksum mismatch", resp.StatusCode)
	}
	// A corrupt download must never be cached: a retry still goes upstream.
	resp = doGet(t, ts, zipPath)
	resp.Body.Close()
	if got := resp.Header.Get("X-Cache"); got == "HIT" {
		t.Error("checksum-mismatched zip must not be cached")
	}
}

func TestZipMissingUpstreamChecksumIsRejected(t *testing.T) {
	reg := newFakeRegistry(t)
	reg.servedShasum = "" // registry provides no checksum
	h := newTestHandler(t, reg.server.URL)
	mux := http.NewServeMux()
	h.Routes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	zipPath := "/registry.terraform.io/hashicorp/null/3.2.0/download/linux_amd64/terraform-provider-null_3.2.0_linux_amd64.zip"
	resp := doGet(t, ts, zipPath)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 when upstream provides no checksum", resp.StatusCode)
	}
}

func TestVersionsIndexTTLRevalidatesWhenStale(t *testing.T) {
	reg := newFakeRegistry(t)
	h := newTestHandlerTTL(t, reg.server.URL, 1*time.Minute)

	clock := time.Now()
	h.now = func() time.Time { return clock } // deterministic time

	mux := http.NewServeMux()
	h.Routes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	const path = "/registry.terraform.io/hashicorp/null/index.json"

	// 1. Cold: MISS, one upstream call.
	resp := doGet(t, ts, path)
	resp.Body.Close()
	if got := resp.Header.Get("X-Cache"); got != "MISS" {
		t.Fatalf("first X-Cache = %q, want MISS", got)
	}
	afterCold := reg.hits.Load()

	// 2. Within TTL: HIT, no upstream call.
	clock = clock.Add(30 * time.Second)
	resp = doGet(t, ts, path)
	resp.Body.Close()
	if got := resp.Header.Get("X-Cache"); got != "HIT" {
		t.Errorf("within-TTL X-Cache = %q, want HIT", got)
	}
	if reg.hits.Load() != afterCold {
		t.Error("within-TTL request should not hit upstream")
	}

	// 3. Past TTL: revalidate — MISS again, a fresh upstream call.
	clock = clock.Add(2 * time.Minute)
	resp = doGet(t, ts, path)
	resp.Body.Close()
	if got := resp.Header.Get("X-Cache"); got != "MISS" {
		t.Errorf("past-TTL X-Cache = %q, want MISS (revalidated)", got)
	}
	if reg.hits.Load() <= afterCold {
		t.Error("past-TTL request should revalidate against upstream")
	}
}

func TestVersionsIndexServedStaleOnUpstreamFailure(t *testing.T) {
	reg := newFakeRegistry(t)
	h := newTestHandlerTTL(t, reg.server.URL, 1*time.Minute)
	clock := time.Now()
	h.now = func() time.Time { return clock }

	mux := http.NewServeMux()
	h.Routes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	const path = "/registry.terraform.io/hashicorp/null/index.json"

	// Prime the cache.
	doGet(t, ts, path).Body.Close()

	// Upstream goes away, and the cached copy expires.
	reg.server.Close()
	clock = clock.Add(2 * time.Minute)

	resp := doGet(t, ts, path)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stale serve status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Cache"); got != "STALE" {
		t.Errorf("X-Cache = %q, want STALE", got)
	}
	var stale VersionsIndex
	decode(t, resp, &stale)
	if _, ok := stale.Versions["3.2.0"]; !ok {
		t.Errorf("stale body missing expected versions: %+v", stale.Versions)
	}
}

func TestVersionsIndexTTLDisabledNeverRevalidates(t *testing.T) {
	reg := newFakeRegistry(t)
	h := newTestHandlerTTL(t, reg.server.URL, 0) // disabled
	clock := time.Now()
	h.now = func() time.Time { return clock }

	mux := http.NewServeMux()
	h.Routes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	const path = "/registry.terraform.io/hashicorp/null/index.json"
	resp := doGet(t, ts, path)
	resp.Body.Close()
	afterCold := reg.hits.Load()

	// Even far in the future, a disabled TTL keeps serving the cached copy.
	clock = clock.Add(1000 * time.Hour)
	resp = doGet(t, ts, path)
	resp.Body.Close()
	if got := resp.Header.Get("X-Cache"); got != "HIT" {
		t.Errorf("disabled-TTL X-Cache = %q, want HIT", got)
	}
	if reg.hits.Load() != afterCold {
		t.Error("disabled TTL should never revalidate")
	}
}

// recordingMetrics counts versions-index outcomes for assertions.
type recordingMetrics struct {
	mu       sync.Mutex
	outcomes map[string]int
}

func (m *recordingMetrics) CacheLookup(string, bool) {}

func (m *recordingMetrics) VersionsIndexOutcome(outcome string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.outcomes[outcome]++
}

func TestVersionsIndexMetricsOutcomes(t *testing.T) {
	reg := newFakeRegistry(t)
	h := newTestHandlerTTL(t, reg.server.URL, time.Minute)
	rec := &recordingMetrics{outcomes: map[string]int{}}
	h.metrics = rec
	clock := time.Now()
	h.now = func() time.Time { return clock }

	mux := http.NewServeMux()
	h.Routes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	const path = "/registry.terraform.io/hashicorp/null/index.json"

	doGet(t, ts, path).Body.Close() // cold absent -> revalidated
	doGet(t, ts, path).Body.Close() // within TTL -> fresh

	clock = clock.Add(2 * time.Minute)
	doGet(t, ts, path).Body.Close() // stale -> revalidated

	reg.server.Close() // upstream down
	clock = clock.Add(2 * time.Minute)
	doGet(t, ts, path).Body.Close() // stale + upstream down -> stale served

	want := map[string]int{outcomeRevalidated: 2, outcomeFresh: 1, outcomeStale: 1}
	for outcome, n := range want {
		if rec.outcomes[outcome] != n {
			t.Errorf("outcome %q = %d, want %d (all: %v)", outcome, rec.outcomes[outcome], n, rec.outcomes)
		}
	}
}

func decode(t *testing.T, resp *http.Response, dst any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		t.Fatalf("decode: %v", err)
	}
}
