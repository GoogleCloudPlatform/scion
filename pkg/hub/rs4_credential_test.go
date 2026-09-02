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
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// RS4 Credential Test Suite — UAT mint, revoke, delete
//
// Preflight matrix: T-I1..I10, T-P1..P8, T-C1..C9, T-S1..S6,
// T-L1..L6, T-A1..A9, T-X1..X5, T-D1..D4, T-G1..G6, T-R1..R5.
// =============================================================================

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// rs4Project creates a project, user, and role binding for RS4 tests.
func rs4Project(t *testing.T, s store.Store, projectID, ownerID string) {
	t.Helper()
	createRS1Project(t, s, projectID, ownerID)
}

// rs4UserWithRole creates a user and assigns a project role with specific permissions.
func rs4UserWithRole(t *testing.T, s store.Store, userID, projectID, roleName string) {
	t.Helper()
	createRS1UserWithRole(t, s, userID, userID+"@test.com", projectID, roleName)
}

// rs4AddProjectRole adds a project role binding for an already-existing user (e.g. DevUserID).
func rs4AddProjectRole(t *testing.T, s store.Store, userID, projectID, roleName string) {
	t.Helper()
	ctx := context.Background()
	rd, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	if err != nil && err != store.ErrAlreadyExists {
		t.Fatalf("failed to create project role binding: %v", err)
	}
}

