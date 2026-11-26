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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/msgaction"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type cmdRunAuditCollector struct {
	*LogAuditLogger
	mu     sync.Mutex
	events []*MessageActionEvent
}

func (c *cmdRunAuditCollector) LogMessageActionEvent(_ context.Context, ev *MessageActionEvent) error {
	if ev == nil {
		return nil
	}
	cp := *ev
	if ev.Details != nil {
		cp.Details = make(map[string]string, len(ev.Details))
		for k, v := range ev.Details {
			cp.Details[k] = v
		}
	}
	c.mu.Lock()
	c.events = append(c.events, &cp)
	c.mu.Unlock()
	return nil
}

func (c *cmdRunAuditCollector) snapshot() []*MessageActionEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*MessageActionEvent, len(c.events))
	copy(out, c.events)
	return out
}

func (t *commandRunTracker) owns(runID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.owned[runID]
	return ok
}

type cmdRunExecFixture struct {
	*ptyConduitFixture
	audit   *cmdRunAuditCollector
	capture *execCapture
	asker   *store.Agent
}

func newCmdRunExecFixture(t *testing.T, capture *execCapture) *cmdRunExecFixture {
	t.Helper()
	// Speed up polling in tests.
	scheduledExecSeamMu.Lock()
	origInit, origMax := commandRunPollInitial, commandRunPollMax
	commandRunPollInitial = 20 * time.Millisecond
	commandRunPollMax = 50 * time.Millisecond
	scheduledExecSeamMu.Unlock()
	t.Cleanup(func() {
		scheduledExecSeamMu.Lock()
		commandRunPollInitial, commandRunPollMax = origInit, origMax
		scheduledExecSeamMu.Unlock()
	})

	audit := &cmdRunAuditCollector{LogAuditLogger: NewLogAuditLogger("", false)}
	f := newPTYConduitFixtureServer(t, func(s *Server) {
		s.auditLogger = audit
	})
	setHubExperiments(t, f.srv, map[string]bool{
		conduitExperiment:           true,
		experiments.ExecContainers:  true,
		experiments.CommandRequests: true,
	})

	ctx := context.Background()
	// Turn f.launched into a shell execution container owned by f.owner.
	ag, err := f.store.GetAgent(ctx, f.launched.ID)
	require.NoError(t, err)
	ag.HarnessConfig = config.ShellHarnessConfigName
	ag.AppliedConfig = &store.AgentAppliedConfig{HarnessConfig: config.ShellHarnessConfigName}
	ag.OwnerID = f.owner.ID()
	ag.Phase = string(state.PhaseRunning)
	require.NoError(t, f.store.UpdateAgent(ctx, ag))
	f.launched = ag

	// Register it as f.owner's execution container.
	_, err = f.store.PutUserExecContainer(ctx, f.owner.ID(), ag.ID)
	require.NoError(t, err)

	// Create an asking LLM agent in the same project.
	asker := &store.Agent{
		ID:            api.NewUUID(),
		Slug:          "asker-agent",
		Name:          "asker-agent",
		ProjectID:     ag.ProjectID,
		OwnerID:       f.owner.ID(),
		HarnessConfig: "claude",
		Phase:         string(state.PhaseRunning),
	}
	require.NoError(t, f.store.CreateAgent(ctx, asker))

	if capture == nil {
		capture = &execCapture{}
	}
	f.startControlAgentWithOptions(t, f.public.URL, true, execControlOptions(t), capture.wrap)

	return &cmdRunExecFixture{
		ptyConduitFixture: f,
		audit:             audit,
		capture:           capture,
		asker:             asker,
	}
}

func (f *cmdRunExecFixture) createApprovedRun(t *testing.T, key, script, workdir string, timeoutS int) (*store.CommandRequest, *store.CommandRun) {
	t.Helper()
	ctx := context.Background()
	if timeoutS <= 0 {
		timeoutS = 30
	}
	now := time.Now().UTC()
	norm, err := messages.NormalizeCommandRequest("command_request", &messages.CommandRequestInput{
		Key:      key,
		Script:   script,
		Workdir:  workdir,
		TimeoutS: timeoutS,
	})
	require.NoError(t, err)

	promptRes, err := f.store.CreateCommandRequestPrompt(ctx, &store.CommandRequestPromptInput{
		Message: &store.Message{
			ID:             api.NewUUID(),
			ProjectID:      f.launched.ProjectID,
			ConversationID: api.NewUUID(),
			ThreadID:       "dm:user:" + f.owner.ID() + ":agent:" + f.asker.ID,
			Sender:         "agent:" + f.asker.Slug,
			SenderID:       f.asker.ID,
			Recipient:      "user:" + f.owner.Email(),
			RecipientID:    f.owner.ID(),
		},
		AgentID:    f.asker.ID,
		AgentName:  f.asker.Slug,
		ApproverID: f.owner.ID(),
		Request:    norm,
		Now:        now,
		TTL:        msgaction.CommandRequestTTL,
	})
	require.NoError(t, err)

	appRes, err := f.store.ApproveCommandRequest(ctx, &store.ApproveCommandRequestInput{
		RequestID:      promptRes.Request.ID,
		CommandSHA256:  promptRes.Request.CommandSHA256,
		UserID:         f.owner.ID(),
		UserName:       f.owner.DisplayName(),
		UserEmail:      f.owner.Email(),
		Surface:        msgaction.CommandRequestSurfaceCLI,
		IdempotencyKey: "idem-" + key,
		ExecAgentID:    f.launched.ID,
		ExecProjectID:  f.launched.ProjectID,
		ShareOutput:    false,
		LeaseUntil:     now.Add(5 * time.Minute),
		Now:            now,
	})
	require.NoError(t, err)
	require.NotNil(t, appRes.Run)
	return appRes.Request, appRes.Run
}

func (f *cmdRunExecFixture) waitRunFinished(t *testing.T, runID string) *store.CommandRun {
	t.Helper()
	var got *store.CommandRun
	require.Eventually(t, func() bool {
		r, err := f.store.GetCommandRun(context.Background(), runID)
		if err != nil || r == nil {
			return false
		}
		if r.FinishedAt != nil {
			got = r
			return true
		}
		return false
	}, 10*time.Second, 25*time.Millisecond, "command run %s did not finish", runID)
	return got
}

