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
	"go/ast"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Broker reads shared within one re-check pass (conduitBrokerMemo): a
// sweep or a resync reads each Runtime Broker at most once; notify
// re-checks and renewals read fresh; every verdict is decided from the
// stream's own agent row.

// countingBrokerStore counts GetRuntimeBroker reads; while fail is set it
// returns that error.
type countingBrokerStore struct {
	store.Store
	reads atomic.Int64
	fail  atomic.Pointer[error]
}

func (s *countingBrokerStore) GetRuntimeBroker(ctx context.Context, id string) (*store.RuntimeBroker, error) {
	s.reads.Add(1)
	if err := s.fail.Load(); err != nil {
		return nil, *err
	}
	return s.Store.GetRuntimeBroker(ctx, id)
}

// newMemoRecheckFixture is a recheck fixture with no periodic sweep (the
// test runs passes itself), every action allowed (so each stream reaches
// the placement guard), and broker reads counted.
func newMemoRecheckFixture(t *testing.T) (*recheckFixture, *countingBrokerStore) {
	t.Helper()
	f := newRecheckFixture(t, nil, -1, func(f *recheckFixture) {
		f.srv.conduitAuthorizeActionTestHook = func(context.Context, Identity, *store.Agent, Action) error { return nil }
	})
	counter, _ := installStoreFault(t, f.srv, func(inner store.Store, _ *storeFaultSwitch) *countingBrokerStore {
		return &countingBrokerStore{Store: inner}
	})
	return f, counter
}

// memoAgent creates an agent in the fixture's project on broker.
func (f *recheckFixture) memoAgent(t *testing.T, slug, broker string) *store.Agent {
	t.Helper()
	a := &store.Agent{
		ID: tid(slug), Slug: slug, Name: slug, ProjectID: f.agent.ProjectID, OwnerID: f.agent.OwnerID,
		Ancestry: f.agent.Ancestry, RuntimeBrokerID: broker, Phase: string(state.PhaseRunning),
	}
	require.NoError(t, f.store.CreateAgent(context.Background(), a))
	return a
}

// memoTrack tracks a PTY stream of u1 to agent and returns its watch.
func (f *recheckFixture) memoTrack(t *testing.T, agent *store.Agent, id uint32) *streamWatch {
	t.Helper()
	w := &streamWatch{closed: make(chan string, 1)}
	untrack := f.srv.trackConduitUserStream(&conduitUserStream{
		Kind: grant.StreamKindPTY, Identity: f.u1, AgentID: agent.ID, ProjectID: agent.ProjectID,
		SessionID: "sess-memo", StreamID: id, Close: w.close,
	})
	t.Cleanup(untrack)
	return w
}

// count returns how often want was recorded.
func (m *metricWaiter) count(want string) int {
	n := 0
	for _, s := range m.list() {
		if s == want {
			n++
		}
	}
	return n
}

// TestConduitPlacementMemo_OneBrokerReadPerSweep (T1): a sweep reads each
// Runtime Broker once, however many streams and agents use it, and every
// stream gets its verdict.
func TestConduitPlacementMemo_OneBrokerReadPerSweep(t *testing.T) {
	f, counter := newMemoRecheckFixture(t)
	require.NotNil(t, f.a.cfg.SweepContext, "the user-stream re-check shares broker reads per sweep")
	sameBroker := f.memoAgent(t, "memo-same-broker", f.agent.RuntimeBrokerID)
	otherBroker := f.memoAgent(t, "memo-other-broker", "broker-2")
	watches := []*streamWatch{
		f.memoTrack(t, f.agent, 1), f.memoTrack(t, f.agent, 2), f.memoTrack(t, f.agent, 3),
		f.memoTrack(t, sameBroker, 4), f.memoTrack(t, otherBroker, 5),
	}

	f.a.runSweep()
	assert.Equal(t, int64(2), counter.reads.Load(), "one read for each of the two brokers")
	assert.Equal(t, 5, f.metrics.count(conduitAuthzTriggerSweep+"/"+conduitAuthzOutcomePassed+"/"+grant.StreamKindPTY))
	for _, w := range watches {
		w.assertOpen(t)
	}

	f.a.runSweep()
	assert.Equal(t, int64(4), counter.reads.Load(), "nothing survives a pass: the next sweep reads again")
}

