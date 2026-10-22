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
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/agentlifecyclecursor"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/lifecycleevents"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	tc "github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetryemit"
)

// lifecycleFixture is a composite store with the lifecycle observer
// installed and one project.
type lifecycleFixture struct {
	t         *testing.T
	cs        *CompositeStore
	obs       *lifecycleevents.Observer
	counters  *telemetryemit.MemoryCounters
	projectID string
}

func newLifecycleObserver(t *testing.T, counters telemetryemit.LifecycleCounters) *lifecycleevents.Observer {
	t.Helper()
	obs, err := lifecycleevents.NewObserver(lifecycleevents.ObserverConfig{
		HubID: "hub-test", ProducerBuild: "test", Counters: counters,
	})
	require.NoError(t, err)
	return obs
}

func newLifecycleFixtureOn(t *testing.T, cs *CompositeStore) *lifecycleFixture {
	t.Helper()
	f := &lifecycleFixture{t: t, cs: cs, counters: telemetryemit.NewMemoryCounters()}
	f.obs = newLifecycleObserver(t, f.counters)
	f.projectID = uuid.NewString()
	require.NoError(t, cs.CreateProject(context.Background(), &store.Project{ID: f.projectID, Name: "lc", Slug: "lc-" + f.projectID[:8]}))
	cs.SetAgentWriteObserver(f.obs)
	return f
}

func newLifecycleFixture(t *testing.T) *lifecycleFixture {
	return newLifecycleFixtureOn(t, newTestCompositeStore(t))
}

// create commits an agent through WithTx with a create context, as the
// hub's commitAgentCreate does.
func (f *lifecycleFixture) create(slug string, mutate ...func(*store.Agent)) *store.Agent {
	f.t.Helper()
	a := makeAgent(f.projectID, slug)
	for _, m := range mutate {
		m(a)
	}
	ctx := lifecycleevents.WithCreateContext(context.Background(), lifecycleevents.CreateContext{CreatorType: "user", CreatorID: "u1"})
	require.NoError(f.t, f.cs.WithTx(ctx, func(tx store.Store) error { return tx.CreateAgent(ctx, a) }))
	return a
}

// finalize finalizes a's deletion in mode with no predicate.
func (f *lifecycleFixture) finalize(ctx context.Context, id string, mode store.DeletionFinalizeMode) {
	f.t.Helper()
	set := store.DeletionFields{}
	if mode == store.DeletionFinalizeSoft {
		now := time.Now()
		set.DeletedAt = &now
	}
	n, err := f.cs.FinalizeAgentDeletion(ctx, id, store.DeletionPredicate{}, mode, set, nil)
	require.NoError(f.t, err)
	require.Equal(f.t, 1, n)
}

func lifecycleEvents(t *testing.T, cs *CompositeStore, agentID string) ([]*tc.LifecycleEvent, []store.LifecycleOutboxRow) {
	t.Helper()
	rows, err := cs.ClaimLifecycleOutbox(context.Background(), "inspect-"+uuid.NewString(), time.Now().Add(time.Hour), time.Now().Add(2*time.Hour), 10000)
	require.NoError(t, err)
	// Release the inspection lease again.
	for _, r := range rows {
		require.NoError(t, cs.FailLifecycleOutboxRow(context.Background(), r.Seq, r.LeaseOwner,
			store.LifecycleOutboxFailure{NextAttemptAt: r.NextAttemptAt, LastError: r.LastError}))
	}
	var evs []*tc.LifecycleEvent
	var out []store.LifecycleOutboxRow
	for _, r := range rows {
		if agentID != "" && r.AgentID != agentID {
			continue
		}
		var ev tc.LifecycleEvent
		require.NoError(t, tc.Unmarshal(r.Payload, &ev))
		require.Equal(t, r.EventID, ev.EventID)
		require.Equal(t, r.TransitionRevision, ev.TransitionRevision)
		evs = append(evs, &ev)
		out = append(out, r)
	}
	return evs, out
}

