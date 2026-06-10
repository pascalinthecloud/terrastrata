package cache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Local is a filesystem-backed Cache rooted at a single directory. It is the
// fast, primary cache layer (typically a Kubernetes PVC).
type Local struct {
	root        string
	trackAccess bool // touch mtime on read so the evictor can do LRU
}

// LocalOption configures a Local cache.
type LocalOption func(*Local)

// WithAccessTracking makes Get touch each file's mtime on read, so the size
// evictor can use it as an LRU signal. It costs one syscall per cache hit, so it
// is only worth enabling when eviction is active.
func WithAccessTracking() LocalOption {
	return func(l *Local) { l.trackAccess = true }
}

// NewLocal returns a Local cache rooted at dir, creating the directory if needed.
func NewLocal(dir string, opts ...LocalOption) (*Local, error) {
	if dir == "" {
		return nil, errors.New("cache: local root directory must not be empty")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("cache: create local root: %w", err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("cache: resolve local root: %w", err)
	}
	l := &Local{root: abs}
	for _, opt := range opts {
		opt(l)
	}
	return l, nil
}

// resolve maps a cache key to an absolute filesystem path, guaranteeing the
// result stays within the cache root. This is defense-in-depth: keys are already
// validated upstream, but a path-traversal bug must never escape the root.
func (l *Local) resolve(key string) (string, error) {
	clean := filepath.Clean("/" + filepath.FromSlash(key)) // anchor to root, drop ".."
	full := filepath.Join(l.root, clean)
	if full != l.root && !strings.HasPrefix(full, l.root+string(os.PathSeparator)) {
		return "", fmt.Errorf("cache: key %q escapes cache root", key)
	}
	return full, nil
}

// Get implements Cache. A missing file yields hit == false and a nil error.
func (l *Local) Get(_ context.Context, key string) (io.ReadCloser, bool, error) {
	path, err := l.resolve(key)
	if err != nil {
		return nil, false, err
	}
	// path is constrained to the cache root by resolve(); not user-arbitrary.
	f, err := os.Open(path) //nolint:gosec // G304: path contained within cache root
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("cache: open %q: %w", key, err)
	}
	// When eviction is active, touch the modification time so it tracks last
	// access: the size-based evictor uses mtime as an LRU signal (filesystem
	// atime is unreliable under the common noatime/relatime mounts). This is one
	// syscall per hit, so it is skipped entirely when eviction is off.
	// Best-effort; a failure never affects the read.
	if l.trackAccess {
		now := time.Now()
		_ = os.Chtimes(path, now, now)
	}
	return f, true, nil
}

// Put writes r atomically and durably: it streams to a temp file in the
// destination directory, fsyncs the file and its parent directory, then renames
// it into place. Readers never observe a partial file, and a committed object
// survives a node crash whole (so a truncated blob is never served as a HIT).
func (l *Local) Put(_ context.Context, key string, r io.Reader) error {
	path, err := l.resolve(key)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("cache: create dir for %q: %w", key, err)
	}

	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("cache: temp file for %q: %w", key, err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if we bail out before the rename succeeds.
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := io.Copy(tmp, r); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("cache: write %q: %w", key, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("cache: fsync %q: %w", key, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("cache: close temp for %q: %w", key, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("cache: commit %q: %w", key, err)
	}
	// fsync the directory so the rename itself is durable across a crash.
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("cache: fsync dir for %q: %w", key, err)
	}
	return nil
}

// syncDir flushes a directory entry change (the rename) to stable storage.
func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // G304: dir derives from the cache root
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}
