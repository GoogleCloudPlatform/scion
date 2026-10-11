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

// This file tests the rule that agent-token authentication requires a
// successful credential-status evaluation: any credential-store error other
// than "not found" must produce a retryable failure (503), never continued
// authentication. It covers both the UnifiedAuthMiddleware agent-token branch
// (pkg/hub/auth.go) and the agent token refresh handler
// (pkg/hub/handlers_agents_core.go), plus the shared evaluateAgentCredentialStatus
// helper (pkg/hub/agenttoken.go).
package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// erroringCredentialStore wraps a store.Store but makes
// GetAgentCredentialByJTIHash fail with a fixed error while fault is active
// (always, when fault is nil), simulating a credential-store outage. All
// other methods delegate to the embedded store.
type erroringCredentialStore struct {
	store.Store
	err   error
	fault *storeFaultSwitch
}

func (e *erroringCredentialStore) GetAgentCredentialByJTIHash(ctx context.Context, jtiHash string) (*store.AgentCredential, error) {
	if !e.fault.Active() {
		return e.Store.GetAgentCredentialByJTIHash(ctx, jtiHash)
	}
	return nil, e.err
}

// setupCredentialTestServerWithCredFault is setupCredentialTestServer with
// the server's store wrapped in an erroringCredentialStore (returning err)
// before setup. The wrapper delegates until the returned switch is
// armed; arm it instead of reassigning srv.store after setup, which races
// the setup's emitMutationAudit goroutines (ptone/scion#2577).
func setupCredentialTestServerWithCredFault(t *testing.T, err error) (*Server, store.Store, *store.User, *store.Project, *storeFaultSwitch) {
	t.Helper()
	srv, s, _, fault := testServerWithStoreFault(t, func(inner store.Store, fault *storeFaultSwitch) *erroringCredentialStore {
		return &erroringCredentialStore{Store: inner, err: err, fault: fault}
	})
	srv, s, user, project := setupCredentialTestServerOn(t, srv, s)
	return srv, s, user, project, fault
}

// apiErrorBody mirrors the JSON shape written by writeError, for assertions
// on the error code and message returned to the client.
type apiErrorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func decodeAPIError(t *testing.T, body []byte) apiErrorBody {
	t.Helper()
	var resp apiErrorBody
	require.NoError(t, json.Unmarshal(body, &resp))
	return resp
}

// --- evaluateAgentCredentialStatus (unit-level) ---------------------------

// fakeAgentCredentialStore is a minimal store.AgentCredentialStore double
// used to unit-test evaluateAgentCredentialStatus without a real backing
// store. Only GetAgentCredentialByJTIHash is exercised.
type fakeAgentCredentialStore struct {
	store.AgentCredentialStore
	cred *store.AgentCredential
	err  error
}

func (f *fakeAgentCredentialStore) GetAgentCredentialByJTIHash(ctx context.Context, jtiHash string) (*store.AgentCredential, error) {
	return f.cred, f.err
}

func TestEvaluateAgentCredentialStatus_Active(t *testing.T) {
	cred := &store.AgentCredential{ID: "cred-1"}
	fake := &fakeAgentCredentialStore{cred: cred}

	got, isLegacy, err := evaluateAgentCredentialStatus(context.Background(), fake, "some-jti")
	require.NoError(t, err)
	assert.False(t, isLegacy)
	assert.Equal(t, cred, got)
}

func TestEvaluateAgentCredentialStatus_Revoked(t *testing.T) {
	revokedAt := time.Now()
	cred := &store.AgentCredential{ID: "cred-1", RevokedAt: &revokedAt}
	fake := &fakeAgentCredentialStore{cred: cred}

	got, isLegacy, err := evaluateAgentCredentialStatus(context.Background(), fake, "some-jti")
	assert.True(t, errors.Is(err, errAgentCredentialRevoked))
	assert.False(t, isLegacy)
	assert.Nil(t, got)
}

func TestEvaluateAgentCredentialStatus_NotFoundIsLegacy(t *testing.T) {
	fake := &fakeAgentCredentialStore{err: store.ErrNotFound}

	got, isLegacy, err := evaluateAgentCredentialStatus(context.Background(), fake, "some-jti")
	require.NoError(t, err)
	assert.True(t, isLegacy)
	assert.Nil(t, got)
}

