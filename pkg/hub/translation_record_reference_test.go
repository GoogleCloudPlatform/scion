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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const trrEmail = "broker-host@trr-project.iam.gserviceaccount.com"

// trrRecord stores a verified hub record created by sandbox passthrough
// translation for a broker host account.
func trrRecord(t *testing.T, s store.Store, id, brokerID string) *store.GCPServiceAccount {
	t.Helper()
	sa := &store.GCPServiceAccount{
		ID: tid(id), Scope: store.ScopeHub, ScopeID: brokerID, Email: trrEmail, ProjectID: "trr-project",
		Verified: true, VerifiedAt: time.Now(), VerificationStatus: store.GCPVerificationVerified,
		CreatedBy: hostSATranslationCreator, CreatedAt: time.Now(),
	}
	require.NoError(t, s.CreateGCPServiceAccount(context.Background(), sa))
	return sa
}

func TestSAReferencePreconditions(t *testing.T) {
	assert.NotNil(t, saReferencePreconditions(&store.GCPServiceAccount{Scope: store.ScopeHub, CreatedBy: hostSATranslationCreator}))
	assert.Nil(t, saReferencePreconditions(&store.GCPServiceAccount{Scope: store.ScopeHub, CreatedBy: tid("an-admin")}))
	assert.Nil(t, saReferencePreconditions(&store.GCPServiceAccount{Scope: store.ScopeProject, CreatedBy: hostSATranslationCreator}))
	assert.Nil(t, saReferencePreconditions(nil))
}

func TestTranslationRecord_ExplicitCreate_RefusedForMemberAndAdmin(t *testing.T) {
	f := bypassAgentsSetup(t)
	sa := trrRecord(t, f.store, "trr-explicit", f.broker.ID)
	checker := store.NewFakeCallerPermissionChecker().AllowTarget(trrEmail)
	enforceSAAssign(f.srv, checker)

	rec := createAgentAsOwner(t, f, CreateAgentRequest{
		Name:        "trr-explicit-member",
		GCPIdentity: &GCPIdentityAssignment{MetadataMode: "assign", ServiceAccountID: sa.ID},
	})
	assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "cannot be assigned directly")

	grantSuperAdmin(t, f.store, f.owner.ID)
	rec = createAgentAsOwner(t, f, CreateAgentRequest{
		Name:        "trr-explicit-admin",
		GCPIdentity: &GCPIdentityAssignment{MetadataMode: "assign", ServiceAccountID: sa.ID},
	})
	assert.Equal(t, http.StatusForbidden, rec.Code, "admins are refused too; body: %s", rec.Body.String())
	assert.Equal(t, 0, checker.CallCount(), "refused before any actAs check")
}

func TestTranslationRecord_ExplicitPatch_Refused(t *testing.T) {
	f := bypassAgentsSetup(t)
	sa := trrRecord(t, f.store, "trr-patch", f.broker.ID)
	enforceSAAssign(f.srv, store.NewFakeCallerPermissionChecker().AllowTarget(trrEmail))
	f.srv.seedProjectCreatorMembership(context.Background(), f.proj)
	require.NoError(t, f.srv.createProjectOwnerRoleBinding(context.Background(), f.proj.ID, f.owner.ID))
	agent := &store.Agent{
		ID: tid("trr-patch-agent"), Slug: "trr-patch-agent", Name: "trr-patch-agent", ProjectID: f.proj.ID,
		RuntimeBrokerID: f.broker.ID, Phase: string(state.PhaseCreated), CreatedBy: f.owner.ID, OwnerID: f.owner.ID,
		Created: time.Now(), Updated: time.Now(),
		AppliedConfig: &store.AgentAppliedConfig{GCPIdentity: &store.GCPIdentityConfig{MetadataMode: store.GCPMetadataModeBlock}},
	}
	require.NoError(t, f.store.CreateAgent(context.Background(), agent))

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, "/api/v1/agents/"+agent.ID,
		map[string]interface{}{"gcp_identity": map[string]string{"metadata_mode": "assign", "service_account_id": sa.ID}})

	assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "cannot be assigned directly")
	assert.Equal(t, store.GCPMetadataModeBlock, mustGetAgent(t, f.store, agent.ID).AppliedConfig.GCPIdentity.MetadataMode)
}

