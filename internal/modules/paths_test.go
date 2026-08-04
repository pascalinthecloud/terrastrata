package modules

import "testing"

func TestValidate(t *testing.T) {
	valid := []struct{ ns, name, system string }{
		{"hashicorp", "consul", "aws"},
		{"terraform-aws-modules", "vpc", "aws"},
		{"my_org", "my-module", "google"},
	}
	for _, v := range valid {
		if _, err := Validate(v.ns, v.name, v.system); err != nil {
			t.Errorf("Validate(%q,%q,%q) unexpected error: %v", v.ns, v.name, v.system, err)
		}
	}

	invalid := []struct {
		desc            string
		ns, name, systm string
	}{
		{"traversal in namespace", "..", "vpc", "aws"},
		{"traversal in name", "ns", "..", "aws"},
		{"traversal in system", "ns", "vpc", ".."},
		{"slash in name", "ns", "a/b", "aws"},
		{"dotdot inside name", "ns", "a..b", "aws"},
		{"backslash", "ns", "a\\b", "aws"},
		{"null byte", "ns", "vpc\x00", "aws"},
		{"empty namespace", "", "vpc", "aws"},
		{"empty name", "ns", "", "aws"},
		{"empty system", "ns", "vpc", ""},
	}
	for _, v := range invalid {
		if _, err := Validate(v.ns, v.name, v.systm); err == nil {
			t.Errorf("%s: expected error, got nil", v.desc)
		}
	}
}

func TestWithVersion(t *testing.T) {
	base, err := Validate("ns", "vpc", "aws")
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	for _, v := range []string{"1.0.0", "5.1.2", "1.0.0-rc1", "2.0.0+meta"} {
		if _, err := base.withVersion(v); err != nil {
			t.Errorf("withVersion(%q): unexpected error: %v", v, err)
		}
	}
	for _, v := range []string{"", "1.0", "../1.0.0", "1..0.0", "1.0.0/x", "latest"} {
		if _, err := base.withVersion(v); err == nil {
			t.Errorf("withVersion(%q): expected error", v)
		}
	}
}

// Cache keys must stay under the "_modules" root so they can never collide with
// a provider entry, which is keyed by hostname.
func TestCacheKeys(t *testing.T) {
	c, err := Validate("terraform-aws-modules", "vpc", "aws")
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	c, err = c.withVersion("5.1.2")
	if err != nil {
		t.Fatalf("withVersion: %v", err)
	}

	cases := []struct{ got, want string }{
		{VersionsCacheKey(c), "_modules/terraform-aws-modules/vpc/aws/versions.json"},
		{LocationCacheKey(c), "_modules/terraform-aws-modules/vpc/aws/5.1.2/location.json"},
		{ArchiveCacheKey(c), "_modules/terraform-aws-modules/vpc/aws/5.1.2/archive"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("cache key = %q, want %q", tc.got, tc.want)
		}
	}
}
