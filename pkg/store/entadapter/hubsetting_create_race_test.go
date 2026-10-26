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
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// fakeSQLiteError stands in for modernc.org/sqlite's *sqlite.Error, which
// isUniqueViolation matches through its Code method.
type fakeSQLiteError struct{ code int }

func (e *fakeSQLiteError) Error() string { return fmt.Sprintf("sqlite error %d", e.code) }
func (e *fakeSQLiteError) Code() int     { return e.code }

// Only a unique violation on the section key is a lost create race; other
// constraint errors are not (ptone/scion#5772).
func TestIsHubSettingSectionConflict(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain error", errors.New("boom"), false},
		{"postgres section unique index", &pgconn.PgError{Code: "23505", ConstraintName: "hubsetting_section"}, true},
		{"postgres section unique column", &pgconn.PgError{Code: "23505", ConstraintName: "hub_settings_section_key"}, true},
		{"postgres other unique", &pgconn.PgError{Code: "23505", ConstraintName: "hub_settings_pkey"}, false},
		{"postgres unique without name", &pgconn.PgError{Code: "23505"}, false},
		{"postgres check", &pgconn.PgError{Code: "23514", ConstraintName: "hubsetting_section"}, false},
		{"postgres foreign key", &pgconn.PgError{Code: "23503"}, false},
		{"postgres wrapped", fmt.Errorf("create: %w", &pgconn.PgError{Code: "23505", ConstraintName: "hubsetting_section"}), true},
		{"sqlite unique", &fakeSQLiteError{code: sqliteConstraintUnique}, true},
		{"sqlite check", &fakeSQLiteError{code: 275}, false},       // SQLITE_CONSTRAINT_CHECK
		{"sqlite foreign key", &fakeSQLiteError{code: 787}, false}, // SQLITE_CONSTRAINT_FOREIGNKEY
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, isHubSettingSectionConflict(c.err))
		})
	}
}

// retryHubSettingCreateRace retries a lost create race only for -1, and
// reports an exhausted bound as a wrapped ErrRevisionConflict naming the
// section and the attempt count (ptone/scion#5772).
func TestRetryHubSettingCreateRace(t *testing.T) {
	alwaysLoses := func(calls *int) func() (*store.HubSetting, error) {
		return func() (*store.HubSetting, error) {
			*calls++
			return nil, errHubSettingCreateRace
		}
	}

	t.Run("exhausted -1 is wrapped", func(t *testing.T) {
		calls := 0
		_, err := retryHubSettingCreateRace("lifecycle", -1, alwaysLoses(&calls))
		require.Error(t, err)
		assert.Equal(t, upsertCreateRaceAttempts, calls)
		assert.ErrorIs(t, err, store.ErrRevisionConflict)
		assert.NotErrorIs(t, err, errHubSettingCreateRace)
		assert.Contains(t, err.Error(), `"lifecycle"`)
		assert.Contains(t, err.Error(), fmt.Sprintf("%d attempts", upsertCreateRaceAttempts))
	})

	t.Run("-1 succeeds after a lost race", func(t *testing.T) {
		calls := 0
		row, err := retryHubSettingCreateRace("lifecycle", -1, func() (*store.HubSetting, error) {
			calls++
			if calls == 1 {
				return nil, errHubSettingCreateRace
			}
			return &store.HubSetting{Section: "lifecycle", Revision: 2}, nil
		})
		require.NoError(t, err)
		assert.Equal(t, 2, calls)
		assert.Equal(t, int64(2), row.Revision)
	})

	for _, rev := range []int64{0, 3} {
		t.Run(fmt.Sprintf("expected revision %d is not retried", rev), func(t *testing.T) {
			calls := 0
			_, err := retryHubSettingCreateRace("lifecycle", rev, alwaysLoses(&calls))
			assert.Equal(t, 1, calls)
			assert.True(t, errors.Is(err, store.ErrRevisionConflict))
			assert.Equal(t, store.ErrRevisionConflict, err, "a CAS or create-only conflict stays bare")
		})
	}

	t.Run("other errors pass through unretried", func(t *testing.T) {
		calls := 0
		other := errors.New("upsert hub setting: create: check constraint")
		_, err := retryHubSettingCreateRace("lifecycle", -1, func() (*store.HubSetting, error) {
			calls++
			return nil, other
		})
		assert.Equal(t, 1, calls)
		assert.Equal(t, other, err)
		assert.NotErrorIs(t, err, store.ErrRevisionConflict)
	})
}