func TestTranslationRecord_ProjectDefaultSetting_Refused(t *testing.T) {
	f := bypassAgentsSetup(t)
	sa := trrRecord(t, f.store, "trr-setting", f.broker.ID)
	enforceSAAssign(f.srv, store.NewFakeCallerPermissionChecker().AllowTarget(trrEmail))
	f.srv.seedProjectCreatorMembership(context.Background(), f.proj)
	require.NoError(t, f.srv.createProjectOwnerRoleBinding(context.Background(), f.proj.ID, f.owner.ID))

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPut, "/api/v1/projects/"+f.proj.ID+"/settings",
		map[string]interface{}{
			"defaultGCPIdentityMode":             store.GCPMetadataModeAssign,
			"defaultGCPIdentityServiceAccountID": sa.ID,
		})

	assert.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "cannot be assigned directly")
	proj, err := f.store.GetProject(context.Background(), f.proj.ID)
	require.NoError(t, err)
	assert.Empty(t, proj.Annotations[projectSettingDefaultGCPIdentitySAID], "nothing is written")
}

func TestTranslationRecord_HubDefaultSetting_Refused(t *testing.T) {
	srv, s := testServer(t)
	sa := trrRecord(t, s, "trr-hub-setting", tid("trr-some-broker"))
	enforceSAAssign(srv, store.NewFakeCallerPermissionChecker())
	rec := httptest.NewRecorder()

	ok := srv.validateHubDefaultGCPIdentity(rec, context.Background(), opsettings.AgentDefaultsSettings{
		DefaultGCPIdentityMode:             store.GCPMetadataModeAssign,
		DefaultGCPIdentityServiceAccountID: sa.ID,
	})

	assert.False(t, ok)
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Contains(t, rec.Body.String(), "cannot be assigned directly")
}

// TestTranslationRecord_PersistedProjectDefault_RefusedWhenConsumed: a
// project default naming a translation record that is already stored (set
// before the settings refusal existed) is refused when it is consumed, by
// agent create and by scheduled dispatch.
func TestTranslationRecord_PersistedProjectDefault_RefusedWhenConsumed(t *testing.T) {
	f := bypassAgentsSetup(t)
	sa := trrRecord(t, f.store, "trr-persisted", f.broker.ID)
	enforceSAAssign(f.srv, store.NewFakeCallerPermissionChecker().AllowTarget(trrEmail))
	setStaleProjectDefaultSA(t, f, sa.ID)

	rec := createAgentAsOwner(t, f, CreateAgentRequest{Name: "trr-persisted-create"})
	assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "cannot be assigned directly")

	err := fireScheduledDispatchAsOwner(t, f, "trr-persisted-sched")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot be assigned directly")
	_, getErr := f.store.GetAgentBySlug(context.Background(), f.proj.ID, "trr-persisted-sched")
	assert.ErrorIs(t, getErr, store.ErrNotFound)
}

// TestTranslationRecord_TranslationStillUsesIt: the translation path accepts
// (and reuses) the record that an explicit reference is refused.
func TestTranslationRecord_TranslationStillUsesIt(t *testing.T) {
	f := setupPTG(t, "trr-translation", trrEmail)
	enforceSAAssign(f.srv, store.NewFakeCallerPermissionChecker().AllowTarget(trrEmail))

	code, body := f.createDefault(t, "trr-translation-1")
	require.Equal(t, http.StatusCreated, code, "body: %s", body)
	first := agentBySlug(t, f.store, f.project.ID, "trr-translation-1")
	require.Equal(t, store.GCPMetadataModeAssign, first.AppliedConfig.GCPIdentity.MetadataMode)
	recordID := first.AppliedConfig.GCPIdentity.ServiceAccountID
	record, err := f.store.GetGCPServiceAccount(context.Background(), recordID)
	require.NoError(t, err)
	require.True(t, store.IsHostSATranslationRecord(record))

	code, body = f.createDefault(t, "trr-translation-2")
	require.Equal(t, http.StatusCreated, code, "body: %s", body)
	assert.Equal(t, recordID, agentBySlug(t, f.store, f.project.ID, "trr-translation-2").AppliedConfig.GCPIdentity.ServiceAccountID,
		"translation reuses the record")

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "trr-translation-explicit", ProjectID: f.project.ID, Task: "test",
		GCPIdentity: &GCPIdentityAssignment{MetadataMode: "assign", ServiceAccountID: recordID},
	})
	assert.Equal(t, http.StatusForbidden, rec.Code, "the same record named explicitly is refused; body: %s", rec.Body.String())
}

