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

// Package hub — tests for the project-scope per-item check (check 7): the
// single per-request project.secret_read decision, the existing delegation
// ceiling it inherits, and the neutrality/ordering regressions pinned
// alongside it.
package hub

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"context"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	"github.com/stretchr/testify/require"
)

// assertProjectDenied issues both a fetch (multi-key POST) and a get
// (by-key GET) for key against agentID/token and asserts the full check-7
// deny contract: not_found with no value on both endpoints, no backend
// value read, and an audited denied_by_policy reason with a non-empty
// Detail.
func assertProjectDenied(t *testing.T, f *materialFixture, agentID, token, key string) {
	t.Helper()

	counting := &countingSecretBackend{SecretBackend: f.Server.secretBackend}
	f.Server.SetSecretBackend(counting)

	rec := newRecordingMaterialAuditor()
	f.Server.SetAuditLogger(rec)

	fetchRec := doRequestWithAgentToken(t, f.Server, http.MethodPost, "/api/v1/agent/secrets",
		secretFetchRequest{Keys: []string{key}}, token)
	if fetchRec.Code != http.StatusOK {
		t.Fatalf("fetch: expected 200 (per-item status, not a request-level error), got %d: %s", fetchRec.Code, fetchRec.Body.String())
	}
	var fetchResp secretFetchResponse
	require.NoError(t, json.NewDecoder(fetchRec.Body).Decode(&fetchResp))
	if len(fetchResp.Secrets) != 1 || fetchResp.Secrets[0].Status != "not_found" ||
		fetchResp.Secrets[0].Value != "" || fetchResp.Secrets[0].Error != "secret not found" {
		t.Fatalf("fetch: expected not_found/secret not found with no value, got %+v", fetchResp.Secrets)
	}

	getRec := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+agentID+"/secrets/"+key, nil, token)
	if getRec.Code != http.StatusNotFound {
		t.Fatalf("get: expected 404, got %d: %s", getRec.Code, getRec.Body.String())
	}

	if counting.getCalls != 0 {
		t.Fatalf("expected no value read (Get) on a denied item, got %d Get calls", counting.getCalls)
	}

	if len(rec.events) != 2 {
		t.Fatalf("expected 2 material selection events (fetch and get), got %d", len(rec.events))
	}
	for _, e := range rec.events {
		if len(e.Items) != 1 {
			t.Fatalf("expected 1 item per event, got %d", len(e.Items))
		}
		item := e.Items[0]
		if item.Reason != ReasonDeniedByPolicy {
			t.Fatalf("expected reason %s, got %s", ReasonDeniedByPolicy, item.Reason)
		}
		if item.Detail == "" {
			t.Fatalf("expected a non-empty Detail carrying Decision.Reason verbatim")
		}
	}
}

// TestAgentSecretFetch_OtherProjectKeyNotReturned covers a project-scope key
// that exists only in a different project: not_found, never a value.
func TestAgentSecretFetch_OtherProjectKeyNotReturned(t *testing.T) {
	f := newMaterialFixture(t, "other-project-key")
	ctx := context.Background()

	otherProjectID := tid("other-project-key-target")
	require.NoError(t, f.Store.CreateProject(ctx, &store.Project{
		ID: otherProjectID, Name: "other", Slug: "other-project-key-target", Created: time.Now(), Updated: time.Now(),
	}))
	seedSecret(t, f.Server.secretBackend, "OTHER_PROJECT_KEY", "v", "", "", otherProjectID)

	rec := doRequestWithAgentToken(t, f.Server, http.MethodPost, "/api/v1/agent/secrets",
		secretFetchRequest{Keys: []string{"OTHER_PROJECT_KEY"}}, f.Token)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp secretFetchResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	if len(resp.Secrets) != 1 || resp.Secrets[0].Status != "not_found" || resp.Secrets[0].Value != "" {
		t.Fatalf("expected not_found with no value, got %+v", resp.Secrets)
	}
}

// TestMaterialRuntime_ProjectSecretReadActionMatchesRegistry pins that
// actionProjectSecretRead is named after the project.secret_read
// registry row, not after any effect, so a reader cannot mistake it for a
// purpose-built action.
func TestMaterialRuntime_ProjectSecretReadActionMatchesRegistry(t *testing.T) {
	for _, row := range permissions.Registry {
		if row.ID == "project.secret_read" {
			if string(actionProjectSecretRead) != row.Action {
				t.Fatalf("actionProjectSecretRead = %q, want registry row action %q", actionProjectSecretRead, row.Action)
			}
			return
		}
	}
	t.Fatal("project.secret_read row not found in permissions.Registry")
}

