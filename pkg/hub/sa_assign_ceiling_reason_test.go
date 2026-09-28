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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ptone/scion#2144: when the project-default service-account assignment is
// denied at the delegation ceiling, the 403 body must say why — orphaned
// creator vs. a creator that lost the permission — instead of the generic
// "You don't have permission..." message that also covers ordinary policy
// denials. These tests exercise evaluateSAAssignment directly (the
// transport-independent body authorizeSAAssignment calls), the same way
// TestEvaluateSAAssignment_NilRequestPolicyDenial does.

// scaCreateSA registers a project-scoped, verified GCP service account for
// the ceiling-reason tests. Verification is irrelevant to Layer 1 (the Hub
// policy / delegation ceiling check evaluateSAAssignment performs before
// ever reaching Layer 2's actAs check), but true matches a realistic
// project-default assignment.
func scaCreateSA(t *testing.T, s store.Store, projectID string) *store.GCPServiceAccount {
	t.Helper()
	sa := &store.GCPServiceAccount{
		ID:        tid("sca-sa-" + projectID),
		Scope:     store.ScopeProject,
		ScopeID:   projectID,
		Email:     "sca-default@proj.iam.gserviceaccount.com",
		ProjectID: "gcp-proj",
		Verified:  true,
		CreatedAt: time.Now(),
	}
	require.NoError(t, s.CreateGCPServiceAccount(context.Background(), sa))
	return sa
}

const scaGenericDenyMsg = "You don't have permission to assign this GCP service account"

// TestEvaluateSAAssignment_CeilingOrphanedDelegator covers the case that
// motivated the issue: the agent's delegator (its creator, here a user) no
// longer exists. The Hub policy kernel allows via the agent-jwt-scope
// synthetic binding (project:agent:create covers gcp_service_account.assign),
// then Step 10 (the delegation ceiling) flips it to a deny because the
// delegator cannot be resolved.
func TestEvaluateSAAssignment_CeilingOrphanedDelegator(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("sca-orphan-proj")
	agentID := tid("sca-orphan-agent")
	goneUserID := tid("sca-orphan-gone-user")

	createDCProject(t, s, projectID, "sca-orphan-project")
	createDCAgent(t, s, agentID, projectID, goneUserID, AgentRoleFull)
	sa := scaCreateSA(t, s, projectID)

	// The delegator (goneUserID) is never created — GetUser returns
	// ErrNotFound, which is the orphaned-delegation branch.
	createDCEdge(t, s, store.DelegationPrincipalUser, goneUserID,
		store.DelegationPrincipalAgent, agentID,
		store.RoleScopeProject, projectID, string(AgentRoleFull))

	agent := dcAgentIdentity(agentID, projectID, AgentRoleFull)
	agentCtx := contextWithIdentity(ctx, agent)

	denial := srv.evaluateSAAssignment(agentCtx, nil, sa, SurfaceProjectDefault)
	require.NotNil(t, denial, "an orphaned delegator must deny the assignment")
	assert.Equal(t, saAssignDenyForbiddenStructured, denial.kind)
	assert.Equal(t,
		"This agent cannot assign service accounts: the principal that created it no longer exists. "+
			"Ask an admin to recreate the agent under a current user.",
		denial.msg)
	assert.NotContains(t, denial.msg, goneUserID, "the 403 body must not name the missing principal's ID")
	assert.NotContains(t, denial.msg, agentID, "the 403 body must not name the agent's ID")
}

// TestEvaluateSAAssignment_CeilingDelegatorLacksPermission covers the second
// ceiling cause: the delegator still exists and is active, but no longer
// holds gcp_service_account.assign (no seeded project role grants it — only
// super-admin does).
func TestEvaluateSAAssignment_CeilingDelegatorLacksPermission(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("sca-lacks-proj")
	agentID := tid("sca-lacks-agent")
	delegatorID := tid("sca-lacks-delegator")

	createDCProject(t, s, projectID, "sca-lacks-project")
	createDCUser(t, s, delegatorID, "sca-lacks-delegator@example.com", projectID, store.ProjectRoleOwner)
	assertNotSystemAdmin(t, srv.authzService, ctx, delegatorID)
	createDCAgent(t, s, agentID, projectID, delegatorID, AgentRoleFull)
	sa := scaCreateSA(t, s, projectID)

	createDCEdge(t, s, store.DelegationPrincipalUser, delegatorID,
		store.DelegationPrincipalAgent, agentID,
		store.RoleScopeProject, projectID, string(AgentRoleFull))

	agent := dcAgentIdentity(agentID, projectID, AgentRoleFull)
	agentCtx := contextWithIdentity(ctx, agent)

	denial := srv.evaluateSAAssignment(agentCtx, nil, sa, SurfaceProjectDefault)
	require.NotNil(t, denial, "a delegator that lost the permission must deny the assignment")
	assert.Equal(t, saAssignDenyForbiddenStructured, denial.kind)
	assert.Equal(t,
		"This agent cannot assign service accounts: the principal that created it no longer holds "+
			"permission to assign this service account.",
		denial.msg)
	assert.NotContains(t, denial.msg, delegatorID, "the 403 body must not name the delegator's ID")
	assert.NotContains(t, denial.msg, agentID, "the 403 body must not name the agent's ID")
}

