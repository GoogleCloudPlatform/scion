//go:build !no_sqlite

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

package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// prepareForceMoveWorker is prepareMoveWorkerFixture for a forced move,
// with the dispatcher modelling each broker's files for the agent.
func prepareForceMoveWorker(t *testing.T, f *moveFixture) {
	t.Helper()
	prepareMoveWorkerFixture(t, f)
	f.disp.mu.Lock()
	f.disp.recreateFiles = map[string]bool{}
	f.disp.mu.Unlock()
}

// startForceMove sends a real forced move as the agent itself and waits
// for the worker.
func startForceMove(t *testing.T, f *moveFixture) (*store.AgentReincarnation, *store.Agent) {
	t.Helper()
	rec := f.reincarnate(t, ReincarnateAgentRequest{Handoff: "moving on", TargetBroker: f.dst.ID, ForceMove: true})
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	var resp ReincarnateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, MoveTransferModeRecreate, resp.TransferMode, "the 202 names the transfer mode")
	r := waitForReincarnationSettled(t, f.s, f.agent.ID)
	a, err := f.s.GetAgent(context.Background(), f.agent.ID)
	require.NoError(t, err)
	return r, a
}

// assertForceRolledBack checks a failed forced move left the agent stopped
// on the source exactly as before (broker, runtime, run, placement, applied
// config, reservation), removed the target's files with the recreated-files
// delete (run-scoped only with targetRun), and sent the source nothing
// after the stop: no delete of any kind.
func assertForceRolledBack(t *testing.T, f *moveFixture, r *store.AgentReincarnation, a *store.Agent, targetRun string) {
	t.Helper()
	require.Equal(t, store.AgentReincarnationStateFailed, r.State)
	r = waitForMoveSourceCleanup(t, f.s, r.ID)
	assert.Equal(t, store.TransferModeRecreate, r.TransferMode)
	assert.Equal(t, f.src.ID, a.RuntimeBrokerID, "the agent stays on the source")
	assert.Equal(t, "kubernetes", a.Runtime, "the source runtime is restored")
	assert.Equal(t, api.WorkspacePlacementLocal, a.WorkspacePlacement, "the source placement is restored")
	assert.Equal(t, 1, a.Generation)
	require.NotNil(t, a.AppliedConfig)
	assert.Equal(t, "old-image:v1", a.AppliedConfig.Image, "the previous applied config is restored")
	assert.Equal(t, "run-src", a.RunID, "the source's run is restored")
	assert.EqualValues(t, 1, brokerReservationCount(t, f.s, f.src.ID), "the reservation is back on the source")
	assert.EqualValues(t, 0, brokerReservationCount(t, f.s, f.dst.ID))
	assert.Equal(t, sourceCleanupSkippedInterrupted, r.SourceCleanup)

	assert.Equal(t, []string{f.dst.ID + "|" + f.project.ID + "|" + targetRun}, f.disp.recreatedDeletes(),
		"the target's files are removed with one project-scoped files delete")
	_, localDeletes, _ := f.disp.moveSnapshot()
	assert.Empty(t, localDeletes, "no localOnly delete: never on the source, and not the export variant on the target")
	f.disp.mu.Lock()
	assert.Equal(t, 1, f.disp.stopCalls, "the source only sees the stop")
	assert.False(t, f.disp.recreateFiles[f.dst.ID], "the target has no files for the agent after the rollback")
	f.disp.mu.Unlock()
}

