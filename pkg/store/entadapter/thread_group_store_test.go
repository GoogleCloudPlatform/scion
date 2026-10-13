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
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	entgo "entgo.io/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/conversation"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/threadgroup"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/threadgroupmember"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Thread group store tests (ptone/scion#4424, design section 8 phase 1).
// They use enttest.NewClient, so they run on SQLite by default and on
// Postgres in the integration job (make test-launch-store-postgres runs the
// full entadapter suite there).

func newThreadGroupTestStore(t *testing.T) *CompositeStore {
	t.Helper()
	return NewCompositeStore(enttest.NewClient(t))
}

// newGroupConversation creates a live group conversation in projectID.
func newGroupConversation(t *testing.T, cs *CompositeStore, projectID string) string {
	t.Helper()
	pid := projectID
	conv := &store.Conversation{
		ID:          uuid.NewString(),
		ProjectID:   &pid,
		Kind:        "group",
		Surface:     "native",
		ExternalRef: "thread:" + uuid.NewString(),
	}
	require.NoError(t, cs.CreateConversation(context.Background(), conv))
	return conv.ID
}

func newThreadGroup(t *testing.T, cs *CompositeStore, projectID, name string) *store.ThreadGroup {
	t.Helper()
	g := &store.ThreadGroup{
		ProjectID:     projectID,
		Name:          name,
		CreatedBy:     "user-1",
		CreatedByKind: store.ThreadGroupCreatorUser,
	}
	require.NoError(t, cs.CreateThreadGroup(context.Background(), g))
	return g
}

// memberRows counts thread_group_members rows for a conversation, read
// straight from the table (no live-conversation filter).
func memberRows(t *testing.T, cs *CompositeStore, conversationID string) int {
	t.Helper()
	n, err := cs.client.ThreadGroupMember.Query().
		Where(threadgroupmember.ConversationIDEQ(uuid.MustParse(conversationID))).
		Count(context.Background())
	require.NoError(t, err)
	return n
}

func groupMemberRows(t *testing.T, cs *CompositeStore, groupID string) int {
	t.Helper()
	n, err := cs.client.ThreadGroupMember.Query().
		Where(threadgroupmember.GroupIDEQ(uuid.MustParse(groupID))).
		Count(context.Background())
	require.NoError(t, err)
	return n
}

func memberConversationIDs(g *store.ThreadGroup) []string {
	out := make([]string, 0, len(g.Members))
	for _, m := range g.Members {
		out = append(out, m.ConversationID)
	}
	return out
}

func threadGroupTableExists(t *testing.T, cs *CompositeStore, table string) bool {
	t.Helper()
	ok, err := tableExists(context.Background(), cs.client, cs.DB(), table)
	require.NoError(t, err)
	return ok
}

// --- Auto-migration ----------------------------------------------------------

func TestThreadGroupMigrate_FreshDatabase(t *testing.T) {
	cs := newThreadGroupTestStore(t)
	require.NoError(t, cs.Migrate(context.Background()))
	assert.True(t, threadGroupTableExists(t, cs, "thread_groups"))
	assert.True(t, threadGroupTableExists(t, cs, "thread_group_members"))
}

// TestThreadGroupMigrate_ExistingDatabase models a hub database from before
// thread groups: the two tables are dropped from a populated database, and
// the hub's Migrate must create them again without touching existing rows.
func TestThreadGroupMigrate_ExistingDatabase(t *testing.T) {
	ctx := context.Background()
	cs := newThreadGroupTestStore(t)
	projectID := uuid.NewString()
	convID := newGroupConversation(t, cs, projectID)

	for _, table := range []string{"thread_group_members", "thread_groups"} {
		_, err := cs.DB().ExecContext(ctx, "DROP TABLE "+table)
		require.NoError(t, err)
		require.False(t, threadGroupTableExists(t, cs, table), "%s dropped", table)
	}

	require.NoError(t, cs.Migrate(ctx))
	assert.True(t, threadGroupTableExists(t, cs, "thread_groups"))
	assert.True(t, threadGroupTableExists(t, cs, "thread_group_members"))

	_, err := cs.GetConversation(ctx, convID)
	require.NoError(t, err, "existing conversation survives the migration")

	// The recreated tables carry their unique indexes.
	g := newThreadGroup(t, cs, projectID, "Release")
	err = cs.CreateThreadGroup(ctx, &store.ThreadGroup{ProjectID: projectID, Name: "release", CreatedByKind: store.ThreadGroupCreatorUser})
	require.ErrorIs(t, err, store.ErrAlreadyExists)
	require.NoError(t, cs.SetThreadGroupMember(ctx, g.ID, convID, "user-1"))
	_, err = cs.client.ThreadGroupMember.Create().
		SetGroupID(uuid.MustParse(g.ID)).
		SetProjectID(uuid.MustParse(projectID)).
		SetConversationID(uuid.MustParse(convID)).
		SetAddedBy("user-2").
		Save(ctx)
	require.True(t, ent.IsConstraintError(err), "unique conversation_id index present, got %v", err)

	// A second Migrate is a no-op.
	require.NoError(t, cs.Migrate(ctx))
	got, err := cs.GetThreadGroup(ctx, g.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{convID}, memberConversationIDs(got))
}

// --- Create, get, list -------------------------------------------------------

func TestThreadGroupCreateGetList(t *testing.T) {
	ctx := context.Background()
	cs := newThreadGroupTestStore(t)
	projectID := uuid.NewString()

	g := newThreadGroup(t, cs, projectID, "  Release Notes  ")
	require.NotEmpty(t, g.ID)
	assert.Equal(t, "Release Notes", g.Name, "stored trimmed")
	assert.Equal(t, "release notes", g.NameKey)
	assert.False(t, g.CreatedAt.IsZero())
	assert.Equal(t, g.CreatedAt, g.UpdatedAt)
	assert.NotNil(t, g.Members)

	got, err := cs.GetThreadGroup(ctx, g.ID)
	require.NoError(t, err)
	assert.Equal(t, g.ID, got.ID)
	assert.Equal(t, projectID, got.ProjectID)
	assert.Equal(t, "Release Notes", got.Name)
	assert.Equal(t, "user-1", got.CreatedBy)
	assert.Equal(t, store.ThreadGroupCreatorUser, got.CreatedByKind)
	assert.NotNil(t, got.Members)
	assert.Empty(t, got.Members)

	// A caller-supplied ID is kept; agents can create groups.
	agentGroup := &store.ThreadGroup{ID: uuid.NewString(), ProjectID: projectID, Name: "agents", CreatedBy: "agent-1", CreatedByKind: store.ThreadGroupCreatorAgent}
	wantID := agentGroup.ID
	require.NoError(t, cs.CreateThreadGroup(ctx, agentGroup))
	assert.Equal(t, wantID, agentGroup.ID)

	newThreadGroup(t, cs, projectID, "beta")
	newThreadGroup(t, cs, uuid.NewString(), "other project")

	list, err := cs.ListThreadGroups(ctx, projectID)
	require.NoError(t, err)
	names := make([]string, 0, len(list))
	for _, lg := range list {
		names = append(names, lg.Name)
		assert.NotNil(t, lg.Members)
	}
	assert.Equal(t, []string{"agents", "beta", "Release Notes"}, names, "sorted by name key, project-scoped")

	_, err = cs.GetThreadGroup(ctx, uuid.NewString())
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = cs.GetThreadGroup(ctx, "not-a-uuid")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestThreadGroupList_EmptyProjectIsNonNil(t *testing.T) {
	cs := newThreadGroupTestStore(t)
	list, err := cs.ListThreadGroups(context.Background(), uuid.NewString())
	require.NoError(t, err)
	require.NotNil(t, list)
	assert.Empty(t, list)
}

// TestThreadGroupList_ByteOrder pins section 7c's order as byte order of
// name_key on every dialect: a collation-aware database sort would put
// "équipe" among the e's and ignore the hyphen in "a-b".
func TestThreadGroupList_ByteOrder(t *testing.T) {
	cs := newThreadGroupTestStore(t)
	projectID := uuid.NewString()
	for _, name := range []string{"zeta", "Équipe", "ab", "a-b", "Echo", "a b"} {
		newThreadGroup(t, cs, projectID, name)
	}
	list, err := cs.ListThreadGroups(context.Background(), projectID)
	require.NoError(t, err)
	keys := make([]string, 0, len(list))
	for _, g := range list {
		keys = append(keys, g.NameKey)
	}
	assert.Equal(t, []string{"a b", "a-b", "ab", "echo", "zeta", "équipe"}, keys)
}

func TestThreadGroupCreate_Validation(t *testing.T) {
	ctx := context.Background()
	cs := newThreadGroupTestStore(t)
	projectID := uuid.NewString()

	cases := map[string]*store.ThreadGroup{
		"empty name":        {ProjectID: projectID, Name: "", CreatedByKind: "user"},
		"blank name":        {ProjectID: projectID, Name: "   ", CreatedByKind: "user"},
		"control character": {ProjectID: projectID, Name: "a\x07b", CreatedByKind: "user"},
		"newline":           {ProjectID: projectID, Name: "a\nb", CreatedByKind: "user"},
		"101 runes":         {ProjectID: projectID, Name: strings.Repeat("é", 101), CreatedByKind: "user"},
		"missing project":   {Name: "x", CreatedByKind: "user"},
		"bad project":       {ProjectID: "nope", Name: "x", CreatedByKind: "user"},
		"bad creator kind":  {ProjectID: projectID, Name: "x", CreatedByKind: "robot"},
		"bad id":            {ID: "nope", ProjectID: projectID, Name: "x", CreatedByKind: "user"},
	}
	for name, g := range cases {
		t.Run(name, func(t *testing.T) {
			assert.ErrorIs(t, cs.CreateThreadGroup(ctx, g), store.ErrInvalidInput)
		})
	}
	assert.ErrorIs(t, cs.CreateThreadGroup(ctx, nil), store.ErrInvalidInput)

	// 100 runes is the limit, counted in runes, not bytes.
	newThreadGroup(t, cs, projectID, strings.Repeat("é", 100))

	n, err := cs.client.ThreadGroup.Query().Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "no invalid group was written")
}

// --- Name uniqueness ---------------------------------------------------------

func TestThreadGroupName_CaseInsensitiveUniquePerProject(t *testing.T) {
	ctx := context.Background()
	cs := newThreadGroupTestStore(t)
	projectA, projectB := uuid.NewString(), uuid.NewString()

	newThreadGroup(t, cs, projectA, "Release")
	newThreadGroup(t, cs, projectA, "Équipe")

	for _, dup := range []string{"Release", "release", "RELEASE", "  rElEaSe ", "équipe", "ÉQUIPE"} {
		err := cs.CreateThreadGroup(ctx, &store.ThreadGroup{ProjectID: projectA, Name: dup, CreatedByKind: "user"})
		assert.ErrorIs(t, err, store.ErrAlreadyExists, "%q conflicts", dup)
	}

	// Another project may reuse the name.
	newThreadGroup(t, cs, projectB, "RELEASE")

	list, err := cs.ListThreadGroups(ctx, projectA)
	require.NoError(t, err)
	assert.Len(t, list, 2)
}

func TestThreadGroupRename(t *testing.T) {
	ctx := context.Background()
	cs := newThreadGroupTestStore(t)
	projectID := uuid.NewString()
	g := newThreadGroup(t, cs, projectID, "Release")
	other := newThreadGroup(t, cs, projectID, "Backlog")
	// Compare stored values with stored values: Postgres keeps microseconds.
	before, err := cs.GetThreadGroup(ctx, g.ID)
	require.NoError(t, err)

	time.Sleep(5 * time.Millisecond)
	require.NoError(t, cs.RenameThreadGroup(ctx, g.ID, "  Shipping  "))
	got, err := cs.GetThreadGroup(ctx, g.ID)
	require.NoError(t, err)
	assert.Equal(t, "Shipping", got.Name)
	assert.Equal(t, "shipping", got.NameKey)
	assert.True(t, got.UpdatedAt.After(before.UpdatedAt), "updated_at bumped")
	assert.True(t, got.CreatedAt.Equal(before.CreatedAt), "created_at unchanged")

	// A case-only rename of the same group is allowed.
	require.NoError(t, cs.RenameThreadGroup(ctx, g.ID, "SHIPPING"))
	// The old name is free again.
	newThreadGroup(t, cs, projectID, "release")

	assert.ErrorIs(t, cs.RenameThreadGroup(ctx, other.ID, "shipping"), store.ErrAlreadyExists)
	assert.ErrorIs(t, cs.RenameThreadGroup(ctx, other.ID, ""), store.ErrInvalidInput)
	assert.ErrorIs(t, cs.RenameThreadGroup(ctx, uuid.NewString(), "x"), store.ErrNotFound)

	got, err = cs.GetThreadGroup(ctx, other.ID)
	require.NoError(t, err)
	assert.Equal(t, "Backlog", got.Name, "failed renames write nothing")
}

// --- Membership --------------------------------------------------------------

func TestThreadGroupSetMember_MovesAtomically(t *testing.T) {
	ctx := context.Background()
	cs := newThreadGroupTestStore(t)
	projectID := uuid.NewString()
	g1 := newThreadGroup(t, cs, projectID, "one")
	g2 := newThreadGroup(t, cs, projectID, "two")
	conv := newGroupConversation(t, cs, projectID)

	require.NoError(t, cs.SetThreadGroupMember(ctx, g1.ID, conv, "user-1"))
	got, err := cs.GetThreadGroup(ctx, g1.ID)
	require.NoError(t, err)
	require.Len(t, got.Members, 1)
	m := got.Members[0]
	assert.Equal(t, conv, m.ConversationID)
	assert.Equal(t, g1.ID, m.GroupID)
	assert.Equal(t, projectID, m.ProjectID, "project_id copied from the group")
	assert.Equal(t, "user-1", m.AddedBy)

	// Re-adding to the same group is a no-op that keeps added_at.
	time.Sleep(5 * time.Millisecond)
	require.NoError(t, cs.SetThreadGroupMember(ctx, g1.ID, conv, "user-2"))
	again, err := cs.GetThreadGroup(ctx, g1.ID)
	require.NoError(t, err)
	require.Len(t, again.Members, 1)
	assert.True(t, again.Members[0].AddedAt.Equal(m.AddedAt))
	assert.Equal(t, "user-1", again.Members[0].AddedBy)

	// Adding to another group moves it: one row, now in g2.
	require.NoError(t, cs.SetThreadGroupMember(ctx, g2.ID, conv, "agent-7"))
	assert.Equal(t, 1, memberRows(t, cs, conv))
	from, err := cs.GetThreadGroup(ctx, g1.ID)
	require.NoError(t, err)
	assert.Empty(t, from.Members)
	to, err := cs.GetThreadGroup(ctx, g2.ID)
	require.NoError(t, err)
	require.Len(t, to.Members, 1)
	assert.Equal(t, conv, to.Members[0].ConversationID)
	assert.Equal(t, "agent-7", to.Members[0].AddedBy)
}

// TestThreadGroupSetMember_ConcurrentMovesLeaveOneRow files one conversation
// into several groups at once; the unique conversation_id index and the
// upsert leave exactly one membership.
func TestThreadGroupSetMember_ConcurrentMovesLeaveOneRow(t *testing.T) {
	ctx := context.Background()
	cs := newThreadGroupTestStore(t)
	projectID := uuid.NewString()
	conv := newGroupConversation(t, cs, projectID)
	groups := make([]string, 4)
	for i := range groups {
		groups[i] = newThreadGroup(t, cs, projectID, "g"+string(rune('a'+i))).ID
	}

	var wg sync.WaitGroup
	errs := make(chan error, len(groups)*5)
	for round := 0; round < 5; round++ {
		for _, gid := range groups {
			wg.Add(1)
			go func(gid string) {
				defer wg.Done()
				if err := cs.SetThreadGroupMember(ctx, gid, conv, "user-1"); err != nil && !errors.Is(err, store.ErrTransient) {
					errs <- err
				}
			}(gid)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("SetThreadGroupMember: %v", err)
	}
	assert.Equal(t, 1, memberRows(t, cs, conv))

	list, err := cs.ListThreadGroups(ctx, projectID)
	require.NoError(t, err)
	filed := 0
	for _, g := range list {
		filed += len(g.Members)
	}
	assert.Equal(t, 1, filed, "the thread shows in exactly one group")
}

func TestThreadGroupSetMember_MissingGroupWritesNothing(t *testing.T) {
	ctx := context.Background()
	cs := newThreadGroupTestStore(t)
	projectID := uuid.NewString()
	g := newThreadGroup(t, cs, projectID, "one")
	conv := newGroupConversation(t, cs, projectID)
	require.NoError(t, cs.SetThreadGroupMember(ctx, g.ID, conv, "user-1"))

	assert.ErrorIs(t, cs.SetThreadGroupMember(ctx, uuid.NewString(), conv, "user-1"), store.ErrNotFound)
	assert.ErrorIs(t, cs.SetThreadGroupMember(ctx, "not-a-uuid", conv, "user-1"), store.ErrNotFound)
	assert.ErrorIs(t, cs.SetThreadGroupMember(ctx, g.ID, "not-a-uuid", "user-1"), store.ErrInvalidInput)

	got, err := cs.GetThreadGroup(ctx, g.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{conv}, memberConversationIDs(got), "failed move left the thread where it was")
}

// TestThreadGroupSetMember_GroupDeletedInSameTransaction is edge case 7d:
// the group check runs in the write's own transaction, so a group deleted
// earlier in that transaction answers ErrNotFound and nothing is written.
func TestThreadGroupSetMember_GroupDeletedInSameTransaction(t *testing.T) {
	ctx := context.Background()
	cs := newThreadGroupTestStore(t)
	projectID := uuid.NewString()
	g := newThreadGroup(t, cs, projectID, "one")
	conv := newGroupConversation(t, cs, projectID)

	err := cs.WithTx(ctx, func(tx store.Store) error {
		require.NoError(t, tx.DeleteThreadGroup(ctx, g.ID))
		return tx.SetThreadGroupMember(ctx, g.ID, conv, "user-1")
	})
	require.ErrorIs(t, err, store.ErrNotFound)
	assert.Zero(t, memberRows(t, cs, conv))
	_, err = cs.GetThreadGroup(ctx, g.ID)
	require.NoError(t, err, "the failed transaction rolled the delete back")
}

func TestThreadGroupRemoveMember(t *testing.T) {
	ctx := context.Background()
	cs := newThreadGroupTestStore(t)
	projectID := uuid.NewString()
	g1 := newThreadGroup(t, cs, projectID, "one")
	g2 := newThreadGroup(t, cs, projectID, "two")
	c1 := newGroupConversation(t, cs, projectID)
	c2 := newGroupConversation(t, cs, projectID)
	require.NoError(t, cs.SetThreadGroupMember(ctx, g1.ID, c1, "u"))
	require.NoError(t, cs.SetThreadGroupMember(ctx, g1.ID, c2, "u"))

	// Removing from a group the thread is not in changes nothing.
	require.NoError(t, cs.RemoveThreadGroupMember(ctx, g2.ID, c1))
	assert.Equal(t, 1, memberRows(t, cs, c1))

	require.NoError(t, cs.RemoveThreadGroupMember(ctx, g1.ID, c1))
	require.NoError(t, cs.RemoveThreadGroupMember(ctx, g1.ID, c1), "idempotent")
	require.NoError(t, cs.RemoveThreadGroupMember(ctx, "not-a-uuid", c2), "malformed group ID matches nothing")
	require.ErrorIs(t, cs.RemoveThreadGroupMember(ctx, g1.ID, "not-a-uuid"), store.ErrInvalidInput)
	got, err := cs.GetThreadGroup(ctx, g1.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{c2}, memberConversationIDs(got))
}

func TestThreadGroupMembers_OrderedByAddedAt(t *testing.T) {
	ctx := context.Background()
	cs := newThreadGroupTestStore(t)
	projectID := uuid.NewString()
	g := newThreadGroup(t, cs, projectID, "one")
	var want []string
	for i := 0; i < 3; i++ {
		c := newGroupConversation(t, cs, projectID)
		require.NoError(t, cs.SetThreadGroupMember(ctx, g.ID, c, "u"))
		want = append(want, c)
		time.Sleep(5 * time.Millisecond)
	}
	list, err := cs.ListThreadGroups(ctx, projectID)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, want, memberConversationIDs(&list[0]))
}

// --- Deletes and cleanup -----------------------------------------------------

func TestThreadGroupDelete_RemovesMembers(t *testing.T) {
	ctx := context.Background()
	cs := newThreadGroupTestStore(t)
	projectID := uuid.NewString()
	g := newThreadGroup(t, cs, projectID, "doomed")
	keep := newThreadGroup(t, cs, projectID, "kept")
	c1 := newGroupConversation(t, cs, projectID)
	c2 := newGroupConversation(t, cs, projectID)
	c3 := newGroupConversation(t, cs, projectID)
	require.NoError(t, cs.SetThreadGroupMember(ctx, g.ID, c1, "u"))
	require.NoError(t, cs.SetThreadGroupMember(ctx, g.ID, c2, "u"))
	require.NoError(t, cs.SetThreadGroupMember(ctx, keep.ID, c3, "u"))

	require.NoError(t, cs.DeleteThreadGroup(ctx, g.ID))
	assert.Zero(t, groupMemberRows(t, cs, g.ID))
	assert.Equal(t, 1, groupMemberRows(t, cs, keep.ID))
	_, err := cs.GetThreadGroup(ctx, g.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	for _, c := range []string{c1, c2} {
		_, err := cs.GetConversation(ctx, c)
		assert.NoError(t, err, "member threads are not deleted")
	}

	assert.ErrorIs(t, cs.DeleteThreadGroup(ctx, g.ID), store.ErrNotFound)
	assert.ErrorIs(t, cs.DeleteThreadGroup(ctx, "not-a-uuid"), store.ErrNotFound)
}

// failThreadGroupMemberDeletes makes every thread_group_members delete on
// cs's client (and on transactions opened from it, which share its hooks)
// fail with the returned error.
func failThreadGroupMemberDeletes(cs *CompositeStore) error {
	boom := errors.New("member delete failed")
	cs.client.ThreadGroupMember.Use(func(next ent.Mutator) ent.Mutator {
		return ent.MutateFunc(func(ctx context.Context, m ent.Mutation) (ent.Value, error) {
			if m.Op().Is(entgo.OpDeleteOne | entgo.OpDelete) {
				return nil, boom
			}
			return next.Mutate(ctx, m)
		})
	})
	return boom
}

// TestThreadGroupDelete_OneTransaction: when the member delete fails, the
// group delete that ran before it is rolled back.
func TestThreadGroupDelete_OneTransaction(t *testing.T) {
	ctx := context.Background()
	cs := newThreadGroupTestStore(t)
	projectID := uuid.NewString()
	g := newThreadGroup(t, cs, projectID, "doomed")
	conv := newGroupConversation(t, cs, projectID)
	require.NoError(t, cs.SetThreadGroupMember(ctx, g.ID, conv, "u"))

	boom := failThreadGroupMemberDeletes(cs)
	require.ErrorIs(t, cs.DeleteThreadGroup(ctx, g.ID), boom)
	got, err := cs.GetThreadGroup(ctx, g.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{conv}, memberConversationIDs(got))
}

func TestThreadGroupRemoveMembersForConversation(t *testing.T) {
	ctx := context.Background()
	cs := newThreadGroupTestStore(t)
	projectID := uuid.NewString()
	g := newThreadGroup(t, cs, projectID, "one")
	c1 := newGroupConversation(t, cs, projectID)
	c2 := newGroupConversation(t, cs, projectID)
	require.NoError(t, cs.SetThreadGroupMember(ctx, g.ID, c1, "u"))
	require.NoError(t, cs.SetThreadGroupMember(ctx, g.ID, c2, "u"))

	require.NoError(t, cs.RemoveThreadGroupMembersForConversation(ctx, c1))
	require.NoError(t, cs.RemoveThreadGroupMembersForConversation(ctx, c1), "idempotent")
	assert.Zero(t, memberRows(t, cs, c1))
	assert.Equal(t, 1, memberRows(t, cs, c2))
	assert.ErrorIs(t, cs.RemoveThreadGroupMembersForConversation(ctx, "not-a-uuid"), store.ErrInvalidInput)
}

func TestThreadGroupDeleteForProject(t *testing.T) {
	ctx := context.Background()
	cs := newThreadGroupTestStore(t)
	projectA, projectB := uuid.NewString(), uuid.NewString()
	ga1 := newThreadGroup(t, cs, projectA, "a1")
	ga2 := newThreadGroup(t, cs, projectA, "a2")
	gb := newThreadGroup(t, cs, projectB, "b")
	ca := newGroupConversation(t, cs, projectA)
	ca2 := newGroupConversation(t, cs, projectA)
	cb := newGroupConversation(t, cs, projectB)
	require.NoError(t, cs.SetThreadGroupMember(ctx, ga1.ID, ca, "u"))
	require.NoError(t, cs.SetThreadGroupMember(ctx, ga2.ID, ca2, "u"))
	require.NoError(t, cs.SetThreadGroupMember(ctx, gb.ID, cb, "u"))

	require.NoError(t, cs.DeleteThreadGroupsForProject(ctx, projectA))
	require.NoError(t, cs.DeleteThreadGroupsForProject(ctx, projectA), "idempotent")

	listA, err := cs.ListThreadGroups(ctx, projectA)
	require.NoError(t, err)
	assert.Empty(t, listA)
	assert.Zero(t, memberRows(t, cs, ca))
	assert.Zero(t, memberRows(t, cs, ca2))

	listB, err := cs.ListThreadGroups(ctx, projectB)
	require.NoError(t, err)
	require.Len(t, listB, 1)
	assert.Equal(t, []string{cb}, memberConversationIDs(&listB[0]))
}

func TestCompositeDeleteProject_DeletesThreadGroups(t *testing.T) {
	ctx := context.Background()
	cs := newThreadGroupTestStore(t)
	projectID := uuid.NewString()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{ID: projectID, Name: "tg", Slug: "tg-" + projectID[:8]}))
	otherID := uuid.NewString()
	g := newThreadGroup(t, cs, projectID, "one")
	other := newThreadGroup(t, cs, otherID, "other")
	conv := newGroupConversation(t, cs, projectID)
	otherConv := newGroupConversation(t, cs, otherID)
	require.NoError(t, cs.SetThreadGroupMember(ctx, g.ID, conv, "u"))
	require.NoError(t, cs.SetThreadGroupMember(ctx, other.ID, otherConv, "u"))

	require.NoError(t, cs.DeleteProject(ctx, projectID))

	n, err := cs.client.ThreadGroup.Query().Where(threadgroup.ProjectIDEQ(uuid.MustParse(projectID))).Count(ctx)
	require.NoError(t, err)
	assert.Zero(t, n)
	n, err = cs.client.ThreadGroupMember.Query().Where(threadgroupmember.ProjectIDEQ(uuid.MustParse(projectID))).Count(ctx)
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Equal(t, 1, memberRows(t, cs, otherConv), "other projects untouched")
}

