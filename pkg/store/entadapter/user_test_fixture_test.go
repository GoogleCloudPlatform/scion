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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests run on SQLite by default and on Postgres when built with
// -tags integration and SCION_TEST_POSTGRES_URL set (enttest.NewClient).

func newFixtureTestStore(t *testing.T) *CompositeStore {
	t.Helper()
	cs := NewCompositeStore(enttest.NewClient(t))
	t.Cleanup(func() { _ = cs.Close() })
	require.NoError(t, cs.Migrate(context.Background()))
	return cs
}

func newFixtureUser(issuer string, expiresAt time.Time) *store.User {
	purpose := "store test"
	return &store.User{
		ID:          uuid.NewString(),
		Email:       "fixture-" + uuid.NewString()[:8] + "@" + store.TestFixtureEmailDomain,
		DisplayName: "Fixture",
		Role:        store.UserRoleMember,
		Status:      store.UserStatusActive,
		Kind:        store.UserKindTestFixture,
		ExpiresAt:   &expiresAt,
		IssuedBy:    &issuer,
		Purpose:     &purpose,
	}
}

// T2 (ii): the general CreateUser refuses every test-fixture shape.
func TestCreateUser_RefusesTestFixtureRows(t *testing.T) {
	cs := newFixtureTestStore(t)
	ctx := context.Background()
	exp := time.Now().Add(time.Hour)
	issuer := uuid.NewString()

	cases := map[string]*store.User{
		"kind test_fixture":         {ID: uuid.NewString(), Email: "a-" + uuid.NewString() + "@example.com", Kind: store.UserKindTestFixture, ExpiresAt: &exp},
		"unknown kind":              {ID: uuid.NewString(), Email: "b-" + uuid.NewString() + "@example.com", Kind: "robot"},
		"expires_at set":            {ID: uuid.NewString(), Email: "c-" + uuid.NewString() + "@example.com", ExpiresAt: &exp},
		"issued_by set":             {ID: uuid.NewString(), Email: "d-" + uuid.NewString() + "@example.com", IssuedBy: &issuer},
		"fixture domain":            {ID: uuid.NewString(), Email: "e-" + uuid.NewString() + "@" + store.TestFixtureEmailDomain},
		"fixture domain upper case": {ID: uuid.NewString(), Email: "F-" + uuid.NewString() + "@SCION-FIXTURE.INVALID"},
		"fixture subdomain":         {ID: uuid.NewString(), Email: "g-" + uuid.NewString() + "@x." + store.TestFixtureEmailDomain},
	}
	for name, u := range cases {
		t.Run(name, func(t *testing.T) {
			err := cs.CreateUser(ctx, u)
			require.ErrorIs(t, err, store.ErrTestFixtureKindRefused)
			_, getErr := cs.GetUser(ctx, u.ID)
			require.ErrorIs(t, getErr, store.ErrNotFound, "no row may be written")
		})
	}

	// The test-login domain is an ordinary domain: CreateUser accepts it.
	ok := &store.User{ID: uuid.NewString(), Email: "test-fixture-ab@scion-test.invalid", DisplayName: "x"}
	require.NoError(t, cs.CreateUser(ctx, ok))
	got, err := cs.GetUser(ctx, ok.ID)
	require.NoError(t, err)
	assert.Equal(t, store.UserKindHuman, got.Kind)
	assert.False(t, got.IsTestFixture())
}

func TestCreateTestFixtureUser_RoundTripAndValidation(t *testing.T) {
	cs := newFixtureTestStore(t)
	ctx := context.Background()
	issuer := uuid.NewString()
	exp := time.Now().Add(2 * time.Hour).UTC()

	u := newFixtureUser(issuer, exp)
	require.NoError(t, cs.CreateTestFixtureUser(ctx, u))
	got, err := cs.GetUser(ctx, u.ID)
	require.NoError(t, err)
	assert.True(t, got.IsTestFixture())
	assert.True(t, store.IsTestFixtureEmail(got.Email))
	require.NotNil(t, got.ExpiresAt)
	assert.True(t, sameInstantAtStoredPrecision(exp, *got.ExpiresAt))
	require.NotNil(t, got.IssuedBy)
	assert.Equal(t, issuer, *got.IssuedBy)
	require.NotNil(t, got.Purpose)
	assert.Equal(t, "store test", *got.Purpose)

	bad := map[string]func(u *store.User){
		"human kind":     func(u *store.User) { u.Kind = store.UserKindHuman },
		"no expiry":      func(u *store.User) { u.ExpiresAt = nil },
		"no issuer":      func(u *store.User) { u.IssuedBy = nil },
		"blank issuer":   func(u *store.User) { s := " "; u.IssuedBy = &s },
		"other domain":   func(u *store.User) { u.Email = "x-" + uuid.NewString() + "@example.com" },
		"test-login dom": func(u *store.User) { u.Email = "x-" + uuid.NewString() + "@scion-test.invalid" },
		"admin role":     func(u *store.User) { u.Role = store.UserRoleAdmin },
		"empty role":     func(u *store.User) { u.Role = "" },
	}
	for name, mutate := range bad {
		t.Run(name, func(t *testing.T) {
			u := newFixtureUser(issuer, exp)
			mutate(u)
			err := cs.CreateTestFixtureUser(ctx, u)
			require.ErrorIs(t, err, store.ErrInvalidInput)
			_, getErr := cs.GetUser(ctx, u.ID)
			require.ErrorIs(t, getErr, store.ErrNotFound)
		})
	}
}

