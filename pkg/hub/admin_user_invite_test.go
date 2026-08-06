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
	"encoding/json"
	"mime/multipart"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
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
