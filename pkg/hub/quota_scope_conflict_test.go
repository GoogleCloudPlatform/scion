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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
)

// Reservation scope tests (max_agents_per_broker): an agent's active
// reservation on one Runtime Broker never satisfies a request on another.

const scopeTestLimit = "max_test_broker_scope"

// scopeConflictCounter wires a scope conflict counter on qs and returns a
// reader of its count for a site.
func scopeConflictCounter(t *testing.T, qs *QuotaService) func(site string) int64 {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	rec, err := NewOTelQuotaScopeConflictMetrics(mp)
	require.NoError(t, err)
	qs.scopeConflictMetrics.Store(rec)
	return func(site string) int64 {
		var rm metricdata.ResourceMetrics
		require.NoError(t, reader.Collect(context.Background(), &rm))
		var total int64
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if m.Name != "scion.hub.quota.reservation_scope_conflict" {
					continue
				}
				sum, ok := m.Data.(metricdata.Sum[int64])
				require.True(t, ok, "counter data %T", m.Data)
				for _, dp := range sum.DataPoints {
					if v, ok := dp.Attributes.Value("site"); ok && v.AsString() == site {
						total += dp.Value
					}
				}
			}
		}
		return total
	}
}

// activeReservation returns resourceID's active reservation for limitDefID,
// or nil when it holds none.
func activeReservation(t *testing.T, s store.Store, limitDefID, resourceID string) *store.UsageReservation {
	t.Helper()
	r, err := s.GetActiveReservation(context.Background(), limitDefID, resourceID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	require.NoError(t, err)
	return r
}

// seedBrokerRow writes an active broker-scoped reservation of resourceID on
// brokerID directly, as an earlier admission would have, and returns it.
func seedBrokerRow(t *testing.T, s store.Store, limitDefID, brokerID, resourceID string, createdAt time.Time) *store.UsageReservation {
	t.Helper()
	r, err := s.CreateUsageReservation(context.Background(), &store.UsageReservation{
		LimitDefinitionID: limitDefID,
		SubjectID:         brokerID,
		ScopeType:         store.QuotaScopeBroker,
		ScopeID:           brokerID,
		ResourceID:        resourceID,
		Reserved:          1,
		CreatedAt:         createdAt,
	})
	require.NoError(t, err)
	return r
}

// reserveOn calls qs.Reserve for resourceID on brokerID.
func reserveOn(qs *QuotaService, limitName, brokerID, resourceID string) (bool, error) {
	return qs.Reserve(context.Background(), limitName, brokerID, store.QuotaScopeBroker, brokerID, resourceID)
}

// requireScopeConflict asserts err is a ReservationScopeConflictError naming
// held and requested.
func requireScopeConflict(t *testing.T, err error, resourceID, held, requested string) {
	t.Helper()
	var conflict *ReservationScopeConflictError
	require.True(t, errors.As(err, &conflict), "want a ReservationScopeConflictError, got %v", err)
	assert.ErrorIs(t, err, ErrReservationScopeConflict)
	assert.Equal(t, resourceID, conflict.ResourceID)
	assert.Equal(t, held, conflict.HeldScopeID)
	assert.Equal(t, requested, conflict.RequestedScopeID)
}

func TestReserve_SameScopeRetryIdempotent(t *testing.T) {
	qs, s := newTestQuotaService(t)
	def := seedLimit(t, s, scopeTestLimit, 1)
	conflicts := scopeConflictCounter(t, qs)

	created, err := reserveOn(qs, scopeTestLimit, "broker-a", "agent-1")
	require.NoError(t, err)
	require.True(t, created)
	first := activeReservation(t, s, def.ID, "agent-1")
	require.NotNil(t, first)

	// The broker is now at its cap of 1; a retry in the same scope is still
	// a success that creates nothing.
	for i := 0; i < 3; i++ {
		created, err = reserveOn(qs, scopeTestLimit, "broker-a", "agent-1")
		require.NoError(t, err, "retry #%d", i+1)
		assert.False(t, created, "retry #%d creates nothing", i+1)
	}
	got := activeReservation(t, s, def.ID, "agent-1")
	require.NotNil(t, got)
	assert.Equal(t, first.ID, got.ID, "the original reservation is kept")
	n, err := s.CountActiveReservations(context.Background(), def.ID, "broker-a", store.QuotaScopeBroker, "broker-a")
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)
	assert.Zero(t, conflicts(scopeConflictSiteReserve))
}

func TestReserve_AtoBConflict_AUnchanged(t *testing.T) {
	qs, s := newTestQuotaService(t)
	def := seedLimit(t, s, scopeTestLimit, 5)
	conflicts := scopeConflictCounter(t, qs)

	created, err := reserveOn(qs, scopeTestLimit, "broker-a", "agent-1")
	require.NoError(t, err)
	require.True(t, created)
	before := activeReservation(t, s, def.ID, "agent-1")
	require.NotNil(t, before)

	created, err = reserveOn(qs, scopeTestLimit, "broker-b", "agent-1")
	requireScopeConflict(t, err, "agent-1", "broker-a", "broker-b")
	assert.False(t, created)

	after := activeReservation(t, s, def.ID, "agent-1")
	require.NotNil(t, after, "A's reservation is kept")
	assert.Equal(t, before.ID, after.ID)
	assert.Equal(t, "broker-a", after.ScopeID)
	n, err := s.CountActiveReservations(context.Background(), def.ID, "broker-b", store.QuotaScopeBroker, "broker-b")
	require.NoError(t, err)
	assert.Zero(t, n, "no reservation is made on B")
	assert.EqualValues(t, 1, conflicts(scopeConflictSiteReserve))

	// The conflict is answered before B's scope is locked: with B's
	// advisory lock held elsewhere it is still a conflict, not contention.
	locker := s.(store.AdvisoryLocker)
	acquired, release, err := locker.TryAdvisoryLockObject(context.Background(), store.LockQuotaEnforcement, store.StableProjectHash("broker-b"))
	require.NoError(t, err)
	require.True(t, acquired)
	defer func() { _ = release() }()
	_, err = reserveOn(qs, scopeTestLimit, "broker-b", "agent-1")
	requireScopeConflict(t, err, "agent-1", "broker-a", "broker-b")
	assert.EqualValues(t, 2, conflicts(scopeConflictSiteReserve))
}

