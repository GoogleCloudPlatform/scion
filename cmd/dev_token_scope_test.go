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
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// authHeaderHub starts a fake hub on addr that records the Authorization
// header of every request.
func authHeaderHub(t *testing.T, addr string) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	l, err := net.Listen("tcp", addr)
	require.NoError(t, err, "listen on %s", addr)
	srv.Listener = l
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

// With only a dev token, the REST hub client sends it to a loopback hub
// and sends no credential to a hub on another address.
func TestHubClientForEndpoint_DevTokenOnlyForLoopback(t *testing.T) {
	setupContextTest(t)
	t.Setenv("SCION_DEV_TOKEN", "placeholder-dev-token")
	other, otherSeen := authHeaderHub(t, "127.0.0.2:0")
	local, localSeen := authHeaderHub(t, "127.0.0.1:0")

	for _, srv := range []*httptest.Server{other, local} {
		client, err := hubClientForEndpoint(&config.Settings{}, srv.URL)
		require.NoError(t, err)
		_, _ = client.Auth().Me(context.Background())
	}
	got := otherSeen()
	require.NotEmpty(t, got, "the non-loopback hub received no request")
	assert.Empty(t, got[0], "the non-loopback hub got a credential from the REST client")
	got = localSeen()
	require.NotEmpty(t, got)
	assert.Equal(t, "Bearer placeholder-dev-token", got[0])
}

// With only a dev token, the completion client sends it to a loopback hub
// and sends no credential to a hub on another address.
func TestFetchHubAgents_DevTokenOnlyForLoopback(t *testing.T) {
	setupContextTest(t)
	t.Setenv("SCION_DEV_TOKEN", "placeholder-dev-token")
	other, otherSeen := authHeaderHub(t, "127.0.0.2:0")
	local, localSeen := authHeaderHub(t, "127.0.0.1:0")

	for _, srv := range []*httptest.Server{other, local} {
		_, _ = fetchHubAgents(context.Background(), srv.URL, &config.Settings{})
	}
	got := otherSeen()
	require.NotEmpty(t, got, "the non-loopback hub received no request")
	assert.Empty(t, got[0], "the non-loopback hub got a credential from the completion client")
	got = localSeen()
	require.NotEmpty(t, got)
	assert.Equal(t, "Bearer placeholder-dev-token", got[0])
}
