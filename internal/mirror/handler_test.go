package mirror

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
	hits     atomic.Int64
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	t.Helper()
	zip := []byte("PK\x03\x04 this is a fake provider zip payload")
	sum := sha256.Sum256(zip)
	fr := &fakeRegistry{zipBytes: zip, zipSum: hex.EncodeToString(sum[:])}

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
			"shasum":       fr.zipSum,
		})
	})
	mux.HandleFunc("GET /zip", func(w http.ResponseWriter, _ *http.Request) {
		fr.hits.Add(1)
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(fr.zipBytes)
	})

	fr.server = httptest.NewServer(mux)
	t.Cleanup(fr.server.Close)
	return fr
}

func newTestHandler(t *testing.T, base string) *Handler {
	t.Helper()
	c, err := cache.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	u := NewUpstream(base, "terrastrata-test", 5*time.Second)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewHandler(c, u, NopMetrics{}, log)
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

func decode(t *testing.T, resp *http.Response, dst any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		t.Fatalf("decode: %v", err)
	}
}
