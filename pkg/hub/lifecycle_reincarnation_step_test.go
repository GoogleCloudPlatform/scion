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
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/lifecycleevents"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	tc "github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetryemit"
)

// The reincarnation step write (writeReincarnationStep, through
// updateReincarnationStep) after the upstream merge: a step that applies a
// configuration is a config-resolution write on both the plain and the
// transactional (txWork) path, and the lifecycle observer records in the
// write's own transaction. So a committed step leaves its events, and a
// failed or rolled-back step leaves no outbox row, no cursor advance and
// no enqueue count (enqueue counts are added only after commit).

// reincStepFixture is an agent with configuration model m-a on a server
// whose observer is installed but whose drainer is not running, so every
// committed event stays in the outbox.
type reincStepFixture struct {
	srv      *Server
	raw      store.Store
	ls       lifecycleEventStore
	sink     *telemetryemit.MemorySink
	counters *telemetryemit.MemoryCounters
	agent    *store.Agent
}

// failAfterObserver runs the real lifecycle observer (its cursor bump and
// outbox insert happen in the write's transaction) and then fails, so the
// write and everything the observer wrote must roll back.
type failAfterObserver struct {
	inner store.AgentWriteObserver
	// sawRev is the agent's cursor transition revision read in the write's
	// transaction right after the inner observer returned: higher than the
	// committed revision only if the observer really wrote (an inner
	// observer that skipped the write, e.g. on a build failure, also
	// returns nil).
	sawRev int64
}

var errInjectedReincStep = errors.New("injected reincarnation step failure")

func (o *failAfterObserver) ObserveAgentWrite(ctx context.Context, tx store.LifecycleTx, w store.AgentWrite) error {
	if err := o.inner.ObserveAgentWrite(ctx, tx, w); err != nil {
		return err
	}
	c, err := tx.GetLifecycleCursor(ctx, w.AgentID())
	if err != nil {
		return err
	}
	if c != nil && c.TransitionRevision > o.sawRev {
		o.sawRev = c.TransitionRevision
	}
	return errInjectedReincStep
}

func newReincStepFixture(t *testing.T, name string) *reincStepFixture {
	t.Helper()
	srv, s := testServer(t)
	ls, ok := s.(lifecycleEventStore)
	require.True(t, ok)
	srv.config.HubIDPinned = true
	sink, counters := telemetryemit.NewMemorySink(), telemetryemit.NewMemoryCounters()
	srv.SetLifecycleEventsSink(sink, counters)
	st, err := srv.installLifecycleObserver()
	require.NoError(t, err)
	require.Equal(t, telemetryemit.ObserverStateEnabled, st)
	ctx := context.Background()
	project := &store.Project{ID: tid("project-" + name), Slug: name, Name: name, Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, project))
	agent := &store.Agent{
		ID: tid("agent-" + name), Slug: name, Name: name, ProjectID: project.ID,
		Phase: string(state.PhaseRunning), StateVersion: 1, Generation: 1,
		AppliedConfig: &store.AgentAppliedConfig{Harness: "claude", Model: "m-a"},
		Created:       time.Now(), Updated: time.Now(),
	}
	cctx := lifecycleevents.WithCreateContext(ctx, lifecycleevents.CreateContext{CreatorType: "user", CreatorID: "u1"})
	require.NoError(t, s.WithTx(cctx, func(tx store.Store) error { return tx.CreateAgent(cctx, agent) }))
	return &reincStepFixture{srv: srv, raw: s, ls: ls, sink: sink, counters: counters, agent: agent}
}

// lifecycleState is the agent's committed lifecycle metadata: its outbox
// rows, its cursor, and the observer's enqueue count.
type lifecycleState struct {
	rows, maxAgentSeq, maxSeq int64
	transitionRev, metaRev    int64
	metaDigest, fingerprint   string
	enqueued                  int64
}

