// Package mirror implements the Terraform provider network mirror protocol as a
// pull-through cache over an upstream provider registry. It validates request
// coordinates, translates registry responses into mirror responses, and serves
// provider archives from the cache, fetching from upstream on a miss.
package mirror

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// maxZipBytes caps a single provider archive we will stage and cache. Real
// provider zips are tens of MB; this guards against a hostile/broken upstream
// filling the disk. A stream exceeding it is rejected, never truncated.
const maxZipBytes = 1 << 30 // 1 GiB

// Metrics records cache outcomes. The server supplies a Prometheus-backed
// implementation; tests and bare setups can use NopMetrics.
type Metrics interface {
	// CacheLookup records whether a lookup for the given resource kind
	// ("versions", "archives", "zip") hit the cache.
	CacheLookup(resource string, hit bool)
	// VersionsIndexOutcome records how a versions-index request was satisfied:
	// "fresh" (served within TTL), "revalidated" (refetched from upstream),
	// "stale" (served last-known-good after an upstream failure), or "error"
	// (upstream failed with no cached copy to fall back on).
	VersionsIndexOutcome(outcome string)
}

// Versions-index outcome labels.
const (
	outcomeFresh       = "fresh"
	outcomeRevalidated = "revalidated"
	outcomeStale       = "stale"
	outcomeError       = "error"
)

// NopMetrics is a no-op Metrics.
type NopMetrics struct{}

// CacheLookup implements Metrics and does nothing.
func (NopMetrics) CacheLookup(string, bool) {}

// VersionsIndexOutcome implements Metrics and does nothing.
func (NopMetrics) VersionsIndexOutcome(string) {}

// Handler serves the Terraform provider network mirror protocol, backed by a
// pull-through cache over an upstream provider registry.
type Handler struct {
	cache    Cache
	upstream *Upstream
	metrics  Metrics
	log      *slog.Logger

	// stagingDir is a writable directory for staging provider zips while their
	// checksum is verified. It must be on a writable volume (the container root
	// filesystem is read-only), so it lives under the cache directory.
	stagingDir string

	// indexTTL is how long a cached versions index is served before it is
	// revalidated against upstream. Zero disables expiry (cache forever).
	indexTTL time.Duration

	// now returns the current time; overridable in tests for deterministic TTL.
	now func() time.Time
}

// Cache is the subset of internal/cache used by the handler, restated here to
// avoid a package dependency cycle and keep the handler unit-testable.
type Cache interface {
	Get(ctx context.Context, key string) (io.ReadCloser, bool, error)
	Put(ctx context.Context, key string, r io.Reader) error
}

// Options configures a Handler. Cache, Upstream and Logger are required.
type Options struct {
	Cache    Cache
	Upstream *Upstream
	Metrics  Metrics // defaults to NopMetrics{} when nil
	// StagingDir is a writable directory for verifying zips; created if absent.
	StagingDir string
	// IndexTTL is the versions-index freshness window; zero disables expiry.
	IndexTTL time.Duration
	Logger   *slog.Logger
}

