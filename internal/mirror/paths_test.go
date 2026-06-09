package mirror

import "testing"

func TestValidateProvider(t *testing.T) {
	valid := []struct{ host, ns, typ string }{
		{"registry.terraform.io", "hashicorp", "null"},
		{"registry.terraform.io", "hashicorp", "azurerm"},
		{"localhost:8080", "my-org", "my_provider"},
	}
	for _, v := range valid {
		if _, err := ValidateProvider(v.host, v.ns, v.typ); err != nil {
			t.Errorf("ValidateProvider(%q,%q,%q) unexpected error: %v", v.host, v.ns, v.typ, err)
		}
	}

	invalid := []struct {
		name          string
		host, ns, typ string
	}{
		{"traversal in namespace", "registry.terraform.io", "..", "null"},
		{"slash in type", "registry.terraform.io", "hashicorp", "a/b"},
		{"dotdot in type", "registry.terraform.io", "hashicorp", "a..b"},
		{"empty namespace", "registry.terraform.io", "", "null"},
		{"null byte", "registry.terraform.io", "hashi\x00", "null"},
		{"backslash", "registry.terraform.io", "hashicorp", "a\\b"},
		{"bad hostname", "not a host", "hashicorp", "null"},
	}
	for _, v := range invalid {
		if _, err := ValidateProvider(v.host, v.ns, v.typ); err == nil {
			t.Errorf("%s: expected error, got nil", v.name)
		}
	}
}

func TestWithVersion(t *testing.T) {
	base, _ := ValidateProvider("registry.terraform.io", "hashicorp", "null")

	for _, v := range []string{"3.2.0", "1.0.0-beta1", "2.0.0+meta"} {
		if _, err := base.withVersion(v); err != nil {
			t.Errorf("withVersion(%q): unexpected error: %v", v, err)
		}
	}
	for _, v := range []string{"", "3.2", "../3.2.0", "3..2.0", "3.2.0/x", "latest"} {
		if _, err := base.withVersion(v); err == nil {
			t.Errorf("withVersion(%q): expected error", v)
		}
	}
}

func TestWithDownload(t *testing.T) {
	base, _ := ValidateProvider("registry.terraform.io", "hashicorp", "null")
	c, _ := base.withVersion("3.2.0")

	if _, err := c.withDownload("linux_amd64", "terraform-provider-null_3.2.0_linux_amd64.zip"); err != nil {
		t.Errorf("valid download: unexpected error: %v", err)
	}

	bad := []struct{ platform, filename string }{
		{"linux", "x.zip"},               // platform missing arch
		{"linux_amd64", "x.txt"},         // not a zip
		{"linux_amd64", "../escape.zip"}, // traversal
		{"linux_amd64", "a/b.zip"},       // slash
		{"linux/amd64", "x.zip"},         // slash in platform
	}
	for _, b := range bad {
		if _, err := c.withDownload(b.platform, b.filename); err == nil {
			t.Errorf("withDownload(%q,%q): expected error", b.platform, b.filename)
		}
	}
}