// TestTranslationRecord_ExistingHolderStartsAndMints: an agent that already
// holds a translation record passes the start and token-mint admissibility
// check, which does not apply the reference rule.
func TestTranslationRecord_ExistingHolderStartsAndMints(t *testing.T) {
	srv, s := testServer(t)
	sa := trrRecord(t, s, "trr-holder", tid("trr-holder-broker"))
	enforceSAAssign(srv, store.NewFakeCallerPermissionChecker())

	got, err := srv.admissibleGCPServiceAccount(context.Background(), &store.GCPIdentityConfig{
		MetadataMode: store.GCPMetadataModeAssign, ServiceAccountID: sa.ID, ServiceAccountEmail: sa.Email,
	}, tid("any-project"), sa.ScopeID)

	require.NoError(t, err)
	assert.Equal(t, sa.ID, got.ID)
}

// TestTranslationRecord_HolderOnAnotherBroker_RefusedAtStartAndMint: an
// agent that holds a translation record bound to another broker (for
// example one moved before moves were refused) is refused at start and at
// token mint. The binding comes from the record's ScopeID and the agent's
// stored RuntimeBrokerID.
func TestTranslationRecord_HolderOnAnotherBroker_RefusedAtStartAndMint(t *testing.T) {
	srv, s := testServer(t)
	sa := trrRecord(t, s, "trr-moved", tid("trr-broker-a"))
	enforceSAAssign(srv, store.NewFakeCallerPermissionChecker())
	gcpID := &store.GCPIdentityConfig{
		MetadataMode: store.GCPMetadataModeAssign, ServiceAccountID: sa.ID, ServiceAccountEmail: sa.Email,
	}
	agent := &store.Agent{
		ID: tid("trr-moved-agent"), ProjectID: tid("trr-moved-project"), RuntimeBrokerID: tid("trr-broker-b"),
		AppliedConfig: &store.AgentAppliedConfig{GCPIdentity: gcpID},
	}

	_, err := srv.admissibleGCPServiceAccount(context.Background(), gcpID, agent.ProjectID, agent.RuntimeBrokerID)
	assert.ErrorIs(t, err, errGCPSABrokerMismatch)

	assert.False(t, srv.resolveAgentGCPMintFacts(context.Background(), gcpID, agent.ProjectID, agent.RuntimeBrokerID),
		"no token is minted")

	rec := httptest.NewRecorder()
	assert.True(t, srv.gcpIdentityStartRefusal(context.Background(), rec, agent, "start"), "the start is refused")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "not bound to this agent's broker")
}

// TestTranslationRecord_ReincarnateRepairKeepsIdentityWithWarning: a
// same-role reincarnate of an agent holding a translation record proceeds;
// the assignment is not re-recorded and the plan carries the warning. The
// agent's identity is kept (AppliedConfig.GCPIdentity is copied verbatim).
func TestTranslationRecord_ReincarnateRepairKeepsIdentityWithWarning(t *testing.T) {
	f := newRepairFixture(t)
	f.withAssignedSA(t)
	sa := trrRecord(t, f.s, "trr-reinc", f.broker.ID)
	enforceSAAssign(f.srv, store.NewFakeCallerPermissionChecker().AllowTarget(trrEmail))
	agent := mustGetAgent(t, f.s, f.target.ID)
	agent.AppliedConfig.GCPIdentity.ServiceAccountID = sa.ID
	agent.AppliedConfig.GCPIdentity.ServiceAccountEmail = sa.Email
	auth := &reincarnateAuthority{
		DelegatorType: store.DelegationPrincipalUser,
		DelegatorID:   f.userID,
		Role:          string(AgentRoleFull),
		Ceiling:       store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
		Provenance:    recordedProv(store.DelegationPrincipalUser, f.userID, store.SourceCredentialSession),
	}
	w := httptest.NewRecorder()

	ok := f.srv.resolveRepairAssignment(w, reincarnateRequest(t, agent.ID, f.session(), nil), agent, auth, false, nil)

	require.True(t, ok, "the reincarnate proceeds")
	assert.Empty(t, w.Body.String())
	assert.Nil(t, auth.Assignment, "no assignment is recorded")
	assert.Contains(t, auth.AssignmentNotRecorded, reincarnateRepairAssignmentNotRecordedWarning)
	assert.Contains(t, auth.AssignmentNotRecorded, "cannot be assigned directly")
}