// TestAgentSecretRead_ProjectScopeCeilingLookupFailureDenies pins that a
// failing delegation-edge store denies (fail-closed) rather than allowing.
func TestAgentSecretRead_ProjectScopeCeilingLookupFailureDenies(t *testing.T) {
	f := newMaterialFixture(t, "ceiling-lookup-fail")
	setBackfillCompleted(t, f.Store)
	seedSecret(t, f.Server.secretBackend, "CEILING_KEY", "v", "", "", f.ProjectID)

	f.Server.authzService = NewAuthzService(&materialFailingStore{
		Store:                            f.Store,
		getDelegationEdgesForDelegateErr: errors.New("injected edge lookup failure"),
	}, logging.Subsystem("hub.auth"))

	assertProjectDenied(t, f, f.AgentID, f.Token, "CEILING_KEY")
}

// TestAgentSecretRead_ProjectScopeMissingEdgeAfterBackfillDenies pins that
// once the backfill marker is set, a hub-attested agent with no
// recorded edge denies for secret_read (not read-only-classified).
func TestAgentSecretRead_ProjectScopeMissingEdgeAfterBackfillDenies(t *testing.T) {
	f := newMaterialFixture(t, "missing-edge-postbf")
	setBackfillCompleted(t, f.Store)
	seedSecret(t, f.Server.secretBackend, "POSTBF_KEY", "v", "", "", f.ProjectID)

	assertProjectDenied(t, f, f.AgentID, f.Token, "POSTBF_KEY")
}

// TestAgentSecretRead_ProjectScopeDuplicateActiveEdgesDenies pins the
// application-level safety net (check 7e): the partial unique index on
// (delegate_type, delegate_id, scope_type, scope_id) WHERE active=true makes
// a genuine duplicate unreachable through normal store writes (see
// TestDelegationCeiling_DuplicateEdgesFailClosed), so this test injects the
// invariant violation directly through a fake edge store to prove the code
// still fails closed.
func TestAgentSecretRead_ProjectScopeDuplicateActiveEdgesDenies(t *testing.T) {
	f := newMaterialFixture(t, "duplicate-edges")
	setBackfillCompleted(t, f.Store)
	seedSecret(t, f.Server.secretBackend, "DUP_KEY", "v", "", "", f.ProjectID)

	f.Server.authzService = NewAuthzService(&materialFailingStore{
		Store: f.Store,
		delegationEdgesOverride: []*store.DelegationEdge{
			{ID: "dup-edge-1", DelegatorType: store.DelegationPrincipalUser, DelegatorID: f.UserID,
				DelegateType: store.DelegationPrincipalAgent, DelegateID: f.AgentID,
				ScopeType: store.RoleScopeProject, ScopeID: f.ProjectID, Role: string(AgentRoleFull), Active: true},
			{ID: "dup-edge-2", DelegatorType: store.DelegationPrincipalUser, DelegatorID: f.UserID,
				DelegateType: store.DelegationPrincipalAgent, DelegateID: f.AgentID,
				ScopeType: store.RoleScopeProject, ScopeID: f.ProjectID, Role: string(AgentRoleFull), Active: true},
		},
	}, logging.Subsystem("hub.auth"))

	assertProjectDenied(t, f, f.AgentID, f.Token, "DUP_KEY")
}

// TestAgentSecretRead_ProjectScopeRequiresDelegatorProjectSecretRead pins
// that a user delegator without project.secret_read denies, while an
// owner/admin delegator is ok.
func TestAgentSecretRead_ProjectScopeRequiresDelegatorProjectSecretRead(t *testing.T) {
	f := newMaterialFixture(t, "delegator-permission")
	ctx := context.Background()
	setBackfillCompleted(t, f.Store)
	seedSecret(t, f.Server.secretBackend, "DELEGATOR_KEY", "v", "", "", f.ProjectID)

	plainDelegator := tid("plain-delegator")
	createDCUser(t, f.Store, plainDelegator, "plain-delegator@test.com", f.ProjectID, store.ProjectRoleMember)
	plainAgentID := tid("plain-delegate-agent")
	require.NoError(t, f.Store.CreateAgent(ctx, &store.Agent{
		ID: plainAgentID, Slug: "plain-delegate", Name: "plain", ProjectID: f.ProjectID,
		Phase: string(state.PhaseRunning), StateVersion: 1, Ancestry: []string{plainDelegator},
		Created: time.Now(), Updated: time.Now(),
	}))
	seedRecordedDelegationEdge(t, f.Store, store.DelegationPrincipalUser, plainDelegator, store.DelegationPrincipalAgent, plainAgentID,
		store.RoleScopeProject, f.ProjectID, string(AgentRoleFull))
	plainToken, err := f.Server.agentTokenService.GenerateAgentToken(plainAgentID, f.ProjectID, []AgentTokenScope{ScopeProjectSecretRead}, []string{plainDelegator})
	require.NoError(t, err)

	assertProjectDenied(t, f, plainAgentID, plainToken, "DELEGATOR_KEY")

	ownerDelegator := tid("owner-delegator")
	createDCUser(t, f.Store, ownerDelegator, "owner-delegator@test.com", f.ProjectID, store.ProjectRoleOwner)
	ownerAgentID := tid("owner-delegate-agent")
	require.NoError(t, f.Store.CreateAgent(ctx, &store.Agent{
		ID: ownerAgentID, Slug: "owner-delegate", Name: "owner", ProjectID: f.ProjectID,
		Phase: string(state.PhaseRunning), StateVersion: 1, Ancestry: []string{ownerDelegator},
		Created: time.Now(), Updated: time.Now(),
	}))
	seedRecordedDelegationEdge(t, f.Store, store.DelegationPrincipalUser, ownerDelegator, store.DelegationPrincipalAgent, ownerAgentID,
		store.RoleScopeProject, f.ProjectID, string(AgentRoleFull))
	ownerToken, err := f.Server.agentTokenService.GenerateAgentToken(ownerAgentID, f.ProjectID, []AgentTokenScope{ScopeProjectSecretRead}, []string{ownerDelegator})
	require.NoError(t, err)

	rec2 := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+ownerAgentID+"/secrets/DELEGATOR_KEY", nil, ownerToken)
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200 for an owner/admin delegator, got %d: %s", rec2.Code, rec2.Body.String())
	}
}