// A completed forced move: the target is asked to confirm it has nothing
// for the agent and provisions it fresh; the record and the 202 say
// recreate; the new generation's first task carries the recreated-workspace
// sentence and the handoff; the source's local state is removed with the
// localOnly delete; the reservation follows the agent.
func TestReincarnateForceMove_CompletesOnTarget(t *testing.T) {
	f := setupForceMoveFixture(t, true, nil)
	prepareForceMoveWorker(t, f)

	r, a := startForceMove(t, f)
	require.Equal(t, store.AgentReincarnationStateCompleted, r.State, r.Error)
	r = waitForMoveSourceCleanup(t, f.s, r.ID)
	assert.Equal(t, store.TransferModeRecreate, r.TransferMode)
	assert.Equal(t, store.SourceCleanupDone, r.SourceCleanup)
	assert.Equal(t, f.dst.ID, a.RuntimeBrokerID)
	assert.Equal(t, 2, a.Generation)

	provisions, localDeletes, starts := f.disp.moveSnapshot()
	assert.Equal(t, []string{f.dst.ID + "|fresh-agent-dir"}, provisions)
	assert.Equal(t, []string{f.dst.ID}, starts)
	assert.Equal(t, []string{f.src.ID + "|run-src"}, localDeletes, "only the source's run is cleaned up, localOnly")
	assert.Empty(t, f.disp.recreatedDeletes(), "a completed move removes nothing on the target")
	f.disp.mu.Lock()
	assert.Equal(t, []string{r.ID}, f.disp.moveProvisionIDs, "the provision carries the move's reincarnation ID")
	f.disp.mu.Unlock()
	f.disp.mu.Lock()
	task := f.disp.lastStartTask
	f.disp.mu.Unlock()
	assert.Contains(t, task, "This generation runs on broker move-dst with a recreated workspace: a fresh clone of example.com/force-move-repo")
	assert.Contains(t, task, "moving on", "the handoff reaches the new generation")
	assert.EqualValues(t, 0, brokerReservationCount(t, f.s, f.src.ID))
	assert.EqualValues(t, 1, brokerReservationCount(t, f.s, f.dst.ID))
}

// An export move's record says export, and its first task has no
// recreated-workspace sentence.
func TestReincarnateMove_ExportRecordsTransferMode(t *testing.T) {
	f := setupMoveFixture(t, true, nil)
	prepareMoveWorkerFixture(t, f)
	r, _ := startMove(t, f)
	require.Equal(t, store.AgentReincarnationStateCompleted, r.State, r.Error)
	assert.Equal(t, store.TransferModeExport, r.TransferMode)
	f.disp.mu.Lock()
	assert.NotContains(t, f.disp.lastStartTask, "This generation runs on broker")
	f.disp.mu.Unlock()
	assert.Empty(t, f.disp.recreatedDeletes())
}

// The target already has files for the agent: its 409
// (move_target_not_fresh, fixed text, no path) fails the provision, the
// move rolls back without removing anything on the target (the files there
// predate the move), and the record's error carries the broker's text with
// no path in it.
func TestReincarnateForceMove_TargetHasFilesRefused(t *testing.T) {
	f := setupForceMoveFixture(t, true, nil)
	prepareForceMoveWorker(t, f)
	f.disp.mu.Lock()
	f.disp.recreateFiles[f.dst.ID] = true
	f.disp.mu.Unlock()

	r, a := startForceMove(t, f)
	require.Equal(t, store.AgentReincarnationStateFailed, r.State)
	assert.Contains(t, r.Error, "the agent already has files on this broker; refusing to recreate over them")
	for _, leak := range []string{"/srv", "/export", "agents/"} {
		assert.NotContains(t, r.Error, leak, "the record's error must not name a broker path")
	}
	assert.Equal(t, f.src.ID, a.RuntimeBrokerID)
	assert.Equal(t, "run-src", a.RunID)
	_, _, starts := f.disp.moveSnapshot()
	assert.Empty(t, starts, "the agent is not started anywhere")
	_, localDeletes, _ := f.disp.moveSnapshot()
	assert.Empty(t, localDeletes, "the source is never touched after the stop")
	assert.Empty(t, f.disp.recreatedDeletes(), "the target's pre-existing files are not the move's to remove")
	f.disp.mu.Lock()
	assert.True(t, f.disp.recreateFiles[f.dst.ID], "the pre-existing files on the target are left in place")
	f.disp.mu.Unlock()
	assert.EqualValues(t, 1, brokerReservationCount(t, f.s, f.src.ID), "the reservation is back on the source")
}

