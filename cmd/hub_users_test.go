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
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHubUsersProvisionCmd_ModeAvailability pins the CLI mode decision for
// scion hub users provision: available in human mode,
// removed in agent mode (hub is not in agentAllowed). It runs against the
// real command tree, so a change to the command's registration path is
// caught too.
func TestHubUsersProvisionCmd_ModeAvailability(t *testing.T) {
	require.NotNil(t, resolveCommandPath(rootCmd, "hub.users.provision"),
		"hub users provision must exist in the real command tree")
	assert.False(t, agentAllowed["hub.users.provision"], "hub users provision must not be allowed in agent mode")

	for _, tc := range []struct {
		mode    string
		present bool
	}{
		{"human", true},
		{"agent", false},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			root := &cobra.Command{Use: "scion"}
			hubReal := resolveCommandPath(rootCmd, "hub")
			require.NotNil(t, hubReal)
			root.AddCommand(cloneCommandShape(hubReal))

			t.Setenv("SCION_CLI_MODE", tc.mode)
			applyModeRestrictions(root, resolveMode())
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

// TestHubUsersInviteCmd_ModeAvailability pins the CLI mode decision for
// scion hub users invite: human mode only (not on agentAllowed).
func TestHubUsersInviteCmd_ModeAvailability(t *testing.T) {
	require.NotNil(t, resolveCommandPath(rootCmd, "hub.users.invite"),
		"hub users invite must exist in the real command tree")
	assert.False(t, agentAllowed["hub.users.invite"], "hub users invite must not be allowed in agent mode")

	for _, tc := range []struct {
		mode    string
		present bool
	}{
		{"human", true},
		{"agent", false},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			root := &cobra.Command{Use: "scion"}
			hubReal := resolveCommandPath(rootCmd, "hub")
			require.NotNil(t, hubReal)
			root.AddCommand(cloneCommandShape(hubReal))

			t.Setenv("SCION_CLI_MODE", tc.mode)
			applyModeRestrictions(root, resolveMode())
			if tc.present {
				assert.NotNil(t, resolveCommandPath(root, "hub.users.invite"), "%s mode keeps hub users invite", tc.mode)
			} else {
				assert.Nil(t, resolveCommandPath(root, "hub.users.invite"), "%s mode removes hub users invite", tc.mode)
			}
		})
	}
}

// TestCollectInviteAddresses_ArgsFileStdinAndCommas covers the address
// sources: comma-separated arguments, a CSV file with a header, comments,
// blank lines and a note column, stdin, and the --note default.
func TestCollectInviteAddresses_ArgsFileStdinAndCommas(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/people.csv"
	require.NoError(t, os.WriteFile(path, []byte("email,note\n# a comment\n\ncarol@example.com,Contractor\nnot-an-address\n"), 0o600))

	got, err := collectInviteAddresses(hubUsersInviteOptions{
		Args: []string{"alice@example.com, bob@example.com", " ,dave@example.com"},
		File: path,
		Note: "Q3",
	})
	require.NoError(t, err)
	assert.Equal(t, []hubclient.InviteBatchEmail{
		{Email: "alice@example.com", Note: "Q3"},
		{Email: "bob@example.com", Note: "Q3"},
		{Email: "dave@example.com", Note: "Q3"},
		{Email: "carol@example.com", Note: "Contractor"},
		{Email: "not-an-address", Note: "Q3"}, // kept: the hub reports it invalid
	}, got)

	got, err = collectInviteAddresses(hubUsersInviteOptions{File: "-", Stdin: strings.NewReader("erin@example.com\nfrank@example.com\n")})
	require.NoError(t, err)
	assert.Equal(t, []hubclient.InviteBatchEmail{{Email: "erin@example.com"}, {Email: "frank@example.com"}}, got)

	_, err = collectInviteAddresses(hubUsersInviteOptions{File: dir + "/missing.txt"})
	assert.Error(t, err)
}

// inviteTestHub is an httptest hub for hub users invite: one project
// "web" (ID inviteTestProjectID) with three assignable roles, and a bulk
// invite route answering with bulkStatus/bulkBody.
const inviteTestProjectID = "11111111-2222-3333-4444-555555555555"

type inviteTestHub struct {
	bulkStatus int
	bulkBody   string
	bulkBodies []map[string]interface{}
}

