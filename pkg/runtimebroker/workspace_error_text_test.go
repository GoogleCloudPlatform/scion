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
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// Tests for ptone/scion#3496: a failed GCS workspace bootstrap (create) or
// workspace transfer (upload, apply, project upload) answers the client
// with fixed text only, and the cause, which can name broker paths or GCS
// bucket, object or credential detail, reaches only the broker log.

const (
	// bootstrapDetail stands in for the GCS sync error's own text: a
	// bucket/object path plus credential detail that must never reach a
	// client.
	bootstrapDetail = "gs://secret-bucket/workspaces/obj: oauth2: token for sa@proj.iam expired"

	bootstrapProjectID = "proj-3496"
	bootstrapRunID     = "run-3496"
)

// captureLifecycleJSONLog points srv's agent-lifecycle log at a JSON buffer
// and returns the buffer.
func captureLifecycleJSONLog(srv *Server) *syncBuffer {
	logs := &syncBuffer{}
	srv.agentLifecycleLog = slog.New(slog.NewJSONHandler(logs, nil))
	return logs
}

// findLogRecord returns the first JSON log record with msg whose fields
// include every key/value in match, or nil.
func findLogRecord(logs, msg string, match map[string]string) map[string]any {
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		var r map[string]any
		if json.Unmarshal([]byte(line), &r) != nil || r["msg"] != msg {
			continue
		}
		ok := true
		for k, v := range match {
			if r[k] != v {
				ok = false
				break
			}
		}
		if ok {
			return r
		}
	}
	return nil
}

// assertWorkspaceOpLogged checks the broker logged a "runtime op failed"
// record for op whose error carries detail and whose key field is want.
func assertWorkspaceOpLogged(t *testing.T, logs, op, key, want, detail string) {
	t.Helper()
	rec := findLogRecord(logs, "runtime op failed", map[string]string{"op": op})
	if rec == nil {
		t.Fatalf("no runtime op failed record for op %q; logs: %s", op, logs)
	}
	if got, _ := rec["error"].(string); !strings.Contains(got, detail) {
		t.Errorf("log record error = %q, want it to carry the detail %q", got, detail)
	}
	if rec[key] != want {
		t.Errorf("log record %s = %v, want %q", key, rec[key], want)
	}
}

// assertBootstrapLogged checks the broker logged the GCS workspace
// bootstrap failure for op with the detail, agent, project and run.
func assertBootstrapLogged(t *testing.T, logs, op, agentID, detail string) {
	t.Helper()
	rec := findLogRecord(logs, "GCS workspace bootstrap failed", map[string]string{"op": op})
	if rec == nil {
		t.Fatalf("no GCS workspace bootstrap failed record for op %q; logs: %s", op, logs)
	}
	if got, _ := rec["error"].(string); !strings.Contains(got, detail) {
		t.Errorf("log record error = %q, want it to carry the detail %q", got, detail)
	}
	for key, want := range map[string]string{"agent_id": agentID, "project_id": bootstrapProjectID, "run_id": bootstrapRunID} {
		if rec[key] != want {
			t.Errorf("log record %s = %v, want %q", key, rec[key], want)
		}
	}
}

// decodeErrorMessage returns the code and message of w's JSON error body.
func decodeErrorMessage(t *testing.T, w *httptest.ResponseRecorder) (string, string) {
	t.Helper()
	var resp ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode error body: %v (status %d, body %s)", err, w.Code, w.Body.String())
	}
	return resp.Error.Code, resp.Error.Message
}

// bootstrapCreate returns a create body carrying a GCS workspace upload.
func bootstrapCreate(name string, async bool) map[string]any {
	body := map[string]any{
		"id": name, "name": name, "projectId": bootstrapProjectID, "runId": bootstrapRunID,
		"workspaceStoragePath": "some/path", "workspaceStorageBucket": "hub-bucket",
		"config": map[string]any{"template": "claude"},
	}
	if async {
		body["asyncLaunch"] = true
		body["launchId"] = "L-" + name
		body["launchTimeoutSeconds"] = 300
	}
	return body
}

