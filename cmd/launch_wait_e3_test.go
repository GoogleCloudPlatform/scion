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

package cmd

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests of the ended-launch wait rule's limits (design wave 3 D-g, D-T8;
// ptone/scion#4670). They guard behaviour that is new or already on main, so
// they cannot fail on the code before the change; each is proven by its
// mutant instead.

// TestWaitForAgentLaunch_PriorEndedLaunchKeepsWaiting: the agent's earlier,
// already ended launch (PriorLaunchID) does not end the wait while the new
// start is still going; the wait follows it to running.
func TestWaitForAgentLaunch_PriorEndedLaunchKeepsWaiting(t *testing.T) {
	shortenLaunchWaitTimings(t)
	old := &hubclient.AgentLaunch{ID: "l-old", State: "ended", Kind: "start", Error: "crash_loop", EndReason: "failed"}
	seq := &agentSequence{results: []func() (*hubclient.Agent, error){
		agentResult(&hubclient.Agent{Phase: "starting", Launch: old}),
		agentResult(&hubclient.Agent{Phase: "starting", Launch: old}),
		agentResult(&hubclient.Agent{ID: "id-1", Phase: "running"}),
	}}
	a, err := waitForAgentLaunch(context.Background(), launchWaitOptions{
		AgentName: "a1", Get: seq.get, PollInterval: time.Millisecond, Timeout: 3 * time.Second, PriorLaunchID: "l-old",
	})
	require.NoError(t, err, "the earlier launch's error does not end the wait")
	require.NotNil(t, a)
	assert.Equal(t, "id-1", a.ID)
	assert.Equal(t, 3, seq.calls)
}

// TestLaunchPhaseOutcome_ReleasedCLIEndsForEveryStartFailurePhase (D-T8):
// the phase-only rule, which is all released CLIs apply, ends the wait for
// every phase a failed start ends in (stopped, error; halted), and keeps
// waiting in the pre-running phases.
func TestLaunchPhaseOutcome_ReleasedCLIEndsForEveryStartFailurePhase(t *testing.T) {
	failed := &hubclient.AgentLaunch{ID: "l2", State: "ended", Kind: "start", Error: "crash_loop", EndReason: "failed"}
	for _, phase := range []string{"stopped", "error", "halted"} {
		done, err := launchPhaseOutcome("a1", &hubclient.Agent{Phase: phase, Launch: failed})
		assert.True(t, done, "phase %s ends a released CLI's wait", phase)
		assert.Error(t, err, "phase %s", phase)
	}
	for _, phase := range []string{"suspended", "starting", "provisioning", "created"} {
		done, err := launchPhaseOutcome("a1", &hubclient.Agent{Phase: phase, Launch: failed})
		assert.False(t, done, "phase %s does not end the phase-only wait", phase)
		assert.NoError(t, err, "phase %s", phase)
	}
	done, err := launchPhaseOutcome("a1", &hubclient.Agent{Phase: "running"})
	assert.True(t, done)
	assert.NoError(t, err)
}

// TestStartAgentViaHub_AttachWaitIgnoresPriorEndedLaunch: a synchronous
// answer that still shows the agent's earlier failed launch (the new start
// reports no launch) does not fail the --attach wait; it waits for running
// and goes on to attach.
func TestStartAgentViaHub_AttachWaitIgnoresPriorEndedLaunch(t *testing.T) {
	clearAppTokenSources(t)
	orig := resolveAttachTransportFn
	resolveAttachTransportFn = func() (transportauth.TokenSource, transportauth.HeaderMode, error) {
		return nil, transportauth.HeaderAuthorization, nil
	}
	t.Cleanup(func() { resolveAttachTransportFn = orig })

	old := &hubclient.AgentLaunch{ID: "l-old", State: "ended", Kind: "start", Error: "crash_loop", EndReason: "failed"}
	hub := &launchMockHub{t: t, createStatus: http.StatusOK, createBody: hubclient.CreateAgentResponse{
		Agent: &hubclient.Agent{ID: "id-1", Slug: "a1", Phase: "starting", Launch: old},
	}, afterCreate: []interface{}{
		hubclient.Agent{ID: "id-1", Slug: "a1", Phase: "starting", Launch: old},
		hubclient.Agent{ID: "id-1", Slug: "a1", Phase: "running", RuntimeBrokerID: mockAttachBrokerID, Launch: old},
	}}
	hubCtx := setupLaunchStartTest(t, hub)
	attach = true
	var err error
	_ = captureStderr(t, func() {
		_ = captureStdout(t, func() { err = startAgentViaHub(nil, hubCtx, "a1", "", false, nil) })
	})
	// The attach step itself stops at the token gate in this test.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no access token found for Hub", "the wait reached running and went on to attach")
	assert.Equal(t, 2, hub.getsAfterCR)
}