func (h *inviteTestHub) client(t *testing.T) hubclient.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects" && r.URL.Query().Get("name") == "web":
			_, _ = w.Write([]byte(`{"projects":[{"id":"` + inviteTestProjectID + `","name":"web","slug":"web"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects":
			_, _ = w.Write([]byte(`{"projects":[]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/"+inviteTestProjectID+"/members/assignable-roles":
			_, _ = w.Write([]byte(`{"items":[
 {"id":"r-owner","name":"project-owner","roleKind":"builtin","grantable":false,"reason":"actor role \"project-admin\" cannot add target role \"project-owner\"","denialCode":"target_role_protected"},
 {"id":"r-member","name":"project-member","roleKind":"builtin","grantable":true,"reason":""},
 {"id":"r-review","name":"reviewer","roleKind":"custom","grantable":true,"reason":""}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/admin/users/invite/bulk":
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			h.bulkBodies = append(h.bulkBodies, body)
			w.WriteHeader(h.bulkStatus)
			_, _ = w.Write([]byte(h.bulkBody))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"not found"}}`))
		}
	}))
	t.Cleanup(srv.Close)
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	return client
}

const inviteCompleteBody = `{"batchId":"b1","invited":1,"skipped":1,"total":3,"complete":true,
"counts":{"created":1,"already_invited":1,"invalid":1},
"results":[
 {"index":0,"email":"alice@example.com","status":"created","userId":"u1","memberships":[{"projectId":"` + inviteTestProjectID + `","status":"applied"}]},
 {"index":1,"email":"bob@example.com","status":"already_invited","reason":"pending_user_exists","memberships":[{"projectId":"` + inviteTestProjectID + `","status":"already_member"}]},
 {"index":2,"input":"nope","status":"invalid","reason":"invalid_email","memberships":[{"projectId":"` + inviteTestProjectID + `","status":"skipped","code":"address_not_invited"}]}]}`

// TestInviteHubUsers_ProjectRoleResolutionAndOutput resolves --project by
// project name and role names, checks the request body, and prints one
// line per address plus a summary; a complete result exits zero.
func TestInviteHubUsers_ProjectRoleResolutionAndOutput(t *testing.T) {
	hub := &inviteTestHub{bulkStatus: http.StatusOK, bulkBody: inviteCompleteBody}
	client := hub.client(t)
	var out bytes.Buffer
	err := inviteHubUsers(context.Background(), &out, client, hubUsersInviteOptions{
		Args:     []string{"alice@example.com,bob@example.com", "nope"},
		Projects: []string{"web=project-member,r-review,project-member"},
	})
	require.NoError(t, err, out.String())
	require.Len(t, hub.bulkBodies, 1)
	assert.Equal(t, []interface{}{
		map[string]interface{}{"projectId": inviteTestProjectID, "roleDefinitionIds": []interface{}{"r-member", "r-review"}},
	}, hub.bulkBodies[0]["projects"])
	assert.Len(t, hub.bulkBodies[0]["emails"], 3)
	text := out.String()
	assert.Contains(t, text, "created          alice@example.com  [web: applied]")
	assert.Contains(t, text, "already_invited  bob@example.com  (pending_user_exists)  [web: already_member]")
	assert.Contains(t, text, "invalid          nope  (invalid_email)\n")
	assert.Contains(t, text, "3 addresses: 1 created, 1 already invited")
}

