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

//go:build !no_sqlite

package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// POST /api/v1/admin/users/invite Tests
// ============================================================================

func TestAdminUserInvite_Success(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/users/invite", UserInviteRequest{
		Email: "alice@example.com",
		Note:  "Workshop attendee",
	})

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp UserInviteResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Email != "alice@example.com" {
		t.Errorf("expected email alice@example.com, got %q", resp.Email)
	}
	if resp.Status != "invited" {
		t.Errorf("expected status invited, got %q", resp.Status)
	}
	if resp.ID == "" {
		t.Error("expected non-empty ID")
	}
	if resp.InvitedBy == "" {
		t.Error("expected non-empty invitedBy")
	}
	if resp.Created.IsZero() {
		t.Error("expected non-zero created time")
	}
}

func TestAdminUserInvite_DuplicateEmail(t *testing.T) {
	srv, _ := testServer(t)

	// First invite
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/users/invite", UserInviteRequest{
		Email: "bob@example.com",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("first invite failed: status %d: %s", rec.Code, rec.Body.String())
	}

	// Second invite of same email should return 409
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/admin/users/invite", UserInviteRequest{
		Email: "bob@example.com",
	})
	if rec.Code != http.StatusConflict {
		t.Errorf("expected status 409, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestAdminUserInvite_EmailNormalization(t *testing.T) {
	srv, _ := testServer(t)

	// Invite with uppercase email
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/users/invite", UserInviteRequest{
		Email: "Alice@Example.COM",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("invite failed: status %d: %s", rec.Code, rec.Body.String())
	}

	var resp UserInviteResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Email != "alice@example.com" {
		t.Errorf("expected normalized email alice@example.com, got %q", resp.Email)
	}

	// Trying to invite same email with different case should return 409
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/admin/users/invite", UserInviteRequest{
		Email: "alice@example.com",
	})
	if rec.Code != http.StatusConflict {
		t.Errorf("expected 409 for case-insensitive duplicate, got %d", rec.Code)
	}
}

func TestAdminUserInvite_InvalidEmail(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/users/invite", UserInviteRequest{
		Email: "not-an-email",
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", rec.Code)
	}
}

func TestAdminUserInvite_EmptyEmail(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/users/invite", UserInviteRequest{
		Email: "",
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", rec.Code)
	}
}

func TestAdminUserInvite_NonAdmin(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequestNoAuth(t, srv, http.MethodPost, "/api/v1/admin/users/invite", UserInviteRequest{
		Email: "user@example.com",
	})
	// Should be 401 or 403
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Errorf("expected status 401 or 403, got %d", rec.Code)
	}
}

func TestAdminUserInvite_MethodNotAllowed(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/users/invite", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected status 405, got %d", rec.Code)
	}
}

// ============================================================================
// POST /api/v1/admin/users/invite/bulk Tests
// ============================================================================

func TestAdminUserInviteBulk_Success(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/users/invite/bulk", UserInviteBulkRequest{
		Emails: []UserInviteRequest{
			{Email: "alice@example.com", Note: "Note A"},
			{Email: "bob@example.com", Note: "Note B"},
			{Email: "carol@example.com"},
		},
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp UserInviteBulkResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Invited != 3 {
		t.Errorf("expected 3 invited, got %d", resp.Invited)
	}
	if resp.Skipped != 0 {
		t.Errorf("expected 0 skipped, got %d", resp.Skipped)
	}
	if resp.Total != 3 {
		t.Errorf("expected total 3, got %d", resp.Total)
	}
}

func TestAdminUserInviteBulk_MixedSuccessSkip(t *testing.T) {
	srv, _ := testServer(t)

	// Invite one user first
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/users/invite", UserInviteRequest{
		Email: "existing@example.com",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("pre-invite failed: %d", rec.Code)
	}

	// Bulk invite with mix of new and existing
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/admin/users/invite/bulk", UserInviteBulkRequest{
		Emails: []UserInviteRequest{
			{Email: "existing@example.com"}, // should be skipped
			{Email: "new1@example.com"},
			{Email: "new2@example.com"},
		},
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp UserInviteBulkResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Invited != 2 {
		t.Errorf("expected 2 invited, got %d", resp.Invited)
	}
	if resp.Skipped != 1 {
		t.Errorf("expected 1 skipped, got %d", resp.Skipped)
	}
}

func TestAdminUserInviteBulk_ExceedsLimit(t *testing.T) {
	srv, _ := testServer(t)

	// Build a request with 1001 emails
	emails := make([]UserInviteRequest, 1001)
	for i := range emails {
		emails[i] = UserInviteRequest{Email: fmt.Sprintf("user%d@example.com", i)}
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/users/invite/bulk", UserInviteBulkRequest{
		Emails: emails,
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected status 400 for >1000 emails, got %d", rec.Code)
	}
}

func TestAdminUserInviteBulk_EmptyList(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/users/invite/bulk", UserInviteBulkRequest{
		Emails: []UserInviteRequest{},
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", rec.Code)
	}
}

