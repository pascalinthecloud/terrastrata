package config

import (
	"log/slog"
	"slices"
	"testing"
	"time"
)

func TestFromEnvDefaults(t *testing.T) {
	// t.Setenv ensures a clean, isolated environment per test.
	clearEnv(t)

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}

	if cfg.ListenAddr != DefaultListenAddr {
		t.Errorf("ListenAddr = %q, want %q", cfg.ListenAddr, DefaultListenAddr)
	}
	if cfg.CacheDir != DefaultCacheDir {
		t.Errorf("CacheDir = %q, want %q", cfg.CacheDir, DefaultCacheDir)
	}
	if cfg.UpstreamBase != DefaultUpstreamBase {
		t.Errorf("UpstreamBase = %q, want %q", cfg.UpstreamBase, DefaultUpstreamBase)
	}
	if cfg.UpstreamTimeout != DefaultUpstreamTimeout {
		t.Errorf("UpstreamTimeout = %v, want %v", cfg.UpstreamTimeout, DefaultUpstreamTimeout)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want info", cfg.LogLevel)
	}
	if cfg.S3.Enabled() {
		t.Error("S3 should be disabled by default")
	}
	if cfg.IndexTTL != DefaultIndexTTL {
		t.Errorf("IndexTTL = %v, want %v", cfg.IndexTTL, DefaultIndexTTL)
	}
	if cfg.MirrorHostname != "registry.terraform.io" {
		t.Errorf("MirrorHostname = %q, want registry.terraform.io", cfg.MirrorHostname)
	}
}

func TestFromEnvMirrorHostname(t *testing.T) {
	t.Run("derived from custom upstream", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("UPSTREAM_BASE", "https://registry.corp.example:8443")
		cfg, err := FromEnv()
		if err != nil {
			t.Fatalf("FromEnv: %v", err)
		}
		if cfg.MirrorHostname != "registry.corp.example:8443" {
			t.Errorf("MirrorHostname = %q, want registry.corp.example:8443", cfg.MirrorHostname)
		}
	})

	t.Run("explicit override wins", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("MIRROR_HOSTNAME", "registry.terraform.io")
		t.Setenv("UPSTREAM_BASE", "https://registry-proxy.corp.example")
		cfg, err := FromEnv()
		if err != nil {
			t.Fatalf("FromEnv: %v", err)
		}
		if cfg.MirrorHostname != "registry.terraform.io" {
			t.Errorf("MirrorHostname = %q, want registry.terraform.io", cfg.MirrorHostname)
		}
	})
}

func TestFromEnvIndexTTL(t *testing.T) {
	clearEnv(t)
	t.Setenv("INDEX_TTL", "30m")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.IndexTTL != 30*time.Minute {
		t.Errorf("IndexTTL = %v, want 30m", cfg.IndexTTL)
	}
}

func TestFromEnvIndexTTLDisabled(t *testing.T) {
	clearEnv(t)
	t.Setenv("INDEX_TTL", "0")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.IndexTTL != 0 {
		t.Errorf("IndexTTL = %v, want 0 (disabled)", cfg.IndexTTL)
	}
}

func TestFromEnvIndexTTLInvalid(t *testing.T) {
	for _, v := range []string{"soon", "-5m"} {
		t.Run(v, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("INDEX_TTL", v)
			if _, err := FromEnv(); err == nil {
				t.Fatalf("expected error for INDEX_TTL=%q", v)
			}
		})
	}
}

func TestFromEnvTrimsTrailingSlashes(t *testing.T) {
	clearEnv(t)
	t.Setenv("UPSTREAM_BASE", "https://example.com/")
	t.Setenv("S3_BUCKET", "b")
	t.Setenv("S3_ACCESS_KEY", "a")
	t.Setenv("S3_SECRET_KEY", "s")
	t.Setenv("S3_ENDPOINT", "https://s3.example.com/")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.UpstreamBase != "https://example.com" {
		t.Errorf("UpstreamBase = %q, want trailing slash trimmed", cfg.UpstreamBase)
	}
	if cfg.S3.Endpoint != "https://s3.example.com" {
		t.Errorf("S3.Endpoint = %q, want trailing slash trimmed", cfg.S3.Endpoint)
	}
}

func TestFromEnvS3BucketWithoutStaticCredentials(t *testing.T) {
	// No static keys means the AWS default credential chain: valid.
	clearEnv(t)
	t.Setenv("S3_BUCKET", "my-bucket")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if !cfg.S3.Enabled() {
		t.Error("S3 should be enabled with bucket alone (default credential chain)")
	}
}

