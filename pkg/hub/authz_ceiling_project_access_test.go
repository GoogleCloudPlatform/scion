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

// Tests for the project-access stage on the delegation ceiling's user hop
// (ptone/scion#3433): a delegator's owner or ancestor relationship is
// honoured only while the delegator is admitted to the target's project.

import (
	"context"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func msUser(t *testing.T, f *msFixture, id string) *store.User {
	t.Helper()
	u, err := f.s.GetUser(context.Background(), id)
	require.NoError(t, err)
	return u
}

// Owner relationship on U's own agent: honoured while U is a member, refused
// after U's removal with no hold present.
func TestCeilingUserHop_RelationshipNeedsProjectAccess(t *testing.T) {
	f := newMSFixture(t, "hop-owner")
	ctx := context.Background()
	ok, _, err := f.srv.authzService.userRelationshipAuthority(ctx, msUser(t, f, f.userID), agentResource(f.agentA), ActionAttach, "agent.attach")
	require.NoError(t, err)
	require.True(t, ok, "member: the owner relationship is honoured")

	f.dropBindings(f.userID)
	require.False(t, f.held(f.agentA.ID))
	ok, reason, err := f.srv.authzService.userRelationshipAuthority(ctx, msUser(t, f, f.userID), agentResource(f.agentA), ActionAttach, "agent.attach")
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Contains(t, reason, RelationshipRejectProjectAccess)
}

// Ancestor relationship (U is in C's ancestry) is refused the same way.
func TestCeilingUserHop_AncestorNeedsProjectAccess(t *testing.T) {
	f := newMSFixture(t, "hop-ancestor")
	ctx := context.Background()
	ok, _, err := f.srv.authzService.userRelationshipAuthority(ctx, msUser(t, f, f.userID), agentResource(f.childC), ActionAttach, "agent.attach")
	require.NoError(t, err)
	require.True(t, ok, "member: the ancestor relationship is honoured")
	f.dropBindings(f.userID)
	ok, _, err = f.srv.authzService.userRelationshipAuthority(ctx, msUser(t, f, f.userID), agentResource(f.childC), ActionAttach, "agent.attach")
	require.NoError(t, err)
	assert.False(t, ok)
}

// A non-project target is unchanged by the stage.
func TestCeilingUserHop_NonProjectTargetUnchanged(t *testing.T) {
	f := newMSFixture(t, "hop-nonproject")
	ctx := context.Background()
	f.dropBindings(f.userID)
	res := Resource{Type: "agent", ID: f.agentA.ID, OwnerID: f.userID}
	ok, _, err := f.srv.authzService.userRelationshipAuthority(ctx, msUser(t, f, f.userID), res, ActionAttach, "agent.attach")
	require.NoError(t, err)
	assert.True(t, ok, "a target with no project is not gated by project access")
}

// System authority that applies to the project admits the delegator.
func TestCeilingUserHop_SystemAuthorityAdmitted(t *testing.T) {
	f := newMSFixture(t, "hop-system")
	ctx := context.Background()
	f.dropBindings(f.userID)
	rd, err := f.s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = f.s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: f.userID,
		ScopeType: store.RoleScopeSystem, CreatedBy: "test",
	})
	require.NoError(t, err)
	ok, _, err := f.srv.authzService.userRelationshipAuthority(ctx, msUser(t, f, f.userID), agentResource(f.agentA), ActionAttach, "agent.attach")
	require.NoError(t, err)
	assert.True(t, ok)
	require.NoError(t, f.srv.agentStanding(ctx, f.agentA.ID), "standing admits system authority too (Q5)")
}

