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
