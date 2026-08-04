package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pascalinthecloud/terrastrata/internal/cache"
	"github.com/pascalinthecloud/terrastrata/internal/config"
	"github.com/pascalinthecloud/terrastrata/internal/mirror"
	"github.com/pascalinthecloud/terrastrata/internal/modules"
	"github.com/pascalinthecloud/terrastrata/internal/observ"
)

// newFakeRegistry serves the upstream registry protocol for hashicorp/null@3.2.0.
func newFakeRegistry(t *testing.T) *httptest.Server {
	t.Helper()
	zip := []byte("PK\x03\x04 fake provider zip")
	sum := sha256.Sum256(zip)

	mux := http.NewServeMux()
	var server *httptest.Server
	mux.HandleFunc("GET /v1/providers/hashicorp/null/versions", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"versions": []map[string]any{
				{"version": "3.2.0", "platforms": []map[string]string{{"os": "linux", "arch": "amd64"}}},
			},
		})
	})
	mux.HandleFunc("GET /v1/providers/hashicorp/null/3.2.0/download/linux/amd64", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"os": "linux", "arch": "amd64",
			"filename":     "terraform-provider-null_3.2.0_linux_amd64.zip",
			"download_url": server.URL + "/zip",
			"shasum":       hex.EncodeToString(sum[:]),
		})
	})
	mux.HandleFunc("GET /zip", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(zip)
	})

	// Module registry protocol for ns/vpc/aws@1.0.0, so the wiring tests can
	// exercise both protocols against one upstream.
	mux.HandleFunc("GET /v1/modules/ns/vpc/aws/versions", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"modules": []map[string]any{{"versions": []map[string]string{{"version": "1.0.0"}}}},
		})
	})
	mux.HandleFunc("GET /v1/modules/ns/vpc/aws/1.0.0/download", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Terraform-Get", server.URL+"/mod.tar.gz//*?archive=tar.gz")
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /mod.tar.gz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("\x1f\x8b fake module tarball"))
	})

	server = httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// newTestServer wires the full production handler tree (buildServer) over a
