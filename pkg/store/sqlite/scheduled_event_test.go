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

package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createTestProject creates a project for scheduled event tests.
func createTestProject(t *testing.T, s *SQLiteStore) string {
	t.Helper()
	ctx := context.Background()

	projectID := api.NewUUID()
	project := &store.Project{
		ID:         projectID,
		Name:       "Scheduled Event Test Project",
		Slug:       "sched-project-" + projectID[:8],
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateProject(ctx, project))
	return projectID
}

func TestScheduledEventCRUD(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	projectID := createTestProject(t, s)

	eventID := api.NewUUID()
	fireAt := time.Now().Add(1 * time.Hour).UTC().Truncate(time.Second)

	evt := &store.ScheduledEvent{
		ID:        eventID,
		ProjectID: projectID,
		EventType: "message",
		FireAt:    fireAt,
		Payload:   `{"text":"hello"}`,
		CreatedBy: "user-123",
	}

	// Create
	err := s.CreateScheduledEvent(ctx, evt)
	require.NoError(t, err)
	assert.False(t, evt.CreatedAt.IsZero(), "CreatedAt should be set automatically")
	assert.Equal(t, store.ScheduledEventPending, evt.Status)

	// Get
	got, err := s.GetScheduledEvent(ctx, eventID)
	require.NoError(t, err)
	assert.Equal(t, eventID, got.ID)
	assert.Equal(t, projectID, got.ProjectID)
	assert.Equal(t, "message", got.EventType)
	assert.Equal(t, fireAt, got.FireAt.UTC().Truncate(time.Second))
	assert.Equal(t, `{"text":"hello"}`, got.Payload)
	assert.Equal(t, store.ScheduledEventPending, got.Status)
	assert.Equal(t, "user-123", got.CreatedBy)
	assert.Nil(t, got.FiredAt)
	assert.Empty(t, got.Error)

	// Get not found
	_, err = s.GetScheduledEvent(ctx, "nonexistent")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestScheduledEventCreateValidation(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	// Missing ID
	err := s.CreateScheduledEvent(ctx, &store.ScheduledEvent{
		ProjectID: "project-1",
		EventType: "message",
	})
	assert.ErrorIs(t, err, store.ErrInvalidInput)

	// Missing ProjectID
	err = s.CreateScheduledEvent(ctx, &store.ScheduledEvent{
		ID:        api.NewUUID(),
		EventType: "message",
	})
	assert.ErrorIs(t, err, store.ErrInvalidInput)

	// Missing EventType
	err = s.CreateScheduledEvent(ctx, &store.ScheduledEvent{
		ID:        api.NewUUID(),
		ProjectID: "project-1",
	})
	assert.ErrorIs(t, err, store.ErrInvalidInput)

	// Non-existent project (FK constraint)
	err = s.CreateScheduledEvent(ctx, &store.ScheduledEvent{
		ID:        api.NewUUID(),
		ProjectID: "nonexistent-project",
		EventType: "message",
		Payload:   "{}",
	})
	assert.ErrorIs(t, err, store.ErrInvalidInput)
}

func TestScheduledEventListPending(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	projectID := createTestProject(t, s)

	// Create events with different statuses
	pending1 := &store.ScheduledEvent{
		ID:        api.NewUUID(),
		ProjectID: projectID,
		EventType: "message",
		FireAt:    time.Now().Add(2 * time.Hour),
		Payload:   "{}",
		Status:    store.ScheduledEventPending,
	}
	pending2 := &store.ScheduledEvent{
		ID:        api.NewUUID(),
		ProjectID: projectID,
		EventType: "message",
		FireAt:    time.Now().Add(1 * time.Hour), // Fires sooner
		Payload:   "{}",
		Status:    store.ScheduledEventPending,
	}
	require.NoError(t, s.CreateScheduledEvent(ctx, pending1))
	require.NoError(t, s.CreateScheduledEvent(ctx, pending2))

	// Mark one as fired to exclude it
	now := time.Now()
	require.NoError(t, s.UpdateScheduledEventStatus(ctx, pending1.ID, store.ScheduledEventFired, &now, ""))

	// ListPending should only return the pending one
	events, err := s.ListPendingScheduledEvents(ctx)
	require.NoError(t, err)
	assert.Len(t, events, 1)
	assert.Equal(t, pending2.ID, events[0].ID)
}

func TestScheduledEventListPendingOrderByFireAt(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	projectID := createTestProject(t, s)

	// Create events in reverse fire_at order
	later := &store.ScheduledEvent{
		ID:        api.NewUUID(),
		ProjectID: projectID,
		EventType: "message",
		FireAt:    time.Now().Add(3 * time.Hour),
		Payload:   "{}",
	}
	sooner := &store.ScheduledEvent{
		ID:        api.NewUUID(),
		ProjectID: projectID,
		EventType: "message",
		FireAt:    time.Now().Add(1 * time.Hour),
		Payload:   "{}",
	}
	require.NoError(t, s.CreateScheduledEvent(ctx, later))
	require.NoError(t, s.CreateScheduledEvent(ctx, sooner))

	events, err := s.ListPendingScheduledEvents(ctx)
	require.NoError(t, err)
	require.Len(t, events, 2)
	// Should be ordered by fire_at ASC (sooner first)
	assert.Equal(t, sooner.ID, events[0].ID)
	assert.Equal(t, later.ID, events[1].ID)
}

func TestScheduledEventUpdateStatus(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	projectID := createTestProject(t, s)

	eventID := api.NewUUID()
	evt := &store.ScheduledEvent{
		ID:        eventID,
		ProjectID: projectID,
		EventType: "message",
		FireAt:    time.Now().Add(1 * time.Hour),
		Payload:   "{}",
	}
	require.NoError(t, s.CreateScheduledEvent(ctx, evt))

	// Update to fired with firedAt
	now := time.Now().UTC().Truncate(time.Second)
	err := s.UpdateScheduledEventStatus(ctx, eventID, store.ScheduledEventFired, &now, "")
	require.NoError(t, err)

	got, err := s.GetScheduledEvent(ctx, eventID)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduledEventFired, got.Status)
	require.NotNil(t, got.FiredAt)
	assert.Equal(t, now, got.FiredAt.UTC().Truncate(time.Second))
	assert.Empty(t, got.Error)

	// Update with error
	err = s.UpdateScheduledEventStatus(ctx, eventID, store.ScheduledEventExpired, &now, "handler failed")
	require.NoError(t, err)

	got, err = s.GetScheduledEvent(ctx, eventID)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduledEventExpired, got.Status)
	assert.Equal(t, "handler failed", got.Error)
}

