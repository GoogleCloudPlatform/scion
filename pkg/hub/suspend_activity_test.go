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
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func createSuspendActivityTestAgent(t *testing.T, s store.Store, name, activity, stalledFrom string) *store.Agent {
	t.Helper()
	ctx := context.Background()

	project := &store.Project{
		ID:   tid("sa-proj-" + name),
		Name: "Suspend Activity " + name,
		Slug: "sa-proj-" + name,
	}
	require.NoError(t, s.CreateProject(ctx, project))

	broker := &store.RuntimeBroker{
		ID:       tid("sa-broker-" + name),
		Name:     "SA Broker " + name,
		Slug:     "sa-broker-" + name,
		Status:   store.BrokerStatusOnline,
		Endpoint: "http://localhost:9800",
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	now := time.Now().Add(-5 * time.Minute)
	agent := &store.Agent{
		ID:                  tid("sa-agent-" + name),
		Name:                "sa-" + name,
		Slug:                "sa-" + name,
		ProjectID:           project.ID,
		RuntimeBrokerID:     broker.ID,
		Phase:               string(state.PhaseRunning),
		Activity:            activity,
		StalledFromActivity: stalledFrom,
		ToolName:            "bash",
		ActivitySince:       &now,
		Created:             now,
		Updated:             now,
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	_, err := s.SetAgentRunID(ctx, agent.ID, "run-1", nil)
	require.NoError(t, err)
	return mustGetAgent(t, s, agent.ID)
}

func waitAgentStatusEvent(t *testing.T, ch <-chan Event, agentID string) (AgentStatusEvent, map[string]any) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case evt := <-ch:
			var statusEvt AgentStatusEvent
			require.NoError(t, json.Unmarshal(evt.Data, &statusEvt))
			if statusEvt.AgentID != agentID {
				continue
			}
			var raw map[string]any
			require.NoError(t, json.Unmarshal(evt.Data, &raw))
			return statusEvt, raw
		case <-deadline:
			t.Fatalf("timed out waiting for AgentStatusEvent for %s", agentID)
			return AgentStatusEvent{}, nil
		}
	}
}

