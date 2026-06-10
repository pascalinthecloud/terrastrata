package cache

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// DefaultEvictInterval is how often the evictor sweeps the cache.
	DefaultEvictInterval = 5 * time.Minute

	// evictLowWaterFraction is the fraction of the budget we evict down to once
	// the cache exceeds it. The gap (hysteresis) avoids evicting on every sweep.
	evictLowWaterFraction = 0.90
)

// EvictorMetrics records eviction observability. Pass NopEvictorMetrics{} to
// disable.
type EvictorMetrics interface {
	// CacheSize reports the total bytes on disk after a sweep.
	CacheSize(bytes int64)
	// Evicted reports files and bytes removed by a sweep.
	Evicted(files int, bytes int64)
}

// NopEvictorMetrics is a no-op EvictorMetrics.
type NopEvictorMetrics struct{}

// CacheSize implements EvictorMetrics and does nothing.
func (NopEvictorMetrics) CacheSize(int64) {}

// Evicted implements EvictorMetrics and does nothing.
func (NopEvictorMetrics) Evicted(int, int64) {}

// Evictor bounds the local filesystem cache to a byte budget by deleting
// least-recently-used files (by mtime, which Local touches on every read). It is
// safe to run alongside reads and writes: deleting a file a reader already
// opened is harmless on Unix, and a deleted-then-requested object is simply
// re-fetched as a miss.
type Evictor struct {
	root     string
	maxBytes int64
	interval time.Duration
	skipDir  string // staging dir, excluded from the cache size
	metrics  EvictorMetrics
	log      *slog.Logger
}

// NewEvictor returns an Evictor for the cache rooted at root with the given byte
// budget. A non-positive maxBytes disables eviction.
func NewEvictor(root string, maxBytes int64, metrics EvictorMetrics, log *slog.Logger) *Evictor {
	if metrics == nil {
		metrics = NopEvictorMetrics{}
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		abs = root
	}
	return &Evictor{
		root:     abs,
		maxBytes: maxBytes,
		interval: DefaultEvictInterval,
		skipDir:  filepath.Join(abs, ".staging"),
		metrics:  metrics,
		log:      log,
	}
}

// Run sweeps once immediately, then every interval until ctx is cancelled.
func (e *Evictor) Run(ctx context.Context) {
	e.log.Info("cache evictor started", "max_bytes", e.maxBytes, "interval", e.interval)
	e.sweep()
	t := time.NewTicker(e.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.sweep()
		}
	}
}

type fileEntry struct {
	path string
	size int64
	mod  time.Time
}

// sweep measures the cache, reports its size, and — if over budget — evicts the
// least-recently-used files down to the low-water mark.
//
// The common case (under budget) only sums file sizes; the more expensive
// collection of a sorted file list happens only when an eviction is actually
// needed.
func (e *Evictor) sweep() {
	total, err := e.totalSize()
	if err != nil {
		e.log.Warn("cache evictor scan failed", "err", err)
		return
	}
	e.metrics.CacheSize(total)
	if e.maxBytes <= 0 || total <= e.maxBytes {
		return
	}

	files, total, err := e.collectFiles()
	if err != nil {
		e.log.Warn("cache evictor scan failed", "err", err)
		return
	}

	target := int64(float64(e.maxBytes) * evictLowWaterFraction)
	// Oldest mtime first => least recently used first.
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })

	var (
		evictedFiles int
		evictedBytes int64
	)
	for _, f := range files {
		if total <= target {
			break
		}
		if err := os.Remove(f.path); err != nil {
			e.log.Warn("cache evict failed", "path", f.path, "err", err)
			continue
		}
		total -= f.size
		evictedBytes += f.size
		evictedFiles++
	}

	if evictedFiles > 0 {
		e.metrics.Evicted(evictedFiles, evictedBytes)
		e.metrics.CacheSize(total)
		e.log.Info("cache eviction", "files", evictedFiles, "bytes", evictedBytes, "remaining_bytes", total)
	}
}

// walkFiles invokes visit for every cached file under the root, excluding the
// staging dir and in-progress temp files.
func (e *Evictor) walkFiles(visit func(path string, info fs.FileInfo)) error {
	return filepath.WalkDir(e.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == e.skipDir {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(d.Name(), ".tmp-") {
			return nil // an atomic Put in progress
		}
		info, err := d.Info()
		if err != nil {
			return nil // raced with a delete; skip
		}
		visit(path, info)
		return nil
	})
}

// totalSize sums the size of all cached files. This is the cheap common-path
// measurement: it allocates nothing per file.
func (e *Evictor) totalSize() (int64, error) {
	var total int64
	err := e.walkFiles(func(_ string, info fs.FileInfo) {
		total += info.Size()
	})
	return total, err
}

// collectFiles returns every cached file with its size and mtime, plus the
// total. Only called when an eviction is actually needed.
func (e *Evictor) collectFiles() ([]fileEntry, int64, error) {
	var (
		files []fileEntry
		total int64
	)
	err := e.walkFiles(func(path string, info fs.FileInfo) {
		files = append(files, fileEntry{path: path, size: info.Size(), mod: info.ModTime()})
		total += info.Size()
	})
	return files, total, err
}
