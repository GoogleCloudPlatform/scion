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
