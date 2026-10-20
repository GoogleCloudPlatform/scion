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
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
)

const ptrHostSA = "broker-host@ptr-project.iam.gserviceaccount.com"

// ptrFixture is a cloudrun-sandbox broker server in enforce mode with the
// broker's translation record for its host account.
type ptrFixture struct {
	srv     *Server
	store   store.Store
	owner   *store.User // project and broker owner
	other   *store.User // a hub member who owns nothing
	project *store.Project
	broker  *store.RuntimeBroker
	record  *store.GCPServiceAccount
	checker *store.FakeCallerPermissionChecker
}

func setupPTR(t *testing.T, name string) *ptrFixture {
	t.Helper()
	owner := ptUser(tid(name+"-owner"), name+"-owner@test.com", store.UserRoleMember)
	srv, s, project, broker := setupPassthroughSandboxServer(t, owner, ptrHostSA, "ptr-project")
	f := &ptrFixture{srv: srv, store: s, owner: owner, project: project, broker: broker}
	f.other = addExtraUser(t, s, tid(name+"-other"), name+"-other@test.com", store.UserRoleMember)
	f.checker = store.NewFakeCallerPermissionChecker().AllowTarget(ptrHostSA)
	enforceSAAssign(srv, f.checker)
	f.record = &store.GCPServiceAccount{
		ID: tid(name + "-record"), Scope: store.ScopeHub, ScopeID: broker.ID, Email: ptrHostSA, ProjectID: "ptr-project",
		Verified: true, VerifiedAt: time.Now(), VerificationStatus: store.GCPVerificationVerified,
		CreatedBy: hostSATranslationCreator, CreatedAt: time.Now(),
	}
	require.NoError(t, s.CreateGCPServiceAccount(context.Background(), f.record))
	return f
}

// translatedAgent stores an agent holding the translated assignment, created
// by creatorID, with an active assignment row.
func (f *ptrFixture) translatedAgent(t *testing.T, slug, creatorID string, annotate bool) *store.Agent {
	t.Helper()
	a := &store.Agent{
		ID: tid(slug), Slug: slug, Name: slug, ProjectID: f.project.ID, RuntimeBrokerID: f.broker.ID,
		Phase: string(state.PhaseCreated), CreatedBy: creatorID, OwnerID: creatorID,
		Created: time.Now(), Updated: time.Now(),
		AppliedConfig: &store.AgentAppliedConfig{GCPIdentity: &store.GCPIdentityConfig{
			MetadataMode: store.GCPMetadataModeAssign, ServiceAccountID: f.record.ID,
			ServiceAccountEmail: f.record.Email, ProjectID: f.record.ProjectID,
		}},
	}
	if annotate {
		a.Annotations = map[string]string{annotationGCPIdentityTranslatedFrom: "passthrough"}
	}
	require.NoError(t, f.store.CreateAgent(context.Background(), a))
	seedAssignment(t, f.store, a, creatorID, f.record.ID)
	return a
}

func (f *ptrFixture) run(t *testing.T, params map[string]string) (string, error) {
	t.Helper()
	exec, err := f.srv.resolveMaintenanceExecutor(entadapter.PassthroughTranslationReviewKey)
	require.NoError(t, err)
	var buf bytes.Buffer
	err = exec.Run(context.Background(), &buf, params)
	return buf.String(), err
}

func (f *ptrFixture) reviewOf(t *testing.T, agentID string) translationReviewEntry {
	t.Helper()
	entries, err := f.srv.reviewTranslatedAgents(context.Background(), false)
	require.NoError(t, err)
	for _, en := range entries {
		if en.AgentID == agentID {
			return en
		}
	}
	t.Fatalf("agent %s not reviewed", agentID)
	return translationReviewEntry{}
}

func TestPassthroughTranslationReview_ReportOnlyByDefault(t *testing.T) {
	f := setupPTR(t, "ptr-report")
	pass := f.translatedAgent(t, "ptr-report-pass", f.owner.ID, true)
	fail := f.translatedAgent(t, "ptr-report-fail", f.other.ID, true)
	f.translatedAgent(t, "ptr-report-unannotated", f.other.ID, false)

	out, err := f.run(t, map[string]string{})

	require.NoError(t, err)
	assert.Contains(t, out, "Report only")
	assert.Contains(t, out, "agent="+pass.ID)
	assert.Contains(t, out, "agent="+fail.ID)
	assert.NotContains(t, out, tid("ptr-report-unannotated"), "the annotation selects the agents")
	assert.Contains(t, out, "Reviewed 2 annotated agent(s): pass 1, fail 1, error 0, skipped 0, blocked 0")
	assert.Contains(t, out, "record_created=")
	for _, id := range []string{pass.ID, fail.ID} {
		got := mustGetAgent(t, f.store, id)
		assert.Equal(t, store.GCPMetadataModeAssign, got.AppliedConfig.GCPIdentity.MetadataMode, "nothing is changed")
		assert.Len(t, activeAssignments(t, f.store, id), 1)
	}
}

