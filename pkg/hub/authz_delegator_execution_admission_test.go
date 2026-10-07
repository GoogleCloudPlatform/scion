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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// delegatorAdmissionFixture is an agent project and a user delegator who
// holds no role granting skill.read or agent.attach, so any authority on
// their own resources comes from the owner relationship.
type delegatorAdmissionFixture struct {
	authz     *AuthzService
	store     store.Store
	projectID string
	userID    string
}

func newDelegatorAdmissionFixture(t *testing.T, name string) delegatorAdmissionFixture {
	t.Helper()
	authz, s := authzTestSetup(t)
	f := delegatorAdmissionFixture{authz: authz, store: s, projectID: tid("dea-proj-" + name), userID: tid("dea-user-" + name)}
	createDCProject(t, s, f.projectID, "dea-"+name)
	require.NoError(t, s.CreateUser(context.Background(), &store.User{
		ID: f.userID, Email: "dea-" + name + "@test.com", DisplayName: "dea-" + name, Role: "member", Status: store.UserStatusActive,
	}))
	return f
}

// admitWithoutGrant gives the user project membership in projectID through
// a custom project role that holds only project.read.
func (f delegatorAdmissionFixture) admitWithoutGrant(t *testing.T, projectID string) {
	t.Helper()
	ctx := context.Background()
	rd, err := f.store.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name: "dea-reader-" + projectID[:8], ScopeType: store.RoleScopeProject, Permissions: []string{"project.read"},
	})
	require.NoError(t, err)
	_, err = f.store.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: f.userID,
		ScopeType: store.RoleScopeProject, ScopeID: projectID, CreatedBy: "test",
	})
	require.NoError(t, err)
}

func (f delegatorAdmissionFixture) personalSkill() Resource {
	r := skillScopeResource(store.SkillScopeUser, f.userID)
	r.ID = tid("dea-skill")
	r.OwnerID = f.userID
	return r
}

func (f delegatorAdmissionFixture) ownedAgent() Resource {
	return agentResource(&store.Agent{ID: tid("dea-target"), ProjectID: f.projectID, OwnerID: f.userID, CreatedBy: f.userID, Ancestry: []string{f.userID}})
}

func (f delegatorAdmissionFixture) evaluate(t *testing.T, res Resource, action Action, perm string) (bool, string) {
	t.Helper()
	ok, reason, err := f.authz.evaluateUserDelegatorAuthority(context.Background(), f.userID, res, action, perm, store.RoleScopeProject, f.projectID)
	require.NoError(t, err)
	return ok, reason
}

func TestRelationshipExecutionClassFollowsExecutionRules(t *testing.T) {
	for _, tc := range []struct {
		resource string
		perm     string
		want     bool
	}{
		{"skill", "skill.read", true},
		{"secret", "project.secret_read", true},
		{"secret", "secret.use", true},
		{"secret", "secret.deliver", true},
		{"env_var", "env_var.deliver", true},
		{"agent", "agent.attach", false},
		{"agent", "agent.read", false},
		{"broker", "broker.dispatch", false},
		{"gcp_service_account", "gcp_service_account.assign", false},
		{"skill", "skill.update", false},
	} {
		assert.Equal(t, tc.want, relationshipExecutionClass(Resource{Type: tc.resource}, tc.perm), "%s %s", tc.resource, tc.perm)
	}
	assert.True(t, executionProjectRule(RelationshipRuleProgeny))
	assert.False(t, executionProjectRule(RelationshipRuleOwner))
}

// A user delegator holding an execution-class permission through a
// relationship grant must be admitted to the agent's project.
func TestRelationshipDelegatorExecutionAdmission_Allowed(t *testing.T) {
	f := newDelegatorAdmissionFixture(t, "allowed")
	f.admitWithoutGrant(t, f.projectID)
	ok, reason := f.evaluate(t, f.personalSkill(), ActionRead, "skill.read")
	assert.True(t, ok, "reason %q", reason)
	assert.Contains(t, reason, "relationship grant")
}

func TestRelationshipDelegatorExecutionAdmission_DeniedCrossProject(t *testing.T) {
	f := newDelegatorAdmissionFixture(t, "cross")
	other := tid("dea-other-proj")
	createDCProject(t, f.store, other, "dea-other")
	f.admitWithoutGrant(t, other)
	ok, reason := f.evaluate(t, f.personalSkill(), ActionRead, "skill.read")
	assert.False(t, ok)
	assert.Contains(t, reason, "restricted by "+RelationshipRejectExecutionProject)
	assert.Contains(t, reason, "execution source user lacks admission to the agent's project")

	t.Run("non-project delegation scope", func(t *testing.T) {
		ok, reason, err := f.authz.evaluateUserDelegatorAuthority(context.Background(), f.userID, f.personalSkill(), ActionRead, "skill.read", store.RoleScopeSystem, "")
		require.NoError(t, err)
		assert.False(t, ok)
		assert.Contains(t, reason, "execution project does not match the agent's project")
	})
}

