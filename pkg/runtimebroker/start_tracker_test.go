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

package runtimebroker

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

func TestStartTracker_BeginKeysFinish(t *testing.T) {
	tr := newStartTracker()
	k := launchKey{ProjectID: "p", Slug: "a"}
	_, f1 := tr.begin(context.Background(), k)
	_, f2 := tr.begin(context.Background(), k)
	_, f3 := tr.begin(context.Background(), launchKey{ProjectID: "p", Slug: "b"})
	if got := tr.keys(); !reflect.DeepEqual(got, []launchKey{k, {ProjectID: "p", Slug: "b"}}) {
		t.Fatalf("keys = %v", got)
	}
	f1()
	f1() // idempotent
	if got := tr.keys(); len(got) != 2 {
		t.Fatalf("a second start of the same agent must keep it listed: %v", got)
	}
	f2()
	f3()
	if got := tr.keys(); len(got) != 0 {
		t.Fatalf("keys after finish = %v", got)
	}

	var nilTracker *startTracker
	ctx, f := nilTracker.begin(context.Background(), k)
	f()
	if ctx == nil || nilTracker.keys() != nil || !nilTracker.cancelAndWait(context.Background(), k) {
		t.Fatal("a nil tracker tracks nothing")
	}
}

func TestStartTracker_CancelAndWait(t *testing.T) {
	tr := newStartTracker()
	k := launchKey{ProjectID: "p", Slug: "a"}
	ctx, finish := tr.begin(context.Background(), k)
	cleaned := make(chan struct{})
	go func() {
		<-ctx.Done()
		time.Sleep(20 * time.Millisecond) // cleanup
		close(cleaned)
		finish()
	}()
	if !tr.cancelAndWait(context.Background(), k) {
		t.Fatal("cancelAndWait should report the start finished")
	}
	select {
	case <-cleaned:
	default:
		t.Fatal("cancelAndWait returned before the start's cleanup finished")
	}

	// A start that ignores cancellation is bounded by the wait context.
	_, stuckFinish := tr.begin(context.Background(), k)
	defer stuckFinish()
	waitCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if tr.cancelAllAndWait(waitCtx) {
		t.Fatal("a start that never finishes must time out the wait")
	}
}

// racingListManager models a start finishing during the inventory listing:
// the listing sees no container, and the start then completes (its
// container is created and it leaves the in-flight set) before anything
// else is read.
type racingListManager struct {
	heartbeatMockManager
	finish func()
	once   sync.Once
}

func (m *racingListManager) List(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
	agents, err := m.heartbeatMockManager.List(ctx, filter)
	m.once.Do(m.finish)
	return agents, err
}

// The in-flight set is read before the agents are listed, so a start that
// completes between the two reads is still reported in flight.
func TestHeartbeat_StartsInFlightReadBeforeListing(t *testing.T) {
	tr := newStartTracker()
	k := launchKey{ProjectID: "p1", Slug: "a1"}
	_, finish := tr.begin(context.Background(), k)
	mgr := &racingListManager{finish: finish}

	client := &mockRuntimeBrokerService{}
	svc := NewHeartbeatService(client, "b", time.Hour, mgr, nil, slog.Default())
	svc.startsInFlight = tr.keys
	hb := lastHeartbeat(t, svc, client)

	listed := heartbeatAgentTargets(hb)
	inFlight := false
	for _, s := range hb.StartsInFlight {
		if s.ProjectID == k.ProjectID && s.Slug == k.Slug {
			inFlight = true
		}
	}
	if _, ok := listed[k.Slug]; !ok && !inFlight {
		t.Fatal("a start finishing during the listing is neither listed nor reported in flight")
	}
	if !hb.Capabilities.StartsInFlight {
		t.Fatal("the heartbeat must advertise the starts_in_flight capability")
	}
}

func TestHeartbeat_StartsInFlightCapabilityAndProjectFilter(t *testing.T) {
	client := &mockRuntimeBrokerService{}
	svc := NewHeartbeatService(client, "b", time.Hour, &heartbeatMockManager{}, nil, slog.Default())
	hb := lastHeartbeat(t, svc, client)
	if hb.Capabilities.StartsInFlight || len(hb.StartsInFlight) != 0 {
		t.Fatal("without an in-flight source the heartbeat claims nothing")
	}

	svc = NewHeartbeatService(client, "b", time.Hour, &heartbeatMockManager{}, func(p string) bool { return p == "mine" }, slog.Default())
	svc.startsInFlight = func() []launchKey {
		return []launchKey{{ProjectID: "mine", Slug: "a"}, {ProjectID: "other", Slug: "b"}}
	}
	if err := svc.ForceHeartbeat(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := client.getHeartbeatCalls()
	hb = calls[len(calls)-1].Heartbeat
	want := []hubclient.StartInFlight{{ProjectID: "mine", Slug: "a"}}
	if !reflect.DeepEqual(hb.StartsInFlight, want) {
		t.Fatalf("StartsInFlight = %v, want %v (other hubs' projects filtered out)", hb.StartsInFlight, want)
	}
}

// blockedStart queues a Start on mgr that signals started, then blocks
// until its context is cancelled or release is closed.
func blockedStart(mgr *startFuncManager, started chan<- struct{}, release <-chan struct{}, cleanup func()) {
	mgr.starts <- func(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
		close(started)
		select {
		case <-ctx.Done():
			if cleanup != nil {
				cleanup()
			}
			return nil, ctx.Err()
		case <-release:
			return &api.AgentInfo{ID: "c-1", Name: opts.Name, Phase: "running"}, nil
		}
	}
}

func agentAction(srv *Server, action string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/same-name/"+action, strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

func snapshotHas(srv *Server, slug string) bool {
	for _, k := range srv.startsInFlightSnapshot() {
		if k.Slug == slug {
			return true
		}
	}
	return false
}

// Starts on the create, start and restart handlers are all reported in
// flight until Manager.Start has returned.
func TestStartsInFlight_CoversCreateStartRestart(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(srv *Server, projectPath string) *httptest.ResponseRecorder
	}{
		{"create", func(srv *Server, projectPath string) *httptest.ResponseRecorder {
			return createSync(srv, "agent-id", projectPath)
		}},
		{"start", func(srv *Server, _ string) *httptest.ResponseRecorder { return agentAction(srv, "start") }},
		{"restart", func(srv *Server, _ string) *httptest.ResponseRecorder { return agentAction(srv, "restart") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, mgr, projectPath, _ := newSyncStartTestServer(t)
			started, release := make(chan struct{}), make(chan struct{})
			blockedStart(mgr, started, release, nil)
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- tc.run(srv, projectPath) }()
			waitSignal(t, started, "the start")
			if !snapshotHas(srv, "same-name") {
				t.Fatal("a running start is not reported in flight")
			}
			close(release)
			select {
			case <-done:
			case <-time.After(syncStartTestTimeout):
				t.Fatal("start did not return")
			}
			if snapshotHas(srv, "same-name") {
				t.Fatal("a finished start is still reported in flight")
			}
		})
	}
}

