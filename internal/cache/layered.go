package cache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"
)

// Verifier checks an object that has just arrived from the durable layer, before
// it is trusted. Returning an error means the bytes must not be served: the
// local copy is discarded and the caller is told the object is absent, so it
// refetches from the authoritative source and repairs both layers.
//
// It reads the object whole, so it is only worth applying to a layer we did not
// write ourselves in this process — which is exactly the durable one.
type Verifier func(ctx context.Context, key string, r io.Reader) error

// IntegrityMetrics records durable-layer verification failures.
type IntegrityMetrics interface {
	// IntegrityFailure reports one object rejected by the verifier.
	IntegrityFailure()
}

// deleter is implemented by cache layers that can drop a single object. The
// local layer can; the durable layer deliberately is not asked to, since a
// terrastrata replica is not the right thing to be deleting shared objects.
type deleter interface {
	Delete(ctx context.Context, key string) error
}

// LayeredOption configures a Layered cache.
type LayeredOption func(*Layered)

// WithDurableVerifier makes every durable-layer hit pass through v before it is
// served. Without it, durable content is trusted as-is (the behaviour when no
// durable layer is configured at all).
func WithDurableVerifier(v Verifier) LayeredOption {
	return func(l *Layered) { l.verify = v }
}

// WithIntegrityMetrics records verification failures.
func WithIntegrityMetrics(m IntegrityMetrics) LayeredOption {
	return func(l *Layered) { l.metrics = m }
}

// Layered composes a fast local cache with an optional durable cache (S3),
// implementing the lookup order local -> durable -> miss.
//
//   - Get: returns the first hit. On a durable hit, the object is written back
//     to the local layer ("warming") so subsequent reads are fast.
//   - Put: writes the local layer synchronously and the durable layer
//     asynchronously, so request latency never depends on the remote store.
type Layered struct {
	local   Cache
	durable Cache // nil when S3 is disabled
	log     *slog.Logger

	// asyncPutTimeout bounds each background durable upload.
	asyncPutTimeout time.Duration

	// verify, when set, checks each object arriving from the durable layer.
	verify Verifier

	// metrics records verification failures; nil is fine.
	metrics IntegrityMetrics

	// onDurablePut, if non-nil, is invoked after each async durable Put
	// completes. It exists purely as a test synchronization hook.
	onDurablePut func(key string, err error)
}

// NewLayered returns a Layered cache. durable may be nil, in which case Layered
// behaves as a thin wrapper over local.
func NewLayered(local, durable Cache, log *slog.Logger, opts ...LayeredOption) *Layered {
	l := &Layered{
		local:           local,
		durable:         durable,
		log:             log,
		asyncPutTimeout: 2 * time.Minute,
	}
	for _, opt := range opts {
		opt(l)
	}
	return l
}

// Get implements Cache.
func (l *Layered) Get(ctx context.Context, key string) (io.ReadCloser, bool, error) {
	rc, hit, err := l.local.Get(ctx, key)
	if err != nil {
		return nil, false, err
	}
	if hit {
		return rc, true, nil
	}

	if l.durable == nil {
		return nil, false, nil
	}

	rc, hit, err = l.durable.Get(ctx, key)
	if err != nil {
		return nil, false, err
	}
	if !hit {
		return nil, false, nil
	}

	// Durable hit: stream it into the local layer (warming), then re-open the
	// warmed file to hand back to the caller. This keeps memory flat — the object
	// is never buffered whole — at the cost of one local read-back. Warming is
	// best-effort: if the local layer is degraded (full disk, bad volume), the
	// object is re-read from the durable layer so a durable hit is still served.
	defer func() { _ = rc.Close() }()
	if err := l.local.Put(ctx, key, rc); err != nil {
		l.log.Warn("cache warm failed", "key", key, "err", err)
		return l.durable.Get(ctx, key)
	}
	local, localHit, err := l.local.Get(ctx, key)
	if err != nil || !localHit {
		l.log.Warn("cache warm read-back failed", "key", key, "hit", localHit, "err", err)
		return l.durable.Get(ctx, key)
	}

	// The durable layer is shared and outlives this process, so it is the one
	// input to the cache that this process did not produce and verify itself.
	// Check it before serving; a failure is reported as a miss so the caller
	// refetches from upstream — which re-verifies against the registry and
	// overwrites both layers, healing the entry rather than just refusing it.
	if l.verify != nil {
		if err := l.verifyWarmed(ctx, key); err != nil {
			_ = local.Close()
			l.log.Error("durable cache object failed verification, discarding",
				"key", key, "err", err)
			if l.metrics != nil {
				l.metrics.IntegrityFailure()
			}
			if d, ok := l.local.(deleter); ok {
				if derr := d.Delete(ctx, key); derr != nil {
					l.log.Warn("could not discard unverified local copy", "key", key, "err", derr)
				}
			}
			return nil, false, nil
		}
	}
	return local, true, nil
}

// verifyWarmed runs the verifier over the freshly warmed local copy. It opens
// its own reader so the one already handed to the caller stays at the start of
// the object; on a local disk that read is page-cached and costs a hash, not a
// second download.
func (l *Layered) verifyWarmed(ctx context.Context, key string) error {
	rc, hit, err := l.local.Get(ctx, key)
	if err != nil {
		return fmt.Errorf("cache: reopen for verification: %w", err)
	}
	if !hit {
		return errors.New("cache: warmed object vanished before verification")
	}
	defer func() { _ = rc.Close() }()
	return l.verify(ctx, key, rc)
}

// Put writes the object to the local layer synchronously, then (if a durable
// layer exists) uploads to it asynchronously by re-reading the warmed local
// file — so nothing is buffered in memory and request latency never depends on
// the remote store.
func (l *Layered) Put(ctx context.Context, key string, r io.Reader) error {
	if err := l.local.Put(ctx, key, r); err != nil {
		return err
	}
	if l.durable == nil {
		return nil
	}
	//nolint:gosec // G118: detached context is intentional — the durable upload
	// must outlive the originating request.
	go l.putDurable(key)
	return nil
}

func (l *Layered) putDurable(key string) {
	// A detached context: the originating request may already be done.
	ctx, cancel := context.WithTimeout(context.Background(), l.asyncPutTimeout)
	defer cancel()

	rc, hit, err := l.local.Get(ctx, key)
	if err != nil || !hit {
		l.log.Error("durable put: local read-back failed", "key", key, "hit", hit, "err", err)
		l.notifyDurablePut(key, err)
		return
	}
	defer func() { _ = rc.Close() }()

	err = l.durable.Put(ctx, key, rc)
	if err != nil {
		l.log.Error("durable cache put failed", "key", key, "err", err)
	} else {
		l.log.Debug("durable cache put", "key", key)
	}
	l.notifyDurablePut(key, err)
}

func (l *Layered) notifyDurablePut(key string, err error) {
	if l.onDurablePut != nil {
		l.onDurablePut(key, err)
	}
}
