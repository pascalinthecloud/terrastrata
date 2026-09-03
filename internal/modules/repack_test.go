package modules

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"strings"
	"testing"
)

// tarEntry is one file or directory to place in a test archive.
type tarEntry struct {
	name     string
	body     string
	typeflag byte
	linkname string
}

func makeTarGz(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for _, e := range entries {
		flag := e.typeflag
		if flag == 0 {
			flag = tar.TypeReg
		}
		hdr := &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body)), Typeflag: flag, Linkname: e.linkname}
		switch flag {
		case tar.TypeSymlink, tar.TypeLink:
			hdr.Size = 0
		case tar.TypeDir:
			hdr.Size = 0
			hdr.Mode = 0o755
		case tar.TypeXGlobalHeader:
			// archive/tar accepts nothing but PAXRecords on a global header.
			hdr = &tar.Header{
				Typeflag:   tar.TypeXGlobalHeader,
				Name:       e.name,
				PAXRecords: map[string]string{"comment": e.body},
				Format:     tar.FormatPAX,
			}
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("WriteHeader(%q): %v", e.name, err)
		}
		if flag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatalf("Write(%q): %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tw.Close: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gw.Close: %v", err)
	}
	return buf.Bytes()
}

// readTarGz returns a name -> body map of a gzipped tar's regular files, plus
// the names of every entry in order.
func readTarGz(t *testing.T, data []byte) (map[string]string, []string) {
	t.Helper()
	files, names, _ := readTarGzLinks(t, data)
	return files, names
}

// readTarGzLinks additionally returns a name -> target map of the link entries,
// so tests can assert how a symlink or hard link survived the repack.
func readTarGzLinks(t *testing.T, data []byte) (map[string]string, []string, map[string]string) {
	t.Helper()
	gr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	defer func() { _ = gr.Close() }()

	files := map[string]string{}
	links := map[string]string{}
	var names []string
	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar.Next: %v", err)
		}
		names = append(names, hdr.Name)
		if hdr.Typeflag == tar.TypeSymlink || hdr.Typeflag == tar.TypeLink {
			links[hdr.Name] = hdr.Linkname
		}
		if hdr.Typeflag == tar.TypeReg {
			body, err := io.ReadAll(tr)
			if err != nil {
				t.Fatalf("read %q: %v", hdr.Name, err)
			}
			files[hdr.Name] = string(body)
		}
	}
	return files, names, links
}

// This is the shape GitHub's codeload actually returns: a PAX global header
// followed by everything nested under one "REPO-REF/" directory.
func TestRepackStripRoot(t *testing.T) {
	in := makeTarGz(t, []tarEntry{
		{name: "pax_global_header", body: "52 comment=abc\n", typeflag: tar.TypeXGlobalHeader},
		{name: "repo-abc123/", typeflag: tar.TypeDir},
		{name: "repo-abc123/main.tf", body: "resource \"null_resource\" \"a\" {}\n"},
		{name: "repo-abc123/modules/", typeflag: tar.TypeDir},
		{name: "repo-abc123/modules/vpc/main.tf", body: "# vpc\n"},
	})

	var out bytes.Buffer
	n, err := repackStripRoot(&out, bytes.NewReader(in))
	if err != nil {
		t.Fatalf("repackStripRoot: %v", err)
	}
	if n != int64(out.Len()) {
		t.Errorf("reported size %d, wrote %d", n, out.Len())
	}

	files, names := readTarGz(t, out.Bytes())
	for _, name := range names {
		if strings.HasPrefix(name, "repo-abc123") {
			t.Errorf("entry %q still carries the wrapper directory", name)
		}
		if name == "pax_global_header" {
			t.Error("pax_global_header leaked into the repacked archive")
		}
	}
	if got, want := files["main.tf"], "resource \"null_resource\" \"a\" {}\n"; got != want {
		t.Errorf("main.tf = %q, want %q", got, want)
	}
	if got, want := files["modules/vpc/main.tf"], "# vpc\n"; got != want {
		t.Errorf("modules/vpc/main.tf = %q, want %q", got, want)
	}
}

