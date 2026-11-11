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

// Package hub — tests for the whole-request precheck (checks 1-5) of the
// runtime material selection check sequence: identity locality, the store
// record, store facts (project, token/project match, provenance root),
// credential capability, and root human live authority.
package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	"github.com/stretchr/testify/require"
)

// --- Precheck and identity ---

// TestAgentSecretFetch_RevokedTokenRejected characterizes that a revoked
// agent credential is rejected by the auth middleware (auth.go, which the
// runtime material checks do not own) before the runtime material checks
// run. The runtime material checks inherit this behaviour and do not change
// it.
func TestAgentSecretFetch_RevokedTokenRejected(t *testing.T) {
	f := newMaterialFixture(t, "revoked-token-fetch")
	ctx := context.Background()

	claims, err := f.Server.agentTokenService.ValidateAgentToken(f.Token)
	require.NoError(t, err)
	cred, err := f.Store.GetAgentCredentialByJTIHash(ctx, hashJTI(claims.ID))
	require.NoError(t, err)
	require.NoError(t, f.Store.RevokeAgentCredential(ctx, cred.ID, "test", "explicit"))

	rec := doRequestWithAgentToken(t, f.Server, http.MethodPost, "/api/v1/agent/secrets",
		secretFetchRequest{Keys: []string{"ANY_KEY"}}, f.Token)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for revoked token, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAgentGetSecret_ExpiredTokenRejected characterizes that an expired
// agent token is rejected by the auth middleware (auth.go, which the runtime
// material checks do not own) before the runtime material checks run.
func TestAgentGetSecret_ExpiredTokenRejected(t *testing.T) {
	f := newMaterialFixture(t, "expired-token-get")

	expiredSvc, err := NewAgentTokenService(AgentTokenConfig{
		SigningKey:    f.Server.agentTokenService.config.SigningKey,
		TokenDuration: -time.Hour,
	})
	require.NoError(t, err)
	expiredToken, err := expiredSvc.GenerateAgentToken(f.AgentID, f.ProjectID, []AgentTokenScope{ScopeProjectSecretRead}, []string{f.UserID})
	require.NoError(t, err)

	rec := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+f.AgentID+"/secrets/ANY_KEY", nil, expiredToken)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for expired token, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAgentSecretFetch_MissingIdentityKeepsForbiddenStatus pins the
// exception: the fetch endpoint keeps returning 403 "agent authentication
// required" when the caller is authenticated but not as an agent
// (GetAgentFromContext nil), rather than adopting the neutral
// whole-request-denial status.
func TestAgentSecretFetch_MissingIdentityKeepsForbiddenStatus(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agent/secrets", secretFetchRequest{Keys: []string{"ANY_KEY"}})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 (keeps today's status for a non-agent caller), got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAgentGetSecret_MissingIdentityKeepsUnauthorizedStatus pins the
// exception: the get endpoint keeps returning 401 when the caller is not an
// agent identity (validateAgentSecretAccess, not modified here).
func TestAgentGetSecret_MissingIdentityKeepsUnauthorizedStatus(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/agents/"+tid("missing-identity-agent")+"/secrets/ANY_KEY", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 (keeps today's status for a non-agent caller), got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestMaterialRuntimePrecheck_FederatedIdentityDenied unit-tests check 1
// directly with a federated identity. On the by-key get endpoint,
// validateAgentSecretAccess already rejects federated identities (their
// ProjectID() is empty), so this check is exercised here rather than at the
// HTTP layer.
func TestMaterialRuntimePrecheck_FederatedIdentityDenied(t *testing.T) {
	srv, _ := testServer(t)

	fed := NewFederatedAgentIdentity("https://issuer.example", "remote-agent-1", "remote-project-1",
		"Remote Agent", "remote-root-user", []string{"remote-root-user"}, []AgentTokenScope{ScopeProjectSecretRead})

	_, reason, status := srv.materialRuntimePrecheck(context.Background(), fed)
	if status != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", status)
	}
	if reason != ReasonIdentityNotLocal {
		t.Fatalf("expected reason %q, got %q", ReasonIdentityNotLocal, reason)
	}
}

// TestAgentSecretRead_DeletedAgentDenied covers check 2: GetAgent returns
// soft-deleted rows, so a retained-but-deleted agent record must be denied
// explicitly.
func TestAgentSecretRead_DeletedAgentDenied(t *testing.T) {
	f := newMaterialFixture(t, "deleted-agent")
	ctx := context.Background()

	agent := f.getAgent(t)
	agent.DeletedAt = time.Now()
	require.NoError(t, f.Store.UpdateAgent(ctx, agent))

	ident := newFullAgentIdentity(f.AgentID, f.ProjectID, []string{f.UserID}, []AgentTokenScope{ScopeProjectSecretRead})
	_, reason, status := f.Server.materialRuntimePrecheck(ctx, ident)
	if status != http.StatusForbidden || reason != ReasonTargetUnresolved {
		t.Fatalf("expected 403/%s, got %d/%s", ReasonTargetUnresolved, status, reason)
	}
}

// TestAgentSecretRead_TokenProjectMustMatchAgentRecord covers check 3: the
// token's ProjectID must match the stored agent record's ProjectID.
func TestAgentSecretRead_TokenProjectMustMatchAgentRecord(t *testing.T) {
	f := newMaterialFixture(t, "project-mismatch")
	ctx := context.Background()

	otherProjectID := tid("other-project-mismatch")
	require.NoError(t, f.Store.CreateProject(ctx, &store.Project{
		ID: otherProjectID, Name: "other", Slug: "other-mismatch", Created: time.Now(), Updated: time.Now(),
	}))

	ident := newFullAgentIdentity(f.AgentID, otherProjectID, []string{f.UserID}, []AgentTokenScope{ScopeProjectSecretRead})
	_, reason, status := f.Server.materialRuntimePrecheck(ctx, ident)
	if status != http.StatusForbidden || reason != ReasonTokenProjectMismatch {
		t.Fatalf("expected 403/%s, got %d/%s", ReasonTokenProjectMismatch, status, reason)
	}
}

// TestAgentSecretRead_EmptyAncestryDenied covers check 3: an empty stored
// ancestry (scheduler children and legacy rows) has no root and is denied,
// with no fallback to CreatedBy, OwnerID, or token OriginUserID().
func TestAgentSecretRead_EmptyAncestryDenied(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))
	ctx := context.Background()

	projectID := tid("project-empty-ancestry")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "p", Slug: "p-empty-ancestry", Created: time.Now(), Updated: time.Now(),
	}))
	agentID := tid("agent-empty-ancestry")
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: agentID, Slug: "a-empty-ancestry", Name: "a", ProjectID: projectID,
		Phase: string(state.PhaseRunning), StateVersion: 1, Created: time.Now(), Updated: time.Now(),
	}))

	ident := newFullAgentIdentity(agentID, projectID, nil, []AgentTokenScope{ScopeProjectSecretRead})
	_, reason, status := srv.materialRuntimePrecheck(ctx, ident)
	if status != http.StatusForbidden || reason != ReasonTargetUnresolved {
		t.Fatalf("expected 403/%s, got %d/%s", ReasonTargetUnresolved, status, reason)
	}
}

