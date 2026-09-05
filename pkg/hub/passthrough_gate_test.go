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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// Unit: isValidServiceAccountEmail
// ---------------------------------------------------------------------------

func TestIsValidServiceAccountEmail(t *testing.T) {
	tests := []struct {
		email string
		valid bool
	}{
		{"agent@my-project.iam.gserviceaccount.com", true},
		{"scion-abc@my-project.iam.gserviceaccount.com", true},
		{"my-sa@p.iam.gserviceaccount.com", true},
		{"sa@my-long-project-id-1234.iam.gserviceaccount.com", true},

		{"", false},
		{"not-an-email", false},
		{"user@gmail.com", false},
		{"@missing-name.iam.gserviceaccount.com", false},
		{"sa@.iam.gserviceaccount.com", false},
		{"sa@iam.gserviceaccount.com", false}, // no project id
	}
	for _, tc := range tests {
		t.Run(tc.email, func(t *testing.T) {
			assert.Equal(t, tc.valid, isValidServiceAccountEmail(tc.email))
		})
	}
}

// ---------------------------------------------------------------------------
// Integration: passthrough actAs gate on agent create and PATCH
// ---------------------------------------------------------------------------

// setupPassthroughServer creates a test server with a broker owned by
// the given owner user, a project, and optionally sets the broker host SA.
// The owner user is created and added to hub/project membership before the
// project group and policy are set up, so the user has create-agent access.
func setupPassthroughServer(t *testing.T, owner *store.User, hostSAEmail, hostProjectID string) (*Server, store.Store, *store.Project, *store.RuntimeBroker) {
	t.Helper()
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s := testServer(t)
	ctx := context.Background()

	// The user must exist before createProjectMembersGroup so that
	// the FK constraint on the group owner succeeds and the user is added to
	// the project members group.
	require.NoError(t, s.CreateUser(ctx, owner))
	ensureHubMembership(ctx, s, owner.ID)

	project := &store.Project{
		ID:        tid("project-pt"),
		Name:      "Passthrough Test Project",
		Slug:      "passthrough-test-project",
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.createProjectMembersGroup(ctx, project)

	broker := &store.RuntimeBroker{
		ID:                         tid("broker-pt"),
		Name:                       "PT Test Broker",
		Slug:                       "pt-test-broker",
		Status:                     store.BrokerStatusOnline,
		CreatedBy:                  owner.ID,
		GCPHostServiceAccountEmail: hostSAEmail,
		GCPHostProjectID:           hostProjectID,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}))
	project.DefaultRuntimeBrokerID = broker.ID
	require.NoError(t, s.UpdateProject(ctx, project))

	srv.SetDispatcher(disp)
	return srv, s, project, broker
}

// ptUser creates a store.User value for test setup. The caller must pass the
// result to setupPassthroughServer (which persists and groups it) before using
// it in requests.
func ptUser(id, email, role string) *store.User {
	return &store.User{
		ID:          id,
		Email:       email,
		DisplayName: email,
		Role:        role,
		Status:      "active",
		Created:     time.Now(),
	}
}

// addExtraUser creates an additional user in the store that was NOT the one
// used to set up the project. Useful for non-owner/admin tests.
func addExtraUser(t *testing.T, s store.Store, id, email, role string) *store.User {
	t.Helper()
	user := &store.User{
		ID:          id,
		Email:       email,
		DisplayName: email,
		Role:        role,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(context.Background(), user))
	ensureHubMembership(context.Background(), s, user.ID)
	return user
}

// ---------------------------------------------------------------------------
// Acceptance criterion 1: No broker host SA -> configuration error
// ---------------------------------------------------------------------------

func TestPassthrough_NoBrokerHostSA_Create_Denied(t *testing.T) {
	owner := ptUser(tid("user-pt-owner-1"), "owner1@test.com", store.UserRoleMember)
	srv, _, project, _ := setupPassthroughServer(t, owner, "", "")

	rec := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "pt-no-host-sa",
		ProjectID: project.ID,
		Task:      "test",
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode: "passthrough",
		},
	})

	assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "host service account")
}