func TestSuspendPathsRetainEligibleActivityAndClearOthers(t *testing.T) {
	type activityCase struct {
		name         string
		activity     string
		stalledFrom  string
		wantActivity string
	}

	paths := []struct {
		name       string
		wantReason string
		cases      []activityCase
		run        func(t *testing.T, srv *Server, s store.Store, a *store.Agent) *store.Agent
	}{
		{
			name:       "http-suspend",
			wantReason: string(state.SuspendReasonUser),
			cases: []activityCase{
				{name: "completed", activity: string(state.ActivityCompleted), wantActivity: string(state.ActivityCompleted)},
				{name: "idle", activity: string(state.ActivityIdle), wantActivity: string(state.ActivityIdle)},
				{name: "harness_error", activity: string(state.ActivityHarnessError), wantActivity: string(state.ActivityHarnessError)},
				{name: "blocked", activity: string(state.ActivityBlocked), wantActivity: ""},
				{name: "waiting_for_input", activity: string(state.ActivityWaitingForInput), wantActivity: ""},
				{name: "working", activity: string(state.ActivityWorking), wantActivity: ""},
				{name: "stalled_from_completed", activity: string(state.ActivityStalled), stalledFrom: string(state.ActivityCompleted), wantActivity: string(state.ActivityCompleted)},
				{name: "stalled_from_idle", activity: string(state.ActivityStalled), stalledFrom: string(state.ActivityIdle), wantActivity: string(state.ActivityIdle)},
				{name: "stalled_from_harness_error", activity: string(state.ActivityStalled), stalledFrom: string(state.ActivityHarnessError), wantActivity: string(state.ActivityHarnessError)},
				{name: "stalled_from_working", activity: string(state.ActivityStalled), stalledFrom: string(state.ActivityWorking), wantActivity: ""},
			},
			run: func(t *testing.T, srv *Server, _ store.Store, a *store.Agent) *store.Agent {
				rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/suspend", nil)
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				var httpAgent store.Agent
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &httpAgent))
				return &httpAgent
			},
		},
		{
			name:       "auto-suspend",
			wantReason: string(state.SuspendReasonStalled),
			cases: []activityCase{
				{name: "stalled_from_completed", activity: string(state.ActivityStalled), stalledFrom: string(state.ActivityCompleted), wantActivity: string(state.ActivityCompleted)},
				{name: "stalled_from_idle", activity: string(state.ActivityStalled), stalledFrom: string(state.ActivityIdle), wantActivity: string(state.ActivityIdle)},
				{name: "stalled_from_harness_error", activity: string(state.ActivityStalled), stalledFrom: string(state.ActivityHarnessError), wantActivity: string(state.ActivityHarnessError)},
				{name: "stalled_from_working", activity: string(state.ActivityStalled), stalledFrom: string(state.ActivityWorking), wantActivity: ""},
				{name: "stalled_from_waiting", activity: string(state.ActivityStalled), stalledFrom: string(state.ActivityWaitingForInput), wantActivity: ""},
			},
			run: func(t *testing.T, srv *Server, s store.Store, a *store.Agent) *store.Agent {
				srv.autoSuspendStalledAgents(context.Background(), []store.Agent{*mustGetAgent(t, s, a.ID)})
				return nil
			},
		},
		{
			name:       "membership-hold",
			wantReason: "",
			cases: []activityCase{
				{name: "completed", activity: string(state.ActivityCompleted), wantActivity: string(state.ActivityCompleted)},
				{name: "idle", activity: string(state.ActivityIdle), wantActivity: string(state.ActivityIdle)},
				{name: "harness_error", activity: string(state.ActivityHarnessError), wantActivity: string(state.ActivityHarnessError)},
				{name: "blocked", activity: string(state.ActivityBlocked), wantActivity: ""},
				{name: "waiting_for_input", activity: string(state.ActivityWaitingForInput), wantActivity: ""},
				{name: "working", activity: string(state.ActivityWorking), wantActivity: ""},
			},
			run: func(t *testing.T, srv *Server, _ store.Store, a *store.Agent) *store.Agent {
				require.NoError(t, srv.stopHeldAgent(context.Background(), a.ID, 1))
				return nil
			},
		},
	}

	for _, path := range paths {
		t.Run(path.name, func(t *testing.T) {
			for i, tc := range path.cases {
				t.Run(tc.name, func(t *testing.T) {
					srv, s, _, _ := engineTestServer(t)
					statusCh, unsub := srv.events.Subscribe("project.>.agent.status")
					defer unsub()

					agent := createSuspendActivityTestAgent(
						t, s, fmt.Sprintf("%s-%d", path.name, i), tc.activity, tc.stalledFrom,
					)

					httpResp := path.run(t, srv, s, agent)

					persisted := mustGetAgent(t, s, agent.ID)
					assert.Equal(t, string(state.PhaseSuspended), persisted.Phase)
					assert.Equal(t, path.wantReason, persisted.SuspendReason)
					assert.Equal(t, tc.wantActivity, persisted.Activity)
					assert.Empty(t, persisted.StalledFromActivity)
					assert.Empty(t, persisted.ToolName)
					assert.Nil(t, persisted.ActivitySince, "activity_since must clear when leaving running")

					if httpResp != nil {
						assert.Equal(t, string(state.PhaseSuspended), httpResp.Phase)
						assert.Equal(t, path.wantReason, httpResp.SuspendReason, "HTTP response must carry suspend reason")
						assert.Equal(t, tc.wantActivity, httpResp.Activity, "HTTP response must carry retained/cleared activity")
					}

					evt, raw := waitAgentStatusEvent(t, statusCh, agent.ID)
					assert.Equal(t, string(state.PhaseSuspended), evt.Phase)
					assert.Equal(t, tc.wantActivity, evt.Activity, "SSE status event must carry retained/cleared activity")
					rawAct, present := raw["activity"]
					assert.True(t, present, "SSE status event JSON must always include activity key so web clears stale values")
					assert.Equal(t, tc.wantActivity, rawAct)
				})
			}
		})
	}
}

func TestSuspendedAgentWithRetainedActivityBlocksRegressionGuard(t *testing.T) {
	agent := &store.Agent{
		ID:       tid("sa-guard"),
		Phase:    string(state.PhaseSuspended),
		Activity: string(state.ActivityCompleted),
	}

	lateWorking := store.AgentStatusUpdate{
		Phase:    string(state.PhaseRunning),
		Activity: string(state.ActivityWorking),
	}
	guardAgentPhaseTransition(agent, &lateWorking)
	assert.Empty(t, lateWorking.Phase, "suspended -> running without Restore must be blocked by regression guard")

	lateActivityOnly := store.AgentStatusUpdate{
		Activity: string(state.ActivityWorking),
	}
	guardAgentPhaseTransition(agent, &lateActivityOnly)
	assert.Empty(t, lateActivityOnly.Phase, "activity report on suspended agent must not auto-promote phase to running")
}

