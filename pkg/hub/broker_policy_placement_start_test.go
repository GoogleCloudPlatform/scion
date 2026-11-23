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
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setBrokerPolicyStateP4(t *testing.T, srv *Server, s store.Store, expOn, policyOn bool) {
	t.Helper()
	setBrokerPolicyStateP4WithOverrides(t, srv, s, expOn, policyOn, nil)
}

func setBrokerPolicyStateP4WithOverrides(t *testing.T, srv *Server, s store.Store, expOn, policyOn bool, extra map[string]bool) {
	t.Helper()
	ctx := context.Background()
	overrides := map[string]bool{
		experiments.AdminManagedBrokers: expOn,
	}
	for k, v := range extra {
		overrides[k] = v
	}
	expDoc, err := json.Marshal(map[string]any{
		"overrides": overrides,
	})
	require.NoError(t, err)
	_, err = s.UpsertHubSetting(ctx, "experiments", json.RawMessage(expDoc), "test", -1, "managed")
	require.NoError(t, err)

	policyDoc := json.RawMessage(`{"admin_managed_only":false}`)
	if policyOn {
		policyDoc = json.RawMessage(`{"admin_managed_only":true}`)
	}
	_, err = s.UpsertHubSetting(ctx, "broker_policy", policyDoc, "test", -1, "managed")
	require.NoError(t, err)

	ops := NewOperationalSettings(s, emptyKoanf(), emptyKoanf())
	_, err = ops.Refresh(ctx)
	require.NoError(t, err)
	ApplySnapshot(srv, ops.Snapshot())
	srv.SetOperationalSettings(ops)
}

func makeP4User(t *testing.T, s store.Store, slug, role string) *store.User {
	t.Helper()
	ctx := context.Background()
	u := &store.User{
		ID:          api.NewUUID(),
		Email:       slug + "@test.example",
		DisplayName: slug,
		Role:        role,
		Status:      store.UserStatusActive,
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, u))
	if role == store.UserRoleAdmin {
		grantSuperAdmin(t, s, u.ID)
	}
	return u
}

func makeP4Broker(t *testing.T, s store.Store, projectID, name, createdBy string, autoProvide, adminManaged bool) *store.RuntimeBroker {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	b := &store.RuntimeBroker{
		ID:            uuid.New().String(),
		Name:          name,
		Slug:          name,
		Status:        store.BrokerStatusOnline,
		LastHeartbeat: now,
		CreatedBy:     createdBy,
		AutoProvide:   autoProvide,
		AdminManaged:  adminManaged,
		Capabilities:  &store.BrokerCapabilities{AgentMove: true},
		Created:       now,
		Updated:       now,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, b))
	if projectID != "" {
		linkedBy := createdBy
		if autoProvide {
			linkedBy = autoProvideLinkedBy
		}
		require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
			ProjectID:  projectID,
			BrokerID:   b.ID,
			BrokerName: b.Name,
			Status:     store.BrokerStatusOnline,
			LinkedBy:   linkedBy,
		}))
	}
	return b
}

