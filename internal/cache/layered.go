package cache

import (
	"bytes"
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

	// Durable hit: read it fully so we can both warm the local layer and hand a
	// reader back to the caller. Objects are bounded (provider zips ~tens of MB).
	data, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		return nil, false, err
	}
	if err := l.local.Put(ctx, key, data); err != nil {
		// Warming is best-effort; a failure here must not fail the request.
		l.log.Warn("cache warm failed", "key", key, "err", err)
	}
	return io.NopCloser(bytes.NewReader(data)), true, nil
}

// Put writes locally (synchronous) and to the durable store (asynchronous).
func (l *Layered) Put(ctx context.Context, key string, data []byte) error {
	if err := l.local.Put(ctx, key, data); err != nil {
		return err
	}
	if l.durable == nil {
		return nil
	}

	// Copy because the caller may reuse the buffer once Put returns.
	buf := make([]byte, len(data))
	copy(buf, data)
	go l.putDurable(key, buf)
	return nil
}

func (l *Layered) putDurable(key string, data []byte) {
	// A detached context: the originating request may already be done.
	ctx, cancel := context.WithTimeout(context.Background(), l.asyncPutTimeout)
	defer cancel()

	err := l.durable.Put(ctx, key, data)
	if err != nil {
		l.log.Error("durable cache put failed", "key", key, "err", err)
	} else {
		l.log.Debug("durable cache put", "key", key, "bytes", len(data))
	}
	if l.onDurablePut != nil {
		l.onDurablePut(key, err)
	}
}