// TestTranslationRecord_MoveToAnotherBroker_Refused: a move of an agent
// holding its broker's translation record to another broker is refused, as
// a dry run and for real, and nothing changes.
func TestTranslationRecord_MoveToAnotherBroker_Refused(t *testing.T) {
	f, user, id, _, _ := moveRepairFixture(t)
	sa := trrRecord(t, f.s, "trr-move", f.agent.RuntimeBrokerID)
	agent := mustGetAgent(t, f.s, f.agent.ID)
	agent.AppliedConfig.GCPIdentity.ServiceAccountID = sa.ID
	agent.AppliedConfig.GCPIdentity.ServiceAccountEmail = sa.Email
	require.NoError(t, f.s.UpdateAgent(context.Background(), agent))
	f.agent = mustGetAgent(t, f.s, f.agent.ID)
	old := seedAssignment(t, f.s, f.agent, user.ID, sa.ID)
	enforceSAAssign(f.srv, store.NewFakeCallerPermissionChecker().AllowTarget(trrEmail))

	for _, dryRun := range []bool{true, false} {
		rec := httptest.NewRecorder()
		f.srv.handleReincarnateAgent(rec, reincarnateRequest(t, f.agent.ID, id,
			ReincarnateAgentRequest{DryRun: dryRun, Handoff: "h", TargetBroker: f.dst.ID}), f.agent.ID)

		assert.Equal(t, http.StatusConflict, rec.Code, "dryRun=%v body: %s", dryRun, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "host service account of broker "+sa.ScopeID)
	}
	got := mustGetAgent(t, f.s, f.agent.ID)
	assert.Equal(t, f.agent.RuntimeBrokerID, got.RuntimeBrokerID, "the agent stays on its broker")
	assert.Equal(t, sa.ID, got.AppliedConfig.GCPIdentity.ServiceAccountID)
	rows := activeAssignments(t, f.s, f.agent.ID)
	require.Len(t, rows, 1)
	assert.Equal(t, old.ID, rows[0].ID)
}

// TestTranslationRecord_SameBrokerReincarnate_KeepsIdentity: an in-place
// reincarnate of an agent holding its own broker's translation record is
// admitted (the start check passes on the record's broker) and keeps the
// identity.
func TestTranslationRecord_SameBrokerReincarnate_KeepsIdentity(t *testing.T) {
	f := newRepairFixture(t)
	f.withAssignedSA(t)
	sa := trrRecord(t, f.s, "trr-inplace", f.target.RuntimeBrokerID)
	agent := mustGetAgent(t, f.s, f.target.ID)
	agent.AppliedConfig.GCPIdentity.ServiceAccountID = sa.ID
	agent.AppliedConfig.GCPIdentity.ServiceAccountEmail = sa.Email
	require.NoError(t, f.s.UpdateAgent(context.Background(), agent))
	enforceSAAssign(f.srv, store.NewFakeCallerPermissionChecker().AllowTarget(trrEmail))

	rec := f.reincarnateWith(t, f.session(), ReincarnateAgentRequest{DryRun: true})

	require.Equal(t, http.StatusOK, rec.Code, "the start check passes on the record's own broker; body: %s", rec.Body.String())
	assert.Equal(t, sa.ID, mustGetAgent(t, f.s, f.target.ID).AppliedConfig.GCPIdentity.ServiceAccountID)
	// The repair's not-recorded warning is pinned by
	// TestTranslationRecord_ReincarnateRepairKeepsIdentityWithWarning.
}

// TestLifecycleHookExecutor_TranslationRecordIdentity_RefusedNoMint: a
// stored hook whose execution identity is a translation record fails at
// execution, before any token is minted and before the request is sent.
func TestLifecycleHookExecutor_TranslationRecordIdentity_RefusedNoMint(t *testing.T) {
	s := executorTestStore(t)
	projID := seedExecutorProject(t, s, "trr-hook-project")
	sa := trrRecord(t, s, "trr-hook", tid("trr-hook-broker"))
	hit := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	tokenGen := &mockTokenGenerator{accessToken: "tok", email: "hub@sa.com"}
	executor := newTestExecutor(s, tokenGen, newCapturingAuditLogger(), slog.Default())
	hook := makeTestHook(sa.ID, &store.LifecycleHookAction{
		Type: store.LifecycleHookActionHTTP, Method: "POST", URL: ts.URL + "/x",
		OnError: store.LifecycleHookOnErrorLog, TimeoutSeconds: 10,
	})

	_ = executor.Execute(context.Background(), hook, makeTestAgent(projID), "running")

	tokenGen.mu.Lock()
	calls := tokenGen.calls
	tokenGen.mu.Unlock()
	assert.Equal(t, 0, calls, "no token is minted")
	assert.False(t, hit, "the hook request is not sent")
}

