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

package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUserLastLoginJSON(t *testing.T) {
	for _, lastLogin := range []time.Time{{}, time.Date(2026, 10, 16, 8, 0, 0, 0, time.UTC)} {
		t.Run(lastLogin.String(), func(t *testing.T) {
			data, err := json.Marshal(User{LastLogin: lastLogin})
			require.NoError(t, err)
			var fields map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(data, &fields))
			if lastLogin.IsZero() {
				assert.NotContains(t, fields, "lastLogin")
			} else {
				assert.JSONEq(t, `"2026-10-16T08:00:00Z"`, string(fields["lastLogin"]))
			}
		})
	}
}

// TestUserService_List_SearchQueryEncoding verifies ListUsersOptions.Search
// is encoded as the "search" query param the Hub's listUsers handler expects
// (pkg/hub/handlers_users_core.go: store.UserFilter.Search). This is how the
// CLI's `scion list --owner <name-or-email>` resolves a name/email to a user
// ID (ptone/scion#2146).
func TestUserService_List_SearchQueryEncoding(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "alice@example.com", r.URL.Query().Get("search"))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"users": []}`))
	}))
	defer server.Close()

	client, err := New(server.URL)
	require.NoError(t, err)

	_, err = client.Users().List(context.Background(), &ListUsersOptions{Search: "alice@example.com"})
	require.NoError(t, err)
}

// TestUserService_List_SearchOmittedWhenUnset verifies no "search" param is
// sent when Search is empty.
func TestUserService_List_SearchOmittedWhenUnset(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, has := r.URL.Query()["search"]
		assert.False(t, has)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"users": []}`))
	}))
	defer server.Close()

	client, err := New(server.URL)
	require.NoError(t, err)

	_, err = client.Users().List(context.Background(), &ListUsersOptions{})
	require.NoError(t, err)
}

// TestUserService_List_PageAndSearchCombine verifies Search combines with
// pagination options on the same request.
func TestUserService_List_PageAndSearchCombine(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		assert.Equal(t, "bob", query.Get("search"))
		assert.Equal(t, "10", query.Get("limit"))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"users": []}`))
	}))
	defer server.Close()

	client, err := New(server.URL)
	require.NoError(t, err)

	_, err = client.Users().List(context.Background(), &ListUsersOptions{
		Search: "bob",
		Page:   apiclient.PageOptions{Limit: 10},
	})
	require.NoError(t, err)
}

// TestUserService_List_NilOptions verifies List tolerates a nil options
// pointer (mirrors AgentService.List's nil-safety).
func TestUserService_List_NilOptions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"users": []}`))
	}))
	defer server.Close()

	client, err := New(server.URL)
	require.NoError(t, err)

	_, err = client.Users().List(context.Background(), nil)
	require.NoError(t, err)
}

// TestUserService_Provision_RequestAndResponse pins the wire shape of
// Provision: POST /api/v1/users with email, displayName and note (no role),
// and the 201 response decoded into ProvisionUserResponse.
func TestUserService_Provision_RequestAndResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/users", r.URL.Path)
		var body map[string]interface{}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, map[string]interface{}{"email": "bob@example.com", "displayName": "Bob", "note": "hi"}, body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"user":{"id":"u1","email":"bob@example.com","status":"invited","displayName":"Bob","invitedBy":"a1","inviteNote":"hi","created":"2026-10-06T00:00:00Z"},"created":true,"warnings":["domain_not_authorized"]}`))
	}))
	defer server.Close()

	client, err := New(server.URL)
	require.NoError(t, err)
	name, note := "Bob", "hi"
	resp, err := client.Users().Provision(context.Background(), &ProvisionUserRequest{Email: "bob@example.com", DisplayName: &name, Note: &note})
	require.NoError(t, err)
	assert.True(t, resp.Created)
	assert.Equal(t, "u1", resp.User.ID)
	assert.Equal(t, "invited", resp.User.Status)
	require.NotNil(t, resp.User.InviteNote)
	assert.Equal(t, "hi", *resp.User.InviteNote)
	require.NotNil(t, resp.User.Created)
	assert.Equal(t, []string{"domain_not_authorized"}, resp.Warnings)
}

// TestUserService_Provision_MinimalReplay pins that a 200 replay carrying
// only {email, status} decodes with every other field empty.
func TestUserService_Provision_MinimalReplay(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"user":{"email":"bob@example.com","status":"invited"},"created":false}`))
	}))
	defer server.Close()

	client, err := New(server.URL)
	require.NoError(t, err)
	resp, err := client.Users().Provision(context.Background(), &ProvisionUserRequest{Email: "bob@example.com"})
	require.NoError(t, err)
	assert.False(t, resp.Created)
	assert.Empty(t, resp.User.ID)
	assert.Nil(t, resp.User.Created)
	assert.Equal(t, "bob@example.com", resp.User.Email)
}