func TestFromEnvS3RejectsPartialStaticCredentials(t *testing.T) {
	for _, partial := range []string{"S3_ACCESS_KEY", "S3_SECRET_KEY"} {
		clearEnv(t)
		t.Setenv("S3_BUCKET", "my-bucket")
		t.Setenv(partial, "only-one-half")

		if _, err := FromEnv(); err == nil {
			t.Errorf("expected error with only %s set", partial)
		}
	}
}

func TestFromEnvS3CredentialsWithoutBucket(t *testing.T) {
	clearEnv(t)
	t.Setenv("S3_ACCESS_KEY", "a")
	t.Setenv("S3_SECRET_KEY", "s")

	if _, err := FromEnv(); err == nil {
		t.Fatal("expected error when S3 credentials set without bucket")
	}
}

func TestFromEnvS3Enabled(t *testing.T) {
	clearEnv(t)
	t.Setenv("S3_BUCKET", "my-bucket")
	t.Setenv("S3_ACCESS_KEY", "a")
	t.Setenv("S3_SECRET_KEY", "s")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if !cfg.S3.Enabled() {
		t.Error("S3 should be enabled")
	}
	if cfg.S3.Prefix != DefaultS3Prefix {
		t.Errorf("S3.Prefix = %q, want default", cfg.S3.Prefix)
	}
}

func TestFromEnvInvalidS3Endpoint(t *testing.T) {
	clearEnv(t)
	t.Setenv("S3_BUCKET", "b")
	t.Setenv("S3_ACCESS_KEY", "a")
	t.Setenv("S3_SECRET_KEY", "s")
	t.Setenv("S3_ENDPOINT", "s3.de.io.cloud.ovh.net") // missing scheme

	if _, err := FromEnv(); err == nil {
		t.Fatal("expected error for scheme-less S3_ENDPOINT")
	}
}

func TestFromEnvValidS3Endpoint(t *testing.T) {
	clearEnv(t)
	t.Setenv("S3_BUCKET", "b")
	t.Setenv("S3_ACCESS_KEY", "a")
	t.Setenv("S3_SECRET_KEY", "s")
	t.Setenv("S3_ENDPOINT", "https://s3.de.io.cloud.ovh.net")

	if _, err := FromEnv(); err != nil {
		t.Fatalf("unexpected error for valid S3_ENDPOINT: %v", err)
	}
}

func TestFromEnvInvalidUpstream(t *testing.T) {
	clearEnv(t)
	t.Setenv("UPSTREAM_BASE", "not-a-url")

	if _, err := FromEnv(); err == nil {
		t.Fatal("expected error for invalid UPSTREAM_BASE")
	}
}

func TestFromEnvInvalidLogLevel(t *testing.T) {
	clearEnv(t)
	t.Setenv("LOG_LEVEL", "loud")

	if _, err := FromEnv(); err == nil {
		t.Fatal("expected error for invalid LOG_LEVEL")
	}
}

func TestFromEnvPrewarmDefaults(t *testing.T) {
	clearEnv(t)
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if len(cfg.PrewarmProviders) != 0 {
		t.Errorf("PrewarmProviders = %v, want empty", cfg.PrewarmProviders)
	}
	// Platforms default even when providers are empty (harmless).
	if len(cfg.PrewarmPlatforms) != 1 || cfg.PrewarmPlatforms[0] != DefaultPrewarmPlatform {
		t.Errorf("PrewarmPlatforms = %v, want [%s]", cfg.PrewarmPlatforms, DefaultPrewarmPlatform)
	}
}

func TestFromEnvPrewarmParsing(t *testing.T) {
	clearEnv(t)
	t.Setenv("PREWARM_PROVIDERS", " hashicorp/azurerm@3.110.0 , hashicorp/null ,, ")
	t.Setenv("PREWARM_PLATFORMS", "linux_amd64, darwin_arm64")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	wantProviders := []string{"hashicorp/azurerm@3.110.0", "hashicorp/null"}
	if !slices.Equal(cfg.PrewarmProviders, wantProviders) {
		t.Errorf("PrewarmProviders = %v, want %v", cfg.PrewarmProviders, wantProviders)
	}
	wantPlatforms := []string{"linux_amd64", "darwin_arm64"}
	if !slices.Equal(cfg.PrewarmPlatforms, wantPlatforms) {
		t.Errorf("PrewarmPlatforms = %v, want %v", cfg.PrewarmPlatforms, wantPlatforms)
	}
}

func TestParseByteSize(t *testing.T) {
	ok := map[string]int64{
		"":       0,
		"0":      0,
		"1024":   1024,
		"1kb":    1000,
		"1KiB":   1024,
		"512Mi":  512 << 20,
		"20GB":   20_000_000_000,
		"2GiB":   2 << 30,
		"1.5GiB": int64(1.5 * (1 << 30)),
	}
	for in, want := range ok {
		got, err := parseByteSize(in)
		if err != nil {
			t.Errorf("parseByteSize(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parseByteSize(%q) = %d, want %d", in, got, want)
		}
	}
	for _, bad := range []string{"abc", "-5", "10XB", "1.2.3G"} {
		if _, err := parseByteSize(bad); err == nil {
			t.Errorf("parseByteSize(%q): expected error", bad)
		}
	}
}