func TestRelationshipDelegatorExecutionAdmission_InactiveDelegator(t *testing.T) {
	f := newDelegatorAdmissionFixture(t, "inactive")
	f.admitWithoutGrant(t, f.projectID)
	setUserStatus(t, f.store, f.userID, store.UserStatusSuspended)
	ok, _ := f.evaluate(t, f.personalSkill(), ActionRead, "skill.read")
	assert.False(t, ok)

	// The shared admission step gives the execution-project stage's reason
	// for an inactive source.
	u, err := f.store.GetUser(context.Background(), f.userID)
	require.NoError(t, err)
	ok, reason, err := f.authz.delegatorExecutionAdmission(context.Background(), u, store.RoleScopeProject, f.projectID, "skill.read", "relationship grant: owner")
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Contains(t, reason, "execution source user is not active")
}

// A relationship permission outside the execution class is unchanged: the
// owner of an agent attaches to it without admission to its project.
func TestRelationshipDelegatorNonExecutionUnchanged(t *testing.T) {
	f := newDelegatorAdmissionFixture(t, "nonexec")
	ok, reason := f.evaluate(t, f.ownedAgent(), ActionAttach, "agent.attach")
	assert.True(t, ok, "reason %q", reason)
	assert.Contains(t, reason, "relationship grant")

	// Without admission the execution-class permission is refused for the
	// same delegator.
	ok, _ = f.evaluate(t, f.personalSkill(), ActionRead, "skill.read")
	assert.False(t, ok)
}

// A role grant is unaffected: a project owner holds skill.read through the
// role, whatever the relationship stage would say.
func TestRelationshipDelegatorRoleGrantUnaffected(t *testing.T) {
	f := newDelegatorAdmissionFixture(t, "role")
	createDCUser(t, f.store, f.userID, "", f.projectID, store.ProjectRoleOwner)
	ok, reason := f.evaluate(t, f.personalSkill(), ActionRead, "skill.read")
	assert.True(t, ok)
	assert.Equal(t, "role grant", reason)
}

// The admission lookup fails closed with an error.
func TestRelationshipDelegatorExecutionAdmission_LookupError(t *testing.T) {
	f := newDelegatorAdmissionFixture(t, "fault")
	u, err := f.store.GetUser(context.Background(), f.userID)
	require.NoError(t, err)
	ok, _, err := f.authz.delegatorExecutionAdmission(context.Background(), u, store.RoleScopeProject, "", "skill.read", "relationship grant: owner")
	assert.False(t, ok)
	assert.NoError(t, err, "an empty scope is a denial, not a lookup")
	ok, _, err = f.authz.sourceUserExecutionAdmission(context.Background(), u, "", "skill.read")
	assert.False(t, ok)
	assert.ErrorIs(t, err, ErrProjectAccessDenied)
}

// A source agent's user delegator who holds an execution-class permission on
// a record through the owner relationship, but has no admission to the
// agent's project, does not carry that permission down the chain.
func TestRelationshipDelegatorExecutionAdmission_SourceAgentDelegatorNotAdmitted(t *testing.T) {
	withTestProgenyPolicyRow(t, "template", "template.read")
	f := newGoldenFixture(t)
	ctx := context.Background()

	sourceUser := tid("dea-src-user")
	require.NoError(t, f.store.CreateUser(ctx, &store.User{ID: sourceUser, Email: "dea-src@golden.test", DisplayName: "dea-src", Role: "member", Status: store.UserStatusActive}))
	sourceAgent := tid("dea-src-agent")
	seedExecutionAgent(t, f.store, sourceAgent, f.projectAlpha.ID, []string{sourceUser}, []string{sourceUser})

	record := Resource{Type: "template", ID: tid("dea-template"), OwnerID: sourceUser}
	holds, _ := f.authz.relationshipSourceDelegationHolds(ctx, sourceAgent, record, "template.read")
	assert.False(t, holds)
	var steps []DecisionStep
	allowed, _, err := f.authz.walkDelegationChain(ctx, record, ActionRead, "template.read", sourceAgent, true, store.RoleScopeProject, f.projectAlpha.ID, &steps)
	require.NoError(t, err)
	assert.False(t, allowed)
	require.NotEmpty(t, steps)
	last := steps[len(steps)-1]
	assert.Equal(t, "delegation_ceiling_denied", last.Step)
	assert.Contains(t, last.Detail, "restricted by "+RelationshipRejectExecutionProject+": execution source user lacks admission to the agent's project")
}

