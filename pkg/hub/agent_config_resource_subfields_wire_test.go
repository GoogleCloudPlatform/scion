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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Wire tests for ptone/scion#5676 that need no SQLite store: both broker
// transports send the inline config's resources sub-field clears under
// inlineConfig. They build in every tag set, unlike the store-backed tests
// in agent_config_resource_subfields_test.go.

// subfieldClearInline is an inline config carrying a resources sub-field
// clear, as a start sends it.
func subfieldClearInline() *api.ScionConfig {
	return &api.ScionConfig{
		Resources:     &api.ResourceSpec{Limits: api.ResourceList{CPU: "3"}},
		ClearedFields: []string{"resources.limits.memory"},
	}
}

// wireInlineConfig decodes a start request body's inlineConfig.
func wireInlineConfig(t *testing.T, body []byte) *api.ScionConfig {
	t.Helper()
	var payload struct {
		InlineConfig *api.ScionConfig `json:"inlineConfig"`
	}
	require.NoError(t, json.Unmarshal(body, &payload))
	require.NotNil(t, payload.InlineConfig, "the start body carries no inlineConfig")
	return payload.InlineConfig
}

// TestBrokerHTTPTransport_StartSendsResourceSubfieldClears: the HTTP
// transport's start body carries the inline config's cleared_fields
// sub-paths under inlineConfig, the wire field the broker reads.
func TestBrokerHTTPTransport_StartSendsResourceSubfieldClears(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	transport := newBrokerHTTPTransport(false, nil)
	_, err := transport.StartAgent(context.Background(), "broker-1", srv.URL, "agent-1", "project-id-1",
		"", "", "", "", "", "", nil, nil, subfieldClearInline(), nil, false, false, StartExtras{})
	require.NoError(t, err)
	ic := wireInlineConfig(t, gotBody)
	assert.Equal(t, []string{"resources.limits.memory"}, ic.ClearedFields)
	assert.Empty(t, ic.Resources.Limits.Memory)
}

// TestControlChannelBrokerClient_StartSendsResourceSubfieldClears: the
// same over the control channel.
func TestControlChannelBrokerClient_StartSendsResourceSubfieldClears(t *testing.T) {
	runOverHubTunnels(t, func(t *testing.T, tr hubTunnelTransport) {
		tunnel := &mockControlChannelTunnel{connected: true}
		client := &ControlChannelBrokerClient{manager: tr.wrap(t, tunnel), signer: &mockBrokerSigner{}}
		_, err := client.StartAgent(context.Background(), "broker-1", "unused", "agent-1", "project-id-1",
			"", "", "", "", "", "", nil, nil, subfieldClearInline(), nil, false, false, StartExtras{})
		require.NoError(t, err)
		require.NotNil(t, tunnel.lastRequest)
		ic := wireInlineConfig(t, tunnel.lastRequest.Body)
		assert.Equal(t, []string{"resources.limits.memory"}, ic.ClearedFields)
		assert.Empty(t, ic.Resources.Limits.Memory)
	})
}
