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

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Ownership-negative coverage for a flat instance (ptone/scion#3274):
// each refusal happens before any runtime call or ownership
// record is created, and the frozen target checks still come first.

func noOwnedRecord(t *testing.T, f *flatInstanceFixture, agentID string) {
	t.Helper()
	if _, ok, err := f.srv.ownership.Get(flatTestProjectID, agentID); err != nil || ok {
		t.Fatalf("an ownership record exists for %s (err %v)", agentID, err)
	}
}

func TestFlatOwnership_StartWithoutBindingIDsRefused(t *testing.T) {
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	f.seedOwnedAgent(t)
	target := `"expectedRuntimeTargetId":"` + f.identity.RuntimeTarget.ID + `"`
	for name, req := range map[string][2]string{
		"no project":  {"/api/v1/agents/test-agent-1/start", `{` + target + `,` + flatAgentEnv + `}`},
		"no agent ID": {"/api/v1/agents/test-agent-1/start" + flatStartQuery, `{` + target + `}`},
	} {
		t.Run(name, func(t *testing.T) {
			w := serveFlat(f.srv, http.MethodPost, req[0], req[1])
			if w.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409: %s", w.Code, w.Body.String())
			}
		})
	}
	if n := mgrStartCalls(f); n != 0 {
		t.Fatalf("runtime started without an ownership binding: %d", n)
	}
}

func TestFlatOwnership_CreateWithoutAgentIDRefused(t *testing.T) {
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	w := serveFlat(f.srv, http.MethodPost, "/api/v1/agents", flatCreateBody("req-no-id", "flat-agent",
		map[string]interface{}{"id": "", "expectedRuntimeTargetId": f.identity.RuntimeTarget.ID}))
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", w.Code, w.Body.String())
	}
	if n := mgrStartCalls(f); n != 0 {
		t.Fatalf("runtime started: %d", n)
	}
}

func TestFlatOwnership_ExistingAgentWithoutRecordRefused(t *testing.T) {
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	for _, path := range []string{"/api/v1/agents/test-agent-1/start" + flatStartQuery, flatRestartPath + flatStartQuery} {
		w := serveFlat(f.srv, http.MethodPost, path, `{"expectedRuntimeTargetId":"`+f.identity.RuntimeTarget.ID+`",`+flatAgentEnv+`}`)
		if w.Code != http.StatusConflict {
			t.Fatalf("%s: status = %d, want 409 (not adopted): %s", path, w.Code, w.Body.String())
		}
	}
	if stops, starts := mgrCalls(f); stops != 0 || starts != 0 {
		t.Fatalf("an unrecorded agent was stopped or started: stops=%d starts=%d", stops, starts)
	}
	noOwnedRecord(t, f, flatTestAgentID)
}

func TestFlatOwnership_ForeignSlugHolderRefused(t *testing.T) {
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	// Another agent ID holds the slug "flat-agent" in this instance.
	if err := f.srv.ownership.BeginRun(flatTestProjectID, "another-agent-id", "flat-agent", "run-x"); err != nil {
		t.Fatal(err)
	}
	w := serveFlat(f.srv, http.MethodPost, "/api/v1/agents", flatCreateBody("req-foreign", "flat-agent",
		map[string]interface{}{"expectedRuntimeTargetId": f.identity.RuntimeTarget.ID}))
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", w.Code, w.Body.String())
	}
	if n := mgrStartCalls(f); n != 0 {
		t.Fatalf("runtime started for a slug held by another agent: %d", n)
	}
	noOwnedRecord(t, f, "agent-id-flat-agent")
}

// TestFlatOwnership_TargetChecksPrecedeRecordCreation: a create refused by
// the frozen target checks creates no ownership record.
func TestFlatOwnership_TargetChecksPrecedeRecordCreation(t *testing.T) {
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	for _, extra := range []map[string]interface{}{
		{},                                   // target required
		{"expectedRuntimeTargetId": "other"}, // mismatch
		{"expectedRuntimeTargetId": f.identity.RuntimeTarget.ID, "config": map[string]interface{}{"profile": "p"}}, // profile
	} {
		w := serveFlat(f.srv, http.MethodPost, "/api/v1/agents", flatCreateBody("req-order", "flat-agent", extra))
		if w.Code < 400 {
			t.Fatalf("status = %d, want a refusal", w.Code)
		}
	}
	noOwnedRecord(t, f, "agent-id-flat-agent")
}

