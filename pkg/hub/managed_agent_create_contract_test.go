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
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestManagedAgentCreate_BrokerlessProject verifies that explicit ManagedAgent
// selection creates a hub-direct managed agent in a project with zero Runtime
// Brokers registered (no 503 no_runtime_broker failure).
func TestManagedAgentCreate_BrokerlessProject(t *testing.T) {
	srv, s := testServer(t)
	backend := newInteractionLedgerBackend()
	useManagedBackend(t, backend)

	ctx := context.Background()
	project := &store.Project{
		ID:   tid("brokerless-mgd-project"),
		Name: "Brokerless Managed Project",
		Slug: "brokerless-mgd-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:         "mgd-brokerless",
		ProjectID:    project.ID,
		ManagedAgent: true,
		Task:         "summarize the spec",
	})
	row := requireManagedCreated(t, rec, s)
	assert.Equal(t, "managed:"+backend.Name(), row.Runtime)
	assert.True(t, isManagedAgentRuntime(row.Runtime))
	assert.Empty(t, row.RuntimeBrokerID, "hub-direct managed agent must have no RuntimeBrokerID")
	assert.False(t, row.IsPinned(), "hub-direct managed agent must not be pinned to a Runtime Broker")
	require.NotNil(t, row.AppliedConfig)
	assert.Empty(t, row.AppliedConfig.Profile)
	require.NotNil(t, row.AppliedConfig.CreateInputs)
	assert.Empty(t, row.AppliedConfig.CreateInputs.Profile)
	assert.Equal(t, string(state.PhaseRunning), row.Phase)
	assert.Equal(t, "working", row.Activity)
	assert.Equal(t, "interaction-1", row.Annotations[annotationInteractionID])
}

