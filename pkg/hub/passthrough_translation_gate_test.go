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
	"net/http"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const ptgHostSA = "broker-host@ptg-project.iam.gserviceaccount.com"

// ptgFixture is a cloudrun-sandbox broker server whose project defaults to
// passthrough.
type ptgFixture struct {
	srv     *Server
	store   store.Store
	owner   *store.User // project owner and broker owner
	project *store.Project
	broker  *store.RuntimeBroker
	audit   *mockAuditLogger
}

func setupPTG(t *testing.T, name, hostSA string) *ptgFixture {
	t.Helper()
	owner := ptUser(tid(name+"-owner"), name+"-owner@test.com", store.UserRoleMember)
	srv, s, project, broker := setupPassthroughSandboxServer(t, owner, hostSA, "ptg-project")
	f := &ptgFixture{srv: srv, store: s, owner: owner, project: project, broker: broker}
	ctx := context.Background()
	proj, err := s.GetProject(ctx, project.ID)
	require.NoError(t, err)
	if proj.Annotations == nil {
		proj.Annotations = map[string]string{}
	}
	proj.Annotations[projectSettingDefaultGCPIdentityMode] = store.GCPMetadataModePassthrough
	require.NoError(t, s.UpdateProject(ctx, proj))
	f.project = proj
	f.audit = &mockAuditLogger{}
	srv.SetAuditLogger(f.audit)
	return f
}

// nonOwnerCreator returns a project member who is neither the broker owner
// nor a hub admin. The broker is set to auto-provide so it stays
// dispatchable for that user. (The broker's owner is fixed at creation:
// UpdateRuntimeBroker does not write CreatedBy.)
func (f *ptgFixture) nonOwnerCreator(t *testing.T, name string) *store.User {
	t.Helper()
	id := tid(name + "-project-member")
	createTestUserWithProjectRole(t, f.store, id, name+"-project-member@test.com", f.project.ID, store.ProjectRoleMember)
	ensureHubMembership(context.Background(), f.store, id)
	b, err := f.store.GetRuntimeBroker(context.Background(), f.broker.ID)
	require.NoError(t, err)
	require.Equal(t, f.owner.ID, b.CreatedBy, "the broker stays owned by the project owner")
	b.AutoProvide = true
	require.NoError(t, f.store.UpdateRuntimeBroker(context.Background(), b))
	u, err := f.store.GetUser(context.Background(), id)
	require.NoError(t, err)
	return u
}

func (f *ptgFixture) createDefault(t *testing.T, name string) (int, string) {
	t.Helper()
	return f.createDefaultAs(t, f.owner, name)
}

func (f *ptgFixture) createDefaultAs(t *testing.T, as *store.User, name string) (int, string) {
	t.Helper()
	rec := doRequestAsUser(t, f.srv, as, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: name, ProjectID: f.project.ID, Task: "test",
	})
	return rec.Code, rec.Body.String()
}

func (f *ptgFixture) hostSARecords(t *testing.T, email string) []store.GCPServiceAccount {
	t.Helper()
	rows, err := f.store.ListGCPServiceAccounts(context.Background(), store.GCPServiceAccountFilter{Email: email})
	require.NoError(t, err)
	return rows
}

func (f *ptgFixture) agentMissing(t *testing.T, slug string) {
	t.Helper()
	_, err := f.store.GetAgentBySlug(context.Background(), f.project.ID, slug)
	assert.ErrorIs(t, err, store.ErrNotFound, "a refused create stores no agent")
}

func TestPassthroughTranslation_ProjectDefault_NonOwnerDenied_Create(t *testing.T) {
	f := setupPTG(t, "ptg-nonowner", ptgHostSA)
	creator := f.nonOwnerCreator(t, "ptg-nonowner")
	enforceSAAssign(f.srv, store.NewFakeCallerPermissionChecker().AllowTarget(ptgHostSA))

	code, body := f.createDefaultAs(t, creator, "ptg-nonowner-agent")

	assert.Equal(t, http.StatusForbidden, code, "body: %s", body)
	assert.Contains(t, body, "requires broker ownership")
	assert.Empty(t, f.hostSARecords(t, ptgHostSA), "a refused translation creates no record")
	f.agentMissing(t, "ptg-nonowner-agent")
}

func TestPassthroughTranslation_ProjectDefault_OwnerLacksActAs_Denied_Create(t *testing.T) {
	f := setupPTG(t, "ptg-noactas", ptgHostSA)
	checker := store.NewFakeCallerPermissionChecker().DenyTarget(ptgHostSA, "no actAs grant")
	enforceSAAssign(f.srv, checker)

	code, body := f.createDefault(t, "ptg-noactas-agent")

	assert.Equal(t, http.StatusForbidden, code, "body: %s", body)
	assert.Contains(t, body, store.PermissionActAs)
	assert.Equal(t, 1, checker.CallCount())
	assert.Empty(t, f.hostSARecords(t, ptgHostSA))
	f.agentMissing(t, "ptg-noactas-agent")
}