// TestAgentSecretRead_RootNotAUserDenied covers check 5: Ancestry[0] being
// an agent ID (not a user) makes GetUser return ErrNotFound, which denies
// target_unresolved. Ancestry is immutable after creation (UpdateAgent does
// not set it), so the agent is created directly with the desired ancestry
// rather than built from the shared fixture and mutated.
func TestAgentSecretRead_RootNotAUserDenied(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))
	ctx := context.Background()

	projectID := tid("project-root-not-user")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "p", Slug: "p-root-not-user", Created: time.Now(), Updated: time.Now(),
	}))

	otherAgentID := tid("other-agent-as-root")
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: otherAgentID, Slug: "other-as-root", Name: "other", ProjectID: projectID,
		Phase: string(state.PhaseRunning), StateVersion: 1, Created: time.Now(), Updated: time.Now(),
	}))

	agentID := tid("agent-root-not-user")
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: agentID, Slug: "a-root-not-user", Name: "a", ProjectID: projectID,
		Phase: string(state.PhaseRunning), StateVersion: 1, Ancestry: []string{otherAgentID},
		Created: time.Now(), Updated: time.Now(),
	}))

	ident := newFullAgentIdentity(agentID, projectID, []string{otherAgentID}, []AgentTokenScope{ScopeProjectSecretRead})
	_, reason, status := srv.materialRuntimePrecheck(ctx, ident)
	if status != http.StatusForbidden || reason != ReasonTargetUnresolved {
		t.Fatalf("expected 403/%s, got %d/%s", ReasonTargetUnresolved, status, reason)
	}
}