// blockWorkspaceDir makes WorktreeBase read-only, so creating
// <WorktreeBase>/<name>/workspace fails with an os error that names the
// broker path. The directory still passes validation (it does not exist
// yet). It returns that path.
func blockWorkspaceDir(t *testing.T, srv *Server, name string) string {
	t.Helper()
	srv.config.WorktreeBase = t.TempDir()
	if err := os.Chmod(srv.config.WorktreeBase, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(srv.config.WorktreeBase, 0o755) })
	probe := filepath.Join(srv.config.WorktreeBase, "probe")
	if err := os.Mkdir(probe, 0o755); err == nil {
		t.Skip("running with permissions that ignore directory modes")
	}
	return filepath.Join(srv.config.WorktreeBase, name)
}

// TestGCSBootstrapError_FixedText covers a create's GCS workspace bootstrap
// failing at directory creation or at the GCS download: the synchronous
// 500 and the async failed report carry the same fixed text, with no path,
// bucket or error detail, and the detail reaches the broker log with the
// agent, project and run.
func TestGCSBootstrapError_FixedText(t *testing.T) {
	cases := []struct {
		name     string
		op       string
		wantText string
		// setup arranges the failure and returns the detail that must
		// reach the log and not the client.
		setup func(t *testing.T, srv *Server, name string) string
	}{
		{
			name: "mkdir", op: opCreateWorkspaceDir, wantText: "Failed to create workspace directory",
			setup: func(t *testing.T, srv *Server, name string) string {
				installFakeWorkspaceSync(t, nil)
				return blockWorkspaceDir(t, srv, name)
			},
		},
		{
			name: "gcs-sync", op: opDownloadWorkspace, wantText: "Failed to download workspace from GCS",
			setup: func(t *testing.T, srv *Server, name string) string {
				srv.config.WorktreeBase = t.TempDir()
				installFakeWorkspaceSync(t, errors.New(bootstrapDetail))
				return bootstrapDetail
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Synchronous create.
			syncName := "agent-3496-sync-" + tc.name
			mgr := newAsyncManager()
			srv, _ := newAsyncTestServer(t, mgr)
			logs := captureLifecycleJSONLog(srv)
			detail := tc.setup(t, srv, syncName)

			w := postCreate(t, srv, bootstrapCreate(syncName, false))
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("sync create: status = %d, body = %s", w.Code, w.Body.String())
			}
			code, syncText := decodeErrorMessage(t, w)
			if code != ErrCodeRuntimeError {
				t.Errorf("sync code = %q, want %q", code, ErrCodeRuntimeError)
			}
			if syncText != tc.wantText {
				t.Errorf("sync message = %q, want %q", syncText, tc.wantText)
			}
			assertBootstrapLogged(t, logs.String(), tc.op, syncName, detail)

			// Async create.
			asyncName := "agent-3496-async-" + tc.name
			mgr = newAsyncManager()
			srv, rtb := newAsyncTestServer(t, mgr)
			logs = captureLifecycleJSONLog(srv)
			rtb.launchReportFunc = func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
				return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
			}
			detail = tc.setup(t, srv, asyncName)

			w = postCreate(t, srv, bootstrapCreate(asyncName, true))
			if w.Code != http.StatusCreated {
				t.Fatalf("async create: status = %d, body = %s", w.Code, w.Body.String())
			}
			var failed *hubclient.AgentLaunchReport
			if !waitUntil(t, 5*time.Second, func() bool {
				for _, r := range rtb.getLaunchReports() {
					if r.Report.State == hubclient.AgentLaunchReportStateFailed {
						failed = r.Report
						return true
					}
				}
				return false
			}) {
				t.Fatalf("no failed terminal report; reports: %+v", rtb.getLaunchReports())
			}
			if failed.ErrorCode != "runtime_error" {
				t.Errorf("async code = %q, want runtime_error", failed.ErrorCode)
			}
			if failed.Message != syncText {
				t.Errorf("async message = %q, want it byte-identical to the sync message %q", failed.Message, syncText)
			}
			assertBootstrapLogged(t, logs.String(), tc.op, asyncName, detail)
			if n := mgr.StartCallCount(); n != 0 {
				t.Errorf("Start must never be called after a failed bootstrap, got %d calls", n)
			}
		})
	}
}