func lcSeq(evs []*tc.LifecycleEvent) string {
	parts := make([]string, len(evs))
	for i, e := range evs {
		s := fmt.Sprintf("%s@%d", e.Transition, e.TransitionRevision)
		if e.Cause != "" {
			s += "(" + e.Cause + ")"
		}
		if e.DeleteMode != "" {
			s += "(" + e.DeleteMode + ")"
		}
		parts[i] = s
	}
	return strings.Join(parts, " ")
}

func cursorExists(t *testing.T, cs *CompositeStore, agentID string) bool {
	t.Helper()
	n, err := cs.client.AgentLifecycleCursor.Query().Where(agentlifecyclecursor.IDEQ(uuid.MustParse(agentID))).Count(context.Background())
	require.NoError(t, err)
	return n > 0
}

func TestLifecycleStore_CreateCommitsEventAndCursor(t *testing.T) {
	f := newLifecycleFixture(t)
	a := f.create("lc-create", func(a *store.Agent) {
		a.Labels = map[string]string{`p/u.n-c_t`: `< > & " \ ' :`, "empty": ""}
		a.AppliedConfig = &store.AgentAppliedConfig{Model: "m-1", ModelSize: "large", Harness: "claude", TemplateID: "t1", TemplateHash: "h1"}
	})
	evs, _ := lifecycleEvents(t, f.cs, a.ID)
	require.Equal(t, "created@1", lcSeq(evs))
	ev := evs[0]
	assert.Equal(t, "scion-hub/hub-test", ev.ProducerNamespace)
	assert.Equal(t, a.ID, ev.AgentID)
	assert.Equal(t, f.projectID, ev.ProjectID)
	assert.Equal(t, "user", ev.Metadata.CreatorPrincipalType)
	assert.Equal(t, "u1", ev.Metadata.CreatorPrincipalID)
	assert.Equal(t, "m-1", ev.Metadata.RequestedModel)
	assert.Equal(t, a.Labels, ev.Metadata.Labels.Map(), "labels round trip exactly")
	assert.True(t, cursorExists(t, f.cs, a.ID))
	assert.Equal(t, int64(1), f.counters.Get(telemetryemit.MetricEventsEnqueued, "created"), "counted after commit")
}

func TestLifecycleStore_CreateOutsideTxIsObserved(t *testing.T) {
	f := newLifecycleFixture(t)
	a := makeAgent(f.projectID, "lc-notx")
	require.NoError(t, f.cs.CreateAgent(context.Background(), a))
	evs, _ := lifecycleEvents(t, f.cs, a.ID)
	assert.Equal(t, "created@1", lcSeq(evs))
}

// TestLifecycleStore_RolledBackCreateLeavesNothing: a create whose
// transaction rolls back after CreateAgent leaves no outbox row, no cursor
// and no count.
func TestLifecycleStore_RolledBackCreateLeavesNothing(t *testing.T) {
	f := newLifecycleFixture(t)
	a := makeAgent(f.projectID, "lc-rollback")
	errLater := errors.New("a later create step failed")
	err := f.cs.WithTx(context.Background(), func(tx store.Store) error {
		require.NoError(t, tx.CreateAgent(context.Background(), a))
		return errLater
	})
	require.ErrorIs(t, err, errLater)
	evs, _ := lifecycleEvents(t, f.cs, "")
	assert.Empty(t, evs)
	assert.False(t, cursorExists(t, f.cs, a.ID))
	assert.Zero(t, f.counters.Get(telemetryemit.MetricEventsEnqueued, "created"))
}

type failingObserver struct{ err error }

func (o failingObserver) ObserveAgentWrite(context.Context, store.LifecycleTx, store.AgentWrite) error {
	return o.err
}