// TestAgentSecretRead_RequiresProjectSecretReadPermission covers check 4:
// the credential must carry ScopeProjectSecretRead. Missing it denies the
// bulk fetch endpoint and both scopes of the by-key get endpoint.
func TestAgentSecretRead_RequiresProjectSecretReadPermission(t *testing.T) {
	f := newMaterialFixture(t, "cap-required")
	f.reissueToken(t, []AgentTokenScope{ScopeAgentStatusUpdate}, []string{f.UserID})

	seedSecret(t, f.Server.secretBackend, "CAP_KEY", "v", "", "", f.ProjectID)

	rec := doRequestWithAgentToken(t, f.Server, http.MethodPost, "/api/v1/agent/secrets",
		secretFetchRequest{Keys: []string{"CAP_KEY"}}, f.Token)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("bulk fetch: expected 403, got %d: %s", rec.Code, rec.Body.String())
	}

	recProject := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+f.AgentID+"/secrets/CAP_KEY", nil, f.Token)
	if recProject.Code != http.StatusForbidden {
		t.Fatalf("by-key get, project scope: expected 403, got %d: %s", recProject.Code, recProject.Body.String())
	}

	recUser := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+f.AgentID+"/secrets/CAP_KEY?scope=user", nil, f.Token)
	if recUser.Code != http.StatusForbidden {
		t.Fatalf("by-key get, user scope: expected 403, got %d: %s", recUser.Code, recUser.Body.String())
	}
}

// --- Root authority and seeded roles ---

// TestAgentSecretRead_SuspendedRootUserDenied covers check 5: a suspended
// root user denies with source_inactive.
func TestAgentSecretRead_SuspendedRootUserDenied(t *testing.T) {
	f := newMaterialFixture(t, "suspended-root")
	ctx := context.Background()

	u, err := f.Store.GetUser(ctx, f.UserID)
	require.NoError(t, err)
	u.Status = store.UserStatusSuspended
	require.NoError(t, f.Store.UpdateUser(ctx, u))

	ident := newFullAgentIdentity(f.AgentID, f.ProjectID, []string{f.UserID}, []AgentTokenScope{ScopeProjectSecretRead})
	_, reason, status := f.Server.materialRuntimePrecheck(ctx, ident)
	if status != http.StatusForbidden || reason != ReasonSourceInactive {
		t.Fatalf("expected 403/%s, got %d/%s", ReasonSourceInactive, status, reason)
	}
}

// TestAgentSecretRead_RootUserWithoutProjectMembershipDenied covers check 5:
// an active root user who is not a current member of the agent's project is
// denied on both the bulk fetch endpoint and the by-key get endpoint.
func TestAgentSecretRead_RootUserWithoutProjectMembershipDenied(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))
	ctx := context.Background()

	projectID := tid("project-no-membership")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "p", Slug: "p-no-membership", Created: time.Now(), Updated: time.Now(),
	}))
	userID := tid("user-no-membership")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: "u-no-membership@test.com", DisplayName: "u", Role: "member", Status: store.UserStatusActive,
	}))
	agentID := tid("agent-no-membership")
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: agentID, Slug: "a-no-membership", Name: "a", ProjectID: projectID,
		Phase: string(state.PhaseRunning), StateVersion: 1, Ancestry: []string{userID},
		Created: time.Now(), Updated: time.Now(),
	}))
	token, err := srv.agentTokenService.GenerateAgentToken(agentID, projectID, []AgentTokenScope{ScopeProjectSecretRead}, []string{userID})
	require.NoError(t, err)

	seedSecret(t, srv.secretBackend, "NOMEM_KEY", "v", "", "", projectID)

	rec := doRequestWithAgentToken(t, srv, http.MethodPost, "/api/v1/agent/secrets", secretFetchRequest{Keys: []string{"NOMEM_KEY"}}, token)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("bulk fetch: expected 403, got %d: %s", rec.Code, rec.Body.String())
	}

	rec2 := doRequestWithAgentToken(t, srv, http.MethodGet, "/api/v1/agents/"+agentID+"/secrets/NOMEM_KEY", nil, token)
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("by-key get: expected 403, got %d: %s", rec2.Code, rec2.Body.String())
	}
}

