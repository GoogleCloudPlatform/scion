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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// finalizeFixture is a composite store with one agent in finalizing under
// claim 1, plus a DELETED subscription on it (a cascade target).
func finalizeFixture(t *testing.T) (*CompositeStore, *store.Agent, store.DeletionPredicate) {
	t.Helper()
	ctx := context.Background()
	cs := newTestCompositeStore(t)
	projectID := uuid.NewString()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{ID: projectID, Name: "fin", Slug: "fin"}))
	a := makeAgent(projectID, "fin-agent")
	require.NoError(t, cs.CreateAgent(ctx, a))
	require.NoError(t, cs.CreateNotificationSubscription(ctx, &store.NotificationSubscription{
		ID: uuid.NewString(), Scope: store.SubscriptionScopeAgent, AgentID: a.ID,
		SubscriberType: store.SubscriberTypeUser, SubscriberID: "watcher", ProjectID: projectID,
		TriggerActivities: []string{"DELETED"}, CreatedAt: time.Now(), CreatedBy: "test",
	}))
	seedDeletion(t, cs.AgentStore, a.ID, store.DeletionStateFinalizing, time.Now().Add(time.Minute), "")
	claim := int64(1)
	return cs, a, store.DeletionPredicate{Claim: &claim, States: []string{store.DeletionStateFinalizing}, DeletedAtNull: true}
}

func subscriptionCount(t *testing.T, cs *CompositeStore, agentID string) int {
	t.Helper()
	subs, err := cs.GetNotificationSubscriptions(context.Background(), agentID)
	require.NoError(t, err)
	return len(subs)
}

func TestFinalizeAgentDeletion_Soft(t *testing.T) {
	ctx := context.Background()
	cs, a, pred := finalizeFixture(t)
	now := time.Now()
	none := store.DeletionStateNone
	var hookAgent *store.Agent
	n, err := cs.FinalizeAgentDeletion(ctx, a.ID, pred, store.DeletionFinalizeSoft,
		store.DeletionFields{State: &none, DeletedAt: &now, ClearLeaseAt: true},
		func(_ context.Context, tx store.Store, got *store.Agent, mode store.DeletionFinalizeMode) error {
			assert.Equal(t, store.DeletionFinalizeSoft, mode)
			hookAgent = got
			return nil
		})
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	require.NotNil(t, hookAgent)
	assert.False(t, hookAgent.DeletedAt.IsZero(), "the hook sees the post-write row")

	got, err := cs.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.False(t, got.DeletedAt.IsZero())
	assert.Equal(t, store.DeletionStateNone, got.DeletionState)
	assert.Equal(t, 1, subscriptionCount(t, cs, a.ID), "a soft delete keeps subscriptions")
}

