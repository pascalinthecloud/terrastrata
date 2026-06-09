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
}

// NopMetrics is a no-op Metrics.
type NopMetrics struct{}

// CacheLookup implements Metrics and does nothing.
func (NopMetrics) CacheLookup(string, bool) {}

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
}

// Cache is the subset of internal/cache used by the handler, restated here to
// avoid a package dependency cycle and keep the handler unit-testable.
type Cache interface {
	Get(ctx context.Context, key string) (io.ReadCloser, bool, error)
	Put(ctx context.Context, key string, r io.Reader) error
}

// NewHandler builds a mirror Handler. metrics may be NopMetrics{}. stagingDir
// must be a writable directory (typically a subdir of the cache dir); it is
// created if absent.
func NewHandler(c Cache, u *Upstream, m Metrics, stagingDir string, log *slog.Logger) (*Handler, error) {
	if m == nil {
		m = NopMetrics{}
	}
	if err := os.MkdirAll(stagingDir, 0o750); err != nil {
		return nil, fmt.Errorf("mirror: create staging dir: %w", err)
	}
	return &Handler{cache: c, upstream: u, metrics: m, stagingDir: stagingDir, log: log}, nil
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
