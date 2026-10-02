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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// syncStartTestTimeout is a failure guard only: every wait below is on an
// explicit signal, and this bounds how long a broken build hangs.
const syncStartTestTimeout = 10 * time.Second

// startFuncManager is a mockManager whose Start is supplied per call by the
// test, in call order, so a test can hold one start blocked while another
// runs.
type startFuncManager struct {
	*mockManager
	starts chan func(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error)
}

func (m *startFuncManager) Start(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
	fn := <-m.starts
	return fn(ctx, opts)
}

func newSyncStartTestServer(t *testing.T) (*Server, *startFuncManager, string, string) {
	t.Helper()
	projectPath := filepath.Join(t.TempDir(), ".scion")
	agentDir := filepath.Join(projectPath, "agents", "same-name")
	mgr := &startFuncManager{
		mockManager: &mockManager{},
		starts:      make(chan func(context.Context, api.StartOptions) (*api.AgentInfo, error), 4),
	}
	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	return New(cfg, mgr, rt), mgr, projectPath, agentDir
}

// createSync issues a synchronous create for "same-name" and returns the
// response once the handler has fully returned (including deferred work).
func createSync(srv *Server, agentID, projectPath string) *httptest.ResponseRecorder {
	body := fmt.Sprintf(`{"id": %q, "name": "same-name", "projectPath": %q, "config": {"task": "t"}}`, agentID, projectPath)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

// writeAgentFiles stands in for provisioning: it creates the named agent's
// directory with a file identifying which agent wrote it.
func writeAgentFiles(t *testing.T, agentDir, owner string) {
	t.Helper()
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Errorf("mkdir agent dir: %v", err)
		return
	}
	if err := os.WriteFile(filepath.Join(agentDir, "scion-agent.yaml"), []byte("owner: "+owner+"\n"), 0644); err != nil {
		t.Errorf("write agent file: %v", err)
	}
}

func readAgentOwner(agentDir string) string {
	data, err := os.ReadFile(filepath.Join(agentDir, "scion-agent.yaml"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(string(data), "owner:"))
}

func waitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(syncStartTestTimeout):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// A start that fails after a newer start has taken the same agent name must
// not remove the newer agent's files. The newer owner here is a marker
// written for the name by another start (as a replica sharing the storage,
// or an async launch, would), with that agent's files in place.
func TestSyncCreateStartFailure_SkipsCleanupWhenNameReowned(t *testing.T) {
	srv, mgr, projectPath, agentDir := newSyncStartTestServer(t)

	started := make(chan struct{})
	release := make(chan struct{})
	mgr.starts <- func(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
		writeAgentFiles(t, agentDir, "agent-a")
		close(started)
		<-release // ignores ctx: models a start stuck in a call that does not return early
		return nil, errors.New("required skill could not be resolved")
	}

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- createSync(srv, "agent-a-id", projectPath) }()
	waitSignal(t, started, "agent A's start")

	// A newer agent B takes the name and provisions its own files.
	if err := writeLaunchMarker(projectPath, false, "same-name", "launch-b"); err != nil {
		t.Fatalf("writeLaunchMarker: %v", err)
	}
	writeAgentFiles(t, agentDir, "agent-b")

	close(release)
	var w *httptest.ResponseRecorder
	select {
	case w = <-done:
	case <-time.After(syncStartTestTimeout):
		t.Fatal("timed out waiting for agent A's create to return")
	}
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("agent A create status = %d, want %d: %s", w.Code, http.StatusInternalServerError, w.Body.String())
	}

	if got := readAgentOwner(agentDir); got != "agent-b" {
		t.Fatalf("agent B's files were removed by agent A's failure cleanup (owner file now %q)", got)
	}
	if got := readLaunchMarker(projectPath, false, "same-name"); got != "launch-b" {
		t.Errorf("name marker = %q, want agent B's %q left in place", got, "launch-b")
	}
}

// A start that fails while it still owns the name cleans up its own files,
// as before, and releases its marker.
func TestSyncCreateStartFailure_CleansUpWhenStillOwner(t *testing.T) {
	srv, mgr, projectPath, agentDir := newSyncStartTestServer(t)

	mgr.starts <- func(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
		writeAgentFiles(t, agentDir, "agent-a")
		return nil, errors.New("auth resolution failed")
	}

	w := createSync(srv, "agent-a-id", projectPath)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusInternalServerError, w.Body.String())
	}
	if _, err := os.Stat(agentDir); !os.IsNotExist(err) {
		t.Fatalf("agent directory still exists after a start failure while owning the name (stat err %v)", err)
	}
	if got := readLaunchMarker(projectPath, false, "same-name"); got != "" {
		t.Errorf("name marker = %q after the create ended, want it removed", got)
	}
}

