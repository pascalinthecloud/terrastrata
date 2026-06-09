package cache

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// memCache is an in-memory Cache used as a stand-in for the durable layer.
type memCache struct {
	mu   sync.Mutex
	data map[string][]byte
}

func newMemCache() *memCache { return &memCache{data: map[string][]byte{}} }

func (m *memCache) Get(_ context.Context, key string) (io.ReadCloser, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.data[key]
	if !ok {
		return nil, false, nil
	}
	return io.NopCloser(bytes.NewReader(b)), true, nil
}

func (m *memCache) Put(_ context.Context, key string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]byte, len(data))
	copy(cp, data)
	m.data[key] = cp
	return nil
}

func (m *memCache) has(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.data[key]
	return ok
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestLayeredLocalHitSkipsDurable(t *testing.T) {
	local := newMemCache()
	durable := newMemCache()
	l := NewLayered(local, durable, discardLogger())
	ctx := context.Background()

	_ = local.Put(ctx, "k", []byte("local-value"))

	rc, hit, err := l.Get(ctx, "k")
	if err != nil || !hit {
		t.Fatalf("Get hit=%v err=%v", hit, err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "local-value" {
		t.Errorf("got %q, want local-value", got)
	}
}

func TestLayeredDurableHitWarmsLocal(t *testing.T) {
	local := newMemCache()
	durable := newMemCache()
	l := NewLayered(local, durable, discardLogger())
	ctx := context.Background()

	_ = durable.Put(ctx, "k", []byte("durable-value"))

	rc, hit, err := l.Get(ctx, "k")
	if err != nil || !hit {
		t.Fatalf("Get hit=%v err=%v", hit, err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "durable-value" {
		t.Errorf("got %q, want durable-value", got)
	}
	// Warming is synchronous within Get, so local must now hold the value.
	if !local.has("k") {
		t.Error("expected local layer to be warmed after durable hit")
	}
}

func TestLayeredMiss(t *testing.T) {
	l := NewLayered(newMemCache(), newMemCache(), discardLogger())
	_, hit, err := l.Get(context.Background(), "absent")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if hit {
		t.Error("expected miss")
	}
}

func TestLayeredPutWritesBothLayers(t *testing.T) {
	local := newMemCache()
	durable := newMemCache()
	l := NewLayered(local, durable, discardLogger())

	done := make(chan error, 1)
	l.onDurablePut = func(_ string, err error) { done <- err }

	if err := l.Put(context.Background(), "k", []byte("v")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Local is synchronous.
	if !local.has("k") {
		t.Error("expected local layer written synchronously")
	}
	// Durable is asynchronous; wait for the hook.
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("durable put: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for async durable put")
	}
	if !durable.has("k") {
		t.Error("expected durable layer written asynchronously")
	}
}

func TestLayeredPutWithoutDurable(t *testing.T) {
	local := newMemCache()
	l := NewLayered(local, nil, discardLogger())
	if err := l.Put(context.Background(), "k", []byte("v")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !local.has("k") {
		t.Error("expected local write")
	}
	// And Get should still work with no durable layer.
	_, hit, err := l.Get(context.Background(), "k")
	if err != nil || !hit {
		t.Fatalf("Get hit=%v err=%v", hit, err)
	}
}
