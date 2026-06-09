package prewarm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pascalinthecloud/terrastrata/internal/cache"
	"github.com/pascalinthecloud/terrastrata/internal/mirror"
)

func TestParseEntry(t *testing.T) {
	cases := []struct {
		in   string
		want Entry
	}{
		{"hashicorp/null", Entry{Hostname: "registry.terraform.io", Namespace: "hashicorp", Type: "null"}},
		{"hashicorp/azurerm@3.110.0", Entry{Hostname: "registry.terraform.io", Namespace: "hashicorp", Type: "azurerm", Version: "3.110.0"}},
		{"example.com/acme/foo@1.0.0", Entry{Hostname: "example.com", Namespace: "acme", Type: "foo", Version: "1.0.0"}},
	}
	for _, c := range cases {
		got, err := parseEntry(c.in)
		if err != nil {
			t.Errorf("parseEntry(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseEntry(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}

	for _, bad := range []string{"null", "a/b/c/d", "/type", "ns/"} {
		if _, err := parseEntry(bad); err == nil {
			t.Errorf("parseEntry(%q): expected error", bad)
		}
	}
}

// fakeRegistry implements the upstream registry protocol for hashicorp/null@3.2.0.
type fakeRegistry struct {
	server *httptest.Server
	hits   atomic.Int64
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	t.Helper()
	zip := []byte("PK\x03\x04 fake provider zip")
	sum := sha256.Sum256(zip)
	fr := &fakeRegistry{}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/providers/hashicorp/null/versions", func(w http.ResponseWriter, _ *http.Request) {
		fr.hits.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"versions": []map[string]any{
				{"version": "3.2.0", "platforms": []map[string]string{{"os": "linux", "arch": "amd64"}}},
			},
		})
	})
	mux.HandleFunc("GET /v1/providers/hashicorp/null/3.2.0/download/linux/amd64", func(w http.ResponseWriter, _ *http.Request) {
		fr.hits.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"os": "linux", "arch": "amd64",
			"filename":     "terraform-provider-null_3.2.0_linux_amd64.zip",
			"download_url": fr.server.URL + "/zip",
			"shasum":       hex.EncodeToString(sum[:]),
		})
	})
	mux.HandleFunc("GET /zip", func(w http.ResponseWriter, _ *http.Request) {
		fr.hits.Add(1)
		_, _ = w.Write(zip)
	})

	fr.server = httptest.NewServer(mux)
	t.Cleanup(fr.server.Close)
	return fr
}

func newTestMux(t *testing.T, upstreamURL, cacheDir string) http.Handler {
	t.Helper()
	c, err := cache.NewLocal(cacheDir)
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	h, err := mirror.NewHandler(mirror.Options{
		Cache:      c,
		Upstream:   mirror.NewUpstream(upstreamURL, "prewarm-test", 5*time.Second),
		StagingDir: t.TempDir(),
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	return mux
}

func TestRunWarmsVersionsArchivesAndZip(t *testing.T) {
	reg := newFakeRegistry(t)
	cacheDir := t.TempDir()
	mux := newTestMux(t, reg.server.URL, cacheDir)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	Run(context.Background(), mux, []string{"hashicorp/null@3.2.0"}, []string{"linux_amd64"}, log)

	// All three artifacts should be cached on disk.
	for _, rel := range []string{
		"registry.terraform.io/hashicorp/null/index.json",
		"registry.terraform.io/hashicorp/null/3.2.0.json",
		"registry.terraform.io/hashicorp/null/3.2.0/download/linux_amd64/terraform-provider-null_3.2.0_linux_amd64.zip",
	} {
		if _, err := os.Stat(filepath.Join(cacheDir, rel)); err != nil {
			t.Errorf("expected cached artifact %q: %v", rel, err)
		}
	}
}

func TestRunVersionsOnlyWhenNoVersionPinned(t *testing.T) {
	reg := newFakeRegistry(t)
	cacheDir := t.TempDir()
	mux := newTestMux(t, reg.server.URL, cacheDir)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	Run(context.Background(), mux, []string{"hashicorp/null"}, []string{"linux_amd64"}, log)

	if _, err := os.Stat(filepath.Join(cacheDir, "registry.terraform.io/hashicorp/null/index.json")); err != nil {
		t.Errorf("versions index should be warmed: %v", err)
	}
	// No version pinned -> archives/zip must not be fetched.
	if _, err := os.Stat(filepath.Join(cacheDir, "registry.terraform.io/hashicorp/null/3.2.0.json")); !os.IsNotExist(err) {
		t.Errorf("archives should not be warmed without a pinned version (err=%v)", err)
	}
}

func TestRunSkipsInvalidEntries(t *testing.T) {
	reg := newFakeRegistry(t)
	mux := newTestMux(t, reg.server.URL, t.TempDir())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	// Must not panic on a malformed entry; it is logged and skipped.
	Run(context.Background(), mux, []string{"not-a-valid-entry-with-too/many/slashes/here"}, []string{"linux_amd64"}, log)
}
