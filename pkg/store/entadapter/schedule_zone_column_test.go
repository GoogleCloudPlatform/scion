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

// TestScheduleTimeZone covers the time_zone column (ptone/scion#6771):
// create persists it (empty by default), UpdateSchedule writes it only when
// the TimeZone field is set, and it can be cleared back to empty (UTC).
func TestScheduleTimeZone(t *testing.T) {
	s := newTestScheduleStore(t)
	ctx := context.Background()
	projectID := uuid.NewString()

	// The default is empty, meaning UTC.
	plain := newTestSchedule(projectID, "utc")
	require.NoError(t, s.CreateSchedule(ctx, plain))
	got, err := s.GetSchedule(ctx, plain.ID)
	require.NoError(t, err)
	assert.Empty(t, got.TimeZone, "a schedule created without a zone is UTC")

	// Create persists a zone.
	zoned := newTestSchedule(projectID, "pacific")
	zoned.TimeZone = "America/Los_Angeles"
	require.NoError(t, s.CreateSchedule(ctx, zoned))
	got, err = s.GetSchedule(ctx, zoned.ID)
	require.NoError(t, err)
	assert.Equal(t, "America/Los_Angeles", got.TimeZone)

	// A write that does not name TimeZone leaves it alone, even when the
	// struct passed carries a different value.
	got.Name = "renamed"
	got.TimeZone = "Europe/London"
	require.NoError(t, s.UpdateSchedule(ctx, got, store.ScheduleFieldMask{Name: true},
		got.AuthorizationRevision, got.AuthorizationRevision != 0, nil))
	got, err = s.GetSchedule(ctx, zoned.ID)
	require.NoError(t, err)
	assert.Equal(t, "renamed", got.Name)
	assert.Equal(t, "America/Los_Angeles", got.TimeZone, "an unmasked TimeZone is not written")

	// A masked write changes it.
	got.TimeZone = "Europe/London"
	require.NoError(t, s.UpdateSchedule(ctx, got, store.ScheduleFieldMask{TimeZone: true},
		got.AuthorizationRevision, got.AuthorizationRevision != 0, nil))
	got, err = s.GetSchedule(ctx, zoned.ID)
	require.NoError(t, err)
	assert.Equal(t, "Europe/London", got.TimeZone)

	// ...and clears it back to UTC.
	got.TimeZone = ""
	require.NoError(t, s.UpdateSchedule(ctx, got, store.ScheduleFieldMask{TimeZone: true},
		got.AuthorizationRevision, got.AuthorizationRevision != 0, nil))
	got, err = s.GetSchedule(ctx, zoned.ID)
	require.NoError(t, err)
	assert.Empty(t, got.TimeZone, "a masked write of an empty zone clears it")

	// The list carries it too.
	got.TimeZone = "Asia/Kolkata"
	require.NoError(t, s.UpdateSchedule(ctx, got, store.ScheduleFieldMask{TimeZone: true},
		got.AuthorizationRevision, got.AuthorizationRevision != 0, nil))
	list, err := s.ListSchedules(ctx, store.ScheduleFilter{ProjectID: projectID, Name: "renamed"},
		store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, list.Items, 1)
	assert.Equal(t, "Asia/Kolkata", list.Items[0].TimeZone)
}
