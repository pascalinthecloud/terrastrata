// Package config loads and validates terrastrata's runtime configuration.
//
// All configuration is sourced from environment variables so the binary stays
// twelve-factor and container-friendly. Construct a Config with FromEnv, which
// fails fast on inconsistent input rather than surfacing errors later at request
// time.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"time"
)

// Default values for optional settings. Exposed as constants so tests and docs
// can reference them without duplicating literals.
const (
	DefaultListenAddr   = ":8080"
	DefaultCacheDir     = "/cache"
	DefaultUpstreamBase = "https://registry.terraform.io"
	DefaultS3Prefix     = "tf-mirror"
	DefaultS3Region     = "us-east-1"
	DefaultLogLevel     = "info"

	// DefaultPrewarmPlatform is the os_arch warmed when PREWARM_PLATFORMS is unset.
	DefaultPrewarmPlatform = "linux_amd64"

	// DefaultUpstreamTimeout bounds a single upstream registry request. Provider
	// zips are tens of MB, so this is generous without being unbounded.
	DefaultUpstreamTimeout = 60 * time.Second

	// DefaultIndexTTL is how long a cached provider versions index is served
	// before it is revalidated against upstream, so newly published versions
	// become visible. Archives and zips are immutable per version and are never
	// expired.
	DefaultIndexTTL = 10 * time.Minute
)

// Config is the fully validated runtime configuration. Treat it as immutable
// once returned by FromEnv.
type Config struct {
	ListenAddr   string
	CacheDir     string
	UpstreamBase string

	// AuthToken, when non-empty, enables bearer-token authentication on the
	// mirror endpoints. Empty means auth is disabled (the default internal mode).
	AuthToken string

	LogLevel        slog.Level
	UpstreamTimeout time.Duration

	// IndexTTL is the freshness window for the versions index. Zero disables
	// expiry (cache forever).
	IndexTTL time.Duration

	// PrewarmProviders lists providers to warm into the cache at startup. Each
	// entry is "[host/]namespace/type[@version]"; a bare provider warms only its
	// versions index, while an @version also warms that version's archives and
	// zips (for PrewarmPlatforms). Empty disables pre-warming.
	PrewarmProviders []string
	// PrewarmPlatforms is the os_arch list to warm zips for (default linux_amd64).
	PrewarmPlatforms []string

	S3 S3Config
}

// S3Config holds the optional durable cache backend settings. It is only active
// when Bucket is non-empty.
type S3Config struct {
	Bucket    string
	Prefix    string
	Endpoint  string
	Region    string
	AccessKey string
	SecretKey string
}

// Enabled reports whether the durable S3 cache layer should be used.
func (s S3Config) Enabled() bool { return s.Bucket != "" }

// FromEnv builds a Config from the process environment, applying defaults and
// validating the result. It returns a descriptive error if the configuration is
// internally inconsistent (for example, an S3 bucket without credentials).
func FromEnv() (Config, error) {
	cfg := Config{
		ListenAddr:      envOr("LISTEN_ADDR", DefaultListenAddr),
		CacheDir:        envOr("CACHE_DIR", DefaultCacheDir),
		UpstreamBase:    strings.TrimRight(envOr("UPSTREAM_BASE", DefaultUpstreamBase), "/"),
		AuthToken:       os.Getenv("AUTH_TOKEN"),
		UpstreamTimeout: DefaultUpstreamTimeout,
		S3: S3Config{
			Bucket:    os.Getenv("S3_BUCKET"),
			Prefix:    envOr("S3_PREFIX", DefaultS3Prefix),
			Endpoint:  strings.TrimRight(os.Getenv("S3_ENDPOINT"), "/"),
			Region:    envOr("S3_REGION", DefaultS3Region),
			AccessKey: os.Getenv("S3_ACCESS_KEY"),
			SecretKey: os.Getenv("S3_SECRET_KEY"),
		},
	}

	level, err := parseLogLevel(envOr("LOG_LEVEL", DefaultLogLevel))
	if err != nil {
		return Config{}, err
	}
	cfg.LogLevel = level

	ttl, err := parseIndexTTL(os.Getenv("INDEX_TTL"))
	if err != nil {
		return Config{}, err
	}
	cfg.IndexTTL = ttl

	cfg.PrewarmProviders = splitList(os.Getenv("PREWARM_PROVIDERS"))
	cfg.PrewarmPlatforms = splitList(os.Getenv("PREWARM_PLATFORMS"))
	if len(cfg.PrewarmPlatforms) == 0 {
		cfg.PrewarmPlatforms = []string{DefaultPrewarmPlatform}
	}

	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// parseIndexTTL parses the INDEX_TTL duration. An empty value selects the
// default; "0" (or any zero duration) disables expiry. Negative is rejected.
func parseIndexTTL(s string) (time.Duration, error) {
	if s == "" {
		return DefaultIndexTTL, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("config: invalid INDEX_TTL %q: %w", s, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("config: INDEX_TTL %q must not be negative", s)
	}
	return d, nil
}

func (c Config) validate() error {
	if c.CacheDir == "" {
		return errors.New("config: CACHE_DIR must not be empty")
	}

	u, err := url.Parse(c.UpstreamBase)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("config: UPSTREAM_BASE %q is not a valid absolute URL", c.UpstreamBase)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("config: UPSTREAM_BASE scheme %q must be http or https", u.Scheme)
	}

	// S3 is all-or-nothing: enabling the bucket requires credentials so we fail
	// at startup instead of on the first async upload.
	if c.S3.Enabled() {
		var missing []string
		if c.S3.AccessKey == "" {
			missing = append(missing, "S3_ACCESS_KEY")
		}
		if c.S3.SecretKey == "" {
			missing = append(missing, "S3_SECRET_KEY")
		}
		if c.S3.Region == "" {
			missing = append(missing, "S3_REGION")
		}
		if len(missing) > 0 {
			return fmt.Errorf("config: S3_BUCKET is set but %s missing", strings.Join(missing, ", "))
		}
		// Validate the custom endpoint at startup rather than failing on the first
		// upload: a scheme-less value would otherwise be accepted here and only
		// rejected later by the AWS SDK inside a swallowed async error.
		if c.S3.Endpoint != "" {
			u, err := url.Parse(c.S3.Endpoint)
			if err != nil || u.Scheme == "" || u.Host == "" {
				return fmt.Errorf("config: S3_ENDPOINT %q is not a valid absolute URL", c.S3.Endpoint)
			}
			if u.Scheme != "https" && u.Scheme != "http" {
				return fmt.Errorf("config: S3_ENDPOINT scheme %q must be http or https", u.Scheme)
			}
		}
	} else if c.S3.AccessKey != "" || c.S3.SecretKey != "" || c.S3.Endpoint != "" {
		return errors.New("config: S3 credentials/endpoint set but S3_BUCKET is empty")
	}

	return nil
}

// splitList parses a comma-separated environment value into a trimmed,
// empty-free slice. An empty input yields a nil slice.
func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func parseLogLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("config: invalid LOG_LEVEL %q (want debug|info|warn|error)", s)
	}
}
