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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/group"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newChildEdgeTestStore returns a CompositeStore on the active test backend
// and a helper that creates an explicit group.
func newChildEdgeTestStore(t *testing.T) (*CompositeStore, func(name string) string) {
	t.Helper()
	s := NewCompositeStore(enttest.NewClient(t))
	newGroup := func(name string) string {
		t.Helper()
		id := uuid.NewString()
		require.NoError(t, s.CreateGroup(context.Background(), &store.Group{
			ID: id, Name: name, Slug: name + "-" + id[:8], GroupType: store.GroupTypeExplicit,
		}))
		return id
	}
	return s, newGroup
}

func addChildGroupEdge(t *testing.T, s store.Store, parentID, childID string) {
	t.Helper()
	require.NoError(t, s.AddGroupMember(context.Background(), &store.GroupMember{
		GroupID: parentID, MemberType: store.GroupMemberTypeGroup, MemberID: childID,
		Role: store.GroupMemberRoleMember,
	}))
}

// TestRemoveChildGroupEdge_RemovesOnlyThatEdge pins the store contract on
// both backends: exactly the named edge is removed, a missing edge or parent
// is ErrNotFound, a self-edge can be removed, and project_agents parents are
// refused like RemoveGroupMember refuses them.
func TestRemoveChildGroupEdge_RemovesOnlyThatEdge(t *testing.T) {
	ctx := context.Background()
	s, newGroup := newChildEdgeTestStore(t)

	parent := newGroup("edge-parent")
	other := newGroup("edge-other")
	child := newGroup("edge-child")
	self := newGroup("edge-self")
	addChildGroupEdge(t, s, parent, child)
	addChildGroupEdge(t, s, parent, other)
	addChildGroupEdge(t, s, other, child)
	addChildGroupEdge(t, s, self, self)

	require.NoError(t, s.RemoveChildGroupEdge(ctx, parent, child))

	parents, err := s.GetDirectParentGroupIDs(ctx, child)
	require.NoError(t, err)
	assert.Equal(t, []string{other}, parents, "only the named edge is removed")
	parents, err = s.GetDirectParentGroupIDs(ctx, other)
	require.NoError(t, err)
	assert.Equal(t, []string{parent}, parents, "the parent's other child edge is kept")

	err = s.RemoveChildGroupEdge(ctx, parent, child)
	assert.True(t, errors.Is(err, store.ErrNotFound), "a missing edge is ErrNotFound, got %v", err)
	err = s.RemoveChildGroupEdge(ctx, child, parent)
	assert.True(t, errors.Is(err, store.ErrNotFound), "the reverse direction is not an edge, got %v", err)
	err = s.RemoveChildGroupEdge(ctx, uuid.NewString(), child)
	assert.True(t, errors.Is(err, store.ErrNotFound), "an unknown parent is ErrNotFound, got %v", err)
	assert.Error(t, s.RemoveChildGroupEdge(ctx, "not-a-uuid", child))

	require.NoError(t, s.RemoveChildGroupEdge(ctx, self, self))
	parents, err = s.GetDirectParentGroupIDs(ctx, self)
	require.NoError(t, err)
	assert.Empty(t, parents, "the self-edge is removed")

	// A project_agents parent is refused, and its edge is kept. The edge is
	// written with ent directly because AddGroupMember refuses it.
	projectAgents := uuid.New()
	_, err = s.client.Group.Create().
		SetID(projectAgents).SetName("edge-project-agents").SetSlug("edge-project-agents-" + projectAgents.String()[:8]).
		SetGroupType(group.GroupTypeProjectAgents).
		AddChildGroupIDs(uuid.MustParse(child)).
		Save(ctx)
	require.NoError(t, err)
	err = s.RemoveChildGroupEdge(ctx, projectAgents.String(), child)
	assert.True(t, errors.Is(err, store.ErrInvalidInput), "project_agents parent is refused, got %v", err)
	parents, err = s.GetDirectParentGroupIDs(ctx, child)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{other, projectAgents.String()}, parents)
}

// removeChildEdgeWithAudit removes the edge and writes one audit record in
// one transaction, the same shape as the hub's startup removal. ready, when
// set, is called after the delete and before the commit.
func removeChildEdgeWithAudit(ctx context.Context, s store.Store, parentID, childID string, ready func(tx store.Store) error) error {
	return s.WithTx(ctx, func(tx store.Store) error {
		if err := tx.RemoveChildGroupEdge(ctx, parentID, childID); err != nil {
			return err
		}
		if err := tx.CreateMutationAudit(ctx, &store.MutationAuditRecord{
			Timestamp: time.Now().UTC(), MutationType: "child_edge_removed_test",
			ActorPrincipalKind: "system", ActorPrincipalID: "test",
			TargetType: "group_membership", TargetID: parentID, AfterSummary: "removed",
		}); err != nil {
			return err
		}
		if ready != nil {
			return ready(tx)
		}
		return nil
	})
}