func TestPassthroughTranslation_ProjectDefault_OwnerWithActAs_Allowed_Create(t *testing.T) {
	f := setupPTG(t, "ptg-allow", ptgHostSA)
	checker := store.NewFakeCallerPermissionChecker().AllowTarget(ptgHostSA)
	enforceSAAssign(f.srv, checker)

	code, body := f.createDefault(t, "ptg-allow-agent")

	require.Equal(t, http.StatusCreated, code, "body: %s", body)
	agent := agentBySlug(t, f.store, f.project.ID, "ptg-allow-agent")
	require.NotNil(t, agent.AppliedConfig.GCPIdentity)
	assert.Equal(t, store.GCPMetadataModeAssign, agent.AppliedConfig.GCPIdentity.MetadataMode)
	assert.Equal(t, ptgHostSA, agent.AppliedConfig.GCPIdentity.ServiceAccountEmail)
	calls := checker.Calls()
	require.Len(t, calls, 1)
	assert.Equal(t, f.owner.ID, calls[0].Caller.ID, "actAs is evaluated for the creator")
}

// TestPassthroughTranslation_ProjectDefault_AgentCreatorDenied_Create: an
// agent principal on ctx never triggers translation. This is the single
// enforcement point for agents creating agents and for agent-owned
// schedules alike: both reach translatePassthroughForSandbox with the agent
// principal on ctx.
func TestPassthroughTranslation_ProjectDefault_AgentCreatorDenied_Create(t *testing.T) {
	f := setupPTG(t, "ptg-agentcreator", ptgHostSA)
	checker := store.NewFakeCallerPermissionChecker().AllowTarget(ptgHostSA)
	enforceSAAssign(f.srv, checker)

	agentCtx := contextWithIdentity(context.Background(), &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: tid("ptg-creator-agent")},
		ProjectID: f.project.ID,
	}})
	agent := &store.Agent{
		ID: tid("ptg-child"), Name: "ptg-child", ProjectID: f.project.ID,
		AppliedConfig: &store.AgentAppliedConfig{
			GCPIdentity: &store.GCPIdentityConfig{MetadataMode: store.GCPMetadataModePassthrough},
		},
	}

	err := f.srv.translatePassthroughForSandbox(agentCtx, nil, agent, f.broker.ID, f.project)

	var denied *passthroughTranslationDenied
	require.True(t, errors.As(err, &denied), "got %v", err)
	assert.Equal(t, http.StatusForbidden, denied.status)
	assert.Equal(t, store.GCPMetadataModePassthrough, agent.AppliedConfig.GCPIdentity.MetadataMode)
	assert.Equal(t, 0, checker.CallCount())
	assert.Empty(t, f.hostSARecords(t, ptgHostSA))
}

func TestPassthroughTranslation_ProjectDefault_Scheduled_NonOwnerFailsDispatch(t *testing.T) {
	f := setupPTG(t, "ptg-sched", ptgHostSA)
	creator := f.nonOwnerCreator(t, "ptg-sched")
	enforceSAAssign(f.srv, store.NewFakeCallerPermissionChecker().AllowTarget(ptgHostSA))
	ctx := context.Background()

	err := f.srv.dispatchAgentEventHandler()(ctx, withSessionRevision(store.ScheduledEvent{
		ID:        "evt-ptg-sched",
		ProjectID: f.project.ID,
		EventType: "dispatch_agent",
		Payload:   `{"agentName":"ptg-sched-agent","task":"scheduled work"}`,
		CreatedBy: creator.ID,
	}, creator.ID))

	require.Error(t, err, "a scheduled translation by a non-owner fails the dispatch")
	assert.Contains(t, err.Error(), "requires broker ownership")
	assert.Empty(t, f.hostSARecords(t, ptgHostSA))
	f.agentMissing(t, "ptg-sched-agent")
}

func TestPassthroughTranslation_Explicit_CallerLacksActAs_Denied(t *testing.T) {
	f := setupPTG(t, "ptg-explicit", ptgHostSA)
	checker := store.NewFakeCallerPermissionChecker().DenyTarget(ptgHostSA, "no actAs grant")
	enforceSAAssign(f.srv, checker)

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "ptg-explicit-agent", ProjectID: f.project.ID, Task: "test",
		GCPIdentity: &GCPIdentityAssignment{MetadataMode: "passthrough"},
	})

	assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, 1, checker.CallCount(), "the first gate refuses; translation never runs")
	assert.Empty(t, f.hostSARecords(t, ptgHostSA))
}

