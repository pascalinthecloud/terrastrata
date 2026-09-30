package modules

import (
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

// fakeRegistry implements the upstream module registry protocol for a single
// module (ns/vpc/aws @ 1.0.0). It counts hits so tests can assert that the cache
// and the coalescing group prevent repeat upstream calls.
type fakeRegistry struct {
	server  *httptest.Server
	archive []byte

	versionsHits atomic.Int64
	downloadHits atomic.Int64
	archiveHits  atomic.Int64

	// getOverride replaces the X-Terraform-Get value the download endpoint
	// serves, so tests can exercise the pass-through path.
	getOverride string
	// versionsFail makes the versions endpoint fail, for the serve-stale path.
	versionsFail atomic.Bool
	// archiveDelay widens the window in which concurrent archive requests
	// overlap, making coalescing observable.
	archiveDelay time.Duration
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	t.Helper()
	fr := &fakeRegistry{archive: []byte("\x1f\x8b fake module tarball payload")}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/modules/ns/vpc/aws/versions", func(w http.ResponseWriter, _ *http.Request) {
		fr.versionsHits.Add(1)
		if fr.versionsFail.Load() {
			http.Error(w, "upstream down", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"modules": []map[string]any{
				{"versions": []map[string]string{{"version": "1.0.0"}, {"version": "1.1.0"}}},
			},
		})
	})
	mux.HandleFunc("GET /v1/modules/ns/vpc/aws/1.0.0/download", func(w http.ResponseWriter, _ *http.Request) {
		fr.downloadHits.Add(1)
		get := fr.getOverride
		if get == "" {
			get = fr.server.URL + "/archives/mod.tar.gz//*?archive=tar.gz"
		}
		w.Header().Set("X-Terraform-Get", get)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /archives/mod.tar.gz", func(w http.ResponseWriter, _ *http.Request) {
		fr.archiveHits.Add(1)
		if fr.archiveDelay > 0 {
			time.Sleep(fr.archiveDelay)
		}
		_, _ = w.Write(fr.archive)
	})

	fr.server = httptest.NewServer(mux)
	t.Cleanup(fr.server.Close)
	return fr
}

