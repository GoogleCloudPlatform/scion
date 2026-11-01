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
	"encoding/json"
	"errors"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The store satisfies the optional guarded batch interface.
var _ store.HubSettingGuardedBatchWriter = (*HubSettingStore)(nil)

// A guarded batch whose read guard or expected revision no longer holds
// writes nothing: no section changes and the settings revision does not
// move.
func TestUpsertHubSettingsGuardedConflictWritesNothing(t *testing.T) {
	s := newTestHubSettingStore(t)
	ctx := context.Background()
	rt, err := s.UpsertHubSetting(ctx, "runtimes", json.RawMessage(`{"a":{"type":"docker"}}`), "admin", 0, "managed")
	require.NoError(t, err)
	before := settingsRevision(t, s)

	cases := map[string]struct {
		writes []store.HubSettingWrite
		reads  map[string]int64
	}{
		"stale read guard": {
			writes: []store.HubSettingWrite{{Section: "profiles", Value: json.RawMessage(`{"p":{"runtime":"a"}}`), ExpectedRevision: -1}},
			reads:  map[string]int64{"runtimes": rt.Revision + 1},
		},
		"read guard expects no row": {
			writes: []store.HubSettingWrite{{Section: "profiles", Value: json.RawMessage(`{}`), ExpectedRevision: -1}},
			reads:  map[string]int64{"runtimes": 0},
		},
		"stale expected revision on the second write": {
			writes: []store.HubSettingWrite{
				{Section: "profiles", Value: json.RawMessage(`{}`), ExpectedRevision: 0},
				{Section: "runtimes", Value: json.RawMessage(`{}`), ExpectedRevision: rt.Revision + 5},
			},
		},
		"create-only over an existing row": {
			writes: []store.HubSettingWrite{{Section: "runtimes", Value: json.RawMessage(`{}`), ExpectedRevision: 0}},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := s.UpsertHubSettingsGuarded(ctx, tc.writes, tc.reads, "admin", "managed")
			require.True(t, errors.Is(err, store.ErrRevisionConflict), "err = %v", err)
			got, err := s.GetHubSetting(ctx, "runtimes")
			require.NoError(t, err)
			assert.Equal(t, rt.Revision, got.Revision)
			assert.JSONEq(t, `{"a":{"type":"docker"}}`, string(got.Value))
			_, err = s.GetHubSetting(ctx, "profiles")
			assert.ErrorIs(t, err, store.ErrNotFound)
			assert.Equal(t, before, settingsRevision(t, s))
		})
	}

	// Invalid batches are refused before any write.
	for _, bad := range [][]store.HubSettingWrite{
		nil,
		{{Section: store.HubSettingsRevisionSection, Value: json.RawMessage(`{}`), ExpectedRevision: -1}},
		{{Section: "profiles", Value: json.RawMessage(`{}`), ExpectedRevision: -1}, {Section: "profiles", Value: json.RawMessage(`{}`), ExpectedRevision: -1}},
	} {
		_, err := s.UpsertHubSettingsGuarded(ctx, bad, nil, "admin", "managed")
		assert.ErrorIs(t, err, store.ErrInvalidInput)
	}
	_, err = s.UpsertHubSettingsGuarded(ctx, []store.HubSettingWrite{{Section: "profiles", Value: json.RawMessage(`{}`), ExpectedRevision: -1}},
		map[string]int64{"profiles": 0}, "admin", "managed")
	assert.ErrorIs(t, err, store.ErrInvalidInput, "a section both written and read")
}

