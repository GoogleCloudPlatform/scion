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
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Unit: resolvePassthroughProfile
// ---------------------------------------------------------------------------

// TestResolvePassthroughProfile checks that the passthrough-to-assign switch
// follows the type of the one profile the agent resolves to: the named
// profile, else the broker's default profile, else a sole profile. It also
// checks the resolved name, which is recorded on the agent. A profile that
// cannot be resolved has no name and is not a cloudrun-sandbox profile.
func TestResolvePassthroughProfile(t *testing.T) {
	localAndSandbox := []store.BrokerProfile{
		{Name: "local", Type: "docker", Available: true},
		{Name: "sandbox", Type: "cloudrun-sandbox", Available: true},
	}
	tests := []struct {
		name           string
		profiles       []store.BrokerProfile
		defaultProfile string
		runtimeTarget  *api.RuntimeTargetDescriptor
		profileName    string
		wantProfile    string
		want           bool
	}{
		{
			name:     "no profiles",
			profiles: nil,
			want:     false,
		},
		{
			name: "sole docker profile",
			profiles: []store.BrokerProfile{
				{Name: "default", Type: "docker", Available: true},
			},
			wantProfile: "default",
			want:        false,
		},
		{
			name: "sole cloudrun-sandbox profile",
			profiles: []store.BrokerProfile{
				{Name: "default", Type: "cloudrun-sandbox", Available: true},
			},
			wantProfile: "default",
			want:        true,
		},
		{
			name:        "named docker profile on a broker that also has a sandbox profile",
			profiles:    localAndSandbox,
			profileName: "local",
			wantProfile: "local",
			want:        false,
		},
		{
			name:        "named cloudrun-sandbox profile",
			profiles:    localAndSandbox,
			profileName: "sandbox",
			wantProfile: "sandbox",
			want:        true,
		},
		{
			name:           "broker default docker profile",
			profiles:       localAndSandbox,
			defaultProfile: "local",
			wantProfile:    "local",
			want:           false,
		},
		{
			name:           "broker default cloudrun-sandbox profile",
			profiles:       localAndSandbox,
			defaultProfile: "sandbox",
			wantProfile:    "sandbox",
			want:           true,
		},
		{
			name:           "named profile wins over the broker default",
			profiles:       localAndSandbox,
			defaultProfile: "sandbox",
			profileName:    "local",
			wantProfile:    "local",
			want:           false,
		},
		{
			name:     "no name and no default among several profiles is unresolvable",
			profiles: localAndSandbox,
			want:     false,
		},
		{
			name:        "named profile the broker does not have is unresolvable",
			profiles:    localAndSandbox,
			profileName: "missing",
			want:        false,
		},
		{
			name:          "flat Runtime Broker stores no profiles",
			runtimeTarget: &api.RuntimeTargetDescriptor{ID: "target-1", Type: "cloudrun-sandbox"},
			want:          false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			broker := &store.RuntimeBroker{
				Profiles:       tc.profiles,
				DefaultProfile: tc.defaultProfile,
				RuntimeTarget:  tc.runtimeTarget,
			}
			gotProfile, got := resolvePassthroughProfile(broker, tc.profileName)
			assert.Equal(t, tc.wantProfile, gotProfile)
			assert.Equal(t, tc.want, got)
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
	srv, s, project, _ := setupPassthroughSandboxServer(t, owner, hostSAEmail, hostProjectID)

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

	// The broker's sole profile, which the mode was decided on, is recorded.
	assert.Equal(t, "default", resp.Agent.AppliedConfig.Profile)
	stored, err := s.GetAgent(context.Background(), resp.Agent.ID)
	require.NoError(t, err)
	require.NotNil(t, stored.AppliedConfig.CreateInputs)
	assert.Equal(t, "default", stored.AppliedConfig.CreateInputs.Profile)
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

// ---------------------------------------------------------------------------
// Integration: the mode follows the agent's profile on a broker with a docker
// profile and a cloudrun-sandbox profile
// ---------------------------------------------------------------------------

// useLocalAndSandboxProfiles gives broker a docker profile "local" and a
// cloudrun-sandbox profile "sandbox", with defaultProfile as its default.
func useLocalAndSandboxProfiles(t *testing.T, s store.Store, brokerID, defaultProfile string) {
	t.Helper()
	ctx := context.Background()
	b, err := s.GetRuntimeBroker(ctx, brokerID)
	require.NoError(t, err)
	b.Profiles = []store.BrokerProfile{
		{Name: "local", Type: "docker", Available: true},
		{Name: "sandbox", Type: "cloudrun-sandbox", Available: true},
	}
	b.DefaultProfile = defaultProfile
	require.NoError(t, s.UpdateRuntimeBroker(ctx, b))
}

// setProjectActiveProfile sets the project's active-profile setting.
func setProjectActiveProfile(t *testing.T, s store.Store, projectID, profile string) {
	t.Helper()
	ctx := context.Background()
	proj, err := s.GetProject(ctx, projectID)
	require.NoError(t, err)
	if proj.Annotations == nil {
		proj.Annotations = map[string]string{}
	}
	proj.Annotations[projectSettingActiveProfile] = profile
	require.NoError(t, s.UpdateProject(ctx, proj))
}

// TestPassthrough_CreateFollowsResolvedProfileType: an explicit passthrough
// create on a broker with both a docker and a cloudrun-sandbox profile is
// translated to assign only when the profile the agent resolves to (request
// profile, else the project's active profile, else the broker default) is
// the cloudrun-sandbox one. That profile is recorded on the agent and in its
// create inputs whichever way the mode goes; an unresolvable profile is not
// recorded.
func TestPassthrough_CreateFollowsResolvedProfileType(t *testing.T) {
	tests := []struct {
		name                 string
		requestProfile       string
		projectActiveProfile string
		brokerDefault        string
		wantTranslated       bool
		wantProfile          string
	}{
		{
			name:           "request names the docker profile",
			requestProfile: "local",
			brokerDefault:  "sandbox",
			wantProfile:    "local",
		},
		{
			name:           "request names the cloudrun-sandbox profile",
			requestProfile: "sandbox",
			brokerDefault:  "local",
			wantTranslated: true,
			wantProfile:    "sandbox",
		},
		{
			name:                 "request profile wins over the project active profile",
			requestProfile:       "local",
			projectActiveProfile: "sandbox",
			brokerDefault:        "sandbox",
			wantProfile:          "local",
		},
		{
			name:                 "project active profile is the cloudrun-sandbox profile",
			projectActiveProfile: "sandbox",
			brokerDefault:        "local",
			wantTranslated:       true,
			wantProfile:          "sandbox",
		},
		{
			name:                 "project active profile is the docker profile",
			projectActiveProfile: "local",
			brokerDefault:        "sandbox",
			wantProfile:          "local",
		},
		{
			name:          "broker default is the docker profile",
			brokerDefault: "local",
			wantProfile:   "local",
		},
		{
			name:           "broker default is the cloudrun-sandbox profile",
			brokerDefault:  "sandbox",
			wantTranslated: true,
			wantProfile:    "sandbox",
		},
		{
			name: "no profile resolvable",
		},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hostSAEmail := "broker-host@mixed-create.iam.gserviceaccount.com"
			owner := ptUser(tid(fmt.Sprintf("user-mixed-create-%d", i)), fmt.Sprintf("mixed-create-%d@test.com", i), store.UserRoleMember)
			srv, s, project, broker := setupPassthroughSandboxServer(t, owner, hostSAEmail, "mixed-create")
			useLocalAndSandboxProfiles(t, s, broker.ID, tc.brokerDefault)
			if tc.projectActiveProfile != "" {
				setProjectActiveProfile(t, s, project.ID, tc.projectActiveProfile)
			}
			enforceSAAssign(srv, store.NewFakeCallerPermissionChecker().AllowTarget(hostSAEmail))

			rec := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
				Name:      "pt-mixed-create",
				ProjectID: project.ID,
				Task:      "passthrough on a broker with two profiles",
				Profile:   tc.requestProfile,
				GCPIdentity: &GCPIdentityAssignment{
					MetadataMode: "passthrough",
				},
			})
			require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

			var resp CreateAgentResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			require.NotNil(t, resp.Agent.AppliedConfig)
			gcpID := resp.Agent.AppliedConfig.GCPIdentity
			require.NotNil(t, gcpID)

			stored, err := s.GetAgent(context.Background(), resp.Agent.ID)
			require.NoError(t, err)
			require.NotNil(t, stored.AppliedConfig)
			require.NotNil(t, stored.AppliedConfig.CreateInputs)
			assert.Equal(t, tc.wantProfile, stored.AppliedConfig.Profile)
			assert.Equal(t, tc.wantProfile, stored.AppliedConfig.CreateInputs.Profile)

			if !tc.wantTranslated {
				assert.Equal(t, store.GCPMetadataModePassthrough, gcpID.MetadataMode)
				assert.Empty(t, gcpID.ServiceAccountID)
				assert.Empty(t, gcpID.ServiceAccountEmail)
				assert.Empty(t, resp.Agent.Annotations["scion.dev/gcp-identity-translated-from"])
				return
			}
			assert.Equal(t, store.GCPMetadataModeAssign, gcpID.MetadataMode)
			assert.Equal(t, hostSAEmail, gcpID.ServiceAccountEmail)
			assert.NotEmpty(t, gcpID.ServiceAccountID)
			assert.Equal(t, "passthrough", resp.Agent.Annotations["scion.dev/gcp-identity-translated-from"])
		})
	}
}

