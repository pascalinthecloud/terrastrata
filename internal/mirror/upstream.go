package mirror

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// Upstream talks to a Terraform *provider registry* (registry.terraform.io) using
// the registry protocol. terrastrata translates these responses into the network
// *mirror* protocol it serves to clients.
type Upstream struct {
	base   string
	client *http.Client
	ua     string
}

// VersionMeta is one entry from the registry "list versions" response.
type VersionMeta struct {
	Version   string         `json:"version"`
	Platforms []PlatformMeta `json:"platforms"`
}

// PlatformMeta is an os/arch pair a provider version is published for.
type PlatformMeta struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
}

// DownloadMeta is the registry "find a package" response for one os/arch.
type DownloadMeta struct {
	OS          string `json:"os"`
	Arch        string `json:"arch"`
	Filename    string `json:"filename"`
	DownloadURL string `json:"download_url"`
	Shasum      string `json:"shasum"`
}

// NewUpstream constructs an Upstream client. base must be an absolute URL with no
// trailing slash (config.FromEnv guarantees this). userAgent identifies
// terrastrata to the registry.
func NewUpstream(base, userAgent string, timeout time.Duration) *Upstream {
	// Transport-level timeouts bound connection setup and the time to first byte
	// without capping the total body transfer (large zips on slow links). The
	// overall deadline is governed by the request context instead.
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
		base:   base,
		client: &http.Client{Transport: transport},
		ua:     userAgent,
	}
}

// ListVersions returns all published versions (with their platforms) for a
// provider via GET /v1/providers/:ns/:type/versions.
func (u *Upstream) ListVersions(ctx context.Context, c Coordinates) ([]VersionMeta, error) {
	endpoint := fmt.Sprintf("%s/v1/providers/%s/%s/versions",
		u.base, url.PathEscape(c.Namespace), url.PathEscape(c.Type))

	var body struct {
		Versions []VersionMeta `json:"versions"`
	}
	if err := u.getJSON(ctx, endpoint, &body); err != nil {
		return nil, err
	}
	return body.Versions, nil
}

// GetDownload resolves the package metadata (download URL + checksum) for one
// platform via GET /v1/providers/:ns/:type/:version/download/:os/:arch.
func (u *Upstream) GetDownload(ctx context.Context, c Coordinates, os, arch string) (DownloadMeta, error) {
	endpoint := fmt.Sprintf("%s/v1/providers/%s/%s/%s/download/%s/%s",
		u.base,
		url.PathEscape(c.Namespace), url.PathEscape(c.Type),
		url.PathEscape(c.Version), url.PathEscape(os), url.PathEscape(arch))

	var meta DownloadMeta
	if err := u.getJSON(ctx, endpoint, &meta); err != nil {
		return DownloadMeta{}, err
	}
	return meta, nil
}

// FetchZip streams a provider archive from an absolute download URL. The caller
// owns and must Close the returned reader. Only http/https URLs are permitted.
func (u *Upstream) FetchZip(ctx context.Context, downloadURL string) (io.ReadCloser, error) {
	parsed, err := url.Parse(downloadURL)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return nil, fmt.Errorf("upstream: refusing non-http download url %q", downloadURL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", u.ua)

	resp, err := u.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upstream: fetch zip: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("upstream: fetch zip: unexpected status %s", resp.Status)
	}
	return resp.Body, nil
}

// getJSON performs a GET and decodes a JSON body, mapping common upstream
// failures to descriptive errors. A 404 becomes ErrNotFound so callers can map
// it to a 404 for clients.
func (u *Upstream) getJSON(ctx context.Context, endpoint string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", u.ua)
	req.Header.Set("Accept", "application/json")

	resp, err := u.client.Do(req)
	if err != nil {
		return fmt.Errorf("upstream: GET %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		// Bound the metadata body to guard against a misbehaving upstream.
		limited := io.LimitReader(resp.Body, 8<<20) // 8 MiB
		if err := json.NewDecoder(limited).Decode(dst); err != nil {
			return fmt.Errorf("upstream: decode %s: %w", endpoint, err)
		}
		return nil
	case http.StatusNotFound:
		return ErrNotFound
	default:
		return fmt.Errorf("upstream: GET %s: unexpected status %s", endpoint, resp.Status)
	}
}