// TestTranslationRecord_MoveWorkerRecheck_Refused: the move worker's
// eligibility recheck applies the same probe as the move handler.
func TestTranslationRecord_MoveWorkerRecheck_Refused(t *testing.T) {
	f := setupMoveFixture(t, true, nil)
	ctx := context.Background()
	sa := trrRecord(t, f.s, "trr-recheck", f.src.ID)
	a, err := f.s.GetAgent(ctx, f.agent.ID)
	require.NoError(t, err)
	if a.AppliedConfig == nil {
		a.AppliedConfig = &store.AgentAppliedConfig{}
	}
	a.AppliedConfig.GCPIdentity = &store.GCPIdentityConfig{
		MetadataMode: store.GCPMetadataModeAssign, ServiceAccountID: sa.ID, ServiceAccountEmail: sa.Email,
	}
	require.NoError(t, f.s.UpdateAgent(ctx, a))

	_, err = f.srv.recheckMoveEligibility(ctx, f.agent.ID, &reincarnationMove{
		SourceBrokerID: f.src.ID, TargetBrokerID: f.dst.ID, ProjectID: f.project.ID, SelfMove: true, AgentDirWorkspace: true,
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "host service account of broker "+f.src.ID)
}

// blankScopeIDStore returns the record with ID id with an empty ScopeID while
// armed. The store refuses to write an empty scope_id, so this is how a test
// presents an unbound translation record to the code that reads it.
type blankScopeIDStore struct {
	store.Store
	fault *storeFaultSwitch
	id    string
}

func (b *blankScopeIDStore) GetGCPServiceAccount(ctx context.Context, id string) (*store.GCPServiceAccount, error) {
	sa, err := b.Store.GetGCPServiceAccount(ctx, id)
	if err == nil && sa != nil && id == b.id && b.fault.Active() {
		c := *sa
		c.ScopeID = ""
		return &c, nil
	}
	return sa, err
}

// TestTranslationRecord_UnboundRecord_RefusedAtStartMintAndMove: an agent
// holding a translation record with no broker binding is refused at start,
// at token mint and at move, even on the broker the record was made for.
// With the binding present (wrapper disarmed) the same agent is admitted,
// so the refusal comes from the missing binding alone.
func TestTranslationRecord_UnboundRecord_RefusedAtStartMintAndMove(t *testing.T) {
	srv, s, blank, fault := testServerWithStoreFault(t, func(inner store.Store, fault *storeFaultSwitch) *blankScopeIDStore {
		return &blankScopeIDStore{Store: inner, fault: fault}
	})
	brokerID := tid("trr-unbound-broker")
	sa := trrRecord(t, s, "trr-unbound-rec", brokerID)
	blank.id = sa.ID
	enforceSAAssign(srv, store.NewFakeCallerPermissionChecker())
	ctx := context.Background()
	gcpID := &store.GCPIdentityConfig{
		MetadataMode: store.GCPMetadataModeAssign, ServiceAccountID: sa.ID, ServiceAccountEmail: sa.Email,
	}
	agent := &store.Agent{
		ID: tid("trr-unbound-agent"), ProjectID: tid("trr-unbound-project"), RuntimeBrokerID: brokerID,
		AppliedConfig: &store.AgentAppliedConfig{GCPIdentity: gcpID},
	}
	dst := &store.RuntimeBroker{ID: brokerID, Name: "trr-unbound-broker"}

	// Bound: admitted, and the move probe has no objection.
	_, err := srv.admissibleGCPServiceAccount(ctx, gcpID, agent.ProjectID, agent.RuntimeBrokerID)
	require.NoError(t, err)
	assert.Empty(t, srv.hostSARecordMoveProbe(ctx, agent.AppliedConfig)(dst))

	fault.Arm()

	_, err = srv.admissibleGCPServiceAccount(ctx, gcpID, agent.ProjectID, agent.RuntimeBrokerID)
	assert.ErrorIs(t, err, errGCPSABrokerMismatch)
	assert.False(t, srv.resolveAgentGCPMintFacts(ctx, gcpID, agent.ProjectID, agent.RuntimeBrokerID), "no token is minted")
	rec := httptest.NewRecorder()
	assert.True(t, srv.gcpIdentityStartRefusal(ctx, rec, agent, "start"), "the start is refused")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "not bound to this agent's broker")
	assert.Contains(t, srv.hostSARecordMoveProbe(ctx, agent.AppliedConfig)(dst), "no broker binding",
		"the move probe used by the move handler and the move worker refuses")
}