// TestGCSBootstrapError_GlobalDirFixedText covers the hub-managed bootstrap
// failing to resolve the global dir: the client gets the fixed text, and
// the detail is logged.
func TestGCSBootstrapError_GlobalDirFixedText(t *testing.T) {
	// An unset HOME makes config.GetGlobalDir (os.UserHomeDir) fail.
	t.Setenv("HOME", "")
	srv := &Server{}
	logs := captureLifecycleJSONLog(srv)
	req := CreateAgentRequest{
		ID: "agent-3496-gd", Name: "agent-3496-gd", ProjectID: bootstrapProjectID, RunID: bootstrapRunID,
		ProjectSlug: "notes", WorkspaceStoragePath: "some/path",
	}
	_, attemptMsg, httpMessage, err := srv.downloadWorkspaceFromGCS(context.Background(), req, api.StartOptions{})
	if err == nil {
		t.Fatal("expected config.GetGlobalDir to fail with HOME unset")
	}
	if httpMessage != "Failed to get global dir" {
		t.Errorf("httpMessage = %q, want the fixed text", httpMessage)
	}
	if attemptMsg != "failed to resolve global dir" {
		t.Errorf("attemptMsg = %q, want it unchanged", attemptMsg)
	}
	rec := findLogRecord(logs.String(), "GCS workspace bootstrap failed", map[string]string{"op": opGetGlobalDir})
	if rec == nil {
		t.Fatalf("no bootstrap failure record; logs: %s", logs.String())
	}
	if got, _ := rec["error"].(string); got == "" || !strings.Contains(err.Error(), got) {
		t.Errorf("log record error = %q, want the cause (%q)", got, err.Error())
	}
}

// TestGCSBootstrapError_ClientRefusalsUnchanged pins the two intentional
// client texts the bootstrap keeps: the invalid-workspace-dir 400 and the
// unconfigured-bucket 422 (ptone/scion#3422), on the sync and async paths.
func TestGCSBootstrapError_ClientRefusalsUnchanged(t *testing.T) {
	for _, async := range []bool{false, true} {
		mode := "sync"
		if async {
			mode = "async"
		}
		t.Run("invalid-dir-"+mode, func(t *testing.T) {
			name := "agent-3496-invalid-" + mode
			srv, _ := newAsyncTestServer(t, newAsyncManager())
			symlinkedWorktreeAgentDir(t, srv, name)
			installFakeWorkspaceSync(t, nil)
			_, verr := runtime.ValidateWorkspaceSource(filepath.Join(srv.config.WorktreeBase, name, "workspace"), srv.config.WorktreeBase)
			if verr == nil {
				t.Fatal("expected the symlinked workspace directory to fail validation")
			}
			w := postCreate(t, srv, bootstrapCreate(name, async))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
			}
			code, msg := decodeErrorMessage(t, w)
			if code != ErrCodeInvalidRequest {
				t.Errorf("code = %q, want %q", code, ErrCodeInvalidRequest)
			}
			if want := "Invalid workspace directory: " + verr.Error(); msg != want {
				t.Fatalf("message = %q, want the unchanged invalid-directory text %q", msg, want)
			}
		})
		t.Run("no-bucket-"+mode, func(t *testing.T) {
			name := "agent-3496-nobucket-" + mode
			srv, _ := newAsyncTestServer(t, newAsyncManager())
			srv.config.WorktreeBase = t.TempDir()
			installFakeWorkspaceSync(t, nil)
			body := bootstrapCreate(name, async)
			delete(body, "workspaceStorageBucket")
			w := postCreate(t, srv, body)
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, body = %s; want 422", w.Code, w.Body.String())
			}
			code, msg := decodeErrorMessage(t, w)
			if code != ErrCodeWorkspaceStorageUnconfigured {
				t.Errorf("code = %q, want %q", code, ErrCodeWorkspaceStorageUnconfigured)
			}
			if msg != pinnedWorkspaceStorageUnconfiguredText {
				t.Errorf("message = %q, want the unchanged text %q", msg, pinnedWorkspaceStorageUnconfiguredText)
			}
		})
	}
}