// moveTargetUntouched recognises the refusals that leave the target
// untouched, and nothing else.
func TestMoveTargetUntouched(t *testing.T) {
	conflict := &brokerStatusError{StatusCode: http.StatusConflict, Body: `{"error":{"code":"conflict","message":"x"}}`}
	notFresh := &brokerStatusError{StatusCode: http.StatusConflict, Body: freshAgentDirConflictBody}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		// Not marked sent: returned before the request left the hub.
		{"capability refusal", errBrokerLacksAgentMove, true},
		{"wrapped capability refusal", fmt.Errorf("provision: %w", errBrokerLacksAgentMoveRecreate), true},
		{"unmarked error", errors.New("identity not mapped on the target profile"), true},
		{"unmarked broker status", conflict, true},
		// Marked sent: the target may have created files, unless it refused.
		{"sent: target refused, not fresh", provisionSent(notFresh), true},
		{"sent: wrapped refusal", fmt.Errorf("provision: %w", provisionSent(notFresh)), true},
		{"sent: other 409 code", provisionSent(conflict), false},
		{"sent: the not-fresh code on a 500", provisionSent(&brokerStatusError{StatusCode: http.StatusInternalServerError, Body: freshAgentDirConflictBody}), false},
		{"sent: transport error", provisionSent(errors.New("dial tcp 10.0.0.9:9800: connection refused")), false},
		{"sent, then wrapped", fmt.Errorf("provision: %w", provisionSent(errors.New("broker returned 500"))), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, moveTargetUntouched(tc.err))
		})
	}
	marked := provisionSent(errNoRuntimeBrokerAssigned)
	assert.Equal(t, errNoRuntimeBrokerAssigned.Error(), marked.Error(), "the mark keeps the text")
	assert.True(t, errors.Is(marked, errNoRuntimeBrokerAssigned), "the mark keeps the chain")
}

// A failure at the target's provision (after it created files, with no run
// recorded there) rolls back: the source is untouched, the target's files
// are removed by name within the project (no run ID), and a retried forced
// move succeeds.
func TestReincarnateForceMove_ProvisionFailureRollsBackAndRetrySucceeds(t *testing.T) {
	f := setupForceMoveFixture(t, true, nil)
	prepareForceMoveWorker(t, f)
	f.disp.mu.Lock()
	f.disp.moveProvisionErr = provisionSent(errors.New("broker returned 500: provision failed"))
	f.disp.mu.Unlock()

	r, a := startForceMove(t, f)
	assertForceRolledBack(t, f, r, a, "")
	assert.Contains(t, r.Error, "provision on the target broker failed")

	f.disp.mu.Lock()
	f.disp.moveProvisionErr = nil
	f.disp.mu.Unlock()
	r, a = startForceMove(t, f)
	require.Equal(t, store.AgentReincarnationStateCompleted, r.State, "the retry after the rollback: %s", r.Error)
	assert.Equal(t, f.dst.ID, a.RuntimeBrokerID)
}

// A start on the target that definitely left no container rolls back,
// removing the target's files with a delete scoped to the run the target
// recorded, and a retried forced move succeeds.
func TestReincarnateForceMove_DefinitiveStartFailureRollsBackAndRetrySucceeds(t *testing.T) {
	f := setupForceMoveFixture(t, true, nil)
	prepareForceMoveWorker(t, f)
	f.disp.mu.Lock()
	f.disp.startErr = fmt.Errorf("clone failed on the target: %w", errStartRequestNotSent)
	f.disp.mu.Unlock()

	r, a := startForceMove(t, f)
	assertForceRolledBack(t, f, r, a, "run-"+f.dst.ID+"-1")
	assert.Contains(t, r.Error, "start failed")

	f.disp.mu.Lock()
	f.disp.startErr = nil
	f.disp.mu.Unlock()
	r, _ = startForceMove(t, f)
	require.Equal(t, store.AgentReincarnationStateCompleted, r.State, "the retry after the rollback: %s", r.Error)
}

