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
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

// Tests for claimless deferred delete intents: one is executed from the
// intent alone once the agent row is gone, as after a project delete
// (ptone/scion#3665), and each carries a notAfter fixed when it is written
// (ptone/scion#3674).

// rowlessDelete is one delete a broker received.
type rowlessDelete struct {
	brokerID, slug, projectID, runtime string
	opts                               DeleteAgentOptions
}

// rowlessRecordingClient records every delete with the broker, slug,
// project and recorded runtime it was sent with, and answers each with
// returnErr.
type rowlessRecordingClient struct {
	*mockRuntimeBrokerClient
	returnErr error

	mu      sync.Mutex
	deletes []rowlessDelete
}

func newRowlessRecordingClient(returnErr error) *rowlessRecordingClient {
	return &rowlessRecordingClient{mockRuntimeBrokerClient: &mockRuntimeBrokerClient{}, returnErr: returnErr}
}

func (c *rowlessRecordingClient) DeleteAgent(ctx context.Context, brokerID, _, slug, projectID string, opts DeleteAgentOptions) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deletes = append(c.deletes, rowlessDelete{
		brokerID: brokerID, slug: slug, projectID: projectID,
		runtime: recordedRuntimeParam(ctx), opts: opts,
	})
	return c.returnErr
}

func (c *rowlessRecordingClient) sent() []rowlessDelete {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]rowlessDelete(nil), c.deletes...)
}

func rowlessRuns(sent []rowlessDelete) []string {
	runs := make([]string, 0, len(sent))
	for _, d := range sent {
		runs = append(runs, d.opts.RunID)
	}
	return runs
}

// levelLog is one captured log record.
type levelLog struct {
	level slog.Level
	msg   string
	attrs map[string]any
}

// levelLogHandler captures log records with their level and attributes.
type levelLogHandler struct {
	mu   sync.Mutex
	recs []levelLog
}

func (h *levelLogHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *levelLogHandler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *levelLogHandler) WithGroup(string) slog.Handler            { return h }
func (h *levelLogHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	rec := levelLog{level: r.Level, msg: r.Message, attrs: map[string]any{}}
	r.Attrs(func(a slog.Attr) bool {
		rec.attrs[a.Key] = a.Value.Any()
		return true
	})
	h.recs = append(h.recs, rec)
	return nil
}

func (h *levelLogHandler) named(msg string) []levelLog {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []levelLog
	for _, r := range h.recs {
		if r.msg == msg {
			out = append(out, r)
		}
	}
	return out
}

const staleClaimlessDeleteMsg = "reconcile: claimless deferred delete intent was stale; nothing was deleted"

// rowlessFixture is an agent on a broker connected to another hub node,
// whose row records run-a after run-p and whose recorded runtime is
// docker.
type rowlessFixture struct {
	store    store.Store
	events   *ChannelEventPublisher
	broker   string
	snapshot store.Agent
}

func newRowlessFixture(t *testing.T) *rowlessFixture {
	t.Helper()
	ctx := context.Background()
	cs := entadapter.NewCompositeStore(enttest.NewClient(t))
	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)
	broker := uuid.NewString()
	agent := seedAgentWithBrokerID(t, cs, broker)
	for _, run := range []string{"run-p", "run-a"} {
		_, err := cs.SetAgentRunID(ctx, agent.ID, run, nil)
		require.NoError(t, err)
	}
	snapshot := *mustGetAgent(t, cs, agent.ID)
	snapshot.Runtime = "docker"
	return &rowlessFixture{store: cs, events: events, broker: broker, snapshot: snapshot}
}

// originator is a hub node the broker is not connected to: its deletes
// for the broker are deferred to an intent.
func (f *rowlessFixture) originator(t *testing.T) *Server {
	t.Helper()
	srv, _ := testServerWithStore(t, f.store)
	disp := NewHTTPAgentDispatcherWithClient(f.store, &deferredDataOpTestClient{localBroker: "local-broker"}, false, slog.Default())
	disp.SetCrossNodeDeps(f.events, NoopCommandBus{})
	srv.SetDispatcher(disp)
	return srv
}

