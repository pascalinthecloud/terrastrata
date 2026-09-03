package mirror

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// Integrity of the durable cache layer.
//
// A provider archive is verified against the registry's published SHA-256 before
// it is cached (see stageVerifiedZip), so anything this process wrote is known
// good. The durable layer is different: it is shared between replicas, outlives
// every one of them, and is reachable by anything holding the bucket
// credentials. It is the one cache input terrastrata did not produce itself.
//
// So an archive arriving from the durable layer is re-hashed before it is served,
// and compared against a digest from outside that layer:
//
//  1. the registry's own published SHA-256, asked for over the network. This is
//     authoritative and beyond the reach of anyone holding only bucket
//     credentials, so it catches even an attacker who rewrote the archive and the
//     cached archives index together.
//  2. failing that — the registry is unreachable, which is the situation this
//     whole program exists for — the "zh:" digest in our own cached archives
//     index. Weaker, since that index lives in the same storage being checked,
//     but it still catches single-object tampering and silent corruption, and it
//     keeps the guarantee that what we serve matches the hashes we publish.
//
// If neither is available the object passes: refusing what cannot be checked
// would turn a registry outage into an outage here, which is the opposite of the
// point. A rejection is reported as a cache miss rather than an error, so the
// archive is refetched from upstream and both layers are repaired.

// CacheReader is the read side of the cache, as the verifier needs it.
type CacheReader interface {
	Get(ctx context.Context, key string) (io.ReadCloser, bool, error)
}

// DurableVerifier returns a verifier for objects arriving from the durable cache
// layer.
//
// upstreams is keyed by served hostname, as the handler keys it, and is asked for
// the authoritative digest first. index reads back our own archives index as the
// offline fallback; passing the full cache is intended, so that index may itself
// come from the durable layer.
//
// Objects it cannot check — the JSON indexes, module objects, an archive whose
// digest is available from nowhere — pass.
func DurableVerifier(index CacheReader, upstreams map[string]*Upstream, log *slog.Logger) func(ctx context.Context, key string, r io.Reader) error {
	return func(ctx context.Context, key string, r io.Reader) error {
		c, ok := zipKeyCoordinates(key)
		if !ok {
			return nil // not a provider archive
		}
		want, source, ok := expectedZipDigest(ctx, index, upstreams, c)
		if !ok {
			log.Warn("serving a durable archive unverified: no digest available", "key", key)
			return nil
		}
		log.Debug("verifying durable archive", "key", key, "digest_source", source)

		hasher := sha256.New()
		if _, err := io.Copy(hasher, r); err != nil {
			return fmt.Errorf("mirror: hash durable archive %q: %w", key, err)
		}
		if got := hex.EncodeToString(hasher.Sum(nil)); !strings.EqualFold(got, want) {
			return fmt.Errorf("mirror: durable archive %q has digest %s, %s digest is %s",
				key, got, source, want)
		}
		return nil
	}
}

// zipKeyCoordinates is the inverse of ZipCacheKey. It re-validates every
// component rather than trusting the key's shape, so a key from anywhere but
// ZipCacheKey simply reports "not an archive" instead of producing coordinates
// that were never checked.
func zipKeyCoordinates(key string) (Coordinates, bool) {
	parts := strings.Split(key, "/")
	// hostname/namespace/type/version/download/platform/filename
	if len(parts) != 7 || parts[4] != "download" {
		return Coordinates{}, false
	}
	c, err := ValidateProvider(parts[0], parts[1], parts[2])
	if err != nil {
		return Coordinates{}, false
	}
	c, err = c.withVersion(parts[3])
	if err != nil {
		return Coordinates{}, false
	}
	c, err = c.withDownload(parts[5], parts[6])
	if err != nil {
		return Coordinates{}, false
	}
	return c, true
}

// expectedZipDigest resolves the digest c's archive must have, preferring the
// registry over anything stored locally. The returned string names the source it
// came from, for logs and error messages.
func expectedZipDigest(ctx context.Context, index CacheReader, upstreams map[string]*Upstream, c Coordinates) (digest, source string, ok bool) {
	if d, ok := upstreamZipDigest(ctx, upstreams, c); ok {
		return d, "registry-published", true
	}
	if d, ok := publishedZipDigest(ctx, index, c); ok {
		return d, "cached-index", true
	}
	return "", "", false
}

// upstreamZipDigest asks the registry what c's archive should hash to. Any
// failure — no client for the hostname, network down, a filename that no longer
// matches — means "no answer", not an error: the caller falls back.
func upstreamZipDigest(ctx context.Context, upstreams map[string]*Upstream, c Coordinates) (string, bool) {
	u := upstreams[strings.ToLower(c.Hostname)]
	if u == nil {
		return "", false
	}
	osName, arch, _ := strings.Cut(c.Platform, "_")
	meta, err := u.GetDownload(ctx, c, osName, arch)
	if err != nil {
		return "", false
	}
	// A digest for a different file is not a digest for this one.
	if meta.Filename != c.Filename || !isSHA256Hex(meta.Shasum) {
		return "", false
	}
	return meta.Shasum, true
}

// publishedZipDigest reads the cached archives index for c and returns the
// SHA-256 terrastrata publishes for c's platform.
func publishedZipDigest(ctx context.Context, index CacheReader, c Coordinates) (string, bool) {
	rc, hit, err := index.Get(ctx, ArchivesCacheKey(c))
	if err != nil || !hit {
		return "", false
	}
	defer func() { _ = rc.Close() }()

	// The index is a small JSON document; the bound guards against a corrupt or
	// hostile object in the same shared storage we are checking.
	var idx ArchivesIndex
	if err := json.NewDecoder(io.LimitReader(rc, 8<<20)).Decode(&idx); err != nil {
		return "", false
	}
	archive, ok := idx.Archives[c.Platform]
	if !ok {
		return "", false
	}
	for _, h := range archive.Hashes {
		digest, ok := strings.CutPrefix(h, "zh:")
		if ok && isSHA256Hex(digest) {
			return digest, true
		}
	}
	return "", false
}
