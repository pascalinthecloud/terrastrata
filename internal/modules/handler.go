// Package modules implements the Terraform module registry protocol as a
// pull-through cache over an upstream module registry.
//
// Terraform has no module *mirror* protocol — only the registry protocol — so
// terrastrata acts as a caching registry rather than a mirror: consumers address
// it directly (source = "tf-mirror.internal/namespace/name/system") and it
// serves the same protocol it consumes upstream.
//
// The one translation it performs is on the download endpoint's X-Terraform-Get
// header, which is rewritten to point at terrastrata's own archive endpoint so
// the module archive is fetched, cached, and served here instead of from
// wherever the upstream registry points (typically a GitHub tarball).
package modules

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/pascalinthecloud/terrastrata/internal/freshness"
)

// maxArchiveBytes caps a single module archive we will stage and cache. Real
// module tarballs are well under a megabyte; this only guards against a
// hostile or broken upstream filling the disk. A stream exceeding it is
// rejected, never truncated.
//
// Note this cap is the *only* size defense here. Unlike provider zips, the
// module registry protocol publishes no checksum, so a module archive cannot be
// verified against upstream-published bytes the way mirror.stageVerifiedZip
// verifies a provider zip. Integrity rests on the https-only fetch.
const maxArchiveBytes = 512 << 20 // 512 MiB

// Download outcome labels for the module download metric.
const (
	outcomeCached = "cached"
	outcomeBypass = "bypass"
	outcomeError  = "error"
)

// Metrics records module cache and download outcomes. The server supplies a
// Prometheus-backed implementation; tests can use NopMetrics.
type Metrics interface {
	// CacheLookup records whether a lookup for the given resource kind
	// ("module_versions", "module_location", "module_archive") hit the cache.
	CacheLookup(resource string, hit bool)
	// ModuleDownload records how a download request was resolved: "cached" (the
	// archive is re-hosted by terrastrata), "bypass" (a source we cannot cache,
	// passed through verbatim), or "error".
	ModuleDownload(outcome string)
}

// NopMetrics is a no-op Metrics.
type NopMetrics struct{}

// CacheLookup implements Metrics and does nothing.
func (NopMetrics) CacheLookup(string, bool) {}

// ModuleDownload implements Metrics and does nothing.
func (NopMetrics) ModuleDownload(string) {}

// Cache is the subset of internal/cache used by the handler, restated here to
// avoid a package dependency cycle and keep the handler unit-testable.
type Cache interface {
	Get(ctx context.Context, key string) (io.ReadCloser, bool, error)
	Put(ctx context.Context, key string, r io.Reader) error
}

// Handler serves the module registry protocol backed by a pull-through cache.
type Handler struct {
	cache    Cache
	upstream *Upstream
	metrics  Metrics
	log      *slog.Logger

	// stagingDir is a writable directory for staging archives before they are
	// cached. It must be on a writable volume (the container root filesystem is
	// read-only), so it lives under the cache directory.
	stagingDir string

	// versionsTTL is how long a cached versions document is served before it is
	// revalidated against upstream. Zero disables expiry.
	versionsTTL time.Duration

	// maxArchive caps a staged archive. Defaults to maxArchiveBytes; tests
	// lower it so the cap can be exercised without moving half a gigabyte.
	maxArchive int64

	// signer signs and verifies archive URLs when auth is enabled. Nil when no
	// AUTH_TOKEN is configured, leaving the archive endpoint open (the default
	// internal-network mode). See sign.go for why the URL rather than a header
	// carries the authorization.
	signer *signer

	// now returns the current time; overridable in tests for deterministic TTL.
	now func() time.Time

	// group coalesces concurrent cold requests for the same cache key.
	group singleflight.Group
}

// Options configures a Handler. Cache, Upstream and Logger are required.
type Options struct {
	Cache      Cache
	Upstream   *Upstream
	Metrics    Metrics // defaults to NopMetrics{} when nil
	StagingDir string
	// VersionsTTL is the versions-document freshness window; zero disables expiry.
	VersionsTTL time.Duration
	// AuthToken is the bearer token guarding the registry endpoints. When it is
	// set, archive URLs minted by the download endpoint are signed with it and
	// the archive endpoint rejects unsigned requests — the archive route cannot
	// use the bearer header itself (see ArchivePattern and sign.go). Empty
	// leaves the archive endpoint unauthenticated, matching auth being off.
	AuthToken string
	Logger    *slog.Logger
}

