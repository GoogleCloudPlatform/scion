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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteAgentGetResponse_IdleSuspendFields(t *testing.T) {
	srv, _ := testServer(t)
	srv.config.AutoSuspendIdle = true
	srv.config.IdleSuspendAfter = 15 * time.Minute
	setExperimentOverrides(t, srv, map[string]bool{
		experiments.IdleSuspend:  true,
		experiments.NeverSuspend: true,
	})

	now := time.Now().UTC().Truncate(time.Second)
	activitySince := now.Add(-5 * time.Minute)
	freshHeartbeat := now.Add(-15 * time.Second)

	callGetResponse := func(a *store.Agent) map[string]any {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/"+a.ID, nil)
		req = req.WithContext(context.Background())
		rec := httptest.NewRecorder()
		srv.writeAgentGetResponse(rec, req, a)
		require.Equal(t, http.StatusOK, rec.Code)
		var out map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		return out
	}

	t.Run("eligible running idle agent includes activitySince and idleSuspendAt", func(t *testing.T) {
		as := activitySince
		a := &store.Agent{
			ID:            "agent-eligible",
			Name:          "eligible",
			Phase:         string(state.PhaseRunning),
			Activity:      string(state.ActivityIdle),
			ActivitySince: &as,
			LastSeen:      freshHeartbeat,
			RunIntent:     store.RunIntentRunning,
		}
		out := callGetResponse(a)
		assert.Equal(t, activitySince.Format(time.RFC3339), out["activitySince"])
		assert.Equal(t, activitySince.Add(15*time.Minute).Format(time.RFC3339), out["idleSuspendAt"])
	})

	t.Run("never-suspend agent keeps activitySince but omits idleSuspendAt", func(t *testing.T) {
		as := activitySince
		a := &store.Agent{
			ID:            "agent-never",
			Name:          "never",
			Phase:         string(state.PhaseRunning),
			Activity:      string(state.ActivityIdle),
			ActivitySince: &as,
			LastSeen:      freshHeartbeat,
			RunIntent:     store.RunIntentRunning,
			SuspendPolicy: store.SuspendPolicyNever,
		}
		out := callGetResponse(a)
		assert.Equal(t, activitySince.Format(time.RFC3339), out["activitySince"])
		_, hasSA := out["idleSuspendAt"]
		assert.False(t, hasSA, "idleSuspendAt must be omitted for never-suspend agent")
	})

	t.Run("suspended agent omits both activitySince and idleSuspendAt", func(t *testing.T) {
		a := &store.Agent{
			ID:       "agent-suspended",
			Name:     "suspended",
			Phase:    string(state.PhaseSuspended),
			Activity: string(state.ActivityIdle),
			LastSeen: freshHeartbeat,
		}
		out := callGetResponse(a)
		_, hasAS := out["activitySince"]
		_, hasSA := out["idleSuspendAt"]
		assert.False(t, hasAS)
		assert.False(t, hasSA)
	})

	t.Run("auto_suspend_idle disabled omits idleSuspendAt while keeping activitySince", func(t *testing.T) {
		srv.config.AutoSuspendIdle = false
		t.Cleanup(func() { srv.config.AutoSuspendIdle = true })
		as := activitySince
		a := &store.Agent{
			ID:            "agent-disabled",
			Name:          "disabled",
			Phase:         string(state.PhaseRunning),
			Activity:      string(state.ActivityIdle),
			ActivitySince: &as,
			LastSeen:      freshHeartbeat,
			RunIntent:     store.RunIntentRunning,
		}
		out := callGetResponse(a)
		assert.Equal(t, activitySince.Format(time.RFC3339), out["activitySince"])
		_, hasSA := out["idleSuspendAt"]
		assert.False(t, hasSA)
	})
}

func TestWriteAgentGetResponse_LastReincarnatedAt(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	callGetResponse := func(a *store.Agent) map[string]any {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/"+a.ID, nil)
		req = req.WithContext(ctx)
		rec := httptest.NewRecorder()
		srv.writeAgentGetResponse(rec, req, a)
		require.Equal(t, http.StatusOK, rec.Code)
		var out map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		return out
	}

	t.Run("omitted when agent has no completed reincarnation", func(t *testing.T) {
		a := &store.Agent{ID: "agent-never-reinc", Name: "never", Phase: string(state.PhaseRunning)}
		out := callGetResponse(a)
		_, has := out["lastReincarnatedAt"]
		assert.False(t, has)
	})

	t.Run("returns newest completed reincarnation and ignores later failed attempt", func(t *testing.T) {
		a := &store.Agent{ID: "agent-reinc", Name: "reinc", Phase: string(state.PhaseRunning)}
		t1 := time.Date(2026, 11, 28, 9, 0, 0, 0, time.UTC)
		t2 := time.Date(2026, 11, 28, 11, 15, 0, 0, time.UTC)
		require.NoError(t, s.CreateAgentReincarnation(ctx, &store.AgentReincarnation{
			AgentID: a.ID, FromGeneration: 1, ToGeneration: 2,
			State: store.AgentReincarnationStateCompleted, CompletedAt: &t1,
		}))
		require.NoError(t, s.CreateAgentReincarnation(ctx, &store.AgentReincarnation{
			AgentID: a.ID, FromGeneration: 2, ToGeneration: 3,
			State: store.AgentReincarnationStateCompleted, CompletedAt: &t2,
		}))
		require.NoError(t, s.CreateAgentReincarnation(ctx, &store.AgentReincarnation{
			AgentID: a.ID, FromGeneration: 3, ToGeneration: 4,
			State: store.AgentReincarnationStateFailed, RequestedAt: t2.Add(5 * time.Minute),
		}))

		out := callGetResponse(a)
		assert.Equal(t, t2.Format(time.RFC3339), out["lastReincarnatedAt"])
	})
}

func TestWriteAgentList_LastReincarnatedAt(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	t1 := time.Date(2026, 11, 28, 9, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 11, 28, 11, 15, 0, 0, time.UTC)
	require.NoError(t, s.CreateAgentReincarnation(ctx, &store.AgentReincarnation{
		AgentID: "agent-reinc", FromGeneration: 1, ToGeneration: 2,
		State: store.AgentReincarnationStateCompleted, CompletedAt: &t1,
	}))
	require.NoError(t, s.CreateAgentReincarnation(ctx, &store.AgentReincarnation{
		AgentID: "agent-reinc", FromGeneration: 2, ToGeneration: 3,
		State: store.AgentReincarnationStateCompleted, CompletedAt: &t2,
	}))
	require.NoError(t, s.CreateAgentReincarnation(ctx, &store.AgentReincarnation{
		AgentID: "agent-reinc", FromGeneration: 3, ToGeneration: 4,
		State: store.AgentReincarnationStateFailed, RequestedAt: t2.Add(5 * time.Minute),
	}))

	rec := httptest.NewRecorder()
	srv.writeAgentList(ctx, rec, agentListViewFull, agentListIncludes{}, ListAgentsResponse{
		Agents: []AgentWithCapabilities{
			{Agent: store.Agent{ID: "agent-reinc", Name: "reinc", Phase: string(state.PhaseRunning)}},
			{Agent: store.Agent{ID: "agent-never", Name: "never", Phase: string(state.PhaseRunning)}},
		},
	})
	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Agents []map[string]any `json:"agents"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Agents, 2)
	assert.Equal(t, t2.Format(time.RFC3339), body.Agents[0]["lastReincarnatedAt"])
	_, hasNever := body.Agents[1]["lastReincarnatedAt"]
	assert.False(t, hasNever, "never-reincarnated agent must omit lastReincarnatedAt")
}