// An ambiguous start on the target (a container may exist there) keeps
// the existing rule: no rollback, the source keeps its only copy of the
// local work, the source cleanup is recorded skipped:ambiguous-start, and
// no files delete reaches any broker, so the broker's files-delete guard is
// never invoked on this path.
func TestReincarnateForceMove_AmbiguousStartSkipsCleanupAndNeverDeletes(t *testing.T) {
	f := setupForceMoveFixture(t, true, nil)
	prepareForceMoveWorker(t, f)
	f.disp.mu.Lock()
	f.disp.startErr = errors.New("context deadline exceeded")
	f.disp.mu.Unlock()

	r, a := startForceMove(t, f)
	require.Equal(t, store.AgentReincarnationStateFailed, r.State)
	r = waitForMoveSourceCleanup(t, f.s, r.ID)
	assert.Equal(t, sourceCleanupSkippedAmbiguousStart, r.SourceCleanup)
	assert.Equal(t, store.TransferModeRecreate, r.TransferMode)
	assert.Equal(t, f.dst.ID, a.RuntimeBrokerID, "the agent stays assigned to the target, where a container may run")
	assert.Empty(t, f.disp.recreatedDeletes(), "no files delete on the target")
	_, localDeletes, _ := f.disp.moveSnapshot()
	assert.Empty(t, localDeletes, "no delete on the source")
}

// The worker's re-check runs with the request's force flag and the
// project's workspace facts, and refuses a move whose transfer mode changed
// since it was accepted, in both directions.
func TestRecheckMoveEligibility_ForceAndTransferMode(t *testing.T) {
	mv := func(f *moveFixture, force bool, mode string) *reincarnationMove {
		return &reincarnationMove{SourceBrokerID: f.src.ID, TargetBrokerID: f.dst.ID, ProjectID: f.project.ID,
			SelfMove: true, AgentDirWorkspace: true, Force: force, TransferMode: mode}
	}
	t.Run("planned recreate still recreate", func(t *testing.T) {
		f := setupForceMoveFixture(t, true, nil)
		a, err := f.srv.recheckMoveEligibility(context.Background(), f.agent.ID, mv(f, true, MoveTransferModeRecreate))
		require.NoError(t, err)
		assert.Equal(t, f.agent.ID, a.ID)
	})
	t.Run("planned recreate now export", func(t *testing.T) {
		f := setupMoveFixture(t, true, nil)
		_, err := f.srv.recheckMoveEligibility(context.Background(), f.agent.ID, mv(f, true, MoveTransferModeRecreate))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "transfer mode changed from recreate to export")
	})
	t.Run("planned export now recreate", func(t *testing.T) {
		f := setupForceMoveFixture(t, true, nil)
		_, err := f.srv.recheckMoveEligibility(context.Background(), f.agent.ID, mv(f, true, MoveTransferModeExport))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "transfer mode changed from export to recreate")
	})
	t.Run("project lost its git remote", func(t *testing.T) {
		f := setupForceMoveFixture(t, true, nil)
		ctx := context.Background()
		p, err := f.s.GetProject(ctx, f.project.ID)
		require.NoError(t, err)
		p.GitRemote = ""
		require.NoError(t, f.s.UpdateProject(ctx, p))
		_, err = f.srv.recheckMoveEligibility(ctx, f.agent.ID, mv(f, true, MoveTransferModeRecreate))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "this project has no git remote")
	})
}

