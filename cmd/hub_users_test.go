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
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHubUsersProvisionCmd_ModeAvailability pins the CLI mode decision for
// scion hub users provision: available in human and assistant modes,
// removed in agent mode (hub is not in agentAllowed). It runs against the
// real command tree, so a change to the command's registration path is
// caught too.
func TestHubUsersProvisionCmd_ModeAvailability(t *testing.T) {
	require.NotNil(t, resolveCommandPath(rootCmd, "hub.users.provision"),
		"hub users provision must exist in the real command tree")
	assert.False(t, assistantDenied["hub.users.provision"], "hub users provision must not be denied in assistant mode")
	assert.False(t, assistantDenied["hub.users"], "hub users must not be denied in assistant mode")
	assert.False(t, agentAllowed["hub.users.provision"], "hub users provision must not be allowed in agent mode")

	for _, tc := range []struct {
		mode    string
		present bool
	}{
		{"human", true},
		{"assistant", true},
		{"agent", false},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			root := &cobra.Command{Use: "scion"}
			hubReal := resolveCommandPath(rootCmd, "hub")
			require.NotNil(t, hubReal)
			root.AddCommand(cloneCommandShape(hubReal))

			t.Setenv("SCION_CLI_MODE", tc.mode)
			applyModeRestrictions(root)
			if tc.present {
				assert.NotNil(t, resolveCommandPath(root, "hub.users.provision"), "%s mode keeps hub users provision", tc.mode)
			} else {
				assert.Nil(t, resolveCommandPath(root, "hub.users.provision"), "%s mode removes hub users provision", tc.mode)
			}
		})
	}
}

func newProvisionTestClient(t *testing.T, handler http.HandlerFunc) (hubclient.Client, *[]map[string]interface{}) {
	t.Helper()
	var bodies []map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		bodies = append(bodies, body)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	return client, &bodies
}

func TestProvisionHubUser_CreatedAndReplayOutput(t *testing.T) {
	client, bodies := newProvisionTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/users", r.URL.Path)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"user":{"id":"u1","email":"alice@example.com","status":"invited","displayName":"Alice"},"created":true,"warnings":["domain_not_authorized"]}`))
	})
	name := "Alice"
	var out bytes.Buffer
	err := provisionHubUser(context.Background(), &out, client.Users(), &hubclient.ProvisionUserRequest{Email: "alice@example.com", DisplayName: &name}, false)
	require.NoError(t, err)
	assert.Contains(t, out.String(), "Provisioned alice@example.com (status invited).")
	assert.Contains(t, out.String(), "User ID: u1")
	assert.Contains(t, out.String(), "outside the hub's authorized domains")
	require.Len(t, *bodies, 1)
	assert.Equal(t, map[string]interface{}{"email": "alice@example.com", "displayName": "Alice"}, (*bodies)[0],
		"the CLI sends no role and omits an unset note")

	replay, _ := newProvisionTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"user":{"email":"alice@example.com","status":"invited"},"created":false}`))
	})
	out.Reset()
	require.NoError(t, provisionHubUser(context.Background(), &out, replay.Users(), &hubclient.ProvisionUserRequest{Email: "alice@example.com"}, false))
	assert.Contains(t, out.String(), "already pre-registered with these details")
	assert.NotContains(t, out.String(), "User ID", "the minimal replay view has no ID to print")
}

func TestProvisionHubUser_JSONOutput(t *testing.T) {
	client, _ := newProvisionTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"user":{"id":"u1","email":"alice@example.com","status":"invited"},"created":true}`))
	})
	var out bytes.Buffer
	require.NoError(t, provisionHubUser(context.Background(), &out, client.Users(), &hubclient.ProvisionUserRequest{Email: "alice@example.com"}, true))
	var got hubclient.ProvisionUserResponse
	require.NoError(t, json.Unmarshal(out.Bytes(), &got))
	assert.True(t, got.Created)
	assert.Equal(t, "u1", got.User.ID)
}

func TestProvisionHubUser_ConflictMessages(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{`{"error":{"code":"conflict","message":"x","details":{"reason":"pending_user_exists","userId":"u9"}}}`, "a pending record for this email exists with different details (user ID u9)"},
		{`{"error":{"code":"conflict","message":"x","details":{"reason":"user_suspended_exists"}}}`, "this email belongs to a suspended user"},
		{`{"error":{"code":"conflict","message":"user already exists","details":{"reason":"user_exists"}}}`, "user already exists"},
	}
	for _, tc := range cases {
		client, _ := newProvisionTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(tc.body))
		})
		var out bytes.Buffer
		err := provisionHubUser(context.Background(), &out, client.Users(), &hubclient.ProvisionUserRequest{Email: "x@example.com"}, false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), tc.want)
	}
}
