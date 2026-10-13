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
	"context"
	"fmt"
	"sort"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/conversation"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/predicate"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/threadgroup"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/threadgroupmember"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// ThreadGroupStore implements store.ThreadGroupStore using the Ent ORM
// (ptone/scion#4424). One implementation serves SQLite and Postgres.
//
// Lock order. SetThreadGroupMember locks the target group row (FOR UPDATE on
// Postgres) before it writes a member row, and both delete paths delete the
// group rows before the member rows. So a member insert racing a group
// delete either commits first, and the delete's later member statement sees
// and removes it, or waits for the delete and then finds no group
// (ErrNotFound). No member row is left behind for a deleted group.
type ThreadGroupStore struct {
	client *ent.Client
	inTx   bool // true when client wraps an ambient WithTx transaction
}

// NewThreadGroupStore creates a new Ent-backed ThreadGroupStore.
func NewThreadGroupStore(client *ent.Client) *ThreadGroupStore {
	return &ThreadGroupStore{client: client}
}

// Compile-time assertion.
var _ store.ThreadGroupStore = (*ThreadGroupStore)(nil)

func entThreadGroupToStore(e *ent.ThreadGroup) store.ThreadGroup {
	return store.ThreadGroup{
		ID:            e.ID.String(),
		ProjectID:     e.ProjectID.String(),
		Name:          e.Name,
		NameKey:       e.NameKey,
		CreatedBy:     e.CreatedBy,
		CreatedByKind: string(e.CreatedByKind),
		CreatedAt:     e.CreatedAt,
		UpdatedAt:     e.UpdatedAt,
		Members:       []store.ThreadGroupMember{},
	}
}

func entThreadGroupMemberToStore(e *ent.ThreadGroupMember) store.ThreadGroupMember {
	return store.ThreadGroupMember{
		ID:             e.ID.String(),
		GroupID:        e.GroupID.String(),
		ProjectID:      e.ProjectID.String(),
		ConversationID: e.ConversationID.String(),
		AddedBy:        e.AddedBy,
		AddedAt:        e.AddedAt,
	}
}

// liveConversationMember restricts a member query to members whose
// conversation row exists and is not soft-deleted (design section 7a: a
// missed cleanup never shows up). Archived conversations stay visible
// (section 7a2). It is a correlated EXISTS on the conversations primary key,
// so the cost follows the group's members, not the size of the hub.
func liveConversationMember() predicate.ThreadGroupMember {
	return func(s *entsql.Selector) {
		t := entsql.Table(conversation.Table)
		s.Where(entsql.Exists(
			entsql.Select(t.C(conversation.FieldID)).
				From(t).
				Where(entsql.And(
					entsql.ColumnsEQ(t.C(conversation.FieldID), s.C(threadgroupmember.FieldConversationID)),
					entsql.IsNull(t.C(conversation.FieldDeletedAt)),
				)),
		))
	}
}

// attachMembers loads the live members of groups and appends them to each
// group in added_at order.
func attachMembers(ctx context.Context, c *ent.Client, groups []store.ThreadGroup) error {
	if len(groups) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(groups))
	idx := make(map[string]int, len(groups))
	for i := range groups {
		uid, err := uuid.Parse(groups[i].ID)
		if err != nil {
			return err
		}
		ids = append(ids, uid)
		idx[groups[i].ID] = i
	}
	members, err := c.ThreadGroupMember.Query().
		Where(
			threadgroupmember.GroupIDIn(ids...),
			liveConversationMember(),
		).
		Order(
			ent.Asc(threadgroupmember.FieldAddedAt),
			ent.Asc(threadgroupmember.FieldID),
		).
		All(ctx)
	if err != nil {
		return mapError(err)
	}
	for _, m := range members {
		if i, ok := idx[m.GroupID.String()]; ok {
			groups[i].Members = append(groups[i].Members, entThreadGroupMemberToStore(m))
		}
	}
	return nil
}

