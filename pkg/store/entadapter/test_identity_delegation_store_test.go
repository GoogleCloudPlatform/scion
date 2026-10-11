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
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Store tests for workload-bound issuer delegations (ptone/scion#4240,
// phase X2). Like user_test_fixture_test.go they run on SQLite by default
// and on Postgres with -tags integration and SCION_TEST_POSTGRES_URL set
// (the T1 store job runs the whole package there).

func newDelegation(grantor string, created time.Time, ttl time.Duration) *store.TestIdentityDelegation {
	return &store.TestIdentityDelegation{
		GrantorID:   grantor,
		WorkloadSub: "100000000000000000042",
		Scope:       "test_identity:issue",
		MemberCap:   5,
		Label:       "window",
		Created:     created,
		ExpiresAt:   created.Add(ttl),
	}
}

func TestTestIdentityDelegation_CreateGetListRevoke(t *testing.T) {
	cs := newFixtureTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	a, b := uuid.NewString(), uuid.NewString()

	d1 := newDelegation(a, now.Add(-time.Minute), time.Hour)
	require.NoError(t, cs.CreateTestIdentityDelegation(ctx, d1))
	require.NotEmpty(t, d1.ID)
	d2 := newDelegation(a, now, time.Hour)
	require.NoError(t, cs.CreateTestIdentityDelegation(ctx, d2))
	require.NoError(t, cs.CreateTestIdentityDelegation(ctx, newDelegation(b, now, time.Hour)))

	got, err := cs.GetTestIdentityDelegation(ctx, d1.ID)
	require.NoError(t, err)
	assert.Equal(t, a, got.GrantorID)
	assert.Equal(t, "100000000000000000042", got.WorkloadSub)
	assert.Equal(t, "test_identity:issue", got.Scope)
	assert.Equal(t, 5, got.MemberCap)
	assert.Equal(t, "window", got.Label)
	assert.True(t, got.ExpiresAt.Equal(d1.ExpiresAt))
	assert.Nil(t, got.RevokedAt)
	assert.True(t, got.LiveAt(now))

	_, err = cs.GetTestIdentityDelegation(ctx, uuid.NewString())
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = cs.GetTestIdentityDelegation(ctx, "not-a-uuid")
	assert.ErrorIs(t, err, store.ErrNotFound)

	mine, err := cs.ListTestIdentityDelegations(ctx, a, 0)
	require.NoError(t, err)
	require.Len(t, mine, 2)
	assert.Equal(t, d2.ID, mine[0].ID, "newest first")
	all, err := cs.ListTestIdentityDelegations(ctx, "", 0)
	require.NoError(t, err)
	assert.Len(t, all, 3)
	limited, err := cs.ListTestIdentityDelegations(ctx, "", 1)
	require.NoError(t, err)
	assert.Len(t, limited, 1)

	// Revoke once; a second revoke changes nothing.
	revoker := uuid.NewString()
	require.NoError(t, cs.RevokeTestIdentityDelegation(ctx, d1.ID, revoker, now))
	got, err = cs.GetTestIdentityDelegation(ctx, d1.ID)
	require.NoError(t, err)
	require.NotNil(t, got.RevokedAt)
	require.NotNil(t, got.RevokedBy)
	assert.Equal(t, revoker, *got.RevokedBy)
	assert.False(t, got.LiveAt(now))
	require.NoError(t, cs.RevokeTestIdentityDelegation(ctx, d1.ID, "someone-else", now.Add(time.Hour)))
	again, err := cs.GetTestIdentityDelegation(ctx, d1.ID)
	require.NoError(t, err)
	assert.Equal(t, revoker, *again.RevokedBy, "the first revocation is kept")
	assert.True(t, again.RevokedAt.Equal(*got.RevokedAt))

	assert.ErrorIs(t, cs.RevokeTestIdentityDelegation(ctx, uuid.NewString(), revoker, now), store.ErrNotFound)
}

func TestTestIdentityDelegation_CreateValidation(t *testing.T) {
	cs := newFixtureTestStore(t)
	ctx := context.Background()
	now := time.Now()
	revoked := now
	who := "x"
	cases := map[string]func(d *store.TestIdentityDelegation){
		"no grantor":        func(d *store.TestIdentityDelegation) { d.GrantorID = "" },
		"no sub":            func(d *store.TestIdentityDelegation) { d.WorkloadSub = " " },
		"no scope":          func(d *store.TestIdentityDelegation) { d.Scope = "" },
		"two scopes":        func(d *store.TestIdentityDelegation) { d.Scope = "test_identity:issue test_identity:issue_admin" },
		"zero cap":          func(d *store.TestIdentityDelegation) { d.MemberCap = 0 },
		"expiry not after":  func(d *store.TestIdentityDelegation) { d.ExpiresAt = d.Created },
		"created revoked":   func(d *store.TestIdentityDelegation) { d.RevokedAt = &revoked },
		"created revokedBy": func(d *store.TestIdentityDelegation) { d.RevokedBy = &who },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			d := newDelegation(uuid.NewString(), now, time.Hour)
			mutate(d)
			assert.ErrorIs(t, cs.CreateTestIdentityDelegation(ctx, d), store.ErrInvalidInput)
		})
	}
}