// A transfer-mode mismatch at the re-check fails the move from pending,
// before any broker side effect.
func TestReincarnationWorker_TransferModeMismatchFailsBeforeAnySideEffect(t *testing.T) {
	for _, tc := range []struct {
		name    string
		force   bool
		planned string
	}{
		{"planned recreate, now export", false, MoveTransferModeRecreate},
		{"planned export, now recreate", true, MoveTransferModeExport},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var f *moveFixture
			if tc.force {
				f = setupForceMoveFixture(t, true, nil)
			} else {
				f = setupMoveFixture(t, true, nil)
			}
			prepareForceMoveWorker(t, f)
			ctx := context.Background()
			a, err := f.s.GetAgent(ctx, f.agent.ID)
			require.NoError(t, err)
			a.ReincarnationState = store.ReincarnationStatePending
			require.NoError(t, f.s.UpdateAgent(ctx, a))
			rec := &store.AgentReincarnation{
				AgentID: a.ID, FromGeneration: 1, ToGeneration: 2, State: store.AgentReincarnationStatePending,
				PreviousAppliedConfig: a.AppliedConfig, SourceBrokerID: f.src.ID, TargetBrokerID: f.dst.ID, TransferMode: tc.planned,
			}
			require.NoError(t, f.s.CreateAgentReincarnation(ctx, rec))
			fresh := *a.AppliedConfig
			mv := &reincarnationMove{SourceBrokerID: f.src.ID, TargetBrokerID: f.dst.ID, ProjectID: f.project.ID,
				SelfMove: true, AgentDirWorkspace: true, Force: true, TransferMode: tc.planned}

			f.srv.runReincarnationWorker(ctx, a.ID, rec.ID, a.AppliedConfig, &fresh, "h", time.Now(), "", &ReincarnationPlan{}, 2, a.DeletionClaim, mv)

			got, err := f.s.GetAgentReincarnation(ctx, rec.ID)
			require.NoError(t, err)
			assert.Equal(t, store.AgentReincarnationStateFailed, got.State)
			assert.Contains(t, got.Error, "transfer mode changed")
			f.disp.mu.Lock()
			assert.Zero(t, f.disp.stopCalls, "nothing is stopped")
			f.disp.mu.Unlock()
			provisions, localDeletes, starts := f.disp.moveSnapshot()
			assert.Empty(t, provisions)
			assert.Empty(t, localDeletes)
			assert.Empty(t, starts)
			assert.Empty(t, f.disp.recreatedDeletes())
		})
	}
}

// The recreate rollback sends a files delete, the export rollback a
// localOnly one; the files delete names the target's run only when the
// target recorded one.
func TestRollbackMoveTarget_VariantByTransferMode(t *testing.T) {
	ctx := context.Background()
	dst := &store.Agent{ID: "a1", ProjectID: "p1", RuntimeBrokerID: "dst", RunID: "run-src"}
	for _, tc := range []struct {
		name          string
		mode          string
		dstRun        string
		wantRecreated []string
		wantLocal     []string
	}{
		{"export", MoveTransferModeExport, "run-src", nil, []string{"dst|run-src"}},
		{"legacy empty mode is export", "", "run-src", nil, []string{"dst|run-src"}},
		{"recreate, no target run", MoveTransferModeRecreate, "run-src", []string{"dst|p1|"}, nil},
		{"recreate, target run", MoveTransferModeRecreate, "run-dst", []string{"dst|p1|run-dst"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newReincarnateTestDispatcher()
			a := *dst
			a.RunID = tc.dstRun
			require.NoError(t, rollbackMoveTarget(ctx, d, &a, &reincarnationMove{TransferMode: tc.mode}, "run-src", "reincarnation-1"))
			assert.Equal(t, tc.wantRecreated, d.recreatedDeletes())
			_, local, _ := d.moveSnapshot()
			assert.Equal(t, tc.wantLocal, local)
		})
	}
}

// A forced move expects the target to have nothing for the agent; an
// export move keeps its NFS expectation.
func TestReincarnationMove_ExpectedWorkspace(t *testing.T) {
	assert.Equal(t, "fresh-agent-dir", (&reincarnationMove{TransferMode: MoveTransferModeRecreate, AgentDirWorkspace: true}).expectedNFSWorkspace())
	assert.Equal(t, "agent-dir", (&reincarnationMove{TransferMode: MoveTransferModeExport, AgentDirWorkspace: true}).expectedNFSWorkspace())
	assert.Equal(t, "agent-dir", (&reincarnationMove{AgentDirWorkspace: true}).expectedNFSWorkspace())
	assert.Equal(t, "project", (&reincarnationMove{TransferMode: MoveTransferModeExport}).expectedNFSWorkspace())
}

