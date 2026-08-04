// Package pathsafe validates the path components of registry coordinates.
//
// These validators are the cache's first line of defense. Every path segment
// that becomes part of a cache key or an upstream URL MUST pass through them, so
// that no request can inject "..", path separators, or control characters into a
// filesystem path or remote request. They live in their own package because both
// the provider mirror and the module registry depend on exactly this behavior,
// and a fix here must apply to both.
package pathsafe

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	// hostnameRe matches a DNS-style registry hostname, optionally with a port.
	hostnameRe = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?)*(:[0-9]{1,5})?$`)

	// nameRe matches a namespace, provider type, module name, or module target
	// system: alphanumerics with internal hyphens/underscores. No dots, so "."
	// and ".." are impossible.
	nameRe = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9_-]*[a-zA-Z0-9])?$`)

	// versionRe matches a SemVer-like version (digits, dots, pre-release/build
	// metadata). The explicit "no .." check in Validate covers dot-adjacency.
	versionRe = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:[-+][a-zA-Z0-9._-]+)?$`)

	// platformRe matches an os_arch identifier such as "linux_amd64".
	platformRe = regexp.MustCompile(`^[a-z0-9]+_[a-z0-9]+$`)

	// filenameRe matches a provider zip filename with no path component.
	filenameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*\.zip$`)
)

// Validate checks value against re after rejecting the traversal and separator
// characters no coordinate may ever contain. field names the component in the
// returned error.
func Validate(field, value string, re *regexp.Regexp) error {
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

// Hostname validates a registry hostname (optionally with a port).
func Hostname(field, value string) error { return Validate(field, value, hostnameRe) }

// Name validates a namespace, provider type, module name, or module target system.
func Name(field, value string) error { return Validate(field, value, nameRe) }

// Version validates a SemVer-like version string.
func Version(field, value string) error { return Validate(field, value, versionRe) }

// Platform validates an "os_arch" platform identifier.
func Platform(field, value string) error { return Validate(field, value, platformRe) }

// ZipFilename validates a provider archive filename with no path component.
func ZipFilename(field, value string) error { return Validate(field, value, filenameRe) }