// A stop cancels a start of the same agent still running on this broker and
// waits for its cleanup before stopping, so the start cannot create a
// container after the stop.
func TestStopAgent_CancelsAndWaitsForInFlightStart(t *testing.T) {
	srv, mgr, _, _ := newSyncStartTestServer(t)
	mgr.agents = []api.AgentInfo{{ID: "c-1", Name: "same-name", Phase: "running"}}
	started := make(chan struct{})
	var stopsDuringCleanup int
	blockedStart(mgr, started, make(chan struct{}), func() {
		time.Sleep(30 * time.Millisecond)
		stopsDuringCleanup = mgr.StopCalls()
	})
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- agentAction(srv, "start") }()
	waitSignal(t, started, "the start")

	stopDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { stopDone <- agentAction(srv, "stop") }()
	select {
	case <-done:
	case <-time.After(syncStartTestTimeout):
		t.Fatal("the stop did not cancel the in-flight start")
	}
	sw := <-stopDone
	if sw.Code != http.StatusAccepted && sw.Code != http.StatusOK {
		t.Fatalf("stop status = %d: %s", sw.Code, sw.Body.String())
	}
	if stopsDuringCleanup != 0 {
		t.Fatal("the stop ran before the cancelled start finished its cleanup")
	}
	if mgr.StopCalls() != 1 {
		t.Fatalf("stop calls = %d, want 1", mgr.StopCalls())
	}
}

// Shutdown cancels every start still running on the create, start and
// restart handlers and waits for its cleanup.
func TestShutdown_CancelsAndWaitsForInFlightStarts(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(srv *Server, projectPath string) *httptest.ResponseRecorder
	}{
		{"create", func(srv *Server, projectPath string) *httptest.ResponseRecorder {
			return createSync(srv, "agent-id", projectPath)
		}},
		{"start", func(srv *Server, _ string) *httptest.ResponseRecorder { return agentAction(srv, "start") }},
		{"restart", func(srv *Server, _ string) *httptest.ResponseRecorder { return agentAction(srv, "restart") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, mgr, projectPath, _ := newSyncStartTestServer(t)
			started := make(chan struct{})
			cleaned := make(chan struct{})
			blockedStart(mgr, started, make(chan struct{}), func() {
				time.Sleep(20 * time.Millisecond)
				close(cleaned)
			})
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- tc.run(srv, projectPath) }()
			waitSignal(t, started, "the start")

			shut := make(chan error, 1)
			go func() { shut <- srv.Shutdown(context.Background()) }()
			select {
			case err := <-shut:
				if err != nil && !errors.Is(err, http.ErrServerClosed) {
					t.Fatalf("Shutdown: %v", err)
				}
			case <-time.After(syncStartTestTimeout):
				t.Fatal("Shutdown did not cancel the in-flight start")
			}
			select {
			case <-cleaned:
			default:
				t.Fatal("Shutdown returned before the in-flight start finished its cleanup")
			}
			select {
			case <-done:
			case <-time.After(syncStartTestTimeout):
				t.Fatal("start handler did not return")
			}
			if snapshotHas(srv, "same-name") {
				t.Fatal("a cancelled start is still reported in flight")
			}
		})
	}
}

// A synchronous create stays tracked in flight while its sync-start
// bookkeeping finishes, so Shutdown and Stop wait for all of it.
func TestStartsInFlight_CreateTrackedThroughSyncStartFinish(t *testing.T) {
	srv, mgr, projectPath, _ := newSyncStartTestServer(t)
	var trackedAtFinish bool
	testHookSyncStartFinish = func(key launchKey) {
		for _, k := range srv.startsInFlight.keys() {
			if k == key {
				trackedAtFinish = true
			}
		}
	}
	t.Cleanup(func() { testHookSyncStartFinish = nil })
	mgr.starts <- func(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
		return &api.AgentInfo{ID: "c-1", Name: opts.Name, Phase: "running"}, nil
	}
	if w := createSync(srv, "agent-id", projectPath); w.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", w.Code, w.Body.String())
	}
	if !trackedAtFinish {
		t.Fatal("the start left the in-flight tracker before its sync-start bookkeeping finished")
	}
}