// T2 (i): kind (and the other fixture fields) are Immutable, so UpdateUser
// cannot change them in either direction.
func TestUpdateUser_CannotChangeKind(t *testing.T) {
	cs := newFixtureTestStore(t)
	ctx := context.Background()

	human := &store.User{ID: uuid.NewString(), Email: "h-" + uuid.NewString() + "@example.com", DisplayName: "Human", Role: store.UserRoleMember}
	require.NoError(t, cs.CreateUser(ctx, human))
	exp := time.Now().Add(time.Hour)
	issuer := uuid.NewString()
	human.Kind = store.UserKindTestFixture
	human.ExpiresAt = &exp
	human.IssuedBy = &issuer
	human.DisplayName = "Human renamed"
	require.NoError(t, cs.UpdateUser(ctx, human))
	got, err := cs.GetUser(ctx, human.ID)
	require.NoError(t, err)
	assert.Equal(t, "Human renamed", got.DisplayName, "the update itself applies")
	assert.Equal(t, store.UserKindHuman, got.Kind, "human must not become a fixture")
	assert.Nil(t, got.ExpiresAt)
	assert.Nil(t, got.IssuedBy)

	fixture := newFixtureUser(uuid.NewString(), time.Now().Add(time.Hour))
	require.NoError(t, cs.CreateTestFixtureUser(ctx, fixture))
	origExp := *fixture.ExpiresAt
	fixture.Kind = store.UserKindHuman
	later := origExp.Add(100 * time.Hour)
	fixture.ExpiresAt = &later
	fixture.IssuedBy = nil
	fixture.Purpose = nil
	require.NoError(t, cs.UpdateUser(ctx, fixture))
	got, err = cs.GetUser(ctx, fixture.ID)
	require.NoError(t, err)
	assert.Equal(t, store.UserKindTestFixture, got.Kind, "fixture must not become human")
	require.NotNil(t, got.ExpiresAt)
	assert.True(t, sameInstantAtStoredPrecision(origExp, *got.ExpiresAt), "expiry must not move")
	assert.NotNil(t, got.IssuedBy)
	assert.NotNil(t, got.Purpose)

	// A raw UPDATE outside the store still runs (Immutable is an ent-level
	// guard); that is why the store has no update path for these columns.
}

// T4: the DB CHECK rejects a test-fixture row without an expiry.
func TestUsers_DBCheckRejectsFixtureWithoutExpiry(t *testing.T) {
	cs := newFixtureTestStore(t)
	ctx := context.Background()
	db := cs.DB()
	require.NotNil(t, db)

	q := rebindForDialect(cs.Dialect(),
		"INSERT INTO users (id, email, display_name, role, status, created, session_generation, kind, issued_by) VALUES (?, ?, 'x', 'member', 'active', ?, 0, 'test_fixture', ?)")
	_, err := db.ExecContext(ctx, q, uuid.NewString(), "raw-"+uuid.NewString()+"@"+store.TestFixtureEmailDomain, time.Now().UTC(), uuid.NewString())
	require.Error(t, err)
	require.Contains(t, strings.ToLower(err.Error()), "check constraint")

	// A human row without an expiry is fine.
	q = rebindForDialect(cs.Dialect(),
		"INSERT INTO users (id, email, display_name, role, status, created, session_generation, kind) VALUES (?, ?, 'x', 'member', 'active', ?, 0, 'human')")
	_, err = db.ExecContext(ctx, q, uuid.NewString(), "raw-"+uuid.NewString()+"@example.com", time.Now().UTC())
	require.NoError(t, err)
}

