// Package freshness stores a cached document alongside the time it was fetched
// from upstream.
//
// Keeping the timestamp *inside* the cached object means freshness (TTL) is
// evaluated against the original upstream fetch, independent of where the bytes
// physically live or how many times they are copied between cache layers
// (local <-> S3). The envelope is an internal cache representation; only the
// body is ever served to clients.
//
// It is shared by the provider versions index and the module versions document,
// which have the same "grows over time, revalidate on a TTL" character.
package freshness

import (
	"encoding/json"
	"time"
)

// envelope is the on-disk representation. The JSON field names are part of the
// cache format: changing them invalidates every cached index.
type envelope struct {
	FetchedAt time.Time       `json:"fetched_at"`
	Body      json.RawMessage `json:"body"`
}

// Wrap serializes a document body into a freshness envelope.
func Wrap(body []byte, now time.Time) ([]byte, error) {
	return json.Marshal(envelope{FetchedAt: now, Body: body})
}

// Unwrap parses a cached envelope, returning the served body and the fetch
// time. It returns ok == false for anything it cannot interpret as a populated
// envelope — including data written before envelopes existed — so the caller
// revalidates rather than serving garbage.
func Unwrap(raw []byte) (body []byte, fetchedAt time.Time, ok bool) {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, time.Time{}, false
	}
	if len(env.Body) == 0 || env.FetchedAt.IsZero() {
		return nil, time.Time{}, false
	}
	return env.Body, env.FetchedAt, true
}