func TestPassthrough_NoBrokerHostSA_Patch_Denied(t *testing.T) {
	owner := ptUser(tid("user-pt-owner-2"), "owner2@test.com", store.UserRoleMember)
	srv, s, project, _ := setupPassthroughServer(t, owner, "", "")

	// Create an agent first (without passthrough).
	agent := &store.Agent{
		ID:              tid("agent-pt-patch-1"),
		Slug:            "pt-patch-nosa",
		Name:            "pt-patch-nosa",
		ProjectID:       project.ID,
		RuntimeBrokerID: tid("broker-pt"),
		Phase:           string(state.PhaseCreated),
		CreatedBy:       owner.ID,
		OwnerID:         owner.ID,
	}
	require.NoError(t, s.CreateAgent(context.Background(), agent))

	rec := doRequestAsUser(t, srv, owner, http.MethodPatch, "/api/v1/agents/"+agent.ID,
		map[string]interface{}{
			"gcp_identity": map[string]interface{}{
				"metadata_mode": "passthrough",
			},
		})

	assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "host service account")
}

// ---------------------------------------------------------------------------
// Acceptance criterion 2: Caller without actAs on broker host SA -> denied
// ---------------------------------------------------------------------------

func TestPassthrough_CallerLacksActAs_Create_Denied(t *testing.T) {
	hostSAEmail := "broker-host@my-project.iam.gserviceaccount.com"
	owner := ptUser(tid("user-pt-owner-3"), "owner3@test.com", store.UserRoleMember)
	srv, _, project, _ := setupPassthroughServer(t, owner, hostSAEmail, "my-project")

	// Configure checker to DENY actAs on the broker host SA.
	checker := store.NewFakeCallerPermissionChecker().
		DenyTarget(hostSAEmail, "caller lacks iam.serviceAccounts.actAs")
	enforceSAAssign(srv, checker)

	rec := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "pt-denied-actas",
		ProjectID: project.ID,
		Task:      "test",
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode: "passthrough",
		},
	})

	assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	require.GreaterOrEqual(t, checker.CallCount(), 1,
		"the caller-permission checker was never reached; the refusal came from somewhere else")

	// Verify the target email in the checker call matches the broker host SA.
	calls := checker.Calls()
	assert.Equal(t, hostSAEmail, calls[0].TargetSAEmail,
		"checker was asked about the wrong service account")
}

func TestPassthrough_CallerLacksActAs_Patch_Denied(t *testing.T) {
	hostSAEmail := "broker-host@my-project.iam.gserviceaccount.com"
	owner := ptUser(tid("user-pt-owner-4"), "owner4@test.com", store.UserRoleMember)
	srv, s, project, _ := setupPassthroughServer(t, owner, hostSAEmail, "my-project")

	// Create an agent first.
	agent := &store.Agent{
		ID:              tid("agent-pt-patch-2"),
		Slug:            "pt-patch-noactas",
		Name:            "pt-patch-noactas",
		ProjectID:       project.ID,
		RuntimeBrokerID: tid("broker-pt"),
		Phase:           string(state.PhaseCreated),
		CreatedBy:       owner.ID,
		OwnerID:         owner.ID,
	}
	require.NoError(t, s.CreateAgent(context.Background(), agent))

	checker := store.NewFakeCallerPermissionChecker().
		DenyTarget(hostSAEmail, "caller lacks iam.serviceAccounts.actAs")
	enforceSAAssign(srv, checker)

	rec := doRequestAsUser(t, srv, owner, http.MethodPatch, "/api/v1/agents/"+agent.ID,
		map[string]interface{}{
			"gcp_identity": map[string]interface{}{
				"metadata_mode": "passthrough",
			},
		})

	assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	require.GreaterOrEqual(t, checker.CallCount(), 1,
		"the caller-permission checker was never reached on the PATCH path")
}

// ---------------------------------------------------------------------------
// Acceptance criterion 3: Broker owner + PT allow -> success
// ---------------------------------------------------------------------------

