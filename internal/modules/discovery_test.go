package modules

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// discoveryRegistry serves the module registry protocol under a configurable API
// path, plus a discovery document that advertises it. It stands in for the
// private registries (Artifactory, Nexus) that do not use /v1/modules/.
type discoveryRegistry struct {
	server *httptest.Server

	// apiPath is where the module API actually lives, e.g.
	// "/artifactory/api/terraform/repo/v1/modules/".
	apiPath string
	// advertise is the modules.v1 value the discovery document returns. Empty
	// means the document is served without the key; see discoveryStatus for
	// serving no document at all.
	advertise string
	// discoveryStatus is the status the discovery endpoint answers with.
	discoveryStatus int

	discoveryHits atomic.Int64
	versionsHits  atomic.Int64
}

func newDiscoveryRegistry(t *testing.T, apiPath, advertise string) *discoveryRegistry {
	t.Helper()
	r := &discoveryRegistry{apiPath: apiPath, advertise: advertise, discoveryStatus: http.StatusOK}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/terraform.json", func(w http.ResponseWriter, _ *http.Request) {
		r.discoveryHits.Add(1)
		if r.discoveryStatus != http.StatusOK {
			http.Error(w, "nope", r.discoveryStatus)
			return
		}
		doc := map[string]string{}
		if r.advertise != "" {
			doc["modules.v1"] = r.advertise
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	})
	// The module API, wherever this registry keeps it.
	mux.HandleFunc("GET "+apiPath+"{namespace}/{name}/{system}/versions", func(w http.ResponseWriter, _ *http.Request) {
		r.versionsHits.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"modules": []map[string]any{
				{"versions": []map[string]string{{"version": "1.0.0"}}},
			},
		})
	})

	r.server = httptest.NewServer(mux)
	t.Cleanup(r.server.Close)
	return r
}

func testCoords(t *testing.T) Coordinates {
	t.Helper()
	c, err := Validate("ns", "vpc", "aws")
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	return c
}

// The point of the change: a registry that keeps its module API somewhere other
// than /v1/modules/ is usable, because the path comes from its own discovery
// document rather than from an assumption.
func TestUpstreamUsesTheAdvertisedModulesPath(t *testing.T) {
	const apiPath = "/artifactory/api/terraform/repo/v1/modules/"
	reg := newDiscoveryRegistry(t, apiPath, apiPath)
	u := NewUpstream(reg.server.URL, "terrastrata-test", 5*time.Second, nil)

	body, err := u.ListVersions(context.Background(), testCoords(t))
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if !strings.Contains(string(body), "1.0.0") {
		t.Errorf("body = %s", body)
	}
	if n := reg.versionsHits.Load(); n != 1 {
		t.Errorf("versions hits at the advertised path = %d, want 1", n)
	}
}

// An absolute modules.v1 is equally valid, and is what a registry serving its
// API from another host would return.
func TestUpstreamAcceptsAnAbsoluteAdvertisedPath(t *testing.T) {
	const apiPath = "/tf/v1/modules/"
	reg := newDiscoveryRegistry(t, apiPath, "")
	reg.advertise = reg.server.URL + apiPath
	u := NewUpstream(reg.server.URL, "terrastrata-test", 5*time.Second, nil)

	if _, err := u.ListVersions(context.Background(), testCoords(t)); err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if n := reg.versionsHits.Load(); n != 1 {
		t.Errorf("versions hits = %d, want 1", n)
	}
}

// A registry with no trailing slash on its advertised path still works: the
// prefix is normalised rather than producing "…v1/modulesns/vpc/aws/versions".
func TestUpstreamNormalisesAMissingTrailingSlash(t *testing.T) {
	const apiPath = "/tf/v1/modules/"
	reg := newDiscoveryRegistry(t, apiPath, strings.TrimSuffix(apiPath, "/"))
	u := NewUpstream(reg.server.URL, "terrastrata-test", 5*time.Second, nil)

	if _, err := u.ListVersions(context.Background(), testCoords(t)); err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if n := reg.versionsHits.Load(); n != 1 {
		t.Errorf("versions hits = %d, want 1", n)
	}
}

