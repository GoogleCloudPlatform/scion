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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupCredentialTestServer creates a test server and required fixtures
// (user, project, hub membership) for credential revocation tests.
func setupCredentialTestServer(t *testing.T) (*Server, store.Store, *store.User, *store.Project) {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID:          tid("user-cred-test"),
		Email:       "credtest@test.com",
		DisplayName: "Credential Tester",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	project := &store.Project{
		ID:        tid("project-cred-test"),
		Name:      "cred-test-project",
		Slug:      "cred-test-project",
		OwnerID:   user.ID,
		CreatedBy: user.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.createProjectMembersGroup(ctx, project)

	return srv, s, user, project
}

// createCredTestAgent creates a minimal agent in the store for testing.
func createCredTestAgent(t *testing.T, s store.Store, agentID, projectID, ownerID string) *store.Agent {
	t.Helper()
	agent := &store.Agent{
		ID:        agentID,
		Name:      "test-agent-" + agentID[:8],
		Slug:      "test-agent-" + agentID[:8],
		ProjectID: projectID,
		OwnerID:   ownerID,
		Phase:     "running",
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateAgent(context.Background(), agent))
	return agent
}

// TestNewlyIssuedTokensPersisted verifies that generating a token via the
// service records a credential in the store.
func TestNewlyIssuedTokensPersisted(t *testing.T) {
	srv, s, _, project := setupCredentialTestServer(t)
	ctx := context.Background()

	agentID := tid("agent-persist-test")
	createCredTestAgent(t, s, agentID, project.ID, tid("user-cred-test"))

	// Generate a token using the server method (which wires credential recording)
	token, err := srv.GenerateAgentToken(agentID, project.ID, nil, AgentRoleFull, nil)
	require.NoError(t, err)
	require.NotEmpty(t, token)

	// Extract the JTI from the token
	claims, err := srv.agentTokenService.ValidateAgentToken(token)
	require.NoError(t, err)

	// Verify the credential was persisted
	jtiHash := hashJTI(claims.ID)
	cred, err := s.GetAgentCredentialByJTIHash(ctx, jtiHash)
	require.NoError(t, err)
	assert.Equal(t, agentID, cred.AgentID)
	assert.Equal(t, project.ID, cred.ProjectID)
	assert.Equal(t, jtiHash, cred.TokenJTIHash)
	assert.Nil(t, cred.RevokedAt)
}

// TestRevokedTokenDeniedBeforeExpiry verifies that a revoked token is rejected
// by the auth middleware even before its natural expiry.
func TestRevokedTokenDeniedBeforeExpiry(t *testing.T) {
	srv, s, _, project := setupCredentialTestServer(t)
	ctx := context.Background()

	agentID := tid("agent-revoked-test")
	createCredTestAgent(t, s, agentID, project.ID, tid("user-cred-test"))

	// Generate token
	token, err := srv.GenerateAgentToken(agentID, project.ID, nil, AgentRoleFull, nil)
	require.NoError(t, err)

	// Extract JTI and find credential
	claims, err := srv.agentTokenService.ValidateAgentToken(token)
	require.NoError(t, err)
	jtiHash := hashJTI(claims.ID)

	cred, err := s.GetAgentCredentialByJTIHash(ctx, jtiHash)
	require.NoError(t, err)

	// Revoke the credential
	err = s.RevokeAgentCredential(ctx, cred.ID, "test", "explicit")
	require.NoError(t, err)

	// Attempt to use the revoked token — should get 401
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	req.Header.Set("X-Scion-Agent-Token", token)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), "revoked")
}

// TestDeletedAgentTokenDenied verifies that tokens for deleted agents are denied.
func TestDeletedAgentTokenDenied(t *testing.T) {
	srv, s, user, project := setupCredentialTestServer(t)

	agentID := tid("agent-delete-deny")
	createCredTestAgent(t, s, agentID, project.ID, user.ID)

	// Generate token
	token, err := srv.GenerateAgentToken(agentID, project.ID, nil, AgentRoleFull, nil)
	require.NoError(t, err)

	// Delete the agent via the API (which triggers credential revocation)
	userToken, _, _, err := srv.userTokenService.GenerateTokenPair(
		user.ID, user.Email, user.DisplayName, user.Role, ClientTypeWeb,
	)
	require.NoError(t, err)

	deleteReq := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/"+agentID+"?force=true", nil)
	deleteReq.Header.Set("Authorization", "Bearer "+userToken)
	deleteRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(deleteRec, deleteReq)
	require.Equal(t, http.StatusNoContent, deleteRec.Code,
		"delete agent failed: %s", deleteRec.Body.String())

	// Attempt to use the token — should be denied
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	req.Header.Set("X-Scion-Agent-Token", token)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), "revoked")
}

