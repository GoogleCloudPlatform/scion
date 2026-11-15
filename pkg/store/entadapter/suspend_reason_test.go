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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	entagent "github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// suspendWithReason moves a to suspended with reason through
// UpdateAgentStatus, as the hub's suspend paths do.
func suspendWithReason(t *testing.T, ctx context.Context, s *AgentStore, id string, reason state.SuspendReason) {
	t.Helper()
	require.NoError(t, s.UpdateAgentStatus(ctx, id, store.AgentStatusUpdate{
		Phase:         string(state.PhaseSuspended),
		SuspendReason: string(reason),
	}))
}

func suspendReasonOf(t *testing.T, ctx context.Context, s *AgentStore, id string) string {
	t.Helper()
	got, err := s.GetAgent(ctx, id)
	require.NoError(t, err)
	return got.SuspendReason
}

func TestSuspendReason_RecordedAndCleared(t *testing.T) {
	ctx := context.Background()

	t.Run("suspend records the reason", func(t *testing.T) {
		s, projectID := newTestAgentStore(t)
		a := createAgentInPhase(t, ctx, s, projectID, "sr-set", "running")
		suspendWithReason(t, ctx, s, a.ID, state.SuspendReasonIdle)
		assert.Equal(t, "idle", suspendReasonOf(t, ctx, s, a.ID))
	})

	t.Run("a suspended report without a reason keeps it", func(t *testing.T) {
		s, projectID := newTestAgentStore(t)
		a := createAgentInPhase(t, ctx, s, projectID, "sr-keep", "running")
		suspendWithReason(t, ctx, s, a.ID, state.SuspendReasonStalled)
		require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
			Phase: string(state.PhaseSuspended), ContainerStatus: "stopped",
		}))
		assert.Equal(t, "stalled", suspendReasonOf(t, ctx, s, a.ID))
	})

	t.Run("a suspend without a reason records none", func(t *testing.T) {
		s, projectID := newTestAgentStore(t)
		a := createAgentInPhase(t, ctx, s, projectID, "sr-none", "running")
		suspendWithReason(t, ctx, s, a.ID, "")
		assert.Empty(t, suspendReasonOf(t, ctx, s, a.ID))
	})

	t.Run("a resume's starting write and its rollback keep it", func(t *testing.T) {
		s, projectID := newTestAgentStore(t)
		a := createAgentInPhase(t, ctx, s, projectID, "sr-rollback", "running")
		suspendWithReason(t, ctx, s, a.ID, state.SuspendReasonIdle)
		require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
			Phase: string(state.PhaseStarting), IfPhase: string(state.PhaseSuspended),
		}))
		assert.Equal(t, "idle", suspendReasonOf(t, ctx, s, a.ID), "starting keeps the reason")
		require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
			Phase: string(state.PhaseSuspended), IfPhase: string(state.PhaseStarting), Restore: true,
		}))
		assert.Equal(t, "idle", suspendReasonOf(t, ctx, s, a.ID), "a failed resume's rollback keeps the reason")
	})

	for _, phase := range []state.Phase{state.PhaseRunning, state.PhaseStopped, state.PhaseError, state.PhaseStopping} {
		t.Run("leaving suspended for "+string(phase)+" clears it", func(t *testing.T) {
			s, projectID := newTestAgentStore(t)
			a := createAgentInPhase(t, ctx, s, projectID, "sr-clear-"+string(phase), "running")
			suspendWithReason(t, ctx, s, a.ID, state.SuspendReasonIdle)
			require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{Phase: string(phase)}))
			assert.Empty(t, suspendReasonOf(t, ctx, s, a.ID))
		})
	}

	t.Run("a full-row write to running clears it, a suspended one keeps it", func(t *testing.T) {
		s, projectID := newTestAgentStore(t)
		a := createAgentInPhase(t, ctx, s, projectID, "sr-full", "running")
		suspendWithReason(t, ctx, s, a.ID, state.SuspendReasonUser)

		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		got.Message = "still suspended"
		require.NoError(t, s.UpdateAgent(ctx, got))
		assert.Equal(t, "user", suspendReasonOf(t, ctx, s, a.ID))

		got, err = s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		got.Phase = string(state.PhaseRunning)
		require.NoError(t, s.UpdateAgent(ctx, got))
		assert.Empty(t, suspendReasonOf(t, ctx, s, a.ID))
	})

	t.Run("a status write without a phase keeps it", func(t *testing.T) {
		s, projectID := newTestAgentStore(t)
		a := createAgentInPhase(t, ctx, s, projectID, "sr-nophase", "running")
		suspendWithReason(t, ctx, s, a.ID, state.SuspendReasonIdle)
		require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{Message: "hello"}))
		assert.Equal(t, "idle", suspendReasonOf(t, ctx, s, a.ID))
	})
}

// The hook acts on every client a writer can use: the store's and the
// launch writers' transaction client, by UpdateOne and bulk Update.
func TestClearSuspendReasonHook(t *testing.T) {
	ctx := context.Background()
	clears := map[string]bool{
		"running": true, "stopped": true, "stopping": true, "error": true,
		"created": true, "provisioning": true, "halted": true,
		"suspended": false, "starting": false,
	}
	type writer func(c *ent.Client, id string, phase string) error
	updateOne := func(c *ent.Client, id, phase string) error {
		uid, err := parseUUID(id)
		if err != nil {
			return err
		}
		_, err = c.Agent.UpdateOneID(uid).SetPhase(phase).Save(ctx)
		return err
	}
	bulk := func(c *ent.Client, id, phase string) error {
		uid, err := parseUUID(id)
		if err != nil {
			return err
		}
		_, err = c.Agent.Update().Where(entagent.IDEQ(uid)).SetPhase(phase).Save(ctx)
		return err
	}
	type via func(t *testing.T, s *AgentStore, w func(c *ent.Client) error)
	storeClient := func(t *testing.T, s *AgentStore, w func(c *ent.Client) error) {
		require.NoError(t, w(s.client))
	}
	txClient := func(t *testing.T, s *AgentStore, w func(c *ent.Client) error) {
		ltx, err := s.beginLaunchTx(ctx)
		require.NoError(t, err)
		defer ltx.cleanup()
		if err := w(ltx.client); err != nil {
			_ = ltx.tx.Rollback()
			require.NoError(t, err)
		}
		require.NoError(t, ltx.tx.Commit())
	}
	for viaName, v := range map[string]via{"entc": storeClient, "newTxClient": txClient} {
		for wName, w := range map[string]writer{"UpdateOne": updateOne, "Update": bulk} {
			for phase, wantClear := range clears {
				t.Run(viaName+"/"+wName+"/"+phase, func(t *testing.T) {
					s, projectID := newTestAgentStore(t)
					a := createAgentInPhase(t, ctx, s, projectID, "srhook", "running")
					suspendWithReason(t, ctx, s, a.ID, state.SuspendReasonIdle)
					v(t, s, func(c *ent.Client) error { return w(c, a.ID, phase) })
					if wantClear {
						assert.Empty(t, suspendReasonOf(t, ctx, s, a.ID), "a %s write clears the reason", phase)
					} else {
						assert.Equal(t, "idle", suspendReasonOf(t, ctx, s, a.ID), "a %s write keeps the reason", phase)
					}
				})
			}
		}
	}
}