// NewHandler builds a module Handler, creating the staging directory if needed.
func NewHandler(opts Options) (*Handler, error) {
	if opts.Metrics == nil {
		opts.Metrics = NopMetrics{}
	}
	if opts.Upstream == nil {
		return nil, errors.New("modules: Options.Upstream is required")
	}
	if err := os.MkdirAll(opts.StagingDir, 0o750); err != nil {
		return nil, fmt.Errorf("modules: create staging dir: %w", err)
	}
	return &Handler{
		cache:       opts.Cache,
		upstream:    opts.Upstream,
		metrics:     opts.Metrics,
		stagingDir:  opts.StagingDir,
		versionsTTL: opts.VersionsTTL,
		maxArchive:  maxArchiveBytes,
		signer:      newSigner(opts.AuthToken),
		now:         time.Now,
		log:         opts.Logger,
	}, nil
}

// discoveryDoc is the service discovery document Terraform fetches from
// /.well-known/terraform.json to locate the module registry API. Only modules.v1
// is advertised: terrastrata is a module registry but a provider *mirror*, and
// advertising providers.v1 would invite clients to use it as a provider registry
// it does not implement.
var discoveryDoc = []byte(`{"modules.v1":"/v1/modules/"}` + "\n")

// Discovery serves the Terraform service discovery document. It must be
// reachable unauthenticated: it is the first request a client makes, before any
// credentials are looked up.
func (h *Handler) Discovery(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(discoveryDoc)
}

// RoutesMeta registers the module metadata endpoints (versions and download).
// They are registered separately from the archive endpoint because the archive
// must remain unauthenticated — see RouteArchive.
func (h *Handler) RoutesMeta(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/modules/{namespace}/{name}/{system}/versions", h.handleVersions)
	mux.HandleFunc("GET /v1/modules/{namespace}/{name}/{system}/{version}/download", h.handleDownload)
}

// ArchivePattern is the route pattern for the module archive endpoint.
//
// It is registered by the caller, on the root mux and *outside* any auth
// middleware, for two reasons. First, Terraform attaches registry credentials
// only to registry requests; the go-getter fetch of X-Terraform-Get that follows
// carries no Authorization header, so an authenticated archive endpoint would
// break terraform init whenever AUTH_TOKEN is set. Second, module patterns and
// the provider zip pattern overlap without either being more specific, so Go's
// ServeMux panics if both are registered on the same mux.
//
// Being outside the bearer middleware does not mean unauthorized: when
// AUTH_TOKEN is set, the handler requires the short-lived signature that the
// (authenticated) download endpoint puts in the URL. See sign.go.
const ArchivePattern = "GET /v1/modules/{namespace}/{name}/{system}/{version}/archive"

// coords validates the namespace/name/system triple from the request path.
func coords(r *http.Request) (Coordinates, error) {
	return Validate(r.PathValue("namespace"), r.PathValue("name"), r.PathValue("system"))
}

// versionCoords validates the full coordinate including the version.
func versionCoords(r *http.Request) (Coordinates, error) {
	c, err := coords(r)
	if err != nil {
		return Coordinates{}, err
	}
	return c.withVersion(r.PathValue("version"))
}

// handleVersions serves a module's version list with TTL-based revalidation. The
// list grows as new versions are published, so a cached copy older than the TTL
// is revalidated; if upstream is unreachable during revalidation the
// last-known-good copy is served stale, which is the whole point of a mirror.
func (h *Handler) handleVersions(w http.ResponseWriter, r *http.Request) {
	c, err := coords(r)
	if err != nil {
		h.fail(w, r, http.StatusBadRequest, err)
		return
	}
	key := VersionsCacheKey(c)

	cachedBody, fetchedAt, cacheHit := h.loadVersions(r.Context(), key)
	if cacheHit && h.versionsFresh(fetchedAt) {
		h.metrics.CacheLookup("module_versions", true)
		writeBody(w, "application/json", "HIT", cachedBody)
		return
	}

	dctx := context.WithoutCancel(r.Context())
	v, err := h.coalesce(r.Context(), key, func() (any, error) {
		body, ferr := h.upstream.ListVersions(dctx, c)
		if ferr != nil {
			return nil, ferr
		}
		h.storeVersions(dctx, key, body)
		return body, nil
	})
	if err != nil {
		// Serve a stale-but-present copy on a transient upstream failure; a
		// definitive 404 (module removed) is passed through instead.
		if !errors.Is(err, ErrNotFound) && cacheHit && len(cachedBody) > 0 {
			h.metrics.CacheLookup("module_versions", true)
			h.log.Warn("serving stale module versions after upstream failure", "key", key, "err", err)
			writeBody(w, "application/json", "STALE", cachedBody)
			return
		}
		h.metrics.CacheLookup("module_versions", false)
		h.failUpstream(w, r, err)
		return
	}

	h.metrics.CacheLookup("module_versions", false)
	writeBody(w, "application/json", "MISS", v.([]byte))
}

