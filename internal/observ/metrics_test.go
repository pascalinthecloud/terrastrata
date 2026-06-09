package observ

import (
	"io"
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
