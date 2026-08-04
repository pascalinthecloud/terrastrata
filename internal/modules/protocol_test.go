package modules

import "testing"

func TestSplitSubdir(t *testing.T) {
	cases := []struct {
		name           string
		src            string
		wantBase, want string
	}{
		{
			name:     "registry tarball with glob subdir",
			src:      "https://api.github.com/repos/o/r/tarball/v1.0.0//*?archive=tar.gz",
			wantBase: "https://api.github.com/repos/o/r/tarball/v1.0.0?archive=tar.gz",
			want:     "//*",
		},
		{
			name:     "nested subdir",
			src:      "https://example.com/mod.tar.gz//modules/vpc",
			wantBase: "https://example.com/mod.tar.gz",
			want:     "//modules/vpc",
		},
		{
			name:     "no subdir",
			src:      "https://example.com/mod.tar.gz",
			wantBase: "https://example.com/mod.tar.gz",
			want:     "",
		},
		{
			name:     "scheme slashes are not a subdir",
			src:      "https://example.com",
			wantBase: "https://example.com",
			want:     "",
		},
		{
			name:     "forced getter keeps its prefix",
			src:      "git::https://example.com/repo.git//sub",
			wantBase: "git::https://example.com/repo.git",
			want:     "//sub",
		},
		{
			name:     "query only, no subdir",
			src:      "https://example.com/mod?archive=zip",
			wantBase: "https://example.com/mod?archive=zip",
			want:     "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, subdir := splitSubdir(tc.src)
			if base != tc.wantBase {
				t.Errorf("base = %q, want %q", base, tc.wantBase)
			}
			if subdir != tc.want {
				t.Errorf("subdir = %q, want %q", subdir, tc.want)
			}
		})
	}
}

func TestParseSourceCacheable(t *testing.T) {
	cases := []struct {
		name                        string
		src                         string
		wantURL, wantSub, wantArchv string
	}{
		{
			name:      "public registry github tarball",
			src:       "https://api.github.com/repos/o/r/tarball/v1.0.0//*?archive=tar.gz",
			wantURL:   "https://api.github.com/repos/o/r/tarball/v1.0.0?archive=tar.gz",
			wantSub:   "//*",
			wantArchv: "tar.gz",
		},
		{
			name:      "archive type inferred from extension",
			src:       "https://example.com/module.tar.gz",
			wantURL:   "https://example.com/module.tar.gz",
			wantArchv: "tar.gz",
		},
		{
			name:      "zip extension",
			src:       "https://example.com/module.zip//sub",
			wantURL:   "https://example.com/module.zip",
			wantSub:   "//sub",
			wantArchv: "zip",
		},
		{
			name:      "forced https getter is unwrapped",
			src:       "https::https://example.com/module.tgz",
			wantURL:   "https://example.com/module.tgz",
			wantArchv: "tgz",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseSource(tc.src, false)
			if !ok {
				t.Fatalf("ParseSource(%q) = not cacheable, want cacheable", tc.src)
			}
			if got.URL != tc.wantURL {
				t.Errorf("URL = %q, want %q", got.URL, tc.wantURL)
			}
			if got.Subdir != tc.wantSub {
				t.Errorf("Subdir = %q, want %q", got.Subdir, tc.wantSub)
			}
			if got.Archive != tc.wantArchv {
				t.Errorf("Archive = %q, want %q", got.Archive, tc.wantArchv)
			}
		})
	}
}

func TestParseSourceNotCacheable(t *testing.T) {
	// Each of these must fall back to pass-through rather than being fetched:
	// we either have no getter for it or cannot name its archive type.
	for _, src := range []string{
		"git::https://github.com/o/r.git",                // GitHub but no ref to pin
		"git::ssh://git@github.com/o/r.git//modules/vpc", // ssh implies private; no credentials
		"git::https://git.corp.example/o/r.git?ref=abc",  // not GitHub: no tarball endpoint
		"git::https://github.com/only-owner?ref=abc",     // not an OWNER/REPO path
		"s3::https://s3.amazonaws.com/bucket/mod.tar.gz",
		"hg::https://example.com/repo",
		"https://example.com/module",             // no archive type
		"https://example.com/module?archive=rar", // unknown archive type
		"http://example.com/module.tar.gz",       // plain http against https upstream
		"file:///local/path/module.tar.gz",       // no host
		"./relative/module.tar.gz",               // not absolute
	} {
		if _, ok := ParseSource(src, false); ok {
			t.Errorf("ParseSource(%q) = cacheable, want pass-through", src)
		}
	}
}