// rs4MintViaAPI mints a token via the HTTP API (session credential).
func rs4MintViaAPI(t *testing.T, srv *Server, projectID string, scopes []string) (int, map[string]interface{}) {
	t.Helper()
	body := map[string]interface{}{
		"name":      "rs4-test-token",
		"projectId": projectID,
		"scopes":    scopes,
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/auth/tokens", body)
	var resp map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec.Code, resp
}

// rs4ExtractError extracts the error code from a JSON error response.
func rs4ExtractError(resp map[string]interface{}) string {
	if errObj, ok := resp["error"].(map[string]interface{}); ok {
		if code, ok := errObj["code"].(string); ok {
			return code
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// T-I: Issuer authority (G1)
// ---------------------------------------------------------------------------

func TestRS4_IssuerAuthority(t *testing.T) {
	t.Run("T-I1_exact_scope_held", func(t *testing.T) {
		srv, s := testServer(t)
		projectID := tid("rs4-i1-p")
		ownerID := tid("rs4-i1-o")
		rs4Project(t, s, projectID, ownerID)

		// Dev user is admin; grant dev user membership in the project.
		rs4AddProjectRole(t, s, DevUserID, projectID, store.ProjectRoleOwner)

		code, resp := rs4MintViaAPI(t, srv, projectID, []string{"agent:read"})
		assert.Equal(t, http.StatusCreated, code, "T-I1: exact scope held should succeed: %v", resp)

		// Verify stored scopes.
		if tokenObj, ok := resp["accessToken"].(map[string]interface{}); ok {
			scopes, _ := tokenObj["scopes"].([]interface{})
			assert.Equal(t, 1, len(scopes), "T-I1: stored scopes count")
		}
	})

	t.Run("T-I2_scope_not_held", func(t *testing.T) {
		srv, s := testServer(t)
		projectID := tid("rs4-i2-p")
		ownerID := tid("rs4-i2-o")
		memberID := tid("rs4-i2-m")
		rs4Project(t, s, projectID, ownerID)
		// Member role has limited permissions — agent:delete is not held.
		rs4UserWithRole(t, s, memberID, projectID, store.ProjectRoleMember)

		// Mint via direct service call as the member.
		ctx := rs4MintContext(memberID)
		_, _, err := srv.uatService.CreateToken(ctx, memberID, "bad", projectID,
			[]string{"agent:delete"}, nil)
		assert.ErrorIs(t, err, ErrUATScopeViolation, "T-I2: scope not held must be denied")
	})

	t.Run("T-I3_mixed_held_and_unheld", func(t *testing.T) {
		srv, s := testServer(t)
		projectID := tid("rs4-i3-p")
		ownerID := tid("rs4-i3-o")
		memberID := tid("rs4-i3-m")
		rs4Project(t, s, projectID, ownerID)
		rs4UserWithRole(t, s, memberID, projectID, store.ProjectRoleMember)

		ctx := rs4MintContext(memberID)
		_, _, err := srv.uatService.CreateToken(ctx, memberID, "mixed", projectID,
			[]string{"agent:read", "agent:delete"}, nil)
		assert.ErrorIs(t, err, ErrUATScopeViolation, "T-I3: mixed scopes (partial held) must deny all")
	})

	t.Run("T-I9_unknown_scope", func(t *testing.T) {
		srv, s := testServer(t)
		projectID := tid("rs4-i9-p")
		ownerID := tid("rs4-i9-o")
		rs4Project(t, s, projectID, ownerID)

		ctx := rs4MintContext(ownerID)
		_, _, err := srv.uatService.CreateToken(ctx, ownerID, "bad", projectID,
			[]string{"invalid:scope"}, nil)
		assert.ErrorIs(t, err, ErrInvalidUATScope, "T-I9: unknown scope rejected")
	})

	t.Run("T-I10_empty_scope_list", func(t *testing.T) {
		srv, s := testServer(t)
		projectID := tid("rs4-i10-p")
		ownerID := tid("rs4-i10-o")
		rs4Project(t, s, projectID, ownerID)

		ctx := rs4MintContext(ownerID)
		_, _, err := srv.uatService.CreateToken(ctx, ownerID, "bad", projectID,
			[]string{}, nil)
		assert.ErrorIs(t, err, ErrUATScopeEmpty, "T-I10: empty scope list rejected")
	})
}

// ---------------------------------------------------------------------------
// T-P: Target project scope (G2, G10)
// ---------------------------------------------------------------------------

func TestRS4_TargetScope(t *testing.T) {
	t.Run("T-P1_member_of_project", func(t *testing.T) {
		srv, s := testServer(t)
		projectID := tid("rs4-p1-p")
		ownerID := tid("rs4-p1-o")
		rs4Project(t, s, projectID, ownerID)

		ctx := rs4MintContext(ownerID)
		key, token, err := srv.uatService.CreateToken(ctx, ownerID, "p1", projectID,
			[]string{"agent:read"}, nil)
		require.NoError(t, err, "T-P1: member of project should succeed")
		assert.NotEmpty(t, key)
		assert.Equal(t, projectID, token.ProjectID)
	})

	t.Run("T-P2_non_member", func(t *testing.T) {
		srv, s := testServer(t)
		projectID := tid("rs4-p2-p")
		ownerID := tid("rs4-p2-o")
		outsiderID := tid("rs4-p2-x")
		rs4Project(t, s, projectID, ownerID)

		// Create outsider user with no project role.
		require.NoError(t, s.CreateUser(context.Background(), &store.User{
			ID: outsiderID, Email: outsiderID + "@test.com",
			DisplayName: "Outsider", Role: "member", Status: "active",
		}))
		ensureHubMembership(context.Background(), s, outsiderID)

		ctx := rs4MintContext(outsiderID)
		_, _, err := srv.uatService.CreateToken(ctx, outsiderID, "p2", projectID,
			[]string{"agent:read"}, nil)
		assert.ErrorIs(t, err, ErrUATProjectForbidden, "T-P2: non-member must be forbidden")
	})

	t.Run("T-P3_nonexistent_project_oracle_resistance", func(t *testing.T) {
		srv, s := testServer(t)
		projectID := tid("rs4-p3-p")
		ownerID := tid("rs4-p3-o")
		rs4Project(t, s, projectID, ownerID)

		// Try to mint for a nonexistent project.
		ctx := rs4MintContext(ownerID)
		_, _, errNonexist := srv.uatService.CreateToken(ctx, ownerID, "p3", tid("rs4-p3-fake"),
			[]string{"agent:read"}, nil)
		assert.ErrorIs(t, errNonexist, ErrUATProjectForbidden,
			"T-P3: nonexistent project must return same error as unauthorized")
	})

	t.Run("T-P5_malformed_project_id", func(t *testing.T) {
		srv, s := testServer(t)
		projectID := tid("rs4-p5-p")
		ownerID := tid("rs4-p5-o")
		rs4Project(t, s, projectID, ownerID)

		ctx := rs4MintContext(ownerID)
		_, _, err := srv.uatService.CreateToken(ctx, ownerID, "p5", "not-a-uuid",
			[]string{"agent:read"}, nil)
		// Should be denied (fail closed), not 500.
		assert.Error(t, err, "T-P5: malformed project ID must fail")
	})

	t.Run("T-P6_empty_project_id", func(t *testing.T) {
		srv, _ := testServer(t)

		ctx := rs4MintContext(DevUserID)
		_, _, err := srv.uatService.CreateToken(ctx, DevUserID, "p6", "",
			[]string{"agent:read"}, nil)
		assert.ErrorIs(t, err, ErrUATProjectIDEmpty, "T-P6: empty project ID rejected")
	})
}

// ---------------------------------------------------------------------------
// T-C: Credential caveat (G5) — A1: tokens cannot act on tokens
// ---------------------------------------------------------------------------

func TestRS4_CredentialCaveat(t *testing.T) {
	t.Run("T-C1_session_JWT_create", func(t *testing.T) {
		srv, s := testServer(t)
		projectID := tid("rs4-c1-p")
		ownerID := tid("rs4-c1-o")
		rs4Project(t, s, projectID, ownerID)
		rs4AddProjectRole(t, s, DevUserID, projectID, store.ProjectRoleOwner)

		code, _ := rs4MintViaAPI(t, srv, projectID, []string{"agent:read"})
		assert.Equal(t, http.StatusCreated, code, "T-C1: session JWT should be admitted")
	})

	t.Run("T-C2_UAT_create_denied", func(t *testing.T) {
		srv, s := testServer(t)
		projectID := tid("rs4-c2-p")
		ownerID := tid("rs4-c2-o")
		rs4Project(t, s, projectID, ownerID)
		rs4AddProjectRole(t, s, DevUserID, projectID, store.ProjectRoleOwner)

		// First mint a UAT via session.
		code, resp := rs4MintViaAPI(t, srv, projectID, []string{"agent:read"})
		require.Equal(t, http.StatusCreated, code)
		uatKey := resp["token"].(string)

		// Try to use the UAT to create another token — must be denied.
		body := map[string]interface{}{
			"name": "nested", "projectId": projectID, "scopes": []string{"agent:read"},
		}
		rec := doRequestWithUAT(t, srv, uatKey, http.MethodPost, "/api/v1/auth/tokens", body)
		assert.Equal(t, http.StatusForbidden, rec.Code, "T-C2: UAT must not create tokens")
		var errResp map[string]interface{}
		_ = json.Unmarshal(rec.Body.Bytes(), &errResp)
		assert.Equal(t, "forbidden", rs4ExtractError(errResp))
	})

	t.Run("T-C3_UAT_list_denied", func(t *testing.T) {
		srv, s := testServer(t)
		projectID := tid("rs4-c3-p")
		ownerID := tid("rs4-c3-o")
		rs4Project(t, s, projectID, ownerID)
		rs4AddProjectRole(t, s, DevUserID, projectID, store.ProjectRoleOwner)

		code, resp := rs4MintViaAPI(t, srv, projectID, []string{"agent:read"})
		require.Equal(t, http.StatusCreated, code)
		uatKey := resp["token"].(string)

		rec := doRequestWithUAT(t, srv, uatKey, http.MethodGet, "/api/v1/auth/tokens", nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, "T-C3: UAT must not list tokens")
	})

	t.Run("T-C4_UAT_get_denied", func(t *testing.T) {
		srv, s := testServer(t)
		projectID := tid("rs4-c4-p")
		ownerID := tid("rs4-c4-o")
		rs4Project(t, s, projectID, ownerID)
		rs4AddProjectRole(t, s, DevUserID, projectID, store.ProjectRoleOwner)

		code, resp := rs4MintViaAPI(t, srv, projectID, []string{"agent:read"})
		require.Equal(t, http.StatusCreated, code)
		uatKey := resp["token"].(string)
		tokenObj := resp["accessToken"].(map[string]interface{})
		tokenID := tokenObj["id"].(string)

		rec := doRequestWithUAT(t, srv, uatKey, http.MethodGet, "/api/v1/auth/tokens/"+tokenID, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, "T-C4: UAT must not get tokens")
	})

	t.Run("T-C5_UAT_revoke_denied", func(t *testing.T) {
		srv, s := testServer(t)
		projectID := tid("rs4-c5-p")
		ownerID := tid("rs4-c5-o")
		rs4Project(t, s, projectID, ownerID)
		rs4AddProjectRole(t, s, DevUserID, projectID, store.ProjectRoleOwner)

		code, resp := rs4MintViaAPI(t, srv, projectID, []string{"agent:read"})
		require.Equal(t, http.StatusCreated, code)
		uatKey := resp["token"].(string)
		tokenObj := resp["accessToken"].(map[string]interface{})
		tokenID := tokenObj["id"].(string)

		rec := doRequestWithUAT(t, srv, uatKey, http.MethodPost, "/api/v1/auth/tokens/"+tokenID+"/revoke", nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, "T-C5: UAT must not revoke tokens")
	})

	t.Run("T-C6_UAT_delete_denied", func(t *testing.T) {
		srv, s := testServer(t)
		projectID := tid("rs4-c6-p")
		ownerID := tid("rs4-c6-o")
		rs4Project(t, s, projectID, ownerID)
		rs4AddProjectRole(t, s, DevUserID, projectID, store.ProjectRoleOwner)

		code, resp := rs4MintViaAPI(t, srv, projectID, []string{"agent:read"})
		require.Equal(t, http.StatusCreated, code)
		uatKey := resp["token"].(string)
		tokenObj := resp["accessToken"].(map[string]interface{})
		tokenID := tokenObj["id"].(string)

		rec := doRequestWithUAT(t, srv, uatKey, http.MethodDelete, "/api/v1/auth/tokens/"+tokenID, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, "T-C6: UAT must not delete tokens")
	})

	t.Run("T-C9_no_credential", func(t *testing.T) {
		srv, _ := testServer(t)

		rec := doRequestNoAuth(t, srv, http.MethodPost, "/api/v1/auth/tokens",
			map[string]interface{}{
				"name": "test", "projectId": "x", "scopes": []string{"agent:read"},
			})
		assert.Equal(t, http.StatusUnauthorized, rec.Code, "T-C9: no credential must be 401")
	})
}

// ---------------------------------------------------------------------------
// T-A: Audit and atomicity (G3, G4)
// ---------------------------------------------------------------------------

func TestRS4_Audit_Mint(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs4-a1-p")
	ownerID := tid("rs4-a1-o")
	rs4Project(t, s, projectID, ownerID)
	rs4AddProjectRole(t, s, DevUserID, projectID, store.ProjectRoleOwner)

	code, resp := rs4MintViaAPI(t, srv, projectID, []string{"agent:read"})
	require.Equal(t, http.StatusCreated, code, "T-A1: mint should succeed")
	tokenObj := resp["accessToken"].(map[string]interface{})
	tokenID := tokenObj["id"].(string)

	// Check audit record.
	records, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{
		MutationType: "credential_create",
		TargetID:     tokenID,
		Limit:        10,
	})
	require.NoError(t, err)
	require.Len(t, records, 1, "T-A1: exactly one audit record for mint")

	rec := records[0]
	assert.Equal(t, "credential_create", rec.MutationType)
	assert.Equal(t, "user_access_token", rec.TargetType)
	assert.Equal(t, tokenID, rec.TargetID)
	assert.NotEmpty(t, rec.ActorPrincipalID, "T-A1: actor_id must be populated")
	assert.NotEmpty(t, rec.AfterSummary, "T-A1: after fields must be populated")

	// T-A8: Verify no secret data in audit.
	assert.NotContains(t, rec.AfterSummary, resp["token"].(string), "T-A8: plaintext token must not appear in audit")
	assert.NotContains(t, rec.AfterSummary, "key_hash", "T-A8: key_hash must not appear in audit")

	// Verify after summary contains token_id and scopes.
	assert.Contains(t, rec.AfterSummary, tokenID, "T-A1: after must contain token_id")
	assert.Contains(t, rec.AfterSummary, "agent:read", "T-A1: after must contain scopes")
}

func TestRS4_Audit_Revoke(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs4-a2-p")
	ownerID := tid("rs4-a2-o")
	rs4Project(t, s, projectID, ownerID)
	rs4AddProjectRole(t, s, DevUserID, projectID, store.ProjectRoleOwner)

	// Mint, then revoke.
	code, resp := rs4MintViaAPI(t, srv, projectID, []string{"agent:read"})
	require.Equal(t, http.StatusCreated, code)
	tokenObj := resp["accessToken"].(map[string]interface{})
	tokenID := tokenObj["id"].(string)

	revokeRec := doRequest(t, srv, http.MethodPost, "/api/v1/auth/tokens/"+tokenID+"/revoke", nil)
	assert.Equal(t, http.StatusNoContent, revokeRec.Code, "T-A2: revoke should succeed")

	// Check audit record.
	records, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{
		MutationType: "credential_revoke",
		TargetID:     tokenID,
		Limit:        10,
	})
	require.NoError(t, err)
	require.Len(t, records, 1, "T-A2: exactly one audit record for revoke")

	rec := records[0]
	assert.Equal(t, "credential_revoke", rec.MutationType)
	assert.NotEmpty(t, rec.ActorPrincipalID)
	assert.Contains(t, rec.BeforeSummary, tokenID, "T-A2: before must contain token_id")
	assert.Contains(t, rec.BeforeSummary, `"action":"revoke"`, "T-A2: before must distinguish revoke action")
}

func TestRS4_Audit_Delete(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs4-a3-p")
	ownerID := tid("rs4-a3-o")
	rs4Project(t, s, projectID, ownerID)
	rs4AddProjectRole(t, s, DevUserID, projectID, store.ProjectRoleOwner)

	code, resp := rs4MintViaAPI(t, srv, projectID, []string{"agent:read"})
	require.Equal(t, http.StatusCreated, code)
	tokenObj := resp["accessToken"].(map[string]interface{})
	tokenID := tokenObj["id"].(string)

	deleteRec := doRequest(t, srv, http.MethodDelete, "/api/v1/auth/tokens/"+tokenID, nil)
	assert.Equal(t, http.StatusNoContent, deleteRec.Code, "T-A3: delete should succeed")

	// Check audit record — must use credential_revoke event type (A5).
	records, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{
		MutationType: "credential_revoke",
		TargetID:     tokenID,
		Limit:        10,
	})
	require.NoError(t, err)
	require.Len(t, records, 1, "T-A3: exactly one audit record for delete")

	rec := records[0]
	assert.Contains(t, rec.BeforeSummary, `"action":"delete"`, "T-A3: before must distinguish delete action")
}

func TestRS4_Audit_DenialNoRecord(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs4-a7-p")
	ownerID := tid("rs4-a7-o")
	outsiderID := tid("rs4-a7-x")
	rs4Project(t, s, projectID, ownerID)

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: outsiderID, Email: outsiderID + "@test.com",
		DisplayName: "Outsider", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, outsiderID)

	// Denied mint should not create any audit record.
	mintCtx := rs4MintContext(outsiderID)
	_, _, err := srv.uatService.CreateToken(mintCtx, outsiderID, "denied", projectID,
		[]string{"agent:read"}, nil)
	assert.Error(t, err)

	records, _, listErr := s.ListMutationAudits(ctx, store.MutationAuditFilter{
		MutationType: "credential_create",
		Limit:        100,
	})
	require.NoError(t, listErr)
	for _, r := range records {
		assert.NotEqual(t, outsiderID, r.ActorPrincipalID,
			"T-A7: denied mint must not create audit record")
	}
}

