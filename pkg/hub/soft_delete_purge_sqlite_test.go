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
// deletes scheduled events that are not pending (failed and expired
// included) once they completed more than scheduledEventRetention ago, and
// keeps pending ones and ones that completed since. Completion is fired_at,
// or created for an event with no fired_at, such as a cancelled one
// (ptone/scion#4517).
func TestPurgeHandler_ScheduledEventPurgeDeletesRowsWithZeroRetention(t *testing.T) {
	st, err := newTestStore(t, ":memory:")
	require.NoError(t, err)
	ctx := context.Background()
	srv := &Server{store: st}
	srv.recordStartupLayer1Values() // startup retention 0
	retention, _ := srv.softDeleteSettings()
	require.Zero(t, retention)

	projectID := uuid.NewString()
	now := time.Now().UTC().Truncate(time.Second)
	old := now.Add(-scheduledEventRetention - 24*time.Hour)
	recent := now.Add(-scheduledEventRetention + 24*time.Hour)
	// create adds an event created at created. A pending status leaves it
	// pending, cancelled cancels it (no fired_at), and any other status is
	// recorded with firedAt, which may be nil.
	create := func(created time.Time, status string, firedAt *time.Time) string {
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
		switch status {
		case store.ScheduledEventPending:
		case store.ScheduledEventCancelled:
			require.NoError(t, st.CancelScheduledEvent(ctx, evt.ID))
		default:
			require.NoError(t, st.UpdateScheduledEventStatus(ctx, evt.ID, status, firedAt, ""))
		}
		got, err := st.GetScheduledEvent(ctx, evt.ID)
		require.NoError(t, err)
		require.Equal(t, status, got.Status)
		if firedAt == nil {
			require.Nil(t, got.FiredAt, "the row has no fired_at")
		} else {
			require.NotNil(t, got.FiredAt)
		}
		return evt.ID
	}

	purged := map[string]string{}
	purged["fired long ago"] = create(old, store.ScheduledEventFired, &old)
	purged["failed long ago"] = create(old, store.ScheduledEventFailed, &old)
	purged["expired long ago"] = create(old, store.ScheduledEventExpired, &old)
	purged["created and fired long ago"] = create(old, store.ScheduledEventFired, timePtr(old.Add(time.Hour)))
	purged["cancelled, created long ago"] = create(old, store.ScheduledEventCancelled, nil)
	purged["failed without fired_at, old"] = create(old, store.ScheduledEventFailed, nil)
	kept := map[string]string{}
	kept["pending, created long ago"] = create(old, store.ScheduledEventPending, nil)
	kept["created long ago, fired recently"] = create(old, store.ScheduledEventFired, &recent)
	kept["created long ago, failed recently"] = create(old, store.ScheduledEventFailed, &recent)
	kept["created long ago, expired recently"] = create(old, store.ScheduledEventExpired, &recent)
	kept["fired recently"] = create(recent, store.ScheduledEventFired, &recent)
	kept["cancelled, created recently"] = create(recent, store.ScheduledEventCancelled, nil)
	kept["failed without fired_at, recent"] = create(recent, store.ScheduledEventFailed, nil)

	srv.purgeHandler()(ctx)

	for name, id := range purged {
		_, err := st.GetScheduledEvent(ctx, id)
		assert.ErrorIs(t, err, store.ErrNotFound, "%s: purged", name)
	}
	for name, id := range kept {
		_, err := st.GetScheduledEvent(ctx, id)
		assert.NoError(t, err, "%s: kept", name)
	}
}