// TestConduitPlacementMemo_PlacementChangeSeenNextPass (T2): a placement
// change between sweeps is seen by the next sweep; an inconsistent saved
// placement is unavailable for every stream, never a close (T5).
func TestConduitPlacementMemo_PlacementChangeSeenNextPass(t *testing.T) {
	f, counter := newMemoRecheckFixture(t)
	w1, w2, w3 := f.memoTrack(t, f.agent, 1), f.memoTrack(t, f.agent, 2), f.memoTrack(t, f.agent, 3)

	f.a.runSweep()
	require.Equal(t, 3, f.metrics.count(conduitAuthzTriggerSweep+"/"+conduitAuthzOutcomePassed+"/"+grant.StreamKindPTY))

	pinInconsistently(t, f.conduitFixture)
	f.a.runSweep()
	assert.Equal(t, int64(2), counter.reads.Load(), "one read per pass")
	assert.Equal(t, 3, f.metrics.count(conduitAuthzTriggerSweep+"/"+conduitAuthzOutcomeDeferred+"/"+grant.StreamKindPTY),
		"each stream is unavailable for the inconsistent placement")
	for _, w := range []*streamWatch{w1, w2, w3} {
		w.assertOpen(t)
	}
}

// TestConduitPlacementMemo_StreamsDecideFromTheirOwnAgentRow: within one
// pass a stream is decided from its own agent row, read fresh, with the
// shared broker read: a pin that changes mid-pass applies to the streams
// checked after it.
func TestConduitPlacementMemo_StreamsDecideFromTheirOwnAgentRow(t *testing.T) {
	f, counter := newMemoRecheckFixture(t)
	st := func(id uint32) *conduitUserStream {
		return &conduitUserStream{
			Kind: grant.StreamKindPTY, Identity: f.u1, UserID: f.u1.ID(), AgentID: f.agent.ID, ProjectID: f.agent.ProjectID,
			SessionID: "sess-memo", StreamID: id,
		}
	}
	pass := withConduitBrokerMemo(context.Background())
	verdict, _ := f.srv.checkConduitUserStream(pass, st(1))
	require.Equal(t, conduitAuthzAllowed, verdict)

	pinInconsistently(t, f.conduitFixture)
	verdict, reason := f.srv.checkConduitUserStream(pass, st(2))
	assert.Equal(t, conduitAuthzUnavailable, verdict, "the later stream sees the agent's new pin")
	assert.Equal(t, "agent placement inconsistent", reason)
	assert.Equal(t, int64(1), counter.reads.Load(), "the broker was read once for the pass")
}

// TestConduitPlacementMemo_HintsAndRenewalsReadFresh (T3, T4): a notify
// re-check reads the broker for every stream; a resync is a pass of its
// own and shares its reads; renewals read fresh.
func TestConduitPlacementMemo_HintsAndRenewalsReadFresh(t *testing.T) {
	ctx := context.Background()

	t.Run("notify re-check reads fresh", func(t *testing.T) {
		f, counter := newMemoRecheckFixture(t)
		f.memoTrack(t, f.agent, 1)
		f.memoTrack(t, f.agent, 2)
		f.memoTrack(t, f.agent, 3)
		f.a.Recheck(ctx, conduitAuthzTriggerNotify, conduitAuthzMatch{})
		assert.Equal(t, int64(3), counter.reads.Load())
	})

	t.Run("resync shares reads within its pass", func(t *testing.T) {
		f, counter := newMemoRecheckFixture(t)
		f.memoTrack(t, f.agent, 1)
		f.memoTrack(t, f.agent, 2)
		f.memoTrack(t, f.agent, 3)
		f.srv.bindConduitAuthzEvents()
		assert.Equal(t, int64(1), counter.reads.Load(), "the resync on (re)binding is one pass")
		assert.Equal(t, 3, f.metrics.count(conduitAuthzTriggerResync+"/"+conduitAuthzOutcomePassed+"/"+grant.StreamKindPTY))
	})

	t.Run("renewals read fresh", func(t *testing.T) {
		f := newRenewalFixture(t)
		tok, open := f.openGrant(t, grant.ViaPTY, renewalPTYHeader(), nil)
		counter, _ := installStoreFault(t, f.srv, func(inner store.Store, _ *storeFaultSwitch) *countingBrokerStore {
			return &countingBrokerStore{Store: inner}
		})
		_, err := f.renew(tok, open, 7)
		require.NoError(t, err)
		_, err = f.renew(tok, open, 7)
		require.NoError(t, err)
		assert.Equal(t, int64(2), counter.reads.Load())
	})
}

