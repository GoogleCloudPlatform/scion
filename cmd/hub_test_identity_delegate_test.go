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
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubprofile"
)

// scion hub test-identity delegate (ptone/scion#4240, X3): the human
// consent step on the target hub.

type delegateHub struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []string
	body map[string]interface{}
}

func newDelegateHub(t *testing.T) *delegateHub {
	t.Helper()
	h := &delegateHub{}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.reqs = append(h.reqs, r.Method+" "+r.URL.Path)
		if r.Method == http.MethodPost {
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &h.body)
		}
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/test-identity-delegations":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"dlg-1","grantorId":"u-1","workloadSub":"123456789","scope":"test_identity:issue","memberCap":2,"label":"uat","created":"2026-10-12T08:00:00Z","expiresAt":"2026-10-12T16:00:00Z","live":true}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/test-identity-delegations":
			_, _ = w.Write([]byte(`{"items":[{"id":"dlg-1","grantorId":"u-1","workloadSub":"123456789","scope":"test_identity:issue","memberCap":2,"label":"uat","created":"2026-10-12T08:00:00Z","expiresAt":"2026-10-12T16:00:00Z","live":true}],"truncated":false}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/test-identity-delegations/dlg-1":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"delegation not found"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *delegateHub) requests() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.reqs...)
}

// delegateEnv points the CLI at hub with no profile, from an empty
// directory and home.
func delegateEnv(t *testing.T, hub *delegateHub) {
	t.Helper()
	profileGateEnv(t, "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SCION_HUB_ENDPOINT", hub.srv.URL)
	t.Setenv("SCION_NETWORK_MODE", "host")
	t.Setenv("SCION_AUTH_TOKEN", "")
	t.Setenv("SCION_HUB_TOKEN", "")
	dir := filepath.Join(t.TempDir(), "work")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	t.Chdir(dir)
}

func TestHubTestIdentityDelegate_CreateListRevoke(t *testing.T) {
	hub := newDelegateHub(t)
	delegateEnv(t, hub)

	r, err := runProfileRoot(t, "hub", "test-identity", "delegate", "create", "--workload-sub", "123456789", "--ttl", "8h", "--member-cap", "2", "--label", "uat")
	require.NoError(t, err, r.stderr)
	assert.Contains(t, r.stdout, "Created delegation dlg-1")
	assert.Contains(t, r.stdout, "--issuer-delegation member=dlg-1")
	hub.mu.Lock()
	body := hub.body
	hub.mu.Unlock()
	assert.Equal(t, "123456789", body["workloadSub"])
	assert.Equal(t, []interface{}{"test_identity:issue"}, body["scopes"])
	assert.Equal(t, float64(28800), body["ttlSeconds"])
	assert.Equal(t, float64(2), body["memberCap"])
	assert.Equal(t, "uat", body["label"])

	r, err = runProfileRoot(t, "hub", "test-identity", "delegate", "create", "--workload-sub", "123456789", "--json")
	require.NoError(t, err, r.stderr)
	var d hubclient.TestIdentityDelegation
	require.NoError(t, json.Unmarshal([]byte(r.stdout), &d), r.stdout)
	assert.Equal(t, "dlg-1", d.ID)

	r, err = runProfileRoot(t, "hub", "test-identity", "delegate", "list")
	require.NoError(t, err, r.stderr)
	assert.Contains(t, r.stdout, "dlg-1")
	assert.Contains(t, r.stdout, "123456789")
	r, err = runProfileRoot(t, "hub", "test-identity", "delegate", "list", "--json")
	require.NoError(t, err, r.stderr)
	var list hubclient.ListTestIdentityDelegationsResponse
	require.NoError(t, json.Unmarshal([]byte(r.stdout), &list), r.stdout)
	require.Len(t, list.Items, 1)

	r, err = runProfileRoot(t, "hub", "test-identity", "delegate", "revoke", "dlg-1")
	require.NoError(t, err, r.stderr)
	assert.Contains(t, r.stdout, "Revoked delegation dlg-1.")
	_, err = runProfileRoot(t, "hub", "test-identity", "delegate", "revoke", "dlg-gone")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "delegation dlg-gone not found")

	assert.Equal(t, []string{
		"POST /api/v1/test-identity-delegations",
		"POST /api/v1/test-identity-delegations",
		"GET /api/v1/test-identity-delegations",
		"GET /api/v1/test-identity-delegations",
		"DELETE /api/v1/test-identity-delegations/dlg-1",
		"DELETE /api/v1/test-identity-delegations/dlg-gone",
	}, hub.requests())
}

// --member-cap outside 1-5 is refused before any request.
func TestHubTestIdentityDelegate_MemberCapRefusedBeforeRequest(t *testing.T) {
	hub := newDelegateHub(t)
	delegateEnv(t, hub)
	for _, c := range []string{"6", "-1"} {
		_, err := runProfileRoot(t, "hub", "test-identity", "delegate", "create", "--workload-sub", "1", "--member-cap", c)
		require.Error(t, err, c)
		assert.Contains(t, err.Error(), "invalid --member-cap")
	}
	assert.Empty(t, hub.requests())
}

// Every delegate subcommand is refused with a hub profile selected, and
// sends nothing.
func TestHubTestIdentityDelegate_RefusedWithAProfile(t *testing.T) {
	hub := newDelegateHub(t)
	delegateEnv(t, hub)
	home := os.Getenv("HOME")
	require.NoError(t, hubprofile.Save(home, &hubprofile.Profile{Name: "t1", Endpoint: hub.srv.URL, Transport: hubprofile.TransportNone}, false))

	for _, args := range [][]string{
		{"--hub-profile", "t1", "hub", "test-identity", "delegate", "create", "--workload-sub", "1"},
		{"--hub-profile", "t1", "hub", "test-identity", "delegate", "list"},
		{"--hub-profile", "t1", "hub", "test-identity", "delegate", "revoke", "dlg-1"},
	} {
		_, err := runProfileRoot(t, args...)
		require.Error(t, err, args)
		assert.Contains(t, err.Error(), "is not available with a hub profile", args)
	}
	// The command-level guard refuses too, independent of the gate.
	t.Setenv(hubprofile.EnvHubProfile, "t1")
	_, err := delegationClient()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "never a hub profile")
	assert.Empty(t, hub.requests())
}
