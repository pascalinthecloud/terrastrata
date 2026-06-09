package config

import (
	"log/slog"
	"testing"
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

func TestFromEnvS3RequiresCredentials(t *testing.T) {
	clearEnv(t)
	t.Setenv("S3_BUCKET", "my-bucket")

	if _, err := FromEnv(); err == nil {
		t.Fatal("expected error when S3_BUCKET set without credentials")
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
		"LISTEN_ADDR", "CACHE_DIR", "UPSTREAM_BASE", "AUTH_TOKEN", "LOG_LEVEL",
		"S3_BUCKET", "S3_PREFIX", "S3_ENDPOINT", "S3_REGION", "S3_ACCESS_KEY", "S3_SECRET_KEY",
	} {
		t.Setenv(k, "")
	}
}