// Every module on the live public registry resolves to a git:: source, so this
// translation is what makes module content cacheable at all.
func TestParseSourceGitHubGitSource(t *testing.T) {
	const sha = "8f5239c3689d08631363fcff392b50a6bb1a33f1"
	cases := []struct {
		name             string
		src              string
		wantURL, wantSub string
	}{
		{
			name:    "repo root",
			src:     "git::https://github.com/claranet/terraform-azurerm-regions?ref=" + sha,
			wantURL: "https://codeload.github.com/claranet/terraform-azurerm-regions/tar.gz/" + sha,
			// No subdir: repacking strips the tarball's wrapper directory, so
			// the served archive is shaped like the clone the source described.
			wantSub: "",
		},
		{
			name:    "dot-git suffix stripped",
			src:     "git::https://github.com/o/r.git?ref=" + sha,
			wantURL: "https://codeload.github.com/o/r/tar.gz/" + sha,
			wantSub: "",
		},
		{
			name:    "subdir carried over unchanged",
			src:     "git::https://github.com/o/r//modules/vpc?ref=" + sha,
			wantURL: "https://codeload.github.com/o/r/tar.gz/" + sha,
			wantSub: "//modules/vpc",
		},
		{
			name:    "tag ref",
			src:     "git::https://github.com/o/r?ref=v1.2.3",
			wantURL: "https://codeload.github.com/o/r/tar.gz/v1.2.3",
			wantSub: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseSource(tc.src, false)
			if !ok {
				t.Fatalf("ParseSource(%q) = not cacheable", tc.src)
			}
			if got.URL != tc.wantURL {
				t.Errorf("URL = %q, want %q", got.URL, tc.wantURL)
			}
			if got.Subdir != tc.wantSub {
				t.Errorf("Subdir = %q, want %q", got.Subdir, tc.wantSub)
			}
			if got.Archive != "tar.gz" {
				t.Errorf("Archive = %q, want tar.gz", got.Archive)
			}
			if !got.Repack {
				t.Error("Repack = false; the codeload wrapper directory must be stripped")
			}
		})
	}
}

func TestParseSourceAllowHTTP(t *testing.T) {
	const src = "http://localhost:9000/module.tar.gz"
	if _, ok := ParseSource(src, false); ok {
		t.Error("plain http accepted against an https upstream")
	}
	got, ok := ParseSource(src, true)
	if !ok {
		t.Fatal("plain http rejected even though the upstream is http")
	}
	if got.URL != src {
		t.Errorf("URL = %q, want %q", got.URL, src)
	}
}

func TestRehostedGet(t *testing.T) {
	c := Coordinates{Namespace: "terraform-aws-modules", Name: "vpc", System: "aws", Version: "5.1.2"}

	// The subdir must survive verbatim: we re-serve the upstream bytes, so
	// dropping "//*" would leave Terraform in the tarball's wrapper directory
	// instead of the module root.
	got := RehostedGet(c, Source{Subdir: "//*", Archive: "tar.gz"})
	want := "/v1/modules/terraform-aws-modules/vpc/aws/5.1.2/archive//*?archive=tar.gz"
	if got != want {
		t.Errorf("RehostedGet = %q, want %q", got, want)
	}

	got = RehostedGet(c, Source{Archive: "zip"})
	want = "/v1/modules/terraform-aws-modules/vpc/aws/5.1.2/archive?archive=zip"
	if got != want {
		t.Errorf("RehostedGet without subdir = %q, want %q", got, want)
	}
}

func TestResolveLocation(t *testing.T) {
	const endpoint = "https://registry.terraform.io/v1/modules/ns/name/aws/1.0.0/download"

	// Absolute values pass through untouched.
	abs := "https://api.github.com/repos/o/r/tarball/v1//*?archive=tar.gz"
	if got, err := resolveLocation(endpoint, abs); err != nil || got != abs {
		t.Errorf("resolveLocation(absolute) = %q, %v; want unchanged", got, err)
	}

	// Relative values resolve against the download endpoint, and the go-getter
	// subdir must survive URL resolution.
	got, err := resolveLocation(endpoint, "/archives/mod.tar.gz//*?archive=tar.gz")
	if err != nil {
		t.Fatalf("resolveLocation: %v", err)
	}
	want := "https://registry.terraform.io/archives/mod.tar.gz//*?archive=tar.gz"
	if got != want {
		t.Errorf("resolveLocation(relative) = %q, want %q", got, want)
	}
}