func TestCommandRunExec_EndToEnd_SucceededAuditAndOutcomeResponse(t *testing.T) {
	f := newCmdRunExecFixture(t, nil)
	secretPayload := "echo SUPER_SECRET_SCRIPT_TOKEN_XYZ"
	req, run := f.createApprovedRun(t, "deploy-check", secretPayload, "", 30)

	f.srv.EnqueueCommandRun(req, run)
	fin := f.waitRunFinished(t, run.ID)

	assert.Equal(t, store.CommandRunStatusSucceeded, fin.Status)
	assert.Empty(t, fin.Reason)
	require.NotNil(t, fin.ExitCode)
	assert.Equal(t, 0, *fin.ExitCode)
	require.NotNil(t, fin.DurationMS)
	assert.GreaterOrEqual(t, *fin.DurationMS, int64(0))
	assert.Equal(t, agentcontrol.ExecOutputCaptured, fin.OutputStatus)
	require.NotNil(t, fin.OutputBytes)
	assert.Greater(t, *fin.OutputBytes, int64(0))

	// Exactly one exec-start with bash --login -c <script> scion-command and workdir "~".
	require.Equal(t, 1, f.capture.count(execRoutePath(agentcontrol.RouteExecStart)))
	var sent agentcontrol.ExecStartRequest
	require.NoError(t, json.Unmarshal(f.capture.bodies[execRoutePath(agentcontrol.RouteExecStart)][0], &sent))
	assert.Equal(t, run.ID, sent.RunID)
	assert.Equal(t, []string{"/bin/bash", "--login", "-c", secretPayload, "scion-command"}, sent.Argv)
	assert.Equal(t, "~", sent.Workdir)
	assert.Equal(t, agentcontrol.ExecCaptureTail, sent.Capture)

	// AC-S12: run_dispatched and run_succeeded emitted with allow-listed details only;
	// neither script text nor output text appears anywhere in audit events.
	events := f.audit.snapshot()
	var sawDispatched, sawSucceeded bool
	for _, ev := range events {
		assert.Equal(t, msgaction.KindCommandRequest, ev.Kind)
		for k, v := range ev.Details {
			assert.NotContains(t, v, "SUPER_SECRET_SCRIPT_TOKEN_XYZ", "audit detail %s leaked script/output", k)
		}
		if ev.EventType == msgaction.EventCmdRunDispatched {
			sawDispatched = true
			assert.Equal(t, req.CommandSHA256, ev.Details["command_sha256"])
			assert.Equal(t, f.launched.ID, ev.Details["exec_agent_id"])
			assert.Equal(t, run.ID, ev.Details["run_id"])
			assert.Equal(t, "home", ev.Details["workdir_kind"])
			assert.NotEmpty(t, ev.Details["exec_argv_sha256"])
		}
		if ev.EventType == msgaction.EventCmdRunSucceeded {
			sawSucceeded = true
			assert.Equal(t, "succeeded", ev.Details["outcome"])
			assert.Equal(t, "0", ev.Details["exit_code"])
			assert.Equal(t, agentcontrol.ExecOutputCaptured, ev.Details["output_status"])
		}
	}
	assert.True(t, sawDispatched, "expected run_dispatched audit event")
	assert.True(t, sawSucceeded, "expected run_succeeded audit event")

	// Outcome-only command-response message persisted and recorded on run.
	require.Eventually(t, func() bool {
		r, err := f.store.GetCommandRun(context.Background(), run.ID)
		return err == nil && r != nil && r.OutcomeMessageID != "" && r.OutcomeDeliveredAt != nil
	}, 5*time.Second, 20*time.Millisecond)
	reloaded, err := f.store.GetCommandRun(context.Background(), run.ID)
	require.NoError(t, err)
	msg, err := f.store.GetMessage(context.Background(), reloaded.OutcomeMessageID)
	require.NoError(t, err)
	assert.Equal(t, messages.TypeCommandResponse, msg.Type)
	assert.Contains(t, msg.Msg, "succeeded")
	assert.NotContains(t, msg.Msg, "SUPER_SECRET_SCRIPT_TOKEN_XYZ", "P1 outcome-only response must not include output tail")
}

func TestCommandRunExec_OutcomeMapping(t *testing.T) {
	t.Run("nonzero_exit_maps_to_failed", func(t *testing.T) {
		f := newCmdRunExecFixture(t, nil)
		req, run := f.createApprovedRun(t, "nonzero", "exit 7", "", 30)
		f.srv.EnqueueCommandRun(req, run)
		fin := f.waitRunFinished(t, run.ID)
		assert.Equal(t, store.CommandRunStatusFailed, fin.Status)
		assert.Equal(t, "exit status 7", fin.Reason)
		require.NotNil(t, fin.ExitCode)
		assert.Equal(t, 7, *fin.ExitCode)
	})

	t.Run("missing_explicit_workdir_maps_to_not_started_start_failed", func(t *testing.T) {
		f := newCmdRunExecFixture(t, nil)
		req, run := f.createApprovedRun(t, "bad-wd", "pwd", "/nonexistent/scion-cmd-workdir-xyz", 30)
		f.srv.EnqueueCommandRun(req, run)
		fin := f.waitRunFinished(t, run.ID)
		assert.Equal(t, store.CommandRunStatusNotStarted, fin.Status)
		assert.Equal(t, agentcontrol.ExecOutcomeStartFailed, fin.Reason)
	})

	t.Run("timeout_maps_to_unknown", func(t *testing.T) {
		cap := &execCapture{
			answer: func(req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
				if req.GetPath() == execRoutePath(agentcontrol.RouteExecStart) {
					return &conduitv1.RpcResponse{Status: 202, Body: []byte(`{"state":"running"}`)}
				}
				if req.GetPath() == execRoutePath(agentcontrol.RouteExecStatus) {
					return &conduitv1.RpcResponse{
						Status: 200,
						Body:   []byte(`{"state":"finished","outcome":"timeout","exit_code":-1,"duration_ms":1000,"output_bytes":4,"output_status":"captured","output":"part"}`),
					}
				}
				return nil
			},
		}
		f := newCmdRunExecFixture(t, cap)
		req, run := f.createApprovedRun(t, "timeout-case", "sleep 60", "", 5)
		f.srv.EnqueueCommandRun(req, run)
		fin := f.waitRunFinished(t, run.ID)
		assert.Equal(t, store.CommandRunStatusUnknown, fin.Status)
		assert.Equal(t, "timeout", fin.Reason)
		assert.Equal(t, agentcontrol.ExecOutputCaptured, fin.OutputStatus)
	})

	t.Run("busy_refusal_maps_to_not_started_busy", func(t *testing.T) {
		cap := &execCapture{
			answer: func(req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
				if req.GetPath() == execRoutePath(agentcontrol.RouteExecStart) {
					return &conduitv1.RpcResponse{Status: 409, Body: []byte(`{"outcome":"busy"}`)}
				}
				return nil
			},
		}
		f := newCmdRunExecFixture(t, cap)
		req, run := f.createApprovedRun(t, "busy-case", "echo hi", "", 30)
		f.srv.EnqueueCommandRun(req, run)
		fin := f.waitRunFinished(t, run.ID)
		assert.Equal(t, store.CommandRunStatusNotStarted, fin.Status)
		assert.Equal(t, agentcontrol.ExecOutcomeBusy, fin.Reason)
	})

	t.Run("conflict_mismatch_maps_to_unknown", func(t *testing.T) {
		cap := &execCapture{
			answer: func(req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
				if req.GetPath() == execRoutePath(agentcontrol.RouteExecStart) {
					return &conduitv1.RpcResponse{Status: 409, Body: []byte(`{"outcome":"mismatch"}`)}
				}
				return nil
			},
		}
		f := newCmdRunExecFixture(t, cap)
		req, run := f.createApprovedRun(t, "mismatch-case", "echo hi", "", 30)
		f.srv.EnqueueCommandRun(req, run)
		fin := f.waitRunFinished(t, run.ID)
		assert.Equal(t, store.CommandRunStatusUnknown, fin.Status)
		assert.Equal(t, agentcontrol.ExecOutcomeMismatch, fin.Reason)
		assert.Equal(t, agentcontrol.ExecOutputLost, fin.OutputStatus)
	})

	t.Run("stopped_container_not_started_d5", func(t *testing.T) {
		f := newCmdRunExecFixture(t, nil)
		ctx := context.Background()
		ag, err := f.store.GetAgent(ctx, f.launched.ID)
		require.NoError(t, err)
		ag.Phase = string(state.PhaseStopped)
		require.NoError(t, f.store.UpdateAgent(ctx, ag))

		req, run := f.createApprovedRun(t, "phase-stopped", "echo nope", "", 30)
		f.srv.EnqueueCommandRun(req, run)
		fin := f.waitRunFinished(t, run.ID)
		assert.Equal(t, store.CommandRunStatusNotStarted, fin.Status)
		assert.Equal(t, "container_stopped", fin.Reason)
		assert.Zero(t, f.capture.count(execRoutePath(agentcontrol.RouteExecStart)))
	})

	t.Run("hub_busy_when_tracker_slots_full", func(t *testing.T) {
		scheduledExecSeamMu.Lock()
		orig := commandRunMaxTracked
		commandRunMaxTracked = 0
		scheduledExecSeamMu.Unlock()
		t.Cleanup(func() {
			scheduledExecSeamMu.Lock()
			commandRunMaxTracked = orig
			scheduledExecSeamMu.Unlock()
		})

		f := newCmdRunExecFixture(t, nil)
		req, run := f.createApprovedRun(t, "hub-busy", "echo nope", "", 30)
		f.srv.EnqueueCommandRun(req, run)
		fin := f.waitRunFinished(t, run.ID)
		assert.Equal(t, store.CommandRunStatusNotStarted, fin.Status)
		assert.Equal(t, "hub_busy", fin.Reason)
		assert.Zero(t, f.capture.count(execRoutePath(agentcontrol.RouteExecStart)))
	})
}

