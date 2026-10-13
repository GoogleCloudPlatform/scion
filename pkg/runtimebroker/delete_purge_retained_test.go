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
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// A delete with purgeRetained=true removes the retained files of a
// soft-deleted agent after the soft-delete window (ptone/scion#4744), and
// only those: a live agent's files, an agent still held by a runtime entry
// and a symlinked agent directory are refused with nothing removed. These
// tests use the real agent manager, so the files are really removed.

// purgeRetainedQuery is the query the hub sends for a purge of retained
// files of agent "dev" in the linked project at root.
func purgeRetainedQuery(root string) string {
	return api.PurgeRetainedQueryParam + "=true&removeBranch=false&projectId=" + scopeProjB +
		"&projectPath=" + url.QueryEscape(root)
}

// markSoftDeleted writes the soft-delete mark into agent-info.json at info,
// as the broker's soft delete does.
func markSoftDeleted(t *testing.T, info string) {
	t.Helper()
	if err := os.WriteFile(info, []byte(`{"name":"dev","phase":"deleted","deletedAt":"2026-10-01T00:00:00Z"}`), 0o644); err != nil {
		t.Fatal(err)
	}
}

// assertRetainedRefused checks that rec is the 409 retained_files_refused
// answer, with a reason in its details containing want.
func assertRetainedRefused(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode error response %q: %v", rec.Body.String(), err)
	}
	if resp.Error.Code != api.BrokerErrCodeRetainedFilesRefused {
		t.Errorf("error code = %q, want %q", resp.Error.Code, api.BrokerErrCodeRetainedFilesRefused)
	}
	if reason, _ := resp.Error.Details[api.BrokerErrDetailReason].(string); !strings.Contains(reason, want) {
		t.Errorf("details reason = %q, want it to contain %q", reason, want)
	}
}

func TestDeleteAgent_PurgeRetained_RemovesMarkedFilesAndLogs(t *testing.T) {
	var entries []api.AgentInfo
	srv := newRealManagerScopeServer(t, &entries)
	var logs bytes.Buffer
	srv.agentLifecycleLog = slog.New(slog.NewTextHandler(&logs, nil))
	root := makeLinkedGitProject(t, scopeProjB, "dev")
	agentDir, info := writeLinkedAgentFiles(t, resolvedScionDir(t, root), "dev")
	markSoftDeleted(t, info)

	rec := doDelete(t, srv, "dev", purgeRetainedQuery(root))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(agentDir, "workspace", "work.txt")); !os.IsNotExist(err) {
		t.Fatalf("retained files were not removed (stat: %v)", err)
	}
	if _, err := os.Stat(info); !os.IsNotExist(err) {
		t.Fatalf("retained agent-info.json was not removed (stat: %v)", err)
	}
	if !strings.Contains(logs.String(), "Agent retained files removed after the soft-delete retention window") {
		t.Errorf("removal not logged; logs:\n%s", logs.String())
	}
}

func TestDeleteAgent_PurgeRetained_RefusesLiveAgentFiles(t *testing.T) {
	var entries []api.AgentInfo
	srv := newRealManagerScopeServer(t, &entries)
	root := makeLinkedGitProject(t, scopeProjB, "dev")
	// No soft-delete mark: agent-info.json says running.
	agentDir, info := writeLinkedAgentFiles(t, resolvedScionDir(t, root), "dev")

	rec := doDelete(t, srv, "dev", purgeRetainedQuery(root))
	assertRetainedRefused(t, rec, "soft-delete mark")
	if _, err := os.Stat(filepath.Join(agentDir, "workspace", "work.txt")); err != nil {
		t.Fatalf("a live agent's files were touched: %v", err)
	}
	if data, err := os.ReadFile(info); err != nil || !strings.Contains(string(data), `"running"`) {
		t.Fatalf("a live agent's agent-info.json was changed: %s %v", data, err)
	}
}

func TestDeleteAgent_PurgeRetained_RefusesWhileRuntimeEntryHoldsAgent(t *testing.T) {
	var entries []api.AgentInfo
	srv := newRealManagerScopeServer(t, &entries)
	root := makeLinkedGitProject(t, scopeProjB, "dev")
	projectDir := resolvedScionDir(t, root)
	agentDir, info := writeLinkedAgentFiles(t, projectDir, "dev")
	markSoftDeleted(t, info)
	entries = []api.AgentInfo{labelled("dev", "cid-b", scopeProjB, projectDir)}

	rec := doDelete(t, srv, "dev", purgeRetainedQuery(root))
	assertRetainedRefused(t, rec, "runtime entry")
	if _, err := os.Stat(filepath.Join(agentDir, "workspace", "work.txt")); err != nil {
		t.Fatalf("files of an agent a runtime entry holds were removed: %v", err)
	}
}

