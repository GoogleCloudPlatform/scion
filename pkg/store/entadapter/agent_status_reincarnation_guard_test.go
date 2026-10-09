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

package entadapter

import (
	"context"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAgentStore_UpdateAgentStatus_ReincarnationGuard covers the store-side
// reincarnation guard (ptone/scion#2887). The reincarnation state is set
// directly on the row, as if a reincarnation started after the hub read
// the agent; the store must still drop the report's status fields when the
// update is marked GuardReincarnation, while the other fields and the
// LastSeen bump apply. Without the flag the write is unchanged.
func TestAgentStore_UpdateAgentStatus_ReincarnationGuard(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	newReport := func(guard bool) store.AgentStatusUpdate {
		code := 1
		return store.AgentStatusUpdate{
			Phase: "error", Activity: "crashed", Message: "boom",
			ExitCode: &code, ExitReason: "crashed",
			ContainerStatus: "Exited (1)", TaskSummary: "summary",
			Heartbeat: true, GuardReincarnation: guard,
		}
	}

	seed := func(t *testing.T, slug, reincarnationState string) *store.Agent {
		t.Helper()
		a := makeAgent(projectID, slug)
		require.NoError(t, s.CreateAgent(ctx, a))
		if reincarnationState != "" {
			require.NoError(t, s.client.Agent.UpdateOneID(uuid.MustParse(a.ID)).
				SetReincarnationState(reincarnationState).Exec(ctx))
		}
		before, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		return before
	}

	assertApplied := func(t *testing.T, id string, before *store.Agent) {
		t.Helper()
		got, err := s.GetAgent(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, "error", got.Phase)
		assert.Equal(t, "crashed", got.Activity)
		assert.Equal(t, "boom", got.Message)
		assert.Equal(t, "crashed", got.ExitReason)
		require.NotNil(t, got.ExitCode)
		assert.Equal(t, 1, *got.ExitCode)
		assert.Equal(t, "Exited (1)", got.ContainerStatus)
		assert.Equal(t, "summary", got.TaskSummary)
		assert.True(t, got.LastSeen.After(before.LastSeen) || before.LastSeen.IsZero(), "LastSeen must advance")
	}

	for _, rs := range []string{
		store.ReincarnationStatePending,
		store.ReincarnationStateStopping,
		store.ReincarnationStateProvisioning,
		store.ReincarnationStateStarting,
	} {
		t.Run("in flight "+rs+" with flag keeps status fields", func(t *testing.T) {
			before := seed(t, "rg-held-"+rs, rs)
			time.Sleep(2 * time.Millisecond)
			require.NoError(t, s.UpdateAgentStatus(ctx, before.ID, newReport(true)))

			got, err := s.GetAgent(ctx, before.ID)
			require.NoError(t, err)
			assert.Equal(t, "running", got.Phase, "phase must not change")
			assert.Equal(t, "thinking", got.Activity)
			assert.Equal(t, "", got.Message)
			assert.Equal(t, "", got.ExitReason)
			assert.Nil(t, got.ExitCode)
			assert.Equal(t, rs, got.ReincarnationState)
			// Fields the reincarnation does not own still apply.
			assert.Equal(t, "Exited (1)", got.ContainerStatus)
			assert.Equal(t, "summary", got.TaskSummary)
			assert.True(t, got.LastSeen.After(before.LastSeen), "LastSeen must advance")
		})
	}

	// With the flag unset, UpdateAgentStatus behaves exactly as before,
	// even while a reincarnation is in flight: hub-internal writers (the
	// reincarnation worker among them) keep writing the phase.
	t.Run("in flight without flag applies", func(t *testing.T) {
		before := seed(t, "rg-noflag", store.ReincarnationStateStopping)
		time.Sleep(2 * time.Millisecond)
		require.NoError(t, s.UpdateAgentStatus(ctx, before.ID, newReport(false)))
		assertApplied(t, before.ID, before)
	})

	for _, rs := range []string{store.ReincarnationStateNone, store.ReincarnationStateFailed} {
		t.Run("not in flight "+rs+" with flag applies", func(t *testing.T) {
			before := seed(t, "rg-idle-"+rs, rs)
			time.Sleep(2 * time.Millisecond)
			require.NoError(t, s.UpdateAgentStatus(ctx, before.ID, newReport(true)))
			assertApplied(t, before.ID, before)
		})
	}
}