// A successful start releases its marker when the create ends, so a later
// failure of an older start can never match it.
func TestSyncCreateSuccess_ReleasesMarker(t *testing.T) {
	srv, mgr, projectPath, agentDir := newSyncStartTestServer(t)

	var markerDuringStart string
	mgr.starts <- func(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
		markerDuringStart = readLaunchMarker(projectPath, false, "same-name")
		writeAgentFiles(t, agentDir, "agent-a")
		return &api.AgentInfo{ID: "c1", Name: opts.Name, Phase: "running"}, nil
	}

	w := createSync(srv, "agent-a-id", projectPath)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusCreated, w.Body.String())
	}
	if !strings.HasPrefix(markerDuringStart, "sync-") {
		t.Errorf("name marker during start = %q, want this start's own owner value", markerDuringStart)
	}
	if got := readLaunchMarker(projectPath, false, "same-name"); got != "" {
		t.Errorf("name marker = %q after the create ended, want it removed", got)
	}
	if got := readAgentOwner(agentDir); got != "agent-a" {
		t.Errorf("agent files missing after a successful create (owner %q)", got)
	}
}

// Deleting an agent whose start is blocked cancels the start promptly --
// even though the delete itself finds nothing to remove yet (404) -- and a
// newer agent created with the same name afterwards keeps its files.
func TestDeleteAgent_CancelsBlockedSyncStart(t *testing.T) {
	srv, mgr, projectPath, agentDir := newSyncStartTestServer(t)

	started := make(chan struct{})
	var aCtxErr error
	mgr.starts <- func(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
		writeAgentFiles(t, agentDir, "agent-a")
		close(started)
		<-ctx.Done() // blocked in provisioning until cancelled
		aCtxErr = ctx.Err()
		return nil, fmt.Errorf("required skill could not be resolved: %w", ctx.Err())
	}

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- createSync(srv, "agent-a-id", projectPath) }()
	waitSignal(t, started, "agent A's start")

	del := httptest.NewRequest(http.MethodDelete, "/api/v1/agents/same-name?deleteFiles=true", nil)
	dw := httptest.NewRecorder()
	srv.Handler().ServeHTTP(dw, del)
	if dw.Code != http.StatusNotFound {
		t.Fatalf("delete status = %d, want %d (no listable entry yet): %s", dw.Code, http.StatusNotFound, dw.Body.String())
	}

	var w *httptest.ResponseRecorder
	select {
	case w = <-done:
	case <-time.After(syncStartTestTimeout):
		t.Fatal("agent A's start was not cancelled by the delete")
	}
	if !errors.Is(aCtxErr, context.Canceled) {
		t.Errorf("agent A's start context error = %v, want context.Canceled", aCtxErr)
	}
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("agent A create status = %d, want %d: %s", w.Code, http.StatusInternalServerError, w.Body.String())
	}

	// A newer agent B with the same name starts and runs.
	mgr.starts <- func(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
		writeAgentFiles(t, agentDir, "agent-b")
		return &api.AgentInfo{ID: "c-b", Name: opts.Name, Phase: "running"}, nil
	}
	if bw := createSync(srv, "agent-b-id", projectPath); bw.Code != http.StatusCreated {
		t.Fatalf("agent B create status = %d, want %d: %s", bw.Code, http.StatusCreated, bw.Body.String())
	}
	if got := readAgentOwner(agentDir); got != "agent-b" {
		t.Fatalf("agent B's files are missing (owner file %q)", got)
	}
}