func TestEvaluateAgentCredentialStatus_StoreErrorIsRetryable(t *testing.T) {
	storeErr := errors.New("credential status lookup failed")
	fake := &fakeAgentCredentialStore{err: storeErr}

	got, isLegacy, err := evaluateAgentCredentialStatus(context.Background(), fake, "some-jti")
	assert.ErrorIs(t, err, storeErr)
	assert.False(t, isLegacy)
	assert.False(t, errors.Is(err, errAgentCredentialRevoked))
	assert.Nil(t, got)
}

// --- UnifiedAuthMiddleware agent-token branch (behavior-level) ------------

// TestAgentAuthStoreErrorReturns503 verifies that when the credential store
// returns an error other than "not found", the middleware responds 503 and
// the request never reaches the handler.
func TestAgentAuthStoreErrorReturns503(t *testing.T) {
	srv, s, _, project := setupCredentialTestServer(t)

	agentID := tid("agent-auth-store-err")
	createCredTestAgent(t, s, agentID, project.ID, tid("user-cred-test"))

	token, err := srv.GenerateAgentToken(agentID, project.ID, nil, AgentRoleFull, nil)
	require.NoError(t, err)

	// From this point on, credential-status evaluation cannot succeed or
	// determine "not found" — simulate a credential-store outage.
	srv.authConfig.CredentialStore = &erroringCredentialStore{
		Store: s,
		err:   errors.New("credential store unavailable for testing"),
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/"+agentID, nil)
	req.Header.Set("X-Scion-Agent-Token", token)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code, "body: %s", rec.Body.String())

	resp := decodeAPIError(t, rec.Body.Bytes())
	assert.Equal(t, ErrCodeUnavailable, resp.Error.Code)
	// The response must be the generic service-unavailable error produced by
	// the middleware, not agent data from the handler, and must not leak the
	// underlying store error text.
	assert.NotContains(t, rec.Body.String(), "credential store unavailable for testing")
}

// TestAgentAuthActiveCredentialAllowsRequest verifies that a token backed by
// an active (found, not revoked) credential authenticates normally.
func TestAgentAuthActiveCredentialAllowsRequest(t *testing.T) {
	srv, s, _, project := setupCredentialTestServer(t)

	agentID := tid("agent-auth-active-ok")
	createCredTestAgent(t, s, agentID, project.ID, tid("user-cred-test"))

	token, err := srv.GenerateAgentToken(agentID, project.ID, nil, AgentRoleFull, nil)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/"+agentID, nil)
	req.Header.Set("X-Scion-Agent-Token", token)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	// Authentication must succeed for an active credential; a subsequent
	// authorization denial (RBAC bindings) is a separate concern from
	// credential-status evaluation and is not what this test checks.
	assert.NotEqual(t, http.StatusUnauthorized, rec.Code, "body: %s", rec.Body.String())
	assert.NotEqual(t, http.StatusServiceUnavailable, rec.Code, "body: %s", rec.Body.String())
}

// --- Refresh handler: credential-ID marker present ------------------------

// buildAgentRefreshRequest constructs a POST .../token/refresh request whose
// context carries the given agent identity and optional credential markers,
// without routing it through UnifiedAuthMiddleware, so the refresh handler's
// own status evaluation (including the "marker absent" fallback) can be
// exercised directly and deterministically.
func buildAgentRefreshRequest(agentID string, claims *AgentTokenClaims, credentialID string, legacy bool) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agentID+"/token/refresh", nil)
	ctx := req.Context()
	identity := &agentIdentityWrapper{claims}
	ctx = contextWithIdentity(ctx, identity)
	if credentialID != "" {
		ctx = context.WithValue(ctx, agentCredentialIDContextKey{}, credentialID)
	}
	if legacy {
		ctx = context.WithValue(ctx, legacyTokenContextKey{}, true)
	}
	return req.WithContext(ctx)
}

