package discord

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

// countingTransport is a test RoundTripper that counts calls and delegates
// to a base transport. Used to verify that IAP-wrapped transports are
// actually used on all HTTP paths.
type countingTransport struct {
	base  http.RoundTripper
	calls atomic.Int64
}

func (ct *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ct.calls.Add(1)
	if ct.base != nil {
		return ct.base.RoundTrip(req)
	}
	return http.DefaultTransport.RoundTrip(req)
}

func TestNewHTTPHubClient_TransportInheritance(t *testing.T) {
	t.Run("longHTTPClient inherits transport from httpClient", func(t *testing.T) {
		// Create a custom transport that records calls.
		transport := &countingTransport{}

		httpClient := &http.Client{
			Timeout:   15 * time.Second,
			Transport: transport,
		}

		client := NewHTTPHubClient("http://example.com", "key", "broker-1", httpClient)
		hc := client.(*httpHubClient)

		// Verify the longHTTPClient has the same transport.
		assert.Same(t, transport, hc.longHTTPClient.Transport,
			"longHTTPClient should inherit the Transport from httpClient")

		// Verify the longHTTPClient has no global timeout.
		assert.Zero(t, hc.longHTTPClient.Timeout,
			"longHTTPClient should have no global timeout")

		// Verify the main httpClient retains its timeout.
		assert.Equal(t, 15*time.Second, hc.httpClient.Timeout,
			"httpClient should keep its original timeout")
	})

	t.Run("nil httpClient gets default with nil transport", func(t *testing.T) {
		client := NewHTTPHubClient("http://example.com", "key", "broker-1", nil)
		hc := client.(*httpHubClient)

		// Default httpClient should have 15s timeout.
		assert.Equal(t, 15*time.Second, hc.httpClient.Timeout)

		// longHTTPClient should have nil transport (uses http.DefaultTransport).
		assert.Nil(t, hc.longHTTPClient.Transport,
			"longHTTPClient transport should be nil when httpClient has nil transport")

		// longHTTPClient should have no timeout.
		assert.Zero(t, hc.longHTTPClient.Timeout)
	})
}

func TestHTTPHubClient_IAPTransport_UsedOnAllPaths(t *testing.T) {
	// Set up a fake Hub API server.
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/projects":
			json.NewEncoder(w).Encode(hubProjectsResponse{
				Projects: []hubProject{{ID: "p1", Name: "test", Slug: "test"}},
			})
		case "/api/v1/projects/p1/agents":
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(hubCreateAgentResponse{
				Agent: struct {
					Slug string `json:"slug"`
					Name string `json:"name"`
				}{Slug: "agent-1", Name: "Agent 1"},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer hub.Close()

	transport := &countingTransport{base: hub.Client().Transport}
	httpClient := &http.Client{
		Timeout:   15 * time.Second,
		Transport: transport,
	}

	client := NewHTTPHubClient(hub.URL, "", "", httpClient)
	ctx := context.Background()

	// Test ListProjects (uses httpClient).
	_, err := client.ListProjects(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), transport.calls.Load(),
		"ListProjects should use the custom transport")

	// Test CreateAgent (uses longHTTPClient).
	_, err = client.CreateAgent(ctx, "p1", CreateAgentRequest{
		Name:     "Agent 1",
		Template: "default",
	}, "")
	require.NoError(t, err)
	assert.Equal(t, int64(2), transport.calls.Load(),
		"CreateAgent should also use the custom transport via longHTTPClient")
}

func TestHTTPHubClient_PlainClient_WhenNoIAP(t *testing.T) {
	// Set up a fake Hub API server.
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(hubProjectsResponse{
			Projects: []hubProject{{ID: "p1", Name: "test", Slug: "test"}},
		})
	}))
	defer hub.Close()

	// Create hub client with nil httpClient (no IAP).
	client := NewHTTPHubClient(hub.URL, "", "", nil)
	ctx := context.Background()

	// Should work without IAP transport.
	projects, err := client.ListProjects(ctx)
	require.NoError(t, err)
	assert.Len(t, projects, 1)
	assert.Equal(t, "test", projects[0].Slug)
}
