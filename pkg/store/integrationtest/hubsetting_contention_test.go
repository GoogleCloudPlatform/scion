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
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestContention_HubSettingCAS races N goroutines to perform a CAS upsert on
// the same hub-setting section at the same expectedRevision. Under Postgres
// the SELECT ... FOR UPDATE row lock serializes the read-check-update within
// each transaction, so exactly one writer must win and the remaining N-1 must
// receive ErrRevisionConflict. This exercises the Postgres-specific row-locking
// path that the SQLite single-writer test cannot reach.
//
// Asserted invariants:
//   - exactly one CAS upsert succeeds;
//   - N-1 upserts return ErrRevisionConflict;
//   - final revision == initial(1) + 1 successful update == 2;
//   - no unexpected errors.
func TestContention_HubSettingCAS(t *testing.T) {
	cs := newStore(t)
	ctx := context.Background()
	n := concurrency(t)

	section := "cas-race-" + shortID()

	// Seed the row with revision 1.
	val := json.RawMessage(`{"v":0}`)
	_, err := cs.UpsertHubSetting(ctx, section, val, "setup", 0, "")
	require.NoError(t, err)

	var wins, conflicts atomic.Int64
	errs := make(chan error, n)

	// All N goroutines attempt a CAS upsert at expectedRevision=1.
	// The FOR UPDATE lock means only one transaction can hold the row at a
	// time; the winner commits revision 2, and every subsequent reader sees
	// revision 2 which mismatches their expectedRevision=1.
	runConcurrently(n, func(i int) {
		v := json.RawMessage(fmt.Sprintf(`{"v":%d}`, i))
		_, err := cs.UpsertHubSetting(ctx, section, v, fmt.Sprintf("racer-%d", i), 1, "")
		switch {
		case err == nil:
			wins.Add(1)
		case isRevisionConflict(err):
			conflicts.Add(1)
		default:
			errs <- err
		}
	})
	close(errs)
	for err := range errs {
		require.NoError(t, err, "unexpected error during hub-setting CAS contention")
	}

	assert.Equal(t, int64(1), wins.Load(), "exactly one CAS writer must succeed")
	assert.Equal(t, int64(n-1), conflicts.Load(), "all other writers must get ErrRevisionConflict")

	// Final revision must be 2 (initial 1 + one successful update).
	final, err := cs.GetHubSetting(ctx, section)
	require.NoError(t, err)
	assert.Equal(t, int64(2), final.Revision, "final revision must be initial(1) + 1 successful update")
	t.Logf("HubSetting CAS contention: N=%d wins=%d conflicts=%d finalRevision=%d",
		n, wins.Load(), conflicts.Load(), final.Revision)
}

// TestContention_HubSettingCreateOnly races N goroutines to create-only upsert
// (expectedRevision=0) the same section. The unique constraint on `section`
// ensures exactly one creator wins; the rest receive ErrRevisionConflict.
func TestContention_HubSettingCreateOnly(t *testing.T) {
	cs := newStore(t)
	ctx := context.Background()
	n := concurrency(t)

	section := "create-race-" + shortID()
	val := json.RawMessage(`{"created":true}`)

	var wins, conflicts atomic.Int64
	errs := make(chan error, n)

	runConcurrently(n, func(i int) {
		_, err := cs.UpsertHubSetting(ctx, section, val, fmt.Sprintf("creator-%d", i), 0, "")
		switch {
		case err == nil:
			wins.Add(1)
		case isRevisionConflict(err):
			conflicts.Add(1)
		default:
			errs <- err
		}
	})
	close(errs)
	for err := range errs {
		require.NoError(t, err, "unexpected error during hub-setting create-only race")
	}

	assert.Equal(t, int64(1), wins.Load(), "exactly one create-only must succeed")
	assert.Equal(t, int64(n-1), conflicts.Load(), "all other creates must get ErrRevisionConflict")

	got, err := cs.GetHubSetting(ctx, section)
	require.NoError(t, err)
	assert.Equal(t, int64(1), got.Revision)
}

// isRevisionConflict checks for store.ErrRevisionConflict using errors.Is
// (handles wrapped errors).
func isRevisionConflict(err error) bool {
	return errors.Is(err, store.ErrRevisionConflict)
}