// TestAgentGetSecret_OwnerWithoutMembershipDenied covers check 5: OwnerID
// naming the root human is not membership. Only a current project
// membership row admits.
func TestAgentGetSecret_OwnerWithoutMembershipDenied(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))
	ctx := context.Background()

	userID := tid("owner-no-membership")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: "owner-no-membership@test.com", DisplayName: "u", Role: "member", Status: store.UserStatusActive,
	}))
	projectID := tid("project-owner-no-membership")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "p", Slug: "p-owner-no-membership", OwnerID: userID, CreatedBy: userID,
		Created: time.Now(), Updated: time.Now(),
	}))
	agentID := tid("agent-owner-no-membership")
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: agentID, Slug: "a-owner-no-membership", Name: "a", ProjectID: projectID, OwnerID: userID,
		Phase: string(state.PhaseRunning), StateVersion: 1, Ancestry: []string{userID},
		Created: time.Now(), Updated: time.Now(),
	}))
	token, err := srv.agentTokenService.GenerateAgentToken(agentID, projectID, []AgentTokenScope{ScopeProjectSecretRead}, []string{userID})
	require.NoError(t, err)

	seedSecret(t, srv.secretBackend, "OWNER_KEY", "v", "", "", projectID)

	rec := doRequestWithAgentToken(t, srv, http.MethodGet, "/api/v1/agents/"+agentID+"/secrets/OWNER_KEY", nil, token)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 (OwnerID is not membership), got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAgentSecretRead_LostMembershipWithRetainedAncestryDenied covers check
// 5: membership is evaluated live per request. Removing the root's role
// binding denies even though the agent's stored ancestry is unchanged.
func TestAgentSecretRead_LostMembershipWithRetainedAncestryDenied(t *testing.T) {
	f := newMaterialFixture(t, "lost-membership")
	ctx := context.Background()

	bindings, err := f.Store.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, f.UserID)
	require.NoError(t, err)
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == f.ProjectID {
			require.NoError(t, f.Store.DeleteRoleBinding(ctx, b.ID))
		}
	}

	ident := newFullAgentIdentity(f.AgentID, f.ProjectID, []string{f.UserID}, []AgentTokenScope{ScopeProjectSecretRead})
	_, reason, status := f.Server.materialRuntimePrecheck(ctx, ident)
	if status != http.StatusForbidden || reason != ReasonMembershipRequired {
		t.Fatalf("expected 403/%s, got %d/%s", ReasonMembershipRequired, status, reason)
	}
}

// TestAgentSecretRead_HubMemberCatalogGrantsDoNotAdmit covers check 5: a
// hub-member catalog grant (system scope) is not project membership.
func TestAgentSecretRead_HubMemberCatalogGrantsDoNotAdmit(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))
	ctx := context.Background()

	projectID := tid("project-hubmember-grant")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "p", Slug: "p-hubmember-grant", Created: time.Now(), Updated: time.Now(),
	}))
	userID := tid("user-hubmember-grant")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: "hubmember-grant@test.com", DisplayName: "u", Role: "member", Status: store.UserStatusActive,
	}))
	ensureHubMembership(ctx, s, userID)
	agentID := tid("agent-hubmember-grant")
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: agentID, Slug: "a-hubmember-grant", Name: "a", ProjectID: projectID,
		Phase: string(state.PhaseRunning), StateVersion: 1, Ancestry: []string{userID},
		Created: time.Now(), Updated: time.Now(),
	}))

	ident := newFullAgentIdentity(agentID, projectID, []string{userID}, []AgentTokenScope{ScopeProjectSecretRead})
	_, reason, status := srv.materialRuntimePrecheck(ctx, ident)
	if status != http.StatusForbidden || reason != ReasonMembershipRequired {
		t.Fatalf("expected 403/%s, got %d/%s", ReasonMembershipRequired, status, reason)
	}
}