// owner is the hub node the broker is connected to.
func (f *rowlessFixture) owner(t *testing.T, client RuntimeBrokerClient) (*Server, *levelLogHandler) {
	t.Helper()
	srv, _ := testServerWithStore(t, f.store)
	srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(f.store, client, false, slog.Default()))
	logs := &levelLogHandler{}
	srv.agentLifecycleLog = slog.New(logs)
	return srv, logs
}

// removeRows deletes the agent and project rows, as a project delete
// does before it dispatches the broker deletes.
func (f *rowlessFixture) removeRows(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, f.store.DeleteAgent(ctx, f.snapshot.ID))
	require.NoError(t, f.store.DeleteProject(ctx, f.snapshot.ProjectID))
	_, err := f.store.GetAgent(ctx, f.snapshot.ID)
	require.ErrorIs(t, err, store.ErrNotFound)
}

// projectDeleteIntent runs the originator's project-delete dispatch for
// the snapshot in the background and returns the delete intent it wrote,
// and a wait for the dispatch to return once the intent is settled.
func (f *rowlessFixture) projectDeleteIntent(t *testing.T) (store.BrokerDispatch, func()) {
	t.Helper()
	ctx := context.Background()
	origin := f.originator(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		origin.dispatchAgentDeletions(ctx, []store.Agent{f.snapshot})
	}()
	var d store.BrokerDispatch
	require.Eventually(t, func() bool {
		pending, err := f.store.ListPendingDispatch(ctx, f.broker)
		if err != nil || len(pending) == 0 {
			return false
		}
		d = pending[0]
		return true
	}, 5*time.Second, 10*time.Millisecond, "no delete intent was written")
	require.Equal(t, brokerDispatchOpDelete, d.Op)
	wait := func() {
		t.Helper()
		f.events.PublishDispatchDone(ctx, d.ID)
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Fatal("the project-delete dispatch never returned")
		}
	}
	return d, wait
}

// A project delete deferred to the owning node writes an intent that
// records the agent's target, its runs and a notAfter; the owning node,
// finding the row gone, still sends the delete for exactly those runs to
// the right broker, project, slug and runtime, fenced by that notAfter,
// and the intent completes.
func TestProjectDeleteDeferred_RowGone_DeletesFromIntent(t *testing.T) {
	t0 := fenceNow(t)
	ctx := context.Background()
	f := newRowlessFixture(t)
	f.removeRows(t)
	d, wait := f.projectDeleteIntent(t)

	args, err := UnmarshalDeleteArgs(d.Args)
	require.NoError(t, err)
	assert.Equal(t, &DeleteIntentTarget{BrokerID: f.broker, ProjectID: f.snapshot.ProjectID, Slug: f.snapshot.Slug, Runtime: "docker"}, args.Target)
	assert.Equal(t, "run-a", args.RunID)
	assert.Equal(t, []string{"run-p"}, args.PreviousRunIDs)
	assert.Zero(t, args.Claim)
	wantNotAfter := t0.Add(deleteDispatchBudget - deleteNotAfterMargin)
	assert.True(t, args.NotAfter.Equal(wantNotAfter), "intent notAfter = %v, want %v", args.NotAfter, wantNotAfter)

	client := newRowlessRecordingClient(nil)
	owner, logs := f.owner(t, client)
	owner.drainBrokerDispatch(ctx, f.broker, nil)
	wait()

	row, err := f.store.GetBrokerDispatch(ctx, d.ID)
	require.NoError(t, err)
	assert.Equal(t, store.DispatchStateDone, row.State, "intent error: %s", row.Error)
	sent := client.sent()
	require.Equal(t, []string{"run-a", "run-p"}, rowlessRuns(sent))
	for _, s := range sent {
		assert.Equal(t, f.broker, s.brokerID)
		assert.Equal(t, f.snapshot.Slug, s.slug)
		assert.Equal(t, f.snapshot.ProjectID, s.projectID)
		assert.Contains(t, s.runtime, "docker")
		assert.True(t, s.opts.DeleteFiles)
		assert.True(t, s.opts.RemoveBranch)
		assert.True(t, s.opts.NotAfter.Equal(wantNotAfter), "run %s: notAfter = %v, want %v", s.opts.RunID, s.opts.NotAfter, wantNotAfter)
	}
	assert.Empty(t, logs.named(staleClaimlessDeleteMsg))
}