// NewHandler builds a mirror Handler from Options, creating the staging
// directory if needed.
func NewHandler(opts Options) (*Handler, error) {
	if opts.Metrics == nil {
		opts.Metrics = NopMetrics{}
	}
	if err := os.MkdirAll(opts.StagingDir, 0o750); err != nil {
		return nil, fmt.Errorf("mirror: create staging dir: %w", err)
	}
	return &Handler{
		cache:      opts.Cache,
		upstream:   opts.Upstream,
		metrics:    opts.Metrics,
		stagingDir: opts.StagingDir,
		indexTTL:   opts.IndexTTL,
		now:        time.Now,
		log:        opts.Logger,
	}, nil
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

// handleVersions serves the versions index with TTL-based revalidation. Unlike
// archives and zips (immutable per version), the versions list grows over time,
// so a cached copy older than indexTTL is revalidated against upstream. If
// upstream is unreachable during revalidation, the last-known-good copy is
// served stale — the mirror's whole point is to survive registry outages.
func (h *Handler) handleVersions(w http.ResponseWriter, r *http.Request) {
	c, err := ValidateProvider(r.PathValue("hostname"), r.PathValue("namespace"), r.PathValue("type"))
	if err != nil {
		h.fail(w, r, http.StatusBadRequest, err)
		return
	}
	key := VersionsCacheKey(c)

	cachedBody, fetchedAt, cacheHit := h.loadVersions(r.Context(), key)
	if cacheHit && h.versionsFresh(fetchedAt) {
		h.metrics.CacheLookup("versions", true)
		h.metrics.VersionsIndexOutcome(outcomeFresh)
		writeBody(w, "application/json", "HIT", cachedBody)
		return
	}

	// Stale or absent: (re)validate against upstream.
	body, err := h.fetchVersions(r.Context(), c)
	if err != nil {
		// Serve a stale-but-present copy on a transient upstream failure; a
		// definitive 404 (provider removed) is passed through instead.
		if !errors.Is(err, ErrNotFound) && cacheHit && len(cachedBody) > 0 {
			h.metrics.CacheLookup("versions", true)
			h.metrics.VersionsIndexOutcome(outcomeStale)
			h.log.Warn("serving stale versions index after upstream failure", "key", key, "err", err)
			writeBody(w, "application/json", "STALE", cachedBody)
			return
		}
		h.metrics.CacheLookup("versions", false)
		h.metrics.VersionsIndexOutcome(outcomeError)
		h.failUpstream(w, r, err)
		return
	}

	h.metrics.CacheLookup("versions", false)
	h.metrics.VersionsIndexOutcome(outcomeRevalidated)
	h.storeVersions(r.Context(), key, body)
	writeBody(w, "application/json", "MISS", body)
}

// versionsFresh reports whether a versions index fetched at fetchedAt is still
// within the TTL. A non-positive TTL disables expiry (always fresh).
func (h *Handler) versionsFresh(fetchedAt time.Time) bool {
	if h.indexTTL <= 0 {
		return true
	}
	return h.now().Sub(fetchedAt) < h.indexTTL
}

// loadVersions reads and unwraps a cached versions envelope. Any cache or
// decode error is treated as a miss (the cache is never a hard dependency).
func (h *Handler) loadVersions(ctx context.Context, key string) (body []byte, fetchedAt time.Time, hit bool) {
	rc, found, err := h.cache.Get(ctx, key)
	if err != nil {
		h.log.Warn("cache read failed", "key", key, "err", err)
		return nil, time.Time{}, false
	}
	if !found {
		return nil, time.Time{}, false
	}
	defer func() { _ = rc.Close() }()

	raw, err := io.ReadAll(rc)
	if err != nil {
		h.log.Warn("cache read failed", "key", key, "err", err)
		return nil, time.Time{}, false
	}
	return unwrapVersions(raw)
}

// fetchVersions retrieves and builds the mirror versions index from upstream.
func (h *Handler) fetchVersions(ctx context.Context, c Coordinates) ([]byte, error) {
	versions, err := h.upstream.ListVersions(ctx, c)
	if err != nil {
		return nil, err
	}
	return json.Marshal(BuildVersionsIndex(versions))
}

// storeVersions caches the versions body wrapped in a freshness envelope.
func (h *Handler) storeVersions(ctx context.Context, key string, body []byte) {
	enveloped, err := wrapVersions(body, h.now())
	if err != nil {
		h.log.Warn("versions envelope marshal failed", "key", key, "err", err)
		return
	}
	h.store(ctx, key, enveloped)
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
	writeBody(w, "application/json", "MISS", body)
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

	osName, arch, _ := strings.Cut(c.Platform, "_")
	meta, err := h.upstream.GetDownload(r.Context(), c, osName, arch)
	if err != nil {
		h.failUpstream(w, r, err)
		return
	}
	if meta.Filename != c.Filename {
		h.fail(w, r, http.StatusNotFound, errors.New("mirror: requested filename does not match upstream"))
		return
	}
	// Refuse to mirror an archive the registry won't vouch for: without a
	// published checksum we cannot guarantee integrity, so we must not cache it.
	if meta.Shasum == "" {
		h.fail(w, r, http.StatusBadGateway, errors.New("mirror: upstream provided no checksum"))
		return
	}

	rc, err := h.upstream.FetchZip(r.Context(), meta.DownloadURL)
	if err != nil {
		h.failUpstream(w, r, err)
		return
	}
	defer func() { _ = rc.Close() }()

	// Stream the archive to a staging file (never into memory), verifying both
	// the size cap and the checksum before it is cached or served.
	staged, size, err := h.stageVerifiedZip(rc, meta.Shasum)
	if err != nil {
		h.failUpstream(w, r, err)
		return
	}
	defer func() {
		_ = staged.Close()
		_ = os.Remove(staged.Name())
	}()

	// Persist (best-effort) then serve, both straight from the staged file.
	if _, err := staged.Seek(0, io.SeekStart); err == nil {
		if perr := h.cache.Put(r.Context(), key, staged); perr != nil {
			h.log.Warn("cache write failed", "key", key, "err", perr)
		}
	}
	if _, err := staged.Seek(0, io.SeekStart); err != nil {
		h.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("X-Cache", "MISS")
	if _, err := io.Copy(w, staged); err != nil {
		h.log.Warn("write zip response failed", "key", key, "err", err)
	}
}

// stageVerifiedZip streams r to a temp file under the staging dir while computing
// its SHA-256 and enforcing the size cap. It returns the file (seeked to start,
// caller closes and removes) only if the stream is within the cap and matches
// wantSha. Nothing is held in memory beyond a small copy buffer.
func (h *Handler) stageVerifiedZip(r io.Reader, wantSha string) (*os.File, int64, error) {
	f, err := os.CreateTemp(h.stagingDir, "zip-*")
	if err != nil {
		return nil, 0, fmt.Errorf("mirror: stage temp: %w", err)
	}
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}

	hasher := sha256.New()
	// Read one byte past the cap so we can distinguish "exactly at cap" from
	// "too large" rather than silently truncating.
	limited := io.LimitReader(r, maxZipBytes+1)
	size, err := io.Copy(io.MultiWriter(f, hasher), limited)
	if err != nil {
		cleanup()
		return nil, 0, fmt.Errorf("mirror: stage zip: %w", err)
	}
	if size > maxZipBytes {
		cleanup()
		return nil, 0, fmt.Errorf("mirror: upstream zip exceeds %d byte limit", int64(maxZipBytes))
	}
	if got := hex.EncodeToString(hasher.Sum(nil)); got != wantSha {
		cleanup()
		return nil, 0, errors.New("mirror: upstream zip checksum mismatch")
	}
	return f, size, nil
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
	defer func() { _ = rc.Close() }()

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Cache", "HIT")
	if _, err := io.Copy(w, rc); err != nil {
		h.log.Warn("write cached response failed", "key", key, "err", err)
	}
	return true
}

// store writes a small in-memory body (a JSON index) to the cache, logging (but
// not failing the request) on error.
func (h *Handler) store(ctx context.Context, key string, data []byte) {
	if err := h.cache.Put(ctx, key, bytes.NewReader(data)); err != nil {
		h.log.Warn("cache write failed", "key", key, "err", err)
	}
}

// writeBody writes an in-memory response with a content type and an explicit
// X-Cache status (HIT, MISS, or STALE).
func writeBody(w http.ResponseWriter, contentType, cacheStatus string, body []byte) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Cache", cacheStatus)
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