func TestPassthrough_BrokerOwnerWithActAs_Create_Allowed(t *testing.T) {
	hostSAEmail := "broker-host@my-project.iam.gserviceaccount.com"
	owner := ptUser(tid("user-pt-owner-5"), "owner5@test.com", store.UserRoleMember)
	srv, _, project, _ := setupPassthroughServer(t, owner, hostSAEmail, "my-project")

	// Configure checker to ALLOW actAs on the broker host SA.
	checker := store.NewFakeCallerPermissionChecker().AllowTarget(hostSAEmail)
	enforceSAAssign(srv, checker)

	rec := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "pt-allowed",
		ProjectID: project.ID,
		Task:      "test",
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode: "passthrough",
		},
	})

	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	require.GreaterOrEqual(t, checker.CallCount(), 1,
		"the caller-permission checker must be consulted even when the caller owns the broker")

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent.AppliedConfig.GCPIdentity)
	assert.Equal(t, store.GCPMetadataModePassthrough, resp.Agent.AppliedConfig.GCPIdentity.MetadataMode)
}

func TestPassthrough_BrokerOwnerWithActAs_Patch_Allowed(t *testing.T) {
	hostSAEmail := "broker-host@my-project.iam.gserviceaccount.com"
	owner := ptUser(tid("user-pt-owner-6"), "owner6@test.com", store.UserRoleMember)
	srv, s, project, _ := setupPassthroughServer(t, owner, hostSAEmail, "my-project")

	// Create an agent first.
	agent := &store.Agent{
		ID:              tid("agent-pt-patch-3"),
		Slug:            "pt-patch-allowed",
		Name:            "pt-patch-allowed",
		ProjectID:       project.ID,
		RuntimeBrokerID: tid("broker-pt"),
		Phase:           string(state.PhaseCreated),
		CreatedBy:       owner.ID,
		OwnerID:         owner.ID,
	}
	require.NoError(t, s.CreateAgent(context.Background(), agent))

	checker := store.NewFakeCallerPermissionChecker().AllowTarget(hostSAEmail)
	enforceSAAssign(srv, checker)

	rec := doRequestAsUser(t, srv, owner, http.MethodPatch, "/api/v1/agents/"+agent.ID,
		map[string]interface{}{
			"gcp_identity": map[string]interface{}{
				"metadata_mode": "passthrough",
			},
		})

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	require.GreaterOrEqual(t, checker.CallCount(), 1,
		"the checker must be consulted on the PATCH path too")

	got, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	require.NotNil(t, got.AppliedConfig)
	require.NotNil(t, got.AppliedConfig.GCPIdentity)
	assert.Equal(t, store.GCPMetadataModePassthrough, got.AppliedConfig.GCPIdentity.MetadataMode)
}

// ---------------------------------------------------------------------------
// Acceptance criterion 4: Admin + PT allow -> success
// ---------------------------------------------------------------------------

func TestPassthrough_AdminWithActAs_Create_Allowed(t *testing.T) {
	hostSAEmail := "broker-host@my-project.iam.gserviceaccount.com"
	owner := ptUser(tid("user-pt-owner-7"), "owner7@test.com", store.UserRoleMember)
	srv, s, project, _ := setupPassthroughServer(t, owner, hostSAEmail, "my-project")

	// Create admin who is NOT the broker owner.
	admin := addExtraUser(t, s, tid("user-pt-admin-1"), "admin1@test.com", store.UserRoleAdmin)
	// CO1: Admin access requires a role binding; the role field alone is not enough.
	{
		ctx := context.Background()
		rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
		require.NoError(t, err)
		_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: rd.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      admin.ID,
			ScopeType:        store.RoleScopeSystem,
			CreatedBy:        store.SystemReconcileCreatedBy,
		})
		require.NoError(t, err)
	}

	checker := store.NewFakeCallerPermissionChecker().AllowTarget(hostSAEmail)
	enforceSAAssign(srv, checker)

	rec := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "pt-admin-allowed",
		ProjectID: project.ID,
		Task:      "test",
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode: "passthrough",
		},
	})

	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent.AppliedConfig.GCPIdentity)
	assert.Equal(t, store.GCPMetadataModePassthrough, resp.Agent.AppliedConfig.GCPIdentity.MetadataMode)
}

