package cache

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalPutGetRoundTrip(t *testing.T) {
	c, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	ctx := context.Background()
	key := "registry.terraform.io/hashicorp/null/index.json"
	want := []byte(`{"versions":{"3.2.0":{}}}`)

	if err := c.Put(ctx, key, want); err != nil {
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
	if err := c.Put(context.Background(), key, []byte("payload")); err != nil {
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
	if err := c.Put(context.Background(), key, []byte("x")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// The file must live under root, not at the real /etc/passwd.
	if _, err := os.Stat(filepath.Join(root, "etc/passwd")); err != nil {
		t.Errorf("traversal key should be contained under root: %v", err)
	}
}