func TestAgentRefresh_MarkerPresent_StoreErrorReturns503(t *testing.T) {
	srv, s, _, project, credFault := setupCredentialTestServerWithCredFault(t, errors.New("boom"))

	agentID := tid("agent-refresh-marker-store-err")
	createCredTestAgent(t, s, agentID, project.ID, tid("user-cred-test"))

	token, err := srv.GenerateAgentToken(agentID, project.ID, nil, AgentRoleFull, nil)
	require.NoError(t, err)
	claims, err := srv.agentTokenService.ValidateAgentToken(token)
	require.NoError(t, err)
	cred, err := s.GetAgentCredentialByJTIHash(context.Background(), hashJTI(claims.ID))
	require.NoError(t, err)

	// The refresh handler must re-check the credential-ID marker's lookup
	// result itself; a store error there must 503, not proceed.
	credFault.Arm()

	req := buildAgentRefreshRequest(agentID, claims, cred.ID, false)
	rec := httptest.NewRecorder()
	srv.handleAgentTokenRefresh(rec, req, agentID)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code, "body: %s", rec.Body.String())
	resp := decodeAPIError(t, rec.Body.Bytes())
	assert.Equal(t, ErrCodeUnavailable, resp.Error.Code)
}

func TestAgentRefresh_MarkerPresent_RevokedDenied(t *testing.T) {
	srv, s, _, project := setupCredentialTestServer(t)

	agentID := tid("agent-refresh-marker-revoked")
	createCredTestAgent(t, s, agentID, project.ID, tid("user-cred-test"))

	token, err := srv.GenerateAgentToken(agentID, project.ID, nil, AgentRoleFull, nil)
	require.NoError(t, err)
	claims, err := srv.agentTokenService.ValidateAgentToken(token)
	require.NoError(t, err)
	cred, err := s.GetAgentCredentialByJTIHash(context.Background(), hashJTI(claims.ID))
	require.NoError(t, err)
	require.NoError(t, s.RevokeAgentCredential(context.Background(), cred.ID, "test", "explicit"))

	req := buildAgentRefreshRequest(agentID, claims, cred.ID, false)
	rec := httptest.NewRecorder()
	srv.handleAgentTokenRefresh(rec, req, agentID)

	require.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "revoked")
}

// --- Refresh handler: credential-ID marker absent -------------------------
//
// UnifiedAuthMiddleware sets either the credential-ID marker or the legacy
// marker on every agent-token request it authenticates; a store error returns
// 503 before the handler is reached. The refresh handler does not rely on
// that invariant alone: it re-derives the JTI from the validated token and
// evaluates status directly whenever both markers are absent, so these tests
// exercise the handler in isolation with no marker set.

func TestAgentRefresh_MarkerAbsent_StoreErrorReturns503(t *testing.T) {
	srv, s, _, project, credFault := setupCredentialTestServerWithCredFault(t, errors.New("boom"))

	agentID := tid("agent-refresh-nomarker-store-err")
	createCredTestAgent(t, s, agentID, project.ID, tid("user-cred-test"))

	token, err := srv.GenerateAgentToken(agentID, project.ID, nil, AgentRoleFull, nil)
	require.NoError(t, err)
	claims, err := srv.agentTokenService.ValidateAgentToken(token)
	require.NoError(t, err)

	credFault.Arm()

	req := buildAgentRefreshRequest(agentID, claims, "", false)
	rec := httptest.NewRecorder()
	srv.handleAgentTokenRefresh(rec, req, agentID)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code, "body: %s", rec.Body.String())
	resp := decodeAPIError(t, rec.Body.Bytes())
	assert.Equal(t, ErrCodeUnavailable, resp.Error.Code)
}

func TestAgentRefresh_MarkerAbsent_RevokedDenied(t *testing.T) {
	srv, s, _, project := setupCredentialTestServer(t)

	agentID := tid("agent-refresh-nomarker-revoked")
	createCredTestAgent(t, s, agentID, project.ID, tid("user-cred-test"))

	token, err := srv.GenerateAgentToken(agentID, project.ID, nil, AgentRoleFull, nil)
	require.NoError(t, err)
	claims, err := srv.agentTokenService.ValidateAgentToken(token)
	require.NoError(t, err)
	cred, err := s.GetAgentCredentialByJTIHash(context.Background(), hashJTI(claims.ID))
	require.NoError(t, err)
	require.NoError(t, s.RevokeAgentCredential(context.Background(), cred.ID, "test", "explicit"))

	// No markers set: the handler must detect the revocation itself.
	req := buildAgentRefreshRequest(agentID, claims, "", false)
	rec := httptest.NewRecorder()
	srv.handleAgentTokenRefresh(rec, req, agentID)

	require.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "revoked")
}

