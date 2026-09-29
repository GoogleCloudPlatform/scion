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

// Package hub — F.2a tests for the whole-request precheck (checks 1-5) of
// the runtime material selection check sequence: identity locality, the
// store record, store facts (project, token/project match, provenance
// root), credential capability, and root human live authority. See
// F/design/f2-material-selection.md section 8.2.
package hub

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// --- Precheck and identity ---

// TestAgentSecretFetch_RevokedTokenRejected characterizes that a revoked
// agent credential is rejected by the auth middleware (auth.go, not
// F.2a-owned) before F.2a's own checks run. F.2a inherits this behaviour and
// does not change it.
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
// agent token is rejected by the auth middleware (auth.go, not F.2a-owned)
// before F.2a's own checks run.
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

// TestAgentSecretFetch_MissingIdentityKeepsForbiddenStatus pins the N-1
// exception: P7 keeps returning 403 "agent authentication required" when
// the caller is authenticated but not as an agent (GetAgentFromContext nil),
// rather than adopting F.2a's neutral whole-request-denial status.
func TestAgentSecretFetch_MissingIdentityKeepsForbiddenStatus(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agent/secrets", secretFetchRequest{Keys: []string{"ANY_KEY"}})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 (N-1: keeps today's status for a non-agent caller), got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAgentGetSecret_MissingIdentityKeepsUnauthorizedStatus pins the N-1
// exception: P8 keeps returning 401 when the caller is not an agent
// identity (validateAgentSecretAccess, D-owned, not modified by F.2a).
func TestAgentGetSecret_MissingIdentityKeepsUnauthorizedStatus(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/agents/"+tid("missing-identity-agent")+"/secrets/ANY_KEY", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 (N-1: keeps today's status for a non-agent caller), got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestMaterialRuntimePrecheck_FederatedIdentityDenied unit-tests check 1
// directly with a federated identity. At P8, validateAgentSecretAccess
// already rejects federated identities (their ProjectID() is empty), so
// this check is exercised here rather than at the HTTP layer.
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
// the credential must carry ScopeProjectSecretRead. Missing it denies both
// P7 and both scopes of P8.
func TestAgentSecretRead_RequiresProjectSecretReadPermission(t *testing.T) {
	f := newMaterialFixture(t, "cap-required")
	f.reissueToken(t, []AgentTokenScope{ScopeAgentStatusUpdate}, []string{f.UserID})

	seedSecret(t, f.Server.secretBackend, "CAP_KEY", "v", "", "", f.ProjectID)

	rec := doRequestWithAgentToken(t, f.Server, http.MethodPost, "/api/v1/agent/secrets",
		secretFetchRequest{Keys: []string{"CAP_KEY"}}, f.Token)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("P7: expected 403, got %d: %s", rec.Code, rec.Body.String())
	}

	recProject := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+f.AgentID+"/secrets/CAP_KEY", nil, f.Token)
	if recProject.Code != http.StatusForbidden {
		t.Fatalf("P8 project scope: expected 403, got %d: %s", recProject.Code, recProject.Body.String())
	}

	recUser := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+f.AgentID+"/secrets/CAP_KEY?scope=user", nil, f.Token)
	if recUser.Code != http.StatusForbidden {
		t.Fatalf("P8 user scope: expected 403, got %d: %s", recUser.Code, recUser.Body.String())
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
// denied on both P7 and P8.
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
		t.Fatalf("P7: expected 403, got %d: %s", rec.Code, rec.Body.String())
	}

	rec2 := doRequestWithAgentToken(t, srv, http.MethodGet, "/api/v1/agents/"+agentID+"/secrets/NOMEM_KEY", nil, token)
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("P8: expected 403, got %d: %s", rec2.Code, rec2.Body.String())
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

// TestAgentSecretRead_UnrelatedScheduledEventGrantDoesNotAdmit covers check
// 5: a project-scoped role binding that is not one of the built-in
// membership roles (owner/admin/member) does not admit, no matter what
// permissions it carries.
func TestAgentSecretRead_UnrelatedScheduledEventGrantDoesNotAdmit(t *testing.T) {
	f := newMaterialFixture(t, "unrelated-grant")
	ctx := context.Background()

	// Remove the fixture's own owner membership so only the unrelated grant
	// remains.
	bindings, err := f.Store.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, f.UserID)
	require.NoError(t, err)
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == f.ProjectID {
			require.NoError(t, f.Store.DeleteRoleBinding(ctx, b.ID))
		}
	}

	rd, err := f.Store.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        "scheduled-event-editor-" + f.UserID,
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
	_, reason, status := f.Server.materialRuntimePrecheck(ctx, ident)
	if status != http.StatusForbidden || reason != ReasonMembershipRequired {
		t.Fatalf("expected 403/%s, got %d/%s", ReasonMembershipRequired, status, reason)
	}
}

// TestAgentSecretRead_SystemRoleWithoutMembershipDeniedInterim documents the
// interim rule: a super-admin or hub-admin root without a project membership
// row is denied. System authority for the exact permission arrives in F.2b
// (OQ-16).
func TestAgentSecretRead_SystemRoleWithoutMembershipDeniedInterim(t *testing.T) {
	for _, roleName := range []string{store.SystemRoleSuperAdmin, store.SystemRoleHubAdmin} {
		t.Run(roleName, func(t *testing.T) {
			srv, s := testServer(t)
			srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))
			ctx := context.Background()

			projectID := tid("project-sysrole-" + roleName)
			require.NoError(t, s.CreateProject(ctx, &store.Project{
				ID: projectID, Name: "p", Slug: "p-sysrole-" + roleName, Created: time.Now(), Updated: time.Now(),
			}))
			userID := tid("user-sysrole-" + roleName)
			createTestUserWithRole(t, s, userID, roleName+"@test.com", "member", roleName)
			agentID := tid("agent-sysrole-" + roleName)
			require.NoError(t, s.CreateAgent(ctx, &store.Agent{
				ID: agentID, Slug: "a-sysrole-" + roleName, Name: "a", ProjectID: projectID,
				Phase: string(state.PhaseRunning), StateVersion: 1, Ancestry: []string{userID},
				Created: time.Now(), Updated: time.Now(),
			}))

			ident := newFullAgentIdentity(agentID, projectID, []string{userID}, []AgentTokenScope{ScopeProjectSecretRead})
			_, reason, status := srv.materialRuntimePrecheck(ctx, ident)
			if status != http.StatusForbidden || reason != ReasonMembershipRequired {
				t.Fatalf("expected 403/%s, got %d/%s", ReasonMembershipRequired, status, reason)
			}
		})
	}
}

// TestAgentSecretRead_MembershipLookupErrorDenies covers check 5: a genuine
// store fault while checking membership (as opposed to a definite
// non-member) fails closed with a 500, not a 403.
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