// TestManagedAgentCreate_FlatBrokerProject_ExplicitAndLegacyAdapter verifies
// that both the primary explicit selector (ManagedAgent: true) and the bounded
// legacy adapter (Profile: ManagedAgentsProfile) bypass flat Runtime Brokers
// completely — even when hub.flat_runtime_brokers is enabled (or disabled),
// the project's default/only Runtime Broker is a flat instance, and the project
// configures scion.io/active-profile and per-profile GCP SA defaults.
func TestManagedAgentCreate_FlatBrokerProject_ExplicitAndLegacyAdapter(t *testing.T) {
	for _, expOn := range []bool{true, false} {
		t.Run(fmt.Sprintf("flat_experiment_%v", expOn), func(t *testing.T) {
			f := newFlatHubFixture(t, flatHubOpts{experimentOn: expOn, linkFlat: true})
			backend := newInteractionLedgerBackend()
			useManagedBackend(t, backend)
			setAgentQuotaLimits(t, f.s)

			ctx := context.Background()
			// Make the flat Runtime Broker the project's ONLY linked and default broker,
			// and configure project-level active-profile + per-profile SA annotations.
			require.NoError(t, f.s.RemoveProjectProvider(ctx, f.project.ID, f.legacy.ID))
			f.project.DefaultRuntimeBrokerID = f.flat.ID
			f.project.Annotations = map[string]string{
				store.AnnotationProjectActiveProfile:          "k8s-prod",
				projectSettingDefaultGCPIdentitySAIDByProfile: `{"k8s-prod":"sa-nonexistent"}`,
			}
			require.NoError(t, f.s.UpdateProject(ctx, f.project))

			// Control: when hub.flat_runtime_brokers is off, ordinary container create
			// against the flat default broker is refused with 412 experiment_disabled.
			if !expOn {
				ctrlRec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
					Name:      "ctrl-container",
					ProjectID: f.project.ID,
					Task:      "container work",
				})
				require.Equal(t, http.StatusPreconditionFailed, ctrlRec.Code, ctrlRec.Body.String())
			}

			cases := []struct {
				name  string
				route string
				req   CreateAgentRequest
			}{
				{
					name:  "explicit_selector_top_level_route",
					route: "/api/v1/agents",
					req: CreateAgentRequest{
						Name:         "mgd-explicit-top",
						ProjectID:    f.project.ID,
						ManagedAgent: true,
						Task:         "analyze architecture",
						WorkspaceFiles: []transfer.FileInfo{
							{Path: "README.md", Size: 12, Hash: "sha256:deadbeef"},
						},
					},
				},
				{
					name:  "explicit_selector_project_route",
					route: "/api/v1/projects/" + f.project.ID + "/agents",
					req: CreateAgentRequest{
						Name:         "mgd-explicit-proj",
						ManagedAgent: true,
						Task:         "analyze architecture",
					},
				},
				{
					name:  "legacy_profile_adapter_top_level_route",
					route: "/api/v1/agents",
					req: CreateAgentRequest{
						Name:      "mgd-legacy-top",
						ProjectID: f.project.ID,
						Profile:   ManagedAgentsProfile,
						Task:      "legacy caller task",
					},
				},
				{
					name:  "legacy_profile_adapter_project_route",
					route: "/api/v1/projects/" + f.project.ID + "/agents",
					req: CreateAgentRequest{
						Name:    "mgd-legacy-proj",
						Profile: ManagedAgentsProfile,
						Task:    "legacy caller task",
					},
				},
				{
					name:  "both_explicit_and_legacy_sentinel",
					route: "/api/v1/agents",
					req: CreateAgentRequest{
						Name:         "mgd-both",
						ProjectID:    f.project.ID,
						ManagedAgent: true,
						Profile:      ManagedAgentsProfile,
						Task:         "idempotent dual caller",
					},
				},
			}

			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					rec := doRequest(t, f.srv, http.MethodPost, tc.route, tc.req)
					row := requireManagedCreated(t, rec, f.s)

					var resp CreateAgentResponse
					require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
					assert.Empty(t, resp.Warnings, "must not emit flatDefaultProfileDroppedWarning for managed agents")
					assert.Empty(t, resp.UploadURLs, "must not enter broker workspace-bootstrap provisioning")

					assert.Equal(t, "managed:"+backend.Name(), row.Runtime)
					assert.True(t, isManagedAgentRuntime(row.Runtime))
					assert.Empty(t, row.RuntimeBrokerID, "must not resolve or attach flat Runtime Broker")
					assert.False(t, row.IsPinned(), "must not pin managed agent to flat Runtime Broker")
					assert.Empty(t, row.PinnedRuntimeBrokerID)
					assert.Empty(t, row.PinnedRuntimeTargetID)
					assert.Empty(t, row.PinnedRuntimeTargetType)

					require.NotNil(t, row.AppliedConfig)
					assert.Empty(t, row.AppliedConfig.Profile, "req.Profile sentinel must be cleared and active-profile must not be applied")
					require.NotNil(t, row.AppliedConfig.CreateInputs)
					assert.Empty(t, row.AppliedConfig.CreateInputs.Profile, "CreateInputs.Profile must remain empty")
					assert.Nil(t, row.AppliedConfig.GCPIdentity, "per-profile GCP SA default must not apply to managed agents")

					assert.False(t, f.client.createCalled, "Runtime Broker dispatcher must never be called")
					assert.EqualValues(t, 0, brokerReservationCount(t, f.s, f.flat.ID),
						"managed agents must consume zero per-broker quota on the flat Runtime Broker")
					assert.True(t, hasReservation(t, f.s, store.LimitMaxAgentsPerProject, row.ID),
						"managed agents still hold a per-project quota slot")
				})
			}
		})
	}
}