// customOnlyDelegateAgent creates a user whose only binding in the fixture
// project is a custom project role with the given permissions, plus a
// running agent rooted at that user with a recorded full-role delegation
// edge. It returns the user ID, the agent ID, and a token carrying
// ScopeProjectSecretRead.
func customOnlyDelegateAgent(t *testing.T, f *materialFixture, name string, permissions []string) (string, string, string) {
	t.Helper()
	ctx := context.Background()

	userID := tid("custom-only-" + name)
	require.NoError(t, f.Store.CreateUser(ctx, &store.User{
		ID: userID, Email: "custom-only-" + name + "@test.com", DisplayName: name, Role: "member", Status: store.UserStatusActive,
	}))
	rd, err := f.Store.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        "custom-only-" + name,
		ScopeType:   store.RoleScopeProject,
		Permissions: permissions,
	})
	require.NoError(t, err)
	_, err = f.Store.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          f.ProjectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	m := f.Server.CheckEffectiveMembership(ctx, userID, f.ProjectID)
	require.NoError(t, m.Err)
	require.True(t, m.IsMember, "custom-only binding must count as project membership")
	require.Empty(t, m.Role, "Role reports only the built-in tier")

	agentID := tid("custom-only-agent-" + name)
	require.NoError(t, f.Store.CreateAgent(ctx, &store.Agent{
		ID: agentID, Slug: "custom-only-" + name, Name: name, ProjectID: f.ProjectID,
		Phase: string(state.PhaseRunning), StateVersion: 1, Ancestry: []string{userID},
		Created: time.Now(), Updated: time.Now(),
	}))
	seedRecordedDelegationEdge(t, f.Store, store.DelegationPrincipalUser, userID, store.DelegationPrincipalAgent, agentID,
		store.RoleScopeProject, f.ProjectID, string(AgentRoleFull))
	token, err := f.Server.agentTokenService.GenerateAgentToken(agentID, f.ProjectID, []AgentTokenScope{ScopeProjectSecretRead}, []string{userID})
	require.NoError(t, err)
	return userID, agentID, token
}