// fake registry and returns the running test server plus its metrics.
func newTestServer(t *testing.T, cfg config.Config, blobCache mirror.Cache) (*httptest.Server, *observ.Metrics) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	metrics := observ.NewMetrics()
	if blobCache == nil {
		local, err := cache.NewLocal(t.TempDir())
		if err != nil {
			t.Fatalf("NewLocal: %v", err)
		}
		blobCache = local
	}
	handler, err := mirror.NewHandler(mirror.Options{
		Cache:      blobCache,
		Upstream:   mirror.NewUpstream(cfg.UpstreamBase, "terrastrata-test", 5*time.Second),
		Metrics:    metrics,
		Hostname:   "registry.terraform.io",
		StagingDir: t.TempDir(),
		Logger:     logger,
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	var mods *modules.Handler
	if cfg.Modules.Enabled {
		mods, err = modules.NewHandler(modules.Options{
			Cache:      blobCache,
			Upstream:   modules.NewUpstream(cfg.Modules.UpstreamBase, "terrastrata-test", 5*time.Second),
			Metrics:    metrics,
			StagingDir: t.TempDir(),
			Logger:     logger,
		})
		if err != nil {
			t.Fatalf("modules.NewHandler: %v", err)
		}
	}
	srv := buildServer(cfg, handler, mods, metrics, logger)
	ts := httptest.NewServer(srv.Handler)
	t.Cleanup(ts.Close)
	return ts, metrics
}

// httpResult captures what tests assert on, so no response body escapes the
// helper unclosed.
type httpResult struct {
	status int
	body   string
}

func get(t *testing.T, url string) httpResult {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return httpResult{status: resp.StatusCode, body: string(body)}
}

func TestServerWiringSmoke(t *testing.T) {
	reg := newFakeRegistry(t)
	ts, _ := newTestServer(t, config.Config{UpstreamBase: reg.URL}, nil)

	health := get(t, ts.URL+"/health")
	if health.status != http.StatusOK || !strings.Contains(health.body, `"status":"ok"`) {
		t.Errorf("/health = %d %q", health.status, health.body)
	}

	if root := get(t, ts.URL+"/"); root.status != http.StatusNotFound {
		t.Errorf("/ = %d, want 404", root.status)
	}

	// One mirror request, then its route pattern must appear as a metrics label
	// (pins r.Pattern population through the nested muxes).
	if idx := get(t, ts.URL+"/registry.terraform.io/hashicorp/null/index.json"); idx.status != http.StatusOK {
		t.Fatalf("index.json = %d, want 200", idx.status)
	}
	metrics := get(t, ts.URL+"/metrics")
	if metrics.status != http.StatusOK {
		t.Fatalf("/metrics = %d", metrics.status)
	}
	wantLabel := `route="GET /{hostname}/{namespace}/{type}/index.json"`
	if !strings.Contains(metrics.body, wantLabel) {
		t.Errorf("/metrics missing %s", wantLabel)
	}
}

func TestAuthProtectsMirrorRoutesOnly(t *testing.T) {
	reg := newFakeRegistry(t)
	ts, _ := newTestServer(t, config.Config{UpstreamBase: reg.URL, AuthToken: "sekrit"}, nil)

	if r := get(t, ts.URL+"/registry.terraform.io/hashicorp/null/index.json"); r.status != http.StatusUnauthorized {
		t.Errorf("mirror route without token = %d, want 401", r.status)
	}
	for _, open := range []string{"/health", "/metrics"} {
		if r := get(t, ts.URL+open); r.status != http.StatusOK {
			t.Errorf("%s = %d, want 200 (unauthenticated)", open, r.status)
		}
	}

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/registry.terraform.io/hashicorp/null/index.json", nil)
	req.Header.Set("Authorization", "Bearer sekrit")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET with token: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("mirror route with token = %d, want 200", resp2.StatusCode)
	}
}

// panicCache panics on every operation, simulating a handler bug, so the test
// can assert that panicking requests still surface in metrics.
type panicCache struct{}

func (panicCache) Get(context.Context, string) (io.ReadCloser, bool, error) { panic("boom") }
func (panicCache) Put(context.Context, string, io.Reader) error             { panic("boom") }

func TestPanickingRequestIsRecordedInMetrics(t *testing.T) {
	reg := newFakeRegistry(t)
	ts, _ := newTestServer(t, config.Config{UpstreamBase: reg.URL}, panicCache{})

	if r := get(t, ts.URL+"/registry.terraform.io/hashicorp/null/index.json"); r.status != http.StatusInternalServerError {
		t.Fatalf("panicking route = %d, want 500", r.status)
	}
	if metrics := get(t, ts.URL+"/metrics"); !strings.Contains(metrics.body, `code="500"`) {
		t.Error("/metrics missing code=\"500\" for panicking request")
	}
}

// modulesConfig returns a config with the module registry enabled against reg.
func modulesConfig(regURL, token string) config.Config {
	return config.Config{
		UpstreamBase: regURL,
		AuthToken:    token,
		Modules:      config.ModulesConfig{Enabled: true, UpstreamBase: regURL},
	}
}

// Registering the module and provider route patterns on one ServeMux panics:
// /v1/modules/{ns}/{name}/{system}/{version}/download and
// /{hostname}/{namespace}/{type}/{version}/download/{platform}/{filename} both
// match some paths with neither being more specific. buildServer keeps them on
// separate muxes; this guards that split, because the failure mode is a crash at
// startup rather than a test failure somewhere quiet.
func TestBothProtocolsRouteWithoutConflict(t *testing.T) {
	reg := newFakeRegistry(t)
	ts, _ := newTestServer(t, modulesConfig(reg.URL, ""), nil)

	// Provider mirror still works alongside the module routes.
	if r := get(t, ts.URL+"/registry.terraform.io/hashicorp/null/index.json"); r.status != http.StatusOK {
		t.Errorf("provider index.json = %d, want 200", r.status)
	}
	if r := get(t, ts.URL+"/v1/modules/ns/vpc/aws/versions"); r.status != http.StatusOK {
		t.Errorf("module versions = %d, want 200", r.status)
	}
}