func TestCommandRunExec_AC_S4_AmbiguousStartPollsOnceNeverResends(t *testing.T) {
	var startCalls atomic.Int32
	cap := &execCapture{
		answer: func(req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
			if req.GetPath() == execRoutePath(agentcontrol.RouteExecStart) {
				startCalls.Add(1)
				// Return a 500 (outside schema / ambiguous) while we pretend the
				// run actually started and finished on the agent.
				return &conduitv1.RpcResponse{Status: 500, Body: []byte(`{}`)}
			}
			if req.GetPath() == execRoutePath(agentcontrol.RouteExecStatus) {
				return &conduitv1.RpcResponse{
					Status: 200,
					Body:   []byte(`{"state":"finished","outcome":"ok","exit_code":0,"duration_ms":42,"output_bytes":3,"output_status":"captured","output":"ok\n"}`),
				}
			}
			return nil
		},
	}
	f := newCmdRunExecFixture(t, cap)
	req, run := f.createApprovedRun(t, "ambiguous-start", "echo ok", "", 30)
	f.srv.EnqueueCommandRun(req, run)
	fin := f.waitRunFinished(t, run.ID)

	assert.Equal(t, store.CommandRunStatusSucceeded, fin.Status)
	assert.Equal(t, int32(1), startCalls.Load(), "ambiguous exec-start must never be resent (AC-S4)")
	assert.Equal(t, 1, f.capture.count(execRoutePath(agentcontrol.RouteExecStart)))
	assert.GreaterOrEqual(t, f.capture.count(execRoutePath(agentcontrol.RouteExecStatus)), 1)
}

