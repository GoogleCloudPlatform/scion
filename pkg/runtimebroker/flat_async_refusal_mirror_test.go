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
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// A flat instance's async create installs an ownership mirror for the run
// in buildStartContext. When beginAsyncLaunch refuses the create before
// accepting it, the mirror must be released when the request ends, so that
// no mirror stays behind and the same slug can be created again. Only an
// accepted launch hands the mirror to runLaunch.

// ownedStartCount returns the number of in-flight ownership mirrors.
func ownedStartCount(srv *Server) int {
	n := 0
	srv.ownedStarts.Range(func(any, any) bool { n++; return true })
	return n
}

// flatAsyncCreateBody is an async create of name on the flat instance; extra
// adds or overrides fields.
func flatAsyncCreateBody(f *flatInstanceFixture, name string, extra map[string]interface{}) string {
	fields := map[string]interface{}{
		"expectedRuntimeTargetId": f.identity.RuntimeTarget.ID,
		"asyncLaunch":             true,
		"launchId":                "L-" + name,
		"launchTimeoutSeconds":    300,
		"config":                  map[string]interface{}{"template": "claude"},
	}
	for k, v := range extra {
		fields[k] = v
	}
	return flatCreateBody("req-async-"+name, name, fields)
}

// assertMirrorReleasedAndSlugReusable checks that a refused async create left
// no ownership mirror, then creates the same slug again (synchronously) and
// expects it to succeed with no mirror left behind either.
func assertMirrorReleasedAndSlugReusable(t *testing.T, f *flatInstanceFixture, name string) {
	t.Helper()
	if n := ownedStartCount(f.srv); n != 0 {
		t.Fatalf("%d ownership mirror(s) left after a refused async create, want 0", n)
	}
	w := serveFlat(f.srv, http.MethodPost, "/api/v1/agents", flatCreateBody("req-again-"+name, name,
		map[string]interface{}{"expectedRuntimeTargetId": f.identity.RuntimeTarget.ID, "config": map[string]interface{}{"template": "claude"}}))
	if w.Code != http.StatusCreated {
		t.Fatalf("creating %q again: status = %d, want 201: %s", name, w.Code, w.Body.String())
	}
	if n := ownedStartCount(f.srv); n != 0 {
		t.Fatalf("%d ownership mirror(s) left after creating %q again, want 0", n, name)
	}
}

// TestFlatAsyncCreate_PreflightNotFoundReleasesOwnershipMirror: a Preflight
// that finds no template refuses the async create with 404.
func TestFlatAsyncCreate_PreflightNotFoundReleasesOwnershipMirror(t *testing.T) {
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	f.mgr.preflightErr = config.ErrTemplateNotFound

	w := serveFlat(f.srv, http.MethodPost, "/api/v1/agents", flatAsyncCreateBody(f, "async-404", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", w.Code, w.Body.String())
	}
	f.mgr.preflightErr = nil
	assertMirrorReleasedAndSlugReusable(t, f, "async-404")
}

// TestFlatAsyncCreate_PreflightRuntimeErrorReleasesOwnershipMirror: any
// other Preflight error refuses the async create with 500.
func TestFlatAsyncCreate_PreflightRuntimeErrorReleasesOwnershipMirror(t *testing.T) {
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	f.mgr.preflightErr = errors.New("runtime not ready")

	w := serveFlat(f.srv, http.MethodPost, "/api/v1/agents", flatAsyncCreateBody(f, "async-500", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", w.Code, w.Body.String())
	}
	f.mgr.preflightErr = nil
	assertMirrorReleasedAndSlugReusable(t, f, "async-500")
}

// TestFlatAsyncCreate_HarnessPolicyRefusalReleasesOwnershipMirror: a
// Preflight refused by the harness-config policy refuses the async create
// with 403 and releases the ownership mirror.
func TestFlatAsyncCreate_HarnessPolicyRefusalReleasesOwnershipMirror(t *testing.T) {
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	f.mgr.preflightErr = fmt.Errorf("resolve harness-config: %w", agent.ErrHarnessConfigPolicy)

	w := serveFlat(f.srv, http.MethodPost, "/api/v1/agents", flatAsyncCreateBody(f, "async-403", nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", w.Code, w.Body.String())
	}
	f.mgr.preflightErr = nil
	assertMirrorReleasedAndSlugReusable(t, f, "async-403")
}

// TestFlatAsyncCreate_InvalidWorkspaceDirReleasesOwnershipMirror: a GCS
// workspace upload whose directory fails validation (the hub-managed
// project directory is a symlink to a directory outside the projects root)
// refuses the async create with 400.
func TestFlatAsyncCreate_InvalidWorkspaceDirReleasesOwnershipMirror(t *testing.T) {
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	projectDir := filepath.Join(f.globalDir, "projects", "flat-project")
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(projectDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, projectDir); err != nil {
		t.Fatal(err)
	}

	w := serveFlat(f.srv, http.MethodPost, "/api/v1/agents", flatAsyncCreateBody(f, "async-400",
		map[string]interface{}{"workspaceStoragePath": "some/path", "workspaceStorageBucket": "hub-bucket"}))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "Invalid workspace directory") {
		t.Fatalf("status = %d, want 400 for the workspace directory: %s", w.Code, w.Body.String())
	}
	// The same slug in a valid project directory.
	if err := os.Remove(projectDir); err != nil {
		t.Fatal(err)
	}
	assertMirrorReleasedAndSlugReusable(t, f, "async-400")
}

// TestFlatAsyncCreate_NoWorkspaceBucketReleasesOwnershipMirror: a GCS
// workspace upload with no bucket in the request or on the broker refuses
// the async create with 422.
func TestFlatAsyncCreate_NoWorkspaceBucketReleasesOwnershipMirror(t *testing.T) {
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})

	w := serveFlat(f.srv, http.MethodPost, "/api/v1/agents", flatAsyncCreateBody(f, "async-422",
		map[string]interface{}{"workspaceStoragePath": "some/path"}))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", w.Code, w.Body.String())
	}
	assertMirrorReleasedAndSlugReusable(t, f, "async-422")
}

