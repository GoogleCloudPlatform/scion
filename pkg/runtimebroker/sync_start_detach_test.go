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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// Tests for ptone/scion#4817 around the detached synchronous start: before
// the start begins, the request ending still ends the create (409
// start_cancelled, no start); once it has begun, only a stop, a delete, a
// newer start or broker shutdown ends it.
//
// A Start supplied here never calls t.Error or t.Fatal: under a fault it
// may run after the test has returned. It records into atomics instead.

// startRecorder returns a Start func that records that it ran and returns
// success at once.
func startRecorder(ran *atomic.Bool) func(context.Context, api.StartOptions) (*api.AgentInfo, error) {
	return func(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
		ran.Store(true)
		return &api.AgentInfo{ID: "c-a", Name: opts.Name, Phase: "running"}, nil
	}
}

func wantStartCancelled409(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusConflict, w.Body.String())
	}
	if code := errorCodeOf(t, w); code != api.BrokerErrCodeStartCancelled {
		t.Errorf("error code = %q, want %q", code, api.BrokerErrCodeStartCancelled)
	}
}

// The request ending while the create waits for an earlier start of the
// name ends the wait at once with 409 start_cancelled, well before the
// supersede wait would, and nothing starts.
func TestSyncCreate_RequestEndsDuringSupersedeWait_409(t *testing.T) {
	srv, mgr, projectPath, _ := newSyncStartTestServer(t)
	srv.createSupersedeWait = 3 * syncStartTestTimeout
	key := launchKey{Slug: "same-name"}
	old, _, _ := holdRecord(t, srv, key, "agent-old-id")
	var ran atomic.Bool
	mgr.starts <- startRecorder(&ran)

	reqCtx, cancelReq := context.WithCancel(context.Background())
	defer cancelReq()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- createSyncCtx(reqCtx, srv, "agent-new-id", projectPath, "") }()
	if !waitUntil(t, syncStartTestTimeout, func() bool {
		rec := registryRecord(srv, key)
		return rec != nil && rec != old
	}) {
		t.Fatal("the new create never registered under the name")
	}
	cancelReq()

	var w *httptest.ResponseRecorder
	select {
	case w = <-done:
	case <-time.After(syncStartTestTimeout):
		t.Fatal("the request ending did not end the create's supersede wait")
	}
	wantStartCancelled409(t, w)
	if ran.Load() {
		t.Error("Start ran for a create whose request ended during the supersede wait")
	}
	if rec := registryRecord(srv, key); rec != nil {
		t.Errorf("registry still holds %+v for the refused create", rec)
	}
}

// The request ending while buildStartContext runs ends a synchronous create
// with 409 start_cancelled before anything starts, and releases the name.
func TestSyncCreate_RequestEndsDuringBuild_409(t *testing.T) {
	srv, mgr, projectPath, _ := newSyncStartTestServer(t)
	key := launchKey{Slug: "same-name"}
	reqCtx, cancelReq := context.WithCancel(context.Background())
	defer cancelReq()
	buildStartContextProbe(t, "same-name", cancelReq)
	var ran atomic.Bool
	mgr.starts <- startRecorder(&ran)

	w := createSyncCtx(reqCtx, srv, "agent-new-id", projectPath, "")
	wantStartCancelled409(t, w)
	if ran.Load() {
		t.Error("Start ran for a create whose request ended during buildStartContext")
	}
	if rec := registryRecord(srv, key); rec != nil {
		t.Errorf("registry still holds %+v for the refused create", rec)
	}
}

// The request ending after the check that follows buildStartContext, but
// before the start is bound, still ends the create: from bind on the
// request no longer reaches the start, so this is the last point it may.
func TestSyncCreate_RequestEndsBeforeBind_409(t *testing.T) {
	srv, mgr, projectPath, _ := newSyncStartTestServer(t)
	key := launchKey{Slug: "same-name"}
	reqCtx, cancelReq := context.WithCancel(context.Background())
	defer cancelReq()
	testHookSyncCreateBeforeBind = func(req CreateAgentRequest) {
		if req.Name == "same-name" {
			cancelReq()
		}
	}
	t.Cleanup(func() { testHookSyncCreateBeforeBind = nil })
	var ran atomic.Bool
	mgr.starts <- startRecorder(&ran)

	w := createSyncCtx(reqCtx, srv, "agent-new-id", projectPath, "")
	wantStartCancelled409(t, w)
	if ran.Load() {
		t.Error("Start ran for a create whose request ended before the start was bound")
	}
	if rec := registryRecord(srv, key); rec != nil {
		t.Errorf("registry still holds %+v for the refused create", rec)
	}
	if owner := readLaunchMarker(projectPath, false, "same-name"); owner != "" {
		t.Errorf("the refused create left its launch marker behind (owner %q)", owner)
	}
}

