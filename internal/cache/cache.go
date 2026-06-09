// Package cache provides terrastrata's pull-through object cache.
//
// The cache is content-addressed by an opaque, pre-validated key that mirrors
// the Terraform Network Mirror Protocol directory layout (for example
// "registry.terraform.io/hashicorp/null/index.json"). Callers are responsible
// for sanitizing keys before use; see internal/mirror/paths.go.
//
// Two backends are provided — a local filesystem store and an S3-compatible
// store — composed by Layered into the lookup order: local -> S3 -> miss.
package cache

import (
	"context"
	"io"
)

// Cache is a simple key/value blob store. Implementations must be safe for
// concurrent use.
//
// Put streams from a reader rather than taking a []byte so that large objects
// (provider archives are tens to hundreds of MB) are never fully buffered in
// memory.
type Cache interface {
	// Get returns a reader for the object stored under key. When the object is
	// absent it returns hit == false and a nil reader (not an error). The caller
	// must Close a non-nil reader.
	Get(ctx context.Context, key string) (rc io.ReadCloser, hit bool, err error)

	// Put stores the contents of r under key, overwriting any existing object.
	// It reads r to EOF. Some implementations (Layered) may persist to slower
	// backends asynchronously.
	Put(ctx context.Context, key string, r io.Reader) error
}