func TestFromEnvCacheMaxBytes(t *testing.T) {
	clearEnv(t)
	t.Setenv("CACHE_MAX_BYTES", "20GiB")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if cfg.CacheMaxBytes != 20<<30 {
		t.Errorf("CacheMaxBytes = %d, want %d", cfg.CacheMaxBytes, int64(20<<30))
	}
}

func TestParseLogLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"debug": slog.LevelDebug,
		"INFO":  slog.LevelInfo,
		"warn":  slog.LevelWarn,
		"error": slog.LevelError,
	}
	for in, want := range cases {
		got, err := parseLogLevel(in)
		if err != nil {
			t.Errorf("parseLogLevel(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parseLogLevel(%q) = %v, want %v", in, got, want)
		}
	}
}

// clearEnv unsets every configuration variable so each test starts from a known
// baseline regardless of the host environment. t.Setenv restores values on cleanup.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"LISTEN_ADDR", "CACHE_DIR", "UPSTREAM_BASE", "MIRROR_HOSTNAME", "AUTH_TOKEN", "LOG_LEVEL", "INDEX_TTL",
		"S3_BUCKET", "S3_PREFIX", "S3_ENDPOINT", "S3_REGION", "S3_ACCESS_KEY", "S3_SECRET_KEY",
		"PREWARM_PROVIDERS", "PREWARM_PLATFORMS", "CACHE_MAX_BYTES",
		"MODULES_ENABLED", "MODULES_UPSTREAM_BASE",
	} {
		t.Setenv(k, "")
	}
}

func TestFromEnvModules(t *testing.T) {
	t.Run("disabled by default", func(t *testing.T) {
		clearEnv(t)
		cfg, err := FromEnv()
		if err != nil {
			t.Fatalf("FromEnv: %v", err)
		}
		if cfg.Modules.Enabled {
			t.Error("module registry should be opt-in, but defaulted to enabled")
		}
	})

	t.Run("upstream defaults to the provider upstream", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("MODULES_ENABLED", "true")
		t.Setenv("UPSTREAM_BASE", "https://registry.corp.example/")
		cfg, err := FromEnv()
		if err != nil {
			t.Fatalf("FromEnv: %v", err)
		}
		if !cfg.Modules.Enabled {
			t.Error("Modules.Enabled = false, want true")
		}
		// Also asserts the trailing slash is trimmed, since upstream URLs are
		// built by concatenation.
		if want := "https://registry.corp.example"; cfg.Modules.UpstreamBase != want {
			t.Errorf("Modules.UpstreamBase = %q, want %q", cfg.Modules.UpstreamBase, want)
		}
	})

	t.Run("upstream overridable independently", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("MODULES_ENABLED", "1")
		t.Setenv("MODULES_UPSTREAM_BASE", "https://modules.corp.example")
		cfg, err := FromEnv()
		if err != nil {
			t.Fatalf("FromEnv: %v", err)
		}
		if cfg.Modules.UpstreamBase != "https://modules.corp.example" {
			t.Errorf("Modules.UpstreamBase = %q", cfg.Modules.UpstreamBase)
		}
		if cfg.UpstreamBase != DefaultUpstreamBase {
			t.Errorf("provider UpstreamBase changed to %q", cfg.UpstreamBase)
		}
	})

	t.Run("invalid boolean fails fast", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("MODULES_ENABLED", "yes")
		if _, err := FromEnv(); err == nil {
			t.Error("expected an error for MODULES_ENABLED=yes, got nil")
		}
	})

	t.Run("invalid upstream fails fast when enabled", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("MODULES_ENABLED", "true")
		t.Setenv("MODULES_UPSTREAM_BASE", "not-a-url")
		if _, err := FromEnv(); err == nil {
			t.Error("expected an error for a scheme-less MODULES_UPSTREAM_BASE, got nil")
		}
	})

	t.Run("invalid upstream ignored when disabled", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("MODULES_UPSTREAM_BASE", "not-a-url")
		if _, err := FromEnv(); err != nil {
			t.Errorf("unexpected error while modules are disabled: %v", err)
		}
	})
}

