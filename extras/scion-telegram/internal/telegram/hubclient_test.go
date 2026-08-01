package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingTransport is a test RoundTripper that records whether it was called
// and delegates to a base transport.
type recordingTransport struct {
	base  http.RoundTripper
	calls atomic.Int64
}

func (rt *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.calls.Add(1)
	if rt.base != nil {
		return rt.base.RoundTrip(req)
	}
	return http.DefaultTransport.RoundTrip(req)
}

func TestNewHTTPHubClient_UsesProvidedTransport(t *testing.T) {
	t.Run("custom transport is used for API calls", func(t *testing.T) {
		// Set up a fake Hub API server.
		hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(hubProjectsResponse{
				Projects: []hubProject{{ID: "p1", Name: "test", Slug: "test"}},
			})
		}))
		defer hub.Close()

		transport := &recordingTransport{base: hub.Client().Transport}
		httpClient := &http.Client{
			Timeout:   10 * time.Second,
			Transport: transport,
		}

		client := NewHTTPHubClient(hub.URL, "", "", httpClient)
		ctx := context.Background()

		// Verify transport is used when listing projects.
		projects, err := client.ListProjects(ctx)
		require.NoError(t, err)
		assert.Len(t, projects, 1)
		assert.Equal(t, int64(1), transport.calls.Load(),
			"ListProjects should use the custom transport (IAP transport)")
	})

	t.Run("custom transport is used for ListAgents", func(t *testing.T) {
		hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(hubAgentsResponse{
				Agents: []hubAgent{{Slug: "coder", Activity: "active"}},
			})
		}))
		defer hub.Close()

		transport := &recordingTransport{base: hub.Client().Transport}
		httpClient := &http.Client{
			Timeout:   10 * time.Second,
			Transport: transport,
		}

		client := NewHTTPHubClient(hub.URL, "", "", httpClient)
		ctx := context.Background()

		agents, err := client.ListAgents(ctx, "p1")
		require.NoError(t, err)
		assert.Len(t, agents, 1)
		assert.Equal(t, int64(1), transport.calls.Load(),
			"ListAgents should use the custom transport (IAP transport)")
	})
}

func TestNewHTTPHubClient_NilClient_PlainTransport(t *testing.T) {
	// Set up a fake Hub API server.
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(hubProjectsResponse{
			Projects: []hubProject{{ID: "p1", Name: "test", Slug: "test"}},
		})
	}))
	defer hub.Close()

	// Create hub client with nil httpClient (no IAP configured).
	client := NewHTTPHubClient(hub.URL, "", "", nil)
	hc := client.(*httpHubClient)

	// Should have a default timeout.
	assert.Equal(t, 10*time.Second, hc.httpClient.Timeout)

	// Should work without IAP transport.
	ctx := context.Background()
	projects, err := client.ListProjects(ctx)
	require.NoError(t, err)
	assert.Len(t, projects, 1)
	assert.Equal(t, "test", projects[0].Slug)
}

func TestNewHTTPHubClient_TransportPreserved(t *testing.T) {
	// Verify that the httpClient's transport is preserved as-is.
	transport := &recordingTransport{}
	httpClient := &http.Client{
		Timeout:   10 * time.Second,
		Transport: transport,
	}

	client := NewHTTPHubClient("http://example.com", "key", "broker-1", httpClient)
	hc := client.(*httpHubClient)

	assert.Same(t, transport, hc.httpClient.Transport,
		"httpClient transport should be the same object as provided")
}
