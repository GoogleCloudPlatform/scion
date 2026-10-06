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

//go:build integration

package integrationtest

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// lockWaitProbe is how long a transaction is expected to stay blocked on a
// row lock held by another transaction before the holder is released.
const lockWaitProbe = 300 * time.Millisecond

// TestLockUserRow_DeleteFirstFailsCreate_Postgres: the user delete holds the
// user row FOR UPDATE; an agent create for that user takes FOR KEY SHARE,
// waits for the delete to commit, then finds the user gone
// (ptone/scion#2769).
func TestLockUserRow_DeleteFirstFailsCreate_Postgres(t *testing.T) {
	cs := newStore(t)
	ctx := context.Background()
	u := makeUser("lock-del-" + shortID() + "@example.com")
	require.NoError(t, cs.CreateUser(ctx, u))

	locked := make(chan struct{})
	release := make(chan struct{})
	deleteDone := make(chan error, 1)
	go func() {
		deleteDone <- cs.WithTx(ctx, func(tx store.Store) error {
			if err := tx.LockUserRow(ctx, u.ID, true); err != nil {
				return err
			}
			close(locked)
			<-release
			return tx.DeleteUser(ctx, u.ID)
		})
	}()
	<-locked

	createDone := make(chan error, 1)
	go func() {
		createDone <- cs.WithTx(ctx, func(tx store.Store) error {
			return tx.LockUserRow(ctx, u.ID, false)
		})
	}()

	select {
	case err := <-createDone:
		t.Fatalf("FOR KEY SHARE must wait for the FOR UPDATE holder, returned early: %v", err)
	case <-time.After(lockWaitProbe):
	}
	close(release)
	require.NoError(t, <-deleteDone)
	assert.ErrorIs(t, <-createDone, store.ErrNotFound,
		"after the delete commits, the create's re-check must find the user gone")
}

// TestLockUserRow_CreateFirstIsSeenByDelete_Postgres: an agent create holds
// the owner's row FOR KEY SHARE and inserts the agent; the user delete's
// FOR UPDATE waits for it to commit, and its owned-agents read then sees the
// new agent (ptone/scion#2769).
func TestLockUserRow_CreateFirstIsSeenByDelete_Postgres(t *testing.T) {
	cs := newStore(t)
	ctx := context.Background()
	project := seedProject(t, cs)
	u := makeUser("lock-create-" + shortID() + "@example.com")
	require.NoError(t, cs.CreateUser(ctx, u))

	locked := make(chan struct{})
	release := make(chan struct{})
	createDone := make(chan error, 1)
	ag := makeAgent(project.ID, "lock-"+shortID())
	ag.OwnerID = u.ID
	ag.CreatedBy = u.ID
	go func() {
		createDone <- cs.WithTx(ctx, func(tx store.Store) error {
			if err := tx.LockUserRow(ctx, u.ID, false); err != nil {
				return err
			}
			if err := tx.CreateAgent(ctx, ag); err != nil {
				return err
			}
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked

	type result struct {
		owned int
		err   error
	}
	deleteDone := make(chan result, 1)
	go func() {
		var owned int
		err := cs.WithTx(ctx, func(tx store.Store) error {
			if err := tx.LockUserRow(ctx, u.ID, true); err != nil {
				return err
			}
			page, err := tx.ListAgents(ctx, store.AgentFilter{OwnerID: u.ID}, store.ListOptions{Limit: 10})
			if err != nil {
				return err
			}
			owned = len(page.Items)
			return nil
		})
		deleteDone <- result{owned: owned, err: err}
	}()

	select {
	case r := <-deleteDone:
		t.Fatalf("FOR UPDATE must wait for the FOR KEY SHARE holder, returned early: %+v", r)
	case <-time.After(lockWaitProbe):
	}
	close(release)
	require.NoError(t, <-createDone)
	r := <-deleteDone
	require.NoError(t, r.err)
	assert.Equal(t, 1, r.owned, "the delete's owned-agents read must see the agent committed first")
}

// TestLockUserRow_SharedDoesNotBlockUserUpdate_Postgres: the shared lock is
// FOR KEY SHARE, not FOR SHARE, so a plain UPDATE of the user row (which
// takes FOR NO KEY UPDATE), such as the last-seen write, does not wait for
// an open agent create or restore transaction (ptone/scion#2769).
func TestLockUserRow_SharedDoesNotBlockUserUpdate_Postgres(t *testing.T) {
	cs := newStore(t)
	ctx := context.Background()
	u := makeUser("lock-upd-" + shortID() + "@example.com")
	require.NoError(t, cs.CreateUser(ctx, u))

	locked := make(chan struct{})
	release := make(chan struct{})
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- cs.WithTx(ctx, func(tx store.Store) error {
			if err := tx.LockUserRow(ctx, u.ID, false); err != nil {
				return err
			}
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	defer func() {
		close(release)
		require.NoError(t, <-holderDone)
	}()

	updateDone := make(chan error, 1)
	go func() {
		updateDone <- cs.UpdateUserLastSeen(ctx, u.ID, time.Now())
	}()
	select {
	case err := <-updateDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("a plain user-row UPDATE must not wait for the shared (FOR KEY SHARE) lock holder")
	}
}