func TestParseUpstreams(t *testing.T) {
	t.Run("single value behaves as before", func(t *testing.T) {
		clearEnv(t)
		cfg, err := FromEnv()
		if err != nil {
			t.Fatalf("FromEnv: %v", err)
		}
		if len(cfg.Upstreams) != 1 {
			t.Fatalf("Upstreams = %d entries, want 1", len(cfg.Upstreams))
		}
		if cfg.Upstreams[0].Hostname != "registry.terraform.io" || cfg.Upstreams[0].Base != DefaultUpstreamBase {
			t.Errorf("Upstreams[0] = %+v", cfg.Upstreams[0])
		}
		// The convenience fields must keep pointing at the primary.
		if cfg.UpstreamBase != DefaultUpstreamBase || cfg.MirrorHostname != "registry.terraform.io" {
			t.Errorf("UpstreamBase=%q MirrorHostname=%q", cfg.UpstreamBase, cfg.MirrorHostname)
		}
	})

	t.Run("hostnames derived from URL hosts", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("UPSTREAM_BASE", "https://registry.terraform.io,https://registry.opentofu.org/")
		cfg, err := FromEnv()
		if err != nil {
			t.Fatalf("FromEnv: %v", err)
		}
		want := []UpstreamConfig{
			{Hostname: "registry.terraform.io", Base: "https://registry.terraform.io"},
			// Also asserts the trailing slash is trimmed.
			{Hostname: "registry.opentofu.org", Base: "https://registry.opentofu.org"},
		}
		if !slices.Equal(cfg.Upstreams, want) {
			t.Errorf("Upstreams = %+v, want %+v", cfg.Upstreams, want)
		}
	})

	t.Run("explicit hostname=url form", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("UPSTREAM_BASE", "https://registry.terraform.io,registry.corp.example=https://nexus.corp/repo/tf")
		cfg, err := FromEnv()
		if err != nil {
			t.Fatalf("FromEnv: %v", err)
		}
		got := cfg.Upstreams[1]
		if got.Hostname != "registry.corp.example" || got.Base != "https://nexus.corp/repo/tf" {
			t.Errorf("Upstreams[1] = %+v", got)
		}
	})

	t.Run("a URL containing = is not split", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("UPSTREAM_BASE", "https://nexus.corp/repo?token=abc")
		cfg, err := FromEnv()
		if err != nil {
			t.Fatalf("FromEnv: %v", err)
		}
		if cfg.Upstreams[0].Base != "https://nexus.corp/repo?token=abc" {
			t.Errorf("Base = %q, want the URL intact", cfg.Upstreams[0].Base)
		}
	})

	t.Run("MIRROR_HOSTNAME overrides the first entry only", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("UPSTREAM_BASE", "https://registry.terraform.io,https://registry.opentofu.org")
		t.Setenv("MIRROR_HOSTNAME", "tf.internal")
		cfg, err := FromEnv()
		if err != nil {
			t.Fatalf("FromEnv: %v", err)
		}
		if cfg.Upstreams[0].Hostname != "tf.internal" {
			t.Errorf("Upstreams[0].Hostname = %q, want tf.internal", cfg.Upstreams[0].Hostname)
		}
		if cfg.Upstreams[1].Hostname != "registry.opentofu.org" {
			t.Errorf("Upstreams[1].Hostname = %q, want it untouched", cfg.Upstreams[1].Hostname)
		}
		if cfg.MirrorHostname != "tf.internal" {
			t.Errorf("MirrorHostname = %q", cfg.MirrorHostname)
		}
	})
}

func TestParseUpstreamsRejectsBadInput(t *testing.T) {
	cases := []struct {
		name     string
		upstream string
		hostname string
	}{
		{"malformed URL", "not-a-url", ""},
		{"missing scheme", "registry.terraform.io", ""},
		{"unsupported scheme", "ftp://registry.terraform.io", ""},
		{"empty url after hostname", "registry.corp.example=", ""},
		// A silently shadowed upstream would be a nasty bug, so duplicates fail fast.
		{"duplicate hostname", "https://registry.terraform.io,https://registry.terraform.io", ""},
		{"duplicate differing only in case", "https://registry.terraform.io,Registry.Terraform.IO=https://mirror.corp", ""},
		// A hostname the router could never match is a misconfiguration.
		{"hostname with a slash", "reg/istry=https://registry.terraform.io", ""},
		{"hostname with a space", "bad host=https://registry.terraform.io", ""},
		// The override must not be able to smuggle one in either.
		{"MIRROR_HOSTNAME collides with a later entry", "https://registry.terraform.io,https://registry.opentofu.org", "registry.opentofu.org"},
		{"MIRROR_HOSTNAME is not routable", "https://registry.terraform.io", "not a host"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("UPSTREAM_BASE", tc.upstream)
			if tc.hostname != "" {
				t.Setenv("MIRROR_HOSTNAME", tc.hostname)
			}
			if _, err := FromEnv(); err == nil {
				t.Errorf("FromEnv(UPSTREAM_BASE=%q) = nil error, want a failure", tc.upstream)
			}
		})
	}
}
