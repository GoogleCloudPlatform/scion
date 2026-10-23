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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// Tests for the re-link of a hub-managed git workspace together with the
// broker's record of the workspace's .scion entry (workspace_config_record.go):
// the re-link runs inside withCheckedWorkspaceConfig
// (relinkSharedProjectIdentityChecked).

// hubNativeRelink is a hub-managed git workspace (~/.scion/projects/proj)
// whose .scion/project-id names relinkOldID, with the agent's state in the
// earlier project config directory, which records the workspace.
type hubNativeRelink struct {
	srv              *Server
	mgr              *hubDefaultsCapturingManager
	root, dotScion   string
	oldRoot, newRoot string
}

func newHubNativeRelink(t *testing.T) *hubNativeRelink {
	t.Helper()
	clearSCIONEnv(t)
	t.Setenv("HOME", t.TempDir())
	srv, mgr := newHubDefaultsWiringServer()
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(globalDir, "projects", "proj")
	dotScion := filepath.Join(root, config.DotScion)
	if err := os.MkdirAll(dotScion, 0755); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteProjectID(dotScion, relinkOldID); err != nil {
		t.Fatal(err)
	}
	oldRoot, ok := config.ConfinedProjectConfigRoot("proj", relinkOldID)
	newRoot, ok2 := config.ConfinedProjectConfigRoot("proj", relinkNewID)
	if !ok || !ok2 {
		t.Fatal("project config roots")
	}
	writeRelinkSettings(t, filepath.Join(oldRoot, config.DotScion), root)
	agentDir := filepath.Join(oldRoot, config.DotScion, "agents", relinkAgent)
	writeRelinkFile(t, filepath.Join(agentDir, "home", "state.txt"), relinkState)
	writeRelinkFile(t, filepath.Join(agentDir, "scion-agent.json"), `{}`)
	return &hubNativeRelink{srv: srv, mgr: mgr, root: root, dotScion: dotScion, oldRoot: oldRoot, newRoot: newRoot}
}

