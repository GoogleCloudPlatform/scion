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
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/agentlifecyclecursor"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/labels"
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

// TestLifecycleStore_LabelRoundTripAtAPILimits stores labels.MaxLabels
// labels whose keys and values are 63 bytes of printable ASCII, including
// the characters JSON escapes, and requires the created event to carry
// them exactly.
func TestLifecycleStore_LabelRoundTripAtAPILimits(t *testing.T) {
	f := newLifecycleFixture(t)
	const alphabet = "\"\\/<>&'.:-_ ~!@#$%^*()[]{}|?=+,;`"
	fill := func(prefix string, n int) string {
		s := prefix
		for i := 0; len(s) < n; i++ {
			s += string(alphabet[i%len(alphabet)])
		}
		return s[:n]
	}
	in := map[string]string{}
	for i := 0; i < labels.MaxLabels; i++ {
		in[fill(fmt.Sprintf("k%02d", i), labels.MaxKeyLen)] = fill(fmt.Sprintf("v%02d", i), labels.MaxValueLen)
	}
	require.NoError(t, labels.Validate(in), "the label set must be API-valid")
	a := f.create("lc-label-limits", func(a *store.Agent) { a.Labels = in })
	evs, _ := lifecycleEvents(t, f.cs, a.ID)
	require.Len(t, evs, 1)
	assert.Equal(t, in, evs[0].Metadata.Labels.Map(), "labels round trip exactly at the API limits")
}

// panickingObserver panics inside the writer's transaction.
type panickingObserver struct{ value any }

func (o panickingObserver) ObserveAgentWrite(context.Context, store.LifecycleTx, store.AgentWrite) error {
	panic(o.value)
}

// TestLifecycleStore_CreateAgentPanicRollsBack: a panic inside CreateAgent's
// own transaction (observer installed, no ambient transaction) propagates
// unchanged and rolls everything back. The transaction is rolled back by
// default unless the success path commits it, so the agent row can never
// commit without its outbox rows (wp1s1-ci-fix R1).
func TestLifecycleStore_CreateAgentPanicRollsBack(t *testing.T) {
	f := newLifecycleFixture(t)
	ctx := context.Background()
	before := lifecycleSnapshot(t, f.cs)
	sentinel := &struct{ name string }{"observer panic"}
	f.cs.SetAgentWriteObserver(panickingObserver{value: sentinel})
	a := makeAgent(f.projectID, "lc-panic")
	var got any
	func() {
		defer func() { got = recover() }()
		_ = f.cs.CreateAgent(ctx, a)
	}()
	require.Same(t, sentinel, got, "the original panic value is re-panicked")
	_, err := f.cs.GetAgent(ctx, a.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "the agent row must not commit")
	assert.Equal(t, before, lifecycleSnapshot(t, f.cs), "no outbox row or cursor may move")

	// The store stays usable afterwards (the transaction was released).
	f.cs.SetAgentWriteObserver(f.obs)
	b := f.create("lc-after-panic")
	evs, _ := lifecycleEvents(t, f.cs, b.ID)
	assert.Equal(t, "created@1", lcSeq(evs))
}