func decodePolicyError(t *testing.T, rec *httptest.ResponseRecorder) (code, msg string, details map[string]interface{}) {
	t.Helper()
	var body struct {
		Error struct {
			Code    string                 `json:"code"`
			Message string                 `json:"message"`
			Details map[string]interface{} `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), "raw body: %s", rec.Body.String())
	return body.Error.Code, body.Error.Message, body.Error.Details
}

func TestBrokerPolicy_Placement_DispatchAndImageOps(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   api.NewUUID(),
		Name: "P4 Placement Project",
		Slug: "p4-placement-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	owner := makeP4User(t, s, "p4-owner", store.UserRoleMember)
	superAdmin := makeP4User(t, s, "p4-superadmin", store.UserRoleAdmin)

	unmanaged := makeP4Broker(t, s, project.ID, "p4-unmanaged", owner.ID, false, false)
	approved := makeP4Broker(t, s, project.ID, "p4-approved", owner.ID, false, true)
	embedded := makeP4Broker(t, s, project.ID, "p4-embedded", "", false, false)
	autoProv := makeP4Broker(t, s, project.ID, "p4-autoprov", owner.ID, true, false)

	ownerCtx := contextWithIdentity(ctx, NewAuthenticatedUser(owner.ID, owner.Email, owner.DisplayName, owner.Role, "web"))
	adminCtx := contextWithIdentity(ctx, NewAuthenticatedUser(superAdmin.ID, superAdmin.Email, superAdmin.DisplayName, superAdmin.Role, "web"))
	agentIdent := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: api.NewUUID()},
		ProjectID: project.ID,
		Scopes:    []AgentTokenScope{ScopeAgentCreate},
	}}
	agentCtx := contextWithIdentity(ctx, agentIdent)

	// Policy off: unmanaged broker is allowed for owner, super-admin, and agent.
	setBrokerPolicyStateP4(t, srv, s, true, false)
	assert.True(t, srv.canDispatchToBroker(ownerCtx, unmanaged))
	assert.True(t, srv.canDispatchToBroker(adminCtx, unmanaged))
	assert.True(t, srv.canDispatchToBroker(agentCtx, unmanaged))
	assert.True(t, srv.canUseBrokerForProject(ownerCtx, unmanaged, project))
	assert.True(t, srv.canRunBrokerImageOps(ownerCtx, unmanaged))
	assert.True(t, srv.canRunBrokerImageOps(adminCtx, unmanaged))

	// Experiment off with policy setting true: enforcement stays inactive.
	setBrokerPolicyStateP4(t, srv, s, false, true)
	assert.True(t, srv.canDispatchToBroker(ownerCtx, unmanaged))
	assert.True(t, srv.canRunBrokerImageOps(adminCtx, unmanaged))

	// Policy on + experiment on: unmanaged broker is refused for every identity
	// including owner, super-admin (no exemption), and agent identity arm.
	setBrokerPolicyStateP4(t, srv, s, true, true)
	assert.False(t, srv.canDispatchToBroker(ownerCtx, unmanaged))
	assert.False(t, srv.canDispatchToBroker(adminCtx, unmanaged))
	assert.False(t, srv.canDispatchToBroker(agentCtx, unmanaged))
	assert.False(t, srv.canUseBrokerForProject(ownerCtx, unmanaged, project))
	assert.False(t, srv.canUseBrokerForProject(adminCtx, unmanaged, project))
	assert.False(t, srv.canUseBrokerForProject(agentCtx, unmanaged, project))
	assert.False(t, srv.canRunBrokerImageOps(ownerCtx, unmanaged))
	assert.False(t, srv.canRunBrokerImageOps(adminCtx, unmanaged))

	// checkBrokerDispatchAccess surfaces the §4.4 policy error (operation: dispatch).
	rec := httptest.NewRecorder()
	ok := srv.checkBrokerDispatchAccess(adminCtx, rec, unmanaged.ID, project)
	assert.False(t, ok)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	code, msg, details := decodePolicyError(t, rec)
	assert.Equal(t, ErrCodeBrokerPolicyAdminManagedOnly, code)
	assert.Contains(t, msg, brokerPolicySettingKey)
	assert.Contains(t, msg, unmanaged.Name)
	assert.Equal(t, brokerPolicySettingKey, details["setting"])
	assert.Equal(t, brokerPolicyOpDispatch, details["operation"])
	assert.Equal(t, unmanaged.ID, details["brokerId"])

	// Approved, embedded, and auto-provide brokers remain allowed.
	for _, b := range []*store.RuntimeBroker{approved, embedded, autoProv} {
		assert.True(t, srv.canUseBrokerForProject(ownerCtx, b, project), "broker %s should be usable by owner", b.Name)
		assert.True(t, srv.canUseBrokerForProject(adminCtx, b, project), "broker %s should be usable by super-admin", b.Name)
		assert.True(t, srv.canUseBrokerForProject(agentCtx, b, project), "broker %s should be usable by agent", b.Name)
		assert.True(t, srv.canRunBrokerImageOps(adminCtx, b), "broker %s should allow image ops", b.Name)

		allowRec := httptest.NewRecorder()
		assert.True(t, srv.checkBrokerDispatchAccess(adminCtx, allowRec, b.ID, project), "broker %s checkBrokerDispatchAccess", b.Name)
	}
}

func TestBrokerPolicy_Placement_SelectProjectRuntimeBroker(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	owner := makeP4User(t, s, "p4-sel-owner", store.UserRoleMember)
	ownerCtx := contextWithIdentity(ctx, NewAuthenticatedUser(owner.ID, owner.Email, owner.DisplayName, owner.Role, "web"))

	t.Run("single unmanaged provider returns policy error with 0 eligible", func(t *testing.T) {
		project := &store.Project{ID: api.NewUUID(), Name: "Sel Single", Slug: "sel-single-" + api.NewUUID()[:8]}
		require.NoError(t, s.CreateProject(ctx, project))
		unmanaged := makeP4Broker(t, s, project.ID, "sel-unmanaged-1", owner.ID, false, false)

		setBrokerPolicyStateP4(t, srv, s, true, true)
		allProviders, err := s.GetProjectProviders(ctx, project.ID)
		require.NoError(t, err)
		avail, err := srv.getAvailableBrokersForProject(ctx, project.ID)
		require.NoError(t, err)

		id, refusal := srv.selectProjectRuntimeBroker(ownerCtx, project, allProviders, avail)
		assert.Empty(t, id)
		assert.Contains(t, refusal, brokerPolicySettingKey)
		assert.Contains(t, refusal, "0 eligible")
		assert.Contains(t, refusal, unmanaged.Name)

		rec := httptest.NewRecorder()
		resolvedID, err := srv.resolveRuntimeBroker(ownerCtx, rec, "", project)
		assert.Error(t, err)
		assert.Empty(t, resolvedID)
		assert.Equal(t, http.StatusForbidden, rec.Code)
		code, msg, details := decodePolicyError(t, rec)
		assert.Equal(t, ErrCodeBrokerPolicyAdminManagedOnly, code)
		assert.Contains(t, msg, "0 eligible")
		assert.Equal(t, brokerPolicyOpDispatch, details["operation"])
		assert.Equal(t, unmanaged.ID, details["brokerId"])
		assert.Equal(t, float64(0), details["eligibleBrokers"])

		// Policy off: selects the single unmanaged provider as before.
		setBrokerPolicyStateP4(t, srv, s, true, false)
		idOff, refusalOff := srv.selectProjectRuntimeBroker(ownerCtx, project, allProviders, avail)
		assert.Equal(t, unmanaged.ID, idOff)
		assert.Empty(t, refusalOff)
	})

	t.Run("default unmanaged broker with no alternatives returns policy error", func(t *testing.T) {
		project := &store.Project{ID: api.NewUUID(), Name: "Sel Default", Slug: "sel-default-" + api.NewUUID()[:8]}
		require.NoError(t, s.CreateProject(ctx, project))
		unmanaged := makeP4Broker(t, s, project.ID, "sel-default-unmanaged", owner.ID, false, false)
		project.DefaultRuntimeBrokerID = unmanaged.ID
		require.NoError(t, s.UpdateProject(ctx, project))

		setBrokerPolicyStateP4(t, srv, s, true, true)
		rec := httptest.NewRecorder()
		resolvedID, err := srv.resolveRuntimeBroker(ownerCtx, rec, "", project)
		assert.Error(t, err)
		assert.Empty(t, resolvedID)
		assert.Equal(t, http.StatusForbidden, rec.Code)
		code, msg, details := decodePolicyError(t, rec)
		assert.Equal(t, ErrCodeBrokerPolicyAdminManagedOnly, code)
		assert.Contains(t, msg, "0 eligible")
		assert.Equal(t, unmanaged.ID, details["brokerId"])
	})

	t.Run("multiple providers skips unmanaged and picks approved", func(t *testing.T) {
		project := &store.Project{ID: api.NewUUID(), Name: "Sel Multi", Slug: "sel-multi-" + api.NewUUID()[:8]}
		require.NoError(t, s.CreateProject(ctx, project))
		_ = makeP4Broker(t, s, project.ID, "sel-multi-unmanaged", owner.ID, false, false)
		approved := makeP4Broker(t, s, project.ID, "sel-multi-approved", owner.ID, false, true)

		setBrokerPolicyStateP4(t, srv, s, true, true)
		allProviders, err := s.GetProjectProviders(ctx, project.ID)
		require.NoError(t, err)
		avail, err := srv.getAvailableBrokersForProject(ctx, project.ID)
		require.NoError(t, err)

		id, refusal := srv.selectProjectRuntimeBroker(ownerCtx, project, allProviders, avail)
		assert.Equal(t, approved.ID, id)
		assert.Empty(t, refusal)
	})

	t.Run("multiple unmanaged providers returns policy error with 0 eligible", func(t *testing.T) {
		project := &store.Project{ID: api.NewUUID(), Name: "Sel All Unmanaged", Slug: "sel-all-unm-" + api.NewUUID()[:8]}
		require.NoError(t, s.CreateProject(ctx, project))
		_ = makeP4Broker(t, s, project.ID, "sel-unm-a", owner.ID, false, false)
		_ = makeP4Broker(t, s, project.ID, "sel-unm-b", owner.ID, false, false)

		setBrokerPolicyStateP4(t, srv, s, true, true)
		rec := httptest.NewRecorder()
		resolvedID, err := srv.resolveRuntimeBroker(ownerCtx, rec, "", project)
		assert.Error(t, err)
		assert.Empty(t, resolvedID)
		assert.Equal(t, http.StatusForbidden, rec.Code)
		code, msg, details := decodePolicyError(t, rec)
		assert.Equal(t, ErrCodeBrokerPolicyAdminManagedOnly, code)
		assert.Contains(t, msg, "0 eligible")
		assert.Equal(t, brokerPolicyOpDispatch, details["operation"])
		assert.Equal(t, float64(0), details["eligibleBrokers"])
	})

	t.Run("mixed unmanaged and unauthorized managed keeps permission message", func(t *testing.T) {
		project := &store.Project{ID: api.NewUUID(), Name: "Sel Mixed", Slug: "sel-mixed-" + api.NewUUID()[:8]}
		require.NoError(t, s.CreateProject(ctx, project))
		otherOwner := makeP4User(t, s, "p4-sel-other", store.UserRoleMember)
		_ = makeP4Broker(t, s, project.ID, "sel-mixed-unm", owner.ID, false, false)
		// Approved broker owned by another user, linked without owner consent
		// (LinkedBy == "") so owner has no permission to dispatch to it even with policy off.
		managedNoAccess := &store.RuntimeBroker{
			ID:            uuid.New().String(),
			Name:          "sel-mixed-managed-no-access",
			Slug:          "sel-mixed-managed-no-access",
			Status:        store.BrokerStatusOnline,
			LastHeartbeat: time.Now(),
			CreatedBy:     otherOwner.ID,
			AdminManaged:  true,
			Created:       time.Now(),
			Updated:       time.Now(),
		}
		require.NoError(t, s.CreateRuntimeBroker(ctx, managedNoAccess))
		require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
			ProjectID:  project.ID,
			BrokerID:   managedNoAccess.ID,
			BrokerName: managedNoAccess.Name,
			Status:     store.BrokerStatusOnline,
			LinkedBy:   "",
		}))

		setBrokerPolicyStateP4(t, srv, s, true, true)
		rec := httptest.NewRecorder()
		resolvedID, err := srv.resolveRuntimeBroker(ownerCtx, rec, "", project)
		assert.Error(t, err)
		assert.Empty(t, resolvedID)
		assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
		code, msg, _ := decodePolicyError(t, rec)
		assert.NotEqual(t, ErrCodeBrokerPolicyAdminManagedOnly, code)
		assert.Equal(t, "No runtime brokers available for this project that you have permission to use", msg)
	})

	t.Run("scheduled dispatch returns clean single policy message", func(t *testing.T) {
		project := &store.Project{ID: api.NewUUID(), Name: "Sel Sched", Slug: "sel-sched-" + api.NewUUID()[:8]}
		require.NoError(t, s.CreateProject(ctx, project))
		unmanaged := makeP4Broker(t, s, project.ID, "sel-sched-unm", owner.ID, false, false)

		setBrokerPolicyStateP4(t, srv, s, true, true)
		id, err := srv.resolveScheduledRuntimeBroker(ownerCtx, project)
		require.Error(t, err)
		assert.Empty(t, id)
		assert.Contains(t, err.Error(), brokerPolicySettingKey)
		assert.Contains(t, err.Error(), "0 eligible")
		assert.Contains(t, err.Error(), unmanaged.Name)
		assert.NotContains(t, err.Error(), ".;")
		assert.NotContains(t, err.Error(), "a schedule cannot choose a runtime broker")
	})
}

func TestBrokerPolicy_Placement_MoveRefusal(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: api.NewUUID(), Name: "Move Project", Slug: "p4-move-project"}
	require.NoError(t, s.CreateProject(ctx, project))

	owner := makeP4User(t, s, "p4-move-owner", store.UserRoleMember)
	superAdmin := makeP4User(t, s, "p4-move-admin", store.UserRoleAdmin)

	src := makeP4Broker(t, s, project.ID, "p4-move-src", "", false, false)
	dstUnmanaged := makeP4Broker(t, s, project.ID, "p4-move-dst-unm", owner.ID, false, false)
	dstApproved := makeP4Broker(t, s, project.ID, "p4-move-dst-app", owner.ID, false, true)

	// Give brokers matching NFS export + profiles so eligibility reaches step 7.
	storage := &api.BrokerWorkspaceStorage{
		Backend: api.WorkspaceStorageBackendNFS,
		NFS:     &api.BrokerNFSWorkspaceStorage{Server: "10.0.0.2", Export: "/vol1", SubPathRoot: "projects", Healthy: true, ExportID: "exp-p4"},
	}
	profiles := []store.BrokerProfile{{Name: "k8s", Type: "kubernetes", Available: true}}
	for _, b := range []*store.RuntimeBroker{src, dstUnmanaged, dstApproved} {
		b.WorkspaceStorage = storage
		b.DefaultProfile = "k8s"
		b.Profiles = profiles
		require.NoError(t, s.UpdateRuntimeBroker(ctx, b))
	}

	agent := &store.Agent{
		ID:                 api.NewUUID(),
		Slug:               "p4-move-agent",
		Name:               "p4-move-agent",
		ProjectID:          project.ID,
		RuntimeBrokerID:    src.ID,
		Runtime:            "kubernetes",
		WorkspacePlacement: api.WorkspacePlacementExport,
		AppliedConfig:      &store.AgentAppliedConfig{},
	}

	setBrokerPolicyStateP4(t, srv, s, true, true)

	// User (even super-admin) move to unmanaged target -> 403 with operation: move.
	adminReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agent.ID+"/reincarnate", nil).
		WithContext(contextWithIdentity(ctx, NewAuthenticatedUser(superAdmin.ID, superAdmin.Email, superAdmin.DisplayName, superAdmin.Role, "web")))
	v, ref := evaluateMoveEligibility(moveEligibilityInput{
		Agent:     agent,
		Src:       src,
		Dst:       dstUnmanaged,
		CloneMode: true,
		SelfMove:  false,
		Probes:    srv.moveProbesFor(adminReq, project, agent.AppliedConfig),
	})
	require.NotNil(t, ref)
	assert.False(t, v.Eligible)
	rec := httptest.NewRecorder()
	writeMoveRefusal(rec, ref, v)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	code, msg, details := decodePolicyError(t, rec)
	assert.Equal(t, ErrCodeBrokerPolicyAdminManagedOnly, code)
	assert.Contains(t, msg, brokerPolicySettingKey)
	assert.Equal(t, brokerPolicyOpMove, details["operation"])
	assert.Equal(t, dstUnmanaged.ID, details["brokerId"])

	// Agent self-move to unmanaged target already serving its project -> 403 with operation: move.
	agentIdent := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: agent.ID},
		ProjectID: project.ID,
		Scopes:    []AgentTokenScope{ScopeAgentCreate},
	}}
	agentReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agent.ID+"/reincarnate", nil).
		WithContext(contextWithIdentity(ctx, agentIdent))
	vSelf, refSelf := evaluateMoveEligibility(moveEligibilityInput{
		Agent:     agent,
		Src:       src,
		Dst:       dstUnmanaged,
		CloneMode: true,
		SelfMove:  true,
		Probes:    srv.moveProbesFor(agentReq, project, agent.AppliedConfig),
	})
	require.NotNil(t, refSelf)
	assert.False(t, vSelf.Eligible)
	recSelf := httptest.NewRecorder()
	writeMoveRefusal(recSelf, refSelf, vSelf)
	assert.Equal(t, http.StatusForbidden, recSelf.Code)
	codeSelf, _, detailsSelf := decodePolicyError(t, recSelf)
	assert.Equal(t, ErrCodeBrokerPolicyAdminManagedOnly, codeSelf)
	assert.Equal(t, brokerPolicyOpMove, detailsSelf["operation"])
	assert.Equal(t, dstUnmanaged.ID, detailsSelf["brokerId"])

	// Move to approved target -> eligible.
	vOK, refOK := evaluateMoveEligibility(moveEligibilityInput{
		Agent:     agent,
		Src:       src,
		Dst:       dstApproved,
		CloneMode: true,
		SelfMove:  false,
		Probes:    srv.moveProbesFor(adminReq, project, agent.AppliedConfig),
	})
	assert.Nil(t, refOK)
	assert.True(t, vOK.Eligible)
}

func TestBrokerPolicy_StartClaim_RefusedAndRollback(t *testing.T) {
	f, d, a := newClaimFixture(t)
	ctx := context.Background()

	// Create a user-owned, unapproved broker and move the agent onto it
	// (CreatedBy is immutable after CreateRuntimeBroker).
	owner := makeP4User(t, f.s, "p4-sc-owner", store.UserRoleMember)
	broker := makeP4Broker(t, f.s, a.ProjectID, "p4-sc-unmanaged", owner.ID, false, false)
	a.RuntimeBrokerID = broker.ID
	require.NoError(t, f.s.UpdateAgent(ctx, a))

	// Configure a broker quota limit so reserveStartCapacity creates a real reservation.
	setBrokerAgentCeiling(t, f.s, 2)
	def, err := f.s.GetLimitDefinitionByName(ctx, store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)

	startCalled := 0
	d.start = func(ctx context.Context, cur *store.Agent) error {
		startCalled++
		return nil
	}

	setBrokerPolicyStateP4(t, f.srv, f.s, true, true)

	// Acquire a capacity hold AND an existing start claim before calling startAgentCore
	// (mirroring the restart path) and verify both are cleanly rolled back on refusal.
	sd, err := f.srv.reserveStartCapacity(ctx, a)
	require.NoError(t, err)
	existingClaim, err := f.srv.acquireStartClaim(ctx, a, store.StartClaimRestart)
	require.NoError(t, err)

	// Confirm reservation exists before startAgentCore.
	hasBefore, err := f.s.HasActiveReservation(ctx, def.ID, a.ID)
	require.NoError(t, err)
	require.True(t, hasBefore)

	err = f.srv.startAgentCore(ctx, a, StartOpts{
		Kind:     store.StartClaimRestart,
		Existing: existingClaim,
		Dispatch: sd,
	})
	require.Error(t, err)
	var bpe *brokerPolicyError
	require.True(t, errors.As(err, &bpe), "expected *brokerPolicyError, got %T: %v", err, err)
	assert.Equal(t, brokerPolicyOpStart, bpe.Operation)
	assert.Equal(t, broker.ID, bpe.Broker.ID)
	assert.Equal(t, 0, startCalled, "dispatcher start must not be called")

	// Assert no leaked quota reservation and released start claim.
	hasAfter, err := f.s.HasActiveReservation(ctx, def.ID, a.ID)
	require.NoError(t, err)
	assert.False(t, hasAfter, "capacity hold must be rolled back on broker policy refusal")

	got := getAgent(t, f.s, a.ID)
	assert.Empty(t, got.StartClaimID, "existing start claim must be released")
	assert.Empty(t, got.StartClaimState, "start claim state must be cleared")
	assert.Equal(t, string(state.PhaseStopped), got.Phase, "agent phase must be restored from starting")

	// Missing broker row keeps today's behavior (checkBrokerPolicyForStart returns nil).
	ghostAgent := &store.Agent{ID: api.NewUUID(), RuntimeBrokerID: "nonexistent-broker-id"}
	assert.NoError(t, f.srv.checkBrokerPolicyForStart(ctx, ghostAgent))

	// Approving the broker makes the next start succeed.
	broker.AdminManaged = true
	require.NoError(t, f.s.UpdateRuntimeBroker(ctx, broker))
	require.NoError(t, f.srv.startAgentCore(ctx, got, StartOpts{Kind: store.StartClaimUser}))
	assert.Equal(t, 1, startCalled)
}

func TestBrokerPolicy_Lifecycle_StartResumeRestartAndRunningUnaffected(t *testing.T) {
	f, d, a := newClaimFixture(t)
	ctx := context.Background()

	owner := makeP4User(t, f.s, "p4-lc-owner", store.UserRoleMember)
	broker := makeP4Broker(t, f.s, a.ProjectID, "p4-lc-unmanaged", owner.ID, false, false)
	a.RuntimeBrokerID = broker.ID
	require.NoError(t, f.s.UpdateAgent(ctx, a))

	startCalls := 0
	d.start = func(ctx context.Context, cur *store.Agent) error {
		startCalls++
		return nil
	}

	setBrokerPolicyStateP4(t, f.srv, f.s, true, true)

	// 1. Start of stopped agent on unmanaged broker -> 403 operation: start.
	status, body := lifecycle(t, f, a.ID, "start")
	assert.Equal(t, http.StatusForbidden, status)
	code, details := errorDetails(body)
	assert.Equal(t, ErrCodeBrokerPolicyAdminManagedOnly, code)
	assert.Equal(t, brokerPolicyOpStart, details["operation"])
	assert.Equal(t, broker.ID, details["brokerId"])
	assert.Equal(t, 0, startCalls)

	// 2. Resume of suspended agent on unmanaged broker -> 403 operation: start.
	require.NoError(t, f.s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{Phase: string(state.PhaseSuspended)}))
	status, body = lifecycle(t, f, a.ID, "start")
	assert.Equal(t, http.StatusForbidden, status)
	code, details = errorDetails(body)
	assert.Equal(t, ErrCodeBrokerPolicyAdminManagedOnly, code)
	assert.Equal(t, brokerPolicyOpStart, details["operation"])
	assert.Equal(t, 0, startCalls)

	// 3. Running agent on unmanaged broker: restart is refused BEFORE the stop leg
	//    and leaves the running agent running.
	require.NoError(t, f.s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{Phase: string(state.PhaseRunning)}))
	status, body = lifecycle(t, f, a.ID, "restart")
	assert.Equal(t, http.StatusForbidden, status)
	code, details = errorDetails(body)
	assert.Equal(t, ErrCodeBrokerPolicyAdminManagedOnly, code)
	assert.Equal(t, brokerPolicyOpStart, details["operation"])
	assert.Equal(t, int32(0), d.stops.Load(), "refused restart must never call stop")
	assert.Equal(t, 0, startCalls, "refused restart must never call start")
	assert.Equal(t, string(state.PhaseRunning), getAgent(t, f.s, a.ID).Phase, "refused restart must leave agent running")

	// 4. Running agent on unmanaged broker: stop is unaffected (§4.3.E).
	status, _ = lifecycle(t, f, a.ID, "stop")
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, int32(1), d.stops.Load(), "stop on running agent must succeed")
	assert.Equal(t, string(state.PhaseStopped), getAgent(t, f.s, a.ID).Phase)

	// 5. Approving the broker makes the next start succeed.
	broker.AdminManaged = true
	require.NoError(t, f.s.UpdateRuntimeBroker(ctx, broker))
	status, _ = lifecycle(t, f, a.ID, "start")
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, 1, startCalls)

	// 6. Policy off: unmanaged broker start succeeds.
	broker.AdminManaged = false
	require.NoError(t, f.s.UpdateRuntimeBroker(ctx, broker))
	setBrokerPolicyStateP4(t, f.srv, f.s, true, false)
	require.NoError(t, f.s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{Phase: string(state.PhaseStopped)}))
	status, _ = lifecycle(t, f, a.ID, "start")
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, 2, startCalls)
}

func TestBrokerPolicy_WakeDM_RefusedAndNonRetryable(t *testing.T) {
	srv, s, sender, target := createWakeDMFixtures(t, string(state.PhaseSuspended))
	ctx := context.Background()

	// Create a user-owned, unapproved broker and assign target onto it.
	owner := makeP4User(t, s, "p4-wake-owner", store.UserRoleMember)
	broker := makeP4Broker(t, s, target.ProjectID, "p4-wake-unmanaged", owner.ID, false, false)
	target.RuntimeBrokerID = broker.ID
	require.NoError(t, s.UpdateAgent(ctx, target))

	disp := &wakeTrackingDispatcher{}
	srv.SetDispatcher(disp)

	setBrokerPolicyStateP4WithOverrides(t, srv, s, true, true, map[string]bool{
		experiments.IdleSuspend: true,
	})

	// 1. Direct wakeAgentForDM returns 403 broker_policy_admin_managed_only, leaves agent suspended.
	res, dmErr := srv.wakeAgentForDM(ctx, target)
	assert.Nil(t, res)
	require.NotNil(t, dmErr)
	assert.Equal(t, http.StatusForbidden, dmErr.HTTPStatus)
	assert.Equal(t, ErrCodeBrokerPolicyAdminManagedOnly, dmErr.Code)
	assert.Contains(t, dmErr.Message, brokerPolicySettingKey)
	assert.Equal(t, brokerPolicyOpStart, dmErr.Details["operation"])
	assert.Equal(t, broker.ID, dmErr.Details["brokerId"])
	assert.Empty(t, disp.getStartCalls(), "refused wake must not dispatch start")

	got, err := s.GetAgent(ctx, target.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseSuspended), got.Phase, "agent must remain suspended")

	// 2. Wake refusal classification: non-retryable (wakeFailureRefused -> ScheduledFailureWakeRefused).
	assert.True(t, isWakeRefusal(dmErr), "403 policy refusal must be classified as wake refusal")
	serr := chatSendFromAgentDMError(dmErr)
	serr.wakeFailure = wakeFailureRefused
	reason := scheduledFailureFromSendError(serr)
	assert.Equal(t, ScheduledFailureWakeRefused, reason)
	assert.False(t, scheduledSendNowAllowed(&ScheduledChatMessage{
		Status:        ScheduledMessageFailed,
		FailureReason: reason,
	}), "policy-refused scheduled wake must not be retryable")

	// 3. Background autoWake fails once, calls fail callback with policy message, no retry.
	var failCalls int
	var failMsg string
	item := autoWakeItem{
		deliver: func(context.Context, *store.Agent) {
			t.Error("deliver must not be called when wake is refused by broker policy")
		},
		fail: func(_ context.Context, err *AgentDMError) {
			failCalls++
			failMsg = err.Message
		},
	}
	run := &autoWakeRun{items: []autoWakeItem{item}}
	srv.autoWakes.runs = map[string]*autoWakeRun{target.ID: run}
	// Mark idle-suspended so autoWakeAgent attempts the wake rather than returning not-running.
	require.NoError(t, s.UpdateAgentStatus(ctx, target.ID, store.AgentStatusUpdate{
		Phase:         string(state.PhaseSuspended),
		SuspendReason: string(state.SuspendReasonIdle),
	}))
	srv.runAutoWake(ctx, target, run)
	assert.Equal(t, 1, failCalls, "fail callback must be invoked once without retry")
	assert.Contains(t, failMsg, brokerPolicySettingKey)
	assert.Empty(t, disp.getStartCalls())

	// 4. Already-running agent on unmanaged broker: message delivery is unaffected (§4.3.E).
	require.NoError(t, s.UpdateAgentStatus(ctx, target.ID, store.AgentStatusUpdate{Phase: string(state.PhaseRunning)}))
	target.Phase = string(state.PhaseRunning)
	senderIdent := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: sender.ID},
		ProjectID: sender.ProjectID,
		Ancestry:  sender.Ancestry,
	}}
	dmRes, deliverErr := srv.ExecuteAgentDM(ctx, &AgentDMInput{
		ProjectID:      sender.ProjectID,
		SenderAgent:    sender,
		SenderIdentity: senderIdent,
		TargetAgent:    target,
		Msg:            "hello running agent",
	})
	require.Nil(t, deliverErr)
	require.NotNil(t, dmRes)

	// 5. Approving the broker makes the next wake succeed.
	require.NoError(t, s.UpdateAgentStatus(ctx, target.ID, store.AgentStatusUpdate{Phase: string(state.PhaseSuspended)}))
	target.Phase = string(state.PhaseSuspended)
	broker.AdminManaged = true
	require.NoError(t, s.UpdateRuntimeBroker(ctx, broker))

	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = s.UpdateAgentStatus(context.Background(), target.ID, store.AgentStatusUpdate{
			Phase:    string(state.PhaseRunning),
			Activity: "idle",
		})
	}()
	resOK, errOK := srv.wakeAgentForDM(ctx, target)
	require.Nil(t, errOK)
	require.NotNil(t, resOK)
	assert.Equal(t, WakeResumed, resOK.Outcome)
	assert.Len(t, disp.getStartCalls(), 1)
}

func TestBrokerPolicy_InPlaceReincarnate_RefusedBeforeStop(t *testing.T) {
	f, d, a := newClaimFixture(t)
	ctx := context.Background()

	owner := makeP4User(t, f.s, "p4-reinc-owner", store.UserRoleMember)
	broker := makeP4Broker(t, f.s, a.ProjectID, "p4-reinc-unmanaged", owner.ID, false, false)
	broker.Capabilities = &store.BrokerCapabilities{Reprovision: true, AgentMove: true}
	require.NoError(t, f.s.UpdateRuntimeBroker(ctx, broker))

	a.RuntimeBrokerID = broker.ID
	a.Phase = string(state.PhaseRunning)
	a.AppliedConfig = &store.AgentAppliedConfig{
		GitClone: &api.GitCloneConfig{URL: "https://github.com/example/repo.git"},
	}
	require.NoError(t, f.s.UpdateAgent(ctx, a))

	startCalls := 0
	d.start = func(ctx context.Context, cur *store.Agent) error {
		startCalls++
		return nil
	}

	setBrokerPolicyStateP4(t, f.srv, f.s, true, true)

	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/reincarnate", map[string]any{})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	code, msg, details := decodePolicyError(t, rec)
	assert.Equal(t, ErrCodeBrokerPolicyAdminManagedOnly, code)
	assert.Contains(t, msg, brokerPolicySettingKey)
	assert.Equal(t, brokerPolicyOpStart, details["operation"])
	assert.Equal(t, broker.ID, details["brokerId"])

	assert.Equal(t, int32(0), d.stops.Load(), "refused in-place reincarnate must not dispatch stop")
	assert.Equal(t, 0, startCalls, "refused in-place reincarnate must not dispatch start")

	got := getAgent(t, f.s, a.ID)
	assert.Equal(t, string(state.PhaseRunning), got.Phase, "agent must remain running")
	assert.Equal(t, store.ReincarnationStateNone, got.ReincarnationState, "no reincarnation record may be created")
}

func TestBrokerPolicy_CreateOnExistingSuspended_Refused(t *testing.T) {
	f, d, a := newClaimFixture(t)
	ctx := context.Background()

	owner := makeP4User(t, f.s, "p4-existing-owner", store.UserRoleMember)
	broker := makeP4Broker(t, f.s, a.ProjectID, "p4-existing-unmanaged", owner.ID, false, false)
	a.RuntimeBrokerID = broker.ID
	a.Phase = string(state.PhaseSuspended)
	require.NoError(t, f.s.UpdateAgent(ctx, a))

	startCalls := 0
	d.start = func(ctx context.Context, cur *store.Agent) error {
		startCalls++
		return nil
	}

	setBrokerPolicyStateP4(t, f.srv, f.s, true, true)

	// Without resume=true, upstream #5194 refuses with 409 agent_exists_resumable
	// before any start dispatch (broker policy is not bypassed).
	recNoResume := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents", map[string]any{
		"name":      a.Name,
		"projectId": a.ProjectID,
	})
	assert.Equal(t, http.StatusConflict, recNoResume.Code, recNoResume.Body.String())
	noResumeCode, _, _ := decodePolicyError(t, recNoResume)
	assert.Equal(t, ErrCodeAgentExistsResumable, noResumeCode)
	assert.Equal(t, 0, startCalls, "refused create-on-existing without resume must not dispatch start")

	// With explicit resume=true, handleExistingAgent enters Branch 1 -> startAgentCore
	// and surfaces the §4.3.D broker policy 403 (operation: start).
	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents", map[string]any{
		"name":      a.Name,
		"projectId": a.ProjectID,
		"resume":    true,
	})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	code, msg, details := decodePolicyError(t, rec)
	assert.Equal(t, ErrCodeBrokerPolicyAdminManagedOnly, code)
	assert.Contains(t, msg, brokerPolicySettingKey)
	assert.Equal(t, brokerPolicyOpStart, details["operation"])
	assert.Equal(t, broker.ID, details["brokerId"])
	assert.Equal(t, 0, startCalls, "refused resume-on-existing must not dispatch start")
}

func TestBrokerPolicy_HarnessConfigImageOp_HTTP403(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	owner := makeP4User(t, s, "p4-img-owner", store.UserRoleMember)
	superAdmin := makeP4User(t, s, "p4-img-admin", store.UserRoleAdmin)
	unmanaged := makeP4Broker(t, s, "", "p4-img-unmanaged", owner.ID, false, false)

	srv.brokerClient = &HybridBrokerClient{}
	hc := &store.HarnessConfig{
		ID:     api.NewUUID(),
		Slug:   "p4-img-hc",
		Name:   "p4-img-hc",
		Config: &store.HarnessConfigData{Image: "my-image:latest"},
	}

	setBrokerPolicyStateP4(t, srv, s, true, true)

	adminCtx := contextWithIdentity(ctx, NewAuthenticatedUser(superAdmin.ID, superAdmin.Email, superAdmin.DisplayName, superAdmin.Role, "web"))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/harness-configs/"+hc.ID+"/pull-image?broker_id="+unmanaged.ID, nil).WithContext(adminCtx)
	rec := httptest.NewRecorder()
	srv.handleHarnessConfigPullImage(rec, req, hc)

	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	code, msg, details := decodePolicyError(t, rec)
	assert.Equal(t, ErrCodeBrokerPolicyAdminManagedOnly, code)
	assert.Contains(t, msg, brokerPolicySettingKey)
	assert.Equal(t, brokerPolicyOpDispatch, details["operation"])
	assert.Equal(t, unmanaged.ID, details["brokerId"])
}