// TestLifecycleStore_ObserverErrorRollsBackWrite: a cursor/outbox failure
// rolls the product write back with it (crash injection before commit).
func TestLifecycleStore_ObserverErrorRollsBackWrite(t *testing.T) {
	f := newLifecycleFixture(t)
	a := f.create("lc-obs-err")
	f.cs.SetAgentWriteObserver(failingObserver{err: errInjectedLC})
	_, err := f.cs.FinalizeAgentDeletion(context.Background(), a.ID, store.DeletionPredicate{}, store.DeletionFinalizeHard, store.DeletionFields{}, nil)
	require.ErrorIs(t, err, errInjectedLC)
	_, err = f.cs.GetAgent(context.Background(), a.ID)
	require.NoError(t, err, "the delete must roll back with the observer failure")
	require.ErrorIs(t, f.cs.DeleteAgent(context.Background(), a.ID), errInjectedLC)
	_, err = f.cs.GetAgent(context.Background(), a.ID)
	require.NoError(t, err)
}

var errInjectedLC = errors.New("injected lifecycle failure")

func TestLifecycleStore_HardFinalize(t *testing.T) {
	f := newLifecycleFixture(t)
	a := f.create("lc-hard")
	f.finalize(context.Background(), a.ID, store.DeletionFinalizeHard)
	evs, _ := lifecycleEvents(t, f.cs, a.ID)
	require.Equal(t, "created@1 deleted@2(hard)", lcSeq(evs))
	assert.Equal(t, evs[0].Metadata, evs[1].Metadata, "deleted carries the full last metadata after the row is gone")
	assert.False(t, cursorExists(t, f.cs, a.ID), "cursor removed in the delete's transaction")
}

// TestLifecycleStore_UnobservedWriteThenHardFinalize is the store form of
// the done-when sequence: the create is observed, a later write is not
// (slice 1 leaves UpdateAgent unobserved), and the hard delete then
// emits a gap, resynced and deleted.
func TestLifecycleStore_UnobservedWriteThenHardFinalize(t *testing.T) {
	f := newLifecycleFixture(t)
	a := f.create("lc-gap")
	got, err := f.cs.GetAgent(context.Background(), a.ID)
	require.NoError(t, err)
	got.Phase = "stopped"
	require.NoError(t, f.cs.UpdateAgent(context.Background(), got))
	f.finalize(context.Background(), a.ID, store.DeletionFinalizeHard)
	evs, _ := lifecycleEvents(t, f.cs, a.ID)
	require.Equal(t, "created@1 resynced@3(unobserved_change) deleted@4(hard)", lcSeq(evs))
	require.NotNil(t, evs[1].Gap)
	assert.True(t, *evs[1].Gap)
}

func TestLifecycleStore_SoftFinalizeThenPurge(t *testing.T) {
	f := newLifecycleFixture(t)
	a := f.create("lc-soft")
	f.finalize(context.Background(), a.ID, store.DeletionFinalizeSoft)
	assert.True(t, cursorExists(t, f.cs, a.ID), "a soft delete keeps the cursor")
	n, err := f.cs.PurgeDeletedAgentsByID(context.Background(), []string{a.ID}, time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1, n)
	evs, _ := lifecycleEvents(t, f.cs, a.ID)
	assert.Equal(t, "created@1 deleted@2(soft)", lcSeq(evs), "purge of an observed soft delete emits nothing")
	assert.False(t, cursorExists(t, f.cs, a.ID), "purge removes the cursor")
}

// TestLifecycleStore_ToggleUnobservedSoftDelete is review r6 N2: the soft
// delete runs with the observer off; after it is back on, the purge
// emits gap, resynced and deleted instead of leaving the agent live
// forever downstream.
func TestLifecycleStore_ToggleUnobservedSoftDelete(t *testing.T) {
	f := newLifecycleFixture(t)
	a := f.create("lc-toggle")
	f.cs.SetAgentWriteObserver(nil)
	f.finalize(context.Background(), a.ID, store.DeletionFinalizeSoft)
	f.cs.SetAgentWriteObserver(f.obs)
	_, err := f.cs.PurgeDeletedAgentsByID(context.Background(), []string{a.ID}, time.Now().Add(time.Hour))
	require.NoError(t, err)
	evs, _ := lifecycleEvents(t, f.cs, a.ID)
	assert.Equal(t, "created@1 resynced@3(unobserved_change) deleted@4(hard)", lcSeq(evs))
	assert.False(t, cursorExists(t, f.cs, a.ID))
}

