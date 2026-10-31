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
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// wireSignedHub makes rtb, the test server's Hub connection, answer
// workspace download pages from h.
func wireSignedHub(rtb *mockRuntimeBrokerService, h *fakeSignedHub) {
	rtb.mu.Lock()
	defer rtb.mu.Unlock()
	rtb.workspaceDownloadFunc = func(ctx context.Context, brokerID, agentID string, req *api.WorkspaceDownloadRequest) (*api.WorkspaceDownloadResponse, error) {
		return h.fetch(ctx, CreateAgentRequest{ID: agentID}, req.Cursor)
	}
}

// forbidGCSDownload fails the test if the bucket download is ever used.
func forbidGCSDownload(t *testing.T, srv *Server) {
	t.Helper()
	srv.SetWorkspaceDownloader(func(context.Context, string, string, string) error {
		t.Error("the bucket download must not be used in the signed mode")
		return errors.New("bucket download used")
	})
}

// signedCreate is a create body with a workspace upload and the given
// download mode.
func signedCreate(name, mode string, async bool) map[string]any {
	body := bootstrapCreate(name, async)
	delete(body, "workspaceStorageBucket")
	if mode != "" {
		body["workspaceDownloadMode"] = mode
	}
	return body
}

func TestCreateAgent_WorkspaceDownloadModeAbsent_UsesGCSDownloader(t *testing.T) {
	srv, rtb := newAsyncTestServer(t, newAsyncManager())
	srv.config.WorktreeBase = t.TempDir()
	h := newFakeSignedHub(t, map[string]string{"a": "1"})
	wireSignedHub(rtb, h)
	sync := installFakeWorkspaceSync(t, srv, nil)
	w := postCreate(t, srv, bootstrapCreate("agent-mode-absent", false))
	if w.Code >= 300 {
		t.Fatalf("create = %d: %s", w.Code, w.Body.String())
	}
	if n := len(sync.Calls()); n != 1 {
		t.Fatalf("bucket download calls = %d, want 1", n)
	}
	if h.pageCalls != 0 {
		t.Fatalf("the signed download must not run without the mode (page calls %d)", h.pageCalls)
	}
}

func TestCreateAgent_WorkspaceDownloadModeSignedURLs_NeverCallsGCS(t *testing.T) {
	srv, rtb := newAsyncTestServer(t, newAsyncManager())
	srv.config.WorktreeBase = t.TempDir()
	srv.signedDownload.sleep = noSleep
	h := newFakeSignedHub(t, map[string]string{"README.md": "hi", "src/x.go": "package x"})
	var gotBroker, gotAgent string
	rtb.mu.Lock()
	rtb.workspaceDownloadFunc = func(ctx context.Context, brokerID, agentID string, req *api.WorkspaceDownloadRequest) (*api.WorkspaceDownloadResponse, error) {
		gotBroker, gotAgent = brokerID, agentID
		return h.fetch(ctx, CreateAgentRequest{}, req.Cursor)
	}
	rtb.mu.Unlock()
	forbidGCSDownload(t, srv)
	name := "agent-mode-signed"
	w := postCreate(t, srv, signedCreate(name, api.WorkspaceDownloadModeSignedURLs, false))
	if w.Code >= 300 {
		t.Fatalf("create = %d: %s", w.Code, w.Body.String())
	}
	if gotBroker != "broker-on-a" || gotAgent != name {
		t.Fatalf("page request for broker %q agent %q", gotBroker, gotAgent)
	}
	got := readTree(t, filepath.Join(srv.config.WorktreeBase, name, "workspace"))
	if got["README.md"] != "hi" || got["src/x.go"] != "package x" {
		t.Fatalf("workspace = %v", got)
	}
}

func TestCreateAgent_WorkspaceDownloadModeUnknown_FailsClosed(t *testing.T) {
	for _, async := range []bool{false, true} {
		srv, rtb := newAsyncTestServer(t, newAsyncManager())
		srv.config.WorktreeBase = t.TempDir()
		h := newFakeSignedHub(t, map[string]string{"a": "1"})
		wireSignedHub(rtb, h)
		forbidGCSDownload(t, srv)
		w := postCreate(t, srv, signedCreate("agent-mode-unknown", "presigned_v2", async))
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("async=%v: status = %d, body = %s", async, w.Code, w.Body.String())
		}
		code, msg := decodeErrorMessage(t, w)
		if code != ErrCodeWorkspaceDownloadModeUnsupported || msg != workspaceDownloadModeUnsupportedMessage {
			t.Fatalf("async=%v: code %q message %q", async, code, msg)
		}
		if h.pageCalls != 0 {
			t.Fatalf("async=%v: neither download may run (page calls %d)", async, h.pageCalls)
		}
	}
}