// TestConduitPlacementMemo_FailedReadIsUnavailable (T6): a broker read
// that fails is unavailable for a pinned agent's streams and closes
// nothing.
func TestConduitPlacementMemo_FailedReadIsUnavailable(t *testing.T) {
	f, counter := newMemoRecheckFixture(t)
	pinInconsistently(t, f.conduitFixture)
	w1, w2 := f.memoTrack(t, f.agent, 1), f.memoTrack(t, f.agent, 2)
	boom := errors.New("injected broker read failure")
	counter.fail.Store(&boom)

	f.a.runSweep()
	assert.Equal(t, 2, f.metrics.count(conduitAuthzTriggerSweep+"/"+conduitAuthzOutcomeDeferred+"/"+grant.StreamKindPTY))
	w1.assertOpen(t)
	w2.assertOpen(t)
	n := counter.reads.Load()
	assert.True(t, n >= 1 && n <= 2, "a failed read is not stored: a stream checked after it reads again (%d reads)", n)
}

// TestConduitBrokerMemo_SharedReads (T7): the memo itself. Concurrent
// reads of one broker share the first; a definite answer (a row or
// store.ErrNotFound) is kept for the pass; a failed read is given to its
// waiters and not kept; a waiter whose own context ends stops waiting.
func TestConduitBrokerMemo_SharedReads(t *testing.T) {
	ctx := context.Background()
	memoOf := func() *conduitBrokerMemo { return conduitBrokerMemoFrom(withConduitBrokerMemo(ctx)) }
	row := &store.RuntimeBroker{ID: "broker-1"}

	t.Run("definite answers are kept, per broker", func(t *testing.T) {
		m := memoOf()
		var reads atomic.Int64
		read := func(_ context.Context, id string) (*store.RuntimeBroker, error) {
			reads.Add(1)
			if id == "gone" {
				return nil, store.ErrNotFound
			}
			return row, nil
		}
		for range 3 {
			b, err := m.getRuntimeBroker(ctx, "broker-1", read)
			require.NoError(t, err)
			assert.Same(t, row, b)
			_, err = m.getRuntimeBroker(ctx, "gone", read)
			assert.ErrorIs(t, err, store.ErrNotFound)
		}
		assert.Equal(t, int64(2), reads.Load())
	})

	t.Run("concurrent reads share the first", func(t *testing.T) {
		m := memoOf()
		var reads atomic.Int64
		entered, release := make(chan struct{}), make(chan struct{})
		read := func(context.Context, string) (*store.RuntimeBroker, error) {
			if reads.Add(1) == 1 {
				close(entered)
				<-release
			}
			return row, nil
		}
		var parked atomic.Int32
		m.brokerMemoWaitTestHook = func() { parked.Add(1) }
		var wg sync.WaitGroup
		results := make([]*store.RuntimeBroker, 8)
		wg.Add(1)
		go func() { defer wg.Done(); results[0], _ = m.getRuntimeBroker(ctx, "broker-1", read) }()
		<-entered
		for i := 1; i < len(results); i++ {
			wg.Add(1)
			go func() { defer wg.Done(); results[i], _ = m.getRuntimeBroker(ctx, "broker-1", read) }()
		}
		require.Eventually(t, func() bool { return parked.Load() == int32(len(results)-1) }, recheckWait, time.Millisecond,
			"every other caller joins the first read")
		close(release)
		wg.Wait()
		assert.Equal(t, int64(1), reads.Load())
		for i, b := range results {
			assert.Same(t, row, b, "waiter %d", i)
		}
	})

	t.Run("a failed read goes to its waiters and is not kept", func(t *testing.T) {
		m := memoOf()
		var reads atomic.Int64
		boom := errors.New("injected broker read failure")
		entered, release := make(chan struct{}), make(chan struct{})
		read := func(context.Context, string) (*store.RuntimeBroker, error) {
			if reads.Add(1) == 1 {
				close(entered)
				<-release
				return nil, boom
			}
			return row, nil
		}
		var parked atomic.Int32
		m.brokerMemoWaitTestHook = func() { parked.Add(1) }
		leader := make(chan error, 1)
		go func() { _, err := m.getRuntimeBroker(ctx, "broker-1", read); leader <- err }()
		<-entered
		waiter := make(chan error, 1)
		go func() { _, err := m.getRuntimeBroker(ctx, "broker-1", read); waiter <- err }()
		require.Eventually(t, func() bool { return parked.Load() == 1 }, recheckWait, time.Millisecond,
			"the waiter has joined the leader's read")
		close(release)
		assert.ErrorIs(t, <-leader, boom)
		assert.ErrorIs(t, <-waiter, boom, "the waiter gets the failure, without a read of its own")
		assert.Equal(t, int64(1), reads.Load())

		b, err := m.getRuntimeBroker(ctx, "broker-1", read)
		require.NoError(t, err, "nothing was kept: a later call reads again")
		assert.Same(t, row, b)
		assert.Equal(t, int64(2), reads.Load())
	})

	t.Run("a waiter stops at its own deadline", func(t *testing.T) {
		m := memoOf()
		entered, release := make(chan struct{}), make(chan struct{})
		defer close(release)
		read := func(context.Context, string) (*store.RuntimeBroker, error) {
			close(entered)
			<-release
			return row, nil
		}
		go func() { _, _ = m.getRuntimeBroker(ctx, "broker-1", read) }()
		<-entered
		wctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
		defer cancel()
		_, err := m.getRuntimeBroker(wctx, "broker-1", read)
		assert.ErrorIs(t, err, context.DeadlineExceeded)
	})
}

