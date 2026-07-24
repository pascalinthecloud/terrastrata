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
	srv := buildServer(cfg, handler, metrics, logger)
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