// versionsFresh reports whether a versions document fetched at fetchedAt is
// still within the TTL. A non-positive TTL disables expiry.
func (h *Handler) versionsFresh(fetchedAt time.Time) bool {
	if h.versionsTTL <= 0 {
		return true
	}
	return h.now().Sub(fetchedAt) < h.versionsTTL
}

// loadVersions reads and unwraps a cached versions envelope. Any cache or decode
// error is treated as a miss (the cache is never a hard dependency).
func (h *Handler) loadVersions(ctx context.Context, key string) (body []byte, fetchedAt time.Time, hit bool) {
	raw, ok := h.readCache(ctx, key)
	if !ok {
		return nil, time.Time{}, false
	}
	return freshness.Unwrap(raw)
}

// storeVersions caches the versions body wrapped in a freshness envelope.
func (h *Handler) storeVersions(ctx context.Context, key string, body []byte) {
	enveloped, err := freshness.Wrap(body, h.now())
	if err != nil {
		h.log.Warn("module versions envelope marshal failed", "key", key, "err", err)
		return
	}
	h.store(ctx, key, enveloped)
}

// location is the cached result of resolving a module version's source. Exactly
// one of Source or Bypass is set.
type location struct {
	// Source is the upstream archive URL terrastrata will fetch and re-host.
	Source Source `json:"source,omitempty"`
	// Bypass is an upstream X-Terraform-Get we cannot cache (git::, an unknown
	// archive type, …), passed through to the client verbatim.
	Bypass string `json:"bypass,omitempty"`
}

// handleDownload answers the module download endpoint with an X-Terraform-Get
// header, per the module registry protocol (204 No Content plus the header).
//
// It deliberately does not fetch the archive: the archive endpoint populates the
// cache lazily, so coalescing sits where the real load is and a client that only
// resolves metadata never triggers a download.
func (h *Handler) handleDownload(w http.ResponseWriter, r *http.Request) {
	c, err := versionCoords(r)
	if err != nil {
		h.fail(w, r, http.StatusBadRequest, err)
		return
	}

	loc, status, err := h.resolveLocation(r.Context(), c)
	if err != nil {
		h.metrics.ModuleDownload(outcomeError)
		h.failUpstream(w, r, err)
		return
	}

	get := loc.Bypass
	outcome := outcomeBypass
	if get == "" {
		get = h.rehostedGet(c, loc.Source)
		outcome = outcomeCached
	}
	h.metrics.ModuleDownload(outcome)

	w.Header().Set("X-Terraform-Get", get)
	w.Header().Set("X-Cache", status)
	w.WriteHeader(http.StatusNoContent)
}

// resolveLocation returns the cached or freshly resolved source location for c,
// along with the X-Cache status describing where it came from.
func (h *Handler) resolveLocation(ctx context.Context, c Coordinates) (location, string, error) {
	key := LocationCacheKey(c)

	if raw, ok := h.readCache(ctx, key); ok {
		var loc location
		if err := json.Unmarshal(raw, &loc); err == nil && (loc.Bypass != "" || loc.Source.URL != "") {
			h.metrics.CacheLookup("module_location", true)
			return loc, "HIT", nil
		}
		h.log.Warn("discarding malformed cached module location", "key", key)
	}
	h.metrics.CacheLookup("module_location", false)

	dctx := context.WithoutCancel(ctx)
	v, err := h.coalesce(ctx, key, func() (any, error) {
		raw, ferr := h.upstream.Location(dctx, c)
		if ferr != nil {
			return nil, ferr
		}

		var loc location
		if src, ok := ParseSource(raw, h.upstream.AllowHTTP()); ok {
			loc.Source = src
		} else {
			// Not something we can fetch and re-host (git::, s3::, an archive
			// type we cannot name). Hand the client the upstream source so
			// terraform init still works where it has direct network access,
			// rather than failing outright on a module we merely cannot cache.
			h.log.Warn("module source is not cacheable, passing through",
				"module", fmt.Sprintf("%s/%s/%s", c.Namespace, c.Name, c.System),
				"version", c.Version, "source", raw)
			loc.Bypass = raw
		}

		if encoded, merr := json.Marshal(loc); merr == nil {
			h.store(dctx, key, encoded)
		}
		return loc, nil
	})
	if err != nil {
		return location{}, "", err
	}
	return v.(location), "MISS", nil
}