func (h *hubNativeRelink) projectID(t *testing.T) string {
	t.Helper()
	id, err := config.ReadProjectID(h.dotScion)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// assertCarried checks that the agent's state is at the current ID, the
// earlier directory is gone and the project-id names the current ID.
func (h *hubNativeRelink) assertCarried(t *testing.T) {
	t.Helper()
	if got := h.projectID(t); got != relinkNewID {
		t.Fatalf("project-id = %q, want %q", got, relinkNewID)
	}
	if got, err := os.ReadFile(filepath.Join(h.newRoot, config.DotScion, "agents", relinkAgent, "home", "state.txt")); err != nil || string(got) != relinkState {
		t.Fatalf("agent state at the current ID: %q, %v", got, err)
	}
	assertAbsent(t, h.oldRoot)
}

// assertNotCarried checks that the agent's state is still at the earlier
// ID and no directory exists for the current ID.
func (h *hubNativeRelink) assertNotCarried(t *testing.T) {
	t.Helper()
	if got, err := os.ReadFile(filepath.Join(h.oldRoot, config.DotScion, "agents", relinkAgent, "home", "state.txt")); err != nil || string(got) != relinkState {
		t.Fatalf("agent state at the earlier ID: %q, %v", got, err)
	}
	assertAbsent(t, h.newRoot)
}

// record returns the broker's record of the workspace's .scion entry for
// relinkNewID.
func (h *hubNativeRelink) record(t *testing.T) workspaceConfigRecord {
	t.Helper()
	_, recordPath, ok, err := workspaceConfigRecordFor(h.root, relinkNewID)
	if err != nil || !ok {
		t.Fatalf("workspaceConfigRecordFor: ok=%v err=%v", ok, err)
	}
	data, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	var rec workspaceConfigRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

// assertRecordNamesNewID checks that the record is for relinkNewID and
// holds a .scion directory whose project-id is relinkNewID.
func (h *hubNativeRelink) assertRecordNamesNewID(t *testing.T) {
	t.Helper()
	rec := h.record(t)
	if rec.HubProjectID != relinkNewID || rec.State.Kind != workspaceConfigDirectory || rec.State.Detail != relinkNewID {
		t.Fatalf("record = %+v, want a directory with project-id %s for %s", rec, relinkNewID, relinkNewID)
	}
}

func (h *hubNativeRelink) create(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"name": "` + relinkAgent + `", "id": "agent-uuid-relink", "slug": "` + relinkAgent + `", "provisionOnly": true,
		"projectId": "` + relinkNewID + `", "projectSlug": "proj", "previousProjectIds": ["` + relinkOldID + `"],
		"config": {"template": "claude", "sharedWorkspace": true}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(w, req)
	return w
}

// startOrRestart sends a shared-workspace start or restart (op) for the
// hub-managed workspace, with the dispatch slug, the settings profile (the
// fixture agent has no broker-side profile record) and the earlier ID
// listed.
func (h *hubNativeRelink) startOrRestart(t *testing.T, op string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"sharedWorkspace": true, "projectPath": ` + strconvQuote(h.root) + `, "projectSlug": "proj", "profile": "local", "previousProjectIds": ["` + relinkOldID + `"]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+relinkAgent+"/"+op+"?projectId="+relinkNewID, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(w, req)
	return w
}

// assertNotEntryRefusal checks that a response is not a refusal by the
// workspace .scion record check or by the re-link.
func assertNotEntryRefusal(t *testing.T, what string, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code == http.StatusConflict {
		t.Fatalf("%s refused: %s", what, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "not in the state the Runtime Broker recorded") {
		t.Fatalf("%s: workspace record refusal: %s", what, w.Body.String())
	}
}

// TestRelinkRecord_CreateRecordsNewIDAndSecondStartPasses: a create that
// carries the agent over leaves the broker's record naming the current ID,
// and a following start of the same agent passes the record check.
func TestRelinkRecord_CreateRecordsNewIDAndSecondStartPasses(t *testing.T) {
	h := newHubNativeRelink(t)
	assertNotEntryRefusal(t, "create", h.create(t))
	h.assertCarried(t)
	h.assertRecordNamesNewID(t)

	assertNotEntryRefusal(t, "second start", h.startOrRestart(t, "start"))
	h.assertCarried(t)
	h.assertRecordNamesNewID(t)
}

// TestRelinkRecord_StartAndRestartCarry: the start and restart paths
// (buildStartContext) carry the agent over on a hub-managed workspace and
// record the current ID; a second start passes the record check.
func TestRelinkRecord_StartAndRestartCarry(t *testing.T) {
	for _, op := range []string{"start", "restart"} {
		t.Run(op, func(t *testing.T) {
			h := newHubNativeRelink(t)
			assertNotEntryRefusal(t, op, h.startOrRestart(t, op))
			h.assertCarried(t)
			h.assertRecordNamesNewID(t)

			assertNotEntryRefusal(t, "second "+op, h.startOrRestart(t, op))
			h.assertCarried(t)
		})
	}
}

// recordFile returns the bytes of the broker's record of the workspace's
// .scion entry (one record file per workspace path; the Hub project ID it
// is for is inside it), or nil when there is none.
func (h *hubNativeRelink) recordFile(t *testing.T) []byte {
	t.Helper()
	_, recordPath, ok, err := workspaceConfigRecordFor(h.root, relinkNewID)
	if err != nil || !ok {
		t.Fatalf("workspaceConfigRecordFor: ok=%v err=%v", ok, err)
	}
	data, err := os.ReadFile(recordPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestRelinkRecord_EntryChangedBeforeCarryRefused: a .scion entry changed by
// someone else between the record check and the re-link is refused as a
// changed entry, nothing is carried over and the change is not recorded,
// under each record state a real flow leaves before the carry:
//   - create: the create path's checked identity alignment, run for the
//     current ID, has recorded the entry as it found it (a directory whose
//     project-id names the earlier ID);
//   - start or restart: no record for the current ID, the only record being
//     the one an earlier start kept for the earlier ID;
//   - no record at all.
//
// withCheckedWorkspaceConfig compares the entry after the re-link with the
// state it observed at its check (not with the stored record) and accepts
// only a change the broker makes, so the refusal does not depend on a
// record for the current ID.
func TestRelinkRecord_EntryChangedBeforeCarryRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed func(t *testing.T, h *hubNativeRelink)
		// recordID is the Hub project ID of the record the seed leaves
		// ("" for none); the record always holds the entry as found (a
		// directory whose project-id names the earlier ID).
		recordID string
	}{
		{name: "create: alignment recorded the entry for the current ID", seed: func(t *testing.T, h *hubNativeRelink) {
			if err := h.srv.alignHubManagedProjectIdentityChecked(context.Background(), "agent-uuid-relink", h.root, "proj", relinkNewID); err != nil {
				t.Fatal(err)
			}
		}, recordID: relinkNewID},
		{name: "start: record only for the earlier ID", seed: func(t *testing.T, h *hubNativeRelink) {
			if err := recordWorkspaceConfigState(h.root, relinkOldID); err != nil {
				t.Fatal(err)
			}
		}, recordID: relinkOldID},
		{name: "no record", seed: func(*testing.T, *hubNativeRelink) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHubNativeRelink(t)
			tc.seed(t, h)
			before := h.recordFile(t)
			if tc.recordID == "" {
				if before != nil {
					t.Fatalf("seed left a record: %s", before)
				}
			} else {
				var rec workspaceConfigRecord
				if err := json.Unmarshal(before, &rec); err != nil {
					t.Fatalf("seeded record: %v", err)
				}
				if rec.HubProjectID != tc.recordID || rec.State.Kind != workspaceConfigDirectory || rec.State.Detail != relinkOldID {
					t.Fatalf("seeded record = %+v, want a directory with project-id %s for %s", rec, relinkOldID, tc.recordID)
				}
			}
			afterWorkspaceConfigCheckHook = func() {
				if err := config.WriteProjectID(h.dotScion, relinkOtherID); err != nil {
					t.Error(err)
				}
			}
			t.Cleanup(func() { afterWorkspaceConfigCheckHook = nil })

			lockErr, checkErr, relinkErr := h.srv.relinkSharedProjectIdentityChecked(context.Background(), h.root, "proj", relinkNewID, relinkAgent, []string{relinkOldID}, false)
			if lockErr != nil || relinkErr != nil {
				t.Fatalf("lockErr=%v relinkErr=%v", lockErr, relinkErr)
			}
			var ce *workspaceConfigChangedError
			if !errors.As(checkErr, &ce) {
				t.Fatalf("expected a changed-entry refusal, got %v", checkErr)
			}
			if got := h.projectID(t); got != relinkOtherID {
				t.Fatalf("project-id = %q, want the other writer's %q", got, relinkOtherID)
			}
			h.assertNotCarried(t)
			if after := h.recordFile(t); string(after) != string(before) {
				t.Fatalf("the record changed: before %q, after %q", before, after)
			}
		})
	}
}

// changeEntryOnCheck makes the n-th withCheckedWorkspaceConfig check of the
// test rewrite the workspace's project-id to relinkOtherID, as another
// writer would between that check and the write it guards.
func (h *hubNativeRelink) changeEntryOnCheck(t *testing.T, n int) {
	t.Helper()
	calls := 0
	afterWorkspaceConfigCheckHook = func() {
		calls++
		if calls == n {
			if err := config.WriteProjectID(h.dotScion, relinkOtherID); err != nil {
				t.Error(err)
			}
		}
	}
	t.Cleanup(func() { afterWorkspaceConfigCheckHook = nil })
}

// assertEntryConflict checks a 409 carrying the workspace record refusal
// and, when code is set, that error code.
func assertEntryConflict(t *testing.T, w *httptest.ResponseRecorder, code string) {
	t.Helper()
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "not in the state the Runtime Broker recorded") {
		t.Fatalf("expected 409 with the workspace record refusal, got %d: %s", w.Code, w.Body.String())
	}
	if code != "" && !strings.Contains(w.Body.String(), `"code":"`+code+`"`) {
		t.Fatalf("expected error code %q: %s", code, w.Body.String())
	}
}

// TestRelinkRecord_HTTPEntryChangedDuringCarryRefused: through the create
// and start handlers, a .scion entry changed by someone else between the
// re-link's record check and the re-link is answered with the workspace
// record 409, and nothing is carried over. On create the re-link's check is
// the second (after the identity alignment's); on start it is the first.
func TestRelinkRecord_HTTPEntryChangedDuringCarryRefused(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		h := newHubNativeRelink(t)
		h.changeEntryOnCheck(t, 2)
		assertEntryConflict(t, h.create(t), ErrCodeConflict)
		if got := h.projectID(t); got != relinkOtherID {
			t.Fatalf("project-id = %q, want the other writer's %q", got, relinkOtherID)
		}
		h.assertNotCarried(t)
		if h.mgr.provisionCalled {
			t.Fatal("Provision ran after the refusal")
		}
	})
	t.Run("start", func(t *testing.T) {
		h := newHubNativeRelink(t)
		h.changeEntryOnCheck(t, 1)
		assertEntryConflict(t, h.startOrRestart(t, "start"), "")
		if got := h.projectID(t); got != relinkOtherID {
			t.Fatalf("project-id = %q, want the other writer's %q", got, relinkOtherID)
		}
		h.assertNotCarried(t)
	})
}

