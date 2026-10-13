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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// On a hub started with soft-delete retention 0, the soft-delete-purge job
// deletes scheduled events that are not pending (failed included) once they
// are older than scheduledEventRetention, and keeps pending and younger
// ones (ptone/scion#4517).
func TestPurgeHandler_ScheduledEventPurgeDeletesRowsWithZeroRetention(t *testing.T) {
	st, err := newTestStore(t, ":memory:")
	require.NoError(t, err)
	ctx := context.Background()
	srv := &Server{store: st}
	srv.recordStartupLayer1Values() // startup retention 0
	retention, _ := srv.softDeleteSettings()
	require.Zero(t, retention)

	projectID := uuid.NewString()
	old := time.Now().Add(-scheduledEventRetention - 24*time.Hour).UTC().Truncate(time.Second)
	young := time.Now().Add(-scheduledEventRetention + 24*time.Hour).UTC().Truncate(time.Second)
	create := func(created time.Time, status string) string {
		t.Helper()
		evt := &store.ScheduledEvent{
			ID:        uuid.NewString(),
			ProjectID: projectID,
			EventType: "message",
			FireAt:    created.Add(time.Minute),
			Payload:   `{"message":"hello"}`,
			CreatedBy: "user-123",
			CreatedAt: created,
		}
		require.NoError(t, st.CreateScheduledEvent(ctx, evt))
		if status != store.ScheduledEventPending {
			require.NoError(t, st.UpdateScheduledEventStatus(ctx, evt.ID, status, &evt.FireAt, ""))
		}
		return evt.ID
	}

	oldFired := create(old, store.ScheduledEventFired)
	oldFailed := create(old, store.ScheduledEventFailed)
	oldCancelled := create(old, store.ScheduledEventCancelled)
	oldPending := create(old, store.ScheduledEventPending)
	youngFired := create(young, store.ScheduledEventFired)
	youngFailed := create(young, store.ScheduledEventFailed)

	srv.purgeHandler()(ctx)

	for name, id := range map[string]string{"fired": oldFired, "failed": oldFailed, "cancelled": oldCancelled} {
		_, err := st.GetScheduledEvent(ctx, id)
		assert.ErrorIs(t, err, store.ErrNotFound, "old %s event is purged", name)
	}
	for name, id := range map[string]string{"old pending": oldPending, "young fired": youngFired, "young failed": youngFailed} {
		_, err := st.GetScheduledEvent(ctx, id)
		assert.NoError(t, err, "%s event is kept", name)
	}
}