func TestLifecycleStore_DeleteAgent(t *testing.T) {
	f := newLifecycleFixture(t)
	a := f.create("lc-delete-agent")
	require.NoError(t, f.cs.DeleteAgent(context.Background(), a.ID))
	evs, _ := lifecycleEvents(t, f.cs, a.ID)
	assert.Equal(t, "created@1 deleted@2(hard)", lcSeq(evs))
	assert.False(t, cursorExists(t, f.cs, a.ID))
}

// TestLifecycleStore_DeleteOfSoftDeletedAndRestore covers N4: removing an
// already soft-deleted row emits nothing, and a restore then delete emits
// a second deleted with a new revision.
func TestLifecycleStore_DeleteOfSoftDeletedAndRestore(t *testing.T) {
	f := newLifecycleFixture(t)
	a := f.create("lc-n4")
	f.finalize(context.Background(), a.ID, store.DeletionFinalizeSoft)
	require.NoError(t, f.cs.DeleteAgent(context.Background(), a.ID))
	evs, _ := lifecycleEvents(t, f.cs, a.ID)
	assert.Equal(t, "created@1 deleted@2(soft)", lcSeq(evs))

	b := f.create("lc-n4-restore")
	f.finalize(context.Background(), b.ID, store.DeletionFinalizeSoft)
	got, err := f.cs.GetAgent(context.Background(), b.ID)
	require.NoError(t, err)
	// Restore is UpdateAgent (unobserved until slice 2).
	got.DeletedAt = time.Time{}
	require.NoError(t, f.cs.UpdateAgent(context.Background(), got))
	f.finalize(context.Background(), b.ID, store.DeletionFinalizeHard)
	evs, _ = lifecycleEvents(t, f.cs, b.ID)
	assert.Equal(t, "created@1 deleted@2(soft) resynced@4(unobserved_change) deleted@5(hard)", lcSeq(evs))
}

// TestLifecycleStore_DeleteProject covers B1: a forced project delete with
// one live and one soft-deleted agent emits exactly one
// deleted(project_deleted) with the before snapshot and removes both
// cursors; a delete over only soft-deleted agents emits nothing.
func TestLifecycleStore_DeleteProject(t *testing.T) {
	f := newLifecycleFixture(t)
	live := f.create("lc-proj-live")
	gone := f.create("lc-proj-gone")
	f.finalize(context.Background(), gone.ID, store.DeletionFinalizeSoft)
	require.NoError(t, f.cs.DeleteProject(context.Background(), f.projectID))
	evs, _ := lifecycleEvents(t, f.cs, live.ID)
	assert.Equal(t, "created@1 deleted@2(project_deleted)", lcSeq(evs))
	assert.Equal(t, evs[0].Metadata, evs[1].Metadata)
	evs, _ = lifecycleEvents(t, f.cs, gone.ID)
	assert.Equal(t, "created@1 deleted@2(soft)", lcSeq(evs))
	assert.False(t, cursorExists(t, f.cs, live.ID))
	assert.False(t, cursorExists(t, f.cs, gone.ID))

	g := newLifecycleFixture(t)
	only := g.create("lc-proj-only-soft")
	g.finalize(context.Background(), only.ID, store.DeletionFinalizeSoft)
	before, _ := lifecycleEvents(t, g.cs, "")
	require.NoError(t, g.cs.DeleteProject(context.Background(), g.projectID))
	after, _ := lifecycleEvents(t, g.cs, "")
	assert.Equal(t, len(before), len(after), "no event for soft-deleted agents")
	assert.False(t, cursorExists(t, g.cs, only.ID))
}

