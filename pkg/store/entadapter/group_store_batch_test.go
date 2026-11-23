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
	"fmt"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

const groupMembershipUserIDIndex = "groupmembership_user_id"

// TestGetEffectiveGroupsForUsers_Parity verifies that GetEffectiveGroupsForUsers
// matches per-user GetEffectiveGroups across direct memberships, multi-level
// nesting, diamond DAGs, cycles, expired memberships, duplicate inputs, and
// users with no memberships. Runs on SQLite and (with -tags integration +
// SCION_TEST_POSTGRES_URL) on Postgres.
func TestGetEffectiveGroupsForUsers_Parity(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	gs := NewGroupStore(client)

	createUser := func(tag string) string {
		id := uuid.New()
		_, err := client.User.Create().
			SetID(id).
			SetEmail(fmt.Sprintf("%s-%s@example.com", tag, id.String()[:8])).
			SetDisplayName(tag).
			Save(ctx)
		require.NoError(t, err)
		return id.String()
	}

	uNone := createUser("none")
	uDirect := createUser("direct")
	uNested := createUser("nested")
	uDiamond := createUser("diamond")
	uExpiredOnly := createUser("expired-only")
	uMixedExpiry := createUser("mixed-expiry")
	uCycle := createUser("cycle")

	mkGroup := func(name string) *store.Group {
		g := &store.Group{
			ID:   uuid.NewString(),
			Name: name,
			Slug: name + "-" + uuid.NewString()[:8],
		}
		require.NoError(t, gs.CreateGroup(ctx, g))
		return g
	}

	// Chain: gTop -> gMid -> gLeaf
	gTop := mkGroup("top")
	gMid := mkGroup("mid")
	gLeaf := mkGroup("leaf")
	addChildGroup(t, gs, gTop, gMid)
	addChildGroup(t, gs, gMid, gLeaf)

	// Diamond: gRoot -> gLeft, gRight -> gBottom
	gRoot := mkGroup("root")
	gLeft := mkGroup("left")
	gRight := mkGroup("right")
	gBottom := mkGroup("bottom")
	addChildGroup(t, gs, gRoot, gLeft)
	addChildGroup(t, gs, gRoot, gRight)
	addChildGroup(t, gs, gLeft, gBottom)
	addChildGroup(t, gs, gRight, gBottom)

	// Cycle: gCycA <-> gCycB
	gCycA := mkGroup("cyc-a")
	gCycB := mkGroup("cyc-b")
	addChildGroup(t, gs, gCycA, gCycB)
	addChildGroup(t, gs, gCycB, gCycA)

	// Expired group target: gExpParent -> gExpChild
	gExpParent := mkGroup("exp-parent")
	gExpChild := mkGroup("exp-child")
	addChildGroup(t, gs, gExpParent, gExpChild)

	past := time.Now().UTC().Add(-time.Hour)
	future := time.Now().UTC().Add(time.Hour)

	addMember := func(groupID, userID string, exp *time.Time) {
		require.NoError(t, gs.AddGroupMember(ctx, &store.GroupMember{
			GroupID:    groupID,
			MemberType: store.GroupMemberTypeUser,
			MemberID:   userID,
			Role:       store.GroupMemberRoleMember,
			ExpiresAt:  exp,
		}))
	}

	addMember(gTop.ID, uDirect, nil)
	addMember(gLeaf.ID, uNested, &future)
	addMember(gBottom.ID, uDiamond, nil)
	addMember(gLeft.ID, uDiamond, nil) // overlapping direct + transitive
	addMember(gExpChild.ID, uExpiredOnly, &past)
	addMember(gExpChild.ID, uMixedExpiry, &past)
	addMember(gMid.ID, uMixedExpiry, &future)
	addMember(gCycA.ID, uCycle, nil)

	// Empty input.
	emptyRes, err := gs.GetEffectiveGroupsForUsers(ctx, nil)
	require.NoError(t, err)
	assert.Empty(t, emptyRes)

	// Malformed UUID fails closed.
	_, err = gs.GetEffectiveGroupsForUsers(ctx, []string{uDirect, "not-a-uuid"})
	assert.Error(t, err)

	// Full batch including duplicate user ID.
	allUsers := []string{uNone, uDirect, uNested, uDiamond, uExpiredOnly, uMixedExpiry, uCycle, uDirect}
	got, err := gs.GetEffectiveGroupsForUsers(ctx, allUsers)
	require.NoError(t, err)
	require.Len(t, got, 7, "deduplicated user keys")

	for _, uid := range []string{uNone, uDirect, uNested, uDiamond, uExpiredOnly, uMixedExpiry, uCycle} {
		want, serr := gs.GetEffectiveGroups(ctx, uid)
		require.NoError(t, serr)
		assert.ElementsMatch(t, want, got[uid], "parity mismatch for user %s", uid)
	}

	assert.Empty(t, got[uNone])
	assert.ElementsMatch(t, []string{gTop.ID}, got[uDirect])
	assert.ElementsMatch(t, []string{gLeaf.ID, gMid.ID, gTop.ID}, got[uNested])
	assert.ElementsMatch(t, []string{gBottom.ID, gLeft.ID, gRight.ID, gRoot.ID}, got[uDiamond])
	assert.Empty(t, got[uExpiredOnly], "expired membership must not seed any group")
	assert.ElementsMatch(t, []string{gMid.ID, gTop.ID}, got[uMixedExpiry], "only active membership seeds")
	assert.ElementsMatch(t, []string{gCycA.ID, gCycB.ID}, got[uCycle])
}