// TestInviteHubUsers_RoleErrorsAreLocal: an unknown role name lists the
// roles that exist, and a role the caller cannot grant gives the hub's
// reason; neither sends the invite request.
func TestInviteHubUsers_RoleErrorsAreLocal(t *testing.T) {
	hub := &inviteTestHub{bulkStatus: http.StatusOK, bulkBody: inviteCompleteBody}
	client := hub.client(t)

	err := inviteHubUsers(context.Background(), io.Discard, client, hubUsersInviteOptions{
		Args: []string{"alice@example.com"}, Projects: []string{"web=nosuchrole"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `no assignable role "nosuchrole"`)
	assert.Contains(t, err.Error(), "project-member, project-owner, reviewer")

	err = inviteHubUsers(context.Background(), io.Discard, client, hubUsersInviteOptions{
		Args: []string{"alice@example.com"}, Projects: []string{"web=project-owner"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot grant role project-owner in project web")
	assert.Contains(t, err.Error(), "cannot add target role")

	for _, bad := range []string{"web", "=project-member", "web="} {
		err = inviteHubUsers(context.Background(), io.Discard, client, hubUsersInviteOptions{
			Args: []string{"alice@example.com"}, Projects: []string{bad},
		})
		assert.Error(t, err, "--project %q", bad)
	}
	err = inviteHubUsers(context.Background(), io.Discard, client, hubUsersInviteOptions{
		Args: []string{"alice@example.com"}, Projects: []string{"missing=project-member"},
	})
	assert.Error(t, err)
	assert.Empty(t, hub.bulkBodies, "no invite request is sent after a local error")
}

// TestInviteHubUsers_IncompleteExitsNonZero: complete=false returns an
// error after printing the result, so a script can run it again.
func TestInviteHubUsers_IncompleteExitsNonZero(t *testing.T) {
	hub := &inviteTestHub{bulkStatus: http.StatusOK, bulkBody: `{"batchId":"b2","invited":0,"total":1,"complete":false,
"counts":{"failed":1},"results":[{"index":0,"email":"alice@example.com","status":"failed","reason":"internal_error"}]}`}
	var out bytes.Buffer
	err := inviteHubUsers(context.Background(), &out, hub.client(t), hubUsersInviteOptions{Args: []string{"alice@example.com"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "run the same command again")
	assert.Contains(t, out.String(), "failed           alice@example.com  (internal_error)")
}

// TestInviteHubUsers_JSONOutput prints the response as JSON.
func TestInviteHubUsers_JSONOutput(t *testing.T) {
	hub := &inviteTestHub{bulkStatus: http.StatusOK, bulkBody: inviteCompleteBody}
	var out bytes.Buffer
	err := inviteHubUsers(context.Background(), &out, hub.client(t), hubUsersInviteOptions{
		Args: []string{"alice@example.com"}, JSON: true,
	})
	require.NoError(t, err)
	var resp hubclient.InviteBatchResponse
	require.NoError(t, json.Unmarshal(out.Bytes(), &resp), out.String())
	assert.Equal(t, "b1", resp.BatchID)
	assert.Len(t, resp.Results, 3)
}

// TestInviteHubUsers_RefusedRequest prints the refused project entries of
// a 403 batch_projects_not_grantable and returns an error.
func TestInviteHubUsers_RefusedRequest(t *testing.T) {
	hub := &inviteTestHub{bulkStatus: http.StatusForbidden, bulkBody: `{"error":{"code":"forbidden","message":"nothing was created","details":{"reason":"batch_projects_not_grantable","batchId":"b3","projects":[{"index":0,"projectId":"` + inviteTestProjectID + `","reason":"role_not_grantable","roleDefinitionId":"r-member","denialCode":"role_assignment_forbidden"}]}}}`}
	var out bytes.Buffer
	err := inviteHubUsers(context.Background(), &out, hub.client(t), hubUsersInviteOptions{
		Args: []string{"alice@example.com"}, Projects: []string{"web=project-member"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invite refused")
	assert.Contains(t, out.String(), "no one was invited")
	assert.Contains(t, out.String(), "web: role_not_grantable (role r-member) [role_assignment_forbidden]")
}

// TestInviteHubUsers_LocalCaps refuses more than 1000 addresses, or more
// than 2000 memberships, without sending a request.
func TestInviteHubUsers_LocalCaps(t *testing.T) {
	hub := &inviteTestHub{bulkStatus: http.StatusOK, bulkBody: inviteCompleteBody}
	client := hub.client(t)
	many := make([]string, 1001)
	for i := range many {
		many[i] = fmt.Sprintf("u%d@example.com", i)
	}
	err := inviteHubUsers(context.Background(), io.Discard, client, hubUsersInviteOptions{Args: many})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "split the list")

	err = inviteHubUsers(context.Background(), io.Discard, client, hubUsersInviteOptions{
		Args: many[:1000], Projects: []string{"web=project-member", "other=project-member", "third=project-member"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "split the list")

	err = inviteHubUsers(context.Background(), io.Discard, client, hubUsersInviteOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no addresses given")
	assert.Empty(t, hub.bulkBodies)
}

// TestParseImportCSV_KeepsAllowListBehavior pins the allow-list import
// parser after it was factored into parseEmailRecords: the header row,
// empty records and rows without '@' are dropped, a second column is the
// note, and '#' lines are not comments there.
func TestParseImportCSV_KeepsAllowListBehavior(t *testing.T) {
	got, err := parseImportCSV(strings.NewReader("Email,Note\na@example.com, first\n\nno-at-sign,x\n#b@example.com\nc@example.com\n"))
	require.NoError(t, err)
	assert.Equal(t, []hubclient.AllowListAddRequest{
		{Email: "a@example.com", Note: "first"},
		{Email: "#b@example.com"},
		{Email: "c@example.com"},
	}, got)
}
