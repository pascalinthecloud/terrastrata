// Package prewarm seeds the cache at startup by replaying mirror requests
// against terrastrata's own handler in-process. Replaying through the real
// handler reuses all of its logic — validation, caching, checksum verification —
// with no duplication, and keeps pre-warming entirely best-effort: failures are
// logged, never fatal.
package prewarm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"sync"

	"github.com/pascalinthecloud/terrastrata/internal/mirror"
)

// defaultHostname is assumed when a provider entry omits the registry host.
const defaultHostname = "registry.terraform.io"

// maxConcurrency bounds how many providers are warmed in parallel.
const maxConcurrency = 4

// Entry is a parsed provider to warm. Version is optional: when empty only the
// versions index is warmed; when set, that version's archives index and zips
// (for the configured platforms) are warmed too.
type Entry struct {
	Hostname  string
	Namespace string
	Type      string
	Version   string
}

// parseEntry parses "[host/]namespace/type[@version]".
func parseEntry(raw string) (Entry, error) {
	pathPart, version, _ := strings.Cut(raw, "@")
	segs := strings.Split(pathPart, "/")
	var e Entry
	switch len(segs) {
	case 2:
		e = Entry{Hostname: defaultHostname, Namespace: segs[0], Type: segs[1]}
	case 3:
		e = Entry{Hostname: segs[0], Namespace: segs[1], Type: segs[2]}
	default:
		return Entry{}, fmt.Errorf("prewarm: invalid provider %q (want [host/]namespace/type[@version])", raw)
	}
	e.Version = version
	if e.Namespace == "" || e.Type == "" {
		return Entry{}, fmt.Errorf("prewarm: invalid provider %q", raw)
	}
	return e, nil
}

// Run warms every configured provider through handler, concurrently and
// best-effort. It returns when all warming is done or ctx is cancelled.
func Run(ctx context.Context, handler http.Handler, providers, platforms []string, log *slog.Logger) {
	log.Info("prewarm starting", "providers", len(providers), "platforms", platforms)

	sem := make(chan struct{}, maxConcurrency)
	var wg sync.WaitGroup
	for _, raw := range providers {
		entry, err := parseEntry(raw)
		if err != nil {
			log.Warn("prewarm skip", "entry", raw, "err", err)
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(e Entry) {
			defer wg.Done()
			defer func() { <-sem }()
			warmProvider(ctx, handler, e, platforms, log)
		}(entry)
	}
	wg.Wait()
	log.Info("prewarm complete")
}

func warmProvider(ctx context.Context, handler http.Handler, e Entry, platforms []string, log *slog.Logger) {
	base := "/" + e.Hostname + "/" + e.Namespace + "/" + e.Type

	if status := warm(ctx, handler, base+"/index.json"); status != http.StatusOK {
		log.Warn("prewarm versions failed", "provider", e.Namespace+"/"+e.Type, "status", status)
		return
	}
	if e.Version == "" {
		return
	}

	archivesPath := base + "/" + e.Version + ".json"
	status, body := fetch(ctx, handler, archivesPath)
	if status != http.StatusOK {
		log.Warn("prewarm archives failed", "provider", e.Namespace+"/"+e.Type, "version", e.Version, "status", status)
		return
	}
	var idx mirror.ArchivesIndex
	if err := json.Unmarshal(body, &idx); err != nil {
		log.Warn("prewarm archives decode failed", "version", e.Version, "err", err)
		return
	}

	// Archive URLs are relative to the version JSON document; resolve against its
	// directory and warm only the requested platforms.
	dir := path.Dir(archivesPath)
	for _, plat := range platforms {
		arch, ok := idx.Archives[plat]
		if !ok {
			continue
		}
		zipPath := dir + "/" + arch.URL
		if status := warm(ctx, handler, zipPath); status != http.StatusOK {
			log.Warn("prewarm zip failed", "version", e.Version, "platform", plat, "status", status)
			continue
		}
		log.Debug("prewarmed zip", "version", e.Version, "platform", plat)
	}
}

// warm issues an in-process GET and discards the body (so large zips are never
// buffered here — the handler still streams them to the cache). Returns the
// status code.
func warm(ctx context.Context, handler http.Handler, urlPath string) int {
	rw := &respWriter{out: io.Discard}
	handler.ServeHTTP(rw, newRequest(ctx, urlPath))
	return rw.statusOrOK()
}

// fetch issues an in-process GET and captures the (small) JSON body.
func fetch(ctx context.Context, handler http.Handler, urlPath string) (int, []byte) {
	var buf strings.Builder
	rw := &respWriter{out: &buf}
	handler.ServeHTTP(rw, newRequest(ctx, urlPath))
	return rw.statusOrOK(), []byte(buf.String())
}

func newRequest(ctx context.Context, urlPath string) *http.Request {
	// The host is irrelevant to the mux; only the path drives routing.
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://prewarm"+urlPath, nil)
	return req
}

// respWriter is a minimal http.ResponseWriter that records the status code and
// forwards the body to a configurable writer (a buffer or io.Discard).
type respWriter struct {
	header http.Header
	status int
	out    io.Writer
}

func (w *respWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *respWriter) WriteHeader(status int) { w.status = status }

func (w *respWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.out.Write(b)
}

// statusOrOK reports the recorded status, defaulting to 200 when the handler
// wrote a body without an explicit WriteHeader.
func (w *respWriter) statusOrOK() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}