func TestPassthroughTranslation_Create_CheckModeOff_RefusedNoRecord(t *testing.T) {
	f := setupPTG(t, "ptg-create-off", ptgHostSA)
	require.Equal(t, SAAssignCheckOff, f.srv.saAssignCheckModeValue())

	for _, explicit := range []bool{true, false} {
		name := "ptg-create-off-default"
		req := CreateAgentRequest{Name: name, ProjectID: f.project.ID, Task: "test"}
		if explicit {
			name = "ptg-create-off-explicit"
			req.Name = name
			req.GCPIdentity = &GCPIdentityAssignment{MetadataMode: "passthrough"}
		}
		rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, "/api/v1/agents", req)

		assert.Equal(t, http.StatusForbidden, rec.Code, "explicit=%v body: %s", explicit, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "gcpIamCheckMode=enforce")
		f.agentMissing(t, name)
	}
	assert.Empty(t, f.hostSARecords(t, ptgHostSA), "off mode creates no hub-scoped record")
}

func TestPassthroughTranslation_Patch_CheckModeOff_Refused(t *testing.T) {
	f := setupPTG(t, "ptg-patch-off", ptgHostSA)
	agent := &store.Agent{
		ID: tid("ptg-patch-off-agent"), Slug: "ptg-patch-off-agent", Name: "ptg-patch-off-agent",
		ProjectID: f.project.ID, RuntimeBrokerID: f.broker.ID, Phase: string(state.PhaseCreated),
		CreatedBy: f.owner.ID, OwnerID: f.owner.ID, Created: time.Now(), Updated: time.Now(),
		AppliedConfig: &store.AgentAppliedConfig{GCPIdentity: &store.GCPIdentityConfig{MetadataMode: store.GCPMetadataModeBlock}},
	}
	require.NoError(t, f.store.CreateAgent(context.Background(), agent))

	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, "/api/v1/agents/"+agent.ID,
		map[string]interface{}{"gcp_identity": map[string]string{"metadata_mode": "passthrough"}})

	assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "gcpIamCheckMode=enforce")
	got := mustGetAgent(t, f.store, agent.ID)
	assert.Equal(t, store.GCPMetadataModeBlock, got.AppliedConfig.GCPIdentity.MetadataMode)
	assert.Empty(t, f.hostSARecords(t, ptgHostSA))
}

func TestPassthroughTranslation_AuditSurface(t *testing.T) {
	f := setupPTG(t, "ptg-audit", ptgHostSA)
	enforceSAAssign(f.srv, store.NewFakeCallerPermissionChecker().AllowTarget(ptgHostSA))

	code, body := f.createDefault(t, "ptg-audit-agent")

	require.Equal(t, http.StatusCreated, code, "body: %s", body)
	evs := saEventsOnSurface(f.audit, SurfacePassthroughTranslation)
	require.Len(t, evs, 1, "translation writes exactly one actAs decision record")
	assert.Equal(t, ptgHostSA, evs[0].TargetSAEmail)
	assert.Equal(t, store.SAAssignmentDecision, evs[0].Type)
}

// saEventsOnSurface returns the recorded SA events with the given surface.
func saEventsOnSurface(audit *mockAuditLogger, surface string) []*store.SAAssignmentEvent {
	var out []*store.SAAssignmentEvent
	for _, ev := range audit.saEvents {
		if ev.Surface == surface {
			out = append(out, ev)
		}
	}
	return out
}

func TestPassthroughTranslation_NoHostSA_StaysPassthrough(t *testing.T) {
	f := setupPTG(t, "ptg-nohost", "")

	code, body := f.createDefault(t, "ptg-nohost-agent")

	require.Equal(t, http.StatusCreated, code, "body: %s", body)
	agent := agentBySlug(t, f.store, f.project.ID, "ptg-nohost-agent")
	require.NotNil(t, agent.AppliedConfig.GCPIdentity)
	assert.Equal(t, store.GCPMetadataModePassthrough, agent.AppliedConfig.GCPIdentity.MetadataMode)
	assert.Empty(t, saEventsOnSurface(f.audit, SurfacePassthroughTranslation))
}

// The hub-default scheduled rung reaches the same translatePassthroughForSandbox
// call; it is covered by the single enforcement point pinned above rather
// than by a scenario of its own (hubDefaultPassthroughAllowed admits only
// docker and podman runtimes, so a sandbox profile does not reach it in a
// test unless that gate is set aside).