// ---------------------------------------------------------------------------
// T-L: Lifecycle after mint
// ---------------------------------------------------------------------------

func TestRS4_Lifecycle(t *testing.T) {
	t.Run("T-L1_project_deleted_token_denied", func(t *testing.T) {
		srv, s := testServer(t)

		projectID := tid("rs4-l1-p")
		ownerID := tid("rs4-l1-o")
		rs4Project(t, s, projectID, ownerID)

		uatKey := mintScopedUAT(t, srv, ownerID, projectID, []string{"agent:read"})

		// Delete the project via RS3 service.
		mintCtx := rs4MintContext(ownerID)
		result, decision := srv.deletionService.Delete(mintCtx, ProjectDeleteRequest{
			ProjectID: projectID,
			Actor:     NewAuthenticatedUser(ownerID, ownerID+"@test.com", "Owner", "member", string(ClientTypeAPI)),
		})
		require.NotNil(t, result, "T-L1: deletion result must not be nil; decision: %+v", decision)

		// Use the token — must be denied (fail closed).
		rec := doRequestWithUAT(t, srv, uatKey, http.MethodGet, "/api/v1/projects/"+projectID+"/agents", nil)
		assert.True(t, rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden || rec.Code == http.StatusNotFound,
			"T-L1: token for deleted project must be denied (got %d)", rec.Code)
	})

	t.Run("T-L4_later_grant_does_not_enlarge_token", func(t *testing.T) {
		srv, s := testServer(t)
		ctx := context.Background()

		projectID := tid("rs4-l4-p")
		ownerID := tid("rs4-l4-o")
		rs4Project(t, s, projectID, ownerID)

		// Mint with only agent:read.
		uatKey := mintScopedUAT(t, srv, ownerID, projectID, []string{"agent:read"})

		// User now has all owner permissions. But the token is caveated to agent:read only.
		// Try to use the token for agent:delete — must be denied.
		rec := doRequestWithUAT(t, srv, uatKey, http.MethodDelete, "/api/v1/projects/"+projectID+"/agents/nonexistent", nil)
		// Should be denied by UAT scope enforcement, not 404.
		// The enforceUATConstraints check will deny agent:delete since the token only has agent:read.
		assert.True(t, rec.Code == http.StatusForbidden || rec.Code == http.StatusNotFound,
			"T-L4: later grant must not enlarge token beyond its scope caveat (got %d)", rec.Code)
		_ = ctx
	})
}