func TestCommandRunExec_AC_S4_DoubleApproveAndRestartRecovery(t *testing.T) {
	t.Run("double_approve_produces_at_most_one_exec_start", func(t *testing.T) {
		f := newCmdRunExecFixture(t, nil)
		req, run := f.createApprovedRun(t, "double-app", "echo once", "", 30)

		// Second approve with a different idempotency key fails CAS (already_resolved).
		res2, err := f.store.ApproveCommandRequest(context.Background(), &store.ApproveCommandRequestInput{
			RequestID:      req.ID,
			CommandSHA256:  req.CommandSHA256,
			UserID:         f.owner.ID(),
			UserName:       f.owner.DisplayName(),
			UserEmail:      f.owner.Email(),
			Surface:        msgaction.CommandRequestSurfaceCLI,
			IdempotencyKey: "second-key",
			ExecAgentID:    f.launched.ID,
			ExecProjectID:  f.launched.ProjectID,
			LeaseUntil:     time.Now().UTC().Add(5 * time.Minute),
			Now:            time.Now().UTC(),
		})
		require.NoError(t, err)
		assert.Nil(t, res2.Run)
		assert.Equal(t, msgaction.ConflictAlreadyResolved, res2.ConflictCode)

		// Even if EnqueueCommandRun is called twice concurrently for the same run,
		// tracker ownership + dispatching CAS guarantee exactly one exec-start.
		f.srv.EnqueueCommandRun(req, run)
		f.srv.EnqueueCommandRun(req, run)
		fin := f.waitRunFinished(t, run.ID)
		assert.Equal(t, store.CommandRunStatusSucceeded, fin.Status)
		assert.Equal(t, 1, f.capture.count(execRoutePath(agentcontrol.RouteExecStart)))
	})

	t.Run("hub_restart_before_dispatch_sweeper_marks_not_started_never_sends", func(t *testing.T) {
		f := newCmdRunExecFixture(t, nil)
		// Create a pending request and approve it with a short lease so the
		// interrupted run itself expires and is resolved by sweepCommandRuns (N3).
		now := time.Now().UTC()
		norm, err := messages.NormalizeCommandRequest("command_request", &messages.CommandRequestInput{
			Key: "restart-before-dispatch", Script: "echo never", TimeoutS: 30,
		})
		require.NoError(t, err)
		pr, err := f.store.CreateCommandRequestPrompt(context.Background(), &store.CommandRequestPromptInput{
			Message: &store.Message{
				ID: api.NewUUID(), ProjectID: f.launched.ProjectID, ConversationID: api.NewUUID(),
				ThreadID: "dm:user:" + f.owner.ID() + ":agent:" + f.asker.ID,
				Sender:   "agent:" + f.asker.Slug, SenderID: f.asker.ID,
				Recipient: "user:" + f.owner.Email(), RecipientID: f.owner.ID(),
			},
			AgentID: f.asker.ID, AgentName: f.asker.Slug, ApproverID: f.owner.ID(),
			Request: norm, Now: now, TTL: msgaction.CommandRequestTTL,
		})
		require.NoError(t, err)
		app, err := f.store.ApproveCommandRequest(context.Background(), &store.ApproveCommandRequestInput{
			RequestID: pr.Request.ID, CommandSHA256: pr.Request.CommandSHA256,
			UserID: f.owner.ID(), UserName: f.owner.DisplayName(), UserEmail: f.owner.Email(),
			Surface: msgaction.CommandRequestSurfaceCLI, IdempotencyKey: "idem-restart-before",
			ExecAgentID: f.launched.ID, ExecProjectID: f.launched.ProjectID,
			LeaseUntil: now.Add(200 * time.Millisecond), Now: now,
		})
		require.NoError(t, err)
		req, run := app.Request, app.Run

		// Simulate hub shutdown right before committing dispatching.
		commandRunExecHook = func(stage, runID string) {
			if stage == commandRunStageBeforeDispatch && runID == run.ID {
				f.srv.commandRuns.work.closeAdmission()
				go f.srv.stopCommandRuns(context.Background())
				time.Sleep(50 * time.Millisecond)
			}
		}
		t.Cleanup(func() { commandRunExecHook = nil })

		scheduledExecSeamMu.Lock()
		origGrace := commandRunStopGrace
		commandRunStopGrace = 10 * time.Millisecond
		scheduledExecSeamMu.Unlock()
		t.Cleanup(func() {
			scheduledExecSeamMu.Lock()
			commandRunStopGrace = origGrace
			scheduledExecSeamMu.Unlock()
		})

		f.srv.EnqueueCommandRun(req, run)
		require.Eventually(t, func() bool { return !f.srv.commandRuns.owns(run.ID) }, 5*time.Second, 20*time.Millisecond)
		assert.Zero(t, f.capture.count(execRoutePath(agentcontrol.RouteExecStart)))

		got, err := f.store.GetCommandRun(context.Background(), run.ID)
		require.NoError(t, err)
		assert.Equal(t, store.CommandRunStatusQueued, got.Status)
		assert.Nil(t, got.FinishedAt)

		// Wait past the interrupted run's own lease and sweep it.
		time.Sleep(250 * time.Millisecond)
		f.srv.sweepCommandRuns(context.Background())
		swept, err := f.store.GetCommandRun(context.Background(), run.ID)
		require.NoError(t, err)
		assert.Equal(t, store.CommandRunStatusNotStarted, swept.Status)
		assert.Equal(t, "lease_expired", swept.Reason)
		assert.Zero(t, f.capture.count(execRoutePath(agentcontrol.RouteExecStart)), "sweeper must never send exec-start")
	})

	t.Run("hub_restart_after_dispatching_before_send_resolves_dispatch_failed_only_after_grace", func(t *testing.T) {
		f := newCmdRunExecFixture(t, nil)
		scheduledExecSeamMu.Lock()
		origNotAfter := commandRunNotAfter
		origAbsentGrace := scheduledExecStartAbsentGrace
		origLeaseGrace := commandRunLeaseGrace
		origStopGrace := commandRunStopGrace
		commandRunNotAfter = 120 * time.Millisecond
		scheduledExecStartAbsentGrace = 120 * time.Millisecond
		commandRunLeaseGrace = 400 * time.Millisecond
		commandRunStopGrace = 10 * time.Millisecond
		scheduledExecSeamMu.Unlock()
		t.Cleanup(func() {
			scheduledExecSeamMu.Lock()
			commandRunNotAfter = origNotAfter
			scheduledExecStartAbsentGrace = origAbsentGrace
			commandRunLeaseGrace = origLeaseGrace
			commandRunStopGrace = origStopGrace
			scheduledExecSeamMu.Unlock()
		})

		req, run := f.createApprovedRun(t, "restart-after-dispatch", "echo never-sent", "", 1)

		// Interrupt right after dispatching is committed and BEFORE exec-start is sent.
		commandRunExecHook = func(stage, runID string) {
			if stage == commandRunStageDispatched && runID == run.ID {
				f.srv.commandRuns.work.closeAdmission()
				go f.srv.stopCommandRuns(context.Background())
				time.Sleep(40 * time.Millisecond)
			}
		}
		t.Cleanup(func() { commandRunExecHook = nil })

		f.srv.EnqueueCommandRun(req, run)
		require.Eventually(t, func() bool { return !f.srv.commandRuns.owns(run.ID) }, 5*time.Second, 20*time.Millisecond)
		assert.Zero(t, f.capture.count(execRoutePath(agentcontrol.RouteExecStart)), "stopped before exec-start")

		// Row is dispatching and unfinished.
		mid, err := f.store.GetCommandRun(context.Background(), run.ID)
		require.NoError(t, err)
		assert.Equal(t, store.CommandRunStatusDispatching, mid.Status)
		assert.Nil(t, mid.FinishedAt)

		// Polling 404 BEFORE not_after + grace must NOT finish as dispatch_failed yet.
		doneEarly := f.srv.pollCommandRunOnce(context.Background(), req, mid, false)
		assert.False(t, doneEarly, "404 before not_after+grace must not finish prematurely")
		stillOpen, err := f.store.GetCommandRun(context.Background(), run.ID)
		require.NoError(t, err)
		assert.Nil(t, stillOpen.FinishedAt)

		// Once not_after + grace passes, 404 while still dispatching finishes as
		// not_started/dispatch_failed with zero exec-start calls.
		time.Sleep(300 * time.Millisecond)
		doneLate := f.srv.pollCommandRunOnce(context.Background(), req, stillOpen, false)
		assert.True(t, doneLate)
		fin, err := f.store.GetCommandRun(context.Background(), run.ID)
		require.NoError(t, err)
		assert.Equal(t, store.CommandRunStatusNotStarted, fin.Status)
		assert.Equal(t, "dispatch_failed", fin.Reason)
		assert.Zero(t, f.capture.count(execRoutePath(agentcontrol.RouteExecStart)))
	})

	t.Run("sweeper_expires_pending_command_requests", func(t *testing.T) {
		f := newCmdRunExecFixture(t, nil)
		now := time.Now().UTC()
		norm, err := messages.NormalizeCommandRequest("command_request", &messages.CommandRequestInput{
			Key: "stale-pending", Script: "echo stale", TimeoutS: 30,
		})
		require.NoError(t, err)
		pr, err := f.store.CreateCommandRequestPrompt(context.Background(), &store.CommandRequestPromptInput{
			Message: &store.Message{
				ID: api.NewUUID(), ProjectID: f.launched.ProjectID, ConversationID: api.NewUUID(),
				ThreadID: "dm:user:" + f.owner.ID() + ":agent:" + f.asker.ID,
				Sender:   "agent:" + f.asker.Slug, SenderID: f.asker.ID,
				Recipient: "user:" + f.owner.Email(), RecipientID: f.owner.ID(),
			},
			AgentID: f.asker.ID, AgentName: f.asker.Slug, ApproverID: f.owner.ID(),
			Request: norm, Now: now.Add(-25 * time.Hour), TTL: msgaction.CommandRequestTTL,
		})
		require.NoError(t, err)

		f.srv.sweepCommandRuns(context.Background())
		got, err := f.store.GetCommandRequest(context.Background(), pr.Request.ID)
		require.NoError(t, err)
		assert.Equal(t, store.CommandRequestStatusExpired, got.Status)

		var sawExpired bool
		for _, ev := range f.audit.snapshot() {
			if ev.EventType == MessageActionEventExpired && ev.RowID == pr.Request.ID {
				sawExpired = true
			}
		}
		assert.True(t, sawExpired, "sweeper must emit expired audit event for expired command request")
	})

	t.Run("sweeper_polls_expired_dispatching_run_once_then_unknown", func(t *testing.T) {
		cap := &execCapture{
			answer: func(req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
				if req.GetPath() == execRoutePath(agentcontrol.RouteExecStatus) {
					return &conduitv1.RpcResponse{Status: 200, Body: []byte(`{"state":"running"}`)}
				}
				return nil
			},
		}
		f := newCmdRunExecFixture(t, cap)
		_, run := f.createApprovedRun(t, "expired-running", "sleep 999", "", 30)
		now := time.Now().UTC()
		won, err := f.store.TransitionCommandRunToDispatching(context.Background(), &store.CommandRunDispatchInput{
			RunID:        run.ID,
			LaunchID:     f.launched.RunID,
			NotAfter:     now.Add(-5 * time.Minute),
			LeaseUntil:   now.Add(-time.Minute),
			DispatchedAt: now.Add(-6 * time.Minute),
		})
		require.NoError(t, err)
		require.True(t, won)

		f.srv.sweepCommandRuns(context.Background())
		swept, err := f.store.GetCommandRun(context.Background(), run.ID)
		require.NoError(t, err)
		assert.Equal(t, store.CommandRunStatusUnknown, swept.Status)
		assert.Equal(t, "lease_expired", swept.Reason)
		assert.Equal(t, agentcontrol.ExecOutputLost, swept.OutputStatus)
		assert.Equal(t, 1, f.capture.count(execRoutePath(agentcontrol.RouteExecStatus)), "sweeper polls once")
		assert.Zero(t, f.capture.count(execRoutePath(agentcontrol.RouteExecStart)), "sweeper never sends exec-start")
	})
}

