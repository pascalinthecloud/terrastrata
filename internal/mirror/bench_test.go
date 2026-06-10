package mirror

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pascalinthecloud/terrastrata/internal/cache"
)

// benchWriter is a no-op http.ResponseWriter so serve benchmarks measure the
// handler + cache, not response buffering.
type benchWriter struct{ h http.Header }

func (w *benchWriter) Header() http.Header {
	if w.h == nil {
		w.h = http.Header{}
	}
	return w.h
}
func (w *benchWriter) Write(b []byte) (int, error) { return len(b), nil }
func (w *benchWriter) WriteHeader(int)             {}

// realisticVersions builds a versions index body with n entries (azurerm has ~150).
func realisticVersions(n int) []byte {
	var b strings.Builder
	b.WriteString(`{"versions":{`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`"3.` + strconv.Itoa(i) + `.0":{}`)
	}
	b.WriteString(`}}`)
	return []byte(b.String())
}

func BenchmarkValidateCoordinates(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		base, _ := ValidateProvider("registry.terraform.io", "hashicorp", "azurerm")
		c, _ := base.withVersion("3.110.0")
		_, _ = c.withDownload("linux_amd64", "terraform-provider-azurerm_3.110.0_linux_amd64.zip")
	}
}

func BenchmarkVersionsEnvelope(b *testing.B) {
	body := realisticVersions(150)
	now := time.Now()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		env, _ := wrapVersions(body, now)
		if _, _, ok := unwrapVersions(env); !ok {
			b.Fatal("unwrap failed")
		}
	}
}

func benchHandler(b *testing.B) *Handler {
	b.Helper()
	c, err := cache.NewLocal(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	h, err := NewHandler(Options{
		Cache:      c,
		Upstream:   NewUpstream("http://unused.invalid", "bench", time.Second),
		StagingDir: b.TempDir(),
		IndexTTL:   time.Hour, // keep cached entry fresh so we measure the HIT path
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		b.Fatal(err)
	}
	return h
}

func BenchmarkServeVersionsHit(b *testing.B) {
	h := benchHandler(b)
	c, _ := ValidateProvider("registry.terraform.io", "hashicorp", "azurerm")
	h.storeVersions(context.Background(), VersionsCacheKey(c), realisticVersions(150))

	mux := http.NewServeMux()
	h.Routes(mux)
	const path = "/registry.terraform.io/hashicorp/azurerm/index.json"

	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := &benchWriter{}
		for pb.Next() {
			mux.ServeHTTP(w, req)
		}
	})
}

func BenchmarkServeZipHit(b *testing.B) {
	const zipSize = 256 * 1024 // 256 KiB
	h := benchHandler(b)
	base, _ := ValidateProvider("registry.terraform.io", "hashicorp", "azurerm")
	cv, _ := base.withVersion("3.110.0")
	c, _ := cv.withDownload("linux_amd64", "terraform-provider-azurerm_3.110.0_linux_amd64.zip")
	payload := make([]byte, zipSize)
	if err := h.cache.Put(context.Background(), ZipCacheKey(c), bytes.NewReader(payload)); err != nil {
		b.Fatal(err)
	}

	mux := http.NewServeMux()
	h.Routes(mux)
	path := "/registry.terraform.io/hashicorp/azurerm/3.110.0/download/linux_amd64/terraform-provider-azurerm_3.110.0_linux_amd64.zip"

	b.SetBytes(zipSize)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := &benchWriter{}
		for pb.Next() {
			mux.ServeHTTP(w, req)
		}
	})
}
