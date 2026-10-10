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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// saWriteFaultStore fails the service-account assignment write, inside
// transactions too.
type saWriteFaultStore struct {
	store.Store
}

func (f *saWriteFaultStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	return f.Store.WithTx(ctx, func(tx store.Store) error {
		return fn(&saWriteFaultStore{Store: tx})
	})
}

func (f *saWriteFaultStore) ReplaceAgentServiceAccountAssignment(context.Context, *store.AgentServiceAccountAssignment) error {
	return errSPCInjected
}

// installSAWriteFault makes srv's store fail assignment writes until the
// test ends.
func installSAWriteFault(t *testing.T, srv *Server) {
	t.Helper()
	real := srv.store
	srv.store = &saWriteFaultStore{Store: real}
	t.Cleanup(func() { srv.store = real })
}

func activeAssignments(t *testing.T, s store.Store, agentID string) []store.AgentServiceAccountAssignment {
	t.Helper()
	rows, err := s.GetActiveAgentServiceAccountAssignments(context.Background(), agentID)
	require.NoError(t, err)
	return rows
}

func agentBySlug(t *testing.T, s store.Store, projectID, slug string) *store.Agent {
	t.Helper()
	a, err := s.GetAgentBySlug(context.Background(), projectID, slug)
	require.NoError(t, err)
	return a
}

func ownerSession(f *bypassAgentsFixture) store.AuthorityProvenance {
	return store.AuthorityProvenance{
		ProvenanceVersion:    store.ProvenanceVersionV1,
		SourcePrincipalKind:  store.DelegationPrincipalUser,
		SourcePrincipalID:    f.owner.ID,
		SourceCredentialKind: store.SourceCredentialSession,
	}
}

// ---- create ------------------------------------------------------------------

func TestSAParentCeiling_CreateAssignmentInSameTx(t *testing.T) {
	f := bypassAgentsSetup(t)
	sa := bypassAgentsCreateSA(t, f, f.proj.ID, true)
	req := func(name string) CreateAgentRequest {
		return CreateAgentRequest{Name: name, GCPIdentity: &GCPIdentityAssignment{
			MetadataMode: store.GCPMetadataModeAssign, ServiceAccountID: sa.ID}}
	}

	rec := createAgentAsOwner(t, f, req("sa-rec-create"))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	agent := agentBySlug(t, f.store, f.proj.ID, "sa-rec-create")
	rows := activeAssignments(t, f.store, agent.ID)
	require.Len(t, rows, 1)
	row := rows[0]
	assert.Equal(t, sa.ID, row.ServiceAccountID)
	assert.Equal(t, f.proj.ID, row.ProjectID)
	assert.Equal(t, store.SAAssignmentOriginCreateExplicit, row.Origin)
	assert.Equal(t, ownerSession(f), row.AuthorityProvenance)
	assert.Equal(t, store.EffectCeilingPrincipal, row.Kind)
	edges := activeEdgesFor(t, f.store, agent.ID)
	require.Len(t, edges, 1)
	assert.Equal(t, edges[0].AuthorityProvenance, row.AuthorityProvenance, "the assignment records the create's own source")
	assert.Equal(t, edges[0].EffectCeiling, row.EffectCeiling)

	// A failing assignment write rolls back the whole create.
	installSAWriteFault(t, f.srv)
	rec = createAgentAsOwner(t, f, req("sa-rec-create-fail"))
	assert.GreaterOrEqual(t, rec.Code, http.StatusInternalServerError, rec.Body.String())
	_, err := f.store.GetAgentBySlug(context.Background(), f.proj.ID, "sa-rec-create-fail")
	assert.ErrorIs(t, err, store.ErrNotFound, "no agent row without its assignment")
}

func TestSAParentCeiling_CreateWithoutAssignModeRecordsNothing(t *testing.T) {
	f := bypassAgentsSetup(t)
	rec := createAgentAsOwner(t, f, CreateAgentRequest{Name: "sa-rec-block",
		GCPIdentity: &GCPIdentityAssignment{MetadataMode: store.GCPMetadataModeBlock}})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Empty(t, activeAssignments(t, f.store, agentBySlug(t, f.store, f.proj.ID, "sa-rec-block").ID))
}

func TestSAParentCeiling_CreateRequiresOriginForAssignMode(t *testing.T) {
	f := bypassAgentsSetup(t)
	agent := &store.Agent{
		ID: api.NewUUID(), Slug: "sa-rec-no-origin", Name: "sa-rec-no-origin", ProjectID: f.proj.ID,
		CreatedBy: f.owner.ID, OwnerID: f.owner.ID, Phase: string(state.PhaseCreated),
		AppliedConfig: &store.AgentAppliedConfig{GCPIdentity: &store.GCPIdentityConfig{
			MetadataMode: store.GCPMetadataModeAssign, ServiceAccountID: "sa-x"}},
	}
	err := f.srv.commitAgentCreate(context.Background(), agentCreateWrite{
		Ceiling:    store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
		Provenance: ownerSession(f),
		Agent:      agent,
		Slug:       agent.Slug,
		Edge: &store.DelegationEdge{DelegatorType: store.DelegationPrincipalUser, DelegatorID: f.owner.ID,
			DelegateType: store.DelegationPrincipalAgent, ScopeType: store.RoleScopeProject, ScopeID: f.proj.ID,
			Role: string(AgentRoleFull), Active: true},
		Audit: &store.MutationAuditRecord{MutationType: mutationTypeAgentDelegation},
	})
	require.ErrorIs(t, err, errAgentCreateWriteInvalid)
	_, err = f.store.GetAgent(context.Background(), agent.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "nothing is written")
}

