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
	"encoding/json"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestComputeAgentIdleSuspendView(t *testing.T) {
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	after := 15 * time.Minute
	activitySince := now.Add(-5 * time.Minute)
	freshHeartbeat := now.Add(-30 * time.Second)

	baseEligible := func() *store.Agent {
		as := activitySince
		return &store.Agent{
			ID:            "agent-1",
			Phase:         string(state.PhaseRunning),
			Activity:      string(state.ActivityIdle),
			ActivitySince: &as,
			LastSeen:      freshHeartbeat,
			RunIntent:     store.RunIntentRunning,
		}
	}

	t.Run("eligible idle agent returns activitySince + after", func(t *testing.T) {
		got := computeAgentIdleSuspendView(baseEligible(), now, true, after, false, true)
		require.NotNil(t, got)
		assert.Equal(t, activitySince.Add(after).UTC(), *got)
	})

	t.Run("eligible completed and harness_error activities", func(t *testing.T) {
		for _, act := range []state.Activity{state.ActivityCompleted, state.ActivityHarnessError} {
			a := baseEligible()
			a.Activity = string(act)
			got := computeAgentIdleSuspendView(a, now, true, after, false, true)
			require.NotNil(t, got, "activity %s should be eligible", act)
			assert.Equal(t, activitySince.Add(after).UTC(), *got)
		}
	})

	t.Run("empty run intent is eligible", func(t *testing.T) {
		a := baseEligible()
		a.RunIntent = ""
		got := computeAgentIdleSuspendView(a, now, true, after, false, true)
		require.NotNil(t, got)
		assert.Equal(t, activitySince.Add(after).UTC(), *got)
	})

	t.Run("delivery after activitySince pushes idleSuspendAt to lastDeliveryAt + after", func(t *testing.T) {
		a := baseEligible()
		deliveryAt := now.Add(-2 * time.Minute) // 3m after activitySince
		a.LastDeliveryAt = &deliveryAt
		got := computeAgentIdleSuspendView(a, now, true, after, false, true)
		require.NotNil(t, got)
		assert.Equal(t, deliveryAt.Add(after).UTC(), *got)
	})

	t.Run("delivery before activitySince does not alter idleSuspendAt", func(t *testing.T) {
		a := baseEligible()
		deliveryAt := now.Add(-10 * time.Minute) // 5m before activitySince
		a.LastDeliveryAt = &deliveryAt
		got := computeAgentIdleSuspendView(a, now, true, after, false, true)
		require.NotNil(t, got)
		assert.Equal(t, activitySince.Add(after).UTC(), *got)
	})

	t.Run("past-due eligible agent still returns past idleSuspendAt", func(t *testing.T) {
		a := baseEligible()
		oldIdle := now.Add(-20 * time.Minute)
		a.ActivitySince = &oldIdle
		got := computeAgentIdleSuspendView(a, now, true, after, false, true)
		require.NotNil(t, got)
		assert.Equal(t, oldIdle.Add(after).UTC(), *got)
		assert.True(t, got.Before(now))
	})

	t.Run("heartbeat exactly at recency boundary is eligible", func(t *testing.T) {
		a := baseEligible()
		a.LastSeen = now.Add(-agentHeartbeatRecency)
		got := computeAgentIdleSuspendView(a, now, true, after, false, true)
		require.NotNil(t, got)
	})

	t.Run("live work exactly at freshness boundary is eligible", func(t *testing.T) {
		a := baseEligible()
		liveAt := now.Add(-store.LiveWorkFreshness)
		a.LastLiveProcessAt = &liveAt
		got := computeAgentIdleSuspendView(a, now, true, after, false, true)
		require.NotNil(t, got)
	})

	t.Run("expired idle suspend claim past backoff is eligible", func(t *testing.T) {
		a := baseEligible()
		expiredClaim := now.Add(-idleSuspendBackoff() - time.Second)
		a.IdleSuspendClaimUntil = &expiredClaim
		got := computeAgentIdleSuspendView(a, now, true, after, false, true)
		require.NotNil(t, got)
	})

	t.Run("failed reincarnation state is not in flight and remains eligible", func(t *testing.T) {
		a := baseEligible()
		a.ReincarnationState = store.ReincarnationStateFailed
		got := computeAgentIdleSuspendView(a, now, true, after, false, true)
		require.NotNil(t, got)
	})

	ineligibleCases := []struct {
		name               string
		mutate             func(*store.Agent)
		enabled            bool
		after              time.Duration
		neverSuspendActive bool
		resumable          bool
	}{
		{
			name:    "disabled",
			enabled: false, after: after, resumable: true,
		},
		{
			name:    "zero after duration",
			enabled: true, after: 0, resumable: true,
		},
		{
			name:    "deleted agent",
			mutate:  func(a *store.Agent) { a.DeletedAt = now.Add(-time.Minute) },
			enabled: true, after: after, resumable: true,
		},
		{
			name: "active deletion holds row",
			mutate: func(a *store.Agent) {
				a.DeletionState = store.DeletionStateDeleting
				lease := now.Add(time.Minute)
				a.DeletionLeaseAt = &lease
			},
			enabled: true, after: after, resumable: true,
		},
		{
			name:    "reincarnation in flight",
			mutate:  func(a *store.Agent) { a.ReincarnationState = store.ReincarnationStateStarting },
			enabled: true, after: after, resumable: true,
		},
		{
			name:    "start claim held",
			mutate:  func(a *store.Agent) { a.StartClaimID = "claim-1" },
			enabled: true, after: after, resumable: true,
		},
		{
			name: "active idle suspend claim",
			mutate: func(a *store.Agent) {
				until := now.Add(30 * time.Second)
				a.IdleSuspendClaimUntil = &until
			},
			enabled: true, after: after, resumable: true,
		},
		{
			name: "idle suspend claim in backoff window",
			mutate: func(a *store.Agent) {
				expired := now.Add(-time.Second)
				a.IdleSuspendClaimUntil = &expired
			},
			enabled: true, after: after, resumable: true,
		},
		{
			name:    "phase not running",
			mutate:  func(a *store.Agent) { a.Phase = string(state.PhaseSuspended) },
			enabled: true, after: after, resumable: true,
		},
		{
			name:               "never-suspend active",
			enabled:            true,
			after:              after,
			neverSuspendActive: true,
			resumable:          true,
		},
		{
			name:    "run intent stopped",
			mutate:  func(a *store.Agent) { a.RunIntent = store.RunIntentStopped },
			enabled: true, after: after, resumable: true,
		},
		{
			name:    "activity working",
			mutate:  func(a *store.Agent) { a.Activity = string(state.ActivityWorking) },
			enabled: true, after: after, resumable: true,
		},
		{
			name:    "activity waiting_for_input",
			mutate:  func(a *store.Agent) { a.Activity = string(state.ActivityWaitingForInput) },
			enabled: true, after: after, resumable: true,
		},
		{
			name:    "activity blocked",
			mutate:  func(a *store.Agent) { a.Activity = string(state.ActivityBlocked) },
			enabled: true, after: after, resumable: true,
		},
		{
			name:    "activitySince nil",
			mutate:  func(a *store.Agent) { a.ActivitySince = nil },
			enabled: true, after: after, resumable: true,
		},
		{
			name:    "lastSeen zero",
			mutate:  func(a *store.Agent) { a.LastSeen = time.Time{} },
			enabled: true, after: after, resumable: true,
		},
		{
			name:    "heartbeat stale",
			mutate:  func(a *store.Agent) { a.LastSeen = now.Add(-agentHeartbeatRecency - time.Second) },
			enabled: true, after: after, resumable: true,
		},
		{
			name: "recent live work",
			mutate: func(a *store.Agent) {
				liveAt := now.Add(-store.LiveWorkFreshness + time.Second)
				a.LastLiveProcessAt = &liveAt
			},
			enabled: true, after: after, resumable: true,
		},
		{
			name:    "non-resumable harness",
			enabled: true, after: after, resumable: false,
		},
	}

	for _, tc := range ineligibleCases {
		t.Run("ineligible: "+tc.name, func(t *testing.T) {
			a := baseEligible()
			if tc.mutate != nil {
				tc.mutate(a)
			}
			got := computeAgentIdleSuspendView(a, now, tc.enabled, tc.after, tc.neverSuspendActive, tc.resumable)
			assert.Nil(t, got)
		})
	}

	t.Run("nil agent returns nil", func(t *testing.T) {
		assert.Nil(t, computeAgentIdleSuspendView(nil, now, true, after, false, true))
	})
}