// TestAgentSecretRead_ViewerCatalogGrantsDoNotAdmit covers check 5: a
// hub-viewer catalog grant (system scope) is not project membership.
func TestAgentSecretRead_ViewerCatalogGrantsDoNotAdmit(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))
	ctx := context.Background()

	projectID := tid("project-viewer-grant")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "p", Slug: "p-viewer-grant", Created: time.Now(), Updated: time.Now(),
	}))
	userID := tid("user-viewer-grant")
	createTestUserWithRole(t, s, userID, "viewer-grant@test.com", "member", store.SystemRoleHubViewer)
	agentID := tid("agent-viewer-grant")
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: agentID, Slug: "a-viewer-grant", Name: "a", ProjectID: projectID,
		Phase: string(state.PhaseRunning), StateVersion: 1, Ancestry: []string{userID},
		Created: time.Now(), Updated: time.Now(),
	}))

	ident := newFullAgentIdentity(agentID, projectID, []string{userID}, []AgentTokenScope{ScopeProjectSecretRead})
	_, reason, status := srv.materialRuntimePrecheck(ctx, ident)
	if status != http.StatusForbidden || reason != ReasonMembershipRequired {
		t.Fatalf("expected 403/%s, got %d/%s", ReasonMembershipRequired, status, reason)
	}
}

// TestAgentSecretRead_CustomOnlyProjectBindingAdmits covers check 5: any
// active project-scoped role binding, including a custom role that is not one
// of the built-in membership roles (owner/admin/member), satisfies
// CheckEffectiveMembership. Admission here does not grant reads: check 7
// still requires project.secret_read (see
// TestAgentSecretRead_CustomOnlyMemberWithoutSecretReadDeniedAtCheck7).
func TestAgentSecretRead_CustomOnlyProjectBindingAdmits(t *testing.T) {
	f := newMaterialFixture(t, "custom-only-binding")
	ctx := context.Background()

	// Remove the fixture's own owner membership so only the custom binding
	// remains.
	bindings, err := f.Store.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, f.UserID)
	require.NoError(t, err)
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == f.ProjectID {
			require.NoError(t, f.Store.DeleteRoleBinding(ctx, b.ID))
		}
	}

	rd, err := f.Store.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        "custom-project-role-" + f.UserID,
		ScopeType:   store.RoleScopeProject,
		Permissions: []string{"scheduled_event.update"},
	})
	require.NoError(t, err)
	_, err = f.Store.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      f.UserID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          f.ProjectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	ident := newFullAgentIdentity(f.AgentID, f.ProjectID, []string{f.UserID}, []AgentTokenScope{ScopeProjectSecretRead})
	facts, reason, status := f.Server.materialRuntimePrecheck(ctx, ident)
	if status != 0 || reason != ReasonAllowed || facts == nil {
		t.Fatalf("expected check 5 to admit a custom-only project member, got %d/%s", status, reason)
	}
}

// TestAgentSecretRead_CustomBindingInOtherProjectDoesNotAdmit covers check
// 5: a custom role binding scoped to a different project is not membership
// in the agent's project, even when it carries secret.use and
// project.secret_read, and it is not system authority for secret.use (it is
// project-scoped).
func TestAgentSecretRead_CustomBindingInOtherProjectDoesNotAdmit(t *testing.T) {
	f := newMaterialFixture(t, "custom-other-project")
	ctx := context.Background()

	// Remove the fixture's own owner membership so only the other-project
	// binding remains.
	bindings, err := f.Store.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, f.UserID)
	require.NoError(t, err)
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == f.ProjectID {
			require.NoError(t, f.Store.DeleteRoleBinding(ctx, b.ID))
		}
	}

	otherProjectID := tid("project-custom-other-project-peer")
	require.NoError(t, f.Store.CreateProject(ctx, &store.Project{
		ID: otherProjectID, Name: "peer", Slug: "proj-custom-other-project-peer", Created: time.Now(), Updated: time.Now(),
	}))
	rd, err := f.Store.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        "other-project-secret-reader-" + f.UserID,
		ScopeType:   store.RoleScopeProject,
		Permissions: []string{"secret.use", "project.secret_read"},
	})
	require.NoError(t, err)
	_, err = f.Store.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      f.UserID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          otherProjectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	ident := newFullAgentIdentity(f.AgentID, f.ProjectID, []string{f.UserID}, []AgentTokenScope{ScopeProjectSecretRead})
	_, reason, status := f.Server.materialRuntimePrecheck(ctx, ident)
	if status != http.StatusForbidden || reason != ReasonMembershipRequired {
		t.Fatalf("expected 403/%s, got %d/%s", ReasonMembershipRequired, status, reason)
	}
}

