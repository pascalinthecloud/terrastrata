package modules

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

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
}

// NewUpstream constructs an Upstream client. base must be an absolute URL with
// no trailing slash (config.FromEnv guarantees this).
func NewUpstream(base, userAgent string, timeout time.Duration) *Upstream {
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
	return &Upstream{
		base:      base,
		client:    &http.Client{Transport: transport},
		ua:        userAgent,
		allowHTTP: strings.HasPrefix(base, "http://"),
	}
}

// AllowHTTP reports whether plain-http archive sources are tolerated.
func (u *Upstream) AllowHTTP() bool { return u.allowHTTP }

// ListVersions returns the raw versions document for a module via
// GET /v1/modules/:namespace/:name/:system/versions. The body is returned as
// received (after a well-formedness check) because terrastrata serves the same
// protocol it consumes.
func (u *Upstream) ListVersions(ctx context.Context, c Coordinates) ([]byte, error) {
	endpoint := fmt.Sprintf("%s/v1/modules/%s/%s/%s/versions",
		u.base, url.PathEscape(c.Namespace), url.PathEscape(c.Name), url.PathEscape(c.System))

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
// GET /v1/modules/:namespace/:name/:system/:version/download, which answers with
// the X-Terraform-Get header. The spec prescribes 204 No Content; some
// registries answer 200, so both are accepted.
//
// A relative header value is resolved against the download endpoint's URL, as
// the protocol requires and as Terraform's own client does.
func (u *Upstream) Location(ctx context.Context, c Coordinates) (string, error) {
	endpoint := fmt.Sprintf("%s/v1/modules/%s/%s/%s/%s/download",
		u.base, url.PathEscape(c.Namespace), url.PathEscape(c.Name),
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