// TestConduitPlacementMemo_NoMemoWithoutPass (T8): without a pass memo
// on the context the placement guard reads the store directly, every
// time.
func TestConduitPlacementMemo_NoMemoWithoutPass(t *testing.T) {
	f, counter := newMemoRecheckFixture(t)
	agent, err := f.store.GetAgent(context.Background(), f.agent.ID)
	require.NoError(t, err)
	assert.Nil(t, conduitBrokerMemoFrom(context.Background()))
	for range 2 {
		_, err := f.srv.conduitStreamPlacement(context.Background(), agent, nil)
		require.NoError(t, err)
	}
	assert.Equal(t, int64(2), counter.reads.Load())
}

// TestConduitSweepContext_OnlyTheUserStreamReCheck: production code sets
// SweepContext only on the user-stream re-check (startConduitStreamAuthz),
// as a composite-literal key, and never assigns it; the broker-session
// re-check, which shares the tracker, has none.
func TestConduitSweepContext_OnlyTheUserStreamReCheck(t *testing.T) {
	var sites []string
	for name, file := range productionFiles(t) {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				if id, ok := lit.Type.(*ast.Ident); !ok || id.Name != "conduitStreamAuthzConfig" {
					return true
				}
				for _, e := range lit.Elts {
					if kv, ok := e.(*ast.KeyValueExpr); ok {
						if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "SweepContext" {
							sites = append(sites, name+":"+fn.Name.Name)
						}
					}
				}
				return true
			})
			ast.Inspect(fn, func(n ast.Node) bool {
				as, ok := n.(*ast.AssignStmt)
				if !ok {
					return true
				}
				for _, lhs := range as.Lhs {
					if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "SweepContext" {
						sites = append(sites, name+":"+fn.Name.Name+" (assignment)")
					}
				}
				return true
			})
		}
	}
	assert.Equal(t, []string{"conduit_authz_recheck.go:startConduitStreamAuthz"}, sites)
}