// rehostedGet builds the X-Terraform-Get value pointing at terrastrata's own
// archive endpoint, signed when auth is enabled so the (unauthenticated) archive
// route still only serves clients that came through the authenticated download
// endpoint.
func (h *Handler) rehostedGet(c Coordinates, src Source) string {
	get := RehostedGet(c, src)
	if h.signer == nil {
		return get
	}
	return get + h.signer.query(c, h.now())
}

// handleArchive serves the module archive, fetching and caching it on a miss.
func (h *Handler) handleArchive(w http.ResponseWriter, r *http.Request) {
	c, err := versionCoords(r)
	if err != nil {
		h.fail(w, r, http.StatusBadRequest, err)
		return
	}
	// Authorize before anything else: an unsigned request must not read the
	// cache or reach upstream.
	if h.signer != nil {
		q := r.URL.Query()
		if err := h.signer.verify(c, q.Get(paramExpiry), q.Get(paramSignature), h.now()); err != nil {
			h.fail(w, r, http.StatusForbidden, err)
			return
		}
	}
	key := ArchiveCacheKey(c)

	if h.serveFromCache(w, r, key, "module_archive") {
		return
	}

	// Coalesce concurrent cold requests: the leader fetches and populates the
	// cache while the rest wait, collapsing a fleet of CI agents starting at
	// once into a single upstream download.
	dctx := context.WithoutCancel(r.Context())
	if _, err := h.coalesce(r.Context(), key, func() (any, error) {
		return nil, h.populateArchive(dctx, c, key)
	}); err != nil {
		h.failUpstream(w, r, err)
		return
	}

	if h.streamFromCache(w, r, key, "MISS") {
		return
	}
	// Only reached when the cache write failed (e.g. degraded disk): fetch and
	// stream directly so the request still succeeds.
	h.fetchAndServeArchive(w, r, c)
}

// ArchiveHandler returns the http.HandlerFunc for the archive endpoint, which
// the caller mounts per ArchivePattern.
func (h *Handler) ArchiveHandler() http.HandlerFunc { return h.handleArchive }

// archiveSource resolves where c's archive is fetched from. A bypassed location
// has no terrastrata-hosted archive, so requesting one is a 404: the client was
// handed the upstream URL and should never have come here.
func (h *Handler) archiveSource(ctx context.Context, c Coordinates) (Source, error) {
	loc, _, err := h.resolveLocation(ctx, c)
	if err != nil {
		return Source{}, err
	}
	if loc.Bypass != "" || loc.Source.URL == "" {
		return Source{}, fmt.Errorf("%w: module source is not hosted by this registry", ErrNotFound)
	}
	return loc.Source, nil
}

// populateArchive fetches, stages, and caches c's archive. Run inside the
// coalescing group so a burst of cold requests triggers exactly one download.
func (h *Handler) populateArchive(ctx context.Context, c Coordinates, key string) error {
	staged, _, err := h.fetchStageArchive(ctx, c)
	if err != nil {
		return err
	}
	defer h.discard(staged)

	if _, err := staged.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("modules: rewind staged archive: %w", err)
	}
	if err := h.cache.Put(ctx, key, staged); err != nil {
		h.log.Warn("cache write failed", "key", key, "err", err)
	}
	return nil
}

// fetchStageArchive streams c's archive to a temp file under the staging dir,
// enforcing the size cap. The caller closes and removes the file.
func (h *Handler) fetchStageArchive(ctx context.Context, c Coordinates) (*os.File, int64, error) {
	src, err := h.archiveSource(ctx, c)
	if err != nil {
		return nil, 0, err
	}
	rc, err := h.upstream.FetchArchive(ctx, src.URL)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rc.Close() }()

	f, err := os.CreateTemp(h.stagingDir, "module-*")
	if err != nil {
		return nil, 0, fmt.Errorf("modules: stage temp: %w", err)
	}
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}

	// Read one byte past the cap so "exactly at cap" is distinguishable from
	// "too large" rather than silently truncating. The counter measures the
	// bytes actually pulled from upstream, which for a repack differs from the
	// bytes written out.
	counted := &countingReader{r: io.LimitReader(rc, h.maxArchive+1)}

	var size int64
	if src.Repack {
		size, err = repackStripRoot(f, counted)
	} else {
		size, err = io.Copy(f, counted)
	}
	if err != nil {
		cleanup()
		return nil, 0, fmt.Errorf("modules: stage archive: %w", err)
	}
	if counted.n > h.maxArchive {
		cleanup()
		return nil, 0, fmt.Errorf("modules: upstream archive exceeds %d byte limit", h.maxArchive)
	}
	return f, size, nil
}

