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

//go:build !no_sqlite

package hub

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApplyStartExtras_Profile(t *testing.T) {
	payload := map[string]interface{}{}
	applyStartExtras(payload, StartExtras{})
	assert.NotContains(t, payload, "profile", "a nil profile sends nothing")

	for _, profile := range []string{"", "gpu"} {
		payload := map[string]interface{}{}
		p := profile
		applyStartExtras(payload, StartExtras{Profile: &p})
		got, ok := payload["profile"]
		require.True(t, ok, "profile %q must be sent, even when empty", profile)
		assert.Equal(t, profile, got)
	}
}

// TestDispatch_StartAndRestartCarryProfile: start and restart send the
// agent's recorded AppliedConfig.Profile, and an empty profile (never nil)
// when it has none, so the broker can tell "no profile" from an older hub.
func TestDispatch_StartAndRestartCarryProfile(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		applied *store.AgentAppliedConfig
		want    string
	}{
		{name: "recorded profile", applied: &store.AgentAppliedConfig{Profile: "gpu", CreateInputs: &store.AgentCreateInputs{}}, want: "gpu"},
		{name: "no profile", applied: &store.AgentAppliedConfig{CreateInputs: &store.AgentCreateInputs{}}, want: ""},
		{name: "no applied config", applied: nil, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, client, ag := autoExposeDispatchFixture(t)
			ag.AppliedConfig = tc.applied
			require.NoError(t, d.DispatchAgentStart(ctx, ag, "", false))
			require.NotNil(t, client.lastStartExtras.Profile)
			assert.Equal(t, tc.want, *client.lastStartExtras.Profile)
			require.NoError(t, d.DispatchAgentRestart(ctx, ag))
			require.NotNil(t, client.lastRestartExtras.Profile)
			assert.Equal(t, tc.want, *client.lastRestartExtras.Profile)
		})
	}
}

// TestStartRestartWire_Profile checks the start and restart request bodies
// on both transports carry the profile key, including an empty profile.
func TestStartRestartWire_Profile(t *testing.T) {
	type sendFn func(t *testing.T, extras StartExtras) map[string]interface{}

	decode := func(t *testing.T, body []byte) map[string]interface{} {
		t.Helper()
		wire := map[string]interface{}{}
		if len(body) == 0 {
			return wire
		}
		require.NoError(t, json.Unmarshal(body, &wire))
		return wire
	}
	httpSend := func(restart bool) sendFn {
		return func(t *testing.T, extras StartExtras) map[string]interface{} {
			var body []byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ = io.ReadAll(r.Body)
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte(`{"created":false}`))
			}))
			defer server.Close()
			client := NewHTTPRuntimeBrokerClient()
			var err error
			if restart {
				_, err = client.RestartAgent(context.Background(), tid("host-1"), server.URL, "a1", "p1", nil, extras)
			} else {
				_, err = client.StartAgent(context.Background(), tid("host-1"), server.URL, "a1", "p1", "", "", "", "", "", "", nil, nil, nil, nil, false, false, extras)
			}
			require.NoError(t, err)
			return decode(t, body)
		}
	}
	ccSend := func(restart bool) sendFn {
		return func(t *testing.T, extras StartExtras) map[string]interface{} {
			tunnel := &mockControlChannelTunnel{connected: true}
			client := &ControlChannelBrokerClient{manager: tunnel}
			if restart {
				_, _ = client.RestartAgent(context.Background(), "broker-1", "unused", "a1", "p1", nil, extras)
			} else {
				_, _ = client.StartAgent(context.Background(), "broker-1", "unused", "a1", "p1", "", "", "", "", "", "", nil, nil, nil, nil, false, false, extras)
			}
			require.NotNil(t, tunnel.lastRequest)
			return decode(t, tunnel.lastRequest.Body)
		}
	}

	for name, send := range map[string]sendFn{
		"http start":              httpSend(false),
		"http restart":            httpSend(true),
		"control channel start":   ccSend(false),
		"control channel restart": ccSend(true),
	} {
		t.Run(name, func(t *testing.T) {
			gpu, empty := "gpu", ""
			assert.Equal(t, "gpu", send(t, StartExtras{Profile: &gpu})["profile"])
			wire := send(t, StartExtras{Profile: &empty})
			assert.Contains(t, wire, "profile")
			assert.Equal(t, "", wire["profile"])
		})
	}
}
