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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
)

func TestFormatNotificationThreshold(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{-time.Second, ""},
		{0, ""},
		{30 * time.Second, "30s"},
		{5 * time.Minute, "5m"},
		{15 * time.Minute, "15m"},
		{2*time.Minute + 30*time.Second, "2m30s"},
		{time.Hour, "1h"},
		{time.Hour + 30*time.Minute, "1h30m"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, formatNotificationThreshold(tt.d), tt.d.String())
	}
}

func TestFormatSuspendedNotificationMessage(t *testing.T) {
	agent := &store.Agent{Slug: "bot"}
	tests := []struct {
		name      string
		reason    state.SuspendReason
		threshold time.Duration
		autoWakes bool
		want      string
	}{
		{
			name:      "idle auto-suspend with threshold and auto-wake on",
			reason:    state.SuspendReasonIdle,
			threshold: 15 * time.Minute,
			autoWakes: true,
			want:      "bot was automatically SUSPENDED (idle for 15m; any message wakes it)",
		},
		{
			name:      "idle auto-suspend with threshold and auto-wake off",
			reason:    state.SuspendReasonIdle,
			threshold: 15 * time.Minute,
			autoWakes: false,
			want:      "bot was automatically SUSPENDED (idle for 15m; resume with 'scion resume bot')",
		},
		{
			name:      "stalled auto-suspend with threshold and auto-wake on",
			reason:    state.SuspendReasonStalled,
			threshold: 5 * time.Minute,
			autoWakes: true,
			want:      "bot was automatically SUSPENDED (stalled for 5m; any message wakes it)",
		},
		{
			name:      "stalled auto-suspend with threshold and auto-wake off",
			reason:    state.SuspendReasonStalled,
			threshold: 5 * time.Minute,
			autoWakes: false,
			want:      "bot was automatically SUSPENDED (stalled for 5m; resume with 'scion resume bot')",
		},
		{
			name:      "idle auto-suspend without threshold and auto-wake on",
			reason:    state.SuspendReasonIdle,
			threshold: 0,
			autoWakes: true,
			want:      "bot was automatically SUSPENDED (idle; any message wakes it)",
		},
		{
			name:      "stalled auto-suspend without threshold and auto-wake off",
			reason:    state.SuspendReasonStalled,
			threshold: 0,
			autoWakes: false,
			want:      "bot was automatically SUSPENDED (stalled; resume with 'scion resume bot')",
		},
		{
			name:      "manual suspend keeps existing notice",
			reason:    state.SuspendReasonUser,
			threshold: 15 * time.Minute,
			autoWakes: true,
			want:      "bot has reached status: SUSPENDED",
		},
		{
			name:      "empty reason keeps existing notice",
			reason:    "",
			threshold: 15 * time.Minute,
			autoWakes: false,
			want:      "bot has reached status: SUSPENDED",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, formatSuspendedNotificationMessage(agent, tt.reason, tt.threshold, tt.autoWakes))
		})
	}
}

func TestNotificationDispatcher_FormatEventMessage_AutoSuspend(t *testing.T) {
	thresholdFn := func(reason state.SuspendReason) time.Duration {
		switch reason {
		case state.SuspendReasonIdle:
			return 20 * time.Minute
		case state.SuspendReasonStalled:
			return 7 * time.Minute
		default:
			return 99 * time.Minute
		}
	}
	ndAutoWakeOn := &NotificationDispatcher{
		idleSuspendGateFn:      func() bool { return true },
		autoSuspendThresholdFn: thresholdFn,
	}
	ndAutoWakeOff := &NotificationDispatcher{
		idleSuspendGateFn:      func() bool { return false },
		autoSuspendThresholdFn: thresholdFn,
	}

	t.Run("event suspend reason takes precedence over stale agent row", func(t *testing.T) {
		evt := AgentStatusEvent{
			Phase:         string(state.PhaseSuspended),
			Activity:      string(state.ActivityIdle),
			SuspendReason: string(state.SuspendReasonIdle),
		}
		agent := &store.Agent{Slug: "worker", SuspendReason: string(state.SuspendReasonUser)}
		assert.Equal(t, "worker was automatically SUSPENDED (idle for 20m; any message wakes it)", ndAutoWakeOn.formatEventMessage(evt, agent))
	})

	t.Run("falls back to agent suspend reason when event reason empty", func(t *testing.T) {
		evt := AgentStatusEvent{
			Phase: string(state.PhaseSuspended),
		}
		agent := &store.Agent{Slug: "worker", SuspendReason: string(state.SuspendReasonStalled)}
		assert.Equal(t, "worker was automatically SUSPENDED (stalled for 7m; any message wakes it)", ndAutoWakeOn.formatEventMessage(evt, agent))
	})

	t.Run("stalled auto-suspend with idleSuspendGateFn false names explicit resume", func(t *testing.T) {
		evt := AgentStatusEvent{
			Phase:         string(state.PhaseSuspended),
			SuspendReason: string(state.SuspendReasonStalled),
		}
		agent := &store.Agent{Slug: "worker", SuspendReason: string(state.SuspendReasonStalled)}
		assert.Equal(t, "worker was automatically SUSPENDED (stalled for 7m; resume with 'scion resume worker')", ndAutoWakeOff.formatEventMessage(evt, agent))
	})

	t.Run("manual suspend uses existing notice and ignores threshold", func(t *testing.T) {
		evt := AgentStatusEvent{
			Phase:         string(state.PhaseSuspended),
			SuspendReason: string(state.SuspendReasonUser),
		}
		agent := &store.Agent{Slug: "worker", SuspendReason: string(state.SuspendReasonUser)}
		assert.Equal(t, "worker has reached status: SUSPENDED", ndAutoWakeOn.formatEventMessage(evt, agent))
	})

	t.Run("nil getters omit threshold and default auto-wake off", func(t *testing.T) {
		plain := &NotificationDispatcher{}
		evt := AgentStatusEvent{
			Phase:         string(state.PhaseSuspended),
			SuspendReason: string(state.SuspendReasonIdle),
		}
		agent := &store.Agent{Slug: "worker", SuspendReason: string(state.SuspendReasonIdle)}
		assert.Equal(t, "worker was automatically SUSPENDED (idle; resume with 'scion resume worker')", plain.formatEventMessage(evt, agent))
	})
}