// TestManagedAgentCreate_ContradictoryRequestsRejected verifies that combining
// managed-agent selection (explicit or legacy sentinel) with Runtime Broker
// placement fields is rejected with a 400 validation_error before any store
// writes, quota reservations, or backend calls occur.
func TestManagedAgentCreate_ContradictoryRequestsRejected(t *testing.T) {
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	backend := newInteractionLedgerBackend()
	useManagedBackend(t, backend)
	setAgentQuotaLimits(t, f.s)

	cases := []struct {
		name      string
		req       CreateAgentRequest
		wantField string
	}{
		{
			name: "managed_with_broker_profile",
			req: CreateAgentRequest{
				Name:         "bad-mgd-profile",
				ProjectID:    f.project.ID,
				ManagedAgent: true,
				Profile:      "local",
				Task:         "do work",
			},
			wantField: "profile",
		},
		{
			name: "managed_with_runtime_broker_id",
			req: CreateAgentRequest{
				Name:            "bad-mgd-broker",
				ProjectID:       f.project.ID,
				ManagedAgent:    true,
				RuntimeBrokerID: f.flat.ID,
				Task:            "do work",
			},
			wantField: "runtimeBrokerId",
		},
		{
			name: "legacy_sentinel_with_runtime_broker_id",
			req: CreateAgentRequest{
				Name:            "bad-legacy-broker",
				ProjectID:       f.project.ID,
				Profile:         ManagedAgentsProfile,
				RuntimeBrokerID: f.flat.ID,
				Task:            "do work",
			},
			wantField: "runtimeBrokerId",
		},
		{
			name: "managed_with_expected_runtime_target_id",
			req: CreateAgentRequest{
				Name:                    "bad-mgd-target",
				ProjectID:               f.project.ID,
				ManagedAgent:            true,
				ExpectedRuntimeTargetID: f.flat.RuntimeTarget.ID,
				Task:                    "do work",
			},
			wantField: "expectedRuntimeTargetId",
		},
		{
			name: "legacy_sentinel_with_expected_runtime_target_id",
			req: CreateAgentRequest{
				Name:                    "bad-legacy-target",
				ProjectID:               f.project.ID,
				Profile:                 ManagedAgentsProfile,
				ExpectedRuntimeTargetID: f.flat.RuntimeTarget.ID,
				Task:                    "do work",
			},
			wantField: "expectedRuntimeTargetId",
		},
	}

	routes := []string{
		"/api/v1/agents",
		"/api/v1/projects/" + f.project.ID + "/agents",
	}

	for _, route := range routes {
		for _, tc := range cases {
			t.Run(route+"_"+tc.name, func(t *testing.T) {
				rec := doRequest(t, f.srv, http.MethodPost, route, tc.req)
				require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

				var body ErrorResponse
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
				assert.Equal(t, ErrCodeValidationError, body.Error.Code)
				assert.Equal(t, tc.wantField, body.Error.Details["field"])
			})
		}
	}

	ctx := context.Background()
	listed, err := f.s.ListAgents(ctx, store.AgentFilter{ProjectID: f.project.ID, IncludeDeleted: true}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, listed.Items, "contradictory requests must not persist any agent rows")
	assert.Empty(t, backend.inProgress(), "contradictory requests must not start any managed interactions")
	assert.False(t, f.client.createCalled, "contradictory requests must not dispatch to any Runtime Broker")
	assert.EqualValues(t, 0, brokerReservationCount(t, f.s, f.flat.ID))
	assert.EqualValues(t, 0, brokerReservationCount(t, f.s, f.legacy.ID))
}

// TestManagedAgentCreate_ExistingContainerAgentConflict verifies that a
// managed-agent create request colliding with an existing container agent
// returns 409 Conflict instead of resuming/starting the container agent on its
// Runtime Broker.
func TestManagedAgentCreate_ExistingContainerAgentConflict(t *testing.T) {
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	backend := newInteractionLedgerBackend()
	useManagedBackend(t, backend)

	ctx := context.Background()
	existing := &store.Agent{
		ID:              tid("existing-container-agent"),
		Slug:            "shared-slug",
		Name:            "shared-slug",
		ProjectID:       f.project.ID,
		RuntimeBrokerID: f.legacy.ID,
		Runtime:         "docker",
		Phase:           string(state.PhaseStopped),
		OwnerID:         DevUserID,
		CreatedBy:       DevUserID,
	}
	require.NoError(t, f.s.CreateAgent(ctx, existing))

	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:         "shared-slug",
		ProjectID:    f.project.ID,
		ManagedAgent: true,
		Resume:       true,
		Task:         "try managed over stopped container",
	})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.False(t, f.client.startCalled, "must not start existing container agent when ManagedAgent=true")
	assert.Empty(t, backend.inProgress(), "must not start a managed interaction on slug conflict")
}