// pinnedWorkspaceStorageUnconfiguredText is the 422 text from
// ptone/scion#3422, spelled out so a change to the constant is caught.
const pinnedWorkspaceStorageUnconfiguredText = "Cannot download the uploaded workspace: the create request names no storage bucket and this runtime broker has no storage bucket configured. " +
	"Update the hub so it sends the workspace bucket, or configure the broker's storage bucket (storage.bucket or --storage-bucket)."

// fakeWorkspaceTransfers replaces the workspace handlers' GCS calls.
type fakeWorkspaceTransfers struct {
	toErr, fromErr, manifestErr error
}

// installWorkspaceTransfers substitutes f's errors for the GCS upload,
// download and manifest upload for the test.
func installWorkspaceTransfers(t *testing.T, f fakeWorkspaceTransfers) {
	t.Helper()
	prevTo, prevFrom, prevManifest := syncWorkspaceToGCS, syncWorkspaceFromGCS, uploadWorkspaceManifest
	syncWorkspaceToGCS = func(context.Context, string, string, string) error { return f.toErr }
	syncWorkspaceFromGCS = func(context.Context, string, string, string) error { return f.fromErr }
	uploadWorkspaceManifest = func(context.Context, string, string, *transfer.Manifest) error { return f.manifestErr }
	t.Cleanup(func() {
		syncWorkspaceToGCS, syncWorkspaceFromGCS, uploadWorkspaceManifest = prevTo, prevFrom, prevManifest
	})
}

// installFailingWorkspaceTransfers fails the test if a handler reaches any
// GCS call.
func installFailingWorkspaceTransfers(t *testing.T) {
	t.Helper()
	prevTo, prevFrom, prevManifest := syncWorkspaceToGCS, syncWorkspaceFromGCS, uploadWorkspaceManifest
	syncWorkspaceToGCS = func(context.Context, string, string, string) error {
		t.Error("unexpected GCS upload")
		return nil
	}
	syncWorkspaceFromGCS = func(context.Context, string, string, string) error {
		t.Error("unexpected GCS download")
		return nil
	}
	uploadWorkspaceManifest = func(context.Context, string, string, *transfer.Manifest) error {
		t.Error("unexpected manifest upload")
		return nil
	}
	t.Cleanup(func() {
		syncWorkspaceToGCS, syncWorkspaceFromGCS, uploadWorkspaceManifest = prevTo, prevFrom, prevManifest
	})
}

// unreadableSubdir creates dir/locked with no permissions, so walking dir
// fails with an error that names the broker path.
func unreadableSubdir(t *testing.T, dir string) string {
	t.Helper()
	locked := filepath.Join(dir, "locked")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	if _, err := os.ReadDir(locked); err == nil {
		t.Skip("running with permissions that ignore directory modes")
	}
	return locked
}