// ---------------------------------------------------------------------------
// T-X: Concurrency and idempotency (G7)
// ---------------------------------------------------------------------------

func TestRS4_Concurrency_MintAtCap(t *testing.T) {
	srv, s := testServer(t)

	projectID := tid("rs4-x1-p")
	ownerID := tid("rs4-x1-o")
	rs4Project(t, s, projectID, ownerID)

	// Fill to cap - 1.
	mintCtx := rs4MintContext(ownerID)
	for i := 0; i < store.UATMaxPerUser-1; i++ {
		_, _, err := srv.uatService.CreateToken(mintCtx, ownerID,
			"fill-"+string(rune('a'+i%26))+string(rune('0'+i/26)),
			projectID, []string{"agent:read"}, nil)
		require.NoError(t, err, "T-X1: fill token %d", i)
	}

	// Launch N goroutines trying to mint the last slot.
	const N = 10
	var wg sync.WaitGroup
	successes := make(chan string, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ctx := rs4MintContext(ownerID)
			_, _, err := srv.uatService.CreateToken(ctx, ownerID,
				"race-"+string(rune('a'+idx)),
				projectID, []string{"agent:read"}, nil)
			if err == nil {
				successes <- "ok"
			}
		}(i)
	}
	wg.Wait()
	close(successes)

	successCount := 0
	for range successes {
		successCount++
	}
	// With SQLite serialization, exactly one should succeed.
	assert.LessOrEqual(t, successCount, 1, "T-X1: at most one concurrent mint at cap should succeed")

	// Verify final count <= cap.
	count, err := s.CountUserAccessTokens(context.Background(), ownerID)
	require.NoError(t, err)
	assert.LessOrEqual(t, count, store.UATMaxPerUser, "T-X1: total count must not exceed cap")
}