// TestAgentSecretRead_CustomOnlyMemberWithSecretReadAllowed pins that a root
// user whose only project binding is a custom role carrying
// project.secret_read passes check 5 (membership) and check 7, so the agent
// reads the project secret.
func TestAgentSecretRead_CustomOnlyMemberWithSecretReadAllowed(t *testing.T) {
	f := newMaterialFixture(t, "custom-only-with-read")
	setBackfillCompleted(t, f.Store)
	seedSecret(t, f.Server.secretBackend, "CUSTOM_READ_KEY", "v", "", "", f.ProjectID)

	_, agentID, token := customOnlyDelegateAgent(t, f, "with-read", []string{"project.read", "project.secret_read"})

	rec := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+agentID+"/secrets/CUSTOM_READ_KEY", nil, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for a custom-only member holding project.secret_read, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAgentSecretRead_CustomOnlyMemberWithoutSecretReadDeniedAtCheck7 pins
// that membership is admission only: a custom-only member whose role lacks
// project.secret_read passes check 5 and is refused at check 7
// (denied_by_policy per item), not with membership_required.
func TestAgentSecretRead_CustomOnlyMemberWithoutSecretReadDeniedAtCheck7(t *testing.T) {
	f := newMaterialFixture(t, "custom-only-no-read")
	ctx := context.Background()
	setBackfillCompleted(t, f.Store)
	seedSecret(t, f.Server.secretBackend, "CUSTOM_NOREAD_KEY", "v", "", "", f.ProjectID)

	userID, agentID, token := customOnlyDelegateAgent(t, f, "no-read", []string{"project.read", "agent.read"})

	ident := newFullAgentIdentity(agentID, f.ProjectID, []string{userID}, []AgentTokenScope{ScopeProjectSecretRead})
	facts, reason, status := f.Server.materialRuntimePrecheck(ctx, ident)
	if status != 0 || reason != ReasonAllowed || facts == nil {
		t.Fatalf("expected check 5 to admit the custom-only member, got %d/%s", status, reason)
	}

	assertProjectDenied(t, f, agentID, token, "CUSTOM_NOREAD_KEY")
}

// TestAgentSecretRead_ProjectScopeAgentDelegatorWithoutSecretReadDenies
// pins that the parent agent's stored role must itself hold
// project:secret:read.
func TestAgentSecretRead_ProjectScopeAgentDelegatorWithoutSecretReadDenies(t *testing.T) {
	f := newMaterialFixture(t, "agent-delegator-no-secret-read")
	ctx := context.Background()
	setBackfillCompleted(t, f.Store)
	seedSecret(t, f.Server.secretBackend, "AGENT_DELEGATOR_KEY", "v", "", "", f.ProjectID)

	parentID := tid("parent-baseline")
	createDCAgent(t, f.Store, parentID, f.ProjectID, f.UserID, AgentRoleBaseline)
	createDCEdge(t, f.Store, store.DelegationPrincipalUser, f.UserID, store.DelegationPrincipalAgent, parentID,
		store.RoleScopeProject, f.ProjectID, string(AgentRoleBaseline))

	childID := tid("child-of-baseline-parent")
	require.NoError(t, f.Store.CreateAgent(ctx, &store.Agent{
		ID: childID, Slug: "child-of-baseline", Name: "child", ProjectID: f.ProjectID,
		Phase: string(state.PhaseRunning), StateVersion: 1, Ancestry: []string{f.UserID, parentID},
		Created: time.Now(), Updated: time.Now(),
	}))
	createDCEdge(t, f.Store, store.DelegationPrincipalAgent, parentID, store.DelegationPrincipalAgent, childID,
		store.RoleScopeProject, f.ProjectID, string(AgentRoleFull))
	childToken, err := f.Server.agentTokenService.GenerateAgentToken(childID, f.ProjectID, []AgentTokenScope{ScopeProjectSecretRead}, []string{f.UserID, parentID})
	require.NoError(t, err)

	assertProjectDenied(t, f, childID, childToken, "AGENT_DELEGATOR_KEY")
}

// TestAgentSecretRead_ProjectScopeAgentDelegatorWithSecretReadAllowed pins
// the positive case: a full parent with an owner-delegated edge is ok.
func TestAgentSecretRead_ProjectScopeAgentDelegatorWithSecretReadAllowed(t *testing.T) {
	f := newMaterialFixture(t, "agent-delegator-full")
	ctx := context.Background()
	setBackfillCompleted(t, f.Store)
	seedSecret(t, f.Server.secretBackend, "AGENT_DELEGATOR_FULL_KEY", "v", "", "", f.ProjectID)

	parentID := tid("parent-full")
	createDCAgent(t, f.Store, parentID, f.ProjectID, f.UserID, AgentRoleFull)
	seedRecordedDelegationEdge(t, f.Store, store.DelegationPrincipalUser, f.UserID, store.DelegationPrincipalAgent, parentID,
		store.RoleScopeProject, f.ProjectID, string(AgentRoleFull))

	childID := tid("child-of-full-parent")
	require.NoError(t, f.Store.CreateAgent(ctx, &store.Agent{
		ID: childID, Slug: "child-of-full", Name: "child", ProjectID: f.ProjectID,
		Phase: string(state.PhaseRunning), StateVersion: 1, Ancestry: []string{f.UserID, parentID},
		Created: time.Now(), Updated: time.Now(),
	}))
	seedRecordedDelegationEdge(t, f.Store, store.DelegationPrincipalAgent, parentID, store.DelegationPrincipalAgent, childID,
		store.RoleScopeProject, f.ProjectID, string(AgentRoleFull))
	childToken, err := f.Server.agentTokenService.GenerateAgentToken(childID, f.ProjectID, []AgentTokenScope{ScopeProjectSecretRead}, []string{f.UserID, parentID})
	require.NoError(t, err)

	rec := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+childID+"/secrets/AGENT_DELEGATOR_FULL_KEY", nil, childToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (full parent with owner-delegated edge), got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAgentSecretRead_ProjectScopeDeletedDelegatingUserDenies has two
// subtests: the root-user-is-the-delegator case denies at check 5, and the
// different-delegator case denies through the ceiling's orphaned-delegation
// branch.
func TestAgentSecretRead_ProjectScopeDeletedDelegatingUserDenies(t *testing.T) {
	t.Run("root_delegator_deleted", func(t *testing.T) {
		f := newMaterialFixture(t, "deleted-root-delegator")
		ctx := context.Background()
		require.NoError(t, f.Store.DeleteUser(ctx, f.UserID))

		ident := newFullAgentIdentity(f.AgentID, f.ProjectID, []string{f.UserID}, []AgentTokenScope{ScopeProjectSecretRead})
		_, reason, status := f.Server.materialRuntimePrecheck(ctx, ident)
		if status != http.StatusForbidden || reason != ReasonTargetUnresolved {
			t.Fatalf("expected 403/%s, got %d/%s", ReasonTargetUnresolved, status, reason)
		}
	})

	t.Run("edge_delegator_deleted_root_retained", func(t *testing.T) {
		f := newMaterialFixture(t, "deleted-edge-delegator")
		ctx := context.Background()
		setBackfillCompleted(t, f.Store)
		seedSecret(t, f.Server.secretBackend, "DEL_EDGE_KEY", "v", "", "", f.ProjectID)

		// Fixture: the delegator holds project.secret_read through a
		// project owner binding and has no system role binding, so it
		// cannot pass IsSystemAdmin before GetUser sees it is deleted.
		uDeleg := tid("edge-delegator-to-delete")
		createDCUser(t, f.Store, uDeleg, "edge-delegator-to-delete@test.com", f.ProjectID, store.ProjectRoleOwner)
		createDCEdge(t, f.Store, store.DelegationPrincipalUser, uDeleg, store.DelegationPrincipalAgent, f.AgentID,
			store.RoleScopeProject, f.ProjectID, string(AgentRoleFull))

		require.NoError(t, f.Store.DeleteUser(ctx, uDeleg))

		assertProjectDenied(t, f, f.AgentID, f.Token, "DEL_EDGE_KEY")
	})
}

// TestAgentSecretRead_ProjectScopeDeletedParentAgentDenies pins that a
// hard-deleted (purged) parent agent denies, both before and after the
// shared delegation-chain change.
func TestAgentSecretRead_ProjectScopeDeletedParentAgentDenies(t *testing.T) {
	f := newMaterialFixture(t, "deleted-parent-agent")
	ctx := context.Background()
	setBackfillCompleted(t, f.Store)
	seedSecret(t, f.Server.secretBackend, "DEL_PARENT_KEY", "v", "", "", f.ProjectID)

	parentID := tid("parent-to-delete")
	createDCAgent(t, f.Store, parentID, f.ProjectID, f.UserID, AgentRoleFull)
	createDCEdge(t, f.Store, store.DelegationPrincipalUser, f.UserID, store.DelegationPrincipalAgent, parentID,
		store.RoleScopeProject, f.ProjectID, string(AgentRoleFull))

	childID := tid("child-of-deleted-parent")
	require.NoError(t, f.Store.CreateAgent(ctx, &store.Agent{
		ID: childID, Slug: "child-deleted-parent", Name: "child", ProjectID: f.ProjectID,
		Phase: string(state.PhaseRunning), StateVersion: 1, Ancestry: []string{f.UserID, parentID},
		Created: time.Now(), Updated: time.Now(),
	}))
	createDCEdge(t, f.Store, store.DelegationPrincipalAgent, parentID, store.DelegationPrincipalAgent, childID,
		store.RoleScopeProject, f.ProjectID, string(AgentRoleFull))
	childToken, err := f.Server.agentTokenService.GenerateAgentToken(childID, f.ProjectID, []AgentTokenScope{ScopeProjectSecretRead}, []string{f.UserID, parentID})
	require.NoError(t, err)

	require.NoError(t, f.Store.DeleteAgent(ctx, parentID))

	assertProjectDenied(t, f, childID, childToken, "DEL_PARENT_KEY")
}

// TestAgentSecretRead_ProjectScopeUnresolvableMigrationDelegatorDenies pins
// that the synthetic system/migration principal never stands in for a real
// delegator: there is no separate admission path for it.
func TestAgentSecretRead_ProjectScopeUnresolvableMigrationDelegatorDenies(t *testing.T) {
	f := newMaterialFixture(t, "migration-delegator")
	setBackfillCompleted(t, f.Store)
	seedSecret(t, f.Server.secretBackend, "MIGRATION_KEY", "v", "", "", f.ProjectID)

	createDCEdge(t, f.Store, store.DelegationPrincipalUser, "system/migration", store.DelegationPrincipalAgent, f.AgentID,
		store.RoleScopeProject, f.ProjectID, string(AgentRoleFull))

	assertProjectDenied(t, f, f.AgentID, f.Token, "MIGRATION_KEY")
}

// TestAgentSecretRead_ProjectScopeStoppedParentRetainsAuthority pins that a
// stopped-but-not-deleted parent is allowed at its stored role: stopping a
// source is not revocation. This stays green after a follow-up change lands
// the shared non-deleted-source rule, since the parent here is never
// deleted.
func TestAgentSecretRead_ProjectScopeStoppedParentRetainsAuthority(t *testing.T) {
	f := newMaterialFixture(t, "stopped-parent")
	ctx := context.Background()
	setBackfillCompleted(t, f.Store)
	seedSecret(t, f.Server.secretBackend, "STOPPED_PARENT_KEY", "v", "", "", f.ProjectID)

	parentID := tid("stopped-parent-agent")
	createDCAgent(t, f.Store, parentID, f.ProjectID, f.UserID, AgentRoleFull)
	seedRecordedDelegationEdge(t, f.Store, store.DelegationPrincipalUser, f.UserID, store.DelegationPrincipalAgent, parentID,
		store.RoleScopeProject, f.ProjectID, string(AgentRoleFull))

	childID := tid("child-of-stopped-parent")
	require.NoError(t, f.Store.CreateAgent(ctx, &store.Agent{
		ID: childID, Slug: "child-stopped-parent", Name: "child", ProjectID: f.ProjectID,
		Phase: string(state.PhaseRunning), StateVersion: 1, Ancestry: []string{f.UserID, parentID},
		Created: time.Now(), Updated: time.Now(),
	}))
	seedRecordedDelegationEdge(t, f.Store, store.DelegationPrincipalAgent, parentID, store.DelegationPrincipalAgent, childID,
		store.RoleScopeProject, f.ProjectID, string(AgentRoleFull))
	childToken, err := f.Server.agentTokenService.GenerateAgentToken(childID, f.ProjectID, []AgentTokenScope{ScopeProjectSecretRead}, []string{f.UserID, parentID})
	require.NoError(t, err)

	parent, err := f.Store.GetAgent(ctx, parentID)
	require.NoError(t, err)
	parent.Phase = "stopped"
	require.NoError(t, f.Store.UpdateAgent(ctx, parent))

	rec := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+childID+"/secrets/STOPPED_PARENT_KEY", nil, childToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (stopping a source is not revocation), got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAgentSecretRead_ProjectScopeDeletedGrandparentAgentDenies pins that
// the recursive chain walk reaches a hard-deleted (purged) deeper ancestor
// and denies. This stays green after a follow-up change lands the shared
// non-deleted-source rule, since the grandparent here is hard-deleted, not
// soft-deleted.
func TestAgentSecretRead_ProjectScopeDeletedGrandparentAgentDenies(t *testing.T) {
	f := newMaterialFixture(t, "deleted-grandparent")
	ctx := context.Background()
	setBackfillCompleted(t, f.Store)
	seedSecret(t, f.Server.secretBackend, "DEL_GRANDPARENT_KEY", "v", "", "", f.ProjectID)

	grandparentID := tid("grandparent-to-delete")
	createDCAgent(t, f.Store, grandparentID, f.ProjectID, f.UserID, AgentRoleFull)
	createDCEdge(t, f.Store, store.DelegationPrincipalUser, f.UserID, store.DelegationPrincipalAgent, grandparentID,
		store.RoleScopeProject, f.ProjectID, string(AgentRoleFull))

	parentID := tid("parent-of-deleted-grandparent")
	require.NoError(t, f.Store.CreateAgent(ctx, &store.Agent{
		ID: parentID, Slug: "parent-gp-deleted", Name: "parent", ProjectID: f.ProjectID,
		Phase: string(state.PhaseRunning), StateVersion: 1, Ancestry: []string{f.UserID, grandparentID},
		AppliedConfig: &store.AgentAppliedConfig{AgentRole: string(AgentRoleFull)},
		Created:       time.Now(), Updated: time.Now(),
	}))
	createDCEdge(t, f.Store, store.DelegationPrincipalAgent, grandparentID, store.DelegationPrincipalAgent, parentID,
		store.RoleScopeProject, f.ProjectID, string(AgentRoleFull))

	childID := tid("child-of-parent-gp-deleted")
	require.NoError(t, f.Store.CreateAgent(ctx, &store.Agent{
		ID: childID, Slug: "child-gp-deleted", Name: "child", ProjectID: f.ProjectID,
		Phase: string(state.PhaseRunning), StateVersion: 1, Ancestry: []string{f.UserID, grandparentID, parentID},
		Created: time.Now(), Updated: time.Now(),
	}))
	createDCEdge(t, f.Store, store.DelegationPrincipalAgent, parentID, store.DelegationPrincipalAgent, childID,
		store.RoleScopeProject, f.ProjectID, string(AgentRoleFull))
	childToken, err := f.Server.agentTokenService.GenerateAgentToken(childID, f.ProjectID, []AgentTokenScope{ScopeProjectSecretRead}, []string{f.UserID, grandparentID, parentID})
	require.NoError(t, err)

	require.NoError(t, f.Store.DeleteAgent(ctx, grandparentID))

	assertProjectDenied(t, f, childID, childToken, "DEL_GRANDPARENT_KEY")
}

// TestAgentSecretFetch_ProjectDecisionEvaluatedOncePerRequest pins that
// with 100 keys, the delegation edge store is consulted exactly once.
func TestAgentSecretFetch_ProjectDecisionEvaluatedOncePerRequest(t *testing.T) {
	f := newMaterialFixture(t, "decision-once")

	counting := &callCountingStore{Store: f.Store}
	f.Server.authzService = NewAuthzService(counting, logging.Subsystem("hub.auth"))

	keys := make([]string, 100)
	for i := range keys {
		key := fmt.Sprintf("ONCE_KEY_%d", i)
		seedSecret(t, f.Server.secretBackend, key, "v", "", "", f.ProjectID)
		keys[i] = key
	}

	rec := doRequestWithAgentToken(t, f.Server, http.MethodPost, "/api/v1/agent/secrets", secretFetchRequest{Keys: keys}, f.Token)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if counting.getDelegationEdgesForDelegateCalls != 1 {
		t.Fatalf("expected the edge store to be consulted once, got %d calls", counting.getDelegationEdgesForDelegateCalls)
	}
}

// TestAgentSecretFetch_DeniedRequestReadsNoMetadata pins that a denied
// project decision makes no GetMeta call at all.
func TestAgentSecretFetch_DeniedRequestReadsNoMetadata(t *testing.T) {
	f := newMaterialFixture(t, "denied-no-metadata")
	setBackfillCompleted(t, f.Store) // no edge post-backfill -> ceiling denies

	counting := &countingSecretBackend{SecretBackend: f.Server.secretBackend}
	seedSecret(t, counting, "NO_META_KEY", "v", "", "", f.ProjectID)
	counting.getMetaCalls = 0
	f.Server.SetSecretBackend(counting)

	rec := doRequestWithAgentToken(t, f.Server, http.MethodPost, "/api/v1/agent/secrets",
		secretFetchRequest{Keys: []string{"NO_META_KEY"}}, f.Token)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp secretFetchResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	if len(resp.Secrets) != 1 || resp.Secrets[0].Status != "not_found" {
		t.Fatalf("expected not_found, got %+v", resp.Secrets)
	}
	if counting.getMetaCalls != 0 {
		t.Fatalf("expected no GetMeta call on a denied decision, got %d", counting.getMetaCalls)
	}
}

// TestAgentSecretFetch_ErrorTextIsNeutral pins that a backend error string
// never reaches the response, and that the item is audited as backend_error.
func TestAgentSecretFetch_ErrorTextIsNeutral(t *testing.T) {
	f := newMaterialFixture(t, "neutral-error-text")

	wrapped := &erroringMetaBackend{
		SecretBackend: f.Server.secretBackend,
		err:           errors.New("backend detail: disk quota exceeded on volume XYZ123"),
	}
	f.Server.SetSecretBackend(wrapped)

	auditor := newRecordingMaterialAuditor()
	f.Server.SetAuditLogger(auditor)

	rec := doRequestWithAgentToken(t, f.Server, http.MethodPost, "/api/v1/agent/secrets",
		secretFetchRequest{Keys: []string{"NEUTRAL_KEY"}}, f.Token)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "XYZ123") {
		t.Fatalf("backend error text leaked into response: %s", rec.Body.String())
	}
	var resp secretFetchResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	if len(resp.Secrets) != 1 || resp.Secrets[0].Status != "entitled_but_unavailable" || resp.Secrets[0].Error != "secret unavailable" {
		t.Fatalf("expected entitled_but_unavailable/secret unavailable, got %+v", resp.Secrets)
	}
	assertBackendErrorAudited(t, auditor)
}

// TestAgentGetSecret_ProjectMetaErrorIsUnavailable pins that a project-scope
// GetMeta backend error on the by-key get endpoint answers unavailable, not
// not found, with no backend error text in the response, and is audited as
// backend_error. Mirrors TestAgentSecretFetch_ErrorTextIsNeutral on the
// by-key get endpoint instead of the bulk fetch endpoint.
func TestAgentGetSecret_ProjectMetaErrorIsUnavailable(t *testing.T) {
	f := newMaterialFixture(t, "get-project-meta-error")
	seedSecret(t, f.Server.secretBackend, "GET_PROJECT_META_ERROR_KEY", "v", "", "", f.ProjectID)

	wrapped := &erroringMetaBackend{
		SecretBackend: f.Server.secretBackend,
		err:           errors.New("backend detail: disk quota exceeded on volume XYZ123"),
	}
	f.Server.SetSecretBackend(wrapped)

	auditor := newRecordingMaterialAuditor()
	f.Server.SetAuditLogger(auditor)

	rec := doRequestWithAgentToken(t, f.Server, http.MethodGet,
		"/api/v1/agents/"+f.AgentID+"/secrets/GET_PROJECT_META_ERROR_KEY", nil, f.Token)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "secret unavailable") {
		t.Fatalf("expected the fixed error message, got: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "XYZ123") {
		t.Fatalf("backend error text leaked into response: %s", rec.Body.String())
	}
	assertBackendErrorAudited(t, auditor)
}

// TestAgentSecretFetch_NilAuthzServiceDenies covers the whole-request nil or
// absent authz service guard: it fails the entire fetch request closed with
// 500, audited as a request-level backend_error with no items, rather than
// letting a per-item entitled_but_unavailable slip inside a 200.
func TestAgentSecretFetch_NilAuthzServiceDenies(t *testing.T) {
	f := newMaterialFixture(t, "fetch-nil-authz")
	seedSecret(t, f.Server.secretBackend, "NIL_AUTHZ_KEY", "v", "", "", f.ProjectID)
	f.Server.authzService = nil

	auditor := newRecordingMaterialAuditor()
	f.Server.SetAuditLogger(auditor)

	rec := doRequestWithAgentToken(t, f.Server, http.MethodPost, "/api/v1/agent/secrets",
		secretFetchRequest{Keys: []string{"NIL_AUTHZ_KEY"}}, f.Token)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(auditor.events) != 1 {
		t.Fatalf("expected 1 material selection event, got %d", len(auditor.events))
	}
	e := auditor.events[0]
	if e.RequestReason != ReasonBackendError {
		t.Fatalf("expected RequestReason %s, got %s", ReasonBackendError, e.RequestReason)
	}
	if len(e.Items) != 0 {
		t.Fatalf("expected no items, got %d", len(e.Items))
	}
}

// TestAgentSecretRead_ProjectScopeDenialRecordedAsPolicyDenial pins that a
// ceiling denial records denied_by_policy with a non-empty Detail carrying
// Decision.Reason verbatim, and that no code path inspects Decision.Reason
// to decide the caller-visible outcome.
func TestAgentSecretRead_ProjectScopeDenialRecordedAsPolicyDenial(t *testing.T) {
	f := newMaterialFixture(t, "policy-denial-detail")
	setBackfillCompleted(t, f.Store) // ceiling denial: no edge, post-backfill
	seedSecret(t, f.Server.secretBackend, "POLICY_DENIAL_KEY", "v", "", "", f.ProjectID)

	rec := newRecordingMaterialAuditor()
	f.Server.SetAuditLogger(rec)

	httpRec := doRequestWithAgentToken(t, f.Server, http.MethodGet, "/api/v1/agents/"+f.AgentID+"/secrets/POLICY_DENIAL_KEY", nil, f.Token)
	if httpRec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", httpRec.Code, httpRec.Body.String())
	}

	if len(rec.events) != 1 {
		t.Fatalf("expected 1 material selection event, got %d", len(rec.events))
	}
	if len(rec.events[0].Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(rec.events[0].Items))
	}
	item := rec.events[0].Items[0]
	if item.Reason != ReasonDeniedByPolicy {
		t.Fatalf("expected reason %s, got %s", ReasonDeniedByPolicy, item.Reason)
	}
	if item.Detail == "" {
		t.Fatalf("expected a non-empty Detail carrying Decision.Reason verbatim")
	}
}

