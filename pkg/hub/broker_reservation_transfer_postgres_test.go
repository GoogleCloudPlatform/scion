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
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

// Postgres variants of the atomic broker reservation transfer tests
// (ptone/scion#5951). Each is listed in HUB_INTEGRATION_POSTGRES_TESTS in
// the Makefile, so make test-hub-postgres-integration and CI's pkg/hub
// Postgres (integration) shard jobs run it. They skip only through
// requirePG. Each builds its own fixture on a fresh schema.

// pgTransferReplica is one hub replica: a server on its own Postgres pool.
type pgTransferReplica struct {
	srv *Server
	s   store.Store
}

// newPGTransferReplicas opens n hub replicas on one fresh Postgres schema,
// each with its own CompositeStore and pool, and sets the
// max_agents_per_broker default to ceiling.
func newPGTransferReplicas(t *testing.T, n int, ceiling int64) []pgTransferReplica {
	t.Helper()
	dsn := enttest.NewSchemaURL(t)
	out := make([]pgTransferReplica, 0, n)
	for i := 0; i < n; i++ {
		client, err := entc.OpenPostgres(dsn, entc.PoolConfig{MaxOpenConns: 8, MaxIdleConns: 4})
		require.NoError(t, err)
		srv, s := testServerWithStore(t, entadapter.NewCompositeStore(client))
		out = append(out, pgTransferReplica{srv: srv, s: s})
	}
	ctx := context.Background()
	def, err := out[0].s.GetLimitDefinitionByName(ctx, store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)
	def.DefaultValue = ceiling
	_, err = out[0].s.UpdateLimitDefinition(ctx, def)
	require.NoError(t, err)
	return out
}