func TestAgentRefresh_MarkerAbsent_ActiveRefreshesAndRevokesOldCredential(t *testing.T) {
	srv, s, _, project := setupCredentialTestServer(t)

	agentID := tid("agent-refresh-nomarker-active")
	createCredTestAgent(t, s, agentID, project.ID, tid("user-cred-test"))

	token, err := srv.GenerateAgentToken(agentID, project.ID, nil, AgentRoleFull, nil)
	require.NoError(t, err)
	claims, err := srv.agentTokenService.ValidateAgentToken(token)
	require.NoError(t, err)
	cred, err := s.GetAgentCredentialByJTIHash(context.Background(), hashJTI(claims.ID))
	require.NoError(t, err)
	require.Nil(t, cred.RevokedAt)

	// No markers set: the handler evaluates the active credential from the
	// token JTI and revokes it once the replacement token is minted.
	req := buildAgentRefreshRequest(agentID, claims, "", false)
	rec := httptest.NewRecorder()
	srv.handleAgentTokenRefresh(rec, req, agentID)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	got, err := s.GetAgentCredentialByJTIHash(context.Background(), hashJTI(claims.ID))
	require.NoError(t, err)
	require.NotNil(t, got.RevokedAt, "old credential must be revoked after a successful refresh")
}

func TestAgentRefresh_MarkerAbsent_NotFoundFallsBackToLegacy(t *testing.T) {
	srv, s, _, project := setupCredentialTestServer(t)

	agentID := tid("agent-refresh-nomarker-legacy")
	createCredTestAgent(t, s, agentID, project.ID, tid("user-cred-test"))

	claims := newLegacyAgentToken(t, srv, agentID, project.ID)

	req := buildAgentRefreshRequest(agentID, claims, "", false)
	rec := httptest.NewRecorder()
	srv.handleAgentTokenRefresh(rec, req, agentID)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
}

// newLegacyAgentToken returns validated claims for an agent token minted
// without a credential recorder, so its JTI has no credential row.
func newLegacyAgentToken(t *testing.T, srv *Server, agentID, projectID string) *AgentTokenClaims {
	t.Helper()
	legacyService, err := NewAgentTokenService(AgentTokenConfig{
		SigningKey:    srv.agentTokenService.config.SigningKey,
		TokenDuration: time.Hour,
	})
	require.NoError(t, err)
	legacyToken, err := legacyService.GenerateAgentToken(
		agentID, projectID, ScopesForRole(AgentRoleFull), nil)
	require.NoError(t, err)
	claims, err := srv.agentTokenService.ValidateAgentToken(legacyToken)
	require.NoError(t, err)
	return claims
}

func TestAgentRefresh_MarkerAbsent_LegacySuspendedAgentDenied(t *testing.T) {
	srv, s, _, project := setupCredentialTestServer(t)

	agentID := tid("agent-refresh-nomarker-legacy-susp")
	agent := createCredTestAgent(t, s, agentID, project.ID, tid("user-cred-test"))
	agent.Phase = "suspended"
	require.NoError(t, s.UpdateAgent(context.Background(), agent))

	claims := newLegacyAgentToken(t, srv, agentID, project.ID)

	// No markers set and no credential row: the handler classifies the token
	// as legacy and applies the legacy agent-state checks.
	req := buildAgentRefreshRequest(agentID, claims, "", false)
	rec := httptest.NewRecorder()
	srv.handleAgentTokenRefresh(rec, req, agentID)

	require.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "suspended")
}

func TestAgentRefresh_MarkerAbsent_LegacyDeletedAgentDenied(t *testing.T) {
	srv, s, _, project := setupCredentialTestServer(t)

	agentID := tid("agent-refresh-nomarker-legacy-del")
	agent := createCredTestAgent(t, s, agentID, project.ID, tid("user-cred-test"))
	agent.DeletedAt = time.Now()
	require.NoError(t, s.UpdateAgent(context.Background(), agent))

	claims := newLegacyAgentToken(t, srv, agentID, project.ID)

	// No markers set and no credential row: the handler classifies the token
	// as legacy and applies the legacy agent-state checks.
	req := buildAgentRefreshRequest(agentID, claims, "", false)
	rec := httptest.NewRecorder()
	srv.handleAgentTokenRefresh(rec, req, agentID)

	require.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "deleted")
}