func TestSAParentCeiling_ProjectDefaultOriginRequiresLiveCreatorAssign(t *testing.T) {
	f := bypassAgentsSetup(t)
	sa := spcServiceAccount(t, f.store, "sa-rec-project-default-sa", store.ScopeProject, f.proj.ID, true)
	setProjectDefaultSAAnnotations(t, f, sa.ID)

	rec := createAgentAsOwner(t, f, CreateAgentRequest{Name: "sa-rec-project-default"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	agent := agentBySlug(t, f.store, f.proj.ID, "sa-rec-project-default")
	rows := activeAssignments(t, f.store, agent.ID)
	require.Len(t, rows, 1)
	assert.Equal(t, store.SAAssignmentOriginCreateProjectDefault, rows[0].Origin)
	assert.Equal(t, f.owner.ID, rows[0].SourcePrincipalID, "the creator is the source, not whoever configured the default")
	assert.NotEqual(t, f.owner.ID, sa.CreatedBy, "precondition: no resource-owner shortcut")

	ctx := context.Background()
	assertParentCeilingAllowed(t, f.srv.authzService.EvaluateServiceAccountParentCeiling(ctx, agent.ID, sa.ID))
	_, err := f.store.DeleteRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, f.owner.ID)
	require.NoError(t, err)
	assertParentCeilingDenied(t, f.srv.authzService.EvaluateServiceAccountParentCeiling(ctx, agent.ID, sa.ID),
		DenyCauseCeilingDelegatorLacksPermission)
}

func TestSAParentCeiling_HubDefaultOriginRequiresLiveCreatorAssign(t *testing.T) {
	f := bypassAgentsSetup(t)
	sa := spcServiceAccount(t, f.store, "sa-rec-hub-default-sa", store.ScopeProject, f.proj.ID, true)
	setHubAgentDefaults(f.srv, opsettings.AgentDefaultsSettings{
		DefaultGCPIdentityMode:             store.GCPMetadataModeAssign,
		DefaultGCPIdentityServiceAccountID: sa.ID,
	})

	rec := createAgentAsOwner(t, f, CreateAgentRequest{Name: "sa-rec-hub-default"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	agent := agentBySlug(t, f.store, f.proj.ID, "sa-rec-hub-default")
	rows := activeAssignments(t, f.store, agent.ID)
	require.Len(t, rows, 1)
	assert.Equal(t, store.SAAssignmentOriginCreateHubDefault, rows[0].Origin)
	assert.Equal(t, f.owner.ID, rows[0].SourcePrincipalID)

	ctx := context.Background()
	assertParentCeilingAllowed(t, f.srv.authzService.EvaluateServiceAccountParentCeiling(ctx, agent.ID, sa.ID))
	_, err := f.store.DeleteRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, f.owner.ID)
	require.NoError(t, err)
	assertParentCeilingDenied(t, f.srv.authzService.EvaluateServiceAccountParentCeiling(ctx, agent.ID, sa.ID),
		DenyCauseCeilingDelegatorLacksPermission)
}

// ---- scheduled create --------------------------------------------------------

func TestSAParentCeiling_ScheduledDefaultUsesRevisionPrincipal(t *testing.T) {
	f := bypassAgentsSetup(t)
	sa := bypassAgentsCreateSA(t, f, f.proj.ID, true)
	setProjectDefaultSAAnnotations(t, f, sa.ID)
	require.NoError(t, fireScheduledDispatchAsOwner(t, f, "sa-rec-sched"))

	agent := agentBySlug(t, f.store, f.proj.ID, "sa-rec-sched")
	rows := activeAssignments(t, f.store, agent.ID)
	require.Len(t, rows, 1)
	row := rows[0]
	assert.Equal(t, store.SAAssignmentOriginScheduledProjectDefault, row.Origin)
	assert.Equal(t, store.SourceCredentialScheduler, row.SourceCredentialKind)
	assert.Equal(t, store.DelegationPrincipalUser, row.SourcePrincipalKind)
	assert.Equal(t, f.owner.ID, row.SourcePrincipalID, "the revision principal is the source")
	assert.Equal(t, "evt-sa-rec-sched", row.SourceEventID)
	edges := activeEdgesFor(t, f.store, agent.ID)
	require.Len(t, edges, 1)
	assert.Equal(t, edges[0].AuthorityProvenance, row.AuthorityProvenance)
	assert.Equal(t, edges[0].EffectCeiling, row.EffectCeiling)
	assertParentCeilingAllowed(t, f.srv.authzService.EvaluateServiceAccountParentCeiling(context.Background(), agent.ID, sa.ID))
}

func TestSAParentCeiling_ScheduledCreateAssignmentInSameTx(t *testing.T) {
	f := bypassAgentsSetup(t)
	sa := bypassAgentsCreateSA(t, f, f.proj.ID, true)
	setProjectDefaultSAAnnotations(t, f, sa.ID)
	installSAWriteFault(t, f.srv)
	require.Error(t, fireScheduledDispatchAsOwner(t, f, "sa-rec-sched-fail"))
	_, err := f.store.GetAgentBySlug(context.Background(), f.proj.ID, "sa-rec-sched-fail")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// TestSAParentCeiling_ScheduledHostPassthroughRecordsRevisionPrincipal: a
// project-default passthrough on a sandbox broker is translated to the
// broker's host account by the scheduled ladder. The scheduled create
// succeeds and records the assignment with the revision principal as its
// source, under the edge's scheduler provenance and ceiling.
func TestSAParentCeiling_ScheduledHostPassthroughRecordsRevisionPrincipal(t *testing.T) {
	hostSAEmail := "broker-host@sa-rec-sched-pt.iam.gserviceaccount.com"
	owner := ptUser(tid("user-sa-rec-sched-pt"), "sa-rec-sched-pt@test.com", store.UserRoleMember)
	srv, s, project, _ := setupPassthroughSandboxServer(t, owner, hostSAEmail, "sa-rec-sched-pt")
	ctx := context.Background()
	proj, err := s.GetProject(ctx, project.ID)
	require.NoError(t, err)
	if proj.Annotations == nil {
		proj.Annotations = map[string]string{}
	}
	proj.Annotations[projectSettingDefaultGCPIdentityMode] = store.GCPMetadataModePassthrough
	require.NoError(t, s.UpdateProject(ctx, proj))

	evt := withSessionRevision(store.ScheduledEvent{
		ID:        "evt-sa-rec-sched-pt",
		ProjectID: project.ID,
		EventType: "dispatch_agent",
		Payload:   `{"agentName":"sa-rec-sched-pt","task":"scheduled work"}`,
		CreatedBy: owner.ID,
	}, owner.ID)
	require.NoError(t, srv.dispatchAgentEventHandler()(ctx, evt), "the scheduled create on a sandbox broker succeeds")

	agent := agentBySlug(t, s, project.ID, "sa-rec-sched-pt")
	saID := agentAssignedServiceAccountID(agent)
	require.NotEmpty(t, saID, "the passthrough default is translated to an assign-mode identity")
	hostSA, err := s.GetGCPServiceAccount(ctx, saID)
	require.NoError(t, err)
	assert.Equal(t, hostSAEmail, hostSA.Email, "precondition: the account is the broker's host account")

	rows := activeAssignments(t, s, agent.ID)
	require.Len(t, rows, 1)
	row := rows[0]
	assert.Equal(t, store.SAAssignmentOriginHostPassthroughTranslation, row.Origin)
	assert.Equal(t, saID, row.ServiceAccountID)
	assert.Equal(t, store.SourceCredentialScheduler, row.SourceCredentialKind)
	assert.Equal(t, store.DelegationPrincipalUser, row.SourcePrincipalKind)
	assert.Equal(t, owner.ID, row.SourcePrincipalID, "the revision principal is the source")
	assert.NotEqual(t, saID, row.SourcePrincipalID, "the host account is never the source")
	assert.NotEqual(t, hostSAEmail, row.SourcePrincipalID)
	assert.Equal(t, evt.ID, row.SourceEventID)
	edges := activeEdgesFor(t, s, agent.ID)
	require.Len(t, edges, 1)
	assert.Equal(t, edges[0].AuthorityProvenance, row.AuthorityProvenance, "the assignment records the edge's provenance")
	assert.Equal(t, edges[0].EffectCeiling, row.EffectCeiling)
}

// TestSAParentCeiling_ProfileDefaultOrigins: the per-profile rung records
// its own origin on the create path and on the scheduled ladder, distinct
// from the project-wide rung. Origin is descriptive only.
func TestSAParentCeiling_ProfileDefaultOrigins(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		pf := newProfileDefaultFixture(t, "remote")
		pf.setProjectDefaultAssignBroad(t)
		pf.setProfileDefaults(t, map[string]string{"remote": pf.k8s.ID})
		agent := createdAgentRecord(t, pf.bypassAgentsFixture, CreateAgentRequest{Name: "sa-rec-profile-create"})
		rows := activeAssignments(t, pf.store, agent.ID)
		require.Len(t, rows, 1)
		assert.Equal(t, store.SAAssignmentOriginCreateProjectProfileDefault, rows[0].Origin)
		assert.Equal(t, pf.k8s.ID, rows[0].ServiceAccountID)
		assert.Equal(t, pf.owner.ID, rows[0].SourcePrincipalID)
	})

	t.Run("scheduled", func(t *testing.T) {
		pf := newProfileDefaultFixture(t, "remote")
		pf.setProjectDefaultAssignBroad(t)
		pf.setProfileDefaults(t, map[string]string{"remote": pf.k8s.ID})
		require.NoError(t, fireScheduledDispatchAsOwner(t, pf.bypassAgentsFixture, "sa-rec-profile-sched"))
		agent := agentBySlug(t, pf.store, pf.proj.ID, "sa-rec-profile-sched")
		rows := activeAssignments(t, pf.store, agent.ID)
		require.Len(t, rows, 1)
		assert.Equal(t, store.SAAssignmentOriginScheduledProjectProfileDefault, rows[0].Origin)
		assert.Equal(t, pf.k8s.ID, rows[0].ServiceAccountID)
		assert.Equal(t, store.SourceCredentialScheduler, rows[0].SourceCredentialKind)
		assert.Equal(t, pf.owner.ID, rows[0].SourcePrincipalID, "the revision principal is the source")
	})

	t.Run("scheduled without a profile entry", func(t *testing.T) {
		pf := newProfileDefaultFixture(t, "local")
		pf.setProjectDefaultAssignBroad(t)
		pf.setProfileDefaults(t, map[string]string{"remote": pf.k8s.ID})
		require.NoError(t, fireScheduledDispatchAsOwner(t, pf.bypassAgentsFixture, "sa-rec-profile-sched-miss"))
		agent := agentBySlug(t, pf.store, pf.proj.ID, "sa-rec-profile-sched-miss")
		rows := activeAssignments(t, pf.store, agent.ID)
		require.Len(t, rows, 1)
		assert.Equal(t, store.SAAssignmentOriginScheduledProjectDefault, rows[0].Origin)
		assert.Equal(t, pf.broad.ID, rows[0].ServiceAccountID)
	})
}

// ---- PATCH -------------------------------------------------------------------

func patchGCPIdentityAsOwner(t *testing.T, f *bypassAgentsFixture, agentID string, body map[string]interface{}) *httptest.ResponseRecorder {
	t.Helper()
	ctx := context.Background()
	f.srv.seedProjectCreatorMembership(ctx, f.proj)
	_ = f.srv.createProjectOwnerRoleBinding(ctx, f.proj.ID, f.owner.ID)
	return doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, "/api/v1/agents/"+agentID,
		map[string]interface{}{"gcp_identity": body})
}