func TestCommandRunExec_FinishCASRace(t *testing.T) {
	f := newCmdRunExecFixture(t, nil)
	req, run := f.createApprovedRun(t, "cas-race", "echo race", "", 30)

	const workers = 4
	var wins atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			runCopy := *run
			won, err := f.srv.finishCommandRun(context.Background(), req, &runCopy, []string{store.CommandRunStatusQueued}, store.CommandRunStatusSucceeded, "", &agentcontrol.ExecResult{
				State:        agentcontrol.ExecStateFinished,
				Outcome:      agentcontrol.ExecOutcomeOK,
				ExitCode:     0,
				DurationMS:   12,
				OutputBytes:  5,
				OutputStatus: agentcontrol.ExecOutputCaptured,
				Output:       []byte("race\n"),
			}, "")
			if err == nil && won {
				wins.Add(1)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	assert.Equal(t, int32(1), wins.Load(), "exactly one finish CAS must win")
	var finishedAudits int
	for _, ev := range f.audit.snapshot() {
		if strings.HasPrefix(ev.EventType, "run_") {
			finishedAudits++
		}
	}
	assert.Equal(t, 1, finishedAudits, "only the CAS winner may emit the finish audit event")
}

// TestCommandRunExec_ApproveHandlerDefaultLauncherEndToEnd defeats C1: on a
// default Server (no SetCommandRunLauncher), hitting the real
// POST /api/v1/command-requests/{id}/approve handler must launch the executor,
// send exactly one exec-start, and finish as succeeded.
func TestCommandRunExec_ApproveHandlerDefaultLauncherEndToEnd(t *testing.T) {
	f := newCmdRunExecFixture(t, nil)
	ctx := context.Background()
	now := time.Now().UTC()

	norm, err := messages.NormalizeCommandRequest("command_request", &messages.CommandRequestInput{
		Key:      "default-launcher-e2e",
		Script:   "echo default-launcher-ok",
		TimeoutS: 30,
	})
	require.NoError(t, err)
	pr, err := f.store.CreateCommandRequestPrompt(ctx, &store.CommandRequestPromptInput{
		Message: &store.Message{
			ID:             api.NewUUID(),
			ProjectID:      f.launched.ProjectID,
			ConversationID: api.NewUUID(),
			ThreadID:       "dm:user:" + f.owner.ID() + ":agent:" + f.asker.ID,
			Sender:         "agent:" + f.asker.Slug,
			SenderID:       f.asker.ID,
			Recipient:      "user:" + f.owner.Email(),
			RecipientID:    f.owner.ID(),
		},
		AgentID:    f.asker.ID,
		AgentName:  f.asker.Slug,
		ApproverID: f.owner.ID(),
		Request:    norm,
		Now:        now,
		TTL:        msgaction.CommandRequestTTL,
	})
	require.NoError(t, err)

	body, err := json.Marshal(messages.ApproveCommandRequestBody{
		RowID:          pr.Request.ID,
		CommandSHA256:  pr.Request.CommandSHA256,
		Surface:        msgaction.CommandRequestSurfaceCLI,
		IdempotencyKey: "c1-e2e-key",
	})
	require.NoError(t, err)

	// Confirm no test override was installed on the server.
	assert.Nil(t, f.srv.commandRunLauncher)

	httpReq := httptest.NewRequest(http.MethodPost, "/api/v1/command-requests/"+pr.Request.ID+"/approve", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq = httpReq.WithContext(contextWithIdentity(httpReq.Context(), f.owner))
	rec := httptest.NewRecorder()
	f.srv.handleCommandRequestRoutes(rec, httpReq)

	require.Equal(t, http.StatusAccepted, rec.Code, "body: %s", rec.Body.String())
	var appResp messages.ApproveCommandRequestResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &appResp))
	require.NotEmpty(t, appResp.Run.RunID)

	fin := f.waitRunFinished(t, appResp.Run.RunID)
	assert.Equal(t, store.CommandRunStatusSucceeded, fin.Status)
	require.NotNil(t, fin.ExitCode)
	assert.Equal(t, 0, *fin.ExitCode)
	assert.Equal(t, 1, f.capture.count(execRoutePath(agentcontrol.RouteExecStart)), "real approve handler must trigger exactly one exec-start")
}

