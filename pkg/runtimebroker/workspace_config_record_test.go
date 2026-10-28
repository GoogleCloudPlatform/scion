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
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

const recordHubID = "66666666-6666-6666-6666-666666666666"

// hubWorkspace returns a hub-managed workspace root under a temporary HOME.
func hubWorkspace(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	ws := filepath.Join(home, ".scion", "projects", "proj")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	return ws
}

// .scion entry states a workspace can be put in.
type scionEntryState func(t *testing.T, ws string)

func clearEntry(t *testing.T, ws string) {
	t.Helper()
	if err := os.RemoveAll(filepath.Join(ws, ".scion")); err != nil {
		t.Fatal(err)
	}
}

var (
	entryAbsent scionEntryState = func(t *testing.T, ws string) { clearEntry(t, ws) }
	entryMarker scionEntryState = func(t *testing.T, ws string) {
		clearEntry(t, ws)
		m := &config.ProjectMarker{ProjectID: recordHubID, ProjectName: "proj", ProjectSlug: "proj"}
		if err := config.WriteProjectMarker(filepath.Join(ws, ".scion"), m); err != nil {
			t.Fatal(err)
		}
	}
	entryMarkerEdited scionEntryState = func(t *testing.T, ws string) {
		clearEntry(t, ws)
		m := &config.ProjectMarker{ProjectID: recordHubID, ProjectName: "other", ProjectSlug: "other"}
		if err := config.WriteProjectMarker(filepath.Join(ws, ".scion"), m); err != nil {
			t.Fatal(err)
		}
	}
	entryDir = func(id string) scionEntryState {
		return func(t *testing.T, ws string) {
			clearEntry(t, ws)
			if err := os.MkdirAll(filepath.Join(ws, ".scion", "templates"), 0o755); err != nil {
				t.Fatal(err)
			}
			if id != "" {
				if err := config.WriteProjectID(filepath.Join(ws, ".scion"), id); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	entrySymlink = func(target string) scionEntryState {
		return func(t *testing.T, ws string) {
			clearEntry(t, ws)
			if err := os.MkdirAll(filepath.Join(ws, target), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(ws, ".scion")); err != nil {
				t.Fatal(err)
			}
		}
	}
)

// Every change of the .scion entry's state after the broker recorded it is
// refused; an unchanged state passes.
func TestWorkspaceConfigRecord_StateTransitions(t *testing.T) {
	states := map[string]scionEntryState{
		"absent":                   entryAbsent,
		"marker":                   entryMarker,
		"edited marker":            entryMarkerEdited,
		"directory without id":     entryDir(""),
		"directory with hub id":    entryDir(recordHubID),
		"directory with other id":  entryDir("77777777-7777-7777-7777-777777777777"),
		"symlink to cfg":           entrySymlink("cfg"),
		"symlink to another place": entrySymlink("elsewhere"),
	}
	for fromName, from := range states {
		for toName, to := range states {
			t.Run(fromName+" -> "+toName, func(t *testing.T) {
				ws := hubWorkspace(t)
				from(t, ws)
				if err := recordWorkspaceConfigState(ws, recordHubID); err != nil {
					t.Fatalf("record: %v", err)
				}
				to(t, ws)
				err := checkWorkspaceConfigRecord(ws, recordHubID)
				var ce *workspaceConfigChangedError
				if fromName == toName {
					if err != nil {
						t.Fatalf("unchanged state refused: %v", err)
					}
					return
				}
				if !errors.As(err, &ce) {
					t.Fatalf("changed state passed (err %v)", err)
				}
				if strings.Contains(err.Error(), ws) {
					t.Errorf("error carries the host path: %v", err)
				}
			})
		}
	}
}

// A record starts fresh for another Hub project ID or a removed workspace,
// is absent until first written, and fails closed when unreadable. Paths
// that are not hub-managed workspace roots are not recorded.
func TestWorkspaceConfigRecord_Scope(t *testing.T) {
	t.Run("no record", func(t *testing.T) {
		ws := hubWorkspace(t)
		entryDir("")(t, ws)
		if err := checkWorkspaceConfigRecord(ws, recordHubID); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("another hub project id starts fresh", func(t *testing.T) {
		ws := hubWorkspace(t)
		entryMarker(t, ws)
		if err := recordWorkspaceConfigState(ws, recordHubID); err != nil {
			t.Fatal(err)
		}
		entryDir("")(t, ws)
		if err := checkWorkspaceConfigRecord(ws, "88888888-8888-8888-8888-888888888888"); err != nil {
			t.Fatalf("other project id: %v", err)
		}
	})
	t.Run("removed workspace starts fresh", func(t *testing.T) {
		ws := hubWorkspace(t)
		entryMarker(t, ws)
		if err := recordWorkspaceConfigState(ws, recordHubID); err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(ws); err != nil {
			t.Fatal(err)
		}
		if err := checkWorkspaceConfigRecord(ws, recordHubID); err != nil {
			t.Fatalf("removed workspace: %v", err)
		}
		entryDir("")(t, ws)
		if err := checkWorkspaceConfigRecord(ws, recordHubID); err != nil {
			t.Fatalf("recreated workspace: %v", err)
		}
	})
	t.Run("unreadable record fails closed", func(t *testing.T) {
		ws := hubWorkspace(t)
		entryMarker(t, ws)
		if err := recordWorkspaceConfigState(ws, recordHubID); err != nil {
			t.Fatal(err)
		}
		_, recordPath, _, err := workspaceConfigRecordFor(ws, recordHubID)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(recordPath, []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		var ce *workspaceConfigChangedError
		if err := checkWorkspaceConfigRecord(ws, recordHubID); !errors.As(err, &ce) {
			t.Fatalf("corrupt record: %v", err)
		}
	})
	t.Run("record is private and outside the workspace", func(t *testing.T) {
		ws := hubWorkspace(t)
		entryMarker(t, ws)
		if err := recordWorkspaceConfigState(ws, recordHubID); err != nil {
			t.Fatal(err)
		}
		_, recordPath, _, _ := workspaceConfigRecordFor(ws, recordHubID)
		info, err := os.Stat(recordPath)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("record mode %v, want 0600", info.Mode().Perm())
		}
		if dir, _ := os.Stat(filepath.Dir(recordPath)); dir.Mode().Perm() != 0o700 {
			t.Errorf("records dir mode %v, want 0700", dir.Mode().Perm())
		}
		if strings.HasPrefix(recordPath, ws+string(filepath.Separator)) {
			t.Errorf("record %s is inside the workspace", recordPath)
		}
	})
	t.Run("linked projects and .scion paths are not recorded", func(t *testing.T) {
		hubWorkspace(t)
		linked := filepath.Join(t.TempDir(), "repo")
		if err := os.MkdirAll(linked, 0o755); err != nil {
			t.Fatal(err)
		}
		entryMarker(t, linked)
		for _, p := range []string{linked, filepath.Join(linked, ".scion")} {
			if _, _, ok, err := workspaceConfigRecordFor(p, recordHubID); ok || err != nil {
				t.Errorf("%s: (ok=%v, err=%v), want not recorded", p, ok, err)
			}
		}
		ws := filepath.Join(os.Getenv("HOME"), ".scion", "projects", "proj")
		if _, _, ok, _ := workspaceConfigRecordFor(ws, ""); ok {
			t.Error("recorded without a hub project id")
		}
	})
}

// End to end: a hub-managed create records the workspace's marker; when the
// marker is then replaced with a .scion directory, the next create, start
// and restart are refused with 409 before any provision or start runs.
func TestWorkspaceConfigRecord_MarkerReplacedByDirectoryRefused(t *testing.T) {
	srv, mgr := newHubDefaultsWiringServer()
	ws := hubWorkspace(t)
	body := func(name string) string {
		return `{"name": "` + name + `", "id": "id-` + name + `", "slug": "` + name + `", "provisionOnly": true,
			"projectId": "` + recordHubID + `", "projectSlug": "proj",
			"config": {"template": "claude", "sharedWorkspace": true}}`
	}
	postCreateAgent(t, srv, body("first"))
	if !config.IsProjectMarkerFile(filepath.Join(ws, ".scion")) {
		t.Fatal("fixture: the first create should leave a .scion marker")
	}
	mgr.provisionCalled = false

	entryDir(recordHubID)(t, ws)
	if err := os.MkdirAll(filepath.Join(ws, ".scion", "harness-configs", "claude"), 0o755); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body("second")))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "not in the state the Runtime Broker recorded") {
		t.Fatalf("create: expected 409, got %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), ws) {
		t.Errorf("response carries the host path: %s", w.Body.String())
	}
	if mgr.provisionCalled {
		t.Fatal("Provision ran for a workspace whose .scion changed")
	}

	for _, op := range []string{"start", "restart"} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/first/"+op+"?projectId="+recordHubID,
			strings.NewReader(`{"sharedWorkspace": true, "projectSlug": "proj"}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "not in the state the Runtime Broker recorded") {
			t.Fatalf("%s: expected 409, got %d: %s", op, w.Code, w.Body.String())
		}
	}
}

// A change made between the check and the broker's own writes (here a
// marker replaced with a .scion directory without a project-id, which
// recordHubProjectFiles would otherwise fill in) is refused and not
// recorded; the broker's own marker write is recorded.
func TestRecordHubProjectFilesChecked_ChangeDuringWriteRefused(t *testing.T) {
	srv := newTestServer(t)
	ws := hubWorkspace(t)
	in := startContextInputs{AgentID: "a1", ProjectPath: ws, ProjectID: recordHubID, ProjectSlug: "proj"}

	// absent -> marker written by the broker: recorded.
	if err := srv.recordHubProjectFilesChecked(context.Background(), in); err != nil {
		t.Fatalf("first record: %v", err)
	}
	if !config.IsProjectMarkerFile(filepath.Join(ws, ".scion")) {
		t.Fatal("fixture: expected the broker to write a marker")
	}
	recorded, _, err := checkWorkspaceConfigRecordState(ws, recordHubID)
	if err != nil || recorded.Kind != workspaceConfigMarker {
		t.Fatalf("recorded state = %+v, %v", recorded, err)
	}

	afterWorkspaceConfigCheckHook = func() { entryDir("")(t, ws) }
	t.Cleanup(func() { afterWorkspaceConfigCheckHook = nil })
	var ce *workspaceConfigChangedError
	if err := srv.recordHubProjectFilesChecked(context.Background(), in); !errors.As(err, &ce) {
		t.Fatalf("swap during the write: err = %v, want a refusal", err)
	}
	afterWorkspaceConfigCheckHook = nil
	// The record still says marker, so the next start is refused too.
	if err := checkWorkspaceConfigRecord(ws, recordHubID); !errors.As(err, &ce) {
		t.Fatalf("next check: err = %v, want a refusal", err)
	}
}

func TestBrokerWorkspaceConfigChange(t *testing.T) {
	ws := hubWorkspace(t)
	entryMarker(t, ws)
	own, ownData, err := observeWorkspaceConfigStateData(ws)
	if err != nil {
		t.Fatal(err)
	}
	shadow, shadowData := own, []byte("project-id: "+recordHubID+"\nproject-slug: proj\ntype: shadow\n")
	shadow.Detail = "shadow"
	absent := workspaceConfigState{Kind: workspaceConfigAbsent}
	dirNoID := workspaceConfigState{Kind: workspaceConfigDirectory}
	dirHub := workspaceConfigState{Kind: workspaceConfigDirectory, Detail: recordHubID}
	dirOther := workspaceConfigState{Kind: workspaceConfigDirectory, Detail: "77777777-7777-7777-7777-777777777777"}
	for _, tt := range []struct {
		name          string
		before, after workspaceConfigState
		slug          string
		want          bool
	}{
		{"unchanged", dirNoID, dirNoID, "proj", true},
		{"marker written where absent", absent, own, "proj", true},
		{"marker for another slug", absent, own, "other", false},
		{"stale marker rewritten", workspaceConfigState{Kind: workspaceConfigMarker, Detail: "x"}, own, "proj", true},
		{"project-id written into an existing directory", dirNoID, dirHub, "proj", true},
		{"stale project-id rewritten", dirOther, dirHub, "proj", true},
		{"directory with another id", dirNoID, dirOther, "proj", false},
		{"directory where absent", absent, dirHub, "proj", false},
		{"directory where marker", own, dirHub, "proj", false},
		{"absent where marker", own, absent, "proj", false},
		{"symlink", dirNoID, workspaceConfigState{Kind: workspaceConfigSymlink, Detail: "x"}, "proj", false},
	} {
		data := ownData
		if got := brokerWorkspaceConfigChange(tt.before, tt.after, data, recordHubID, tt.slug); got != tt.want {
			t.Errorf("%s: got %v, want %v", tt.name, got, tt.want)
		}
	}
	// A shadow marker, or marker bytes that do not parse, are not the
	// broker's own write.
	if brokerWorkspaceConfigChange(absent, shadow, shadowData, recordHubID, "proj") {
		t.Error("a shadow marker accepted as the broker's write")
	}
	if brokerWorkspaceConfigChange(absent, own, []byte("{"), recordHubID, "proj") {
		t.Error("unparseable marker bytes accepted")
	}
}

// An earlier start recorded a broker copy's old identity while alignment
// was deferred. When a later create's alignment rewrites the identity to
// the hub project ID, the change is recorded in the same locked section,
// so that create and the next start are accepted, not refused with 409.
func TestCreateAgent_IdentityAlignmentIsRecorded(t *testing.T) {
	identityForms(t, func(t *testing.T, markerForm bool) {
		srv, _ := newHubDefaultsWiringServer()
		workspace, _ := seedBrokerWorkspace(t, brokerTestLocalID, markerForm)
		// The record of the old identity, under the hub project ID.
		if err := recordWorkspaceConfigState(workspace, brokerTestHubID); err != nil {
			t.Fatal(err)
		}
		writeBrokerRecord(t, brokerTestHubID)

		postCreateAgent(t, srv, `{"name": "aligned", "id": "id-aligned", "slug": "aligned", "provisionOnly": true,
			"projectId": "`+brokerTestHubID+`", "projectSlug": "`+brokerTestSlug+`",
			"config": {"template": "claude", "sharedWorkspace": true}}`)
		if want, ok := config.ConfinedProjectConfigRoot(brokerTestSlug, brokerTestHubID); !ok || brokerConfigRoot(t, workspace) != want {
			t.Fatalf("fixture: the create's alignment did not record the hub project ID")
		}
		if err := checkWorkspaceConfigRecord(workspace, brokerTestHubID); err != nil {
			t.Fatalf("the aligned identity is not the recorded state: %v", err)
		}
		postCreateAgent(t, srv, `{"name": "next", "id": "id-next", "slug": "next", "provisionOnly": true,
			"projectId": "`+brokerTestHubID+`", "projectSlug": "`+brokerTestSlug+`",
			"config": {"template": "claude", "sharedWorkspace": true}}`)
	})
}
