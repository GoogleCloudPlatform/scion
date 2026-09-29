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

package cmd

import (
	"context"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAgentToUserDispatchStateBackfill_RepairsOnlyEligibleRows is the
// nc-promote-busy round 2 (R3) boot-migration test: it seeds one row of
// each shape (user-recipient pending, user-recipient TTL-expired-failed,
// user-recipient failed for an unrelated reason, agent-recipient pending),
// runs the migration once through the boot entry point, and asserts only
// the two nc-promote-busy shapes are repaired, the marker is written, and a
// second run is a no-op.
func TestAgentToUserDispatchStateBackfill_RepairsOnlyEligibleRows(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	proj := &store.Project{ID: uuid.NewString(), Name: "p", Slug: uuid.NewString()}
	require.NoError(t, s.CreateProject(ctx, proj))

	userPending := &store.Message{
		ID: uuid.NewString(), ProjectID: proj.ID,
		Sender: "agent:a", Recipient: "user:alice", Msg: "reply 1",
		CreatedAt: time.Now().Add(-2 * time.Hour),
	}
	require.NoError(t, s.CreateMessage(ctx, userPending))

	userExpiredFailed := &store.Message{
		ID: uuid.NewString(), ProjectID: proj.ID,
		Sender: "agent:a", Recipient: "user:bob", Msg: "reply 2",
		CreatedAt: time.Now().Add(-30 * time.Hour),
	}
	require.NoError(t, s.CreateMessage(ctx, userExpiredFailed))
	require.NoError(t, s.MarkMessageFailed(ctx, userExpiredFailed.ID, agentToUserDispatchStateExpiredReason))

	userOtherFailed := &store.Message{
		ID: uuid.NewString(), ProjectID: proj.ID,
		Sender: "agent:a", Recipient: "user:carol", Msg: "reply 3",
		CreatedAt: time.Now().Add(-30 * time.Hour),
	}
	require.NoError(t, s.CreateMessage(ctx, userOtherFailed))
	require.NoError(t, s.MarkMessageFailed(ctx, userOtherFailed.ID, "some unrelated delivery failure"))

	agentPending := &store.Message{
		ID: uuid.NewString(), ProjectID: proj.ID,
		Sender: "user:x", Recipient: "agent:b", Msg: "instruction",
		CreatedAt: time.Now().Add(-2 * time.Hour),
	}
	require.NoError(t, s.CreateMessage(ctx, agentPending))

	buf, restore := captureSlog(t)
	defer restore()

	runAgentToUserDispatchStateBackfill(ctx, s)

	logOutput := buf.String()
	assert.Contains(t, logOutput, "repaired=2")
	assert.Contains(t, logOutput, "pass completed")

	gotPending, err := s.GetMessage(ctx, userPending.ID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchDispatched, gotPending.DispatchState)
	assert.Nil(t, gotPending.DispatchFailureReason)

	gotExpiredFailed, err := s.GetMessage(ctx, userExpiredFailed.ID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchDispatched, gotExpiredFailed.DispatchState)
	assert.Nil(t, gotExpiredFailed.DispatchFailureReason)

	gotOtherFailed, err := s.GetMessage(ctx, userOtherFailed.ID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchFailed, gotOtherFailed.DispatchState,
		"a genuine, differently-reasoned failure must never be repaired")

	gotAgentPending, err := s.GetMessage(ctx, agentPending.ID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchPending, gotAgentPending.DispatchState,
		"an agent-recipient row is never touched by this backfill")

	// Marker written.
	done, err := IsMigrationComplete(ctx, s, MigrationAgentToUserDispatchStateBackfill)
	require.NoError(t, err)
	assert.True(t, done, "migration marker should be written after a successful pass")

	// Idempotent: a second run (even against a fresh IsMigrationComplete
	// check bypassed by calling the function directly) finds nothing left
	// to repair and does not disturb the now-dispatched rows.
	buf.Reset()
	runAgentToUserDispatchStateBackfill(ctx, s)
	assert.Contains(t, buf.String(), "already complete, skipping")

	gotPendingAgain, err := s.GetMessage(ctx, userPending.ID)
	require.NoError(t, err)
	assert.Equal(t, store.MessageDispatchDispatched, gotPendingAgain.DispatchState)
}

// TestAgentToUserDispatchStateBackfill_EmptyPassStillWritesMarker verifies
// M-1' semantics: a pass with zero eligible rows is still a completed pass
// and writes the marker (matching the other boot migrations in this file).
func TestAgentToUserDispatchStateBackfill_EmptyPassStillWritesMarker(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	buf, restore := captureSlog(t)
	defer restore()

	runAgentToUserDispatchStateBackfill(ctx, s)

	assert.Contains(t, buf.String(), "repaired=0")

	done, err := IsMigrationComplete(ctx, s, MigrationAgentToUserDispatchStateBackfill)
	require.NoError(t, err)
	assert.True(t, done, "an empty pass is still a completed pass")
}
