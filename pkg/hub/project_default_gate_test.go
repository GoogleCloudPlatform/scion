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
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// P10 — Project-Default SA Assignment Gate
//
// Design §4.5 (ruled by ptone): project-default assignment checks the
// immediate agent creator. The principal is:
//   - for a human-created agent: the human creator;
//   - for agent-creates-agent: the creating agent's assigned SA.
//
// This replaces the former "NO authorization check here, deliberately and
// by ruling (P4 item F)" approach. P10 changes the ruling: the project
// operator selected an available default, but did not grant every future
// creator permission to act as it.
// =============================================================================

// ---------------------------------------------------------------------------
// AC1: Project-default assigned SA emits SurfaceProjectDefault audit record
// ---------------------------------------------------------------------------

func TestProjectDefaultGate_EmitsSurfaceProjectDefaultAudit(t *testing.T) {
	srv, s, project, audit := auditingServer(t)
	sa := wiringSA(t, s, store.ScopeProject, project.ID, "p10-audit@p.iam.gserviceaccount.com")

	ctx := context.Background()
	if project.Annotations == nil {
		project.Annotations = map[string]string{}
	}
	project.Annotations[projectSettingDefaultGCPIdentityMode] = store.GCPMetadataModeAssign
	project.Annotations[projectSettingDefaultGCPIdentitySAID] = sa.ID
	require.NoError(t, s.UpdateProject(ctx, project))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "p10-audit-agent",
		ProjectID: project.ID,
		Task:      "audit test",
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	ev := onlySAEvent(t, audit)
	assert.Equal(t, store.SAAssignmentDecision, ev.Type,
		"P10: project-default produces a decision record via authorizeSAAssignment")
	assert.Equal(t, SurfaceProjectDefault, ev.Surface,
		"the audit record must be labelled as project-default")
	assert.Equal(t, sa.ID, ev.TargetSAID)
	assert.Equal(t, sa.Email, ev.TargetSAEmail)
	require.NotNil(t, ev.Decision)
	assert.Equal(t, store.ActAsAllowed, *ev.Decision)
}

// ---------------------------------------------------------------------------
// AC2: Creator without actAs is denied when default applies
// ---------------------------------------------------------------------------

func TestProjectDefaultGate_CreatorWithoutActAsDenied(t *testing.T) {
	f := bypassAgentsSetup(t)
	sa := bypassAgentsCreateSA(t, f, f.proj.ID, true)

	// Set the project default SA
	ctx := context.Background()
	proj, err := f.store.GetProject(ctx, f.proj.ID)
	require.NoError(t, err)
	if proj.Annotations == nil {
		proj.Annotations = map[string]string{}
	}
	proj.Annotations[projectSettingDefaultGCPIdentityMode] = store.GCPMetadataModeAssign
	proj.Annotations[projectSettingDefaultGCPIdentitySAID] = sa.ID
	require.NoError(t, f.store.UpdateProject(ctx, proj))

	// Enforce actAs with a checker that denies this SA
	enforceSAAssign(f.srv, store.NewFakeCallerPermissionChecker().DenyTarget(sa.Email, "no actAs grant for this caller"))

	// Create agent as owner — should be denied because actAs check fails
	rec := createAgentAsOwner(t, f, CreateAgentRequest{Name: "p10-denied-agent"})
	require.Equal(t, http.StatusForbidden, rec.Code,
		"P10: creator without actAs must be denied when project default applies; got: %s",
		rec.Body.String())
}

// ---------------------------------------------------------------------------
// AC3: Creator with actAs succeeds
// ---------------------------------------------------------------------------

func TestProjectDefaultGate_CreatorWithActAsSucceeds(t *testing.T) {
	f := bypassAgentsSetup(t)
	sa := bypassAgentsCreateSA(t, f, f.proj.ID, true)

	// Set the project default SA
	ctx := context.Background()
	proj, err := f.store.GetProject(ctx, f.proj.ID)
	require.NoError(t, err)
	if proj.Annotations == nil {
		proj.Annotations = map[string]string{}
	}
	proj.Annotations[projectSettingDefaultGCPIdentityMode] = store.GCPMetadataModeAssign
	proj.Annotations[projectSettingDefaultGCPIdentitySAID] = sa.ID
	require.NoError(t, f.store.UpdateProject(ctx, proj))

	// Enforce actAs with a checker that allows this SA
	enforceSAAssign(f.srv, store.NewFakeCallerPermissionChecker().AllowTarget(sa.Email))

	// Create agent as owner — should succeed
	rec := createAgentAsOwner(t, f, CreateAgentRequest{Name: "p10-allowed-agent"})
	require.Equal(t, http.StatusCreated, rec.Code,
		"P10: creator with actAs must succeed when project default applies; got: %s",
		rec.Body.String())

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)

	got, err := f.store.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)
	require.NotNil(t, got.AppliedConfig)
	require.NotNil(t, got.AppliedConfig.GCPIdentity)
	assert.Equal(t, store.GCPMetadataModeAssign, got.AppliedConfig.GCPIdentity.MetadataMode)
	assert.Equal(t, sa.ID, got.AppliedConfig.GCPIdentity.ServiceAccountID)
	assert.Equal(t, sa.Email, got.AppliedConfig.GCPIdentity.ServiceAccountEmail)
}