// TestCreateAgent_SignedEndpointMissing_FailsClosed: a Hub that answers the
// page request with 404 fails the create with fixed text, and the bucket
// download is never tried.
func TestCreateAgent_SignedEndpointMissing_FailsClosed(t *testing.T) {
	srv, _ := newAsyncTestServer(t, newAsyncManager())
	srv.config.WorktreeBase = t.TempDir()
	forbidGCSDownload(t, srv)
	w := postCreate(t, srv, signedCreate("agent-endpoint-404", api.WorkspaceDownloadModeSignedURLs, false))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if _, msg := decodeErrorMessage(t, w); msg != failedOpText(opDownloadWorkspace) {
		t.Fatalf("message = %q", msg)
	}
}

// --- Case A: a project-level workspace checked at admission. ---

// caseAFixture runs downloadWorkspaceFromGCS for a hub-managed project slug
// with h as the page source.
type caseAFixture struct {
	srv *Server
	h   *fakeSignedHub
	w   string
	req CreateAgentRequest
}

func newCaseAFixture(t *testing.T, objects map[string]string) *caseAFixture {
	t.Helper()
	srv, _ := newAsyncTestServer(t, newAsyncManager())
	h := newFakeSignedHub(t, objects)
	srv.signedDownload = signedDownloadSettings{fetchPage: h.fetch, sleep: noSleep}
	forbidGCSDownload(t, srv)
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		t.Fatal(err)
	}
	return &caseAFixture{
		srv: srv, h: h, w: filepath.Join(globalDir, "projects", "notes"),
		req: CreateAgentRequest{ID: "agent-a", Name: "agent-a", ProjectID: "proj-a", ProjectSlug: "notes",
			WorkspaceStoragePath: "workspaces/proj-a/agent-a", WorkspaceDownloadMode: api.WorkspaceDownloadModeSignedURLs,
			workspaceAdmissionChecked: true, workspaceAbsentAtAdmission: true},
	}
}

// writeMarker stands in for the start context's identity entry, written
// into the workspace before the download.
func (f *caseAFixture) writeMarker(t *testing.T, content string) {
	t.Helper()
	if err := os.MkdirAll(f.w, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.w, ".scion"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *caseAFixture) run() (bool, error) {
	var out bootstrapOutcome
	_, _, _, err := f.srv.downloadWorkspaceFromGCS(context.WithValue(context.Background(), bootstrapOutcomeKey{}, &out), f.req, api.StartOptions{})
	return out.skipped, err
}

// stages lists the stage directories beside the workspace.
func stagesOf(t *testing.T, w string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(w))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if isBootstrapStageName(e.Name(), filepath.Base(w)) {
			out = append(out, e.Name())
		}
	}
	return out
}

// crashAt makes the bootstrap stop at step as a process abort would, once.
func crashAt(t *testing.T, step string) {
	t.Helper()
	fired := false
	testHookSignedBootstrap = func(s string) error {
		if s == step && !fired {
			fired = true
			return errSignedBootstrapCrash
		}
		return nil
	}
	t.Cleanup(func() { testHookSignedBootstrap = nil })
}

// wantTree asserts dir holds exactly want (relative path to content). A
// want value of "*" accepts any content: a completed bootstrap rewrites the
// workspace marker in the identity entry.
func wantTree(t *testing.T, dir string, want map[string]string) {
	t.Helper()
	got := readTree(t, dir)
	ok := len(got) == len(want)
	for k, v := range want {
		g, present := got[k]
		if !present || (v != "*" && g != v) {
			ok = false
		}
	}
	if !ok {
		t.Fatalf("tree = %v, want %v", got, want)
	}
}

// waitFailedReport waits for rtb's failed terminal launch report.
func waitFailedReport(t *testing.T, rtb *mockRuntimeBrokerService) *hubclient.AgentLaunchReport {
	t.Helper()
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
	return failed
}

func TestSignedCaseA_CommitKeepsIdentityEntry(t *testing.T) {
	f := newCaseAFixture(t, map[string]string{"a.txt": "A", "d/b.txt": "B"})
	f.writeMarker(t, "marker")
	if skipped, err := f.run(); err != nil || skipped {
		t.Fatalf("run = (skipped %v, err %v)", skipped, err)
	}
	got := readTree(t, f.w)
	if got["a.txt"] != "A" || got["d/b.txt"] != "B" {
		t.Fatalf("tree = %v", got)
	}
	if _, ok := got[".scion"]; !ok {
		t.Fatalf("the identity entry must survive the commit: %v", got)
	}
	if s := stagesOf(t, f.w); len(s) != 0 {
		t.Fatalf("stages left: %v", s)
	}
}