// TestEvaluateSAAssignment_OrdinaryDenialKeepsGenericMessage pins that a
// non-ceiling denial (here: a user with no role anywhere) is completely
// unaffected by this change — byte-for-byte the same message as before.
func TestEvaluateSAAssignment_OrdinaryDenialKeepsGenericMessage(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	projectID := tid("sca-ordinary-proj")
	createDCProject(t, s, projectID, "sca-ordinary-project")
	sa := scaCreateSA(t, s, projectID)

	stranger := NewAuthenticatedUser(tid("sca-ordinary-stranger"), "stranger@example.com", "Stranger",
		store.UserRoleMember, "test")
	strangerCtx := contextWithIdentity(ctx, stranger)

	denial := srv.evaluateSAAssignment(strangerCtx, nil, sa, SurfaceProjectDefault)
	require.NotNil(t, denial, "a stranger with no role must be denied")
	assert.Equal(t, saAssignDenyForbiddenStructured, denial.kind)
	assert.Equal(t, scaGenericDenyMsg, denial.msg,
		"an ordinary policy denial must keep the exact generic message, byte for byte")
}

// scaGetUserErrorStore wraps a real store and forces GetUser to fail with a
// non-ErrNotFound error, simulating a genuine store fault (as opposed to a
// definitively-missing delegator) inside checkUserHoldsPermission.
type scaGetUserErrorStore struct {
	store.Store
	failUserID string
	err        error
}

func (s *scaGetUserErrorStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	if id == s.failUserID {
		return nil, s.err
	}
	return s.Store.GetUser(ctx, id)
}

// TestDelegationCeiling_StoreErrorSetsCeilingErrorCause covers the third
// DenyCause directly at the AuthzService level (the same level
// TestDelegationCeiling_MintAndAssignFailClosed uses): a genuine store fault
// while checking the delegator's permission must classify as
// DenyCauseCeilingError, not one of the two specific causes, so that
// evaluateSAAssignment's switch (tested above) falls through to the generic
// message for it — it is transient/internal, not a fact about the caller.
//
// This is built directly on AuthzService rather than a full *Server: New()
// runs a store-introspection migration step that the error-injecting wrapper
// does not support, and the ceiling classification this test targets lives
// entirely in AuthzService.Decide, one layer below evaluateSAAssignment.
func TestDelegationCeiling_StoreErrorSetsCeilingErrorCause(t *testing.T) {
	s, err := newTestStore(":memory:")
	if err != nil {
		t.Skipf("skipping: test store unavailable (%v)", err)
	}
	ctx := context.Background()
	require.NoError(t, s.Migrate(ctx))
	_ = s.DeleteHubSetting(ctx, "migration_delegation_edge_backfill_v1")
	reconcileBuiltInRoles(ctx, s)

	projectID := tid("sca-storeerr-proj")
	agentID := tid("sca-storeerr-agent")
	delegatorID := tid("sca-storeerr-delegator")

	createDCProject(t, s, projectID, "sca-storeerr-project")
	createDCUser(t, s, delegatorID, "sca-storeerr-delegator@example.com", projectID, store.ProjectRoleOwner)
	createDCAgent(t, s, agentID, projectID, delegatorID, AgentRoleFull)
	sa := scaCreateSA(t, s, projectID)
	createDCEdge(t, s, store.DelegationPrincipalUser, delegatorID,
		store.DelegationPrincipalAgent, agentID,
		store.RoleScopeProject, projectID, string(AgentRoleFull))

	injectedErr := errors.New("simulated store failure")
	errStore := &scaGetUserErrorStore{Store: s, failUserID: delegatorID, err: injectedErr}
	authz := NewAuthzService(errStore, slog.Default())

	agent := dcAgentIdentity(agentID, projectID, AgentRoleFull)
	resource := gcpServiceAccountResource(sa)

	decision := authz.CheckAccess(ctx, agent, resource, ActionAssign)
	require.False(t, decision.Allowed, "a ceiling store fault must fail closed")
	assert.Equal(t, DenyCauseCeilingError, decision.DenyCause,
		"a genuine store fault must classify as ceiling_error, not orphaned or lacks-permission")
}