// TestCompositeDeleteProject_ThreadGroupsInSameTransaction: when the project
// row delete fails, the thread group cascade that ran before it is rolled
// back too.
func TestCompositeDeleteProject_ThreadGroupsInSameTransaction(t *testing.T) {
	ctx := context.Background()
	cs := newThreadGroupTestStore(t)
	projectID := uuid.NewString()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{ID: projectID, Name: "tg", Slug: "tg-" + projectID[:8]}))
	g := newThreadGroup(t, cs, projectID, "one")
	conv := newGroupConversation(t, cs, projectID)
	require.NoError(t, cs.SetThreadGroupMember(ctx, g.ID, conv, "u"))

	boom := errors.New("project delete failed")
	cs.client.Project.Use(func(next ent.Mutator) ent.Mutator {
		return ent.MutateFunc(func(ctx context.Context, m ent.Mutation) (ent.Value, error) {
			if m.Op().Is(entgo.OpDeleteOne | entgo.OpDelete) {
				return nil, boom
			}
			return next.Mutate(ctx, m)
		})
	})
	require.ErrorIs(t, cs.DeleteProject(ctx, projectID), boom)

	got, err := cs.GetThreadGroup(ctx, g.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{conv}, memberConversationIDs(got))
}

// --- Conversation lifecycle (design sections 7a, 7a2) ------------------------

