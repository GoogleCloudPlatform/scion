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
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

// cmdRunP2Dispatcher is a test AgentDispatcher for P2 wake-on-approve tests.
type cmdRunP2Dispatcher struct {
	AgentDispatcher
	mu      sync.Mutex
	calls   int
	resumes []bool
	onStart func(ctx context.Context, agent *store.Agent, task string, resume bool) error
}

func (d *cmdRunP2Dispatcher) DispatchAgentStart(ctx context.Context, agent *store.Agent, task string, resume bool) error {
	d.mu.Lock()
	d.calls++
	d.resumes = append(d.resumes, resume)
	fn := d.onStart
	d.mu.Unlock()
	if fn != nil {
		return fn(ctx, agent, task, resume)
	}
	return nil
}

func (d *cmdRunP2Dispatcher) DispatchAgentMessage(context.Context, *store.Agent, string, bool, *messages.StructuredMessage) error {
	return nil
}

func (d *cmdRunP2Dispatcher) startCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

func (d *cmdRunP2Dispatcher) lastResume() (bool, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.resumes) == 0 {
		return false, false
	}
	return d.resumes[len(d.resumes)-1], true
}

func setCommandRunP2TimingSeams(t *testing.T, wakeTimeout, readyInit, readyMax time.Duration) {
	t.Helper()
	scheduledExecSeamMu.Lock()
	origWake, origInit, origMax := commandRunWakeTimeout, commandRunReadyPollInitial, commandRunReadyPollMax
	commandRunWakeTimeout = wakeTimeout
	commandRunReadyPollInitial = readyInit
	commandRunReadyPollMax = readyMax
	scheduledExecSeamMu.Unlock()
	t.Cleanup(func() {
		scheduledExecSeamMu.Lock()
		commandRunWakeTimeout = origWake
		commandRunReadyPollInitial = origInit
		commandRunReadyPollMax = origMax
		scheduledExecSeamMu.Unlock()
	})
}

// newCmdRunP2Fixture builds a real Hub server with a suspended (no Conduit
// session yet) shell execution container owned by f.owner and a pluggable
// dispatcher.
func newCmdRunP2Fixture(t *testing.T, disp *cmdRunP2Dispatcher, capture *execCapture) *cmdRunExecFixture {
	t.Helper()
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
	setCommandRunP2TimingSeams(t, 10*time.Second, 20*time.Millisecond, 50*time.Millisecond)

	audit := &cmdRunAuditCollector{LogAuditLogger: NewLogAuditLogger("", false)}
	f := newPTYConduitFixtureServer(t, func(s *Server) {
		s.auditLogger = audit
		if disp != nil {
			s.SetDispatcher(disp)
		}
	})
	setHubExperiments(t, f.srv, map[string]bool{
		conduitExperiment:           true,
		experiments.ExecContainers:  true,
		experiments.CommandRequests: true,
	})

	ctx := context.Background()
	ag, err := f.store.GetAgent(ctx, f.launched.ID)
	require.NoError(t, err)
	ag.HarnessConfig = config.ShellHarnessConfigName
	ag.AppliedConfig = &store.AgentAppliedConfig{HarnessConfig: config.ShellHarnessConfigName}
	ag.OwnerID = f.owner.ID()
	ag.Phase = string(state.PhaseSuspended)
	ag.SuspendReason = string(state.SuspendReasonIdle)
	require.NoError(t, f.store.UpdateAgent(ctx, ag))
	f.launched = ag

	_, err = f.store.PutUserExecContainer(ctx, f.owner.ID(), ag.ID)
	require.NoError(t, err)

	asker := &store.Agent{
		ID:            api.NewUUID(),
		Slug:          "asker-p2",
		Name:          "asker-p2",
		ProjectID:     ag.ProjectID,
		OwnerID:       f.owner.ID(),
		HarnessConfig: "claude",
		Phase:         string(state.PhaseRunning),
	}
	require.NoError(t, f.store.CreateAgent(ctx, asker))

	if capture == nil {
		capture = &execCapture{}
	}
	return &cmdRunExecFixture{
		ptyConduitFixture: f,
		audit:             audit,
		capture:           capture,
		asker:             asker,
	}
}