// TestLifecycleStore_ObserverOffChangesNothing is AC 9: with no observer
// no lifecycle rows are written.
func TestLifecycleStore_ObserverOffChangesNothing(t *testing.T) {
	cs := newTestCompositeStore(t)
	projectID := uuid.NewString()
	require.NoError(t, cs.CreateProject(context.Background(), &store.Project{ID: projectID, Name: "off", Slug: "off"}))
	a := makeAgent(projectID, "lc-off")
	require.NoError(t, cs.CreateAgent(context.Background(), a))
	_, err := cs.FinalizeAgentDeletion(context.Background(), a.ID, store.DeletionPredicate{}, store.DeletionFinalizeSoft, store.DeletionFields{}, nil)
	require.NoError(t, err)
	require.NoError(t, cs.DeleteProject(context.Background(), projectID))
	n, err := cs.client.AgentLifecycleOutbox.Query().Count(context.Background())
	require.NoError(t, err)
	assert.Zero(t, n)
	n, err = cs.client.AgentLifecycleCursor.Query().Count(context.Background())
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.False(t, cs.AgentWriteObserverInstalled())
}

// TestLifecycleStore_ConcurrentWritersDenseRevisions: concurrent observed
// writes on distinct agents and on one agent commit dense per-agent
// revisions in commit order.
func TestLifecycleStore_ConcurrentWritersDenseRevisions(t *testing.T) {
	f := newLifecycleFixture(t)
	var wg sync.WaitGroup
	ids := make([]string, 8)
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ids[i] = f.create(fmt.Sprintf("lc-conc-%d", i)).ID
		}(i)
	}
	wg.Wait()
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			assert.NoError(t, f.cs.DeleteAgent(context.Background(), id))
		}(id)
	}
	wg.Wait()
	for _, id := range ids {
		evs, _ := lifecycleEvents(t, f.cs, id)
		assert.Equal(t, "created@1 deleted@2(hard)", lcSeq(evs))
	}
}

func TestLifecycleStore_SweepOrphanCursors(t *testing.T) {
	f := newLifecycleFixture(t)
	orphan := f.create("lc-orphan")
	keep := f.create("lc-keep")
	// The hard delete runs while the observer is off: the cursor stays.
	f.cs.SetAgentWriteObserver(nil)
	require.NoError(t, f.cs.DeleteAgent(context.Background(), orphan.ID))
	require.True(t, cursorExists(t, f.cs, orphan.ID))
	n, err := f.cs.SweepOrphanLifecycleCursors(context.Background(), 500)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.False(t, cursorExists(t, f.cs, orphan.ID))
	assert.True(t, cursorExists(t, f.cs, keep.ID))
	n, err = f.cs.SweepOrphanLifecycleCursors(context.Background(), 500)
	require.NoError(t, err)
	assert.Zero(t, n, "the sweep is idempotent")
}

