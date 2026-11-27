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
	"errors"
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

	dmKey := "dm:user:" + f.owner.ID() + ":agent:" + f.asker.ID
	dmConv := &store.Conversation{
		ID:          api.NewUUID(),
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: dmKey,
	}
	require.NoError(t, f.store.CreateConversation(ctx, dmConv))

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
				ConversationID: dmConv.ID,
				ThreadID:       dmKey,
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

	// Non-approver conversation participant must get 200 with outputAvailable=false on a finished run.
	groupProjID := f.launched.ProjectID
	groupConv := &store.Conversation{
		ID:         api.NewUUID(),
		ProjectID:  &groupProjID,
		Kind:       "group",
		Surface:    "native",
		DriftState: "active",
	}
	require.NoError(t, f.store.CreateConversation(ctx, groupConv))
	for _, p := range []*store.ConversationParticipant{
		{ID: api.NewUUID(), ConversationID: groupConv.ID, PrincipalKind: "user", PrincipalID: f.owner.ID(), Role: "member", JoinedAt: time.Now().UTC()},
		{ID: api.NewUUID(), ConversationID: groupConv.ID, PrincipalKind: "user", PrincipalID: bob.ID, Role: "member", JoinedAt: time.Now().UTC()},
		{ID: api.NewUUID(), ConversationID: groupConv.ID, PrincipalKind: "agent", PrincipalID: f.asker.ID, Role: "member", JoinedAt: time.Now().UTC()},
	} {
		require.NoError(t, f.store.AddParticipant(ctx, p))
	}
	groupNorm, err := messages.NormalizeCommandRequest("command_request", &messages.CommandRequestInput{
		Key:      "group-out-avail",
		Script:   "echo group-output",
		TimeoutS: 30,
	})
	require.NoError(t, err)
	groupPr, err := f.store.CreateCommandRequestPrompt(ctx, &store.CommandRequestPromptInput{
		Message: &store.Message{
			ID:             api.NewUUID(),
			ProjectID:      f.launched.ProjectID,
			ConversationID: groupConv.ID,
			Sender:         "agent:" + f.asker.Slug,
			SenderID:       f.asker.ID,
			Recipient:      "conv:" + groupConv.ID,
			CreatedAt:      time.Now().UTC(),
		},
		AgentID:    f.asker.ID,
		AgentName:  f.asker.Slug,
		ApproverID: f.owner.ID(),
		Request:    groupNorm,
		Now:        time.Now().UTC(),
		TTL:        msgaction.CommandRequestTTL,
	})
	require.NoError(t, err)
	groupApp, err := f.store.ApproveCommandRequest(ctx, &store.ApproveCommandRequestInput{
		RequestID:     groupPr.Request.ID,
		CommandSHA256: groupPr.Request.CommandSHA256,
		UserID:        f.owner.ID(),
		Surface:       msgaction.CommandRequestSurfaceCLI,
		ExecAgentID:   f.launched.ID,
		ExecProjectID: f.launched.ProjectID,
		LeaseUntil:    time.Now().UTC().Add(5 * time.Minute),
		Now:           time.Now().UTC(),
	})
	require.NoError(t, err)
	groupExit := 0
	groupDur := int64(5)
	groupOutLen := int64(len("group-raw-output"))
	groupWon, err := f.store.FinishCommandRun(ctx, &store.FinishCommandRunInput{
		RunID:        groupApp.Run.ID,
		Status:       store.CommandRunStatusSucceeded,
		ExitCode:     &groupExit,
		DurationMS:   &groupDur,
		OutputBytes:  &groupOutLen,
		OutputStatus: agentcontrol.ExecOutputCaptured,
		Output:       []byte("group-raw-output"),
		FinishedAt:   time.Now().UTC(),
	})
	require.NoError(t, err)
	require.True(t, groupWon)

	grantProjectAccessOnly(t, f.store, bob.ID, f.launched.ProjectID)
	bobCardReq := httptest.NewRequest(http.MethodGet, "/api/v1/command-requests/"+groupPr.Request.ID, nil)
	bobCardReq = bobCardReq.WithContext(contextWithCredentialContext(
		contextWithIdentity(bobCardReq.Context(), NewAuthenticatedUser(bob.ID, bob.Email, bob.DisplayName, bob.Role, "web")),
		CredentialContext{Kind: CredentialKindInteractive, ID: bob.ID},
	))
	bobCardRec := httptest.NewRecorder()
	f.srv.mux.ServeHTTP(bobCardRec, bobCardReq)
	require.Equal(t, http.StatusOK, bobCardRec.Code, "group non-approver GET body: %s", bobCardRec.Body.String())
	var bobCardResp messages.CommandRequestDetailResponse
	require.NoError(t, json.Unmarshal(bobCardRec.Body.Bytes(), &bobCardResp))
	assert.False(t, bobCardResp.OutputAvailable, "non-approver must see outputAvailable=false even when output row exists")

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

// failingThenOKDispatcher fails the first N DispatchAgentMessage calls and
// succeeds thereafter, used to verify sweeper outcome-delivery retry (§4.9).
type failingThenOKDispatcher struct {
	AgentDispatcher
	mu             sync.Mutex
	failLeft       int
	failDelay      time.Duration
	onFail         func()
	calls          int
	msgs           []string
	structuredMsgs []*messages.StructuredMessage
}

func (d *failingThenOKDispatcher) DispatchAgentMessage(ctx context.Context, agent *store.Agent, message string, interrupt bool, structuredMsg *messages.StructuredMessage) error {
	d.mu.Lock()
	d.calls++
	if d.failLeft > 0 {
		d.failLeft--
		delay := d.failDelay
		cb := d.onFail
		d.mu.Unlock()
		if cb != nil {
			cb()
		}
		if delay > 0 {
			time.Sleep(delay)
		}
		return errors.New("transient broker failure")
	}
	d.msgs = append(d.msgs, message)
	d.structuredMsgs = append(d.structuredMsgs, structuredMsg)
	d.mu.Unlock()
	return nil
}