// On that path a broker refusal (another run holds the name) can only be
// logged by the project delete: the intent fails with the broker's answer,
// as any refused deferred delete does, and nothing else changes.
func TestProjectDeleteDeferred_RowGone_RefusalSettlesIntent(t *testing.T) {
	fenceNow(t)
	ctx := context.Background()
	f := newRowlessFixture(t)
	f.removeRows(t)
	d, wait := f.projectDeleteIntent(t)

	client := newRowlessRecordingClient(runMismatchEnvelope(t, "run-a", "run-z", true))
	owner, _ := f.owner(t, client)
	owner.drainBrokerDispatch(ctx, f.broker, nil)
	wait()

	row, err := f.store.GetBrokerDispatch(ctx, d.ID)
	require.NoError(t, err)
	assert.Equal(t, store.DispatchStateFailed, row.State)
	assert.Contains(t, row.Error, "run-z", "the broker's answer is recorded")
	assert.Equal(t, []string{"run-a"}, rowlessRuns(client.sent()), "previous runs are not sent after a refusal")
}

// With the row present the intent's target is not used: the delete goes to
// the row's slug and runtime, as before. With the row gone, an intent with
// no target (an older hub), with an engine claim, or whose target names a
// broker other than the one being drained fails not-found, as before, and
// nothing reaches the broker. A lookup error other than not-found is
// returned as is, and the target is not used.
func TestExecDispatchDelete_RowlessOnlyForClaimlessTargetedIntents(t *testing.T) {
	ctx := context.Background()
	exec := func(t *testing.T, f *rowlessFixture, args DeleteDispatchArgs) (*rowlessRecordingClient, error) {
		t.Helper()
		raw, err := MarshalDispatchArgs(&args)
		require.NoError(t, err)
		client := newRowlessRecordingClient(nil)
		owner, _ := f.owner(t, client)
		_, execErr := owner.execDispatchDelete(ctx, store.BrokerDispatch{
			ID: uuid.NewString(), BrokerID: f.broker, AgentID: f.snapshot.ID, AgentSlug: f.snapshot.Slug,
			ProjectID: f.snapshot.ProjectID, Op: brokerDispatchOpDelete, Args: raw,
		})
		return client, execErr
	}
	target := func(f *rowlessFixture) *DeleteIntentTarget {
		return &DeleteIntentTarget{BrokerID: f.broker, ProjectID: f.snapshot.ProjectID, Slug: f.snapshot.Slug, Runtime: "docker"}
	}

	t.Run("row present: the row is used", func(t *testing.T) {
		f := newRowlessFixture(t)
		tgt := target(f)
		tgt.Slug = "other-slug"
		tgt.Runtime = "kubernetes"
		client, err := exec(t, f, DeleteDispatchArgs{RunID: "run-a", Target: tgt})
		require.NoError(t, err)
		sent := client.sent()
		require.Len(t, sent, 1)
		assert.Equal(t, f.snapshot.Slug, sent[0].slug)
		assert.Empty(t, sent[0].runtime, "the row records no runtime")
	})
	t.Run("row gone, no target: not found", func(t *testing.T) {
		f := newRowlessFixture(t)
		f.removeRows(t)
		client, err := exec(t, f, DeleteDispatchArgs{RunID: "run-a", PreviousRunIDs: []string{"run-p"}})
		require.ErrorIs(t, err, store.ErrNotFound)
		assert.Empty(t, client.sent())
	})
	t.Run("row gone, engine claim: not found", func(t *testing.T) {
		f := newRowlessFixture(t)
		f.removeRows(t)
		client, err := exec(t, f, DeleteDispatchArgs{RunID: "run-a", Claim: 3, Target: target(f)})
		require.ErrorIs(t, err, store.ErrNotFound)
		assert.Empty(t, client.sent())
	})
	t.Run("row gone, claimless with target: sent from the intent", func(t *testing.T) {
		f := newRowlessFixture(t)
		f.removeRows(t)
		client, err := exec(t, f, DeleteDispatchArgs{RunID: "run-a", PreviousRunIDs: []string{"run-p"}, Target: target(f)})
		require.NoError(t, err)
		assert.Equal(t, []string{"run-a", "run-p"}, rowlessRuns(client.sent()))
	})
	t.Run("row gone, target on another broker: not found", func(t *testing.T) {
		f := newRowlessFixture(t)
		f.removeRows(t)
		tgt := target(f)
		tgt.BrokerID = uuid.NewString()
		client, err := exec(t, f, DeleteDispatchArgs{RunID: "run-a", PreviousRunIDs: []string{"run-p"}, Target: tgt})
		require.ErrorIs(t, err, store.ErrNotFound)
		assert.Empty(t, client.sent())
	})
	t.Run("lookup error other than not found: returned, target unused", func(t *testing.T) {
		f := newRowlessFixture(t)
		raw, err := MarshalDispatchArgs(&DeleteDispatchArgs{RunID: "run-a", PreviousRunIDs: []string{"run-p"}, Target: target(f)})
		require.NoError(t, err)
		client := newRowlessRecordingClient(nil)
		owner, _ := f.owner(t, client)
		injected := errors.New("injected agent lookup failure")
		_, fault := installStoreFault(t, owner, func(inner store.Store, fs *storeFaultSwitch) *rowlessGetAgentErrStore {
			return &rowlessGetAgentErrStore{Store: inner, fault: fs, err: injected}
		})
		fault.Arm()
		_, execErr := owner.execDispatchDelete(ctx, store.BrokerDispatch{
			ID: uuid.NewString(), BrokerID: f.broker, AgentID: f.snapshot.ID, AgentSlug: f.snapshot.Slug,
			ProjectID: f.snapshot.ProjectID, Op: brokerDispatchOpDelete, Args: raw,
		})
		require.Error(t, execErr)
		assert.NotErrorIs(t, execErr, store.ErrNotFound)
		assert.ErrorIs(t, execErr, injected)
		assert.Empty(t, client.sent())
	})
}