// TestPassthrough_PatchFollowsAgentProfileType: PATCH to passthrough on a
// broker with both a docker and a cloudrun-sandbox profile is translated to
// assign only when the agent's profile is the cloudrun-sandbox one.
func TestPassthrough_PatchFollowsAgentProfileType(t *testing.T) {
	tests := []struct {
		name           string
		agentProfile   string
		wantTranslated bool
	}{
		{name: "docker profile stays passthrough", agentProfile: "local"},
		{name: "cloudrun-sandbox profile is translated", agentProfile: "sandbox", wantTranslated: true},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hostSAEmail := "broker-host@mixed-patch.iam.gserviceaccount.com"
			owner := ptUser(tid(fmt.Sprintf("user-mixed-patch-%d", i)), fmt.Sprintf("mixed-patch-%d@test.com", i), store.UserRoleMember)
			srv, s, project, broker := setupPassthroughSandboxServer(t, owner, hostSAEmail, "mixed-patch")
			// The broker default is the other profile, so only the agent's
			// own profile can produce the expected result.
			brokerDefault := "sandbox"
			if tc.agentProfile == "sandbox" {
				brokerDefault = "local"
			}
			useLocalAndSandboxProfiles(t, s, broker.ID, brokerDefault)
			enforceSAAssign(srv, store.NewFakeCallerPermissionChecker().AllowTarget(hostSAEmail))

			agent := &store.Agent{
				ID:              tid(fmt.Sprintf("agent-mixed-patch-%d", i)),
				Slug:            "pt-mixed-patch",
				Name:            "pt-mixed-patch",
				ProjectID:       project.ID,
				Phase:           string(state.PhaseCreated),
				RuntimeBrokerID: broker.ID,
				AppliedConfig: &store.AgentAppliedConfig{
					Profile: tc.agentProfile,
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

			rec := doRequestAsUser(t, srv, owner, http.MethodPatch, "/api/v1/agents/"+agent.ID, map[string]interface{}{
				"gcp_identity": map[string]string{
					"metadata_mode": "passthrough",
				},
			})
			require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

			var patched store.Agent
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &patched))
			require.NotNil(t, patched.AppliedConfig)
			gcpID := patched.AppliedConfig.GCPIdentity
			require.NotNil(t, gcpID)
			if !tc.wantTranslated {
				assert.Equal(t, store.GCPMetadataModePassthrough, gcpID.MetadataMode)
				assert.Empty(t, gcpID.ServiceAccountID)
				assert.Empty(t, gcpID.ServiceAccountEmail)
				return
			}
			assert.Equal(t, store.GCPMetadataModeAssign, gcpID.MetadataMode)
			assert.Equal(t, hostSAEmail, gcpID.ServiceAccountEmail)
			assert.NotEmpty(t, gcpID.ServiceAccountID)
		})
	}
}