func (f *reincStepFixture) state(t *testing.T) lifecycleState {
	t.Helper()
	db := f.raw.(interface{ DB() *sql.DB }).DB()
	ctx := context.Background()
	var st lifecycleState
	require.NoError(t, db.QueryRowContext(ctx,
		"SELECT COUNT(*), COALESCE(MAX(seq), 0) FROM agent_lifecycle_outbox WHERE agent_id = ?", f.agent.ID).Scan(&st.rows, &st.maxAgentSeq))
	// The outbox-wide seq: a rolled-back step must not advance it either.
	require.NoError(t, db.QueryRowContext(ctx, "SELECT COALESCE(MAX(seq), 0) FROM agent_lifecycle_outbox").Scan(&st.maxSeq))
	require.NoError(t, db.QueryRowContext(ctx,
		"SELECT transition_revision, metadata_revision, metadata_digest, last_fingerprint FROM agent_lifecycle_cursors WHERE agent_id = ?", f.agent.ID).
		Scan(&st.transitionRev, &st.metaRev, &st.metaDigest, &st.fingerprint))
	for _, field := range strings.Fields(f.counters.String()) {
		if k, v, ok := strings.Cut(field, "="); ok && strings.HasPrefix(k, telemetryemit.MetricEventsEnqueued+"{") {
			n, err := strconv.ParseInt(v, 10, 64)
			require.NoError(t, err)
			st.enqueued += n
		}
	}
	return st
}

// step runs one reincarnation step that applies model m-b, with txWork
// when given.
func (f *reincStepFixture) step(maxAttempts int, txWork func(context.Context, store.Store, *store.Agent) error) error {
	upd := reincarnationStepUpdate{
		reincarnationState: store.ReincarnationStateFailed,
		phase:              "error",
		appliedConfig:      &store.AgentAppliedConfig{Harness: "claude", Model: "m-b"},
		txWork:             txWork,
	}
	// A bound on the step: a write that blocked on the transaction's own
	// lock (a write outside it, under SQLite's single writer) fails the
	// test instead of hanging it.
	ctx, cancel := context.WithTimeout(context.Background(), reincStepTimeout)
	defer cancel()
	_, err := f.srv.updateReincarnationStep(ctx, f.agent.ID, upd, maxAttempts)
	return err
}

// reincStepTimeout bounds one step call in these tests.
const reincStepTimeout = 30 * time.Second

func (f *reincStepFixture) configured(t *testing.T) []*tc.LifecycleEvent {
	t.Helper()
	var out []*tc.LifecycleEvent
	for _, e := range lcEvents(t, f.ls, f.sink, f.agent.ID) {
		if e.Transition == tc.TransitionConfigured {
			out = append(out, e)
		}
	}
	return out
}

func (f *reincStepFixture) assertCommitted(t *testing.T, before lifecycleState) {
	t.Helper()
	after := f.state(t)
	assert.Greater(t, after.rows, before.rows, "the committed step left outbox rows")
	assert.Greater(t, after.transitionRev, before.transitionRev, "the cursor advanced")
	assert.Greater(t, after.enqueued, before.enqueued, "the enqueue count moved after commit")
	assert.Equal(t, after.rows-before.rows, after.enqueued-before.enqueued,
		"one enqueue count per committed outbox row (no drainer runs, so no row has left the outbox)")
	assert.Equal(t, "m-b", mustGetAgent(t, f.raw, f.agent.ID).AppliedConfig.Model)
	evs := lcEvents(t, f.ls, f.sink, f.agent.ID)
	assertDense(t, evs)
	got := f.configured(t)
	require.Len(t, got, 1, "a config-resolution step emits configured: %s", lcSeqFull(evs))
	require.NotNil(t, got[0].Metadata)
	assert.Equal(t, "m-b", got[0].Metadata.RequestedModel)
}