// insertBarrierStore holds every armed CreateUsageReservation until all the
// armed callers have arrived, so they insert together, after each passed
// Reserve's checks. Its advisory locks always succeed, as on SQLite, so two
// QuotaService instances on it serialize nothing but through the store.
type insertBarrierStore struct {
	store.Store
	mu      sync.Mutex
	waiting int
	release chan struct{}
}

func (b *insertBarrierStore) TryAdvisoryLock(context.Context, store.AdvisoryLockKey) (bool, func() error, error) {
	return true, func() error { return nil }, nil
}

func (b *insertBarrierStore) TryAdvisoryLockObject(context.Context, store.AdvisoryLockKey, int32) (bool, func() error, error) {
	return true, func() error { return nil }, nil
}

// arm makes the next n inserts wait for each other.
func (b *insertBarrierStore) arm(n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.waiting = n
	b.release = make(chan struct{})
}

func (b *insertBarrierStore) CreateUsageReservation(ctx context.Context, r *store.UsageReservation) (*store.UsageReservation, error) {
	b.mu.Lock()
	release := b.release
	if b.waiting > 0 {
		b.waiting--
		if b.waiting == 0 {
			close(b.release)
			b.release = nil
		}
	} else {
		release = nil
	}
	b.mu.Unlock()
	if release != nil {
		select {
		case <-release:
		case <-time.After(10 * time.Second):
			return nil, errors.New("insert barrier: the other writer never arrived")
		}
	}
	return b.Store.CreateUsageReservation(ctx, r)
}

// TestReserve_SimultaneousAandB_OneWinsOtherConflicts runs two QuotaService
// instances, as two Hub replicas, on one store. Their in-process scope locks
// differ, and A's and B's requests take different scope locks anyway, so
// only the store's unique active index decides. Both pass Reserve's checks
// before either inserts (insertBarrierStore); the loser must read back the
// winner and classify it.
func TestReserve_SimultaneousAandB_OneWinsOtherConflicts(t *testing.T) {
	base, err := newTestStore(t, ":memory:")
	require.NoError(t, err)
	bs := &insertBarrierStore{Store: base}
	def := seedLimit(t, bs, scopeTestLimit, 1000) // above the 40 rows the iterations leave active
	replica1 := &QuotaService{store: bs, logger: slog.Default()}
	replica2 := &QuotaService{store: bs, logger: slog.Default()}

	type result struct {
		broker  string
		created bool
		err     error
	}
	race := func(t *testing.T, resource, broker1, broker2 string) [2]result {
		bs.arm(2)
		var out [2]result
		var wg sync.WaitGroup
		for i, rq := range []struct {
			qs     *QuotaService
			broker string
		}{{replica1, broker1}, {replica2, broker2}} {
			wg.Add(1)
			go func(i int, qs *QuotaService, broker string) {
				defer wg.Done()
				created, err := reserveOn(qs, scopeTestLimit, broker, resource)
				out[i] = result{broker: broker, created: created, err: err}
			}(i, rq.qs, rq.broker)
		}
		wg.Wait()
		return out
	}

	const iterations = 20
	t.Run("A and B", func(t *testing.T) {
		for i := 0; i < iterations; i++ {
			resource := fmt.Sprintf("agent-ab-%d", i)
			out := race(t, resource, "broker-a", "broker-b")
			var winner, loser *result
			for j := range out {
				if out[j].err == nil && out[j].created {
					winner = &out[j]
				} else {
					loser = &out[j]
				}
			}
			require.NotNil(t, winner, "iteration %d: one request wins: %+v", i, out)
			require.NotNil(t, loser, "iteration %d: one request loses: %+v", i, out)
			assert.False(t, loser.created)
			requireScopeConflict(t, loser.err, resource, winner.broker, loser.broker)
			held := activeReservation(t, bs, def.ID, resource)
			require.NotNil(t, held)
			assert.Equal(t, winner.broker, held.ScopeID, "iteration %d: the winner's row is the only active one", i)
		}
	})
	t.Run("same scope", func(t *testing.T) {
		for i := 0; i < iterations; i++ {
			resource := fmt.Sprintf("agent-aa-%d", i)
			out := race(t, resource, "broker-a", "broker-a")
			require.NoError(t, out[0].err, "iteration %d", i)
			require.NoError(t, out[1].err, "iteration %d", i)
			assert.True(t, out[0].created != out[1].created, "iteration %d: exactly one creates: %+v", i, out)
			held := activeReservation(t, bs, def.ID, resource)
			require.NotNil(t, held)
			assert.Equal(t, "broker-a", held.ScopeID)
		}
	})
}

func TestReserve_EnforcementDisabled_ConflictStillReported(t *testing.T) {
	qs, s := newTestQuotaService(t)
	qs.enforced = func(string) bool { return false }
	def := seedLimit(t, s, scopeTestLimit, 1)

	created, err := reserveOn(qs, scopeTestLimit, "broker-a", "agent-1")
	require.NoError(t, err)
	require.True(t, created)

	// B is over its cap too (enforcement off would let that through): the
	// scope conflict is still refused.
	seedBrokerRow(t, s, def.ID, "broker-b", "filler", time.Time{})
	_, err = reserveOn(qs, scopeTestLimit, "broker-b", "agent-1")
	requireScopeConflict(t, err, "agent-1", "broker-a", "broker-b")

	// Lock contention on B's scope with enforcement off proceeds without a
	// reservation only when no conflicting row exists.
	locker := s.(store.AdvisoryLocker)
	acquired, release, err := locker.TryAdvisoryLockObject(context.Background(), store.LockQuotaEnforcement, store.StableProjectHash("broker-b"))
	require.NoError(t, err)
	require.True(t, acquired)
	defer func() { _ = release() }()
	_, err = reserveOn(qs, scopeTestLimit, "broker-b", "agent-1")
	requireScopeConflict(t, err, "agent-1", "broker-a", "broker-b")

	created, err = reserveOn(qs, scopeTestLimit, "broker-b", "agent-2")
	require.NoError(t, err, "control: no conflicting row, contention with enforcement off proceeds")
	assert.False(t, created)

	held := activeReservation(t, s, def.ID, "agent-1")
	require.NotNil(t, held)
	assert.Equal(t, "broker-a", held.ScopeID)
}

