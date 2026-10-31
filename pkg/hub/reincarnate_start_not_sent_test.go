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
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for ptone/scion#5594: a start the hub refused before sending it
// (its run could not be begun, or the hub is shutting down) left no
// container, so a reincarnation treats it as definitive.

// startNotSentErrs are the hub-side refusals as the dispatcher returns
// them: a run not begun wraps its cause, the shutdown gate's error is
// returned bare, and a pending config fold refused before the dispatch
// wraps its cause (a store error, or the compare-and-set conflicts that
// exhausted its retries). The reincarnation start does not fold pending
// config until ptone/scion#3978, so the fold refusals are injected through
// the test dispatcher (ptone/scion#5864).
func startNotSentErrs() map[string]error {
	return map[string]error{
		"run not begun":                     fmt.Errorf("%w: %w", errStartRunNotBegun, errors.New("store unavailable")),
		"hub shutting down":                 ErrHubShuttingDown,
		"pending fold refused: store error": fmt.Errorf("%w: %w", errPendingConfigFold, errors.New("store unavailable")),
		"pending fold refused: conflicts":   fmt.Errorf("%w: %w", errPendingConfigFold, store.ErrVersionConflict),
	}
}

// The classification: both refusals are definitive, also wrapped again; a
// run not begun because a delete holds the row stays ambiguous, and so
// does a deferred start.
func TestReincarnationStartLeftNoContainer_StartNotSent(t *testing.T) {
	for name, err := range startNotSentErrs() {
		assert.True(t, reincarnationStartLeftNoContainer(err), "%s: want definitive", name)
		assert.True(t, reincarnationStartLeftNoContainer(fmt.Errorf("start failed: %w", err)), "%s, wrapped again: want definitive", name)
	}
	assert.False(t, reincarnationStartLeftNoContainer(fmt.Errorf("%w: %w", errStartRunNotBegun, store.ErrDeleteInProgress)),
		"a run not begun because a delete holds the row stays ambiguous")
	assert.False(t, reincarnationStartLeftNoContainer(fmt.Errorf("start: %w", ErrLifecycleDeferred)), "a deferred start stays ambiguous")
	assert.False(t, reincarnationStartLeftNoContainer(fmt.Errorf("%w: %w", errPendingConfigFold, store.ErrDeleteInProgress)),
		"a pending fold refused because a delete holds the row stays ambiguous")
}

// Both classifiers agree on a refused pending config fold
// (ptone/scion#5864): definitive, except under a delete.
func TestPendingFoldRefusal_ClassifiersAgree(t *testing.T) {
	for _, err := range []error{
		fmt.Errorf("%w: %w", errPendingConfigFold, errors.New("store unavailable")),
		fmt.Errorf("%w: %w", errPendingConfigFold, store.ErrVersionConflict),
		fmt.Errorf("%w: %w", errPendingConfigFold, errPendingReauthIndeterminate),
	} {
		assert.True(t, startDidNotHappen(err), "%v", err)
		assert.True(t, reincarnationStartLeftNoContainer(err), "%v", err)
	}
	deleting := fmt.Errorf("%w: %w", errPendingConfigFold, store.ErrDeleteInProgress)
	assert.False(t, startDidNotHappen(deleting))
	assert.False(t, reincarnationStartLeftNoContainer(deleting))
}

// An export move whose target start the hub refused before sending rolls
// back to the source, as for a definitive start failure.
func TestReincarnateMove_StartNotSentRollsBack(t *testing.T) {
	for name, startErr := range startNotSentErrs() {
		t.Run(name, func(t *testing.T) {
			f := setupMoveFixture(t, true, nil)
			prepareMoveWorkerFixture(t, f)
			f.disp.startErr = startErr

			r, a := startMove(t, f)
			assertRolledBackToSource(t, f, r, a)
			assert.NotEqual(t, sourceCleanupSkippedAmbiguousStart, r.SourceCleanup, "want a rollback, not an ambiguous start")
			assert.Contains(t, r.Error, "start failed")
		})
	}
}

// A forced (recreate) move whose target start the hub refused before
// sending rolls back to the source and removes the target's files, as for
// a definitive start failure. No run was recorded on the target, so the
// files delete resolves the agent by name (no run ID).
func TestReincarnateForceMove_StartNotSentRollsBack(t *testing.T) {
	for name, startErr := range startNotSentErrs() {
		t.Run(name, func(t *testing.T) {
			f := setupForceMoveFixture(t, true, nil)
			prepareForceMoveWorker(t, f)
			f.disp.mu.Lock()
			f.disp.startErr = startErr
			f.disp.mu.Unlock()
			before := seedGenerationOneTask(t, f)

			r, a := startForceMove(t, f)
			assert.NotEqual(t, sourceCleanupSkippedAmbiguousStart, r.SourceCleanup, "want a rollback, not an ambiguous start")
			assertForceRolledBack(t, f, r, a, "", store.AgentReincarnationStateStarting, before.AppliedConfig.Task)
		})
	}
}

// A plain reincarnation whose start the hub refused before sending
// re-renders the previous config.
func TestReincarnateAgent_StartNotSentRerendersPreviousConfig(t *testing.T) {
	for name, startErr := range startNotSentErrs() {
		t.Run(name, func(t *testing.T) {
			disp := newReincarnateTestDispatcher()
			disp.startErr = startErr
			srv, s, project, broker := setupReincarnateTestServer(t, disp)
			agent := newReincarnateTestAgent(t, s, project, broker, nil)
			self := agentIdentityFor(agent.ID, project.ID)

			rec := httptest.NewRecorder()
			srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h"}), agent.ID)
			require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
			r := waitForReincarnationSettled(t, s, agent.ID)
			require.Equal(t, store.AgentReincarnationStateFailed, r.State)

			calls, _ := disp.reprovisionSnapshot()
			assert.Equal(t, 2, calls, "want the previous config re-rendered after the refused start")
			final, err := s.GetAgent(context.Background(), agent.ID)
			require.NoError(t, err)
			assert.Equal(t, 1, final.Generation)
			assert.Equal(t, "old-image:v1", final.AppliedConfig.Image, "the previous config is restored on the row")
		})
	}
}
