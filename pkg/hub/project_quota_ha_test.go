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

package hub

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

// TestProjectQuota_TwoHubReplicasPostgres_OneAdmission runs two quota
// services, as two hub replicas would, on one Postgres database with one
// project slot left (ptone/scion#6033). Replica A's admission is held inside
// its project-wide count, holding the real advisory lock on a dedicated
// connection; replica B's admission for another creator, issued only then,
// cannot take that lock and gets ErrQuotaLockContention. Once A commits, B's
// retry takes the lock and is refused at the project total, which counts
// every creator: one row exists.
func TestProjectQuota_TwoHubReplicasPostgres_OneAdmission(t *testing.T) {
	requirePG(t)
	ctx := context.Background()
	dsn := enttest.NewSchemaURL(t)
	open := func() *entadapter.CompositeStore {
		client, err := entc.OpenPostgres(dsn, entc.PoolConfig{MaxOpenConns: 5, MaxIdleConns: 2})
		require.NoError(t, err)
		return entadapter.NewCompositeStore(client)
	}
	_, s := testServerWithStore(t, open()) // migrates and seeds the limit definitions
	storeB := open()
	t.Cleanup(func() { _ = storeB.Close() })

	qsA, hookA := newProjectQuotaTestService(t, s, 1)
	qsB := &QuotaService{store: storeB, logger: slog.Default()}
	projectID := tid("proj-pg-race")

	parked := make(chan struct{})
	release := make(chan struct{})
	var parkOnce sync.Once
	hookA.onCount = func() {
		parkOnce.Do(func() {
			close(parked)
			<-release
		})
	}

	type result struct {
		created bool
		err     error
	}
	first := make(chan result, 1)
	go func() {
		c, err := qsA.Reserve(ctx, store.LimitMaxAgentsPerProject, tid("user-a"), store.QuotaScopeProject, projectID, tid("agent-pg-a"))
		first <- result{c, err}
	}()
	select {
	case <-parked:
	case <-time.After(30 * time.Second):
		close(release)
		t.Fatal("replica A's admission did not reach its count")
	}
	require.EqualValues(t, 1, hookA.locksHeld.Load(), "replica A holds the advisory lock inside its count")

	createdB, errB := qsB.Reserve(ctx, store.LimitMaxAgentsPerProject, tid("user-b"), store.QuotaScopeProject, projectID, tid("agent-pg-b"))
	require.ErrorIs(t, errB, ErrQuotaLockContention, "replica B cannot take the lock replica A holds")
	assert.False(t, createdB)

	close(release)
	rA := <-first
	require.NoError(t, rA.err)
	assert.True(t, rA.created, "replica A is admitted")

	// B's retry, once A's lock is released: the project total counts A's
	// row although it is another creator's, so B is refused at the cap.
	createdB, errB = qsB.Reserve(ctx, store.LimitMaxAgentsPerProject, tid("user-b"), store.QuotaScopeProject, projectID, tid("agent-pg-b"))
	var exceeded *QuotaExceededError
	require.True(t, errors.As(errB, &exceeded), "the retry is refused at the project total: %v", errB)
	assert.False(t, createdB)
	assert.EqualValues(t, 1, exceeded.Count)
	assert.EqualValues(t, 1, exceeded.Max)
	assert.EqualValues(t, 1, projectRowCount(t, s, projectID), "exactly one row")
}
