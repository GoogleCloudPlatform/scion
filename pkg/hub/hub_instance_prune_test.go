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

package hub

import (
	"context"
	"database/sql"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// sharedAdvisoryLock stands for one Postgres advisory lock that several
// replicas contend on: the first TryAdvisoryLock for a key wins until its
// release func runs.
type sharedAdvisoryLock struct {
	mu   sync.Mutex
	held map[store.AdvisoryLockKey]bool
	keys []store.AdvisoryLockKey
}

func (l *sharedAdvisoryLock) try(key store.AdvisoryLockKey) (bool, func() error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.keys = append(l.keys, key)
	if l.held[key] {
		return false, func() error { return nil }
	}
	l.held[key] = true
	return true, func() error {
		l.mu.Lock()
		defer l.mu.Unlock()
		delete(l.held, key)
		return nil
	}
}

// pruneReplicaStore is one replica's store: it takes advisory locks from
// the shared lock and counts PruneHubInstances calls. block, when set,
// holds a prune until it is closed. Every other store method is unused
// (the embedded nil store panics if called).
type pruneReplicaStore struct {
	store.Store
	lock      *sharedAdvisoryLock
	prunes    *atomic.Int32
	retention time.Duration
	entered   chan struct{}
	block     chan struct{}
}

func (p *pruneReplicaStore) TryAdvisoryLock(_ context.Context, key store.AdvisoryLockKey) (bool, func() error, error) {
	ok, release := p.lock.try(key)
	return ok, release, nil
}

func (p *pruneReplicaStore) TryAdvisoryLockObject(context.Context, store.AdvisoryLockKey, int32) (bool, func() error, error) {
	return false, func() error { return nil }, nil
}

func (p *pruneReplicaStore) PruneHubInstances(_ context.Context, retention time.Duration) (int, error) {
	p.prunes.Add(1)
	p.retention = retention
	if p.entered != nil {
		p.entered <- struct{}{}
	}
	if p.block != nil {
		<-p.block
	}
	return 0, nil
}

// pruneJob registers the prune on a fresh scheduler for st, the way
// registerSchedulerHandlers does, and returns the registered handler.
func pruneJob(t *testing.T, st store.Store) RecurringHandler {
	t.Helper()
	srv := &Server{store: st}
	sched := NewScheduler(st, slog.New(slog.DiscardHandler))
	srv.registerHubInstancePrune(sched)
	require.Len(t, sched.recurring, 1)
	return sched.recurring[0]
}

// Two replicas fire the same prune tick: only the one holding
// LockHubInstancePrune prunes. On the next tick, once the lock is
// released, either replica may run it again.
func TestHubInstancePrune_OneReplicaPerTick(t *testing.T) {
	lock := &sharedAdvisoryLock{held: map[store.AdvisoryLockKey]bool{}}
	var prunes atomic.Int32
	a := &pruneReplicaStore{lock: lock, prunes: &prunes, entered: make(chan struct{}, 1), block: make(chan struct{})}
	b := &pruneReplicaStore{lock: lock, prunes: &prunes}
	jobA, jobB := pruneJob(t, a), pruneJob(t, b)
	ctx := context.Background()

	aDone := make(chan struct{})
	go func() {
		defer close(aDone)
		jobA.Fn(ctx)
	}()
	<-a.entered // replica A holds the lock and is pruning

	jobB.Fn(ctx) // the same tick on replica B
	assert.Equal(t, int32(1), prunes.Load(), "only one replica prunes per tick")

	close(a.block)
	<-aDone

	jobB.Fn(ctx) // next tick: the lock is free again
	assert.Equal(t, int32(2), prunes.Load())
	for _, k := range lock.keys {
		assert.Equal(t, store.LockHubInstancePrune, k)
	}
	assert.Equal(t, hubInstanceRetention, b.retention, "prune uses the 24 h retention")
}

// The prune is registered as an hourly singleton job.
func TestRegisterSchedulerHandlers_RegistersHubInstancePrune(t *testing.T) {
	srv, _ := testServer(t)
	srv.scheduler = NewScheduler(srv.store, slog.Default())
	srv.registerSchedulerHandlers()

	var found *RecurringHandler
	for i := range srv.scheduler.recurring {
		if srv.scheduler.recurring[i].Name == hubInstancePruneJobName {
			found = &srv.scheduler.recurring[i]
		}
	}
	require.NotNil(t, found, "hub-instance-prune is registered")
	assert.Equal(t, 60, found.Interval)
	assert.True(t, found.Singleton)
}

// The handler deletes only the rows past the 24 h retention, on a real
// store (a second raw connection ages the rows).
func TestHubInstancePruneHandler_DeletesOnlyOldRows(t *testing.T) {
	dsn := "file:hubprune_" + strings.NewReplacer("/", "_", " ", "_").Replace(t.Name()) + "?mode=memory&cache=shared"
	st, err := newTestStoreAt(t, dsn)
	require.NoError(t, err)
	srv, s := testServerWithStore(t, st)
	db, err := sql.Open("sqlite", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()

	for _, id := range []string{"hub-old", "hub-stopped-old", "hub-recent", "hub-stopped-recent"} {
		require.NoError(t, s.UpsertHubInstance(ctx, store.HubInstance{ID: id, Label: id, Status: HealthStatusHealthy}))
	}
	old := time.Now().UTC().Add(-25 * time.Hour)
	recent := time.Now().UTC().Add(-23 * time.Hour)
	_, err = db.ExecContext(ctx, "UPDATE hub_instances SET last_seen = ? WHERE id = ?", old, "hub-old")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, "UPDATE hub_instances SET last_seen = ?, stopped_at = ? WHERE id = ?", old, old, "hub-stopped-old")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, "UPDATE hub_instances SET last_seen = ?, stopped_at = ? WHERE id = ?", old, recent, "hub-stopped-recent")
	require.NoError(t, err)

	srv.hubInstancePruneHandler()(ctx)

	rows, _, err := s.ListHubInstances(ctx, 48*time.Hour)
	require.NoError(t, err)
	var ids []string
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	assert.Equal(t, []string{"hub-recent", "hub-stopped-recent"}, ids)
}