// pgTransferBroker creates an online broker and a project on it.
func pgTransferBroker(t *testing.T, s store.Store, suffix string) (*store.RuntimeBroker, *store.Project) {
	t.Helper()
	ctx := context.Background()
	broker := &store.RuntimeBroker{
		ID: tid("pgx-broker-" + suffix), Name: "pgx broker " + suffix, Slug: "pgx-broker-" + suffix,
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	project := &store.Project{ID: tid("pgx-proj-" + suffix), Name: "pgx project " + suffix, Slug: "pgx-project-" + suffix}
	require.NoError(t, s.CreateProject(ctx, project))
	return broker, project
}

// pgTransferAgent creates a stopped agent on broker and, when held, an
// active reservation for it there, which it returns.
func pgTransferAgent(t *testing.T, s store.Store, broker *store.RuntimeBroker, project *store.Project, name string, held bool) (*store.Agent, *store.UsageReservation) {
	t.Helper()
	ctx := context.Background()
	a := &store.Agent{ID: tid("pgx-agent-" + name), Slug: "pgx-" + name, Name: "pgx-" + name,
		ProjectID: project.ID, RuntimeBrokerID: broker.ID, Phase: "stopped"}
	require.NoError(t, s.CreateAgent(ctx, a))
	if !held {
		return a, nil
	}
	def, err := s.GetLimitDefinitionByName(ctx, store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)
	row, err := s.CreateUsageReservation(ctx, &store.UsageReservation{
		LimitDefinitionID: def.ID, SubjectID: broker.ID, ScopeType: store.QuotaScopeBroker,
		ScopeID: broker.ID, ResourceID: a.ID, Reserved: 1,
	})
	require.NoError(t, err)
	return a, row
}

// pgReserve makes a checked broker reservation of resourceID on broker.
func pgReserve(ctx context.Context, qs *QuotaService, broker, resourceID string) (bool, error) {
	return qs.Reserve(ctx, store.LimitMaxAgentsPerBroker, broker, store.QuotaScopeBroker, broker, resourceID)
}

// pgBrokerCount returns the active broker-scoped reservations on broker.
func pgBrokerCount(t *testing.T, s store.Store, broker string) int64 {
	t.Helper()
	ctx := context.Background()
	def, err := s.GetLimitDefinitionByName(ctx, store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)
	n, err := s.CountActiveReservations(ctx, def.ID, broker, store.QuotaScopeBroker, broker)
	require.NoError(t, err)
	return n
}

// pgHeld returns agentID's active reservation (nil when none).
func pgHeld(t *testing.T, s store.Store, agentID string) *store.UsageReservation {
	t.Helper()
	ctx := context.Background()
	def, err := s.GetLimitDefinitionByName(ctx, store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)
	r, err := s.GetActiveReservation(ctx, def.ID, agentID)
	if err != nil {
		require.ErrorIs(t, err, store.ErrNotFound)
		return nil
	}
	return r
}

// AC1 on Postgres: the source is at its cap and the target is full; a
// checked Reserve for another agent on the source at moveBrokerQuotaTestHook
// is refused, the source never exceeds its cap, and the moving agent keeps
// its single reservation on the source.
func TestMoveBrokerQuota_SourceAtCapTargetFull_Postgres(t *testing.T) {
	requirePG(t)
	r := newPGTransferReplicas(t, 1, 1)[0]
	ctx := context.Background()
	src, project := pgTransferBroker(t, r.s, "ac1-src")
	dst, _ := pgTransferBroker(t, r.s, "ac1-dst")
	agent, row := pgTransferAgent(t, r.s, src, project, "ac1-mover", true)
	_, err := pgReserve(ctx, r.srv.quotaService, dst.ID, tid("pgx-ac1-occupant"))
	require.NoError(t, err)

	ran := false
	var reserveErr error
	t.Cleanup(setMoveBrokerQuotaTestHook(func(ctx context.Context) {
		ran = true
		_, reserveErr = pgReserve(ctx, r.srv.quotaService, src.ID, tid("pgx-ac1-intruder"))
	}))

	err = r.srv.moveBrokerQuota(ctx, agent, &reincarnationMove{SourceBrokerID: src.ID, TargetBrokerID: dst.ID})
	var exceeded *QuotaExceededError
	require.ErrorAs(t, err, &exceeded)
	assert.Equal(t, dst.ID, exceeded.ScopeID)
	require.True(t, ran, "the hook ran")
	assert.ErrorIs(t, reserveErr, store.ErrQuotaExceeded, "the source had no free slot at any point")
	assert.EqualValues(t, 1, pgBrokerCount(t, r.s, src.ID), "the source never exceeds its cap")
	assert.EqualValues(t, 1, pgBrokerCount(t, r.s, dst.ID), "only the occupant")
	got := pgHeld(t, r.s, agent.ID)
	require.NotNil(t, got)
	assert.Equal(t, row.ID, got.ID, "the source row is untouched")
}

// AC2a on Postgres: a transfer onto a target filled to its cap by checked
// Reserves returns a QuotaExceededError and writes nothing.
func TestTransferBroker_TargetAtCapRefused_Postgres(t *testing.T) {
	requirePG(t)
	r := newPGTransferReplicas(t, 1, 2)[0]
	ctx := context.Background()
	src, project := pgTransferBroker(t, r.s, "ac2a-src")
	dst, _ := pgTransferBroker(t, r.s, "ac2a-dst")
	agent, row := pgTransferAgent(t, r.s, src, project, "ac2a-mover", true)
	for i := 0; i < 2; i++ {
		created, err := pgReserve(ctx, r.srv.quotaService, dst.ID, tid(fmt.Sprintf("pgx-ac2a-full-%d", i)))
		require.NoError(t, err)
		require.True(t, created)
	}

	_, limited, err := r.srv.quotaService.TransferBroker(ctx, agent.ID, dst.ID, TransferOpts{})
	assert.True(t, limited)
	var exceeded *QuotaExceededError
	require.ErrorAs(t, err, &exceeded)
	assert.Equal(t, dst.ID, exceeded.ScopeID)
	assert.EqualValues(t, 2, pgBrokerCount(t, r.s, dst.ID), "nothing written on the target")
	got := pgHeld(t, r.s, agent.ID)
	require.NotNil(t, got)
	assert.Equal(t, row.ID, got.ID, "the source row is untouched")
	assert.Equal(t, src.ID, got.ScopeID)
}

// AC2b: the target's advisory lock is held across the transfer's
// transaction. At the store hook's "locked" step, a second replica's checked
// Reserve on the target, with enforcement on, for an agent holding no row
// (so Reserve reaches the lock), gets ErrQuotaLockContention.
func TestTransferBroker_TargetLockHeldAcrossTransaction_Postgres(t *testing.T) {
	requirePG(t)
	reps := newPGTransferReplicas(t, 2, 5)
	a, b := reps[0], reps[1]
	ctx := context.Background()
	src, project := pgTransferBroker(t, a.s, "ac2b-src")
	dst, _ := pgTransferBroker(t, a.s, "ac2b-dst")
	agent, _ := pgTransferAgent(t, a.s, src, project, "ac2b-mover", true)
	other, _ := pgTransferAgent(t, a.s, dst, project, "ac2b-other", false)

	ran := false
	var reserveErr error
	t.Cleanup(entadapter.SetTransferBrokerReservationTestHook(func(ctx context.Context, _ *ent.Tx, step string, req store.BrokerReservationTransfer) error {
		if req.AgentID != agent.ID || step != "locked" {
			return nil
		}
		ran = true
		_, reserveErr = pgReserve(ctx, b.srv.quotaService, dst.ID, other.ID)
		return nil
	}))

	res, limited, err := a.srv.quotaService.TransferBroker(ctx, agent.ID, dst.ID, TransferOpts{})
	require.NoError(t, err)
	assert.True(t, limited)
	assert.Equal(t, store.TransferMoved, res.Outcome)
	require.True(t, ran, "the hook ran")
	assert.ErrorIs(t, reserveErr, ErrQuotaLockContention, "the second replica could not take the target's lock")
	assert.Nil(t, pgHeld(t, a.s, other.ID), "the refused Reserve wrote nothing")
	assert.EqualValues(t, 1, pgBrokerCount(t, a.s, dst.ID))
}

// AC2 soak: 50 barrier-started rounds of a transfer on one replica and a
// checked Reserve on another, onto a target at cap minus 1. The target
// never exceeds its cap. A contention loss is that call's failure and is not
// retried.
func TestTransferBroker_ConcurrentReserveSoak_Postgres(t *testing.T) {
	requirePG(t)
	const rounds, ceiling = 50, 3
	reps := newPGTransferReplicas(t, 2, ceiling)
	a, b := reps[0], reps[1]
	ctx := context.Background()
	outcomes := map[string]int{}
	for i := 0; i < rounds; i++ {
		src, project := pgTransferBroker(t, a.s, fmt.Sprintf("soak-src-%d", i))
		dst, _ := pgTransferBroker(t, a.s, fmt.Sprintf("soak-dst-%d", i))
		mover, _ := pgTransferAgent(t, a.s, src, project, fmt.Sprintf("soak-mover-%d", i), true)
		rival, _ := pgTransferAgent(t, a.s, dst, project, fmt.Sprintf("soak-rival-%d", i), false)
		for j := 0; j < ceiling-1; j++ {
			_, err := pgReserve(ctx, a.srv.quotaService, dst.ID, tid(fmt.Sprintf("pgx-soak-fill-%d-%d", i, j)))
			require.NoError(t, err)
		}

		var wg sync.WaitGroup
		start := make(chan struct{})
		var transferErr, reserveErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, _, transferErr = a.srv.quotaService.TransferBroker(ctx, mover.ID, dst.ID, TransferOpts{})
		}()
		go func() {
			defer wg.Done()
			<-start
			_, reserveErr = pgReserve(ctx, b.srv.quotaService, dst.ID, rival.ID)
		}()
		close(start)
		wg.Wait()

		for _, e := range []error{transferErr, reserveErr} {
			switch {
			case e == nil:
				outcomes["ok"]++
			case errors.Is(e, ErrQuotaLockContention):
				outcomes["contention"]++
			case errors.Is(e, store.ErrQuotaExceeded):
				outcomes["exceeded"]++
			default:
				require.NoError(t, e, "round %d: only success, contention or quota exceeded are expected", i)
			}
		}
		require.LessOrEqual(t, pgBrokerCount(t, a.s, dst.ID), int64(ceiling), "round %d: the target exceeded its cap", i)
		held := pgHeld(t, a.s, mover.ID)
		require.NotNil(t, held, "round %d: the mover always keeps exactly one reservation", i)
	}
	t.Logf("soak outcomes over %d rounds: %v", rounds, outcomes)
}
