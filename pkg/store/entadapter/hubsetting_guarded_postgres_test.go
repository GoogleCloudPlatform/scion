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
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

// The guarded hub settings batch on Postgres, where concurrent
// transactions run at READ COMMITTED. Skipped unless the enttest Postgres
// backend is built (-tags integration) and SCION_TEST_POSTGRES_URL is set:
//
//	SCION_TEST_POSTGRES_URL='postgres://user:pass@host:5432/postgres?sslmode=disable' \
//	  go test -tags integration -run 'HubSettingsGuarded.*Postgres' ./pkg/store/entadapter/

// guardedRaceRounds is how many times each race below is run.
const guardedRaceRounds = 20

func newPostgresGuardedStore(t *testing.T) *HubSettingStore {
	t.Helper()
	if !enttest.Active() {
		t.Skip("requires -tags integration and SCION_TEST_POSTGRES_URL")
	}
	return NewHubSettingStore(enttest.NewClient(t))
}

// raceGuarded runs a and b at the same time and returns their errors.
func raceGuarded(a, b func() error) (errA, errB error) {
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(2)
	go func() { defer wg.Done(); <-start; errA = a() }()
	go func() { defer wg.Done(); <-start; errB = b() }()
	close(start)
	wg.Wait()
	return errA, errB
}

// requireOneWinner asserts that exactly one of two racing batches
// succeeded and the other got store.ErrRevisionConflict.
func requireOneWinner(t *testing.T, errA, errB error) {
	t.Helper()
	if errA == nil {
		require.True(t, errors.Is(errB, store.ErrRevisionConflict), "both batches succeeded or b failed otherwise: %v", errB)
		return
	}
	require.True(t, errors.Is(errA, store.ErrRevisionConflict), "a failed otherwise: %v", errA)
	require.NoError(t, errB, "neither batch succeeded")
}

// A conflicting batch writes nothing and does not move the settings
// revision; a batch that holds writes both sections with one bump.
func TestUpsertHubSettingsGuardedConflictAndBump_Postgres(t *testing.T) {
	s := newPostgresGuardedStore(t)
	ctx := context.Background()
	rt, err := s.UpsertHubSetting(ctx, "runtimes", json.RawMessage(`{"a":{"type":"docker"}}`), "admin", 0, "managed")
	require.NoError(t, err)
	before := settingsRevision(t, s)

	_, err = s.UpsertHubSettingsGuarded(ctx, []store.HubSettingWrite{
		{Section: "profiles", Value: json.RawMessage(`{"p":{"runtime":"a"}}`), ExpectedRevision: -1},
	}, map[string]int64{"runtimes": rt.Revision + 1}, "admin", "managed")
	require.True(t, errors.Is(err, store.ErrRevisionConflict), "err = %v", err)
	_, err = s.GetHubSetting(ctx, "profiles")
	require.ErrorIs(t, err, store.ErrNotFound)
	require.Equal(t, before, settingsRevision(t, s))

	rows, err := s.UpsertHubSettingsGuarded(ctx, []store.HubSettingWrite{
		{Section: "runtimes", Value: json.RawMessage(`{"a":{"type":"kubernetes"}}`), ExpectedRevision: rt.Revision},
		{Section: "profiles", Value: json.RawMessage(`{"p":{"runtime":"a"}}`), ExpectedRevision: 0},
	}, nil, "admin", "managed")
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, before+1, settingsRevision(t, s))
	assert.Equal(t, before+1, rows[0].SettingsRevision)
	assert.Equal(t, before+1, rows[1].SettingsRevision)
}

// Two batches that each read the other's section as absent and create
// their own (the guard FOR UPDATE cannot lock, as there is no row) never
// both commit; neither do two first creates of the same section.
func TestUpsertHubSettingsGuardedAbsentRowRace_Postgres(t *testing.T) {
	s := newPostgresGuardedStore(t)
	ctx := context.Background()
	clearRows := func() {
		for _, section := range []string{"runtimes", "profiles"} {
			if err := s.DeleteHubSetting(ctx, section); err != nil && !errors.Is(err, store.ErrNotFound) {
				require.NoError(t, err)
			}
		}
	}
	create := func(section string, reads map[string]int64) func() error {
		return func() error {
			_, err := s.UpsertHubSettingsGuarded(ctx, []store.HubSettingWrite{
				{Section: section, Value: json.RawMessage(`{}`), ExpectedRevision: 0},
			}, reads, "admin", "managed")
			return err
		}
	}
	for i := 0; i < guardedRaceRounds; i++ {
		clearRows()
		errA, errB := raceGuarded(
			create("runtimes", map[string]int64{"profiles": 0}),
			create("profiles", map[string]int64{"runtimes": 0}),
		)
		requireOneWinner(t, errA, errB)
		clearRows()
		errA, errB = raceGuarded(create("runtimes", nil), create("runtimes", nil))
		requireOneWinner(t, errA, errB)
	}
}

