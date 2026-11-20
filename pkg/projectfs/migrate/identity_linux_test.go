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

//go:build linux

package migrate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
)

func TestTreeIdentity_MarkerFileAndDirectoryForms(t *testing.T) {
	markerYAML := func(key, id string) string {
		return key + ": " + id + "\nproject-name: Demo\nproject-slug: p1\n"
	}

	cases := []struct {
		name       string
		files      map[string]string
		global     map[string]string
		local      bool
		want       identity
		wantDetail string
	}{
		{
			name:       "marker-file-match",
			files:      map[string]string{".scion": markerYAML(projectkeys.ProjectIDFile, pid1)},
			want:       identityVerified,
			wantDetail: ".scion=" + pid1,
		},
		{
			name:       "marker-file-legacy-key-match",
			files:      map[string]string{".scion": markerYAML(projectkeys.LegacyProjectIDFile, pid1)},
			want:       identityVerified,
			wantDetail: ".scion=" + pid1,
		},
		{
			name:       "marker-file-mismatch",
			files:      map[string]string{".scion": markerYAML(projectkeys.ProjectIDFile, pid2)},
			want:       identityMismatch,
			wantDetail: ".scion=" + pid2 + " (mismatch)",
		},
		{
			name:       "marker-file-legacy-key-mismatch",
			files:      map[string]string{".scion": markerYAML(projectkeys.LegacyProjectIDFile, pid2)},
			want:       identityMismatch,
			wantDetail: ".scion=" + pid2 + " (mismatch)",
		},
		{
			name: "marker-file-mixed-keys-mismatch",
			files: map[string]string{
				".scion": projectkeys.ProjectIDFile + ": " + pid1 + "\n" + projectkeys.LegacyProjectIDFile + ": " + pid2 + "\n",
			},
			want:       identityMismatch,
			wantDetail: "(mismatch)",
		},
		{
			name:       "marker-file-vs-workspace-record-mismatch",
			files:      map[string]string{".scion": markerYAML(projectkeys.ProjectIDFile, pid1)},
			global:     map[string]string{"hub-workspaces/p1": pid2},
			local:      true,
			want:       identityMismatch,
			wantDetail: "hub-workspaces/p1=" + pid2 + " (mismatch)",
		},
		{
			name:       "workspace-record-match-vs-marker-file-mismatch",
			files:      map[string]string{".scion": markerYAML(projectkeys.ProjectIDFile, pid2)},
			global:     map[string]string{"hub-workspaces/p1": pid1},
			local:      true,
			want:       identityMismatch,
			wantDetail: ".scion=" + pid2 + " (mismatch)",
		},
		{
			name:       "marker-file-missing-id-unverified",
			files:      map[string]string{".scion": "project-name: Demo\nproject-slug: p1\n"},
			want:       identityUnverified,
			wantDetail: "checked .scion",
		},
		{
			name:       "marker-file-invalid-yaml-unverified",
			files:      map[string]string{".scion": ":\n\t[not yaml\n"},
			want:       identityUnverified,
			wantDetail: "checked .scion",
		},
		{
			name:       "directory-project-id-match",
			files:      map[string]string{".scion/" + projectkeys.ProjectIDFile: pid1 + "\n"},
			want:       identityVerified,
			wantDetail: ".scion/" + projectkeys.ProjectIDFile + "=" + pid1,
		},
		{
			name:       "directory-settings-match",
			files:      map[string]string{".scion/settings.yaml": idSettings(pid1)},
			want:       identityVerified,
			wantDetail: ".scion/settings.yaml=" + pid1,
		},
		{
			name:       "directory-project-id-mismatch",
			files:      map[string]string{".scion/" + projectkeys.ProjectIDFile: pid2 + "\n"},
			want:       identityMismatch,
			wantDetail: ".scion/" + projectkeys.ProjectIDFile + "=" + pid2 + " (mismatch)",
		},
		{
			name:       "no-scion-unverified",
			files:      map[string]string{"README.md": "hello"},
			want:       identityUnverified,
			wantDetail: "checked .scion",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			globalDir := t.TempDir()
			writeTree(t, filepath.Join(base, "p1"), tc.files)
			if len(tc.global) > 0 {
				writeTree(t, globalDir, tc.global)
			}
			got, detail, err := treeIdentity(Tree{Base: base, Rel: "p1"}, tc.local, globalDir, "p1", pid1)
			if err != nil {
				t.Fatalf("treeIdentity error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("identity = %v, want %v (detail=%q)", got, tc.want, detail)
			}
			if tc.wantDetail != "" && !strings.Contains(detail, tc.wantDetail) {
				t.Fatalf("detail %q lacks %q", detail, tc.wantDetail)
			}
		})
	}
}

func TestTreeIdentity_SymlinkedScionRefused(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	markerPath := filepath.Join(outside, "marker")
	if err := os.WriteFile(markerPath, []byte(projectkeys.ProjectIDFile+": "+pid1+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	treeDir := filepath.Join(base, "p1")
	if err := os.MkdirAll(treeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(markerPath, filepath.Join(treeDir, ".scion")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := treeIdentity(Tree{Base: base, Rel: "p1"}, false, t.TempDir(), "p1", pid1); err == nil {
		t.Fatal("symlinked .scion must return an error")
	}
}
