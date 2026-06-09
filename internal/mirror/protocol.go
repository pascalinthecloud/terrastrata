package mirror

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
)

// ErrNotFound indicates an upstream resource (provider or version) does not
// exist. Handlers map it to an HTTP 404.
var ErrNotFound = errors.New("mirror: not found")

// maxPlatformConcurrency bounds parallel upstream calls when assembling an
// archives index, so a single client request can't fan out unboundedly.
const maxPlatformConcurrency = 8

// VersionsIndex is the network mirror protocol "list versions" response:
// {"versions": {"3.2.0": {}, ...}}.
type VersionsIndex struct {
	Versions map[string]struct{} `json:"versions"`
}

// ArchivesIndex is the network mirror protocol "list packages" response:
// {"archives": {"linux_amd64": {"url": "...", "hashes": ["zh:..."]}}}.
type ArchivesIndex struct {
	Archives map[string]Archive `json:"archives"`
}

// Archive is one platform entry in an ArchivesIndex. URL is relative to the
// version JSON document, per the protocol spec.
type Archive struct {
	URL    string   `json:"url"`
	Hashes []string `json:"hashes,omitempty"`
}

// BuildVersionsIndex converts the registry version list into the mirror versions
// index.
func BuildVersionsIndex(versions []VersionMeta) VersionsIndex {
	out := VersionsIndex{Versions: make(map[string]struct{}, len(versions))}
	for _, v := range versions {
		out.Versions[v.Version] = struct{}{}
	}
	return out
}

// PlatformsForVersion returns the platform list for a specific version from a
// registry version listing, or an error if the version is absent.
func PlatformsForVersion(versions []VersionMeta, version string) ([]PlatformMeta, error) {
	for _, v := range versions {
		if v.Version == version {
			return v.Platforms, nil
		}
	}
	return nil, ErrNotFound
}

// BuildArchivesIndex assembles the archives index for one version by resolving
// each platform's download metadata from upstream (concurrently, bounded). Each
// archive URL is rewritten to a terrastrata-hosted relative path so the zip is
// served and cached by us, and the registry shasum is surfaced as a "zh:" hash.
func BuildArchivesIndex(ctx context.Context, u *Upstream, c Coordinates, platforms []PlatformMeta) (ArchivesIndex, error) {
	type result struct {
		platform string
		archive  Archive
	}

	var (
		mu       sync.Mutex
		results  []result
		firstErr error
		wg       sync.WaitGroup
		sem      = make(chan struct{}, maxPlatformConcurrency)
	)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	for _, p := range platforms {
		wg.Add(1)
		sem <- struct{}{}
		go func(p PlatformMeta) {
			defer wg.Done()
			defer func() { <-sem }()

			meta, err := u.GetDownload(ctx, c, p.OS, p.Arch)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
					cancel() // stop the remaining fetches
				}
				return
			}
			platform := p.OS + "_" + p.Arch
			results = append(results, result{
				platform: platform,
				archive: Archive{
					URL:    archiveURL(c.Version, platform, meta.Filename),
					Hashes: hashesFor(meta.Shasum),
				},
			})
		}(p)
	}
	wg.Wait()

	if firstErr != nil {
		return ArchivesIndex{}, firstErr
	}

	idx := ArchivesIndex{Archives: make(map[string]Archive, len(results))}
	for _, r := range results {
		idx.Archives[r.platform] = r.archive
	}
	return idx, nil
}

// archiveURL builds the relative URL for a provider zip, encoding os/arch in the
// path so the zip endpoint is stateless. Resolved by Terraform against the
// version JSON document's URL.
func archiveURL(version, platform, filename string) string {
	return fmt.Sprintf("%s/download/%s/%s", version, platform, filename)
}

func hashesFor(shasum string) []string {
	if shasum == "" {
		return nil
	}
	return []string{"zh:" + shasum}
}

// Cache key helpers. These derive deterministic, slash-delimited keys from
// already-validated coordinates, mirroring the protocol's directory layout.

// VersionsCacheKey is the cache key for a provider's versions index.
func VersionsCacheKey(c Coordinates) string {
	return fmt.Sprintf("%s/%s/%s/index.json", c.Hostname, c.Namespace, c.Type)
}

// ArchivesCacheKey is the cache key for a version's archives index.
func ArchivesCacheKey(c Coordinates) string {
	return fmt.Sprintf("%s/%s/%s/%s.json", c.Hostname, c.Namespace, c.Type, c.Version)
}

// ZipCacheKey is the cache key for a provider archive (zip).
func ZipCacheKey(c Coordinates) string {
	return fmt.Sprintf("%s/%s/%s/%s/download/%s/%s",
		c.Hostname, c.Namespace, c.Type, c.Version, c.Platform, c.Filename)
}

// SortedVersions returns the version strings of an index in a stable order.
// Useful for deterministic logging and tests.
func SortedVersions(idx VersionsIndex) []string {
	out := make([]string, 0, len(idx.Versions))
	for v := range idx.Versions {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}
