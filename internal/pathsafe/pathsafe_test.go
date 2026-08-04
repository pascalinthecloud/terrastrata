package pathsafe

import (
	"regexp"
	"testing"
)

// permissive matches anything, so these cases prove the traversal and separator
// rejection happens in Validate itself and does not merely fall out of whatever
// pattern a caller happens to pass.
var permissive = regexp.MustCompile(`^.*$`)

func TestValidateRejectsTraversalRegardlessOfPattern(t *testing.T) {
	for _, value := range []string{
		"",
		"..",
		"../etc/passwd",
		"a..b",
		"a/b",
		"a\\b",
		"a\x00b",
		"/absolute",
		"trailing/",
	} {
		if err := Validate("field", value, permissive); err == nil {
			t.Errorf("Validate(%q) = nil, want error", value)
		}
	}

	for _, value := range []string{"ok", "with-hyphen", "with_underscore", "1.2.3"} {
		if err := Validate("field", value, permissive); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", value, err)
		}
	}
}

func TestNamedValidators(t *testing.T) {
	cases := []struct {
		name string
		fn   func(string, string) error
		good []string
		bad  []string
	}{
		{
			name: "Hostname",
			fn:   Hostname,
			good: []string{"registry.terraform.io", "localhost:8080", "example"},
			bad:  []string{"not a host", "-leading.hyphen", "host/path"},
		},
		{
			name: "Name",
			fn:   Name,
			good: []string{"hashicorp", "terraform-aws-modules", "my_module", "a1"},
			bad:  []string{"-lead", "trail-", "has.dot", "has space"},
		},
		{
			name: "Version",
			fn:   Version,
			good: []string{"1.0.0", "3.110.0", "1.0.0-rc1", "2.0.0+meta"},
			bad:  []string{"1.0", "v1.0.0", "latest", "1..0.0"},
		},
		{
			name: "Platform",
			fn:   Platform,
			good: []string{"linux_amd64", "darwin_arm64"},
			bad:  []string{"linux", "Linux_AMD64", "linux/amd64"},
		},
		{
			name: "ZipFilename",
			fn:   ZipFilename,
			good: []string{"terraform-provider-null_3.2.0_linux_amd64.zip"},
			bad:  []string{"x.txt", "../escape.zip", ".zip", "a/b.zip"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, v := range tc.good {
				if err := tc.fn("field", v); err != nil {
					t.Errorf("%s(%q) = %v, want nil", tc.name, v, err)
				}
			}
			for _, v := range tc.bad {
				if err := tc.fn("field", v); err == nil {
					t.Errorf("%s(%q) = nil, want error", tc.name, v)
				}
			}
		})
	}
}