// TestUserService_Provision_TypedErrors pins that 409 and 422 responses map
// to *ProvisionError exposing details.reason and the optional
// details.userId, and that other errors stay plain API errors.
func TestUserService_Provision_TypedErrors(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantTyped  bool
		wantReason string
		wantUserID string
	}{
		{"pending with userId", http.StatusConflict, `{"error":{"code":"conflict","message":"x","details":{"reason":"pending_user_exists","userId":"u9"}}}`, true, ProvisionReasonPendingUserExists, "u9"},
		{"user exists without userId", http.StatusConflict, `{"error":{"code":"conflict","message":"user already exists","details":{"reason":"user_exists"}}}`, true, ProvisionReasonUserExists, ""},
		{"suspended", http.StatusConflict, `{"error":{"code":"conflict","message":"x","details":{"reason":"user_suspended_exists"}}}`, true, ProvisionReasonSuspendedUserExists, ""},
		{"role refused", http.StatusUnprocessableEntity, `{"error":{"code":"unprocessable","message":"x","details":{"field":"role","reason":"privileged_role_not_provisionable"}}}`, true, ProvisionReasonPrivilegedRole, ""},
		{"forbidden is not typed", http.StatusForbidden, `{"error":{"code":"forbidden","message":"x"}}`, false, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			client, err := New(server.URL)
			require.NoError(t, err)
			_, err = client.Users().Provision(context.Background(), &ProvisionUserRequest{Email: "bob@example.com"})
			require.Error(t, err)
			var pe *ProvisionError
			if !tc.wantTyped {
				assert.False(t, errors.As(err, &pe))
				var apiErr *apiclient.APIError
				assert.True(t, errors.As(err, &apiErr))
				return
			}
			require.True(t, errors.As(err, &pe), "got %T: %v", err, err)
			assert.Equal(t, tc.status, pe.StatusCode)
			assert.Equal(t, tc.wantReason, pe.Reason)
			assert.Equal(t, tc.wantUserID, pe.UserID)
			var apiErr *apiclient.APIError
			assert.True(t, errors.As(err, &apiErr), "ProvisionError unwraps to *apiclient.APIError")
		})
	}
}

// TestUserService_InviteBatch_RequestAndResponse pins the batch invite
// request body and the decoding of every response field.
func TestUserService_InviteBatch_RequestAndResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/admin/users/invite/bulk", r.URL.Path)
		var body map[string]interface{}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, map[string]interface{}{
			"emails": []interface{}{
				map[string]interface{}{"email": "alice@example.com", "note": "Q3"},
				map[string]interface{}{"email": "bad"},
			},
			"projects": []interface{}{
				map[string]interface{}{"projectId": "p1", "roleDefinitionIds": []interface{}{"r1"}},
			},
		}, body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"batchId":"b1","invited":1,"skipped":0,"total":3,"complete":false,
"counts":{"created":1,"already_invited":0,"already_active":0,"suspended":0,"exists":0,"invalid":1,"duplicate":1,"failed":0,"membership_failed":1},
"results":[
 {"index":0,"email":"alice@example.com","status":"created","userId":"u1","memberships":[{"projectId":"p1","status":"failed","code":"membership_changed","cause":"actor_authority_changed"}]},
 {"index":1,"input":"bad","status":"invalid","reason":"invalid_email","memberships":[{"projectId":"p1","status":"skipped","code":"address_not_invited"}]},
 {"index":2,"email":"alice@example.com","status":"duplicate","reason":"duplicate_in_batch","duplicateOf":0}
]}`))
	}))
	defer server.Close()

	client, err := New(server.URL)
	require.NoError(t, err)
	resp, err := client.Users().InviteBatch(context.Background(), &InviteBatchRequest{
		Emails:   []InviteBatchEmail{{Email: "alice@example.com", Note: "Q3"}, {Email: "bad"}},
		Projects: []InviteBatchProject{{ProjectID: "p1", RoleDefinitionIDs: []string{"r1"}}},
	})
	require.NoError(t, err)
	assert.Equal(t, "b1", resp.BatchID)
	assert.Equal(t, 3, resp.Total)
	assert.False(t, resp.Complete)
	assert.Equal(t, InviteBatchCounts{Created: 1, Invalid: 1, Duplicate: 1, MembershipFailed: 1}, resp.Counts)
	require.Len(t, resp.Results, 3)
	assert.Equal(t, InviteBatchStatusCreated, resp.Results[0].Status)
	assert.Equal(t, "u1", resp.Results[0].UserID)
	require.Len(t, resp.Results[0].Memberships, 1)
	assert.Equal(t, InviteBatchMembership{ProjectID: "p1", Status: InviteBatchMembershipFailed, Code: "membership_changed", Cause: "actor_authority_changed"}, resp.Results[0].Memberships[0])
	assert.Equal(t, "bad", resp.Results[1].Input)
	assert.Equal(t, InviteBatchMembershipSkipped, resp.Results[1].Memberships[0].Status)
	require.NotNil(t, resp.Results[2].DuplicateOf)
	assert.Equal(t, 0, *resp.Results[2].DuplicateOf)
}

// TestUserService_InviteBatch_ProjectsOmittedWhenUnset pins that a batch
// without projects sends no projects field.
func TestUserService_InviteBatch_ProjectsOmittedWhenUnset(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		_, has := body["projects"]
		assert.False(t, has, "projects omitted: %v", body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"batchId":"b2","invited":1,"total":1,"complete":true,"counts":{"created":1},"results":[{"index":0,"email":"a@example.com","status":"created","userId":"u1"}]}`))
	}))
	defer server.Close()

	client, err := New(server.URL)
	require.NoError(t, err)
	resp, err := client.Users().InviteBatch(context.Background(), &InviteBatchRequest{Emails: []InviteBatchEmail{{Email: "a@example.com"}}})
	require.NoError(t, err)
	assert.True(t, resp.Complete)
}

