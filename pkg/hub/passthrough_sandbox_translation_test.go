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
// Helpers
// ---------------------------------------------------------------------------

// setupPassthroughSandboxServer is like setupPassthroughServer but creates a
// broker with a cloudrun-sandbox profile so the passthrough-to-assign
// translation fires.
func setupPassthroughSandboxServer(
	t *testing.T,
	owner *store.User,
	hostSAEmail, hostProjectID string,
) (*Server, store.Store, *store.Project, *store.RuntimeBroker) {
	t.Helper()
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s := testServer(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, owner))
	ensureHubMembership(ctx, s, owner.ID)

	project := &store.Project{
		ID:        tid("project-sandbox-pt"),
		Name:      "Sandbox PT Test Project",
		Slug:      "sandbox-pt-test-project",
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.createProjectMembersGroup(ctx, project)

	broker := &store.RuntimeBroker{
		ID:                         tid("broker-sandbox-pt"),
		Name:                       "Sandbox PT Test Broker",
		Slug:                       "sandbox-pt-test-broker",
		Status:                     store.BrokerStatusOnline,
		CreatedBy:                  owner.ID,
		GCPHostServiceAccountEmail: hostSAEmail,
		GCPHostProjectID:           hostProjectID,
		Profiles: []store.BrokerProfile{
			{Name: "default", Type: "cloudrun-sandbox", Available: true},
		},
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

// ---------------------------------------------------------------------------
// Unit: brokerHasCloudRunSandboxProfile
// ---------------------------------------------------------------------------

func TestBrokerHasCloudRunSandboxProfile(t *testing.T) {
	tests := []struct {
		name     string
		profiles []store.BrokerProfile
		want     bool
	}{
		{
			name:     "no profiles",
			profiles: nil,
			want:     false,
		},
		{
			name: "docker only",
			profiles: []store.BrokerProfile{
				{Name: "default", Type: "docker", Available: true},
			},
			want: false,
		},
		{
			name: "cloudrun-sandbox",
			profiles: []store.BrokerProfile{
				{Name: "default", Type: "cloudrun-sandbox", Available: true},
			},
			want: true,
		},
		{
			name: "mixed with cloudrun-sandbox",
			profiles: []store.BrokerProfile{
				{Name: "local", Type: "docker", Available: true},
				{Name: "remote", Type: "cloudrun-sandbox", Available: true},
			},
			want: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			broker := &store.RuntimeBroker{Profiles: tc.profiles}
			assert.Equal(t, tc.want, brokerHasCloudRunSandboxProfile(broker))
		})
	}
}

// ---------------------------------------------------------------------------
// Integration: passthrough on cloudrun-sandbox translates to assign
// ---------------------------------------------------------------------------

func TestPassthrough_CloudRunSandbox_TranslatesToAssign(t *testing.T) {
	hostSAEmail := "broker-host@sandbox-project.iam.gserviceaccount.com"
	hostProjectID := "sandbox-project"
	owner := ptUser(tid("user-sandbox-pt-1"), "sandbox-owner1@test.com", store.UserRoleMember)
	srv, _, project, _ := setupPassthroughSandboxServer(t, owner, hostSAEmail, hostProjectID)

	checker := store.NewFakeCallerPermissionChecker().AllowTarget(hostSAEmail)
	enforceSAAssign(srv, checker)

	rec := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "pt-sandbox-assign",
		ProjectID: project.ID,
		Task:      "test passthrough on sandbox",
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode: "passthrough",
		},
	})

	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent.AppliedConfig)
	require.NotNil(t, resp.Agent.AppliedConfig.GCPIdentity)

	gcpID := resp.Agent.AppliedConfig.GCPIdentity

	// The mode must be translated to "assign", not stored as "passthrough".
	assert.Equal(t, store.GCPMetadataModeAssign, gcpID.MetadataMode,
		"passthrough on cloudrun-sandbox should be translated to assign")

	// The SA email and project must come from the broker's host SA.
	assert.Equal(t, hostSAEmail, gcpID.ServiceAccountEmail)
	assert.Equal(t, hostProjectID, gcpID.ProjectID)

	// A ServiceAccountID must be set (FK to GCPServiceAccount table).
	assert.NotEmpty(t, gcpID.ServiceAccountID,
		"translated config must have a ServiceAccountID for JWT scope minting")

	// The annotation should indicate translation.
	assert.Equal(t, "passthrough", resp.Agent.Annotations["scion.dev/gcp-identity-translated-from"])
}

// TestPassthrough_CloudRunSandbox_SARecordCreated verifies that the translation
// creates a hub-scoped GCPServiceAccount record for the broker's host SA.
func TestPassthrough_CloudRunSandbox_SARecordCreated(t *testing.T) {
	hostSAEmail := "broker-host@sa-project.iam.gserviceaccount.com"
	hostProjectID := "sa-project"
	owner := ptUser(tid("user-sandbox-pt-2"), "sandbox-owner2@test.com", store.UserRoleMember)
	srv, s, project, _ := setupPassthroughSandboxServer(t, owner, hostSAEmail, hostProjectID)

	checker := store.NewFakeCallerPermissionChecker().AllowTarget(hostSAEmail)
	enforceSAAssign(srv, checker)

	rec := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "pt-sandbox-sa-check",
		ProjectID: project.ID,
		Task:      "test SA creation",
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode: "passthrough",
		},
	})

	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	// Verify the SA record exists in the store.
	saID := resp.Agent.AppliedConfig.GCPIdentity.ServiceAccountID
	sa, err := s.GetGCPServiceAccount(context.Background(), saID)
	require.NoError(t, err, "SA record must exist in the store")

	assert.Equal(t, hostSAEmail, sa.Email)
	assert.Equal(t, hostProjectID, sa.ProjectID)
	assert.Equal(t, store.ScopeHub, sa.Scope, "broker host SA record must be hub-scoped")
	assert.True(t, sa.Verified, "broker host SA should be pre-verified")
}