func newTestHandler(t *testing.T, base string, ttl time.Duration) *Handler {
	t.Helper()
	c, err := cache.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	h, err := NewHandler(Options{
		Cache:       c,
		Upstream:    NewUpstream(base, "terrastrata-test", 5*time.Second, nil),
		Metrics:     NopMetrics{},
		StagingDir:  t.TempDir(),
		VersionsTTL: ttl,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return h
}

// testMux mirrors the production split: metadata routes on one mux, the archive
// route on another, because they cannot share a mux with the provider routes.
func testMux(h *Handler) *http.ServeMux {
	mux := http.NewServeMux()
	h.RoutesMeta(mux)
	mux.HandleFunc(ArchivePattern, h.ArchiveHandler())
	return mux
}

// newSignedTestHandler is newTestHandler with auth enabled, so archive URLs are
// signed and the archive endpoint demands a valid signature.
func newSignedTestHandler(t *testing.T, base, token string) *Handler {
	t.Helper()
	c, err := cache.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	h, err := NewHandler(Options{
		Cache:      c,
		Upstream:   NewUpstream(base, "terrastrata-test", 5*time.Second, nil),
		Metrics:    NopMetrics{},
		StagingDir: t.TempDir(),
		AuthToken:  token,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return h
}

// archivePathFrom turns an X-Terraform-Get value into the request go-getter
// would actually make: it strips the "//subdir" suffix, keeping the query.
func archivePathFrom(get string) string {
	base, _ := splitSubdir(get)
	return base
}

func do(t *testing.T, mux *http.ServeMux, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestVersionsServedAndCached(t *testing.T) {
	fr := newFakeRegistry(t)
	mux := testMux(newTestHandler(t, fr.server.URL, 0))

	rec := do(t, mux, "/v1/modules/ns/vpc/aws/versions")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("X-Cache"); got != "MISS" {
		t.Errorf("X-Cache = %q, want MISS", got)
	}
	// The document is served back verbatim: we speak the same protocol upstream
	// and downstream, so no translation should have happened.
	var parsed VersionsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(parsed.Modules) != 1 || len(parsed.Modules[0].Versions) != 2 {
		t.Fatalf("unexpected versions document: %s", rec.Body.String())
	}

	rec = do(t, mux, "/v1/modules/ns/vpc/aws/versions")
	if got := rec.Header().Get("X-Cache"); got != "HIT" {
		t.Errorf("second request X-Cache = %q, want HIT", got)
	}
	if n := fr.versionsHits.Load(); n != 1 {
		t.Errorf("upstream versions hits = %d, want 1", n)
	}
}

func TestVersionsServedStaleOnUpstreamFailure(t *testing.T) {
	fr := newFakeRegistry(t)
	// A zero TTL would never revalidate, so use a TTL that has already expired
	// by the time of the second request.
	h := newTestHandler(t, fr.server.URL, time.Nanosecond)
	mux := testMux(h)

	if rec := do(t, mux, "/v1/modules/ns/vpc/aws/versions"); rec.Code != http.StatusOK {
		t.Fatalf("warm-up status = %d, want 200", rec.Code)
	}

	fr.versionsFail.Store(true)
	rec := do(t, mux, "/v1/modules/ns/vpc/aws/versions")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (last-known-good)", rec.Code)
	}
	if got := rec.Header().Get("X-Cache"); got != "STALE" {
		t.Errorf("X-Cache = %q, want STALE", got)
	}
}

func TestVersionsNotFound(t *testing.T) {
	fr := newFakeRegistry(t)
	mux := testMux(newTestHandler(t, fr.server.URL, 0))

	if rec := do(t, mux, "/v1/modules/ns/absent/aws/versions"); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestDownloadRewritesTerraformGet(t *testing.T) {
	fr := newFakeRegistry(t)
	mux := testMux(newTestHandler(t, fr.server.URL, 0))

	rec := do(t, mux, "/v1/modules/ns/vpc/aws/1.0.0/download")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	want := "/v1/modules/ns/vpc/aws/1.0.0/archive//*?archive=tar.gz"
	if got := rec.Header().Get("X-Terraform-Get"); got != want {
		t.Errorf("X-Terraform-Get = %q, want %q", got, want)
	}

	// The resolved location is cached, so a repeat resolution costs no upstream
	// call — this is what keeps terraform init off the GitHub API rate limit.
	rec = do(t, mux, "/v1/modules/ns/vpc/aws/1.0.0/download")
	if got := rec.Header().Get("X-Cache"); got != "HIT" {
		t.Errorf("second download X-Cache = %q, want HIT", got)
	}
	if n := fr.downloadHits.Load(); n != 1 {
		t.Errorf("upstream download hits = %d, want 1", n)
	}
}

func TestDownloadPassesThroughUncacheableSource(t *testing.T) {
	fr := newFakeRegistry(t)
	// A self-hosted git server: not GitHub, so there is no tarball endpoint to
	// rewrite to and no getter we carry.
	fr.getOverride = "git::ssh://git@git.corp.example/o/r.git//modules/vpc?ref=v1.0.0"
	mux := testMux(newTestHandler(t, fr.server.URL, 0))

	rec := do(t, mux, "/v1/modules/ns/vpc/aws/1.0.0/download")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("X-Terraform-Get"); got != fr.getOverride {
		t.Errorf("X-Terraform-Get = %q, want the upstream value verbatim %q", got, fr.getOverride)
	}

	// There is no terrastrata-hosted archive for a bypassed source, so asking
	// for one must 404 rather than trying to fetch a git repo over HTTP.
	if rec := do(t, mux, "/v1/modules/ns/vpc/aws/1.0.0/archive"); rec.Code != http.StatusNotFound {
		t.Errorf("archive status = %d, want 404", rec.Code)
	}
}

// The live public registry hands out git:: sources for every module, so this is
// the path that actually runs against registry.terraform.io — not the https
// tarball form shown in the protocol docs.
func TestDownloadRewritesGitHubGitSource(t *testing.T) {
	fr := newFakeRegistry(t)
	fr.getOverride = "git::https://github.com/claranet/terraform-azurerm-regions?ref=8f5239c3689d08631363fcff392b50a6bb1a33f1"
	mux := testMux(newTestHandler(t, fr.server.URL, 0))

	rec := do(t, mux, "/v1/modules/ns/vpc/aws/1.0.0/download")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	// No subdir: the wrapper directory is stripped when the archive is served,
	// because Terraform does not expand the go-getter "//*" glob for registry
	// modules — it records the literal path and then fails to read it.
	want := "/v1/modules/ns/vpc/aws/1.0.0/archive?archive=tar.gz"
	if got := rec.Header().Get("X-Terraform-Get"); got != want {
		t.Errorf("X-Terraform-Get = %q, want %q", got, want)
	}
}

func TestArchiveFetchedThenCached(t *testing.T) {
	fr := newFakeRegistry(t)
	mux := testMux(newTestHandler(t, fr.server.URL, 0))

	rec := do(t, mux, "/v1/modules/ns/vpc/aws/1.0.0/archive")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("X-Cache"); got != "MISS" {
		t.Errorf("X-Cache = %q, want MISS", got)
	}
	if got := rec.Body.Bytes(); string(got) != string(fr.archive) {
		t.Errorf("body = %q, want the upstream archive bytes", got)
	}

	rec = do(t, mux, "/v1/modules/ns/vpc/aws/1.0.0/archive")
	if got := rec.Header().Get("X-Cache"); got != "HIT" {
		t.Errorf("second request X-Cache = %q, want HIT", got)
	}
	if rec.Body.String() != string(fr.archive) {
		t.Error("cached archive body differs from the upstream bytes")
	}
	if n := fr.archiveHits.Load(); n != 1 {
		t.Errorf("upstream archive hits = %d, want 1", n)
	}
}

// A burst of cold requests for the same archive must collapse into one upstream
// download — the CI-fleet-starting-at-once case.
func TestArchiveRequestsAreCoalesced(t *testing.T) {
	fr := newFakeRegistry(t)
	fr.archiveDelay = 100 * time.Millisecond
	mux := testMux(newTestHandler(t, fr.server.URL, 0))

	const concurrent = 8
	var wg sync.WaitGroup
	codes := make([]int, concurrent)
	for i := range concurrent {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = do(t, mux, "/v1/modules/ns/vpc/aws/1.0.0/archive").Code
		}()
	}
	wg.Wait()

	for i, c := range codes {
		if c != http.StatusOK {
			t.Errorf("request %d: status = %d, want 200", i, c)
		}
	}
	if n := fr.archiveHits.Load(); n != 1 {
		t.Errorf("upstream archive hits = %d, want 1 (requests were not coalesced)", n)
	}
}

func TestArchiveRejectsOversizedUpstream(t *testing.T) {
	// A body past the cap must be rejected outright, never truncated and cached
	// as a valid-looking archive.
	const limit = 4 << 10
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/download") {
			w.Header().Set("X-Terraform-Get", "http://"+r.Host+"/huge.tar.gz")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = w.Write(make([]byte, limit*2))
	}))
	t.Cleanup(big.Close)

	h := newTestHandler(t, big.URL, 0)
	h.maxArchive = limit
	mux := testMux(h)

	if rec := do(t, mux, "/v1/modules/ns/vpc/aws/1.0.0/archive"); rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
	// Nothing may have been cached: a later request must still go upstream
	// rather than serve a truncated archive.
	if rec := do(t, mux, "/v1/modules/ns/vpc/aws/1.0.0/archive"); rec.Code != http.StatusBadGateway {
		t.Errorf("repeat status = %d, want 502 (a truncated archive was cached)", rec.Code)
	}
}

func TestInvalidCoordinatesRejected(t *testing.T) {
	fr := newFakeRegistry(t)
	mux := testMux(newTestHandler(t, fr.server.URL, 0))

	// Percent-encoded traversal survives ServeMux path-value decoding, so the
	// validators — not the router — are what must reject it.
	for _, path := range []string{
		"/v1/modules/ns/%2e%2e/aws/versions",
		"/v1/modules/%2e%2e/vpc/aws/versions",
		"/v1/modules/ns/vpc/aws/%2e%2e/download",
		"/v1/modules/ns/vpc/aws/latest/download",
		"/v1/modules/ns/vpc/aws/%2e%2e/archive",
	} {
		if rec := do(t, mux, path); rec.Code != http.StatusBadRequest {
			t.Errorf("GET %s: status = %d, want 400", path, rec.Code)
		}
	}
}

func TestDiscoveryDocument(t *testing.T) {
	fr := newFakeRegistry(t)
	h := newTestHandler(t, fr.server.URL, 0)

	rec := httptest.NewRecorder()
	h.Discovery(rec, httptest.NewRequest(http.MethodGet, "/.well-known/terraform.json", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var doc map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if doc["modules.v1"] != "/v1/modules/" {
		t.Errorf("modules.v1 = %q, want /v1/modules/", doc["modules.v1"])
	}
	// terrastrata is a provider *mirror*, not a provider registry: advertising
	// providers.v1 would invite clients to use a protocol it does not implement.
	if _, ok := doc["providers.v1"]; ok {
		t.Error("discovery advertises providers.v1, which terrastrata does not implement")
	}
}

// With AUTH_TOKEN set, the archive endpoint cannot check a bearer header
// (Terraform sends none on the X-Terraform-Get fetch), so it must instead
// require the signature the authenticated download endpoint mints. Without this,
// AUTH_TOKEN would cover every route but the one that serves module source.
func TestArchiveRequiresSignatureWhenAuthEnabled(t *testing.T) {
	fr := newFakeRegistry(t)
	h := newSignedTestHandler(t, fr.server.URL, "s3cr3t")
	mux := testMux(h)

	rec := do(t, mux, "/v1/modules/ns/vpc/aws/1.0.0/download")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("download status = %d, want 204", rec.Code)
	}
	get := rec.Header().Get("X-Terraform-Get")
	if !strings.Contains(get, "&sig=") || !strings.Contains(get, "&exp=") {
		t.Fatalf("X-Terraform-Get = %q, want it signed", get)
	}
	signed := archivePathFrom(get)

	// The signed URL works.
	if rec := do(t, mux, signed); rec.Code != http.StatusOK {
		t.Fatalf("signed archive status = %d, want 200", rec.Code)
	}
	if n := fr.archiveHits.Load(); n != 1 {
		t.Fatalf("upstream archive hits = %d, want 1", n)
	}

	// Everything else does not — including a request for an archive already
	// sitting in the cache, which is the case that would otherwise leak module
	// source to an unauthenticated caller.
	bad := []struct {
		name string
		path string
	}{
		{"no signature", "/v1/modules/ns/vpc/aws/1.0.0/archive?archive=tar.gz"},
		{"tampered signature", strings.Replace(signed, "sig=", "sig=00", 1)},
		{"signature for another module", strings.Replace(signed, "/vpc/", "/vpc2/", 1)},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if rec := do(t, mux, tc.path); rec.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403", rec.Code)
			}
		})
	}
	// A rejected request must not have reached upstream either.
	if n := fr.archiveHits.Load(); n != 1 {
		t.Errorf("upstream archive hits = %d, want 1 (rejected requests must not fetch)", n)
	}
}

