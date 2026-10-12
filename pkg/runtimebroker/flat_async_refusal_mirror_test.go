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
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