// TestPassthrough_CloudRunSandbox_SARecordReused verifies that creating
// two agents with passthrough on the same cloudrun-sandbox broker reuses
// the same GCPServiceAccount record.
func TestPassthrough_CloudRunSandbox_SARecordReused(t *testing.T) {
	hostSAEmail := "broker-host@reuse-project.iam.gserviceaccount.com"
	hostProjectID := "reuse-project"
	owner := ptUser(tid("user-sandbox-pt-3"), "sandbox-owner3@test.com", store.UserRoleMember)
	srv, _, project, _ := setupPassthroughSandboxServer(t, owner, hostSAEmail, hostProjectID)

	checker := store.NewFakeCallerPermissionChecker().AllowTarget(hostSAEmail)
	enforceSAAssign(srv, checker)

	// Create first agent.
	rec1 := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "pt-sandbox-reuse-1",
		ProjectID: project.ID,
		Task:      "test 1",
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode: "passthrough",
		},
	})
	require.Equal(t, http.StatusCreated, rec1.Code, "body: %s", rec1.Body.String())

	// Create second agent.
	rec2 := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "pt-sandbox-reuse-2",
		ProjectID: project.ID,
		Task:      "test 2",
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode: "passthrough",
		},
	})
	require.Equal(t, http.StatusCreated, rec2.Code, "body: %s", rec2.Body.String())

	var resp1, resp2 CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &resp1))
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp2))

	// Both agents should reference the same ServiceAccountID.
	assert.Equal(t,
		resp1.Agent.AppliedConfig.GCPIdentity.ServiceAccountID,
		resp2.Agent.AppliedConfig.GCPIdentity.ServiceAccountID,
		"two agents on the same broker should share the same SA record")
}

// TestPassthrough_NonSandboxBroker_RemainsPassthrough verifies that passthrough
// mode is NOT translated on brokers that do not run cloudrun-sandbox.
func TestPassthrough_NonSandboxBroker_RemainsPassthrough(t *testing.T) {
	hostSAEmail := "broker-host@docker-project.iam.gserviceaccount.com"
	owner := ptUser(tid("user-sandbox-pt-4"), "sandbox-owner4@test.com", store.UserRoleMember)
	// Use the standard setup (no sandbox profiles).
	srv, _, project, _ := setupPassthroughServer(t, owner, hostSAEmail, "docker-project")

	checker := store.NewFakeCallerPermissionChecker().AllowTarget(hostSAEmail)
	enforceSAAssign(srv, checker)

	rec := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "pt-docker-stays-pt",
		ProjectID: project.ID,
		Task:      "test passthrough on docker",
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode: "passthrough",
		},
	})

	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent.AppliedConfig.GCPIdentity)

	// Passthrough should remain passthrough on a non-sandbox broker.
	assert.Equal(t, store.GCPMetadataModePassthrough,
		resp.Agent.AppliedConfig.GCPIdentity.MetadataMode,
		"passthrough on non-sandbox broker should remain passthrough")
	assert.Empty(t, resp.Agent.AppliedConfig.GCPIdentity.ServiceAccountID)
}

// TestPassthrough_CloudRunSandbox_PatchTranslates verifies that PATCH to
// passthrough on a cloudrun-sandbox broker also translates to assign.
func TestPassthrough_CloudRunSandbox_PatchTranslates(t *testing.T) {
	hostSAEmail := "broker-host@patch-project.iam.gserviceaccount.com"
	hostProjectID := "patch-project"
	owner := ptUser(tid("user-sandbox-pt-5"), "sandbox-owner5@test.com", store.UserRoleMember)
	srv, s, project, broker := setupPassthroughSandboxServer(t, owner, hostSAEmail, hostProjectID)

	checker := store.NewFakeCallerPermissionChecker().AllowTarget(hostSAEmail)
	enforceSAAssign(srv, checker)

	// Create an agent in "created" phase (PATCH only works in created phase).
	agent := &store.Agent{
		ID:              tid("agent-sandbox-patch-1"),
		Slug:            "pt-sandbox-patch",
		Name:            "pt-sandbox-patch",
		ProjectID:       project.ID,
		Phase:           string(state.PhaseCreated),
		RuntimeBrokerID: broker.ID,
		AppliedConfig: &store.AgentAppliedConfig{
			GCPIdentity: &store.GCPIdentityConfig{
				MetadataMode: store.GCPMetadataModeBlock,
			},
		},
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateAgent(context.Background(), agent))

	// PATCH to passthrough.
	rec := doRequestAsUser(t, srv, owner, http.MethodPatch, "/api/v1/agents/"+agent.ID, map[string]interface{}{
		"gcp_identity": map[string]string{
			"metadata_mode": "passthrough",
		},
	})

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var patched store.Agent
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &patched))
	require.NotNil(t, patched.AppliedConfig)
	require.NotNil(t, patched.AppliedConfig.GCPIdentity)

	// The mode must be translated to "assign".
	assert.Equal(t, store.GCPMetadataModeAssign, patched.AppliedConfig.GCPIdentity.MetadataMode,
		"PATCH passthrough on cloudrun-sandbox should translate to assign")
	assert.Equal(t, hostSAEmail, patched.AppliedConfig.GCPIdentity.ServiceAccountEmail)
	assert.NotEmpty(t, patched.AppliedConfig.GCPIdentity.ServiceAccountID)
}
