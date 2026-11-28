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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedShellMintAgent(t *testing.T, f *mintFixture, name string, role AgentRole, phase state.Phase) *store.Agent {
	t.Helper()
	a := f.agent(t, name, role, phase)
	a.HarnessConfig = config.ShellHarnessConfigName
	if a.AppliedConfig == nil {
		a.AppliedConfig = &store.AgentAppliedConfig{}
	}
	a.AppliedConfig.HarnessConfig = config.ShellHarnessConfigName
	require.NoError(t, f.store.UpdateAgent(context.Background(), a))
	f.edge(t, store.DelegationPrincipalUser, f.userID, a.ID, ceilPrincip, provSession)
	return a
}

func TestAuthorizeAgentToken_ShellPostConditionFailsClosed(t *testing.T) {
	f := newMintFixture(t, "shell-postcond")
	ctx := context.Background()
	a := seedShellMintAgent(t, f, "shell-postcond-agent", AgentRoleFull, state.PhaseStopped)

	// Control: without the fault hook, mint succeeds with exactly the 3 runtime scopes.
	grant, err := f.srv.AuthorizeAgentToken(ctx, a)
	require.NoError(t, err)
	assert.Equal(t, shellRuntimeScopes, grant.Scopes)

	// Inject a candidate path that leaks a non-runtime scope past mintCandidateScopes.
	f.srv.mintCandidatesTestHook = func(_ *store.Agent, candidates []AgentTokenScope) []AgentTokenScope {
		return append(append([]AgentTokenScope(nil), candidates...), ScopeProjectSecretRead)
	}

	// Direct authorizeAgentTokenAt fails closed and records the audit entry.
	deniedGrant, err := authorizeAgentTokenAt(ctx, f.srv, f.store, a, mintSiteStart)
	require.Error(t, err)
	assert.Nil(t, deniedGrant)
	assert.ErrorIs(t, err, errMintLookup)
	assert.Contains(t, err.Error(), "shell agent scope cap violated")

	rec := httptest.NewRecorder()
	require.True(t, writeAgentTokenIssueError(rec, err))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)

	assertIssueDeniedAudit(t, f.store, a.ID, mintSiteStart, mintErrorClassLookup)

	// Dispatch path also refuses before touching the broker.
	err = f.disp.DispatchAgentStart(ctx, a, "", false)
	require.Error(t, err)
	assert.ErrorIs(t, err, errMintLookup)
	assert.False(t, f.client.startCalled, "no broker request when post-condition fails")
}

func TestAgentTokenRefresh_ShellLegacyFullTokenNarrowed(t *testing.T) {
	f := newMintFixture(t, "shell-refresh")
	ctx := context.Background()
	a := seedShellMintAgent(t, f, "shell-refresh-agent", AgentRoleFull, state.PhaseRunning)

	// Mint a legacy full-scope token (simulating a pre-cap shell container).
	legacyToken, err := f.srv.GenerateAgentToken(a.ID, a.ProjectID, a.Ancestry, AgentRoleFull, nil)
	require.NoError(t, err)
	legacyClaims := f.tokenClaims(t, legacyToken)
	require.Greater(t, len(legacyClaims.Scopes), len(shellRuntimeScopes), "legacy token carries full scopes")
	require.Contains(t, legacyClaims.Scopes, ScopeProjectSecretRead)
	require.NotEmpty(t, legacyClaims.ID, "legacy token has JTI")

	beforeCred := getTestAgentCredential(t, f.store, legacyClaims.ID)
	require.Nil(t, beforeCred.RevokedAt)

	// Refresh the legacy token through the real HTTP endpoint.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+a.ID+"/token/refresh", nil)
	req.Header.Set("X-Scion-Agent-Token", legacyToken)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)

	newToken := refreshedToken(t, rec)
	newClaims := f.tokenClaims(t, newToken)
	assert.Equal(t, shellRuntimeScopes, newClaims.Scopes, "refreshed shell token narrowed to the 3 runtime scopes")

	// Old credential is revoked.
	afterCred := getTestAgentCredential(t, f.store, legacyClaims.ID)
	require.NotNil(t, afterCred.RevokedAt, "legacy credential must be revoked after refresh")
	require.NotNil(t, afterCred.RevokeReason)
	assert.Equal(t, "refreshed", *afterCred.RevokeReason)

	// Stored role remains unchanged (invariant 7).
	stored, err := f.store.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	require.NotNil(t, stored.AppliedConfig)
	assert.Equal(t, string(AgentRoleFull), stored.AppliedConfig.AgentRole)
}

