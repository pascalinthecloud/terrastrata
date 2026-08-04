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
	"strconv"
	"strings"
	"time"

	"github.com/pascalinthecloud/terrastrata/internal/pathsafe"
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
	ListenAddr string
	CacheDir   string

	// Upstreams are the registries this mirror serves, in configured order.
	// Each is addressed by its own {hostname} path segment; a request for any
	// hostname not listed here is rejected. Always holds at least one entry.
	Upstreams []UpstreamConfig

	// UpstreamBase and MirrorHostname describe the *primary* (first) upstream.
	// They remain as convenience fields because several call sites only need a
	// sensible default: the module registry's upstream and prewarm's default
	// hostname for entries that omit one.
	UpstreamBase   string
	MirrorHostname string

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

	// CacheMaxBytes is the size budget for the local filesystem cache. When the
	// cache exceeds it, least-recently-used entries are evicted. Zero disables
	// eviction (unbounded growth).
	CacheMaxBytes int64

	Modules ModulesConfig

	S3 S3Config
}

// UpstreamConfig is one registry the mirror serves.
type UpstreamConfig struct {
	// Hostname is the {hostname} path segment clients address this registry by.
	// It defaults to the host of Base, and is set explicitly when the two differ
	// (for example a private registry reached at a path on a shared host).
	Hostname string
	// Base is the upstream registry base URL, without a trailing slash.
	Base string
}

