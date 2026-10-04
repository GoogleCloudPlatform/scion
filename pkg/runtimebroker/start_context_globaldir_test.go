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
	if !canRewriteProjectMarker(filepath.Join(t.TempDir(), "web-demo"), testOtherProjectID) {
		t.Error("an ordinary project dir must stay rewritable")
	}
	if canRewriteProjectMarker(globalDir, testOtherProjectID) {
		t.Error("the global marker must not be rewritable for another project")
	}
}
