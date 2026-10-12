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
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Store side of the test identity purge (ptone/scion#4240, Phase 3b).

// ListFixtureRootedResources selects by fixture_root_id only: rows stamped
// with the root (a soft-deleted agent included) are returned; unstamped
// rows owned or created by the root, and rows of another root, are not.
func TestListFixtureRootedResources_SelectsByStampOnly(t *testing.T) {
	cs := newFixtureTestStore(t)
	ctx := context.Background()
	stamp := newStamp()
	other := newStamp()

	proj := newStampTestProject(t, cs, stamp)
	otherProj := newStampTestProject(t, cs, other)
	plainProj := newStampTestProject(t, cs, store.FixtureProvenance{})

	mkAgent := func(projectID string, st store.FixtureProvenance, owner string) string {
		id := uuid.NewString()
		require.NoError(t, cs.CreateAgent(ctx, &store.Agent{ID: id, Slug: "a-" + id[:8], Name: "a-" + id[:8], ProjectID: projectID,
			Phase: "created", CreatedBy: owner, OwnerID: owner, Fixture: st}))
		return id
	}
	stamped := mkAgent(proj.ID, stamp, stamp.RootID)
	softDeleted := mkAgent(proj.ID, stamp, stamp.RootID)
	_, err := cs.DB().ExecContext(ctx, rebind(cs, "UPDATE agents SET deleted_at = ? WHERE id = ?"), time.Now().UTC(), softDeleted)
	require.NoError(t, err)
	mkAgent(plainProj.ID, store.FixtureProvenance{}, stamp.RootID) // legacy: owned by the root, no stamp
	mkAgent(otherProj.ID, other, other.RootID)

	sc := &store.Schedule{ID: uuid.NewString(), ProjectID: proj.ID, Name: "s", CronExpr: "0 * * * *", EventType: "message", Fixture: stamp}
	require.NoError(t, cs.CreateSchedule(ctx, sc))
	require.NoError(t, cs.CreateSchedule(ctx, &store.Schedule{ID: uuid.NewString(), ProjectID: proj.ID, Name: "plain", CronExpr: "0 * * * *", EventType: "message", CreatedBy: stamp.RootID}))
	gid := uuid.NewString()
	require.NoError(t, cs.CreateGroup(ctx, &store.Group{ID: gid, Name: "g", Slug: "g-" + gid[:8], Fixture: stamp}))
	pgid := uuid.NewString()
	require.NoError(t, cs.CreateGroup(ctx, &store.Group{ID: pgid, Name: "pg", Slug: "pg-" + pgid[:8], CreatedBy: stamp.RootID}))

	set, err := cs.ListFixtureRootedResources(ctx, stamp.RootID, 0)
	require.NoError(t, err)
	var agentIDs []string
	for _, a := range set.Agents {
		agentIDs = append(agentIDs, a.ID)
		assert.Equal(t, stamp, a.Fixture)
	}
	assert.ElementsMatch(t, []string{stamped, softDeleted}, agentIDs)
	require.Len(t, set.Schedules, 1)
	assert.Equal(t, sc.ID, set.Schedules[0].ID)
	require.Len(t, set.Groups, 1)
	assert.Equal(t, gid, set.Groups[0].ID)
	require.Len(t, set.Projects, 1)
	assert.Equal(t, proj.ID, set.Projects[0].ID)
	assert.False(t, set.Truncated)

	limited, err := cs.ListFixtureRootedResources(ctx, stamp.RootID, 1)
	require.NoError(t, err)
	assert.Len(t, limited.Agents, 1)
	assert.True(t, limited.Truncated)

	empty, err := cs.ListFixtureRootedResources(ctx, uuid.NewString(), 0)
	require.NoError(t, err)
	assert.Empty(t, empty.Agents)
	assert.Empty(t, empty.Projects)

	_, err = cs.ListFixtureRootedResources(ctx, "not-a-uuid", 0)
	assert.ErrorIs(t, err, store.ErrInvalidInput)
}

// ListExpiredTestFixtureUsers returns only expired test fixtures in
// (expires_at, id) order, honours the ExpiredAfter bound, and pages with
// the keyset cursor.
func TestListExpiredTestFixtureUsers(t *testing.T) {
	cs := newFixtureTestStore(t)
	ctx := context.Background()
	issuer := uuid.NewString()
	now := time.Now().UTC().Truncate(time.Second)
	older := newFixtureUser(issuer, now.Add(-2*time.Hour))
	newer := newFixtureUser(issuer, now.Add(-time.Minute))
	live := newFixtureUser(issuer, now.Add(time.Hour))
	for _, u := range []*store.User{newer, live, older} {
		require.NoError(t, cs.CreateTestFixtureUser(ctx, u))
	}
	require.NoError(t, cs.CreateUser(ctx, &store.User{ID: uuid.NewString(), Email: "human@example.com", Role: store.UserRoleMember, Status: store.UserStatusActive}))

	ids := func(us []store.User) []string {
		out := []string{}
		for _, u := range us {
			out = append(out, u.ID)
		}
		return out
	}
	got, err := cs.ListExpiredTestFixtureUsers(ctx, store.ExpiredTestFixtureQuery{ExpiredAtOrBefore: now})
	require.NoError(t, err)
	assert.Equal(t, []string{older.ID, newer.ID}, ids(got))

	got, err = cs.ListExpiredTestFixtureUsers(ctx, store.ExpiredTestFixtureQuery{ExpiredAtOrBefore: now, Limit: 1})
	require.NoError(t, err)
	assert.Equal(t, []string{older.ID}, ids(got))

	// The hold window: expired within the last 5 minutes only.
	got, err = cs.ListExpiredTestFixtureUsers(ctx, store.ExpiredTestFixtureQuery{ExpiredAtOrBefore: now, ExpiredAfter: now.Add(-5 * time.Minute)})
	require.NoError(t, err)
	assert.Equal(t, []string{newer.ID}, ids(got))

	// The keyset cursor resumes strictly after (expires_at, id).
	got, err = cs.ListExpiredTestFixtureUsers(ctx, store.ExpiredTestFixtureQuery{ExpiredAtOrBefore: now, AfterExpiresAt: *older.ExpiresAt, AfterID: older.ID})
	require.NoError(t, err)
	assert.Equal(t, []string{newer.ID}, ids(got))
	got, err = cs.ListExpiredTestFixtureUsers(ctx, store.ExpiredTestFixtureQuery{ExpiredAtOrBefore: now, AfterExpiresAt: *newer.ExpiresAt, AfterID: newer.ID})
	require.NoError(t, err)
	assert.Empty(t, got)

	// Two identities with the same expiry are ordered, and paged, by ID.
	tieA := newFixtureUser(issuer, now.Add(-30*time.Minute))
	tieB := newFixtureUser(issuer, now.Add(-30*time.Minute))
	require.NoError(t, cs.CreateTestFixtureUser(ctx, tieA))
	require.NoError(t, cs.CreateTestFixtureUser(ctx, tieB))
	first, second := tieA, tieB
	if second.ID < first.ID {
		first, second = second, first
	}
	got, err = cs.ListExpiredTestFixtureUsers(ctx, store.ExpiredTestFixtureQuery{ExpiredAtOrBefore: now, AfterExpiresAt: *first.ExpiresAt, AfterID: first.ID})
	require.NoError(t, err)
	assert.Equal(t, []string{second.ID, newer.ID}, ids(got))

	_, err = cs.ListExpiredTestFixtureUsers(ctx, store.ExpiredTestFixtureQuery{})
	assert.ErrorIs(t, err, store.ErrInvalidInput)
}

// D8 (N4): a finalize that requires a soft-deleted row
// (DeletedAtNotNull) does nothing to a live row, such as one restored
// after the purge listed it, and removes a soft-deleted one.
func TestFinalizeAgentDeletion_DeletedAtNotNullSparesLiveRow(t *testing.T) {
	cs := newFixtureTestStore(t)
	ctx := context.Background()
	proj := newStampTestProject(t, cs, store.FixtureProvenance{})
	mk := func() string {
		id := uuid.NewString()
		require.NoError(t, cs.CreateAgent(ctx, &store.Agent{ID: id, Slug: "a-" + id[:8], Name: "a", ProjectID: proj.ID, Phase: "stopped"}))
		return id
	}
	pred := store.DeletionPredicate{States: []string{store.DeletionStateNone}, DeletedAtNotNull: true}

	live := mk()
	n, err := cs.FinalizeAgentDeletion(ctx, live, pred, store.DeletionFinalizeHard, store.DeletionFields{}, nil)
	require.NoError(t, err)
	assert.Zero(t, n, "a live (restored) row is not removed")
	_, err = cs.GetAgent(ctx, live)
	require.NoError(t, err)

	soft := mk()
	_, err = cs.DB().ExecContext(ctx, rebind(cs, "UPDATE agents SET deleted_at = ? WHERE id = ?"), time.Now().UTC(), soft)
	require.NoError(t, err)
	n, err = cs.FinalizeAgentDeletion(ctx, soft, pred, store.DeletionFinalizeHard, store.DeletionFields{}, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "a soft-deleted row is removed")
	_, err = cs.GetAgent(ctx, soft)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// The 3a deferred items: on agents, projects, schedules and groups a CHECK
// refuses a row with only one of fixture_root_id and fixture_issuer_id set
// (even through raw SQL that bypasses the store), and fixture_root_id and
// fixture_issuer_id are indexed.
func TestFixtureStamp_CheckConstraintAndIndexes(t *testing.T) {
	cs := newFixtureTestStore(t)
	ctx := context.Background()
	stamp := newStamp()
	proj := newStampTestProject(t, cs, stamp)
	agentID := uuid.NewString()
	require.NoError(t, cs.CreateAgent(ctx, &store.Agent{ID: agentID, Slug: "a-" + agentID[:8], Name: "a", ProjectID: proj.ID, Phase: "created", Fixture: stamp}))
	sc := &store.Schedule{ID: uuid.NewString(), ProjectID: proj.ID, Name: "s", CronExpr: "0 * * * *", EventType: "message", Fixture: stamp}
	require.NoError(t, cs.CreateSchedule(ctx, sc))
	gid := uuid.NewString()
	require.NoError(t, cs.CreateGroup(ctx, &store.Group{ID: gid, Name: "g", Slug: "g-" + gid[:8], Fixture: stamp}))

	for table, id := range map[string]string{"agents": agentID, "projects": proj.ID, "schedules": sc.ID, "groups": gid} {
		for _, col := range []string{"fixture_issuer_id", "fixture_root_id"} {
			_, err := cs.DB().ExecContext(ctx, rebind(cs, "UPDATE "+table+" SET "+col+" = NULL WHERE id = ?"), id)
			require.Error(t, err, "%s: clearing %s alone must be refused", table, col)
			assert.Contains(t, strings.ToLower(err.Error()), "check", "%s: %v", table, err)
		}
		// Clearing both is a valid (unstamped) shape for the CHECK.
		_, err := cs.DB().ExecContext(ctx, rebind(cs, "UPDATE "+table+" SET fixture_root_id = NULL, fixture_issuer_id = NULL WHERE id = ?"), id)
		require.NoError(t, err, table)

		for _, col := range []string{"fixture_root_id", "fixture_issuer_id"} {
			assert.True(t, columnIndexed(t, cs, table, col), "%s.%s is indexed", table, col)
		}
	}
}

// rebind rewrites ? placeholders for PostgreSQL.
func rebind(cs *CompositeStore, q string) string {
	if cs.client.Driver().Dialect() != dialect.Postgres {
		return q
	}
	var b strings.Builder
	n := 0
	for _, r := range q {
		if r == '?' {
			n++
			b.WriteString("$")
			b.WriteString(string(rune('0' + n)))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// columnIndexed reports whether some index on table covers col as its
// first column.
func columnIndexed(t *testing.T, cs *CompositeStore, table, col string) bool {
	t.Helper()
	ctx := context.Background()
	if cs.client.Driver().Dialect() == dialect.Postgres {
		var n int
		require.NoError(t, cs.DB().QueryRowContext(ctx,
			"SELECT count(*) FROM pg_indexes WHERE tablename = $1 AND indexdef LIKE $2", table, "%("+col+")%").Scan(&n))
		return n > 0
	}
	rows, err := cs.DB().QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type = 'index' AND tbl_name = ?", table)
	require.NoError(t, err)
	var names []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		names = append(names, name)
	}
	require.NoError(t, rows.Close())
	for _, name := range names {
		var first string
		require.NoError(t, cs.DB().QueryRowContext(ctx, "SELECT name FROM pragma_index_info(?) WHERE seqno = 0", name).Scan(&first))
		if first == col {
			return true
		}
	}
	return false
}