func TestSAParentCeiling_PatchAssignmentInSameTx(t *testing.T) {
	f := bypassAgentsSetup(t)
	sa := bypassAgentsCreateSA(t, f, f.proj.ID, true)
	agent := pendingAgentForPatch(t, f, "sa-rec-patch")
	assignBody := map[string]interface{}{"metadata_mode": store.GCPMetadataModeAssign, "service_account_id": sa.ID}

	rec := patchGCPIdentityAsOwner(t, f, agent.ID, assignBody)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rows := activeAssignments(t, f.store, agent.ID)
	require.Len(t, rows, 1)
	assert.Equal(t, store.SAAssignmentOriginUpdate, rows[0].Origin)
	assert.Equal(t, ownerSession(f), rows[0].AuthorityProvenance)
	assert.Equal(t, sa.ID, rows[0].ServiceAccountID)

	// A failing assignment write leaves the identity unchanged.
	other := pendingAgentForPatch(t, f, "sa-rec-patch-fail")
	installSAWriteFault(t, f.srv)
	rec = patchGCPIdentityAsOwner(t, f, other.ID, assignBody)
	assert.GreaterOrEqual(t, rec.Code, http.StatusInternalServerError, rec.Body.String())
	got := mustGetAgent(t, f.store, other.ID)
	assert.Equal(t, "", agentAssignedServiceAccountID(got), "the identity is not written without its assignment")
}