func TestModuleDiscoveryDocument(t *testing.T) {
	reg := newFakeRegistry(t)

	// Disabled by default: discovery must not appear, or clients would treat
	// terrastrata as a module registry it is not configured to be.
	off, _ := newTestServer(t, config.Config{UpstreamBase: reg.URL}, nil)
	if r := get(t, off.URL+"/.well-known/terraform.json"); r.status == http.StatusOK {
		t.Errorf("discovery served while modules are disabled = %d, want non-200", r.status)
	}

	ts, _ := newTestServer(t, modulesConfig(reg.URL, ""), nil)
	r := get(t, ts.URL+"/.well-known/terraform.json")
	if r.status != http.StatusOK {
		t.Fatalf("discovery = %d, want 200", r.status)
	}
	if !strings.Contains(r.body, `"modules.v1":"/v1/modules/"`) {
		t.Errorf("discovery body = %q", r.body)
	}
}

// Terraform attaches registry credentials only to registry endpoints; the
// go-getter fetch of X-Terraform-Get carries no Authorization header. So with
// AUTH_TOKEN set, metadata must be protected but the archive must stay open, or
// terraform init breaks on the download step.
func TestModuleArchiveStaysUnauthenticated(t *testing.T) {
	reg := newFakeRegistry(t)
	ts, _ := newTestServer(t, modulesConfig(reg.URL, "sekrit"), nil)

	// Discovery is the client's first request, before credentials are looked up.
	if r := get(t, ts.URL+"/.well-known/terraform.json"); r.status != http.StatusOK {
		t.Errorf("discovery with auth enabled = %d, want 200", r.status)
	}

	for _, protected := range []string{
		"/v1/modules/ns/vpc/aws/versions",
		"/v1/modules/ns/vpc/aws/1.0.0/download",
	} {
		if r := get(t, ts.URL+protected); r.status != http.StatusUnauthorized {
			t.Errorf("%s without token = %d, want 401", protected, r.status)
		}
	}

	if r := get(t, ts.URL+"/v1/modules/ns/vpc/aws/1.0.0/archive"); r.status != http.StatusOK {
		t.Errorf("archive without token = %d, want 200 (must not require auth)", r.status)
	}
}

func TestModuleDownloadRewritesHeaderThroughFullStack(t *testing.T) {
	reg := newFakeRegistry(t)
	ts, _ := newTestServer(t, modulesConfig(reg.URL, ""), nil)

	resp, err := http.Get(ts.URL + "/v1/modules/ns/vpc/aws/1.0.0/download")
	if err != nil {
		t.Fatalf("GET download: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("download = %d, want 204", resp.StatusCode)
	}
	want := "/v1/modules/ns/vpc/aws/1.0.0/archive//*?archive=tar.gz"
	if got := resp.Header.Get("X-Terraform-Get"); got != want {
		t.Fatalf("X-Terraform-Get = %q, want %q", got, want)
	}

	// The header Terraform receives must actually resolve to a served archive.
	// go-getter splits the "//subdir" off before fetching and carries the query
	// over to the base URL, so this is the request terrastrata really sees —
	// the archive route never has to match a path containing "//".
	fetch := "/v1/modules/ns/vpc/aws/1.0.0/archive?archive=tar.gz"
	if r := get(t, ts.URL+fetch); r.status != http.StatusOK {
		t.Errorf("following X-Terraform-Get = %d, want 200", r.status)
	}
}
