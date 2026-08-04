package mirror

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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
	// versionsDelay does the same for the /versions endpoint.
	versionsDelay time.Duration
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
		if fr.versionsDelay > 0 {
			time.Sleep(fr.versionsDelay)
		}
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
		Upstreams:  SingleUpstream("registry.terraform.io", u),
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
	if got := resp.ContentLength; got != int64(len(reg.zipBytes)) {
		t.Errorf("cached zip Content-Length = %d, want %d", got, len(reg.zipBytes))
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

func TestMismatchedHostnameReturns404AndNeverReachesUpstream(t *testing.T) {
	reg := newFakeRegistry(t)
	h := newTestHandler(t, reg.server.URL)
	mux := http.NewServeMux()
	h.Routes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// A hostname this mirror does not serve must 404 on every endpoint without
	// contacting upstream or caching anything under the foreign key.
	for _, p := range []string{
		"/evil.example/hashicorp/null/index.json",
		"/evil.example/hashicorp/null/3.2.0.json",
		"/evil.example/hashicorp/null/3.2.0/download/linux_amd64/terraform-provider-null_3.2.0_linux_amd64.zip",
	} {
		resp := doGet(t, ts, p)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", p, resp.StatusCode)
		}
	}
	if got := reg.hits.Load(); got != 0 {
		t.Errorf("mismatched hostname reached upstream %d times, want 0", got)
	}
}

func TestHostnameMatchIsCaseInsensitive(t *testing.T) {
	reg := newFakeRegistry(t)
	h := newTestHandler(t, reg.server.URL)
	mux := http.NewServeMux()
	h.Routes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp := doGet(t, ts, "/Registry.Terraform.IO/hashicorp/null/index.json")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 for case-insensitive hostname match", resp.StatusCode)
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

func TestZipUppercaseUpstreamChecksumVerifies(t *testing.T) {
	reg := newFakeRegistry(t)
	reg.servedShasum = strings.ToUpper(reg.zipSum) // uppercase hex is valid
	h := newTestHandler(t, reg.server.URL)
	mux := http.NewServeMux()
	h.Routes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	zipPath := "/registry.terraform.io/hashicorp/null/3.2.0/download/linux_amd64/terraform-provider-null_3.2.0_linux_amd64.zip"
	resp := doGet(t, ts, zipPath)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 for uppercase upstream shasum", resp.StatusCode)
	}
	if string(body) != string(reg.zipBytes) {
		t.Error("zip bytes mismatch")
	}
	resp = doGet(t, ts, zipPath)
	resp.Body.Close()
	if got := resp.Header.Get("X-Cache"); got != "HIT" {
		t.Errorf("second zip X-Cache = %q, want HIT", got)
	}
}

func TestZipMalformedUpstreamChecksumIsRejected(t *testing.T) {
	reg := newFakeRegistry(t)
	reg.servedShasum = "zz" + reg.zipSum[2:] // right length, not hex
	h := newTestHandler(t, reg.server.URL)
	mux := http.NewServeMux()
	h.Routes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	zipPath := "/registry.terraform.io/hashicorp/null/3.2.0/download/linux_amd64/terraform-provider-null_3.2.0_linux_amd64.zip"
	resp := doGet(t, ts, zipPath)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 for malformed upstream shasum", resp.StatusCode)
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
	mu        sync.Mutex
	outcomes  map[string]int
	upstreams map[string]int
}

func (m *recordingMetrics) CacheLookup(string, bool) {}

func (m *recordingMetrics) VersionsIndexOutcome(upstream, outcome string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.outcomes[outcome]++
	if m.upstreams == nil {
		m.upstreams = map[string]int{}
	}
	m.upstreams[upstream]++
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

func TestConcurrentColdVersionsRequestsRecordOneRevalidation(t *testing.T) {
	reg := newFakeRegistry(t)
	reg.versionsDelay = 50 * time.Millisecond // widen the overlap window
	h := newTestHandlerTTL(t, reg.server.URL, time.Minute)
	rec := &recordingMetrics{outcomes: map[string]int{}}
	h.metrics = rec

	mux := http.NewServeMux()
	h.Routes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	const n = 10
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			resp, err := http.Get(ts.URL + "/registry.terraform.io/hashicorp/null/index.json")
			if err != nil {
				t.Errorf("GET: %v", err)
				return
			}
			resp.Body.Close()
		}()
	}
	close(start)
	wg.Wait()

	// One request led the upstream fetch; the rest shared it. Requests that
	// arrive after the leader stored the result may count as "fresh" instead of
	// "coalesced" — either way, exactly one revalidation happened and every
	// request recorded exactly one outcome.
	if rec.outcomes[outcomeRevalidated] != 1 {
		t.Errorf("revalidated = %d, want 1 (outcomes: %v)", rec.outcomes[outcomeRevalidated], rec.outcomes)
	}
	total := 0
	for _, c := range rec.outcomes {
		total += c
	}
	if total != n {
		t.Errorf("total outcomes = %d, want %d (outcomes: %v)", total, n, rec.outcomes)
	}
	if got := reg.hits.Load(); got != 1 {
		t.Errorf("upstream versions fetches = %d, want 1", got)
	}
}