// TestSAParentCeiling_PatchSourceIsCaller: the PATCH caller, not the agent's
// creator, is recorded as the source.
func TestSAParentCeiling_PatchSourceIsCaller(t *testing.T) {
	f := bypassAgentsSetup(t)
	ctx := context.Background()
	sa := bypassAgentsCreateSA(t, f, f.proj.ID, true)
	agent := pendingAgentForPatch(t, f, "sa-rec-patch-caller")
	caller := &store.User{
		ID: tid("sa-rec-patch-caller"), Email: "sa-rec-patch-caller@example.com", DisplayName: "Patch Caller",
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	require.NoError(t, f.store.CreateUser(ctx, caller))
	ensureHubMembership(ctx, f.store, caller.ID)
	grantProjectRole(t, f.store, caller.ID, f.proj.ID, store.ProjectRoleAdmin)
	require.NotEqual(t, caller.ID, agent.CreatedBy, "precondition: the caller is not the creator")

	rec := doRequestAsUser(t, f.srv, caller, http.MethodPatch, "/api/v1/agents/"+agent.ID,
		map[string]interface{}{"gcp_identity": map[string]interface{}{
			"metadata_mode": store.GCPMetadataModeAssign, "service_account_id": sa.ID}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rows := activeAssignments(t, f.store, agent.ID)
	require.Len(t, rows, 1)
	assert.Equal(t, store.SAAssignmentOriginUpdate, rows[0].Origin)
	assert.Equal(t, caller.ID, rows[0].SourcePrincipalID, "the PATCH caller is the source")
	assert.NotEqual(t, agent.CreatedBy, rows[0].SourcePrincipalID, "the creator is not the source")
	assert.Equal(t, store.SourceCredentialSession, rows[0].SourceCredentialKind)
	assert.Equal(t, sa.ID, rows[0].ServiceAccountID)
}

func TestSAParentCeiling_ClearIdentityDeactivates(t *testing.T) {
	f := bypassAgentsSetup(t)
	sa := bypassAgentsCreateSA(t, f, f.proj.ID, true)
	agent := pendingAgentForPatch(t, f, "sa-rec-clear")
	rec := patchGCPIdentityAsOwner(t, f, agent.ID,
		map[string]interface{}{"metadata_mode": store.GCPMetadataModeAssign, "service_account_id": sa.ID})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, activeAssignments(t, f.store, agent.ID), 1)

	rec = patchGCPIdentityAsOwner(t, f, agent.ID, map[string]interface{}{"metadata_mode": store.GCPMetadataModeBlock})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Empty(t, activeAssignments(t, f.store, agent.ID), "block clears the assignment")
	assertParentCeilingDenied(t, f.srv.authzService.EvaluateServiceAccountParentCeiling(context.Background(), agent.ID, sa.ID),
		DenyCauseCeilingProvenanceMissing)
}

func TestSAParentCeiling_ReplaceAndClearCauses(t *testing.T) {
	f := bypassAgentsSetup(t)
	sa := bypassAgentsCreateSA(t, f, f.proj.ID, true)
	sa2 := bypassAgentsCreateSA(t, f, f.proj.ID, true)
	agent := pendingAgentForPatch(t, f, "sa-rec-causes")
	for _, id := range []string{sa.ID, sa2.ID} {
		rec := patchGCPIdentityAsOwner(t, f, agent.ID,
			map[string]interface{}{"metadata_mode": store.GCPMetadataModeAssign, "service_account_id": id})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
	rows := activeAssignments(t, f.store, agent.ID)
	require.Len(t, rows, 1, "a replacement leaves one active row")
	assert.Equal(t, sa2.ID, rows[0].ServiceAccountID)
}

// TestSAParentCeiling_PatchSourceCeilingDenied covers the source ceiling an
// identity-setting PATCH records: a source that cannot record authority is
// refused with 403 and a lookup fault answers 503, before any write.
func TestSAParentCeiling_PatchSourceCeilingDenied(t *testing.T) {
	f := bypassAgentsSetup(t)
	resource := Resource{Type: "agent", ID: "a", ParentType: "project", ParentID: f.proj.ID}

	// No identity: not an authority source.
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPatch, "/api/v1/agents/a", nil)
	_, _, ok := f.srv.saAssignmentSourceCeiling(rec, r, resource, ActionUpdate)
	assert.False(t, ok)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, string(DeniedByDelegationCeiling), decodeTargetAPIError(t, rec).Details["denied_by"])

	// An agent source whose row cannot be read: 503.
	saved := f.srv.authzService.store
	f.srv.authzService.store = &spcFaultStore{Store: saved, agentID: f.caller.ID}
	t.Cleanup(func() { f.srv.authzService.store = saved })
	rec = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodPatch, "/api/v1/agents/a", nil).WithContext(
		contextWithIdentity(context.Background(), dcAgentIdentity(f.caller.ID, f.proj.ID, AgentRoleFull)))
	_, _, ok = f.srv.saAssignmentSourceCeiling(rec, r, resource, ActionUpdate)
	assert.False(t, ok)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())

	// A session source records principal provenance.
	f.srv.authzService.store = saved
	rec = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodPatch, "/api/v1/agents/a", nil).WithContext(
		contextWithIdentity(context.Background(), NewAuthenticatedUser(f.owner.ID, f.owner.Email, "", "member", "web")))
	c, p, ok := f.srv.saAssignmentSourceCeiling(rec, r, resource, ActionUpdate)
	require.True(t, ok)
	assert.Equal(t, store.EffectCeilingPrincipal, c.Kind)
	assert.Equal(t, ownerSession(f), p)
}

// TestSAParentCeiling_PatchWithoutAssignModeSkipsSourceCeiling: block and
// plain passthrough PATCHes do not compute a source ceiling, so a fault
// there does not affect them.
func TestSAParentCeiling_PatchWithoutAssignModeSkipsSourceCeiling(t *testing.T) {
	f := bypassAgentsSetup(t)
	agent := pendingAgentForPatch(t, f, "sa-rec-block-patch")
	saved := f.srv.authzService.store
	f.srv.authzService.store = &mintAssignErrStore{Store: saved}
	t.Cleanup(func() { f.srv.authzService.store = saved })
	rec := patchGCPIdentityAsOwner(t, f, agent.ID, map[string]interface{}{"metadata_mode": store.GCPMetadataModeBlock})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, store.GCPMetadataModeBlock, mustGetAgent(t, f.store, agent.ID).AppliedConfig.GCPIdentity.MetadataMode)
}