func countChildEdgeTestAudits(t *testing.T, s store.Store) int {
	t.Helper()
	recs, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{
		MutationType: "child_edge_removed_test", Limit: 100,
	})
	require.NoError(t, err)
	return len(recs)
}

// TestRemoveChildGroupEdge_ConcurrentRemovalWritesOneAudit pins that two
// transactions removing the same edge produce exactly one success, so only
// one of them writes its audit record. On PostgreSQL (default READ
// COMMITTED) the second transaction's delete is observed waiting on the
// first transaction's row lock, and after the first commits it deletes no
// row and gets ErrNotFound. SQLite serializes writers, so there the two
// removals run one after the other.
func TestRemoveChildGroupEdge_ConcurrentRemovalWritesOneAudit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s, newGroup := newChildEdgeTestStore(t)
	parent := newGroup("edge-race-parent")
	child := newGroup("edge-race-child")
	addChildGroupEdge(t, s, parent, child)

	if !enttest.Active() {
		require.NoError(t, removeChildEdgeWithAudit(ctx, s, parent, child, nil))
		err := removeChildEdgeWithAudit(ctx, s, parent, child, nil)
		assert.True(t, errors.Is(err, store.ErrNotFound), "second removal is ErrNotFound, got %v", err)
		assert.Equal(t, 1, countChildEdgeTestAudits(t, s))
		return
	}

	// First transaction: delete the edge, then hold the transaction open
	// until the second transaction is waiting on the row lock.
	holderReady := make(chan int, 1)
	holderDone := make(chan error, 1)
	releaseHolder := make(chan struct{})
	holderReleased := false
	defer func() {
		if !holderReleased {
			close(releaseHolder)
		}
	}()
	go func() {
		holderDone <- removeChildEdgeWithAudit(ctx, s, parent, child, func(tx store.Store) error {
			pid, err := constraintHistoryPostgresBackendPID(ctx, tx.(*CompositeStore))
			if err != nil {
				return err
			}
			holderReady <- pid
			select {
			case <-releaseHolder:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	var holderPID int
	select {
	case holderPID = <-holderReady:
	case err := <-holderDone:
		require.NoError(t, err)
		t.Fatal("first transaction completed before holding the edge row")
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}

	// Second transaction: publish its backend PID, then delete the same
	// edge.
	writerReady := make(chan int, 1)
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- s.WithTx(ctx, func(tx store.Store) error {
			pid, err := constraintHistoryPostgresBackendPID(ctx, tx.(*CompositeStore))
			if err != nil {
				return err
			}
			writerReady <- pid
			return removeChildEdgeWithAudit(ctx, tx, parent, child, nil)
		})
	}()
	var writerPID int
	select {
	case writerPID = <-writerReady:
	case err := <-writerDone:
		require.NoError(t, err)
		t.Fatal("second transaction completed before publishing its backend PID")
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}

	observer := s.DB()
	require.NotNil(t, observer)
	for {
		var waiting bool
		err := observer.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1
				FROM pg_stat_activity
				WHERE pid = $1
				  AND wait_event_type = 'Lock'
				  AND $2::integer = ANY(pg_blocking_pids(pid))
				  AND POSITION('DELETE' IN UPPER(query)) > 0
			)`, writerPID, holderPID).Scan(&waiting)
		require.NoError(t, err)
		if waiting {
			break
		}
		select {
		case err := <-writerDone:
			t.Fatalf("second transaction finished before waiting on the edge row lock: %v", err)
		case <-ctx.Done():
			t.Fatal("second transaction's row-lock wait was not observed before the test deadline")
		case <-time.After(10 * time.Millisecond):
		}
	}

	close(releaseHolder)
	holderReleased = true
	require.NoError(t, <-holderDone)
	err := <-writerDone
	assert.True(t, errors.Is(err, store.ErrNotFound),
		"the waiting removal deletes no row and gets ErrNotFound, got %v", err)

	assert.Equal(t, 1, countChildEdgeTestAudits(t, s), "exactly one audit record for one edge")
	parents, err := s.GetDirectParentGroupIDs(ctx, child)
	require.NoError(t, err)
	assert.Empty(t, parents)
}
