package slack

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordedHubRequest is one request seen by fakeHub.
type recordedHubRequest struct {
	Method     string
	Path       string
	RawQuery   string
	LinkedUser string
	// SignedHeaders is the X-Scion-Signed-Headers value.
	SignedHeaders string
}

// fakeHub is a minimal hub API used by Slack tests. Each route returns the
// configured status and body; every request is recorded.
type fakeHub struct {
	mu       sync.Mutex
	requests []recordedHubRequest
	routes   map[string]fakeHubResponse
	server   *httptest.Server
}

type fakeHubResponse struct {
	status int
	body   string
}

func newFakeHub(t *testing.T) *fakeHub {
	t.Helper()
	h := &fakeHub{routes: map[string]fakeHubResponse{}}
	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.requests = append(h.requests, recordedHubRequest{
			Method:        r.Method,
			Path:          r.URL.Path,
			RawQuery:      r.URL.RawQuery,
			LinkedUser:    r.Header.Get("X-Scion-On-Behalf-Of"),
			SignedHeaders: r.Header.Get("X-Scion-Signed-Headers"),
		})
		resp, ok := h.routes[r.Method+" "+r.URL.Path]
		h.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.status)
		_, _ = w.Write([]byte(resp.body))
	}))
	t.Cleanup(h.server.Close)
	return h
}

func (h *fakeHub) on(method, path string, status int, body string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.routes[method+" "+path] = fakeHubResponse{status: status, body: body}
}

func (h *fakeHub) recorded() []recordedHubRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]recordedHubRequest(nil), h.requests...)
}

func (h *fakeHub) client() HubClient {
	key := base64.StdEncoding.EncodeToString([]byte("test-hmac-key-32-bytes-long!!!!!"))
	return NewHTTPHubClient(h.server.URL, key, "test-broker")
}

func TestHubClient_ListAgents_RequestWithLinkedUser(t *testing.T) {
	hub := newFakeHub(t)
	hub.on("GET", "/api/v1/projects/proj-1/agents", http.StatusOK,
		`{"agents":[{"slug":"alpha","activity":"idle"}]}`)

	agents, err := hub.client().ListAgents(context.Background(), "proj-1", "user:alice@example.com")
	require.NoError(t, err)
	require.Len(t, agents, 1)
	assert.Equal(t, "alpha", agents[0].Slug)

	reqs := hub.recorded()
	require.Len(t, reqs, 1)
	assert.Equal(t, "user:alice@example.com", reqs[0].LinkedUser)
	assert.Contains(t, reqs[0].SignedHeaders, "x-scion-on-behalf-of")
}

func TestHubClient_ListAgents_RequestWithoutLinkedUser(t *testing.T) {
	hub := newFakeHub(t)
	hub.on("GET", "/api/v1/projects/proj-1/agents", http.StatusOK, `{"agents":[]}`)

	_, err := hub.client().ListAgents(context.Background(), "proj-1", "")
	require.NoError(t, err)

	reqs := hub.recorded()
	require.Len(t, reqs, 1)
	assert.Empty(t, reqs[0].LinkedUser)
	assert.Empty(t, reqs[0].SignedHeaders)
}

func TestHubClient_ListUserProjects_RequestWithLinkedUser(t *testing.T) {
	hub := newFakeHub(t)
	hub.on("GET", "/api/v1/projects", http.StatusOK,
		`{"projects":[{"id":"p1","name":"Project One","slug":"p1"}]}`)

	projects, err := hub.client().ListUserProjects(context.Background(), "user:alice@example.com")
	require.NoError(t, err)
	require.Len(t, projects, 1)

	reqs := hub.recorded()
	require.Len(t, reqs, 1)
	assert.Equal(t, "user:alice@example.com", reqs[0].LinkedUser)
	assert.Contains(t, reqs[0].SignedHeaders, "x-scion-on-behalf-of")
	assert.Empty(t, reqs[0].RawQuery, "the user's projects come from the linked user, not a query filter")
}