// ModulesConfig holds the optional module registry settings. Module support is
// opt-in because enabling it adds routes (/.well-known/terraform.json and
// /v1/modules/) that change how a client discovers the service.
type ModulesConfig struct {
	Enabled bool
	// UpstreamBase is the module registry to cache. Defaults to UpstreamBase,
	// since the public registry serves both protocols on one host.
	UpstreamBase string
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

	upstreams, err := parseUpstreams(os.Getenv("UPSTREAM_BASE"), os.Getenv("MIRROR_HOSTNAME"))
	if err != nil {
		return Config{}, err
	}
	cfg.Upstreams = upstreams
	cfg.UpstreamBase = upstreams[0].Base
	cfg.MirrorHostname = upstreams[0].Hostname

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

	maxBytes, err := parseByteSize(os.Getenv("CACHE_MAX_BYTES"))
	if err != nil {
		return Config{}, err
	}
	cfg.CacheMaxBytes = maxBytes

	enabled, err := parseBool("MODULES_ENABLED", os.Getenv("MODULES_ENABLED"))
	if err != nil {
		return Config{}, err
	}
	cfg.Modules = ModulesConfig{
		Enabled:      enabled,
		UpstreamBase: strings.TrimRight(envOr("MODULES_UPSTREAM_BASE", cfg.UpstreamBase), "/"),
	}

	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// parseUpstreams parses UPSTREAM_BASE, a comma-separated list of registries this
// mirror serves. Each entry is either a bare URL — whose host becomes the
// {hostname} clients address it by — or an explicit "hostname=url" pair for when
// the two differ (a private registry living at a path, say). An empty value
// yields the default public registry, so the single-upstream configuration this
// grew out of keeps behaving exactly as before.
//
// mirrorHostname is the legacy MIRROR_HOSTNAME knob; it overrides the *first*
// entry's hostname, which is what it always meant when only one existed.
func parseUpstreams(raw, mirrorHostname string) ([]UpstreamConfig, error) {
	entries := splitList(raw)
	if len(entries) == 0 {
		entries = []string{DefaultUpstreamBase}
	}

	out := make([]UpstreamConfig, 0, len(entries))
	for _, entry := range entries {
		hostname, rawURL := "", entry
		// Split on a leading "hostname=" only. Testing for "://" before the "="
		// keeps a URL that merely contains one (in a query string) intact.
		if i := strings.Index(entry, "="); i > -1 && !strings.Contains(entry[:i], "://") {
			hostname = strings.TrimSpace(entry[:i])
			rawURL = strings.TrimSpace(entry[i+1:])
		}

		base := strings.TrimRight(rawURL, "/")
		u, err := url.Parse(base)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("config: UPSTREAM_BASE entry %q is not a valid absolute URL", entry)
		}
		if u.Scheme != "https" && u.Scheme != "http" {
			return nil, fmt.Errorf("config: UPSTREAM_BASE entry %q: scheme %q must be http or https", entry, u.Scheme)
		}
		if hostname == "" {
			hostname = u.Host
		}
		out = append(out, UpstreamConfig{Hostname: hostname, Base: base})
	}

	if mirrorHostname != "" {
		out[0].Hostname = mirrorHostname
	}

	// Validate hostnames after the override, so MIRROR_HOSTNAME cannot smuggle
	// in a name the router could never match or one that collides with another
	// upstream.
	seen := make(map[string]string, len(out))
	for _, up := range out {
		if err := pathsafe.Hostname("MIRROR_HOSTNAME/UPSTREAM_BASE hostname", up.Hostname); err != nil {
			return nil, fmt.Errorf("config: %w", err)
		}
		key := strings.ToLower(up.Hostname)
		if prev, dup := seen[key]; dup {
			return nil, fmt.Errorf("config: hostname %q is configured for two upstreams (%s and %s)", up.Hostname, prev, up.Base)
		}
		seen[key] = up.Base
	}
	return out, nil
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

	// Upstreams are parsed and validated by parseUpstreams before validate runs.
	if len(c.Upstreams) == 0 {
		return errors.New("config: at least one UPSTREAM_BASE entry is required")
	}

	if c.Modules.Enabled {
		u, err := url.Parse(c.Modules.UpstreamBase)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("config: MODULES_UPSTREAM_BASE %q is not a valid absolute URL", c.Modules.UpstreamBase)
		}
		if u.Scheme != "https" && u.Scheme != "http" {
			return fmt.Errorf("config: MODULES_UPSTREAM_BASE scheme %q must be http or https", u.Scheme)
		}
	}

	// Static S3 credentials are both-or-neither: a partial pair is a
	// misconfiguration we fail on at startup instead of on the first async
	// upload. Both empty means the AWS default credential chain (environment,
	// shared config, IRSA, instance profile) is used.
	if c.S3.Enabled() {
		if (c.S3.AccessKey == "") != (c.S3.SecretKey == "") {
			return errors.New("config: S3_ACCESS_KEY and S3_SECRET_KEY must be set together (leave both empty to use the AWS default credential chain)")
		}
		if c.S3.Region == "" {
			return errors.New("config: S3_BUCKET is set but S3_REGION missing")
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

// byteSuffixes maps size unit suffixes to their multiplier. Decimal (kB, MB, …)
// and binary (KiB, MiB, …) units are both supported, case-insensitively.
var byteSuffixes = []struct {
	suffix string
	mult   int64
}{
	{"kib", 1 << 10}, {"mib", 1 << 20}, {"gib", 1 << 30}, {"tib", 1 << 40},
	{"ki", 1 << 10}, {"mi", 1 << 20}, {"gi", 1 << 30}, {"ti", 1 << 40},
	{"kb", 1e3}, {"mb", 1e6}, {"gb", 1e9}, {"tb", 1e12},
	{"k", 1e3}, {"m", 1e6}, {"g", 1e9}, {"t", 1e12},
	{"b", 1},
}

// parseByteSize parses a human-friendly size like "20GB", "512Mi", or a plain
// byte count. An empty value yields 0 (eviction disabled). Negative is rejected.
func parseByteSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	lower := strings.ToLower(s)
	mult := int64(1)
	num := lower
	for _, suf := range byteSuffixes {
		if strings.HasSuffix(lower, suf.suffix) {
			mult = suf.mult
			num = strings.TrimSpace(lower[:len(lower)-len(suf.suffix)])
			break
		}
	}
	val, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0, fmt.Errorf("config: invalid CACHE_MAX_BYTES %q", s)
	}
	if val < 0 {
		return 0, fmt.Errorf("config: CACHE_MAX_BYTES %q must not be negative", s)
	}
	return int64(val * float64(mult)), nil
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

// parseBool parses a boolean environment value ("true"/"false"/"1"/"0", per
// strconv.ParseBool). Empty means false. Anything else is an error rather than a
// silent false, so MODULES_ENABLED=yes fails loudly instead of quietly disabling
// a feature the operator asked for.
func parseBool(key, s string) (bool, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return false, nil
	}
	v, err := strconv.ParseBool(s)
	if err != nil {
		return false, fmt.Errorf("config: invalid %s %q (want true or false)", key, s)
	}
	return v, nil
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
