package cache

import (
	"context"
	"io"
	"log/slog"
	"time"
)

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

	// onDurablePut, if non-nil, is invoked after each async durable Put
	// completes. It exists purely as a test synchronization hook.
	onDurablePut func(key string, err error)
}

// NewLayered returns a Layered cache. durable may be nil, in which case Layered
// behaves as a thin wrapper over local.
func NewLayered(local, durable Cache, log *slog.Logger) *Layered {
	return &Layered{
		local:           local,
		durable:         durable,
		log:             log,
		asyncPutTimeout: 2 * time.Minute,
	}
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
	// is never buffered whole — at the cost of one local read-back.
	defer func() { _ = rc.Close() }()
	if err := l.local.Put(ctx, key, rc); err != nil {
		// Warming is best-effort; fall back to serving the durable stream directly.
		l.log.Warn("cache warm failed", "key", key, "err", err)
		return nil, false, err
	}
	local, localHit, err := l.local.Get(ctx, key)
	if err != nil || !localHit {
		l.log.Warn("cache warm read-back failed", "key", key, "err", err)
		return nil, false, err
	}
	return local, true, nil
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

	if err := l.durable.Put(ctx, key, rc); err != nil {
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