// TestGetEffectiveGroupsForUsers_Chunking verifies that batches exceeding the
// 500-user chunk size span multiple chunks cleanly with two queries per chunk
// and match GetEffectiveGroups for users in every chunk.
func TestGetEffectiveGroupsForUsers_Chunking(t *testing.T) {
	ctx := context.Background()
	base := enttest.NewClient(t)
	rec := &planRecordingDriver{Driver: base.Driver()}
	gs := NewGroupStore(ent.NewClient(ent.Driver(rec)))

	gParent := &store.Group{ID: uuid.NewString(), Name: "Chunk Parent", Slug: "chunk-parent-" + uuid.NewString()[:8]}
	gChild := &store.Group{ID: uuid.NewString(), Name: "Chunk Child", Slug: "chunk-child-" + uuid.NewString()[:8]}
	require.NoError(t, gs.CreateGroup(ctx, gParent))
	require.NoError(t, gs.CreateGroup(ctx, gChild))
	addChildGroup(t, gs, gParent, gChild)

	const totalUsers = 550
	userIDs := make([]string, totalUsers)
	for i := 0; i < totalUsers; i++ {
		uid := uuid.New()
		_, err := base.User.Create().
			SetID(uid).
			SetEmail(fmt.Sprintf("chunk-u-%d-%s@example.com", i, uid.String()[:8])).
			SetDisplayName(fmt.Sprintf("Chunk User %d", i)).
			Save(ctx)
		require.NoError(t, err)
		userIDs[i] = uid.String()
		if i%2 == 0 {
			target := gParent.ID
			if i%4 == 0 {
				target = gChild.ID
			}
			require.NoError(t, gs.AddGroupMember(ctx, &store.GroupMember{
				GroupID:    target,
				MemberType: store.GroupMemberTypeUser,
				MemberID:   userIDs[i],
				Role:       store.GroupMemberRoleMember,
			}))
		}
	}

	rec.mu.Lock()
	rec.queries = nil
	rec.mu.Unlock()

	got, err := gs.GetEffectiveGroupsForUsers(ctx, userIDs)
	require.NoError(t, err)
	require.Len(t, got, totalUsers)

	rec.mu.Lock()
	queryCount := len(rec.queries)
	rec.mu.Unlock()
	// 550 users -> 2 chunks (500 + 50), each chunk issues 1 membership query + 1 CTE.
	assert.Equal(t, 4, queryCount, "expected exactly 2 queries per 500-user chunk")

	// Spot-check users in both chunks (grouped, nested, ungrouped).
	for _, idx := range []int{0, 1, 2, 499, 500, 501, 502, 548, 549} {
		want, serr := gs.GetEffectiveGroups(ctx, userIDs[idx])
		require.NoError(t, serr)
		assert.ElementsMatch(t, want, got[userIDs[idx]], "user index %d", idx)
	}
}