func TestReserve_Unlimited_ConflictStillReported(t *testing.T) {
	qs, s := newTestQuotaService(t)
	def := seedLimit(t, s, scopeTestLimit, 0) // 0 = unlimited
	seedBrokerRow(t, s, def.ID, "broker-a", "agent-1", time.Time{})

	created, err := reserveOn(qs, scopeTestLimit, "broker-b", "agent-1")
	requireScopeConflict(t, err, "agent-1", "broker-a", "broker-b")
	assert.False(t, created)

	created, err = reserveOn(qs, scopeTestLimit, "broker-a", "agent-1")
	require.NoError(t, err, "a same-scope retry is still a success")
	assert.False(t, created)

	created, err = reserveOn(qs, scopeTestLimit, "broker-b", "agent-2")
	require.NoError(t, err, "control: unlimited still admits without a reservation")
	assert.False(t, created)
	assert.Nil(t, activeReservation(t, s, def.ID, "agent-2"))
}

// TestReserve_FailedRetryDoesNotReleaseExisting drives the action boundary:
// a start of an agent on Runtime Broker B whose reservation is on A is
// refused with 409 broker_reservation_scope_conflict before anything is
// dispatched or written, and the rollback (created=false) releases nothing.
func TestReserve_FailedRetryDoesNotReleaseExisting(t *testing.T) {
	srv, s := testServer(t)
	disp := &quotaLifecycleDispatcher{}
	srv.SetDispatcher(disp)
	setBrokerAgentCeiling(t, s, 5)
	brokerA, _ := newQuotaTestBrokerAndProject(t, s, "scope-a")
	brokerB, projectB := newQuotaTestBrokerAndProject(t, s, "scope-b")
	agent := newQuotaTestAgent(t, s, brokerB, projectB, "scope-retry", state.PhaseStopped)
	reserveBrokerSlot(t, s, brokerA, agent.ID)
	def, err := s.GetLimitDefinitionByName(context.Background(), store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)
	before := activeReservation(t, s, def.ID, agent.ID)
	require.NotNil(t, before)

	for i := 0; i < 2; i++ {
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", nil)
		require.Equal(t, http.StatusConflict, rec.Code, "start #%d: %s", i+1, rec.Body.String())
		var resp ErrorResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), rec.Body.String())
		assert.Equal(t, ErrCodeBrokerReservationScopeConflict, resp.Error.Code)
		assert.Equal(t, fmt.Sprintf("agent %s already holds capacity on Runtime Broker %s; it cannot be admitted on %s",
			agent.ID, brokerA.ID, brokerB.ID), resp.Error.Message)
	}

	after := activeReservation(t, s, def.ID, agent.ID)
	require.NotNil(t, after, "A's reservation survives the refused starts")
	assert.Equal(t, before.ID, after.ID)
	assert.Equal(t, brokerA.ID, after.ScopeID)
	assert.Zero(t, brokerReservationCount(t, s, brokerB.ID), "no reservation on B")
	assert.Zero(t, disp.startCount.Load(), "nothing was dispatched")
	got, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseStopped), got.Phase, "no starting write")

	// A store failure is still a 500, not a 409.
	rec := httptest.NewRecorder()
	writeQuotaReserveError(rec, store.LimitMaxAgentsPerBroker, errors.New("database is gone"))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// newQuotaTestFlatBrokerAndProject is newQuotaTestBrokerAndProject for a flat
// Runtime Broker: the broker row serves one runtime target, so an agent
// pinned to it (pinnedQuotaAgent) has a valid placement.
func newQuotaTestFlatBrokerAndProject(t *testing.T, s store.Store, suffix string) (*store.RuntimeBroker, *store.Project) {
	t.Helper()
	ctx := context.Background()
	broker := &store.RuntimeBroker{
		ID:            tid("broker-quota-flat-" + suffix),
		Name:          "Quota Flat Broker " + suffix,
		Slug:          "quota-flat-broker-" + suffix,
		Status:        store.BrokerStatusOnline,
		RuntimeTarget: &api.RuntimeTargetDescriptor{ID: tid("target-quota-" + suffix), Type: "docker", DisplayName: "Local Docker"},
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	project := &store.Project{
		ID:   tid("proj-quota-flat-" + suffix),
		Name: "Quota Flat Project " + suffix,
		Slug: "quota-flat-project-" + suffix,
	}
	require.NoError(t, s.CreateProject(ctx, project))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     broker.Status,
	}))
	project.DefaultRuntimeBrokerID = broker.ID
	require.NoError(t, s.UpdateProject(ctx, project))
	got, err := s.GetRuntimeBroker(ctx, broker.ID)
	require.NoError(t, err)
	require.True(t, got.IsFlat())
	return got, project
}

// pinnedQuotaAgent creates an agent on the flat broker, pinned to the
// broker's runtime target (a valid placement), in phase.
func pinnedQuotaAgent(t *testing.T, s store.Store, broker *store.RuntimeBroker, project *store.Project, name string, phase state.Phase) *store.Agent {
	t.Helper()
	return pinnedQuotaAgentWith(t, s, broker, project, name, phase, func(*store.Agent) {})
}

// pinnedQuotaAgentWith is pinnedQuotaAgent with mutate applied before the
// store write.
func pinnedQuotaAgentWith(t *testing.T, s store.Store, broker *store.RuntimeBroker, project *store.Project, name string, phase state.Phase, mutate func(*store.Agent)) *store.Agent {
	t.Helper()
	require.True(t, broker.IsFlat(), "pinned agents need a flat Runtime Broker")
	a := &store.Agent{
		ID:                      tid("agent-" + name),
		Slug:                    name,
		Name:                    name,
		ProjectID:               project.ID,
		RuntimeBrokerID:         broker.ID,
		Phase:                   string(phase),
		PinnedRuntimeBrokerID:   broker.ID,
		PinnedRuntimeTargetID:   broker.RuntimeTarget.ID,
		PinnedRuntimeTargetType: broker.RuntimeTarget.Type,
	}
	mutate(a)
	require.NoError(t, s.CreateAgent(context.Background(), a))
	got, err := s.GetAgent(context.Background(), a.ID)
	require.NoError(t, err)
	require.True(t, got.IsPinned())
	return got
}

