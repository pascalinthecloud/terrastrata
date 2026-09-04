package modules

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// defaultModulesPath is the module API prefix assumed when an upstream does not
// answer service discovery. It is what registry.terraform.io and everything
// modelled on it serves.
const defaultModulesPath = "/v1/modules/"

// discoveryRetryAfter bounds how often a failed discovery is retried, so a
// registry that does not serve /.well-known/terraform.json costs one extra
// request every few minutes rather than one per module request.
const discoveryRetryAfter = 5 * time.Minute

// Upstream talks to a Terraform module registry using the module registry
// protocol. Unlike the provider path — where terrastrata translates the registry
// protocol into the network mirror protocol — modules have no mirror protocol,
// so terrastrata speaks the same protocol in both directions and acts as a
// caching registry.
type Upstream struct {
	base   string
	client *http.Client
	ua     string

	// allowHTTP permits plain-http archive URLs. Derived from the base URL's
	// scheme: an operator who configured an http:// upstream (local dev) has
	// opted out of TLS; against an https registry a plain-http source would be a
	// downgrade and is refused.
	allowHTTP bool

	log *slog.Logger

	// Service discovery state. The module registry protocol says a client reads
	// /.well-known/terraform.json and uses the modules.v1 path it advertises;
	// hardcoding /v1/modules/ works for registry.terraform.io but 404s against a
	// private registry (Artifactory, Nexus) that advertises its own path.
	//
	// Discovery is lazy rather than done at startup: a registry that is briefly
	// unreachable must not stop terrastrata from starting or from serving what it
	// already has cached.
	mu          sync.Mutex
	modulesPath string    // resolved absolute prefix, ending in "/"; empty until discovered
	lastAttempt time.Time // when discovery last failed, for the retry cooldown
}

// NewUpstream constructs an Upstream client. base must be an absolute URL with
// no trailing slash (config.FromEnv guarantees this).
func NewUpstream(base, userAgent string, timeout time.Duration, log *slog.Logger) *Upstream {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: timeout,
		ExpectContinueTimeout: 1 * time.Second,
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Upstream{
		base:      base,
		client:    &http.Client{Transport: transport},
		ua:        userAgent,
		allowHTTP: strings.HasPrefix(base, "http://"),
		log:       log,
	}
}

// modulesAPI returns the absolute prefix for module API requests, ending in "/".
//
// The first call performs service discovery against the upstream; the answer is
// cached for the life of the process, since a registry moving its API path is not
// something that happens under a running client. A failure falls back to
// defaultModulesPath and is retried no more often than discoveryRetryAfter, so an
// upstream without a discovery document stays cheap.
func (u *Upstream) modulesAPI(ctx context.Context) string {
	u.mu.Lock()
	defer u.mu.Unlock()

	if u.modulesPath != "" {
		return u.modulesPath
	}
	if !u.lastAttempt.IsZero() && time.Since(u.lastAttempt) < discoveryRetryAfter {
		return u.base + defaultModulesPath
	}

	path, err := u.discoverModulesAPI(ctx)
	if err != nil {
		u.lastAttempt = time.Now()
		u.log.Warn("module service discovery failed, assuming the default API path",
			"upstream", u.base, "path", defaultModulesPath, "err", err)
		return u.base + defaultModulesPath
	}
	u.modulesPath = path
	u.log.Info("module API path resolved by service discovery", "upstream", u.base, "path", path)
	return path
}