// TestFlatOwnership_CreateEstablishesOwnership: a genuine create (IDs, no
// preseeded record) records ownership itself.
func TestFlatOwnership_CreateEstablishesOwnership(t *testing.T) {
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	w := serveFlat(f.srv, http.MethodPost, "/api/v1/agents", flatCreateBody("req-own", "flat-agent",
		map[string]interface{}{"expectedRuntimeTargetId": f.identity.RuntimeTarget.ID, "config": map[string]interface{}{"template": "claude"}}))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", w.Code, w.Body.String())
	}
	rec, ok, err := f.srv.ownership.Get(flatTestProjectID, "agent-id-flat-agent")
	if err != nil || !ok {
		t.Fatalf("no ownership record after a create: %v", err)
	}
	if rec.AgentSlug != "flat-agent" || len(rec.Runs) != 1 {
		t.Fatalf("record = %+v", rec)
	}
}

func launchHandles() []api.ResourceHandle {
	return []api.ResourceHandle{
		{Kind: api.ResourceKindSecret, Name: "s-1", UID: "uid-secret"},
		{Kind: api.ResourceKindContainer, Name: "c-1", UID: "uid-container"},
	}
}

func flatCreateAccepted(f *flatInstanceFixture, name string) *httptest.ResponseRecorder {
	return serveFlat(f.srv, http.MethodPost, "/api/v1/agents", flatCreateBody("req-"+name, name,
		map[string]interface{}{"expectedRuntimeTargetId": f.identity.RuntimeTarget.ID, "config": map[string]interface{}{"template": "claude"}}))
}

// TestFlatOwnership_SyncStartMirrorsEveryResource: a synchronous create
// records every created object, then marks the run created.
func TestFlatOwnership_SyncStartMirrorsEveryResource(t *testing.T) {
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	f.mgr.createHandles = launchHandles()
	w := flatCreateAccepted(f, "mirror-agent")
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	rec, ok, err := f.srv.ownership.Get(flatTestProjectID, "agent-id-mirror-agent")
	if err != nil || !ok || len(rec.Runs) != 1 {
		t.Fatalf("record = %+v, %v", rec, err)
	}
	run := rec.Runs[0]
	if run.State != OwnershipStateCreated || len(run.Resources) != 2 || !rec.OwnsUID("uid-secret") || !rec.OwnsUID("uid-container") {
		t.Fatalf("run = %+v, want created with both objects", run)
	}
}

// TestFlatOwnership_FinalResourcePersistFailureUndoesStart: mirroring the
// LAST created object fails; Start itself succeeded, but the create is
// refused and exactly the journaled objects are cleaned up.
func TestFlatOwnership_FinalResourcePersistFailureUndoesStart(t *testing.T) {
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	f.mgr.createHandles = launchHandles()
	recordDir := filepath.Join(f.srv.stateDir, "ownership", flatTestProjectID)
	f.mgr.beforeCreate = func(i int) {
		if i == 1 { // the record becomes unwritable before the last object
			_ = os.Chmod(recordDir, 0o500)
		}
	}
	t.Cleanup(func() { _ = os.Chmod(recordDir, 0o700) })
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	w := flatCreateAccepted(f, "final-agent")
	if w.Code < 500 {
		t.Fatalf("status = %d, want a server error: %s", w.Code, w.Body.String())
	}
	f.mgr.mu.Lock()
	calls, handles := f.mgr.cleanupLaunchCalls, f.mgr.lastCleanupLaunchHandles
	f.mgr.mu.Unlock()
	if calls != 1 || len(handles) != 2 || handles[0].UID != "uid-secret" || handles[1].UID != "uid-container" {
		t.Fatalf("cleanup calls=%d handles=%+v, want one cleanup of exactly both journaled objects", calls, handles)
	}
	_ = os.Chmod(recordDir, 0o700)
	rec, ok, err := f.srv.ownership.Get(flatTestProjectID, "agent-id-final-agent")
	if err != nil || !ok || rec.Runs[0].State == OwnershipStateCreated {
		t.Fatalf("the run must not be marked created: %+v %v", rec, err)
	}
}