// TestManagedAgentCreate_ContainerCreateCollidingWithManagedAgentConflict
// verifies the symmetric existing-agent guard: a container create request
// (even with Resume: true on a flat-broker project) colliding with a stopped
// brokerless managed-agent row returns 409 Conflict without starting on any
// Runtime Broker or persisting a runtimeBrokerId / flat runtime target pin onto
// the managed row.
func TestManagedAgentCreate_ContainerCreateCollidingWithManagedAgentConflict(t *testing.T) {
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})

	ctx := context.Background()
	require.NoError(t, f.s.RemoveProjectProvider(ctx, f.project.ID, f.legacy.ID))
	f.project.DefaultRuntimeBrokerID = f.flat.ID
	require.NoError(t, f.s.UpdateProject(ctx, f.project))

	existing := &store.Agent{
		ID:        tid("existing-brokerless-managed-agent"),
		Slug:      "shared-managed-slug",
		Name:      "shared-managed-slug",
		ProjectID: f.project.ID,
		Runtime:   "managed:google",
		Phase:     string(state.PhaseStopped),
		OwnerID:   DevUserID,
		CreatedBy: DevUserID,
	}
	require.NoError(t, f.s.CreateAgent(ctx, existing))

	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "shared-managed-slug",
		ProjectID: f.project.ID,
		Resume:    true,
		Task:      "try container resume over stopped managed row",
	})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.False(t, f.client.startCalled, "must not dispatch container start for a managed row")

	stored, err := f.s.GetAgent(ctx, existing.ID)
	require.NoError(t, err)
	assert.Equal(t, "managed:google", stored.Runtime)
	assert.Empty(t, stored.RuntimeBrokerID, "must not attach flat RuntimeBrokerID onto brokerless managed row")
	assert.False(t, stored.IsPinned(), "must not persist flat runtime target pin onto brokerless managed row")
	assert.Empty(t, stored.PinnedRuntimeBrokerID)
	assert.Empty(t, stored.PinnedRuntimeTargetID)
	assert.Empty(t, stored.PinnedRuntimeTargetType)
}

// TestManagedAgentCreate_HubSharedWorkspaceRemoteFlatBrokerProject covers Gate
// ptone/scion#3285 QA Finding F1: on a hub-managed shared-workspace project
// whose default Runtime Broker is a remote flat broker that does not mount the
// Hub's workspace storage, ordinary container create is refused with 409
// (remoteBrokerWorkspaceRefusal), whereas POST /api/v1/agents with omitted
// runtimeBrokerId succeeds (201 Created) via both ManagedAgent: true and the
// legacy Profile: ManagedAgentsProfile adapter.
func TestManagedAgentCreate_HubSharedWorkspaceRemoteFlatBrokerProject(t *testing.T) {
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	backend := newInteractionLedgerBackend()
	useManagedBackend(t, backend)

	ctx := context.Background()
	require.NoError(t, f.s.RemoveProjectProvider(ctx, f.project.ID, f.legacy.ID))
	f.project.DefaultRuntimeBrokerID = f.flat.ID
	f.project.GitRemote = "" // hub-managed shared workspace (mountsHubProjectWorkspace == true)
	require.NoError(t, f.s.UpdateProject(ctx, f.project))
	clearCreateAgentProviderPath(t, f.s, f.project)

	mountRoot := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, "share1"), 0o755))
	f.srv.config.ProjectStorageConfig = projectStorageFromWorkspace(nfsConfig(mountRoot))

	// Control: container create with omitted runtimeBrokerId hits the 409
	// remote-broker shared-workspace refusal.
	ctrl := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "ctrl-remote-ws-refusal",
		ProjectID: f.project.ID,
		Task:      "container work",
	})
	require.Equal(t, http.StatusConflict, ctrl.Code, ctrl.Body.String())

	for _, tc := range []struct {
		name string
		req  CreateAgentRequest
	}{
		{
			name: "explicit_managed_agent",
			req: CreateAgentRequest{
				Name:         "mgd-shared-ws-explicit",
				ProjectID:    f.project.ID,
				ManagedAgent: true,
				Task:         "managed work on shared-ws project",
			},
		},
		{
			name: "legacy_managed_agents_profile",
			req: CreateAgentRequest{
				Name:      "mgd-shared-ws-legacy",
				ProjectID: f.project.ID,
				Profile:   ManagedAgentsProfile,
				Task:      "legacy managed work on shared-ws project",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents", tc.req)
			row := requireManagedCreated(t, rec, f.s)
			assert.Equal(t, "managed:"+backend.Name(), row.Runtime)
			assert.True(t, isManagedAgentRuntime(row.Runtime))
			assert.Empty(t, row.RuntimeBrokerID)
			assert.False(t, row.IsPinned())
		})
	}
}