// TestGroupMembershipUserIDIndex_AutoMigrateAndPlanShape verifies that
// AutoMigrate creates groupmembership_user_id on fresh and existing databases
// and that GetEffectiveGroupsForUsers uses it (no full table scan on
// group_memberships).
func TestGroupMembershipUserIDIndex_AutoMigrateAndPlanShape(t *testing.T) {
	ctx := context.Background()
	base := enttest.NewClient(t)

	// Verify AutoMigrate recreates the index when missing on an existing DB.
	require.NoError(t, base.Driver().Exec(ctx, "DROP INDEX "+groupMembershipUserIDIndex, []any{}, nil))
	require.NoError(t, entc.AutoMigrate(ctx, base))

	rec := &planRecordingDriver{Driver: base.Driver()}
	gs := NewGroupStore(ent.NewClient(ent.Driver(rec)))

	g := &store.Group{ID: uuid.NewString(), Name: "Plan Group", Slug: "plan-grp-" + uuid.NewString()[:8]}
	require.NoError(t, gs.CreateGroup(ctx, g))

	userIDs := make([]string, 0, 200)
	for i := 0; i < 200; i++ {
		uid := uuid.New()
		_, err := base.User.Create().
			SetID(uid).
			SetEmail(fmt.Sprintf("plan-u-%d-%s@example.com", i, uid.String()[:8])).
			SetDisplayName(fmt.Sprintf("Plan User %d", i)).
			Save(ctx)
		require.NoError(t, err)
		require.NoError(t, gs.AddGroupMember(ctx, &store.GroupMember{
			GroupID:    g.ID,
			MemberType: store.GroupMemberTypeUser,
			MemberID:   uid.String(),
			Role:       store.GroupMemberRoleMember,
		}))
		userIDs = append(userIDs, uid.String())
	}

	probeIDs := userIDs[:5]
	rec.mu.Lock()
	rec.queries = nil
	rec.mu.Unlock()

	_, err := gs.GetEffectiveGroupsForUsers(ctx, probeIDs)
	require.NoError(t, err)

	rec.mu.Lock()
	queries := append([]recordedQuery(nil), rec.queries...)
	rec.mu.Unlock()
	require.GreaterOrEqual(t, len(queries), 1)
	membershipQuery := queries[0]
	require.Contains(t, membershipQuery.sql, "group_memberships")

	if base.Driver().Dialect() == dialect.Postgres {
		// On a 200-row table Postgres naturally prefers Seq Scan (2 heap pages).
		// Use SET LOCAL enable_seqscan = off inside a read-only transaction to
		// verify the planner can and does select groupmembership_user_id.
		require.NoError(t, base.Driver().Exec(ctx, "ANALYZE group_memberships", []any{}, nil))
		tx, err := base.Driver().Tx(ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		require.NoError(t, tx.Exec(ctx, "SET LOCAL enable_seqscan = off", []any{}, nil))
		rows := &entsql.Rows{}
		require.NoError(t, tx.Query(ctx, "EXPLAIN "+membershipQuery.sql, membershipQuery.args, rows))
		var plan strings.Builder
		for rows.Next() {
			var line string
			require.NoError(t, rows.Scan(&line))
			plan.WriteString(line)
			plan.WriteByte('\n')
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
		t.Logf("Postgres EXPLAIN (enable_seqscan=off) for GetEffectiveGroupsForUsers:\n%s", plan.String())
		assert.Contains(t, plan.String(), groupMembershipUserIDIndex, "Postgres plan should use groupmembership_user_id")
		return
	}

	// SQLite EXPLAIN QUERY PLAN.
	rows := &entsql.Rows{}
	require.NoError(t, base.Driver().Query(ctx, "EXPLAIN QUERY PLAN "+membershipQuery.sql, membershipQuery.args, rows))
	var plan strings.Builder
	for rows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
		plan.WriteString(detail)
		plan.WriteByte('\n')
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	t.Logf("SQLite EXPLAIN QUERY PLAN for GetEffectiveGroupsForUsers:\n%s", plan.String())
	assert.Contains(t, plan.String(), groupMembershipUserIDIndex, "SQLite plan must use groupmembership_user_id")
	assert.NotContains(t, plan.String(), "SCAN group_memberships", "SQLite plan must not full-scan group_memberships")
}