func TestDeleteConversation_RemovesThreadGroupMembership(t *testing.T) {
	ctx := context.Background()
	cs := newThreadGroupTestStore(t)
	projectID := uuid.NewString()
	g := newThreadGroup(t, cs, projectID, "one")
	conv := newGroupConversation(t, cs, projectID)
	require.NoError(t, cs.SetThreadGroupMember(ctx, g.ID, conv, "u"))

	require.NoError(t, cs.DeleteConversation(ctx, conv))
	assert.Zero(t, memberRows(t, cs, conv))
	assert.ErrorIs(t, cs.DeleteConversation(ctx, conv), store.ErrNotFound)
}

func TestDeleteConversation_MembershipRemovalInSameTransaction(t *testing.T) {
	ctx := context.Background()
	cs := newThreadGroupTestStore(t)
	projectID := uuid.NewString()
	g := newThreadGroup(t, cs, projectID, "one")
	conv := newGroupConversation(t, cs, projectID)
	require.NoError(t, cs.SetThreadGroupMember(ctx, g.ID, conv, "u"))

	boom := failThreadGroupMemberDeletes(cs)
	require.ErrorIs(t, cs.DeleteConversation(ctx, conv), boom)
	_, err := cs.GetConversation(ctx, conv)
	require.NoError(t, err, "soft delete rolled back with the failed membership removal")
	assert.Equal(t, 1, memberRows(t, cs, conv))
}