func TestAdminUserInviteBulk_CSV(t *testing.T) {
	srv, _ := testServer(t)

	// Build a multipart form with a CSV file
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	csvContent := "email,note\nalice@example.com,Workshop\nbob@example.com,Team\n"
	part, err := writer.CreateFormFile("file", "invites.csv")
	if err != nil {
		t.Fatalf("failed to create form file: %v", err)
	}
	if _, err := part.Write([]byte(csvContent)); err != nil {
		t.Fatalf("failed to write CSV: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("failed to close writer: %v", err)
	}

	rec := doRequestRaw(t, srv, http.MethodPost, "/api/v1/admin/users/invite/bulk", buf.Bytes(), writer.FormDataContentType())

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp UserInviteBulkResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Invited != 2 {
		t.Errorf("expected 2 invited, got %d", resp.Invited)
	}
}

func TestAdminUserInviteBulk_NonAdmin(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequestNoAuth(t, srv, http.MethodPost, "/api/v1/admin/users/invite/bulk", UserInviteBulkRequest{
		Emails: []UserInviteRequest{{Email: "x@example.com"}},
	})
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Errorf("expected 401 or 403, got %d", rec.Code)
	}
}

// ============================================================================
// Deprecated Allow-List Wrapper Tests
// ============================================================================

func TestDeprecatedAllowListAdd_CreatesInvitedUser(t *testing.T) {
	srv, s := testServer(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/allow-list", AllowListAddRequest{
		Email: "deprecated@example.com",
		Note:  "via deprecated endpoint",
	})

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status 201, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify Deprecation header
	if rec.Header().Get("Deprecation") != "true" {
		t.Error("expected Deprecation: true header")
	}

	// Verify a User(invited) was created, not an AllowListEntry
	user, err := s.GetUserByEmail(t.Context(), "deprecated@example.com")
	if err != nil {
		t.Fatalf("expected user to be created: %v", err)
	}
	if user.Status != store.UserStatusInvited {
		t.Errorf("expected status invited, got %q", user.Status)
	}
}

func TestDeprecatedAllowListDelete_RemovesInvitedUser(t *testing.T) {
	srv, _ := testServer(t)

	// First create via invite endpoint
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/users/invite", UserInviteRequest{
		Email: "delete-me@example.com",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("invite failed: %d", rec.Code)
	}

	// Delete via deprecated allow-list endpoint
	rec = doRequest(t, srv, http.MethodDelete, "/api/v1/admin/allow-list/delete-me@example.com", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify Deprecation header
	if rec.Header().Get("Deprecation") != "true" {
		t.Error("expected Deprecation: true header")
	}
}

func TestDeprecatedAllowListDelete_CannotDeleteActiveUser(t *testing.T) {
	srv, _ := testServer(t)

	// The dev user already exists as active, so trying to delete it via
	// the deprecated allow-list endpoint should fail.
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/admin/allow-list/dev@localhost", nil)
	if rec.Code != http.StatusConflict {
		t.Errorf("expected status 409, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestDeprecatedAllowListGet_ReturnsInvitedUsers(t *testing.T) {
	srv, _ := testServer(t)

	// Create a few invited users
	for _, email := range []string{"get1@example.com", "get2@example.com"} {
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/users/invite", UserInviteRequest{
			Email: email,
			Note:  "test",
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("invite failed for %s: %d", email, rec.Code)
		}
	}

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/allow-list", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify Deprecation header
	if rec.Header().Get("Deprecation") != "true" {
		t.Error("expected Deprecation: true header")
	}

	var resp AllowListResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.TotalCount < 2 {
		t.Errorf("expected at least 2 items, got %d", resp.TotalCount)
	}

	// All items should have non-empty email
	for _, item := range resp.Items {
		if item.Email == "" {
			t.Error("expected non-empty email in allow list response")
		}
	}
}

func TestDeprecatedAllowListImport_CreatesInvitedUsers(t *testing.T) {
	srv, s := testServer(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/allow-list/import", AllowListBulkAddRequest{
		Emails: []AllowListAddRequest{
			{Email: "import1@example.com", Note: "imported"},
			{Email: "import2@example.com"},
		},
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify Deprecation header
	if rec.Header().Get("Deprecation") != "true" {
		t.Error("expected Deprecation: true header")
	}

	var resp AllowListBulkAddResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Added != 2 {
		t.Errorf("expected 2 added, got %d", resp.Added)
	}

	// Verify users were created with invited status
	user, err := s.GetUserByEmail(t.Context(), "import1@example.com")
	if err != nil {
		t.Fatalf("expected user to be created: %v", err)
	}
	if user.Status != store.UserStatusInvited {
		t.Errorf("expected status invited, got %q", user.Status)
	}
}

func TestAllowListDomains_NoDeprecationHeader(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/allow-list/domains", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Domains endpoint is NOT deprecated — must not have Deprecation header
	if dep := rec.Header().Get("Deprecation"); dep != "" {
		t.Errorf("domains endpoint should NOT have Deprecation header, got %q", dep)
	}
}

// ============================================================================
// GET /api/v1/users?status=invited Tests
// ============================================================================

func TestListUsers_StatusInvitedFilter(t *testing.T) {
	srv, _ := testServer(t)

	// Create invited users
	for _, email := range []string{"invited1@example.com", "invited2@example.com"} {
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/users/invite", UserInviteRequest{
			Email: email,
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("invite failed for %s: %d", email, rec.Code)
		}
	}

	// Query users with status=invited
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/users?status=invited", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp ListUsersResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.Users) < 2 {
		t.Errorf("expected at least 2 invited users, got %d", len(resp.Users))
	}

	// All returned users should have invited status
	for _, u := range resp.Users {
		if u.Status != store.UserStatusInvited {
			t.Errorf("expected status invited, got %q for user %s", u.Status, u.Email)
		}
	}
}

func TestListUsers_StatusActiveDoesNotIncludeInvited(t *testing.T) {
	srv, _ := testServer(t)

	// Create an invited user
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/users/invite", UserInviteRequest{
		Email: "shouldnotappear@example.com",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("invite failed: %d", rec.Code)
	}

	// Query users with status=active
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/users?status=active", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp ListUsersResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// No invited users should appear
	for _, u := range resp.Users {
		if u.Email == "shouldnotappear@example.com" {
			t.Error("invited user should not appear in status=active filter")
		}
	}
}

