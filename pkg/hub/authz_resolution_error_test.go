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
	"errors"
	"log/slog"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for DenyCauseResolutionError and Decision.IsIndeterminate(): the
// terminal-workspace GET handler must not prune an agent whose access check
// could not be decided (a store or resolution fault), only one that was
// denied by policy.
//
// Each wrapper below fails exactly one store call on the path decide() takes
// for a user principal requesting agent.attach: principal resolution
// (Step 2), role-binding resolution (Step 3), role-definition resolution
// (Step 4), and access-constraint load (Step 7c).

// failEffectiveGroupsStore fails GetEffectiveGroups (Step 2: principal
// resolution).
type failEffectiveGroupsStore struct {
	store.Store
	failErr error
}

func (s *failEffectiveGroupsStore) GetEffectiveGroups(ctx context.Context, userID string) ([]string, error) {
	return nil, s.failErr
}

// failBindingsStore fails ListRoleBindingsForPrincipals (Step 3: role-binding
// resolution).
type failBindingsStore struct {
	store.Store
	failErr error
}

func (s *failBindingsStore) ListRoleBindingsForPrincipals(ctx context.Context, principals []store.PrincipalRef, scopeTypes []string, scopeIDs []string) ([]*store.RoleBinding, error) {
	return nil, s.failErr
}

// failRoleDefsStore fails GetRoleDefinitionsByIDs (Step 4: role-definition
// resolution). It only fires when there is at least one role-definition ID to
// load, matching loadRoleDefinitions's short-circuit on an empty ID list.
type failRoleDefsStore struct {
	store.Store
	failErr error
}

func (s *failRoleDefsStore) GetRoleDefinitionsByIDs(ctx context.Context, ids []string) (map[string]*store.RoleDefinition, error) {
	return nil, s.failErr
}

// failConstraintsStore fails ListAccessConstraints (Step 7c: access-constraint
// load), while principal and binding resolution succeed normally.
type failConstraintsStore struct {
	store.Store
	failErr error
}

func (s *failConstraintsStore) ListAccessConstraints(ctx context.Context, limit, offset int) ([]*store.AccessConstraint, error) {
	return nil, s.failErr
}

func TestAuthz_IsIndeterminate_PrincipalResolutionError(t *testing.T) {
	_, s := authzTestSetup(t)
	ctx := context.Background()

	userID := tid("dbe-principal")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: "dbe-principal@test.com", DisplayName: "u", Role: "member", Status: "active",
	}))

	failing := &failEffectiveGroupsStore{Store: s, failErr: errors.New("injected principal resolution failure")}
	authz := NewAuthzService(failing, slog.Default())

	user := NewAuthenticatedUser(userID, "dbe-principal@test.com", "u", "member", "api")
	resource := Resource{Type: "agent", ID: tid("dbe-agent-1")}

	decision := authz.CheckAccess(ctx, user, resource, ActionAttach)
	assert.False(t, decision.Allowed)
	assert.Equal(t, DenyCauseResolutionError, decision.DenyCause)
	assert.True(t, decision.IsIndeterminate())
}

func TestAuthz_IsIndeterminate_BindingResolutionError(t *testing.T) {
	_, s := authzTestSetup(t)
	ctx := context.Background()

	userID := tid("dbe-binding")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: "dbe-binding@test.com", DisplayName: "u", Role: "member", Status: "active",
	}))

	failing := &failBindingsStore{Store: s, failErr: errors.New("injected binding resolution failure")}
	authz := NewAuthzService(failing, slog.Default())

	user := NewAuthenticatedUser(userID, "dbe-binding@test.com", "u", "member", "api")
	resource := Resource{Type: "agent", ID: tid("dbe-agent-2")}

	decision := authz.CheckAccess(ctx, user, resource, ActionAttach)
	assert.False(t, decision.Allowed)
	assert.Equal(t, DenyCauseResolutionError, decision.DenyCause)
	assert.True(t, decision.IsIndeterminate())
}

func TestAuthz_IsIndeterminate_RoleDefinitionResolutionError(t *testing.T) {
	_, s := authzTestSetup(t)
	ctx := context.Background()

	userID := tid("dbe-roledef")
	projectID := tid("dbe-roledef-proj")
	createDelegateTestProject(t, s, projectID, "dbe-roledef-proj", "test")
	// The user needs at least one role binding so loadRoleDefinitions is
	// actually called (it short-circuits on an empty ID list).
	createTestUserWithProjectRole(t, s, userID, "dbe-roledef@test.com", projectID, store.ProjectRoleMember)

	failing := &failRoleDefsStore{Store: s, failErr: errors.New("injected role-definition resolution failure")}
	authz := NewAuthzService(failing, slog.Default())

	user := NewAuthenticatedUser(userID, "dbe-roledef@test.com", "u", "member", "api")
	resource := Resource{Type: "agent", ID: tid("dbe-agent-3"), ParentType: "project", ParentID: projectID}

	decision := authz.CheckAccess(ctx, user, resource, ActionAttach)
	assert.False(t, decision.Allowed)
	assert.Equal(t, DenyCauseResolutionError, decision.DenyCause)
	assert.True(t, decision.IsIndeterminate())
}

