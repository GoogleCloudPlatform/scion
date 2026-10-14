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

// Test identity web-login codes (ptone/scion#4240, WS1). These run on
// SQLite by default and on Postgres when built with -tags integration and
// SCION_TEST_POSTGRES_URL set (enttest.NewClient).

func newLoginCode(userID, hash string, expiresAt time.Time) *store.TestIdentityLoginCode {
	return &store.TestIdentityLoginCode{UserID: userID, CodeHash: hash, IssuedBy: uuid.NewString(), ExpiresAt: expiresAt}
}

func TestTestIdentityLoginCode_ConsumeOnce(t *testing.T) {
	cs := newFixtureTestStore(t)
	ctx := context.Background()
	now := time.Now()
	u := newFixtureUser(uuid.NewString(), now.Add(time.Hour))
	require.NoError(t, cs.CreateTestFixtureUser(ctx, u))

	code := newLoginCode(u.ID, "hash-a", now.Add(5*time.Minute))
	require.NoError(t, cs.ReplaceTestIdentityLoginCode(ctx, code))
	require.NotEmpty(t, code.ID)

	got, err := cs.ConsumeTestIdentityLoginCode(ctx, "hash-a", now)
	require.NoError(t, err)
	assert.Equal(t, code.ID, got.ID)
	assert.Equal(t, u.ID, got.UserID)
	require.NotNil(t, got.ConsumedAt)

	_, err = cs.ConsumeTestIdentityLoginCode(ctx, "hash-a", now)
	assert.ErrorIs(t, err, store.ErrTestIdentityLoginCodeConsumed)
	_, err = cs.ConsumeTestIdentityLoginCode(ctx, "hash-unknown", now)
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = cs.ConsumeTestIdentityLoginCode(ctx, "", now)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// The window is exclusive at expires_at: one second before it redeems, at
// or after it does not.
func TestTestIdentityLoginCode_Expiry(t *testing.T) {
	cs := newFixtureTestStore(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	u := newFixtureUser(uuid.NewString(), now.Add(time.Hour))
	require.NoError(t, cs.CreateTestFixtureUser(ctx, u))

	require.NoError(t, cs.ReplaceTestIdentityLoginCode(ctx, newLoginCode(u.ID, "hash-exp", now.Add(5*time.Minute))))
	_, err := cs.ConsumeTestIdentityLoginCode(ctx, "hash-exp", now.Add(5*time.Minute+time.Second))
	assert.ErrorIs(t, err, store.ErrTestIdentityLoginCodeExpired)
	_, err = cs.ConsumeTestIdentityLoginCode(ctx, "hash-exp", now.Add(5*time.Minute))
	assert.ErrorIs(t, err, store.ErrTestIdentityLoginCodeExpired)
	_, err = cs.ConsumeTestIdentityLoginCode(ctx, "hash-exp", now.Add(4*time.Minute+59*time.Second))
	assert.NoError(t, err)
}

// A second code for the same identity replaces the first: the first no
// longer redeems, and the identity holds exactly one row.
func TestTestIdentityLoginCode_ReplaceInvalidatesPrevious(t *testing.T) {
	cs := newFixtureTestStore(t)
	ctx := context.Background()
	now := time.Now()
	u := newFixtureUser(uuid.NewString(), now.Add(time.Hour))
	require.NoError(t, cs.CreateTestFixtureUser(ctx, u))

	require.NoError(t, cs.ReplaceTestIdentityLoginCode(ctx, newLoginCode(u.ID, "hash-1", now.Add(5*time.Minute))))
	require.NoError(t, cs.ReplaceTestIdentityLoginCode(ctx, newLoginCode(u.ID, "hash-2", now.Add(5*time.Minute))))

	_, err := cs.ConsumeTestIdentityLoginCode(ctx, "hash-1", now)
	assert.ErrorIs(t, err, store.ErrNotFound)
	n, err := cs.client.TestIdentityLoginCode.Query().Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	_, err = cs.ConsumeTestIdentityLoginCode(ctx, "hash-2", now)
	assert.NoError(t, err)
}

func TestTestIdentityLoginCode_ReplaceValidates(t *testing.T) {
	cs := newFixtureTestStore(t)
	ctx := context.Background()
	exp := time.Now().Add(time.Minute)
	for name, c := range map[string]*store.TestIdentityLoginCode{
		"nil":       nil,
		"no hash":   {UserID: uuid.NewString(), IssuedBy: "i", ExpiresAt: exp},
		"no issuer": {UserID: uuid.NewString(), CodeHash: "h", ExpiresAt: exp},
		"no expiry": {UserID: uuid.NewString(), CodeHash: "h", IssuedBy: "i"},
		"bad user":  {UserID: "not-a-uuid", CodeHash: "h", IssuedBy: "i", ExpiresAt: exp},
	} {
		t.Run(name, func(t *testing.T) {
			assert.ErrorIs(t, cs.ReplaceTestIdentityLoginCode(ctx, c), store.ErrInvalidInput)
		})
	}
}

// The code row goes with its user (ON DELETE CASCADE).
func TestTestIdentityLoginCode_CascadesWithUser(t *testing.T) {
	cs := newFixtureTestStore(t)
	ctx := context.Background()
	now := time.Now()
	u := newFixtureUser(uuid.NewString(), now.Add(time.Hour))
	require.NoError(t, cs.CreateTestFixtureUser(ctx, u))
	require.NoError(t, cs.ReplaceTestIdentityLoginCode(ctx, newLoginCode(u.ID, "hash-c", now.Add(5*time.Minute))))

	require.NoError(t, cs.DeleteUser(ctx, u.ID))
	n, err := cs.client.TestIdentityLoginCode.Query().Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
	_, err = cs.ConsumeTestIdentityLoginCode(ctx, "hash-c", now)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// Of many concurrent redeems of one code, exactly one succeeds; every other
// call reports the code as already used.
func TestTestIdentityLoginCode_ConcurrentConsumeExactlyOnce(t *testing.T) {
	cs := newFixtureTestStore(t)
	ctx := context.Background()
	now := time.Now()
	u := newFixtureUser(uuid.NewString(), now.Add(time.Hour))
	require.NoError(t, cs.CreateTestFixtureUser(ctx, u))
	require.NoError(t, cs.ReplaceTestIdentityLoginCode(ctx, newLoginCode(u.ID, "hash-race", now.Add(5*time.Minute))))

	const workers = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- retryBusy(func() error {
				_, err := cs.ConsumeTestIdentityLoginCode(ctx, "hash-race", now)
				return err
			})
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	ok, used := 0, 0
	for err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, store.ErrTestIdentityLoginCodeConsumed):
			used++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	assert.Equal(t, 1, ok)
	assert.Equal(t, workers-1, used)
}