// --- Store returns a nil credential with a nil error ----------------------
//
// A credential store that reports success without a credential record gives
// no status to evaluate. Each status-lookup site treats that as a store
// failure: it does not authenticate or refresh, and the caller gets the
// retryable 503 path.

func TestEvaluateAgentCredentialStatus_NilCredentialNilErrorIsStoreFailure(t *testing.T) {
	fake := &fakeAgentCredentialStore{}

	got, isLegacy, err := evaluateAgentCredentialStatus(context.Background(), fake, "some-jti")
	require.Error(t, err)
	assert.ErrorIs(t, err, errAgentCredentialMissing)
	assert.False(t, errors.Is(err, errAgentCredentialRevoked))
	assert.False(t, isLegacy)
	assert.Nil(t, got)
}

func TestAgentAuthNilCredentialNilErrorReturns503(t *testing.T) {
	srv, s, _, project := setupCredentialTestServer(t)

	agentID := tid("agent-auth-nil-cred")
	createCredTestAgent(t, s, agentID, project.ID, tid("user-cred-test"))

	token, err := srv.GenerateAgentToken(agentID, project.ID, nil, AgentRoleFull, nil)
	require.NoError(t, err)

	// A nil err makes the wrapper return (nil, nil).
	srv.authConfig.CredentialStore = &erroringCredentialStore{Store: s}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/"+agentID, nil)
	req.Header.Set("X-Scion-Agent-Token", token)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code, "body: %s", rec.Body.String())
	resp := decodeAPIError(t, rec.Body.Bytes())
	assert.Equal(t, ErrCodeUnavailable, resp.Error.Code)
}

func TestAgentRefresh_MarkerPresent_NilCredentialNilErrorReturns503(t *testing.T) {
	srv, s, _, project, credFault := setupCredentialTestServerWithCredFault(t, nil)

	agentID := tid("agent-refresh-marker-nil-cred")
	createCredTestAgent(t, s, agentID, project.ID, tid("user-cred-test"))

	token, err := srv.GenerateAgentToken(agentID, project.ID, nil, AgentRoleFull, nil)
	require.NoError(t, err)
	claims, err := srv.agentTokenService.ValidateAgentToken(token)
	require.NoError(t, err)
	cred, err := s.GetAgentCredentialByJTIHash(context.Background(), hashJTI(claims.ID))
	require.NoError(t, err)

	credFault.Arm()

	req := buildAgentRefreshRequest(agentID, claims, cred.ID, false)
	rec := httptest.NewRecorder()
	srv.handleAgentTokenRefresh(rec, req, agentID)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code, "body: %s", rec.Body.String())
	resp := decodeAPIError(t, rec.Body.Bytes())
	assert.Equal(t, ErrCodeUnavailable, resp.Error.Code)
	assert.Equal(t, "unable to verify credential status", resp.Error.Message)

	// Refresh did not proceed: the old credential is still active.
	got, err := s.GetAgentCredentialByJTIHash(context.Background(), hashJTI(claims.ID))
	require.NoError(t, err)
	assert.Nil(t, got.RevokedAt)
}

func TestAgentRefresh_MarkerAbsent_NilCredentialNilErrorReturns503(t *testing.T) {
	srv, s, _, project, credFault := setupCredentialTestServerWithCredFault(t, nil)

	agentID := tid("agent-refresh-nomarker-nil-cred")
	createCredTestAgent(t, s, agentID, project.ID, tid("user-cred-test"))

	token, err := srv.GenerateAgentToken(agentID, project.ID, nil, AgentRoleFull, nil)
	require.NoError(t, err)
	claims, err := srv.agentTokenService.ValidateAgentToken(token)
	require.NoError(t, err)

	credFault.Arm()

	req := buildAgentRefreshRequest(agentID, claims, "", false)
	rec := httptest.NewRecorder()
	srv.handleAgentTokenRefresh(rec, req, agentID)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code, "body: %s", rec.Body.String())
	resp := decodeAPIError(t, rec.Body.Bytes())
	assert.Equal(t, ErrCodeUnavailable, resp.Error.Code)
	assert.Equal(t, "unable to verify credential status", resp.Error.Message)
}
