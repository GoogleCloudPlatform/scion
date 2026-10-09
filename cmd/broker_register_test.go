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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// registerHub is an httptest hub that serves the two-phase broker
// registration API and records the Authorization header and body of each
// call.
type registerHub struct {
	mu          sync.Mutex
	createAuth  []string
	created     []hubclient.CreateBrokerRequest
	joined      []hubclient.JoinBrokerRequest
	createReply int
}

const (
	registerHubBrokerID  = "11111111-2222-3333-4444-555555555555"
	registerHubJoinToken = "join-token-for-test"
	registerHubSecret    = "c2VjcmV0LWtleS1mb3ItdGVzdA=="
)

func newRegisterHub(t *testing.T, createReply int) (*registerHub, *httptest.Server) {
	t.Helper()
	h := &registerHub{createReply: createReply}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/brokers":
			var req hubclient.CreateBrokerRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			h.createAuth = append(h.createAuth, r.Header.Get("Authorization"))
			h.created = append(h.created, req)
			if h.createReply != http.StatusCreated {
				w.WriteHeader(h.createReply)
				_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"forbidden"}}`))
				return
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(hubclient.CreateBrokerResponse{
				BrokerID:  req.BrokerID,
				JoinToken: registerHubJoinToken,
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/brokers/join":
			var req hubclient.JoinBrokerRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			h.joined = append(h.joined, req)
			_ = json.NewEncoder(w).Encode(hubclient.JoinBrokerResponse{
				BrokerID:  req.BrokerID,
				SecretKey: registerHubSecret,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"not found"}}`))
		}
	}))
	t.Cleanup(srv.Close)
	return h, srv
}

// hubTokenClient builds the hub client the register command uses, with
// SCION_HUB_TOKEN as the only configured credential.
func hubTokenClient(t *testing.T, endpoint, token string) hubclient.Client {
	t.Helper()
	clearAppTokenSources(t)
	t.Setenv("SCION_HUB_TOKEN", token)
	t.Setenv("SCION_HUB_ENDPOINT", endpoint)
	client, err := getHubClient(&config.Settings{})
	require.NoError(t, err)
	return client
}

func TestRegisterBrokerWithHub_HubTokenCreatesJoinsAndSavesCredentials(t *testing.T) {
	hub, srv := newRegisterHub(t, http.StatusCreated)
	const token = "scion_pat_register_test_token"
	client := hubTokenClient(t, srv.URL, token)
	store := brokercredentials.NewMultiStore(filepath.Join(t.TempDir(), "broker-credentials"))

	brokerID, err := registerBrokerWithHub(context.Background(), client, store, brokerHubRegistration{
		BrokerID: registerHubBrokerID,
		Name:     "register-test-host",
		HubName:  "test-hub",
		Endpoint: srv.URL,
	})

	require.NoError(t, err)
	assert.Equal(t, registerHubBrokerID, brokerID)
	assert.Equal(t, []string{"Bearer " + token}, hub.createAuth, "registration carries the hub token as a bearer credential")
	require.Len(t, hub.created, 1)
	assert.Equal(t, registerHubBrokerID, hub.created[0].BrokerID)
	assert.Equal(t, "register-test-host", hub.created[0].Name)
	assert.False(t, hub.created[0].AutoProvide)
	require.Len(t, hub.joined, 1)
	assert.Equal(t, registerHubJoinToken, hub.joined[0].JoinToken, "join presents the token returned by registration")
	assert.Equal(t, registerHubBrokerID, hub.joined[0].BrokerID)

	creds, err := store.Load("test-hub")
	require.NoError(t, err)
	assert.Equal(t, registerHubBrokerID, creds.BrokerID)
	assert.Equal(t, registerHubSecret, creds.SecretKey)
	assert.Equal(t, srv.URL, creds.HubEndpoint)
	assert.Equal(t, brokercredentials.AuthModeHMAC, creds.AuthMode)
}

func TestRegisterBrokerWithHub_DeniedRegistrationSavesNothing(t *testing.T) {
	hub, srv := newRegisterHub(t, http.StatusForbidden)
	client := hubTokenClient(t, srv.URL, "scion_pat_register_denied_token")
	store := brokercredentials.NewMultiStore(filepath.Join(t.TempDir(), "broker-credentials"))

	_, err := registerBrokerWithHub(context.Background(), client, store, brokerHubRegistration{
		BrokerID:    registerHubBrokerID,
		Name:        "register-denied-host",
		AutoProvide: true,
		HubName:     "test-hub",
		Endpoint:    srv.URL,
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "broker.auto_provide", "a denied auto-provide registration names the permission")
	assert.Empty(t, hub.joined, "no join without a join token")
	assert.False(t, store.Exists("test-hub"), "a denied registration saves no credentials")
}