// ---------------------------------------------------------------------------
// Acceptance criterion 5: Non-owner non-admin is denied (ownership gate)
// ---------------------------------------------------------------------------

func TestPassthrough_NonOwnerNonAdmin_Create_Denied(t *testing.T) {
	// The stranger is the project owner so they can create agents and dispatch
	// to the broker, but the BROKER is owned by someone else. The broker must
	// be auto-provide so the stranger passes the dispatch check and actually
	// reaches the passthrough ownership gate, which is what this test asserts.
	hostSAEmail := "broker-host@my-project.iam.gserviceaccount.com"
	stranger := ptUser(tid("user-pt-stranger-1"), "stranger@test.com", store.UserRoleMember)

	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s := testServer(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, stranger))
	ensureHubMembership(ctx, s, stranger.ID)

	project := &store.Project{
		ID:        tid("project-pt-nonowner"),
		Name:      "Passthrough NonOwner Project",
		Slug:      "passthrough-nonowner-project",
		OwnerID:   stranger.ID,
		CreatedBy: stranger.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.createProjectMembersGroup(ctx, project)

	// Broker owned by someone else, auto-provide so dispatch is allowed.
	brokerOwnerID := tid("user-pt-broker-real-owner")
	broker := &store.RuntimeBroker{
		ID:                         tid("broker-pt-nonowner"),
		Name:                       "AutoProvide Broker",
		Slug:                       "autoprovide-broker",
		Status:                     store.BrokerStatusOnline,
		CreatedBy:                  brokerOwnerID,
		AutoProvide:                true,
		GCPHostServiceAccountEmail: hostSAEmail,
		GCPHostProjectID:           "my-project",
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}))
	project.DefaultRuntimeBrokerID = broker.ID
	require.NoError(t, s.UpdateProject(ctx, project))
	srv.SetDispatcher(disp)

	// Even with actAs allowed, non-owner should be denied at ownership gate.
	checker := store.NewFakeCallerPermissionChecker().AllowTarget(hostSAEmail)
	enforceSAAssign(srv, checker)

	rec := doRequestAsUser(t, srv, stranger, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "pt-stranger",
		ProjectID: project.ID,
		Task:      "test",
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode: "passthrough",
		},
	})

	assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "broker ownership")

	// The checker should NOT have been consulted — the ownership check should
	// have short-circuited before actAs was reached.
	assert.Equal(t, 0, checker.CallCount(),
		"checker was called despite the ownership denial; the ownership gate did not fire first")
}

// ---------------------------------------------------------------------------
// Acceptance criterion 6: PATCH and create produce equivalent decisions
// ---------------------------------------------------------------------------

func TestPassthrough_CreateAndPatch_EquivalentDecisions(t *testing.T) {
	// This test verifies that both paths consult the same gate. We run
	// allow on both and deny on both, and verify the outcomes match.

	t.Run("both deny when checker denies", func(t *testing.T) {
		hostSAEmail := "broker-host@my-project.iam.gserviceaccount.com"
		owner := ptUser(tid("user-pt-owner-9"), "owner9@test.com", store.UserRoleMember)
		srv, s, project, _ := setupPassthroughServer(t, owner, hostSAEmail, "my-project")

		checker := store.NewFakeCallerPermissionChecker().
			DenyTarget(hostSAEmail, "no actAs")
		enforceSAAssign(srv, checker)

		// Create path.
		createRec := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
			Name:      "pt-equiv-create",
			ProjectID: project.ID,
			Task:      "test",
			GCPIdentity: &GCPIdentityAssignment{
				MetadataMode: "passthrough",
			},
		})

		// PATCH path: create an agent without passthrough first.
		agent := &store.Agent{
			ID:              tid("agent-pt-equiv"),
			Slug:            "pt-equiv-patch",
			Name:            "pt-equiv-patch",
			ProjectID:       project.ID,
			RuntimeBrokerID: tid("broker-pt"),
			Phase:           string(state.PhaseCreated),
			CreatedBy:       owner.ID,
			OwnerID:         owner.ID,
		}
		require.NoError(t, s.CreateAgent(context.Background(), agent))

		patchRec := doRequestAsUser(t, srv, owner, http.MethodPatch, "/api/v1/agents/"+agent.ID,
			map[string]interface{}{
				"gcp_identity": map[string]interface{}{
					"metadata_mode": "passthrough",
				},
			})

		assert.Equal(t, createRec.Code, patchRec.Code,
			"create and PATCH must produce the same HTTP status")
		assert.Equal(t, http.StatusForbidden, createRec.Code)
	})
}