func TestUpdateConversation_SettingDeletedAtRemovesMembership(t *testing.T) {
	ctx := context.Background()
	cs := newThreadGroupTestStore(t)
	projectID := uuid.NewString()
	g := newThreadGroup(t, cs, projectID, "one")
	conv := newGroupConversation(t, cs, projectID)
	require.NoError(t, cs.SetThreadGroupMember(ctx, g.ID, conv, "u"))

	// An update that does not delete keeps the membership.
	c, err := cs.GetConversation(ctx, conv)
	require.NoError(t, err)
	c.DisplayName = "renamed"
	require.NoError(t, cs.UpdateConversation(ctx, c))
	assert.Equal(t, 1, memberRows(t, cs, conv))

	now := time.Now()
	c.DeletedAt = &now
	require.NoError(t, cs.UpdateConversation(ctx, c))
	assert.Zero(t, memberRows(t, cs, conv))

	// Restoring the conversation does not bring the membership back.
	c.DeletedAt = nil
	require.NoError(t, cs.UpdateConversation(ctx, c))
	_, err = cs.GetConversation(ctx, conv)
	require.NoError(t, err)
	assert.Zero(t, memberRows(t, cs, conv))
	got, err := cs.GetThreadGroup(ctx, g.ID)
	require.NoError(t, err)
	assert.Empty(t, got.Members)
}

