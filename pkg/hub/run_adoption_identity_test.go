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
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func recordTestRunCredential(t *testing.T, srv *Server, s store.Store, agent *store.Agent, runID string) {
	t.Helper()
	_, cred, err := srv.agentTokenService.SignAgentToken(AgentTokenGrant{
		AgentID: agent.ID, ProjectID: agent.ProjectID, Scopes: ScopesForRole(AgentRoleFull),
	}, runID)
	require.NoError(t, err)
	require.NoError(t, s.CreateAgentCredential(context.Background(), cred))
}

// TestAdoptBrokerRunID_IdentityAndCredentialChecks verifies that
// adoptBrokerRunID requires matching project, matching agent ID, and a
// hub-minted credential for the reported run before adopting a different run
// ID, and keeps the minted run otherwise (ptone/scion#8240).
func TestAdoptBrokerRunID_IdentityAndCredentialChecks(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name        string
		mintCred    bool
		info        func(a *store.Agent, foreignRun string) *RemoteAgentInfo
		wantAdopted bool
	}{
		{
			name:     "rejects foreign run with no minted credential",
			mintCred: false,
			info: func(a *store.Agent, run string) *RemoteAgentInfo {
				return &RemoteAgentInfo{
					ID: a.ID, Slug: a.Slug, ProjectID: a.ProjectID,
					Labels: map[string]string{"agent_id": a.ID, "scion.project_id": a.ProjectID},
					RunID:  run,
				}
			},
			wantAdopted: false,
		},
		{
			name:     "rejects project mismatch even with minted credential",
			mintCred: true,
			info: func(a *store.Agent, run string) *RemoteAgentInfo {
				return &RemoteAgentInfo{
					ID: a.ID, Slug: a.Slug, ProjectID: "other-project",
					Labels: map[string]string{"agent_id": a.ID, "scion.project_id": "other-project"},
					RunID:  run,
				}
			},
			wantAdopted: false,
		},
		{
			name:     "rejects missing project ID even with minted credential",
			mintCred: true,
			info: func(a *store.Agent, run string) *RemoteAgentInfo {
				return &RemoteAgentInfo{
					ID: a.ID, Slug: a.Slug,
					Labels: map[string]string{"agent_id": a.ID},
					RunID:  run,
				}
			},
			wantAdopted: false,
		},
		{
			name:     "rejects agent ID mismatch even with minted credential",
			mintCred: true,
			info: func(a *store.Agent, run string) *RemoteAgentInfo {
				return &RemoteAgentInfo{
					ID: "other-agent-id", Slug: a.Slug, ProjectID: a.ProjectID,
					Labels: map[string]string{"agent_id": "other-agent-id", "scion.project_id": a.ProjectID},
					RunID:  run,
				}
			},
			wantAdopted: false,
		},
		{
			name:     "rejects missing agent ID even with minted credential",
			mintCred: true,
			info: func(a *store.Agent, run string) *RemoteAgentInfo {
				return &RemoteAgentInfo{
					Slug: a.Slug, ProjectID: a.ProjectID,
					Labels: map[string]string{"scion.project_id": a.ProjectID},
					RunID:  run,
				}
			},
			wantAdopted: false,
		},
		{
			name:     "accepts matching project, labels agent_id, and minted credential (docker shape: empty info.ID)",
			mintCred: true,
			info: func(a *store.Agent, run string) *RemoteAgentInfo {
				return &RemoteAgentInfo{
					Slug: a.Slug, ProjectID: a.ProjectID,
					Labels: map[string]string{"agent_id": a.ID, "scion.project_id": a.ProjectID},
					RunID:  run,
				}
			},
			wantAdopted: true,
		},
		{
			name:     "accepts Cloud Run sanitized scion_project_id label",
			mintCred: true,
			info: func(a *store.Agent, run string) *RemoteAgentInfo {
				return &RemoteAgentInfo{
					ID: a.ID, Slug: a.Slug,
					Labels: map[string]string{"agent_id": a.ID, "scion_project_id": a.ProjectID},
					RunID:  run,
				}
			},
			wantAdopted: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s := testServer(t)
			brokerID := tid("broker-adopt-" + tc.name)
			require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
				ID: brokerID, Name: "broker", Slug: "broker",
				Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
			}))
			project := &store.Project{ID: tid("project-adopt-" + tc.name), Slug: "p", Name: "p", Created: time.Now(), Updated: time.Now()}
			require.NoError(t, s.CreateProject(ctx, project))
			agent := &store.Agent{
				ID: tid("agent-adopt-" + tc.name), Slug: "worker", Name: "worker", ProjectID: project.ID,
				Phase: string(state.PhaseRunning), RuntimeBrokerID: brokerID, StateVersion: 1,
				Created: time.Now(), Updated: time.Now(),
			}
			require.NoError(t, s.CreateAgent(ctx, agent))

			const mintedRun = "run-minted"
			const reportedRun = "run-reported"
			_, err := s.SetAgentRunID(ctx, agent.ID, mintedRun, nil)
			require.NoError(t, err)
			if tc.mintCred {
				recordTestRunCredential(t, srv, s, agent, reportedRun)
			}
			agent = mustGetAgent(t, s, agent.ID)

			d := NewHTTPAgentDispatcherWithClient(s, &mockRuntimeBrokerClient{}, false, slog.Default())
			settled := d.adoptBrokerRunID(ctx, agent, mintedRun, &RemoteAgentResponse{
				Agent: tc.info(agent, reportedRun),
			})

			got := mustGetAgent(t, s, agent.ID)
			if tc.wantAdopted {
				assert.Equal(t, reportedRun, got.RunID)
				assert.Equal(t, reportedRun, agent.RunID)
				assert.Equal(t, reportedRun, settled)
			} else {
				assert.Equal(t, mintedRun, got.RunID, "foreign run must not be adopted")
				assert.Equal(t, mintedRun, agent.RunID, "in-memory agent must keep minted run")
				assert.Equal(t, mintedRun, settled, "settled run must remain minted when adoption is rejected")
			}
		})
	}
}

