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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

const (
	testGlobalProjectID = "48ef8d00-0000-4000-8000-000000000001"
	testOtherProjectID  = "1dfdd6c7-0000-4000-8000-000000000002"
)

// newGlobalDirTestServer builds a test server, then points HOME at a fresh
// temp dir (the server helper sets its own HOME) and writes a global
// directory whose .scion marker records markerID under markerSlug. The
// marker's external project-configs dir is left absent, so the marker reads
// as stale.
func newGlobalDirTestServer(t *testing.T, markerID, markerSlug string) (srv *Server, home, globalDir, markerPath string) {
	t.Helper()
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv = newTestServerForStartContext(t, cfg)

	t.Setenv("SCION_PROJECT_ID", "")
	t.Setenv("SCION_PROJECT", "")
	t.Setenv("SCION_PROJECT_PATH", "")
	home = t.TempDir()
	t.Setenv("HOME", home)
	globalDir = filepath.Join(home, ".scion")
	if err := os.MkdirAll(globalDir, 0755); err != nil {
		t.Fatal(err)
	}
	markerPath = filepath.Join(globalDir, config.DotScion)
	if err := config.WriteProjectMarker(markerPath, &config.ProjectMarker{
		ProjectID: markerID, ProjectName: markerSlug, ProjectSlug: markerSlug,
	}); err != nil {
		t.Fatal(err)
	}
	return srv, home, globalDir, markerPath
}

func readFileOrFatal(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestBuildStartContext_GlobalDirPathRefusedForOtherProject(t *testing.T) {
	cases := []struct {
		name string
		path func(home, globalDir string) string
	}{
		{"global dir", func(_, globalDir string) string { return globalDir }},
		{"home as project root", func(home, _ string) string { return home }},
	}
	for _, tc := range cases {
		for _, slug := range []string{"global", ""} {
			t.Run(tc.name+"/marker slug "+strconv.Quote(slug), func(t *testing.T) {
				srv, home, globalDir, markerPath := newGlobalDirTestServer(t, testGlobalProjectID, slug)
				before := readFileOrFatal(t, markerPath)

				_, err := srv.buildStartContext(context.Background(), startContextInputs{
					Name:        "x",
					ProjectPath: tc.path(home, globalDir),
					ProjectID:   testOtherProjectID,
					Operation:   opCreate,
				})
				var sce *startContextError
				switch {
				case err == nil:
					t.Error("expected an error for the global dir used by another project")
				case !errors.As(err, &sce) || sce.Status != http.StatusConflict:
					t.Errorf("expected a 409 startContextError, got %T %v", err, err)
				case !strings.Contains(sce.Message, "global scion directory"):
					t.Errorf("error message does not name the global directory: %q", sce.Message)
				}

				if after := readFileOrFatal(t, markerPath); !bytes.Equal(before, after) {
					t.Errorf("global marker changed:\nbefore: %q\nafter:  %q", before, after)
				}
				if _, statErr := os.Stat(filepath.Join(globalDir, "project-configs")); !os.IsNotExist(statErr) {
					t.Errorf("project-configs was created under the global dir (stat err %v)", statErr)
				}
				if _, statErr := os.Stat(filepath.Join(globalDir, config.DotScion, "project-id")); statErr == nil {
					t.Error("a project-id was written under the global dir")
				}
			})
		}
	}
}

func TestBuildStartContext_GlobalDirPathAllowedForGlobalProject(t *testing.T) {
	srv, _, globalDir, markerPath := newGlobalDirTestServer(t, testGlobalProjectID, "")
	before := readFileOrFatal(t, markerPath)

	for _, id := range []string{testGlobalProjectID, "global", ""} {
		if _, err := srv.buildStartContext(context.Background(), startContextInputs{
			Name:        "x",
			ProjectPath: globalDir,
			ProjectID:   id,
			Operation:   opCreate,
		}); err != nil {
			t.Fatalf("project id %q: global project dispatch failed: %v", id, err)
		}
	}
	if after := readFileOrFatal(t, markerPath); !bytes.Equal(before, after) {
		t.Errorf("global marker changed for the global project:\nbefore: %q\nafter:  %q", before, after)
	}
}

// When the global settings record a new hub id for the global project (the
// global project was recreated on the hub), the stale global marker is still
// updated to that id, as before.
func TestBuildStartContext_GlobalMarkerUpdatedForRecreatedGlobalProject(t *testing.T) {
	srv, _, globalDir, markerPath := newGlobalDirTestServer(t, testGlobalProjectID, "global")
	const newGlobalID = "77777777-0000-4000-8000-000000000003"
	if err := config.UpdateSetting(globalDir, "hub.projectId", newGlobalID, true); err != nil {
		t.Fatal(err)
	}

	if _, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "x",
		ProjectPath: globalDir,
		ProjectID:   newGlobalID,
		Operation:   opCreate,
	}); err != nil {
		t.Fatalf("recreated global project dispatch failed: %v", err)
	}
	marker, err := config.ReadProjectMarker(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	if marker.ProjectID != newGlobalID {
		t.Errorf("expected global marker updated to %q, got %q", newGlobalID, marker.ProjectID)
	}
}