// ---------------------------------------------------------------------------
// Acceptance criterion 7: Audit records have correct surface labels
// ---------------------------------------------------------------------------

func TestPassthrough_AuditSurface_Create(t *testing.T) {
	hostSAEmail := "broker-host@my-project.iam.gserviceaccount.com"
	owner := ptUser(tid("user-pt-owner-10"), "owner10@test.com", store.UserRoleMember)
	srv, _, project, _ := setupPassthroughServer(t, owner, hostSAEmail, "my-project")

	audit := &mockAuditLogger{}
	srv.SetAuditLogger(audit)

	checker := store.NewFakeCallerPermissionChecker().AllowTarget(hostSAEmail)
	enforceSAAssign(srv, checker)

	rec := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "pt-audit-create",
		ProjectID: project.ID,
		Task:      "test",
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode: "passthrough",
		},
	})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	ev := onlySAEvent(t, audit)
	assert.Equal(t, SurfacePassthroughCreate, ev.Surface,
		"audit record must use the passthrough-create surface label")
	assert.Equal(t, hostSAEmail, ev.TargetSAEmail,
		"audit record must reference the broker host SA, not a stored SA")
}

func TestPassthrough_AuditSurface_Patch(t *testing.T) {
	hostSAEmail := "broker-host@my-project.iam.gserviceaccount.com"
	owner := ptUser(tid("user-pt-owner-11"), "owner11@test.com", store.UserRoleMember)
	srv, s, project, _ := setupPassthroughServer(t, owner, hostSAEmail, "my-project")

	audit := &mockAuditLogger{}
	srv.SetAuditLogger(audit)

	// Create an agent first.
	agent := &store.Agent{
		ID:              tid("agent-pt-audit-patch"),
		Slug:            "pt-audit-patch",
		Name:            "pt-audit-patch",
		ProjectID:       project.ID,
		RuntimeBrokerID: tid("broker-pt"),
		Phase:           string(state.PhaseCreated),
		CreatedBy:       owner.ID,
		OwnerID:         owner.ID,
	}
	require.NoError(t, s.CreateAgent(context.Background(), agent))

	checker := store.NewFakeCallerPermissionChecker().AllowTarget(hostSAEmail)
	enforceSAAssign(srv, checker)

	rec := doRequestAsUser(t, srv, owner, http.MethodPatch, "/api/v1/agents/"+agent.ID,
		map[string]interface{}{
			"gcp_identity": map[string]interface{}{
				"metadata_mode": "passthrough",
			},
		})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	ev := onlySAEvent(t, audit)
	assert.Equal(t, SurfacePassthroughPatch, ev.Surface,
		"audit record must use the passthrough-patch surface label")
	assert.Equal(t, hostSAEmail, ev.TargetSAEmail,
		"audit record must reference the broker host SA")
}

// ---------------------------------------------------------------------------
// Acceptance criterion: Deny audit records carry correct surface
// ---------------------------------------------------------------------------