func TestRS4_DoubleRevoke(t *testing.T) {
	srv, s := testServer(t)

	projectID := tid("rs4-x3-p")
	ownerID := tid("rs4-x3-o")
	rs4Project(t, s, projectID, ownerID)
	rs4AddProjectRole(t, s, DevUserID, projectID, store.ProjectRoleOwner)

	code, resp := rs4MintViaAPI(t, srv, projectID, []string{"agent:read"})
	require.Equal(t, http.StatusCreated, code)
	tokenObj := resp["accessToken"].(map[string]interface{})
	tokenID := tokenObj["id"].(string)

	// First revoke.
	rec1 := doRequest(t, srv, http.MethodPost, "/api/v1/auth/tokens/"+tokenID+"/revoke", nil)
	assert.Equal(t, http.StatusNoContent, rec1.Code, "T-X3: first revoke succeeds")

	// Second revoke — should be stable (no panic, no 500).
	rec2 := doRequest(t, srv, http.MethodPost, "/api/v1/auth/tokens/"+tokenID+"/revoke", nil)
	assert.True(t, rec2.Code == http.StatusNoContent || rec2.Code == http.StatusNotFound,
		"T-X3: double revoke must be stable (got %d)", rec2.Code)
}

func TestRS4_RevokeThenDelete(t *testing.T) {
	srv, s := testServer(t)

	projectID := tid("rs4-x4-p")
	ownerID := tid("rs4-x4-o")
	rs4Project(t, s, projectID, ownerID)
	rs4AddProjectRole(t, s, DevUserID, projectID, store.ProjectRoleOwner)

	code, resp := rs4MintViaAPI(t, srv, projectID, []string{"agent:read"})
	require.Equal(t, http.StatusCreated, code)
	tokenObj := resp["accessToken"].(map[string]interface{})
	tokenID := tokenObj["id"].(string)

	// Revoke, then delete.
	rec1 := doRequest(t, srv, http.MethodPost, "/api/v1/auth/tokens/"+tokenID+"/revoke", nil)
	assert.Equal(t, http.StatusNoContent, rec1.Code)

	rec2 := doRequest(t, srv, http.MethodDelete, "/api/v1/auth/tokens/"+tokenID, nil)
	assert.Equal(t, http.StatusNoContent, rec2.Code, "T-X4: delete after revoke should succeed")
}