// TestWorkspaceHandlerErrors_FixedText covers the workspace upload
// (sync-to), apply (sync-from) and project upload (finalize) handlers: each
// runtime failure answers with the fixed "Failed to <op>" text only, and
// the cause reaches the broker log with the agent or project.
func TestWorkspaceHandlerErrors_FixedText(t *testing.T) {
	const slug = "agent-3496"
	gcsErr := errors.New(bootstrapDetail)

	// agentServer returns a server whose agent slug resolves (through the
	// WorktreeBase fallback) to a real workspace directory, or, when
	// listErr is set, whose agent listing fails with it.
	agentServer := func(t *testing.T, listErr error) (*Server, string) {
		base := filepath.Join(t.TempDir(), "worktrees")
		ws := filepath.Join(base, slug)
		if err := os.MkdirAll(ws, 0o755); err != nil {
			t.Fatal(err)
		}
		cfg := DefaultServerConfig()
		cfg.StorageBucket = "test-bucket"
		cfg.WorktreeBase = base
		var mgr *mockAgentManager
		if listErr != nil {
			mgr = &mockAgentManager{}
		} else {
			mgr = &mockAgentManager{agents: []api.AgentInfo{{Name: slug, ContainerID: slug}}}
		}
		rt := &runtime.MockRuntime{
			NameFunc:             func() string { return "docker" },
			GetWorkspacePathFunc: func(context.Context, string) (string, error) { return "", nil },
		}
		srv := New(cfg, &listErrManager{mockAgentManager: mgr, err: listErr}, rt)
		return srv, ws
	}
	agentRequest := func(handler http.HandlerFunc, path string, body any) *httptest.ResponseRecorder {
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(b)))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec
	}
	upload := func(srv *Server) *httptest.ResponseRecorder {
		return agentRequest(srv.handleWorkspaceUpload, "/api/v1/workspace/upload", WorkspaceUploadRequest{Slug: slug, StoragePath: "workspaces/p/a"})
	}
	apply := func(srv *Server) *httptest.ResponseRecorder {
		return agentRequest(srv.handleWorkspaceApply, "/api/v1/workspace/apply", WorkspaceApplyRequest{Slug: slug, StoragePath: "workspaces/p/a"})
	}

	cases := []struct {
		name     string
		op       string
		wantText string
		logKey   string
		logValue string
		// run arranges the failure, sends the request, and returns the
		// response plus the detail that must reach only the log.
		run func(t *testing.T) (*httptest.ResponseRecorder, *syncBuffer, string)
	}{
		{
			name: "upload/resolve", op: opResolveWorkspacePath, wantText: "Failed to resolve workspace path",
			logKey: "agent_slug", logValue: slug,
			run: func(t *testing.T) (*httptest.ResponseRecorder, *syncBuffer, string) {
				srv, _ := agentServer(t, errors.New("docker ps: /var/run/docker.sock: permission denied"))
				logs := captureLifecycleJSONLog(srv)
				installFailingWorkspaceTransfers(t)
				return upload(srv), logs, "/var/run/docker.sock"
			},
		},
		{
			name: "upload/manifest", op: opBuildWorkspaceManifest, wantText: "Failed to build workspace manifest",
			logKey: "agent_slug", logValue: slug,
			run: func(t *testing.T) (*httptest.ResponseRecorder, *syncBuffer, string) {
				srv, ws := agentServer(t, nil)
				logs := captureLifecycleJSONLog(srv)
				installFailingWorkspaceTransfers(t)
				locked := unreadableSubdir(t, ws)
				return upload(srv), logs, filepath.Base(locked)
			},
		},
		{
			name: "upload/gcs", op: opUploadWorkspace, wantText: "Failed to upload workspace to GCS",
			logKey: "agent_slug", logValue: slug,
			run: func(t *testing.T) (*httptest.ResponseRecorder, *syncBuffer, string) {
				srv, _ := agentServer(t, nil)
				logs := captureLifecycleJSONLog(srv)
				installWorkspaceTransfers(t, fakeWorkspaceTransfers{toErr: gcsErr})
				return upload(srv), logs, bootstrapDetail
			},
		},
		{
			name: "upload/manifest-upload", op: opUploadManifest, wantText: "Failed to upload manifest",
			logKey: "agent_slug", logValue: slug,
			run: func(t *testing.T) (*httptest.ResponseRecorder, *syncBuffer, string) {
				srv, _ := agentServer(t, nil)
				logs := captureLifecycleJSONLog(srv)
				installWorkspaceTransfers(t, fakeWorkspaceTransfers{manifestErr: gcsErr})
				return upload(srv), logs, bootstrapDetail
			},
		},
		{
			name: "apply/resolve", op: opResolveWorkspacePath, wantText: "Failed to resolve workspace path",
			logKey: "agent_slug", logValue: slug,
			run: func(t *testing.T) (*httptest.ResponseRecorder, *syncBuffer, string) {
				srv, _ := agentServer(t, errors.New("docker ps: /var/run/docker.sock: permission denied"))
				logs := captureLifecycleJSONLog(srv)
				installFailingWorkspaceTransfers(t)
				return apply(srv), logs, "/var/run/docker.sock"
			},
		},
		{
			name: "apply/gcs", op: opDownloadWorkspace, wantText: "Failed to download workspace from GCS",
			logKey: "agent_slug", logValue: slug,
			run: func(t *testing.T) (*httptest.ResponseRecorder, *syncBuffer, string) {
				srv, _ := agentServer(t, nil)
				logs := captureLifecycleJSONLog(srv)
				installWorkspaceTransfers(t, fakeWorkspaceTransfers{fromErr: gcsErr})
				return apply(srv), logs, bootstrapDetail
			},
		},
		{
			name: "project-upload/manifest", op: opBuildWorkspaceManifest, wantText: "Failed to build workspace manifest",
			logKey: "project_id", logValue: bootstrapProjectID,
			run: func(t *testing.T) (*httptest.ResponseRecorder, *syncBuffer, string) {
				srv, _ := agentServer(t, nil)
				logs := captureLifecycleJSONLog(srv)
				installFailingWorkspaceTransfers(t)
				ws := t.TempDir()
				locked := unreadableSubdir(t, ws)
				return doProjectUploadRequest(t, srv, ProjectWorkspaceUploadRequest{
					ProjectID: bootstrapProjectID, StoragePath: "workspaces/p/w", WorkspacePath: ws,
				}), logs, filepath.Base(locked)
			},
		},
		{
			name: "project-upload/gcs", op: opUploadWorkspace, wantText: "Failed to upload workspace to GCS",
			logKey: "project_id", logValue: bootstrapProjectID,
			run: func(t *testing.T) (*httptest.ResponseRecorder, *syncBuffer, string) {
				srv, _ := agentServer(t, nil)
				logs := captureLifecycleJSONLog(srv)
				installWorkspaceTransfers(t, fakeWorkspaceTransfers{toErr: gcsErr})
				return doProjectUploadRequest(t, srv, ProjectWorkspaceUploadRequest{
					ProjectID: bootstrapProjectID, StoragePath: "workspaces/p/w", WorkspacePath: t.TempDir(),
				}), logs, bootstrapDetail
			},
		},
		{
			name: "project-upload/manifest-upload", op: opUploadManifest, wantText: "Failed to upload manifest",
			logKey: "project_id", logValue: bootstrapProjectID,
			run: func(t *testing.T) (*httptest.ResponseRecorder, *syncBuffer, string) {
				srv, _ := agentServer(t, nil)
				logs := captureLifecycleJSONLog(srv)
				installWorkspaceTransfers(t, fakeWorkspaceTransfers{manifestErr: gcsErr})
				return doProjectUploadRequest(t, srv, ProjectWorkspaceUploadRequest{
					ProjectID: bootstrapProjectID, StoragePath: "workspaces/p/w", WorkspacePath: t.TempDir(),
				}), logs, bootstrapDetail
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, logs, detail := tc.run(t)
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
			}
			code, msg := decodeErrorMessage(t, w)
			if code != ErrCodeRuntimeError {
				t.Errorf("code = %q, want %q", code, ErrCodeRuntimeError)
			}
			if msg != tc.wantText {
				t.Errorf("message = %q, want %q", msg, tc.wantText)
			}
			assertWorkspaceOpLogged(t, logs.String(), tc.op, tc.logKey, tc.logValue, detail)
		})
	}
}