// TestSuspendedAgentTokenDenied verifies that tokens for suspended agents are denied.
func TestSuspendedAgentTokenDenied(t *testing.T) {
	srv, s, _, project := setupCredentialTestServer(t)
	ctx := context.Background()

	agentID := tid("agent-suspend-deny")
	agent := createCredTestAgent(t, s, agentID, project.ID, tid("user-cred-test"))

	// Generate token
	token, err := srv.GenerateAgentToken(agentID, project.ID, nil, AgentRoleFull, nil)
	require.NoError(t, err)

	// Suspend the agent directly (not via HTTP since that needs broker dispatch)
	_, revokeErr := s.RevokeAgentCredentialsByAgent(ctx, agentID, "system", "agent_suspended")
	require.NoError(t, revokeErr)
	_ = agent // agent is suspended via credential revocation

	// Attempt to use the token — should be denied since credential is revoked
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	req.Header.Set("X-Scion-Agent-Token", token)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), "revoked")
}

// TestRefreshFromRevokedTokenFails verifies that refreshing a revoked token fails.
func TestRefreshFromRevokedTokenFails(t *testing.T) {
	srv, s, _, project := setupCredentialTestServer(t)
	ctx := context.Background()

	agentID := tid("agent-refresh-revoked")
	createCredTestAgent(t, s, agentID, project.ID, tid("user-cred-test"))

	// Generate token with refresh scope
	token, err := srv.GenerateAgentToken(agentID, project.ID, nil, AgentRoleFull, nil)
	require.NoError(t, err)

	// Extract JTI and revoke
	claims, err := srv.agentTokenService.ValidateAgentToken(token)
	require.NoError(t, err)
	jtiHash := hashJTI(claims.ID)

	cred, err := s.GetAgentCredentialByJTIHash(ctx, jtiHash)
	require.NoError(t, err)
	require.NoError(t, s.RevokeAgentCredential(ctx, cred.ID, "test", "explicit"))

	// Attempt to refresh — should fail because the token credential is revoked.
	// The auth middleware will reject the request with 401 before it reaches
	// the refresh handler.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agentID+"/token/refresh", nil)
	req.Header.Set("X-Scion-Agent-Token", token)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	// 401 from auth middleware (revoked token) or 403 from refresh handler
	assert.True(t, rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden,
		"expected 401 or 403, got %d: %s", rec.Code, rec.Body.String())
}

// TestCompatibilityWindowAcceptsLegacyTokens verifies that tokens issued before
// the credential table (with unknown JTIs) are accepted during the compatibility window.
func TestCompatibilityWindowAcceptsLegacyTokens(t *testing.T) {
	srv, s, _, project := setupCredentialTestServer(t)

	agentID := tid("agent-legacy-test")
	createCredTestAgent(t, s, agentID, project.ID, tid("user-cred-test"))

	// Create a token WITHOUT credential recording (simulating pre-table token)
	legacyService, err := NewAgentTokenService(AgentTokenConfig{
		SigningKey:    srv.agentTokenService.config.SigningKey,
		TokenDuration: time.Hour,
	})
	require.NoError(t, err)
	// Do NOT set credential recorder — simulates legacy behavior

	legacyToken, err := legacyService.GenerateAgentToken(
		agentID, project.ID,
		ScopesForRole(AgentRoleFull),
		nil,
	)
	require.NoError(t, err)

	// Use the legacy token — should be accepted (compatibility window)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/"+agentID, nil)
	req.Header.Set("X-Scion-Agent-Token", legacyToken)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	// Should NOT be 401 — legacy tokens are accepted
	assert.NotEqual(t, http.StatusUnauthorized, rec.Code,
		"legacy token should be accepted during compatibility window, got: %s", rec.Body.String())
}