// TestAuthz_IsIndeterminate_AccessConstraintLoadError proves the access-
// constraint-load failure (Step 7c) is tagged even when the kernel deny would
// otherwise have been overturned by an owner relationship candidate: the
// deny-all restriction from the failed load rejects every relationship
// candidate too (Stage 5 of runRelationshipStages), so the final decision is
// still a deny, now tagged DenyCauseResolutionError.
func TestAuthz_IsIndeterminate_AccessConstraintLoadError(t *testing.T) {
	_, s := authzTestSetup(t)
	ctx := context.Background()

	userID := tid("dbe-constraint")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: "dbe-constraint@test.com", DisplayName: "u", Role: "member", Status: "active",
	}))

	failing := &failConstraintsStore{Store: s, failErr: errors.New("injected access-constraint load failure")}
	authz := NewAuthzService(failing, slog.Default())

	user := NewAuthenticatedUser(userID, "dbe-constraint@test.com", "u", "member", "api")
	// Owned by the caller: absent the injected failure, the owner
	// relationship candidate would grant this (TestAuthz_OwnerBypass).
	resource := Resource{Type: "agent", ID: tid("dbe-agent-4"), OwnerID: userID}

	decision := authz.CheckAccess(ctx, user, resource, ActionAttach)
	assert.False(t, decision.Allowed, "the deny-all restriction from the failed constraint load must reject even the owner relationship candidate")
	assert.Equal(t, DenyCauseResolutionError, decision.DenyCause)
	assert.True(t, decision.IsIndeterminate())
	// Pins that the owner candidate was specifically rejected by Stage 5 of
	// runRelationshipStages (restrictedBy == "access_constraint_error"), not
	// denied for some other reason that happens to also set this DenyCause.
	assert.Equal(t, "relationship grant restricted by access_constraint_error", decision.Reason)
}

// TestAuthz_AgentAttachPermissionResolves pins that a user principal
// requesting agent.attach resolves to exactly one permission, so a registry
// mistake would show up here as an "unresolvable permission" reason rather
// than silently changing which paths above can produce a resolution error.
func TestAuthz_AgentAttachPermissionResolves(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	userID := tid("attach-resolves")
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: "attach-resolves@test.com", DisplayName: "u", Role: "member", Status: "active",
	}))

	user := NewAuthenticatedUser(userID, "attach-resolves@test.com", "u", "member", "api")
	// Not owned by the caller and no role binding: an ordinary policy deny.
	resource := Resource{Type: "agent", ID: tid("attach-resolves-agent")}

	decision := authz.CheckAccess(ctx, user, resource, ActionAttach)
	assert.False(t, decision.Allowed)
	assert.NotEqual(t, unresolvablePermissionReason, decision.Reason)
	// An ordinary policy deny carries no DenyCause and is not IsIndeterminate.
	assert.Equal(t, DenyCause(""), decision.DenyCause)
	assert.False(t, decision.IsIndeterminate())
}

// TestDecision_IsIndeterminate_CeilingError proves the predicate also covers
// the pre-existing delegation-ceiling error cause.
func TestDecision_IsIndeterminate_CeilingError(t *testing.T) {
	d := Decision{Allowed: false, DenyCause: DenyCauseCeilingError}
	assert.True(t, d.IsIndeterminate())
}

// TestDecision_IsIndeterminate_AllowedIsNeverError proves IsIndeterminate is
// always false for an allowed decision, regardless of a stray DenyCause.
func TestDecision_IsIndeterminate_AllowedIsNeverError(t *testing.T) {
	d := Decision{Allowed: true, DenyCause: DenyCauseResolutionError}
	assert.False(t, d.IsIndeterminate())
}

// The delegation-ceiling policy causes are ordinary denies, not
// indeterminate ones; only DenyCauseCeilingError marks a ceiling fault.
func TestDecision_IsIndeterminate_CeilingPolicyCausesAreDenies(t *testing.T) {
	for _, c := range []DenyCause{
		DenyCauseCeilingUnrecorded,
		DenyCauseCeilingEffectExceeded,
		DenyCauseCeilingResourceMissing,
		DenyCauseCeilingSourceNotAllowed,
		DenyCauseCeilingOrphaned,
		DenyCauseCeilingDelegatorLacksPermission,
	} {
		assert.False(t, Decision{Allowed: false, DenyCause: c}.IsIndeterminate(), string(c))
	}
	assert.True(t, Decision{Allowed: false, DenyCause: DenyCauseCeilingError}.IsIndeterminate(),
		"control: a ceiling fault is indeterminate")
}