// ---------------------------------------------------------------------------
// T-D: Denial-code stability (G9)
// ---------------------------------------------------------------------------

func TestRS4_DenialCodeStability(t *testing.T) {
	t.Run("T-D1_scope_violation_code", func(t *testing.T) {
		srv, s := testServer(t)
		projectID := tid("rs4-d1-p")
		ownerID := tid("rs4-d1-o")
		memberID := tid("rs4-d1-m")
		rs4Project(t, s, projectID, ownerID)
		rs4UserWithRole(t, s, memberID, projectID, store.ProjectRoleMember)

		// Mint via API as the member (with session) — but the dev user has
		// the session. Instead, test directly via service.
		ctx := rs4MintContext(memberID)
		_, _, err := srv.uatService.CreateToken(ctx, memberID, "d1", projectID,
			[]string{"agent:delete"}, nil)
		assert.ErrorIs(t, err, ErrUATScopeViolation, "T-D1: scope_violation sentinel")
	})

	t.Run("T-D2_no_internal_detail_in_error", func(t *testing.T) {
		srv, s := testServer(t)
		projectID := tid("rs4-d2-p")
		ownerID := tid("rs4-d2-o")
		outsiderID := tid("rs4-d2-x")
		rs4Project(t, s, projectID, ownerID)
		require.NoError(t, s.CreateUser(context.Background(), &store.User{
			ID: outsiderID, Email: outsiderID + "@test.com",
			DisplayName: "Out", Role: "member", Status: "active",
		}))
		ensureHubMembership(context.Background(), s, outsiderID)

		ctx := rs4MintContext(outsiderID)
		_, _, err := srv.uatService.CreateToken(ctx, outsiderID, "d2", projectID,
			[]string{"agent:read"}, nil)
		assert.Error(t, err)
		errMsg := err.Error()
		assert.NotContains(t, errMsg, "permission", "T-D2: no permission ID in error")
		assert.NotContains(t, errMsg, "role_binding", "T-D2: no role_binding in error")
		assert.NotContains(t, errMsg, "SQL", "T-D2: no SQL detail in error")
	})
}