func TestPassthroughTranslationReview_Outcomes(t *testing.T) {
	f := setupPTR(t, "ptr-outcomes")
	owner := f.translatedAgent(t, "ptr-out-owner", f.owner.ID, true)
	nonOwner := f.translatedAgent(t, "ptr-out-nonowner", f.other.ID, true)
	agentCreator := f.translatedAgent(t, "ptr-out-agent", tid("some-agent"), true)

	en := f.reviewOf(t, owner.ID)
	assert.Equal(t, translationReviewPass, en.Outcome)
	assert.True(t, en.RecordBound)

	en = f.reviewOf(t, nonOwner.ID)
	assert.Equal(t, translationReviewFail, en.Outcome)
	assert.Contains(t, en.Reason, "broker ownership")

	en = f.reviewOf(t, agentCreator.ID)
	assert.Equal(t, translationReviewFail, en.Outcome)
	assert.Contains(t, en.Reason, "not a user")
}

func TestPassthroughTranslationReview_CreatorLacksActAs_Fails(t *testing.T) {
	f := setupPTR(t, "ptr-noactas")
	a := f.translatedAgent(t, "ptr-noactas-agent", f.owner.ID, true)
	f.checker.DenyTarget(ptrHostSA, "grant removed")

	en := f.reviewOf(t, a.ID)

	assert.Equal(t, translationReviewFail, en.Outcome)
	assert.Contains(t, en.Reason, store.PermissionActAs)
}

func TestPassthroughTranslationReview_HostAccountChanged_Fails(t *testing.T) {
	f := setupPTR(t, "ptr-hostchange")
	a := f.translatedAgent(t, "ptr-hostchange-agent", f.owner.ID, true)
	b, err := f.store.GetRuntimeBroker(context.Background(), f.broker.ID)
	require.NoError(t, err)
	const newHost = "new-host@ptr-project.iam.gserviceaccount.com"
	_, err = f.store.SetRuntimeBrokerGCPHost(context.Background(), b.ID, newHost, b.GCPHostProjectID)
	require.NoError(t, err)
	f.checker.AllowTarget(newHost)

	en := f.reviewOf(t, a.ID)

	assert.Equal(t, translationReviewFail, en.Outcome)
	assert.Contains(t, en.Reason, "not the broker's current host service account")
}

func TestPassthroughTranslationReview_BlockFlagBlocksFailuresOnly(t *testing.T) {
	f := setupPTR(t, "ptr-block")
	pass := f.translatedAgent(t, "ptr-block-pass", f.owner.ID, true)
	fail := f.translatedAgent(t, "ptr-block-fail", f.other.ID, true)

	out, err := f.run(t, map[string]string{"block": "true"})

	require.NoError(t, err)
	assert.Contains(t, out, "Block mode")
	assert.Contains(t, out, "blocked 1")
	got := mustGetAgent(t, f.store, fail.ID)
	assert.Equal(t, store.GCPMetadataModeBlock, got.AppliedConfig.GCPIdentity.MetadataMode)
	assert.Equal(t, "blocked", got.Annotations[annotationGCPIdentityReview])
	assert.Empty(t, activeAssignments(t, f.store, fail.ID), "the assignment is deactivated")

	kept := mustGetAgent(t, f.store, pass.ID)
	assert.Equal(t, store.GCPMetadataModeAssign, kept.AppliedConfig.GCPIdentity.MetadataMode)
	assert.Len(t, activeAssignments(t, f.store, pass.ID), 1)
}

func TestPassthroughTranslationReview_BlockNeedsExactString(t *testing.T) {
	f := setupPTR(t, "ptr-exact")
	fail := f.translatedAgent(t, "ptr-exact-fail", f.other.ID, true)

	for _, v := range []string{"True", "1", "yes", ""} {
		out, err := f.run(t, map[string]string{"block": v})
		require.NoError(t, err)
		assert.Contains(t, out, "Report only", "block=%q", v)
	}
	assert.Equal(t, store.GCPMetadataModeAssign, mustGetAgent(t, f.store, fail.ID).AppliedConfig.GCPIdentity.MetadataMode)
}