// TestFlatAsyncCreate_AcceptedLaunchKeepsOwnershipMirror: an accepted async
// create hands the mirror to runLaunch: it is still held after the 201
// (the launch is waiting on its claim), and runLaunch's Start consumes it.
func TestFlatAsyncCreate_AcceptedLaunchKeepsOwnershipMirror(t *testing.T) {
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	rtb := &mockRuntimeBrokerService{}
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	rtb.launchReportFunc = func(*hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		<-release
		return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
	}
	f.srv.hubMu.Lock()
	f.srv.hubConnections["hub-a"] = &HubConnection{Name: "hub-a", BrokerID: f.identity.RuntimeBrokerID, HubClient: &stubBrokerHubClient{brokers: rtb}}
	f.srv.hubMu.Unlock()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(flatAsyncCreateBody(f, "async-ok", nil)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Scion-Hub-Connection", "hub-a")
	w := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", w.Code, w.Body.String())
	}
	if n := ownedStartCount(f.srv); n != 1 {
		t.Fatalf("%d ownership mirror(s) after an accepted async create, want 1 held for the launch", n)
	}

	close(release)
	released = true
	if !waitUntil(t, 5*time.Second, func() bool { return f.mgr.StartCalls() >= 1 && ownedStartCount(f.srv) == 0 }) {
		t.Fatalf("launch Start calls = %d, ownership mirrors = %d; want Start to run and consume the mirror",
			f.mgr.StartCalls(), ownedStartCount(f.srv))
	}
	if !waitUntil(t, 5*time.Second, func() bool {
		f.srv.launchRegistry.mu.Lock()
		defer f.srv.launchRegistry.mu.Unlock()
		return len(f.srv.launchRegistry.records) == 0
	}) {
		t.Fatal("the launch did not finish")
	}
}

// acceptFlatAsyncCreate gives the flat instance a hub connection whose
// launch reports answer with claim, then makes an async create of name that
// must be accepted (201). The launch then runs in the background.
func acceptFlatAsyncCreate(t *testing.T, f *flatInstanceFixture, name string, claim func(*hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error)) {
	t.Helper()
	rtb := &mockRuntimeBrokerService{launchReportFunc: claim}
	f.srv.hubMu.Lock()
	f.srv.hubConnections["hub-a"] = &HubConnection{Name: "hub-a", BrokerID: f.identity.RuntimeBrokerID, HubClient: &stubBrokerHubClient{brokers: rtb}}
	f.srv.hubMu.Unlock()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(flatAsyncCreateBody(f, name, nil)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Scion-Hub-Connection", "hub-a")
	w := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", w.Code, w.Body.String())
	}
}

// launchEnded reports whether no launch is registered any more.
func launchEnded(f *flatInstanceFixture) bool {
	f.srv.launchRegistry.mu.Lock()
	defer f.srv.launchRegistry.mu.Unlock()
	return len(f.srv.launchRegistry.records) == 0
}

// TestFlatAsyncLaunch_LocalCancelDuringClaimReleasesOwnershipMirror: an
// accepted launch stopped locally while its claim is in flight ends before
// Start, and its ownership mirror is released.
func TestFlatAsyncLaunch_LocalCancelDuringClaimReleasesOwnershipMirror(t *testing.T) {
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	block := make(chan struct{})
	defer close(block)
	acceptFlatAsyncCreate(t, f, "launch-cancel", func(*hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		<-block
		return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
	})
	if n := ownedStartCount(f.srv); n != 1 {
		t.Fatalf("%d ownership mirror(s) after the 201, want 1 held for the launch", n)
	}

	f.srv.launchRegistry.CancelLocal(launchKey{ProjectID: flatTestProjectID, Slug: "launch-cancel"})
	// runLaunch releases the mirror after its other deferred steps (the
	// registry's Finish among them), so wait for both.
	if !waitUntil(t, 5*time.Second, func() bool { return launchEnded(f) && ownedStartCount(f.srv) == 0 }) {
		t.Fatalf("after the local cancel: launch ended = %v, ownership mirrors = %d; want the launch ended with no mirror left",
			launchEnded(f), ownedStartCount(f.srv))
	}
	if n := f.mgr.StartCalls(); n != 0 {
		t.Fatalf("Start calls = %d, want 0", n)
	}
}

