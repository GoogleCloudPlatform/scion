package slack

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// recordingHub is a fake hub that records the requests it was sent.
type recordingHub struct {
	srv   *httptest.Server
	mu    sync.Mutex
	paths []string
}

func newRecordingHub(t *testing.T) *recordingHub {
	t.Helper()
	h := &recordingHub{}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.paths = append(h.paths, r.Method+" "+r.URL.Path)
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *recordingHub) requests() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.paths...)
}

func newHubAPIURLBroker(t *testing.T, hubKeys map[string]string) *SlackBroker {
	t.Helper()
	b := NewBroker(slog.Default())
	b.client = newFakeSlack(t).client()
	cfg := map[string]string{
		"bot_token":      "xoxb-fake-token",
		"signing_secret": "fake-signing-secret",
		"db_path":        t.TempDir() + "/broker.db",
	}
	for k, v := range hubKeys {
		cfg[k] = v
	}
	require.NoError(t, b.Configure(cfg))
	t.Cleanup(func() { _ = b.Close() })
	require.NotNil(t, b.hubClient)
	require.NotNil(t, b.registration)
	return b
}

// TestHubAPIURL_UsedForHubCalls checks that the hub client, the link
// endpoint and inbound delivery call hub_api_url, while the registration
// link people open is built from hub_url.
func TestHubAPIURL_UsedForHubCalls(t *testing.T) {
	publicHub := newRecordingHub(t)
	apiHub := newRecordingHub(t)
	b := newHubAPIURLBroker(t, map[string]string{
		"hub_url":     publicHub.srv.URL,
		"hub_api_url": apiHub.srv.URL,
	})
	ctx := context.Background()

	link, err := b.registration.StartRegistration(ctx, "U1", "alice", "C1")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(link, publicHub.srv.URL+"/profile/slack?code="), "got %q", link)
	_, err = b.hubClient.ListAgents(ctx, "p1", "user:alice@example.com")
	require.NoError(t, err)
	assert.Nil(t, b.deliverInbound("scion.project.p1.user.u1.messages", &messages.StructuredMessage{Msg: "hi"}))

	reqs := apiHub.requests()
	assert.Contains(t, reqs, "POST /api/v1/slack/link")
	assert.Contains(t, reqs, "GET /api/v1/projects/p1/agents")
	assert.Contains(t, reqs, "POST /api/v1/broker/inbound")
	assert.Empty(t, publicHub.requests(), "no API call goes to the public endpoint")
}

// TestHubAPIURL_FallsBackToHubURL keeps the behaviour for older hubs and
// self-managed deployments, which inject no hub_api_url.
func TestHubAPIURL_FallsBackToHubURL(t *testing.T) {
	hub := newRecordingHub(t)
	b := newHubAPIURLBroker(t, map[string]string{"hub_url": hub.srv.URL})

	_, err := b.hubClient.ListAgents(context.Background(), "p1", "user:alice@example.com")
	require.NoError(t, err)
	assert.Contains(t, hub.requests(), "GET /api/v1/projects/p1/agents")
}
