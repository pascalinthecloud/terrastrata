package cache

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeSized creates a file of n bytes at rel (under root) with the given mtime.
func writeSized(t *testing.T, root, rel string, n int, mod time.Time) string {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, n), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
	return path
}

type recEvictMetrics struct {
	lastSize     int64
	evictedFiles int
	evictedBytes int64
}

func (m *recEvictMetrics) CacheSize(b int64)      { m.lastSize = b }
func (m *recEvictMetrics) Evicted(f int, b int64) { m.evictedFiles += f; m.evictedBytes += b }

func TestEvictorRemovesLeastRecentlyUsed(t *testing.T) {
	root := t.TempDir()
	base := time.Now().Add(-time.Hour)
	// Five 100-byte files, oldest (f0) to newest (f4).
	paths := make([]string, 5)
	for i := range paths {
		paths[i] = writeSized(t, root, filepathName(i), 100, base.Add(time.Duration(i)*time.Minute))
	}

	// Budget 350 -> low-water target 315. Evict oldest until <= 315: drop f0,f1.
	m := &recEvictMetrics{}
	e := NewEvictor(root, 350, m, discardLogger())
	e.sweep()

	for _, gone := range paths[:2] {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("expected %s evicted (err=%v)", gone, err)
		}
	}
	for _, kept := range paths[2:] {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("expected %s retained: %v", kept, err)
		}
	}
	if m.evictedFiles != 2 || m.evictedBytes != 200 {
		t.Errorf("metrics evicted = %d files / %d bytes, want 2 / 200", m.evictedFiles, m.evictedBytes)
	}
	if m.lastSize != 300 {
		t.Errorf("final CacheSize = %d, want 300", m.lastSize)
	}
}

func TestEvictorDisabledKeepsEverything(t *testing.T) {
	root := t.TempDir()
	base := time.Now()
	for i := 0; i < 5; i++ {
		writeSized(t, root, filepathName(i), 100, base)
	}
	e := NewEvictor(root, 0, nil, discardLogger()) // 0 = disabled
	e.sweep()
	if n := countFiles(t, root); n != 5 {
		t.Errorf("disabled evictor removed files: %d remain, want 5", n)
	}
}

func countFiles(t *testing.T, root string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestEvictorExcludesStagingAndTemp(t *testing.T) {
	root := t.TempDir()
	old := time.Now().Add(-time.Hour)
	// Staging file and an in-progress temp file should be ignored entirely.
	staged := writeSized(t, root, ".staging/zip-123", 1000, old)
	tmp := writeSized(t, root, "a/.tmp-456", 1000, old)
	keep := writeSized(t, root, "a/index.json", 50, time.Now())

	m := &recEvictMetrics{}
	e := NewEvictor(root, 10, m, discardLogger()) // tiny budget
	e.sweep()

	// Only the real cache file counts toward size, and the staging/temp files
	// are never evicted.
	if _, err := os.Stat(staged); err != nil {
		t.Errorf("staging file must not be evicted: %v", err)
	}
	if _, err := os.Stat(tmp); err != nil {
		t.Errorf("temp file must not be evicted: %v", err)
	}
	_ = keep
	if m.lastSize > 50 {
		t.Errorf("cache size counted staging/temp: %d", m.lastSize)
	}
}

func TestEvictorRunStopsOnContextCancel(t *testing.T) {
	root := t.TempDir()
	e := NewEvictor(root, 1<<30, nil, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("evictor did not stop on context cancel")
	}
}

func filepathName(i int) string {
	return "registry.terraform.io/ns/type/file" + string(rune('0'+i)) + ".bin"
}
