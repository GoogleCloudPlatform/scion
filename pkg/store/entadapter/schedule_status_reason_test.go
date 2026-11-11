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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestScheduleStatusReason covers the status_reason column
// (ptone/scion#5403): UpdateScheduleStatus sets and clears it with the
// status, UpdateSchedule writes it whenever the Status field is written and
// leaves it alone otherwise, and create persists it.
func TestScheduleStatusReason(t *testing.T) {
	s := newTestScheduleStore(t)
	ctx := context.Background()
	projectID := uuid.NewString()

	sc := newTestSchedule(projectID, "nightly")
	require.NoError(t, s.CreateSchedule(ctx, sc))
	got, err := s.GetSchedule(ctx, sc.ID)
	require.NoError(t, err)
	assert.Empty(t, got.StatusReason, "a new schedule carries no reason")

	// UpdateScheduleStatus sets the reason with the status.
	require.NoError(t, s.UpdateScheduleStatus(ctx, sc.ID, store.ScheduleStatusPaused,
		store.ScheduleStatusReasonCronZoneUnsupported))
	got, err = s.GetSchedule(ctx, sc.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduleStatusPaused, got.Status)
	assert.Equal(t, store.ScheduleStatusReasonCronZoneUnsupported, got.StatusReason, "UpdateScheduleStatus stores the reason")

	// A metadata-only UpdateSchedule leaves the reason alone, even when the
	// struct passed carries a different one.
	got.Name = "renamed"
	got.StatusReason = ""
	require.NoError(t, s.UpdateSchedule(ctx, got, store.ScheduleFieldMask{Name: true},
		got.AuthorizationRevision, got.AuthorizationRevision != 0, nil))
	got, err = s.GetSchedule(ctx, sc.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduleStatusReasonCronZoneUnsupported, got.StatusReason, "a metadata-only write leaves the reason")

	// UpdateScheduleStatus with no reason clears it.
	require.NoError(t, s.UpdateScheduleStatus(ctx, sc.ID, store.ScheduleStatusPaused, store.ScheduleStatusReasonNone))
	got, err = s.GetSchedule(ctx, sc.ID)
	require.NoError(t, err)
	assert.Empty(t, got.StatusReason, "UpdateScheduleStatus with no reason clears it")

	// UpdateSchedule with the Status field writes the reason, and reports
	// the stored one back.
	got.Status = store.ScheduleStatusPaused
	got.StatusReason = store.ScheduleStatusReasonCreatorDeleted
	require.NoError(t, s.UpdateSchedule(ctx, got, store.ScheduleFieldMask{Status: true},
		got.AuthorizationRevision, got.AuthorizationRevision != 0, nil))
	assert.Equal(t, store.ScheduleStatusReasonCreatorDeleted, got.StatusReason)
	got, err = s.GetSchedule(ctx, sc.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduleStatusReasonCreatorDeleted, got.StatusReason)

	// ...and clears it when the struct carries none.
	got.Status = store.ScheduleStatusActive
	got.StatusReason = store.ScheduleStatusReasonNone
	require.NoError(t, s.UpdateSchedule(ctx, got, store.ScheduleFieldMask{Status: true},
		got.AuthorizationRevision, got.AuthorizationRevision != 0, nil))
	got, err = s.GetSchedule(ctx, sc.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduleStatusActive, got.Status)
	assert.Empty(t, got.StatusReason, "a Status write with no reason clears it")

	// Create persists a reason.
	pre := newTestSchedule(projectID, "created-paused")
	pre.Status = store.ScheduleStatusPaused
	pre.StatusReason = store.ScheduleStatusReasonCreatorDeleted
	require.NoError(t, s.CreateSchedule(ctx, pre))
	got, err = s.GetSchedule(ctx, pre.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduleStatusReasonCreatorDeleted, got.StatusReason)

	// The list carries it too.
	list, err := s.ListSchedules(ctx, store.ScheduleFilter{ProjectID: projectID, Status: store.ScheduleStatusPaused},
		store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, list.Items, 1)
	assert.Equal(t, store.ScheduleStatusReasonCreatorDeleted, list.Items[0].StatusReason)
}