func TestScheduledEventCancel(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	projectID := createTestProject(t, s)

	eventID := api.NewUUID()
	evt := &store.ScheduledEvent{
		ID:        eventID,
		ProjectID: projectID,
		EventType: "message",
		FireAt:    time.Now().Add(1 * time.Hour),
		Payload:   "{}",
	}
	require.NoError(t, s.CreateScheduledEvent(ctx, evt))

	// Cancel pending event
	err := s.CancelScheduledEvent(ctx, eventID)
	require.NoError(t, err)

	got, err := s.GetScheduledEvent(ctx, eventID)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduledEventCancelled, got.Status)

	// Cancel again (not pending anymore) — should return ErrNotFound
	err = s.CancelScheduledEvent(ctx, eventID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	// Cancel non-existent event
	err = s.CancelScheduledEvent(ctx, "nonexistent")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestScheduledEventListWithFilter(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	projectID1 := createTestProject(t, s)
	projectID2 := createTestProject(t, s)

	// Create events across projects and types
	events := []*store.ScheduledEvent{
		{ID: api.NewUUID(), ProjectID: projectID1, EventType: "message", FireAt: time.Now().Add(1 * time.Hour), Payload: "{}"},
		{ID: api.NewUUID(), ProjectID: projectID1, EventType: "status_update", FireAt: time.Now().Add(2 * time.Hour), Payload: "{}"},
		{ID: api.NewUUID(), ProjectID: projectID2, EventType: "message", FireAt: time.Now().Add(3 * time.Hour), Payload: "{}"},
	}
	for _, evt := range events {
		require.NoError(t, s.CreateScheduledEvent(ctx, evt))
	}

	// Filter by project
	result, err := s.ListScheduledEvents(ctx, store.ScheduledEventFilter{ProjectID: projectID1}, store.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, result.Items, 2)
	assert.Equal(t, 2, result.TotalCount)

	// Filter by event type
	result, err = s.ListScheduledEvents(ctx, store.ScheduledEventFilter{EventType: "message"}, store.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, result.Items, 2)

	// Filter by status
	result, err = s.ListScheduledEvents(ctx, store.ScheduledEventFilter{Status: store.ScheduledEventPending}, store.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, result.Items, 3)

	// No results
	result, err = s.ListScheduledEvents(ctx, store.ScheduledEventFilter{Status: store.ScheduledEventFired}, store.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, result.Items, 0)
}

func TestScheduledEventPurge(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	projectID := createTestProject(t, s)

	// Create events: one pending, one fired (old), one cancelled (old), one fired (recent)
	pendingEvt := &store.ScheduledEvent{
		ID: api.NewUUID(), ProjectID: projectID, EventType: "message",
		FireAt: time.Now().Add(1 * time.Hour), Payload: "{}",
	}
	firedOldEvt := &store.ScheduledEvent{
		ID: api.NewUUID(), ProjectID: projectID, EventType: "message",
		FireAt: time.Now().Add(-48 * time.Hour), Payload: "{}",
		CreatedAt: time.Now().Add(-48 * time.Hour),
	}
	cancelledOldEvt := &store.ScheduledEvent{
		ID: api.NewUUID(), ProjectID: projectID, EventType: "message",
		FireAt: time.Now().Add(-48 * time.Hour), Payload: "{}",
		CreatedAt: time.Now().Add(-48 * time.Hour),
	}
	firedRecentEvt := &store.ScheduledEvent{
		ID: api.NewUUID(), ProjectID: projectID, EventType: "message",
		FireAt: time.Now().Add(-1 * time.Hour), Payload: "{}",
	}

	require.NoError(t, s.CreateScheduledEvent(ctx, pendingEvt))
	require.NoError(t, s.CreateScheduledEvent(ctx, firedOldEvt))
	require.NoError(t, s.CreateScheduledEvent(ctx, cancelledOldEvt))
	require.NoError(t, s.CreateScheduledEvent(ctx, firedRecentEvt))

	// Mark statuses
	now := time.Now()
	require.NoError(t, s.UpdateScheduledEventStatus(ctx, firedOldEvt.ID, store.ScheduledEventFired, &now, ""))
	require.NoError(t, s.UpdateScheduledEventStatus(ctx, cancelledOldEvt.ID, store.ScheduledEventCancelled, nil, ""))
	require.NoError(t, s.UpdateScheduledEventStatus(ctx, firedRecentEvt.ID, store.ScheduledEventFired, &now, ""))

	// Purge events older than 24 hours
	cutoff := time.Now().Add(-24 * time.Hour)
	purged, err := s.PurgeOldScheduledEvents(ctx, cutoff)
	require.NoError(t, err)
	// Should purge firedOldEvt and cancelledOldEvt (non-pending, created > 24h ago)
	assert.Equal(t, 2, purged)

	// Pending event should still exist
	_, err = s.GetScheduledEvent(ctx, pendingEvt.ID)
	assert.NoError(t, err)

	// Recently fired event should still exist
	_, err = s.GetScheduledEvent(ctx, firedRecentEvt.ID)
	assert.NoError(t, err)

	// Old events should be gone
	_, err = s.GetScheduledEvent(ctx, firedOldEvt.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = s.GetScheduledEvent(ctx, cancelledOldEvt.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestScheduledEventOptionalCreatedBy(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	projectID := createTestProject(t, s)

	evt := &store.ScheduledEvent{
		ID:        api.NewUUID(),
		ProjectID: projectID,
		EventType: "message",
		FireAt:    time.Now().Add(1 * time.Hour),
		Payload:   "{}",
		// No CreatedBy
	}
	require.NoError(t, s.CreateScheduledEvent(ctx, evt))

	got, err := s.GetScheduledEvent(ctx, evt.ID)
	require.NoError(t, err)
	assert.Empty(t, got.CreatedBy)
}
