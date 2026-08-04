package modules

import (
	"fmt"

	"github.com/pascalinthecloud/terrastrata/internal/pathsafe"
)

// cacheRoot namespaces every module cache key. A provider hostname can never
// collide with it: pathsafe.Hostname requires an alphanumeric first character,
// so no provider entry can live under "_modules".
const cacheRoot = "_modules"

// Coordinates identifies a module request after validation. Version is only
// populated for the endpoints that take one.
type Coordinates struct {
	Namespace string
	Name      string
	System    string // the target system, e.g. "aws" — a registry convention, not an OS
	Version   string
}

// Validate checks the namespace/name/system triple shared by every module
// endpoint.
func Validate(namespace, name, system string) (Coordinates, error) {
	if err := pathsafe.Name("namespace", namespace); err != nil {
		return Coordinates{}, err
	}
	if err := pathsafe.Name("name", name); err != nil {
		return Coordinates{}, err
	}
	if err := pathsafe.Name("system", system); err != nil {
		return Coordinates{}, err
	}
	return Coordinates{Namespace: namespace, Name: name, System: system}, nil
}

// withVersion validates and attaches a version to the coordinates.
func (c Coordinates) withVersion(version string) (Coordinates, error) {
	if err := pathsafe.Version("version", version); err != nil {
		return Coordinates{}, err
	}
	c.Version = version
	return c, nil
}

// VersionsCacheKey is the cache key for a module's version list.
func VersionsCacheKey(c Coordinates) string {
	return fmt.Sprintf("%s/%s/%s/%s/versions.json", cacheRoot, c.Namespace, c.Name, c.System)
}

// LocationCacheKey is the cache key for a version's resolved source location.
func LocationCacheKey(c Coordinates) string {
	return fmt.Sprintf("%s/%s/%s/%s/%s/location.json", cacheRoot, c.Namespace, c.Name, c.System, c.Version)
}

// ArchiveCacheKey is the cache key for a version's module archive.
func ArchiveCacheKey(c Coordinates) string {
	return fmt.Sprintf("%s/%s/%s/%s/%s/archive", cacheRoot, c.Namespace, c.Name, c.System, c.Version)
}
