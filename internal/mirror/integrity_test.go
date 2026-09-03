package mirror

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pascalinthecloud/terrastrata/internal/cache"
)

// mapCache is a minimal in-memory cache, standing in for either layer. It is
// mutex-guarded because Layered writes the durable layer from a goroutine.
type mapCache struct {
	mu   sync.Mutex
	data map[string][]byte
}

func newMapCache() *mapCache { return &mapCache{data: map[string][]byte{}} }

func (m *mapCache) Get(_ context.Context, key string) (io.ReadCloser, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.data[key]
	if !ok {
		return nil, false, nil
	}
	return io.NopCloser(bytes.NewReader(b)), true, nil
}

func (m *mapCache) Put(_ context.Context, key string, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = b
	return nil
}

func (m *mapCache) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, key)
	return nil
}

func (m *mapCache) get(key string) []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.data[key]
}

func (m *mapCache) set(key string, b []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = b
}

func (m *mapCache) drop(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, key)
}

// waitFor polls until want lands under key, since the durable layer is written
// asynchronously. It fails the test rather than hanging.
func (m *mapCache) waitFor(t *testing.T, key string, want []byte) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if bytes.Equal(m.get(key), want) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("%q did not reach the expected content within the deadline", key)
}

const (
	testZipKey   = "registry.terraform.io/hashicorp/null/3.2.0/download/linux_amd64/terraform-provider-null_3.2.0_linux_amd64.zip"
	testIndexKey = "registry.terraform.io/hashicorp/null/3.2.0.json"
)