// ---------------------------------------------------------------------------
// AC4: Hub-scoped default SA denied when mode=off
// ---------------------------------------------------------------------------

func TestProjectDefaultGate_HubScopedDefaultDeniedWhenModeOff(t *testing.T) {
	f := bypassAgentsSetup(t)
	// Mode=off (default) — hub-scoped SA assignment requires enforce
	sa := hubScopedSAForAgent(t, f, true)
	setProjectDefaultSA(t, f, sa.ID)

	// Even with hub membership, mode=off denies hub-scoped assignment (D4)
	ensureHubMembership(context.Background(), f.store, f.owner.ID)

	rec := createAgentAsOwner(t, f, CreateAgentRequest{Name: "p10-mode-off-agent"})
	require.Equal(t, http.StatusForbidden, rec.Code,
		"P10+D4: hub-scoped default SA must be denied when mode=off; got: %s",
		rec.Body.String())
	assert.Contains(t, rec.Body.String(), "gcpIamCheckMode=enforce",
		"denial must mention mode coupling")
}

// ---------------------------------------------------------------------------
// AC5: Explicit CLI assignment via gcp_identity works (tested at HTTP layer)
// ---------------------------------------------------------------------------

func TestProjectDefaultGate_ExplicitGCPIdentityOverridesDefault(t *testing.T) {
	srv, s, project, _ := auditingServer(t)

	// Set a default SA
	defaultSA := wiringSA(t, s, store.ScopeProject, project.ID, "p10-default@p.iam.gserviceaccount.com")
	ctx := context.Background()
	if project.Annotations == nil {
		project.Annotations = map[string]string{}
	}
	project.Annotations[projectSettingDefaultGCPIdentityMode] = store.GCPMetadataModeAssign
	project.Annotations[projectSettingDefaultGCPIdentitySAID] = defaultSA.ID
	require.NoError(t, s.UpdateProject(ctx, project))

	// Create a different SA for explicit assignment
	explicitSA := wiringSA(t, s, store.ScopeProject, project.ID, "p10-explicit@p.iam.gserviceaccount.com")

	// Explicitly specify gcp_identity — should use explicit SA, not the default
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "p10-explicit-agent",
		ProjectID: project.ID,
		Task:      "explicit test",
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode:     store.GCPMetadataModeAssign,
			ServiceAccountID: explicitSA.ID,
		},
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)

	got, err := s.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)
	require.NotNil(t, got.AppliedConfig)
	require.NotNil(t, got.AppliedConfig.GCPIdentity)
	assert.Equal(t, explicitSA.ID, got.AppliedConfig.GCPIdentity.ServiceAccountID,
		"explicit gcp_identity must override the project default")
	assert.NotEqual(t, defaultSA.ID, got.AppliedConfig.GCPIdentity.ServiceAccountID)
}

// ---------------------------------------------------------------------------
// AC6: Existing projects without defaults behave unchanged
// ---------------------------------------------------------------------------

func TestProjectDefaultGate_NoDefaultProjectUnchanged(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, _, project := setupCreateAgentServer(t, disp)

	// No project default set — agent should get block mode
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "p10-no-default",
		ProjectID: project.ID,
		Task:      "no default test",
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)

	ctx := context.Background()
	got, err := srv.store.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)
	require.NotNil(t, got.AppliedConfig)
	require.NotNil(t, got.AppliedConfig.GCPIdentity)
	assert.Equal(t, store.GCPMetadataModeBlock, got.AppliedConfig.GCPIdentity.MetadataMode,
		"no project default → block mode, unchanged from pre-P10 behavior")
}