// TestUserService_InviteBatch_TypedRefusal pins that a 403
// batch_projects_not_grantable maps to *InviteBatchError with the batch ID
// and every refused entry, and that other errors stay plain API errors.
func TestUserService_InviteBatch_TypedRefusal(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		wantTyped bool
	}{
		{"projects not grantable", http.StatusForbidden, `{"error":{"code":"forbidden","message":"x","details":{"reason":"batch_projects_not_grantable","batchId":"b9","projects":[{"index":0,"projectId":"p1","reason":"project_not_manageable"},{"index":1,"projectId":"p2","reason":"role_not_grantable","roleDefinitionId":"r2","denialCode":"target_role_protected","details":{"roleDefinitionId":"r2"}}]}}}`, true},
		{"plain forbidden", http.StatusForbidden, `{"error":{"code":"forbidden","message":"x"}}`, false},
		{"bad request", http.StatusBadRequest, `{"error":{"code":"invalid_request","message":"unknown field \"x\""}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			client, err := New(server.URL)
			require.NoError(t, err)
			_, err = client.Users().InviteBatch(context.Background(), &InviteBatchRequest{Emails: []InviteBatchEmail{{Email: "a@example.com"}}})
			require.Error(t, err)
			var be *InviteBatchError
			var apiErr *apiclient.APIError
			assert.True(t, errors.As(err, &apiErr), "unwraps to *apiclient.APIError")
			if !tc.wantTyped {
				assert.False(t, errors.As(err, &be))
				return
			}
			require.True(t, errors.As(err, &be), "got %T: %v", err, err)
			assert.Equal(t, "b9", be.BatchID)
			require.Len(t, be.Projects, 2)
			assert.Equal(t, InviteBatchProjectRefusal{Index: 0, ProjectID: "p1", Reason: "project_not_manageable"}, be.Projects[0])
			assert.Equal(t, "r2", be.Projects[1].RoleDefinitionID)
			assert.Equal(t, "target_role_protected", be.Projects[1].DenialCode)
			assert.Equal(t, "r2", be.Projects[1].Details["roleDefinitionId"])
		})
	}
}

// TestUserService_DeleteBlockers_RequestAndResponse verifies DeleteBlockers
// sends GET /api/v1/users/{id}/delete-blockers and decodes the lists.
func TestUserService_DeleteBlockers_RequestAndResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/api/v1/users/u-1/delete-blockers", r.URL.Path)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"agents":[{"id":"a-1","slug":"worker","projectId":"p-1","deletionState":"failed"}],` +
			`"edges":[{"id":"e-1","delegateType":"agent","delegateId":"a-2","scopeType":"project","scopeId":"p-2"}],"truncated":true}`))
	}))
	defer server.Close()

	client, err := New(server.URL)
	require.NoError(t, err)

	got, err := client.Users().DeleteBlockers(context.Background(), "u-1")
	require.NoError(t, err)
	assert.Equal(t, &UserDeleteBlockers{
		Agents:    []UserDeleteBlockingAgent{{ID: "a-1", Slug: "worker", ProjectID: "p-1", DeletionState: "failed"}},
		Edges:     []UserDeleteBlockingEdge{{ID: "e-1", DelegateType: "agent", DelegateID: "a-2", ScopeType: "project", ScopeID: "p-2"}},
		Truncated: true,
	}, got)
}

