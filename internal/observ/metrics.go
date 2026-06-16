package observ

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/pascalinthecloud/terrastrata/internal/httpx"
)

// httpDurationBuckets must span two very different regimes on one histogram:
// fast cache hits (single-digit milliseconds) and slow cold fetches — a provider
// zip streamed from the upstream registry can take tens of seconds. Prometheus's
// DefBuckets top out at 10s, which collapses every slower request into +Inf and
// blinds the zip-route p95/p99. Extend the tail to 120s so that latency is
// actually measurable; the extra buckets are negligible on the fast routes.
var httpDurationBuckets = []float64{
	0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20, 40, 80, 120,
}

// Metrics holds terrastrata's Prometheus collectors and the registry they belong
// to. It satisfies mirror.Metrics via CacheLookup.
type Metrics struct {
	registry *prometheus.Registry

	cacheLookups  *prometheus.CounterVec
	httpRequests  *prometheus.CounterVec
	httpDuration  *prometheus.HistogramVec
	versionsIndex *prometheus.CounterVec
	prewarm       *prometheus.CounterVec
	cacheSize     prometheus.Gauge
	evictions     prometheus.Counter
	evictedBytes  prometheus.Counter
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
			Buckets: httpDurationBuckets,
		}, []string{"route"}),
		versionsIndex: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "terrastrata_versions_index_total",
			Help: "Versions-index requests by freshness outcome (fresh, revalidated, stale, error).",
		}, []string{"outcome"}),
		prewarm: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "terrastrata_prewarm_total",
			Help: "Startup pre-warm operations by resource and result.",
		}, []string{"resource", "result"}),
		cacheSize: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "terrastrata_cache_size_bytes",
			Help: "Total size of the local filesystem cache, measured on each evictor sweep.",
		}),
		evictions: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "terrastrata_cache_evictions_total",
			Help: "Cache files evicted to stay within the size budget.",
		}),
		evictedBytes: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "terrastrata_cache_evicted_bytes_total",
			Help: "Cache bytes evicted to stay within the size budget.",
		}),
	}
	reg.MustRegister(
		m.cacheLookups, m.httpRequests, m.httpDuration, m.versionsIndex, m.prewarm,
		m.cacheSize, m.evictions, m.evictedBytes,
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

// VersionsIndexOutcome implements mirror.Metrics.
func (m *Metrics) VersionsIndexOutcome(outcome string) {
	m.versionsIndex.WithLabelValues(outcome).Inc()
}

// PrewarmResult implements prewarm.Metrics.
func (m *Metrics) PrewarmResult(resource string, ok bool) {
	result := "error"
	if ok {
		result = "ok"
	}
	m.prewarm.WithLabelValues(resource, result).Inc()
}

// CacheSize implements cache.EvictorMetrics.
func (m *Metrics) CacheSize(bytes int64) {
	m.cacheSize.Set(float64(bytes))
}

// Evicted implements cache.EvictorMetrics.
func (m *Metrics) Evicted(files int, bytes int64) {
	m.evictions.Add(float64(files))
	m.evictedBytes.Add(float64(bytes))
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
