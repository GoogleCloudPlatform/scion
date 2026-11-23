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

package commands

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/telemetry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewAnalyticsHubIDResolver_UsesAgentEndpoint(t *testing.T) {
	var agentHits, healthzHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent/hub":
			agentHits.Add(1)
			assert.Equal(t, "agent-tok", r.Header.Get("X-Scion-Agent-Token"))
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"hub_id": "hub-new"})
		case "/healthz":
			healthzHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"hub_id": "hub-from-healthz"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	resolve := newAnalyticsHubIDResolver(func() *hub.Client {
		return hub.NewClientWithConfig(srv.URL, "agent-tok", "agent-1")
	})

	id, err := resolve(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "hub-new", id)
	assert.Equal(t, int32(1), agentHits.Load())
	assert.Equal(t, int32(0), healthzHits.Load(), "/healthz must not be called when agent endpoint succeeds")
}

func TestNewAnalyticsHubIDResolver_FallsBackOnOlderHub404(t *testing.T) {
	cases := []struct {
		name        string
		healthzBody string
		wantHubID   string
	}{
		{
			name:        "standalone hub /healthz",
			healthzBody: `{"status":"healthy","hub_id":"legacy-hub-1"}`,
			wantHubID:   "legacy-hub-1",
		},
		{
			name:        "combined server /healthz",
			healthzBody: `{"status":"healthy","hub":{"hub_id":"legacy-combo-2"}}`,
			wantHubID:   "legacy-combo-2",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var agentHits, healthzHits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/agent/hub":
					agentHits.Add(1)
					http.NotFound(w, r)
				case "/healthz":
					healthzHits.Add(1)
					assert.Empty(t, r.Header.Get("X-Scion-Agent-Token"), "/healthz fallback must not send agent credentials")
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(tc.healthzBody))
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()

			resolve := newAnalyticsHubIDResolver(func() *hub.Client {
				return hub.NewClientWithConfig(srv.URL, "agent-tok", "agent-1")
			})

			id, err := resolve(context.Background())
			require.NoError(t, err)
			assert.Equal(t, tc.wantHubID, id)
			assert.Equal(t, int32(1), agentHits.Load())
			assert.Equal(t, int32(1), healthzHits.Load())
		})
	}
}

func TestNewAnalyticsHubIDResolver_NoFallbackOnAuthOrServerError(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var healthzHits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/agent/hub":
					w.WriteHeader(status)
					_, _ = w.Write([]byte(`{"error":{"code":"err","message":"fail"}}`))
				case "/healthz":
					healthzHits.Add(1)
					_, _ = w.Write([]byte(`{"hub_id":"should-not-use"}`))
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()

			resolve := newAnalyticsHubIDResolver(func() *hub.Client {
				return hub.NewClientWithConfig(srv.URL, "agent-tok", "agent-1")
			})

			_, err := resolve(context.Background())
			require.Error(t, err)
			assert.Equal(t, int32(0), healthzHits.Load(), "/healthz must not be called on HTTP %d", status)
		})
	}
}

func TestNewAnalyticsHubIDResolver_DrainerUsesAgentEndpointForPinAndMismatch(t *testing.T) {
	var currentHub atomic.Value
	currentHub.Store("hub-a")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agent/hub":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"hub_id": currentHub.Load().(string)})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	resolve := newAnalyticsHubIDResolver(func() *hub.Client {
		return hub.NewClientWithConfig(srv.URL, "agent-tok", "agent-1")
	})

	outbox, err := telemetry.OpenAnalyticsOutbox(filepath.Join(t.TempDir(), "analytics"))
	require.NoError(t, err)

	// First process pins hub-a from GET /api/v1/agent/hub.
	drainerA := telemetry.NewAnalyticsDrainer(outbox, func(context.Context, []byte) error { return nil }, resolve)
	drainerA.RunOnce(context.Background())
	assert.Equal(t, "hub-a", outbox.PinnedHubID())
	assert.Equal(t, "hub-a", drainerA.VerifiedHubID())

	// Second process points at hub-b: drainer refuses to verify against hub-a pin.
	currentHub.Store("hub-b")
	drainerB := telemetry.NewAnalyticsDrainer(outbox, func(context.Context, []byte) error { return nil }, resolve)
	drainerB.RunOnce(context.Background())
	assert.Equal(t, "hub-a", outbox.PinnedHubID())
	assert.Empty(t, drainerB.VerifiedHubID(), "mismatching hub_id from agent endpoint must not authorize finalize")
}