func archivesIndexFor(t *testing.T, digest string) []byte {
	t.Helper()
	body, err := json.Marshal(ArchivesIndex{Archives: map[string]Archive{
		"linux_amd64": {
			URL:    "3.2.0/download/linux_amd64/terraform-provider-null_3.2.0_linux_amd64.zip",
			Hashes: []string{"zh:" + digest},
		},
	}})
	if err != nil {
		t.Fatalf("marshal index: %v", err)
	}
	return body
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestDurableVerifierAcceptsMatchingArchive(t *testing.T) {
	zip := []byte("PK\x03\x04 a provider zip")
	index := newMapCache()
	index.set(testIndexKey, archivesIndexFor(t, digestOf(zip)))

	verify := DurableVerifier(index, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := verify(context.Background(), testZipKey, bytes.NewReader(zip)); err != nil {
		t.Errorf("verify = %v, want nil for an archive matching the published digest", err)
	}
}

func TestDurableVerifierRejectsTamperedArchive(t *testing.T) {
	zip := []byte("PK\x03\x04 a provider zip")
	index := newMapCache()
	index.set(testIndexKey, archivesIndexFor(t, digestOf(zip)))

	verify := DurableVerifier(index, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	err := verify(context.Background(), testZipKey, bytes.NewReader([]byte("PK\x03\x04 not that zip")))
	if err == nil {
		t.Fatal("verify accepted an archive whose digest does not match the published one")
	}
	if !strings.Contains(err.Error(), "cached-index digest") {
		t.Errorf("error = %q, want it to name the digest source it used", err)
	}
}

// Objects the verifier cannot check must pass rather than fail: refusing what it
// cannot vouch for would turn a cold archives index into an outage.
func TestDurableVerifierPassesWhatItCannotCheck(t *testing.T) {
	zip := []byte("PK\x03\x04 a provider zip")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	populated := newMapCache()
	populated.set(testIndexKey, archivesIndexFor(t, digestOf(zip)))

	cases := []struct {
		name  string
		key   string
		index CacheReader
	}{
		{"versions index", "registry.terraform.io/hashicorp/null/index.json", populated},
		{"archives index", testIndexKey, populated},
		{"module object", "_modules/claranet/regions/azurerm/8.0.6/archive", populated},
		{"archive with no cached index", testZipKey, newMapCache()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verify := DurableVerifier(tc.index, nil, log)
			// Deliberately the wrong bytes: an unverifiable object must pass on
			// its key alone, without the content mattering.
			if err := verify(context.Background(), tc.key, bytes.NewReader([]byte("anything"))); err != nil {
				t.Errorf("verify = %v, want nil", err)
			}
		})
	}
}

func TestDurableVerifierIgnoresAnIndexWithoutAUsableHash(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, tc := range []struct {
		name  string
		index ArchivesIndex
	}{
		{"no hashes", ArchivesIndex{Archives: map[string]Archive{"linux_amd64": {URL: "u"}}}},
		{"another platform", ArchivesIndex{Archives: map[string]Archive{"darwin_arm64": {Hashes: []string{"zh:" + digestOf([]byte("x"))}}}}},
		{"not a zh hash", ArchivesIndex{Archives: map[string]Archive{"linux_amd64": {Hashes: []string{"h1:abc"}}}}},
		{"zh hash that is not a digest", ArchivesIndex{Archives: map[string]Archive{"linux_amd64": {Hashes: []string{"zh:not-hex"}}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(tc.index)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			c := newMapCache()
			c.set(testIndexKey, body)
			verify := DurableVerifier(c, nil, log)
			if err := verify(context.Background(), testZipKey, bytes.NewReader([]byte("whatever"))); err != nil {
				t.Errorf("verify = %v, want nil when the index names no usable digest", err)
			}
		})
	}
}

func TestZipKeyCoordinates(t *testing.T) {
	c, ok := zipKeyCoordinates(testZipKey)
	if !ok {
		t.Fatal("zipKeyCoordinates rejected a key produced by ZipCacheKey")
	}
	if c.Hostname != "registry.terraform.io" || c.Namespace != "hashicorp" || c.Type != "null" ||
		c.Version != "3.2.0" || c.Platform != "linux_amd64" {
		t.Errorf("coordinates = %+v", c)
	}
	// Round-trips with the producer.
	if got := ZipCacheKey(c); got != testZipKey {
		t.Errorf("ZipCacheKey(parsed) = %q, want %q", got, testZipKey)
	}

	for _, key := range []string{
		"registry.terraform.io/hashicorp/null/index.json",
		"registry.terraform.io/hashicorp/null/3.2.0.json",
		"_modules/claranet/regions/azurerm/8.0.6/archive",
		"registry.terraform.io/hashicorp/null/3.2.0/downloads/linux_amd64/f.zip", // not "download"
		"registry.terraform.io/hashicorp/null/3.2.0/download/linux_amd64/f.tar.gz",
		"registry.terraform.io/hashicorp/null/not-a-version/download/linux_amd64/f.zip",
		"registry.terraform.io/hashicorp/null/3.2.0/download/LINUX_AMD64/f.zip",
	} {
		if _, ok := zipKeyCoordinates(key); ok {
			t.Errorf("zipKeyCoordinates(%q) = ok, want it treated as not an archive", key)
		}
	}
}

// The property worth having end to end: an archive tampered with in shared
// storage is not served, and the next request repairs it from upstream instead of
// failing. The client gets correct bytes and never learns anything was wrong.
func TestPoisonedDurableArchiveIsRefetchedFromUpstream(t *testing.T) {
	reg := newFakeRegistry(t)
	local := newMapCache()
	durable := newMapCache()

	var layered *cache.Layered
	metrics := &countingIntegrity{}
	upstream := NewUpstream(reg.server.URL, "terrastrata-test", 5*time.Second)
	verifier := DurableVerifier(readerFunc(func(ctx context.Context, key string) (io.ReadCloser, bool, error) {
		return layered.Get(ctx, key)
	}), SingleUpstream("registry.terraform.io", upstream), slog.New(slog.NewTextHandler(io.Discard, nil)))
	layered = cache.NewLayered(local, durable, slog.New(slog.NewTextHandler(io.Discard, nil)),
		cache.WithDurableVerifier(verifier), cache.WithIntegrityMetrics(metrics))

	h, err := NewHandler(Options{
		Cache:      layered,
		Upstreams:  SingleUpstream("registry.terraform.io", upstream),
		Metrics:    NopMetrics{},
		StagingDir: t.TempDir(),
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	zipPath := "/" + testZipKey
	// Warm the cache honestly, so both layers hold the real archive and the index.
	resp := doGet(t, srv, zipPath)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, reg.zipBytes) {
		t.Fatalf("warm-up: status %d, %d bytes", resp.StatusCode, len(body))
	}
	// The durable write is asynchronous, so wait for it rather than racing it.
	durable.waitFor(t, testZipKey, reg.zipBytes)

	// Now tamper with shared storage and drop the local copy, so the next
	// request has to come through the durable layer.
	poisoned := []byte("PK\x03\x04 malicious provider payload")
	durable.set(testZipKey, poisoned)
	local.drop(testZipKey)
	zipHitsBefore := reg.zipHits.Load()

	resp = doGet(t, srv, zipPath)
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: the request should be repaired, not failed", resp.StatusCode)
	}
	if bytes.Equal(body, poisoned) {
		t.Fatal("the tampered archive was served to the client")
	}
	if !bytes.Equal(body, reg.zipBytes) {
		t.Fatalf("served %d bytes, want the upstream archive", len(body))
	}
	if got := reg.zipHits.Load(); got != zipHitsBefore+1 {
		t.Errorf("upstream archive fetches = %d, want one refetch", got-zipHitsBefore)
	}
	if got := metrics.count(); got != 1 {
		t.Errorf("integrity failures = %d, want 1", got)
	}
	// Both layers are healed, so the next request is a plain hit again.
	if !bytes.Equal(local.get(testZipKey), reg.zipBytes) {
		t.Error("local layer was not repaired")
	}
	durable.waitFor(t, testZipKey, reg.zipBytes)
}

// countingIntegrity is called from Layered.Get, which requests may share.
type countingIntegrity struct {
	mu       sync.Mutex
	failures int
}

func (c *countingIntegrity) IntegrityFailure() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failures++
}

func (c *countingIntegrity) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.failures
}

type readerFunc func(ctx context.Context, key string) (io.ReadCloser, bool, error)

func (f readerFunc) Get(ctx context.Context, key string) (io.ReadCloser, bool, error) {
	return f(ctx, key)
}

// The registry is authoritative, so its digest is used even when the cached index
// says something else — which is exactly the case where an attacker rewrote both
// the archive and the index in shared storage.
func TestDurableVerifierPrefersTheRegistryOverTheCachedIndex(t *testing.T) {
	reg := newFakeRegistry(t)
	poisoned := []byte("PK\x03\x04 malicious payload")

	// A cached index that vouches for the poisoned bytes.
	index := newMapCache()
	index.set(testIndexKey, archivesIndexFor(t, digestOf(poisoned)))

	verify := DurableVerifier(index,
		SingleUpstream("registry.terraform.io", NewUpstream(reg.server.URL, "terrastrata-test", 5*time.Second)),
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	err := verify(context.Background(), testZipKey, bytes.NewReader(poisoned))
	if err == nil {
		t.Fatal("a consistently-poisoned archive and index passed verification")
	}
	if !strings.Contains(err.Error(), "registry-published") {
		t.Errorf("error = %q, want it to name the registry as the digest source", err)
	}
	// And the genuine archive still verifies against the registry.
	if err := verify(context.Background(), testZipKey, bytes.NewReader(reg.zipBytes)); err != nil {
		t.Errorf("verify(real archive) = %v, want nil", err)
	}
}

// With the registry unreachable — the situation a mirror exists for — the cached
// index is used instead of giving up on verification altogether.
func TestDurableVerifierFallsBackToTheCachedIndexOffline(t *testing.T) {
	zip := []byte("PK\x03\x04 a provider zip")
	index := newMapCache()
	index.set(testIndexKey, archivesIndexFor(t, digestOf(zip)))

	// A server that is not listening: every metadata call fails.
	dead := httptest.NewServer(http.NewServeMux())
	url := dead.URL
	dead.Close()

	verify := DurableVerifier(index,
		SingleUpstream("registry.terraform.io", NewUpstream(url, "terrastrata-test", time.Second)),
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err := verify(context.Background(), testZipKey, bytes.NewReader(zip)); err != nil {
		t.Errorf("verify = %v, want nil: the cached index vouches for these bytes", err)
	}
	if err := verify(context.Background(), testZipKey, bytes.NewReader([]byte("tampered"))); err == nil {
		t.Error("tampered bytes passed while falling back to the cached index")
	}
}