func TestResumePathsClearRetainedSuspendActivity(t *testing.T) {
	t.Run("lifecycle start clears retained activity in store, response, and SSE event", func(t *testing.T) {
		srv, s, _, _ := engineTestServer(t)
		agent := createSuspendActivityTestAgent(t, s, "resume-start", string(state.ActivityCompleted), "")
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/suspend", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Equal(t, string(state.ActivityCompleted), mustGetAgent(t, s, agent.ID).Activity)

		statusCh, unsub := srv.events.Subscribe("project.>.agent.status")
		defer unsub()

		startRec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", nil)
		require.Equal(t, http.StatusOK, startRec.Code, startRec.Body.String())

		var startedHTTP store.Agent
		require.NoError(t, json.Unmarshal(startRec.Body.Bytes(), &startedHTTP))
		assert.Equal(t, string(state.PhaseRunning), startedHTTP.Phase)
		assert.Empty(t, startedHTTP.Activity, "lifecycle start HTTP response must not carry pre-suspend activity")

		persisted := mustGetAgent(t, s, agent.ID)
		assert.Equal(t, string(state.PhaseRunning), persisted.Phase)
		assert.Empty(t, persisted.Activity, "lifecycle start must clear retained activity in store")

		evt, raw := waitAgentStatusEvent(t, statusCh, agent.ID)
		assert.Equal(t, string(state.PhaseRunning), evt.Phase)
		assert.Empty(t, evt.Activity)
		rawAct, present := raw["activity"]
		assert.True(t, present, "SSE status event JSON must explicitly send empty activity on resume")
		assert.Equal(t, "", rawAct)
	})

	t.Run("handleExistingAgent resume clears retained activity", func(t *testing.T) {
		f := handleExistingAgentAuthzSetup(t)
		f.srv.SetDispatcher(&createAgentDispatcher{})
		agent := f.agent(t, "hea-retained-resume", string(state.PhaseSuspended))
		agent.Activity = string(state.ActivityCompleted)
		agent.ToolName = "bash"
		require.NoError(t, f.store.UpdateAgent(context.Background(), agent))

		rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, "/api/v1/agents", map[string]any{
			"name":      agent.Slug,
			"projectId": f.project.ID,
			"resume":    true,
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		var resp struct {
			Agent store.Agent `json:"agent"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, string(state.PhaseRunning), resp.Agent.Phase)
		assert.Empty(t, resp.Agent.Activity, "handleExistingAgent response must clear retained activity")

		persisted := mustGetAgent(t, f.store, agent.ID)
		assert.Equal(t, string(state.PhaseRunning), persisted.Phase)
		assert.Empty(t, persisted.Activity, "handleExistingAgent must clear retained activity in store")
		assert.Empty(t, persisted.ToolName)
	})

	t.Run("wakeAgentForDM clears retained activity before readiness wait", func(t *testing.T) {
		srv, s, _, target := createWakeDMFixtures(t, string(state.PhaseSuspended))
		target.Activity = string(state.ActivityCompleted)
		require.NoError(t, s.UpdateAgent(context.Background(), target))

		disp := &wakeTrackingDispatcher{}
		srv.SetDispatcher(disp)

		var readySent atomic.Bool
		go func() {
			time.Sleep(150 * time.Millisecond)
			readySent.Store(true)
			_ = s.UpdateAgentStatus(context.Background(), target.ID, store.AgentStatusUpdate{
				Phase:    string(state.PhaseRunning),
				Activity: string(state.ActivityIdle),
			})
		}()

		result, dmErr := srv.wakeAgentForDM(context.Background(), target)
		require.Nil(t, dmErr)
		require.NotNil(t, result)
		assert.Equal(t, WakeResumed, result.Outcome)
		assert.True(t, readySent.Load(), "wakeAgentForDM must wait for the new run's activity rather than returning early on retained completed")
		assert.Equal(t, string(state.ActivityIdle), mustGetAgent(t, s, target.ID).Activity)
	})

	t.Run("failed wake rollback restores retained activity", func(t *testing.T) {
		srv, s, _, target := createWakeDMFixtures(t, string(state.PhaseSuspended))
		target.Activity = string(state.ActivityCompleted)
		require.NoError(t, s.UpdateAgent(context.Background(), target))

		srv.SetDispatcher(&wakeTrackingDispatcher{startReturnErr: assert.AnError})

		_, dmErr := srv.wakeAgentForDM(context.Background(), target)
		require.NotNil(t, dmErr)
		persisted := mustGetAgent(t, s, target.ID)
		assert.Equal(t, string(state.PhaseSuspended), persisted.Phase)
		assert.Equal(t, string(state.ActivityCompleted), persisted.Activity, "failed wake rollback must restore retained suspend activity")
	})
}

func enableIdleSuspendForTest(t *testing.T, srv *Server) {
	t.Helper()
	var active []experiments.Experiment
	for _, e := range experiments.Default().All() {
		if e.Name == experiments.IdleSuspend {
			e.Default = true
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, nil)
	require.NoError(t, err)
	srv.experiments = reg
}

func TestAutoWakeClearsRetainedSuspendActivity(t *testing.T) {
	ctx := context.Background()

	for _, reason := range []state.SuspendReason{state.SuspendReasonIdle, state.SuspendReasonStalled} {
		for _, retained := range []state.Activity{state.ActivityCompleted, state.ActivityIdle, state.ActivityHarnessError} {
			t.Run(string(reason)+"_"+string(retained), func(t *testing.T) {
				srv, s, sender, target := createWakeDMFixtures(t, string(state.PhaseRunning))
				enableIdleSuspendForTest(t, srv)
				t.Cleanup(func() { srv.autoWakeWork.wg.Wait() })

				require.NoError(t, s.UpdateAgentStatus(ctx, target.ID, store.AgentStatusUpdate{
					Phase:    string(state.PhaseRunning),
					Activity: string(retained),
				}))
				require.NoError(t, s.UpdateAgentStatus(ctx, target.ID, store.AgentStatusUpdate{
					Phase:         string(state.PhaseSuspended),
					SuspendReason: string(reason),
				}))
				suspended := mustGetAgent(t, s, target.ID)
				require.Equal(t, string(retained), suspended.Activity)
				require.Equal(t, string(reason), suspended.SuspendReason)

				disp := &wakeTrackingDispatcher{}
				srv.SetDispatcher(disp)

				var readySent atomic.Bool
				go func() {
					time.Sleep(150 * time.Millisecond)
					readySent.Store(true)
					_ = s.UpdateAgentStatus(ctx, target.ID, store.AgentStatusUpdate{
						Phase:    string(state.PhaseRunning),
						Activity: string(state.ActivityWorking),
					})
				}()

				res, dmErr := srv.ExecuteAgentDM(ctx, &AgentDMInput{
					SenderAgent:    sender,
					SenderIdentity: wakeDMSenderIdentity(sender, ScopeProjectRead),
					TargetAgent:    suspended,
					Msg:            "auto-wake work",
					Type:           "instruction",
					ProjectID:      sender.ProjectID,
				})
				require.Nil(t, dmErr)
				require.NotNil(t, res)
				assert.Equal(t, AgentDMAccepted, res.Outcome)
				assert.True(t, readySent.Load(), "auto-wake must clear retained %s before waitForAgentReady rather than returning early", retained)
				require.Len(t, disp.getStartCalls(), 1)
				require.Len(t, disp.getMessageCalls(), 1)
				assert.Equal(t, "auto-wake work", disp.getMessageCalls()[0].Message)

				woken := mustGetAgent(t, s, target.ID)
				assert.Equal(t, string(state.PhaseRunning), woken.Phase)
				assert.Equal(t, string(state.ActivityWorking), woken.Activity)
				assert.Empty(t, woken.SuspendReason, "auto-wake resume must clear suspend_reason")
			})
		}
	}

	t.Run("failed auto-wake rollback preserves both suspend_reason and retained activity", func(t *testing.T) {
		srv, s, sender, target := createWakeDMFixtures(t, string(state.PhaseRunning))
		enableIdleSuspendForTest(t, srv)
		t.Cleanup(func() { srv.autoWakeWork.wg.Wait() })

		require.NoError(t, s.UpdateAgentStatus(ctx, target.ID, store.AgentStatusUpdate{
			Phase:    string(state.PhaseRunning),
			Activity: string(state.ActivityCompleted),
		}))
		require.NoError(t, s.UpdateAgentStatus(ctx, target.ID, store.AgentStatusUpdate{
			Phase:         string(state.PhaseSuspended),
			SuspendReason: string(state.SuspendReasonIdle),
		}))
		suspended := mustGetAgent(t, s, target.ID)

		srv.SetDispatcher(&wakeTrackingDispatcher{startReturnErr: assert.AnError})

		_, dmErr := srv.ExecuteAgentDM(ctx, &AgentDMInput{
			SenderAgent:    sender,
			SenderIdentity: wakeDMSenderIdentity(sender, ScopeProjectRead),
			TargetAgent:    suspended,
			Msg:            "auto-wake fail",
			Type:           "instruction",
			ProjectID:      sender.ProjectID,
		})
		require.NotNil(t, dmErr)

		rolledBack := mustGetAgent(t, s, target.ID)
		assert.Equal(t, string(state.PhaseSuspended), rolledBack.Phase)
		assert.Equal(t, string(state.SuspendReasonIdle), rolledBack.SuspendReason)
		assert.Equal(t, string(state.ActivityCompleted), rolledBack.Activity)
	})
}