func (f *reincStepFixture) assertNothingCommitted(t *testing.T, before lifecycleState) {
	t.Helper()
	assert.Equal(t, before, f.state(t), "no outbox row, cursor advance or enqueue count")
	row := mustGetAgent(t, f.raw, f.agent.ID)
	assert.Equal(t, "m-a", row.AppliedConfig.Model, "the agent write rolled back")
	assert.Empty(t, row.ReincarnationState)
	assert.Empty(t, f.configured(t))
}

// TestReincarnationStepLifecycle_PlainCommit: the plain path (no txWork)
// commits the step and its configured.
func TestReincarnationStepLifecycle_PlainCommit(t *testing.T) {
	f := newReincStepFixture(t, "reinc-lc-plain")
	before := f.state(t)
	require.NoError(t, f.step(1, nil))
	f.assertCommitted(t, before)
}

// TestReincarnationStepLifecycle_TxCommit: the transactional path
// (writeFailedAgent's restore) commits the step, its txWork and its
// configured together.
func TestReincarnationStepLifecycle_TxCommit(t *testing.T) {
	f := newReincStepFixture(t, "reinc-lc-tx")
	before := f.state(t)
	ran := false
	require.NoError(t, f.step(1, func(context.Context, store.Store, *store.Agent) error { ran = true; return nil }))
	require.True(t, ran)
	f.assertCommitted(t, before)
}

// TestReincarnationStepLifecycle_TxWorkFailureRollsBack: txWork fails
// after the agent write, inside the transaction: the write and every
// lifecycle record of it roll back.
func TestReincarnationStepLifecycle_TxWorkFailureRollsBack(t *testing.T) {
	f := newReincStepFixture(t, "reinc-lc-txfail")
	before := f.state(t)
	err := f.step(1, func(context.Context, store.Store, *store.Agent) error { return errInjectedReincStep })
	require.ErrorIs(t, err, errInjectedReincStep)
	f.assertNothingCommitted(t, before)
}

// TestReincarnationStepLifecycle_TxConflictRetryCommitsOnce: the first
// attempt's transaction rolls back on a version conflict and the retry
// commits: exactly one configured, revisions dense (the rolled-back
// attempt left nothing).
func TestReincarnationStepLifecycle_TxConflictRetryCommitsOnce(t *testing.T) {
	f := newReincStepFixture(t, "reinc-lc-txretry")
	before := f.state(t)
	attempts := 0
	require.NoError(t, f.step(2, func(context.Context, store.Store, *store.Agent) error {
		attempts++
		if attempts == 1 {
			return fmt.Errorf("concurrent write: %w", store.ErrVersionConflict)
		}
		return nil
	}))
	require.Equal(t, 2, attempts)
	f.assertCommitted(t, before)
}

// TestReincarnationStepLifecycle_PlainObserverFailureRollsBack: on the plain
// path the real observer runs inside the write's own transaction (it bumps
// the cursor and inserts the outbox row) and then fails, so the agent write
// and everything the observer wrote roll back: no outbox row, no cursor or
// outbox-seq advance, no enqueue count.
func TestReincarnationStepLifecycle_PlainObserverFailureRollsBack(t *testing.T) {
	f := newReincStepFixture(t, "reinc-lc-plainobs")
	before := f.state(t)
	inner, err := lifecycleevents.NewObserver(lifecycleevents.ObserverConfig{
		HubID: f.srv.HubID(), ProducerBuild: "test", Counters: f.counters,
	})
	require.NoError(t, err)
	obs := &failAfterObserver{inner: inner}
	f.ls.SetAgentWriteObserver(obs)
	// Restore a plain observer when the test ends; nothing below writes
	// through it.
	t.Cleanup(func() { f.ls.SetAgentWriteObserver(inner) })
	// One attempt: the injected error is not a version conflict, so the
	// step never retries.
	err = f.step(1, nil)
	require.ErrorIs(t, err, errInjectedReincStep)
	require.Greater(t, obs.sawRev, before.transitionRev,
		"the real observer bumped the cursor inside the write's transaction before the failure")
	f.assertNothingCommitted(t, before)
}
