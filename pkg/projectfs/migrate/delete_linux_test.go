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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
)

// legacyTree creates dir with a workspace file and, when id is set, a
// .scion/project-id marker naming id.
func legacyTree(t *testing.T, dir, id string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".scion"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if id != "" {
		if err := os.WriteFile(filepath.Join(dir, ".scion", projectkeys.ProjectIDFile), []byte(id+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func outcomes(ds []projectfs.Deletion) map[string]projectfs.Outcome {
	m := map[string]projectfs.Outcome{}
	for _, d := range ds {
		m[d.Path] = d.Outcome
	}
	return m
}

// Trees whose markers name the project go (the share's hub-projects tree,
// the host's projects/<slug>, and their .migrated-<ts> copies); a tree with
// no marker or another project's marker is kept, so a slug alone never
// deletes anything; other slugs are untouched.
func TestDeleteLegacyProjectTrees_MarkerMatchOnly(t *testing.T) {
	f := newHubFixture(t)
	hp := filepath.Join(f.share, legacyHubProjectsDir)
	verified := filepath.Join(hp, "demo")
	legacyTree(t, verified, pid1)
	copyVerified := filepath.Join(hp, "demo"+MigratedSuffix+"20261001T120000Z")
	legacyTree(t, copyVerified, pid1)
	notACopy := filepath.Join(hp, "demo"+MigratedSuffix+"garbage")
	legacyTree(t, notACopy, pid1)
	otherSlug := filepath.Join(hp, "demo-two")
	legacyTree(t, otherSlug, pid1)

	local := filepath.Join(f.global, "projects", "demo")
	legacyTree(t, local, "") // no marker: name only
	localCopy := filepath.Join(f.global, "projects", "demo"+MigratedSuffix+"20261002T120000Z")
	legacyTree(t, localCopy, pid2) // another project's tree under a reused slug

	ds, err := DeleteLegacyProjectTrees(f.layout, f.global, "demo", pid1)
	if err != nil {
		t.Fatal(err)
	}
	got := outcomes(ds)
	want := map[string]projectfs.Outcome{
		verified:     projectfs.OutcomeDeleted,
		copyVerified: projectfs.OutcomeDeleted,
		local:        projectfs.OutcomeKept,
		localCopy:    projectfs.OutcomeKept,
	}
	if len(got) != len(want) {
		t.Fatalf("deletions = %v, want %v", got, want)
	}
	for p, o := range want {
		if got[p] != o {
			t.Errorf("%s: outcome %q, want %q", p, got[p], o)
		}
	}
	for _, gone := range []string{verified, copyVerified} {
		if _, err := os.Lstat(gone); !os.IsNotExist(err) {
			t.Errorf("%s must be removed (err=%v)", gone, err)
		}
	}
	for _, kept := range []string{local, localCopy, notACopy, otherSlug} {
		if _, err := os.Stat(filepath.Join(kept, "README.md")); err != nil {
			t.Errorf("%s must be kept: %v", kept, err)
		}
	}
	for _, d := range ds {
		if d.Tree == projectfs.TreeLegacyLocalProject && d.Reason == "" {
			t.Errorf("a kept tree names the markers it read: %+v", d)
		}
	}
}

// A marker in settings.yaml counts; a mismatching workspace record of the
// host-local tree keeps it.
func TestDeleteLegacyProjectTrees_SettingsAndRecords(t *testing.T) {
	f := newHubFixture(t)
	viaSettings := filepath.Join(f.share, legacyHubProjectsDir, "demo")
	legacyTree(t, viaSettings, "")
	if err := os.WriteFile(filepath.Join(viaSettings, ".scion", "settings.yaml"), []byte(idSettings(pid1)), 0o644); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(f.global, "projects", "demo")
	legacyTree(t, local, pid1)
	if err := os.MkdirAll(filepath.Join(f.global, "broker-workspaces"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.global, "broker-workspaces", "demo"), []byte(pid2), 0o644); err != nil {
		t.Fatal(err)
	}

	ds, err := DeleteLegacyProjectTrees(f.layout, f.global, "demo", pid1)
	if err != nil {
		t.Fatal(err)
	}
	got := outcomes(ds)
	if got[viaSettings] != projectfs.OutcomeDeleted {
		t.Errorf("settings marker: %q", got[viaSettings])
	}
	if got[local] != projectfs.OutcomeKept {
		t.Errorf("record naming another project must keep the tree: %q", got[local])
	}
}

// A symlinked legacy tree, or a symlinked hub-projects root, is never
// followed; its target stays.
func TestDeleteLegacyProjectTrees_SymlinksRefused(t *testing.T) {
	f := newHubFixture(t)
	victim := t.TempDir()
	legacyTree(t, victim, pid1)
	hp := filepath.Join(f.share, legacyHubProjectsDir)
	if err := os.MkdirAll(hp, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(hp, "demo")); err != nil {
		t.Fatal(err)
	}
	ds, err := DeleteLegacyProjectTrees(f.layout, "", "demo", pid1)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 1 || ds[0].Outcome != projectfs.OutcomeKept {
		t.Fatalf("symlinked tree: %+v", ds)
	}

	g := newHubFixture(t)
	victim2 := t.TempDir()
	legacyTree(t, filepath.Join(victim2, "demo"), pid1)
	if err := os.Symlink(victim2, filepath.Join(g.share, legacyHubProjectsDir)); err != nil {
		t.Fatal(err)
	}
	ds, err = DeleteLegacyProjectTrees(g.layout, "", "demo", pid1)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 1 || ds[0].Outcome != projectfs.OutcomeFailed {
		t.Fatalf("symlinked root: %+v", ds)
	}
	for _, v := range []string{filepath.Join(victim, "README.md"), filepath.Join(victim2, "demo", "README.md")} {
		if _, err := os.Stat(v); err != nil {
			t.Errorf("symlink target %s must stay: %v", v, err)
		}
	}
}

// The local backend is untouched; bad inputs are refused; nothing present
// is nothing to do.
func TestDeleteLegacyProjectTrees_LocalAndValidation(t *testing.T) {
	global := t.TempDir()
	local := filepath.Join(global, "projects", "demo")
	legacyTree(t, local, pid1)
	ds, err := DeleteLegacyProjectTrees(projectfs.Layout{Backend: projectfs.BackendLocal, HostBase: global}, global, "demo", pid1)
	if err != nil || len(ds) != 0 {
		t.Fatalf("local backend: %+v %v", ds, err)
	}
	if _, err := os.Stat(local); err != nil {
		t.Fatal("local backend tree must stay")
	}
	f := newHubFixture(t)
	for _, in := range [][2]string{{"", pid1}, {"Demo", pid1}, {"../x", pid1}, {"demo", ""}, {"demo", "../x"}} {
		if _, err := DeleteLegacyProjectTrees(f.layout, f.global, in[0], in[1]); err == nil {
			t.Errorf("%q: want an error", in)
		}
	}
	if _, err := DeleteLegacyProjectTrees(f.layout, "relative", "demo", pid1); err == nil {
		t.Error("relative global dir: want an error")
	}
	ds, err = DeleteLegacyProjectTrees(f.layout, f.global, "demo", pid1)
	if err != nil || len(ds) != 0 {
		t.Fatalf("nothing present: %+v %v", ds, err)
	}
}

// A host-local .migrated-<ts> copy is identified by its own markers only:
// the slug-keyed workspace record naming the project does not verify a
// copy without markers.
func TestDeleteLegacyProjectTrees_CopyIgnoresSlugRecord(t *testing.T) {
	f := newHubFixture(t)
	copyDir := filepath.Join(f.global, "projects", "demo"+MigratedSuffix+"20261002T120000Z")
	legacyTree(t, copyDir, "")
	if err := os.MkdirAll(filepath.Join(f.global, "hub-workspaces"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.global, "hub-workspaces", "demo"), []byte(pid1), 0o644); err != nil {
		t.Fatal(err)
	}
	ds, err := DeleteLegacyProjectTrees(f.layout, f.global, "demo", pid1)
	if err != nil {
		t.Fatal(err)
	}
	if got := outcomes(ds)[copyDir]; got != projectfs.OutcomeKept {
		t.Fatalf("markerless copy: %q, want kept", got)
	}
}