// ============================================================================
// Batch invite (ptone/scion#4952): decode, caps, per-address results
// ============================================================================

// postBulkRaw posts body as JSON to the bulk route as the dev admin.
func postBulkRaw(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequestRaw(t, srv, http.MethodPost, "/api/v1/admin/users/invite/bulk", []byte(body), "application/json")
}

func decodeBulk(t *testing.T, rec *httptest.ResponseRecorder) UserInviteBulkResponse {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp UserInviteBulkResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp
}

func countUsers(t *testing.T, s store.Store) int {
	t.Helper()
	res, err := s.ListUsers(context.Background(), store.UserFilter{}, store.ListOptions{Limit: 10000})
	require.NoError(t, err)
	return len(res.Items)
}

// TestAdminUserInviteBulk_StrictDecode: unknown fields at every level,
// wrong types, a non-object body, trailing data, a repeated key and an
// oversized body give 400; a role-like field gives 422
// role_selection_not_supported. None of them writes.
func TestAdminUserInviteBulk_StrictDecode(t *testing.T) {
	srv, s := testServer(t)
	before := countUsers(t, s)
	big := `{"emails":[{"email":"a@example.com","note":"` + strings.Repeat("x", inviteBulkMaxBodyBytes) + `"}]}`
	cases := []struct {
		name, body string
		status     int
		msg        string
		reason     string
	}{
		{"unknown top-level", `{"emails":[{"email":"a@example.com"}],"zeta":1,"extra":2}`, 400, `unknown field "extra"`, ""},
		{"unknown entry field", `{"emails":[{"email":"a@example.com","role":"admin"}]}`, 400, `unknown field "role"`, ""},
		{"unknown project field", `{"emails":[{"email":"a@example.com"}],"projects":[{"projectId":"p","roleDefinitionIds":["r"],"scope":"system"}]}`, 400, `unknown field "scope"`, ""},
		{"emails not an array", `{"emails":{"email":"a@example.com"}}`, 400, "invalid emails", ""},
		{"entry not an object", `{"emails":["a@example.com"]}`, 400, "a JSON object is required", ""},
		{"email wrong type", `{"emails":[{"email":7}]}`, 400, "must be a string", ""},
		{"note wrong type", `{"emails":[{"email":"a@example.com","note":true}]}`, 400, "must be a string", ""},
		{"projectId wrong type", `{"emails":[{"email":"a@example.com"}],"projects":[{"projectId":1,"roleDefinitionIds":["r"]}]}`, 400, "must be a string", ""},
		{"roles wrong type", `{"emails":[{"email":"a@example.com"}],"projects":[{"projectId":"p","roleDefinitionIds":"r"}]}`, 400, "roleDefinitionIds", ""},
		{"array body", `[{"email":"a@example.com"}]`, 400, "a JSON object is required", ""},
		{"trailing data", `{"emails":[{"email":"a@example.com"}]}}`, 400, "trailing data", ""},
		{"repeated key", `{"emails":[{"email":"a@example.com"}],"emails":[]}`, 400, "duplicate field", ""},
		{"body too large", big, 400, "request body too large", ""},
		{"role field", `{"emails":[{"email":"a@example.com"}],"role":"admin"}`, 422, "hub roles cannot be set", provisionReasonRoleNotSupported},
		{"hubRoles field", `{"emails":[{"email":"a@example.com"}],"hubRoles":["hub-admin"]}`, 422, "hub roles cannot be set", provisionReasonRoleNotSupported},
		{"roles field", `{"emails":[{"email":"a@example.com"}],"roles":[]}`, 422, "hub roles cannot be set", provisionReasonRoleNotSupported},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postBulkRaw(t, srv, tc.body)
			require.Equal(t, tc.status, rec.Code, rec.Body.String())
			resp := parseErrorResponse(t, rec.Body.Bytes())
			assert.Contains(t, resp.Error.Message, tc.msg)
			if tc.reason != "" {
				assert.Equal(t, tc.reason, resp.Error.Details["reason"])
			}
		})
	}
	assert.Equal(t, before, countUsers(t, s), "a refused request writes nothing")

	// A null role-like field is the same as an absent one.
	resp := decodeBulk(t, postBulkRaw(t, srv, `{"emails":[{"email":"nullrole@example.com"}],"role":null}`))
	assert.Equal(t, 1, resp.Counts.Created)
}