func TestTestFixtureUsers_CountAndList(t *testing.T) {
	cs := newFixtureTestStore(t)
	ctx := context.Background()
	now := time.Now()
	a, b := uuid.NewString(), uuid.NewString()

	for i := 0; i < 2; i++ {
		require.NoError(t, cs.CreateTestFixtureUser(ctx, newFixtureUser(a, now.Add(time.Hour))))
	}
	require.NoError(t, cs.CreateTestFixtureUser(ctx, newFixtureUser(b, now.Add(time.Hour))))
	// Expired fixture: listed but not live.
	require.NoError(t, cs.CreateTestFixtureUser(ctx, newFixtureUser(a, now.Add(-time.Minute))))
	// A human user is never counted.
	require.NoError(t, cs.CreateUser(ctx, &store.User{ID: uuid.NewString(), Email: "h-" + uuid.NewString() + "@example.com"}))

	n, err := cs.CountLiveTestFixtureUsers(ctx, a, now)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	n, err = cs.CountLiveTestFixtureUsers(ctx, b, now)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	n, err = cs.CountLiveTestFixtureUsers(ctx, "", now)
	require.NoError(t, err)
	assert.Equal(t, 3, n)

	list, err := cs.ListTestFixtureUsers(ctx, a, time.Time{}, 0)
	require.NoError(t, err)
	assert.Len(t, list, 3)
	for _, u := range list {
		assert.Equal(t, a, *u.IssuedBy)
	}
	all, err := cs.ListTestFixtureUsers(ctx, "", time.Time{}, 0)
	require.NoError(t, err)
	assert.Len(t, all, 4)

	// Live only: the expired fixture is left out.
	live, err := cs.ListTestFixtureUsers(ctx, a, now, 0)
	require.NoError(t, err)
	assert.Len(t, live, 2)
	for _, u := range live {
		assert.True(t, u.ExpiresAt.After(now))
	}
	// A limit caps the result.
	capped, err := cs.ListTestFixtureUsers(ctx, "", time.Time{}, 2)
	require.NoError(t, err)
	assert.Len(t, capped, 2)
}

// T5 (store half): LockTestFixtureIssuance plus count-then-insert in one
// transaction never exceeds the cap under concurrency. On SQLite the
// single writer serializes; on Postgres the advisory lock does.
func TestLockTestFixtureIssuance_SerializesCountAndInsert(t *testing.T) {
	cs := newFixtureTestStore(t)
	ctx := context.Background()
	issuer := uuid.NewString()
	const capN, workers = 3, 10

	errCap := errors.New("cap")
	var wg sync.WaitGroup
	results := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- retryBusy(func() error {
				return cs.WithTx(ctx, func(tx store.Store) error {
					if err := tx.LockTestFixtureIssuance(ctx); err != nil {
						return err
					}
					n, err := tx.CountLiveTestFixtureUsers(ctx, issuer, time.Now())
					if err != nil {
						return err
					}
					if n >= capN {
						return errCap
					}
					return tx.CreateTestFixtureUser(ctx, newFixtureUser(issuer, time.Now().Add(time.Hour)))
				})
			})
		}()
	}
	wg.Wait()
	close(results)
	ok, capped := 0, 0
	for err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, errCap):
			capped++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	assert.Equal(t, capN, ok)
	assert.Equal(t, workers-capN, capped)
	n, err := cs.CountLiveTestFixtureUsers(ctx, issuer, time.Now())
	require.NoError(t, err)
	assert.Equal(t, capN, n)
}

// retryBusy retries fn while SQLite reports the database as busy or
// locked; a busy writer is the SQLite form of serialization.
func retryBusy(fn func() error) error {
	var err error
	for i := 0; i < 200; i++ {
		err = fn()
		if err == nil {
			return nil
		}
		msg := strings.ToLower(err.Error())
		if !strings.Contains(msg, "busy") && !strings.Contains(msg, "locked") {
			return err
		}
		time.Sleep(5 * time.Millisecond)
	}
	return err
}

// The email of a human row cannot move into the reserved fixture domain,
// and a fixture row's email cannot leave it.
func TestUpdateUser_EmailStaysOnItsSideOfTheFixtureDomain(t *testing.T) {
	cs := newFixtureTestStore(t)
	ctx := context.Background()

	human := &store.User{ID: uuid.NewString(), Email: "h-" + uuid.NewString()[:8] + "@example.com", DisplayName: "h", Role: store.UserRoleMember}
	require.NoError(t, cs.CreateUser(ctx, human))
	human.Email = "moved@" + store.TestFixtureEmailDomain
	require.ErrorIs(t, cs.UpdateUser(ctx, human), store.ErrTestFixtureKindRefused)

	fixture := newFixtureUser(uuid.NewString(), time.Now().Add(time.Hour))
	require.NoError(t, cs.CreateTestFixtureUser(ctx, fixture))
	fixture.Email = "escaped-" + uuid.NewString()[:8] + "@example.com"
	require.ErrorIs(t, cs.UpdateUser(ctx, fixture), store.ErrTestFixtureKindRefused)

	// A fixture row updated in place (same email) is fine.
	got, err := cs.GetUser(ctx, fixture.ID)
	require.NoError(t, err)
	got.DisplayName = "renamed"
	require.NoError(t, cs.UpdateUser(ctx, got))
}