func TestArchiveSignatureExpires(t *testing.T) {
	fr := newFakeRegistry(t)
	h := newSignedTestHandler(t, fr.server.URL, "s3cr3t")
	mux := testMux(h)

	rec := do(t, mux, "/v1/modules/ns/vpc/aws/1.0.0/download")
	signed := archivePathFrom(rec.Header().Get("X-Terraform-Get"))

	// Move the clock past the URL's lifetime.
	h.now = func() time.Time { return time.Now().Add(archiveURLTTL + time.Minute) }
	if rec := do(t, mux, signed); rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for an expired URL", rec.Code)
	}
}

// With no token configured, nothing changes: the URL carries no signature and
// the archive endpoint serves it. This is the default internal-network mode.
func TestArchiveUnsignedWhenAuthDisabled(t *testing.T) {
	fr := newFakeRegistry(t)
	mux := testMux(newTestHandler(t, fr.server.URL, 0))

	rec := do(t, mux, "/v1/modules/ns/vpc/aws/1.0.0/download")
	get := rec.Header().Get("X-Terraform-Get")
	if strings.Contains(get, "sig=") {
		t.Errorf("X-Terraform-Get = %q, want no signature when auth is disabled", get)
	}
	if rec := do(t, mux, archivePathFrom(get)); rec.Code != http.StatusOK {
		t.Errorf("archive status = %d, want 200", rec.Code)
	}
}
