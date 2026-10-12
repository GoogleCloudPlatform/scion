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
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Tests in this file replace the package variable markAgentSoftDeleted, so
// none of them may call t.Parallel.

// Some runtimes do not let the host write files in the container, so a soft
// delete writes the deleted mark to agent-info.json only after the runtime
// entry is gone (ptone/scion#4766). The fake runtime records the phase on
// disk at Stop and at Delete, which must not show the mark yet; the mark is
// written after both, with the agent-files lock still held, so no start can
// come in between.
func TestDeleteAgent_SoftDeleteMarkAfterContainerStopped(t *testing.T) {
	rt := newNameHeldRuntime("run-old")
	mgr := &k8sStyleManager{real: agent.NewManager(rt)}
	srv, home := newCleanupTestServer(t, mgr)
	scionB, _ := makeHubProject(t, home, "proj-b", scopeProjB, "dev")
	mgr.agents = []api.AgentInfo{withRun(labelled("dev", "dev", scopeProjB, scionB), "run-old")}

	var mu sync.Mutex
	var events []string
	record := func(ev string) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, ev)
	}
	rt.StopFunc = func(context.Context, runtime.RunRef) error {
		phase, _ := readDeleteMark(t, scionB, "dev")
		record("stop phase=" + phase)
		return nil
	}
	rt.DeleteFunc = func(ctx context.Context, ref runtime.RunRef) error {
		phase, _ := readDeleteMark(t, scionB, "dev")
		record("delete phase=" + phase)
		return rt.runCheckedDelete(ctx, ref)
	}
	orig := markAgentSoftDeleted
	t.Cleanup(func() { markAgentSoftDeleted = orig })
	markAgentSoftDeleted = func(name, projectPath string, deletedAt time.Time) error {
		running := "gone"
		if rt.stillPresent() {
			running = "running"
		}
		// The agent-files lock a start takes must still be held.
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		lock := "held"
		if unlock, err := srv.lockAgentFiles(ctx, projectPath, name); err == nil {
			unlock()
			lock = "free"
		}
		record("mark container=" + running + " lock=" + lock)
		return orig(name, projectPath, deletedAt)
	}

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&runId=run-old&softDelete=true&deletedAt=2026-10-03T00:00:00Z")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	want := []string{"stop phase=running", "delete phase=running", "mark container=gone lock=held"}
	mu.Lock()
	got := append([]string(nil), events...)
	mu.Unlock()
	if len(got) != len(want) {
		t.Fatalf("events = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("events = %q, want %q (agent-info.json written while the container ran?)", got, want)
		}
	}
	phase, deletedAt := readDeleteMark(t, scionB, "dev")
	if phase != "deleted" || !deletedAt.Equal(time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("after the delete: phase %q deletedAt %v, want deleted and 2026-10-03", phase, deletedAt)
	}
}