// ---------------------------------------------------------------------------
// AC7: Agent-creates-agent uses the creating agent's SA for actAs check
// ---------------------------------------------------------------------------

func TestProjectDefaultGate_AgentCreatesAgent_UsesCreatingSA(t *testing.T) {
	f := bypassAgentsSetup(t)

	// Give the calling agent an assigned SA identity
	callerSAEmail := "caller-sa@proj.iam.gserviceaccount.com"
	callerSA := &store.GCPServiceAccount{
		ID:        tid("caller-sa"),
		Scope:     store.ScopeProject,
		ScopeID:   f.proj.ID,
		Email:     callerSAEmail,
		ProjectID: "gcp-proj",
		Verified:  true,
		CreatedBy: tid("someone"),
		CreatedAt: time.Now(),
	}
	require.NoError(t, f.store.CreateGCPServiceAccount(context.Background(), callerSA))

	// Update the caller agent's AppliedConfig to have the SA assigned
	f.caller.AppliedConfig = &store.AgentAppliedConfig{
		GCPIdentity: &store.GCPIdentityConfig{
			MetadataMode:        store.GCPMetadataModeAssign,
			ServiceAccountID:    callerSA.ID,
			ServiceAccountEmail: callerSAEmail,
		},
	}
	require.NoError(t, f.store.UpdateAgent(context.Background(), f.caller))

	// Set up the target SA as the project default
	targetSA := bypassAgentsCreateSA(t, f, f.proj.ID, true)
	ctx := context.Background()
	proj, err := f.store.GetProject(ctx, f.proj.ID)
	require.NoError(t, err)
	if proj.Annotations == nil {
		proj.Annotations = map[string]string{}
	}
	proj.Annotations[projectSettingDefaultGCPIdentityMode] = store.GCPMetadataModeAssign
	proj.Annotations[projectSettingDefaultGCPIdentitySAID] = targetSA.ID
	require.NoError(t, f.store.UpdateProject(ctx, proj))

	// Enforce actAs: allow the caller SA, deny everything else
	checker := store.NewFakeCallerPermissionChecker().AllowTarget(targetSA.Email)
	enforceSAAssign(f.srv, checker)

	// Agent creates an agent — the actAs check should use the CALLER agent's
	// SA (callerSAEmail), not the human creator's identity.
	rec := f.asAgent(t, http.MethodPost,
		"/api/v1/projects/"+f.proj.ID+"/agents",
		CreateAgentRequest{Name: "p10-child-agent"},
		ScopeAgentCreate)
	require.Equal(t, http.StatusCreated, rec.Code,
		"P10: agent-creates-agent with project default should succeed when caller SA has actAs; got: %s",
		rec.Body.String())

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)
	got, err := f.store.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)
	require.NotNil(t, got.AppliedConfig)
	require.NotNil(t, got.AppliedConfig.GCPIdentity)
	assert.Equal(t, targetSA.ID, got.AppliedConfig.GCPIdentity.ServiceAccountID,
		"project default SA should be applied")
}

// Agent-creates-agent denied when the creating agent has no SA (block mode).
func TestProjectDefaultGate_AgentCreatesAgent_NoSADenied(t *testing.T) {
	f := bypassAgentsSetup(t)

	// The calling agent has no SA (default block mode)
	targetSA := bypassAgentsCreateSA(t, f, f.proj.ID, true)
	ctx := context.Background()
	proj, err := f.store.GetProject(ctx, f.proj.ID)
	require.NoError(t, err)
	if proj.Annotations == nil {
		proj.Annotations = map[string]string{}
	}
	proj.Annotations[projectSettingDefaultGCPIdentityMode] = store.GCPMetadataModeAssign
	proj.Annotations[projectSettingDefaultGCPIdentitySAID] = targetSA.ID
	require.NoError(t, f.store.UpdateProject(ctx, proj))

	// Enforce actAs — the agent has no SA, so callerPrincipal returns a
	// principal with no GCP identity, and EvaluateActAs denies.
	checker := store.NewFakeCallerPermissionChecker().AllowTarget(targetSA.Email)
	enforceSAAssign(f.srv, checker)

	rec := f.asAgent(t, http.MethodPost,
		"/api/v1/projects/"+f.proj.ID+"/agents",
		CreateAgentRequest{Name: "p10-child-denied"},
		ScopeAgentCreate)
	require.Equal(t, http.StatusForbidden, rec.Code,
		"P10: agent without SA must be denied when project default requires actAs; got: %s",
		rec.Body.String())
}