// The recreated-workspace sentence is the only difference from the export
// and same-broker preamble, placed just before the steps; that preamble
// itself is unchanged (buildReincarnationPreamble is the nil case).
func TestBuildReincarnationPreamble_RecreatedWorkspace(t *testing.T) {
	srv, _ := testServer(t)
	agent := &store.Agent{ID: "agent-1", Slug: "mover"}
	start := time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC)
	requester := reincarnationRequesterContext{Handle: "user:requester@example.com", Resolved: true}
	plain := srv.buildReincarnationPreamble(agent, 3, "next: ship it", start, requester, nil)
	assert.Equal(t, plain, srv.buildReincarnationPreambleWith(agent, 3, "next: ship it", start, requester, nil, nil))
	assert.NotContains(t, plain, "This generation runs on broker")

	for _, tc := range []struct {
		name string
		rec  *recreatedWorkspace
		want string
	}{
		{"clone", &recreatedWorkspace{TargetBroker: "b2", GitRemote: "https://github.com/org/repo.git", Branch: "scion/mover"},
			"This generation runs on broker b2 with a recreated workspace: a fresh clone of github.com/org/repo (branch scion/mover if it was pushed). " +
				"Nothing that was uncommitted or unpushed in the previous generation's workspace is here; rely on the handoff below.\n"},
		{"empty", &recreatedWorkspace{TargetBroker: "b2", Empty: true},
			"This generation runs on broker b2 with a new, empty workspace. " +
				"Nothing from the previous generation's workspace is here; rely on the handoff below.\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := srv.buildReincarnationPreambleWith(agent, 3, "next: ship it", start, requester, nil, tc.rec)
			want := strings.Replace(plain, "Before resuming:\n", tc.want+"Before resuming:\n", 1)
			assert.Equal(t, want, got)
		})
	}
}

// failLinkStore fails every provider link once its switch is armed
// (installStoreFault).
type failLinkStore struct {
	store.Store
	fault *storeFaultSwitch
}

func (s *failLinkStore) AddProjectProvider(ctx context.Context, p *store.ProjectProvider) error {
	if !s.fault.Active() {
		return s.Store.AddProjectProvider(ctx, p)
	}
	return errors.New("injected provider link failure")
}

// failAssignStore fails, once its switch is armed (installStoreFault), every
// agent write that assigns the agent to target.
type failAssignStore struct {
	store.Store
	fault  *storeFaultSwitch
	target string
}

func (s *failAssignStore) UpdateAgent(ctx context.Context, a *store.Agent) error {
	if s.fault.Active() && a.RuntimeBrokerID == s.target {
		return errors.New("injected persist failure")
	}
	return s.Store.UpdateAgent(ctx, a)
}

// A forced move that fails after the agent is assigned to the target but
// before the provision is sent (the provider link, or persisting the new
// config) has sent the target nothing: its absence check never ran, so the
// rollback removes nothing there, and files it holds for the agent stay.
// The source is restored and is never sent a delete.
func TestReincarnateForceMove_FailureBeforeProvisionSendsNoTargetDelete(t *testing.T) {
	for _, tc := range []struct {
		name  string
		wrap  func(inner store.Store, fault *storeFaultSwitch, f *moveFixture) store.Store
		cause string
	}{
		{"provider link fails", func(inner store.Store, fault *storeFaultSwitch, _ *moveFixture) store.Store {
			return &failLinkStore{Store: inner, fault: fault}
		}, "failed to link the target broker to the project"},
		{"persisting the new config fails", func(inner store.Store, fault *storeFaultSwitch, f *moveFixture) store.Store {
			return &failAssignStore{Store: inner, fault: fault, target: f.dst.ID}
		}, "failed to persist new applied config"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setupForceMoveFixture(t, false, func(dst *store.RuntimeBroker) { dst.AutoProvide = true })
			// The fixture's setup writes through the raw store only, so the
			// wrapper is installed before anything reads the server's store in
			// the background; it stays transparent until armed.
			_, fault := installStoreFault(t, f.srv, func(inner store.Store, sw *storeFaultSwitch) store.Store {
				return tc.wrap(inner, sw, f)
			})
			prepareForceMoveWorker(t, f)
			f.disp.mu.Lock()
			f.disp.recreateFiles[f.dst.ID] = true
			f.disp.mu.Unlock()
			fault.Arm()

			rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/reincarnate",
				ReincarnateAgentRequest{TargetBroker: f.dst.ID, ForceMove: true})
			require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
			r := waitForReincarnationSettled(t, f.s, f.agent.ID)
			require.Equal(t, store.AgentReincarnationStateFailed, r.State)
			assert.Contains(t, r.Error, tc.cause)
			waitForMoveSourceCleanup(t, f.s, r.ID)
			a, err := f.s.GetAgent(context.Background(), f.agent.ID)
			require.NoError(t, err)
			assert.Equal(t, f.src.ID, a.RuntimeBrokerID, "the agent stays on the source")
			assert.Equal(t, "run-src", a.RunID)
			assert.EqualValues(t, 1, brokerReservationCount(t, f.s, f.src.ID), "the reservation is back on the source")
			assert.EqualValues(t, 0, brokerReservationCount(t, f.s, f.dst.ID))

			provisions, localDeletes, starts := f.disp.moveSnapshot()
			assert.Empty(t, provisions, "the provision was never sent")
			assert.Empty(t, starts)
			assert.Empty(t, localDeletes, "no localOnly delete anywhere")
			assert.Empty(t, f.disp.recreatedDeletes(), "no files delete on a target that was sent nothing")
			f.disp.mu.Lock()
			assert.True(t, f.disp.recreateFiles[f.dst.ID], "the target's files are kept")
			f.disp.mu.Unlock()
		})
	}
}

