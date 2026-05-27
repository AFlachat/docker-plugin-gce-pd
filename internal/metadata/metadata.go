// Package metadata is a small client for the GCE instance metadata server.
//
// It is intentionally dependency-free (no cloud.google.com/go/compute/metadata)
// so that we control the base URL, headers and timeouts, and can point the
// client at an httptest.Server in unit tests. The official library hardcodes
// the metadata host and is awkward to fake.
//
// See: https://cloud.google.com/compute/docs/metadata/overview
package metadata

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultBaseURL is the well-known address of the GCE metadata server. It is
// reachable only from inside a GCE VM (and resolves to a link-local address).
const DefaultBaseURL = "http://metadata.google.internal/computeMetadata/v1"

// metadataFlavorHeader and its value are required on every request; the server
// rejects requests without it. It also defends against DNS-rebinding tricks.
const (
	metadataFlavorHeader = "Metadata-Flavor"
	metadataFlavorValue  = "Google"
)

// ErrNotOnGCE is returned (wrapped) when the metadata server cannot be reached,
// which in practice means the plugin is not running on a GCE VM.
var ErrNotOnGCE = errors.New("metadata server unreachable: not running on GCE?")

// Client talks to the GCE metadata server.
type Client struct {
	baseURL string
	http    *http.Client
}

// Option configures a Client.
type Option func(*Client)

// WithBaseURL overrides the metadata server base URL. Used by tests to point at
// an httptest.Server. Trailing slashes are trimmed.
func WithBaseURL(u string) Option {
	return func(c *Client) { c.baseURL = strings.TrimRight(u, "/") }
}

// WithHTTPClient overrides the underlying HTTP client (timeouts, transport).
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.http = h }
}

// New builds a metadata Client. By default it targets the real metadata server
// with a short timeout so that "not on GCE" fails fast rather than hanging.
func New(opts ...Option) *Client {
	c := &Client{
		baseURL: DefaultBaseURL,
		http:    &http.Client{Timeout: 5 * time.Second},
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Metadata is a snapshot of the instance identity we care about.
type Metadata struct {
	ProjectID    string
	Zone         string // short form, e.g. "europe-west1-b"
	InstanceName string
}

// get fetches a single metadata key (path relative to baseURL) as a string.
func (c *Client) get(ctx context.Context, path string) (string, error) {
	url := c.baseURL + "/" + strings.TrimLeft(path, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("build metadata request for %q: %w", path, err)
	}
	req.Header.Set(metadataFlavorHeader, metadataFlavorValue)

	resp, err := c.http.Do(req)
	if err != nil {
		// Connection refused / DNS failure / timeout => almost certainly not on GCE.
		return "", fmt.Errorf("%w: %v", ErrNotOnGCE, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", fmt.Errorf("read metadata response for %q: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("metadata server returned %d for %q: %s",
			resp.StatusCode, path, strings.TrimSpace(string(body)))
	}
	return strings.TrimSpace(string(body)), nil
}

// ProjectID returns the numeric/string project ID of the current VM.
func (c *Client) ProjectID(ctx context.Context) (string, error) {
	return c.get(ctx, "project/project-id")
}

// Zone returns the short zone name (e.g. "europe-west1-b").
//
// The metadata server returns the fully-qualified form
// "projects/<num>/zones/<zone>"; we strip it down to the last segment, which is
// what the GCE API expects for zonal operations.
func (c *Client) Zone(ctx context.Context) (string, error) {
	raw, err := c.get(ctx, "instance/zone")
	if err != nil {
		return "", err
	}
	if i := strings.LastIndex(raw, "/"); i >= 0 {
		raw = raw[i+1:]
	}
	if raw == "" {
		return "", fmt.Errorf("metadata server returned empty zone")
	}
	return raw, nil
}

// InstanceName returns the short name of the current VM.
func (c *Client) InstanceName(ctx context.Context) (string, error) {
	return c.get(ctx, "instance/name")
}

// Fetch retrieves project ID, zone and instance name in one call. All three are
// required for the driver to operate, so any failure aborts the whole snapshot.
func (c *Client) Fetch(ctx context.Context) (Metadata, error) {
	var m Metadata
	var err error

	if m.ProjectID, err = c.ProjectID(ctx); err != nil {
		return Metadata{}, err
	}
	if m.Zone, err = c.Zone(ctx); err != nil {
		return Metadata{}, err
	}
	if m.InstanceName, err = c.InstanceName(ctx); err != nil {
		return Metadata{}, err
	}
	return m, nil
}

// OnGCE reports whether the metadata server is reachable, i.e. whether we are
// (almost certainly) running on a GCE VM. It is a cheap probe used at startup
// to fail fast with a clear message.
func (c *Client) OnGCE(ctx context.Context) bool {
	_, err := c.get(ctx, "project/project-id")
	return err == nil
}
