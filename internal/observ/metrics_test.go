package observ

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestMetricsSurface verifies that recorded values render under their expected
// names in the /metrics output (end-to-end through the real registry).
func TestMetricsSurface(t *testing.T) {
	m := NewMetrics()
	m.CacheLookup("versions", true)
	m.VersionsIndexOutcome("stale")
	m.PrewarmResult("zip", true)
	m.PrewarmResult("archives", false)

	body := scrape(t, m)

	for _, want := range []string{
		`terrastrata_cache_lookups_total{resource="versions",result="hit"} 1`,
		`terrastrata_versions_index_total{outcome="stale"} 1`,
		`terrastrata_prewarm_total{resource="zip",result="ok"} 1`,
		`terrastrata_prewarm_total{resource="archives",result="error"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q", want)
		}
	}
}

func TestMiddlewareRecordsRouteAndStatus(t *testing.T) {
	m := NewMetrics()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /widgets/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})
	h := m.Middleware(mux)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/widgets/42", nil))

	body := scrape(t, m)
	want := `terrastrata_http_requests_total{code="201",route="GET /widgets/{id}"} 1`
	if !strings.Contains(body, want) {
		t.Errorf("metrics output missing %q", want)
	}
	if !strings.Contains(body, `terrastrata_http_request_duration_seconds_count{route="GET /widgets/{id}"} 1`) {
		t.Error("metrics output missing duration observation for the route")
	}
}

func TestMiddlewareLabelsUnroutedRequestsAsOther(t *testing.T) {
	m := NewMetrics()
	h := m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/nowhere", nil))

	body := scrape(t, m)
	want := `terrastrata_http_requests_total{code="404",route="other"} 1`
	if !strings.Contains(body, want) {
		t.Errorf("metrics output missing %q", want)
	}
}

func TestEvictorMetricsSurface(t *testing.T) {
	m := NewMetrics()
	m.CacheSize(12345)
	m.Evicted(3, 999)

	body := scrape(t, m)
	for _, want := range []string{
		`terrastrata_cache_size_bytes 12345`,
		`terrastrata_cache_evictions_total 3`,
		`terrastrata_cache_evicted_bytes_total 999`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q", want)
		}
	}
}

func TestNewLoggerRespectsLevel(t *testing.T) {
	var buf strings.Builder
	log := NewLogger(&buf, slog.LevelWarn)
	log.Info("hidden")
	log.Warn("visible")
	out := buf.String()
	if strings.Contains(out, "hidden") {
		t.Error("info line logged despite warn level")
	}
	if !strings.Contains(out, "visible") || !strings.Contains(out, `"level":"WARN"`) {
		t.Errorf("expected JSON warn line, got %q", out)
	}
}

func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 {
		t.Fatalf("metrics status = %d", rec.Code)
	}
	b, _ := io.ReadAll(rec.Result().Body)
	return string(b)
}