func TestDeleteAgent_PurgeRetained_DoesNotFollowSymlinkedAgentDir(t *testing.T) {
	var entries []api.AgentInfo
	srv := newRealManagerScopeServer(t, &entries)
	root := makeLinkedGitProject(t, scopeProjB, "dev")
	projectDir := resolvedScionDir(t, root)

	// The agent directory is a symlink to a directory outside the project
	// that looks like a soft-deleted agent's files.
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "workspace"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "workspace", "work.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(outside, "home"), 0o755); err != nil {
		t.Fatal(err)
	}
	markSoftDeleted(t, filepath.Join(outside, "home", "agent-info.json"))
	agentDir := filepath.Join(projectDir, "agents", "dev")
	if err := os.RemoveAll(agentDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, agentDir); err != nil {
		t.Fatal(err)
	}

	rec := doDelete(t, srv, "dev", purgeRetainedQuery(root))
	assertRetainedRefused(t, rec, "symlink")
	if _, err := os.Stat(filepath.Join(outside, "workspace", "work.txt")); err != nil {
		t.Fatalf("files outside the agent's directory were removed through a symlink: %v", err)
	}
	if fi, err := os.Lstat(agentDir); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the symlinked agent directory was removed or replaced (lstat: %v)", err)
	}
}

// A start of the agent in flight on the broker (of any run) may be the
// start of a restored agent that has not rewritten agent-info.json yet: the
// purge refuses, removes nothing and does not cancel the start.
func TestDeleteAgent_PurgeRetained_RefusesWhileStartInFlight(t *testing.T) {
	for _, tc := range []struct {
		name, launchRun, query string
	}{
		{name: "no run IDs"},
		{name: "same run", launchRun: "run-a", query: "&runId=run-a"},
		{name: "launch of a run, purge names none", launchRun: "run-b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var entries []api.AgentInfo
			srv := newRealManagerScopeServer(t, &entries)
			root := makeLinkedGitProject(t, scopeProjB, "dev")
			agentDir, info := writeLinkedAgentFiles(t, resolvedScionDir(t, root), "dev")
			markSoftDeleted(t, info)
			cancels := 0
			lr := newLaunchRecord("sync-1", "", "start", "", time.Time{}, func() { cancels++ })
			lr.RunID = tc.launchRun
			srv.launchRegistry.Begin(launchKey{ProjectID: scopeProjB, Slug: "dev"}, lr)

			rec := doDelete(t, srv, "dev", purgeRetainedQuery(root)+tc.query)
			assertRetainedRefused(t, rec, "start of the agent is in flight")
			if cancels != 0 {
				t.Errorf("in-flight start cancelled %d times by a purge", cancels)
			}
			if _, err := os.Stat(filepath.Join(agentDir, "workspace", "work.txt")); err != nil {
				t.Fatalf("files were removed while a start was in flight: %v", err)
			}
			if _, err := os.Stat(info); err != nil {
				t.Fatalf("agent-info.json was removed while a start was in flight: %v", err)
			}
		})
	}
}

func TestDeleteAgent_PurgeRetained_RejectsLocalOnly(t *testing.T) {
	var entries []api.AgentInfo
	srv := newRealManagerScopeServer(t, &entries)
	root := makeLinkedGitProject(t, scopeProjB, "dev")
	agentDir, info := writeLinkedAgentFiles(t, resolvedScionDir(t, root), "dev")
	markSoftDeleted(t, info)

	rec := doDelete(t, srv, "dev", purgeRetainedQuery(root)+"&localOnly=true")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(agentDir, "workspace", "work.txt")); err != nil {
		t.Fatalf("files were removed by a rejected request: %v", err)
	}
}

// retainedPurgeRefusal maps each refusal of checkRetainedAgentFiles to a
// reason that names no host path, and allows only a clean check.
func TestRetainedPurgeRefusal(t *testing.T) {
	orig := checkRetainedAgentFiles
	t.Cleanup(func() { checkRetainedAgentFiles = orig })

	var checkErr error
	checkRetainedAgentFiles = func(string, string) error { return checkErr }
	target := &deleteTarget{name: "dev"}

	if got := retainedPurgeRefusal(target, "/p", false); got != "" {
		t.Errorf("clean check refused: %q", got)
	}
	checkErr = agent.ErrRetainedFilesNotMarked
	if got := retainedPurgeRefusal(target, "/p", false); !strings.Contains(got, "soft-delete mark") {
		t.Errorf("unmarked files: %q", got)
	}
	checkErr = agent.ErrRetainedFilesSymlink
	if got := retainedPurgeRefusal(target, "/p", false); !strings.Contains(got, "symlink") {
		t.Errorf("symlinked dir: %q", got)
	}
	checkErr = nil
	if got := retainedPurgeRefusal(target, "", false); got == "" {
		t.Error("an unresolved project directory was allowed")
	}
	if got := retainedPurgeRefusal(&deleteTarget{name: "dev", containerID: "cid"}, "/p", false); got == "" {
		t.Error("a target held by a runtime entry was allowed")
	}
	if got := retainedPurgeRefusal(target, "/p", true); !strings.Contains(got, "in flight") {
		t.Errorf("start in flight: %q", got)
	}
}
