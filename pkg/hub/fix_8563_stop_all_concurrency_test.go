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
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// boundedStopAllDispatcher holds the first wave of stop dispatches briefly
// once it reaches expectedBound so an unbounded fan-out reliably piles up
// more than expectedBound concurrent calls, while a bounded fan-out never
// exceeds expectedBound.
type boundedStopAllDispatcher struct {
	createAgentDispatcher

	expectedBound int
	holdWindow    time.Duration
	failIDs       map[string]error

	mu          sync.Mutex
	inFlight    int
	maxInFlight int
	totalCalls  int
	gate        chan struct{}
	gateOnce    sync.Once
}

func newBoundedStopAllDispatcher(expectedBound int, failIDs map[string]error) *boundedStopAllDispatcher {
	return &boundedStopAllDispatcher{
		expectedBound: expectedBound,
		holdWindow:    30 * time.Millisecond,
		failIDs:       failIDs,
		gate:          make(chan struct{}),
	}
}

func (d *boundedStopAllDispatcher) DispatchAgentStop(ctx context.Context, agent *store.Agent) error {
	d.mu.Lock()
	d.inFlight++
	d.totalCalls++
	if d.inFlight > d.maxInFlight {
		d.maxInFlight = d.inFlight
	}
	reachedBound := d.totalCalls >= d.expectedBound
	d.mu.Unlock()

	if reachedBound {
		d.gateOnce.Do(func() {
			go func() {
				time.Sleep(d.holdWindow)
				close(d.gate)
			}()
		})
	}

	select {
	case <-d.gate:
	case <-ctx.Done():
	}

	d.mu.Lock()
	d.inFlight--
	d.mu.Unlock()

	if err, ok := d.failIDs[agent.ID]; ok {
		return err
	}
	return ctx.Err()
}

// ptone/scion#8563: handleStopAllAgents must bound parallel broker stop
// dispatches while preserving per-agent results, error reporting, and the
// response shape.
func TestStopAll_BoundedConcurrency(t *testing.T) {
	const bound = stopAllMaxConcurrency
	const totalAgents = bound*2 + 2

	failIDs := map[string]error{}
	disp := newBoundedStopAllDispatcher(bound, failIDs)
	srv, s, project := setupCreateAgentServer(t, disp)
	setAgentQuotaLimits(t, s)

	var allIDs []string
	for i := 0; i < totalAgents; i++ {
		a := createSiteAgent(t, s, project, fmt.Sprintf("bounded-stop-all-%02d", i), state.PhaseRunning, store.RunIntentRunning)
		allIDs = append(allIDs, a.ID)
	}
	// Mark two agents (one in the first wave, one in a later wave) to fail
	// broker stop dispatch so per-agent error reporting is verified too.
	failErr1 := errors.New("broker stop failed for agent 1")
	failErr2 := errors.New("broker stop failed for agent 9")
	failIDs[allIDs[1]] = failErr1
	failIDs[allIDs[9]] = failErr2

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents/stop-all", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	assert.LessOrEqual(t, disp.maxInFlight, bound, "stop-all concurrency must not exceed bound")
	assert.Equal(t, bound, disp.maxInFlight, "stop-all should still run up to bound stops in parallel")
	assert.Equal(t, totalAgents, disp.totalCalls, "every agent must be dispatched")

	var resp StopAllAgentsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "all", resp.Scope)
	assert.Equal(t, totalAgents, resp.Total)
	assert.Equal(t, totalAgents-2, resp.Stopped)
	assert.Equal(t, 2, resp.Failed)
	require.Len(t, resp.Results, totalAgents)

	byID := make(map[string]stopAllResult, len(resp.Results))
	for _, r := range resp.Results {
		byID[r.ID] = r
	}
	for _, id := range allIDs {
		r, ok := byID[id]
		require.True(t, ok, "missing result for agent %s", id)
		if wantErr, shouldFail := failIDs[id]; shouldFail {
			assert.Equal(t, "error", r.Status, "agent %s status", id)
			assert.Equal(t, wantErr.Error(), r.Error, "agent %s error", id)
		} else {
			assert.Equal(t, "stopped", r.Status, "agent %s status", id)
			assert.Empty(t, r.Error, "agent %s error", id)
		}
	}
}
