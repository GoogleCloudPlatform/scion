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
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLockInvitedHubRoleGuard_RequiresWithTx(t *testing.T) {
	cs := newFixtureTestStore(t)
	ctx := context.Background()

	err := cs.LockInvitedHubRoleGuard(ctx)
	require.ErrorIs(t, err, store.ErrInvalidInput)

	err = cs.WithTx(ctx, func(tx store.Store) error {
		return tx.LockInvitedHubRoleGuard(ctx)
	})
	require.NoError(t, err)
}

// TestLockInvitedHubRoleGuard_SerializesWriteSkew verifies that
// LockInvitedHubRoleGuard prevents write skew between granting a built-in
// system hub role to a group and adding an invited user to that group
// (ptone/scion#8081). Runs on SQLite by default and on PostgreSQL when built
// with -tags integration and SCION_TEST_POSTGRES_URL set.
func TestLockInvitedHubRoleGuard_SerializesWriteSkew(t *testing.T) {
	cs := newFixtureTestStore(t)
	ctx := context.Background()

	viewerRD, err := cs.CreateRoleDefinition(ctx, &store.RoleDefinition{
		ID:          uuid.NewString(),
		Name:        store.SystemRoleHubViewer,
		ScopeType:   store.RoleScopeSystem,
		System:      true,
		Permissions: []string{"project.read"},
	})
	require.NoError(t, err)

	invited := &store.User{
		ID:          uuid.NewString(),
		Email:       "invited-skew@example.com",
		DisplayName: "Invited Skew",
		Role:        store.UserRoleMember,
		Status:      store.UserStatusInvited,
	}
	require.NoError(t, cs.CreateUser(ctx, invited))

	errGuard := errors.New("invited hub role guard conflict")
	const iterations = 12
	for iter := 0; iter < iterations; iter++ {
		grp := &store.Group{
			ID:        uuid.NewString(),
			Name:      fmt.Sprintf("Skew Group %d", iter),
			Slug:      fmt.Sprintf("skew-group-%d", iter),
			GroupType: store.GroupTypeExplicit,
		}
		require.NoError(t, cs.CreateGroup(ctx, grp))

		start := make(chan struct{})
		var wg sync.WaitGroup
		var bindErr, addErr error
		wg.Add(2)

		go func() {
			defer wg.Done()
			<-start
			bindErr = retryBusy(func() error {
				return cs.WithTx(ctx, func(tx store.Store) error {
					if err := tx.LockInvitedHubRoleGuard(ctx); err != nil {
						return err
					}
					members, err := tx.GetGroupMembers(ctx, grp.ID)
					if err != nil {
						return err
					}
					for _, m := range members {
						if m.MemberType == store.GroupMemberTypeUser {
							u, uErr := tx.GetUser(ctx, m.MemberID)
							if uErr != nil {
								return uErr
							}
							if u.Status == store.UserStatusInvited {
								return errGuard
							}
						}
					}
					_, err = tx.CreateRoleBinding(ctx, &store.RoleBinding{
						RoleDefinitionID: viewerRD.ID,
						PrincipalType:    store.RoleBindingPrincipalGroup,
						PrincipalID:      grp.ID,
						ScopeType:        store.RoleScopeSystem,
						CreatedBy:        "test",
					})
					return err
				})
			})
		}()

		go func() {
			defer wg.Done()
			<-start
			addErr = retryBusy(func() error {
				return cs.WithTx(ctx, func(tx store.Store) error {
					if err := tx.LockInvitedHubRoleGuard(ctx); err != nil {
						return err
					}
					rbs, err := tx.ListRoleBindingsForPrincipals(ctx, []store.PrincipalRef{
						{Type: store.RoleBindingPrincipalGroup, ID: grp.ID},
					}, nil, nil)
					if err != nil {
						return err
					}
					for _, b := range rbs {
						if b.RoleDefinitionID == viewerRD.ID && b.ScopeType == store.RoleScopeSystem {
							return errGuard
						}
					}
					return tx.AddGroupMember(ctx, &store.GroupMember{
						GroupID:    grp.ID,
						MemberType: store.GroupMemberTypeUser,
						MemberID:   invited.ID,
						Role:       store.GroupMemberRoleMember,
					})
				})
			})
		}()

		close(start)
		wg.Wait()

		// Exactly one side commits and the second sees the committed write and rejects.
		assert.False(t, bindErr == nil && addErr == nil,
			"iter %d: write skew allowed both role binding and invited member add", iter)
		if bindErr == nil {
			require.ErrorIs(t, addErr, errGuard, "iter %d", iter)
		} else {
			require.ErrorIs(t, bindErr, errGuard, "iter %d", iter)
			require.NoError(t, addErr, "iter %d", iter)
		}
	}
}

// TestLockInvitedHubRoleGuard_BlocksConcurrentHolder_Postgres deterministically
// proves on PostgreSQL that T2 blocks on pg_advisory_xact_lock while T1 holds
// LockInvitedHubRoleGuard, and proceeds once T1 commits (ptone/scion#8081).
func TestLockInvitedHubRoleGuard_BlocksConcurrentHolder_Postgres(t *testing.T) {
	skipUnlessPostgres(t)

	cs := newFixtureTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), lockOrderTimeout)
	defer cancel()
	db := cs.DB()
	require.NotNil(t, db)

	tx1, err := cs.client.Tx(ctx)
	require.NoError(t, err)
	tx1Done := false
	defer func() {
		if !tx1Done {
			_ = tx1.Rollback()
		}
	}()

	require.NoError(t, newTxCompositeStore(tx1).LockInvitedHubRoleGuard(ctx))

	t2Done := make(chan error, 1)
	go func() {
		t2Done <- cs.WithTx(ctx, func(tx store.Store) error {
			return tx.LockInvitedHubRoleGuard(ctx)
		})
	}()

	require.Eventually(t, func() bool {
		var waiting int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM pg_stat_activity
			  WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			return false
		}
		return waiting > 0
	}, lockOrderTimeout/2, 20*time.Millisecond, "T2 never blocked on LockInvitedHubRoleGuard")

	require.NoError(t, tx1.Commit())
	tx1Done = true

	select {
	case err := <-t2Done:
		require.NoError(t, err)
	case <-time.After(lockOrderTimeout):
		t.Fatal("T2 timed out after T1 released LockInvitedHubRoleGuard")
	}
}