// TestFlatOwnership_LastResourceMirrorFailureUndoesStart: only recording
// the LAST created object fails (the record stays writable, so marking the
// run created would succeed): the start is still undone, through the
// latched error checked after the runtime returned, and the run is never
// marked created.
func TestFlatOwnership_LastResourceMirrorFailureUndoesStart(t *testing.T) {
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	f.mgr.createHandles = launchHandles()
	f.srv.ownership.addResourceFault = func(h api.ResourceHandle) error {
		if h.UID == "uid-container" {
			return errors.New("injected: recording the last object failed")
		}
		return nil
	}
	w := flatCreateAccepted(f, "last-agent")
	if w.Code < 500 {
		t.Fatalf("status = %d, want a server error: %s", w.Code, w.Body.String())
	}
	f.mgr.mu.Lock()
	calls, handles := f.mgr.cleanupLaunchCalls, f.mgr.lastCleanupLaunchHandles
	f.mgr.mu.Unlock()
	if calls != 1 || len(handles) != 2 || handles[0].UID != "uid-secret" || handles[1].UID != "uid-container" {
		t.Fatalf("cleanup calls=%d handles=%+v, want one cleanup of exactly both journaled objects", calls, handles)
	}
	rec, ok, err := f.srv.ownership.Get(flatTestProjectID, "agent-id-last-agent")
	if err != nil || !ok {
		t.Fatalf("record: %v %v", ok, err)
	}
	if rec.Runs[0].State == OwnershipStateCreated {
		t.Fatalf("the run was marked created although its last object was not recorded: %+v", rec.Runs[0])
	}
}

// TestFlatOwnership_PersistFailureStopsFurtherCreates: once mirroring an
// object fails, the next resource-creating call is refused.
func TestFlatOwnership_PersistFailureStopsFurtherCreates(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	f.mgr.createHandles = append(launchHandles(), api.ResourceHandle{Kind: api.ResourceKindContainer, Name: "c-2", UID: "uid-third"})
	recordDir := filepath.Join(f.srv.stateDir, "ownership", flatTestProjectID)
	var created []int
	f.mgr.beforeCreate = func(i int) {
		created = append(created, i)
		if i == 0 {
			_ = os.Chmod(recordDir, 0o500) // the first object's mirror fails
		}
	}
	t.Cleanup(func() { _ = os.Chmod(recordDir, 0o700) })
	w := flatCreateAccepted(f, "stop-agent")
	if w.Code < 400 {
		t.Fatalf("status = %d, want a refusal: %s", w.Code, w.Body.String())
	}
	// beforeCreate ran for the first and the second create attempt; the
	// checkpoint before the second refused it, so no third.
	if len(created) != 2 {
		t.Fatalf("create attempts = %v, want the second refused at its checkpoint", created)
	}
}

// TestFlatOwnership_NoBareNameFallbacks: a flat instance never passes an
// unresolved name to its runtime: a stop of an agent it cannot find is not
// a runtime call, and logs of an agent without a container are 404.
func TestFlatOwnership_NoBareNameFallbacks(t *testing.T) {
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	_ = serveFlat(f.srv, http.MethodPost, "/api/v1/agents/ghost-agent/stop", "")
	if stops, _ := mgrCalls(f); stops != 0 {
		t.Fatalf("the bare name reached the runtime: %d stops", stops)
	}

	// A file-only (no container) owned entry: no logs fallback to its name.
	f.mgr.mu.Lock()
	f.mgr.agents = append(f.mgr.agents, api.AgentInfo{Name: "files-only", Phase: "created"})
	f.mgr.mu.Unlock()
	w := serveFlat(f.srv, http.MethodGet, "/api/v1/agents/files-only/logs", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("logs of an agent without a container: status = %d, want 404: %s", w.Code, w.Body.String())
	}
}