// The re-check judges the workspace gate again, as the request did: a
// project switched to a mode reincarnate does not support is refused, and
// so is an agent whose workspace is no longer the kind the move was
// accepted for.
func TestRecheckMoveEligibility_WorkspaceGate(t *testing.T) {
	mv := func(f *moveFixture, agentDir bool) *reincarnationMove {
		return &reincarnationMove{SourceBrokerID: f.src.ID, TargetBrokerID: f.dst.ID, ProjectID: f.project.ID,
			SelfMove: true, AgentDirWorkspace: agentDir}
	}
	t.Run("project switched to worktree-per-agent", func(t *testing.T) {
		f := setupMoveFixture(t, true, nil)
		ctx := context.Background()
		p, err := f.s.GetProject(ctx, f.project.ID)
		require.NoError(t, err)
		if p.Labels == nil {
			p.Labels = map[string]string{}
		}
		// Worktree-per-agent needs a git remote (Project.IsWorktreePerAgent).
		p.GitRemote = forceMoveTestRemote
		p.Labels[store.LabelWorkspaceMode] = string(store.SharingModeWorktreePerAgent)
		require.NoError(t, f.s.UpdateProject(ctx, p))
		require.True(t, p.IsWorktreePerAgent())
		_, err = f.srv.recheckMoveEligibility(ctx, f.agent.ID, mv(f, true))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "worktree-per-agent")
	})
	t.Run("workspace kind changed", func(t *testing.T) {
		f := setupMoveFixture(t, true, nil)
		_, err := f.srv.recheckMoveEligibility(context.Background(), f.agent.ID, mv(f, false))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "the agent's workspace mode changed since the move was accepted")
	})
	t.Run("unchanged passes", func(t *testing.T) {
		f := setupMoveFixture(t, true, nil)
		_, err := f.srv.recheckMoveEligibility(context.Background(), f.agent.ID, mv(f, true))
		require.NoError(t, err)
	})
}