// TestAdminUserInviteBulk_RequestCaps covers every request cap (design
// §3.2), each refused with 400 before any write.
func TestAdminUserInviteBulk_RequestCaps(t *testing.T) {
	srv, s := testServer(t)
	before := countUsers(t, s)
	emails := func(n int) []UserInviteRequest {
		out := make([]UserInviteRequest, n)
		for i := range out {
			out[i] = UserInviteRequest{Email: fmt.Sprintf("cap%d@example.com", i)}
		}
		return out
	}
	projects := func(n, roles int) []UserInviteBulkProject {
		out := make([]UserInviteBulkProject, n)
		for i := range out {
			out[i].ProjectID = fmt.Sprintf("project-%d", i)
			for j := 0; j < roles; j++ {
				out[i].RoleDefinitionIDs = append(out[i].RoleDefinitionIDs, fmt.Sprintf("role-%d", j))
			}
		}
		return out
	}
	dupProjects := projects(2, 1)
	dupProjects[1].ProjectID = dupProjects[0].ProjectID
	cases := []struct {
		name  string
		req   UserInviteBulkRequest
		msg   string
		field string
	}{
		{"no emails", UserInviteBulkRequest{Emails: []UserInviteRequest{}}, "no emails provided", "emails"},
		{"too many emails", UserInviteBulkRequest{Emails: emails(inviteBulkMaxEmails + 1)}, "maximum 1000 emails", "emails"},
		{"too many projects", UserInviteBulkRequest{Emails: emails(1), Projects: projects(inviteBulkMaxProjects+1, 1)}, "maximum 20 projects", "projects"},
		{"no roles", UserInviteBulkRequest{Emails: emails(1), Projects: projects(1, 0)}, "roleDefinitionIds", "projects[0].roleDefinitionIds"},
		{"too many roles", UserInviteBulkRequest{Emails: emails(1), Projects: projects(1, inviteBulkMaxRolesPerProject+1)}, "roleDefinitionIds", "projects[0].roleDefinitionIds"},
		{"empty role", UserInviteBulkRequest{Emails: emails(1), Projects: []UserInviteBulkProject{{ProjectID: "p", RoleDefinitionIDs: []string{" "}}}}, "non-empty", "projects[0].roleDefinitionIds"},
		{"empty project", UserInviteBulkRequest{Emails: emails(1), Projects: []UserInviteBulkProject{{ProjectID: "", RoleDefinitionIDs: []string{"r"}}}}, "projectId is required", "projects[0].projectId"},
		{"duplicate project", UserInviteBulkRequest{Emails: emails(1), Projects: dupProjects}, "duplicate projectId", "projects[1].projectId"},
		{"too many memberships", UserInviteBulkRequest{Emails: emails(inviteBulkMaxEmails), Projects: projects(3, 1)}, "too many memberships", "projects"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/users/invite/bulk", tc.req)
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			resp := parseErrorResponse(t, rec.Body.Bytes())
			assert.Equal(t, ErrCodeInvalidRequest, resp.Error.Code)
			assert.Contains(t, resp.Error.Message, tc.msg)
			assert.Equal(t, tc.field, resp.Error.Details["field"])
		})
	}
	assert.Equal(t, before, countUsers(t, s), "a refused request writes nothing")
}

// seedBulkExisting creates an invited, an active and a suspended user.
func seedBulkExisting(t *testing.T, s store.Store) (invited, active, suspended *store.User) {
	t.Helper()
	ctx := context.Background()
	mk := func(id, email, status string) *store.User {
		u := &store.User{ID: tid(t.Name() + id), Email: email, Role: store.UserRoleMember, Status: status}
		require.NoError(t, s.CreateUser(ctx, u))
		got, err := s.GetUser(ctx, u.ID)
		require.NoError(t, err)
		return got
	}
	return mk("inv", "existing-invited@example.com", store.UserStatusInvited),
		mk("act", "existing-active@example.com", store.UserStatusActive),
		mk("sus", "existing-suspended@example.com", store.UserStatusSuspended)
}

// TestAdminUserInviteBulk_StatusesAndReasons: one result per input entry,
// in input order, with every address status, its reason, and userId only
// where design §3.5 allows it. No existing row is modified.
func TestAdminUserInviteBulk_StatusesAndReasons(t *testing.T) {
	srv, s := testServer(t)
	invited, active, suspended := seedBulkExisting(t, s)
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/admin/users/invite/bulk", UserInviteBulkRequest{Emails: []UserInviteRequest{
		{Email: "New.Person@Example.com", Note: "n"},
		{Email: invited.Email, Note: "a different note"},
		{Email: active.Email},
		{Email: suspended.Email},
		{Email: "not-an-email"},
		{Email: " new.person@example.COM "},
		{Email: "x@" + store.TestFixtureEmailDomain},
	}})
	resp := decodeBulk(t, rec)
	require.Len(t, resp.Results, 7)
	type row struct{ status, reason string }
	want := []row{
		{inviteBatchStatusCreated, ""},
		{inviteBatchStatusAlreadyInvited, provisionReasonPendingUserExists},
		{inviteBatchStatusAlreadyActive, provisionReasonUserExists},
		{inviteBatchStatusSuspended, provisionReasonSuspendedUserExists},
		{inviteBatchStatusInvalid, inviteBatchReasonInvalidEmail},
		{inviteBatchStatusDuplicate, inviteBatchReasonDuplicateInBatch},
		{inviteBatchStatusInvalid, provisionReasonReservedTestIdentity},
	}
	for i, w := range want {
		assert.Equal(t, i, resp.Results[i].Index)
		assert.Equal(t, w.status, resp.Results[i].Status, "entry %d", i)
		assert.Equal(t, w.reason, resp.Results[i].Reason, "entry %d", i)
		assert.Empty(t, resp.Results[i].Memberships, "no projects, no membership entries")
	}
	created, err := s.GetUserByEmail(context.Background(), "new.person@example.com")
	require.NoError(t, err)
	assert.Equal(t, created.ID, resp.Results[0].UserID)
	assert.Equal(t, invited.ID, resp.Results[1].UserID, "detail view: an invited row carries userId")
	assert.Empty(t, resp.Results[2].UserID, "an active row never carries userId")
	assert.Equal(t, suspended.ID, resp.Results[3].UserID)
	assert.Equal(t, "not-an-email", resp.Results[4].Input)
	require.NotNil(t, resp.Results[5].DuplicateOf)
	assert.Equal(t, 0, *resp.Results[5].DuplicateOf)

	assert.Equal(t, InviteBatchCounts{Created: 1, AlreadyInvited: 1, AlreadyActive: 1, Suspended: 1, Invalid: 2, Duplicate: 1}, resp.Counts)
	assert.Equal(t, 1, resp.Invited)
	assert.Equal(t, 3, resp.Skipped)
	assert.Equal(t, 7, resp.Total)
	assert.True(t, resp.Complete)
	assert.NotEmpty(t, resp.BatchID)

	again, err := s.GetUser(context.Background(), invited.ID)
	require.NoError(t, err)
	assert.Nil(t, again.InviteNote, "an existing invited row is never modified")
	require.NotNil(t, created.InviteNote)
	assert.Equal(t, "n", *created.InviteNote)
}

