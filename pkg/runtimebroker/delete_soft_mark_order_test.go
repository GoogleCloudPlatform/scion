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
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
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

// When the mark cannot be written after a successful runtime delete, the
// broker answers 500 so the hub retries. The retry finds the files only (the
// container is gone) and writes the mark.
func TestDeleteAgent_SoftDeleteMarkFailure_RetryWritesMark(t *testing.T) {
	rt := newNameHeldRuntime("run-old")
	mgr := &k8sStyleManager{real: agent.NewManager(rt)}
	srv, home := newCleanupTestServer(t, mgr)
	scionB, _ := makeHubProject(t, home, "proj-b", scopeProjB, "dev")
	mgr.agents = []api.AgentInfo{withRun(labelled("dev", "dev", scopeProjB, scionB), "run-old")}

	orig := markAgentSoftDeleted
	t.Cleanup(func() { markAgentSoftDeleted = orig })
	markAgentSoftDeleted = func(string, string, time.Time) error { return errors.New("agent home not writable") }

	query := "projectId=" + scopeProjB + "&runId=run-old&softDelete=true&deletedAt=2026-10-03T00:00:00Z"
	rec := doDelete(t, srv, "dev", query)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 when the mark fails, got %d: %s", rec.Code, rec.Body.String())
	}
	if rt.stillPresent() {
		t.Fatal("runtime entry not deleted; the test premise does not hold")
	}
	if phase, _ := readDeleteMark(t, scionB, "dev"); phase != "running" {
		t.Fatalf("phase %q after the failed mark, want running", phase)
	}

	// The hub retries the same soft delete; the container is gone now.
	markAgentSoftDeleted = orig
	mgr.agents = nil
	rec = doDelete(t, srv, "dev", query)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("retry: expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	phase, deletedAt := readDeleteMark(t, scionB, "dev")
	if phase != "deleted" || !deletedAt.Equal(time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("after the retry: phase %q deletedAt %v, want deleted and 2026-10-03", phase, deletedAt)
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

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&runId=run-old&softDelete=true&deletedAt=2026-10-03T00:00:00Z")
	if rec.Code < 500 {
		t.Fatalf("expected a 5xx, got %d: %s", rec.Code, rec.Body.String())
	}
	if phase, deletedAt := readDeleteMark(t, scionB, "dev"); phase != "running" || !deletedAt.IsZero() {
		t.Errorf("soft-delete mark written after a non-sentinel delete error: phase %q deletedAt %v", phase, deletedAt)
	}
}

// When the files are another run's, a soft delete of the requested run's
// container deletes the container and writes no mark on the post-delete
// path either.
func TestDeleteAgent_FilesOfOtherRun_SoftDeleteWritesNoMark(t *testing.T) {
	rt := newNameHeldRuntime("run-old")
	mgr := &k8sStyleManager{real: agent.NewManager(rt)}
	srv, home := newCleanupTestServer(t, mgr)
	scionB, _ := makeHubProject(t, home, "proj-b", scopeProjB, "dev")
	recordRun(t, scionB, "dev", "run-new")
	mgr.agents = []api.AgentInfo{withRun(labelled("dev", "dev", scopeProjB, scionB), "run-old")}

	orig := markAgentSoftDeleted
	t.Cleanup(func() { markAgentSoftDeleted = orig })
	marks := 0
	markAgentSoftDeleted = func(name, projectPath string, deletedAt time.Time) error {
		marks++
		return orig(name, projectPath, deletedAt)
	}

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&runId=run-old&softDelete=true&deletedAt=2026-10-03T00:00:00Z")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	if rt.stillPresent() {
		t.Fatal("the requested run's container was not deleted; the test premise does not hold")
	}
	if marks != 0 {
		t.Errorf("soft-delete mark written %d times on files owned by another run", marks)
	}
	if phase, deletedAt := readDeleteMark(t, scionB, "dev"); phase != "running" || !deletedAt.IsZero() {
		t.Errorf("another run's agent-info.json was marked: phase %q deletedAt %v", phase, deletedAt)
	}
}