// rowlessGetAgentErrStore fails GetAgent with err once its switch is armed.
type rowlessGetAgentErrStore struct {
	store.Store
	fault *storeFaultSwitch
	err   error
}

func (s *rowlessGetAgentErrStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	if !s.fault.Active() {
		return s.Store.GetAgent(ctx, id)
	}
	return nil, s.err
}

// The originating node records a notAfter on a claimless intent and only
// the claim on an engine's intent; a node that defers again keeps the
// notAfter it is executing under.
func TestDeferredDelete_NotAfterOnlyWhenClaimless(t *testing.T) {
	for _, tc := range []struct {
		name      string
		fence     *deleteDispatchFence
		wantClaim int64
		// wantNotAfter from the clock's t0; nil wants none.
		wantNotAfter func(t0 time.Time) time.Time
	}{
		{name: "claimless", wantNotAfter: func(t0 time.Time) time.Time { return t0.Add(deleteDispatchBudget - deleteNotAfterMargin) }},
		{name: "engine claim", fence: &deleteDispatchFence{claim: 7, notAfter: time.Now().Add(time.Minute)}, wantClaim: 7},
		{name: "deferred again under a claimless fence", fence: &deleteDispatchFence{notAfter: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)},
			wantNotAfter: func(time.Time) time.Time { return time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t0 := fenceNow(t)
			ctx := context.Background()
			f := newRowlessFixture(t)
			origin := f.originator(t)
			dctx := ctx
			if tc.fence != nil {
				dctx = withDeleteDispatchFence(ctx, *tc.fence)
			}
			errc := make(chan error, 1)
			go func() {
				errc <- origin.GetDispatcher().DispatchAgentDelete(dctx, &f.snapshot, true, false, false, time.Time{})
			}()
			var d store.BrokerDispatch
			require.Eventually(t, func() bool {
				pending, err := f.store.ListPendingDispatch(ctx, f.broker)
				if err != nil || len(pending) == 0 {
					return false
				}
				d = pending[0]
				return true
			}, 5*time.Second, 10*time.Millisecond)
			_, _ = f.store.ClaimBrokerDispatch(ctx, d.ID, "owner-hub")
			require.NoError(t, f.store.CompleteBrokerDispatch(ctx, d.ID, ""))
			f.events.PublishDispatchDone(ctx, d.ID)
			require.NoError(t, <-errc)

			args, err := UnmarshalDeleteArgs(d.Args)
			require.NoError(t, err)
			assert.Equal(t, tc.wantClaim, args.Claim)
			if tc.wantNotAfter == nil {
				assert.True(t, args.NotAfter.IsZero(), "an engine intent records notAfter %v", args.NotAfter)
				return
			}
			want := tc.wantNotAfter(t0)
			assert.True(t, args.NotAfter.Equal(want), "notAfter = %v, want %v", args.NotAfter, want)
		})
	}
}