// TestSettleLaunchedRun_RejectsForeignRun verifies that settleLaunchedRun
// refuses to adopt a broker-reported run ID when project, agent ID, or
// hub-minted credential checks fail (ptone/scion#8240).
func TestSettleLaunchedRun_RejectsForeignRun(t *testing.T) {
	t.Run("unminted credential", func(t *testing.T) {
		f := newLaunchSettleFixture(t, "lrsettle-foreign-unminted")
		f.report(t, store.LaunchReportStateSucceeded, "run-foreign-unminted")
		got := mustGetAgent(t, f.store, f.agent.ID)
		assert.Equal(t, "run-1", got.RunID, "foreign run with no minted credential must not be adopted")
	})

	t.Run("project mismatch", func(t *testing.T) {
		f := newLaunchSettleFixture(t, "lrsettle-foreign-proj")
		rec := postLaunchReport(t, f.srv, f.brokerID, f.agent.ID, f.brokerID, AgentLaunchReport{
			LaunchID: f.launchID, InstanceID: "i1", State: store.LaunchReportStateSucceeded,
			Agent: &RemoteAgentInfo{
				ID: f.agent.ID, Slug: f.agent.Slug, ProjectID: "other-project",
				Labels: map[string]string{"agent_id": f.agent.ID, "scion.project_id": "other-project"},
				RunID:  "run-0",
			},
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		got := mustGetAgent(t, f.store, f.agent.ID)
		assert.Equal(t, "run-1", got.RunID, "foreign project entry must not be adopted")
	})

	t.Run("agent ID mismatch", func(t *testing.T) {
		f := newLaunchSettleFixture(t, "lrsettle-foreign-agent")
		rec := postLaunchReport(t, f.srv, f.brokerID, f.agent.ID, f.brokerID, AgentLaunchReport{
			LaunchID: f.launchID, InstanceID: "i1", State: store.LaunchReportStateSucceeded,
			Agent: &RemoteAgentInfo{
				ID: "other-agent", Slug: f.agent.Slug, ProjectID: f.agent.ProjectID,
				Labels: map[string]string{"agent_id": "other-agent", "scion.project_id": f.agent.ProjectID},
				RunID:  "run-0",
			},
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		got := mustGetAgent(t, f.store, f.agent.ID)
		assert.Equal(t, "run-1", got.RunID, "foreign agent entry must not be adopted")
	})
}

// TestCompensateLandedRun_UsesSettledRun verifies that when an agent is
// deleted mid-start:
//   - if the broker returns a foreign run ID whose adoption is rejected,
//     compensateLandedRun deletes with the minted (settled) run ID;
//   - if the broker returns a verified kept run of the same agent,
//     compensateLandedRun deletes with that adopted run ID (ptone/scion#8240).
func TestCompensateLandedRun_UsesSettledRun(t *testing.T) {
	ctx := context.Background()

	t.Run("foreign run rejected deletes minted run", func(t *testing.T) {
		_, s := testServer(t)
		brokerID := tid("broker-comp-foreign")
		require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
			ID: brokerID, Name: "broker-comp-foreign", Slug: "broker-comp-foreign",
			Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
		}))
		project := &store.Project{ID: tid("project-comp-foreign"), Slug: "comp-foreign", Name: "comp-foreign", Created: time.Now(), Updated: time.Now()}
		require.NoError(t, s.CreateProject(ctx, project))
		agent := &store.Agent{
			ID: tid("agent-comp-foreign"), Slug: "worker", Name: "worker", ProjectID: project.ID,
			Phase: string(state.PhaseRunning), RuntimeBrokerID: brokerID, StateVersion: 1,
			Created: time.Now(), Updated: time.Now(),
		}
		require.NoError(t, s.CreateAgent(ctx, agent))

		const mintedRun = "run-minted"
		const foreignRun = "run-foreign"
		_, err := s.SetAgentRunID(ctx, agent.ID, mintedRun, nil)
		require.NoError(t, err)
		agent = mustGetAgent(t, s, agent.ID)

		// Deletion wins while the start was in flight.
		require.NoError(t, s.DeleteAgent(ctx, agent.ID))

		client := &heldRunClient{mockRuntimeBrokerClient: &mockRuntimeBrokerClient{}, held: mintedRun}
		d := NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default())
		d.settleLandedRun(ctx, agent, mintedRun, true, &RemoteAgentResponse{
			Agent: &RemoteAgentInfo{
				ID:        "other-agent",
				Slug:      agent.Slug,
				ProjectID: "other-project",
				Labels:    map[string]string{"agent_id": "other-agent", "scion.project_id": "other-project"},
				RunID:     foreignRun,
			},
		})

		assert.Equal(t, []string{mintedRun}, client.deleteRuns, "compensating delete must target the settled (minted) run, not the rejected foreign run")
		assert.Empty(t, client.held, "the newly created container for the minted run must be removed")
	})

	t.Run("verified kept run deletes adopted run", func(t *testing.T) {
		srv, s := testServer(t)
		brokerID := tid("broker-comp-kept")
		require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
			ID: brokerID, Name: "broker-comp-kept", Slug: "broker-comp-kept",
			Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
		}))
		project := &store.Project{ID: tid("project-comp-kept"), Slug: "comp-kept", Name: "comp-kept", Created: time.Now(), Updated: time.Now()}
		require.NoError(t, s.CreateProject(ctx, project))
		agent := &store.Agent{
			ID: tid("agent-comp-kept"), Slug: "worker", Name: "worker", ProjectID: project.ID,
			Phase: string(state.PhaseRunning), RuntimeBrokerID: brokerID, StateVersion: 1,
			Created: time.Now(), Updated: time.Now(),
		}
		require.NoError(t, s.CreateAgent(ctx, agent))

		const keptRun = "run-kept"
		const mintedRun = "run-minted"
		recordTestRunCredential(t, srv, s, agent, keptRun)
		_, err := s.SetAgentRunID(ctx, agent.ID, mintedRun, nil)
		require.NoError(t, err)
		agent = mustGetAgent(t, s, agent.ID)

		// Soft-delete wins while the start was in flight (credential row still exists).
		now := time.Now()
		_, err = s.FinalizeAgentDeletion(ctx, agent.ID, store.DeletionPredicate{}, store.DeletionFinalizeSoft, store.DeletionFields{DeletedAt: &now}, nil)
		require.NoError(t, err)

		client := &heldRunClient{mockRuntimeBrokerClient: &mockRuntimeBrokerClient{}, held: keptRun}
		d := NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default())
		d.settleLandedRun(ctx, agent, mintedRun, true, &RemoteAgentResponse{
			Agent: &RemoteAgentInfo{
				ID:        agent.ID,
				Slug:      agent.Slug,
				ProjectID: agent.ProjectID,
				Labels:    map[string]string{"agent_id": agent.ID, "scion.project_id": agent.ProjectID},
				RunID:     keptRun,
			},
		})

		assert.Equal(t, []string{keptRun}, client.deleteRuns, "compensating delete must target the verified adopted run")
		assert.Empty(t, client.held, "the kept container must be removed")
	})
}

// TestRemoteAgentInfo_ProjectIDAndLabelsWire verifies that projectId and labels
// sent by the broker round-trip into RemoteAgentInfo (ptone/scion#8240).
func TestRemoteAgentInfo_ProjectIDAndLabelsWire(t *testing.T) {
	wire := hubclient.AgentLaunchReportInfo{
		ID:        "agent-1",
		Slug:      "worker",
		ProjectID: "proj-1",
		Labels:    map[string]string{"agent_id": "agent-1", "scion.project_id": "proj-1"},
		RunID:     "run-1",
	}
	b, err := json.Marshal(wire)
	require.NoError(t, err)
	var got RemoteAgentInfo
	require.NoError(t, json.Unmarshal(b, &got))
	assert.Equal(t, "proj-1", got.ProjectID)
	assert.Equal(t, map[string]string{"agent_id": "agent-1", "scion.project_id": "proj-1"}, got.Labels)
}
