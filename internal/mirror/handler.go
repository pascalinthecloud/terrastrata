package mirror

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// maxZipBytes caps a single provider archive we will buffer and cache. Real
// provider zips are tens of MB; this guards against a hostile/broken upstream.
const maxZipBytes = 1 << 30 // 1 GiB

// Metrics records cache outcomes. The server supplies a Prometheus-backed
// implementation; tests and bare setups can use NopMetrics.
type Metrics interface {
	// CacheLookup records whether a lookup for the given resource kind
	// ("versions", "archives", "zip") hit the cache.
	CacheLookup(resource string, hit bool)
}

// NopMetrics is a no-op Metrics.
type NopMetrics struct{}

func (NopMetrics) CacheLookup(string, bool) {}

// Handler serves the Terraform provider network mirror protocol, backed by a
// pull-through cache over an upstream provider registry.
type Handler struct {
	cache    Cache
	upstream *Upstream
	metrics  Metrics
	log      *slog.Logger
}

// Cache is the subset of internal/cache used by the handler, restated here to
// avoid a package dependency cycle and keep the handler unit-testable.
type Cache interface {
	Get(ctx context.Context, key string) (io.ReadCloser, bool, error)
	Put(ctx context.Context, key string, data []byte) error
}

// NewHandler builds a mirror Handler. metrics may be NopMetrics{}.
func NewHandler(c Cache, u *Upstream, m Metrics, log *slog.Logger) *Handler {
	if m == nil {
		m = NopMetrics{}
	}
	return &Handler{cache: c, upstream: u, metrics: m, log: log}
}

// Routes registers the mirror endpoints on a ServeMux. The caller owns the mux
// (and adds /health, /metrics, and middleware).
func (h *Handler) Routes(mux *http.ServeMux) {
	// Order doesn't matter: Go's ServeMux prefers the most specific pattern, so
	// the literal "index.json" wins over the "{versionfile}" wildcard.
	mux.HandleFunc("GET /{hostname}/{namespace}/{type}/index.json", h.handleVersions)
	mux.HandleFunc("GET /{hostname}/{namespace}/{type}/{versionfile}", h.handleArchives)
	mux.HandleFunc("GET /{hostname}/{namespace}/{type}/{version}/download/{platform}/{filename}", h.handleZip)
}

