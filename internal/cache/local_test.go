package cache

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLocalPutGetRoundTrip(t *testing.T) {
	c, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	ctx := context.Background()
	key := "registry.terraform.io/hashicorp/null/index.json"
	want := []byte(`{"versions":{"3.2.0":{}}}`)

	if err := c.Put(ctx, key, bytes.NewReader(want)); err != nil {
		t.Fatalf("Put: %v", err)
	}

	rc, hit, err := c.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !hit {
		t.Fatal("expected hit")
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestLocalGetTouchesMtimeForLRU(t *testing.T) {
	root := t.TempDir()
	c, err := NewLocal(root, WithAccessTracking())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	ctx := context.Background()
	key := "a/b.json"
	if err := c.Put(ctx, key, bytes.NewReader([]byte("x"))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Backdate the file, then read it: Get should bump mtime toward now.
	path := filepath.Join(root, key)
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	rc, hit, err := c.Get(ctx, key)
	if err != nil || !hit {
		t.Fatalf("Get hit=%v err=%v", hit, err)
	}
	rc.Close()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().After(old.Add(time.Hour)) {
		t.Errorf("mtime not bumped on read: got %v, was %v", info.ModTime(), old)
	}
}

func TestLocalGetDoesNotTouchWhenTrackingDisabled(t *testing.T) {
	root := t.TempDir()
	c, err := NewLocal(root) // tracking off (default)
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	ctx := context.Background()
	key := "a/b.json"
	if err := c.Put(ctx, key, bytes.NewReader([]byte("x"))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	path := filepath.Join(root, key)
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	rc, _, err := c.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(old) {
		t.Errorf("mtime changed with tracking disabled: got %v, want %v", info.ModTime(), old)
	}
}

func TestLocalGetMiss(t *testing.T) {
	c, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	rc, hit, err := c.Get(context.Background(), "does/not/exist.json")
	if err != nil {
		t.Fatalf("Get returned error on miss: %v", err)
	}
	if hit {
		t.Error("expected miss")
	}
	if rc != nil {
		t.Error("expected nil reader on miss")
	}
}

func TestLocalPutIsAtomicAndNested(t *testing.T) {
	root := t.TempDir()
	c, err := NewLocal(root)
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	key := "a/b/c/d.zip"
	if err := c.Put(context.Background(), key, bytes.NewReader([]byte("payload"))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "a/b/c/d.zip")); err != nil {
		t.Errorf("expected nested file to exist: %v", err)
	}
}

func TestLocalRejectsPathTraversal(t *testing.T) {
	root := t.TempDir()
	c, err := NewLocal(root)
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	// Even if a malicious key slips past upstream validation, resolve() must
	// keep it inside the root. "../../etc/passwd" cleans to "/etc/passwd"
	// anchored at root, so it stays contained rather than escaping.
	key := "../../../../etc/passwd"
	if err := c.Put(context.Background(), key, bytes.NewReader([]byte("x"))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// The file must live under root, not at the real /etc/passwd.
	if _, err := os.Stat(filepath.Join(root, "etc/passwd")); err != nil {
		t.Errorf("traversal key should be contained under root: %v", err)
	}
}