// TestAgentSecretRead_SystemRoleExactPermissionAdmitted covers check 5's
// system-authority leg: a system-scope role holding the exact secret.use
// permission establishes target-applicable authority for the project, even
// with no project membership row.
func TestAgentSecretRead_SystemRoleExactPermissionAdmitted(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))
	ctx := context.Background()

	projectID := tid("project-sysrole-exact")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "p", Slug: "p-sysrole-exact", Created: time.Now(), Updated: time.Now(),
	}))
	userID := tid("user-sysrole-exact")
	systemRoleUserWithPermissions(t, s, userID, []string{"secret.use"})
	agentID := tid("agent-sysrole-exact")
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: agentID, Slug: "a-sysrole-exact", Name: "a", ProjectID: projectID,
		Phase: string(state.PhaseRunning), StateVersion: 1, Ancestry: []string{userID},
		Created: time.Now(), Updated: time.Now(),
	}))

	ident := newFullAgentIdentity(agentID, projectID, []string{userID}, []AgentTokenScope{ScopeProjectSecretRead})
	facts, reason, status := srv.materialRuntimePrecheck(ctx, ident)
	if status != 0 || reason != ReasonAllowed || facts == nil {
		t.Fatalf("expected check 5 to admit via exact system authority for secret.use, got %d/%s", status, reason)
	}
}

// TestAgentSecretRead_SystemRoleUnrelatedPermissionDenied covers check 5's
// system-authority leg: a system-scope role holding a permission other than
// secret.use does not establish authority, so a root with no project
// membership row is denied.
func TestAgentSecretRead_SystemRoleUnrelatedPermissionDenied(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))
	ctx := context.Background()

	projectID := tid("project-sysrole-unrelated")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "p", Slug: "p-sysrole-unrelated", Created: time.Now(), Updated: time.Now(),
	}))
	userID := tid("user-sysrole-unrelated")
	systemRoleUserWithPermissions(t, s, userID, []string{"agent.delete"})
	agentID := tid("agent-sysrole-unrelated")
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: agentID, Slug: "a-sysrole-unrelated", Name: "a", ProjectID: projectID,
		Phase: string(state.PhaseRunning), StateVersion: 1, Ancestry: []string{userID},
		Created: time.Now(), Updated: time.Now(),
	}))

	ident := newFullAgentIdentity(agentID, projectID, []string{userID}, []AgentTokenScope{ScopeProjectSecretRead})
	_, reason, status := srv.materialRuntimePrecheck(ctx, ident)
	if status != http.StatusForbidden || reason != ReasonMembershipRequired {
		t.Fatalf("expected 403/%s, got %d/%s", ReasonMembershipRequired, status, reason)
	}
}