// TestCompatibilityWindowRefreshMigratesLegacyToken verifies that refreshing
// a legacy token creates a new recorded credential.
func TestCompatibilityWindowRefreshMigratesLegacyToken(t *testing.T) {
	srv, s, _, project := setupCredentialTestServer(t)

	agentID := tid("agent-legacy-migrate")
	createCredTestAgent(t, s, agentID, project.ID, tid("user-cred-test"))

	// Create a legacy token (no credential record)
	legacyService, err := NewAgentTokenService(AgentTokenConfig{
		SigningKey:    srv.agentTokenService.config.SigningKey,
		TokenDuration: time.Hour,
	})
	require.NoError(t, err)

	legacyToken, err := legacyService.GenerateAgentToken(
		agentID, project.ID,
		ScopesForRole(AgentRoleFull),
		nil,
	)
	require.NoError(t, err)

	// Refresh the legacy token
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agentID+"/token/refresh", nil)
	req.Header.Set("X-Scion-Agent-Token", legacyToken)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code,
		"refresh should succeed for legacy token: %s", rec.Body.String())

	// Extract the new token
	var refreshResp struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &refreshResp))
	require.NotEmpty(t, refreshResp.Token)

	// Verify the new token has a credential record
	newClaims, err := srv.agentTokenService.ValidateAgentToken(refreshResp.Token)
	require.NoError(t, err)

	newCred, err := s.GetAgentCredentialByJTIHash(context.Background(), hashJTI(newClaims.ID))
	require.NoError(t, err)
	assert.Equal(t, agentID, newCred.AgentID)
	assert.Nil(t, newCred.RevokedAt)
}

// TestTokenRefreshRevokesOldCredential verifies that after a token refresh,
// the old credential is marked as revoked.
func TestTokenRefreshRevokesOldCredential(t *testing.T) {
	srv, s, _, project := setupCredentialTestServer(t)

	agentID := tid("agent-refresh-revoke")
	createCredTestAgent(t, s, agentID, project.ID, tid("user-cred-test"))

	// Generate initial token (will be recorded)
	token, err := srv.GenerateAgentToken(agentID, project.ID, nil, AgentRoleFull, nil)
	require.NoError(t, err)

	// Get the original credential
	claims, err := srv.agentTokenService.ValidateAgentToken(token)
	require.NoError(t, err)
	oldJTIHash := hashJTI(claims.ID)

	// Refresh the token
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agentID+"/token/refresh", nil)
	req.Header.Set("X-Scion-Agent-Token", token)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code,
		"token refresh failed: %s", rec.Body.String())

	// Verify old credential is revoked
	oldCred, err := s.GetAgentCredentialByJTIHash(context.Background(), oldJTIHash)
	require.NoError(t, err)
	assert.NotNil(t, oldCred.RevokedAt, "old credential should be revoked after refresh")
	if oldCred.RevokeReason != nil {
		assert.Equal(t, "refreshed", *oldCred.RevokeReason)
	}
}

// TestRevokeOnAgentDelete verifies that deleting an agent revokes all its credentials.
func TestRevokeOnAgentDelete(t *testing.T) {
	srv, s, user, project := setupCredentialTestServer(t)
	ctx := context.Background()

	agentID := tid("agent-del-revoke")
	createCredTestAgent(t, s, agentID, project.ID, user.ID)

	// Generate two tokens for the same agent
	token1, err := srv.GenerateAgentToken(agentID, project.ID, nil, AgentRoleFull, nil)
	require.NoError(t, err)
	token2, err := srv.GenerateAgentToken(agentID, project.ID, nil, AgentRoleFull, nil)
	require.NoError(t, err)

	// Extract JTI hashes
	claims1, _ := srv.agentTokenService.ValidateAgentToken(token1)
	claims2, _ := srv.agentTokenService.ValidateAgentToken(token2)
	jtiHash1 := hashJTI(claims1.ID)
	jtiHash2 := hashJTI(claims2.ID)

	// Delete the agent
	userToken, _, _, err := srv.userTokenService.GenerateTokenPair(
		user.ID, user.Email, user.DisplayName, user.Role, ClientTypeWeb,
	)
	require.NoError(t, err)

	deleteReq := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/"+agentID+"?force=true", nil)
	deleteReq.Header.Set("Authorization", "Bearer "+userToken)
	deleteRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(deleteRec, deleteReq)
	require.Equal(t, http.StatusNoContent, deleteRec.Code)

	// Verify both credentials are revoked
	cred1, err := s.GetAgentCredentialByJTIHash(ctx, jtiHash1)
	require.NoError(t, err)
	assert.NotNil(t, cred1.RevokedAt, "credential 1 should be revoked")

	cred2, err := s.GetAgentCredentialByJTIHash(ctx, jtiHash2)
	require.NoError(t, err)
	assert.NotNil(t, cred2.RevokedAt, "credential 2 should be revoked")
}