func (f *cmdRunExecFixture) createPendingRequest(t *testing.T, key, script string) *store.CommandRequest {
	t.Helper()
	now := time.Now().UTC()
	norm, err := messages.NormalizeCommandRequest("command_request", &messages.CommandRequestInput{
		Key:      key,
		Script:   script,
		TimeoutS: 30,
	})
	require.NoError(t, err)
	pr, err := f.store.CreateCommandRequestPrompt(context.Background(), &store.CommandRequestPromptInput{
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
	return pr.Request
}

func (f *cmdRunExecFixture) postApprove(t *testing.T, user UserIdentity, req *store.CommandRequest, idemKey string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(messages.ApproveCommandRequestBody{
		RowID:          req.ID,
		CommandSHA256:  req.CommandSHA256,
		Surface:        msgaction.CommandRequestSurfaceCLI,
		IdempotencyKey: idemKey,
	})
	require.NoError(t, err)
	httpReq := httptest.NewRequest(http.MethodPost, "/api/v1/command-requests/"+req.ID+"/approve", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq = httpReq.WithContext(contextWithIdentity(httpReq.Context(), user))
	rec := httptest.NewRecorder()
	f.srv.handleCommandRequestRoutes(rec, httpReq)
	return rec
}

// TestCommandRunP2_AC_F3_SuspendedWakeAndConduitReadiness_RealHandler verifies
// AC-F3 end-to-end through the real approve handler: approving against a
// suspended container transitions queued -> waking (wake=resume, audit
// run_waking), resumes via startAgentCore(StartClaimWake, Resume=true), waits
// for the new incarnation's Conduit session advertising exec routes + features,
// re-resolves & pins, dispatches exec-start once, and records succeeded.
func TestCommandRunP2_AC_F3_SuspendedWakeAndConduitReadiness_RealHandler(t *testing.T) {
	newRunID := api.NewUUID()
	var fx *cmdRunExecFixture
	disp := &cmdRunP2Dispatcher{
		onStart: func(ctx context.Context, agent *store.Agent, task string, resume bool) error {
			// Simulate broker rotating RunID on resume and container connecting to Conduit.
			if _, err := fx.store.SetAgentRunID(ctx, agent.ID, newRunID, nil); err != nil {
				return err
			}
			agent.RunID = newRunID
			// Connect the resumed container's Conduit session slightly after resume returns.
			go func() {
				time.Sleep(40 * time.Millisecond)
				fx.startControlAgentAs(t, fx.public.URL, newRunID, true, execControlOptions(t), fx.capture.wrap)
			}()
			return nil
		},
	}
	fx = newCmdRunP2Fixture(t, disp, nil)

	reqRow := fx.createPendingRequest(t, "wake-e2e", "echo woken-ok")

	// Verify GET detail reports exec_container.canWake=true for suspended container
	// when approver holds lifecycle permission.
	getReq := httptest.NewRequest(http.MethodGet, "/api/v1/command-requests/"+reqRow.ID, nil)
	getReq = getReq.WithContext(contextWithIdentity(getReq.Context(), fx.owner))
	getRec := httptest.NewRecorder()
	fx.srv.handleCommandRequestRoutes(getRec, getReq)
	require.Equal(t, http.StatusOK, getRec.Code, "body: %s", getRec.Body.String())
	var detailResp messages.CommandRequestDetailResponse
	require.NoError(t, json.Unmarshal(getRec.Body.Bytes(), &detailResp))
	require.NotNil(t, detailResp.ExecContainer)
	assert.Equal(t, string(ExecContainerStateSuspended), detailResp.ExecContainer.State)
	assert.True(t, detailResp.ExecContainer.CanWake, "canWake must be true when suspended and approver has lifecycle permission")

	rec := fx.postApprove(t, fx.owner, reqRow, "wake-e2e-idem")
	require.Equal(t, http.StatusAccepted, rec.Code, "body: %s", rec.Body.String())

	var appResp messages.ApproveCommandRequestResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &appResp))
	fin := fx.waitRunFinished(t, appResp.Run.RunID)

	assert.Equal(t, store.CommandRunStatusSucceeded, fin.Status)
	assert.Equal(t, store.CommandRunWakeResume, fin.Wake)
	assert.Equal(t, newRunID, fin.LaunchID)
	require.NotNil(t, fin.ExitCode)
	assert.Equal(t, 0, *fin.ExitCode)
	assert.Equal(t, 1, disp.startCount())
	resumed, ok := disp.lastResume()
	require.True(t, ok)
	assert.True(t, resumed, "wake must pass Resume=true to startAgentCore")
	assert.Equal(t, 1, fx.capture.count(execRoutePath(agentcontrol.RouteExecStart)))

	var sawWaking bool
	for _, ev := range fx.audit.snapshot() {
		if ev.EventType == msgaction.EventCmdRunWaking && ev.Details["run_id"] == fin.ID && ev.Details["wake"] == store.CommandRunWakeResume {
			sawWaking = true
		}
	}
	assert.True(t, sawWaking, "expected run_waking audit event with wake=resume")
}

// TestCommandRunP2_AC_F3_StartAlreadyInProgress_WaitsNotStarts verifies §4.8
// step 2: when another start already holds the agent's start claim, the
// executor does not dispatch a second start and instead waits for Conduit
// readiness.
func TestCommandRunP2_AC_F3_StartAlreadyInProgress_WaitsNotStarts(t *testing.T) {
	disp := &cmdRunP2Dispatcher{}
	fx := newCmdRunP2Fixture(t, disp, nil)
	ctx := context.Background()

	// Enable start claims and take a competing StartClaimWake from another owner.
	fx.srv.startClaimsOn = true
	claim, err := fx.store.ClaimAgentStart(ctx, fx.launched.ID, "other-hub", store.StartClaimWake, "", 30*time.Second)
	require.NoError(t, err)

	req, run := fx.createApprovedRun(t, "concurrent-start", "echo waited-ok", "", 30)
	fx.srv.EnqueueCommandRun(req, run)

	// Simulate the competing start finishing: release claim, mark running with
	// new RunID, and connect Conduit session.
	newRunID := api.NewUUID()
	go func() {
		time.Sleep(60 * time.Millisecond)
		_, _ = fx.store.ReleaseAgentStart(context.Background(), fx.launched.ID, claim.ID, "other-hub")
		_, _ = fx.store.SetAgentRunID(context.Background(), fx.launched.ID, newRunID, nil)
		ag, gerr := fx.store.GetAgent(context.Background(), fx.launched.ID)
		if gerr == nil {
			ag.Phase = string(state.PhaseRunning)
			_ = fx.store.UpdateAgent(context.Background(), ag)
			fx.startControlAgentAs(t, fx.public.URL, newRunID, true, execControlOptions(t), fx.capture.wrap)
		}
	}()

	fin := fx.waitRunFinished(t, run.ID)
	assert.Equal(t, store.CommandRunStatusSucceeded, fin.Status)
	assert.Equal(t, newRunID, fin.LaunchID)
	assert.Zero(t, disp.startCount(), "must not start when another start claim is already held")
	assert.Equal(t, 1, fx.capture.count(execRoutePath(agentcontrol.RouteExecStart)))
}

// setupDelegatedExecContainer creates a suspended shell execution container
// owned by another user and registers it as fx.owner's execution container
// with a custom project role binding granting perms. Returns the container and
// the binding ID so callers can revoke it mid-flight.
func setupDelegatedExecContainer(t *testing.T, fx *cmdRunExecFixture, perms []string) (*store.Agent, string) {
	t.Helper()
	ctx := context.Background()
	otherOwnerID := api.NewUUID()
	require.NoError(t, fx.store.CreateUser(ctx, &store.User{
		ID:          otherOwnerID,
		Email:       "other-" + otherOwnerID[:8] + "@conduit.test",
		DisplayName: "Other Owner",
		Role:        store.UserRoleMember,
		Status:      "active",
	}))
	runID := api.NewUUID()
	ag := &store.Agent{
		ID:            api.NewUUID(),
		Slug:          "delegated-exec-" + api.NewUUID()[:8],
		Name:          "delegated-exec",
		ProjectID:     fx.launched.ProjectID,
		OwnerID:       otherOwnerID,
		HarnessConfig: config.ShellHarnessConfigName,
		AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: config.ShellHarnessConfigName},
		Phase:         string(state.PhaseSuspended),
		SuspendReason: string(state.SuspendReasonIdle),
	}
	require.NoError(t, fx.store.CreateAgent(ctx, ag))
	_, err := fx.store.SetAgentRunID(ctx, ag.ID, runID, nil)
	require.NoError(t, err)
	ag.RunID = runID
	_, err = fx.store.PutUserExecContainer(ctx, fx.owner.ID(), ag.ID)
	require.NoError(t, err)
	fx.launched = ag

	rd, err := fx.store.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        "p2-perm-" + api.NewUUID()[:8],
		ScopeType:   store.RoleScopeProject,
		Permissions: perms,
	})
	require.NoError(t, err)
	rb, err := fx.store.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      fx.owner.ID(),
		ScopeType:        store.RoleScopeProject,
		ScopeID:          ag.ProjectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
	return ag, rb.ID
}