func TestPassthroughTranslationReview_RequiresEnforceMode(t *testing.T) {
	f := setupPTR(t, "ptr-off")
	f.translatedAgent(t, "ptr-off-agent", f.other.ID, true)
	f.srv.mu.Lock()
	f.srv.saAssignCheckMode = SAAssignCheckOff
	f.srv.mu.Unlock()

	_, err := f.run(t, map[string]string{"block": "true"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "gcpIamCheckMode=enforce")
}

// TestPassthroughTranslationReview_ReassignedAgentSkipped: an agent whose
// identity was set to another account after translation (here a project
// account) is outside the review and never blocked.
func TestPassthroughTranslationReview_ReassignedAgentSkipped(t *testing.T) {
	f := setupPTR(t, "ptr-reassigned")
	a := f.translatedAgent(t, "ptr-reassigned-agent", f.other.ID, true)
	own := wiringSA(t, f.store, store.ScopeProject, f.project.ID, "own-sa@ptr-project.iam.gserviceaccount.com")
	got := mustGetAgent(t, f.store, a.ID)
	got.AppliedConfig.GCPIdentity.ServiceAccountID = own.ID
	got.AppliedConfig.GCPIdentity.ServiceAccountEmail = own.Email
	require.NoError(t, f.store.UpdateAgent(context.Background(), got))

	out, err := f.run(t, map[string]string{"block": "true"})

	require.NoError(t, err)
	assert.Contains(t, out, "skipped 1, blocked 0")
	kept := mustGetAgent(t, f.store, a.ID)
	assert.Equal(t, store.GCPMetadataModeAssign, kept.AppliedConfig.GCPIdentity.MetadataMode)
	assert.Equal(t, own.ID, kept.AppliedConfig.GCPIdentity.ServiceAccountID)
}

// TestPassthroughTranslationReview_ReassignedAgentWithTranslatedRowSkipped:
// as ReassignedAgentSkipped, but the active assignment row left for the
// previous account was recorded by translation. A row for an account other
// than the assigned one does not select the agent.
func TestPassthroughTranslationReview_ReassignedAgentWithTranslatedRowSkipped(t *testing.T) {
	f := setupPTR(t, "ptr-reassigned-row")
	a := f.translatedAgent(t, "ptr-reassigned-row-agent", f.other.ID, true)
	require.NoError(t, f.store.ReplaceAgentServiceAccountAssignment(context.Background(), &store.AgentServiceAccountAssignment{
		AgentID: a.ID, ProjectID: a.ProjectID, ServiceAccountID: f.record.ID,
		Origin:              store.SAAssignmentOriginHostPassthroughTranslation,
		AuthorityProvenance: recordedProv(store.DelegationPrincipalUser, f.other.ID, store.SourceCredentialSession),
		EffectCeiling:       store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
	}))
	own := wiringSA(t, f.store, store.ScopeProject, f.project.ID, "own-row-sa@ptr-project.iam.gserviceaccount.com")
	got := mustGetAgent(t, f.store, a.ID)
	got.AppliedConfig.GCPIdentity.ServiceAccountID = own.ID
	got.AppliedConfig.GCPIdentity.ServiceAccountEmail = own.Email
	require.NoError(t, f.store.UpdateAgent(context.Background(), got))

	out, err := f.run(t, map[string]string{"block": "true"})

	require.NoError(t, err)
	assert.Contains(t, out, "skipped 1, blocked 0")
	kept := mustGetAgent(t, f.store, a.ID)
	assert.Equal(t, store.GCPMetadataModeAssign, kept.AppliedConfig.GCPIdentity.MetadataMode)
	assert.Equal(t, own.ID, kept.AppliedConfig.GCPIdentity.ServiceAccountID)
}

func TestPassthroughTranslationReview_UndecidedCheckIsErrorNotBlocked(t *testing.T) {
	t.Run("check did not complete", func(t *testing.T) {
		f := setupPTR(t, "ptr-checkfail")
		a := f.translatedAgent(t, "ptr-checkfail-agent", f.owner.ID, true)
		f.checker.FailTarget(ptrHostSA, errors.New("timeout"))

		out, err := f.run(t, map[string]string{"block": "true"})

		require.NoError(t, err)
		assert.Contains(t, out, "error 1")
		assert.Contains(t, out, "blocked 0")
		assert.Equal(t, store.GCPMetadataModeAssign, mustGetAgent(t, f.store, a.ID).AppliedConfig.GCPIdentity.MetadataMode)
	})
	t.Run("check unavailable", func(t *testing.T) {
		f := setupPTR(t, "ptr-unavailable")
		a := f.translatedAgent(t, "ptr-unavailable-agent", f.owner.ID, true)
		f.srv.SetGCPTokenGenerator(nil)

		out, err := f.run(t, map[string]string{"block": "true"})

		require.NoError(t, err)
		assert.Contains(t, out, "error 1")
		assert.Equal(t, store.GCPMetadataModeAssign, mustGetAgent(t, f.store, a.ID).AppliedConfig.GCPIdentity.MetadataMode)
	})
	t.Run("agent without broker", func(t *testing.T) {
		f := setupPTR(t, "ptr-nobroker")
		a := f.translatedAgent(t, "ptr-nobroker-agent", f.other.ID, true)
		got := mustGetAgent(t, f.store, a.ID)
		got.RuntimeBrokerID = ""
		require.NoError(t, f.store.UpdateAgent(context.Background(), got))

		out, err := f.run(t, map[string]string{"block": "true"})

		require.NoError(t, err)
		assert.Contains(t, out, "error 1")
		assert.Equal(t, store.GCPMetadataModeAssign, mustGetAgent(t, f.store, a.ID).AppliedConfig.GCPIdentity.MetadataMode)
	})
}

func TestPassthroughTranslationReview_SuspendedPrincipalIsError(t *testing.T) {
	f := setupPTR(t, "ptr-suspended")
	a := f.translatedAgent(t, "ptr-suspended-agent", f.owner.ID, true)
	u, err := f.store.GetUser(context.Background(), f.owner.ID)
	require.NoError(t, err)
	u.Status = store.UserStatusSuspended
	require.NoError(t, f.store.UpdateUser(context.Background(), u))

	out, err := f.run(t, map[string]string{"block": "true"})

	require.NoError(t, err)
	assert.Contains(t, out, "error 1")
	assert.Equal(t, store.GCPMetadataModeAssign, mustGetAgent(t, f.store, a.ID).AppliedConfig.GCPIdentity.MetadataMode)
}

// TestPassthroughTranslationReview_RecordedSourcePrincipal: the principal
// recorded on the assignment is checked, not the agent's creator.
func TestPassthroughTranslationReview_RecordedSourcePrincipal(t *testing.T) {
	f := setupPTR(t, "ptr-source")
	// Created by a non-owner, translated on a request by the broker owner.
	byOwner := f.translatedAgent(t, "ptr-source-owner", f.other.ID, true)
	require.NoError(t, f.store.ReplaceAgentServiceAccountAssignment(context.Background(), &store.AgentServiceAccountAssignment{
		AgentID: byOwner.ID, ProjectID: byOwner.ProjectID, ServiceAccountID: f.record.ID,
		Origin:              store.SAAssignmentOriginHostPassthroughTranslation,
		AuthorityProvenance: recordedProv(store.DelegationPrincipalUser, f.owner.ID, store.SourceCredentialSession),
		EffectCeiling:       store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
	}))
	// Created by the owner, but the recorded source is an agent.
	byAgent := f.translatedAgent(t, "ptr-source-agent", f.owner.ID, true)
	require.NoError(t, f.store.ReplaceAgentServiceAccountAssignment(context.Background(), &store.AgentServiceAccountAssignment{
		AgentID: byAgent.ID, ProjectID: byAgent.ProjectID, ServiceAccountID: f.record.ID,
		Origin:              store.SAAssignmentOriginHostPassthroughTranslation,
		AuthorityProvenance: recordedProv(store.DelegationPrincipalAgent, tid("some-agent"), store.SourceCredentialAgent),
		EffectCeiling:       store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
	}))

	en := f.reviewOf(t, byOwner.ID)
	assert.Equal(t, translationReviewPass, en.Outcome)
	assert.Equal(t, f.owner.ID, en.PrincipalID)

	en = f.reviewOf(t, byAgent.ID)
	assert.Equal(t, translationReviewFail, en.Outcome)
	assert.Contains(t, en.Reason, "agent principal")
}

// TestBlockTranslatedAgent_IdentityChangedSinceReview_Unchanged: the block
// re-checks the identity it reviewed and leaves a changed agent alone.
func TestBlockTranslatedAgent_IdentityChangedSinceReview_Unchanged(t *testing.T) {
	f := setupPTR(t, "ptr-changed")
	a := f.translatedAgent(t, "ptr-changed-agent", f.other.ID, true)

	err := f.srv.blockTranslatedAgent(context.Background(), a.ID, tid("some-other-sa"))

	assert.ErrorIs(t, err, errTranslationReviewChanged)
	got := mustGetAgent(t, f.store, a.ID)
	assert.Equal(t, store.GCPMetadataModeAssign, got.AppliedConfig.GCPIdentity.MetadataMode)
	assert.Len(t, activeAssignments(t, f.store, a.ID), 1)
}

// TestPassthroughTranslationReview_TranslatedOntoAdminHubRecord: an agent
// translated onto a hub record that an admin created (the record is not a
// translation record) is selected through its assignment's origin and
// reviewed, with record_bound false.
func TestPassthroughTranslationReview_TranslatedOntoAdminHubRecord(t *testing.T) {
	f := setupPTR(t, "ptr-adminrec")
	adminRec := &store.GCPServiceAccount{
		ID: tid("ptr-adminrec-sa"), Scope: store.ScopeHub, ScopeID: "hub-instance-1", Email: ptrHostSA, ProjectID: "ptr-project",
		Verified: true, VerifiedAt: time.Now(), VerificationStatus: store.GCPVerificationVerified,
		CreatedBy: tid("some-admin"), CreatedAt: time.Now(),
	}
	require.NoError(t, f.store.CreateGCPServiceAccount(context.Background(), adminRec))
	a := &store.Agent{
		ID: tid("ptr-adminrec-agent"), Slug: "ptr-adminrec-agent", Name: "ptr-adminrec-agent",
		ProjectID: f.project.ID, RuntimeBrokerID: f.broker.ID, Phase: string(state.PhaseCreated),
		CreatedBy: f.other.ID, OwnerID: f.other.ID, Created: time.Now(), Updated: time.Now(),
		Annotations: map[string]string{annotationGCPIdentityTranslatedFrom: "passthrough"},
		AppliedConfig: &store.AgentAppliedConfig{GCPIdentity: &store.GCPIdentityConfig{
			MetadataMode: store.GCPMetadataModeAssign, ServiceAccountID: adminRec.ID,
			ServiceAccountEmail: adminRec.Email, ProjectID: adminRec.ProjectID,
		}},
	}
	require.NoError(t, f.store.CreateAgent(context.Background(), a))
	require.NoError(t, f.store.ReplaceAgentServiceAccountAssignment(context.Background(), &store.AgentServiceAccountAssignment{
		AgentID: a.ID, ProjectID: a.ProjectID, ServiceAccountID: adminRec.ID,
		Origin:              store.SAAssignmentOriginHostPassthroughTranslation,
		AuthorityProvenance: recordedProv(store.DelegationPrincipalUser, f.other.ID, store.SourceCredentialSession),
		EffectCeiling:       store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
	}))

	en := f.reviewOf(t, a.ID)

	assert.Equal(t, translationReviewFail, en.Outcome, "reviewed, not skipped: the creator does not own the broker")
	assert.False(t, en.RecordBound)
	assert.Contains(t, en.Reason, "broker ownership")
}

// TestPassthroughTranslationReview_PrincipalFromReviewedAccountRow: only an
// active assignment row for the reviewed account supplies the principal.
// Here the agent's active row names another account with the broker owner as
// its source, so the creator (a non-owner) is checked and the review fails.
func TestPassthroughTranslationReview_PrincipalFromReviewedAccountRow(t *testing.T) {
	f := setupPTR(t, "ptr-rowmatch")
	a := f.translatedAgent(t, "ptr-rowmatch-agent", f.other.ID, true)
	otherSA := wiringSA(t, f.store, store.ScopeProject, f.project.ID, "row-sa@ptr-project.iam.gserviceaccount.com")
	require.NoError(t, f.store.ReplaceAgentServiceAccountAssignment(context.Background(), &store.AgentServiceAccountAssignment{
		AgentID: a.ID, ProjectID: a.ProjectID, ServiceAccountID: otherSA.ID,
		Origin:              store.SAAssignmentOriginUpdate,
		AuthorityProvenance: recordedProv(store.DelegationPrincipalUser, f.owner.ID, store.SourceCredentialSession),
		EffectCeiling:       store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
	}))

	en := f.reviewOf(t, a.ID)

	assert.Equal(t, f.other.ID, en.PrincipalID, "a row for another account is not used")
	assert.Equal(t, translationReviewFail, en.Outcome)
}