func TestReassert_DifferentScopeNotOverwritten(t *testing.T) {
	srv, s := testServer(t)
	conflicts := scopeConflictCounter(t, srv.quotaService)
	brokerA, _ := newQuotaTestBrokerAndProject(t, s, "reassert-a")
	brokerB, projectB := newQuotaTestFlatBrokerAndProject(t, s, "reassert-b")
	def, err := s.GetLimitDefinitionByName(context.Background(), store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)

	agent := pinnedQuotaAgent(t, s, brokerB, projectB, "reassert-x", state.PhaseRunning)
	before := seedBrokerRow(t, s, def.ID, brokerA.ID, agent.ID, time.Time{})

	assert.False(t, srv.reassertBrokerReservation(context.Background(), agent))
	after := activeReservation(t, s, def.ID, agent.ID)
	require.NotNil(t, after)
	assert.Equal(t, before.ID, after.ID, "the row on A is not overwritten or moved")
	assert.Equal(t, brokerA.ID, after.ScopeID)
	assert.Zero(t, brokerReservationCount(t, s, brokerB.ID))
	assert.EqualValues(t, 1, conflicts(scopeConflictSiteReassert))

	// Controls: a row on the agent's own broker is left alone and not
	// reported; no row at all is re-asserted.
	same := pinnedQuotaAgent(t, s, brokerB, projectB, "reassert-same", state.PhaseRunning)
	reserveBrokerSlot(t, s, brokerB, same.ID)
	assert.False(t, srv.reassertBrokerReservation(context.Background(), same))
	none := pinnedQuotaAgent(t, s, brokerB, projectB, "reassert-none", state.PhaseRunning)
	assert.True(t, srv.reassertBrokerReservation(context.Background(), none))
	assert.EqualValues(t, 1, conflicts(scopeConflictSiteReassert))
}

// TestSweep_PinnedScopeConflictReportedNotMoved: a pinned agent that still
// holds capacity (a counted phase, or a stop queued for an offline broker)
// keeps its row on another broker; the sweep reports it and neither
// releases, moves nor backfills it (contract erratum E-FC11-1).
func TestSweep_PinnedScopeConflictReportedNotMoved(t *testing.T) {
	srv, s := testServer(t)
	conflicts := scopeConflictCounter(t, srv.quotaService)
	// brokerD is registered first and kept out of the sweep's list (the
	// list holds the newest brokers first and is capped below), so a row
	// on it is unlisted but live: the deregistered-scope pass leaves it.
	brokerD, _ := newQuotaTestBrokerAndProject(t, s, "sweep-d")
	time.Sleep(5 * time.Millisecond)
	brokerA, _ := newQuotaTestBrokerAndProject(t, s, "sweep-a")
	time.Sleep(5 * time.Millisecond)
	brokerB, projectB := newQuotaTestFlatBrokerAndProject(t, s, "sweep-b")
	time.Sleep(5 * time.Millisecond)
	brokerC, projectC := newQuotaTestBrokerAndProject(t, s, "sweep-c")
	all, err := s.ListRuntimeBrokers(context.Background(), store.RuntimeBrokerFilter{}, store.ListOptions{})
	require.NoError(t, err)
	oldLimit := reconcileBrokerListLimit
	reconcileBrokerListLimit = len(all.Items) - 1 // every broker but the oldest, brokerD
	t.Cleanup(func() { reconcileBrokerListLimit = oldLimit })
	def, err := s.GetLimitDefinitionByName(context.Background(), store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)
	old := time.Now().Add(-2 * reconcileMinReservationAge)

	running := pinnedQuotaAgent(t, s, brokerB, projectB, "sweep-running", state.PhaseRunning)
	runningRow := seedBrokerRow(t, s, def.ID, brokerA.ID, running.ID, old)
	queued := pinnedQuotaAgentWith(t, s, brokerB, projectB, "sweep-stop-queued", state.PhaseStopped, func(a *store.Agent) {
		a.ContainerStatus = containerStatusStopQueued
	})
	require.True(t, agentHoldsBrokerCapacity(queued), "a queued stop holds capacity")
	queuedRow := seedBrokerRow(t, s, def.ID, brokerA.ID, queued.ID, old)
	// A pinned agent whose row is on a registered broker that is not listed
	// (beyond the list cap): the backfill on B reports it and writes
	// nothing. (A row on a deleted broker is released instead; see
	// TestSweep_DeregisteredBrokerScope_ReleasesReservation_Audited.)
	orphaned := pinnedQuotaAgent(t, s, brokerB, projectB, "sweep-orphaned", state.PhaseRunning)
	orphanedRow := seedBrokerRow(t, s, def.ID, brokerD.ID, orphaned.ID, old)
	// Control: an unpinned agent keeps today's rule (an old row of a stopped
	// agent is released wherever it is).
	unpinned := newQuotaTestAgent(t, s, brokerC, projectC, "sweep-unpinned", state.PhaseStopped)
	seedBrokerRow(t, s, def.ID, brokerA.ID, unpinned.ID, old)

	srv.ReconcileStaleBrokerQuotaReservations(context.Background())

	for _, tc := range []struct {
		agent *store.Agent
		row   *store.UsageReservation
	}{{running, runningRow}, {queued, queuedRow}, {orphaned, orphanedRow}} {
		got := activeReservation(t, s, def.ID, tc.agent.ID)
		require.NotNil(t, got, "%s: the conflicting row is not released", tc.agent.Slug)
		assert.Equal(t, tc.row.ID, got.ID, "%s: not moved or replaced", tc.agent.Slug)
		assert.Equal(t, tc.row.ScopeID, got.ScopeID, "%s", tc.agent.Slug)
	}
	assert.Nil(t, activeReservation(t, s, def.ID, unpinned.ID), "control: the unpinned agent's stale row is released")
	assert.Zero(t, brokerReservationCount(t, s, brokerB.ID), "nothing is backfilled on B")
	assert.EqualValues(t, 2, conflicts(scopeConflictSiteSweep), "running and stop-queued, on listed broker A")
	assert.EqualValues(t, 1, conflicts(scopeConflictSiteBackfill), "orphaned, on an unlisted broker")
}

