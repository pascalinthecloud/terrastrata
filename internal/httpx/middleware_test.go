package httpx

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestIDGeneratesWhenAbsent(t *testing.T) {
	var seen string
	h := RequestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = RequestIDFromContext(r.Context())
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if seen == "" {
		t.Error("expected a generated request ID in context")
	}
	if got := rec.Header().Get("X-Request-Id"); got != seen {
		t.Errorf("response X-Request-Id = %q, want %q (same as context)", got, seen)
	}
}

func TestRequestIDReusesInboundHeader(t *testing.T) {
	var seen string
	h := RequestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = RequestIDFromContext(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-Id", "upstream-id-123")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if seen != "upstream-id-123" {
		t.Errorf("context request ID = %q, want inbound upstream-id-123", seen)
	}
	if got := rec.Header().Get("X-Request-Id"); got != "upstream-id-123" {
		t.Errorf("response X-Request-Id = %q, want echoed inbound ID", got)
	}
}

func TestRequestIDFromContextUnset(t *testing.T) {
	if got := RequestIDFromContext(t.Context()); got != "" {
		t.Errorf("RequestIDFromContext on bare context = %q, want empty", got)
	}
}

func TestLoggingEmitsAccessLine(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))

	h := Logging(log)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("short and stout"))
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/teapot", nil))

	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("access log is not one JSON line: %v (%q)", err, buf.String())
	}
	if line["status"] != float64(http.StatusTeapot) {
		t.Errorf("logged status = %v, want 418", line["status"])
	}
	if line["bytes"] != float64(len("short and stout")) {
		t.Errorf("logged bytes = %v, want %d", line["bytes"], len("short and stout"))
	}
	if line["path"] != "/teapot" {
		t.Errorf("logged path = %v", line["path"])
	}
}

func TestLoggingReusesExistingRecorder(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))

	var inner *ResponseRecorder
	h := Logging(log)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		rec, ok := w.(*ResponseRecorder)
		if !ok {
			t.Fatal("handler did not receive a *ResponseRecorder")
		}
		inner = rec
		w.WriteHeader(http.StatusAccepted)
	}))

	// Pre-wrap, as metrics.Middleware does; Logging must not wrap again.
	outer := NewResponseRecorder(httptest.NewRecorder())
	h.ServeHTTP(outer, httptest.NewRequest(http.MethodGet, "/", nil))

	if inner != outer {
		t.Error("Logging wrapped an already-wrapped ResponseRecorder")
	}
	if outer.Status != http.StatusAccepted {
		t.Errorf("recorded status = %d, want 202", outer.Status)
	}
}

func TestRecoveryConvertsPanicTo500(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))

	h := Recovery(log)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	logged := buf.String()
	if !strings.Contains(logged, "panic recovered") || !strings.Contains(logged, "boom") {
		t.Errorf("panic log missing details: %q", logged)
	}
	if !strings.Contains(logged, "stack") {
		t.Errorf("panic log missing stack: %q", logged)
	}
}

func TestChainOrdersFirstOutermost(t *testing.T) {
	var order []string
	mw := func(name string) Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		}
	}
	h := Chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		order = append(order, "handler")
	}), mw("a"), mw("b"), mw("c"))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	want := []string{"a", "b", "c", "handler"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

func TestResponseRecorderDefaultsAndAccounting(t *testing.T) {
	rec := NewResponseRecorder(httptest.NewRecorder())
	if rec.Status != http.StatusOK {
		t.Errorf("default status = %d, want 200", rec.Status)
	}
	n, err := rec.Write([]byte("abc"))
	if err != nil || n != 3 {
		t.Fatalf("Write = %d, %v", n, err)
	}
	_, _ = rec.Write([]byte("de"))
	if rec.Bytes != 5 {
		t.Errorf("Bytes = %d, want 5", rec.Bytes)
	}

	rec2 := NewResponseRecorder(httptest.NewRecorder())
	rec2.WriteHeader(http.StatusNotFound)
	if rec2.Status != http.StatusNotFound {
		t.Errorf("Status = %d, want 404", rec2.Status)
	}
}

func TestResponseRecorderUnwrapSupportsFlush(t *testing.T) {
	under := httptest.NewRecorder()
	rec := NewResponseRecorder(under)
	rc := http.NewResponseController(rec)
	if err := rc.Flush(); err != nil {
		t.Errorf("Flush through ResponseController: %v", err)
	}
	if !under.Flushed {
		t.Error("underlying writer was not flushed")
	}
}