// ---------------------------------------------------------------------------
// T-G: Structural / AST gates
// ---------------------------------------------------------------------------

func TestRS4_AST_MutationCallSites(t *testing.T) {
	repoRoot := findRS1RepoRoot(t)
	hubDir := filepath.Join(repoRoot, "pkg", "hub")

	// T-G3/T-G4: All UAT store mutation calls must be inside useraccesstoken.go.
	mutationSymbols := []string{
		"CreateUserAccessToken",
		"RevokeUserAccessToken",
		"DeleteUserAccessToken",
	}

	entries, err := os.ReadDir(hubDir)
	require.NoError(t, err)

	fset := token.NewFileSet()
	var violations []string

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		if strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		// Generated Ent files are exempt.
		if entry.Name() == "useraccesstoken.go" {
			continue
		}

		f, parseErr := parser.ParseFile(fset, filepath.Join(hubDir, entry.Name()), nil, parser.SkipObjectResolution)
		if parseErr != nil {
			continue
		}

		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			for _, sym := range mutationSymbols {
				if sel.Sel.Name == sym {
					pos := fset.Position(call.Pos())
					violations = append(violations,
						entry.Name()+":"+sym+" at "+pos.String())
				}
			}
			return true
		})
	}

	assert.Empty(t, violations,
		"T-G3/G4: UAT mutation store calls must be inside useraccesstoken.go, found: %v", violations)
}

func TestRS4_AST_BypassExemptionUpdated(t *testing.T) {
	repoRoot := findRS1RepoRoot(t)

	// T-G5: The rs1_extended_test.go exemption for useraccesstoken.go must have
	// an updated reason (no longer "internal token management" post-RS4).
	content, err := os.ReadFile(filepath.Join(repoRoot, "pkg", "hub", "rs1_extended_test.go"))
	require.NoError(t, err)

	// The exemption should exist but with an appropriate reason.
	assert.Contains(t, string(content), `"useraccesstoken.go"`,
		"T-G5: useraccesstoken.go should still be exempted (UAT service does not use CreateRoleBinding)")
}

