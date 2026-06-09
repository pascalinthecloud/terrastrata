package mirror

import (
	"fmt"
	"regexp"
	"strings"
)

// Path component validation for Terraform provider coordinates.
//
// These validators are the cache's first line of defense. Every path segment
// that becomes part of a cache key or an upstream URL MUST pass through them, so
// that no request can inject "..", path separators, or control characters into a
// filesystem path or remote request.
var (
	// hostnameRe matches a DNS-style registry hostname, optionally with a port.
	hostnameRe = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?)*(:[0-9]{1,5})?$`)

	// nameRe matches a provider namespace or type: alphanumerics with internal
	// hyphens/underscores. No dots, so "." and ".." are impossible.
	nameRe = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9_-]*[a-zA-Z0-9])?$`)

	// versionRe matches a SemVer-like version (digits, dots, pre-release/build
	// metadata). The explicit "no .." check below covers the dot-adjacency case.
	versionRe = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:[-+][a-zA-Z0-9._-]+)?$`)

	// platformRe matches an os_arch identifier such as "linux_amd64".
	platformRe = regexp.MustCompile(`^[a-z0-9]+_[a-z0-9]+$`)

	// filenameRe matches a provider zip filename with no path component.
	filenameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*\.zip$`)
)

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

func validate(field, value string, re *regexp.Regexp) error {
	if value == "" {
		return fmt.Errorf("missing %s", field)
	}
	if strings.Contains(value, "..") || strings.ContainsAny(value, "/\\\x00") {
		return fmt.Errorf("invalid %s %q", field, value)
	}
	if !re.MatchString(value) {
		return fmt.Errorf("invalid %s %q", field, value)
	}
	return nil
}

// ValidateProvider checks the hostname/namespace/type triple shared by every
// endpoint.
func ValidateProvider(hostname, namespace, typ string) (Coordinates, error) {
	if err := validate("hostname", hostname, hostnameRe); err != nil {
		return Coordinates{}, err
	}
	if err := validate("namespace", namespace, nameRe); err != nil {
		return Coordinates{}, err
	}
	if err := validate("type", typ, nameRe); err != nil {
		return Coordinates{}, err
	}
	return Coordinates{Hostname: hostname, Namespace: namespace, Type: typ}, nil
}

// withVersion validates and attaches a version to the coordinates.
func (c Coordinates) withVersion(version string) (Coordinates, error) {
	if err := validate("version", version, versionRe); err != nil {
		return Coordinates{}, err
	}
	c.Version = version
	return c, nil
}

// withDownload validates and attaches the platform and filename used by the zip
// endpoint.
func (c Coordinates) withDownload(platform, filename string) (Coordinates, error) {
	if err := validate("platform", platform, platformRe); err != nil {
		return Coordinates{}, err
	}
	if err := validate("filename", filename, filenameRe); err != nil {
		return Coordinates{}, err
	}
	c.Platform = platform
	c.Filename = filename
	return c, nil
}
