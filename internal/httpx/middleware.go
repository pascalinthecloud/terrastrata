// Package httpx provides small, dependency-free HTTP middleware: request IDs,
// structured access logging, panic recovery, and optional bearer auth. Each
// middleware has the signature func(http.Handler) http.Handler and composes via
// Chain.
package httpx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"
)

// Middleware decorates an http.Handler.
type Middleware func(http.Handler) http.Handler

// Chain applies middleware so the first listed runs outermost (first on the way
// in, last on the way out).
func Chain(h http.Handler, mws ...Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// ResponseRecorder wraps http.ResponseWriter to capture the status code and the
// number of bytes written, for logging and metrics. It is shared so a request is
// only wrapped once.
type ResponseRecorder struct {
	http.ResponseWriter
	Status int
	Bytes  int
}

// NewResponseRecorder returns a recorder defaulting to status 200.
func NewResponseRecorder(w http.ResponseWriter) *ResponseRecorder {
	return &ResponseRecorder{ResponseWriter: w, Status: http.StatusOK}
}

func (r *ResponseRecorder) WriteHeader(code int) {
	r.Status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *ResponseRecorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.Bytes += n
	return n, err
}

// Unwrap exposes the underlying writer to http.ResponseController (flush, hijack).
func (r *ResponseRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

type ctxKey int

const requestIDKey ctxKey = iota

// RequestID ensures every request carries an ID: it reuses an inbound
// X-Request-Id when present, otherwise generates one, and echoes it back.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			id = newID()
		}
		w.Header().Set("X-Request-Id", id)
		ctx := context.WithValue(r.Context(), requestIDKey, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequestIDFromContext returns the request ID, or "" if unset.
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// Logging emits one structured access log line per request at Info level.
func Logging(log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec, ok := w.(*ResponseRecorder)
			if !ok {
				rec = NewResponseRecorder(w)
				w = rec
			}
			next.ServeHTTP(w, r)
			log.LogAttrs(r.Context(), slog.LevelInfo, "http request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rec.Status),
				slog.Int("bytes", rec.Bytes),
				slog.Duration("duration", time.Since(start)),
				slog.String("request_id", RequestIDFromContext(r.Context())),
				slog.String("remote", r.RemoteAddr),
			)
		})
	}
}

// Recovery converts a panic in a downstream handler into a 500, logging the
// stack so one bad request can't take the process down.
func Recovery(log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if v := recover(); v != nil {
					log.Error("panic recovered",
						"err", v,
						"path", r.URL.Path,
						"request_id", RequestIDFromContext(r.Context()),
						"stack", string(debug.Stack()),
					)
					http.Error(w, "internal server error", http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// rand.Read never fails on supported platforms; fall back to a timestamp.
		return time.Now().UTC().Format("20060102150405.000000000")
	}
	return hex.EncodeToString(b[:])
}