func TestLifecycleStore_OutboxLeaseAndTrim(t *testing.T) {
	f := newLifecycleFixture(t)
	f.create("lc-ob-1")
	f.create("lc-ob-2")
	f.create("lc-ob-3")
	ctx := context.Background()
	now := time.Now()

	rows, err := f.cs.ClaimLifecycleOutbox(ctx, "r1", now, now.Add(30*time.Second), 2)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Less(t, rows[0].Seq, rows[1].Seq, "claimed in seq order")
	other, err := f.cs.ClaimLifecycleOutbox(ctx, "r2", now, now.Add(30*time.Second), 10)
	require.NoError(t, err)
	require.Len(t, other, 1, "leased rows are not claimed twice")

	ok, err := f.cs.RenewLifecycleOutboxLease(ctx, rows[0].Seq, "r2", now.Add(time.Minute))
	require.NoError(t, err)
	assert.False(t, ok, "only the owner renews")
	ok, err = f.cs.RenewLifecycleOutboxLease(ctx, rows[0].Seq, "r1", now.Add(time.Minute))
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = f.cs.CompleteLifecycleOutboxRow(ctx, rows[0].Seq, "r2")
	require.NoError(t, err)
	assert.False(t, ok, "a non-owner cannot complete")
	ok, err = f.cs.CompleteLifecycleOutboxRow(ctx, rows[0].Seq, "r1")
	require.NoError(t, err)
	assert.True(t, ok)

	require.NoError(t, f.cs.FailLifecycleOutboxRow(ctx, rows[1].Seq, "r1", store.LifecycleOutboxFailure{
		CountAttempt: true, Permanent: true, NextAttemptAt: now.Add(-time.Second), LastError: strings.Repeat("é", 300),
	}))
	got, err := f.cs.client.AgentLifecycleOutbox.Get(ctx, rows[1].Seq)
	require.NoError(t, err)
	assert.Equal(t, 1, got.Attempts)
	assert.Equal(t, 1, got.PermanentFailures)
	assert.Empty(t, got.LeaseOwner)
	assert.Nil(t, got.LeaseUntil)
	assert.LessOrEqual(t, len(got.LastError), 256)

	// Trim: r2 still leases its row; the poison row (threshold 1 here) and
	// nothing else goes; then overflow trims the oldest unleased row.
	trim, err := f.cs.TrimLifecycleOutbox(ctx, now, 72*time.Hour, 100, 1)
	require.NoError(t, err)
	require.Len(t, trim.Poison, 1)
	assert.Equal(t, rows[1].EventID, trim.Poison[0].EventID)
	assert.Empty(t, trim.Age)
	assert.Empty(t, trim.Overflow)

	trim, err = f.cs.TrimLifecycleOutbox(ctx, now.Add(73*time.Hour), 72*time.Hour, 100, 5)
	require.NoError(t, err)
	assert.Len(t, trim.Age, 1, "an expired lease no longer protects the row")

	st, err := f.cs.LifecycleOutboxStats(ctx)
	require.NoError(t, err)
	assert.Zero(t, st.Pending)
}

func TestLifecycleStore_TrimOverflowSkipsLeased(t *testing.T) {
	f := newLifecycleFixture(t)
	for i := 0; i < 3; i++ {
		f.create(fmt.Sprintf("lc-ov-%d", i))
	}
	ctx := context.Background()
	now := time.Now()
	leased, err := f.cs.ClaimLifecycleOutbox(ctx, "r1", now, now.Add(time.Minute), 1)
	require.NoError(t, err)
	trim, err := f.cs.TrimLifecycleOutbox(ctx, now, 72*time.Hour, 1, 5)
	require.NoError(t, err)
	require.Len(t, trim.Overflow, 2)
	for _, d := range trim.Overflow {
		assert.NotEqual(t, leased[0].Seq, d.Seq, "a leased row is never trimmed")
	}
	st, err := f.cs.LifecycleOutboxStats(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), st.Pending)
	require.NotNil(t, st.Oldest)
	assert.Equal(t, leased[0].Seq, st.MaxSeq)
}

// --- Durable crash tests on a file-backed SQLite store (design 8, N6) -----

func openFileStore(t *testing.T, path string) *CompositeStore {
	t.Helper()
	client, err := entc.OpenSQLite("file:"+path, entc.PoolConfig{MaxOpenConns: 1})
	require.NoError(t, err)
	require.NoError(t, entc.AutoMigrate(context.Background(), client))
	return NewCompositeStore(client)
}

// completeFailingStore loses every delete after delivery: the "process
// stopped after the sink acknowledged and before the row was deleted"
// crash point.
type completeFailingStore struct{ *CompositeStore }