// TestAgentSecretRead_SystemAuthorityLookupErrorDenies covers check 5's
// system-authority leg: a genuine store fault while loading the principal's
// active system-scope role bindings (SystemAuthorityProof ->
// loadActiveSystemScopeCandidates -> ListRoleBindingsForPrincipals) fails
// closed with a 500, not a 403. The no-fault positive control confirms the
// same non-member root is otherwise admitted through secret.use system
// authority, so the later denial is caused by the fault and not by a root
// that was already denied for an unrelated reason.
func TestAgentSecretRead_SystemAuthorityLookupErrorDenies(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))
	ctx := context.Background()

	projectID := tid("project-sysauth-lookup-error")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "p", Slug: "p-sysauth-lookup-error", Created: time.Now(), Updated: time.Now(),
	}))
	userID := tid("user-sysauth-lookup-error")
	systemRoleUserWithPermissions(t, s, userID, []string{"secret.use"})
	agentID := tid("agent-sysauth-lookup-error")
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: agentID, Slug: "a-sysauth-lookup-error", Name: "a", ProjectID: projectID,
		Phase: string(state.PhaseRunning), StateVersion: 1, Ancestry: []string{userID},
		Created: time.Now(), Updated: time.Now(),
	}))
	ident := newFullAgentIdentity(agentID, projectID, []string{userID}, []AgentTokenScope{ScopeProjectSecretRead})

	facts, reason, status := srv.materialRuntimePrecheck(ctx, ident)
	if status != 0 || reason != ReasonAllowed || facts == nil {
		t.Fatalf("positive control: expected check 5 to admit via exact system authority for secret.use, got %d/%s", status, reason)
	}

	srv.authzService = NewAuthzService(&materialFailingStore{Store: s, listRoleBindingsForPrincipalsErr: errors.New("injected")}, logging.Subsystem("hub.auth"))

	_, reason, status = srv.materialRuntimePrecheck(ctx, ident)
	if status != http.StatusInternalServerError || reason != ReasonBackendError {
		t.Fatalf("expected 500/%s, got %d/%s", ReasonBackendError, status, reason)
	}
}

// TestAgentSecretRead_SystemAuthorityServiceUnavailableDenies covers check
// 5's system-authority leg: with no authz service configured, a non-member
// root is denied with a 500, not a 403 -- check 5 never silently skips this
// leg when the service is unavailable.
func TestAgentSecretRead_SystemAuthorityServiceUnavailableDenies(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))
	ctx := context.Background()

	projectID := tid("project-sysauth-unavailable")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "p", Slug: "p-sysauth-unavailable", Created: time.Now(), Updated: time.Now(),
	}))
	userID := tid("user-sysauth-unavailable")
	systemRoleUserWithPermissions(t, s, userID, []string{"secret.use"})
	agentID := tid("agent-sysauth-unavailable")
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: agentID, Slug: "a-sysauth-unavailable", Name: "a", ProjectID: projectID,
		Phase: string(state.PhaseRunning), StateVersion: 1, Ancestry: []string{userID},
		Created: time.Now(), Updated: time.Now(),
	}))
	ident := newFullAgentIdentity(agentID, projectID, []string{userID}, []AgentTokenScope{ScopeProjectSecretRead})
	srv.authzService = nil

	_, reason, status := srv.materialRuntimePrecheck(ctx, ident)
	if status != http.StatusInternalServerError || reason != ReasonBackendError {
		t.Fatalf("expected 500/%s, got %d/%s", ReasonBackendError, status, reason)
	}
}

// TestAgentSecretRead_HubAdminWithoutMembershipDenied covers check 5: a
// hub-admin root without a project membership row is still denied.
// hub-admin's curated permission set does not include secret.use, so check
// 5's system-authority leg (SystemAuthorityProof for the exact secret.use
// permission) does not admit it either. See
// TestAgentSecretRead_SuperAdminExactPermissionAdmitted below for the
// super-admin case, which does hold secret.use and is admitted.
func TestAgentSecretRead_HubAdminWithoutMembershipDenied(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))
	ctx := context.Background()

	projectID := tid("project-sysrole-hub-admin")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "p", Slug: "p-sysrole-hub-admin", Created: time.Now(), Updated: time.Now(),
	}))
	userID := tid("user-sysrole-hub-admin")
	createTestUserWithRole(t, s, userID, "hub-admin-sysrole@test.com", "member", store.SystemRoleHubAdmin)
	agentID := tid("agent-sysrole-hub-admin")
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: agentID, Slug: "a-sysrole-hub-admin", Name: "a", ProjectID: projectID,
		Phase: string(state.PhaseRunning), StateVersion: 1, Ancestry: []string{userID},
		Created: time.Now(), Updated: time.Now(),
	}))

	ident := newFullAgentIdentity(agentID, projectID, []string{userID}, []AgentTokenScope{ScopeProjectSecretRead})
	_, reason, status := srv.materialRuntimePrecheck(ctx, ident)
	if status != http.StatusForbidden || reason != ReasonMembershipRequired {
		t.Fatalf("expected 403/%s, got %d/%s", ReasonMembershipRequired, status, reason)
	}
}