// bulkInviteOnlyCaller creates an active user holding only a custom
// system role with the given hub permissions.
func bulkInviteOnlyCaller(t *testing.T, s store.Store, name string, perms ...string) *store.User {
	t.Helper()
	ctx := context.Background()
	id := tid(t.Name() + name)
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: id, Email: id + "@example.com", DisplayName: name, Role: store.UserRoleMember, Status: store.UserStatusActive}))
	rd, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name: "bulk-" + tid(t.Name() + name)[:8], ScopeType: store.RoleScopeSystem, Permissions: perms,
	})
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: id,
		ScopeType: store.RoleScopeSystem, CreatedBy: "test",
	})
	require.NoError(t, err)
	u, err := s.GetUser(ctx, id)
	require.NoError(t, err)
	return u
}

// TestAdminUserInviteBulk_DetailCollapseWithoutUserRead: a caller holding
// user.invite but not user.read sees every existing row as exists /
// user_exists without userId (the provision rule), and the counts do not
// tell the three states apart.
func TestAdminUserInviteBulk_DetailCollapseWithoutUserRead(t *testing.T) {
	srv, s := testServerNoDevAuth(t)
	invited, active, suspended := seedBulkExisting(t, s)
	caller := bulkInviteOnlyCaller(t, s, "inviter", "user.invite")
	rec := doRequestAsUser(t, srv, caller, http.MethodPost, "/api/v1/admin/users/invite/bulk", UserInviteBulkRequest{Emails: []UserInviteRequest{
		{Email: "fresh@example.com"}, {Email: invited.Email}, {Email: active.Email}, {Email: suspended.Email},
	}})
	resp := decodeBulk(t, rec)
	require.Len(t, resp.Results, 4)
	assert.Equal(t, inviteBatchStatusCreated, resp.Results[0].Status)
	assert.NotEmpty(t, resp.Results[0].UserID, "a created row always carries userId")
	for i := 1; i < 4; i++ {
		assert.Equal(t, inviteBatchStatusExists, resp.Results[i].Status, "entry %d", i)
		assert.Equal(t, provisionReasonUserExists, resp.Results[i].Reason, "entry %d", i)
		assert.Empty(t, resp.Results[i].UserID, "entry %d", i)
	}
	assert.Equal(t, InviteBatchCounts{Created: 1, Exists: 3}, resp.Counts)
	assert.Equal(t, 3, resp.Skipped)
}

// raceOnceStore hides the user with email from GetUserByEmail once, so
// createPendingUserTx tries to insert it and loses the unique-index race.
type raceOnceStore struct {
	store.Store
	fault *storeFaultSwitch
	email string
	mu    sync.Mutex
	used  bool
}

func (r *raceOnceStore) GetUserByEmail(ctx context.Context, email string) (*store.User, error) {
	r.mu.Lock()
	hide := r.fault.Active() && email == r.email && !r.used
	if hide {
		r.used = true
	}
	r.mu.Unlock()
	if hide {
		return nil, store.ErrNotFound
	}
	return r.Store.GetUserByEmail(ctx, email)
}

func (r *raceOnceStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	if !r.fault.Active() {
		return r.Store.WithTx(ctx, fn)
	}
	return r.Store.WithTx(ctx, func(tx store.Store) error {
		return fn(&raceOnceTx{Store: tx, parent: r})
	})
}

type raceOnceTx struct {
	store.Store
	parent *raceOnceStore
}

func (r *raceOnceTx) GetUserByEmail(ctx context.Context, email string) (*store.User, error) {
	r.parent.mu.Lock()
	hide := email == r.parent.email && !r.parent.used
	if hide {
		r.parent.used = true
	}
	r.parent.mu.Unlock()
	if hide {
		return nil, store.ErrNotFound
	}
	return r.Store.GetUserByEmail(ctx, email)
}

// TestAdminUserInviteBulk_AlreadyExistsRace: a row inserted between the
// lookup and the insert surfaces as ErrAlreadyExists; the batch re-reads,
// classifies the address, and creates no second row.
func TestAdminUserInviteBulk_AlreadyExistsRace(t *testing.T) {
	srv, s := testServer(t)
	racer, fault := installStoreFault(t, srv, func(inner store.Store, f *storeFaultSwitch) *raceOnceStore {
		return &raceOnceStore{Store: inner, fault: f}
	})
	invited, _, _ := seedBulkExisting(t, s)
	racer.mu.Lock()
	racer.email = invited.Email
	racer.mu.Unlock()
	before := countUsers(t, s)
	fault.Arm()
	resp := decodeBulk(t, doRequest(t, srv, http.MethodPost, "/api/v1/admin/users/invite/bulk", UserInviteBulkRequest{Emails: []UserInviteRequest{
		{Email: invited.Email}, {Email: "after-race@example.com"},
	}}))
	require.True(t, racer.used, "the race was staged")
	assert.Equal(t, inviteBatchStatusAlreadyInvited, resp.Results[0].Status)
	assert.Equal(t, invited.ID, resp.Results[0].UserID)
	assert.Equal(t, inviteBatchStatusCreated, resp.Results[1].Status)
	assert.Equal(t, before+1, countUsers(t, s), "only the new address adds a row")
	assert.True(t, resp.Complete)
}

