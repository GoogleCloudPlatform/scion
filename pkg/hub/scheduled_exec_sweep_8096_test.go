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
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestScheduledExec8096_OneStatusCallPerRunPerSweep (ptone/scion#8096, P4-R1):
// with >= 1 full batch of still-running runs and default sweep budget (1,000),
// a single sweep polls each unclaimed unfinished run at most once rather than
// looping over the same running set up to the budget.
func TestScheduledExec8096_OneStatusCallPerRunPerSweep(t *testing.T) {
	f := newExecRouteFixture(t)
	setExecTiming2(t, &scheduledExecSweepInFlight, 8)
	setExecTiming2(t, &scheduledExecSweepBatch, 100)
	setExecTiming2(t, &scheduledExecSweepMaxPages, 10) // budget = 1,000

	var statusMu sync.Mutex
	callsByRun := map[string]int{}
	f.startControlAgentWithOptions(t, f.public.URL, true, f.opts, func(_ conduit.RPCHandler) conduit.RPCHandler {
		return conduit.RPCHandlerFunc(func(_ context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
			if req.GetPath() == execRoutePath(agentcontrol.RouteExecStatus) {
				var sr agentcontrol.ExecStatusRequest
				if err := json.Unmarshal(req.GetBody(), &sr); err == nil {
					statusMu.Lock()
					callsByRun[sr.RunID]++
					statusMu.Unlock()
				}
				return &conduitv1.RpcResponse{
					Status: 200,
					Body:   []byte(`{"state":"running"}`),
				}
			}
			return &conduitv1.RpcResponse{Status: 404}
		})
	})

	ctx := context.Background()
	const runCount = 20 // > 2 full inFlight batches of 8, well under budget 1,000
	runIDs := make([]string, 0, runCount)
	for i := 0; i < runCount; i++ {
		evt := f.event(t, 60, "sleep", "60")
		_, err := f.store.(store.ScheduledEventClaimer).ClaimScheduledEvent(ctx, evt.ID, store.ScheduledEventFired)
		require.NoError(t, err)
		require.NoError(t, f.store.CreateScheduledExecRun(ctx, &store.ScheduledExecRun{
			EventID:        evt.ID,
			ProjectID:      evt.ProjectID,
			TargetAgentID:  f.launched.ID,
			CommandSHA256:  agentcontrol.ExecArgvDigest([]string{"sleep", "60"}),
			Argc:           2,
			TimeoutSeconds: 60,
			StartedAt:      time.Now().Add(-time.Minute),
			NotAfter:       time.Now().Add(time.Hour),
			LeaseUntil:     time.Now().Add(time.Hour),
			LaunchID:       f.launched.RunID,
			Outcome:        store.ScheduledExecOutcomeRunning,
		}))
		runIDs = append(runIDs, evt.ID)
	}

	f.srv.sweepScheduledExecRuns(ctx)

	statusMu.Lock()
	defer statusMu.Unlock()
	require.Len(t, callsByRun, runCount, "every still-running run must be polled once")
	for _, id := range runIDs {
		assert.Equal(t, 1, callsByRun[id], "run %s must be polled at most once per sweep", id)
	}
}

// TestScheduledExec8096_TwoSlowTargetsWithinQuotaPollsHealthyInOneSweep
// (ptone/scion#8096, P3-R2 residual): 2 slow targets x 20 older runs (within
// the 20/target quota) and 2 in-flight slots must still poll and finish the
// healthy target's newer run within a single sweep.
func TestScheduledExec8096_TwoSlowTargetsWithinQuotaPollsHealthyInOneSweep(t *testing.T) {
	f := newExecRouteFixture(t)
	setExecTiming(t, &scheduledExecPollInitial, time.Hour)
	setExecTiming(t, &scheduledExecCallTimeout, 120*time.Millisecond)
	setExecTiming2(t, &scheduledExecSweepInFlight, 2)

	// Force healthy target's UUID to sort after both slow targets so the
	// first claim of 2 slots picks slowA and slowB.
	ctx := context.Background()
	startSlowTarget := func(id, slug string) *store.Agent {
		slow := *f.launched
		slow.ID = id
		slow.Slug = slug
		slow.Name = slug
		slow.RunID = uuid.NewString()
		require.NoError(t, f.store.CreateAgent(ctx, &slow))
		_, err := f.store.SetAgentRunID(ctx, slow.ID, slow.RunID, nil)
		require.NoError(t, err)
		rf := *f.relayFixture
		rf.launched = &slow
		sf := &ptyConduitFixture{relayFixture: &rf}
		sf.startControlAgentWithOptions(t, f.public.URL, true, f.opts, func(h conduit.RPCHandler) conduit.RPCHandler {
			return conduit.RPCHandlerFunc(func(ctx context.Context, req *conduitv1.RpcRequest) *conduitv1.RpcResponse {
				if req.GetPath() == execRoutePath(agentcontrol.RouteExecStatus) {
					<-ctx.Done()
					return &conduitv1.RpcResponse{Status: 500}
				}
				return h.HandleRPC(ctx, req)
			})
		})
		return &slow
	}

	slowA := startSlowTarget("00000000-0000-4000-8000-0000000000a1", "slow-a-"+uuid.NewString()[:8])
	slowB := startSlowTarget("00000000-0000-4000-8000-0000000000a2", "slow-b-"+uuid.NewString()[:8])
	f.startAgent(t)

	for _, slow := range []*store.Agent{slowA, slowB} {
		for i := 0; i < 20; i++ {
			evt := f.event(t, 60, "true")
			_, err := f.store.(store.ScheduledEventClaimer).ClaimScheduledEvent(ctx, evt.ID, store.ScheduledEventFired)
			require.NoError(t, err)
			require.NoError(t, f.store.CreateScheduledExecRun(ctx, &store.ScheduledExecRun{
				EventID:        evt.ID,
				ProjectID:      evt.ProjectID,
				TargetAgentID:  slow.ID,
				CommandSHA256:  agentcontrol.ExecArgvDigest([]string{"true"}),
				Argc:           1,
				TimeoutSeconds: 60,
				StartedAt:      time.Now().Add(-time.Hour + time.Duration(i)*time.Second),
				NotAfter:       time.Now().Add(time.Hour),
				LeaseUntil:     time.Now().Add(time.Hour),
				LaunchID:       slow.RunID,
				Outcome:        store.ScheduledExecOutcomeRunning,
			}))
		}
	}

	healthy := f.event(t, 10, "true")
	f.fire(healthy)
	f.waitAgentFinished(t, healthy.ID)
	f.srv.execRuns.work.closeAdmission()
	f.srv.stopScheduledExecRuns(ctx)
	f.waitUntracked(t, healthy.ID)

	// Run one bounded sweep: claim 1 takes slowA[0] and slowB[0] (120ms timeout);
	// claim 2 must rotate to f.launched and record healthy's outcome within 1s,
	// rather than taking slowA[1..19] and slowB[1..19] for 20 rounds.
	sweepCtx, cancel := context.WithTimeout(ctx, 800*time.Millisecond)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		f.srv.sweepScheduledExecRuns(sweepCtx)
	}()
	require.Eventually(t, func() bool {
		return f.getRun(t, healthy.ID).FinishedAt != nil
	}, 750*time.Millisecond, 10*time.Millisecond,
		"healthy target must be polled and finished within one sweep despite 2 slow targets x 20 older runs")
	cancel()
	wg.Wait()
	assert.Equal(t, store.ScheduledExecOutcomeOK, f.getRun(t, healthy.ID).Outcome)
}