// newFedHopFixture serves a federated-shaped user ID through the federated
// binding store, for the authorization service only.
func newFedHopFixture(t *testing.T, name string, withUsersRow bool) (*msFixture, *federatedBindingStore, string) {
	t.Helper()
	f := newMSFixture(t, name)
	fedID := fedPrincipalID("ceiling-" + name)
	fs := &federatedBindingStore{Store: f.srv.authzService.store, groups: map[string][]string{}, users: map[string]*store.User{}}
	if withUsersRow {
		fs.users[fedID] = &store.User{ID: fedID, Email: "fed@idp.example", Role: "member", Status: store.UserStatusActive}
	}
	orig := f.srv.authzService.store
	f.srv.authzService.store = fs
	t.Cleanup(func() { f.srv.authzService.store = orig })
	return f, fs, fedID
}

// Hunk-4 test 1: on the ceiling, a federated-shaped user ID that has a users
// row but no project binding is denied by the project-access stage, which
// evaluated it as a user principal.
func TestCeilingUserHop_FederatedUserNoProjectBinding_Denied(t *testing.T) {
	f, fs, fedID := newFedHopFixture(t, "fed-nobinding", true)
	ctx := context.Background()
	u, err := fs.GetUser(ctx, fedID)
	require.NoError(t, err)
	res := agentResource(f.agentA)
	res.OwnerID = fedID
	ok, reason, err := f.srv.authzService.userRelationshipAuthority(ctx, u, res, ActionAttach, "agent.attach")
	require.NoError(t, err)
	assert.False(t, ok)
	// The stage runs only for user principals (Kind=User): its rejection
	// kind in the reason shows it ran.
	assert.Contains(t, reason, RelationshipRejectProjectAccess)
}

// Hunk-4 test 2: the same ID with no users row is denied before stage 2c (the
// delegator is not live).
func TestCeilingUserHop_NoUsersRow_DeniedBeforeStage2c(t *testing.T) {
	f, _, fedID := newFedHopFixture(t, "fed-nousers", false)
	ctx := context.Background()
	res := agentResource(f.agentA)
	res.OwnerID = fedID
	ok, _, err := f.srv.authzService.evaluateUserDelegatorAuthority(ctx, fedID, res, ActionAttach, "agent.attach", store.RoleScopeProject, f.projectID)
	require.ErrorIs(t, err, store.ErrNotFound)
	assert.False(t, ok)
}

// Hunk-4 test 3: a project-access fault on the ceiling path is
// indeterminate: userRelationshipAuthority fails closed, the chain walk
// records DenyCauseResolutionError, and the public refusal is the same.
func TestCeilingUserHop_ProjectAccessFault_Indeterminate(t *testing.T) {
	f, fs, fedID := newFedHopFixture(t, "fed-fault", true)
	ctx := context.Background()
	fs.set(func(s *federatedBindingStore) { s.failBindings = true })
	u, err := fs.GetUser(ctx, fedID)
	require.NoError(t, err)
	res := agentResource(f.agentA)
	res.OwnerID = fedID
	ok, _, err := f.srv.authzService.userRelationshipAuthority(ctx, u, res, ActionAttach, "agent.attach")
	require.ErrorIs(t, err, errCeilingProjectAccessFault)
	assert.False(t, ok)

	// The chain walk maps the fault to the resolution cause, with no error
	// (the same refusal as any ceiling deny).
	ctx = contextWithDelegationCeilingCache(ctx)
	cache := getDelegationCeilingCache(ctx)
	req := agentCeilingRequest(f.agentA)
	key := f.userID + "|" + req.Resource.Type + "|" + req.Resource.ID + "|" + req.Resource.ParentType + "|" + req.Resource.ParentID + "|" + string(req.Action) + "|" + req.Permission + "|" + store.RoleScopeProject + "|" + f.projectID
	cache.authority[key] = delegatorAuthorityResult{allowed: false, reason: "relationship project access check failed (fail-closed)", err: errCeilingProjectAccessFault}
	var cause DenyCause
	allowed, _, cerr := f.srv.authzService.checkDelegationCeiling(ctx, req, req.Permission, f.agentA.ID, nil, &cause)
	require.NoError(t, cerr)
	assert.False(t, allowed)
	assert.Equal(t, DenyCauseResolutionError, cause)
}