// TestFlatOwnership_WorkspacePathNeverPicksAmongSeveral: the workspace
// lookup (no project scope in its request) refuses a name several owned
// agents match instead of taking the first.
func TestFlatOwnership_WorkspacePathNeverPicksAmongSeveral(t *testing.T) {
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	f.mgr.mu.Lock()
	f.mgr.agents = []api.AgentInfo{
		{Name: "twin", ContainerID: "c-1", ProjectPath: "/p1"},
		{Name: "twin", ContainerID: "c-2", ProjectPath: "/p2"},
	}
	f.mgr.mu.Unlock()
	if _, err := f.srv.getAgentWorkspacePath(context.Background(), "twin"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("err = %v, want an ambiguity refusal", err)
	}
}

// TestFlatOwnership_DeleteTransitionsRecord: a whole-agent delete of an
// owned agent removes or confirms gone every recorded object, marks the
// record deleted (tombstone kept) and releases the slug; a failure to
// confirm absence keeps the record deleting and the slug reserved.
func TestFlatOwnership_DeleteTransitionsRecord(t *testing.T) {
	for _, cleanupFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("cleanupFails=%v", cleanupFails), func(t *testing.T) {
			f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
			f.mgr.createHandles = launchHandles()
			if w := flatCreateAccepted(f, "del-agent"); w.Code != http.StatusCreated {
				t.Fatalf("create: %d %s", w.Code, w.Body.String())
			}
			// The runtime lists the created container with its project.
			f.mgr.mu.Lock()
			for i := range f.mgr.agents {
				if f.mgr.agents[i].Name == "del-agent" {
					f.mgr.agents[i].ContainerID = "uid-container"
					f.mgr.agents[i].Labels = map[string]string{"scion.project_id": flatTestProjectID, "scion.name": "del-agent"}
				}
			}
			f.mgr.mu.Unlock()
			if cleanupFails {
				f.mgr.cleanupLaunchErr = errors.New("cannot confirm")
			}
			confirmAbsence(f, func(api.ResourceHandle) (bool, error) { return true, nil })
			w := serveFlat(f.srv, http.MethodDelete, "/api/v1/agents/del-agent?projectId="+flatTestProjectID+"&deleteFiles=true", "")
			if w.Code >= 400 {
				t.Fatalf("delete: %d %s", w.Code, w.Body.String())
			}
			rec, ok, err := f.srv.ownership.Get(flatTestProjectID, "agent-id-del-agent")
			if err != nil || !ok {
				t.Fatalf("record: %v %v", ok, err)
			}
			holder, _ := f.srv.ownership.SlugHolder(flatTestProjectID, "del-agent")
			if cleanupFails {
				if rec.State != OwnershipStateDeleting || holder != "agent-id-del-agent" {
					t.Fatalf("state=%s holder=%q, want deleting and the slug still reserved", rec.State, holder)
				}
				return
			}
			if rec.State != OwnershipStateDeleted || holder != "" {
				t.Fatalf("state=%s holder=%q, want deleted with the slug released", rec.State, holder)
			}
			f.mgr.mu.Lock()
			handles := f.mgr.lastCleanupLaunchHandles
			f.mgr.mu.Unlock()
			if len(handles) != 2 {
				t.Fatalf("absence confirmed through %d handles, want both recorded objects", len(handles))
			}
		})
	}
}

// TestFlatOwnership_FileOnlyDeleteNeedsRecord: a file-only agent without
// this instance's record is not deleted.
func TestFlatOwnership_FileOnlyDeleteNeedsRecord(t *testing.T) {
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	f.mgr.mu.Lock()
	f.mgr.agents = append(f.mgr.agents, api.AgentInfo{Name: "orphan", Phase: "created", Labels: map[string]string{"scion.project_id": flatTestProjectID}})
	f.mgr.mu.Unlock()
	w := serveFlat(f.srv, http.MethodDelete, "/api/v1/agents/orphan?projectId="+flatTestProjectID+"&deleteFiles=true", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (not owned): %s", w.Code, w.Body.String())
	}
	f.mgr.mu.Lock()
	defer f.mgr.mu.Unlock()
	if f.mgr.deleteCalls != 0 {
		t.Fatalf("a file-only agent without a record was deleted")
	}
}

