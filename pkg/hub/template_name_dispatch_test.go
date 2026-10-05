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

package hub

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testTemplateHash = "sha256:" + strings.Repeat("cd", 32)

func TestApplyStartExtras_TemplateName(t *testing.T) {
	for _, tt := range []struct {
		name, in string
		want     bool
	}{
		{name: "slug", in: "web-dev", want: true},
		{name: "content hash", in: testTemplateHash, want: false},
		{name: "empty", in: "", want: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			payload := map[string]interface{}{}
			applyStartExtras(payload, StartExtras{TemplateName: tt.in})
			got, ok := payload["templateName"]
			assert.Equal(t, tt.want, ok, "templateName present")
			if tt.want {
				assert.Equal(t, tt.in, got)
			}
			_, hasTemplate := payload["template"]
			assert.False(t, hasTemplate, "the template load key must never be sent on start or restart")
		})
	}
}

// TestDispatch_TemplateNameOnStartAndRestart checks that start and restart
// carry the agent's template as StartExtras.TemplateName.
func TestDispatch_TemplateNameOnStartAndRestart(t *testing.T) {
	ctx := context.Background()
	d, client, ag := autoExposeDispatchFixture(t)
	ag.Template = "web-dev"

	require.NoError(t, d.DispatchAgentStart(ctx, ag, "", false))
	assert.Equal(t, "web-dev", client.lastStartExtras.TemplateName)

	require.NoError(t, d.DispatchAgentRestart(ctx, ag))
	assert.Equal(t, "web-dev", client.lastRestartExtras.TemplateName)
}

// TestStartRestartWire_TemplateName checks the start and restart request
// bodies on both transports: the slug is sent as templateName, a content
// hash is not sent, and the template load key is never sent.
func TestStartRestartWire_TemplateName(t *testing.T) {
	type sendFn func(t *testing.T, extras StartExtras) map[string]interface{}

	decode := func(t *testing.T, body []byte) map[string]interface{} {
		t.Helper()
		wire := map[string]interface{}{}
		if len(body) == 0 {
			return wire // an empty payload is sent without a body
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
			wire := send(t, StartExtras{TemplateName: "web-dev"})
			assert.Equal(t, "web-dev", wire["templateName"])
			assert.NotContains(t, wire, "template")

			wire = send(t, StartExtras{TemplateName: testTemplateHash})
			assert.NotContains(t, wire, "templateName")
			assert.NotContains(t, wire, "template")
		})
	}
}
