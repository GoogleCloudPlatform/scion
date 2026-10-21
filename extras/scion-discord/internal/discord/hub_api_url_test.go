package discord

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		_, _ = w.Write([]byte(`{"status":"pending"}`))
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *recordingHub) requests() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.paths...)
}

// newHubAPIURLBroker configures a Discord broker (no gateway connection)
// with the given hub URL keys.
func newHubAPIURLBroker(t *testing.T, hubKeys map[string]string) *DiscordBroker {
	t.Helper()
	b := NewBroker(discardLogger())
	cfg := map[string]string{
		"bot_token": "Bot fake-token",
		"db_path":   filepath.Join(t.TempDir(), "discord.db"),
		"broker_id": "broker-test",
	}
	for k, v := range hubKeys {
		cfg[k] = v
	}
	require.NoError(t, b.Configure(cfg))
	t.Cleanup(func() { _ = b.Close() })
	require.NotNil(t, b.registration)
	require.NotNil(t, b.hubClient)
	return b
}

// TestHubAPIURL_UsedForHubCalls pins the IAP fix: the link endpoints, the
// hub client and inbound delivery call hub_api_url (the hub's loopback URL),
// while links people open (registration link, HubBaseURL for terminal
// links) are built from hub_url.
func TestHubAPIURL_UsedForHubCalls(t *testing.T) {
	publicHub := newRecordingHub(t)
	apiHub := newRecordingHub(t)
	b := newHubAPIURLBroker(t, map[string]string{
		"hub_url":     publicHub.srv.URL,
		"hub_api_url": apiHub.srv.URL,
	})
	ctx := context.Background()

	require.NoError(t, b.registration.registerCodeWithHub(ctx, "ABC123", "u1", "alice", ""))
	_, err := b.registration.checkLinkingStatus(ctx, "u1")
	require.NoError(t, err)
	_, err = b.hubClient.ListAgents(ctx, "p1", "user:alice@example.com")
	require.NoError(t, err)
	assert.Nil(t, b.deliverInbound("scion.project.p1.user.u1.messages", &messages.StructuredMessage{Msg: "hi"}))

	reqs := apiHub.requests()
	assert.Contains(t, reqs, "POST /api/v1/discord/link")
	assert.Contains(t, reqs, "GET /api/v1/discord/link/status")
	assert.Contains(t, reqs, "GET /api/v1/projects/p1/agents")
	assert.Contains(t, reqs, "POST /api/v1/broker/inbound")
	assert.Empty(t, publicHub.requests(), "no API call goes to the public endpoint")

	assert.Equal(t, publicHub.srv.URL, b.registration.registrationBaseURL(),
		"the registration link falls back to hub_url, not hub_api_url")
	assert.Equal(t, publicHub.srv.URL, b.hubClient.HubBaseURL())

	health, err := b.HealthCheck()
	require.NoError(t, err)
	assert.Equal(t, publicHub.srv.URL, health.Details["hub_url"])
	assert.Equal(t, apiHub.srv.URL, health.Details["hub_api_url"])
}

// TestHubAPIURL_FallsBackToHubURL keeps the behaviour for older hubs and
// self-managed deployments, which inject no hub_api_url.
func TestHubAPIURL_FallsBackToHubURL(t *testing.T) {
	hub := newRecordingHub(t)
	b := newHubAPIURLBroker(t, map[string]string{"hub_url": hub.srv.URL})

	require.NoError(t, b.registration.registerCodeWithHub(context.Background(), "ABC123", "u1", "alice", ""))
	assert.Contains(t, hub.requests(), "POST /api/v1/discord/link")
	assert.Equal(t, hub.srv.URL, b.registration.registrationBaseURL())
}

// TestHubAPIURL_RegisterURLWins keeps register_url as the registration link
// base when set.
func TestHubAPIURL_RegisterURLWins(t *testing.T) {
	b := newHubAPIURLBroker(t, map[string]string{
		"hub_url":      "https://hub.example.com",
		"hub_api_url":  "http://localhost:8080",
		"register_url": "https://register.example.com",
	})
	assert.Equal(t, "https://register.example.com", b.registration.registrationBaseURL())
	assert.Equal(t, "http://localhost:8080", b.registration.apiURL)
}