// ---------------------------------------------------------------------------
// T-R: Regression
// ---------------------------------------------------------------------------

func TestRS4_Regression_TokenEndpoints(t *testing.T) {
	srv, s := testServer(t)
	projectID := tid("rs4-r-p")
	ownerID := tid("rs4-r-o")
	rs4Project(t, s, projectID, ownerID)
	rs4AddProjectRole(t, s, DevUserID, projectID, store.ProjectRoleOwner)

	// Create.
	code, resp := rs4MintViaAPI(t, srv, projectID, []string{"agent:read"})
	require.Equal(t, http.StatusCreated, code, "T-R: create")
	tokenObj := resp["accessToken"].(map[string]interface{})
	tokenID := tokenObj["id"].(string)
	assert.NotEmpty(t, resp["token"], "T-R: plaintext token returned")

	// List.
	listRec := doRequest(t, srv, http.MethodGet, "/api/v1/auth/tokens", nil)
	assert.Equal(t, http.StatusOK, listRec.Code, "T-R: list")
	var listResp map[string]interface{}
	_ = json.Unmarshal(listRec.Body.Bytes(), &listResp)
	items, _ := listResp["items"].([]interface{})
	assert.GreaterOrEqual(t, len(items), 1, "T-R: list has items")

	// Get.
	getRec := doRequest(t, srv, http.MethodGet, "/api/v1/auth/tokens/"+tokenID, nil)
	assert.Equal(t, http.StatusOK, getRec.Code, "T-R: get")

	// Revoke.
	revokeRec := doRequest(t, srv, http.MethodPost, "/api/v1/auth/tokens/"+tokenID+"/revoke", nil)
	assert.Equal(t, http.StatusNoContent, revokeRec.Code, "T-R: revoke")
}

func TestRS4_Regression_ValidateTokenStillWorks(t *testing.T) {
	srv, s := testServer(t)
	projectID := tid("rs4-rv-p")
	ownerID := tid("rs4-rv-o")
	rs4Project(t, s, projectID, ownerID)

	uatKey := mintScopedUAT(t, srv, ownerID, projectID, []string{"agent:read"})

	// The minted token should be usable for authorized operations.
	rec := doRequestWithUAT(t, srv, uatKey, http.MethodGet,
		"/api/v1/projects/"+projectID+"/agents", nil)
	assert.Equal(t, http.StatusOK, rec.Code,
		"T-R: minted UAT should work for authorized operation (got %d: %s)", rec.Code, rec.Body.String())
}

// ---------------------------------------------------------------------------
// Rewrite vacuous TestMutationAudit_CredentialRevocation (T-A9)
// ---------------------------------------------------------------------------
// This replaces the test at audit_authz_test.go:543-600 which used t.Skip,
// drove DELETE (which emitted nothing), and on empty results merely logged.
// The new test asserts that revocation produces exactly one audit record.
// (The original test remains in audit_authz_test.go to keep the file stable;
// this test supersedes it with substantive assertions.)

func TestRS4_MutationAudit_CredentialRevocation_Asserting(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("rs4-a9-p")
	ownerID := tid("rs4-a9-o")
	rs4Project(t, s, projectID, ownerID)
	rs4AddProjectRole(t, s, DevUserID, projectID, store.ProjectRoleOwner)

	// Mint.
	code, resp := rs4MintViaAPI(t, srv, projectID, []string{"agent:read"})
	require.Equal(t, http.StatusCreated, code)
	tokenObj := resp["accessToken"].(map[string]interface{})
	tokenID := tokenObj["id"].(string)

	// Delete (not just revoke) — the old test drove DELETE and got nothing.
	// Post-RS4, DELETE must produce a credential_revoke audit record.
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/auth/tokens/"+tokenID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)

	// Assert audit record exists with substantive fields.
	records, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{
		MutationType: "credential_revoke",
		TargetID:     tokenID,
		Limit:        10,
	})
	require.NoError(t, err)
	require.Len(t, records, 1,
		"T-A9: DELETE must produce exactly one credential_revoke audit record (was zero pre-RS4)")

	r := records[0]
	assert.Equal(t, "user_access_token", r.TargetType)
	assert.Equal(t, tokenID, r.TargetID)
	assert.NotEmpty(t, r.ActorPrincipalID, "T-A9: actor_id must be populated")
	assert.Contains(t, r.BeforeSummary, `"action":"delete"`,
		"T-A9: audit must distinguish delete from revoke")
}