var errAdmissionLookupFault = errors.New("admission lookup fault")

// admissionFaultStore counts the unscoped role-binding reads
// (ListRoleBindingsForPrincipals with no scope filters) and fails the
// failAt-th one when failAt is set.
type admissionFaultStore struct {
	store.Store
	calls, failAt int
}

func (s *admissionFaultStore) ListRoleBindingsForPrincipals(ctx context.Context, principals []store.PrincipalRef, scopeTypes, scopeIDs []string) ([]*store.RoleBinding, error) {
	if scopeTypes == nil && scopeIDs == nil {
		s.calls++
		if s.failAt > 0 && s.calls == s.failAt {
			return nil, errAdmissionLookupFault
		}
	}
	return s.Store.ListRoleBindingsForPrincipals(ctx, principals, scopeTypes, scopeIDs)
}

// installAdmissionFault puts an admissionFaultStore in front of authz's
// store, runs control (which must succeed) to count the unscoped
// role-binding reads, and arms the fault on the last of them. In an
// admitted control run that last read is the membership lookup of the
// delegator's execution-project admission, the final step of the decision.
func installAdmissionFault(t *testing.T, authz *AuthzService, control func()) {
	t.Helper()
	real := authz.store
	fs := &admissionFaultStore{Store: real}
	authz.store = fs
	t.Cleanup(func() { authz.store = real })
	control()
	require.Positive(t, fs.calls, "the control run reads role bindings")
	fs.failAt, fs.calls = fs.calls, 0
}

// A failed admission lookup inside evaluateUserDelegatorAuthority denies
// with a non-nil error; it is not treated as admitted.
func TestRelationshipDelegatorExecutionAdmission_LookupErrorFailsClosed(t *testing.T) {
	f := newDelegatorAdmissionFixture(t, "fault-gate")
	f.admitWithoutGrant(t, f.projectID)
	installAdmissionFault(t, f.authz, func() {
		ok, reason := f.evaluate(t, f.personalSkill(), ActionRead, "skill.read")
		require.True(t, ok, "control: admitted delegator holds skill.read, reason %q", reason)
	})

	ok, reason, err := f.authz.evaluateUserDelegatorAuthority(context.Background(), f.userID, f.personalSkill(), ActionRead, "skill.read", store.RoleScopeProject, f.projectID)
	assert.False(t, ok)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrProjectAccessDenied)
	assert.Contains(t, err.Error(), errAdmissionLookupFault.Error())
	assert.Equal(t, "execution project admission check failed", reason)
}

// The same admission lookup failure for a source agent's user delegator
// ends the delegation chain walk with a ceiling error.
func TestRelationshipDelegatorExecutionAdmission_LookupErrorInWalk(t *testing.T) {
	withTestProgenyPolicyRow(t, "template", "template.read")
	f := newGoldenFixture(t)
	ctx := context.Background()

	sourceUser := tid("dea-walk-user")
	require.NoError(t, f.store.CreateUser(ctx, &store.User{ID: sourceUser, Email: "dea-walk@golden.test", DisplayName: "dea-walk", Role: "member", Status: store.UserStatusActive}))
	delegatorAdmissionFixture{store: f.store, userID: sourceUser}.admitWithoutGrant(t, f.projectAlpha.ID)
	sourceAgent := tid("dea-walk-agent")
	seedExecutionAgent(t, f.store, sourceAgent, f.projectAlpha.ID, []string{sourceUser}, []string{sourceUser})
	record := Resource{Type: "template", ID: tid("dea-walk-template"), OwnerID: sourceUser}

	walk := func() (bool, []DecisionStep, error) {
		var steps []DecisionStep
		allowed, _, err := f.authz.walkDelegationChain(context.Background(), record, ActionRead, "template.read", sourceAgent, true, store.RoleScopeProject, f.projectAlpha.ID, &steps)
		return allowed, steps, err
	}
	installAdmissionFault(t, f.authz, func() {
		allowed, steps, err := walk()
		require.NoError(t, err)
		require.True(t, allowed, "control: admitted delegator carries template.read, steps %+v", steps)
	})

	allowed, steps, err := walk()
	assert.False(t, allowed)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrProjectAccessDenied)
	require.NotEmpty(t, steps)
	last := steps[len(steps)-1]
	assert.Equal(t, "delegation_ceiling_error", last.Step)
	assert.Contains(t, last.Detail, errAdmissionLookupFault.Error())
}
