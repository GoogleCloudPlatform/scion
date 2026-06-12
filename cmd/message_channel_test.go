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

package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Stream A: Channel validation tests ---

func newChannelMockServer(t *testing.T, channels []map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		case "/api/v1/message-channels":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"channels": channels})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestValidateChannel_Valid(t *testing.T) {
	server := newChannelMockServer(t, []map[string]string{
		{"name": "telegram", "status": "online"},
		{"name": "discord", "status": "online"},
	})
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL}

	err = validateChannel(hubCtx, "telegram")
	assert.NoError(t, err)
}

func TestValidateChannel_Invalid(t *testing.T) {
	server := newChannelMockServer(t, []map[string]string{
		{"name": "telegram", "status": "online"},
	})
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL}

	err = validateChannel(hubCtx, "nonexistent")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `channel "nonexistent" is not registered`)
	assert.Contains(t, err.Error(), "telegram")
}

func TestValidateChannel_NoChannels(t *testing.T) {
	server := newChannelMockServer(t, []map[string]string{})
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{Client: client, Endpoint: server.URL}

	err = validateChannel(hubCtx, "telegram")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no channels are currently available")
}
