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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Runs on SQLite by default and on Postgres when built with -tags integration
// and SCION_TEST_POSTGRES_URL set (enttest.NewClient), where NULLs would
// otherwise sort first under DESC.
func TestListUsers_LastSeenSortPutsNeverSeenLast(t *testing.T) {
	ctx := context.Background()
	cs := newFixtureTestStore(t)

	newUser := func(name string, lastSeen time.Time) string {
		t.Helper()
		u := &store.User{
			ID:          uuid.NewString(),
			Email:       name + "-" + uuid.NewString()[:8] + "@example.com",
			DisplayName: name,
			Role:        store.UserRoleMember,
			Status:      store.UserStatusActive,
			LastSeen:    lastSeen, // zero leaves last_seen NULL
		}
		require.NoError(t, cs.CreateUser(ctx, u))
		return u.ID
	}

	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	never := newUser("never", time.Time{})
	older := newUser("older", base)
	newer := newUser("newer", base.Add(time.Hour))

	ids := func(dir string) []string {
		t.Helper()
		res, err := cs.ListUsers(ctx, store.UserFilter{}, store.ListOptions{SortBy: "lastSeen", SortDir: dir})
		require.NoError(t, err)
		out := make([]string, 0, len(res.Items))
		for _, u := range res.Items {
			out = append(out, u.ID)
		}
		return out
	}

	require.Equal(t, []string{newer, older, never}, ids("desc"))
	require.Equal(t, []string{older, newer, never}, ids("asc"))
	// No direction defaults to DESC.
	require.Equal(t, []string{newer, older, never}, ids(""))
}