func (h *Handler) handleVersions(w http.ResponseWriter, r *http.Request) {
	c, err := ValidateProvider(r.PathValue("hostname"), r.PathValue("namespace"), r.PathValue("type"))
	if err != nil {
		h.fail(w, r, http.StatusBadRequest, err)
		return
	}

	key := VersionsCacheKey(c)
	if h.serveFromCache(w, r, key, "versions", "application/json") {
		return
	}

	versions, err := h.upstream.ListVersions(r.Context(), c)
	if err != nil {
		h.failUpstream(w, r, err)
		return
	}
	body, err := json.Marshal(BuildVersionsIndex(versions))
	if err != nil {
		h.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	h.store(r.Context(), key, body)
	writeCached(w, "application/json", false, body)
}

func (h *Handler) handleArchives(w http.ResponseWriter, r *http.Request) {
	versionFile := r.PathValue("versionfile")
	version, ok := strings.CutSuffix(versionFile, ".json")
	if !ok {
		// Not a "<version>.json" request — nothing else lives at this depth.
		http.NotFound(w, r)
		return
	}

	base, err := ValidateProvider(r.PathValue("hostname"), r.PathValue("namespace"), r.PathValue("type"))
	if err != nil {
		h.fail(w, r, http.StatusBadRequest, err)
		return
	}
	c, err := base.withVersion(version)
	if err != nil {
		h.fail(w, r, http.StatusBadRequest, err)
		return
	}

	key := ArchivesCacheKey(c)
	if h.serveFromCache(w, r, key, "archives", "application/json") {
		return
	}

	// Resolve the platforms published for this version, then assemble the index.
	versions, err := h.upstream.ListVersions(r.Context(), c)
	if err != nil {
		h.failUpstream(w, r, err)
		return
	}
	platforms, err := PlatformsForVersion(versions, version)
	if err != nil {
		h.failUpstream(w, r, err)
		return
	}
	idx, err := BuildArchivesIndex(r.Context(), h.upstream, c, platforms)
	if err != nil {
		h.failUpstream(w, r, err)
		return
	}
	body, err := json.Marshal(idx)
	if err != nil {
		h.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	h.store(r.Context(), key, body)
	writeCached(w, "application/json", false, body)
}

func (h *Handler) handleZip(w http.ResponseWriter, r *http.Request) {
	base, err := ValidateProvider(r.PathValue("hostname"), r.PathValue("namespace"), r.PathValue("type"))
	if err != nil {
		h.fail(w, r, http.StatusBadRequest, err)
		return
	}
	c, err := base.withVersion(r.PathValue("version"))
	if err != nil {
		h.fail(w, r, http.StatusBadRequest, err)
		return
	}
	c, err = c.withDownload(r.PathValue("platform"), r.PathValue("filename"))
	if err != nil {
		h.fail(w, r, http.StatusBadRequest, err)
		return
	}

	key := ZipCacheKey(c)
	if h.serveFromCache(w, r, key, "zip", "application/zip") {
		return
	}

	os, arch, _ := strings.Cut(c.Platform, "_")
	meta, err := h.upstream.GetDownload(r.Context(), c, os, arch)
	if err != nil {
		h.failUpstream(w, r, err)
		return
	}
	if meta.Filename != c.Filename {
		h.fail(w, r, http.StatusNotFound, errors.New("mirror: requested filename does not match upstream"))
		return
	}

	rc, err := h.upstream.FetchZip(r.Context(), meta.DownloadURL)
	if err != nil {
		h.failUpstream(w, r, err)
		return
	}
	defer rc.Close()

	data, err := io.ReadAll(io.LimitReader(rc, maxZipBytes))
	if err != nil {
		h.failUpstream(w, r, err)
		return
	}

	// Integrity: the registry publishes a sha256 of the zip. Verify it before we
	// cache or serve, so a corrupted/tampered download is never persisted.
	if meta.Shasum != "" {
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != meta.Shasum {
			h.fail(w, r, http.StatusBadGateway, errors.New("mirror: upstream zip checksum mismatch"))
			return
		}
	}

	h.store(r.Context(), key, data)
	writeCached(w, "application/zip", false, data)
}

// serveFromCache writes a cache hit to the response and reports whether it did.
// On any cache read error it logs and returns false so the caller falls through
// to the upstream path (the cache must never be a hard dependency).
func (h *Handler) serveFromCache(w http.ResponseWriter, r *http.Request, key, resource, contentType string) bool {
	rc, hit, err := h.cache.Get(r.Context(), key)
	if err != nil {
		h.log.Warn("cache read failed", "key", key, "err", err)
		hit = false
	}
	h.metrics.CacheLookup(resource, hit)
	if !hit {
		return false
	}
	defer rc.Close()

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Cache", "HIT")
	if _, err := io.Copy(w, rc); err != nil {
		h.log.Warn("write cached response failed", "key", key, "err", err)
	}
	return true
}

// store writes to the cache, logging (but not failing the request) on error.
func (h *Handler) store(ctx context.Context, key string, data []byte) {
	if err := h.cache.Put(ctx, key, data); err != nil {
		h.log.Warn("cache write failed", "key", key, "err", err)
	}
}

func writeCached(w http.ResponseWriter, contentType string, hit bool, body []byte) {
	w.Header().Set("Content-Type", contentType)
	if hit {
		w.Header().Set("X-Cache", "HIT")
	} else {
		w.Header().Set("X-Cache", "MISS")
	}
	_, _ = w.Write(body)
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, status int, err error) {
	h.log.Warn("request failed", "status", status, "path", r.URL.Path, "err", err)
	http.Error(w, http.StatusText(status), status)
}

// failUpstream maps upstream errors to client responses: a not-found becomes a
// 404, everything else a 502 Bad Gateway.
func (h *Handler) failUpstream(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, ErrNotFound) {
		h.fail(w, r, http.StatusNotFound, err)
		return
	}
	h.fail(w, r, http.StatusBadGateway, err)
}