func TestFinalizeAgentDeletion_HardCascades(t *testing.T) {
	ctx := context.Background()
	cs, a, pred := finalizeFixture(t)
	var hookAgent *store.Agent
	n, err := cs.FinalizeAgentDeletion(ctx, a.ID, pred, store.DeletionFinalizeHard, store.DeletionFields{},
		func(_ context.Context, _ store.Store, got *store.Agent, mode store.DeletionFinalizeMode) error {
			assert.Equal(t, store.DeletionFinalizeHard, mode)
			hookAgent = got
			return nil
		})
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	require.NotNil(t, hookAgent)
	assert.Equal(t, a.ID, hookAgent.ID, "the hook sees the pre-delete row")

	_, err = cs.GetAgent(ctx, a.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	assert.Zero(t, subscriptionCount(t, cs, a.ID))
	_, err = cs.GetAgentBySlug(ctx, a.ProjectID, a.Slug)
	assert.ErrorIs(t, err, store.ErrNotFound)
	require.NoError(t, cs.CreateAgent(ctx, makeAgent(a.ProjectID, a.Slug)), "the name can be reused")
}

func TestFinalizeAgentDeletion_PredicateMiss(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []store.DeletionFinalizeMode{store.DeletionFinalizeSoft, store.DeletionFinalizeHard} {
		t.Run(string(mode), func(t *testing.T) {
			cs, a, pred := finalizeFixture(t)
			stale := int64(0)
			pred.Claim = &stale
			called := false
			now := time.Now()
			n, err := cs.FinalizeAgentDeletion(ctx, a.ID, pred, mode, store.DeletionFields{DeletedAt: &now},
				func(context.Context, store.Store, *store.Agent, store.DeletionFinalizeMode) error {
					called = true
					return nil
				})
			require.NoError(t, err)
			assert.Zero(t, n)
			assert.False(t, called)
			got, err := cs.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			assert.True(t, got.DeletedAt.IsZero())

			n, err = cs.FinalizeAgentDeletion(ctx, uuid.NewString(), pred, mode, store.DeletionFields{}, nil)
			require.NoError(t, err, "a missing agent is affected=0, not an error")
			assert.Zero(t, n)
		})
	}
}

// A hook error rolls back the whole finalize, including writes the hook
// made through the transaction-scoped store.
func TestFinalizeAgentDeletion_HookErrorRollsBack(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []store.DeletionFinalizeMode{store.DeletionFinalizeSoft, store.DeletionFinalizeHard} {
		t.Run(string(mode), func(t *testing.T) {
			cs, a, pred := finalizeFixture(t)
			boom := errors.New("hook refused")
			now := time.Now()
			n, err := cs.FinalizeAgentDeletion(ctx, a.ID, pred, mode, store.DeletionFields{DeletedAt: &now},
				func(ctx context.Context, tx store.Store, got *store.Agent, _ store.DeletionFinalizeMode) error {
					require.NoError(t, tx.CreateProject(ctx, &store.Project{ID: uuid.NewString(), Name: "in-tx", Slug: "in-tx"}))
					return boom
				})
			require.ErrorIs(t, err, boom)
			assert.Zero(t, n)

			got, err := cs.GetAgent(ctx, a.ID)
			require.NoError(t, err, "the row survives")
			assert.True(t, got.DeletedAt.IsZero())
			assert.Equal(t, store.DeletionStateFinalizing, got.DeletionState)
			assert.Equal(t, 1, subscriptionCount(t, cs, a.ID), "the cascade rolled back")
			_, err = cs.GetProjectBySlug(ctx, "in-tx")
			assert.ErrorIs(t, err, store.ErrNotFound, "the hook's own write rolled back")
		})
	}
}

func TestFinalizeAgentDeletion_RejectsBadModeAndNesting(t *testing.T) {
	ctx := context.Background()
	cs, a, pred := finalizeFixture(t)
	_, err := cs.FinalizeAgentDeletion(ctx, a.ID, pred, "bogus", store.DeletionFields{}, nil)
	assert.ErrorIs(t, err, store.ErrInvalidInput)
	err = cs.WithTx(ctx, func(tx store.Store) error {
		_, err := tx.FinalizeAgentDeletion(ctx, a.ID, pred, store.DeletionFinalizeHard, store.DeletionFields{}, nil)
		return err
	})
	assert.ErrorIs(t, err, store.ErrInvalidInput)
}

// DeletionFields.DeletedAt sets deleted_at; KeepUpdated leaves updated
// alone (the schema's UpdateDefault would otherwise stamp it), while
// state_version is still bumped.
func TestUpdateAgentDeletion_DeletedAtAndKeepUpdated(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := makeAgent(projectID, "del-keep")
	require.NoError(t, s.CreateAgent(ctx, a))
	seedDeletion(t, s, a.ID, store.DeletionStateDeleting, time.Now().Add(time.Minute), "")
	before, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)

	time.Sleep(10 * time.Millisecond)
	lease := time.Now().Add(2 * time.Minute)
	n, err := s.UpdateAgentDeletion(ctx, a.ID, store.DeletionPredicate{}, store.DeletionFields{LeaseAt: &lease, KeepUpdated: true})
	require.NoError(t, err)
	require.Equal(t, 1, n)
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.True(t, got.Updated.Equal(before.Updated), "updated unchanged")
	assert.Equal(t, before.StateVersion+1, got.StateVersion)

	time.Sleep(10 * time.Millisecond)
	now := time.Now()
	n, err = s.UpdateAgentDeletion(ctx, a.ID, store.DeletionPredicate{}, store.DeletionFields{DeletedAt: &now})
	require.NoError(t, err)
	require.Equal(t, 1, n)
	got, err = s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.WithinDuration(t, now, got.DeletedAt, time.Millisecond)
	assert.True(t, got.Updated.After(before.Updated), "without KeepUpdated, updated moves")
}