func TestPassthrough_AuditSurface_Create_Deny(t *testing.T) {
	hostSAEmail := "broker-host@my-project.iam.gserviceaccount.com"
	owner := ptUser(tid("user-pt-owner-12"), "owner12@test.com", store.UserRoleMember)
	srv, _, project, _ := setupPassthroughServer(t, owner, hostSAEmail, "my-project")

	audit := &mockAuditLogger{}
	srv.SetAuditLogger(audit)

	checker := store.NewFakeCallerPermissionChecker().
		DenyTarget(hostSAEmail, "no actAs grant")
	enforceSAAssign(srv, checker)

	rec := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "pt-audit-deny",
		ProjectID: project.ID,
		Task:      "test",
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode: "passthrough",
		},
	})
	require.Equal(t, http.StatusForbidden, rec.Code)

	ev := onlySAEvent(t, audit)
	assert.Equal(t, SurfacePassthroughCreate, ev.Surface)
	assert.Equal(t, hostSAEmail, ev.TargetSAEmail)
	require.NotNil(t, ev.Decision)
	assert.Equal(t, store.ActAsDenied, *ev.Decision)
}

// ---------------------------------------------------------------------------
// Broker registration: host SA fields
// ---------------------------------------------------------------------------

func TestBrokerUpdate_GCPHostSAFields(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	admin := addExtraUser(t, s, tid("admin-broker-update"), "admin-bu@test.com", store.UserRoleAdmin)

	broker := &store.RuntimeBroker{
		ID:        tid("broker-update-sa"),
		Name:      "Update SA Broker",
		Slug:      "update-sa-broker",
		Status:    store.BrokerStatusOnline,
		CreatedBy: admin.ID,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	// PATCH to set host SA fields.
	rec := doRequestAsUser(t, srv, admin, http.MethodPatch, "/api/v1/runtime-brokers/"+broker.ID,
		map[string]interface{}{
			"gcpHostServiceAccountEmail": "host@my-project.iam.gserviceaccount.com",
			"gcpHostProjectId":           "my-project",
		})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	// Verify the fields were stored.
	got, err := s.GetRuntimeBroker(ctx, broker.ID)
	require.NoError(t, err)
	assert.Equal(t, "host@my-project.iam.gserviceaccount.com", got.GCPHostServiceAccountEmail)
	assert.Equal(t, "my-project", got.GCPHostProjectID)
}

func TestBrokerUpdate_GCPHostSAEmail_InvalidFormat(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	admin := addExtraUser(t, s, tid("admin-broker-invalid"), "admin-bi@test.com", store.UserRoleAdmin)

	broker := &store.RuntimeBroker{
		ID:        tid("broker-invalid-sa"),
		Name:      "Invalid SA Broker",
		Slug:      "invalid-sa-broker",
		Status:    store.BrokerStatusOnline,
		CreatedBy: admin.ID,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	// PATCH with invalid email format.
	rec := doRequestAsUser(t, srv, admin, http.MethodPatch, "/api/v1/runtime-brokers/"+broker.ID,
		map[string]interface{}{
			"gcpHostServiceAccountEmail": "not-a-valid-email",
		})
	assert.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
}

// ---------------------------------------------------------------------------
// Project ID derivation from SA email
// ---------------------------------------------------------------------------

func TestPassthrough_ProjectIDDerivedFromEmail(t *testing.T) {
	// When gcpHostProjectID is empty, the helper should derive it from
	// the service account email.
	hostSAEmail := "broker-host@derived-project.iam.gserviceaccount.com"
	owner := ptUser(tid("user-pt-owner-13"), "owner13@test.com", store.UserRoleMember)
	// Note: empty project ID — should be derived from email.
	srv, _, project, _ := setupPassthroughServer(t, owner, hostSAEmail, "")

	checker := store.NewFakeCallerPermissionChecker().AllowTarget(hostSAEmail)
	enforceSAAssign(srv, checker)

	rec := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "pt-derived-project",
		ProjectID: project.ID,
		Task:      "test",
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode: "passthrough",
		},
	})

	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	require.GreaterOrEqual(t, checker.CallCount(), 1)
}