// A guarded batch whose guards hold writes every section, bumps each
// section's revision and the settings revision once, and returns that
// settings revision on every row.
func TestUpsertHubSettingsGuardedBumpsRevisionOnce(t *testing.T) {
	s := newTestHubSettingStore(t)
	ctx := context.Background()
	rt, err := s.UpsertHubSetting(ctx, "runtimes", json.RawMessage(`{"a":{"type":"docker"}}`), "admin", 0, "managed")
	require.NoError(t, err)
	before := settingsRevision(t, s)

	rows, err := s.UpsertHubSettingsGuarded(ctx, []store.HubSettingWrite{
		{Section: "runtimes", Value: json.RawMessage(`{"a":{"type":"kubernetes"}}`), ExpectedRevision: rt.Revision},
		{Section: "profiles", Value: json.RawMessage(`{"p":{"runtime":"a"}}`), ExpectedRevision: 0},
	}, map[string]int64{"harness_configs": 0}, "admin", "managed")
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, "runtimes", rows[0].Section)
	assert.Equal(t, rt.Revision+1, rows[0].Revision)
	assert.Equal(t, "profiles", rows[1].Section)
	assert.Equal(t, int64(1), rows[1].Revision)
	assert.Equal(t, "admin", rows[1].UpdatedBy)
	assert.Equal(t, before+1, settingsRevision(t, s))
	assert.Equal(t, before+1, rows[0].SettingsRevision)
	assert.Equal(t, before+1, rows[1].SettingsRevision)

	// A read guard at the current revision holds.
	rows, err = s.UpsertHubSettingsGuarded(ctx, []store.HubSettingWrite{
		{Section: "profiles", Value: json.RawMessage(`{}`), ExpectedRevision: -1},
	}, map[string]int64{"runtimes": rt.Revision + 1}, "", "managed")
	require.NoError(t, err)
	assert.Equal(t, int64(2), rows[0].Revision)
	assert.Equal(t, before+2, settingsRevision(t, s))
}

// A guarded delete removes the row and bumps the settings revision once;
// with a stale read guard or expected revision it deletes nothing, and a
// missing row is a conflict at an expected revision and not found at -1.
func TestUpsertHubSettingsGuardedDelete(t *testing.T) {
	s := newTestHubSettingStore(t)
	ctx := context.Background()
	rt, err := s.UpsertHubSetting(ctx, "runtimes", json.RawMessage(`{"a":{"type":"kubernetes"}}`), "admin", 0, "managed")
	require.NoError(t, err)
	pr, err := s.UpsertHubSetting(ctx, "profiles", json.RawMessage(`{"p":{"runtime":"a"}}`), "admin", 0, "managed")
	require.NoError(t, err)
	before := settingsRevision(t, s)

	for name, tc := range map[string]struct {
		write store.HubSettingWrite
		reads map[string]int64
	}{
		"stale read guard":        {store.HubSettingWrite{Section: "runtimes", ExpectedRevision: rt.Revision, Delete: true}, map[string]int64{"profiles": pr.Revision + 1}},
		"stale expected revision": {store.HubSettingWrite{Section: "runtimes", ExpectedRevision: rt.Revision + 1, Delete: true}, nil},
		"missing row":             {store.HubSettingWrite{Section: "harness_configs", ExpectedRevision: 3, Delete: true}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := s.UpsertHubSettingsGuarded(ctx, []store.HubSettingWrite{tc.write}, tc.reads, "", "")
			require.True(t, errors.Is(err, store.ErrRevisionConflict), "err = %v", err)
			got, err := s.GetHubSetting(ctx, "runtimes")
			require.NoError(t, err)
			assert.Equal(t, rt.Revision, got.Revision)
			assert.Equal(t, before, settingsRevision(t, s))
		})
	}
	_, err = s.UpsertHubSettingsGuarded(ctx, []store.HubSettingWrite{{Section: "harness_configs", ExpectedRevision: -1, Delete: true}}, nil, "", "")
	assert.ErrorIs(t, err, store.ErrNotFound)

	rows, err := s.UpsertHubSettingsGuarded(ctx, []store.HubSettingWrite{
		{Section: "runtimes", ExpectedRevision: rt.Revision, Delete: true},
	}, map[string]int64{"profiles": pr.Revision}, "", "")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "runtimes", rows[0].Section)
	assert.Equal(t, before+1, rows[0].SettingsRevision)
	assert.Equal(t, before+1, settingsRevision(t, s))
	_, err = s.GetHubSetting(ctx, "runtimes")
	assert.ErrorIs(t, err, store.ErrNotFound)
}