func TestSignedCaseA_AbsentWorkspace(t *testing.T) {
	f := newCaseAFixture(t, map[string]string{"a.txt": "A"})
	if _, err := f.run(); err != nil {
		t.Fatal(err)
	}
	if got := readTree(t, f.w); got["a.txt"] != "A" {
		t.Fatalf("tree = %v", got)
	}
}

// TestSignedCaseA_FailedPageLeavesMarkerOnlyAndRetryApplies: a failure on
// page 2 of 3 leaves the workspace holding only its identity entry and no
// stage; the next attempt is applied, not skipped. Both failure paths are
// covered: a 404, which fails at once, and a transport error on every
// attempt, which exhausts the bounded retries (1, 2 and 4 s, injected here
// so the test does not sleep) and then fails closed.
func TestSignedCaseA_FailedPageLeavesMarkerOnlyAndRetryApplies(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		// wantCalls is the page requests of the failed run: page 1, then
		// page 2's attempts.
		wantCalls int
		wantWaits []time.Duration
	}{
		{"fail fast on 404", &apiclient.APIError{StatusCode: http.StatusNotFound}, 2, nil},
		{"retry exhaustion on transport error", errors.New("connection reset by peer"), 1 + 1 + len(defaultSignedPageBackoff), defaultSignedPageBackoff},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCaseAFixture(t, map[string]string{"1": "a", "2": "b", "3": "c"})
			var waits []time.Duration
			f.srv.signedDownload.sleep = func(ctx context.Context, d time.Duration) error {
				waits = append(waits, d)
				return ctx.Err()
			}
			f.writeMarker(t, "m")
			f.h.pageSize = 1
			f.h.pageErr = func(call int) error {
				if call >= 2 {
					return tc.err
				}
				return nil
			}
			if _, err := f.run(); err == nil {
				t.Fatal("a failed page must fail the bootstrap")
			}
			if f.h.pageCalls != tc.wantCalls {
				t.Fatalf("page calls = %d, want %d", f.h.pageCalls, tc.wantCalls)
			}
			if len(waits) != len(tc.wantWaits) || (len(waits) > 0 && fmt.Sprint(waits) != fmt.Sprint(tc.wantWaits)) {
				t.Fatalf("backoff waits = %v, want %v", waits, tc.wantWaits)
			}
			wantTree(t, f.w, map[string]string{".scion": "m"})
			if s := stagesOf(t, f.w); len(s) != 0 {
				t.Fatalf("stages left: %v", s)
			}
			f.h.mu.Lock()
			f.h.pageErr = nil
			f.h.mu.Unlock()
			if skipped, err := f.run(); err != nil || skipped {
				t.Fatalf("retry = (skipped %v, err %v); a marker-only workspace counts as absent", skipped, err)
			}
			wantTree(t, f.w, map[string]string{".scion": "*", "1": "a", "2": "b", "3": "c"})
		})
	}
}

func TestSignedCaseA_SkipsWorkspaceWithContent(t *testing.T) {
	f := newCaseAFixture(t, map[string]string{"a.txt": "A"})
	f.writeMarker(t, "m")
	if err := os.WriteFile(filepath.Join(f.w, "theirs.txt"), []byte("t"), 0o644); err != nil {
		t.Fatal(err)
	}
	skipped, err := f.run()
	if err != nil || !skipped {
		t.Fatalf("run = (skipped %v, err %v), want a skip", skipped, err)
	}
	if f.h.pageCalls != 0 {
		t.Fatalf("a skipped workspace downloads nothing (page calls %d)", f.h.pageCalls)
	}
	wantTree(t, f.w, map[string]string{".scion": "m", "theirs.txt": "t"})
}