// listErrManager is a mockAgentManager whose List fails with err when set.
type listErrManager struct {
	*mockAgentManager
	err error
}

func (m *listErrManager) List(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.mockAgentManager.List(ctx, filter)
}

// runLaunchTerminal runs lc's launch to completion and returns its single
// terminal report.
func runLaunchTerminal(t *testing.T, srv *Server, rtb *mockRuntimeBrokerService, lc launchCtx) *hubclient.AgentLaunchReport {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := newLaunchRecord("L-"+lc.key.Slug, lc.key.Slug, store.LaunchKindCreate, "", time.Now().Add(time.Hour), cancel)
	done := make(chan struct{})
	go func() {
		srv.runLaunch(ctx, rec, lc)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runLaunch did not return")
	}
	var terminals []*hubclient.AgentLaunchReport
	for _, r := range rtb.getLaunchReports() {
		if r.Report.State == hubclient.AgentLaunchReportStateSucceeded || r.Report.State == hubclient.AgentLaunchReportStateFailed {
			terminals = append(terminals, r.Report)
		}
	}
	if len(terminals) != 1 {
		t.Fatalf("expected exactly one terminal, got %d: %+v", len(terminals), terminals)
	}
	return terminals[0]
}

// TestRunLaunch_NoBucketReportsSyncText covers runLaunch's own download
// finding no bucket for the upload (admission normally refuses first): the
// failed report carries the synchronous 422's text, under runtime_error,
// not the sentinel error's text (ptone/scion#3496).
func TestRunLaunch_NoBucketReportsSyncText(t *testing.T) {
	mgr := newAsyncManager()
	srv, rtb := newAsyncTestServer(t, mgr)
	rtb.launchReportFunc = func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
	}
	srv.config.WorktreeBase = t.TempDir()
	srv.config.StorageBucket = ""
	installFakeWorkspaceSync(t, nil)
	const name = "agent-3496-runlaunch-nobucket"

	got := runLaunchTerminal(t, srv, rtb, launchCtx{
		req:  CreateAgentRequest{Name: name, WorkspaceStoragePath: "some/path"},
		opts: api.StartOptions{Name: name, ProjectPath: t.TempDir()},
		mgr:  mgr,
		key:  launchKey{Slug: name},
	})
	if got.State != hubclient.AgentLaunchReportStateFailed || got.ErrorCode != "runtime_error" {
		t.Fatalf("terminal = %+v, want failed with runtime_error", got)
	}
	if got.Message != pinnedWorkspaceStorageUnconfiguredText {
		t.Fatalf("terminal message = %q, want the synchronous 422 text %q", got.Message, pinnedWorkspaceStorageUnconfiguredText)
	}
	if n := mgr.StartCallCount(); n != 0 {
		t.Fatalf("Start must never be called without a workspace bucket, got %d calls", n)
	}
}

