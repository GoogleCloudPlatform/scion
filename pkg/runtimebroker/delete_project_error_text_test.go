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
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDeleteProjectErrors_FixedText covers each runtime failure in
// deleteProject (ptone/scion#3496): the client gets a 500 runtime_error
// with the fixed "Failed to <op>" text only, and the cause, which names
// broker paths, reaches the broker log with the project.
func TestDeleteProjectErrors_FixedText(t *testing.T) {
	const slug = "proj-a"

	// failAbs makes srv's deleteProject filepath.Abs fail for paths with
	// suffix.
	failAbs := func(srv *Server, suffix, detail string) {
		srv.setProjectAbs(func(p string) (string, error) {
			if strings.HasSuffix(p, suffix) {
				return "", errors.New(detail)
			}
			return filepath.Abs(p)
		})
	}
	// lock chmods dir to mode for the test; the rows that use it skip
	// under root, which permission bits do not stop.
	lock := func(t *testing.T, dir string, mode os.FileMode) {
		if os.Geteuid() == 0 {
			t.Skip("permission bits do not stop root")
		}
		if err := os.Chmod(dir, mode); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	}

	cases := []struct {
		name, op, wantText string
		projectID          bool
		// setup arranges the failure and returns the detail that must
		// reach the log, plus any log fields beyond project_slug it must
		// carry.
		setup func(t *testing.T, srv *Server, home, ext string) (string, map[string]string)
	}{
		{
			name: "resolve-project-path", op: opResolveProjectPath, wantText: "Failed to resolve project path",
			setup: func(t *testing.T, srv *Server, home, _ string) (string, map[string]string) {
				d := "getwd: " + filepath.Join(home, "secret")
				failAbs(srv, "/projects/"+slug, d)
				return d, nil
			},
		},
		{
			name: "resolve-base-path", op: opResolveProjectsBase, wantText: "Failed to resolve base path",
			setup: func(t *testing.T, srv *Server, home, _ string) (string, map[string]string) {
				d := "getwd: " + filepath.Join(home, "secret")
				failAbs(srv, "/projects", d)
				return d, nil
			},
		},
		{
			name: "check-shared-dir", op: opCheckSharedDirStorage, wantText: "Failed to check project shared-dir storage",
			projectID: true,
			setup: func(t *testing.T, _ *Server, _, ext string) (string, map[string]string) {
				lock(t, filepath.Dir(seedSharedDir(t, ext, "scratch")), 0o600)
				return "permission denied", map[string]string{"project_id": scopeProjA}
			},
		},
		{
			name: "remove-shared-dir", op: opRemoveSharedDirStorage, wantText: "Failed to remove project shared-dir storage",
			projectID: true,
			setup: func(t *testing.T, _ *Server, _, ext string) (string, map[string]string) {
				base := seedSharedDir(t, ext, "scratch")
				lock(t, filepath.Join(base, "scratch"), 0o555)
				return "permission denied", map[string]string{"project_id": scopeProjA, "path": base}
			},
		},
		{
			name: "remove-project-dir", op: opRemoveProjectDir, wantText: "Failed to remove project directory",
			setup: func(t *testing.T, srv *Server, home, _ string) (string, map[string]string) {
				locked := filepath.Join(home, ".scion", "projects", slug, "locked")
				if err := os.MkdirAll(locked, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(locked, "f"), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
				lock(t, locked, 0o555)
				return "permission denied", map[string]string{"path": filepath.Join(home, ".scion", "projects", slug)}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, home := newScopeTestServer(t, &filteringMockManager{})
			ext, _ := makeHubMarkerProject(t, home, slug, scopeProjA, "dev")
			logs := captureLifecycleJSONLog(srv)
			detail, extra := tc.setup(t, srv, home, ext)
			projectID := ""
			if tc.projectID {
				projectID = scopeProjA
			}

			w := doDeleteProject(t, srv, slug, projectID)
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
			}
			code, msg := decodeErrorMessage(t, w)
			if code != ErrCodeRuntimeError {
				t.Errorf("code = %q, want %q", code, ErrCodeRuntimeError)
			}
			if msg != tc.wantText {
				t.Errorf("message = %q, want %q", msg, tc.wantText)
			}
			assertWorkspaceOpLogged(t, logs.String(), tc.op, "project_slug", slug, detail)
			if len(extra) > 0 {
				rec := findLogRecord(logs.String(), "runtime op failed", map[string]string{"op": tc.op})
				for key, want := range extra {
					if rec[key] != want {
						t.Errorf("log record %s = %v, want %q", key, rec[key], want)
					}
				}
			}
		})
	}
}