// TestUpdateConversation_DeletedAtInsideWithTx covers the ambient
// transaction path (ConversationStore.inTx).
func TestUpdateConversation_DeletedAtInsideWithTx(t *testing.T) {
	ctx := context.Background()
	cs := newThreadGroupTestStore(t)
	projectID := uuid.NewString()
	g := newThreadGroup(t, cs, projectID, "one")
	conv := newGroupConversation(t, cs, projectID)
	require.NoError(t, cs.SetThreadGroupMember(ctx, g.ID, conv, "u"))
	c, err := cs.GetConversation(ctx, conv)
	require.NoError(t, err)

	rollback := errors.New("rollback")
	err = cs.WithTx(ctx, func(tx store.Store) error {
		now := time.Now()
		c.DeletedAt = &now
		require.NoError(t, tx.UpdateConversation(ctx, c))
		return rollback
	})
	require.ErrorIs(t, err, rollback)
	assert.Equal(t, 1, memberRows(t, cs, conv), "rolled back with the transaction")

	require.NoError(t, cs.WithTx(ctx, func(tx store.Store) error {
		return tx.UpdateConversation(ctx, c)
	}))
	assert.Zero(t, memberRows(t, cs, conv))
}

// TestUpdateConversation_ProjectMismatchKeepsMembership: a rejected update
// writes nothing, so the membership stays.
func TestUpdateConversation_ProjectMismatchKeepsMembership(t *testing.T) {
	ctx := context.Background()
	cs := newThreadGroupTestStore(t)
	projectID := uuid.NewString()
	g := newThreadGroup(t, cs, projectID, "one")
	conv := newGroupConversation(t, cs, projectID)
	require.NoError(t, cs.SetThreadGroupMember(ctx, g.ID, conv, "u"))
	c, err := cs.GetConversation(ctx, conv)
	require.NoError(t, err)

	otherProject := uuid.NewString()
	now := time.Now()
	c.ProjectID = &otherProject
	c.DeletedAt = &now
	require.ErrorIs(t, cs.UpdateConversation(ctx, c), store.ErrConversationProjectMismatch)
	assert.Equal(t, 1, memberRows(t, cs, conv))
}