// transientTxStore makes CreateUser inside a transaction fail with
// store.ErrTransient for the first fails attempts. Only the user step
// calls CreateUser, so nothing else consumes the failures.
type transientTxStore struct {
	store.Store
	fault *storeFaultSwitch
	mu    sync.Mutex
	fails int
	calls int
}

func (f *transientTxStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	if !f.fault.Active() {
		return f.Store.WithTx(ctx, fn)
	}
	return f.Store.WithTx(ctx, func(tx store.Store) error {
		return fn(&transientCreateTx{Store: tx, parent: f})
	})
}

type transientCreateTx struct {
	store.Store
	parent *transientTxStore
}

func (t *transientCreateTx) CreateUser(ctx context.Context, u *store.User) error {
	t.parent.mu.Lock()
	t.parent.calls++
	fail := t.parent.fails > 0
	if fail {
		t.parent.fails--
	}
	t.parent.mu.Unlock()
	if fail {
		return fmt.Errorf("insert user: %w", store.ErrTransient)
	}
	return t.Store.CreateUser(ctx, u)
}

// TestAdminUserInviteBulk_TransientRetriedOnce: a transient store conflict
// on the user step is retried once; a second one fails that address with
// internal_error while the other addresses go on.
func TestAdminUserInviteBulk_TransientRetriedOnce(t *testing.T) {
	t.Run("one conflict is retried", func(t *testing.T) {
		srv, _ := testServer(t)
		flaky, fault := installStoreFault(t, srv, func(inner store.Store, f *storeFaultSwitch) *transientTxStore {
			return &transientTxStore{Store: inner, fault: f, fails: 1}
		})
		fault.Arm()
		resp := decodeBulk(t, doRequest(t, srv, http.MethodPost, "/api/v1/admin/users/invite/bulk", UserInviteBulkRequest{Emails: []UserInviteRequest{{Email: "retry@example.com"}}}))
		assert.Equal(t, inviteBatchStatusCreated, resp.Results[0].Status)
		assert.Equal(t, 2, flaky.calls)
		assert.True(t, resp.Complete)
	})
	t.Run("two conflicts fail the address only", func(t *testing.T) {
		srv, s := testServer(t)
		flaky, fault := installStoreFault(t, srv, func(inner store.Store, f *storeFaultSwitch) *transientTxStore {
			return &transientTxStore{Store: inner, fault: f, fails: 2}
		})
		fault.Arm()
		resp := decodeBulk(t, doRequest(t, srv, http.MethodPost, "/api/v1/admin/users/invite/bulk", UserInviteBulkRequest{Emails: []UserInviteRequest{
			{Email: "unlucky@example.com"}, {Email: "lucky@example.com"},
		}}))
		assert.Equal(t, inviteBatchStatusFailed, resp.Results[0].Status)
		assert.Equal(t, inviteBatchReasonInternalError, resp.Results[0].Reason)
		assert.Equal(t, inviteBatchStatusCreated, resp.Results[1].Status)
		assert.False(t, resp.Complete)
		assert.Equal(t, 1, resp.Counts.Failed)
		_, err := s.GetUserByEmail(context.Background(), "unlucky@example.com")
		assert.ErrorIs(t, err, store.ErrNotFound)
	})
}

// cancelAfterSetter applies every membership and cancels the run context
// after its first call.
type cancelAfterSetter struct {
	cancel context.CancelFunc
	calls  int
}

func (c *cancelAfterSetter) SetMemberRoles(context.Context, SetMemberRolesRequest) (*SetMemberRolesResult, *MembershipDecision) {
	c.calls++
	c.cancel()
	return &SetMemberRolesResult{Created: true, Changed: true}, nil
}

// TestAdminUserInviteBulk_RunDeadlineNotProcessed: addresses the run did
// not start before its deadline are failed/not_processed and create no
// row; a re-run completes them.
func TestAdminUserInviteBulk_RunDeadlineNotProcessed(t *testing.T) {
	srv, s := testServer(t)
	actor := NewAuthenticatedUser("actor-deadline", "actor@example.com", "Actor", "admin", string(ClientTypeWeb))
	entries := []inviteBatchEntry{{raw: "dl-a@example.com"}, {raw: "dl-b@example.com"}, {raw: "bad"}}

	t.Run("expired before the run", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		addrs := srv.validateInviteBatchAddresses(actor.ID(), entries)
		run := srv.runInviteBatch(ctx, actor, "b1", addrs, nil, nil, true)
		assert.Equal(t, inviteBatchStatusFailed, run.results[0].Status)
		assert.Equal(t, inviteBatchReasonNotProcessed, run.results[0].Reason)
		assert.Equal(t, inviteBatchReasonNotProcessed, run.results[1].Reason)
		assert.Equal(t, inviteBatchStatusInvalid, run.results[2].Status, "pure results are final whatever the deadline")
		assert.Equal(t, 2, run.counts.Failed)
		_, err := s.GetUserByEmail(context.Background(), "dl-a@example.com")
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	t.Run("expired partway", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		setter := &cancelAfterSetter{cancel: cancel}
		projects := []inviteBatchProject{{ProjectID: "p1", RoleDefinitionIDs: []string{"r1"}}}
		addrs := srv.validateInviteBatchAddresses(actor.ID(), entries)
		run := srv.runInviteBatch(ctx, actor, "b2", addrs, projects, setter, true)
		assert.Equal(t, inviteBatchStatusCreated, run.results[0].Status)
		assert.Equal(t, inviteBatchMembershipApplied, run.results[0].Memberships[0].Status)
		assert.Equal(t, inviteBatchStatusFailed, run.results[1].Status)
		assert.Equal(t, inviteBatchReasonNotProcessed, run.results[1].Reason)
		assert.Equal(t, inviteBatchMembershipSkipped, run.results[1].Memberships[0].Status)
		assert.Equal(t, 1, setter.calls)

		rerun := srv.runInviteBatch(context.Background(), actor, "b3", srv.validateInviteBatchAddresses(actor.ID(), entries), nil, nil, true)
		assert.Equal(t, inviteBatchStatusAlreadyInvited, rerun.results[0].Status)
		assert.Equal(t, inviteBatchStatusCreated, rerun.results[1].Status, "a re-run completes the address")
	})
}