// ---- host passthrough translation ------------------------------------------

func TestSAParentCeiling_HostPassthroughWritesGCPIdentity(t *testing.T) {
	hostSAEmail := "broker-host@sa-rec.iam.gserviceaccount.com"
	owner := ptUser(tid("user-sa-rec-pt"), "sa-rec-pt@test.com", store.UserRoleMember)
	srv, s, project, broker := setupPassthroughSandboxServer(t, owner, hostSAEmail, "sa-rec")
	enforceSAAssign(srv, store.NewFakeCallerPermissionChecker().AllowTarget(hostSAEmail))

	// Create: the translated identity is persisted with its assignment.
	rec := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "sa-rec-pt-create", ProjectID: project.ID,
		GCPIdentity: &GCPIdentityAssignment{MetadataMode: "passthrough"},
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	created := agentBySlug(t, s, project.ID, "sa-rec-pt-create")
	saID := agentAssignedServiceAccountID(created)
	require.NotEmpty(t, saID, "the translated assign-mode identity is persisted")
	rows := activeAssignments(t, s, created.ID)
	require.Len(t, rows, 1)
	assert.Equal(t, store.SAAssignmentOriginHostPassthroughTranslation, rows[0].Origin)
	assert.Equal(t, saID, rows[0].ServiceAccountID)

	// PATCH: the same.
	agent := &store.Agent{
		ID: tid("agent-sa-rec-pt-patch"), Slug: "sa-rec-pt-patch", Name: "sa-rec-pt-patch", ProjectID: project.ID,
		Phase: string(state.PhaseCreated), RuntimeBrokerID: broker.ID, OwnerID: owner.ID, CreatedBy: owner.ID,
		AppliedConfig: &store.AgentAppliedConfig{GCPIdentity: &store.GCPIdentityConfig{MetadataMode: store.GCPMetadataModeBlock}},
		Created:       time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateAgent(context.Background(), agent))
	rec = doRequestAsUser(t, srv, owner, http.MethodPatch, "/api/v1/agents/"+agent.ID,
		map[string]interface{}{"gcp_identity": map[string]string{"metadata_mode": "passthrough"}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rows = activeAssignments(t, s, agent.ID)
	require.Len(t, rows, 1)
	assert.Equal(t, store.SAAssignmentOriginHostPassthroughTranslation, rows[0].Origin)
	assert.Equal(t, agentAssignedServiceAccountID(mustGetAgent(t, s, agent.ID)), rows[0].ServiceAccountID)
}

func TestSAParentCeiling_HostPassthroughAssignmentInSameTx(t *testing.T) {
	hostSAEmail := "broker-host@sa-rec-tx.iam.gserviceaccount.com"
	owner := ptUser(tid("user-sa-rec-pt-tx"), "sa-rec-pt-tx@test.com", store.UserRoleMember)
	srv, s, project, broker := setupPassthroughSandboxServer(t, owner, hostSAEmail, "sa-rec-tx")
	enforceSAAssign(srv, store.NewFakeCallerPermissionChecker().AllowTarget(hostSAEmail))
	agent := &store.Agent{
		ID: tid("agent-sa-rec-pt-tx"), Slug: "sa-rec-pt-tx", Name: "sa-rec-pt-tx", ProjectID: project.ID,
		Phase: string(state.PhaseCreated), RuntimeBrokerID: broker.ID, OwnerID: owner.ID, CreatedBy: owner.ID,
		AppliedConfig: &store.AgentAppliedConfig{GCPIdentity: &store.GCPIdentityConfig{MetadataMode: store.GCPMetadataModeBlock}},
		Created:       time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateAgent(context.Background(), agent))
	installSAWriteFault(t, srv)
	rec := doRequestAsUser(t, srv, owner, http.MethodPatch, "/api/v1/agents/"+agent.ID,
		map[string]interface{}{"gcp_identity": map[string]string{"metadata_mode": "passthrough"}})
	assert.GreaterOrEqual(t, rec.Code, http.StatusInternalServerError, rec.Body.String())
	assert.Equal(t, store.GCPMetadataModeBlock, mustGetAgent(t, s, agent.ID).AppliedConfig.GCPIdentity.MetadataMode)
}

func TestSAParentCeiling_HostPassthroughSourceIsRequester(t *testing.T) {
	hostSAEmail := "broker-host@sa-rec-src.iam.gserviceaccount.com"
	owner := ptUser(tid("user-sa-rec-src"), "sa-rec-src@test.com", store.UserRoleMember)
	srv, s, project, _ := setupPassthroughSandboxServer(t, owner, hostSAEmail, "sa-rec-src")
	enforceSAAssign(srv, store.NewFakeCallerPermissionChecker().AllowTarget(hostSAEmail))
	rec := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "sa-rec-src", ProjectID: project.ID,
		GCPIdentity: &GCPIdentityAssignment{MetadataMode: "passthrough"},
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	agent := agentBySlug(t, s, project.ID, "sa-rec-src")
	saID := agentAssignedServiceAccountID(agent)
	rows := activeAssignments(t, s, agent.ID)
	require.Len(t, rows, 1)
	row := rows[0]
	assert.Equal(t, owner.ID, row.SourcePrincipalID, "the requester is the source")
	assert.NotEqual(t, saID, row.SourcePrincipalID, "the host account is never the source")
	assert.NotEqual(t, hostSAEmail, row.SourcePrincipalID)

	// The evaluator's outcome does not depend on the origin.
	ctx := context.Background()
	want := srv.authzService.EvaluateServiceAccountParentCeiling(ctx, agent.ID, saID)
	for _, origin := range []store.SAAssignmentOrigin{store.SAAssignmentOriginCreateExplicit, store.SAAssignmentOriginUpdate, ""} {
		again := row
		again.ID = ""
		again.Origin = origin
		require.NoError(t, s.ReplaceAgentServiceAccountAssignment(ctx, &again))
		got := srv.authzService.EvaluateServiceAccountParentCeiling(ctx, agent.ID, saID)
		assert.Equal(t, want.Allowed, got.Allowed, "origin %q", origin)
		assert.Equal(t, want.DenyCause, got.DenyCause, "origin %q", origin)
	}
}

// ---- lifecycle -----------------------------------------------------------------

// seedAssignment records an active assignment for agent from delegatorID.
func seedAssignment(t *testing.T, s store.Store, agent *store.Agent, delegatorID, saID string) *store.AgentServiceAccountAssignment {
	t.Helper()
	a := &store.AgentServiceAccountAssignment{
		AgentID: agent.ID, ProjectID: agent.ProjectID, ServiceAccountID: saID,
		Origin:              store.SAAssignmentOriginUpdate,
		AuthorityProvenance: recordedProv(store.DelegationPrincipalUser, delegatorID, store.SourceCredentialSession),
		EffectCeiling:       store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
	}
	require.NoError(t, s.ReplaceAgentServiceAccountAssignment(context.Background(), a))
	return a
}

func TestSAParentCeiling_SoftDeleteRestoreReactivatesAssignment(t *testing.T) {
	for name, withEdge := range map[string]bool{"with edge": true, "assignment only": false} {
		t.Run(name, func(t *testing.T) {
			srv, s, _, _ := engineTestServer(t)
			srv.config.SoftDeleteRetention = time.Hour
			agent := setupBrokerAgentInPhase(t, s, "sa-soft", state.PhaseStopped)
			if withEdge {
				seedAgentEdge(t, s, tid("delegator"), agent)
			} else {
				ensureActiveUser(t, s, tid("delegator"))
				ensureStandingRoot(t, s, agent.ProjectID, tid("delegator"))
			}
			row := seedAssignment(t, s, agent, tid("delegator"), "sa-soft-1")

			softDeleteForTest(t, srv, agent.ID)
			assert.Empty(t, activeAssignments(t, s, agent.ID))
			assert.EqualValues(t, 1, auditSummary(t, s, mutationTypeAgentSoftDelete, agent.ID)["assignments_deactivated"])

			rec := restoreForTest(t, srv, agent.ID)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			rows := activeAssignments(t, s, agent.ID)
			require.Len(t, rows, 1, "exactly the soft-deleted assignment returns")
			assert.Equal(t, row.ID, rows[0].ID)
			assert.EqualValues(t, 1, auditSummary(t, s, mutationTypeAgentRestore, agent.ID)["assignments_reactivated"])
		})
	}
}

func TestSAParentCeiling_HardDeleteDeactivatesAssignment(t *testing.T) {
	srv, s, _, _ := engineTestServer(t)
	agent := setupBrokerAgentInPhase(t, s, "sa-hard", state.PhaseStopped)
	seedAgentEdge(t, s, tid("delegator"), agent)
	seedAssignment(t, s, agent, tid("delegator"), "sa-hard-1")
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.True(t, agentGone(t, s, agent.ID))
	assert.Empty(t, activeAssignments(t, s, agent.ID))
	assert.EqualValues(t, 1, auditSummary(t, s, mutationTypeAgentHardDelete, agent.ID)["assignments_deactivated"])
}

func TestSAParentCeiling_ProjectDeleteDeactivatesAssignments(t *testing.T) {
	_, s := testServer(t)
	agent := setupBrokerAgentInPhase(t, s, "sa-proj-del", state.PhaseStopped)
	seedAssignment(t, s, agent, tid("delegator"), "sa-proj-del-1")
	require.NoError(t, s.WithTx(context.Background(), func(tx store.Store) error {
		_, _, err := deactivateProjectAgentEdges(context.Background(), tx, agent.ProjectID, time.Now())
		return err
	}))
	assert.Empty(t, activeAssignments(t, s, agent.ID))
}

// reincarnateClaimFor claims agent for a reincarnation directly, with sa as
// the assignment replacement (nil keeps it).
func reincarnateClaimFor(t *testing.T, srv *Server, agent *store.Agent, sa *store.AgentServiceAccountAssignment) error {
	t.Helper()
	now := time.Now()
	agent.ReincarnationUpdatedAt = &now
	rec := &store.AgentReincarnation{
		AgentID: agent.ID, FromGeneration: agent.Generation, ToGeneration: agent.Generation + 1,
		State: store.AgentReincarnationStatePending, PreviousAppliedConfig: agent.AppliedConfig,
	}
	return srv.reincarnateClaimTx(context.Background(), agent, rec, nil, sa, AuditActor{})
}

func TestSAParentCeiling_ReincarnateKeepsAssignment(t *testing.T) {
	srv, s, project, broker := setupReincarnateTestServer(t, newReincarnateTestDispatcher())
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	seedAgentEdge(t, s, tid("delegator"), agent)
	row := seedAssignment(t, s, agent, tid("delegator"), "sa-reinc-1")
	require.NoError(t, reincarnateClaimFor(t, srv, agent, nil))
	rows := activeAssignments(t, s, agent.ID)
	require.Len(t, rows, 1)
	assert.Equal(t, row.ID, rows[0].ID)
}

func TestSAParentCeiling_ReincarnateWithServiceAccountReplacesAssignment(t *testing.T) {
	srv, s, project, broker := setupReincarnateTestServer(t, newReincarnateTestDispatcher())
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	seedAgentEdge(t, s, tid("delegator"), agent)
	seedAssignment(t, s, agent, tid("delegator"), "sa-reinc-old")
	next := &store.AgentServiceAccountAssignment{
		ServiceAccountID:    "sa-reinc-new",
		Origin:              store.SAAssignmentOriginReincarnate,
		AuthorityProvenance: recordedProv(store.DelegationPrincipalUser, tid("requester"), store.SourceCredentialSession),
		EffectCeiling:       store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
	}
	require.NoError(t, reincarnateClaimFor(t, srv, agent, next))
	rows := activeAssignments(t, s, agent.ID)
	require.Len(t, rows, 1)
	assert.Equal(t, "sa-reinc-new", rows[0].ServiceAccountID)
	assert.Equal(t, store.SAAssignmentOriginReincarnate, rows[0].Origin)
	assert.Equal(t, tid("requester"), rows[0].SourcePrincipalID)
	assert.Equal(t, true, auditSummary(t, s, mutationTypeAgentReincarnateClaim, agent.ID)["service_account_recorded"])

	// An assignment without recorded provenance is refused before any write.
	agent2 := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.ID = tid("reinc-unrecorded-" + t.Name())
		a.Slug = "reinc-unrecorded-" + tidSlugSafe(t.Name())
	})
	err := reincarnateClaimFor(t, srv, agent2, &store.AgentServiceAccountAssignment{ServiceAccountID: "sa-x"})
	assert.ErrorIs(t, err, errAgentCreateWriteInvalid)
}