// TestUserService_DeleteBlockers_Forbidden verifies a 403 is returned as an
// API error.
func TestUserService_DeleteBlockers_Forbidden(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"requires user.delete permission"}}`))
	}))
	defer server.Close()

	client, err := New(server.URL)
	require.NoError(t, err)

	_, err = client.Users().DeleteBlockers(context.Background(), "u-1")
	var apiErr *apiclient.APIError
	require.True(t, errors.As(err, &apiErr), "got %v", err)
	assert.Equal(t, http.StatusForbidden, apiErr.StatusCode)
	assert.Equal(t, "forbidden", apiErr.Code)
}

// TestUserService_ForceDelete_Accepted verifies the request and the decoded
// deletion of a 202.
func TestUserService_ForceDelete_Accepted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodDelete, r.Method)
		assert.Equal(t, "/api/v1/users/u-1", r.URL.Path)
		assert.Equal(t, "true", r.URL.Query().Get("force"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"deletionId":"d-1","userId":"u-1","state":"stopping","requestedAt":"2026-10-28T10:00:00Z",` +
			`"requestedBy":{"kind":"user","id":"admin"},"completedAt":null,"lastReconciledAt":null,"lastError":"",` +
			`"agents":[{"id":"a-1","slug":"worker","projectId":"p-1","outcome":"failed","attempts":2,"lastError":"broker unreachable"}],` +
			`"counts":{"pending":0,"stopping":0,"failed":1,"stopped":0,"removed":0},"orphanedByRevoke":[{"agentId":"a-9","edgeId":"e-9"}]}`))
	}))
	defer server.Close()

	client, err := New(server.URL)
	require.NoError(t, err)
	got, err := client.Users().ForceDelete(context.Background(), "u-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "d-1", got.DeletionID)
	assert.Equal(t, "stopping", got.State)
	assert.Equal(t, UserDeletionPrincipal{Kind: "user", ID: "admin"}, got.RequestedBy)
	assert.Equal(t, []UserDeletionAgent{{ID: "a-1", Slug: "worker", ProjectID: "p-1", Outcome: "failed", Attempts: 2, LastError: "broker unreachable"}}, got.Agents)
	assert.Equal(t, UserDeletionCounts{Failed: 1}, got.Counts)
	assert.Equal(t, []UserDeletionRevokedEdge{{AgentID: "a-9", EdgeID: "e-9"}}, got.OrphanedByRevoke)
}

// TestUserService_ForceDelete_NoContent verifies a 204 (nothing blocked)
// returns no deletion and no error.
func TestUserService_ForceDelete_NoContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client, err := New(server.URL)
	require.NoError(t, err)
	got, err := client.Users().ForceDelete(context.Background(), "u-1")
	require.NoError(t, err)
	assert.Nil(t, got)
}

// TestUserService_ForceDelete_Forbidden verifies a 403
// user_delete_force_forbidden is returned as an API error.
func TestUserService_ForceDelete_Forbidden(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"user_delete_force_forbidden","message":"cannot force-delete this user","details":{"agents":[{"id":"a-1","projectId":"p-1"}]}}}`))
	}))
	defer server.Close()

	client, err := New(server.URL)
	require.NoError(t, err)
	_, err = client.Users().ForceDelete(context.Background(), "u-1")
	var apiErr *apiclient.APIError
	require.True(t, errors.As(err, &apiErr), "got %v", err)
	assert.Equal(t, http.StatusForbidden, apiErr.StatusCode)
	assert.Equal(t, "user_delete_force_forbidden", apiErr.Code)
}

// TestUserService_GetDeletion verifies the request path and the decoded
// deletion.
func TestUserService_GetDeletion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/api/v1/users/u-1/deletion", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"deletionId":"d-1","userId":"u-1","state":"completed","requestedAt":"2026-10-28T10:00:00Z",` +
			`"requestedBy":{"kind":"user","id":"admin"},"completedAt":"2026-10-28T10:05:00Z","agents":[],` +
			`"counts":{"pending":0,"stopping":0,"failed":0,"stopped":2,"removed":1},"orphanedByRevoke":[]}`))
	}))
	defer server.Close()

	client, err := New(server.URL)
	require.NoError(t, err)
	got, err := client.Users().GetDeletion(context.Background(), "u-1")
	require.NoError(t, err)
	assert.Equal(t, "completed", got.State)
	require.NotNil(t, got.CompletedAt)
	assert.Equal(t, UserDeletionCounts{Stopped: 2, Removed: 1}, got.Counts)
}

// TestUserService_GetDeletion_NotFound verifies that a 404 (no deletion,
// or the experiment off) is returned as an API error.
func TestUserService_GetDeletion_NotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"User deletion not found"}}`))
	}))
	defer server.Close()

	client, err := New(server.URL)
	require.NoError(t, err)
	got, err := client.Users().GetDeletion(context.Background(), "u-1")
	assert.Nil(t, got)
	var apiErr *apiclient.APIError
	require.True(t, errors.As(err, &apiErr), "got %v", err)
	assert.Equal(t, http.StatusNotFound, apiErr.StatusCode)
}
