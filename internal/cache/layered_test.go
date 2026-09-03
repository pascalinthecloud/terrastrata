package cache

import (
	"bytes"
	"context"
	"errors"
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

func (m *memCache) Put(_ context.Context, key string, r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = data
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

	_ = local.Put(ctx, "k", bytes.NewReader([]byte("local-value")))

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

	_ = durable.Put(ctx, "k", bytes.NewReader([]byte("durable-value")))

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

// brokenPutCache wraps a Cache so every Put fails after consuming the reader,
// simulating a degraded local layer (full disk, bad volume).
type brokenPutCache struct {
	Cache
	putErr error
}

func (b *brokenPutCache) Put(_ context.Context, _ string, r io.Reader) error {
	_, _ = io.Copy(io.Discard, r) // consume, like a real write would
	return b.putErr
}

// blindCache wraps a Cache so every Get misses, simulating a warm write whose
// read-back fails.
type blindCache struct{ Cache }

func (blindCache) Get(context.Context, string) (io.ReadCloser, bool, error) {
	return nil, false, nil
}

func TestLayeredDurableHitSurvivesWarmFailure(t *testing.T) {
	local := &brokenPutCache{Cache: newMemCache(), putErr: errors.New("disk full")}
	durable := newMemCache()
	l := NewLayered(local, durable, discardLogger())
	ctx := context.Background()

	_ = durable.Put(ctx, "k", bytes.NewReader([]byte("durable-value")))

	rc, hit, err := l.Get(ctx, "k")
	if err != nil || !hit {
		t.Fatalf("Get hit=%v err=%v, want durable hit despite warm failure", hit, err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "durable-value" {
		t.Errorf("got %q, want durable-value", got)
	}
}

func TestLayeredDurableHitSurvivesWarmReadBackFailure(t *testing.T) {
	local := blindCache{newMemCache()}
	durable := newMemCache()
	l := NewLayered(local, durable, discardLogger())
	ctx := context.Background()

	_ = durable.Put(ctx, "k", bytes.NewReader([]byte("durable-value")))

	rc, hit, err := l.Get(ctx, "k")
	if err != nil || !hit {
		t.Fatalf("Get hit=%v err=%v, want durable hit despite read-back failure", hit, err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "durable-value" {
		t.Errorf("got %q, want durable-value", got)
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

	if err := l.Put(context.Background(), "k", bytes.NewReader([]byte("v"))); err != nil {
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

func TestLayeredPutReportsDurableError(t *testing.T) {
	local := newMemCache()
	wantErr := errors.New("bucket gone")
	durable := &brokenPutCache{Cache: newMemCache(), putErr: wantErr}
	l := NewLayered(local, durable, discardLogger())

	done := make(chan error, 1)
	l.onDurablePut = func(_ string, err error) { done <- err }

	if err := l.Put(context.Background(), "k", bytes.NewReader([]byte("v"))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, wantErr) {
			t.Fatalf("onDurablePut err = %v, want %v", err, wantErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for async durable put")
	}
}

func TestLayeredPutWithoutDurable(t *testing.T) {
	local := newMemCache()
	l := NewLayered(local, nil, discardLogger())
	if err := l.Put(context.Background(), "k", bytes.NewReader([]byte("v"))); err != nil {
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

// memCache does not implement deleter, so a rejected object stays in the local
// layer unless the local layer can drop it. deletingMemCache is the stand-in for
// a local layer that can (the real one is *Local).
type deletingMemCache struct {
	*memCache
	deleted []string
}

func (d *deletingMemCache) Delete(_ context.Context, key string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.data, key)
	d.deleted = append(d.deleted, key)
	return nil
}

type countingIntegrityMetrics struct{ failures int }

func (c *countingIntegrityMetrics) IntegrityFailure() { c.failures++ }

func TestLayeredVerifiesDurableContent(t *testing.T) {
	local := newMemCache()
	durable := newMemCache()
	var seen []string
	c := NewLayered(local, durable, discardLogger(), WithDurableVerifier(
		func(_ context.Context, key string, r io.Reader) error {
			body, err := io.ReadAll(r)
			if err != nil {
				return err
			}
			seen = append(seen, key+":"+string(body))
			return nil
		}))

	if err := durable.Put(context.Background(), "k", bytes.NewReader([]byte("trustworthy"))); err != nil {
		t.Fatalf("durable Put: %v", err)
	}
	rc, hit, err := c.Get(context.Background(), "k")
	if err != nil || !hit {
		t.Fatalf("Get = hit %v, err %v; want a hit", hit, err)
	}
	defer func() { _ = rc.Close() }()

	// The verifier reads its own copy, so the caller still gets the object whole.
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(body) != "trustworthy" {
		t.Errorf("body = %q, want %q", body, "trustworthy")
	}
	if len(seen) != 1 || seen[0] != "k:trustworthy" {
		t.Errorf("verifier saw %v, want one call with the durable bytes", seen)
	}
}

// A durable object that fails verification must not reach the caller, and must
// not be left in the local layer where the next request would serve it as a
// trusted local hit. Reporting a miss is what makes the handler refetch from
// upstream and repair both layers.
func TestLayeredRejectsUnverifiedDurableContent(t *testing.T) {
	local := &deletingMemCache{memCache: newMemCache()}
	durable := newMemCache()
	metrics := &countingIntegrityMetrics{}
	c := NewLayered(local, durable, discardLogger(),
		WithDurableVerifier(func(context.Context, string, io.Reader) error {
			return errors.New("digest mismatch")
		}),
		WithIntegrityMetrics(metrics))

	if err := durable.Put(context.Background(), "k", bytes.NewReader([]byte("tampered"))); err != nil {
		t.Fatalf("durable Put: %v", err)
	}

	rc, hit, err := c.Get(context.Background(), "k")
	if err != nil {
		t.Fatalf("Get error = %v, want the rejection reported as a miss", err)
	}
	if hit {
		_ = rc.Close()
		t.Fatal("unverified durable content was served")
	}
	if local.has("k") {
		t.Error("unverified object was left in the local layer")
	}
	if len(local.deleted) != 1 || local.deleted[0] != "k" {
		t.Errorf("deleted = %v, want [k]", local.deleted)
	}
	if metrics.failures != 1 {
		t.Errorf("integrity failures = %d, want 1", metrics.failures)
	}
	// The durable copy is left alone: repairing it is the writer's job, and a
	// replica should not delete objects other replicas may be mid-read on.
	if !durable.has("k") {
		t.Error("durable object was deleted; that is not this layer's call")
	}
}

// Without a verifier, durable content is trusted as before.
func TestLayeredWithoutVerifierTrustsDurable(t *testing.T) {
	local := newMemCache()
	durable := newMemCache()
	c := NewLayered(local, durable, discardLogger())
	if err := durable.Put(context.Background(), "k", bytes.NewReader([]byte("x"))); err != nil {
		t.Fatalf("durable Put: %v", err)
	}
	rc, hit, err := c.Get(context.Background(), "k")
	if err != nil || !hit {
		t.Fatalf("Get = hit %v, err %v; want a hit", hit, err)
	}
	_ = rc.Close()
}