// TestSAParentCeiling_ReincarnateClaimRefusedWritesNoAssignment: a
// reincarnation claim that is refused, because a reincarnation is already
// in flight (store.ErrClaimPredicate) or the row moved on
// (store.ErrVersionConflict), records no assignment for its service
// account. The previous assignment stays the agent's only active row.
func TestSAParentCeiling_ReincarnateClaimRefusedWritesNoAssignment(t *testing.T) {
	for _, tc := range []struct {
		name string
		// refuse makes the claim on the agent fail and returns the agent
		// as the claim reads it.
		refuse func(t *testing.T, s store.Store, agent *store.Agent) *store.Agent
		want   error
	}{
		{
			name: "reincarnation in flight",
			refuse: func(t *testing.T, s store.Store, agent *store.Agent) *store.Agent {
				cur := mustGetAgent(t, s, agent.ID)
				cur.ReincarnationState = store.ReincarnationStateStopping
				require.NoError(t, s.UpdateAgent(context.Background(), cur))
				cur = mustGetAgent(t, s, agent.ID)
				require.Equal(t, store.ReincarnationStateStopping, cur.ReincarnationState)
				return cur
			},
			want: store.ErrClaimPredicate,
		},
		{
			name: "stale state version",
			refuse: func(t *testing.T, s store.Store, agent *store.Agent) *store.Agent {
				cur := mustGetAgent(t, s, agent.ID)
				cur.StateVersion--
				return cur
			},
			want: store.ErrVersionConflict,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, project, broker := setupReincarnateTestServer(t, newReincarnateTestDispatcher())
			agent := newReincarnateTestAgent(t, s, project, broker, nil)
			seedAgentEdge(t, s, tid("delegator"), agent)
			old := seedAssignment(t, s, agent, tid("delegator"), "sa-reinc-refused-old")
			agent = tc.refuse(t, s, agent)

			next := &store.AgentServiceAccountAssignment{
				ServiceAccountID:    "sa-reinc-refused-new",
				Origin:              store.SAAssignmentOriginReincarnate,
				AuthorityProvenance: recordedProv(store.DelegationPrincipalUser, tid("requester"), store.SourceCredentialSession),
				EffectCeiling:       store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
			}
			require.ErrorIs(t, reincarnateClaimFor(t, srv, agent, next), tc.want)

			rows := activeAssignments(t, s, agent.ID)
			require.Len(t, rows, 1, "no new assignment row")
			assert.Equal(t, old.ID, rows[0].ID, "the previous assignment stays active")
			assert.Equal(t, "sa-reinc-refused-old", rows[0].ServiceAccountID)
		})
	}
}