func TestCanRewriteProjectMarker(t *testing.T) {
	_, _, globalDir, _ := newGlobalDirTestServer(t, testGlobalProjectID, "global")
	if !canRewriteProjectMarker(filepath.Join(t.TempDir(), "web-demo"), testOtherProjectID, false) {
		t.Error("an ordinary project dir must stay rewritable")
	}
	if canRewriteProjectMarker(globalDir, testOtherProjectID, false) {
		t.Error("the global marker must not be rewritable for another project")
	}
	if !canRewriteProjectMarker(globalDir, testOtherProjectID, true) {
		t.Error("the global marker must be rewritable for the hub's global project")
	}
}

func TestSplitHubGlobalSlug(t *testing.T) {
	cases := []struct {
		path, slug, wantSlug string
		wantGlobal           bool
	}{
		{"/home/u/.scion", "global", "", true},
		{"/home/u/.scion", "web-app", "web-app", false},
		{"", "global", "global", false},
		{"", "web-app", "web-app", false},
		{"/home/u/.scion", "", "", false},
	}
	for _, tc := range cases {
		slug, global := splitHubGlobalSlug(tc.path, tc.slug)
		if slug != tc.wantSlug || global != tc.wantGlobal {
			t.Errorf("splitHubGlobalSlug(%q, %q) = (%q, %v), want (%q, %v)",
				tc.path, tc.slug, slug, global, tc.wantSlug, tc.wantGlobal)
		}
	}
}

// newGlobalDirOnlyTestServer is newGlobalDirTestServer without a global
// marker: a fresh broker whose global directory holds settings only.
func newGlobalDirOnlyTestServer(t *testing.T) (srv *Server, globalDir string) {
	t.Helper()
	srv, _, globalDir, markerPath := newGlobalDirTestServer(t, testGlobalProjectID, "global")
	if err := os.Remove(markerPath); err != nil {
		t.Fatal(err)
	}
	return srv, globalDir
}

// A fresh broker (no global marker, no hub id in the global settings), as on
// a combined hub and broker server, accepts the global project the hub marks
// as global, and records its id in a new global marker as before.
func TestBuildStartContext_HubGlobalProjectAllowedWithoutRecordedID(t *testing.T) {
	srv, globalDir := newGlobalDirOnlyTestServer(t)
	const hubGlobalID = "aaaaaaaa-0000-4000-8000-000000000004"

	if _, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:             "x",
		ProjectPath:      globalDir,
		ProjectID:        hubGlobalID,
		HubGlobalProject: true,
		Operation:        opCreate,
	}); err != nil {
		t.Fatalf("hub global project dispatch failed: %v", err)
	}
	b := readFileOrFatal(t, filepath.Join(globalDir, config.DotScion))
	if !strings.Contains(string(b), hubGlobalID) {
		t.Errorf("expected the global marker to record %q, got %q", hubGlobalID, b)
	}
}

// Without the hub's mark, the same fresh broker refuses another project and
// writes no marker.
func TestBuildStartContext_FreshGlobalDirRefusesUnmarkedProject(t *testing.T) {
	srv, globalDir := newGlobalDirOnlyTestServer(t)

	_, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "x",
		ProjectPath: globalDir,
		ProjectID:   testOtherProjectID,
		Operation:   opCreate,
	})
	var sce *startContextError
	if !errors.As(err, &sce) || sce.Status != http.StatusConflict {
		t.Fatalf("expected a 409 startContextError, got %T %v", err, err)
	}
	if _, statErr := os.Stat(filepath.Join(globalDir, config.DotScion)); !os.IsNotExist(statErr) {
		t.Errorf("a global marker was written for another project (stat err %v)", statErr)
	}
}

// When the global project was recreated on the hub, the global marker holds
// the old id and the settings record nothing. The hub's mark admits the new
// id, and a readable stale marker is updated to it.
func TestBuildStartContext_HubGlobalProjectRecreated(t *testing.T) {
	const newGlobalID = "bbbbbbbb-0000-4000-8000-000000000005"
	for _, slug := range []string{"global", ""} {
		t.Run("marker slug "+strconv.Quote(slug), func(t *testing.T) {
			srv, _, globalDir, markerPath := newGlobalDirTestServer(t, testGlobalProjectID, slug)
			before := readFileOrFatal(t, markerPath)

			if _, err := srv.buildStartContext(context.Background(), startContextInputs{
				Name:             "x",
				ProjectPath:      globalDir,
				ProjectID:        newGlobalID,
				HubGlobalProject: true,
				Operation:        opCreate,
			}); err != nil {
				t.Fatalf("recreated hub global project dispatch failed: %v", err)
			}
			after := readFileOrFatal(t, markerPath)
			if slug == "" {
				// ReadProjectMarker rejects an empty slug, so the marker
				// block leaves such a marker alone, as before.
				if !bytes.Equal(before, after) {
					t.Errorf("empty-slug marker changed: %q -> %q", before, after)
				}
				return
			}
			if !strings.Contains(string(after), newGlobalID) {
				t.Errorf("expected the stale global marker updated to %q, got %q", newGlobalID, after)
			}
		})
	}
}