// TestCommandRunP2_AC_F3_LifecyclePermissionCheckedAtApproveAndWake verifies
// §4.7 step 4 (approve refuses suspended container without agent.lifecycle
// with 409 exec_container_unavailable + detail wake_not_permitted) and §4.8
// step 2 (executor re-checks agent.lifecycle as approver before waking).
func TestCommandRunP2_AC_F3_LifecyclePermissionCheckedAtApproveAndWake(t *testing.T) {
	t.Run("approve_refuses_suspended_without_lifecycle", func(t *testing.T) {
		disp := &cmdRunP2Dispatcher{}
		fx := newCmdRunP2Fixture(t, disp, nil)
		ctx := context.Background()
		setupDelegatedExecContainer(t, fx, []string{"agent.attach"})

		// Confirm attach still passes (so ResolveExecContainer returns suspended),
		// while lifecycle is denied.
		res, rerr := fx.srv.resolveExecContainerForIdentity(ctx, fx.owner)
		require.NoError(t, rerr)
		require.Equal(t, ExecContainerStateSuspended, res.State)
		require.False(t, fx.srv.agentLifecycleAllowed(ctx, fx.owner, res.Agent))

		reqRow := fx.createPendingRequest(t, "no-lifecycle-approve", "echo nope")

		// GET detail must report exec_container.canWake=false when lifecycle is denied.
		getReq := httptest.NewRequest(http.MethodGet, "/api/v1/command-requests/"+reqRow.ID, nil)
		getReq = getReq.WithContext(contextWithIdentity(getReq.Context(), fx.owner))
		getRec := httptest.NewRecorder()
		fx.srv.handleCommandRequestRoutes(getRec, getReq)
		require.Equal(t, http.StatusOK, getRec.Code, "body: %s", getRec.Body.String())
		var detailResp messages.CommandRequestDetailResponse
		require.NoError(t, json.Unmarshal(getRec.Body.Bytes(), &detailResp))
		require.NotNil(t, detailResp.ExecContainer)
		assert.Equal(t, string(ExecContainerStateSuspended), detailResp.ExecContainer.State)
		assert.False(t, detailResp.ExecContainer.CanWake, "canWake must be false when approver lacks lifecycle permission")

		rec := fx.postApprove(t, fx.owner, reqRow, "no-lc-1")
		require.Equal(t, http.StatusConflict, rec.Code, "body: %s", rec.Body.String())

		var errBody struct {
			Error struct {
				Code    string                 `json:"code"`
				Details map[string]interface{} `json:"details"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errBody))
		assert.Equal(t, msgaction.ConflictCmdExecUnavailable, errBody.Error.Code)
		assert.Equal(t, "wake_not_permitted", errBody.Error.Details["detail"])

		// Row stays pending and nothing was started.
		got, err := fx.store.GetCommandRequest(ctx, reqRow.ID)
		require.NoError(t, err)
		assert.Equal(t, store.CommandRequestStatusPending, got.Status)
		assert.Zero(t, disp.startCount())
	})

	t.Run("executor_rechecks_lifecycle_before_wake", func(t *testing.T) {
		disp := &cmdRunP2Dispatcher{}
		fx := newCmdRunP2Fixture(t, disp, nil)
		ctx := context.Background()

		// Grant attach+lifecycle at approval, then replace with attach-only
		// before the executor runs.
		ag, rbID := setupDelegatedExecContainer(t, fx, []string{"agent.attach", "agent.lifecycle"})
		req, run := fx.createApprovedRun(t, "no-lifecycle-exec", "echo nope", "", 30)
		require.NoError(t, fx.store.DeleteRoleBinding(ctx, rbID))
		rdAttach, err := fx.store.CreateRoleDefinition(ctx, &store.RoleDefinition{
			Name:        "p2-attach-only-" + api.NewUUID()[:8],
			ScopeType:   store.RoleScopeProject,
			Permissions: []string{"agent.attach"},
		})
		require.NoError(t, err)
		_, err = fx.store.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: rdAttach.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      fx.owner.ID(),
			ScopeType:        store.RoleScopeProject,
			ScopeID:          ag.ProjectID,
			CreatedBy:        "test",
		})
		require.NoError(t, err)

		fx.srv.EnqueueCommandRun(req, run)
		fin := fx.waitRunFinished(t, run.ID)
		assert.Equal(t, store.CommandRunStatusNotStarted, fin.Status)
		assert.Equal(t, "wake_not_permitted", fin.Reason)
		assert.Zero(t, disp.startCount())
		assert.Zero(t, fx.capture.count(execRoutePath(agentcontrol.RouteExecStart)))
	})
}

// TestCommandRunP2_AC_F4_WakeFailed_WakeTimeout_ExecUnsupported_MarkerFile
// verifies AC-F4: wake failure, Conduit readiness timeout, and missing exec
// features each finish as not_started with the documented reason, and a marker
// file in the script proves nothing ever ran.
func TestCommandRunP2_AC_F4_WakeFailed_WakeTimeout_ExecUnsupported_MarkerFile(t *testing.T) {
	t.Run("wake_failed", func(t *testing.T) {
		disp := &cmdRunP2Dispatcher{
			onStart: func(context.Context, *store.Agent, string, bool) error {
				return errors.New("broker unavailable")
			},
		}
		fx := newCmdRunP2Fixture(t, disp, nil)
		marker := filepath.Join(t.TempDir(), "should-not-exist")
		req, run := fx.createApprovedRun(t, "wake-fail", "touch "+marker, "", 30)

		fx.srv.EnqueueCommandRun(req, run)
		fin := fx.waitRunFinished(t, run.ID)
		assert.Equal(t, store.CommandRunStatusNotStarted, fin.Status)
		assert.Equal(t, "wake_failed", fin.Reason)
		assert.Zero(t, fx.capture.count(execRoutePath(agentcontrol.RouteExecStart)))
		_, statErr := os.Stat(marker)
		assert.True(t, os.IsNotExist(statErr), "marker file must not exist after wake_failed")
	})

	t.Run("wake_timeout", func(t *testing.T) {
		disp := &cmdRunP2Dispatcher{} // transitions agent to running, but no Conduit session ever connects
		fx := newCmdRunP2Fixture(t, disp, nil)
		setCommandRunP2TimingSeams(t, 120*time.Millisecond, 20*time.Millisecond, 40*time.Millisecond)

		marker := filepath.Join(t.TempDir(), "should-not-exist")
		req, run := fx.createApprovedRun(t, "wake-timeout", "touch "+marker, "", 30)

		fx.srv.EnqueueCommandRun(req, run)
		fin := fx.waitRunFinished(t, run.ID)
		assert.Equal(t, store.CommandRunStatusNotStarted, fin.Status)
		assert.Equal(t, "wake_timeout", fin.Reason)
		assert.Zero(t, fx.capture.count(execRoutePath(agentcontrol.RouteExecStart)))
		_, statErr := os.Stat(marker)
		assert.True(t, os.IsNotExist(statErr), "marker file must not exist after wake_timeout")
	})

	t.Run("exec_unsupported_missing_features", func(t *testing.T) {
		disp := &cmdRunP2Dispatcher{}
		fx := newCmdRunP2Fixture(t, disp, nil)
		ctx := context.Background()
		ag, err := fx.store.GetAgent(ctx, fx.launched.ID)
		require.NoError(t, err)
		ag.Phase = string(state.PhaseRunning)
		require.NoError(t, fx.store.UpdateAgent(ctx, ag))
		// Connect Conduit session advertising routes but NOT features (exec_capture.v1 / exec_workdir.v1).
		fx.startControlAgentAsWithFeatures(t, fx.public.URL, ag.RunID, true, false, execControlOptions(t), fx.capture.wrap)

		marker := filepath.Join(t.TempDir(), "should-not-exist")
		req, run := fx.createApprovedRun(t, "exec-unsup", "touch "+marker, "", 30)

		fx.srv.EnqueueCommandRun(req, run)
		fin := fx.waitRunFinished(t, run.ID)
		assert.Equal(t, store.CommandRunStatusNotStarted, fin.Status)
		assert.Equal(t, "exec_unsupported", fin.Reason)
		assert.Nil(t, fin.DispatchedAt, "readiness check must refuse before dispatching is committed")
		for _, ev := range fx.audit.snapshot() {
			assert.NotEqual(t, msgaction.EventCmdRunDispatched, ev.EventType, "must not emit run_dispatched when readiness refuses")
		}
		assert.Zero(t, fx.capture.count(execRoutePath(agentcontrol.RouteExecStart)))
		_, statErr := os.Stat(marker)
		assert.True(t, os.IsNotExist(statErr), "marker file must not exist after exec_unsupported")
	})
}

// TestCommandRunP2_AC_S5_ContainerChangedDuringWake verifies AC-S5: if the
// user's execution container registration is re-pointed, the container is
// deleted, or attach is revoked between approval and dispatch, the run ends
// not_started/container_changed with zero exec-start calls.
func TestCommandRunP2_AC_S5_ContainerChangedDuringWake(t *testing.T) {
	t.Run("registration_repointed_during_wake", func(t *testing.T) {
		var fx *cmdRunExecFixture
		disp := &cmdRunP2Dispatcher{
			onStart: func(ctx context.Context, agent *store.Agent, _ string, _ bool) error {
				// Connect Conduit on original container, then re-point registration to a second container.
				fx.startControlAgentAs(t, fx.public.URL, agent.RunID, true, execControlOptions(t), fx.capture.wrap)
				other := &store.Agent{
					ID:            api.NewUUID(),
					Slug:          "exec-other",
					Name:          "exec-other",
					ProjectID:     agent.ProjectID,
					OwnerID:       fx.owner.ID(),
					HarnessConfig: config.ShellHarnessConfigName,
					AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: config.ShellHarnessConfigName},
					Phase:         string(state.PhaseRunning),
					RunID:         api.NewUUID(),
				}
				if err := fx.store.CreateAgent(ctx, other); err != nil {
					return err
				}
				_, err := fx.store.PutUserExecContainer(ctx, fx.owner.ID(), other.ID)
				return err
			},
		}
		fx = newCmdRunP2Fixture(t, disp, nil)
		req, run := fx.createApprovedRun(t, "s5-repoint", "echo nope", "", 30)
		fx.srv.EnqueueCommandRun(req, run)

		fin := fx.waitRunFinished(t, run.ID)
		assert.Equal(t, store.CommandRunStatusNotStarted, fin.Status)
		assert.Equal(t, "container_changed", fin.Reason)
		assert.Zero(t, fx.capture.count(execRoutePath(agentcontrol.RouteExecStart)))
	})

	t.Run("container_deleted_during_wake", func(t *testing.T) {
		var fx *cmdRunExecFixture
		disp := &cmdRunP2Dispatcher{
			onStart: func(ctx context.Context, agent *store.Agent, _ string, _ bool) error {
				return fx.store.DeleteAgent(ctx, agent.ID)
			},
		}
		fx = newCmdRunP2Fixture(t, disp, nil)
		req, run := fx.createApprovedRun(t, "s5-delete", "echo nope", "", 30)
		fx.srv.EnqueueCommandRun(req, run)

		fin := fx.waitRunFinished(t, run.ID)
		assert.Equal(t, store.CommandRunStatusNotStarted, fin.Status)
		assert.Equal(t, "container_changed", fin.Reason)
		assert.Zero(t, fx.capture.count(execRoutePath(agentcontrol.RouteExecStart)))
	})

	t.Run("attach_revoked_during_wake", func(t *testing.T) {
		var fx *cmdRunExecFixture
		var rbID string
		disp := &cmdRunP2Dispatcher{
			onStart: func(ctx context.Context, agent *store.Agent, _ string, _ bool) error {
				// Revoke the approver's attach binding during wake, then connect
				// Conduit slightly after resume returns (once Phase=running).
				if err := fx.store.DeleteRoleBinding(ctx, rbID); err != nil {
					return err
				}
				runID := agent.RunID
				go func() {
					time.Sleep(40 * time.Millisecond)
					fx.startControlAgentAs(t, fx.public.URL, runID, true, execControlOptions(t), fx.capture.wrap)
				}()
				return nil
			},
		}
		fx = newCmdRunP2Fixture(t, disp, nil)
		ctx := context.Background()

		// Create a second user who is NOT the container's owner, grant them
		// agent.attach + agent.lifecycle via a revocable role binding, and
		// register fx.launched as their execution container.
		approver2ID := api.NewUUID()
		require.NoError(t, fx.store.CreateUser(ctx, &store.User{
			ID:          approver2ID,
			Email:       "approver2-" + approver2ID[:8] + "@conduit.test",
			DisplayName: "Approver Two",
			Role:        store.UserRoleMember,
			Status:      "active",
		}))
		rd, err := fx.store.CreateRoleDefinition(ctx, &store.RoleDefinition{
			Name:        "p2-revoke-attach-" + api.NewUUID()[:8],
			ScopeType:   store.RoleScopeProject,
			Permissions: []string{"agent.attach", "agent.lifecycle"},
		})
		require.NoError(t, err)
		rb, err := fx.store.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: rd.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      approver2ID,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          fx.launched.ProjectID,
			CreatedBy:        "test",
		})
		require.NoError(t, err)
		rbID = rb.ID
		_, err = fx.store.PutUserExecContainer(ctx, approver2ID, fx.launched.ID)
		require.NoError(t, err)

		// Approve as approver2.
		now := time.Now().UTC()
		norm, err := messages.NormalizeCommandRequest("command_request", &messages.CommandRequestInput{
			Key: "s5-revoke", Script: "echo nope", TimeoutS: 30,
		})
		require.NoError(t, err)
		pr, err := fx.store.CreateCommandRequestPrompt(ctx, &store.CommandRequestPromptInput{
			Message: &store.Message{
				ID: api.NewUUID(), ProjectID: fx.launched.ProjectID, ConversationID: api.NewUUID(),
				ThreadID: "dm:user:" + approver2ID + ":agent:" + fx.asker.ID,
				Sender:   "agent:" + fx.asker.Slug, SenderID: fx.asker.ID,
				Recipient: "user:approver2@conduit.test", RecipientID: approver2ID,
			},
			AgentID: fx.asker.ID, AgentName: fx.asker.Slug, ApproverID: approver2ID,
			Request: norm, Now: now, TTL: msgaction.CommandRequestTTL,
		})
		require.NoError(t, err)
		appRes, err := fx.store.ApproveCommandRequest(ctx, &store.ApproveCommandRequestInput{
			RequestID: pr.Request.ID, CommandSHA256: pr.Request.CommandSHA256,
			UserID: approver2ID, Surface: msgaction.CommandRequestSurfaceCLI,
			IdempotencyKey: "s5-revoke-idem", ExecAgentID: fx.launched.ID,
			ExecProjectID: fx.launched.ProjectID, LeaseUntil: now.Add(5 * time.Minute), Now: now,
		})
		require.NoError(t, err)

		fx.srv.EnqueueCommandRun(appRes.Request, appRes.Run)
		fin := fx.waitRunFinished(t, appRes.Run.ID)
		assert.Equal(t, store.CommandRunStatusNotStarted, fin.Status)
		assert.Equal(t, "container_changed", fin.Reason)
		assert.Zero(t, fx.capture.count(execRoutePath(agentcontrol.RouteExecStart)))
	})
}

// TestCommandRunP2_SweeperC_MultiReplicaClaimAndRecovery verifies design §4.12
// sweeper (c):
//   - an unowned live dispatching/running run (e.g. after replica 1 restarted)
//     is claimed via ClaimCommandRunsForPoll and polled once to record its real
//     outcome without ever resending exec-start;
//   - a run owned by a replica's in-memory tracker is excluded from that
//     replica's sweeper;
//   - two replicas sweeping concurrently divide unowned runs via
//     poll_claimed_at without double-polling within claimTTL.
func TestCommandRunP2_SweeperC_MultiReplicaClaimAndRecovery(t *testing.T) {
	fx := newCmdRunExecFixture(t, nil)
	ctx := context.Background()

	// Start a real run directly on the container so its finished tombstone exists,
	// then record a live running command_runs row as if replica 1 died before
	// polling its completion.
	_, run := fx.createApprovedRun(t, "sweeper-c-recover", "echo recovered-by-sweeper", "", 30)
	now := time.Now().UTC()
	won, err := fx.store.TransitionCommandRunToDispatching(ctx, &store.CommandRunDispatchInput{
		RunID:        run.ID,
		LaunchID:     fx.launched.RunID,
		NotAfter:     now.Add(1 * time.Minute),
		LeaseUntil:   now.Add(5 * time.Minute),
		DispatchedAt: now,
	})
	require.NoError(t, err)
	require.True(t, won)

	res, err := fx.srv.StartAgentExecRun(ctx, execOwnerCommandRun, fx.launched.ID, fx.launched.RunID, agentcontrol.ExecStartRequest{
		RunID:          run.ID,
		Argv:           commandRunArgv("echo recovered-by-sweeper"),
		TimeoutS:       30,
		NotAfterUnixMS: now.Add(1 * time.Minute).UnixMilli(),
		Capture:        agentcontrol.ExecCaptureTail,
		Workdir:        "~",
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusAccepted, res.Status)

	// Wait briefly for echo to finish into its tombstone.
	time.Sleep(150 * time.Millisecond)

	// Create a second live run that is owned by this replica's tracker -> must
	// be excluded by sweepCommandRuns.
	_, ownedRun := fx.createApprovedRun(t, "sweeper-c-owned", "echo owned", "", 30)
	wonOwned, err := fx.store.TransitionCommandRunToDispatching(ctx, &store.CommandRunDispatchInput{
		RunID:        ownedRun.ID,
		LaunchID:     fx.launched.RunID,
		NotAfter:     now.Add(1 * time.Minute),
		LeaseUntil:   now.Add(5 * time.Minute),
		DispatchedAt: now,
	})
	require.NoError(t, err)
	require.True(t, wonOwned)
	acq, _ := fx.srv.commandRuns.acquire(ownedRun.ID)
	require.True(t, acq)
	defer fx.srv.commandRuns.release(ownedRun.ID)

	startsBefore := fx.capture.count(execRoutePath(agentcontrol.RouteExecStart))
	fx.srv.sweepCommandRuns(ctx)

	// Unowned run was claimed and polled to succeeded; no new exec-start sent.
	got, err := fx.store.GetCommandRun(ctx, run.ID)
	require.NoError(t, err)
	assert.Equal(t, store.CommandRunStatusSucceeded, got.Status)
	require.NotNil(t, got.ExitCode)
	assert.Equal(t, 0, *got.ExitCode)
	assert.Equal(t, startsBefore, fx.capture.count(execRoutePath(agentcontrol.RouteExecStart)), "sweeper must never send exec-start")

	// Owned run was excluded and never polled or claimed.
	gotOwned, err := fx.store.GetCommandRun(ctx, ownedRun.ID)
	require.NoError(t, err)
	assert.Nil(t, gotOwned.PollClaimedAt, "owned run must be excluded from sweeper claim")
	assert.Nil(t, gotOwned.FinishedAt)

	// Multi-replica claim test: two replicas sharing the same store sweep
	// concurrently over unowned in-flight runs; poll_claimed_at prevents double
	// polling within claimTTL.
	var statusPolls atomic.Int32
	cap2 := &execCapture{
		answer: func(req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
			if req.GetPath() == execRoutePath(agentcontrol.RouteExecStatus) {
				statusPolls.Add(1)
				return &conduitv1.RpcResponse{Status: http.StatusOK, Body: []byte(`{"state":"running"}`)}
			}
			return nil
		},
	}
	fx2 := newCmdRunExecFixture(t, cap2)
	for i := 0; i < 4; i++ {
		_, r := fx2.createApprovedRun(t, "multi-"+api.NewUUID()[:8], "sleep 60", "", 60)
		w, terr := fx2.store.TransitionCommandRunToDispatching(ctx, &store.CommandRunDispatchInput{
			RunID:        r.ID,
			LaunchID:     fx2.launched.RunID,
			NotAfter:     now.Add(1 * time.Minute),
			LeaseUntil:   now.Add(5 * time.Minute),
			DispatchedAt: now,
		})
		require.NoError(t, terr)
		require.True(t, w)
	}

	// Simulate two replicas sharing the same store & conduit router.
	replica2 := &Server{
		store:       fx2.store,
		auditLogger: fx2.audit,
	}
	replica2.conduit.Store(fx2.srv.conduit.Load())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); fx2.srv.sweepCommandRuns(ctx) }()
	go func() { defer wg.Done(); replica2.sweepCommandRuns(ctx) }()
	wg.Wait()

	assert.Equal(t, int32(4), statusPolls.Load(), "each of the 4 unowned runs must be polled exactly once across both replicas within claimTTL")
}

// TestCommandRunP2_Q2_ResumeDispatchesCurrentExperiments verifies design §12 Q2:
// resuming a suspended execution container via HTTPAgentDispatcher.DispatchAgentStart(..., resume=true)
// re-reads dispatchExperiments() and sends the current SCION_HUB_EXPERIMENTS
// (including hub.command_requests) in the broker start payload.
func TestCommandRunP2_Q2_ResumeDispatchesCurrentExperiments(t *testing.T) {
	var capturedEnv map[string]string
	var capturedResume bool
	brokerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && filepath.Base(r.URL.Path) == "start" {
			var body struct {
				Resume      bool              `json:"resume"`
				ResolvedEnv map[string]string `json:"resolvedEnv"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			capturedResume = body.Resume
			capturedEnv = body.ResolvedEnv
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"agent":{"id":"ag-q2","status":"running","phase":"running"}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer brokerSrv.Close()

	fx := newCmdRunP2Fixture(t, nil, nil)
	ctx := context.Background()

	// Register a broker pointing at brokerSrv and bind the suspended agent to it.
	broker := &store.RuntimeBroker{
		ID:       api.NewUUID(),
		Name:     "q2-broker",
		Slug:     "q2-broker",
		Endpoint: brokerSrv.URL,
		Status:   store.BrokerStatusOnline,
	}
	require.NoError(t, fx.store.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, fx.store.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  fx.launched.ProjectID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		LocalPath:  "/tmp/q2",
		Status:     store.BrokerStatusOnline,
	}))
	ag, err := fx.store.GetAgent(ctx, fx.launched.ID)
	require.NoError(t, err)
	ag.RuntimeBrokerID = broker.ID
	require.NoError(t, fx.store.UpdateAgent(ctx, ag))

	disp := NewHTTPAgentDispatcher(fx.store, false, nil)
	disp.SetDispatchExperimentsProvider(fx.srv.dispatchExperiments)

	require.NoError(t, disp.DispatchAgentStart(ctx, ag, "", true))
	assert.True(t, capturedResume, "resume flag must be sent to broker")
	assert.Contains(t, capturedEnv[envHubExperiments], experiments.CommandRequests,
		"Q2: resuming a suspended container must dispatch current SCION_HUB_EXPERIMENTS including hub.command_requests")
}

// TestCommandRunP2_R1_MultiReplicaRunningNeverRecordedNotStarted is the
// regression test for Review 1 R1: when another replica's sweeper (c) wins the
// dispatching->running transition in the DB and a 404 later arrives from
// exec-status after not_after + grace, the owner must record
// unknown/agent_restarted (output_status=lost), never not_started/dispatch_failed.
func TestCommandRunP2_R1_MultiReplicaRunningNeverRecordedNotStarted(t *testing.T) {
	t.Run("poll_stale_dispatching_snapshot_after_peer_moved_to_running", func(t *testing.T) {
		cap := &execCapture{
			answer: func(req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
				if req.GetPath() == execRoutePath(agentcontrol.RouteExecStatus) {
					return &conduitv1.RpcResponse{Status: http.StatusNotFound, Body: []byte(`{"state":"unknown"}`)}
				}
				return nil
			},
		}
		fx := newCmdRunExecFixture(t, cap)
		ctx := context.Background()
		req, run := fx.createApprovedRun(t, "r1-poll-stale", "echo ran", "", 60)

		pastNotAfter := time.Now().UTC().Add(-5 * time.Minute)
		won, err := fx.store.TransitionCommandRunToDispatching(ctx, &store.CommandRunDispatchInput{
			RunID:        run.ID,
			LaunchID:     fx.launched.RunID,
			NotAfter:     pastNotAfter,
			LeaseUntil:   time.Now().UTC().Add(5 * time.Minute),
			DispatchedAt: pastNotAfter.Add(-time.Minute),
		})
		require.NoError(t, err)
		require.True(t, won)

		// Snapshot the owner's stale in-memory copy while still dispatching.
		staleOwnerCopy, err := fx.store.GetCommandRun(ctx, run.ID)
		require.NoError(t, err)
		require.Equal(t, store.CommandRunStatusDispatching, staleOwnerCopy.Status)

		// Peer replica's sweeper (c) moves the DB row to running.
		wonRun, err := fx.store.TransitionCommandRunToRunning(ctx, run.ID)
		require.NoError(t, err)
		require.True(t, wonRun)

		// Owner polls with stale dispatching copy and receives 404 after not_after+grace.
		done := fx.srv.pollCommandRunOnce(ctx, req, staleOwnerCopy, false)
		require.True(t, done)

		fin, err := fx.store.GetCommandRun(ctx, run.ID)
		require.NoError(t, err)
		assert.Equal(t, store.CommandRunStatusUnknown, fin.Status, "run that ran must never be recorded not_started")
		assert.Equal(t, "agent_restarted", fin.Reason)
		assert.Equal(t, agentcontrol.ExecOutputLost, fin.OutputStatus)
	})

	t.Run("executor_202_loses_running_cas_then_404_after_grace", func(t *testing.T) {
		scheduledExecSeamMu.Lock()
		origNotAfter, origGrace := commandRunNotAfter, scheduledExecStartAbsentGrace
		commandRunNotAfter = 1 * time.Millisecond
		scheduledExecStartAbsentGrace = 1 * time.Millisecond
		scheduledExecSeamMu.Unlock()
		t.Cleanup(func() {
			scheduledExecSeamMu.Lock()
			commandRunNotAfter, scheduledExecStartAbsentGrace = origNotAfter, origGrace
			scheduledExecSeamMu.Unlock()
		})

		var fx *cmdRunExecFixture
		var runID string
		cap := &execCapture{
			answer: func(req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
				switch req.GetPath() {
				case execRoutePath(agentcontrol.RouteExecStart):
					// Peer replica's sweeper wins dispatching->running before owner processes 202.
					_, _ = fx.store.TransitionCommandRunToRunning(context.Background(), runID)
					return &conduitv1.RpcResponse{Status: http.StatusAccepted, Body: []byte(`{"state":"running"}`)}
				case execRoutePath(agentcontrol.RouteExecStatus):
					time.Sleep(15 * time.Millisecond)
					return &conduitv1.RpcResponse{Status: http.StatusNotFound, Body: []byte(`{"state":"unknown"}`)}
				}
				return nil
			},
		}
		fx = newCmdRunExecFixture(t, cap)
		req, run := fx.createApprovedRun(t, "r1-exec-202", "echo ran", "", 60)
		runID = run.ID

		fx.srv.EnqueueCommandRun(req, run)
		fin := fx.waitRunFinished(t, run.ID)
		assert.Equal(t, store.CommandRunStatusUnknown, fin.Status, "executor 202 path losing running CAS must still record unknown on 404")
		assert.Equal(t, "agent_restarted", fin.Reason)
		assert.Equal(t, agentcontrol.ExecOutputLost, fin.OutputStatus)
	})
}

// TestCommandRunP2_OptionalGuards pins the defence-in-depth guards raised in
// Review 1: session-incarnation pin between step 3 and step 4, D5
// suspended-snapshot vs stopped-row race before startAgentCore, and
// canWake=false for managed runtimes.
func TestCommandRunP2_OptionalGuards(t *testing.T) {
	t.Run("incarnation_rotated_between_readiness_and_reresolve", func(t *testing.T) {
		fx := newCmdRunExecFixture(t, nil)
		ctx := context.Background()

		origHook := commandRunExecHook
		t.Cleanup(func() { commandRunExecHook = origHook })
		commandRunExecHook = func(stage, _ string) {
			if stage == commandRunStageReady {
				_, _ = fx.store.SetAgentRunID(ctx, fx.launched.ID, api.NewUUID(), nil)
			}
		}

		req, run := fx.createApprovedRun(t, "opt-incarnation-pin", "echo nope", "", 30)
		fx.srv.EnqueueCommandRun(req, run)
		fin := fx.waitRunFinished(t, run.ID)

		assert.Equal(t, store.CommandRunStatusNotStarted, fin.Status)
		assert.Equal(t, "container_changed", fin.Reason)
		assert.Nil(t, fin.DispatchedAt, "must refuse in step 4 before dispatching commits")
		assert.Zero(t, fx.capture.count(execRoutePath(agentcontrol.RouteExecStart)))
	})

	t.Run("suspended_snapshot_stopped_before_startAgentCore_d5", func(t *testing.T) {
		disp := &cmdRunP2Dispatcher{}
		fx := newCmdRunP2Fixture(t, disp, nil)
		ctx := context.Background()

		origHook := commandRunExecHook
		t.Cleanup(func() { commandRunExecHook = origHook })
		commandRunExecHook = func(stage, _ string) {
			if stage == commandRunStageWaking {
				ag, err := fx.store.GetAgent(ctx, fx.launched.ID)
				if err == nil && ag != nil {
					ag.Phase = string(state.PhaseStopped)
					_ = fx.store.UpdateAgent(ctx, ag)
				}
			}
		}

		req, run := fx.createApprovedRun(t, "opt-d5-stopped-race", "echo nope", "", 30)
		fx.srv.EnqueueCommandRun(req, run)
		fin := fx.waitRunFinished(t, run.ID)

		assert.Equal(t, store.CommandRunStatusNotStarted, fin.Status)
		assert.Equal(t, "container_stopped", fin.Reason)
		assert.Zero(t, disp.startCount(), "D5: stopped container must never be started")
		assert.Zero(t, fx.capture.count(execRoutePath(agentcontrol.RouteExecStart)))
	})

	t.Run("canWake_false_for_managed_runtime", func(t *testing.T) {
		disp := &cmdRunP2Dispatcher{}
		fx := newCmdRunP2Fixture(t, disp, nil)
		ctx := context.Background()

		ag, err := fx.store.GetAgent(ctx, fx.launched.ID)
		require.NoError(t, err)
		ag.Runtime = ManagedRuntimePrefix + "test"
		require.NoError(t, fx.store.UpdateAgent(ctx, ag))

		reqRow := fx.createPendingRequest(t, "opt-managed-canwake", "echo nope")
		getReq := httptest.NewRequest(http.MethodGet, "/api/v1/command-requests/"+reqRow.ID, nil)
		getReq = getReq.WithContext(contextWithIdentity(getReq.Context(), fx.owner))
		getRec := httptest.NewRecorder()
		fx.srv.handleCommandRequestRoutes(getRec, getReq)
		require.Equal(t, http.StatusOK, getRec.Code, "body: %s", getRec.Body.String())

		var detailResp messages.CommandRequestDetailResponse
		require.NoError(t, json.Unmarshal(getRec.Body.Bytes(), &detailResp))
		require.NotNil(t, detailResp.ExecContainer)
		assert.Equal(t, string(ExecContainerStateSuspended), detailResp.ExecContainer.State)
		assert.False(t, detailResp.ExecContainer.CanWake, "canWake must be false for managed runtimes")
	})
}