func TestShellAgent_ConduitAdmittedWithCappedToken(t *testing.T) {
	f := newMintFixture(t, "shell-conduit")
	ctx := context.Background()
	a := seedShellMintAgent(t, f, "shell-conduit-agent", AgentRoleFull, state.PhaseRunning)

	// Mint a real capped token via the production path.
	grant, err := authorizeAgentTokenAt(ctx, f.srv, f.store, a, mintSiteStart)
	require.NoError(t, err)
	require.Equal(t, shellRuntimeScopes, grant.Scopes)
	tok, err := signAndRecordAgentToken(ctx, f.srv, f.store, *grant, "")
	require.NoError(t, err)

	claims := f.tokenClaims(t, tok)
	ident := &agentIdentityWrapper{AgentTokenClaims: claims}

	// Conduit scope check and full admission check accept the capped token.
	require.NoError(t, conduitAgentScopeCheck(ident))
	admitted, err := f.srv.conduitAgentAdmissionCheck(ctx, ident)
	require.NoError(t, err)
	assert.Equal(t, a.ID, admitted.ID)

	doAgentReq := func(method, path, token string, body any) *httptest.ResponseRecorder {
		t.Helper()
		var buf bytes.Buffer
		if body != nil {
			require.NoError(t, json.NewEncoder(&buf).Encode(body))
		}
		req := httptest.NewRequest(method, path, &buf)
		req.Header.Set("X-Scion-Agent-Token", token)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		f.srv.Handler().ServeHTTP(rec, req)
		return rec
	}

	// updateAgentStatus succeeds.
	statusRec := doAgentReq(http.MethodPost, "/api/v1/agents/"+a.ID+"/status", tok, store.AgentStatusUpdate{
		Activity: string(state.ActivityIdle),
	})
	require.Equal(t, http.StatusOK, statusRec.Code, statusRec.Body.String())

	// Heartbeat succeeds.
	hbRec := doAgentReq(http.MethodPost, "/api/v1/agents/"+a.ID+"/status", tok, store.AgentStatusUpdate{
		Heartbeat: true,
	})
	require.Equal(t, http.StatusOK, hbRec.Code, hbRec.Body.String())

	// RefreshToken succeeds and yields another capped token.
	refreshRec := doAgentReq(http.MethodPost, "/api/v1/agents/"+a.ID+"/token/refresh", tok, nil)
	tok = refreshedToken(t, refreshRec)
	assert.Equal(t, shellRuntimeScopes, f.tokenClaims(t, tok).Scopes)

	// ListSecrets (project scope) is refused with missing_scope (403).
	secretsRec := doAgentReq(http.MethodGet, "/api/v1/secrets?scope=project&scopeId="+f.projectID, tok, nil)
	require.Equal(t, http.StatusForbidden, secretsRec.Code, secretsRec.Body.String())
	assert.Contains(t, secretsRec.Body.String(), string(ScopeProjectSecretRead))

	// RegisterPort is refused (403).
	portsRec := doAgentReq(http.MethodPost, "/api/v1/agents/"+a.ID+"/ports", tok, map[string]any{
		"port":  8080,
		"label": "web",
	})
	require.Equal(t, http.StatusForbidden, portsRec.Code, portsRec.Body.String())

	// Agent create is refused with missing_scope (403).
	createRec := doAgentReq(http.MethodPost, "/api/v1/projects/"+f.projectID+"/agents", tok, CreateAgentRequest{
		Name: "unauthorized-child",
	})
	require.Equal(t, http.StatusForbidden, createRec.Code, createRec.Body.String())
	assert.Contains(t, createRec.Body.String(), string(ScopeAgentCreate))

	// Secondary §4.1 callers inherit the cap from mintCandidateScopes:
	// - scope reissue candidates
	assert.Equal(t, shellRuntimeScopes, reissueCandidateScopes(f.srv.authzService, a, AgentRoleFull))
	// - EffectiveAgentAuthority
	effScopes, _, err := f.srv.authzService.EffectiveAgentAuthority(ctx, a, AgentAuthorityOptions{})
	require.NoError(t, err)
	assert.Equal(t, shellRuntimeScopes, effScopes)
	// - artifact recipient eligibility (shell agent lacks project:artifact:read)
	elig, err := f.srv.authzService.ceilingFilteredAgentScopes(ctx, a, f.srv.authzService.mintCandidateScopes(a))
	require.NoError(t, err)
	assert.NotContains(t, elig, ScopeProjectArtifactRead)
}

