// Package observ wires terrastrata's observability: structured logging and
// Prometheus metrics.
package observ

import (
	"io"
	"log/slog"
)

// NewLogger returns a JSON slog.Logger writing to w at the given level. JSON is
// chosen for machine-parseable logs in aggregators (Loki, ELK, Cloud logging).
func NewLogger(w io.Writer, level slog.Level) *slog.Logger {
	handler := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: level,
	})
	return slog.New(handler)
}