func TestRepackStripRootRejectsBadArchives(t *testing.T) {
	cases := []struct {
		name    string
		entries []tarEntry
		want    string
	}{
		{
			name: "two top-level entries",
			entries: []tarEntry{
				{name: "a/main.tf", body: "x"},
				{name: "b/main.tf", body: "y"},
			},
			want: "more than one top-level entry",
		},
		{
			name:    "traversal escapes the root",
			entries: []tarEntry{{name: "../evil.tf", body: "x"}},
			want:    "escapes the root",
		},
		{
			name:    "absolute path",
			entries: []tarEntry{{name: "/etc/passwd", body: "x"}},
			want:    "escapes the root",
		},
		{
			name:    "empty archive",
			entries: nil,
			want:    "empty",
		},
		{
			name: "symlink escaping the root",
			entries: []tarEntry{
				{name: "repo-abc/main.tf", body: "x"},
				{name: "repo-abc/leak.tf", typeflag: tar.TypeSymlink, linkname: "../../../../etc/passwd"},
			},
			want: "escapes the root",
		},
		{
			name: "symlink to an absolute path",
			entries: []tarEntry{
				{name: "repo-abc/main.tf", body: "x"},
				{name: "repo-abc/leak.tf", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"},
			},
			want: "absolute path",
		},
		{
			name: "symlink escaping from a subdirectory",
			entries: []tarEntry{
				{name: "repo-abc/modules/vpc/main.tf", body: "x"},
				{name: "repo-abc/modules/vpc/leak.tf", typeflag: tar.TypeSymlink, linkname: "../../../../../secrets"},
			},
			want: "escapes the root",
		},
		{
			name: "hard link outside the archive root",
			entries: []tarEntry{
				{name: "repo-abc/main.tf", body: "x"},
				{name: "repo-abc/leak.tf", typeflag: tar.TypeLink, linkname: "other-repo/secret.tf"},
			},
			want: "outside the archive root",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := repackStripRoot(io.Discard, bytes.NewReader(makeTarGz(t, tc.entries)))
			if err == nil {
				t.Fatalf("repackStripRoot = nil error, want %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

// Links that stay inside the tree are legitimate — git tracks symlinks — so they
// survive, with a hard link's root-relative target stripped the same way names
// are. Dropping them would corrupt the module; leaving a hard link's target
// prefixed would point it at a path the repacked archive no longer contains.
func TestRepackStripRootKeepsInTreeLinks(t *testing.T) {
	in := makeTarGz(t, []tarEntry{
		{name: "repo-abc123/main.tf", body: "# root\n"},
		{name: "repo-abc123/modules/", typeflag: tar.TypeDir},
		{name: "repo-abc123/modules/link.tf", typeflag: tar.TypeSymlink, linkname: "../main.tf"},
		{name: "repo-abc123/hard.tf", typeflag: tar.TypeLink, linkname: "repo-abc123/main.tf"},
	})

	var out bytes.Buffer
	if _, err := repackStripRoot(&out, bytes.NewReader(in)); err != nil {
		t.Fatalf("repackStripRoot: %v", err)
	}

	_, _, links := readTarGzLinks(t, out.Bytes())
	if got, want := links["modules/link.tf"], "../main.tf"; got != want {
		t.Errorf("symlink target = %q, want %q (kept verbatim)", got, want)
	}
	if got, want := links["hard.tf"], "main.tf"; got != want {
		t.Errorf("hard link target = %q, want %q (wrapper stripped)", got, want)
	}
}

func TestRepackStripRootRejectsNonGzip(t *testing.T) {
	if _, err := repackStripRoot(io.Discard, strings.NewReader("not a gzip stream")); err == nil {
		t.Error("expected an error for a non-gzip body, got nil")
	}
}
