package mirror

import (
	"github.com/pascalinthecloud/terrastrata/internal/pathsafe"
)

// Path component validation for Terraform provider coordinates.
//
// The traversal-proof checks themselves live in internal/pathsafe, shared with
// the module registry; this file only names the coordinates a provider request
// is made of.

// Coordinates identifies a single provider request after validation. Fields are
// only populated for the parts relevant to a given endpoint.
type Coordinates struct {
	Hostname  string
	Namespace string
	Type      string
	Version   string
	Platform  string // "os_arch"
	Filename  string
}

// ValidateProvider checks the hostname/namespace/type triple shared by every
// endpoint.
func ValidateProvider(hostname, namespace, typ string) (Coordinates, error) {
	if err := pathsafe.Hostname("hostname", hostname); err != nil {
		return Coordinates{}, err
	}
	if err := pathsafe.Name("namespace", namespace); err != nil {
		return Coordinates{}, err
	}
	if err := pathsafe.Name("type", typ); err != nil {
		return Coordinates{}, err
	}
	return Coordinates{Hostname: hostname, Namespace: namespace, Type: typ}, nil
}

// withVersion validates and attaches a version to the coordinates.
func (c Coordinates) withVersion(version string) (Coordinates, error) {
	if err := pathsafe.Version("version", version); err != nil {
		return Coordinates{}, err
	}
	c.Version = version
	return c, nil
}

// withDownload validates and attaches the platform and filename used by the zip
// endpoint.
func (c Coordinates) withDownload(platform, filename string) (Coordinates, error) {
	if err := pathsafe.Platform("platform", platform); err != nil {
		return Coordinates{}, err
	}
	if err := pathsafe.ZipFilename("filename", filename); err != nil {
		return Coordinates{}, err
	}
	c.Platform = platform
	c.Filename = filename
	return c, nil
}