// TestCommandRequestOutput_AuthzAndSharing verifies P4 end-to-end through the
// real approve handler, real Conduit executor, real agent list handler, and
// real GET /api/v1/command-requests/{id}/output handler (AC F8, S9, S10, S1).
func TestCommandRequestOutput_AuthzAndSharing(t *testing.T) {
	f := newCmdRunExecFixture(t, nil)
	ctx := context.Background()

	// Capture slog + messageLog + SSE to prove output never leaks (AC-S9).
	var logBuf bytes.Buffer
	var logMu sync.Mutex
	testLogger := slog.New(slog.NewJSONHandler(&captureLogWriter{w: &logBuf, mu: &logMu}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	prevLogger := slog.Default()
	slog.SetDefault(testLogger)
	t.Cleanup(func() { slog.SetDefault(prevLogger) })
	f.srv.messageLog = testLogger

	var sseMu sync.Mutex
	var sseEvents []string
	if pub, ok := f.srv.events.(*ChannelEventPublisher); ok {
		ch, unsub := pub.Subscribe("project." + f.launched.ProjectID + ".>")
		t.Cleanup(unsub)
		go func() {
			for ev := range ch {
				sseMu.Lock()
				sseEvents = append(sseEvents, string(ev.Data))
				sseMu.Unlock()
			}
		}()
	}

	// Create a second human (bob) who is NOT the approver.
	bob := &store.User{
		ID:          api.NewUUID(),
		Email:       "bob-output@example.com",
		DisplayName: "Bob Output",
		Role:        store.UserRoleMember,
		Status:      "active",
	}
	require.NoError(t, f.store.CreateUser(ctx, bob))

	createPendingReq := func(key, script string, shareRequested bool) *store.CommandRequest {
		t.Helper()
		norm, err := messages.NormalizeCommandRequest("command_request", &messages.CommandRequestInput{
			Key:                  key,
			Script:               script,
			TimeoutS:             30,
			ShareOutputRequested: shareRequested,
		})
		require.NoError(t, err)
		pr, err := f.store.CreateCommandRequestPrompt(ctx, &store.CommandRequestPromptInput{
			Message: &store.Message{
				ID:             api.NewUUID(),
				ProjectID:      f.launched.ProjectID,
				ConversationID: api.NewUUID(),
				ThreadID:       "dm:user:" + f.owner.ID() + ":agent:" + f.asker.ID,
				Sender:         "agent:" + f.asker.Slug,
				SenderID:       f.asker.ID,
				Recipient:      "user:" + f.owner.Email(),
				RecipientID:    f.owner.ID(),
			},
			AgentID:    f.asker.ID,
			AgentName:  f.asker.Slug,
			ApproverID: f.owner.ID(),
			Request:    norm,
			Now:        time.Now().UTC(),
			TTL:        msgaction.CommandRequestTTL,
		})
		require.NoError(t, err)
		return pr.Request
	}

	approveViaHandler := func(req *store.CommandRequest, shareOverride *bool) messages.ApproveCommandRequestResponse {
		t.Helper()
		body, err := json.Marshal(messages.ApproveCommandRequestBody{
			RowID:          req.ID,
			CommandSHA256:  req.CommandSHA256,
			ShareOutput:    shareOverride,
			Surface:        msgaction.CommandRequestSurfaceCLI,
			IdempotencyKey: api.NewUUID(),
		})
		require.NoError(t, err)
		httpReq := httptest.NewRequest(http.MethodPost, "/api/v1/command-requests/"+req.ID+"/approve", bytes.NewReader(body))
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq = httpReq.WithContext(contextWithCredentialContext(
			contextWithIdentity(httpReq.Context(), f.owner),
			CredentialContext{Kind: CredentialKindInteractive, ID: f.owner.ID()},
		))
		rec := httptest.NewRecorder()
		f.srv.mux.ServeHTTP(rec, httpReq)
		require.Equal(t, http.StatusAccepted, rec.Code, "approve body: %s", rec.Body.String())
		var resp messages.ApproveCommandRequestResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		return resp
	}

	listAsAgent := func(key string) messages.AgentCommandRequestListResponse {
		t.Helper()
		httpReq := httptest.NewRequest(http.MethodGet, "/api/v1/agent/command-requests?key="+key, nil)
		httpReq = httpReq.WithContext(agentCtx(httpReq.Context(), f.asker))
		rec := httptest.NewRecorder()
		f.srv.mux.ServeHTTP(rec, httpReq)
		require.Equal(t, http.StatusOK, rec.Code, "list body: %s", rec.Body.String())
		var resp messages.AgentCommandRequestListResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		return resp
	}

	// 1. share_output omitted (nil) on a request where agent asked for share_output_requested=true:
	//    explicit opt-in only (M1) -> records share_output=false, agent receives outcome only,
	//    no output_tail in command-response or agent list.
	privateCanary := "PRIVATE_OUTPUT_CANARY_" + api.NewUUID()
	reqNoShare := createPendingReq("no-share", "echo "+privateCanary, true)
	appNoShare := approveViaHandler(reqNoShare, nil)
	assert.False(t, appNoShare.Run.ShareOutput, "omitted share_output must default to false even when share_output_requested=true")
	finNoShare := f.waitRunFinished(t, appNoShare.Run.RunID)
	assert.Equal(t, store.CommandRunStatusSucceeded, finNoShare.Status)
	assert.False(t, finNoShare.ShareOutput)

	// Wait briefly for outcome message persistence.
	require.Eventually(t, func() bool {
		r, err := f.store.GetCommandRun(ctx, finNoShare.ID)
		return err == nil && r.OutcomeMessageID != ""
	}, 5*time.Second, 25*time.Millisecond)
	finNoShare, err := f.store.GetCommandRun(ctx, finNoShare.ID)
	require.NoError(t, err)
	noShareMsg, err := f.store.GetMessage(ctx, finNoShare.OutcomeMessageID)
	require.NoError(t, err)
	assert.NotContains(t, noShareMsg.Msg, privateCanary, "share_output=false must not include output in command-response")

	agentListNoShare := listAsAgent("no-share")
	require.Len(t, agentListNoShare.Items, 1)
	require.NotNil(t, agentListNoShare.Items[0].Run)
	assert.Empty(t, agentListNoShare.Items[0].Run.OutputTail, "share_output=false must not include outputTail in agent list")

	// 2. share_output=true (explicit opt-in):
	//    fixture with ghp_..., PEM private key block, and > 4 KiB UTF-8 comes back redacted & bounded (AC F8).
	//    Construct the visible canary dynamically in bash so the literal string
	//    never appears in request.script (tightening card/SSE/slog leak checks).
	ghpSecret := "ghp_1234567890abcdef1234567890abcdef1234"
	pemBody := "MIIEpAIBAAKCAQEASecretPemPayload98765"
	sharedUUID := api.NewUUID()
	sharedCanary := "SHARED_VISIBLE_TAIL_" + sharedUUID
	scriptWithSecrets := strings.Join([]string{
		"printf '%0.s€' {1..1500}", // 4500 bytes of 3-byte UTF-8 before the tail
		"echo ''",
		"echo 'token=" + ghpSecret + "'",
		"echo '-----BEGIN RSA PRIVATE KEY-----'",
		"echo '" + pemBody + "'",
		"echo '-----END RSA PRIVATE KEY-----'",
		"printf 'SHARED_%s_%s\\n' 'VISIBLE_TAIL' '" + sharedUUID + "'",
	}, "\n")
	reqShare := createPendingReq("with-share", scriptWithSecrets, true)
	shareTrue := true
	appShare := approveViaHandler(reqShare, &shareTrue)
	assert.True(t, appShare.Run.ShareOutput, "explicit share_output=true must be recorded on the run")
	finShare := f.waitRunFinished(t, appShare.Run.RunID)
	assert.Equal(t, store.CommandRunStatusSucceeded, finShare.Status)
	assert.True(t, finShare.ShareOutput)

	require.Eventually(t, func() bool {
		r, err := f.store.GetCommandRun(ctx, finShare.ID)
		return err == nil && r.OutcomeMessageID != ""
	}, 5*time.Second, 25*time.Millisecond)
	finShare, err = f.store.GetCommandRun(ctx, finShare.ID)
	require.NoError(t, err)
	shareMsg, err := f.store.GetMessage(ctx, finShare.OutcomeMessageID)
	require.NoError(t, err)
	assert.Contains(t, shareMsg.Msg, sharedCanary, "shared tail must reach asker in command-response")
	assert.Contains(t, shareMsg.Msg, "[REDACTED]")
	assert.NotContains(t, shareMsg.Msg, ghpSecret, "ghp_ token must be redacted in command-response")
	assert.NotContains(t, shareMsg.Msg, pemBody, "PEM block must be redacted in command-response")

	// Agent list returns the same bounded, redacted UTF-8 tail.
	agentListShare := listAsAgent("with-share")
	require.Len(t, agentListShare.Items, 1)
	require.NotNil(t, agentListShare.Items[0].Run)
	agentTail := agentListShare.Items[0].Run.OutputTail
	assert.NotEmpty(t, agentTail)
	assert.LessOrEqual(t, len(agentTail), commandOutputShareMaxBytes)
	assert.True(t, utf8.ValidString(agentTail), "outputTail must be valid UTF-8")
	assert.Contains(t, agentTail, sharedCanary)
	assert.Contains(t, agentTail, "[REDACTED]")
	assert.NotContains(t, agentTail, ghpSecret)
	assert.NotContains(t, agentTail, pemBody)

	// Audit event output_returned emitted for shared run with output_bytes and redaction_count (never for no-share).
	var foundOutputReturned bool
	for _, ev := range f.audit.snapshot() {
		if ev.EventType == msgaction.EventCmdOutputReturned {
			assert.Equal(t, finShare.ID, ev.Details["run_id"], "output_returned must only fire for shared run")
			assert.NotEmpty(t, ev.Details["output_bytes"])
			assert.Equal(t, "2", ev.Details["redaction_count"])
			foundOutputReturned = true
		}
		for _, v := range ev.Details {
			assert.NotContains(t, v, privateCanary, "output must never appear in audit details")
			assert.NotContains(t, v, sharedCanary, "output must never appear in audit details")
			assert.NotContains(t, v, ghpSecret, "secret must never appear in audit details")
			assert.NotContains(t, v, pemBody, "PEM must never appear in audit details")
		}
	}
	assert.True(t, foundOutputReturned, "expected output_returned audit event")

	// Card GET /api/v1/command-requests/{id} never returns outputTail, and sets
	// outputAvailable=true for the approver when finished and an output row exists.
	cardReq := httptest.NewRequest(http.MethodGet, "/api/v1/command-requests/"+reqShare.ID, nil)
	cardReq = cardReq.WithContext(contextWithCredentialContext(
		contextWithIdentity(cardReq.Context(), f.owner),
		CredentialContext{Kind: CredentialKindInteractive, ID: f.owner.ID()},
	))
	cardRec := httptest.NewRecorder()
	f.srv.mux.ServeHTTP(cardRec, cardReq)
	require.Equal(t, http.StatusOK, cardRec.Code)
	var cardResp messages.CommandRequestDetailResponse
	require.NoError(t, json.Unmarshal(cardRec.Body.Bytes(), &cardResp))
	require.NotNil(t, cardResp.Run)
	assert.Empty(t, cardResp.Run.OutputTail, "card GET must never populate run.outputTail")
	assert.True(t, cardResp.OutputAvailable, "card GET must set outputAvailable=true for approver when output row exists")
	assert.NotContains(t, cardRec.Body.String(), sharedCanary, "card GET must never include output")

	// Pending request (no run yet) must have outputAvailable=false for approver.
	pendingReq := createPendingReq("pending-no-output", "echo pending", false)
	pendCardReq := httptest.NewRequest(http.MethodGet, "/api/v1/command-requests/"+pendingReq.ID, nil)
	pendCardReq = pendCardReq.WithContext(contextWithCredentialContext(
		contextWithIdentity(pendCardReq.Context(), f.owner),
		CredentialContext{Kind: CredentialKindInteractive, ID: f.owner.ID()},
	))
	pendCardRec := httptest.NewRecorder()
	f.srv.mux.ServeHTTP(pendCardRec, pendCardReq)
	require.Equal(t, http.StatusOK, pendCardRec.Code)
	var pendCardResp messages.CommandRequestDetailResponse
	require.NoError(t, json.Unmarshal(pendCardRec.Body.Bytes(), &pendCardResp))
	assert.False(t, pendCardResp.OutputAvailable, "pending request must report outputAvailable=false")

	// Non-approver conversation participant must see outputAvailable=false on a finished run.
	require.NoError(t, f.store.AddConversationParticipant(ctx, reqShare.ConversationID, "user", bob.ID, "member"))
	grantProjectAccessOnly(t, f.store, bob.ID, f.launched.ProjectID)
	bobCardReq := httptest.NewRequest(http.MethodGet, "/api/v1/command-requests/"+reqShare.ID, nil)
	bobCardReq = bobCardReq.WithContext(contextWithCredentialContext(
		contextWithIdentity(bobCardReq.Context(), NewAuthenticatedUser(bob.ID, bob.Email, bob.DisplayName, bob.Role, "web")),
		CredentialContext{Kind: CredentialKindInteractive, ID: bob.ID},
	))
	bobCardRec := httptest.NewRecorder()
	f.srv.mux.ServeHTTP(bobCardRec, bobCardReq)
	if bobCardRec.Code == http.StatusOK {
		var bobCardResp messages.CommandRequestDetailResponse
		require.NoError(t, json.Unmarshal(bobCardRec.Body.Bytes(), &bobCardResp))
		assert.False(t, bobCardResp.OutputAvailable, "non-approver must see outputAvailable=false")
	}

	// 3. Approver output view: GET /api/v1/command-requests/{id}/output (AC-S10).
	outURL := "/api/v1/command-requests/" + reqNoShare.ID + "/output"

	// 3a. Approver with interactive session + live agent.attach -> 200 + Cache-Control: no-store.
	getOutReq := httptest.NewRequest(http.MethodGet, outURL, nil)
	getOutReq = getOutReq.WithContext(contextWithCredentialContext(
		contextWithIdentity(getOutReq.Context(), f.owner),
		CredentialContext{Kind: CredentialKindInteractive, ID: f.owner.ID()},
	))
	getOutRec := httptest.NewRecorder()
	f.srv.mux.ServeHTTP(getOutRec, getOutReq)
	require.Equal(t, http.StatusOK, getOutRec.Code, "approver output body: %s", getOutRec.Body.String())
	assert.Equal(t, "no-store", getOutRec.Header().Get("Cache-Control"))
	var outResp messages.CommandRequestOutputResponse
	require.NoError(t, json.Unmarshal(getOutRec.Body.Bytes(), &outResp))
	assert.Equal(t, finNoShare.ID, outResp.RunID)
	assert.Contains(t, outResp.Output, privateCanary)

	// output_viewed audit event emitted.
	var foundOutputViewed bool
	for _, ev := range f.audit.snapshot() {
		if ev.EventType == msgaction.EventCmdOutputViewed && ev.Details["run_id"] == finNoShare.ID {
			foundOutputViewed = true
		}
	}
	assert.True(t, foundOutputViewed, "expected output_viewed audit event")

	// 3b. Non-GET method on /output -> 405.
	postOutReq := httptest.NewRequest(http.MethodPost, outURL, nil)
	postOutReq = postOutReq.WithContext(contextWithCredentialContext(
		contextWithIdentity(postOutReq.Context(), f.owner),
		CredentialContext{Kind: CredentialKindInteractive, ID: f.owner.ID()},
	))
	postOutRec := httptest.NewRecorder()
	f.srv.mux.ServeHTTP(postOutRec, postOutReq)
	assert.Equal(t, http.StatusMethodNotAllowed, postOutRec.Code)

	// 3c. Non-approver human (bob) -> 403 not_approver.
	bobIdent := NewAuthenticatedUser(bob.ID, bob.Email, bob.DisplayName, bob.Role, "web")
	bobReq := httptest.NewRequest(http.MethodGet, outURL, nil)
	bobReq = bobReq.WithContext(contextWithCredentialContext(
		contextWithIdentity(bobReq.Context(), bobIdent),
		CredentialContext{Kind: CredentialKindInteractive, ID: bob.ID},
	))
	bobRec := httptest.NewRecorder()
	f.srv.mux.ServeHTTP(bobRec, bobReq)
	assert.Equal(t, http.StatusForbidden, bobRec.Code)
	assert.NotContains(t, bobRec.Body.String(), privateCanary)

	// 3d. Session-only credential checks through Handler() (AC-S1):
	//     real agent JWT, real PAT/UAT, visitor, federated identity -> all 403.
	agentJWT, err := f.srv.GenerateAgentToken(f.asker.ID, f.launched.ProjectID, nil, AgentRoleFull, nil)
	require.NoError(t, err)
	agentOutReq := httptest.NewRequest(http.MethodGet, outURL, nil)
	agentOutReq.Header.Set("Authorization", "Bearer "+agentJWT)
	agentOutRec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(agentOutRec, agentOutReq)
	assert.Equal(t, http.StatusForbidden, agentOutRec.Code, "agent JWT must be refused on /output")

	patPresented := "scion_pat_cmdreq-p4-output-token"
	patHash := sha256.Sum256([]byte(patPresented))
	patExpiry := time.Now().Add(time.Hour)
	require.NoError(t, f.store.CreateUserAccessToken(ctx, &store.UserAccessToken{
		ID:        api.NewUUID(),
		UserID:    f.owner.ID(),
		Name:      "cmdreq-p4-pat",
		Prefix:    "scion_pat_p401",
		KeyHash:   hex.EncodeToString(patHash[:]),
		ProjectID: f.launched.ProjectID,
		Scopes:    []string{"project:read", "agent:attach"},
		ExpiresAt: &patExpiry,
		Created:   time.Now(),
	}))
	patOutReq := httptest.NewRequest(http.MethodGet, outURL, nil)
	patOutReq.Header.Set("Authorization", "Bearer "+patPresented)
	patOutRec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(patOutRec, patOutReq)
	assert.Equal(t, http.StatusForbidden, patOutRec.Code, "PAT must be refused on /output")

	visitorIdent := NewVisitorIdentity(api.NewUUID(), "https://issuer.p4.test", "p4-visitor", "visitor@p4.test", "Visitor")
	visReq := httptest.NewRequest(http.MethodGet, outURL, nil)
	visReq = visReq.WithContext(contextWithCredentialContext(
		contextWithIdentity(visReq.Context(), visitorIdent),
		CredentialContext{Kind: CredentialKindInteractive, ID: visitorIdent.ID()},
	))
	visRec := httptest.NewRecorder()
	f.srv.mux.ServeHTTP(visRec, visReq)
	assert.Equal(t, http.StatusForbidden, visRec.Code, "visitor must be refused on /output")

	fedIdent := NewFederatedUserIdentity("https://issuer.p4.test", "p4-fed", f.owner.Email(), "Fed Owner", store.UserRoleMember, nil)
	fedReq := httptest.NewRequest(http.MethodGet, outURL, nil)
	fedReq = fedReq.WithContext(contextWithCredentialContext(
		contextWithIdentity(fedReq.Context(), fedIdent),
		CredentialContext{Kind: CredentialKindFederation, ID: fedIdent.ID()},
	))
	fedRec := httptest.NewRecorder()
	f.srv.mux.ServeHTTP(fedRec, fedReq)
	assert.Equal(t, http.StatusForbidden, fedRec.Code, "federated user must be refused on /output")

	// 3e. Approver without live agent.attach on exec_agent_id -> 403 (AC-S10).
	//     Seed a finished run where bob is the named approver (so the approver
	//     check passes), pointing at f.launched which bob cannot attach to.
	bobNorm, err := messages.NormalizeCommandRequest("command_request", &messages.CommandRequestInput{
		Key:      "bob-no-attach",
		Script:   "echo secret-for-bob",
		TimeoutS: 30,
	})
	require.NoError(t, err)
	bobPrompt, err := f.store.CreateCommandRequestPrompt(ctx, &store.CommandRequestPromptInput{
		Message: &store.Message{
			ID:             api.NewUUID(),
			ProjectID:      f.launched.ProjectID,
			ConversationID: api.NewUUID(),
			ThreadID:       "dm:user:" + bob.ID + ":agent:" + f.asker.ID,
			Sender:         "agent:" + f.asker.Slug,
			SenderID:       f.asker.ID,
			Recipient:      "user:" + bob.Email,
			RecipientID:    bob.ID,
		},
		AgentID:    f.asker.ID,
		AgentName:  f.asker.Slug,
		ApproverID: bob.ID,
		Request:    bobNorm,
		Now:        time.Now().UTC(),
		TTL:        msgaction.CommandRequestTTL,
	})
	require.NoError(t, err)
	bobApp, err := f.store.ApproveCommandRequest(ctx, &store.ApproveCommandRequestInput{
		RequestID:     bobPrompt.Request.ID,
		CommandSHA256: bobPrompt.Request.CommandSHA256,
		UserID:        bob.ID,
		Surface:       msgaction.CommandRequestSurfaceCLI,
		ExecAgentID:   f.launched.ID,
		ExecProjectID: f.launched.ProjectID,
		LeaseUntil:    time.Now().UTC().Add(5 * time.Minute),
		Now:           time.Now().UTC(),
	})
	require.NoError(t, err)
	bobExit := 0
	bobDur := int64(5)
	bobOutBytes := int64(len("revoked-attach-output"))
	bobWon, err := f.store.FinishCommandRun(ctx, &store.FinishCommandRunInput{
		RunID:        bobApp.Run.ID,
		Status:       store.CommandRunStatusSucceeded,
		ExitCode:     &bobExit,
		DurationMS:   &bobDur,
		OutputBytes:  &bobOutBytes,
		OutputStatus: agentcontrol.ExecOutputCaptured,
		Output:       []byte("revoked-attach-output"),
		FinishedAt:   time.Now().UTC(),
	})
	require.NoError(t, err)
	require.True(t, bobWon)

	noAttachReq := httptest.NewRequest(http.MethodGet, "/api/v1/command-requests/"+bobPrompt.Request.ID+"/output", nil)
	noAttachReq = noAttachReq.WithContext(contextWithCredentialContext(
		contextWithIdentity(noAttachReq.Context(), bobIdent),
		CredentialContext{Kind: CredentialKindInteractive, ID: bob.ID},
	))
	noAttachRec := httptest.NewRecorder()
	f.srv.mux.ServeHTTP(noAttachRec, noAttachReq)
	assert.Equal(t, http.StatusForbidden, noAttachRec.Code, "approver without live agent.attach must be refused")
	assert.NotContains(t, noAttachRec.Body.String(), "revoked-attach-output")

	// 3f. Soft-delete exec container -> must fail closed with 403 for real approver (AC-S10).
	ag, err := f.store.GetAgent(ctx, f.launched.ID)
	require.NoError(t, err)
	ag.DeletedAt = time.Now().UTC()
	require.NoError(t, f.store.UpdateAgent(ctx, ag))
	deletedReq := httptest.NewRequest(http.MethodGet, outURL, nil)
	deletedReq = deletedReq.WithContext(contextWithCredentialContext(
		contextWithIdentity(deletedReq.Context(), f.owner),
		CredentialContext{Kind: CredentialKindInteractive, ID: f.owner.ID()},
	))
	deletedRec := httptest.NewRecorder()
	f.srv.mux.ServeHTTP(deletedRec, deletedReq)
	assert.Equal(t, http.StatusForbidden, deletedRec.Code, "deleted execution container must fail closed on /output")
	assert.NotContains(t, deletedRec.Body.String(), privateCanary)

	// 4. Verify output canaries never reached slog/messageLog or SSE (AC-S9).
	logMu.Lock()
	logged := logBuf.String()
	logMu.Unlock()
	assert.NotContains(t, logged, privateCanary, "private output must never appear in slog/messageLog")
	assert.NotContains(t, logged, sharedCanary, "shared output must never appear in slog/messageLog")
	assert.NotContains(t, logged, ghpSecret, "ghp secret must never appear in slog/messageLog")
	assert.NotContains(t, logged, pemBody, "PEM body must never appear in slog/messageLog")

	sseMu.Lock()
	allSSE := strings.Join(sseEvents, "\n")
	sseMu.Unlock()
	assert.NotContains(t, allSSE, privateCanary, "private output must never appear in SSE")
	assert.NotContains(t, allSSE, sharedCanary, "shared output must never appear in SSE")
}

// TestCommandRunSweeper_PurgesOldActionRunOutputs verifies carry-forward I1:
// sweepCommandRuns purges action_run exec_run_outputs rows older than 7 days
// while keeping newer rows.
func TestCommandRunSweeper_PurgesOldActionRunOutputs(t *testing.T) {
	f := newCmdRunExecFixture(t, nil)
	ctx := context.Background()
	now := time.Now().UTC()

	seedFinished := func(key string, finishedAt time.Time, content string) *store.CommandRun {
		t.Helper()
		norm, err := messages.NormalizeCommandRequest("command_request", &messages.CommandRequestInput{
			Key:      key,
			Script:   "echo " + key,
			TimeoutS: 30,
		})
		require.NoError(t, err)
		pr, err := f.store.CreateCommandRequestPrompt(ctx, &store.CommandRequestPromptInput{
			Message: &store.Message{
				ID:             api.NewUUID(),
				ProjectID:      f.launched.ProjectID,
				ConversationID: api.NewUUID(),
				ThreadID:       "dm:user:" + f.owner.ID() + ":agent:" + f.asker.ID,
				Sender:         "agent:" + f.asker.Slug,
				SenderID:       f.asker.ID,
				Recipient:      "user:" + f.owner.Email(),
				RecipientID:    f.owner.ID(),
			},
			AgentID:    f.asker.ID,
			AgentName:  f.asker.Slug,
			ApproverID: f.owner.ID(),
			Request:    norm,
			Now:        finishedAt.Add(-time.Minute),
			TTL:        msgaction.CommandRequestTTL,
		})
		require.NoError(t, err)
		app, err := f.store.ApproveCommandRequest(ctx, &store.ApproveCommandRequestInput{
			RequestID:     pr.Request.ID,
			CommandSHA256: pr.Request.CommandSHA256,
			UserID:        f.owner.ID(),
			Surface:       msgaction.CommandRequestSurfaceCLI,
			ExecAgentID:   f.launched.ID,
			ExecProjectID: f.launched.ProjectID,
			LeaseUntil:    finishedAt.Add(5 * time.Minute),
			Now:           finishedAt.Add(-30 * time.Second),
		})
		require.NoError(t, err)
		code := 0
		dur := int64(5)
		outLen := int64(len(content))
		won, err := f.store.FinishCommandRun(ctx, &store.FinishCommandRunInput{
			RunID:        app.Run.ID,
			Status:       store.CommandRunStatusSucceeded,
			ExitCode:     &code,
			DurationMS:   &dur,
			OutputBytes:  &outLen,
			OutputStatus: agentcontrol.ExecOutputCaptured,
			Output:       []byte(content),
			FinishedAt:   finishedAt,
		})
		require.NoError(t, err)
		require.True(t, won)
		return app.Run
	}

	oldRun := seedFinished("sweep-old", now.Add(-8*24*time.Hour), "stale-output")
	freshRun := seedFinished("sweep-fresh", now.Add(-24*time.Hour), "fresh-output")

	f.srv.sweepCommandRuns(ctx)

	_, err := f.store.GetActionRunOutput(ctx, oldRun.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "8-day-old action_run output must be purged by sweepCommandRuns")
	gotFresh, err := f.store.GetActionRunOutput(ctx, freshRun.ID)
	require.NoError(t, err)
	assert.Equal(t, "fresh-output", string(gotFresh))
}
