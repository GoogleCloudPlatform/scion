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
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	tc "github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetryemit"
)

// TestLifecycleEvents_CreateCleanupPlacementRefusalKeptRow pins the lifecycle
// event of cleanupFailedCreate's placement-refusal branch (#4434): the
// dispatcher refused the failed create's runtime delete because the saved
// placement is inconsistent, so the row is kept in phase error. That write
// carries lifecycleCreateFailed(ctx, rb.Stage), like the run-mismatch branch
// and the row-delete-failed branch, so it is observed as the existing
// launch_failed with end_reason create_failed and error_code = the stage
// (contract ruling on the WP1 #4432 conflict round). The write keeps its
// IfRunID guard: when the row moved to another run before the mark, nothing
// is written and no create_failed event is attributed to that run.
func TestLifecycleEvents_CreateCleanupPlacementRefusalKeptRow(t *testing.T) {
	for _, c := range []struct {
		name    string
		wrapRun bool   // the refusal carries the run the delete named
		moveTo  string // the row moves to this run before the mark
		marked  bool   // the kept row is marked failed (and observed)
	}{
		{name: "run carried", wrapRun: true, marked: true},
		{name: "no run carried", marked: true},
		{name: "row moved to another run", wrapRun: true, moveTo: "run-other"},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv, s := testServer(t)
			ls, ok := s.(lifecycleEventStore)
			require.True(t, ok)
			srv.config.HubIDPinned = true
			sink := telemetryemit.NewMemorySink()
			srv.SetLifecycleEventsSink(sink, telemetryemit.NewMemoryCounters())
			st, err := srv.installLifecycleObserver()
			require.NoError(t, err)
			require.Equal(t, telemetryemit.ObserverStateEnabled, st)

			setBrokerAgentCeiling(t, s, 5)
			agent := setupBrokerAgentInPhase(t, s, "lc-pr-cleanup-"+uuid.NewString()[:8], state.PhaseProvisioning)
			ctx := context.Background()
			broker, err := s.GetRuntimeBroker(ctx, agent.RuntimeBrokerID)
			require.NoError(t, err)
			reserveBrokerSlot(t, s, broker, agent.ID)
			_, err = s.SetAgentRunID(ctx, agent.ID, "run-mine", nil)
			require.NoError(t, err)
			agent = mustGetAgent(t, s, agent.ID)
			before := len(lcEvents(t, ls, sink, agent.ID))

			var refusalErr error = placementConflict(stalePinRefusal(agent, "test: refused by the backstop"))
			if c.wrapRun {
				refusalErr = &createCleanupDeleteError{runID: "run-mine", err: refusalErr}
			}
			corrID := srv.cleanupFailedCreate(ctx, createRollback{
				Agent:           agent,
				RuntimeBrokerID: agent.RuntimeBrokerID,
				Stage:           createStageDispatch,
				Cause:           errors.New("dispatch failed"),
				DeleteRuntime: func(dctx context.Context) error {
					if c.moveTo != "" {
						if _, err := s.SetAgentRunID(dctx, agent.ID, c.moveTo, nil); err != nil {
							return err
						}
					}
					return refusalErr
				},
			})
			assert.Empty(t, corrID)

			got := mustGetAgent(t, s, agent.ID) // the row is kept in every case
			evs := lcEvents(t, ls, sink, agent.ID)
			var createFailed []*tc.LifecycleEvent
			for _, e := range evs[before:] {
				if e.EndReason == tc.EndReasonCreateFailed {
					createFailed = append(createFailed, e)
				}
			}

			if c.marked {
				require.Equal(t, string(state.PhaseError), got.Phase)
				assert.True(t, strings.HasPrefix(got.Message, createCleanupRefusedPrefix), got.Message)
				require.Len(t, createFailed, 1, "exactly one create_failed event: %s", lcSeqFull(evs))
				e := createFailed[0]
				assert.Equal(t, tc.TransitionLaunchFailed, e.Transition, "the existing launch_failed, not a new event")
				assert.Equal(t, createStageDispatch, e.ErrorCode, "error_code is the failed create stage")
				// The event names the failed create's own run: the row keeps
				// run-mine in both marked cases, so launch_id is not omitted.
				assert.Equal(t, "run-mine", e.LaunchID)
				assert.Equal(t, got.LaunchID, e.AsyncLaunchID, "async_launch_id is the row's launch")
				return
			}
			// The row moved on: the guarded write changed nothing, so no
			// create_failed event exists, and none names the other run.
			assert.Equal(t, c.moveTo, got.RunID)
			assert.NotEqual(t, string(state.PhaseError), got.Phase, "the live run was marked failed")
			assert.Empty(t, createFailed, "a create_failed event after the row moved: %s", lcSeqFull(evs))
			for _, e := range evs[before:] {
				if e.LaunchID == c.moveTo {
					assert.NotEqual(t, tc.TransitionLaunchFailed, e.Transition, "a launch_failed names the other run: %s", lcSeqFull(evs))
				}
			}
		})
	}
}