// A claimless intent that the executing node cannot deliver itself (the
// broker is connected to another node) is deferred again by
// execDispatchDelete with the notAfter it was written with, not a new one.
func TestExecDispatchDelete_DeferredAgainKeepsNotAfter(t *testing.T) {
	t0 := fenceNow(t)
	ctx := context.Background()
	f := newRowlessFixture(t)
	// Not the notAfter a new claimless intent written at t0 would get.
	notAfter := t0.Add(time.Minute)
	require.False(t, notAfter.Equal(claimlessDeleteNotAfter(t0)))
	raw, err := MarshalDispatchArgs(&DeleteDispatchArgs{
		DeleteFiles: true, RunID: "run-a", PreviousRunIDs: []string{"run-p"},
		Target:   &DeleteIntentTarget{BrokerID: f.broker, ProjectID: f.snapshot.ProjectID, Slug: f.snapshot.Slug, Runtime: "docker"},
		NotAfter: notAfter,
	})
	require.NoError(t, err)

	// The executing node is not connected to the broker either.
	node := f.originator(t)
	errc := make(chan error, 1)
	go func() {
		_, execErr := node.execDispatchDelete(ctx, store.BrokerDispatch{
			ID: uuid.NewString(), BrokerID: f.broker, AgentID: f.snapshot.ID, AgentSlug: f.snapshot.Slug,
			ProjectID: f.snapshot.ProjectID, Op: brokerDispatchOpDelete, Args: raw,
		})
		errc <- execErr
	}()
	var d store.BrokerDispatch
	require.Eventually(t, func() bool {
		pending, err := f.store.ListPendingDispatch(ctx, f.broker)
		if err != nil || len(pending) == 0 {
			return false
		}
		d = pending[0]
		return true
	}, 5*time.Second, 10*time.Millisecond, "the intent was not deferred again")
	_, _ = f.store.ClaimBrokerDispatch(ctx, d.ID, "owner-hub")
	require.NoError(t, f.store.CompleteBrokerDispatch(ctx, d.ID, ""))
	f.events.PublishDispatchDone(ctx, d.ID)
	select {
	case execErr := <-errc:
		require.NoError(t, execErr)
	case <-time.After(20 * time.Second):
		t.Fatal("execDispatchDelete never returned")
	}

	require.Equal(t, brokerDispatchOpDelete, d.Op)
	args, err := UnmarshalDeleteArgs(d.Args)
	require.NoError(t, err)
	assert.Zero(t, args.Claim)
	assert.True(t, args.NotAfter.Equal(notAfter), "deferred-again notAfter = %v, want the original %v", args.NotAfter, notAfter)
	assert.Equal(t, "run-a", args.RunID)
}

