package observ

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/pascaltoepke/terrastrata/internal/httpx"
)

// Metrics holds terrastrata's Prometheus collectors and the registry they belong
// to. It satisfies mirror.Metrics via CacheLookup.
type Metrics struct {
	registry *prometheus.Registry

	cacheLookups *prometheus.CounterVec
	httpRequests *prometheus.CounterVec
	httpDuration *prometheus.HistogramVec
}

// NewMetrics creates and registers terrastrata's collectors on a private
// registry (plus standard Go/process collectors), avoiding global state.
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		registry: reg,
		cacheLookups: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "terrastrata_cache_lookups_total",
			Help: "Cache lookups by resource kind and result.",
		}, []string{"resource", "result"}),
		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "terrastrata_http_requests_total",
			Help: "HTTP requests by route and status code.",
		}, []string{"route", "code"}),
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "terrastrata_http_request_duration_seconds",
			Help:    "HTTP request duration by route.",
			Buckets: prometheus.DefBuckets,
		}, []string{"route"}),
	}
	reg.MustRegister(
		m.cacheLookups, m.httpRequests, m.httpDuration,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}

// CacheLookup implements mirror.Metrics.
func (m *Metrics) CacheLookup(resource string, hit bool) {
	result := "miss"
	if hit {
		result = "hit"
	}
	m.cacheLookups.WithLabelValues(resource, result).Inc()
}

// Handler returns the /metrics HTTP handler scoped to terrastrata's registry.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// Middleware records request counts and latency. The route label uses the
// matched ServeMux pattern (populated after routing), keeping cardinality bounded.
func (m *Metrics) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec, ok := w.(*httpx.ResponseRecorder)
		if !ok {
			rec = httpx.NewResponseRecorder(w)
			w = rec
		}

		next.ServeHTTP(w, r)

		route := r.Pattern
		if route == "" {
			route = "other"
		}
		m.httpRequests.WithLabelValues(route, strconv.Itoa(rec.Status)).Inc()
		m.httpDuration.WithLabelValues(route).Observe(time.Since(start).Seconds())
	})
}