func (s completeFailingStore) CompleteLifecycleOutboxRow(context.Context, int64, string) (bool, error) {
	return false, errInjectedLC
}

func drainAll(t *testing.T, st store.LifecycleOutboxStore, sink telemetryemit.Sink) {
	t.Helper()
	d, err := lifecycleevents.NewDrainer(lifecycleevents.DrainerConfig{Store: st, Sink: sink})
	require.NoError(t, err)
	_, err = d.DrainOnce(context.Background())
	require.NoError(t, err)
}

func storedPayloads(t *testing.T, cs *CompositeStore) [][]byte {
	t.Helper()
	_, rows := lifecycleEvents(t, cs, "")
	out := make([][]byte, len(rows))
	for i, r := range rows {
		out[i] = r.Payload
	}
	return out
}

func TestLifecycleDurable_ReopenDeliversIdenticalBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	cs := openFileStore(t, path)
	f := newLifecycleFixtureOn(t, cs)
	a := f.create("lc-durable")
	f.finalize(context.Background(), a.ID, store.DeletionFinalizeHard)
	// Project delete with a live agent, for the bulk path.
	b := f.create("lc-durable-proj")
	require.NoError(t, cs.DeleteProject(context.Background(), f.projectID))
	want := storedPayloads(t, cs)
	require.Len(t, want, 4)
	require.NoError(t, cs.Close())

	// (i) The process died before draining; a fresh drainer delivers.
	cs2 := openFileStore(t, path)
	sink := telemetryemit.NewMemorySink()
	drainAll(t, cs2, sink)
	got := sink.Delivered()
	require.Len(t, got, 4)
	for i := range want {
		assert.True(t, bytes.Equal(want[i], got[i]), "payload %d differs after reopen", i)
	}
	evs, _ := lifecycleEvents(t, cs2, "")
	assert.Empty(t, evs, "delivered rows are deleted")
	_ = b
	require.NoError(t, cs2.Close())
}

func TestLifecycleDurable_LostDeleteRedeliversIdenticalBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	cs := openFileStore(t, path)
	f := newLifecycleFixtureOn(t, cs)
	a := f.create("lc-durable-2")
	f.finalize(context.Background(), a.ID, store.DeletionFinalizeHard)
	want := storedPayloads(t, cs)

	// (ii) The sink acknowledges, then the process stops before the delete.
	first := telemetryemit.NewMemorySink()
	drainAll(t, completeFailingStore{cs}, first)
	require.Len(t, first.Delivered(), 2)
	require.NoError(t, cs.Close())

	cs2 := openFileStore(t, path)
	// The old leases expire with the process; a new drainer re-delivers.
	_, err := cs2.client.AgentLifecycleOutbox.Update().ClearLeaseUntil().SetLeaseOwner("").Save(context.Background())
	require.NoError(t, err)
	second := telemetryemit.NewMemorySink()
	drainAll(t, cs2, second)
	got := second.Delivered()
	require.Len(t, got, 2)
	for i := range want {
		assert.True(t, bytes.Equal(want[i], got[i]), "redelivered payload %d differs", i)
		assert.True(t, bytes.Equal(first.Delivered()[i], got[i]))
	}
	require.NoError(t, cs2.Close())
}

func TestLifecycleDurable_RolledBackWriteLeavesNoRowAfterReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	cs := openFileStore(t, path)
	f := newLifecycleFixtureOn(t, cs)
	a := makeAgent(f.projectID, "lc-durable-rb")
	_ = cs.WithTx(context.Background(), func(tx store.Store) error {
		require.NoError(t, tx.CreateAgent(context.Background(), a))
		return errInjectedLC
	})
	require.NoError(t, cs.Close())
	cs2 := openFileStore(t, path)
	evs, _ := lifecycleEvents(t, cs2, "")
	assert.Empty(t, evs)
	assert.False(t, cursorExists(t, cs2, a.ID))
	require.NoError(t, cs2.Close())
}
