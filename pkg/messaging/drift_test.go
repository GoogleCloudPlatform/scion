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

package messaging

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTransitionDriftState_ValidTransitions(t *testing.T) {
	tests := []struct {
		name    string
		from    string
		trigger string
		want    string
	}{
		{
			name:    "active to orphaned on send_failed_not_found",
			from:    DriftStateActive,
			trigger: DriftTriggerSendFailedNotFound,
			want:    DriftStateOrphaned,
		},
		{
			name:    "active to unresolvable on send_failed_permanent",
			from:    DriftStateActive,
			trigger: DriftTriggerSendFailedPermanent,
			want:    DriftStateUnresolvable,
		},
		{
			name:    "orphaned to active on inbound_message (resurrection)",
			from:    DriftStateOrphaned,
			trigger: DriftTriggerInboundMessage,
			want:    DriftStateActive,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := TransitionDriftState(tt.from, tt.trigger)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestTransitionDriftState_InvalidTransitions(t *testing.T) {
	tests := []struct {
		name    string
		from    string
		trigger string
		reason  string
	}{
		{
			name:    "orphaned + send_attempt fails fast",
			from:    DriftStateOrphaned,
			trigger: DriftTriggerSendAttempt,
			reason:  "cannot send to orphaned conversation",
		},
		{
			name:    "unresolvable + inbound_message is terminal",
			from:    DriftStateUnresolvable,
			trigger: DriftTriggerInboundMessage,
			reason:  "unresolvable is terminal",
		},
		{
			name:    "unresolvable + send_attempt is terminal",
			from:    DriftStateUnresolvable,
			trigger: DriftTriggerSendAttempt,
			reason:  "unresolvable is terminal",
		},
		{
			name:    "unresolvable + send_failed_not_found is terminal",
			from:    DriftStateUnresolvable,
			trigger: DriftTriggerSendFailedNotFound,
			reason:  "unresolvable is terminal",
		},
		{
			name:    "active + unknown trigger",
			from:    DriftStateActive,
			trigger: "unknown_trigger",
			reason:  "no transition defined",
		},
		{
			name:    "orphaned + unknown trigger",
			from:    DriftStateOrphaned,
			trigger: "unknown_trigger",
			reason:  "no transition defined",
		},
		{
			name:    "unknown state",
			from:    "bogus",
			trigger: DriftTriggerSendAttempt,
			reason:  "unknown drift state",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := TransitionDriftState(tt.from, tt.trigger)
			require.Error(t, err)

			var driftErr *ErrInvalidDriftTransition
			require.ErrorAs(t, err, &driftErr)
			assert.Equal(t, tt.from, driftErr.From)
			assert.Equal(t, tt.trigger, driftErr.Trigger)
			assert.Contains(t, driftErr.Reason, tt.reason)
		})
	}
}

func TestErrInvalidDriftTransition_Error(t *testing.T) {
	err := &ErrInvalidDriftTransition{
		From:    DriftStateOrphaned,
		Trigger: DriftTriggerSendAttempt,
		Reason:  "cannot send to orphaned conversation; fail fast",
	}
	msg := err.Error()
	assert.Contains(t, msg, "orphaned")
	assert.Contains(t, msg, "send_attempt")
	assert.Contains(t, msg, "fail fast")
}
