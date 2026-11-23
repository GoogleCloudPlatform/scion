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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApplyStartExtras_PriorStatus(t *testing.T) {
	payload := map[string]interface{}{}
	applyStartExtras(payload, StartExtras{})
	assert.NotContains(t, payload, "priorStatus", "nil PriorStatus sends nothing")

	ts := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	ps := &api.PriorStatus{
		Activity: string(state.ActivityCompleted),
		Summary:  "PR #123 opened",
		At:       ts,
	}
	payload = map[string]interface{}{}
	applyStartExtras(payload, StartExtras{PriorStatus: ps})
	got, ok := payload["priorStatus"]
	require.True(t, ok, "priorStatus must be written on the wire")
	assert.Equal(t, ps, got)
}

// TestDispatch_StartAndRestartCarryPriorStatus verifies that DispatchAgentStart
// and DispatchAgentRestart snapshot PriorStatus from the agent record, and that
// DispatchAgentStart skips falling back to AppliedConfig.Task when the prior
// run already completed (ptone/scion#8900).
func TestDispatch_StartAndRestartCarryPriorStatus(t *testing.T) {
	ctx := context.Background()
	ts := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)

	t.Run("stopped completed agent suppresses AppliedConfig.Task fallback", func(t *testing.T) {
		d, client, ag := autoExposeDispatchFixture(t)
		ag.Phase = string(state.PhaseStopped)
		ag.Activity = string(state.ActivityCompleted)
		ag.TaskSummary = "finished review"
		ag.LastActivityEvent = ts
		ag.AppliedConfig = &store.AgentAppliedConfig{
			Task:         "original brief task",
			CreateInputs: &store.AgentCreateInputs{},
		}

		require.NoError(t, d.DispatchAgentStart(ctx, ag, "", false))
		assert.Empty(t, client.lastTask, "completed agent must not replay AppliedConfig.Task")
		require.NotNil(t, client.lastStartExtras.PriorStatus)
		assert.Equal(t, string(state.ActivityCompleted), client.lastStartExtras.PriorStatus.Activity)
		assert.Equal(t, "finished review", client.lastStartExtras.PriorStatus.Summary)
		assert.Equal(t, ts, client.lastStartExtras.PriorStatus.At)
	})

	t.Run("stopped completed agent with explicit task passes explicit task", func(t *testing.T) {
		d, client, ag := autoExposeDispatchFixture(t)
		ag.Phase = string(state.PhaseStopped)
		ag.Activity = string(state.ActivityCompleted)
		ag.TaskSummary = "finished review"
		ag.LastActivityEvent = ts
		ag.AppliedConfig = &store.AgentAppliedConfig{
			Task:         "original brief task",
			CreateInputs: &store.AgentCreateInputs{},
		}

		require.NoError(t, d.DispatchAgentStart(ctx, ag, "follow-up task", false))
		assert.Equal(t, "follow-up task", client.lastTask)
		require.NotNil(t, client.lastStartExtras.PriorStatus)
	})

	t.Run("suspended completed agent carries PriorStatus with empty task", func(t *testing.T) {
		d, client, ag := autoExposeDispatchFixture(t)
		ag.Phase = string(state.PhaseSuspended)
		ag.Activity = string(state.ActivityCompleted)
		ag.TaskSummary = "all done"
		ag.LastActivityEvent = ts
		ag.AppliedConfig = &store.AgentAppliedConfig{
			Task:         "original brief task",
			CreateInputs: &store.AgentCreateInputs{},
		}

		require.NoError(t, d.DispatchAgentStart(ctx, ag, "", true))
		assert.Empty(t, client.lastTask)
		require.NotNil(t, client.lastStartExtras.PriorStatus)
		assert.Equal(t, string(state.ActivityCompleted), client.lastStartExtras.PriorStatus.Activity)
		assert.Equal(t, "all done", client.lastStartExtras.PriorStatus.Summary)
	})

	t.Run("stopped non-completed agent keeps AppliedConfig.Task fallback", func(t *testing.T) {
		d, client, ag := autoExposeDispatchFixture(t)
		ag.Phase = string(state.PhaseStopped)
		ag.Activity = ""
		ag.AppliedConfig = &store.AgentAppliedConfig{
			Task:         "original brief task",
			CreateInputs: &store.AgentCreateInputs{},
		}

		require.NoError(t, d.DispatchAgentStart(ctx, ag, "", false))
		assert.Equal(t, "original brief task", client.lastTask)
		assert.Nil(t, client.lastStartExtras.PriorStatus)
	})

	t.Run("restart carries PriorStatus", func(t *testing.T) {
		d, client, ag := autoExposeDispatchFixture(t)
		ag.Phase = string(state.PhaseRunning)
		ag.Activity = string(state.ActivityCompleted)
		ag.TaskSummary = "shipped fix"
		ag.LastActivityEvent = ts

		require.NoError(t, d.DispatchAgentRestart(ctx, ag))
		require.NotNil(t, client.lastRestartExtras.PriorStatus)
		assert.Equal(t, string(state.ActivityCompleted), client.lastRestartExtras.PriorStatus.Activity)
		assert.Equal(t, "shipped fix", client.lastRestartExtras.PriorStatus.Summary)
		assert.Equal(t, ts, client.lastRestartExtras.PriorStatus.At)
	})
}