// The general CreateUser refuses a delegation stamp; only
// CreateTestFixtureUser writes it, and the per-delegation count and list
// key on it.
func TestTestIdentityDelegation_FixtureStampAndCount(t *testing.T) {
	cs := newFixtureTestStore(t)
	ctx := context.Background()
	now := time.Now()
	issuer := uuid.NewString()
	dID := uuid.NewString()

	stamp := dID
	refused := &store.User{ID: uuid.NewString(), Email: "h-" + uuid.NewString()[:8] + "@example.com", IssuerDelegationID: &stamp}
	require.ErrorIs(t, cs.CreateUser(ctx, refused), store.ErrTestFixtureKindRefused)

	empty := ""
	bad := newFixtureUser(issuer, now.Add(time.Hour))
	bad.IssuerDelegationID = &empty
	require.ErrorIs(t, cs.CreateTestFixtureUser(ctx, bad), store.ErrInvalidInput)

	for i := 0; i < 2; i++ {
		u := newFixtureUser(issuer, now.Add(time.Hour))
		id := dID
		u.IssuerDelegationID = &id
		require.NoError(t, cs.CreateTestFixtureUser(ctx, u))
	}
	expired := newFixtureUser(issuer, now.Add(-time.Minute))
	id := dID
	expired.IssuerDelegationID = &id
	require.NoError(t, cs.CreateTestFixtureUser(ctx, expired))
	require.NoError(t, cs.CreateTestFixtureUser(ctx, newFixtureUser(issuer, now.Add(time.Hour)))) // session-issued

	n, err := cs.CountLiveTestFixtureUsersByDelegation(ctx, dID, now)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	n, err = cs.CountLiveTestFixtureUsersByDelegation(ctx, uuid.NewString(), now)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
	_, err = cs.CountLiveTestFixtureUsersByDelegation(ctx, "", now)
	assert.ErrorIs(t, err, store.ErrInvalidInput)

	live, err := cs.ListTestFixtureUsersByDelegation(ctx, dID, now, 0)
	require.NoError(t, err)
	require.Len(t, live, 2)
	for _, u := range live {
		require.NotNil(t, u.IssuerDelegationID)
		assert.Equal(t, dID, *u.IssuerDelegationID)
	}
	all, err := cs.ListTestFixtureUsersByDelegation(ctx, dID, time.Time{}, 0)
	require.NoError(t, err)
	assert.Len(t, all, 3)

	got, err := cs.GetUser(ctx, live[0].ID)
	require.NoError(t, err)
	require.NotNil(t, got.IssuerDelegationID, "the stamp round-trips through GetUser")
}

// T5 (store half) for the per-delegation cap: lock, count by delegation,
// count hub-wide, insert, all in one transaction, from many workers across
// two delegations, never exceeds either the per-delegation cap or the
// hub-wide cap. On SQLite the single writer serializes; on Postgres the
// advisory lock does.
func TestTestIdentityDelegation_PerDelegationCapUnderLock(t *testing.T) {
	cs := newFixtureTestStore(t)
	ctx := context.Background()
	issuer := uuid.NewString()
	delegations := []string{uuid.NewString(), uuid.NewString()}
	const perDelegation, hubCap, workers = 3, 5, 16

	errCap := errors.New("cap")
	var wg sync.WaitGroup
	results := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			dID := delegations[i%2]
			results <- retryBusy(func() error {
				return cs.WithTx(ctx, func(tx store.Store) error {
					if err := tx.LockTestFixtureIssuance(ctx); err != nil {
						return err
					}
					n, err := tx.CountLiveTestFixtureUsersByDelegation(ctx, dID, time.Now())
					if err != nil {
						return err
					}
					if n >= perDelegation {
						return errCap
					}
					n, err = tx.CountLiveTestFixtureUsers(ctx, "", time.Now())
					if err != nil {
						return err
					}
					if n >= hubCap {
						return errCap
					}
					u := newFixtureUser(issuer, time.Now().Add(time.Hour))
					id := dID
					u.IssuerDelegationID = &id
					return tx.CreateTestFixtureUser(ctx, u)
				})
			})
		}(i)
	}
	wg.Wait()
	close(results)
	ok := 0
	for err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, errCap):
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	assert.Equal(t, hubCap, ok, "the hub-wide cap bounds the sum across delegations")
	total := 0
	for _, dID := range delegations {
		n, err := cs.CountLiveTestFixtureUsersByDelegation(ctx, dID, time.Now())
		require.NoError(t, err)
		assert.LessOrEqual(t, n, perDelegation)
		total += n
	}
	assert.Equal(t, hubCap, total)
}
