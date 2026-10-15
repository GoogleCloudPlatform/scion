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
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// End-to-end halt dispatch tests (ptone/scion#4853): a halt written through
// the heartbeat, the self-report path or the reconcile reaches the
// notification dispatcher and the hook evaluator wired into the server, and
// the bus does not fire them a second time.

// haltDispatchWiring is a notification dispatcher and a hook evaluator, both
// started on a channel event publisher, wired into a test server.
type haltDispatchWiring struct {
	srv  *Server
	s    store.Store
	exec *recordingExecutor
}

func wireHaltDispatch(t *testing.T, srv *Server, s store.Store) haltDispatchWiring {
	t.Helper()
	pub := NewChannelEventPublisher()
	t.Cleanup(pub.Close)
	srv.events = pub
	nd := NewNotificationDispatcher(s, pub, func() AgentDispatcher { return nil }, slog.Default())
	exec := &recordingExecutor{}
	ev := NewLifecycleHookEvaluator(s, pub, exec, slog.Default())
	nd.Start()
	ev.Start()
	t.Cleanup(func() {
		nd.Stop()
		ev.Stop()
	})
	srv.mu.Lock()
	srv.notificationDispatcher = nd
	srv.lifecycleHookEvaluator = ev
	srv.mu.Unlock()
	for _, trig := range []string{"halted", "stopped", "error"} {
		seedLifecycleHook(t, s, "h-"+trig, trig, true, nil)
	}
	return haltDispatchWiring{srv: srv, s: s, exec: exec}
}

// watch subscribes user "watcher" to agentID with triggers.
func (w haltDispatchWiring) watch(t *testing.T, agentID, projectID string, triggers ...string) {
	t.Helper()
	require.NoError(t, w.s.CreateNotificationSubscription(context.Background(), &store.NotificationSubscription{
		ID:                api.NewUUID(),
		Scope:             store.SubscriptionScopeAgent,
		AgentID:           agentID,
		SubscriberType:    store.SubscriberTypeUser,
		SubscriberID:      "watcher",
		ProjectID:         projectID,
		TriggerActivities: triggers,
		CreatedAt:         time.Now().Add(-time.Hour),
		CreatedBy:         "test",
	}))
}

// The helpers below are polled on the test goroutine, never inside
// testify's Eventually or Never. Those run the condition on a separate
// goroutine and can return while a call is still in flight; a call that
// reads the store after the test's cleanup has closed it fails with
// "sql: database is closed", and a require inside it fails the test from a
// non-test goroutine (ptone/scion#5260).

// watcherStatuses returns the sorted statuses of user "watcher"'s
// notifications.
func (w haltDispatchWiring) watcherStatuses() ([]string, error) {
	notifs, err := w.s.GetNotifications(context.Background(), store.SubscriberTypeUser, "watcher", false)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(notifs))
	for _, n := range notifs {
		out = append(out, n.Status)
	}
	sort.Strings(out)
	return out, nil
}

func (w haltDispatchWiring) triggers() []string {
	var out []string
	for _, c := range w.exec.getCalls() {
		out = append(out, c.Trigger)
	}
	sort.Strings(out)
	return out
}

// matches reports whether the watcher's statuses and the fired hook
// triggers equal the wanted ones (a nil wantStatuses skips the statuses),
// and describes what was observed.
func (w haltDispatchWiring) matches(wantStatuses, wantTriggers []string) (bool, string) {
	gotTriggers := w.triggers()
	if wantStatuses == nil {
		return assert.ObjectsAreEqual(wantTriggers, gotTriggers), fmt.Sprintf("triggers %v", gotTriggers)
	}
	gotStatuses, err := w.watcherStatuses()
	if err != nil {
		return false, fmt.Sprintf("reading notifications: %v (triggers %v)", err, gotTriggers)
	}
	ok := assert.ObjectsAreEqual(wantStatuses, gotStatuses) && assert.ObjectsAreEqual(wantTriggers, gotTriggers)
	return ok, fmt.Sprintf("statuses %v triggers %v", gotStatuses, gotTriggers)
}

