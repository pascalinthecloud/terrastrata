package modules

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// ErrNotFound indicates an upstream module or version does not exist. Handlers
// map it to an HTTP 404.
var ErrNotFound = errors.New("modules: not found")

// VersionsResponse is the module registry "list available versions" response:
// {"modules":[{"versions":[{"version":"1.0.0"}]}]}. terrastrata serves the
// upstream document back verbatim — unlike the provider mirror, no translation
// between protocols is needed because we speak the same protocol upstream and
// downstream. This type exists only to validate that a response is well formed
// before it is cached.
type VersionsResponse struct {
	Modules []struct {
		Versions []struct {
			Version string `json:"version"`
		} `json:"versions"`
	} `json:"modules"`
}

// forcedGetterRe matches go-getter's "type::url" forced-getter prefix.
var forcedGetterRe = regexp.MustCompile(`^([A-Za-z0-9]+)::(.+)$`)

// githubRepoRe matches the "/OWNER/REPO" path of a GitHub repository URL, with
// an optional ".git" suffix.
var githubRepoRe = regexp.MustCompile(`^/([^/]+)/([^/]+?)(?:\.git)?$`)

// archiveTypes are the archive extensions go-getter can decompress, longest
// first so that ".tar.gz" wins over ".gz".
var archiveTypes = []string{
	"tar.bz2", "tar.gz", "tar.xz", "tar.zst",
	"tbz2", "tgz", "txz", "tzst",
	"bz2", "gz", "xz", "zip", "zst",
}

// Source is a parsed X-Terraform-Get value.
type Source struct {
	// URL is the archive to fetch, with any subdir removed and the query
	// preserved. Empty for a source terrastrata cannot cache.
	URL string
	// Subdir is go-getter's "//path" suffix (commonly "//*"), carried through
	// verbatim onto the re-hosted URL because we re-serve the identical bytes.
	Subdir string
	// Archive is the go-getter archive type ("tar.gz", "zip", …).
	Archive string
	// Repack indicates the fetched tarball wraps its contents in a single
	// top-level directory that must be stripped before serving. Set for GitHub
	// codeload tarballs; see repackStripRoot.
	Repack bool
}

// splitSubdir splits a go-getter source into its URL and "//subdir" suffix,
// reproducing go-getter's SourceDirSubdir: the scheme's "//" is skipped, and a
// query string is moved back onto the URL rather than kept with the subdir.
func splitSubdir(src string) (base, subdir string) {
	stop := len(src)
	if i := strings.Index(src, "?"); i > -1 {
		stop = i
	}

	// Skip past "://" so a scheme's slashes are never mistaken for a subdir.
	var offset int
	if i := strings.Index(src[:stop], "://"); i > -1 {
		offset = i + 3
	}

	i := strings.Index(src[offset:stop], "//")
	if i == -1 {
		return src, ""
	}
	i += offset

	subdir = src[i:]
	base = src[:i]

	// A query string belongs to the URL, not the subdir.
	if q := strings.Index(subdir, "?"); q > -1 {
		base += subdir[q:]
		subdir = subdir[:q]
	}
	return base, subdir
}

// ParseSource interprets an X-Terraform-Get value and reports whether
// terrastrata can cache it. Only plain http(s) archives are cacheable — the
// form the public registry uses. Anything else (git::, s3::, an URL with no
// determinable archive type) yields ok == false, and the caller passes the
// upstream value through untouched rather than failing the request.
//
// allowHTTP mirrors the provider path: plain http is only tolerated when the
// configured upstream is itself http (local dev), never against an https
// registry, where it would be a downgrade.
func ParseSource(raw string, allowHTTP bool) (Source, bool) {
	base, subdir := splitSubdir(strings.TrimSpace(raw))

	if m := forcedGetterRe.FindStringSubmatch(base); m != nil {
		switch m[1] {
		case "http", "https":
			base = m[2]
		case "git":
			// The public registry answers with a git:: source for every module
			// (despite the protocol docs showing an https tarball), so without
			// this branch nothing from registry.terraform.io would ever be
			// cached. GitHub serves any commit as a tarball, which we can fetch
			// and cache like any other archive — no git client required.
			return githubTarball(m[2], subdir)
		default:
			// Some other getter (s3::, hg::, …) we deliberately do not carry.
			return Source{}, false
		}
	}

	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return Source{}, false
	}
	if u.Scheme != "https" && (u.Scheme != "http" || !allowHTTP) {
		return Source{}, false
	}

	archive := archiveType(u)
	if archive == "" {
		return Source{}, false
	}
	return Source{URL: base, Subdir: subdir, Archive: archive}, true
}

// githubTarball rewrites a git:: source pointing at a GitHub repository into
// the equivalent codeload tarball, which terrastrata can fetch and cache with no
// git client and no extra dependency.
//
//	git::https://github.com/OWNER/REPO?ref=<commit>
//	→ https://codeload.github.com/OWNER/REPO/tar.gz/<commit>
//
// The registry pins ref to a commit SHA, so the tarball is immutable and safe to
// cache under the module version forever.
//
// It deliberately requires an https inner URL: an ssh:// source implies a
// private repository that codeload would refuse anyway, and we hold no
// credentials for it. Those fall back to pass-through.
func githubTarball(raw, subdir string) (Source, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Host, "github.com") {
		return Source{}, false
	}
	m := githubRepoRe.FindStringSubmatch(u.Path)
	if m == nil {
		return Source{}, false
	}
	ref := u.Query().Get("ref")
	if ref == "" {
		// Without a ref we would be caching "whatever the default branch is
		// today" under an immutable module version.
		return Source{}, false
	}

	// The subdir carries over unchanged: repacking strips the tarball's wrapper
	// directory, so the served archive is shaped like the clone the source
	// described, and a "//modules/vpc" stays exactly that.
	return Source{
		URL: fmt.Sprintf("https://codeload.github.com/%s/%s/tar.gz/%s",
			url.PathEscape(m[1]), url.PathEscape(m[2]), url.PathEscape(ref)),
		Subdir:  subdir,
		Archive: "tar.gz",
		Repack:  true,
	}, true
}

// archiveType determines the go-getter archive type of u: the explicit
// "archive" query parameter wins, otherwise a known extension on the path. An
// empty result means we cannot tell, and must not re-host the bytes under a
// type we would be guessing at.
func archiveType(u *url.URL) string {
	if v := u.Query().Get("archive"); v != "" {
		for _, t := range archiveTypes {
			if v == t {
				return t
			}
		}
		return ""
	}
	path := strings.ToLower(u.Path)
	for _, t := range archiveTypes {
		if strings.HasSuffix(path, "."+t) {
			return t
		}
	}
	return ""
}

// RehostedGet builds the X-Terraform-Get value pointing at terrastrata's own
// archive endpoint. It is deliberately host-relative: Terraform resolves it
// against the download endpoint's URL, so terrastrata never needs to be told
// the external hostname or scheme it is reached under.
//
// The subdir is reattached verbatim — dropping it would leave Terraform in the
// archive's wrapper directory instead of the module root.
func RehostedGet(c Coordinates, s Source) string {
	return fmt.Sprintf("/v1/modules/%s/%s/%s/%s/archive%s?archive=%s",
		url.PathEscape(c.Namespace), url.PathEscape(c.Name), url.PathEscape(c.System),
		url.PathEscape(c.Version), s.Subdir, url.QueryEscape(s.Archive))
}
