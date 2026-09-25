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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #1797: agents created by the scheduler's dispatch_agent path must carry the
// same creator attribution and project-default GCP identity as agents created
// through the agent-create API.

func fireScheduledDispatchAsOwner(t *testing.T, f *bypassAgentsFixture, agentName string) error {
	t.Helper()
	ctx := context.Background()
	f.srv.createProjectMembersGroup(ctx, f.proj, f.owner.ID)
	return f.srv.dispatchAgentEventHandler()(ctx, store.ScheduledEvent{
		ID:        "evt-" + agentName,
		ProjectID: f.proj.ID,
		EventType: "dispatch_agent",
		Payload:   `{"agentName":"` + agentName + `","task":"scheduled work"}`,
		CreatedBy: f.owner.ID,
	})
}

func setProjectDefaultSAAnnotations(t *testing.T, f *bypassAgentsFixture, saID string) {
	t.Helper()
	ctx := context.Background()
	proj, err := f.store.GetProject(ctx, f.proj.ID)
	require.NoError(t, err)
	if proj.Annotations == nil {
		proj.Annotations = map[string]string{}
	}
	proj.Annotations[projectSettingDefaultGCPIdentityMode] = store.GCPMetadataModeAssign
	proj.Annotations[projectSettingDefaultGCPIdentitySAID] = saID
	require.NoError(t, f.store.UpdateProject(ctx, proj))
}

func TestScheduledDispatch_UserCreatorSetsCreatorName(t *testing.T) {
	f := bypassAgentsSetup(t)

	require.NoError(t, fireScheduledDispatchAsOwner(t, f, "sched-creator-name"))

	got, err := f.store.GetAgentBySlug(context.Background(), f.proj.ID, "sched-creator-name")
	require.NoError(t, err)
	require.NotNil(t, got.AppliedConfig)
	assert.Equal(t, f.owner.Email, got.AppliedConfig.CreatorName,
		"scheduled agent must record the scheduling user's email as CreatorName, as the create path does")
	assert.Equal(t, f.owner.ID, got.CreatedBy)
}

func TestScheduledDispatch_NoProjectDefaultLeavesGCPIdentityUnchanged(t *testing.T) {
	f := bypassAgentsSetup(t)

	require.NoError(t, fireScheduledDispatchAsOwner(t, f, "sched-no-sa"))

	got, err := f.store.GetAgentBySlug(context.Background(), f.proj.ID, "sched-no-sa")
	require.NoError(t, err)
	require.NotNil(t, got.AppliedConfig)
	assert.Nil(t, got.AppliedConfig.GCPIdentity,
		"without a project-default GCP identity the scheduler path must keep its prior behaviour")
}

func TestScheduledDispatch_ProjectDefaultSAAssigned(t *testing.T) {
	f := bypassAgentsSetup(t)
	sa := bypassAgentsCreateSA(t, f, f.proj.ID, true)
	setProjectDefaultSAAnnotations(t, f, sa.ID)
	enforceSAAssign(f.srv, store.NewFakeCallerPermissionChecker().AllowTarget(sa.Email))

	require.NoError(t, fireScheduledDispatchAsOwner(t, f, "sched-default-sa"))

	got, err := f.store.GetAgentBySlug(context.Background(), f.proj.ID, "sched-default-sa")
	require.NoError(t, err)
	require.NotNil(t, got.AppliedConfig)
	require.NotNil(t, got.AppliedConfig.GCPIdentity)
	assert.Equal(t, store.GCPMetadataModeAssign, got.AppliedConfig.GCPIdentity.MetadataMode)
	assert.Equal(t, sa.ID, got.AppliedConfig.GCPIdentity.ServiceAccountID)
	assert.Equal(t, sa.Email, got.AppliedConfig.GCPIdentity.ServiceAccountEmail)
	assert.Equal(t, f.owner.Email, got.AppliedConfig.CreatorName)
}

func TestScheduledDispatch_ProjectDefaultSADeniedFailsDispatch(t *testing.T) {
	f := bypassAgentsSetup(t)
	sa := bypassAgentsCreateSA(t, f, f.proj.ID, true)
	setProjectDefaultSAAnnotations(t, f, sa.ID)
	enforceSAAssign(f.srv, store.NewFakeCallerPermissionChecker().DenyTarget(sa.Email, "no actAs grant"))

	err := fireScheduledDispatchAsOwner(t, f, "sched-denied-sa")
	require.Error(t, err, "a creator without actAs on the project-default SA must not get a scheduled agent")
	assert.Contains(t, err.Error(), store.PermissionActAs, "denial must come from the actAs gate")

	_, getErr := f.store.GetAgentBySlug(context.Background(), f.proj.ID, "sched-denied-sa")
	assert.ErrorIs(t, getErr, store.ErrNotFound, "denied dispatch must not create the agent record")
}

func TestScheduledDispatch_UnverifiedProjectDefaultSAFailsDispatch(t *testing.T) {
	f := bypassAgentsSetup(t)
	sa := bypassAgentsCreateSA(t, f, f.proj.ID, false)
	setProjectDefaultSAAnnotations(t, f, sa.ID)

	err := fireScheduledDispatchAsOwner(t, f, "sched-unverified-sa")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not verified")
}

// The scheduler calls evaluateSAAssignment with no *http.Request. Every denial
// path logs through logAuthzDenial with that nil request; these pin that a nil
// request is accepted rather than dereferenced. The callerPrincipal-failure
// site calls the same logAuthzDenial with the same r, so the direct test
// covers it; it is not independently reachable past the policy layer.
func TestEvaluateSAAssignment_NilRequestLogAuthzDenial(t *testing.T) {
	assert.NotPanics(t, func() {
		logAuthzDenial(nil, nil, Resource{Type: "gcp_service_account"}, ActionAssign, "test")
	})
}

func TestEvaluateSAAssignment_NilRequestPolicyDenial(t *testing.T) {
	f := bypassAgentsSetup(t)
	sa := bypassAgentsCreateSA(t, f, f.proj.ID, true)

	// A user with no membership anywhere: the Hub policy layer denies, which
	// is the first logAuthzDenial call site in evaluateSAAssignment.
	stranger := NewAuthenticatedUser("stranger-user", "stranger@example.com", "Stranger", store.UserRoleMember, "scheduler")
	ctx := contextWithIdentity(context.Background(), stranger)

	var denial *saAssignDenial
	require.NotPanics(t, func() {
		denial = f.srv.evaluateSAAssignment(ctx, nil, sa, SurfaceProjectDefault)
	})
	require.NotNil(t, denial, "a stranger must be denied")
	assert.Equal(t, saAssignDenyForbiddenStructured, denial.kind)
}
