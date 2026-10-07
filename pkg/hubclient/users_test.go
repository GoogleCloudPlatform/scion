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

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