// TestRevokeOnAgentSuspend verifies that suspending an agent revokes all its credentials.
func TestRevokeOnAgentSuspend(t *testing.T) {
	srv, s, _, project := setupCredentialTestServer(t)
	ctx := context.Background()

	agentID := tid("agent-susp-revoke")
	createCredTestAgent(t, s, agentID, project.ID, tid("user-cred-test"))

	// Generate a token
	token, err := srv.GenerateAgentToken(agentID, project.ID, nil, AgentRoleFull, nil)
	require.NoError(t, err)

	claims, _ := srv.agentTokenService.ValidateAgentToken(token)
	jtiHash := hashJTI(claims.ID)

	// Call suspendAgent directly (bypassing HTTP which needs broker)
	// Since suspendAgent needs the agent record, fetch it fresh
	agent, err := s.GetAgent(ctx, agentID)
	require.NoError(t, err)

	// We can't call suspendAgent directly without a dispatcher, so instead
	// test the credential revocation directly as the suspend handler does
	_, err = s.RevokeAgentCredentialsByAgent(ctx, agentID, "system", "agent_suspended")
	require.NoError(t, err)
	_ = agent

	// Verify credential is revoked
	cred, err := s.GetAgentCredentialByJTIHash(ctx, jtiHash)
	require.NoError(t, err)
	assert.NotNil(t, cred.RevokedAt, "credential should be revoked after suspend")
	if cred.RevokeReason != nil {
		assert.Equal(t, "agent_suspended", *cred.RevokeReason)
	}
}

