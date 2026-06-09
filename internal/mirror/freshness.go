package mirror

import (
	"encoding/json"
	"time"
)

// versionsEnvelope wraps a cached versions index with the time it was fetched
// from upstream. Storing the timestamp *inside* the cached object means
// freshness (TTL) is evaluated against the original upstream fetch, independent
// of where the bytes physically live or how many times they are copied between
// cache layers (local <-> S3). The envelope is an internal cache representation;
// only Body is ever served to clients.
type versionsEnvelope struct {
	FetchedAt time.Time       `json:"fetched_at"`
	Body      json.RawMessage `json:"body"`
}

// wrapVersions serializes a versions-index body into a freshness envelope.
func wrapVersions(body []byte, now time.Time) ([]byte, error) {
	return json.Marshal(versionsEnvelope{FetchedAt: now, Body: body})
}

// unwrapVersions parses a cached envelope, returning the served body and the
// fetch time. It returns ok == false for anything it cannot interpret as a
// populated envelope — including data written before envelopes existed — so the
// caller revalidates rather than serving garbage.
func unwrapVersions(raw []byte) (body []byte, fetchedAt time.Time, ok bool) {
	var env versionsEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, time.Time{}, false
	}
	if len(env.Body) == 0 || env.FetchedAt.IsZero() {
		return nil, time.Time{}, false
	}
	return env.Body, env.FetchedAt, true
}