// TestFlatOwnership_ConflictingKeyRefusedOthersServed: a key another
// configured instance also claims (multi-instance conflicting ownership) is
// refused for start, create and file-only ownership on this instance, while
// its other agents are served.
func TestFlatOwnership_ConflictingKeyRefusedOthersServed(t *testing.T) {
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	f.seedOwnedAgent(t)
	f.srv.ownership.SetConflicting(map[string]bool{
		OwnershipAgentKey(flatTestProjectID, flatTestAgentID): true,
		OwnershipSlugKey(flatTestProjectID, "shared-slug"):    true,
	})
	target := `"expectedRuntimeTargetId":"` + f.identity.RuntimeTarget.ID + `"`

	w := serveFlat(f.srv, http.MethodPost, "/api/v1/agents/test-agent-1/start"+flatStartQuery, `{`+target+`,`+flatAgentEnv+`}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("start of a conflicting agent: status = %d, want 409: %s", w.Code, w.Body.String())
	}
	w = serveFlat(f.srv, http.MethodPost, "/api/v1/agents", flatCreateBody("req-shared", "shared-slug",
		map[string]interface{}{"expectedRuntimeTargetId": f.identity.RuntimeTarget.ID}))
	if w.Code != http.StatusConflict {
		t.Fatalf("create claiming a conflicting slug: status = %d, want 409: %s", w.Code, w.Body.String())
	}
	if n := mgrStartCalls(f); n != 0 {
		t.Fatalf("runtime started for a conflicting key: %d", n)
	}
	if f.srv.fileAgentOwned(flatTestProjectID, "test-agent-1") {
		t.Fatal("a file-only agent of a conflicting key is owned")
	}

	w = serveFlat(f.srv, http.MethodPost, "/api/v1/agents", flatCreateBody("req-other", "flat-agent",
		map[string]interface{}{"expectedRuntimeTargetId": f.identity.RuntimeTarget.ID, "config": map[string]interface{}{"template": "claude"}}))
	if w.Code != http.StatusCreated {
		t.Fatalf("an unrelated agent is refused: status = %d: %s", w.Code, w.Body.String())
	}
}

// TestFlatOwnership_FailedStartFinishesOnlyItsRun: a synchronous start that
// fails after creating objects has exactly those objects removed (UID
// preconditions) and only its run finished as deleted, with the objects
// confirmed absent; the record and the agent's other runs are untouched.
// When the removal fails, the run stays deleting with its objects recorded.
func TestFlatOwnership_FailedStartFinishesOnlyItsRun(t *testing.T) {
	for _, cleanupFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("cleanupFails=%v", cleanupFails), func(t *testing.T) {
			f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
			f.seedOwnedAgent(t)
			f.mgr.createHandles = launchHandles()
			f.mgr.startErrAfterCreate = errors.New("container exited immediately")
			confirmAbsence(f, func(api.ResourceHandle) (bool, error) { return true, nil })
			if cleanupFails {
				f.mgr.cleanupLaunchErr = errors.New("delete refused")
			}
			w := serveFlat(f.srv, http.MethodPost, "/api/v1/agents/test-agent-1/start"+flatStartQuery,
				`{"expectedRuntimeTargetId":"`+f.identity.RuntimeTarget.ID+`",`+flatAgentEnv+`}`)
			if w.Code < 400 {
				t.Fatalf("status %d, want a failed start: %s", w.Code, w.Body.String())
			}
			rec, ok, err := f.srv.ownership.Get(flatTestProjectID, flatTestAgentID)
			if err != nil || !ok {
				t.Fatalf("record: %v %v", ok, err)
			}
			if rec.State != OwnershipStateActive {
				t.Fatalf("record = %s, want active", rec.State)
			}
			if seed := rec.Run("seed-run"); seed == nil || seed.State != OwnershipStateCreated {
				t.Fatalf("the agent's other run changed: %+v", seed)
			}
			var failed *OwnedRun
			for i := range rec.Runs {
				if rec.Runs[i].RunID != "seed-run" {
					failed = &rec.Runs[i]
				}
			}
			if failed == nil {
				t.Fatalf("no failed run recorded: %+v", rec.Runs)
			}
			want, wantRes := OwnershipStateDeleted, OwnedResourceAbsent
			if cleanupFails {
				want, wantRes = OwnershipStateDeleting, OwnedResourceRecorded
			}
			if failed.State != want {
				t.Errorf("failed run = %s, want %s", failed.State, want)
			}
			for _, res := range failed.Resources {
				if res.State != wantRes {
					t.Errorf("object %s = %s, want %s", res.UID, res.State, wantRes)
				}
			}
			if len(failed.Resources) != 2 {
				t.Errorf("failed run recorded %d objects, want 2", len(failed.Resources))
			}
		})
	}
}

// TestFlatOwnership_AsyncLaunchCleanupFinishesOnlyItsRun: an async launch's
// successful cleanup finishes only that launch's run.
func TestFlatOwnership_AsyncLaunchCleanupFinishesOnlyItsRun(t *testing.T) {
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	f.seedOwnedAgent(t)
	st := f.srv.ownership
	if err := st.BeginRun(flatTestProjectID, flatTestAgentID, "test-agent-1", "run-async"); err != nil {
		t.Fatal(err)
	}
	h := api.ResourceHandle{Kind: api.ResourceKindContainer, Name: "test-agent-1", UID: "uid-async"}
	if err := st.AddResource(flatTestProjectID, flatTestAgentID, "run-async", h); err != nil {
		t.Fatal(err)
	}
	confirmAbsence(f, func(api.ResourceHandle) (bool, error) { return true, nil })
	rec := &launchRecord{ID: "launch-1", RunID: "run-async", Handles: []agent.ResourceHandle{h},
		ownedRun: ownedRunKey{projectID: flatTestProjectID, agentID: flatTestAgentID, runID: "run-async"}}
	f.srv.cleanupLaunchResources(f.mgr, rec)
	got, _, _ := st.Get(flatTestProjectID, flatTestAgentID)
	if r := got.Run("run-async"); r == nil || r.State != OwnershipStateDeleted || got.OwnsUID("uid-async") {
		t.Fatalf("async run after cleanup = %+v", r)
	}
	if seed := got.Run("seed-run"); seed.State != OwnershipStateCreated || got.State != OwnershipStateActive {
		t.Fatalf("other run or record changed: %+v / %s", seed, got.State)
	}
}

// plainManager is an agent.Manager that cannot be owner-scoped.
type plainManager struct{ agent.Manager }

// TestFlatInstance_UnscopableManagerFailsClosed: a flat instance whose
// manager cannot be restricted to its own objects refuses to start its
// services instead of serving every instance's agents unfiltered.
func TestFlatInstance_UnscopableManagerFailsClosed(t *testing.T) {
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	cfg := f.srv.config
	srv := New(cfg, plainManager{f.mgr}, f.srv.runtime)
	err := srv.StartServices(context.Background())
	if err == nil || !strings.Contains(err.Error(), "cannot be restricted") {
		t.Fatalf("StartServices = %v, want the fail-closed refusal", err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
}

// TestFlatOwnership_ProvisionOnlyCreateReleasesMirror: a create that never
// starts (provision only) leaves no ownership mirror behind, while its
// record (the created, not started agent) stays.
func TestFlatOwnership_ProvisionOnlyCreateReleasesMirror(t *testing.T) {
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	w := serveFlat(f.srv, http.MethodPost, "/api/v1/agents", flatCreateBody("req-prov", "prov-agent",
		map[string]interface{}{"expectedRuntimeTargetId": f.identity.RuntimeTarget.ID, "provisionOnly": true,
			"config": map[string]interface{}{"template": "claude"}}))
	if w.Code >= 300 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	n := 0
	f.srv.ownedStarts.Range(func(any, any) bool { n++; return true })
	if n != 0 {
		t.Fatalf("%d ownership mirror(s) left after a provision-only create", n)
	}
	if _, ok, err := f.srv.ownership.Get(flatTestProjectID, "agent-id-prov-agent"); err != nil || !ok {
		t.Fatalf("the created agent's record: %v %v", ok, err)
	}
}

// confirmAbsence sets the flat instance runtime's exact absence check.
func confirmAbsence(f *flatInstanceFixture, fn func(api.ResourceHandle) (bool, error)) {
	f.srv.currentRuntime().(*runtime.MockRuntime).ResourceAbsentFunc = func(_ context.Context, h api.ResourceHandle) (bool, error) { return fn(h) }
}
