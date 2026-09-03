package modules

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
)

// repackStripRoot copies a gzipped tar from r to w, removing the single
// top-level directory every entry sits under.
//
// GitHub's codeload tarballs wrap the repository in a "REPO-REF/" directory that
// a git clone does not have. The go-getter "//*" subdir glob exists for exactly
// this, but Terraform's module installer does not expand it for registry
// modules — it records the literal path ".terraform/modules/<name>/*" and then
// fails with "Unreadable module subdirectory". Stripping the wrapper here means
// the archive terrastrata serves looks like a clone, so no glob is needed and
// any subdir the source asked for stays a plain relative path.
//
// Deriving the wrapper name from REPO and REF would be simpler but wrong:
// GitHub strips a leading "v" from tag refs, so "?ref=v1.2.3" yields
// "repo-1.2.3". Reading the actual entry names is robust for tags and commit
// SHAs alike.
//
// It returns the number of bytes written.
func repackStripRoot(w io.Writer, r io.Reader) (int64, error) {
	gr, err := gzip.NewReader(r)
	if err != nil {
		return 0, fmt.Errorf("modules: read gzip: %w", err)
	}
	defer func() { _ = gr.Close() }()

	counter := &countingWriter{w: w}
	gw := gzip.NewWriter(counter)
	tw := tar.NewWriter(gw)
	tr := tar.NewReader(gr)

	var root string
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("modules: read tar: %w", err)
		}

		// PAX extended-header records are tar metadata, not files. GitHub
		// tarballs start with a "pax_global_header" entry; archive/tar already
		// applies these to the following header, so they are dropped here rather
		// than mistaken for a second top-level entry.
		if hdr.Typeflag == tar.TypeXGlobalHeader || hdr.Typeflag == tar.TypeXHeader {
			continue
		}

		name := path.Clean(strings.TrimPrefix(hdr.Name, "./"))
		if name == "." {
			continue
		}
		// Refuse anything that would escape the extraction root. Terraform
		// unpacks what we serve, so a malicious archive must not pass through
		// us unexamined.
		if path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return 0, fmt.Errorf("modules: archive entry %q escapes the root", hdr.Name)
		}

		first, rest, _ := strings.Cut(name, "/")
		if root == "" {
			root = first
		}
		if first != root {
			return 0, fmt.Errorf("modules: archive has more than one top-level entry (%q and %q)", root, first)
		}
		if rest == "" {
			// The wrapper directory itself disappears.
			continue
		}

		// Link entries carry a second path — the target — which must be checked
		// too. Terraform extracts what we serve, and the module protocol
		// publishes no checksum, so a link pointing out of the tree is the one
		// way a malicious module could reach files outside its own directory on
		// the client.
		if err := stripLinkTarget(hdr, root, rest); err != nil {
			return 0, err
		}

		hdr.Name = rest
		if err := tw.WriteHeader(hdr); err != nil {
			return 0, fmt.Errorf("modules: write tar header: %w", err)
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := io.Copy(tw, tr); err != nil { //nolint:gosec // G110: input is already size-capped by the caller
				return 0, fmt.Errorf("modules: write tar entry: %w", err)
			}
		}
	}

	if root == "" {
		return 0, errors.New("modules: archive is empty")
	}
	if err := tw.Close(); err != nil {
		return 0, fmt.Errorf("modules: close tar: %w", err)
	}
	if err := gw.Close(); err != nil {
		return 0, fmt.Errorf("modules: close gzip: %w", err)
	}
	return counter.n, nil
}

// stripLinkTarget validates (and for hard links rewrites) a link entry's
// target. name is the entry's path with the wrapper directory already removed.
//
//   - A symlink target is relative to the link's own directory, and is kept
//     verbatim as long as it resolves inside the tree.
//   - A hard link target is relative to the archive root, so it still carries
//     the wrapper prefix every name was just stripped of, and must be stripped
//     the same way or it would point at a path this archive no longer contains.
//
// Anything resolving outside the tree — or an absolute target — fails the whole
// repack rather than being silently dropped: a module that ships one is not an
// archive we want to re-serve as our own.
func stripLinkTarget(hdr *tar.Header, root, name string) error {
	switch hdr.Typeflag {
	case tar.TypeSymlink:
		target := hdr.Linkname
		if path.IsAbs(target) {
			return fmt.Errorf("modules: archive symlink %q targets an absolute path %q", hdr.Name, target)
		}
		//nolint:gosec // G305: this *is* the traversal check — the joined path is
		// only tested for escape, never opened or written.
		if escapes(path.Join(path.Dir(name), target)) {
			return fmt.Errorf("modules: archive symlink %q escapes the root (target %q)", hdr.Name, target)
		}
	case tar.TypeLink:
		target := path.Clean(strings.TrimPrefix(hdr.Linkname, "./"))
		if path.IsAbs(target) || escapes(target) {
			return fmt.Errorf("modules: archive hard link %q escapes the root (target %q)", hdr.Name, hdr.Linkname)
		}
		first, rest, _ := strings.Cut(target, "/")
		if first != root || rest == "" {
			return fmt.Errorf("modules: archive hard link %q targets %q outside the archive root", hdr.Name, hdr.Linkname)
		}
		hdr.Linkname = rest
	}
	return nil
}

// escapes reports whether a cleaned, root-relative path leaves the tree.
func escapes(p string) bool {
	return p == ".." || strings.HasPrefix(p, "../")
}

// countingWriter counts the bytes written through it.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// countingReader counts the bytes read through it, so a repack can enforce the
// size cap against the bytes actually pulled from upstream rather than the
// (smaller) bytes it produces.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