func TestUsage_ScopeConflictFlag(t *testing.T) {
	srv, s := testServer(t)
	brokerA, projectA := newQuotaTestFlatBrokerAndProject(t, s, "usage-a")
	brokerB, projectB := newQuotaTestFlatBrokerAndProject(t, s, "usage-b")
	brokerC, projectC := newQuotaTestBrokerAndProject(t, s, "usage-c")
	def, err := s.GetLimitDefinitionByName(context.Background(), store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)

	conflicted := pinnedQuotaAgent(t, s, brokerB, projectB, "usage-conflicted", state.PhaseRunning)
	seedBrokerRow(t, s, def.ID, brokerA.ID, conflicted.ID, time.Time{})
	aligned := pinnedQuotaAgent(t, s, brokerA, projectA, "usage-aligned", state.PhaseRunning)
	seedBrokerRow(t, s, def.ID, brokerA.ID, aligned.ID, time.Time{})
	unpinned := newQuotaTestAgent(t, s, brokerC, projectC, "usage-unpinned", state.PhaseRunning)
	seedBrokerRow(t, s, def.ID, brokerA.ID, unpinned.ID, time.Time{})

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/usage/"+def.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp usageByLimitResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	flags := map[string]bool{}
	for _, v := range resp.Reservations {
		flags[v.ResourceID] = v.ScopeConflict
	}
	require.Len(t, flags, 3)
	assert.True(t, flags[conflicted.ID], "a pinned agent's row on another broker is flagged")
	assert.False(t, flags[aligned.ID])
	assert.False(t, flags[unpinned.ID], "unpinned agents are not flagged")

	var raw struct {
		Reservations []map[string]any `json:"reservations"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	for _, row := range raw.Reservations {
		if row["resourceId"] == conflicted.ID {
			assert.Equal(t, true, row["scopeConflict"], "wire name is scopeConflict")
		}
	}
}

// racingWinnerStore makes the first `fails` inserts report a racing active
// reservation without writing, as if another writer won and released its
// row before the loser could read it back.
type racingWinnerStore struct {
	*lockingStoreWrapper
	fails atomic.Int32
}

func (r *racingWinnerStore) CreateUsageReservation(ctx context.Context, res *store.UsageReservation) (*store.UsageReservation, error) {
	if r.fails.Add(-1) >= 0 {
		return nil, store.ErrActiveReservationExists
	}
	return r.lockingStoreWrapper.CreateUsageReservation(ctx, res)
}

func TestReleaseRacesReconcile(t *testing.T) {
	t.Run("a winner released before the read back is retried once", func(t *testing.T) {
		base, err := newTestStore(t, ":memory:")
		require.NoError(t, err)
		rs := &racingWinnerStore{lockingStoreWrapper: newLockingStoreWrapper(base)}
		def := seedLimit(t, rs, scopeTestLimit, 5)
		qs := &QuotaService{store: rs, logger: slog.Default()}
		srv := &Server{store: rs, quotaService: qs, agentLifecycleLog: slog.Default()}

		rs.fails.Store(1)
		created, err := reserveOn(qs, scopeTestLimit, "broker-a", "agent-reserve")
		require.NoError(t, err)
		assert.True(t, created, "the second insert attempt wins")

		rs.fails.Store(1)
		created, err = srv.recordBrokerReservationUnchecked(context.Background(), def.ID, "broker-a", "agent-record")
		require.NoError(t, err)
		assert.True(t, created)

		// Losing the race twice is a retryable 429, not a 500, and writes
		// nothing.
		rs.fails.Store(2)
		created, err = reserveOn(qs, scopeTestLimit, "broker-a", "agent-twice")
		assert.ErrorIs(t, err, ErrQuotaLockContention)
		assert.False(t, created)
		assert.Nil(t, activeReservation(t, rs, def.ID, "agent-twice"))
		rec := httptest.NewRecorder()
		writeQuotaReserveError(rec, scopeTestLimit, err)
		assert.Equal(t, http.StatusTooManyRequests, rec.Code)

		rs.fails.Store(2)
		created, err = srv.recordBrokerReservationUnchecked(context.Background(), def.ID, "broker-a", "agent-record-twice")
		assert.ErrorIs(t, err, ErrQuotaLockContention)
		assert.False(t, created)
	})

	t.Run("concurrent release, reconcile and reserve keep the agent on its broker", func(t *testing.T) {
		srv, s := testServer(t)
		brokerA, projectA := newQuotaTestFlatBrokerAndProject(t, s, "race-a")
		brokerB, projectB := newQuotaTestFlatBrokerAndProject(t, s, "race-b")
		def, err := s.GetLimitDefinitionByName(context.Background(), store.LimitMaxAgentsPerBroker)
		require.NoError(t, err)
		agent := pinnedQuotaAgent(t, s, brokerA, projectA, "race-x", state.PhaseRunning)
		// A pinned agent on B whose row sits on A: the release, the
		// reconcile and its reserve race, and its row must never move.
		conflicted := pinnedQuotaAgent(t, s, brokerB, projectB, "race-conflicted", state.PhaseRunning)
		conflictRow := seedBrokerRow(t, s, def.ID, brokerA.ID, conflicted.ID, time.Now().Add(-2*reconcileMinReservationAge))

		ctx := context.Background()
		for i := 0; i < 25; i++ {
			var wg sync.WaitGroup
			wg.Add(4)
			go func() { defer wg.Done(); srv.releaseBrokerQuota(ctx, agent) }()
			go func() { defer wg.Done(); srv.ReconcileStaleBrokerQuotaReservations(ctx) }()
			go func() { defer wg.Done(); _, _ = srv.checkAndReserveBrokerQuota(ctx, agent) }()
			go func() { defer wg.Done(); _, _ = srv.checkAndReserveBrokerQuota(ctx, conflicted) }()
			wg.Wait()

			if held := activeReservation(t, s, def.ID, agent.ID); held != nil {
				assert.Equal(t, brokerA.ID, held.ScopeID, "iteration %d: the agent's row is only ever on its broker", i)
			}
			got := activeReservation(t, s, def.ID, conflicted.ID)
			require.NotNil(t, got, "iteration %d: the conflicting row is never released", i)
			assert.Equal(t, conflictRow.ID, got.ID, "iteration %d: never moved or replaced", i)
			assert.Zero(t, brokerReservationCount(t, s, brokerB.ID), "iteration %d: nothing on B", i)
		}
	})
}

func TestQuotaScopeConflictMetrics_CountsOncePerConflictWithSite(t *testing.T) {
	srv, s := testServer(t)
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	rec, err := NewOTelQuotaScopeConflictMetrics(mp)
	require.NoError(t, err)
	srv.SetQuotaScopeConflictMetrics(rec)

	collect := func() map[string]int64 {
		var rm metricdata.ResourceMetrics
		require.NoError(t, reader.Collect(context.Background(), &rm))
		out := map[string]int64{}
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if m.Name != "scion.hub.quota.reservation_scope_conflict" {
					continue
				}
				assert.Equal(t, "{conflict}", m.Unit)
				sum, ok := m.Data.(metricdata.Sum[int64])
				require.True(t, ok, "counter data %T", m.Data)
				assert.True(t, sum.IsMonotonic)
				for _, dp := range sum.DataPoints {
					v, ok := dp.Attributes.Value("site")
					require.True(t, ok, "every data point has a site")
					out[v.AsString()] += dp.Value
				}
			}
		}
		return out
	}

	brokerA, projectA := newQuotaTestFlatBrokerAndProject(t, s, "metric-a")
	brokerB, projectB := newQuotaTestFlatBrokerAndProject(t, s, "metric-b")
	def, err := s.GetLimitDefinitionByName(context.Background(), store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)
	agent := pinnedQuotaAgent(t, s, brokerB, projectB, "metric-x", state.PhaseRunning)
	seedBrokerRow(t, s, def.ID, brokerA.ID, agent.ID, time.Time{})

	assert.Empty(t, collect(), "nothing counted before a conflict")

	_, err = srv.checkAndReserveBrokerQuota(context.Background(), agent)
	require.Error(t, err)
	assert.Equal(t, map[string]int64{scopeConflictSiteReserve: 1}, collect())

	srv.reassertBrokerReservation(context.Background(), agent)
	assert.Equal(t, map[string]int64{scopeConflictSiteReserve: 1, scopeConflictSiteReassert: 1}, collect())

	// A same-scope retry is not a conflict.
	ok := pinnedQuotaAgent(t, s, brokerA, projectA, "metric-ok", state.PhaseRunning)
	_, err = srv.checkAndReserveBrokerQuota(context.Background(), ok)
	require.NoError(t, err)
	_, err = srv.checkAndReserveBrokerQuota(context.Background(), ok)
	require.NoError(t, err)
	assert.Equal(t, map[string]int64{scopeConflictSiteReserve: 1, scopeConflictSiteReassert: 1}, collect())
}

// lockingPassthroughStore forwards the advisory locks of the wrapped store,
// which store.Store does not carry, so a wrapper can stand in as a
// QuotaService store.
type lockingPassthroughStore struct{ store.Store }

func (p lockingPassthroughStore) TryAdvisoryLock(ctx context.Context, key store.AdvisoryLockKey) (bool, func() error, error) {
	return p.Store.(store.AdvisoryLocker).TryAdvisoryLock(ctx, key)
}

func (p lockingPassthroughStore) TryAdvisoryLockObject(ctx context.Context, class store.AdvisoryLockKey, obj int32) (bool, func() error, error) {
	return p.Store.(store.AdvisoryLocker).TryAdvisoryLockObject(ctx, class, obj)
}

// A move whose transaction fails after it released the source row (a
// failure injected through the store's transfer hook right before the
// insert on the target) fails the move and rolls the release back: the
// agent keeps exactly its source row, unchanged, and nothing is on the
// target.
func TestMoveBrokerQuota_FailedTransferKeepsSource(t *testing.T) {
	srv, s := testServer(t)
	brokerA, projectA := newQuotaTestBrokerAndProject(t, s, "move-a")
	brokerB, _ := newQuotaTestBrokerAndProject(t, s, "move-b")
	def, err := s.GetLimitDefinitionByName(context.Background(), store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)
	agent := newQuotaTestAgent(t, s, brokerA, projectA, "move-x", state.PhaseStopped)
	row := seedBrokerRow(t, s, def.ID, brokerA.ID, agent.ID, time.Time{})

	injected := errors.New("injected transfer failure")
	var steps []string
	t.Cleanup(entadapter.SetTransferBrokerReservationTestHook(func(_ context.Context, _ *ent.Tx, step string, req store.BrokerReservationTransfer) error {
		if req.AgentID != agent.ID {
			return nil
		}
		steps = append(steps, step)
		if step == "before_insert" {
			return injected
		}
		return nil
	}))

	err = srv.moveBrokerQuota(context.Background(), agent, &reincarnationMove{SourceBrokerID: brokerA.ID, TargetBrokerID: brokerB.ID})
	require.ErrorIs(t, err, injected, "the injected transfer failure fails the move")
	assert.Equal(t, []string{"locked", "released", "before_insert"}, steps, "the source row was released inside the transaction")
	got := activeReservation(t, s, def.ID, agent.ID)
	require.NotNil(t, got)
	assert.Equal(t, row.ID, got.ID, "the slot stays on the source, unchanged")
	assert.Equal(t, brokerA.ID, got.ScopeID)
	assert.Zero(t, brokerReservationCount(t, s, brokerB.ID), "nothing on the target")
}

// racingRowStore makes the next CreateUsageReservation lose to a row it
// first writes itself on winnerScope, as a racing writer would. It is
// transparent until its fault switch is armed.
type racingRowStore struct {
	store.Store
	fault       *storeFaultSwitch
	winnerScope string
}

func (r *racingRowStore) CreateUsageReservation(ctx context.Context, res *store.UsageReservation) (*store.UsageReservation, error) {
	if !r.fault.Active() || r.winnerScope == "" {
		return r.Store.CreateUsageReservation(ctx, res)
	}
	winner := *res
	winner.SubjectID, winner.ScopeID = r.winnerScope, r.winnerScope
	r.winnerScope = ""
	if _, err := r.Store.CreateUsageReservation(ctx, &winner); err != nil {
		return nil, err
	}
	return nil, store.ErrActiveReservationExists
}

func TestRecordBrokerReservationUnchecked_RacingWinnerClassified(t *testing.T) {
	srv, s, rs, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *racingRowStore {
		return &racingRowStore{Store: inner, fault: f}
	})
	def, err := s.GetLimitDefinitionByName(context.Background(), store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)
	fault.Arm()

	rs.winnerScope = "broker-a"
	created, err := srv.recordBrokerReservationUnchecked(context.Background(), def.ID, "broker-a", "agent-same")
	require.NoError(t, err, "a racing winner on the same broker is a retry")
	assert.False(t, created)
	held := activeReservation(t, s, def.ID, "agent-same")
	require.NotNil(t, held)
	assert.Equal(t, "broker-a", held.ScopeID)

	rs.winnerScope = "broker-other"
	created, err = srv.recordBrokerReservationUnchecked(context.Background(), def.ID, "broker-a", "agent-other")
	requireScopeConflict(t, err, "agent-other", "broker-other", "broker-a")
	assert.False(t, created)
	held = activeReservation(t, s, def.ID, "agent-other")
	require.NotNil(t, held)
	assert.Equal(t, "broker-other", held.ScopeID, "the winner's row is left as it is")
}

// An unpinned running agent on B whose row is on listed broker A keeps the
// row (it holds capacity); the backfill on B is refused by the typed
// conflict and reported on every pass, and writes nothing.
func TestSweep_UnpinnedCountedAgentBackfillConflictReported(t *testing.T) {
	srv, s := testServer(t)
	conflicts := scopeConflictCounter(t, srv.quotaService)
	brokerA, _ := newQuotaTestBrokerAndProject(t, s, "unpinned-a")
	brokerB, projectB := newQuotaTestBrokerAndProject(t, s, "unpinned-b")
	def, err := s.GetLimitDefinitionByName(context.Background(), store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)
	agent := newQuotaTestAgent(t, s, brokerB, projectB, "unpinned-running", state.PhaseRunning)
	row := seedBrokerRow(t, s, def.ID, brokerA.ID, agent.ID, time.Now().Add(-2*reconcileMinReservationAge))

	for pass := 1; pass <= 2; pass++ {
		srv.ReconcileStaleBrokerQuotaReservations(context.Background())
		got := activeReservation(t, s, def.ID, agent.ID)
		require.NotNil(t, got, "pass %d", pass)
		assert.Equal(t, row.ID, got.ID, "pass %d: kept, not replaced", pass)
		assert.Zero(t, brokerReservationCount(t, s, brokerB.ID), "pass %d: nothing on B", pass)
		assert.EqualValues(t, pass, conflicts(scopeConflictSiteBackfill), "pass %d", pass)
		assert.Zero(t, conflicts(scopeConflictSiteSweep), "pass %d: unpinned rows are not a sweep conflict", pass)
	}
}

// TestSweep_PinnedConflictWithoutCapacityReleasedThenStartSucceeds is the
// stuck case of contract erratum E-FC11-1: a stopped or errored pinned
// agent whose row on another broker is past the grace holds no capacity, so
// the sweep releases the row. The release touches neither the pin nor the
// agent's Runtime Broker and writes nothing on B; the next start passes the
// placement check and reserves on the agent's own broker.
func TestSweep_PinnedConflictWithoutCapacityReleasedThenStartSucceeds(t *testing.T) {
	srv, s := testServer(t)
	disp := &quotaLifecycleDispatcher{}
	srv.SetDispatcher(disp)
	conflicts := scopeConflictCounter(t, srv.quotaService)
	brokerA, _ := newQuotaTestBrokerAndProject(t, s, "stuck-a")
	brokerB, projectB := newQuotaTestFlatBrokerAndProject(t, s, "stuck-b")
	def, err := s.GetLimitDefinitionByName(context.Background(), store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)
	old := time.Now().Add(-2 * reconcileMinReservationAge)

	stopped := pinnedQuotaAgent(t, s, brokerB, projectB, "stuck-stopped", state.PhaseStopped)
	seedBrokerRow(t, s, def.ID, brokerA.ID, stopped.ID, old)
	errored := pinnedQuotaAgent(t, s, brokerB, projectB, "stuck-error", state.PhaseError)
	seedBrokerRow(t, s, def.ID, brokerA.ID, errored.ID, old)

	// Before the sweep the start of either agent is refused by the scope
	// conflict, by status and by code: the placement is valid, so the
	// refusal can only come from Reserve.
	for _, a := range []*store.Agent{stopped, errored} {
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/start", nil)
		require.Equal(t, http.StatusConflict, rec.Code, "%s: %s", a.Slug, rec.Body.String())
		var pre ErrorResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &pre), rec.Body.String())
		require.Equal(t, ErrCodeBrokerReservationScopeConflict, pre.Error.Code, "%s: %s", a.Slug, rec.Body.String())
	}

	srv.ReconcileStaleBrokerQuotaReservations(context.Background())

	for _, a := range []*store.Agent{stopped, errored} {
		assert.Nil(t, activeReservation(t, s, def.ID, a.ID), "%s: the row on A is released", a.Slug)
		got, err := s.GetAgent(context.Background(), a.ID)
		require.NoError(t, err)
		assert.Equal(t, a.Placement(), got.Placement(), "%s: pin and Runtime Broker untouched", a.Slug)
		assert.Equal(t, a.PinnedRuntimeBrokerID, got.PinnedRuntimeBrokerID, "%s", a.Slug)
	}
	assert.Zero(t, brokerReservationCount(t, s, brokerB.ID), "nothing is transferred or backfilled to B")
	assert.Zero(t, conflicts(scopeConflictSiteSweep), "released, not reported")

	for _, a := range []*store.Agent{stopped, errored} {
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/start", nil)
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", a.Slug, rec.Body.String())
		held := activeReservation(t, s, def.ID, a.ID)
		require.NotNil(t, held, "%s: the start reserved", a.Slug)
		assert.Equal(t, brokerB.ID, held.ScopeID, "%s: on its own broker", a.Slug)
	}
	assert.EqualValues(t, 2, disp.startCount.Load())
}

// A released conflict authorizes nothing: an agent whose pin names another
// Runtime Broker than the one it is assigned to is still refused at start
// after the sweep released its row, and no reservation is made.
func TestSweep_ReleasedConflictStalePinStillRefused(t *testing.T) {
	srv, s := testServer(t)
	disp := &quotaLifecycleDispatcher{}
	srv.SetDispatcher(disp)
	brokerA, projectA := newQuotaTestFlatBrokerAndProject(t, s, "stalepin-a")
	brokerB, _ := newQuotaTestBrokerAndProject(t, s, "stalepin-b")
	def, err := s.GetLimitDefinitionByName(context.Background(), store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)

	agent := pinnedQuotaAgent(t, s, brokerA, projectA, "stalepin-x", state.PhaseStopped)
	seedBrokerRow(t, s, def.ID, brokerA.ID, agent.ID, time.Now().Add(-2*reconcileMinReservationAge))
	agent.RuntimeBrokerID = brokerB.ID // the pin still names A
	require.NoError(t, s.UpdateAgent(context.Background(), agent))
	agent, err = s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	require.True(t, agent.IsPinned())
	require.False(t, agent.PinValid())

	srv.ReconcileStaleBrokerQuotaReservations(context.Background())
	require.Nil(t, activeReservation(t, s, def.ID, agent.ID), "the row on A is released")

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", nil)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), rec.Body.String())
	assert.Equal(t, ErrCodeRuntimeTargetPinStale, resp.Error.Code)
	assert.Nil(t, activeReservation(t, s, def.ID, agent.ID), "no reservation on either broker")
	assert.Zero(t, disp.startCount.Load())
	got, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, agent.Placement(), got.Placement(), "the pin is untouched")
	assert.Equal(t, brokerA.ID, got.PinnedRuntimeBrokerID)
}

// reReserveAfterReadStore runs reReserve once, right after the sweep's batch
// read of the agents holding rows (so the sweep acts on a stale
// observation), as a start that re-reserved in that window would. It is
// transparent until its fault switch is armed.
type reReserveAfterReadStore struct {
	store.Store
	fault     *storeFaultSwitch
	agentID   string
	reReserve func()
	done      bool
}

func (r *reReserveAfterReadStore) GetAgentsByIDs(ctx context.Context, ids []string) (map[string]*store.Agent, error) {
	out, err := r.Store.GetAgentsByIDs(ctx, ids)
	if r.fault.Active() && !r.done {
		for _, id := range ids {
			if id == r.agentID {
				r.done = true
				r.reReserve()
				break
			}
		}
	}
	return out, err
}

// The sweep's release is conditional on the row it read: a start that
// re-reserved between the sweep's read and its release holds a new row, and
// that slot is not freed.
func TestSweep_StaleObservationDoesNotReleaseAReReservedSlot(t *testing.T) {
	srv, s, rr, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *reReserveAfterReadStore {
		return &reReserveAfterReadStore{Store: inner, fault: f}
	})
	brokerA, _ := newQuotaTestBrokerAndProject(t, s, "rerez-a")
	brokerB, projectB := newQuotaTestFlatBrokerAndProject(t, s, "rerez-b")
	def, err := s.GetLimitDefinitionByName(context.Background(), store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)
	agent := pinnedQuotaAgent(t, s, brokerB, projectB, "rerez-x", state.PhaseStopped)
	seedBrokerRow(t, s, def.ID, brokerA.ID, agent.ID, time.Now().Add(-2*reconcileMinReservationAge))

	var fresh *store.UsageReservation
	rr.agentID = agent.ID
	rr.reReserve = func() {
		// The old row goes and a start reserves on the agent's own broker.
		require.NoError(t, s.ReleaseReservation(context.Background(), def.ID, agent.ID))
		fresh = seedBrokerRow(t, s, def.ID, brokerB.ID, agent.ID, time.Time{})
	}
	fault.Arm()

	srv.ReconcileStaleBrokerQuotaReservations(context.Background())

	require.NotNil(t, fresh, "the start re-reserved during the pass")
	held := activeReservation(t, s, def.ID, agent.ID)
	require.NotNil(t, held, "the re-reserved slot is not freed by the stale observation")
	assert.Equal(t, fresh.ID, held.ID)
	assert.Equal(t, brokerB.ID, held.ScopeID)
}

// Placement is checked before capacity: a pinned agent whose pin is stale
// (it names flat Runtime Broker A, but the agent is assigned to flat Runtime
// Broker B) and whose reservation is still on A is refused at start with
// runtime_target_pin_stale, not the scope conflict. The reservation on A is
// unchanged and nothing is reserved on B.
func TestStart_StalePinRefusedBeforeReservationScopeConflict(t *testing.T) {
	srv, s := testServer(t)
	disp := &quotaLifecycleDispatcher{}
	srv.SetDispatcher(disp)
	brokerA, projectA := newQuotaTestFlatBrokerAndProject(t, s, "order-a")
	brokerB, _ := newQuotaTestFlatBrokerAndProject(t, s, "order-b")
	def, err := s.GetLimitDefinitionByName(context.Background(), store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)

	agent := pinnedQuotaAgent(t, s, brokerA, projectA, "order-x", state.PhaseStopped)
	row := seedBrokerRow(t, s, def.ID, brokerA.ID, agent.ID, time.Time{})
	agent.RuntimeBrokerID = brokerB.ID // the pin still names A
	require.NoError(t, s.UpdateAgent(context.Background(), agent))
	agent, err = s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	require.True(t, agent.IsPinned())
	require.False(t, agent.PinValid())

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", nil)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), rec.Body.String())
	assert.Equal(t, ErrCodeRuntimeTargetPinStale, resp.Error.Code, "placement is refused before the reservation is looked at")

	got := activeReservation(t, s, def.ID, agent.ID)
	require.NotNil(t, got, "the reservation on A is kept")
	assert.Equal(t, row.ID, got.ID)
	assert.Equal(t, brokerA.ID, got.ScopeID)
	assert.Zero(t, brokerReservationCount(t, s, brokerB.ID), "nothing is reserved on B")
	assert.Zero(t, disp.startCount.Load(), "nothing was dispatched")
}