// TestMaterialRuntime_AuthorizeProjectItemKernelDenialMapsToPolicyDenial unit-tests
// check 7 directly with a synthetic denied Decision, rather than relying on
// a fixture's ceiling shape to produce one: any kernel denial maps to
// denied_by_policy and carries Decision.Reason verbatim as the audit-only
// Detail.
func TestMaterialRuntime_AuthorizeProjectItemKernelDenialMapsToPolicyDenial(t *testing.T) {
	srv, _ := testServer(t)

	facts := &TargetFacts{ProjectID: "synthetic-project"}
	decision := Decision{Allowed: false, Reason: "synthetic kernel denial"}

	item, detail := srv.authorizeRuntimeProjectItem(context.Background(), "SYNTHETIC_KEY", facts, decision)
	if item.Allowed {
		t.Fatalf("expected the item to be denied")
	}
	if item.Reason != ReasonDeniedByPolicy {
		t.Fatalf("expected reason %s, got %s", ReasonDeniedByPolicy, item.Reason)
	}
	if item.Grant != GrantProjectSecretRead {
		t.Fatalf("expected grant %s, got %s", GrantProjectSecretRead, item.Grant)
	}
	if detail != decision.Reason {
		t.Fatalf("expected Detail %q (Decision.Reason verbatim), got %q", decision.Reason, detail)
	}
}