// The executing node sends a claimless intent with its recorded notAfter,
// drops it without dispatching once that has passed, and logs a warning
// when it is stale (dropped here, or refused by the broker). An engine's
// intent is fenced from its claim, whatever notAfter it might carry.
func TestExecDispatchDelete_ClaimlessNotAfter(t *testing.T) {
	ctx := context.Background()
	run := func(t *testing.T, f *rowlessFixture, client *rowlessRecordingClient, args DeleteDispatchArgs) (*levelLogHandler, error) {
		t.Helper()
		raw, err := MarshalDispatchArgs(&args)
		require.NoError(t, err)
		owner, logs := f.owner(t, client)
		_, execErr := owner.execDispatchDelete(ctx, store.BrokerDispatch{ID: "d1", BrokerID: f.broker, AgentID: f.snapshot.ID, Op: brokerDispatchOpDelete, Args: raw})
		return logs, execErr
	}
	requireStaleWarn := func(t *testing.T, f *rowlessFixture, logs *levelLogHandler, notAfter time.Time, by string) {
		t.Helper()
		recs := logs.named(staleClaimlessDeleteMsg)
		require.Len(t, recs, 1)
		assert.Equal(t, slog.LevelWarn, recs[0].level)
		assert.Equal(t, f.snapshot.ID, recs[0].attrs["agent_id"])
		assert.Equal(t, f.snapshot.Slug, recs[0].attrs["agent"])
		assert.Equal(t, f.broker, recs[0].attrs["broker"])
		assert.True(t, notAfter.Equal(recs[0].attrs["not_after"].(time.Time)))
		assert.Equal(t, by, recs[0].attrs["refused_by"])
	}

	t.Run("in time: sent with the intent's notAfter", func(t *testing.T) {
		t0 := fenceNow(t)
		f := newRowlessFixture(t)
		client := newRowlessRecordingClient(nil)
		notAfter := t0.Add(time.Minute)
		logs, err := run(t, f, client, DeleteDispatchArgs{RunID: "run-a", PreviousRunIDs: []string{"run-p"}, NotAfter: notAfter})
		require.NoError(t, err)
		sent := client.sent()
		require.Equal(t, []string{"run-a", "run-p"}, rowlessRuns(sent))
		for _, s := range sent {
			assert.True(t, s.opts.NotAfter.Equal(notAfter), "run %s: notAfter = %v", s.opts.RunID, s.opts.NotAfter)
		}
		assert.Empty(t, logs.named(staleClaimlessDeleteMsg))
	})
	t.Run("late: dropped by the hub", func(t *testing.T) {
		t0 := fenceNow(t)
		f := newRowlessFixture(t)
		f.removeRows(t)
		client := newRowlessRecordingClient(nil)
		notAfter := t0.Add(-time.Second)
		tgt := &DeleteIntentTarget{BrokerID: f.broker, ProjectID: f.snapshot.ProjectID, Slug: f.snapshot.Slug}
		logs, err := run(t, f, client, DeleteDispatchArgs{RunID: "run-a", Target: tgt, NotAfter: notAfter})
		require.ErrorIs(t, err, errStaleDeleteDispatch)
		assert.True(t, staleDeleteDispatchFromText(err.Error()), "the row error text keeps the stale marker")
		assert.Empty(t, client.sent(), "a stale intent reached the broker")
		requireStaleWarn(t, f, logs, notAfter, "hub")
	})
	t.Run("late at the broker: refused as stale", func(t *testing.T) {
		t0 := fenceNow(t)
		f := newRowlessFixture(t)
		client := newRowlessRecordingClient(staleDispatchErr())
		notAfter := t0.Add(time.Minute)
		logs, err := run(t, f, client, DeleteDispatchArgs{RunID: "run-a", NotAfter: notAfter})
		require.Error(t, err)
		assert.True(t, staleDeleteDispatchFromText(err.Error()), "error text %q lacks the stale marker", err.Error())
		requireStaleWarn(t, f, logs, notAfter, "broker")
	})
	t.Run("engine claim: fenced from the claim", func(t *testing.T) {
		fenceNow(t)
		f := newRowlessFixture(t)
		seedAgentDeletion(t, f.store, f.snapshot.ID, seedLiveDeleting)
		row := mustGetAgent(t, f.store, f.snapshot.ID)
		client := newRowlessRecordingClient(nil)
		bogus := time.Now().Add(-time.Hour)
		logs, err := run(t, f, client, DeleteDispatchArgs{RunID: "run-a", Claim: row.DeletionClaim, NotAfter: bogus})
		require.NoError(t, err)
		sent := client.sent()
		require.Len(t, sent, 1)
		want := row.DeletionLeaseAt.Add(-deleteNotAfterMargin)
		assert.True(t, sent[0].opts.NotAfter.Equal(want), "notAfter = %v, want %v", sent[0].opts.NotAfter, want)
		assert.Empty(t, logs.named(staleClaimlessDeleteMsg))
	})
}