// The stale-marker branch must not rewrite the global marker for a dispatch
// the guard admits without the hub's mark or a settings id: id "global" with
// a readable, stale global marker.
func TestBuildStartContext_GlobalIDDoesNotRewriteGlobalMarker(t *testing.T) {
	srv, _, globalDir, markerPath := newGlobalDirTestServer(t, testGlobalProjectID, "global")
	before := readFileOrFatal(t, markerPath)

	if _, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "x",
		ProjectPath: globalDir,
		ProjectID:   "global",
		Operation:   opCreate,
	}); err != nil {
		t.Fatalf("global id dispatch failed: %v", err)
	}
	if after := readFileOrFatal(t, markerPath); !bytes.Equal(before, after) {
		t.Errorf("global marker changed for id \"global\":\nbefore: %q\nafter:  %q", before, after)
	}
}

// A global marker that disagrees with the id the global settings record does
// not admit the marker's id (for example a marker left over from an earlier
// faulty dispatch).
func TestBuildStartContext_SettingsIDOverridesGlobalMarker(t *testing.T) {
	srv, _, globalDir, _ := newGlobalDirTestServer(t, testOtherProjectID, "")
	if err := config.UpdateSetting(globalDir, "hub.projectId", testGlobalProjectID, true); err != nil {
		t.Fatal(err)
	}

	_, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:        "x",
		ProjectPath: globalDir,
		ProjectID:   testOtherProjectID,
		Operation:   opCreate,
	})
	var sce *startContextError
	if !errors.As(err, &sce) || sce.Status != http.StatusConflict {
		t.Fatalf("expected a 409 for the marker's id when settings record another, got %T %v", err, err)
	}
}

// The createAgent handler turns the hub's global slug sent with a path into
// the global mark: the fresh-broker global project is created, and another
// project at the same path is refused.
func TestCreateAgent_HubGlobalSlugWithGlobalDirPath(t *testing.T) {
	for _, tc := range []struct {
		name       string
		slug       string
		wantStatus int
	}{
		{"global slug", "global", http.StatusCreated},
		{"no slug", "", http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, mgr := newTestServerWithProvisionCapture()
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("SCION_PROJECT_ID", "")
			globalDir := filepath.Join(home, ".scion")
			if err := os.MkdirAll(globalDir, 0755); err != nil {
				t.Fatal(err)
			}
			body := `{
				"name": "x",
				"id": "agent-uuid-gd",
				"slug": "x",
				"projectId": "cccccccc-0000-4000-8000-000000000006",
				"projectSlug": ` + strconv.Quote(tc.slug) + `,
				"projectPath": ` + strconv.Quote(globalDir) + `,
				"provisionOnly": true,
				"config": {"template": "claude"}
			}`
			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)

			if w.Code != tc.wantStatus {
				t.Fatalf("expected status %d, got %d: %s", tc.wantStatus, w.Code, w.Body.String())
			}
			if tc.wantStatus == http.StatusCreated && mgr.lastOpts.ProjectPath != globalDir {
				t.Errorf("expected ProjectPath %q, got %q", globalDir, mgr.lastOpts.ProjectPath)
			}
		})
	}
}

// The startAgent handler turns the hub's global slug sent with a path into
// the global mark, as createAgent does: a stopped global-project agent on a
// fresh broker starts, and another project at the same path is refused.
func TestStartAgent_HubGlobalSlugWithGlobalDirPath(t *testing.T) {
	for _, tc := range []struct {
		name       string
		slug       string
		wantStatus int
	}{
		{"global slug", "global", http.StatusAccepted},
		{"no slug", "", http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t)
			mgr := srv.manager.(*mockManager)
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("SCION_PROJECT_ID", "")
			globalDir := filepath.Join(home, ".scion")
			if err := os.MkdirAll(globalDir, 0755); err != nil {
				t.Fatal(err)
			}
			body := `{"projectPath": ` + strconv.Quote(globalDir) + `, "projectSlug": ` + strconv.Quote(tc.slug) + `}`
			req := httptest.NewRequest(http.MethodPost,
				"/api/v1/agents/x/start?projectId=dddddddd-0000-4000-8000-000000000007", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)

			if w.Code != tc.wantStatus {
				t.Fatalf("expected status %d, got %d: %s", tc.wantStatus, w.Code, w.Body.String())
			}
			if tc.wantStatus == http.StatusAccepted && mgr.startCalls != 1 {
				t.Errorf("expected Start to be called once, got %d", mgr.startCalls)
			}
			if tc.wantStatus == http.StatusConflict {
				if _, statErr := os.Stat(filepath.Join(globalDir, config.DotScion)); !os.IsNotExist(statErr) {
					t.Errorf("a global marker was written for a refused start (stat err %v)", statErr)
				}
			}
		})
	}
}
