package mirror

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFetchZipSchemeRestrictions(t *testing.T) {
	zip := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("zip-bytes"))
	}))
	defer zip.Close()

	t.Run("https upstream refuses http download url", func(t *testing.T) {
		u := NewUpstream("https://registry.example", "test", 5*time.Second)
		_, err := u.FetchZip(context.Background(), zip.URL+"/zip")
		if err == nil || !strings.Contains(err.Error(), "https required") {
			t.Fatalf("err = %v, want https-required refusal", err)
		}
	})

	t.Run("http upstream allows http download url", func(t *testing.T) {
		u := NewUpstream(zip.URL, "test", 5*time.Second)
		rc, err := u.FetchZip(context.Background(), zip.URL+"/zip")
		if err != nil {
			t.Fatalf("FetchZip: %v", err)
		}
		body, _ := io.ReadAll(rc)
		rc.Close()
		if string(body) != "zip-bytes" {
			t.Errorf("body = %q", body)
		}
	})

	t.Run("non-http schemes always refused", func(t *testing.T) {
		u := NewUpstream(zip.URL, "test", 5*time.Second)
		for _, bad := range []string{"ftp://host/zip", "file:///etc/passwd", "://broken"} {
			if _, err := u.FetchZip(context.Background(), bad); err == nil {
				t.Errorf("FetchZip(%q) succeeded, want refusal", bad)
			}
		}
	})
}