// Discovery is asked once and the answer reused: it must not cost a request per
// module request.
func TestUpstreamCachesDiscovery(t *testing.T) {
	const apiPath = "/tf/v1/modules/"
	reg := newDiscoveryRegistry(t, apiPath, apiPath)
	u := NewUpstream(reg.server.URL, "terrastrata-test", 5*time.Second, nil)

	for range 3 {
		if _, err := u.ListVersions(context.Background(), testCoords(t)); err != nil {
			t.Fatalf("ListVersions: %v", err)
		}
	}
	if n := reg.discoveryHits.Load(); n != 1 {
		t.Errorf("discovery requests = %d, want 1 for three module requests", n)
	}
}

// Registries that serve no discovery document — or one without modules.v1 — keep
// working on the assumption that held before this existed.
func TestUpstreamFallsBackToTheDefaultPath(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(*discoveryRegistry)
		expect string
	}{
		{"no discovery document", func(r *discoveryRegistry) { r.discoveryStatus = http.StatusNotFound }, defaultModulesPath},
		{"document without modules.v1", func(r *discoveryRegistry) { r.advertise = "" }, defaultModulesPath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := newDiscoveryRegistry(t, defaultModulesPath, defaultModulesPath)
			tc.setup(reg)
			u := NewUpstream(reg.server.URL, "terrastrata-test", 5*time.Second, nil)

			if _, err := u.ListVersions(context.Background(), testCoords(t)); err != nil {
				t.Fatalf("ListVersions: %v", err)
			}
			if n := reg.versionsHits.Load(); n != 1 {
				t.Errorf("versions hits on the default path = %d, want 1", n)
			}
		})
	}
}

// A failed discovery is retried on a cooldown rather than on every request, so a
// registry without a discovery document does not double every module request.
func TestUpstreamDoesNotRetryDiscoveryOnEveryRequest(t *testing.T) {
	reg := newDiscoveryRegistry(t, defaultModulesPath, defaultModulesPath)
	reg.discoveryStatus = http.StatusNotFound
	u := NewUpstream(reg.server.URL, "terrastrata-test", 5*time.Second, nil)

	for range 3 {
		if _, err := u.ListVersions(context.Background(), testCoords(t)); err != nil {
			t.Fatalf("ListVersions: %v", err)
		}
	}
	if n := reg.discoveryHits.Load(); n != 1 {
		t.Errorf("discovery attempts = %d, want 1 within the cooldown", n)
	}

	// Past the cooldown it tries again — and picks up a registry that has since
	// started serving one.
	u.mu.Lock()
	u.lastAttempt = time.Now().Add(-discoveryRetryAfter - time.Minute)
	u.mu.Unlock()
	reg.discoveryStatus = http.StatusOK

	if _, err := u.ListVersions(context.Background(), testCoords(t)); err != nil {
		t.Fatalf("ListVersions after the cooldown: %v", err)
	}
	if n := reg.discoveryHits.Load(); n != 2 {
		t.Errorf("discovery attempts after the cooldown = %d, want 2", n)
	}
}

// A discovery document must not be able to downgrade an https upstream to a
// plaintext API; that is something anyone on the path could ask for.
func TestUpstreamRefusesAPlaintextAdvertisedPath(t *testing.T) {
	reg := newDiscoveryRegistry(t, defaultModulesPath, "http://insecure.example/v1/modules/")
	u := NewUpstream(reg.server.URL, "terrastrata-test", 5*time.Second, nil)
	// The test server is http, so allowHTTP is true and the downgrade guard is
	// not in play; force the https posture the guard exists for.
	u.allowHTTP = false

	if _, err := u.ListVersions(context.Background(), testCoords(t)); err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	// It fell back to the default path on this registry rather than following
	// the plaintext address.
	if n := reg.versionsHits.Load(); n != 1 {
		t.Errorf("versions hits = %d, want 1 against the original host", n)
	}
}

func TestDiscoveryEndpointIsBuiltFromTheBase(t *testing.T) {
	reg := newDiscoveryRegistry(t, defaultModulesPath, defaultModulesPath)
	u := NewUpstream(reg.server.URL, "terrastrata-test", 5*time.Second, nil)
	got := u.modulesAPI(context.Background())
	want := fmt.Sprintf("%s%s", reg.server.URL, defaultModulesPath)
	if got != want {
		t.Errorf("modulesAPI = %q, want %q", got, want)
	}
}