// errPassthroughProjectLookup is returned by passthroughProjectFaultStore.
var errPassthroughProjectLookup = errors.New("injected project lookup failure")

// passthroughProjectFaultStore fails GetProject once the armed request has
// loaded the runtime broker, which the PATCH passthrough branch does right
// before it loads the project for the translation.
type passthroughProjectFaultStore struct {
	store.Store
	fault        *storeFaultSwitch
	brokerLoaded atomic.Bool
}

func (w *passthroughProjectFaultStore) GetRuntimeBroker(ctx context.Context, id string) (*store.RuntimeBroker, error) {
	if w.fault.Active() {
		w.brokerLoaded.Store(true)
	}
	return w.Store.GetRuntimeBroker(ctx, id)
}

func (w *passthroughProjectFaultStore) GetProject(ctx context.Context, id string) (*store.Project, error) {
	if w.fault.Active() && w.brokerLoaded.Load() {
		return nil, errPassthroughProjectLookup
	}
	return w.Store.GetProject(ctx, id)
}

// TestPassthrough_PatchProjectLookupFailureFailsRequest: when the project
// cannot be loaded for the translation, the PATCH fails and the agent's
// identity is left as it was.
func TestPassthrough_PatchProjectLookupFailureFailsRequest(t *testing.T) {
	hostSAEmail := "broker-host@patch-project-fault.iam.gserviceaccount.com"
	owner := ptUser(tid("user-patch-project-fault"), "patch-project-fault@test.com", store.UserRoleMember)
	srv, s, _, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *passthroughProjectFaultStore {
		return &passthroughProjectFaultStore{Store: inner, fault: f}
	})
	project, broker := seedPassthroughSandboxServer(t, srv, s, owner, hostSAEmail, "patch-project-fault")
	enforceSAAssign(srv, store.NewFakeCallerPermissionChecker().AllowTarget(hostSAEmail))

	agent := &store.Agent{
		ID:              tid("agent-patch-project-fault"),
		Slug:            "pt-patch-project-fault",
		Name:            "pt-patch-project-fault",
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

	fault.Arm()
	rec := doRequestAsUser(t, srv, owner, http.MethodPatch, "/api/v1/agents/"+agent.ID, map[string]interface{}{
		"gcp_identity": map[string]string{
			"metadata_mode": "passthrough",
		},
	})
	assert.Equal(t, http.StatusInternalServerError, rec.Code, "body: %s", rec.Body.String())

	stored, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	require.NotNil(t, stored.AppliedConfig)
	require.NotNil(t, stored.AppliedConfig.GCPIdentity)
	assert.Equal(t, store.GCPMetadataModeBlock, stored.AppliedConfig.GCPIdentity.MetadataMode,
		"a failed PATCH leaves the identity unchanged")
	assert.Empty(t, stored.Annotations["scion.dev/gcp-identity-translated-from"])
}