func TestSignedCaseA_CrashPoints(t *testing.T) {
	objs := map[string]string{"a.txt": "A", "d/b.txt": "B"}
	full := map[string]string{".scion": "m", "a.txt": "A", "d/b.txt": "B"}

	t.Run("before the identity move", func(t *testing.T) {
		f := newCaseAFixture(t, objs)
		f.writeMarker(t, "m")
		crashAt(t, "before_rename")
		if _, err := f.run(); !errors.Is(err, errSignedBootstrapCrash) {
			t.Fatalf("err = %v", err)
		}
		wantTree(t, f.w, map[string]string{".scion": "m"})
		if len(stagesOf(t, f.w)) != 1 {
			t.Fatal("the crash leaves a stale stage")
		}
		if skipped, err := f.run(); err != nil || skipped {
			t.Fatalf("next = (skipped %v, err %v)", skipped, err)
		}
		wantTree(t, f.w, map[string]string{".scion": "*", "a.txt": "A", "d/b.txt": "B"})
		if s := stagesOf(t, f.w); len(s) != 0 {
			t.Fatalf("recovery left stages: %v", s)
		}
	})

	t.Run("between the two renames, recovery first: roll forward", func(t *testing.T) {
		f := newCaseAFixture(t, objs)
		f.writeMarker(t, "m")
		crashAt(t, "identity_moved")
		if _, err := f.run(); !errors.Is(err, errSignedBootstrapCrash) {
			t.Fatalf("err = %v", err)
		}
		if entries, _ := os.ReadDir(f.w); len(entries) != 0 {
			t.Fatalf("the workspace is empty between the renames: %v", entries)
		}
		pages := f.h.pageCalls
		skipped, err := f.run()
		if err != nil {
			t.Fatal(err)
		}
		wantTree(t, f.w, full)
		if !skipped || f.h.pageCalls != pages {
			t.Fatalf("after the roll forward the complete workspace is in use: skipped %v, new page calls %d", skipped, f.h.pageCalls-pages)
		}
	})

	t.Run("between the two renames, fresh marker first: stage removed and applied again", func(t *testing.T) {
		f := newCaseAFixture(t, objs)
		f.writeMarker(t, "old")
		crashAt(t, "identity_moved")
		if _, err := f.run(); !errors.Is(err, errSignedBootstrapCrash) {
			t.Fatalf("err = %v", err)
		}
		// The next create's start context writes a fresh identity entry
		// before the download step runs recovery.
		f.writeMarker(t, "fresh")
		if skipped, err := f.run(); err != nil || skipped {
			t.Fatalf("next = (skipped %v, err %v)", skipped, err)
		}
		// No mix: the old stage (with the old identity entry) is gone and the
		// listed set was applied again.
		wantTree(t, f.w, map[string]string{".scion": "*", "a.txt": "A", "d/b.txt": "B"})
		if b, _ := os.ReadFile(filepath.Join(f.w, ".scion")); string(b) == "old" {
			t.Fatal("the stale stage's identity entry must not be rolled forward")
		}
		if s := stagesOf(t, f.w); len(s) != 0 {
			t.Fatalf("stages left: %v", s)
		}
	})

	t.Run("after the rename", func(t *testing.T) {
		f := newCaseAFixture(t, objs)
		f.writeMarker(t, "m")
		crashAt(t, "after_rename")
		if _, err := f.run(); !errors.Is(err, errSignedBootstrapCrash) {
			t.Fatalf("err = %v", err)
		}
		wantTree(t, f.w, full)
	})
}

func TestSignedCaseA_RecoveryAmbiguousTouchesNothing(t *testing.T) {
	f := newCaseAFixture(t, map[string]string{"a.txt": "A"})
	if err := os.MkdirAll(f.w, 0o755); err != nil {
		t.Fatal(err)
	}
	var names []string
	for i := 0; i < 2; i++ {
		name, err := newBootstrapStageName(filepath.Base(f.w))
		if err != nil {
			t.Fatal(err)
		}
		stage := filepath.Join(filepath.Dir(f.w), name)
		if err := os.MkdirAll(stage, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stage, ".scion"), []byte("m"), 0o644); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	_, err := f.run()
	var ce *signedCommitError
	if !errors.As(err, &ce) || ce.Class != signedCommitAmbiguous {
		t.Fatalf("err = %v, want recovery_ambiguous", err)
	}
	if got := stagesOf(t, f.w); len(got) != 2 {
		t.Fatalf("stages = %v, want both untouched", got)
	}
	if f.h.pageCalls != 0 {
		t.Fatal("nothing is downloaded after an ambiguous recovery")
	}
}