// TestSAParentCeiling_ReincarnateRequestRecordsRequester drives a
// reincarnation with a service account through the HTTP handler. The
// requester, not the agent's creator, is recorded as the source of the new
// assignment, under the requester's ceiling; a fault computing that ceiling
// answers 503 with nothing claimed.
func TestSAParentCeiling_ReincarnateRequestRecordsRequester(t *testing.T) {
	t.Run("session requester", func(t *testing.T) {
		disp := newReincarnateTestDispatcher()
		srv, s, project, broker := setupReincarnateTestServer(t, disp)
		agent := newReincarnateTestAgent(t, s, project, broker, nil)
		seedAgentEdge(t, s, tid("delegator"), agent)
		seedAssignment(t, s, agent, tid("delegator"), "sa-reinc-http-old")
		user := newReincarnateAuthzUser(t, s, "sa-reinc-http")
		grantAgentLifecycleAtProject(t, s, user.ID, project.ID)
		grantAgentDelegationAtProject(t, s, user.ID, project.ID)
		grantPermissionViaRoleBinding(t, s, user.ID, "agent.update", store.RoleScopeProject, project.ID)
		sa := patchTestSA(t, s, project.ID, true, user.ID)
		require.NotEqual(t, user.ID, agent.CreatedBy, "precondition: the requester is not the creator")

		requester := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "web")
		rec := httptest.NewRecorder()
		srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, requester,
			ReincarnateAgentRequest{Handoff: "h", ServiceAccount: sa.ID}), agent.ID)
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
		settled := waitForReincarnationSettled(t, s, agent.ID)
		assert.Equal(t, store.AgentReincarnationStateCompleted, settled.State)

		rows := activeAssignments(t, s, agent.ID)
		require.Len(t, rows, 1, "the new account's assignment replaces the old one")
		row := rows[0]
		assert.Equal(t, sa.ID, row.ServiceAccountID)
		assert.Equal(t, store.SAAssignmentOriginReincarnate, row.Origin)
		assert.Equal(t, store.DelegationPrincipalUser, row.SourcePrincipalKind)
		assert.Equal(t, user.ID, row.SourcePrincipalID, "the requester is the source")
		assert.NotEqual(t, agent.CreatedBy, row.SourcePrincipalID, "the creator is not the source")
		assert.Equal(t, store.SourceCredentialSession, row.SourceCredentialKind)
		assertCeilingFromSource(t, srv, requester, row.EffectCeiling)
		assert.Equal(t, sa.ID, agentAssignedServiceAccountID(mustGetAgent(t, s, agent.ID)))
	})

	t.Run("source ceiling fault", func(t *testing.T) {
		disp := newReincarnateTestDispatcher()
		srv, s, project, broker := setupReincarnateTestServer(t, disp)
		agent := newReincarnateTestAgent(t, s, project, broker, saAssigningAgent(t, s, project.ID))
		sa := patchTestSA(t, s, project.ID, true, "someone")
		self := agentIdentityFor(agent.ID, project.ID,
			append(ScopesForRole(AgentRoleBaseline), ScopeAgentLifecycle, ScopeAgentSAAssign)...)
		// The requester's ceiling reads its own assignment rows; that read
		// fails.
		saved := srv.authzService.store
		srv.authzService.store = &mintAssignErrStore{Store: saved}
		t.Cleanup(func() { srv.authzService.store = saved })

		// Every check before the claim passes under the fault: a dry run is
		// planned.
		rec := httptest.NewRecorder()
		srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self,
			ReincarnateAgentRequest{DryRun: true, ServiceAccount: sa.ID}), agent.ID)
		require.Equal(t, http.StatusOK, rec.Code, "precondition: %s", rec.Body.String())

		before := snapshotAgent(t, s, agent.ID)
		rec = httptest.NewRecorder()
		srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self,
			ReincarnateAgentRequest{ServiceAccount: sa.ID}), agent.ID)
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
		assertAgentUntouched(t, s, disp, agent.ID, before)
		assert.Empty(t, activeAssignments(t, s, agent.ID), "nothing is recorded")
	})
}