// ---------------------------------------------------------------------------
// Error handling: project-default SA failure surfaces an error
// ---------------------------------------------------------------------------

func TestProjectDefaultGate_UnreachableSAFailsCreate(t *testing.T) {
	f := bypassAgentsSetup(t)
	// Set a non-existent SA ID as the project default
	ctx := context.Background()
	proj, err := f.store.GetProject(ctx, f.proj.ID)
	require.NoError(t, err)
	if proj.Annotations == nil {
		proj.Annotations = map[string]string{}
	}
	proj.Annotations[projectSettingDefaultGCPIdentityMode] = store.GCPMetadataModeAssign
	proj.Annotations[projectSettingDefaultGCPIdentitySAID] = "nonexistent-sa-id"
	require.NoError(t, f.store.UpdateProject(ctx, proj))

	rec := createAgentAsOwner(t, f, CreateAgentRequest{Name: "p10-missing-default"})
	require.Equal(t, http.StatusBadRequest, rec.Code,
		"P10: missing project-default SA must fail agent creation; got: %s",
		rec.Body.String())
	assert.Contains(t, rec.Body.String(), "project default GCP service account is not available",
		"error must mention project default")
}

func TestProjectDefaultGate_UnverifiedSAFailsCreate(t *testing.T) {
	f := bypassAgentsSetup(t)
	// Create an unverified SA
	sa := bypassAgentsCreateSA(t, f, f.proj.ID, false)

	ctx := context.Background()
	proj, err := f.store.GetProject(ctx, f.proj.ID)
	require.NoError(t, err)
	if proj.Annotations == nil {
		proj.Annotations = map[string]string{}
	}
	proj.Annotations[projectSettingDefaultGCPIdentityMode] = store.GCPMetadataModeAssign
	proj.Annotations[projectSettingDefaultGCPIdentitySAID] = sa.ID
	require.NoError(t, f.store.UpdateProject(ctx, proj))

	rec := createAgentAsOwner(t, f, CreateAgentRequest{Name: "p10-unverified-default"})
	require.Equal(t, http.StatusBadRequest, rec.Code,
		"P10: unverified project-default SA must fail agent creation; got: %s",
		rec.Body.String())
	assert.Contains(t, rec.Body.String(), "not verified",
		"error must indicate the SA is not verified")
}

// ---------------------------------------------------------------------------
// Hubclient GCPIdentityConfig is correctly marshaled/unmarshaled
// ---------------------------------------------------------------------------

func TestGCPIdentityConfig_JSONRoundTrip(t *testing.T) {
	req := CreateAgentRequest{
		Name:      "json-test",
		ProjectID: "test-project",
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode:     store.GCPMetadataModeAssign,
			ServiceAccountID: "test-sa-id",
		},
	}

	data, err := json.Marshal(req)
	require.NoError(t, err)

	var decoded CreateAgentRequest
	require.NoError(t, json.Unmarshal(data, &decoded))
	require.NotNil(t, decoded.GCPIdentity)
	assert.Equal(t, store.GCPMetadataModeAssign, decoded.GCPIdentity.MetadataMode)
	assert.Equal(t, "test-sa-id", decoded.GCPIdentity.ServiceAccountID)
}

// ---------------------------------------------------------------------------
// Passthrough default mode should still work without authorization gate
// ---------------------------------------------------------------------------

func TestProjectDefaultGate_PassthroughDefaultNoGate(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)

	ctx := context.Background()
	if project.Annotations == nil {
		project.Annotations = map[string]string{}
	}
	project.Annotations[projectSettingDefaultGCPIdentityMode] = store.GCPMetadataModePassthrough
	require.NoError(t, s.UpdateProject(ctx, project))

	// Note: Passthrough default does not have a SA to gate on.
	// authorizePassthroughIdentity is only called for explicit passthrough
	// requests, not for the project default passthrough mode. The project
	// default just sets the mode; the passthrough actAs check (P8) runs
	// separately when a broker is involved.
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "p10-passthrough-default",
		ProjectID: project.ID,
		Task:      "passthrough test",
	})
	require.Equal(t, http.StatusCreated, rec.Code,
		"passthrough default should succeed without SA gate; got: %s", rec.Body.String())

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)

	got, err := s.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)
	require.NotNil(t, got.AppliedConfig)
	require.NotNil(t, got.AppliedConfig.GCPIdentity)
	assert.Equal(t, store.GCPMetadataModePassthrough, got.AppliedConfig.GCPIdentity.MetadataMode)
}