func decode(t *testing.T, resp *http.Response, dst any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

// --- Multi-upstream ---

// newMultiTestHandler serves two registries from one handler and one cache.
func newMultiTestHandler(t *testing.T, hosts map[string]string) *Handler {
	t.Helper()
	c, err := cache.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	ups := make(map[string]*Upstream, len(hosts))
	for hostname, base := range hosts {
		ups[hostname] = NewUpstream(base, "terrastrata-test", 5*time.Second)
	}
	h, err := NewHandler(Options{
		Cache:      c,
		Upstreams:  ups,
		Metrics:    NopMetrics{},
		StagingDir: t.TempDir(),
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return h
}

func multiMux(h *Handler) *http.ServeMux {
	mux := http.NewServeMux()
	h.Routes(mux)
	return mux
}

func getBody(t *testing.T, mux *http.ServeMux, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code, rec.Body.String()
}

// Each configured hostname must reach its own registry.
func TestMultipleUpstreamsRouteByHostname(t *testing.T) {
	a, b := newFakeRegistry(t), newFakeRegistry(t)
	mux := multiMux(newMultiTestHandler(t, map[string]string{
		"registry.terraform.io": a.server.URL,
		"registry.opentofu.org": b.server.URL,
	}))

	for _, host := range []string{"registry.terraform.io", "registry.opentofu.org"} {
		if code, _ := getBody(t, mux, "/"+host+"/hashicorp/null/index.json"); code != http.StatusOK {
			t.Errorf("%s index.json = %d, want 200", host, code)
		}
	}
	if a.hits.Load() == 0 || b.hits.Load() == 0 {
		t.Errorf("expected both upstreams to be reached (a=%d b=%d)", a.hits.Load(), b.hits.Load())
	}
}

// A hostname that is not configured stays a 404 — the guard against caching one
// registry's content under another's cache keys.
func TestUnconfiguredHostnameIsNotFound(t *testing.T) {
	a := newFakeRegistry(t)
	mux := multiMux(newMultiTestHandler(t, map[string]string{"registry.terraform.io": a.server.URL}))

	if code, _ := getBody(t, mux, "/registry.opentofu.org/hashicorp/null/index.json"); code != http.StatusNotFound {
		t.Errorf("unconfigured hostname = %d, want 404", code)
	}
	if n := a.hits.Load(); n != 0 {
		t.Errorf("unconfigured hostname reached an upstream %d times", n)
	}
}

// The property that makes a shared cache safe: the same namespace/type on two
// hostnames must serve each registry's own bytes, never the other's.
func TestUpstreamsDoNotAliasEachOther(t *testing.T) {
	a, b := newFakeRegistry(t), newFakeRegistry(t)
	// Give the two registries distinguishable content for the same coordinate.
	b.zipBytes = []byte("PK\x03\x04 opentofu's very different payload")
	sum := sha256.Sum256(b.zipBytes)
	b.zipSum = hex.EncodeToString(sum[:])
	b.servedShasum = b.zipSum

	mux := multiMux(newMultiTestHandler(t, map[string]string{
		"registry.terraform.io": a.server.URL,
		"registry.opentofu.org": b.server.URL,
	}))

	const zipPath = "/hashicorp/null/3.2.0/download/linux_amd64/terraform-provider-null_3.2.0_linux_amd64.zip"

	codeA, bodyA := getBody(t, mux, "/registry.terraform.io"+zipPath)
	codeB, bodyB := getBody(t, mux, "/registry.opentofu.org"+zipPath)
	if codeA != http.StatusOK || codeB != http.StatusOK {
		t.Fatalf("zip status: terraform=%d opentofu=%d, want 200 each", codeA, codeB)
	}

	if bodyA != string(a.zipBytes) {
		t.Errorf("registry.terraform.io served %q, want its own payload", bodyA)
	}
	if bodyB != string(b.zipBytes) {
		t.Errorf("registry.opentofu.org served %q, want its own payload", bodyB)
	}
	if bodyA == bodyB {
		t.Fatal("both hostnames served identical bytes: the cache is aliasing across upstreams")
	}

	// And on the warm path too, where a shared cache key would show up as a hit
	// returning the wrong registry's bytes.
	if _, warm := getBody(t, mux, "/registry.opentofu.org"+zipPath); warm != string(b.zipBytes) {
		t.Error("cached read served the wrong upstream's bytes")
	}
}

// The complement of TestHostnameMatchIsCaseInsensitive: a mixed-case hostname in
// the *configuration* must still match a lowercase request, since NewHandler
// normalizes the map keys.
func TestMixedCaseConfiguredHostnameMatches(t *testing.T) {
	a := newFakeRegistry(t)
	mux := multiMux(newMultiTestHandler(t, map[string]string{"Registry.Terraform.IO": a.server.URL}))

	if code, _ := getBody(t, mux, "/registry.terraform.io/hashicorp/null/index.json"); code != http.StatusOK {
		t.Errorf("lowercase request against mixed-case config = %d, want 200", code)
	}
}

func TestNewHandlerRejectsEmptyUpstreams(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := NewHandler(Options{StagingDir: t.TempDir(), Logger: log}); err == nil {
		t.Error("expected an error with no upstreams configured, got nil")
	}
	if _, err := NewHandler(Options{
		Upstreams:  map[string]*Upstream{"registry.terraform.io": nil},
		StagingDir: t.TempDir(),
		Logger:     log,
	}); err == nil {
		t.Error("expected an error for a nil upstream, got nil")
	}
}