// After a successful runtime delete the mark is advisory (the hub's agent
// row records the soft delete), so a mark that cannot be written, here
// because agent-info.json is not valid JSON, is logged as a warning and the
// delete still answers 204.
func TestDeleteAgent_SoftDeleteMarkFailure_Warns204(t *testing.T) {
	rt := newNameHeldRuntime("run-old")
	mgr := &k8sStyleManager{real: agent.NewManager(rt)}
	srv, home := newCleanupTestServer(t, mgr)
	var logs bytes.Buffer
	srv.agentLifecycleLog = slog.New(slog.NewTextHandler(&logs, nil))
	scionB, _ := makeHubProject(t, home, "proj-b", scopeProjB, "dev")
	mgr.agents = []api.AgentInfo{withRun(labelled("dev", "dev", scopeProjB, scionB), "run-old")}
	infoPath := filepath.Join(config.GetAgentHomePath(scionB, "dev"), "agent-info.json")
	const corrupt = `{"name":"dev","phase":"running"`
	if err := os.WriteFile(infoPath, []byte(corrupt), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&runId=run-old&softDelete=true&deletedAt=2026-10-03T00:00:00Z")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	if rt.stillPresent() {
		t.Fatal("runtime entry not deleted; the test premise does not hold")
	}
	data, err := os.ReadFile(infoPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != corrupt {
		t.Errorf("agent-info.json changed although the mark failed: %s", data)
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "Failed to mark agent as deleted in agent-info.json") {
		t.Errorf("no warning logged for the failed mark; logs:\n%s", logs.String())
	}
}

// plainErrorManager fails DeleteTarget with an error that is not
// agent.ErrAgentFilesDelete and deletes nothing, as any lookup or runtime
// failure that is not wrapped by a sentinel would.
type plainErrorManager struct{ k8sStyleManager }

func (m *plainErrorManager) DeleteTarget(context.Context, string, runtime.RunRef, bool, string, bool) (bool, error) {
	return false, errors.New("runtime lookup failed")
}

// A delete error that is not the file-cleanup sentinel gives no positive
// signal that the container is gone, so no soft-delete mark is written.
func TestDeleteAgent_NonSentinelDeleteError_NoSoftDeleteMark(t *testing.T) {
	rt := newNameHeldRuntime("run-old")
	mgr := &plainErrorManager{k8sStyleManager{real: agent.NewManager(rt)}}
	srv, home := newCleanupTestServer(t, mgr)
	scionB, _ := makeHubProject(t, home, "proj-b", scopeProjB, "dev")
	mgr.agents = []api.AgentInfo{withRun(labelled("dev", "dev", scopeProjB, scionB), "run-old")}

	marks := countSoftDeleteMarks(t)

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&runId=run-old&softDelete=true&deletedAt=2026-10-03T00:00:00Z")
	if rec.Code < 500 {
		t.Fatalf("expected a 5xx, got %d: %s", rec.Code, rec.Body.String())
	}
	if n := marks(); n != 0 {
		t.Errorf("soft-delete mark written %d times after a non-sentinel delete error", n)
	}
	if phase, deletedAt := readDeleteMark(t, scionB, "dev"); phase != "running" || !deletedAt.IsZero() {
		t.Errorf("soft-delete mark left after a non-sentinel delete error: phase %q deletedAt %v", phase, deletedAt)
	}
}

// When the files are another run's, a soft delete of the requested run's
// container deletes the container and writes no mark on the post-delete
// path either. This guards existing behaviour (main skips the mark here
// too): it fails if the !filesOfOtherRun condition is dropped from the
// post-delete mark.
func TestDeleteAgent_FilesOfOtherRun_SoftDeleteWritesNoMark(t *testing.T) {
	rt := newNameHeldRuntime("run-old")
	mgr := &k8sStyleManager{real: agent.NewManager(rt)}
	srv, home := newCleanupTestServer(t, mgr)
	scionB, _ := makeHubProject(t, home, "proj-b", scopeProjB, "dev")
	recordRun(t, scionB, "dev", "run-new")
	mgr.agents = []api.AgentInfo{withRun(labelled("dev", "dev", scopeProjB, scionB), "run-old")}

	marks := countSoftDeleteMarks(t)

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&runId=run-old&softDelete=true&deletedAt=2026-10-03T00:00:00Z")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	if rt.stillPresent() {
		t.Fatal("the requested run's container was not deleted; the test premise does not hold")
	}
	if n := marks(); n != 0 {
		t.Errorf("soft-delete mark written %d times on files owned by another run", n)
	}
	if phase, deletedAt := readDeleteMark(t, scionB, "dev"); phase != "running" || !deletedAt.IsZero() {
		t.Errorf("another run's agent-info.json was marked: phase %q deletedAt %v", phase, deletedAt)
	}
}

// countSoftDeleteMarks wraps markAgentSoftDeleted to count its calls, and
// restores it when the test ends. The returned func reads the count.
func countSoftDeleteMarks(t *testing.T) func() int {
	t.Helper()
	orig := markAgentSoftDeleted
	t.Cleanup(func() { markAgentSoftDeleted = orig })
	var mu sync.Mutex
	n := 0
	markAgentSoftDeleted = func(name, projectPath string, deletedAt time.Time) error {
		mu.Lock()
		n++
		mu.Unlock()
		return orig(name, projectPath, deletedAt)
	}
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return n
	}
}