func TestCommandRunExec_OutcomeDeliveryRetryInSweeper(t *testing.T) {
	f := newCmdRunExecFixture(t, nil)
	ctx := context.Background()

	disp := &failingThenOKDispatcher{failLeft: 1}
	f.srv.SetDispatcher(disp)

	req, run := f.createApprovedRun(t, "retry-outcome", "echo retry", "", 30)
	// Finish directly to exercise outcome delivery + sweeper retry deterministically.
	exitZero := 0
	won, err := f.srv.finishCommandRun(ctx, req, run, []string{store.CommandRunStatusQueued}, store.CommandRunStatusSucceeded, "", &agentcontrol.ExecResult{
		State:    agentcontrol.ExecStateFinished,
		Outcome:  agentcontrol.ExecOutcomeOK,
		ExitCode: exitZero,
	}, "")
	require.NoError(t, err)
	require.True(t, won)

	// First delivery attempt failed transiently: outcome_delivered_at cleared, outcome_message_id preserved.
	afterFail, err := f.store.GetCommandRun(ctx, run.ID)
	require.NoError(t, err)
	assert.Nil(t, afterFail.OutcomeDeliveredAt, "transient dispatch failure must clear outcome_delivered_at for retry")
	assert.NotEmpty(t, afterFail.OutcomeMessageID, "persisted outcome_message_id should be retained for retry")
	assert.Equal(t, 1, disp.calls)

	// Sweeper retries and succeeds.
	f.srv.sweepCommandRuns(ctx)
	afterRetry, err := f.store.GetCommandRun(ctx, run.ID)
	require.NoError(t, err)
	assert.NotNil(t, afterRetry.OutcomeDeliveredAt, "sweeper retry should mark outcome_delivered_at")
	assert.Equal(t, afterFail.OutcomeMessageID, afterRetry.OutcomeMessageID, "retry should reuse persisted message")
	assert.Equal(t, 2, disp.calls)
	require.Len(t, disp.msgs, 1)
	assert.Contains(t, disp.msgs[0], "retry-outcome")

	// Second sweep does not redeliver.
	f.srv.sweepCommandRuns(ctx)
	assert.Equal(t, 2, disp.calls)
	// O7: outcome-delivery retry in sweeper never sends exec-start.
	assert.Equal(t, 0, f.capture.count(execRoutePath(agentcontrol.RouteExecStart)), "sweeper outcome retry must never send exec-start")

	// O3: A run whose pre-message claim aged past commandRunOutcomeClaimLease is re-listed and delivered.
	req2, run2 := f.createApprovedRun(t, "retry-stale-claim", "echo stale", "", 30)
	finAt := time.Now().UTC().Add(-5 * time.Minute)
	won2, err := f.store.FinishCommandRun(ctx, &store.FinishCommandRunInput{
		RunID:      run2.ID,
		Status:     store.CommandRunStatusSucceeded,
		FinishedAt: finAt,
	})
	require.NoError(t, err)
	require.True(t, won2)
	// Simulate crash right after claiming outcome_delivered_at (before CreateMessage/RecordOutcomeMessage).
	staleClaimAt := time.Now().UTC().Add(-4 * time.Minute)
	claimed, err := f.store.ClaimCommandRunOutcomeDelivery(ctx, run2.ID, staleClaimAt, time.Time{})
	require.NoError(t, err)
	require.True(t, claimed)

	f.srv.sweepCommandRuns(ctx)
	gotRun2, err := f.store.GetCommandRun(ctx, run2.ID)
	require.NoError(t, err)
	assert.NotEmpty(t, gotRun2.OutcomeMessageID, "sweeper must recover stale pre-message outcome claim")
	assert.Equal(t, 3, disp.calls)
	assert.Equal(t, 0, f.capture.count(execRoutePath(agentcontrol.RouteExecStart)))
	_ = req2

	// N1: A dispatch that blocks longer than commandRunOutcomeRetryBudget must not
	// strand the outcome claim; the budget stops the loop between runs, and a
	// later sweep with a healthy dispatcher delivers the outcome exactly once.
	scheduledExecSeamMu.Lock()
	origBudget := commandRunOutcomeRetryBudget
	commandRunOutcomeRetryBudget = 50 * time.Millisecond
	scheduledExecSeamMu.Unlock()
	t.Cleanup(func() {
		scheduledExecSeamMu.Lock()
		commandRunOutcomeRetryBudget = origBudget
		scheduledExecSeamMu.Unlock()
	})

	_, run3 := f.createApprovedRun(t, "retry-budget-hang-1", "echo hang1", "", 30)
	_, run4 := f.createApprovedRun(t, "retry-budget-hang-2", "echo hang2", "", 30)
	finBase := time.Now().UTC().Add(-2 * time.Minute)
	won3, err := f.store.FinishCommandRun(ctx, &store.FinishCommandRunInput{
		RunID:      run3.ID,
		Status:     store.CommandRunStatusSucceeded,
		FinishedAt: finBase,
	})
	require.NoError(t, err)
	require.True(t, won3)
	won4, err := f.store.FinishCommandRun(ctx, &store.FinishCommandRunInput{
		RunID:      run4.ID,
		Status:     store.CommandRunStatusSucceeded,
		FinishedAt: finBase.Add(time.Second),
	})
	require.NoError(t, err)
	require.True(t, won4)

	disp.mu.Lock()
	disp.failLeft = 1
	disp.failDelay = 200 * time.Millisecond
	disp.onFail = nil
	callsBeforeHang := disp.calls
	msgsBeforeHang := len(disp.msgs)
	disp.mu.Unlock()

	f.srv.sweepCommandRuns(ctx)

	afterHang3, err := f.store.GetCommandRun(ctx, run3.ID)
	require.NoError(t, err)
	assert.Nil(t, afterHang3.OutcomeDeliveredAt, "hung dispatch past retry budget must clear outcome_delivered_at rather than stranding the claim")
	assert.NotEmpty(t, afterHang3.OutcomeMessageID, "persisted outcome_message_id should be retained after hung dispatch")

	afterHang4, err := f.store.GetCommandRun(ctx, run4.ID)
	require.NoError(t, err)
	assert.Nil(t, afterHang4.OutcomeDeliveredAt, "second run should be deferred once retry budget expires between runs")
	assert.Empty(t, afterHang4.OutcomeMessageID)
	assert.Equal(t, callsBeforeHang+1, disp.calls, "budget expiry between runs must stop after the hung run")

	// Next sweep with working dispatcher and normal budget delivers both runs cleanly and reuses run3's message.
	scheduledExecSeamMu.Lock()
	commandRunOutcomeRetryBudget = origBudget
	scheduledExecSeamMu.Unlock()
	disp.mu.Lock()
	disp.failDelay = 0
	disp.mu.Unlock()
	f.srv.sweepCommandRuns(ctx)

	recovered3, err := f.store.GetCommandRun(ctx, run3.ID)
	require.NoError(t, err)
	assert.NotNil(t, recovered3.OutcomeDeliveredAt)
	assert.Equal(t, afterHang3.OutcomeMessageID, recovered3.OutcomeMessageID, "retry after hung dispatch must reuse persisted message")

	recovered4, err := f.store.GetCommandRun(ctx, run4.ID)
	require.NoError(t, err)
	assert.NotNil(t, recovered4.OutcomeDeliveredAt)
	assert.NotEmpty(t, recovered4.OutcomeMessageID)
	assert.Equal(t, msgsBeforeHang+2, len(disp.msgs))

	// Detached finalization: even if the caller's ctx is cancelled mid-dispatch,
	// ClearCommandRunOutcomeDelivery still clears the claim so the next sweep retries.
	req5, run5 := f.createApprovedRun(t, "retry-cancelled-ctx", "echo cancel", "", 30)
	won5, err := f.store.FinishCommandRun(ctx, &store.FinishCommandRunInput{
		RunID:      run5.ID,
		Status:     store.CommandRunStatusSucceeded,
		FinishedAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	require.True(t, won5)
	run5, err = f.store.GetCommandRun(ctx, run5.ID)
	require.NoError(t, err)

	callCtx, cancelCall := context.WithCancel(ctx)
	disp.mu.Lock()
	disp.failLeft = 1
	disp.failDelay = 0
	disp.onFail = cancelCall
	disp.mu.Unlock()

	f.srv.deliverCommandRunOutcome(callCtx, req5, run5, nil)
	cancelCall()

	afterCancel5, err := f.store.GetCommandRun(ctx, run5.ID)
	require.NoError(t, err)
	assert.Nil(t, afterCancel5.OutcomeDeliveredAt, "cancelled caller ctx must not prevent ClearCommandRunOutcomeDelivery")
	assert.NotEmpty(t, afterCancel5.OutcomeMessageID)

	disp.mu.Lock()
	disp.onFail = nil
	disp.mu.Unlock()
	f.srv.sweepCommandRuns(ctx)
	recovered5, err := f.store.GetCommandRun(ctx, run5.ID)
	require.NoError(t, err)
	assert.NotNil(t, recovered5.OutcomeDeliveredAt)
	assert.Equal(t, afterCancel5.OutcomeMessageID, recovered5.OutcomeMessageID)
	assert.Equal(t, msgsBeforeHang+3, len(disp.msgs))
}

type claimPollErrStore struct {
	store.Store
	err   error
	calls atomic.Int32
}

func (s *claimPollErrStore) ClaimCommandRunsForPoll(_ context.Context, _ time.Time, _ time.Duration, _ int, _ []string) ([]*store.CommandRun, error) {
	s.calls.Add(1)
	return nil, s.err
}

// TestCommandRunExec_SweeperPhaseCAndPhaseDBothRun guards the P2+P5 sweeper
// control-flow integration: phase (c) (live unowned poll claim) must wait for
// its goroutines and poll each claimed run once, and must break (not return)
// when claiming fewer than reserved, claiming none, or encountering a claim
// error so phase (d) (outcome delivery retry) still executes in the same pass.
func TestCommandRunExec_SweeperPhaseCAndPhaseDBothRun(t *testing.T) {
	t.Run("phase_c_claims_fewer_than_reserved_still_runs_phase_d_and_waits", func(t *testing.T) {
		var pollDone atomic.Int32
		cap := &execCapture{
			answer: func(req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
				if req.GetPath() == execRoutePath(agentcontrol.RouteExecStatus) {
					time.Sleep(40 * time.Millisecond)
					pollDone.Add(1)
					return &conduitv1.RpcResponse{Status: 200, Body: []byte(`{"state":"running"}`)}
				}
				return nil
			},
		}
		f := newCmdRunExecFixture(t, cap)
		ctx := context.Background()
		disp := &failingThenOKDispatcher{}
		f.srv.SetDispatcher(disp)

		// Live unowned dispatching run (1 < commandRunSweepInFlight=8 -> fewer than reserved).
		_, liveRun := f.createApprovedRun(t, "sweeper-live-c", "sleep 60", "", 60)
		now := time.Now().UTC()
		wonDisp, err := f.store.TransitionCommandRunToDispatching(ctx, &store.CommandRunDispatchInput{
			RunID:        liveRun.ID,
			LaunchID:     f.launched.RunID,
			NotAfter:     now.Add(2 * time.Minute),
			LeaseUntil:   now.Add(5 * time.Minute),
			DispatchedAt: now,
		})
		require.NoError(t, err)
		require.True(t, wonDisp)

		// Finished run pending outcome delivery (phase d).
		_, finRun := f.createApprovedRun(t, "sweeper-pending-d", "echo ok", "", 30)
		wonFin, err := f.store.FinishCommandRun(ctx, &store.FinishCommandRunInput{
			RunID:      finRun.ID,
			Status:     store.CommandRunStatusSucceeded,
			FinishedAt: now.Add(-time.Minute),
		})
		require.NoError(t, err)
		require.True(t, wonFin)

		f.srv.sweepCommandRuns(ctx)

		// Phase (c) must have waited for its goroutine and polled the live run exactly once.
		assert.Equal(t, int32(1), pollDone.Load(), "sweepCommandRuns must wait for phase (c) poll goroutines before returning")
		assert.Equal(t, 1, f.capture.count(execRoutePath(agentcontrol.RouteExecStatus)), "phase (c) must poll the claimed live run exactly once")
		assert.Zero(t, f.capture.count(execRoutePath(agentcontrol.RouteExecStart)), "sweeper must never send exec-start")

		// Phase (d) must also have run in the same sweep pass.
		gotFin, err := f.store.GetCommandRun(ctx, finRun.ID)
		require.NoError(t, err)
		assert.NotNil(t, gotFin.OutcomeDeliveredAt, "phase (d) must deliver pending outcome even when phase (c) claimed fewer than reserved")
		assert.NotEmpty(t, gotFin.OutcomeMessageID)
		assert.Equal(t, 1, disp.calls)
		require.Len(t, disp.msgs, 1)
		assert.Contains(t, disp.msgs[0], "sweeper-pending-d")
	})

	t.Run("phase_c_claims_none_still_runs_phase_d", func(t *testing.T) {
		f := newCmdRunExecFixture(t, nil)
		ctx := context.Background()
		disp := &failingThenOKDispatcher{}
		f.srv.SetDispatcher(disp)

		_, finRun := f.createApprovedRun(t, "sweeper-none-d", "echo none", "", 30)
		wonFin, err := f.store.FinishCommandRun(ctx, &store.FinishCommandRunInput{
			RunID:      finRun.ID,
			Status:     store.CommandRunStatusSucceeded,
			FinishedAt: time.Now().UTC().Add(-time.Minute),
		})
		require.NoError(t, err)
		require.True(t, wonFin)

		f.srv.sweepCommandRuns(ctx)

		gotFin, err := f.store.GetCommandRun(ctx, finRun.ID)
		require.NoError(t, err)
		assert.NotNil(t, gotFin.OutcomeDeliveredAt, "phase (d) must run when phase (c) claims zero runs")
		assert.NotEmpty(t, gotFin.OutcomeMessageID)
		assert.Equal(t, 1, disp.calls)
	})

	t.Run("phase_c_claim_error_still_runs_phase_d", func(t *testing.T) {
		f := newCmdRunExecFixture(t, nil)
		ctx := context.Background()
		disp := &failingThenOKDispatcher{}

		_, finRun := f.createApprovedRun(t, "sweeper-err-d", "echo err", "", 30)
		wonFin, err := f.store.FinishCommandRun(ctx, &store.FinishCommandRunInput{
			RunID:      finRun.ID,
			Status:     store.CommandRunStatusSucceeded,
			FinishedAt: time.Now().UTC().Add(-time.Minute),
		})
		require.NoError(t, err)
		require.True(t, wonFin)

		wrapped, _ := installStoreFault(t, f.srv, func(inner store.Store, _ *storeFaultSwitch) *claimPollErrStore {
			return &claimPollErrStore{Store: inner, err: errors.New("simulated claim poll failure")}
		})
		f.srv.SetDispatcher(disp)

		f.srv.sweepCommandRuns(ctx)

		assert.Equal(t, int32(1), wrapped.calls.Load(), "phase (c) must have attempted ClaimCommandRunsForPoll")
		gotFin, err := f.store.GetCommandRun(ctx, finRun.ID)
		require.NoError(t, err)
		assert.NotNil(t, gotFin.OutcomeDeliveredAt, "phase (d) must still run when phase (c) ClaimCommandRunsForPoll errors")
		assert.NotEmpty(t, gotFin.OutcomeMessageID)
		assert.Equal(t, 1, disp.calls)
	})
}

// TestCommandRunExec_GroupConversationOutcomePrivacyAndRetry verifies the
// group-conversation privacy rule (design §4.9 + §15) across both initial
// delivery and P5 sweeper retry, and confirms DMs remain unchanged:
//   - Group conversation with ShareOutput=true: stored conversation message has
//     status + exit code only (no reason, no output tail); asking agent still
//     receives reason + redacted output tail directly and via agent list;
//     non-approver views never carry outputTail or reason.
//   - Retry path (transient broker failure -> sweeper retry): honours the same
//     rule (reuses stored status-only message while delivering full outcome
//     with reason + redacted tail to the asking agent).
//   - DM with ShareOutput=true: unchanged (both stored message and agent
//     delivery include reason + redacted output tail).
func TestCommandRunExec_GroupConversationOutcomePrivacyAndRetry(t *testing.T) {
	f := newCmdRunExecFixture(t, nil)
	ctx := context.Background()
	now := time.Now().UTC()

	// Create a group conversation with approver (owner), another participant (bob), and asker.
	bob := &store.User{
		ID:          api.NewUUID(),
		Email:       "bob-group-privacy@conduit.test",
		DisplayName: "Bob",
		Role:        store.UserRoleMember,
		Status:      store.UserStatusActive,
	}
	require.NoError(t, f.store.CreateUser(ctx, bob))
	groupConv := &store.Conversation{
		ID:          api.NewUUID(),
		ProjectID:   &f.launched.ProjectID,
		Kind:        "group",
		Surface:     "native",
		DisplayName: "Deploy Group",
	}
	require.NoError(t, f.store.CreateConversation(ctx, groupConv))
	require.NoError(t, f.store.AddParticipant(ctx, &store.ConversationParticipant{ConversationID: groupConv.ID, PrincipalKind: "user", PrincipalID: f.owner.ID()}))
	require.NoError(t, f.store.AddParticipant(ctx, &store.ConversationParticipant{ConversationID: groupConv.ID, PrincipalKind: "user", PrincipalID: bob.ID}))
	require.NoError(t, f.store.AddParticipant(ctx, &store.ConversationParticipant{ConversationID: groupConv.ID, PrincipalKind: "agent", PrincipalID: f.asker.ID}))

	createGroupRun := func(key string) (*store.CommandRequest, *store.CommandRun) {
		norm, err := messages.NormalizeCommandRequest("command_request", &messages.CommandRequestInput{
			Key:                  key,
			Script:               "echo " + key,
			TimeoutS:             30,
			ShareOutputRequested: true,
		})
		require.NoError(t, err)
		pr, err := f.store.CreateCommandRequestPrompt(ctx, &store.CommandRequestPromptInput{
			Message: &store.Message{
				ID:             api.NewUUID(),
				ProjectID:      f.launched.ProjectID,
				ConversationID: groupConv.ID,
				Sender:         "agent:" + f.asker.Slug,
				SenderID:       f.asker.ID,
				Recipient:      "conv:" + groupConv.ID,
			},
			AgentID:    f.asker.ID,
			AgentName:  f.asker.Slug,
			ApproverID: f.owner.ID(),
			Request:    norm,
			Now:        now,
			TTL:        msgaction.CommandRequestTTL,
		})
		require.NoError(t, err)
		app, err := f.store.ApproveCommandRequest(ctx, &store.ApproveCommandRequestInput{
			RequestID:     pr.Request.ID,
			UserID:        f.owner.ID(),
			UserName:      "Owner",
			UserEmail:     f.owner.Email(),
			CommandSHA256: pr.Request.CommandSHA256,
			Surface:       msgaction.CommandRequestSurfaceCLI,
			ShareOutput:   true,
			ExecAgentID:   f.launched.ID,
			ExecProjectID: f.launched.ProjectID,
			Now:           now.Add(time.Second),
			LeaseUntil:    now.Add(5 * time.Minute),
		})
		require.NoError(t, err)
		return app.Request, app.Run
	}

	const secretOutput = "GROUP_SHARED_TAIL_LINE_42\n"
	const runReason = "exit_nonzero_detail"

	// 1. Group conversation initial delivery with ShareOutput=true.
	disp := &failingThenOKDispatcher{}
	f.srv.SetDispatcher(disp)

	grpReq, grpRun := createGroupRun("grp-share-initial")
	won, err := f.srv.finishCommandRun(ctx, grpReq, grpRun, []string{store.CommandRunStatusQueued}, store.CommandRunStatusFailed, runReason, &agentcontrol.ExecResult{
		State:        agentcontrol.ExecStateFinished,
		Outcome:      agentcontrol.ExecOutcomeNonzero,
		ExitCode:     7,
		DurationMS:   55,
		OutputBytes:  int64(len(secretOutput)),
		OutputStatus: agentcontrol.ExecOutputCaptured,
		Output:       []byte(secretOutput),
	}, "")
	require.NoError(t, err)
	require.True(t, won)

	gotGrpRun, err := f.store.GetCommandRun(ctx, grpRun.ID)
	require.NoError(t, err)
	require.NotEmpty(t, gotGrpRun.OutcomeMessageID)

	// Stored conversation message visible to other participants: status + exit code + agent pointer only.
	storedGrpMsg, err := f.store.GetMessage(ctx, gotGrpRun.OutcomeMessageID)
	require.NoError(t, err)
	assert.Contains(t, storedGrpMsg.Msg, "failed")
	assert.Contains(t, storedGrpMsg.Msg, "exit code 7")
	assert.Contains(t, storedGrpMsg.Msg, "Full outcome: `scion command request list`")
	assert.NotContains(t, storedGrpMsg.Msg, secretOutput, "group conversation stored message must NOT contain output tail (§4.9)")
	assert.NotContains(t, storedGrpMsg.Msg, runReason, "group conversation stored message must NOT contain run reason (§4.9)")

	// Asking agent received full outcome (reason + redacted output tail).
	require.Len(t, disp.msgs, 1)
	assert.Contains(t, disp.msgs[0], "failed")
	assert.Contains(t, disp.msgs[0], "exit code 7")
	assert.Contains(t, disp.msgs[0], runReason, "asking agent delivery must include run reason")
	assert.Contains(t, disp.msgs[0], secretOutput, "asking agent delivery must include shared output tail")

	// Non-approver view strips outputTail, outputStatus, and reason; agent view carries them.
	require.NotEmpty(t, gotGrpRun.OutputStatus, "precondition: run has OutputStatus populated")
	sseGrpView := commandRequestSSEView(grpReq, nil, gotGrpRun)
	require.NotNil(t, sseGrpView.Run)
	assert.Empty(t, sseGrpView.Run.OutputTail, "non-approver view must never carry outputTail")
	assert.Empty(t, sseGrpView.Run.OutputStatus, "non-approver view must never carry outputStatus (I1)")
	assert.Empty(t, sseGrpView.Run.Reason, "non-approver view must never carry reason")

	// 2. Group conversation retry path (P5 sweeper delivery retry) honours the same rule.
	disp.mu.Lock()
	disp.failLeft = 1
	disp.mu.Unlock()

	grpRetryReq, grpRetryRun := createGroupRun("grp-share-retry")
	wonRetry, err := f.srv.finishCommandRun(ctx, grpRetryReq, grpRetryRun, []string{store.CommandRunStatusQueued}, store.CommandRunStatusFailed, runReason, &agentcontrol.ExecResult{
		State:        agentcontrol.ExecStateFinished,
		Outcome:      agentcontrol.ExecOutcomeNonzero,
		ExitCode:     9,
		DurationMS:   33,
		OutputBytes:  int64(len(secretOutput)),
		OutputStatus: agentcontrol.ExecOutputCaptured,
		Output:       []byte(secretOutput),
	}, "")
	require.NoError(t, err)
	require.True(t, wonRetry)

	afterFailRetry, err := f.store.GetCommandRun(ctx, grpRetryRun.ID)
	require.NoError(t, err)
	assert.Nil(t, afterFailRetry.OutcomeDeliveredAt, "transient failure must clear claim for sweeper retry")
	require.NotEmpty(t, afterFailRetry.OutcomeMessageID)

	f.srv.sweepCommandRuns(ctx)

	afterSweepRetry, err := f.store.GetCommandRun(ctx, grpRetryRun.ID)
	require.NoError(t, err)
	assert.NotNil(t, afterSweepRetry.OutcomeDeliveredAt)
	assert.Equal(t, afterFailRetry.OutcomeMessageID, afterSweepRetry.OutcomeMessageID, "retry must reuse stored message ID")

	storedRetryMsg, err := f.store.GetMessage(ctx, afterSweepRetry.OutcomeMessageID)
	require.NoError(t, err)
	assert.Contains(t, storedRetryMsg.Msg, "exit code 9")
	assert.NotContains(t, storedRetryMsg.Msg, secretOutput, "group retry stored message must NOT contain output tail")
	assert.NotContains(t, storedRetryMsg.Msg, runReason, "group retry stored message must NOT contain run reason")

	require.Len(t, disp.msgs, 2)
	assert.Contains(t, disp.msgs[1], runReason, "retried asking agent delivery must include run reason")
	assert.Contains(t, disp.msgs[1], secretOutput, "retried asking agent delivery must include shared output tail")

	// 3. DM with ShareOutput=true (real direct conversation) remains unchanged.
	dmKey := "dm:user:" + f.owner.ID() + ":agent:" + f.asker.ID
	dmConv := &store.Conversation{
		ID:          api.NewUUID(),
		Kind:        "direct",
		Surface:     "native",
		ExternalRef: dmKey,
	}
	require.NoError(t, f.store.CreateConversation(ctx, dmConv))
	dmNorm, err := messages.NormalizeCommandRequest("command_request", &messages.CommandRequestInput{
		Key:                  "dm-share-unchanged",
		Script:               "echo dm",
		TimeoutS:             30,
		ShareOutputRequested: true,
	})
	require.NoError(t, err)
	dmPR, err := f.store.CreateCommandRequestPrompt(ctx, &store.CommandRequestPromptInput{
		Message: &store.Message{
			ID:             api.NewUUID(),
			ProjectID:      f.launched.ProjectID,
			ConversationID: dmConv.ID,
			ThreadID:       dmKey,
			Sender:         "agent:" + f.asker.Slug,
			SenderID:       f.asker.ID,
			Recipient:      "user:" + f.owner.Email(),
			RecipientID:    f.owner.ID(),
		},
		AgentID:    f.asker.ID,
		AgentName:  f.asker.Slug,
		ApproverID: f.owner.ID(),
		Request:    dmNorm,
		Now:        now,
		TTL:        msgaction.CommandRequestTTL,
	})
	require.NoError(t, err)
	dmApp, err := f.store.ApproveCommandRequest(ctx, &store.ApproveCommandRequestInput{
		RequestID:     dmPR.Request.ID,
		UserID:        f.owner.ID(),
		UserName:      "Owner",
		UserEmail:     f.owner.Email(),
		CommandSHA256: dmPR.Request.CommandSHA256,
		Surface:       msgaction.CommandRequestSurfaceCLI,
		ShareOutput:   true,
		ExecAgentID:   f.launched.ID,
		ExecProjectID: f.launched.ProjectID,
		Now:           now.Add(time.Second),
		LeaseUntil:    now.Add(5 * time.Minute),
	})
	require.NoError(t, err)

	wonDM, err := f.srv.finishCommandRun(ctx, dmApp.Request, dmApp.Run, []string{store.CommandRunStatusQueued}, store.CommandRunStatusFailed, runReason, &agentcontrol.ExecResult{
		State:        agentcontrol.ExecStateFinished,
		Outcome:      agentcontrol.ExecOutcomeNonzero,
		ExitCode:     3,
		DurationMS:   12,
		OutputBytes:  int64(len(secretOutput)),
		OutputStatus: agentcontrol.ExecOutputCaptured,
		Output:       []byte(secretOutput),
	}, "")
	require.NoError(t, err)
	require.True(t, wonDM)

	gotDMRun, err := f.store.GetCommandRun(ctx, dmApp.Run.ID)
	require.NoError(t, err)
	storedDMMsg, err := f.store.GetMessage(ctx, gotDMRun.OutcomeMessageID)
	require.NoError(t, err)
	assert.Contains(t, storedDMMsg.Msg, secretOutput, "DM stored message remains unchanged and includes shared output tail")
	assert.Contains(t, storedDMMsg.Msg, runReason, "DM stored message remains unchanged and includes run reason")
	require.Len(t, disp.msgs, 3)
	assert.Contains(t, disp.msgs[2], secretOutput)
	assert.Contains(t, disp.msgs[2], runReason)

	// 4. R1: Delivery-time defense against spoofed dm: chat_key on a group conversation.
	//    Even if an existing row in the DB carries ConversationID=groupConv.ID and a
	//    valid dm: ChatKey, isCommandRequestDM consults the conversation record and
	//    fails closed to false so reason and tail are never stored in the group.
	spoofNorm, err := messages.NormalizeCommandRequest("command_request", &messages.CommandRequestInput{
		Key:                  "grp-dm-key-spoof-delivery",
		Script:               "echo spoof",
		TimeoutS:             30,
		ShareOutputRequested: true,
	})
	require.NoError(t, err)
	spoofPR, err := f.store.CreateCommandRequestPrompt(ctx, &store.CommandRequestPromptInput{
		Message: &store.Message{
			ID:             api.NewUUID(),
			ProjectID:      f.launched.ProjectID,
			ConversationID: groupConv.ID,
			ThreadID:       "dm:agent:" + f.asker.ID + ":user:" + f.owner.ID(),
			Sender:         "agent:" + f.asker.Slug,
			SenderID:       f.asker.ID,
			Recipient:      "conv:" + groupConv.ID,
		},
		AgentID:    f.asker.ID,
		AgentName:  f.asker.Slug,
		ApproverID: f.owner.ID(),
		Request:    spoofNorm,
		Now:        now,
		TTL:        msgaction.CommandRequestTTL,
	})
	require.NoError(t, err)
	assert.False(t, f.srv.isCommandRequestDM(ctx, spoofPR.Request), "group conversation with spoofed dm: chat_key must not be treated as DM (R1)")
	assert.True(t, f.srv.isCommandRequestDM(ctx, &store.CommandRequest{ConversationID: dmConv.ID}), "direct conversation with empty ChatKey is DM")
	assert.False(t, f.srv.isCommandRequestDM(ctx, &store.CommandRequest{ConversationID: groupConv.ID}), "group conversation with empty ChatKey is not DM")
	assert.False(t, f.srv.isCommandRequestDM(ctx, &store.CommandRequest{ConversationID: api.NewUUID(), ChatKey: dmKey}), "missing conversation must fail closed even with dm: ChatKey")

	spoofApp, err := f.store.ApproveCommandRequest(ctx, &store.ApproveCommandRequestInput{
		RequestID:     spoofPR.Request.ID,
		UserID:        f.owner.ID(),
		UserName:      "Owner",
		UserEmail:     f.owner.Email(),
		CommandSHA256: spoofPR.Request.CommandSHA256,
		Surface:       msgaction.CommandRequestSurfaceCLI,
		ShareOutput:   true,
		ExecAgentID:   f.launched.ID,
		ExecProjectID: f.launched.ProjectID,
		Now:           now.Add(time.Second),
		LeaseUntil:    now.Add(5 * time.Minute),
	})
	require.NoError(t, err)
	wonSpoof, err := f.srv.finishCommandRun(ctx, spoofApp.Request, spoofApp.Run, []string{store.CommandRunStatusQueued}, store.CommandRunStatusFailed, runReason, &agentcontrol.ExecResult{
		State:        agentcontrol.ExecStateFinished,
		Outcome:      agentcontrol.ExecOutcomeNonzero,
		ExitCode:     5,
		DurationMS:   15,
		OutputBytes:  int64(len(secretOutput)),
		OutputStatus: agentcontrol.ExecOutputCaptured,
		Output:       []byte(secretOutput),
	}, "")
	require.NoError(t, err)
	require.True(t, wonSpoof)
	gotSpoofRun, err := f.store.GetCommandRun(ctx, spoofApp.Run.ID)
	require.NoError(t, err)
	storedSpoofMsg, err := f.store.GetMessage(ctx, gotSpoofRun.OutcomeMessageID)
	require.NoError(t, err)
	assert.NotContains(t, storedSpoofMsg.Msg, secretOutput, "spoofed dm: chat_key on group conv must NOT store output tail (R1)")
	assert.NotContains(t, storedSpoofMsg.Msg, runReason, "spoofed dm: chat_key on group conv must NOT store run reason (R1)")

	// 5. R2: End-to-end conduit hold -> claim -> processClaimedHeldRow re-drive path
	//    must (a) pass revalidateHeldMessageSender for hub-emitted system:hub
	//    TypeCommandResponse rows backed by command_runs.outcome_message_id, and
	//    (b) reconstruct the full asking-agent outcome (reason + redacted shared tail +
	//    command_* metadata) while keeping the stored group DB row status-only.
	//    Clear the dispatcher briefly so finishCommandRun persists the status-only
	//    outcome message in DispatchState=dispatched without live dispatch, allowing
	//    us to transition it through the real conduit no-session hold -> re-drive flow.
	// Give f.asker a live RunID so PrepareOrHoldMessage stamps hold_launch_id,
	// matching real conduit-deliver hold behavior.
	f.asker.RunID = api.NewUUID()
	_, err = f.store.SetAgentRunID(ctx, f.asker.ID, f.asker.RunID, nil)
	require.NoError(t, err)

	f.srv.SetDispatcher(nil)
	holdReq, holdRun := createGroupRun("grp-share-hold-redrive")
	wonHold, err := f.srv.finishCommandRun(ctx, holdReq, holdRun, []string{store.CommandRunStatusQueued}, store.CommandRunStatusFailed, runReason, &agentcontrol.ExecResult{
		State:        agentcontrol.ExecStateFinished,
		Outcome:      agentcontrol.ExecOutcomeNonzero,
		ExitCode:     11,
		DurationMS:   27,
		OutputBytes:  int64(len(secretOutput)),
		OutputStatus: agentcontrol.ExecOutputCaptured,
		Output:       []byte(secretOutput),
	}, "")
	f.srv.SetDispatcher(disp)
	require.NoError(t, err)
	require.True(t, wonHold)

	gotHoldRun, err := f.store.GetCommandRun(ctx, holdRun.ID)
	require.NoError(t, err)
	require.NotEmpty(t, gotHoldRun.OutcomeMessageID)

	// Transition the persisted status-only outcome row through PrepareOrHoldMessage
	// (stamping hold_launch_id), conduit no-session proof, and awaiting_session hold.
	prepRes, err := f.store.PrepareOrHoldMessage(ctx, f.asker.ID, gotHoldRun.OutcomeMessageID, 50)
	require.NoError(t, err)
	require.Equal(t, f.asker.RunID, prepRes.HoldLaunchID)
	holdDeliveryID := "hold-" + gotHoldRun.OutcomeMessageID
	_, err = f.store.BindMessageDelivery(ctx, f.asker.ID, holdDeliveryID, []string{gotHoldRun.OutcomeMessageID}, now.Add(time.Minute))
	require.NoError(t, err)
	require.NoError(t, f.store.UnbindMessageDeliveryWithProof(ctx, holdDeliveryID, []string{gotHoldRun.OutcomeMessageID}, agentcontrol.ReasonNoSession))
	heldOK, err := f.store.HoldMessageAwaitingSession(ctx, store.HoldMessageParams{
		MessageID:   gotHoldRun.OutcomeMessageID,
		AgentID:     f.asker.ID,
		MaxProofAge: time.Hour,
		HoldTTL:     10 * time.Minute,
		MaxHeld:     50,
	})
	require.NoError(t, err)
	require.True(t, heldOK, "persisted group command-response row must enter awaiting_session hold")

	leaseOwner := api.NewUUID()
	leaseOK, err := f.store.AcquireAgentRedriveLease(ctx, f.asker.ID, leaseOwner, time.Minute)
	require.NoError(t, err)
	require.True(t, leaseOK)
	defer func() { _ = f.store.ReleaseAgentRedriveLease(context.Background(), f.asker.ID, leaseOwner) }()

	claimedHeld, err := f.store.ClaimHeldMessages(store.WithRedriveLeaseOwner(ctx, leaseOwner), f.asker.ID, 10)
	require.NoError(t, err)
	require.Len(t, claimedHeld, 1)
	require.Equal(t, gotHoldRun.OutcomeMessageID, claimedHeld[0].ID)

	dispBeforeRedrive := len(disp.msgs)
	outcome := f.srv.processClaimedHeldRow(ctx, disp, f.asker.ID, &claimedHeld[0], nil)
	assert.Equal(t, claimedRowDispatched, outcome, "held system:hub command-response must pass revalidation and dispatch on re-drive (R2)")
	require.Len(t, disp.msgs, dispBeforeRedrive+1, "re-drive must dispatch enriched command-response to asking agent")
	redrivenMsg := disp.msgs[dispBeforeRedrive]
	redrivenSM := disp.structuredMsgs[dispBeforeRedrive]
	require.NotNil(t, redrivenSM)
	assert.Contains(t, redrivenMsg, runReason, "re-driven delivery text must restore run reason for asking agent (R2)")
	assert.Contains(t, redrivenMsg, secretOutput, "re-driven delivery text must restore redacted shared output tail (R2)")
	assert.Contains(t, redrivenSM.Msg, runReason)
	assert.Contains(t, redrivenSM.Msg, secretOutput)
	assert.Equal(t, gotHoldRun.ID, redrivenSM.Metadata["command_run_id"])
	assert.Equal(t, runReason, redrivenSM.Metadata["command_run_reason"])
	assert.Equal(t, "11", redrivenSM.Metadata["command_run_exit_code"])

	// Re-check stored row in DB is still status-only (enrichment never mutates stored row)
	// and carries the fixed agent-only pointer line.
	recheckedStored, err := f.store.GetMessage(ctx, gotHoldRun.OutcomeMessageID)
	require.NoError(t, err)
	assert.Contains(t, recheckedStored.Msg, "Full outcome: `scion command request list`.")
	assert.NotContains(t, recheckedStored.Msg, secretOutput)
	assert.NotContains(t, recheckedStored.Msg, runReason)

	// Negative provenance checks for system:hub TypeCommandResponse revalidation:
	// (a) unreferenced message ID fails closed;
	okUnref, err := f.srv.revalidateHeldMessageSender(ctx, &store.Message{
		ID:          api.NewUUID(),
		Sender:      "system:hub",
		SenderID:    "system:hub",
		Type:        messages.TypeCommandResponse,
		RecipientID: f.asker.ID,
	}, f.asker)
	require.NoError(t, err)
	assert.False(t, okUnref, "forged system:hub command-response without matching outcome_message_id must fail revalidation")

	// (b) real outcome_message_id targeted at a different agent fails closed;
	okWrongTarget, err := f.srv.revalidateHeldMessageSender(ctx, recheckedStored, f.launched)
	require.NoError(t, err)
	assert.False(t, okWrongTarget, "command-response outcome_message_id must not revalidate for a different target agent")

	// (c) system:hub row with mismatched SenderID fails closed.
	spoofSenderIDRow := *recheckedStored
	spoofSenderIDRow.SenderID = f.owner.ID()
	okSpoofSenderID, err := f.srv.revalidateHeldMessageSender(ctx, &spoofSenderIDRow, f.asker)
	require.NoError(t, err)
	assert.False(t, okSpoofSenderID, "system:hub command-response with non-system:hub SenderID must fail revalidation")
}