// A forced move's provision that fails on the hub before its request is
// sent (an error not marked errProvisionSent: for example the target's GCP
// identity check) removes nothing on the target: the rollback sends no
// delete, and files the target holds for the agent stay. The source is
// restored.
func TestReincarnateForceMove_ProvisionNotSentSendsNoTargetDelete(t *testing.T) {
	f := setupForceMoveFixture(t, true, nil)
	prepareForceMoveWorker(t, f)
	f.disp.mu.Lock()
	f.disp.recreateFiles[f.dst.ID] = true
	f.disp.moveProvisionErr = errors.New("GCP service account not mapped on the target profile")
	f.disp.mu.Unlock()

	r, a := startForceMove(t, f)
	require.Equal(t, store.AgentReincarnationStateFailed, r.State)
	assert.Contains(t, r.Error, "GCP service account not mapped on the target profile")
	waitForMoveSourceCleanup(t, f.s, r.ID)
	assert.Equal(t, f.src.ID, a.RuntimeBrokerID, "the agent stays on the source")
	assert.Equal(t, "run-src", a.RunID)
	assert.EqualValues(t, 1, brokerReservationCount(t, f.s, f.src.ID), "the reservation is back on the source")
	_, localDeletes, starts := f.disp.moveSnapshot()
	assert.Empty(t, starts)
	assert.Empty(t, localDeletes, "no localOnly delete anywhere")
	assert.Empty(t, f.disp.recreatedDeletes(), "no files delete on a target that was sent nothing")
	f.disp.mu.Lock()
	assert.True(t, f.disp.recreateFiles[f.dst.ID], "the target's files are kept")
	f.disp.mu.Unlock()
}

// The target refuses the forced move's provision before its absence check
// runs (here a 503: the NFS export is unavailable), so the error comes back
// sent and the rollback sends the guarded files delete with the move's
// reincarnation ID. The target holds files for the agent that this move did
// not create (no move marker), so it refuses the delete with
// move_marker_mismatch and removes nothing; the rollback records the skip
// and still restores the agent on the source.
func TestReincarnateForceMove_RefusedBeforeF4RollbackDeleteIsSkipped(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		code int
	}{
		{"nfs unavailable", `{"error":{"code":"nfs_unavailable","message":"the NFS export is unavailable"}}`, http.StatusServiceUnavailable},
		{"create in progress", `{"error":{"code":"conflict","message":"a create of this agent is in progress"}}`, http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setupForceMoveFixture(t, true, nil)
			prepareForceMoveWorker(t, f)
			f.disp.mu.Lock()
			f.disp.recreateFiles[f.dst.ID] = true
			f.disp.moveProvisionErr = provisionSent(&brokerStatusError{StatusCode: tc.code, Body: tc.body})
			f.disp.moveProvisionFailsBeforeF4 = true
			f.disp.mu.Unlock()

			r, a := startForceMove(t, f)
			require.Equal(t, store.AgentReincarnationStateFailed, r.State)
			r = waitForMoveSourceCleanup(t, f.s, r.ID)
			assert.Equal(t, f.src.ID, a.RuntimeBrokerID, "the agent stays on the source")
			assert.Equal(t, "run-src", a.RunID)
			assert.EqualValues(t, 1, brokerReservationCount(t, f.s, f.src.ID), "the reservation is back on the source")
			assert.Len(t, f.disp.recreatedDeletes(), 1, "the guarded delete is sent")
			_, localDeletes, _ := f.disp.moveSnapshot()
			assert.Empty(t, localDeletes)
			f.disp.mu.Lock()
			assert.Equal(t, []string{r.ID}, f.disp.recreatedDeleteIDs, "the delete carries the move's reincarnation ID")
			assert.True(t, f.disp.recreateFiles[f.dst.ID], "files the move did not create are kept")
			f.disp.mu.Unlock()
		})
	}
}

// isMoveMarkerMismatch matches only the exact code on a 409.
func TestIsMoveMarkerMismatch(t *testing.T) {
	assert.True(t, isMoveMarkerMismatch(&brokerStatusError{StatusCode: http.StatusConflict, Body: moveMarkerMismatchBody}))
	assert.True(t, isMoveMarkerMismatch(fmt.Errorf("delete: %w", &brokerStatusError{StatusCode: http.StatusConflict, Body: moveMarkerMismatchBody})))
	assert.False(t, isMoveMarkerMismatch(&brokerStatusError{StatusCode: http.StatusConflict, Body: freshAgentDirConflictBody}))
	assert.False(t, isMoveMarkerMismatch(&brokerStatusError{StatusCode: http.StatusInternalServerError, Body: moveMarkerMismatchBody}))
	assert.False(t, isMoveMarkerMismatch(errors.New("move_marker_mismatch")))
	assert.False(t, isMoveMarkerMismatch(nil))
}
