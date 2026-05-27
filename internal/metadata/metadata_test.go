package metadata

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// newTestServer spins up a fake metadata server. The handler enforces the
// Metadata-Flavor header (like the real one) and serves a fixed routing table.
func newTestServer(t *testing.T, routes map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(metadataFlavorHeader) != metadataFlavorValue {
			http.Error(w, "missing Metadata-Flavor: Google", http.StatusForbidden)
			return
		}
		body, ok := routes[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetch(t *testing.T) {
	srv := newTestServer(t, map[string]string{
		"/computeMetadata/v1/project/project-id": "my-project-123",
		// The real server returns the fully-qualified zone path.
		"/computeMetadata/v1/instance/zone": "projects/424242/zones/europe-west1-b",
		"/computeMetadata/v1/instance/name": "worker-vm-0",
	})

	c := New(WithBaseURL(srv.URL + "/computeMetadata/v1"))
	got, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}

	want := Metadata{
		ProjectID:    "my-project-123",
		Zone:         "europe-west1-b", // stripped to short form
		InstanceName: "worker-vm-0",
	}
	if got != want {
		t.Errorf("Fetch() = %+v, want %+v", got, want)
	}
}

func TestZoneStripsPrefix(t *testing.T) {
	cases := map[string]string{
		"projects/424242/zones/us-central1-a": "us-central1-a",
		"us-central1-a":                       "us-central1-a", // already short
	}
	for raw, want := range cases {
		srv := newTestServer(t, map[string]string{
			"/computeMetadata/v1/instance/zone": raw,
		})
		c := New(WithBaseURL(srv.URL + "/computeMetadata/v1"))
		got, err := c.Zone(context.Background())
		if err != nil {
			t.Fatalf("Zone(%q) error = %v", raw, err)
		}
		if got != want {
			t.Errorf("Zone(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestRequiresMetadataFlavorHeader(t *testing.T) {
	// Server that rejects anything without the header (newTestServer does this).
	srv := newTestServer(t, map[string]string{
		"/computeMetadata/v1/project/project-id": "p",
	})

	// Build a client whose transport strips the header to simulate a bad client.
	c := New(WithBaseURL(srv.URL+"/computeMetadata/v1"), WithHTTPClient(&http.Client{
		Transport: stripHeaderTransport{},
		Timeout:   5 * time.Second,
	}))

	if _, err := c.ProjectID(context.Background()); err == nil {
		t.Fatal("expected error when Metadata-Flavor header is stripped, got nil")
	}
}

type stripHeaderTransport struct{}

func (stripHeaderTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r.Header.Del(metadataFlavorHeader)
	return http.DefaultTransport.RoundTrip(r)
}

func TestNotOnGCE(t *testing.T) {
	// Point at an address that refuses connections immediately.
	c := New(
		WithBaseURL("http://127.0.0.1:1"), // port 1: nothing listens
		WithHTTPClient(&http.Client{Timeout: 500 * time.Millisecond}),
	)

	_, err := c.Fetch(context.Background())
	if err == nil {
		t.Fatal("expected error when metadata server is unreachable")
	}
	if !errors.Is(err, ErrNotOnGCE) {
		t.Errorf("error = %v, want it to wrap ErrNotOnGCE", err)
	}

	if c.OnGCE(context.Background()) {
		t.Error("OnGCE() = true, want false when unreachable")
	}
}

func TestNon200Status(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	c := New(WithBaseURL(srv.URL + "/computeMetadata/v1"))
	_, err := c.ProjectID(context.Background())
	if err == nil {
		t.Fatal("expected error on 500 status")
	}
	// A 500 means we *did* reach a server, so it must NOT be classified as not-on-GCE.
	if errors.Is(err, ErrNotOnGCE) {
		t.Errorf("a 500 response should not be reported as ErrNotOnGCE: %v", err)
	}
}