// fetchAndServeArchive fetches and streams the archive directly. It is the
// fallback when coalesced cache population could not produce a readable entry.
func (h *Handler) fetchAndServeArchive(w http.ResponseWriter, r *http.Request, c Coordinates) {
	staged, size, err := h.fetchStageArchive(r.Context(), c)
	if err != nil {
		h.failUpstream(w, r, err)
		return
	}
	defer h.discard(staged)

	if _, err := staged.Seek(0, io.SeekStart); err != nil {
		h.fail(w, r, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("X-Cache", "MISS")
	if _, err := io.Copy(w, staged); err != nil {
		h.log.Warn("write archive response failed", "err", err)
	}
}

// discard closes and removes a staged temp file.
func (h *Handler) discard(f *os.File) {
	_ = f.Close()
	//nolint:gosec // G703: name is from os.CreateTemp under our own staging dir
	_ = os.Remove(f.Name())
}

// coalesce runs fn at most once for a given key while a call is in flight,
// sharing the single result among all concurrent callers. fn runs under a
// detached context so one caller cancelling does not abort the fetch the others
// are waiting on; each caller still observes its own context below.
func (h *Handler) coalesce(ctx context.Context, key string, fn func() (any, error)) (any, error) {
	ch := h.group.DoChan(key, fn)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-ch:
		return res.Val, res.Err
	}
}

// readCache reads a small cached document whole. Any error is reported as a
// miss: the cache is never a hard dependency.
func (h *Handler) readCache(ctx context.Context, key string) ([]byte, bool) {
	rc, found, err := h.cache.Get(ctx, key)
	if err != nil {
		h.log.Warn("cache read failed", "key", key, "err", err)
		return nil, false
	}
	if !found {
		return nil, false
	}
	defer func() { _ = rc.Close() }()

	raw, err := io.ReadAll(rc)
	if err != nil {
		h.log.Warn("cache read failed", "key", key, "err", err)
		return nil, false
	}
	return raw, true
}

// serveFromCache writes a cache hit to the response and reports whether it did.
func (h *Handler) serveFromCache(w http.ResponseWriter, r *http.Request, key, resource string) bool {
	rc, hit, err := h.cache.Get(r.Context(), key)
	if err != nil {
		h.log.Warn("cache read failed", "key", key, "err", err)
		hit = false
	}
	h.metrics.CacheLookup(resource, hit)
	if !hit {
		if rc != nil {
			_ = rc.Close()
		}
		return false
	}
	defer func() { _ = rc.Close() }()
	h.writeStream(w, rc, "HIT", key)
	return true
}

// streamFromCache writes a cache entry with an explicit X-Cache status,
// reporting whether it found one. It records no lookup metric — it serves the
// post-population read of a miss already counted on arrival.
func (h *Handler) streamFromCache(w http.ResponseWriter, r *http.Request, key, status string) bool {
	rc, hit, err := h.cache.Get(r.Context(), key)
	if err != nil {
		h.log.Warn("cache read failed", "key", key, "err", err)
		return false
	}
	if !hit {
		return false
	}
	defer func() { _ = rc.Close() }()
	h.writeStream(w, rc, status, key)
	return true
}

// writeStream copies a cache stream to the response, advertising Content-Length
// when the stream can report it (the local layer hands back an *os.File).
func (h *Handler) writeStream(w http.ResponseWriter, rc io.Reader, status, key string) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Cache", status)
	if s, ok := rc.(interface{ Stat() (os.FileInfo, error) }); ok {
		if info, err := s.Stat(); err == nil {
			w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
		}
	}
	if _, err := io.Copy(w, rc); err != nil {
		h.log.Warn("write cached response failed", "key", key, "err", err)
	}
}

// store writes a small in-memory document to the cache, logging (but not
// failing the request) on error.
func (h *Handler) store(ctx context.Context, key string, data []byte) {
	if err := h.cache.Put(ctx, key, bytes.NewReader(data)); err != nil {
		h.log.Warn("cache write failed", "key", key, "err", err)
	}
}

// writeBody writes an in-memory response with a content type and X-Cache status.
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