// TestAdminUserInviteBulk_AuditPerCreatedAndAggregate: one user_invited
// per created row, in single invite's form plus user_id and batch_id, and
// one user_invited_bulk per run with the counts.
func TestAdminUserInviteBulk_AuditPerCreatedAndAggregate(t *testing.T) {
	srv, s := testServer(t)
	invited, _, _ := seedBulkExisting(t, s)
	audit := &recordingAuditLogger{}
	srv.SetAuditLogger(audit)
	resp := decodeBulk(t, doRequest(t, srv, http.MethodPost, "/api/v1/admin/users/invite/bulk", UserInviteBulkRequest{Emails: []UserInviteRequest{
		{Email: "aud-1@example.com"}, {Email: "aud-2@example.com"}, {Email: invited.Email}, {Email: "bad"},
	}}))

	var perUser, bulk []*InviteAuditEvent
	for _, e := range audit.invite {
		switch e.EventType {
		case InviteAuditUserInvited:
			perUser = append(perUser, e)
		case InviteAuditUserInvitedBulk:
			bulk = append(bulk, e)
		}
	}
	require.Len(t, perUser, 2, "one user_invited per created row")
	for i, e := range perUser {
		assert.Equal(t, resp.Results[i].Email, e.Email)
		assert.True(t, e.Success)
		assert.Equal(t, resp.Results[i].UserID, e.Details["user_id"])
		assert.Equal(t, resp.BatchID, e.Details["batch_id"])
		assert.NotEmpty(t, e.ActorID)
	}
	require.Len(t, bulk, 1, "one user_invited_bulk per run")
	b := bulk[0]
	assert.True(t, b.Success)
	assert.Equal(t, 2, b.Count)
	assert.Equal(t, resp.BatchID, b.Details["batch_id"])
	assert.Equal(t, "1", b.Details["already_invited"])
	assert.Equal(t, "1", b.Details["invalid"])
	assert.Equal(t, "0", b.Details["failed"])
	assert.Equal(t, "", b.Details["projects"])
}

// TestAdminUserInviteBulk_MultipartRules: the CSV form takes only the file
// field; a projects field gives 400 "projects require a JSON request".
func TestAdminUserInviteBulk_MultipartRules(t *testing.T) {
	srv, s := testServer(t)
	before := countUsers(t, s)
	build := func(fields map[string]string) ([]byte, string) {
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		part, err := w.CreateFormFile("file", "invites.csv")
		require.NoError(t, err)
		_, err = part.Write([]byte("email\nmp@example.com\n"))
		require.NoError(t, err)
		for k, v := range fields {
			require.NoError(t, w.WriteField(k, v))
		}
		require.NoError(t, w.Close())
		return buf.Bytes(), w.FormDataContentType()
	}
	body, ct := build(map[string]string{"projects": `[{"projectId":"p","roleDefinitionIds":["r"]}]`})
	rec := doRequestRaw(t, srv, http.MethodPost, "/api/v1/admin/users/invite/bulk", body, ct)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, parseErrorResponse(t, rec.Body.Bytes()).Error.Message, "projects require a JSON request")

	body, ct = build(map[string]string{"other": "x"})
	rec = doRequestRaw(t, srv, http.MethodPost, "/api/v1/admin/users/invite/bulk", body, ct)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Equal(t, before, countUsers(t, s))

	body, ct = build(nil)
	resp := decodeBulk(t, doRequestRaw(t, srv, http.MethodPost, "/api/v1/admin/users/invite/bulk", body, ct))
	assert.Equal(t, 1, resp.Counts.Created)
	require.Len(t, resp.Results, 1)
	assert.Equal(t, "mp@example.com", resp.Results[0].Email)
}

// bulkStatusTally counts the results by status.
func bulkStatusTally(resp UserInviteBulkResponse) map[string]int {
	tally := map[string]int{}
	for _, r := range resp.Results {
		tally[r.Status]++
	}
	return tally
}

