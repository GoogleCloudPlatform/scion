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
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
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
	}, "broker-1", store.EffectiveConfigSourceSync, nil)
	require.True(t, ok)
	assert.Equal(t, "30m", eff.MaxDuration)
	assert.Equal(t, deadline, eff.RunDeadline)

	other := eff
	other.RunDeadline = "2026-11-26T21:00:00Z"
	assert.False(t, eff.SameEcho(other), "SameEcho must compare RunDeadline")
}

func TestDurationBackstopHeartbeat_RecordsLimitsExceededWithoutDuplicateEvent(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   "proj-backstop",
		Name: "Backstop Project",
		Slug: "backstop-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	broker := &store.RuntimeBroker{
		ID:     "broker-backstop",
		Name:   "backstop-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	agent := &store.Agent{
		ID:              "00000000-0000-0000-0000-000000009372",
		Slug:            "backstop-agent",
		Name:            "backstop-agent",
		ProjectID:       project.ID,
		RuntimeBrokerID: broker.ID,
		RunID:           "run-backstop-1",
		Phase:           string(state.PhaseRunning),
		Activity:        string(state.ActivityThinking),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	postBackstopHB := func() {
		body, err := json.Marshal(brokerHeartbeatRequest{
			Status: "online",
			Projects: []brokerProjectHeartbeat{
				{
					ProjectID:  project.ID,
					AgentCount: 1,
					Agents: []brokerAgentHeartbeat{
						{
							Slug:       agent.Slug,
							AgentID:    agent.ID,
							RunID:      agent.RunID,
							Status:     string(state.ActivityLimitsExceeded),
							Phase:      string(state.PhaseStopped),
							Activity:   string(state.ActivityLimitsExceeded),
							ExitReason: string(state.ExitReasonLimitsExceeded),
							Message:    api.ReasonMaxDurationBackstop,
						},
					},
				},
			},
		})
		require.NoError(t, err)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/runtime-brokers/"+broker.ID+"/heartbeat", bytes.NewReader(body))
		req = req.WithContext(contextWithBrokerIdentity(req.Context(), broker.ID))
		w := httptest.NewRecorder()
		srv.handleBrokerHeartbeat(w, req, broker.ID)
		require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	}

	postBackstopHB()

	got, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseStopped), got.Phase)
	assert.Equal(t, string(state.ActivityLimitsExceeded), got.Activity)
	assert.Equal(t, string(state.ExitReasonLimitsExceeded), got.ExitReason)
	assert.Equal(t, api.ReasonMaxDurationBackstop, got.Message)
	firstUpdated := got.Updated

	// Re-posting the same backstop heartbeat (or racing after sciontool already
	// reported limits_exceeded) must not regress or duplicate the state change.
	time.Sleep(10 * time.Millisecond)
	postBackstopHB()

	got2, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseStopped), got2.Phase)
	assert.Equal(t, string(state.ActivityLimitsExceeded), got2.Activity)
	assert.Equal(t, string(state.ExitReasonLimitsExceeded), got2.ExitReason)
	assert.Equal(t, api.ReasonMaxDurationBackstop, got2.Message)
	assert.Equal(t, firstUpdated, got2.Updated, "duplicate backstop heartbeat should not rewrite unchanged terminal state")
}