// homeUnsetDetail is os.UserHomeDir's error text with HOME unset, the
// cause the global-dir failures below must log and not send.
const homeUnsetDetail = "$HOME is not defined"

// TestCreateAgent_GlobalDirError_FixedText covers createAgent failing to
// resolve the global dir for a hub-managed project: fixed text at the
// client, cause in the broker log with the agent, project and run.
func TestCreateAgent_GlobalDirError_FixedText(t *testing.T) {
	srv, _ := newAsyncTestServer(t, newAsyncManager())
	logs := captureLifecycleJSONLog(srv)
	t.Setenv("HOME", "")
	const name = "agent-3496-create-gd"

	w := postCreate(t, srv, map[string]any{
		"id": name, "name": name, "projectId": bootstrapProjectID, "runId": bootstrapRunID,
		"projectSlug": "notes", "config": map[string]any{"template": "claude"},
	})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	code, msg := decodeErrorMessage(t, w)
	if code != ErrCodeRuntimeError || msg != "Failed to get global dir" {
		t.Fatalf("error = %q %q, want runtime_error with the fixed text", code, msg)
	}
	rec := findLogRecord(logs.String(), "Create failed to resolve the global dir", map[string]string{"op": opGetGlobalDir})
	if rec == nil {
		t.Fatalf("no global dir failure record; logs: %s", logs.String())
	}
	if got, _ := rec["error"].(string); !strings.Contains(got, homeUnsetDetail) {
		t.Errorf("log record error = %q, want it to carry %q", got, homeUnsetDetail)
	}
	for key, want := range map[string]string{"agent_id": name, "project_id": bootstrapProjectID, "run_id": bootstrapRunID} {
		if rec[key] != want {
			t.Errorf("log record %s = %v, want %q", key, rec[key], want)
		}
	}
}

// TestDeleteProject_GlobalDirError_FixedText covers deleteProject failing
// to resolve the global dir: fixed text at the client, cause in the broker
// log with the project slug.
func TestDeleteProject_GlobalDirError_FixedText(t *testing.T) {
	cfg := DefaultServerConfig()
	srv := New(cfg, &mockAgentManager{}, &runtime.MockRuntime{NameFunc: func() string { return "docker" }})
	logs := captureLifecycleJSONLog(srv)
	t.Setenv("HOME", "")

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/projects/notes", nil)
	w := httptest.NewRecorder()
	srv.handleProjectBySlug(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	code, msg := decodeErrorMessage(t, w)
	if code != ErrCodeRuntimeError || msg != "Failed to get global dir" {
		t.Fatalf("error = %q %q, want runtime_error with the fixed text", code, msg)
	}
	assertWorkspaceOpLogged(t, logs.String(), opGetGlobalDir, "project_slug", "notes", homeUnsetDetail)
}