// TestThreadGroupReads_SkipDeletedKeepArchived: reads hide members whose
// conversation is soft-deleted (even when cleanup was missed) or missing,
// and keep archived conversations.
func TestThreadGroupReads_SkipDeletedKeepArchived(t *testing.T) {
	ctx := context.Background()
	cs := newThreadGroupTestStore(t)
	projectID := uuid.NewString()
	g := newThreadGroup(t, cs, projectID, "one")
	live := newGroupConversation(t, cs, projectID)
	archived := newGroupConversation(t, cs, projectID)
	deleted := newGroupConversation(t, cs, projectID)
	missing := uuid.NewString()
	for _, c := range []string{live, archived, deleted, missing} {
		require.NoError(t, cs.SetThreadGroupMember(ctx, g.ID, c, "u"))
	}

	// Archive through the store (keeps membership).
	ac, err := cs.GetConversation(ctx, archived)
	require.NoError(t, err)
	now := time.Now()
	ac.ArchivedAt = &now
	require.NoError(t, cs.UpdateConversation(ctx, ac))

	// Soft-delete behind the store's back, as a missed cleanup would leave it.
	require.NoError(t, cs.client.Conversation.UpdateOneID(uuid.MustParse(deleted)).
		Where(conversation.DeletedAtIsNil()).
		SetDeletedAt(now).
		Exec(ctx))
	assert.Equal(t, 1, memberRows(t, cs, deleted), "row still present")

	got, err := cs.GetThreadGroup(ctx, g.ID)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{live, archived}, memberConversationIDs(got))

	list, err := cs.ListThreadGroups(ctx, projectID)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.ElementsMatch(t, []string{live, archived}, memberConversationIDs(&list[0]))
}