// CreateThreadGroup creates a thread group.
func (s *ThreadGroupStore) CreateThreadGroup(ctx context.Context, g *store.ThreadGroup) error {
	if g == nil {
		return fmt.Errorf("thread group is nil: %w", store.ErrInvalidInput)
	}
	if g.ProjectID == "" {
		return fmt.Errorf("thread group project ID is required: %w", store.ErrInvalidInput)
	}
	pid, err := parseUUID(g.ProjectID)
	if err != nil {
		return err
	}
	name, err := store.ValidateThreadGroupName(g.Name)
	if err != nil {
		return err
	}
	kind := threadgroup.CreatedByKind(g.CreatedByKind)
	if err := threadgroup.CreatedByKindValidator(kind); err != nil {
		return fmt.Errorf("thread group creator kind %q must be user or agent: %w", g.CreatedByKind, store.ErrInvalidInput)
	}
	id := uuid.New()
	if g.ID != "" {
		if id, err = parseUUID(g.ID); err != nil {
			return err
		}
	}
	now := time.Now()
	key := store.ThreadGroupNameKey(name)
	created, err := s.client.ThreadGroup.Create().
		SetID(id).
		SetProjectID(pid).
		SetName(name).
		SetNameKey(key).
		SetCreatedBy(g.CreatedBy).
		SetCreatedByKind(kind).
		SetCreatedAt(now).
		SetUpdatedAt(now).
		Save(ctx)
	if err != nil {
		return mapError(err)
	}
	g.ID = created.ID.String()
	g.Name = created.Name
	g.NameKey = created.NameKey
	g.CreatedAt = created.CreatedAt
	g.UpdatedAt = created.UpdatedAt
	if g.Members == nil {
		g.Members = []store.ThreadGroupMember{}
	}
	return nil
}

// GetThreadGroup returns a thread group with its live members.
func (s *ThreadGroupStore) GetThreadGroup(ctx context.Context, id string) (*store.ThreadGroup, error) {
	uid, err := parseGetID(id)
	if err != nil {
		return nil, err
	}
	e, err := s.client.ThreadGroup.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	groups := []store.ThreadGroup{entThreadGroupToStore(e)}
	if err := attachMembers(ctx, s.client, groups); err != nil {
		return nil, err
	}
	return &groups[0], nil
}

// ListThreadGroups returns a project's thread groups, ordered by name key in
// byte order, with their live members.
func (s *ThreadGroupStore) ListThreadGroups(ctx context.Context, projectID string) ([]store.ThreadGroup, error) {
	pid, err := parseUUID(projectID)
	if err != nil {
		return nil, err
	}
	rows, err := s.client.ThreadGroup.Query().
		Where(threadgroup.ProjectIDEQ(pid)).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	groups := make([]store.ThreadGroup, 0, len(rows))
	for _, r := range rows {
		groups = append(groups, entThreadGroupToStore(r))
	}
	// Sort in Go, byte by byte, and not with ORDER BY: Postgres would order
	// name_key by the database collation, a dialect-dependent order for
	// non-ASCII names and punctuation (design section 7c). (NameKey, ID) is
	// a total order, so an unstable sort is enough.
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].NameKey != groups[j].NameKey {
			return groups[i].NameKey < groups[j].NameKey
		}
		return groups[i].ID < groups[j].ID
	})
	if err := attachMembers(ctx, s.client, groups); err != nil {
		return nil, err
	}
	return groups, nil
}

// CountThreadGroups returns how many thread groups a project has, without
// loading them or their members.
func (s *ThreadGroupStore) CountThreadGroups(ctx context.Context, projectID string) (int, error) {
	pid, err := parseUUID(projectID)
	if err != nil {
		return 0, err
	}
	n, err := s.client.ThreadGroup.Query().
		Where(threadgroup.ProjectIDEQ(pid)).
		Count(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	return n, nil
}

// GetThreadGroupIDForConversation returns the ID of the thread group a
// conversation is filed in, or "" when it is in none. It is a point lookup on
// the unique conversation_id index. A malformed ID is in no group.
func (s *ThreadGroupStore) GetThreadGroupIDForConversation(ctx context.Context, conversationID string) (string, error) {
	cid, err := uuid.Parse(conversationID)
	if err != nil {
		return "", nil
	}
	m, err := s.client.ThreadGroupMember.Query().
		Where(threadgroupmember.ConversationIDEQ(cid)).
		Only(ctx)
	if ent.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", mapError(err)
	}
	return m.GroupID.String(), nil
}

// RenameThreadGroup renames a thread group and bumps updated_at.
func (s *ThreadGroupStore) RenameThreadGroup(ctx context.Context, id, name string) error {
	uid, err := parseGetID(id)
	if err != nil {
		return err
	}
	trimmed, err := store.ValidateThreadGroupName(name)
	if err != nil {
		return err
	}
	err = s.client.ThreadGroup.UpdateOneID(uid).
		SetName(trimmed).
		SetNameKey(store.ThreadGroupNameKey(trimmed)).
		SetUpdatedAt(time.Now()).
		Exec(ctx)
	return mapError(err)
}