// TestFlatAsyncLaunch_GateAbortReleasesOwnershipMirror: an accepted launch
// whose claim is answered 409 superseded ends before Start, and its
// ownership mirror is released.
func TestFlatAsyncLaunch_GateAbortReleasesOwnershipMirror(t *testing.T) {
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	acceptFlatAsyncCreate(t, f, "launch-abort", func(*hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		return &hubclient.AgentLaunchReportResult{HTTPStatus: http.StatusConflict, Reason: hubclient.AgentLaunchReportReasonSuperseded}, nil
	})

	// runLaunch releases the mirror after its other deferred steps (the
	// registry's Finish among them), so wait for both.
	if !waitUntil(t, 5*time.Second, func() bool { return launchEnded(f) && ownedStartCount(f.srv) == 0 }) {
		t.Fatalf("after the gate abort: launch ended = %v, ownership mirrors = %d; want the launch ended with no mirror left",
			launchEnded(f), ownedStartCount(f.srv))
	}
	if n := f.mgr.StartCalls(); n != 0 {
		t.Fatalf("Start calls = %d, want 0", n)
	}
}

// TestFlatOwnedStart_ReleaseAfterCompleteIsNoOp: releasing a mirror that
// Start's completion already consumed, or releasing it twice, changes
// nothing, and in particular leaves another run's mirror in place.
func TestFlatOwnedStart_ReleaseAfterCompleteIsNoOp(t *testing.T) {
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	optsX := api.StartOptions{RunID: "run-x"}
	optsY := api.StartOptions{RunID: "run-y"}
	f.srv.installOwnedStart(&optsX, flatTestProjectID, "agent-x")
	f.srv.installOwnedStart(&optsY, flatTestProjectID, "agent-y")
	if n := ownedStartCount(f.srv); n != 2 {
		t.Fatalf("%d ownership mirrors installed, want 2", n)
	}

	startErr := errors.New("start failed")
	if err := f.srv.completeOwnedStart(context.Background(), f.mgr, "run-x", startErr, false); !errors.Is(err, startErr) {
		t.Fatalf("completeOwnedStart = %v, want the start error", err)
	}
	f.srv.releaseOwnedStart("run-x")
	f.srv.releaseOwnedStart("run-x")
	f.srv.releaseOwnedStart("")
	if n := ownedStartCount(f.srv); n != 1 {
		t.Fatalf("%d ownership mirror(s) after releasing a consumed run twice, want 1 (run-y)", n)
	}
	if got := f.srv.ownedRunKeyFor("run-y"); got.runID != "run-y" {
		t.Fatalf("run-y's mirror = %+v, want it still in place", got)
	}
	f.srv.releaseOwnedStart("run-y")
	if n := ownedStartCount(f.srv); n != 0 {
		t.Fatalf("%d ownership mirror(s) after releasing run-y, want 0", n)
	}
}

// TestFlatOwnedStart_ReleaseKeepsALaterMirrorOfTheSameRunID: when a later
// start installed its own mirror under the same run ID, releasing the
// earlier start's mirror leaves the later one in place.
func TestFlatOwnedStart_ReleaseKeepsALaterMirrorOfTheSameRunID(t *testing.T) {
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	first := api.StartOptions{RunID: "run-same"}
	second := api.StartOptions{RunID: "run-same"}
	o1 := f.srv.installOwnedStart(&first, flatTestProjectID, "agent-same")
	o2 := f.srv.installOwnedStart(&second, flatTestProjectID, "agent-same")
	if o1 == nil || o2 == nil || o1 == o2 {
		t.Fatalf("installOwnedStart returned %p and %p, want two distinct mirrors", o1, o2)
	}

	f.srv.releaseOwnedMirror("run-same", o1)
	if v, ok := f.srv.ownedStarts.Load("run-same"); !ok || v.(*ownedStart) != o2 {
		t.Fatalf("after releasing the earlier mirror: present = %v, want the later mirror kept", ok)
	}
	f.srv.releaseOwnedMirror("run-same", nil)
	if n := ownedStartCount(f.srv); n != 1 {
		t.Fatalf("%d ownership mirror(s) after a nil release, want 1", n)
	}
	f.srv.releaseOwnedMirror("run-same", o2)
	if n := ownedStartCount(f.srv); n != 0 {
		t.Fatalf("%d ownership mirror(s) after releasing the later mirror, want 0", n)
	}
}
