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
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// countingHubInstanceStore is an in-memory store.HubInstanceStore that
// counts calls. missing makes the next Touch report found=false; failNext
// makes the next write fail.
type countingHubInstanceStore struct {
	mu       sync.Mutex
	upserts  int
	touches  int
	rows     map[string]store.HubInstance
	missing  bool
	failNext bool
}

func newCountingHubInstanceStore() *countingHubInstanceStore {
	return &countingHubInstanceStore{rows: map[string]store.HubInstance{}}
}

func (c *countingHubInstanceStore) UpsertHubInstance(_ context.Context, in store.HubInstance) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.upserts++
	if c.failNext {
		c.failNext = false
		return errors.New("write failed")
	}
	c.rows[in.ID] = in
	return nil
}

func (c *countingHubInstanceStore) TouchHubInstance(_ context.Context, id string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.touches++
	if c.failNext {
		c.failNext = false
		return false, errors.New("write failed")
	}
	if c.missing {
		c.missing = false
		delete(c.rows, id)
		return false, nil
	}
	_, ok := c.rows[id]
	return ok, nil
}

func (c *countingHubInstanceStore) ListHubInstances(context.Context, time.Time) ([]store.HubInstance, time.Time, error) {
	return nil, time.Now(), nil
}

func (c *countingHubInstanceStore) counts() (upserts, touches int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.upserts, c.touches
}

// newTestHubInstanceRegistry returns a registry writer over st whose
// snapshot is *snap (so a test can change it between ticks).
func newTestHubInstanceRegistry(st store.HubInstanceStore, snap *hubInstanceSnapshot) *hubInstanceRegistry {
	return &hubInstanceRegistry{
		id:       "hub-test-1",
		store:    st,
		snapshot: func(context.Context) hubInstanceSnapshot { return *snap },
		interval: func() time.Duration { return time.Hour },
		log:      slog.New(slog.DiscardHandler),
	}
}

func quietSnapshot() *hubInstanceSnapshot {
	return &hubInstanceSnapshot{
		Label: "hub-a", Version: "v1", Status: HealthStatusHealthy,
		Checks: map[string]string{"database": "healthy"},
	}
}

// Over 20 quiet ticks (nothing material changes), the writer upserts once
// (the first tick) and touches 19 times.
func TestHubInstanceRegistry_TwentyQuietTicks(t *testing.T) {
	st := newCountingHubInstanceStore()
	reg := newTestHubInstanceRegistry(st, quietSnapshot())
	ctx := context.Background()

	for i := 0; i < 20; i++ {
		reg.tick(ctx)
	}
	upserts, touches := st.counts()
	assert.Equal(t, 1, upserts)
	assert.Equal(t, 19, touches)

	// The 21st tick is the next forced upsert (every 20th tick).
	reg.tick(ctx)
	upserts, touches = st.counts()
	assert.Equal(t, 2, upserts)
	assert.Equal(t, 19, touches)
}

func TestHubInstanceRegistry_MaterialChangeUpserts(t *testing.T) {
	st := newCountingHubInstanceStore()
	snap := quietSnapshot()
	reg := newTestHubInstanceRegistry(st, snap)
	ctx := context.Background()

	reg.tick(ctx) // first tick: upsert
	reg.tick(ctx) // quiet: touch

	snap.Checks = map[string]string{"database": "unhealthy"}
	snap.Status = HealthStatusUnhealthy
	reg.tick(ctx) // changed: upsert
	reg.tick(ctx) // quiet again: touch

	snap.Version = "v2"
	reg.tick(ctx) // changed: upsert

	upserts, touches := st.counts()
	assert.Equal(t, 3, upserts)
	assert.Equal(t, 2, touches)
	assert.Equal(t, "v2", st.rows["hub-test-1"].Version)
	assert.Equal(t, HealthStatusUnhealthy, st.rows["hub-test-1"].Status)
}

// A Touch that finds no row (pruned, or never written) upserts in the same
// tick.
func TestHubInstanceRegistry_TouchNotFoundUpserts(t *testing.T) {
	st := newCountingHubInstanceStore()
	reg := newTestHubInstanceRegistry(st, quietSnapshot())
	ctx := context.Background()

	reg.tick(ctx)
	st.missing = true
	reg.tick(ctx)

	upserts, touches := st.counts()
	assert.Equal(t, 2, upserts)
	assert.Equal(t, 1, touches)
	assert.Contains(t, st.rows, "hub-test-1", "the row is written again")
}

// After a failed write the next tick upserts, even when nothing changed.
func TestHubInstanceRegistry_FailedWriteUpsertsNext(t *testing.T) {
	st := newCountingHubInstanceStore()
	reg := newTestHubInstanceRegistry(st, quietSnapshot())
	ctx := context.Background()

	reg.tick(ctx) // upsert
	st.failNext = true
	reg.tick(ctx) // touch fails
	reg.tick(ctx) // upsert (last write failed)
	reg.tick(ctx) // touch

	upserts, touches := st.counts()
	assert.Equal(t, 2, upserts)
	assert.Equal(t, 2, touches)
}

