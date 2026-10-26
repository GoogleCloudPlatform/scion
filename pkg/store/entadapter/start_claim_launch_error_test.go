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

package entadapter

import (
	"context"
	"fmt"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStartClaim_ClearsEndedStartLaunchError pins that a start claim clears
// the launch error and cause an ended start or restart launch left, so a
// synchronous start after a failed asynchronous start does not show the old
// error next to its own outcome (ptone/scion#5273). Kept: a create launch's
// error (the incomplete-create marker), an unconfirmed launch outcome
// (broker_lost, launch_timeout: later starts keep treating the agent's state
// as unknown), and an active launch's columns.
func TestStartClaim_ClearsEndedStartLaunchError(t *testing.T) {
	cases := []struct {
		name      string
		kind      string
		state     string
		code      string
		wantClear bool
	}{
		{"ended start launch", store.LaunchKindStart, store.LaunchStateEnded, "boom", true},
		{"ended restart launch", store.LaunchKindRestart, store.LaunchStateEnded, "boom", true},
		{"ended start launch, broker_lost", store.LaunchKindStart, store.LaunchStateEnded, store.LaunchErrorBrokerLost, false},
		{"ended start launch, launch_timeout", store.LaunchKindStart, store.LaunchStateEnded, store.LaunchErrorLaunchTimeout, false},
		{"ended restart launch, broker_lost", store.LaunchKindRestart, store.LaunchStateEnded, store.LaunchErrorBrokerLost, false},
		{"ended create launch", store.LaunchKindCreate, store.LaunchStateEnded, "boom", false},
		{"active start launch", store.LaunchKindStart, store.LaunchStateActive, "boom", false},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			s, projectID := newTestAgentStore(t)
			a := newClaimAgent(t, ctx, s, projectID, fmt.Sprintf("sc-launch-err-%d", i))
			uid, err := parseUUID(a.ID)
			require.NoError(t, err)
			_, err = s.client.Agent.UpdateOneID(uid).
				SetLaunchID("launch-1").
				SetLaunchKind(c.kind).
				SetLaunchState(c.state).
				SetLaunchError(c.code).
				SetLaunchCause("runtime_forbidden").
				Save(ctx)
			require.NoError(t, err)

			_, err = s.ClaimAgentStart(ctx, a.ID, "hub-1", store.StartClaimUser, "default", testClaimTTL)
			require.NoError(t, err)

			got, err := s.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			if c.wantClear {
				assert.Empty(t, got.LaunchError, "the start claim must clear the ended start launch's error")
				assert.Empty(t, got.LaunchCause, "the start claim must clear the ended start launch's cause")
			} else {
				assert.Equal(t, c.code, got.LaunchError, "the start claim must keep this launch's error")
				assert.Equal(t, "runtime_forbidden", got.LaunchCause, "the start claim must keep this launch's cause")
			}
			assert.Equal(t, "launch-1", got.LaunchID, "the claim leaves the launch record itself alone")
			assert.Equal(t, c.state, got.LaunchState)
		})
	}
}