// --- Postgres row locks (design section 7d across transactions) -----------

// TestThreadGroupSetMember_WaitsForUncommittedGroupDelete_Postgres: a filing
// that races an uncommitted group delete waits on the group row lock (FOR
// UPDATE) and, once the delete commits, finds no group: ErrNotFound and no
// member row. Without the row lock it would insert an orphan member.
func TestThreadGroupSetMember_WaitsForUncommittedGroupDelete_Postgres(t *testing.T) {
	skipUnlessPostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), lockOrderTimeout)
	defer cancel()
	cs := newThreadGroupTestStore(t)
	projectID := uuid.NewString()
	g := newThreadGroup(t, cs, projectID, "one")
	conv := newGroupConversation(t, cs, projectID)

	tx, err := cs.client.Tx(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	require.NoError(t, newTxCompositeStore(tx).DeleteThreadGroup(ctx, g.ID))

	done := make(chan error, 1)
	go func() { done <- cs.SetThreadGroupMember(ctx, g.ID, conv, "u") }()
	waitForLockWaiter(t, ctx, cs)
	select {
	case err := <-done:
		t.Fatalf("SetThreadGroupMember returned before the delete committed: %v", err)
	default:
	}

	require.NoError(t, tx.Commit())
	select {
	case err := <-done:
		require.ErrorIs(t, err, store.ErrNotFound)
	case <-ctx.Done():
		t.Fatal("SetThreadGroupMember never returned")
	}
	assert.Zero(t, memberRows(t, cs, conv), "no orphan member row")
}