// DeleteThreadGroup deletes a thread group and its member rows in one
// transaction. The group row goes first; see the lock order note on
// ThreadGroupStore.
func (s *ThreadGroupStore) DeleteThreadGroup(ctx context.Context, id string) error {
	uid, err := parseGetID(id)
	if err != nil {
		return err
	}
	return runInEntTx(ctx, s.client, s.inTx, func(c *ent.Client) error {
		n, err := c.ThreadGroup.Delete().Where(threadgroup.IDEQ(uid)).Exec(ctx)
		if err != nil {
			return mapError(err)
		}
		if n == 0 {
			return store.ErrNotFound
		}
		_, err = c.ThreadGroupMember.Delete().Where(threadgroupmember.GroupIDEQ(uid)).Exec(ctx)
		return mapError(err)
	})
}

// SetThreadGroupMember files a conversation into a group, moving it out of
// any other group, in one transaction that first checks (and on Postgres
// locks) the group row.
func (s *ThreadGroupStore) SetThreadGroupMember(ctx context.Context, groupID, conversationID, addedBy string) error {
	gid, err := parseGetID(groupID)
	if err != nil {
		return err
	}
	cid, err := parseUUID(conversationID)
	if err != nil {
		return err
	}
	rowLocks := s.client.Driver().Dialect() == dialect.Postgres
	return runInEntTx(ctx, s.client, s.inTx, func(c *ent.Client) error {
		q := c.ThreadGroup.Query().Where(threadgroup.IDEQ(gid))
		if rowLocks {
			q = q.ForUpdate()
		}
		g, err := q.Only(ctx)
		if err != nil {
			return mapError(err)
		}
		existing, err := c.ThreadGroupMember.Query().
			Where(threadgroupmember.ConversationIDEQ(cid)).
			Only(ctx)
		switch {
		case err == nil && existing.GroupID == gid:
			return nil // already filed here: idempotent, keeps added_at
		case err != nil && !ent.IsNotFound(err):
			return mapError(err)
		}
		now := time.Now()
		// The upsert on the unique conversation_id index also covers a
		// concurrent first filing of the same conversation.
		err = c.ThreadGroupMember.Create().
			SetGroupID(gid).
			SetProjectID(g.ProjectID).
			SetConversationID(cid).
			SetAddedBy(addedBy).
			SetAddedAt(now).
			OnConflictColumns(threadgroupmember.FieldConversationID).
			Update(func(u *ent.ThreadGroupMemberUpsert) {
				u.SetGroupID(gid)
				u.SetProjectID(g.ProjectID)
				u.SetAddedBy(addedBy)
				u.SetAddedAt(now)
			}).
			Exec(ctx)
		return mapError(err)
	})
}

// RemoveThreadGroupMember takes a conversation out of a group. Idempotent.
// A malformed group ID matches no group, so, as with the other group ID
// lookups, it is treated as not found, which this idempotent call reports as
// nil.
func (s *ThreadGroupStore) RemoveThreadGroupMember(ctx context.Context, groupID, conversationID string) error {
	gid, err := parseGetID(groupID)
	if err != nil {
		return nil
	}
	cid, err := parseUUID(conversationID)
	if err != nil {
		return err
	}
	_, err = s.client.ThreadGroupMember.Delete().
		Where(
			threadgroupmember.GroupIDEQ(gid),
			threadgroupmember.ConversationIDEQ(cid),
		).
		Exec(ctx)
	return mapError(err)
}

// RemoveThreadGroupMembersForConversation removes a conversation from any
// group. Idempotent.
func (s *ThreadGroupStore) RemoveThreadGroupMembersForConversation(ctx context.Context, conversationID string) error {
	cid, err := parseUUID(conversationID)
	if err != nil {
		return err
	}
	return removeThreadGroupMembersForConversation(ctx, s.client, cid)
}

func removeThreadGroupMembersForConversation(ctx context.Context, c *ent.Client, cid uuid.UUID) error {
	_, err := c.ThreadGroupMember.Delete().
		Where(threadgroupmember.ConversationIDEQ(cid)).
		Exec(ctx)
	return mapError(err)
}

// DeleteThreadGroupsForProject deletes a project's groups and their member
// rows in one transaction (the ambient one inside WithTx, as in
// CompositeStore.DeleteProject). Group rows go first; see the lock order
// note on ThreadGroupStore.
func (s *ThreadGroupStore) DeleteThreadGroupsForProject(ctx context.Context, projectID string) error {
	pid, err := parseUUID(projectID)
	if err != nil {
		return err
	}
	return runInEntTx(ctx, s.client, s.inTx, func(c *ent.Client) error {
		if _, err := c.ThreadGroup.Delete().Where(threadgroup.ProjectIDEQ(pid)).Exec(ctx); err != nil {
			return mapError(err)
		}
		_, err := c.ThreadGroupMember.Delete().Where(threadgroupmember.ProjectIDEQ(pid)).Exec(ctx)
		return mapError(err)
	})
}