// TestLifecycleWriteTx_PassThroughAndOwnTx: in an ambient transaction, or
// with no observer, lifecycleWriteTx writes through the store's own client
// and transaction; outside a transaction with an observer it opens a new one.
func TestLifecycleWriteTx_PassThroughAndOwnTx(t *testing.T) {
	f := newLifecycleFixture(t)
	ctx := context.Background()
	as := f.cs.AgentStore

	client, tx, done, err := as.lifecycleWriteTx(ctx, nil)
	require.NoError(t, err)
	done()
	assert.Same(t, as.client, client, "no observer: the store's own client")
	assert.Nil(t, tx)

	require.NoError(t, f.cs.WithTx(ctx, func(txs store.Store) error {
		inner := txs.(*CompositeStore).AgentStore
		c, x, d, err := inner.lifecycleWriteTx(ctx, f.obs)
		require.NoError(t, err)
		d()
		assert.Same(t, inner.client, c, "ambient transaction: its client")
		assert.Same(t, inner.entTx, x, "ambient transaction: no new one")
		require.NotNil(t, x)
		return nil
	}))

	client, tx, done, err = as.lifecycleWriteTx(ctx, f.obs)
	require.NoError(t, err)
	assert.NotNil(t, tx, "outside a transaction with an observer: a new one")
	assert.NotSame(t, as.client, client)
	done() // rolled back unless committed
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

// lifecycleSnapshot is the lifecycle table state a failed write must not
// change: the outbox row count and every cursor's revisions, digest and
// fingerprint.
func lifecycleSnapshot(t *testing.T, cs *CompositeStore) string {
	t.Helper()
	ctx := context.Background()
	n, err := cs.client.AgentLifecycleOutbox.Query().Count(ctx)
	require.NoError(t, err)
	cursors, err := cs.client.AgentLifecycleCursor.Query().Order(func(s *entsql.Selector) { s.OrderBy(s.C(agentlifecyclecursor.FieldID)) }).All(ctx)
	require.NoError(t, err)
	var b strings.Builder
	fmt.Fprintf(&b, "outbox=%d", n)
	for _, c := range cursors {
		fmt.Fprintf(&b, " %s:%d:%d:%s:%s:%v", c.ID, c.TransitionRevision, c.MetadataRevision, c.MetadataDigest, c.LastFingerprint, c.LastDeleted)
	}
	return b.String()
}

// TestLifecycleStore_ObserverErrorRollsBackEveryWriter is the per-writer
// atomicity test of design section 8: for every observed entry point, an
// observer failure (a cursor or outbox statement error) surfaces as the
// write's error, the product row is unchanged, and no lifecycle row or
// cursor moves.
func TestLifecycleStore_ObserverErrorRollsBackEveryWriter(t *testing.T) {
	ctx := context.Background()
	type check func(t *testing.T, f *lifecycleFixture, a *store.Agent)
	present := func(t *testing.T, f *lifecycleFixture, a *store.Agent) {
		got, err := f.cs.GetAgent(ctx, a.ID)
		require.NoError(t, err, "the product row must survive the rolled-back write")
		assert.True(t, got.DeletedAt.IsZero(), "and stay live")
	}
	softDeleted := func(t *testing.T, f *lifecycleFixture, a *store.Agent) {
		got, err := f.cs.GetAgent(ctx, a.ID)
		require.NoError(t, err, "the soft-deleted row must survive the rolled-back purge")
		assert.False(t, got.DeletedAt.IsZero())
	}
	absent := func(t *testing.T, f *lifecycleFixture, a *store.Agent) {
		_, err := f.cs.GetAgent(ctx, a.ID)
		assert.ErrorIs(t, err, store.ErrNotFound, "the create must roll back")
	}
	cases := []struct {
		name  string
		setup func(f *lifecycleFixture) *store.Agent // runs with the real observer
		write func(f *lifecycleFixture, a *store.Agent) error
		after check
	}{
		{"CreateAgent in WithTx",
			func(f *lifecycleFixture) *store.Agent { return makeAgent(f.projectID, "rb-create-tx") },
			func(f *lifecycleFixture, a *store.Agent) error {
				return f.cs.WithTx(ctx, func(tx store.Store) error { return tx.CreateAgent(ctx, a) })
			}, absent},
		{"CreateAgent outside a transaction",
			func(f *lifecycleFixture) *store.Agent { return makeAgent(f.projectID, "rb-create-notx") },
			func(f *lifecycleFixture, a *store.Agent) error { return f.cs.CreateAgent(ctx, a) }, absent},
		{"soft finalize",
			func(f *lifecycleFixture) *store.Agent { return f.create("rb-soft") },
			func(f *lifecycleFixture, a *store.Agent) error {
				now := time.Now()
				_, err := f.cs.FinalizeAgentDeletion(ctx, a.ID, store.DeletionPredicate{}, store.DeletionFinalizeSoft, store.DeletionFields{DeletedAt: &now}, nil)
				return err
			}, present},
		{"hard finalize",
			func(f *lifecycleFixture) *store.Agent { return f.create("rb-hard") },
			func(f *lifecycleFixture, a *store.Agent) error {
				_, err := f.cs.FinalizeAgentDeletion(ctx, a.ID, store.DeletionPredicate{}, store.DeletionFinalizeHard, store.DeletionFields{}, nil)
				return err
			}, present},
		{"DeleteAgent",
			func(f *lifecycleFixture) *store.Agent { return f.create("rb-delete") },
			func(f *lifecycleFixture, a *store.Agent) error { return f.cs.DeleteAgent(ctx, a.ID) }, present},
		{"purge",
			func(f *lifecycleFixture) *store.Agent {
				// Soft delete with the observer off, so the purge takes the
				// r6 N2 path (gap + resynced + deleted) and the rollback
				// covers real event rows. (The purge calls the observer for
				// every removed row in any case.)
				a := f.create("rb-purge")
				f.cs.SetAgentWriteObserver(nil)
				f.finalize(ctx, a.ID, store.DeletionFinalizeSoft)
				return a
			},
			func(f *lifecycleFixture, a *store.Agent) error {
				_, err := f.cs.PurgeDeletedAgentsByID(ctx, []string{a.ID}, time.Now().Add(time.Hour))
				return err
			}, softDeleted},
		{"DeleteProject",
			func(f *lifecycleFixture) *store.Agent { return f.create("rb-project") },
			func(f *lifecycleFixture, a *store.Agent) error { return f.cs.DeleteProject(ctx, f.projectID) },
			func(t *testing.T, f *lifecycleFixture, a *store.Agent) {
				present(t, f, a)
				_, err := f.cs.GetProject(ctx, f.projectID)
				require.NoError(t, err, "the project must survive the rolled-back delete")
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newLifecycleFixture(t)
			a := c.setup(f)
			before := lifecycleSnapshot(t, f.cs)
			f.cs.SetAgentWriteObserver(failingObserver{err: errInjectedLC})
			require.ErrorIs(t, c.write(f, a), errInjectedLC, "the observer failure must surface")
			c.after(t, f, a)
			assert.Equal(t, before, lifecycleSnapshot(t, f.cs), "no outbox row or cursor may move")
		})
	}
}

// TestLifecycleStore_ObserverSwapDuringWrites is the r2 N11 test: one
// goroutine installs and removes the observer while others create and
// delete agents. Every write succeeds, every outbox row decodes, and each
// agent's revisions are dense from 1, except for explicit gap markers
// (a skipped revision followed by resynced(unobserved_change)). Each write
// loads the observer once and keeps it through commit, so a swap never
// tears a write. The gate runs it under -race.
func TestLifecycleStore_ObserverSwapDuringWrites(t *testing.T) {
	f := newLifecycleFixture(t)
	ctx := context.Background()
	stop := make(chan struct{})
	var swaps sync.WaitGroup
	swaps.Add(1)
	go func() {
		defer swaps.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				f.cs.SetAgentWriteObserver(f.obs)
				return
			default:
			}
			if i%2 == 0 {
				f.cs.SetAgentWriteObserver(nil)
			} else {
				f.cs.SetAgentWriteObserver(f.obs)
			}
			time.Sleep(time.Millisecond)
		}
	}()
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 6; i++ {
				a := makeAgent(f.projectID, fmt.Sprintf("swap-%d-%d", w, i))
				if err := f.cs.CreateAgent(ctx, a); err != nil {
					errs <- err
					continue
				}
				if err := f.cs.DeleteAgent(ctx, a.ID); err != nil {
					errs <- err
				}
			}
		}(w)
	}
	wg.Wait()
	close(stop)
	swaps.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("a write failed during observer swaps: %v", err)
	}
	evs, _ := lifecycleEvents(t, f.cs, "") // decodes every row
	byAgent := map[string][]*tc.LifecycleEvent{}
	for _, e := range evs {
		byAgent[e.AgentID] = append(byAgent[e.AgentID], e)
	}
	for id, es := range byAgent {
		sort.Slice(es, func(i, j int) bool { return es[i].TransitionRevision < es[j].TransitionRevision })
		if es[0].TransitionRevision != 1 {
			t.Errorf("agent %s: first revision %d, want 1: %s", id, es[0].TransitionRevision, lcSeq(es))
		}
		for i := 1; i < len(es); i++ {
			step := es[i].TransitionRevision - es[i-1].TransitionRevision
			gapMarker := step == 2 && es[i].Transition == tc.TransitionResynced && es[i].Cause == tc.CauseUnobservedChange
			if step != 1 && !gapMarker {
				t.Errorf("agent %s: revisions not dense and no gap marker: %s", id, lcSeq(es))
			}
		}
	}
	t.Logf("%d events for %d agents", len(evs), len(byAgent))
}

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
// (it runs with the observer off), and the hard delete then emits a gap,
// resynced and deleted.
func TestLifecycleStore_UnobservedWriteThenHardFinalize(t *testing.T) {
	f := newLifecycleFixture(t)
	a := f.create("lc-gap")
	got, err := f.cs.GetAgent(context.Background(), a.ID)
	require.NoError(t, err)
	got.Phase = "stopped"
	f.cs.SetAgentWriteObserver(nil)
	require.NoError(t, f.cs.UpdateAgent(context.Background(), got))
	f.cs.SetAgentWriteObserver(f.obs)
	f.finalize(context.Background(), a.ID, store.DeletionFinalizeHard)
	evs, _ := lifecycleEvents(t, f.cs, a.ID)
	require.Equal(t, "created@1 resynced@3(unobserved_change) deleted@4(hard)", lcSeq(evs))
	require.NotNil(t, evs[1].Gap)
	assert.True(t, *evs[1].Gap)
}

