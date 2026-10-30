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
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// treeSnapshot maps every entry under root (relative path) to its type and,
// for regular files, its content.
func treeSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	snap := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		switch {
		case d.IsDir():
			snap[rel] = "dir"
		case d.Type().IsRegular():
			b, rErr := os.ReadFile(path)
			if rErr != nil {
				return rErr
			}
			snap[rel] = "file:" + string(b)
		default:
			snap[rel] = "other:" + d.Type().String()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// A dispatch that names only the project ID (no slug) for an ordinary
// project directory writes no .scion marker and no project-configs entry;
// with a slug the marker is written as before.
func TestBuildStartContext_NoMarkerWithoutProjectSlug(t *testing.T) {
	for _, slug := range []string{"", "linked-proj"} {
		t.Run("slug "+strconv.Quote(slug), func(t *testing.T) {
			srv, globalDir := newGlobalDirOnlyTestServer(t)
			projectPath := filepath.Join(t.TempDir(), "linked-proj")
			if err := os.MkdirAll(projectPath, 0755); err != nil {
				t.Fatal(err)
			}
			scionPath := filepath.Join(projectPath, config.DotScion)

			if _, err := srv.buildStartContext(context.Background(), startContextInputs{
				Name:        "x",
				ProjectPath: projectPath,
				ProjectSlug: slug,
				ProjectID:   testOtherProjectID,
				Operation:   opCreate,
			}); err != nil {
				t.Fatalf("dispatch failed: %v", err)
			}

			if slug == "" {
				if _, statErr := os.Lstat(scionPath); !os.IsNotExist(statErr) {
					t.Errorf("a .scion entry was written without a project slug (stat err %v)", statErr)
				}
				if _, statErr := os.Stat(filepath.Join(globalDir, config.ProjectConfigsDir)); !os.IsNotExist(statErr) {
					t.Errorf("project-configs was created without a project slug (stat err %v)", statErr)
				}
				return
			}
			marker, err := config.ReadProjectMarker(scionPath)
			if err != nil {
				t.Fatalf("expected a readable marker: %v", err)
			}
			if marker.ProjectID != testOtherProjectID || marker.ProjectSlug != slug {
				t.Errorf("marker = %+v, want id %q slug %q", marker, testOtherProjectID, slug)
			}
		})
	}
}

// On a fresh broker the global project's dispatch (which carries no slug)
// records only the hub id in the global directory: the record is slug-less,
// isGlobalDirProjectID admits the id, and no project-configs entry is made.
func TestBuildStartContext_FreshGlobalDirRecordsSlugLessID(t *testing.T) {
	srv, globalDir := newGlobalDirOnlyTestServer(t)
	const hubGlobalID = "eeeeeeee-0000-4000-8000-000000000008"

	if _, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:             "x",
		ProjectPath:      globalDir,
		ProjectID:        hubGlobalID,
		HubGlobalProject: true,
		Operation:        opCreate,
	}); err != nil {
		t.Fatalf("hub global project dispatch failed: %v", err)
	}
	markerPath := filepath.Join(globalDir, config.DotScion)
	b := readFileOrFatal(t, markerPath)
	if !strings.Contains(string(b), "project-id: "+hubGlobalID) || !strings.Contains(string(b), `project-slug: ""`) {
		t.Errorf("unexpected global id record: %q", b)
	}
	if !isGlobalDirProjectID(globalDir, hubGlobalID) {
		t.Error("isGlobalDirProjectID does not admit the recorded id")
	}
	if _, statErr := os.Stat(filepath.Join(globalDir, config.ProjectConfigsDir)); !os.IsNotExist(statErr) {
		t.Errorf("project-configs was created for the global id record (stat err %v)", statErr)
	}
}

// A dispatch for another project with the broker's global directory as its
// path gets a 409 and leaves every file under HOME as it was, with and
// without an existing global marker.
func TestBuildStartContext_GlobalDirRefusalWritesNothing(t *testing.T) {
	for _, withMarker := range []bool{true, false} {
		for _, rootIsHome := range []bool{false, true} {
			name := "marker " + strconv.FormatBool(withMarker) + "/home root " + strconv.FormatBool(rootIsHome)
			t.Run(name, func(t *testing.T) {
				srv, home, globalDir, markerPath := newGlobalDirTestServer(t, testGlobalProjectID, "global")
				if !withMarker {
					if err := os.Remove(markerPath); err != nil {
						t.Fatal(err)
					}
				}
				path := globalDir
				if rootIsHome {
					path = home
				}
				before := treeSnapshot(t, home)

				for _, slug := range []string{"", "web-app"} {
					_, err := srv.buildStartContext(context.Background(), startContextInputs{
						Name:        "x",
						ProjectPath: path,
						ProjectSlug: slug,
						ProjectID:   testOtherProjectID,
						Operation:   opCreate,
					})
					var sce *startContextError
					if !errors.As(err, &sce) || sce.Status != http.StatusConflict {
						t.Errorf("slug %q: expected a 409 startContextError, got %T %v", slug, err, err)
					}
				}

				if after := treeSnapshot(t, home); !reflect.DeepEqual(before, after) {
					t.Errorf("files under HOME changed:\nbefore: %v\nafter:  %v", before, after)
				}
			})
		}
	}
}

// The same refusal through the createAgent handler: a 409 response and no
// change to any file under HOME, with the embedded default harness config
// in effect. The refusal comes before the env-gather preflight and the
// harness-config policy gate, which resolve the template chain and can
// hydrate the default template under HOME (ptone/scion#5432). It used to
// fail whenever a v1 global settings file was present.
func TestCreateAgent_GlobalDirRefusalWritesNothing(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settings string // global settings.yaml; "" writes none
	}{
		{name: "no global settings file"},
		{name: "v1 global settings file", settings: "schema_version: \"1\"\n"},
		{name: "v1 global settings with a default template", settings: "schema_version: \"1\"\ndefault_template: claude\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newTestServerWithProvisionCapture()
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("SCION_PROJECT_ID", "")
			globalDir := filepath.Join(home, ".scion")
			if err := os.MkdirAll(globalDir, 0755); err != nil {
				t.Fatal(err)
			}
			if tc.settings != "" {
				if err := os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(tc.settings), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			before := treeSnapshot(t, home)

			body := `{
				"name": "x",
				"id": "agent-uuid-gd-refused",
				"slug": "x",
				"projectId": ` + strconv.Quote(testOtherProjectID) + `,
				"projectSlug": "web-app",
				"projectPath": ` + strconv.Quote(globalDir) + `,
				"provisionOnly": true,
				"config": {"template": "claude"}
			}`
			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)

			if w.Code != http.StatusConflict {
				t.Fatalf("expected status %d, got %d: %s", http.StatusConflict, w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "global scion directory") {
				t.Errorf("response does not name the refusal: %s", w.Body.String())
			}
			if after := treeSnapshot(t, home); !reflect.DeepEqual(before, after) {
				t.Errorf("files under HOME changed:\nbefore: %v\nafter:  %v", before, after)
			}
		})
	}
}