func TestJitteredHubInstanceInterval_WithinTenPercent(t *testing.T) {
	lo := hubInstanceTickInterval - hubInstanceTickInterval/10
	hi := hubInstanceTickInterval + hubInstanceTickInterval/10
	for i := 0; i < 1000; i++ {
		d := jitteredHubInstanceInterval()
		require.GreaterOrEqual(t, d, lo)
		require.LessOrEqual(t, d, hi)
	}
}

// tickCountingStore wraps a real store and counts the agent, project and
// runtime broker list calls that GetHealthInfo's stats make.
type tickCountingStore struct {
	store.Store
	mu      sync.Mutex
	agents  int
	project int
	brokers int
}

func (c *tickCountingStore) ListAgents(ctx context.Context, f store.AgentFilter, o store.ListOptions) (*store.ListResult[store.Agent], error) {
	c.mu.Lock()
	c.agents++
	c.mu.Unlock()
	return c.Store.ListAgents(ctx, f, o)
}

func (c *tickCountingStore) ListProjects(ctx context.Context, f store.ProjectFilter, o store.ListOptions) (*store.ListResult[store.Project], error) {
	c.mu.Lock()
	c.project++
	c.mu.Unlock()
	return c.Store.ListProjects(ctx, f, o)
}

func (c *tickCountingStore) ListRuntimeBrokers(ctx context.Context, f store.RuntimeBrokerFilter, o store.ListOptions) (*store.ListResult[store.RuntimeBroker], error) {
	c.mu.Lock()
	c.brokers++
	c.mu.Unlock()
	return c.Store.ListRuntimeBrokers(ctx, f, o)
}

// The registry tick runs the health checks only: no ListAgents,
// ListProjects or runtime broker count. It writes this server's row with
// its instance ID, version and normalised checks.
func TestHubInstanceRegistry_TickRunsNoCountQueries(t *testing.T) {
	srv, s := testServer(t)
	counting := &tickCountingStore{Store: s}
	srv.store = counting
	ctx := context.Background()

	reg := srv.newHubInstanceRegistry()
	for i := 0; i < 3; i++ {
		reg.tick(ctx)
	}

	counting.mu.Lock()
	assert.Zero(t, counting.agents, "tick must not call ListAgents")
	assert.Zero(t, counting.project, "tick must not call ListProjects")
	assert.Zero(t, counting.brokers, "tick must not list runtime brokers")
	counting.mu.Unlock()

	rows, _, err := s.ListHubInstances(ctx, time.Time{})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, srv.InstanceID(), rows[0].ID)
	assert.NotEmpty(t, rows[0].Version)
	assert.Equal(t, "healthy", rows[0].Checks["database"])
	assert.Equal(t, deriveHealthStatus(srv.healthChecks(ctx)), rows[0].Status)
}

// The stored status comes from the raw checks; the stored checks are
// normalised to fixed values, and an existing long check name
// (workspace_storage_mount_verification, 36 characters) is kept.
func TestHubInstanceSnapshot_StatusFromRawChecksAndNormalisedChecks(t *testing.T) {
	raw := map[string]string{
		"database":                             "healthy",
		"workspace_storage":                    "healthy",
		"workspace_storage_mount_verification": "unavailable: could not compare filesystem device IDs",
		strings.Repeat("x", 65):                "unhealthy: detail",
	}
	snap := hubInstanceSnapshotFromChecks("hub-a", "v1", raw)
	// The 65-character key is dropped from the stored checks, but it is a
	// non-critical non-healthy check, so the status is degraded.
	assert.Equal(t, HealthStatusDegraded, snap.Status)
	assert.Equal(t, map[string]string{
		"database":                             "healthy",
		"workspace_storage":                    "healthy",
		"workspace_storage_mount_verification": "unavailable",
	}, snap.Checks)
}

func TestHubInstanceLabel(t *testing.T) {
	t.Run("pod name", func(t *testing.T) {
		t.Setenv("POD_NAME", "scion-hub-7d9f")
		t.Setenv("K_REVISION", "")
		assert.Equal(t, "scion-hub-7d9f", hubInstanceLabel("scion-hub-7d9f-1234"))
	})
	t.Run("cloud run revision", func(t *testing.T) {
		t.Setenv("POD_NAME", "")
		t.Setenv("K_REVISION", "scion-hub-00042-abc")
		assert.Equal(t, "scion-hub-00042-abc/0123abcd", hubInstanceLabel("0123abcd-ef01-2345-6789-abcdef012345"))
	})
	t.Run("host name", func(t *testing.T) {
		t.Setenv("POD_NAME", "")
		t.Setenv("K_REVISION", "")
		assert.NotEmpty(t, hubInstanceLabel("0123abcd"))
	})
	t.Run("bounded printable", func(t *testing.T) {
		t.Setenv("POD_NAME", "pod\x01\x7fname"+strings.Repeat("p", 100))
		got := hubInstanceLabel("x")
		assert.Len(t, got, hubInstanceMaxLabelBytes)
		assert.True(t, strings.HasPrefix(got, "podname"))
	})
}

// startHubInstanceRegistry returns after the first tick, so this replica's
// row exists before the listener serves.
func TestStartHubInstanceRegistry_FirstTickBeforeReturn(t *testing.T) {
	srv, s := testServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv.startHubInstanceRegistry(ctx)

	rows, _, err := s.ListHubInstances(context.Background(), time.Time{})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, srv.InstanceID(), rows[0].ID)
}