func TestAgentWithCapabilities_JSON_ActivitySinceAndIdleSuspendAt(t *testing.T) {
	as := time.Date(2026, 11, 28, 11, 55, 0, 0, time.UTC)
	sa := time.Date(2026, 11, 28, 12, 10, 0, 0, time.UTC)

	t.Run("round-trips activitySince and idleSuspendAt when set", func(t *testing.T) {
		in := AgentWithCapabilities{
			Agent: store.Agent{
				ID:    "agent-1",
				Phase: string(state.PhaseRunning),
			},
			ActivitySince: &as,
			IdleSuspendAt: &sa,
		}
		raw, err := json.Marshal(in)
		require.NoError(t, err)

		var m map[string]any
		require.NoError(t, json.Unmarshal(raw, &m))
		assert.Equal(t, as.Format(time.RFC3339), m["activitySince"])
		assert.Equal(t, sa.Format(time.RFC3339), m["idleSuspendAt"])

		var out AgentWithCapabilities
		require.NoError(t, json.Unmarshal(raw, &out))
		require.NotNil(t, out.ActivitySince)
		require.NotNil(t, out.IdleSuspendAt)
		assert.True(t, out.ActivitySince.Equal(as))
		assert.True(t, out.IdleSuspendAt.Equal(sa))
	})

	t.Run("omits activitySince and idleSuspendAt when nil", func(t *testing.T) {
		in := AgentWithCapabilities{
			Agent: store.Agent{
				ID:    "agent-2",
				Phase: string(state.PhaseSuspended),
			},
		}
		raw, err := json.Marshal(in)
		require.NoError(t, err)

		var m map[string]any
		require.NoError(t, json.Unmarshal(raw, &m))
		_, hasAS := m["activitySince"]
		_, hasSA := m["idleSuspendAt"]
		assert.False(t, hasAS, "activitySince should be omitted when nil")
		assert.False(t, hasSA, "idleSuspendAt should be omitted when nil")
	})
}