func TestDroppedScopesWarning_ShellAgentNone(t *testing.T) {
	f := newMintFixture(t, "shell-dropped")
	ctx := context.Background()

	// Build a ceiling that permits the 3 runtime scopes' permissions but omits
	// ceiling-optional scopes (e.g. artifact/template/skill write).
	runtimeOnlyCeiling := boundedCeiling(sortedUniqueIDs(agentScopeCoverage(shellRuntimeScopes))...)

	// Shell agent with full role under the narrowed ceiling: no dropped-scope
	// warning because the cap is applied to candidates before ceiling filtering.
	shellAgent := f.agent(t, "shell-dropped-agent", AgentRoleFull, state.PhaseStopped)
	shellAgent.HarnessConfig = config.ShellHarnessConfigName
	shellAgent.AppliedConfig.HarnessConfig = config.ShellHarnessConfigName
	require.NoError(t, f.store.UpdateAgent(ctx, shellAgent))
	f.edge(t, store.DelegationPrincipalUser, f.userID, shellAgent.ID, runtimeOnlyCeiling, provSession)

	shellGrant, err := f.srv.AuthorizeAgentToken(ctx, shellAgent)
	require.NoError(t, err)
	assert.Equal(t, shellRuntimeScopes, shellGrant.Scopes)
	assert.Empty(t, shellGrant.DroppedScopes)
	assert.Empty(t, droppedScopesWarning(shellAgent, &shellGrant))

	// Also verify under a full (principal) ceiling for shell create/start.
	shellFull := seedShellMintAgent(t, f, "shell-full-ceiling", AgentRoleFull, state.PhaseStopped)
	shellFullGrant, err := f.srv.AuthorizeAgentToken(ctx, shellFull)
	require.NoError(t, err)
	assert.Equal(t, shellRuntimeScopes, shellFullGrant.Scopes)
	assert.Empty(t, shellFullGrant.DroppedScopes)
	assert.Empty(t, droppedScopesWarning(shellFull, &shellFullGrant))

	// Control: a non-shell full-role agent under the same narrowed ceiling
	// produces a non-empty droppedScopesWarning, proving the test is sensitive.
	nonShell := f.agent(t, "nonshell-dropped-agent", AgentRoleFull, state.PhaseStopped)
	f.edge(t, store.DelegationPrincipalUser, f.userID, nonShell.ID, runtimeOnlyCeiling, provSession)

	nonShellGrant, err := f.srv.AuthorizeAgentToken(ctx, nonShell)
	require.NoError(t, err)
	assert.NotEmpty(t, nonShellGrant.DroppedScopes)
	assert.NotEmpty(t, droppedScopesWarning(nonShell, &nonShellGrant))
}