// The request ending while an async create runs buildStartContext refuses
// the create at handOff with 409 start_cancelled: no launch is accepted or
// reported, and the name is released.
func TestAsyncCreate_RequestEndsBeforeHandoff_409(t *testing.T) {
	mgr := newAsyncManager()
	srv, rtb := newAsyncTestServer(t, mgr)
	key := launchKey{Slug: "agent-req-ends"}
	reqCtx, cancelReq := context.WithCancel(context.Background())
	defer cancelReq()
	buildStartContextProbe(t, "agent-req-ends", cancelReq)

	raw, err := json.Marshal(map[string]any{
		"id": "agent-req-ends-id", "name": "agent-req-ends", "asyncLaunch": true, "launchId": "L-req-ends",
		"launchTimeoutSeconds": 300, "config": map[string]any{"template": "claude"},
	})
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", bytes.NewReader(raw)).WithContext(reqCtx)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	wantStartCancelled409(t, w)
	time.Sleep(50 * time.Millisecond) // give a wrongly accepted launch a chance to report
	if n := len(rtb.getLaunchReports()); n != 0 {
		t.Errorf("%d launch report(s) sent for a refused create", n)
	}
	if rec := registryRecord(srv, key); rec != nil {
		t.Errorf("registry still holds %+v for the refused create", rec)
	}
}

// detachedStart begins a synchronous create of "same-name" whose Start
// blocks until its context is done, then ends the create's request. It
// returns a channel that receives the Start context's error when Start
// returns. The request having ended, only an explicit cancel can end the
// start. The create's handler is joined at test end.
func detachedStart(t *testing.T, srv *Server, mgr *startFuncManager, projectPath string) <-chan error {
	t.Helper()
	started := make(chan struct{})
	ended := make(chan error, 1)
	mgr.starts <- func(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
		close(started)
		<-ctx.Done()
		ended <- ctx.Err()
		return nil, ctx.Err()
	}
	reqCtx, cancelReq := context.WithCancel(context.Background())
	t.Cleanup(cancelReq)
	createDone := make(chan struct{})
	go func() {
		defer close(createDone)
		createSyncCtx(reqCtx, srv, "agent-a-id", projectPath, "")
	}()
	joinOnCleanup(t, createDone, "agent A's create")
	waitSignal(t, started, "agent A's start")
	cancelReq()
	return ended
}

// joinOnCleanup waits at test end, bounded, for done, so a handler or
// Shutdown started by the test has returned before t.Cleanup removes its
// temporary directories. A test that has already failed does not wait (the
// goroutine may be held by the fault under test).
func joinOnCleanup(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	t.Cleanup(func() {
		if t.Failed() {
			return
		}
		select {
		case <-done:
		case <-time.After(syncStartTestTimeout):
			t.Errorf("%s did not return", what)
		}
	})
}

// wantStartEnded waits for a detached start to end with context.Canceled.
func wantStartEnded(t *testing.T, ended <-chan error, by string) {
	t.Helper()
	select {
	case err := <-ended:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("agent A's start context error = %v, want context.Canceled", err)
		}
	case <-time.After(syncStartTestTimeout):
		t.Fatalf("%s did not cancel agent A's start after its request had ended", by)
	}
}

// A stop of the agent cancels a start whose request has already ended.
func TestSyncCreate_StopCancelsStartAfterRequestEnded(t *testing.T) {
	srv, mgr, projectPath, _ := newSyncStartTestServer(t)
	mgr.agents = []api.AgentInfo{{ID: "c-1", Name: "same-name", Phase: "running"}}
	ended := detachedStart(t, srv, mgr, projectPath)

	stopDone := make(chan struct{})
	go func() {
		defer close(stopDone)
		agentAction(srv, "stop")
	}()
	joinOnCleanup(t, stopDone, "the stop")
	wantStartEnded(t, ended, "a stop")
}

// A newer create of the same name cancels a start whose request has
// already ended, waits for it, and then starts.
func TestSyncCreate_SupersedingCreateCancelsStartAfterRequestEnded(t *testing.T) {
	srv, mgr, projectPath, _ := newSyncStartTestServer(t)
	srv.createSupersedeWait = 500 * time.Millisecond
	ended := detachedStart(t, srv, mgr, projectPath)

	var ran atomic.Bool
	mgr.starts <- startRecorder(&ran)
	bw := createSync(srv, "agent-b-id", projectPath)
	wantStartEnded(t, ended, "a newer create of the name")
	if bw.Code != http.StatusCreated || !ran.Load() {
		t.Errorf("agent B create status = %d (started %v), want %d and started: %s", bw.Code, ran.Load(), http.StatusCreated, bw.Body.String())
	}
}

// Broker shutdown cancels a start whose request has already ended.
func TestSyncCreate_ShutdownCancelsStartAfterRequestEnded(t *testing.T) {
	srv, mgr, projectPath, _ := newSyncStartTestServer(t)
	ended := detachedStart(t, srv, mgr, projectPath)

	shutDone := make(chan struct{})
	go func() {
		defer close(shutDone)
		_ = srv.Shutdown(context.Background())
	}()
	joinOnCleanup(t, shutDone, "Shutdown")
	wantStartEnded(t, ended, "broker shutdown")
}