// TestSAParentCeiling_ReincarnateFailedRestoreWithholdsUntilReassigned pins
// the behaviour when a reincarnation that replaced the assignment fails and
// the previous configuration is restored: the restored account differs
// from the active assignment, so its token scope is withheld and the
// parent ceiling denies until the account is assigned again.
func TestSAParentCeiling_ReincarnateFailedRestoreWithholdsUntilReassigned(t *testing.T) {
	f := newSPCFixture(t)
	f.record(t, nil)
	assertParentCeilingAllowed(t, f.eval(t))

	// The claim replaced the assignment with the next generation's account;
	// the agent's applied configuration is the restored previous one.
	f.record(t, func(a *store.AgentServiceAccountAssignment) {
		a.ServiceAccountID = "sa-next-generation"
		a.Origin = store.SAAssignmentOriginReincarnate
	})
	assertParentCeilingDenied(t, f.eval(t), DenyCauseCeilingProvenanceStale)
	agent := mustGetAgent(t, f.s, f.agent.ID)
	scopes, err := f.srv.authzService.ceilingFilteredAgentScopes(context.Background(), agent, f.srv.authzService.mintCandidateScopes(agent))
	require.NoError(t, err)
	assert.NotContains(t, scopes, GCPTokenScopeForSA(f.sa.ID))

	// Assigning the account again restores both.
	f.record(t, nil)
	assertParentCeilingAllowed(t, f.eval(t))
}

// ---- create compensation runs the hard-delete work ---------------------------

func compensationAgent(t *testing.T, s store.Store) *store.Agent {
	t.Helper()
	agent := setupBrokerAgentInPhase(t, s, "sa-comp", state.PhaseStopped)
	seedAgentEdge(t, s, tid("delegator"), agent)
	seedAssignment(t, s, agent, tid("delegator"), "sa-comp-1")
	return agent
}

func TestCompensateAgentCreate_RunsHardDeleteHooks(t *testing.T) {
	srv, s := testServer(t)
	agent := compensationAgent(t, s)
	var log hookLog
	var seen []string
	srv.RegisterHardDeleteHook("grants", func(_ context.Context, tx store.Store, a *store.Agent, _ AuditActor) error {
		log.add("grants")
		seen = append(seen, a.ID)
		rows, err := tx.GetActiveAgentServiceAccountAssignments(context.Background(), a.ID)
		require.NoError(t, err)
		assert.Empty(t, rows, "assignments are deactivated before the hooks")
		return nil
	})
	srv.RegisterSoftDeleteHook("soft", recordingHook(&log, "soft", nil, nil))
	require.NoError(t, srv.compensateAgentCreate(context.Background(), createCompensation{
		Agent: agent, Stage: createStageDispatch,
	}))
	assert.Equal(t, []string{"grants"}, log.get(), "hard-delete hooks run on compensation; soft hooks do not")
	assert.Equal(t, []string{agent.ID}, seen)
	assert.True(t, agentGone(t, s, agent.ID))
}

func TestCompensateAgentCreate_DeactivatesAssignment(t *testing.T) {
	srv, s := testServer(t)
	agent := compensationAgent(t, s)
	require.NoError(t, srv.compensateAgentCreate(context.Background(), createCompensation{Agent: agent, Stage: createStageDispatch, OpID: "op-comp"}))
	assert.Empty(t, activeAssignments(t, s, agent.ID))
	assert.Empty(t, activeEdgeIDs(t, s, agent.ID))
	// The assignment was deactivated under the compensation's op ID and
	// cause: it can be selected by them.
	n, err := s.ReactivateAgentServiceAccountAssignments(context.Background(), agent.ID, store.EdgeDeactivationCreateCompensation, "op-comp")
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}

func TestCompensateAgentCreate_HookErrorRollsBack(t *testing.T) {
	srv, s := testServer(t)
	agent := compensationAgent(t, s)
	srv.RegisterHardDeleteHook("failing", recordingHook(&hookLog{}, "failing", nil, errSPCInjected))
	err := srv.compensateAgentCreate(context.Background(), createCompensation{Agent: agent, Stage: createStageDispatch})
	require.ErrorIs(t, err, errSPCInjected)
	assert.False(t, agentGone(t, s, agent.ID), "the row delete rolls back")
	assert.Len(t, activeAssignments(t, s, agent.ID), 1, "the assignment stays active")
	assert.NotEmpty(t, activeEdgeIDs(t, s, agent.ID), "the edge stays active")
}

func TestCompensateAgentCreate_RepeatRunsNoHook(t *testing.T) {
	srv, s := testServer(t)
	agent := compensationAgent(t, s)
	var log hookLog
	srv.RegisterHardDeleteHook("grants", recordingHook(&log, "grants", nil, nil))
	require.NoError(t, srv.compensateAgentCreate(context.Background(), createCompensation{Agent: agent, Stage: createStageDispatch}))
	// The row is gone, so the repeat changes nothing and reports that the
	// row is left to a delete.
	err := srv.compensateAgentCreate(context.Background(), createCompensation{Agent: agent, Stage: createStageDispatch})
	require.ErrorIs(t, err, errCreateRowDeleteHeld)
	assert.Equal(t, []string{"grants"}, log.get(), "a repeated compensation that changes nothing runs no hook")
	assert.Len(t, agentAudits(t, s, mutationTypeAgentCreateDispatchFailed, agent.ID), 1)
}