// discoverModulesAPI reads /.well-known/terraform.json and resolves the
// modules.v1 service it advertises. The value may be relative (the usual case)
// or absolute, and is resolved against the discovery document's own URL, as the
// protocol requires and as Terraform's client does.
func (u *Upstream) discoverModulesAPI(ctx context.Context) (string, error) {
	endpoint := u.base + "/.well-known/terraform.json"
	req, err := u.newRequest(ctx, endpoint)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := u.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("modules: GET %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("modules: GET %s: unexpected status %s", endpoint, resp.Status)
	}

	var doc struct {
		Modules string `json:"modules.v1"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return "", fmt.Errorf("modules: decode %s: %w", endpoint, err)
	}
	if doc.Modules == "" {
		return "", fmt.Errorf("modules: %s advertises no modules.v1 service", endpoint)
	}

	base, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("modules: parse %s: %w", endpoint, err)
	}
	ref, err := url.Parse(doc.Modules)
	if err != nil {
		return "", fmt.Errorf("modules: parse modules.v1 %q: %w", doc.Modules, err)
	}
	resolved := base.ResolveReference(ref)
	// A discovery document must not talk us into a plaintext API when the
	// upstream itself is https; that would be a downgrade an attacker on the
	// path could ask for.
	if resolved.Scheme != "https" && (resolved.Scheme != "http" || !u.allowHTTP) {
		return "", fmt.Errorf("modules: modules.v1 %q is not https", doc.Modules)
	}
	out := resolved.String()
	if !strings.HasSuffix(out, "/") {
		out += "/"
	}
	return out, nil
}

// AllowHTTP reports whether plain-http archive sources are tolerated.
func (u *Upstream) AllowHTTP() bool { return u.allowHTTP }

// ListVersions returns the raw versions document for a module via
// GET {modules.v1}/:namespace/:name/:system/versions. The body is returned as
// received (after a well-formedness check) because terrastrata serves the same
// protocol it consumes.
func (u *Upstream) ListVersions(ctx context.Context, c Coordinates) ([]byte, error) {
	endpoint := fmt.Sprintf("%s%s/%s/%s/versions", u.modulesAPI(ctx),
		url.PathEscape(c.Namespace), url.PathEscape(c.Name), url.PathEscape(c.System))

	req, err := u.newRequest(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := u.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("modules: GET %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, ErrNotFound
	default:
		return nil, fmt.Errorf("modules: GET %s: unexpected status %s", endpoint, resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20)) // 8 MiB
	if err != nil {
		return nil, fmt.Errorf("modules: read %s: %w", endpoint, err)
	}
	// Validate before caching so a proxy error page never becomes a cached
	// "versions" document.
	var parsed VersionsResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("modules: decode %s: %w", endpoint, err)
	}
	if len(parsed.Modules) == 0 {
		return nil, fmt.Errorf("modules: %s returned no modules", endpoint)
	}
	return body, nil
}

// Location resolves a module version's source via
// GET {modules.v1}/:namespace/:name/:system/:version/download, which answers with
// the X-Terraform-Get header. The spec prescribes 204 No Content; some
// registries answer 200, so both are accepted.
//
// A relative header value is resolved against the download endpoint's URL, as
// the protocol requires and as Terraform's own client does.
func (u *Upstream) Location(ctx context.Context, c Coordinates) (string, error) {
	endpoint := fmt.Sprintf("%s%s/%s/%s/%s/download", u.modulesAPI(ctx),
		url.PathEscape(c.Namespace), url.PathEscape(c.Name),
		url.PathEscape(c.System), url.PathEscape(c.Version))

	req, err := u.newRequest(ctx, endpoint)
	if err != nil {
		return "", err
	}

	resp, err := u.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("modules: GET %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()
	// Drain so the connection can be reused; a 204 body is empty anyway.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))

	switch resp.StatusCode {
	case http.StatusOK, http.StatusNoContent:
	case http.StatusNotFound:
		return "", ErrNotFound
	default:
		return "", fmt.Errorf("modules: GET %s: unexpected status %s", endpoint, resp.Status)
	}

	get := resp.Header.Get("X-Terraform-Get")
	if get == "" {
		return "", fmt.Errorf("modules: %s returned no X-Terraform-Get header", endpoint)
	}
	return resolveLocation(endpoint, get)
}

// resolveLocation turns a possibly-relative X-Terraform-Get value into an
// absolute source. Relative values are resolved against the download endpoint,
// per the protocol.
func resolveLocation(endpoint, get string) (string, error) {
	if !strings.HasPrefix(get, "/") && !strings.HasPrefix(get, "./") && !strings.HasPrefix(get, "../") {
		return get, nil
	}
	base, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("modules: parse download endpoint %q: %w", endpoint, err)
	}
	ref, err := url.Parse(get)
	if err != nil {
		return "", fmt.Errorf("modules: parse X-Terraform-Get %q: %w", get, err)
	}
	return base.ResolveReference(ref).String(), nil
}

// FetchArchive streams a module archive from an absolute URL. The caller owns
// and must Close the returned reader. Only https is permitted, plus plain http
// when the upstream base itself is http.
func (u *Upstream) FetchArchive(ctx context.Context, archiveURL string) (io.ReadCloser, error) {
	parsed, err := url.Parse(archiveURL)
	if err != nil {
		return nil, fmt.Errorf("modules: refusing archive url %q (https required)", archiveURL)
	}
	if allowed := parsed.Scheme == "https" || (parsed.Scheme == "http" && u.allowHTTP); !allowed {
		return nil, fmt.Errorf("modules: refusing archive url %q (https required)", archiveURL)
	}

	req, err := u.newRequest(ctx, archiveURL)
	if err != nil {
		return nil, err
	}
	resp, err := u.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("modules: fetch archive: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("modules: fetch archive: unexpected status %s", resp.Status)
	}
	return resp.Body, nil
}

func (u *Upstream) newRequest(ctx context.Context, endpoint string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", u.ua)
	return req, nil
}