// TestReconcileHarnessConfigColumnCausesNoResync is design r2 N12: the
// boot reconcile rewrites the denormalized harness_config column (an
// exempt writer), and since the snapshot reads applied_config, not the
// column, the next observed write finds the fingerprint unchanged.
func TestReconcileHarnessConfigColumnCausesNoResync(t *testing.T) {
	f := newLifecycleFixture(t)
	ctx := context.Background()
	a := f.create("lc-reconcile", func(a *store.Agent) {
		a.AppliedConfig = &store.AgentAppliedConfig{HarnessConfig: "claude"}
	})
	// Make the column look like a row written before the column existed.
	_, err := f.cs.client.Agent.UpdateOneID(uuid.MustParse(a.ID)).ClearHarnessConfig().Save(ctx)
	require.NoError(t, err)
	require.NoError(t, f.cs.ReconcileHarnessConfigColumn(ctx))
	got, err := f.cs.client.Agent.Get(ctx, uuid.MustParse(a.ID))
	require.NoError(t, err)
	require.Equal(t, "claude", got.HarnessConfig, "the reconcile rewrote the column")
	f.finalize(ctx, a.ID, store.DeletionFinalizeHard)
	evs, _ := lifecycleEvents(t, f.cs, a.ID)
	assert.Equal(t, "created@1 deleted@2(hard)", lcSeq(evs), "no resynced after the reconcile")
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
	assert.Equal(t, "created@1 resynced@3(unobserved_change) deleted@4(purged)", lcSeq(evs))
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
	// Restore is UpdateAgent clearing deleted_at (observed since slice 2).
	got.DeletedAt = time.Time{}
	require.NoError(t, f.cs.UpdateAgent(context.Background(), got))
	f.finalize(context.Background(), b.ID, store.DeletionFinalizeHard)
	evs, _ = lifecycleEvents(t, f.cs, b.ID)
	assert.Equal(t, "created@1 deleted@2(soft) restored@3 deleted@4(hard)", lcSeq(evs))
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
// writes on distinct agents commit dense per-agent revisions (same-agent
// races: TestLifecycleStore_SameAgentConcurrentWriters).
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

// TestLifecycleStore_SameAgentConcurrentWriters is the design section 8
// same-agent concurrency test: every slice-1 observed writer races on ONE
// agent (soft finalize, hard finalize, DeleteAgent and the purge), for
// several agents at once. Whatever the commit order, each agent gets
// created(1) and exactly one deleted(2): dense revisions in commit order,
// no resynced, no write rolled back by a cursor conflict, and no cursor
// left behind. On PostgreSQL (integration build) this exercises the agent
// row locks that serialize the observers; on SQLite the single writer.
func TestLifecycleStore_SameAgentConcurrentWriters(t *testing.T) {
	f := newLifecycleFixture(t)
	ctx := context.Background()
	const agents = 6
	ids := make([]string, agents)
	for i := range ids {
		ids[i] = f.create(fmt.Sprintf("lc-same-%d", i)).ID
	}
	live := store.DeletionPredicate{DeletedAtNull: true}
	var wg sync.WaitGroup
	errs := make(chan error, agents*4)
	for _, id := range ids {
		id := id
		writers := []func() error{
			func() error {
				now := time.Now()
				_, err := f.cs.FinalizeAgentDeletion(ctx, id, live, store.DeletionFinalizeSoft, store.DeletionFields{DeletedAt: &now}, nil)
				return err
			},
			func() error {
				_, err := f.cs.FinalizeAgentDeletion(ctx, id, live, store.DeletionFinalizeHard, store.DeletionFields{}, nil)
				return err
			},
			func() error {
				err := f.cs.DeleteAgent(ctx, id)
				if errors.Is(err, store.ErrNotFound) {
					return nil // another writer removed it first
				}
				return err
			},
			func() error {
				_, err := f.cs.PurgeDeletedAgentsByID(ctx, []string{id}, time.Now().Add(time.Hour))
				return err
			},
		}
		for _, w := range writers {
			wg.Add(1)
			go func(w func() error) {
				defer wg.Done()
				if err := w(); err != nil {
					errs <- err
				}
			}(w)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("a concurrent writer failed (a cursor conflict would roll a product write back): %v", err)
	}
	for _, id := range ids {
		evs, _ := lifecycleEvents(t, f.cs, id)
		require.Len(t, evs, 2, "agent %s: %s", id, lcSeq(evs))
		assert.Equal(t, tc.TransitionCreated, evs[0].Transition)
		assert.Equal(t, int64(1), evs[0].TransitionRevision)
		assert.Equal(t, tc.TransitionDeleted, evs[1].Transition, "exactly one deleted: %s", lcSeq(evs))
		assert.Equal(t, int64(2), evs[1].TransitionRevision, "dense revisions: %s", lcSeq(evs))
		assert.Contains(t, []string{tc.DeleteModeSoft, tc.DeleteModeHard}, evs[1].DeleteMode)
		assert.False(t, cursorExists(t, f.cs, id), "agent %s: the row is gone, so is its cursor", id)
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
	has, err := f.cs.LifecycleOutboxHasRows(ctx)
	require.NoError(t, err)
	assert.False(t, has, "empty outbox")
	f.create("lc-ob-4")
	has, err = f.cs.LifecycleOutboxHasRows(ctx)
	require.NoError(t, err)
	assert.True(t, has)
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