// waitMatch polls matches on the test goroutine until it holds, failing the
// test with the last observation after 3s.
func (w haltDispatchWiring) waitMatch(t *testing.T, wantStatuses, wantTriggers []string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		ok, got := w.matches(wantStatuses, wantTriggers)
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("want statuses %v triggers %v; last saw %s", wantStatuses, wantTriggers, got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// settle waits for the in-process halt dispatch, then checks that the bus
// adds nothing more within a short window. Every check runs on the test
// goroutine and is finished when settle returns.
func (w haltDispatchWiring) settle(t *testing.T, wantStatuses, wantTriggers []string) {
	t.Helper()
	w.srv.haltedDispatches.Wait()
	w.waitMatch(t, wantStatuses, wantTriggers)
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		if ok, got := w.matches(wantStatuses, wantTriggers); !ok {
			t.Errorf("the bus must not notify or fire a halt again: want statuses %v triggers %v; saw %s", wantStatuses, wantTriggers, got)
			return
		}
	}
}

func TestHaltedDispatch_HeartbeatPreempt(t *testing.T) {
	f := newHaltFixture(t)
	w := wireHaltDispatch(t, f.srv, f.s)
	w.watch(t, f.agent.ID, f.project.ID, "HALTED", "ERROR")

	f.heartbeat(t, brokerAgentHeartbeat{Phase: "stopped", ExitReason: "preempted", RunID: "run-1"})
	require.Equal(t, "halted", f.get(t).Phase)

	w.settle(t, []string{"HALTED"}, []string{"halted", "stopped"})
}

func TestHaltedDispatch_ErrorUpgradeOverPlainStop(t *testing.T) {
	// The workspace did not survive: the agent's plain stop lands first,
	// then the broker reports error + preempted. One ERROR (alias) and one
	// error hook; the stopped hook, already fired by the plain stop, is not
	// fired again.
	f := newHaltFixture(t)
	w := wireHaltDispatch(t, f.srv, f.s)
	w.watch(t, f.agent.ID, f.project.ID, "ERROR")

	require.Equal(t, http.StatusOK, f.selfReport(t, "run-1", map[string]any{"phase": "stopped"}))
	w.waitMatch(t, nil, []string{"stopped"})

	f.heartbeat(t, brokerAgentHeartbeat{Phase: "error", ExitReason: "preempted", RunID: "run-1"})
	require.Equal(t, "halted", f.get(t).Phase)

	w.settle(t, []string{"ERROR"}, []string{"error", "halted", "stopped"})
}

func TestHaltedDispatch_OOMAfterCrash(t *testing.T) {
	// The crash is published on the bus (ERROR and the error hook), then the
	// OOM reason halts the same run: exactly one ERROR in total, and only
	// the halted hook from the halt.
	f := newHaltFixture(t)
	w := wireHaltDispatch(t, f.srv, f.s)
	w.watch(t, f.agent.ID, f.project.ID, "ERROR")

	code := 137
	f.heartbeat(t, brokerAgentHeartbeat{Phase: "error", ExitCode: &code, RunID: "run-1"})
	require.Equal(t, "error", f.get(t).Phase)
	w.waitMatch(t, []string{"ERROR"}, []string{"error"})

	f.heartbeat(t, brokerAgentHeartbeat{Phase: "error", ExitReason: "oom_killed", ExitCode: &code, RunID: "run-1"})
	require.Equal(t, "halted", f.get(t).Phase)

	w.settle(t, []string{"ERROR"}, []string{"error", "halted"})
}

func TestHaltedDispatch_Reconcile(t *testing.T) {
	f := newReconcileFixture(t)
	w := wireHaltDispatch(t, f.srv, f.s)
	omitted := f.addAgent("omitted", "running", "working")
	_, err := f.s.SetRunIntent(context.Background(), omitted.ID, store.RunIntentRunning)
	require.NoError(t, err)
	w.watch(t, omitted.ID, f.projectID, "ERROR")

	inv := completeInventory()
	inv.Targets[0].Runtime = "docker"
	f.heartbeat(inv)
	f.expireClock(omitted.ID)
	f.heartbeat(inv)
	require.Equal(t, "halted", f.get(omitted.ID).Phase)

	// The container-missing write would have stored error: ERROR
	// subscribers and error hooks fire as before, with the halted hook.
	w.settle(t, []string{"ERROR"}, []string{"error", "halted"})
}

func TestHaltedDispatch_SelfReportedStopThenRuntimeReason(t *testing.T) {
	// A self-reported stop never suppresses the runtime's later halt, its
	// HALTED notification or its hook.
	f := newHaltFixture(t)
	w := wireHaltDispatch(t, f.srv, f.s)
	w.watch(t, f.agent.ID, f.project.ID, "HALTED")

	require.Equal(t, http.StatusOK, f.selfReport(t, "run-1", map[string]any{"phase": "stopped", "exitCode": 0}))
	w.waitMatch(t, nil, []string{"stopped"})

	f.heartbeat(t, brokerAgentHeartbeat{Phase: "stopped", ExitReason: "preempted", RunID: "run-1"})
	require.Equal(t, "halted", f.get(t).Phase)

	w.settle(t, []string{"HALTED"}, []string{"halted", "stopped"})
}

func TestOnAgentHalted_NothingWired(t *testing.T) {
	srv, _ := testServer(t)
	done := srv.onAgentHalted(context.Background(), &store.Agent{ID: "a", Slug: "a"}, "")
	select {
	case <-done:
	default:
		t.Fatal("with no dispatcher and no evaluator the done channel is closed at once")
	}
	select {
	case <-srv.onAgentHalted(context.Background(), nil, ""):
	default:
		t.Fatal("a nil agent closes the done channel at once")
	}
}

// panickingSubscriptionStore panics when the halt dispatch reads
// subscriptions.
type panickingSubscriptionStore struct {
	store.Store
}

func (panickingSubscriptionStore) GetNotificationSubscriptions(context.Context, string) ([]store.NotificationSubscription, error) {
	panic("subscription read failed")
}

func TestOnAgentHalted_RecoversPanic(t *testing.T) {
	srv, s := testServer(t)
	srv.mu.Lock()
	srv.notificationDispatcher = NewNotificationDispatcher(panickingSubscriptionStore{s}, nil, func() AgentDispatcher { return nil }, slog.Default())
	srv.mu.Unlock()

	done := srv.onAgentHalted(context.Background(), &store.Agent{ID: "a", Slug: "a"}, "")
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the halt dispatch did not finish after a panic")
	}
	srv.haltedDispatches.Wait()
}