// A second create for the same name on this broker cancels the first start
// and waits for its failure cleanup to finish before taking the name, so
// that cleanup only ever sees the first agent's own files and the second
// agent's files survive.
func TestSyncCreate_SameNameSupersedesAndWaitsForCleanup(t *testing.T) {
	srv, mgr, projectPath, agentDir := newSyncStartTestServer(t)

	aStarted := make(chan struct{})
	mgr.starts <- func(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
		writeAgentFiles(t, agentDir, "agent-a")
		close(aStarted)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	aDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { aDone <- createSync(srv, "agent-a-id", projectPath) }()
	waitSignal(t, aStarted, "agent A's start")

	// When B's start runs, A's failure cleanup must already have removed
	// A's files: B waited for it rather than provisioning alongside it.
	ownerAtBStart := "unset"
	mgr.starts <- func(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
		ownerAtBStart = readAgentOwner(agentDir)
		writeAgentFiles(t, agentDir, "agent-b")
		return &api.AgentInfo{ID: "c-b", Name: opts.Name, Phase: "running"}, nil
	}
	if bw := createSync(srv, "agent-b-id", projectPath); bw.Code != http.StatusCreated {
		t.Fatalf("agent B create status = %d, want %d: %s", bw.Code, http.StatusCreated, bw.Body.String())
	}
	if ownerAtBStart != "" {
		t.Errorf("agent B's start ran before agent A's cleanup had finished (agent files owned by %q at B's start)", ownerAtBStart)
	}
	select {
	case aw := <-aDone:
		if aw.Code != http.StatusInternalServerError {
			t.Errorf("agent A create status = %d, want %d", aw.Code, http.StatusInternalServerError)
		}
	case <-time.After(syncStartTestTimeout):
		t.Fatal("timed out waiting for agent A's create to return")
	}
	if got := readAgentOwner(agentDir); got != "agent-b" {
		t.Fatalf("agent B's files are missing (owner file %q)", got)
	}
}

// If the superseded start does not finish within the wait, the newer create
// fails instead of provisioning over files the older start may be removing.
func TestSyncCreate_SupersedeWaitTimeoutFails(t *testing.T) {
	srv, mgr, projectPath, agentDir := newSyncStartTestServer(t)
	srv.syncStartSupersedeWait = time.Millisecond

	aStarted := make(chan struct{})
	releaseA := make(chan struct{})
	mgr.starts <- func(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
		writeAgentFiles(t, agentDir, "agent-a")
		close(aStarted)
		<-releaseA // ignores cancellation
		return nil, errors.New("failed")
	}
	aDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { aDone <- createSync(srv, "agent-a-id", projectPath) }()
	waitSignal(t, aStarted, "agent A's start")

	mgr.starts <- func(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
		t.Error("agent B's start ran although agent A had not finished")
		return nil, errors.New("unexpected start")
	}
	bw := createSync(srv, "agent-b-id", projectPath)
	if bw.Code != http.StatusInternalServerError {
		t.Fatalf("agent B create status = %d, want a runtime error: %s", bw.Code, bw.Body.String())
	}
	if !strings.Contains(bw.Body.String(), errSyncStartSupersedeTimeout.Error()) {
		t.Errorf("agent B create body = %s, want the supersede-timeout error", bw.Body.String())
	}
	close(releaseA)
	select {
	case <-aDone:
	case <-time.After(syncStartTestTimeout):
		t.Fatal("timed out waiting for agent A's create to return")
	}
}

// beginSyncStart refuses a slug that is not a single path element before it
// registers anything or touches the filesystem.
func TestBeginSyncStart_RejectsInvalidSlug(t *testing.T) {
	for _, slug := range []string{"../x", "a/b", "", "."} {
		t.Run(fmt.Sprintf("%q", slug), func(t *testing.T) {
			srv, _, projectPath, _ := newSyncStartTestServer(t)
			req := CreateAgentRequest{ID: "agent-id", Slug: slug}
			_, ss, err := srv.beginSyncStart(context.Background(), req, api.StartOptions{ProjectPath: projectPath})
			if !errors.Is(err, errInvalidLaunchSlug) {
				t.Fatalf("beginSyncStart error = %v, want errInvalidLaunchSlug", err)
			}
			if ss != nil {
				t.Fatal("beginSyncStart returned a start for an invalid slug")
			}
			if _, statErr := os.Stat(filepath.Join(projectPath, "launch-markers")); !os.IsNotExist(statErr) {
				t.Errorf("launch markers directory was created for an invalid slug (stat err %v)", statErr)
			}
			srv.launchRegistry.mu.Lock()
			n := len(srv.launchRegistry.records)
			srv.launchRegistry.mu.Unlock()
			if n != 0 {
				t.Errorf("registry holds %d records after an invalid slug, want 0", n)
			}
		})
	}
}

// createAgent rejects a non-empty slug that is not a single path element
// with 400, before any start runs.
func TestCreateAgent_RejectsInvalidSlug(t *testing.T) {
	for _, slug := range []string{"../x", "a/b", "."} {
		t.Run(fmt.Sprintf("%q", slug), func(t *testing.T) {
			srv, _, projectPath, _ := newSyncStartTestServer(t)
			body := fmt.Sprintf(`{"id": "agent-id", "name": "same-name", "slug": %q, "projectPath": %q, "config": {"task": "t"}}`, slug, projectPath)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusBadRequest, w.Body.String())
			}
		})
	}
}

// Stop and delete must not panic on a server built without a launch
// registry (for example, one constructed directly rather than via New).
func TestStopAndDelete_NilLaunchRegistry(t *testing.T) {
	srv, _, _, _ := newSyncStartTestServer(t)
	srv.launchRegistry = nil

	for _, tc := range []struct{ method, path string }{
		{http.MethodDelete, "/api/v1/agents/same-name"},
		{http.MethodDelete, "/api/v1/agents/same-name?projectId=p1&deleteFiles=true"},
		{http.MethodPost, "/api/v1/agents/same-name/stop"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("handler panicked with a nil launch registry: %v", r)
				}
			}()
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
			if w.Code >= 500 {
				t.Errorf("status = %d: %s", w.Code, w.Body.String())
			}
		})
	}
}