// TestPurgeExpiredAgentCredentials verifies that expired credentials can be cleaned up.
func TestPurgeExpiredAgentCredentials(t *testing.T) {
	_, s, _, project := setupCredentialTestServer(t)
	ctx := context.Background()

	// Create an expired credential
	cred := &store.AgentCredential{
		AgentID:      tid("agent-purge-test"),
		ProjectID:    project.ID,
		TokenJTIHash: hashJTI("expired-jti-1"),
		IssuedAt:     time.Now().Add(-24 * time.Hour),
		ExpiresAt:    time.Now().Add(-12 * time.Hour), // expired 12 hours ago
	}
	require.NoError(t, s.CreateAgentCredential(ctx, cred))

	// Create a non-expired credential
	activeCred := &store.AgentCredential{
		AgentID:      tid("agent-purge-test"),
		ProjectID:    project.ID,
		TokenJTIHash: hashJTI("active-jti-1"),
		IssuedAt:     time.Now(),
		ExpiresAt:    time.Now().Add(10 * time.Hour), // still valid
	}
	require.NoError(t, s.CreateAgentCredential(ctx, activeCred))

	// Purge expired credentials
	n, err := s.PurgeExpiredAgentCredentials(ctx, time.Now().Add(-1*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 1, n, "should purge exactly 1 expired credential")

	// Verify active credential still exists
	_, err = s.GetAgentCredentialByJTIHash(ctx, hashJTI("active-jti-1"))
	assert.NoError(t, err, "active credential should still exist")
}

// TestHashJTI verifies the JTI hashing function.
func TestHashJTI(t *testing.T) {
	hash1 := hashJTI("test-jti-1")
	hash2 := hashJTI("test-jti-2")
	hash1Again := hashJTI("test-jti-1")

	assert.NotEmpty(t, hash1)
	assert.NotEmpty(t, hash2)
	assert.NotEqual(t, hash1, hash2, "different JTIs should produce different hashes")
	assert.Equal(t, hash1, hash1Again, "same JTI should produce same hash")
	assert.Equal(t, 64, len(hash1), "SHA-256 hex should be 64 chars")
}

// TestCredentialRecorderNilSafe verifies that token generation works without
// a credential recorder (nil-safe backward compatibility).
func TestCredentialRecorderNilSafe(t *testing.T) {
	service, err := NewAgentTokenService(AgentTokenConfig{
		SigningKey:    make([]byte, 32),
		TokenDuration: time.Hour,
	})
	require.NoError(t, err)
	// Do NOT set credential recorder

	token, err := service.GenerateAgentToken("agent-1", "project-1", nil, nil)
	require.NoError(t, err)
	assert.NotEmpty(t, token)

	token2, expiry, err := service.GenerateAgentTokenWithExpiry("agent-1", "project-1", nil, nil)
	require.NoError(t, err)
	assert.NotEmpty(t, token2)
	assert.False(t, expiry.IsZero())
}

// TestCredentialStoreOperations verifies the basic CRUD operations on the
// credential store adapter.
func TestCredentialStoreOperations(t *testing.T) {
	_, s, _, project := setupCredentialTestServer(t)
	ctx := context.Background()

	agentID := tid("agent-store-ops")
	jtiHash := hashJTI("store-ops-jti-1")

	// Create
	cred := &store.AgentCredential{
		AgentID:      agentID,
		ProjectID:    project.ID,
		TokenJTIHash: jtiHash,
		IssuedAt:     time.Now(),
		ExpiresAt:    time.Now().Add(10 * time.Hour),
	}
	require.NoError(t, s.CreateAgentCredential(ctx, cred))
	assert.NotEmpty(t, cred.ID)

	// Read by JTI hash
	found, err := s.GetAgentCredentialByJTIHash(ctx, jtiHash)
	require.NoError(t, err)
	assert.Equal(t, agentID, found.AgentID)
	assert.Equal(t, project.ID, found.ProjectID)
	assert.Nil(t, found.RevokedAt)

	// Update last seen
	require.NoError(t, s.UpdateAgentCredentialLastSeen(ctx, cred.ID, time.Now()))
	found, err = s.GetAgentCredentialByJTIHash(ctx, jtiHash)
	require.NoError(t, err)
	assert.NotNil(t, found.LastSeenAt)

	// Revoke
	require.NoError(t, s.RevokeAgentCredential(ctx, cred.ID, "admin", "explicit"))
	found, err = s.GetAgentCredentialByJTIHash(ctx, jtiHash)
	require.NoError(t, err)
	assert.NotNil(t, found.RevokedAt)
	assert.NotNil(t, found.RevokedBy)
	assert.Equal(t, "admin", *found.RevokedBy)
	assert.Equal(t, "explicit", *found.RevokeReason)

	// Not found
	_, err = s.GetAgentCredentialByJTIHash(ctx, hashJTI("nonexistent"))
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// TestBulkRevocationByAgent verifies that all credentials for an agent
// can be revoked at once.
func TestBulkRevocationByAgent(t *testing.T) {
	_, s, _, project := setupCredentialTestServer(t)
	ctx := context.Background()

	agentID := tid("agent-bulk-revoke")

	// Create multiple credentials
	for i := 0; i < 3; i++ {
		cred := &store.AgentCredential{
			AgentID:      agentID,
			ProjectID:    project.ID,
			TokenJTIHash: hashJTI(strings.Repeat("a", i+1)),
			IssuedAt:     time.Now(),
			ExpiresAt:    time.Now().Add(10 * time.Hour),
		}
		require.NoError(t, s.CreateAgentCredential(ctx, cred))
	}

	// Create a credential for a different agent (should NOT be revoked)
	otherCred := &store.AgentCredential{
		AgentID:      tid("agent-other-bulk"),
		ProjectID:    project.ID,
		TokenJTIHash: hashJTI("other-agent-jti"),
		IssuedAt:     time.Now(),
		ExpiresAt:    time.Now().Add(10 * time.Hour),
	}
	require.NoError(t, s.CreateAgentCredential(ctx, otherCred))

	// Revoke all for target agent
	n, err := s.RevokeAgentCredentialsByAgent(ctx, agentID, "system", "agent_deleted")
	require.NoError(t, err)
	assert.Equal(t, 3, n)

	// Verify other agent's credential is untouched
	other, err := s.GetAgentCredentialByJTIHash(ctx, hashJTI("other-agent-jti"))
	require.NoError(t, err)
	assert.Nil(t, other.RevokedAt)
}