// TestAdminUserInviteBulk_CountsMatchRowsForEachCallerClass: for a caller
// with user.read and for one without it, every count equals the number of
// results with that status, so the counts tell no caller more than its
// rows; counts.exists appears only in the view whose rows show "exists".
func TestAdminUserInviteBulk_CountsMatchRowsForEachCallerClass(t *testing.T) {
	srv, s := testServerNoDevAuth(t)
	invited, active, suspended := seedBulkExisting(t, s)
	classes := []struct {
		name       string
		caller     *store.User
		wantExists bool
	}{
		{"with user.read", bulkInviteOnlyCaller(t, s, "reader", "user.invite", "user.read"), false},
		{"without user.read", bulkInviteOnlyCaller(t, s, "inviteonly", "user.invite"), true},
	}
	for _, c := range classes {
		t.Run(c.name, func(t *testing.T) {
			fresh := tid(t.Name()) + "@example.com"
			rec := doRequestAsUser(t, srv, c.caller, http.MethodPost, "/api/v1/admin/users/invite/bulk", UserInviteBulkRequest{Emails: []UserInviteRequest{
				{Email: fresh}, {Email: invited.Email}, {Email: active.Email}, {Email: suspended.Email}, {Email: "bad"}, {Email: strings.ToUpper(fresh)},
			}})
			resp := decodeBulk(t, rec)
			tally := bulkStatusTally(resp)
			assert.Equal(t, InviteBatchCounts{
				Created:        tally[inviteBatchStatusCreated],
				AlreadyInvited: tally[inviteBatchStatusAlreadyInvited],
				AlreadyActive:  tally[inviteBatchStatusAlreadyActive],
				Suspended:      tally[inviteBatchStatusSuspended],
				Exists:         tally[inviteBatchStatusExists],
				Invalid:        tally[inviteBatchStatusInvalid],
				Duplicate:      tally[inviteBatchStatusDuplicate],
				Failed:         tally[inviteBatchStatusFailed],
			}, resp.Counts, "every count is the tally of its row status")
			assert.Equal(t, tally[inviteBatchStatusAlreadyInvited]+tally[inviteBatchStatusAlreadyActive]+tally[inviteBatchStatusSuspended]+tally[inviteBatchStatusExists], resp.Skipped)
			assert.Equal(t, tally[inviteBatchStatusCreated], resp.Invited)

			var raw struct {
				Counts map[string]json.RawMessage `json:"counts"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
			_, hasExists := raw.Counts["exists"]
			assert.Equal(t, c.wantExists, hasExists, "counts.exists only in the view whose rows show exists")
			if c.wantExists {
				assert.Equal(t, 3, tally[inviteBatchStatusExists])
				assert.Zero(t, tally[inviteBatchStatusAlreadyInvited]+tally[inviteBatchStatusAlreadyActive]+tally[inviteBatchStatusSuspended])
			} else {
				assert.Zero(t, tally[inviteBatchStatusExists])
				assert.Equal(t, 1, tally[inviteBatchStatusAlreadyInvited])
			}
		})
	}
}

// TestAdminUserInviteBulk_BodyCapBoundaries: a JSON body of exactly
// inviteBulkMaxBodyBytes is decoded and one byte more is refused with 400;
// a multipart body over inviteBulkMaxMultipartBytes is refused with 400.
// Neither refusal writes.
func TestAdminUserInviteBulk_BodyCapBoundaries(t *testing.T) {
	srv, s := testServer(t)
	body := `{"emails":[{"email":"cap-edge@example.com"}]}`
	atCap := body + strings.Repeat(" ", inviteBulkMaxBodyBytes-len(body))
	require.Len(t, atCap, inviteBulkMaxBodyBytes)
	resp := decodeBulk(t, postBulkRaw(t, srv, atCap))
	assert.Equal(t, 1, resp.Counts.Created, "a body of exactly the cap is decoded")

	before := countUsers(t, s)
	over := `{"emails":[{"email":"cap-over@example.com"}]}`
	over += strings.Repeat(" ", inviteBulkMaxBodyBytes+1-len(over))
	require.Len(t, over, inviteBulkMaxBodyBytes+1)
	rec := postBulkRaw(t, srv, over)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Equal(t, "request body too large", parseErrorResponse(t, rec.Body.Bytes()).Error.Message)

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("file", "big.csv")
	require.NoError(t, err)
	_, err = part.Write([]byte("email\ncap-mp@example.com\n" + strings.Repeat("#", inviteBulkMaxMultipartBytes)))
	require.NoError(t, err)
	require.NoError(t, w.Close())
	rec = doRequestRaw(t, srv, http.MethodPost, "/api/v1/admin/users/invite/bulk", buf.Bytes(), w.FormDataContentType())
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Equal(t, "failed to parse multipart form", parseErrorResponse(t, rec.Body.Bytes()).Error.Message)
	assert.Equal(t, before, countUsers(t, s), "refused bodies write nothing")
}

// TestAdminUserInviteBulk_InvalidThenValidSameAddress: an entry refused for
// its note does not claim the address, so a later valid entry for the
// same address is processed, not reported as a duplicate.
func TestAdminUserInviteBulk_InvalidThenValidSameAddress(t *testing.T) {
	srv, s := testServer(t)
	resp := decodeBulk(t, doRequest(t, srv, http.MethodPost, "/api/v1/admin/users/invite/bulk", UserInviteBulkRequest{Emails: []UserInviteRequest{
		{Email: "twice@example.com", Note: "bad\x01note"},
		{Email: "Twice@Example.com", Note: "fine"},
	}}))
	require.Len(t, resp.Results, 2)
	assert.Equal(t, inviteBatchStatusInvalid, resp.Results[0].Status)
	assert.Equal(t, inviteBatchReasonNoteInvalidCharacters, resp.Results[0].Reason)
	assert.Equal(t, inviteBatchStatusCreated, resp.Results[1].Status)
	assert.Nil(t, resp.Results[1].DuplicateOf)
	u, err := s.GetUserByEmail(context.Background(), "twice@example.com")
	require.NoError(t, err)
	require.NotNil(t, u.InviteNote)
	assert.Equal(t, "fine", *u.InviteNote)
}
