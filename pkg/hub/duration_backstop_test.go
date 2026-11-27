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
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDurationBackstopExperiment_DispatchAndEffectiveConfigEcho(t *testing.T) {
	off := dispatchExperimentsServer(t, "")
	assert.False(t, slices.Contains(off.dispatchExperiments(), experiments.AgentPolicyDurationBackstop),
		"agent_policy.duration_backstop must be off by default")

	on := dispatchExperimentsServer(t, `{"overrides":{"agent_policy.duration_backstop":true}}`)
	assert.True(t, slices.Contains(on.dispatchExperiments(), experiments.AgentPolicyDurationBackstop),
		"agent_policy.duration_backstop must propagate in dispatchExperiments when enabled")

	deadline := "2026-11-26T20:00:00Z"
	eff, ok := brokerEffectiveConfigOf(&RemoteAgentInfo{
		RunID:        "run-1",
		Harness:      "claude",
		Model:        "claude-sonnet",
		MaxDuration:  "30m",
		RunDeadline:  deadline,
		BrokerBuild:  "dev",
		TemplateHash: "abc123",
	}, "broker-1", nil)
	require.True(t, ok)
	assert.Equal(t, "30m", eff.MaxDuration)
	assert.Equal(t, deadline, eff.RunDeadline)

	other := eff
	other.RunDeadline = "2026-11-26T21:00:00Z"
	assert.False(t, eff.SameEcho(other), "SameEcho must compare RunDeadline")
}

func TestDurationBackstopHeartbeat_RecordsLimitsExceededWithoutDuplicateEvent(t *testing.T) {
	srv, s, brokerID, projectID, agentSlug := setupHeartbeatExitCodeTest(t)

	agent := getAgentState(t, s, agentSlug, projectID)
	hb := brokerAgentHeartbeat{
		Slug:       agentSlug,
		AgentID:    agent.ID,
		RunID:      agent.RunID,
		Status:     string(state.ActivityLimitsExceeded),
		Phase:      string(state.PhaseStopped),
		Activity:   string(state.ActivityLimitsExceeded),
		ExitReason: string(state.ExitReasonLimitsExceeded),
		Message:    api.ReasonMaxDurationBackstop,
	}

	code := sendHeartbeat(t, srv, brokerID, projectID, hb)
	require.Equal(t, http.StatusOK, code)

	got := getAgentState(t, s, agentSlug, projectID)
	assert.Equal(t, string(state.PhaseStopped), got.Phase)
	assert.Equal(t, string(state.ActivityLimitsExceeded), got.Activity)
	assert.Equal(t, string(state.ExitReasonLimitsExceeded), got.ExitReason)
	assert.Equal(t, api.ReasonMaxDurationBackstop, got.Message)

	// Re-posting the same backstop heartbeat (or racing after sciontool already
	// reported limits_exceeded) must not regress or visibly change state (so no
	// duplicate status event is published).
	time.Sleep(10 * time.Millisecond)
	code = sendHeartbeat(t, srv, brokerID, projectID, hb)
	require.Equal(t, http.StatusOK, code)

	got2 := getAgentState(t, s, agentSlug, projectID)
	assert.Equal(t, string(state.PhaseStopped), got2.Phase)
	assert.Equal(t, string(state.ActivityLimitsExceeded), got2.Activity)
	assert.Equal(t, string(state.ExitReasonLimitsExceeded), got2.ExitReason)
	assert.Equal(t, api.ReasonMaxDurationBackstop, got2.Message)
	assert.False(t, agentStatusVisiblyChanged(got, got2), "duplicate backstop heartbeat must not produce a visible status change")
}