// TestPassthrough_PatchUsesProjectActiveProfile: PATCH to passthrough for an
// agent that names no profile resolves the project's active profile before
// the broker default, and records the profile it resolved.
func TestPassthrough_PatchUsesProjectActiveProfile(t *testing.T) {
	tests := []struct {
		name                 string
		projectActiveProfile string
		brokerDefault        string
		wantTranslated       bool
	}{
		{name: "docker project active profile stays passthrough", projectActiveProfile: "local", brokerDefault: "sandbox"},
		{name: "cloudrun-sandbox project active profile is translated", projectActiveProfile: "sandbox", brokerDefault: "local", wantTranslated: true},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hostSAEmail := "broker-host@project-patch.iam.gserviceaccount.com"
			owner := ptUser(tid(fmt.Sprintf("user-project-patch-%d", i)), fmt.Sprintf("project-patch-%d@test.com", i), store.UserRoleMember)
			srv, s, project, broker := setupPassthroughSandboxServer(t, owner, hostSAEmail, "project-patch")
			// The broker default is the other profile, so only the project's
			// active profile can produce the expected result.
			useLocalAndSandboxProfiles(t, s, broker.ID, tc.brokerDefault)
			setProjectActiveProfile(t, s, project.ID, tc.projectActiveProfile)
			enforceSAAssign(srv, store.NewFakeCallerPermissionChecker().AllowTarget(hostSAEmail))

			agent := &store.Agent{
				ID:              tid(fmt.Sprintf("agent-project-patch-%d", i)),
				Slug:            "pt-project-patch",
				Name:            "pt-project-patch",
				ProjectID:       project.ID,
				Phase:           string(state.PhaseCreated),
				RuntimeBrokerID: broker.ID,
				AppliedConfig: &store.AgentAppliedConfig{
					GCPIdentity: &store.GCPIdentityConfig{
						MetadataMode: store.GCPMetadataModeBlock,
					},
					CreateInputs: &store.AgentCreateInputs{},
				},
				OwnerID:   owner.ID,
				CreatedBy: owner.ID,
				Created:   time.Now(),
				Updated:   time.Now(),
			}
			require.NoError(t, s.CreateAgent(context.Background(), agent))

			rec := doRequestAsUser(t, srv, owner, http.MethodPatch, "/api/v1/agents/"+agent.ID, map[string]interface{}{
				"gcp_identity": map[string]string{
					"metadata_mode": "passthrough",
				},
			})
			require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

			stored, err := s.GetAgent(context.Background(), agent.ID)
			require.NoError(t, err)
			require.NotNil(t, stored.AppliedConfig)
			assert.Equal(t, tc.projectActiveProfile, stored.AppliedConfig.Profile)
			require.NotNil(t, stored.AppliedConfig.CreateInputs)
			assert.Equal(t, tc.projectActiveProfile, stored.AppliedConfig.CreateInputs.Profile)
			gcpID := stored.AppliedConfig.GCPIdentity
			require.NotNil(t, gcpID)
			if !tc.wantTranslated {
				assert.Equal(t, store.GCPMetadataModePassthrough, gcpID.MetadataMode)
				assert.Empty(t, gcpID.ServiceAccountID)
				assert.Empty(t, gcpID.ServiceAccountEmail)
				return
			}
			assert.Equal(t, store.GCPMetadataModeAssign, gcpID.MetadataMode)
			assert.Equal(t, hostSAEmail, gcpID.ServiceAccountEmail)
			assert.NotEmpty(t, gcpID.ServiceAccountID)
		})
	}
}