// TestThreadGroupDelete_WaitsForUncommittedFiling_Postgres: a group delete
// that races an uncommitted filing waits on the group row, then removes the
// member committed meanwhile. Deleting the members before the group row
// would miss it and leave an orphan.
func TestThreadGroupDelete_WaitsForUncommittedFiling_Postgres(t *testing.T) {
	skipUnlessPostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), lockOrderTimeout)
	defer cancel()
	cs := newThreadGroupTestStore(t)
	projectID := uuid.NewString()
	g := newThreadGroup(t, cs, projectID, "one")
	conv := newGroupConversation(t, cs, projectID)

	tx, err := cs.client.Tx(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	require.NoError(t, newTxCompositeStore(tx).SetThreadGroupMember(ctx, g.ID, conv, "u"))

	done := make(chan error, 1)
	go func() { done <- cs.DeleteThreadGroup(ctx, g.ID) }()
	waitForLockWaiter(t, ctx, cs)
	select {
	case err := <-done:
		t.Fatalf("DeleteThreadGroup returned before the filing committed: %v", err)
	default:
	}

	require.NoError(t, tx.Commit())
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("DeleteThreadGroup never returned")
	}
	assert.Zero(t, memberRows(t, cs, conv), "no orphan member row")
	assert.Zero(t, groupMemberRows(t, cs, g.ID))
}

func TestThreadGroupCount(t *testing.T) {
	ctx := context.Background()
	cs := newThreadGroupTestStore(t)
	projectID := uuid.NewString()

	n, err := cs.CountThreadGroups(ctx, projectID)
	require.NoError(t, err)
	assert.Zero(t, n)

	g := newThreadGroup(t, cs, projectID, "one")
	newThreadGroup(t, cs, projectID, "two")
	newThreadGroup(t, cs, uuid.NewString(), "other project")
	require.NoError(t, cs.SetThreadGroupMember(ctx, g.ID, newGroupConversation(t, cs, projectID), "u"))

	n, err = cs.CountThreadGroups(ctx, projectID)
	require.NoError(t, err)
	assert.Equal(t, 2, n, "groups of this project only; members do not count")

	_, err = cs.CountThreadGroups(ctx, "not-a-uuid")
	assert.ErrorIs(t, err, store.ErrInvalidInput)
}

func TestThreadGroupGetIDForConversation(t *testing.T) {
	ctx := context.Background()
	cs := newThreadGroupTestStore(t)
	projectID := uuid.NewString()
	a := newThreadGroup(t, cs, projectID, "a")
	b := newThreadGroup(t, cs, projectID, "b")
	c := newGroupConversation(t, cs, projectID)

	id, err := cs.GetThreadGroupIDForConversation(ctx, c)
	require.NoError(t, err)
	assert.Empty(t, id, "not filed")

	require.NoError(t, cs.SetThreadGroupMember(ctx, a.ID, c, "u"))
	id, err = cs.GetThreadGroupIDForConversation(ctx, c)
	require.NoError(t, err)
	assert.Equal(t, a.ID, id)

	require.NoError(t, cs.SetThreadGroupMember(ctx, b.ID, c, "u"))
	id, err = cs.GetThreadGroupIDForConversation(ctx, c)
	require.NoError(t, err)
	assert.Equal(t, b.ID, id, "follows a move")

	require.NoError(t, cs.RemoveThreadGroupMember(ctx, b.ID, c))
	id, err = cs.GetThreadGroupIDForConversation(ctx, c)
	require.NoError(t, err)
	assert.Empty(t, id, "removed")

	id, err = cs.GetThreadGroupIDForConversation(ctx, "not-a-uuid")
	require.NoError(t, err)
	assert.Empty(t, id, "a malformed ID is in no group")
}