// TestRemoveStaleBootstrapStages_ExactNamesNoSymlinks: a symlink named like
// a stage, and similar but different names, are neither followed nor
// removed.
func TestRemoveStaleBootstrapStages_ExactNamesNoSymlinks(t *testing.T) {
	parent := t.TempDir()
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "keep.txt"), []byte("k"), 0o644); err != nil {
		t.Fatal(err)
	}
	stale, _ := newBootstrapStageName("ws")
	link, _ := newBootstrapStageName("ws")
	similar := []string{".ws.bootstrap-short", ".ws.bootstrap-" + "ZZ" + stale[len(stale)-30:], "ws.bootstrap-" + stale[len(stale)-32:], ".other" + stale[3:]}
	for _, n := range append([]string{stale}, similar...) {
		if err := os.MkdirAll(filepath.Join(parent, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(target, filepath.Join(parent, link)); err != nil {
		t.Fatal(err)
	}
	removed, err := removeStaleBootstrapStages(parent, "ws")
	if err != nil || removed != 1 {
		t.Fatalf("removed %d, err %v; want exactly the one stale stage", removed, err)
	}
	if _, err := os.Stat(filepath.Join(parent, stale)); !os.IsNotExist(err) {
		t.Fatal("the stale stage must be removed")
	}
	for _, n := range append(similar, link) {
		if _, err := os.Lstat(filepath.Join(parent, n)); err != nil {
			t.Fatalf("%s must be left alone: %v", n, err)
		}
	}
	if _, err := os.Stat(filepath.Join(target, "keep.txt")); err != nil {
		t.Fatal("the symlink target must not be touched")
	}
}

// --- Case C: always applied in place. ---

func newCaseCFixture(t *testing.T, objects map[string]string) *caseAFixture {
	t.Helper()
	f := newCaseAFixture(t, objects)
	f.srv.config.WorktreeBase = t.TempDir()
	f.req.ProjectSlug = ""
	f.req.workspaceAdmissionChecked = false
	f.req.workspaceAbsentAtAdmission = false
	f.w = filepath.Join(f.srv.config.WorktreeBase, f.req.Name, "workspace")
	return f
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for p, c := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSignedCaseC_MirrorsListedSetAndTypeChanges(t *testing.T) {
	f := newCaseCFixture(t, map[string]string{"keep.txt": "new", "a/b": "AB", "c": "C"})
	// a is a file where a/b is listed; c is a directory with content where
	// c is listed as a file; stale.txt and old/ are extras; .scion stays.
	writeFiles(t, f.w, map[string]string{"keep.txt": "old", "a": "file", "c/inner/x": "x", "stale.txt": "s", "old/y": "y", ".scion": "id"})
	if _, err := f.run(); err != nil {
		t.Fatal(err)
	}
	wantTree(t, f.w, map[string]string{"keep.txt": "new", "a/b": "AB", "c": "C", ".scion": "id"})
	if s := stagesOf(t, f.w); len(s) != 0 {
		t.Fatalf("stages left: %v", s)
	}
}

func TestSignedCaseC_FailedDownloadLeavesWorkspaceUnchanged(t *testing.T) {
	f := newCaseCFixture(t, map[string]string{"x": "1", "y": "2"})
	before := map[string]string{"theirs.txt": "t", "x": "old"}
	writeFiles(t, f.w, before)
	f.h.objectStatus = func(p string, _ int) int {
		if p == "y" {
			return http.StatusNotFound
		}
		return 0
	}
	if _, err := f.run(); err == nil {
		t.Fatal("want a failure")
	}
	wantTree(t, f.w, before)
	if s := stagesOf(t, f.w); len(s) != 0 {
		t.Fatalf("stages left: %v", s)
	}
}

func TestSignedCaseC_CrashMidApplyThenRedispatch(t *testing.T) {
	f := newCaseCFixture(t, map[string]string{"1": "a", "2": "b", "3": "c"})
	writeFiles(t, f.w, map[string]string{"extra": "e"})
	crashAt(t, "apply_file")
	if _, err := f.run(); !errors.Is(err, errSignedBootstrapCrash) {
		t.Fatalf("err = %v", err)
	}
	if _, err := f.run(); err != nil {
		t.Fatal(err)
	}
	wantTree(t, f.w, map[string]string{"1": "a", "2": "b", "3": "c"})
	if s := stagesOf(t, f.w); len(s) != 0 {
		t.Fatalf("stages left: %v", s)
	}
}

// TestSignedBootstrap_AsyncFailureTextIsFixed: the async launch reports the
// synchronous path's fixed text for a signed download failure.
func TestSignedBootstrap_AsyncFailureTextIsFixed(t *testing.T) {
	srv, rtb := newAsyncTestServer(t, newAsyncManager())
	srv.config.WorktreeBase = t.TempDir()
	srv.signedDownload.sleep = noSleep
	forbidGCSDownload(t, srv)
	rtb.launchReportFunc = func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
	}
	w := postCreate(t, srv, signedCreate("agent-signed-async", api.WorkspaceDownloadModeSignedURLs, true))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	failed := waitFailedReport(t, rtb)
	if failed.Message != failedOpText(opDownloadWorkspace) {
		t.Fatalf("message = %q", failed.Message)
	}
}