// A guarded delete (a section reset) racing a write of the section it read
// never both commit: the loser gets a conflict and changes nothing. A
// delete that holds removes the row with one bump.
func TestUpsertHubSettingsGuardedDeleteRace_Postgres(t *testing.T) {
	s := newPostgresGuardedStore(t)
	ctx := context.Background()
	for i := 0; i < guardedRaceRounds; i++ {
		_ = s.DeleteHubSetting(ctx, "runtimes")
		_ = s.DeleteHubSetting(ctx, "profiles")
		rt, err := s.UpsertHubSetting(ctx, "runtimes", json.RawMessage(`{"a":{"type":"kubernetes"}}`), "admin", 0, "managed")
		require.NoError(t, err)
		pr, err := s.UpsertHubSetting(ctx, "profiles", json.RawMessage(`{"p":{"runtime":"a"}}`), "admin", 0, "managed")
		require.NoError(t, err)
		before := settingsRevision(t, s)

		errDelete, errWrite := raceGuarded(
			func() error {
				_, err := s.UpsertHubSettingsGuarded(ctx, []store.HubSettingWrite{
					{Section: "runtimes", ExpectedRevision: rt.Revision, Delete: true},
				}, map[string]int64{"profiles": pr.Revision}, "", "")
				return err
			},
			func() error {
				_, err := s.UpsertHubSettingsGuarded(ctx, []store.HubSettingWrite{
					{Section: "profiles", Value: json.RawMessage(`{"p":{"runtime":"a","runtime_class_name":"gvisor"}}`), ExpectedRevision: pr.Revision},
				}, map[string]int64{"runtimes": rt.Revision}, "admin", "managed")
				return err
			},
		)
		requireOneWinner(t, errDelete, errWrite)
		assert.Equal(t, before+1, settingsRevision(t, s), "one batch, one bump")
		gotRT, errRT := s.GetHubSetting(ctx, "runtimes")
		gotPR, errPR := s.GetHubSetting(ctx, "profiles")
		require.NoError(t, errPR)
		if errDelete == nil {
			require.ErrorIs(t, errRT, store.ErrNotFound)
			assert.Equal(t, pr.Revision, gotPR.Revision)
		} else {
			require.NoError(t, errRT)
			assert.Equal(t, rt.Revision, gotRT.Revision)
			assert.Equal(t, pr.Revision+1, gotPR.Revision)
		}
	}
}

// Two saves that merged onto the same runtimes row revision (the runtimes
// compare-and-swap, ptone/scion#6580) and also write profiles never both
// commit: the loser gets a conflict and writes neither section.
func TestUpsertHubSettingsGuardedRuntimesCASRace_Postgres(t *testing.T) {
	s := newPostgresGuardedStore(t)
	ctx := context.Background()
	for i := 0; i < guardedRaceRounds; i++ {
		_ = s.DeleteHubSetting(ctx, "runtimes")
		_ = s.DeleteHubSetting(ctx, "profiles")
		rt, err := s.UpsertHubSetting(ctx, "runtimes", json.RawMessage(`{"a":{"type":"kubernetes"}}`), "admin", 0, "managed")
		require.NoError(t, err)
		save := func(ns string) func() error {
			return func() error {
				_, err := s.UpsertHubSettingsGuarded(ctx, []store.HubSettingWrite{
					{Section: "runtimes", Value: json.RawMessage(`{"a":{"type":"kubernetes","namespace":"` + ns + `"}}`), ExpectedRevision: rt.Revision},
					{Section: "profiles", Value: json.RawMessage(`{"p":{"runtime":"a"}}`), ExpectedRevision: -1},
				}, nil, "admin", "managed")
				return err
			}
		}
		errA, errB := raceGuarded(save("one"), save("two"))
		requireOneWinner(t, errA, errB)
		got, err := s.GetHubSetting(ctx, "runtimes")
		require.NoError(t, err)
		assert.Equal(t, rt.Revision+1, got.Revision, "one write, one revision bump")
		pr, err := s.GetHubSetting(ctx, "profiles")
		require.NoError(t, err)
		assert.Equal(t, int64(1), pr.Revision, "the loser wrote no profiles")
	}
}