// TestAgentSecretRead_SuperAdminExactPermissionAdmitted covers check 5's
// system-authority leg: a super-admin root without a project membership row
// is admitted, because super-admin holds every registry permission
// (including secret.use) and SystemAuthorityProof admits a
// target-applicable exact permission held through an active system-scope
// role. Check 7 then decides project.secret_read for the agent, which
// holds it through its project:secret:read scope; this test records no
// delegation edge and no backfill marker, so the ceiling's pre-backfill
// exception applies and the value is delivered end to end.
func TestAgentSecretRead_SuperAdminExactPermissionAdmitted(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))
	ctx := context.Background()

	projectID := tid("project-sysrole-super-admin")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "p", Slug: "p-sysrole-super-admin", Created: time.Now(), Updated: time.Now(),
	}))
	userID := tid("user-sysrole-super-admin")
	createTestUserWithRole(t, s, userID, "super-admin-sysrole@test.com", "member", store.SystemRoleSuperAdmin)
	agentID := tid("agent-sysrole-super-admin")
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: agentID, Slug: "a-sysrole-super-admin", Name: "a", ProjectID: projectID,
		Phase: string(state.PhaseRunning), StateVersion: 1, Ancestry: []string{userID},
		Created: time.Now(), Updated: time.Now(),
	}))
	token, err := srv.agentTokenService.GenerateAgentToken(agentID, projectID, []AgentTokenScope{ScopeProjectSecretRead}, []string{userID})
	require.NoError(t, err)

	seedSecret(t, srv.secretBackend, "SUPERADMIN_KEY", "super-secret-value", "", "", projectID)

	rec := doRequestWithAgentToken(t, srv, http.MethodPost, "/api/v1/agent/secrets",
		secretFetchRequest{Keys: []string{"SUPERADMIN_KEY"}}, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp secretFetchResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	if len(resp.Secrets) != 1 || resp.Secrets[0].Status != "ok" || resp.Secrets[0].Value != "super-secret-value" {
		t.Fatalf("expected the value delivered for the super-admin root, got %+v", resp.Secrets)
	}
}

// TestAgentSecretRead_MembershipLookupErrorDenies covers check 5: a genuine
// store fault while checking membership (as opposed to a definite
// non-member) fails closed with a 500, not a 403. Check 5 calls
// CheckEffectiveMembership, which resolves a principal's direct bindings
// through the singular ListRoleBindingsForPrincipal, so the fault is
// injected there.
func TestAgentSecretRead_MembershipLookupErrorDenies(t *testing.T) {
	f := newMaterialFixture(t, "membership-lookup-error")
	f.Server.store = &materialFailingStore{
		Store:                           f.Store,
		listRoleBindingsForPrincipalErr: errors.New("injected membership lookup failure"),
	}

	ident := newFullAgentIdentity(f.AgentID, f.ProjectID, []string{f.UserID}, []AgentTokenScope{ScopeProjectSecretRead})
	_, reason, status := f.Server.materialRuntimePrecheck(context.Background(), ident)
	if status != http.StatusInternalServerError || reason != ReasonBackendError {
		t.Fatalf("expected 500/%s, got %d/%s", ReasonBackendError, status, reason)
	}
}

// TestAgentSecretRead_UserLookupErrorDenies covers check 5: a genuine store
// fault looking up the root user fails closed with a 500, not a 403.
func TestAgentSecretRead_UserLookupErrorDenies(t *testing.T) {
	f := newMaterialFixture(t, "user-lookup-error")
	f.Server.store = &materialFailingStore{
		Store:      f.Store,
		getUserErr: errors.New("injected user lookup failure"),
	}

	ident := newFullAgentIdentity(f.AgentID, f.ProjectID, []string{f.UserID}, []AgentTokenScope{ScopeProjectSecretRead})
	_, reason, status := f.Server.materialRuntimePrecheck(context.Background(), ident)
	if status != http.StatusInternalServerError || reason != ReasonBackendError {
		t.Fatalf("expected 500/%s, got %d/%s", ReasonBackendError, status, reason)
	}
}