// TestRelinkRecord_EntryChangedBeforeCheckRefused: a .scion entry that no
// longer matches the record when the re-link starts is refused before the
// re-link runs.
func TestRelinkRecord_EntryChangedBeforeCheckRefused(t *testing.T) {
	h := newHubNativeRelink(t)
	if err := recordWorkspaceConfigState(h.root, relinkNewID); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteProjectID(h.dotScion, relinkOtherID); err != nil {
		t.Fatal(err)
	}
	_, checkErr, relinkErr := h.srv.relinkSharedProjectIdentityChecked(context.Background(), h.root, "proj", relinkNewID, relinkAgent, []string{relinkOldID}, false)
	var ce *workspaceConfigChangedError
	if !errors.As(checkErr, &ce) || relinkErr != nil {
		t.Fatalf("checkErr=%v relinkErr=%v", checkErr, relinkErr)
	}
	h.assertNotCarried(t)
}

// TestRelinkRecord_HubManagedWithoutSlugNotCarried: on a hub-managed
// workspace a re-link without the dispatch slug carries nothing over, even
// though the earlier config records this workspace (which would otherwise
// allow the carry).
func TestRelinkRecord_HubManagedWithoutSlugNotCarried(t *testing.T) {
	h := newHubNativeRelink(t)
	if err := h.srv.relinkSharedProjectIdentity(context.Background(), h.root, "", relinkNewID, relinkAgent, []string{relinkOldID}, true); err != nil {
		t.Fatal(err)
	}
	if got := h.projectID(t); got != relinkOldID {
		t.Fatalf("project-id rewritten to %q", got)
	}
	h.assertNotCarried(t)
}
