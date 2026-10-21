// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package teams

import (
	"context"
	"log/slog"
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

func newHubAPIURLBroker(t *testing.T, hubKeys map[string]string) *TeamsBroker {
	t.Helper()
	broker := NewBroker(slog.Default())
	cfg := map[string]string{
		"app_id":     "id",
		"app_secret": "secret",
		"tenant_id":  "tenant",
		"hmac_key":   "dGVzdC1rZXk=",
		"broker_id":  "broker",
		"db_path":    filepath.Join(t.TempDir(), "test.db"),
	}
	for k, v := range hubKeys {
		cfg[k] = v
	}
	require.NoError(t, broker.Configure(cfg))
	t.Cleanup(func() { _ = broker.Close() })
	require.NotNil(t, broker.hubClient)
	return broker
}

// TestHubAPIURL_UsedForHubCalls checks that the hub client calls
// hub_api_url while hub_url stays the base of the link people open.
func TestHubAPIURL_UsedForHubCalls(t *testing.T) {
	publicHub := newRecordingHub(t)
	apiHub := newRecordingHub(t)
	broker := newHubAPIURLBroker(t, map[string]string{
		"hub_url":     publicHub.srv.URL,
		"hub_api_url": apiHub.srv.URL,
	})
	ctx := context.Background()

	_, err := broker.hubClient.RegisterTeamsLink(ctx, "teams-user-1")
	require.NoError(t, err)
	require.NoError(t, broker.hubClient.DeliverInbound(ctx, "scion.project.p1.user.u1.messages", &messages.StructuredMessage{Msg: "hi"}))

	reqs := apiHub.requests()
	assert.Contains(t, reqs, "POST /api/v1/teams/link")
	assert.Contains(t, reqs, "POST /api/v1/broker/inbound")
	assert.Empty(t, publicHub.requests(), "no API call goes to the public endpoint")
	assert.Equal(t, publicHub.srv.URL, broker.config.HubURL, "links people open use hub_url")
}

// TestHubAPIURL_FallsBackToHubURL keeps the behaviour for older hubs and
// self-managed deployments, which inject no hub_api_url.
func TestHubAPIURL_FallsBackToHubURL(t *testing.T) {
	hub := newRecordingHub(t)
	broker := newHubAPIURLBroker(t, map[string]string{"hub_url": hub.srv.URL})

	_, err := broker.hubClient.RegisterTeamsLink(context.Background(), "teams-user-1")
	require.NoError(t, err)
	assert.Contains(t, hub.requests(), "POST /api/v1/teams/link")
}