// profileRecordingDispatcher is a createAgentDispatcher that records the
// profile and GCP settings each start is dispatched with.
type profileRecordingDispatcher struct {
	createAgentDispatcher
	startedProfile string
	startedGCP     *store.GCPIdentityConfig
}

func (d *profileRecordingDispatcher) DispatchAgentStart(_ context.Context, agent *store.Agent, _ string, _ bool) error {
	d.startCalled = true
	if agent.AppliedConfig != nil {
		d.startedProfile = agent.AppliedConfig.Profile
		if agent.AppliedConfig.GCPIdentity != nil {
			g := *agent.AppliedConfig.GCPIdentity
			d.startedGCP = &g
		}
	}
	agent.Phase = string(state.PhaseRunning)
	return nil
}

// TestPassthrough_RecordedProfileSurvivesProjectActiveProfileChange: the
// profile a passthrough create resolved from the project's active profile is
// recorded, so after the project's active profile changes, a later start and
// a reincarnation both use that profile and the GCP settings decided for it.
func TestPassthrough_RecordedProfileSurvivesProjectActiveProfileChange(t *testing.T) {
	tests := []struct {
		name           string
		atCreate       string
		later          string
		wantTranslated bool
	}{
		{name: "cloudrun-sandbox profile at create", atCreate: "sandbox", later: "local", wantTranslated: true},
		{name: "docker profile at create", atCreate: "local", later: "sandbox"},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			hostSAEmail := "broker-host@recorded-profile.iam.gserviceaccount.com"
			owner := ptUser(tid(fmt.Sprintf("user-recorded-profile-%d", i)), fmt.Sprintf("recorded-profile-%d@test.com", i), store.UserRoleMember)
			srv, s, project, broker := setupPassthroughSandboxServer(t, owner, hostSAEmail, "recorded-profile")
			// The broker default is the later profile, so falling back to it
			// would also give the wrong answer.
			useLocalAndSandboxProfiles(t, s, broker.ID, tc.later)
			setProjectActiveProfile(t, s, project.ID, tc.atCreate)
			enforceSAAssign(srv, store.NewFakeCallerPermissionChecker().AllowTarget(hostSAEmail))
			disp := &profileRecordingDispatcher{createAgentDispatcher: createAgentDispatcher{createPhase: string(state.PhaseRunning)}}
			srv.SetDispatcher(disp)

			assertGCP := func(t *testing.T, gcpID *store.GCPIdentityConfig) {
				t.Helper()
				require.NotNil(t, gcpID)
				if !tc.wantTranslated {
					assert.Equal(t, store.GCPMetadataModePassthrough, gcpID.MetadataMode)
					assert.Empty(t, gcpID.ServiceAccountEmail)
					return
				}
				assert.Equal(t, store.GCPMetadataModeAssign, gcpID.MetadataMode)
				assert.Equal(t, hostSAEmail, gcpID.ServiceAccountEmail)
			}

			rec := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
				Name:      "pt-recorded-profile",
				ProjectID: project.ID,
				Task:      "passthrough with the project active profile",
				GCPIdentity: &GCPIdentityAssignment{
					MetadataMode: "passthrough",
				},
			})
			require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
			var resp CreateAgentResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

			// The create dispatch names the profile, which the broker keeps
			// for the agent's later starts.
			require.NotNil(t, disp.capturedAgent)
			require.NotNil(t, disp.capturedAgent.AppliedConfig)
			assert.Equal(t, tc.atCreate, disp.capturedAgent.AppliedConfig.Profile)
			assertGCP(t, disp.capturedAgent.AppliedConfig.GCPIdentity)

			setProjectActiveProfile(t, s, project.ID, tc.later)

			// Start: stop the agent in the store, then start it.
			stopped, err := s.GetAgent(ctx, resp.Agent.ID)
			require.NoError(t, err)
			stopped.Phase = string(state.PhaseStopped)
			require.NoError(t, s.UpdateAgent(ctx, stopped))
			rec = doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/agents/"+resp.Agent.ID+"/start", nil)
			require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
			require.True(t, disp.startCalled)
			assert.Equal(t, tc.atCreate, disp.startedProfile)
			assertGCP(t, disp.startedGCP)

			// Reincarnate: the next generation's config.
			stored, err := s.GetAgent(ctx, resp.Agent.ID)
			require.NoError(t, err)
			proj, err := s.GetProject(ctx, project.ID)
			require.NoError(t, err)
			fresh, _, err := srv.buildFreshAppliedConfig(ctx, stored, proj, "")
			require.NoError(t, err)
			require.NotNil(t, fresh)
			assert.Equal(t, tc.atCreate, fresh.Profile)
			require.NotNil(t, fresh.CreateInputs)
			assert.Equal(t, tc.atCreate, fresh.CreateInputs.Profile)
			assertGCP(t, fresh.GCPIdentity)
		})
	}
}
