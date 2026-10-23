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
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
)

const (
	pid1 = "0f8fad5b-d9cb-469f-a165-70867728950e"
	pid2 = "7c9e6679-7425-40de-944b-e07fc1f90ae7"
)

// fakeObjects is an in-memory ObjectStore.
type fakeObjects struct {
	mu      sync.Mutex
	objects map[string]bool
	deletes []string
}

func (f *fakeObjects) HasObjects(_ context.Context, prefix string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeObjects) DeletePrefix(_ context.Context, prefix string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, prefix)
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			delete(f.objects, k)
		}
	}
	return nil
}

// idSettings is a hub seed settings.yaml naming project id.
func idSettings(id string) string { return "hub:\n  project_id: " + id + "\n" }

func objectPrefix(id string) string { return LegacyObjectPrefix("h1", id) }

type hubFixture struct {
	share, global string
	layout        projectfs.Layout
	objects       *fakeObjects
}

func newHubFixture(t *testing.T) hubFixture {
	t.Helper()
	f := hubFixture{share: t.TempDir(), global: t.TempDir(), objects: &fakeObjects{objects: map[string]bool{}}}
	f.layout = projectfs.Layout{Backend: projectfs.BackendNFS, HostBase: f.share, SubPathRoot: "projects"}
	return f
}

func (f hubFixture) input(projects ...ProjectRef) HubInput {
	return HubInput{
		Workspace:    f.layout,
		SharedDirs:   f.layout,
		GlobalDir:    f.global,
		Projects:     projects,
		Objects:      f.objects,
		ObjectPrefix: objectPrefix,
		Tracked:      func(context.Context, string, string) (bool, error) { return false, nil },
	}
}

func TestLegacyHubProjectsRoot(t *testing.T) {
	nfs := projectfs.Layout{Backend: projectfs.BackendNFS, HostBase: "/mnt/nfs/hub1", SubPathRoot: "tier/projects"}
	got, err := LegacyHubProjectsRoot(nfs)
	if err != nil {
		t.Fatal(err)
	}
	// nfs hubs wrote <mount_root>/<share>/<legacyHubProjectsDir>, without subpath_root.
	if want := (Tree{Base: "/mnt/nfs/hub1", Rel: legacyHubProjectsDir}); got != want {
		t.Fatalf("nfs: got %+v, want %+v", got, want)
	}
	// The volume backends wrote <mnt>/<volume>/<subpath_root>/<legacyHubProjectsDir>.
	for _, b := range []projectfs.Backend{projectfs.BackendCloudRunVolume, projectfs.BackendGKESharedVolume} {
		got, err := LegacyHubProjectsRoot(projectfs.Layout{Backend: b, HostBase: "/mnt/vol", SubPathRoot: "tier/projects"})
		if err != nil {
			t.Fatal(err)
		}
		if want := (Tree{Base: "/mnt/vol", Rel: "tier/projects/" + legacyHubProjectsDir}); got != want {
			t.Fatalf("%s: got %+v, want %+v", b, got, want)
		}
		got, err = LegacyHubProjectsRoot(projectfs.Layout{Backend: b, HostBase: "/mnt/vol"})
		if err != nil || got.Rel != "projects/"+legacyHubProjectsDir {
			t.Fatalf("%s default subpath_root: got %+v err=%v", b, got, err)
		}
	}
	for name, l := range map[string]projectfs.Layout{
		"local":        {Backend: projectfs.BackendLocal, HostBase: "/home/u/.scion"},
		"nfs-no-base":  {Backend: projectfs.BackendNFS},
		"nfs-relative": {Backend: projectfs.BackendNFS, HostBase: "mnt/x"},
		"unknown":      {Backend: "cloudrun-volume", HostBase: ""},
	} {
		if _, err := LegacyHubProjectsRoot(l); err == nil {
			t.Fatalf("%s: want an error (fail closed)", name)
		}
	}
}

// T1: on a local workspace backend the operation touches nothing.
func TestRun_LocalBackendIsNoOp(t *testing.T) {
	global := t.TempDir()
	writeTree(t, global, map[string]string{"projects/p1/a.txt": "A", "project-configs/p1__0f8fad5b/shared-dirs/s/x": "x"})
	snap := snapshot(t, global)
	objects := &fakeObjects{objects: map[string]bool{objectPrefix(pid1) + "a": true}}
	for _, dry := range []bool{true, false} {
		rep, err := Run(context.Background(), HubInput{
			Workspace:    projectfs.Layout{Backend: projectfs.BackendLocal, HostBase: global},
			SharedDirs:   projectfs.Layout{Backend: projectfs.BackendLocal, HostBase: global},
			GlobalDir:    global,
			Projects:     []ProjectRef{{ID: pid1, Slug: "p1"}},
			Objects:      objects,
			ObjectPrefix: objectPrefix,
		}, opts(dry))
		if err != nil || rep.Skipped == "" || rep.Moves() != 0 || len(rep.Projects) != 0 {
			t.Fatalf("dry=%t: err=%v skipped=%q moves=%d", dry, err, rep.Skipped, rep.Moves())
		}
	}
	if got := snapshot(t, global); !reflect.DeepEqual(got, snap) {
		t.Fatal("local backend run changed the hub's files")
	}
	if len(objects.deletes) != 0 {
		t.Fatal("local backend run deleted objects")
	}
}

func seedHubProject(t *testing.T, f hubFixture) {
	t.Helper()
	writeTree(t, f.share, map[string]string{
		legacyHubProjectsDir + "/p1/.scion/settings.yaml": "hub:\n  project_id: " + pid1,
		legacyHubProjectsDir + "/p1/.scion/project-id":    pid1,
		legacyHubProjectsDir + "/p1/README.md":            "readme",
		legacyHubProjectsDir + "/p1/src/main.go":          "package main",
	})
	writeTree(t, f.global, map[string]string{
		"projects/p1/.scion/settings.yaml":                          idSettings(pid1) + "# older local seed\n",
		"projects/p1/notes.txt":                                     "local notes",
		"projects/p1/README.md":                                     "a different readme",
		"project-configs/p1__0f8fad5b/shared-dirs/scratchpad/f.txt": "scratch",
		"project-configs/p1__0f8fad5b/shared-dirs/Bad Name/g.txt":   "bad",
	})
	f.objects.objects[objectPrefix(pid1)+"README.md"] = true
	f.objects.objects["hubs/h1/workspaces/"+pid1+"/agent-1/file"] = true
}

func TestRun_SharedBackendMigratesEveryStep(t *testing.T) {
	f := newHubFixture(t)
	seedHubProject(t, f)
	in := f.input(ProjectRef{ID: pid1, Slug: "p1"})

	// A dry run first: reports moves, changes nothing.
	before := map[string]map[string]string{"share": snapshot(t, f.share), "global": snapshot(t, f.global)}
	dry, err := Run(context.Background(), in, opts(true))
	if err != nil {
		t.Fatal(err)
	}
	if dry.Moves() == 0 || len(dry.PendingProjects()) != 1 {
		t.Fatalf("dry run planned %d moves, pending %v", dry.Moves(), dry.PendingProjects())
	}
	// The invalid shared-dir name fails in the dry run too, so the plan
	// already says the objects would be kept.
	if dry.Objects[0].Outcome != ObjectsKept {
		t.Fatalf("dry-run objects = %s", dry.Objects[0].Outcome)
	}
	after := map[string]map[string]string{"share": snapshot(t, f.share), "global": snapshot(t, f.global)}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("dry run changed files")
	}
	if len(f.objects.deletes) != 0 {
		t.Fatal("dry run deleted objects")
	}

	rep, err := Run(context.Background(), in, opts(false))
	if err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	rep.WriteLog(&log)
	root := filepath.Join(f.share, "projects", pid1)
	ws := filepath.Join(root, "workspace")
	checks := map[string]string{
		filepath.Join(root, ".scion/settings.yaml"):                      "hub:\n  project_id: " + pid1, // step 4 before 1
		filepath.Join(root, ".scion/project-id"):                         pid1,
		filepath.Join(ws, "README.md"):                                   "readme", // step 1 rename wins
		filepath.Join(ws, "src/main.go"):                                 "package main",
		filepath.Join(ws, "notes.txt"):                                   "local notes", // step 2 copy
		filepath.Join(root, "shared-dirs/scratchpad/f.txt"):              "scratch",     // step 3
		filepath.Join(f.global, "projects/p1"+backupSuffix, "README.md"): "a different readme",
	}
	for p, want := range checks {
		if got := readFile(t, p); got != want {
			t.Fatalf("%s = %q, want %q\n%s", p, got, want, log.String())
		}
	}
	assertAbsent(t, filepath.Join(ws, ".scion"))
	assertAbsent(t, filepath.Join(f.share, legacyHubProjectsDir+"/p1"))
	if !strings.Contains(log.String(), "CONFLICT README.md") {
		t.Fatalf("conflict not reported:\n%s", log.String())
	}
	// The invalid shared-dir name fails its step, so the migration did not succeed
	// for this project and its objects are kept.
	if !rep.Failed() || rep.Objects[0].Outcome != ObjectsKept || len(f.objects.deletes) != 0 {
		t.Fatalf("failed=%t objects=%s deletes=%v\n%s", rep.Failed(), rep.Objects[0].Outcome, f.objects.deletes, log.String())
	}

	// After the operator removes the bad entry, a re-run completes the migration for
	// the project and the object-storage cleanup deletes only its project-level copy.
	if err := os.RemoveAll(filepath.Join(f.global, "project-configs/p1__0f8fad5b/shared-dirs/Bad Name")); err != nil {
		t.Fatal(err)
	}
	again, err := Run(context.Background(), in, opts(false))
	if err != nil {
		t.Fatal(err)
	}
	log.Reset()
	again.WriteLog(&log)
	if again.Moves() != 0 || again.Failed() {
		t.Fatalf("re-run: moves=%d failed=%t\n%s", again.Moves(), again.Failed(), log.String())
	}
	if !strings.Contains(log.String(), "moves=0") {
		t.Fatalf("summary does not report 0 moves:\n%s", log.String())
	}
	if want := []string{objectPrefix(pid1)}; !reflect.DeepEqual(f.objects.deletes, want) || again.Objects[0].Outcome != ObjectsDeleted {
		t.Fatalf("deletes = %v (%s), want %v", f.objects.deletes, again.Objects[0].Outcome, want)
	}
	if !f.objects.objects["hubs/h1/workspaces/"+pid1+"/agent-1/file"] {
		t.Fatal("the object cleanup deleted a per-agent sync object")
	}
	// A third run is a no-op end to end.
	third, _ := Run(context.Background(), in, opts(false))
	if third.Moves() != 0 || third.Failed() || third.Objects[0].Outcome != ObjectsNone {
		t.Fatalf("third run: moves=%d failed=%t objects=%s", third.Moves(), third.Failed(), third.Objects[0].Outcome)
	}
}

// the object-storage cleanup never deletes for a project whose the migration failed (a symlinked legacy tree
// is refused).
func TestRun_ObjectsKeptWhenM2Fails(t *testing.T) {
	f := newHubFixture(t)
	outside := t.TempDir()
	writeTree(t, outside, map[string]string{"a.txt": "A"})
	writeTree(t, f.share, map[string]string{legacyHubProjectsDir + "/p1": "->" + outside})
	f.objects.objects[objectPrefix(pid1)+"a.txt"] = true
	rep, err := Run(context.Background(), f.input(ProjectRef{ID: pid1, Slug: "p1"}), opts(false))
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Failed() || rep.Objects[0].Outcome != ObjectsKept || len(f.objects.deletes) != 0 {
		t.Fatalf("failed=%t objects=%s deletes=%v", rep.Failed(), rep.Objects[0].Outcome, f.objects.deletes)
	}
	assertAbsent(t, filepath.Join(f.share, "projects", pid1, "workspace"))
}

func TestRun_EmptyPerAgentLeavesContentMovesSeed(t *testing.T) {
	f := newHubFixture(t)
	writeTree(t, f.share, map[string]string{legacyHubProjectsDir + "/p1/.scion/settings.yaml": idSettings(pid1), legacyHubProjectsDir + "/p1/data.txt": "d"})
	f.objects.objects[objectPrefix(pid1)+"x"] = true
	rep, err := Run(context.Background(), f.input(ProjectRef{ID: pid1, Slug: "p1", NoWorkspace: true}), opts(false))
	if err != nil || rep.Failed() {
		t.Fatalf("err=%v failed=%t", err, rep.Failed())
	}
	if readFile(t, filepath.Join(f.share, "projects", pid1, ".scion/settings.yaml")) != idSettings(pid1) {
		t.Fatal("seed not moved")
	}
	if readFile(t, filepath.Join(f.share, legacyHubProjectsDir+"/p1/data.txt")) != "d" {
		t.Fatal("content of a project without a project-scoped workspace must stay")
	}
	if len(rep.Projects[0].Notes) != 1 {
		t.Fatalf("notes = %v", rep.Projects[0].Notes)
	}
	// Legacy content was left in place: the object-storage cleanup keeps the legacy copy.
	if !f.objects.objects[objectPrefix(pid1)+"x"] || rep.Objects[0].Outcome != ObjectsKept {
		t.Fatalf("objects=%s deletes=%v", rep.Objects[0].Outcome, f.objects.deletes)
	}
}

func TestRun_InfraOnlyLocalTreeIsNotMoved(t *testing.T) {
	f := newHubFixture(t)
	writeTree(t, f.global, map[string]string{"projects/p1/shared-dirs/": ""})
	rep, err := Run(context.Background(), f.input(ProjectRef{ID: pid1, Slug: "p1"}), opts(false))
	if err != nil || rep.Moves() != 0 || rep.Failed() {
		t.Fatalf("err=%v moves=%d failed=%t", err, rep.Moves(), rep.Failed())
	}
	assertAbsent(t, filepath.Join(f.share, "projects", pid1, "workspace"))
}

// Git source: only untracked hub identity entries move; tracked repository
// .scion content and other untracked entries stay in the workspace.
func TestRun_GitSeedMovesOnlyUntrackedIdentity(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	f := newHubFixture(t)
	repo := filepath.Join(f.share, legacyHubProjectsDir+"/p1")
	writeTree(t, repo, map[string]string{
		".scion/templates/t.yaml": "repo template",
		".scion/settings.yaml":    "hub seed",
		".scion/project-id":       pid1,
		".scion/other.txt":        "untracked other",
		"main.go":                 "package main",
	})
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "init.defaultBranch=main", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = repo
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "GIT_CONFIG_NOSYSTEM=1"}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	git("add", ".scion/templates/t.yaml", "main.go")
	git("commit", "-q", "-m", "init")

	in := f.input(ProjectRef{ID: pid1, Slug: "p1"})
	in.Tracked = nil // the real GitTracked
	rep, err := Run(context.Background(), in, opts(false))
	if err != nil || rep.Failed() {
		var b bytes.Buffer
		rep.WriteLog(&b)
		t.Fatalf("err=%v\n%s", err, b.String())
	}
	root := filepath.Join(f.share, "projects", pid1)
	if readFile(t, filepath.Join(root, ".scion/settings.yaml")) != "hub seed" || readFile(t, filepath.Join(root, ".scion/project-id")) != pid1 {
		t.Fatal("identity entries not moved to the project root")
	}
	ws := filepath.Join(root, "workspace")
	if readFile(t, filepath.Join(ws, ".scion/templates/t.yaml")) != "repo template" {
		t.Fatal("tracked repository .scion content must stay in the workspace")
	}
	if readFile(t, filepath.Join(ws, ".scion/other.txt")) != "untracked other" {
		t.Fatal("other untracked entry must stay in the workspace")
	}
	assertAbsent(t, filepath.Join(ws, ".scion/settings.yaml"))
	if countNotes(rep.Projects[0].Notes, "other.txt") != 1 {
		t.Fatalf("notes = %v", rep.Projects[0].Notes)
	}
}

// When step 4 cannot tell the hub seed apart (.git is a file) or fails
// (git errors), steps 1 and 2 do not move that source: the seed never lands
// in agent-visible workspace/. The project stays failed, so the object-storage cleanup keeps its
// objects and the notice stays.
func TestRun_SeedUndecidedOrFailedBlocksWorkspaceMove(t *testing.T) {
	for _, tc := range []struct {
		name    string
		files   map[string]string
		tracked func(context.Context, string, string) (bool, error)
	}{
		{"git-file", map[string]string{legacyHubProjectsDir + "/p1/.git": "gitdir: /elsewhere", legacyHubProjectsDir + "/p1/.scion/settings.yaml": idSettings(pid1), legacyHubProjectsDir + "/p1/a": "a"},
			func(context.Context, string, string) (bool, error) {
				return false, errors.New("tracked check must not run for a .git file")
			}},
		{"git-error", map[string]string{legacyHubProjectsDir + "/p1/.git/HEAD": "ref", legacyHubProjectsDir + "/p1/.scion/settings.yaml": idSettings(pid1), legacyHubProjectsDir + "/p1/a": "a"},
			func(context.Context, string, string) (bool, error) {
				return false, errors.New("exit status 128: detected dubious ownership")
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHubFixture(t)
			writeTree(t, f.share, tc.files)
			f.objects.objects[objectPrefix(pid1)+"a"] = true
			before := snapshot(t, filepath.Join(f.share, legacyHubProjectsDir+"/p1"))
			in := f.input(ProjectRef{ID: pid1, Slug: "p1"})
			in.Tracked = tc.tracked
			rep, err := Run(context.Background(), in, opts(false))
			if err != nil || !rep.Failed() {
				t.Fatalf("err=%v failed=%t, want a failed project", err, rep.Failed())
			}
			assertAbsent(t, filepath.Join(f.share, "projects", pid1, "workspace"))
			assertAbsent(t, filepath.Join(f.share, "projects", pid1, ".scion"))
			if got := snapshot(t, filepath.Join(f.share, legacyHubProjectsDir+"/p1")); !reflect.DeepEqual(got, before) {
				t.Fatal("the legacy tree changed")
			}
			if rep.Objects[0].Outcome != ObjectsKept || len(f.objects.deletes) != 0 {
				t.Fatalf("objects=%s deletes=%v", rep.Objects[0].Outcome, f.objects.deletes)
			}
			if len(rep.PendingProjects()) != 1 {
				t.Fatal("the project must stay pending")
			}
		})
	}
}

func TestRun_TwoProjectsIndependent(t *testing.T) {
	f := newHubFixture(t)
	writeTree(t, f.share, map[string]string{legacyHubProjectsDir + "/p1/a": "1", legacyHubProjectsDir + "/p1/.scion/project-id": pid1, legacyHubProjectsDir + "/p2": "->/etc"})
	f.objects.objects[objectPrefix(pid1)+"a"] = true
	f.objects.objects[objectPrefix(pid2)+"a"] = true
	rep, err := Run(context.Background(), f.input(ProjectRef{ID: pid1, Slug: "p1"}, ProjectRef{ID: pid2, Slug: "p2"}), opts(false))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Projects[0].Failed() || !rep.Projects[1].Failed() {
		t.Fatalf("p1 failed=%t p2 failed=%t", rep.Projects[0].Failed(), rep.Projects[1].Failed())
	}
	if want := []string{objectPrefix(pid1)}; !reflect.DeepEqual(f.objects.deletes, want) {
		t.Fatalf("deletes = %v, want %v", f.objects.deletes, want)
	}
}

func TestCleanObjects_RefusesBadPrefix(t *testing.T) {
	store := &fakeObjects{objects: map[string]bool{"a/b": true}}
	for _, p := range []string{"", "/", "a", "/a/", "a//b/"} {
		if r := CleanObjects(context.Background(), store, p, ProjectReport{}, false); r.Outcome != ObjectsFailed {
			t.Fatalf("%q: outcome %s, want failed", p, r.Outcome)
		}
	}
	if len(store.deletes) != 0 {
		t.Fatal("deleted with a bad prefix")
	}
}

func TestMigrateProject_RejectsBadInput(t *testing.T) {
	for _, in := range []ProjectInput{
		{ID: pid1, Slug: "../x", GlobalDir: "/g"},
		{ID: pid1, Slug: "Up", GlobalDir: "/g"},
		{ID: "", Slug: "p1", GlobalDir: "/g"},
		{ID: pid1, Slug: "p1", GlobalDir: "rel"},
	} {
		if rep := MigrateProject(context.Background(), in, opts(true)); !rep.Failed() {
			t.Fatalf("%+v: want failure", in)
		}
	}
}

func TestRun_DryRunWouldDeleteObjectsButDoesNot(t *testing.T) {
	f := newHubFixture(t)
	writeTree(t, f.share, map[string]string{legacyHubProjectsDir + "/p1/a": "1", legacyHubProjectsDir + "/p1/.scion/project-id": pid1})
	f.objects.objects[objectPrefix(pid1)+"a"] = true
	rep, err := Run(context.Background(), f.input(ProjectRef{ID: pid1, Slug: "p1"}), opts(true))
	if err != nil || rep.Failed() {
		t.Fatalf("err=%v failed=%t", err, rep.Failed())
	}
	if rep.Objects[0].Outcome != ObjectsWouldDelete || len(f.objects.deletes) != 0 || !f.objects.objects[objectPrefix(pid1)+"a"] {
		t.Fatalf("objects=%s deletes=%v", rep.Objects[0].Outcome, f.objects.deletes)
	}
}

func TestRun_VolumeBackendLegacyRoot(t *testing.T) {
	vol := t.TempDir()
	global := t.TempDir()
	writeTree(t, vol, map[string]string{"projects/" + legacyHubProjectsDir + "/p1/a.txt": "A", "projects/" + legacyHubProjectsDir + "/p1/.scion/project-id": pid1})
	layout := projectfs.Layout{Backend: projectfs.BackendCloudRunVolume, HostBase: vol, SubPathRoot: "projects"}
	rep, err := Run(context.Background(), HubInput{Workspace: layout, SharedDirs: projectfs.Layout{Backend: projectfs.BackendLocal}, GlobalDir: global, Projects: []ProjectRef{{ID: pid1, Slug: "p1"}}}, opts(false))
	if err != nil || rep.Failed() || rep.Moves() != 2 { // seed + workspace
		var b bytes.Buffer
		rep.WriteLog(&b)
		t.Fatalf("err=%v failed=%t moves=%d\n%s", err, rep.Failed(), rep.Moves(), b.String())
	}
	if readFile(t, filepath.Join(vol, "projects", pid1, "workspace/a.txt")) != "A" {
		t.Fatal("not moved to the uuid tree on the volume")
	}
	// Fail closed: a volume backend without a mount root is an error.
	if _, err := Run(context.Background(), HubInput{Workspace: projectfs.Layout{Backend: projectfs.BackendGKESharedVolume}, GlobalDir: global}, opts(true)); err == nil {
		t.Fatal("want an error for a volume backend without a mount root")
	}
}

func TestLegacyObjectPrefix(t *testing.T) {
	if got, want := LegacyObjectPrefix("h1", pid1), "hubs/h1/workspaces/"+pid1+"/"+projectkeys.LegacyProjectWorkspaceObjectDir+"/"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got, want := LegacyObjectPrefix("", pid1), "workspaces/"+pid1+"/"+projectkeys.LegacyProjectWorkspaceObjectDir+"/"; got != want {
		t.Fatalf("no hub ID: got %q, want %q", got, want)
	}
	for _, c := range [][2]string{{"h1", ""}, {"h1", ".."}, {"h1", "a/b"}, {"a/b", pid1}, {"..", pid1}} {
		if got := LegacyObjectPrefix(c[0], c[1]); got != "" {
			t.Fatalf("%q: got %q, want refusal", c, got)
		}
	}
}

// the object-storage cleanup deletes only the project-level legacy copy: the live per-agent
// prefixes beside it (CLI bootstrap, scion sync), a sibling project whose
// ID shares a prefix, and other hubs' objects survive.
func TestRun_M3LeavesPerAgentAndSiblingObjects(t *testing.T) {
	f := newHubFixture(t)
	writeTree(t, f.share, map[string]string{legacyHubProjectsDir + "/p1/a": "1", legacyHubProjectsDir + "/p1/.scion/project-id": pid1})
	keep := []string{
		"hubs/h1/workspaces/" + pid1 + "/agent-1/file",
		"hubs/h1/workspaces/" + pid1 + "/" + projectkeys.LegacyProjectWorkspaceObjectDir + "-extra/file",
		"hubs/h1/workspaces/" + pid1 + "x/" + projectkeys.LegacyProjectWorkspaceObjectDir + "/file",
		"hubs/h2/workspaces/" + pid1 + "/" + projectkeys.LegacyProjectWorkspaceObjectDir + "/file",
	}
	for _, k := range keep {
		f.objects.objects[k] = true
	}
	f.objects.objects[objectPrefix(pid1)+"file"] = true
	rep, err := Run(context.Background(), f.input(ProjectRef{ID: pid1, Slug: "p1"}), opts(false))
	if err != nil || rep.Failed() || rep.Objects[0].Outcome != ObjectsDeleted {
		t.Fatalf("err=%v failed=%t objects=%s", err, rep.Failed(), rep.Objects[0].Outcome)
	}
	for _, k := range keep {
		if !f.objects.objects[k] {
			t.Fatalf("the object cleanup deleted %s", k)
		}
	}
	if f.objects.objects[objectPrefix(pid1)+"file"] {
		t.Fatal("legacy copy not deleted")
	}
}

// A hub-local tree carrying broker state is copied from, never changed;
// .scion/agents and shared-dirs are not copied; a re-run moves nothing.
func TestRun_LocalBrokerTreeIsCopyOnly(t *testing.T) {
	for _, tc := range []struct {
		name     string
		files    map[string]string
		provider bool
	}{
		{"evidence", map[string]string{
			"projects/p1/.scion/settings.yaml":    "broker settings",
			"projects/p1/.scion/project-id":       pid1,
			"projects/p1/.scion/agents/a1/home/x": "agent state",
			"projects/p1/notes.txt":               "notes",
			"projects/p1/shared-dirs/s/f":         "old shared",
		}, false},
		{"provider", map[string]string{
			"projects/p1/.scion/settings.yaml": idSettings(pid1),
			"projects/p1/notes.txt":            "notes",
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHubFixture(t)
			writeTree(t, f.global, tc.files)
			local := filepath.Join(f.global, "projects/p1")
			before := snapshot(t, local)
			in := f.input(ProjectRef{ID: pid1, Slug: "p1", LocalBrokerTree: tc.provider})

			dry, err := Run(context.Background(), in, opts(true))
			if err != nil || dry.Moves() == 0 {
				t.Fatalf("dry run: err=%v moves=%d", err, dry.Moves())
			}
			rep, err := Run(context.Background(), in, opts(false))
			if err != nil || rep.Failed() {
				var b bytes.Buffer
				rep.WriteLog(&b)
				t.Fatalf("err=%v\n%s", err, b.String())
			}
			if got := snapshot(t, local); !reflect.DeepEqual(got, before) {
				t.Fatalf("the broker's tree changed:\nbefore %v\nafter  %v", before, got)
			}
			root := filepath.Join(f.share, "projects", pid1)
			if readFile(t, filepath.Join(root, "workspace/notes.txt")) != "notes" {
				t.Fatal("content not copied")
			}
			if readFile(t, filepath.Join(root, ".scion/settings.yaml")) != tc.files["projects/p1/.scion/settings.yaml"] {
				t.Fatal("seed not copied")
			}
			assertAbsent(t, filepath.Join(root, ".scion/agents"))
			assertAbsent(t, filepath.Join(root, "workspace/shared-dirs"))
			assertAbsent(t, filepath.Join(root, "workspace/.scion"))
			again, err := Run(context.Background(), in, opts(false))
			if err != nil || again.Moves() != 0 || again.Failed() {
				t.Fatalf("re-run: err=%v moves=%d failed=%t", err, again.Moves(), again.Failed())
			}
		})
	}
}

// Precondition: with a local workspace Layout the workspace steps do not
// run; a shared shared-dir Layout still gets step 3.
func TestRun_LocalWorkspaceSharedDirsOnly(t *testing.T) {
	f := newHubFixture(t)
	writeTree(t, f.global, map[string]string{
		"projects/p1/a.txt": "A",
		"project-configs/p1__0f8fad5b/shared-dirs/scratchpad/f.txt": "scratch",
	})
	f.objects.objects[objectPrefix(pid1)+"a.txt"] = true
	in := f.input(ProjectRef{ID: pid1, Slug: "p1"})
	in.Workspace = projectfs.Layout{Backend: projectfs.BackendLocal, HostBase: f.global}
	rep, err := Run(context.Background(), in, opts(false))
	if err != nil || rep.Failed() || rep.Skipped != "" {
		t.Fatalf("err=%v failed=%t skipped=%q", err, rep.Failed(), rep.Skipped)
	}
	if readFile(t, filepath.Join(f.share, "projects", pid1, "shared-dirs/scratchpad/f.txt")) != "scratch" {
		t.Fatal("step 3 did not run")
	}
	if readFile(t, filepath.Join(f.global, "projects/p1/a.txt")) != "A" {
		t.Fatal("a workspace step ran on a local workspace Layout")
	}
	assertAbsent(t, filepath.Join(f.share, "projects", pid1, "workspace"))
	if len(rep.Projects[0].Notes) == 0 || !strings.Contains(rep.Projects[0].Notes[0], "not shared") {
		t.Fatalf("notes = %v", rep.Projects[0].Notes)
	}
	// Nothing of the workspace was migrated: the object-storage cleanup keeps the legacy copy.
	if !f.objects.objects[objectPrefix(pid1)+"a.txt"] || len(f.objects.deletes) != 0 || rep.Objects[0].Outcome != ObjectsKept {
		t.Fatalf("objects=%s deletes=%v", rep.Objects[0].Outcome, f.objects.deletes)
	}
}

// A slug-keyed legacy tree is migrated only when its identity markers
// name the project. A tree of another project (a deleted project whose
// slug was reused) is never migrated, not even with adoptUnverified.
func TestRun_IdentityMismatchNeverMigrates(t *testing.T) {
	f := newHubFixture(t)
	writeTree(t, f.share, map[string]string{
		legacyHubProjectsDir + "/p1/.scion/project-id": pid2,
		legacyHubProjectsDir + "/p1/secret.txt":        "other project's data",
	})
	f.objects.objects[objectPrefix(pid1)+"a"] = true
	before := snapshot(t, filepath.Join(f.share, legacyHubProjectsDir, "p1"))
	o := opts(false)
	o.AdoptUnverified = func(string) bool { return true }
	rep, err := Run(context.Background(), f.input(ProjectRef{ID: pid1, Slug: "p1"}), o)
	if err != nil || !rep.Failed() {
		t.Fatalf("err=%v failed=%t", err, rep.Failed())
	}
	assertAbsent(t, filepath.Join(f.share, "projects", pid1))
	if got := snapshot(t, filepath.Join(f.share, legacyHubProjectsDir, "p1")); !reflect.DeepEqual(got, before) {
		t.Fatal("a mismatched tree changed")
	}
	if rep.Objects[0].Outcome != ObjectsKept {
		t.Fatalf("objects = %s", rep.Objects[0].Outcome)
	}
	var b bytes.Buffer
	rep.WriteLog(&b)
	for _, want := range []string{"identity mismatch", pid2, "never adopted"} {
		if !strings.Contains(b.String(), want) {
			t.Fatalf("log lacks %q:\n%s", want, b.String())
		}
	}
}

// A tree without any identity marker is skipped by default (named in the
// report with the parameter to adopt it) and migrates when its project's
// slug is adopted; adopting another slug does not adopt it.
func TestRun_UnverifiedTreeAdoptedPerProject(t *testing.T) {
	f := newHubFixture(t)
	writeTree(t, f.share, map[string]string{legacyHubProjectsDir + "/p1/a.txt": "A"})
	in := f.input(ProjectRef{ID: pid1, Slug: "p1"})

	rep, err := Run(context.Background(), in, opts(false))
	if err != nil || !rep.Failed() {
		t.Fatalf("default: err=%v failed=%t", err, rep.Failed())
	}
	var b bytes.Buffer
	rep.WriteLog(&b)
	for _, want := range []string{"unverified", "no identity marker found", `"adoptUnverified":"p1"`} {
		if !strings.Contains(b.String(), want) {
			t.Fatalf("log lacks %q:\n%s", want, b.String())
		}
	}
	assertAbsent(t, filepath.Join(f.share, "projects", pid1))

	other := opts(false)
	other.AdoptUnverified = func(slug string) bool { return slug == "p2" }
	if rep, _ := Run(context.Background(), in, other); !rep.Failed() {
		t.Fatal("adopting another slug adopted this tree")
	}

	adopt := opts(false)
	adopt.AdoptUnverified = func(slug string) bool { return slug == "p1" }
	rep, err = Run(context.Background(), in, adopt)
	if err != nil || rep.Failed() || rep.Moves() != 1 {
		t.Fatalf("adopted: err=%v failed=%t moves=%d", err, rep.Failed(), rep.Moves())
	}
	if readFile(t, filepath.Join(f.share, "projects", pid1, "workspace/a.txt")) != "A" {
		t.Fatal("adopted tree not migrated")
	}
}

// A seed written by a pre-rename hub carries the legacy project key; read
// through pkg/projectkeys, it verifies the tree (the customer case: such
// projects migrate on the default run), and a legacy key naming another
// project is a mismatch.
func TestRun_LegacyKeyedSeedVerifies(t *testing.T) {
	legacyKey := strings.TrimPrefix(projectkeys.LegacyConfigHubProjectIDKey, "hub.")
	for _, tc := range []struct {
		name   string
		id     string
		wantOK bool
	}{{"same-project", pid1, true}, {"other-project", pid2, false}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHubFixture(t)
			writeTree(t, f.share, map[string]string{
				legacyHubProjectsDir + "/p1/.scion/settings.yaml": "hub:\n  " + legacyKey + ": " + tc.id + "\n",
				legacyHubProjectsDir + "/p1/README.md":            "readme",
			})
			rep, err := Run(context.Background(), f.input(ProjectRef{ID: pid1, Slug: "p1"}), opts(false))
			if err != nil || rep.Failed() == tc.wantOK {
				t.Fatalf("err=%v failed=%t, want ok=%t", err, rep.Failed(), tc.wantOK)
			}
			if tc.wantOK && readFile(t, filepath.Join(f.share, "projects", pid1, "workspace/README.md")) != "readme" {
				t.Fatal("not migrated")
			}
			if !tc.wantOK {
				assertAbsent(t, filepath.Join(f.share, "projects", pid1))
			}
		})
	}
	// The legacy project-id file name is a marker too.
	f := newHubFixture(t)
	writeTree(t, f.share, map[string]string{
		legacyHubProjectsDir + "/p1/.scion/" + projectkeys.LegacyProjectIDFile: pid2,
		legacyHubProjectsDir + "/p1/a":                                         "a",
	})
	if rep, _ := Run(context.Background(), f.input(ProjectRef{ID: pid1, Slug: "p1"}), opts(false)); !rep.Failed() {
		t.Fatal("a legacy project-id file naming another project must be a mismatch")
	}
}

// The hub's workspace record binds a hub-local tree without other markers.
func TestRun_LocalTreeBoundByWorkspaceRecord(t *testing.T) {
	f := newHubFixture(t)
	writeTree(t, f.global, map[string]string{"projects/p1/notes.txt": "n", "hub-workspaces/p1": pid1})
	rep, err := Run(context.Background(), f.input(ProjectRef{ID: pid1, Slug: "p1"}), opts(false))
	if err != nil || rep.Failed() || rep.Moves() != 1 {
		t.Fatalf("err=%v failed=%t moves=%d", err, rep.Failed(), rep.Moves())
	}
	if readFile(t, filepath.Join(f.share, "projects", pid1, "workspace/notes.txt")) != "n" {
		t.Fatal("not migrated")
	}
}

// A legacy workspace source with a shared-dirs/ subtree: the rest moves,
// shared-dirs/ never lands in workspace/ and stays in the .migrated copy.
func TestRun_SharedDirsSubtreeNotMovedIntoWorkspace(t *testing.T) {
	f := newHubFixture(t)
	writeTree(t, f.share, map[string]string{
		legacyHubProjectsDir + "/p1/.scion/project-id":   pid1,
		legacyHubProjectsDir + "/p1/a.txt":               "A",
		legacyHubProjectsDir + "/p1/shared-dirs/s/f.txt": "old shared",
	})
	rep, err := Run(context.Background(), f.input(ProjectRef{ID: pid1, Slug: "p1"}), opts(false))
	if err != nil || rep.Failed() {
		t.Fatalf("err=%v failed=%t", err, rep.Failed())
	}
	ws := filepath.Join(f.share, "projects", pid1, "workspace")
	if readFile(t, filepath.Join(ws, "a.txt")) != "A" {
		t.Fatal("content not migrated")
	}
	assertAbsent(t, filepath.Join(ws, "shared-dirs"))
	if readFile(t, filepath.Join(f.share, legacyHubProjectsDir, "p1"+backupSuffix, "shared-dirs/s/f.txt")) != "old shared" {
		t.Fatal("shared-dirs/ not kept in the .migrated copy")
	}
}

// Residual 3: the broker created the leaf and cloned into it before the
// migration ran. The copy lists conflicts, overwrites nothing, copies the
// rest, and a re-run reports 0 moves.
func TestRun_BrokerClonedFirstThenMigration(t *testing.T) {
	f := newHubFixture(t)
	ws := filepath.Join(f.share, "projects", pid1, "workspace")
	writeTree(t, ws, map[string]string{".git/HEAD": "ref: refs/heads/main", "main.go": "package main // fresh clone"})
	cloned := snapshot(t, ws)
	writeTree(t, f.share, map[string]string{
		legacyHubProjectsDir + "/p1/.scion/project-id": pid1,
		legacyHubProjectsDir + "/p1/.git/HEAD":         "ref: refs/heads/old",
		legacyHubProjectsDir + "/p1/main.go":           "package main // legacy edit",
		legacyHubProjectsDir + "/p1/notes.txt":         "legacy notes",
	})
	in := f.input(ProjectRef{ID: pid1, Slug: "p1", Git: true})
	rep, err := Run(context.Background(), in, opts(false))
	if err != nil || rep.Failed() {
		t.Fatalf("err=%v failed=%t", err, rep.Failed())
	}
	got := snapshot(t, ws)
	for p, v := range cloned {
		if got[p] != v {
			t.Fatalf("clone's %s changed: %q -> %q", p, v, got[p])
		}
	}
	if readFile(t, filepath.Join(ws, "notes.txt")) != "legacy notes" {
		t.Fatal("missing file not copied")
	}
	var b bytes.Buffer
	rep.WriteLog(&b)
	// The broker's .git exists: the legacy .git is a conflict as a whole
	// (never merged into it, no temp dir created).
	assertAbsent(t, filepath.Join(ws, GitMigratingName))
	for _, want := range []string{"CONFLICT main.go", "CONFLICT .git ("} {
		if !strings.Contains(b.String(), want) {
			t.Fatalf("log lacks %q:\n%s", want, b.String())
		}
	}
	again, err := Run(context.Background(), in, opts(false))
	if err != nil || again.Moves() != 0 || again.Failed() {
		t.Fatalf("re-run: err=%v moves=%d failed=%t", err, again.Moves(), again.Failed())
	}
}

// Residual 1: a git project's tree migrated without a .git is reported,
// because its first agent start will refuse.
func TestRun_GitProjectWithoutGitDirIsReported(t *testing.T) {
	f := newHubFixture(t)
	writeTree(t, f.share, map[string]string{
		legacyHubProjectsDir + "/p1/.scion/project-id": pid1,
		legacyHubProjectsDir + "/p1/notes.txt":         "n",
	})
	rep, err := Run(context.Background(), f.input(ProjectRef{ID: pid1, Slug: "p1", Git: true}), opts(false))
	if err != nil || rep.Failed() {
		t.Fatalf("err=%v failed=%t", err, rep.Failed())
	}
	found := false
	for _, n := range rep.Projects[0].Notes {
		found = found || strings.Contains(n, "first agent start will refuse")
	}
	if !found {
		t.Fatalf("notes = %v", rep.Projects[0].Notes)
	}
	// A non-git project gets no such note.
	g := newHubFixture(t)
	writeTree(t, g.share, map[string]string{legacyHubProjectsDir + "/p1/.scion/project-id": pid1, legacyHubProjectsDir + "/p1/notes.txt": "n"})
	rep, _ = Run(context.Background(), g.input(ProjectRef{ID: pid1, Slug: "p1"}), opts(false))
	for _, n := range rep.Projects[0].Notes {
		if strings.Contains(n, "refuse") {
			t.Fatalf("unexpected note for a non-git project: %q", n)
		}
	}
}

// The hub's startup identity pass has already created <ProjectRoot>/.scion
// with project-id and project-slug: step 4 is a plain merge. settings.yaml
// and the template tiers are copied in, the identical project-id is
// identical (not a conflict), a differing project-slug is a conflict that
// is not overwritten and not an identity mismatch, broker records stay
// out, and nothing .scion-ish lands in workspace/ (the step-4 backups stay
// in the source's .migrated copy). Run plain, with every rename flag
// rejected (NFS) and with renames failing EXDEV (cross-device).
func TestRun_HubSeededTargetMergeNoSeedLeak(t *testing.T) {
	for _, mode := range []struct {
		name  string
		errno syscall.Errno
	}{{"plain", 0}, {"nfs-einval", syscall.EINVAL}, {"exdev", syscall.EXDEV}} {
		t.Run(mode.name, func(t *testing.T) {
			switch mode.errno {
			case syscall.EXDEV:
				// Cross-device: renames between directories fail; a rename
				// within one directory (the .migrated backup) cannot.
				old := renameat2
				renameat2 = func(od int, on string, nd int, nn string, flags uint) error {
					if od != nd {
						return syscall.EXDEV
					}
					return old(od, on, nd, nn, flags)
				}
				t.Cleanup(func() { renameat2 = old })
			case 0:
			default:
				forceRename(t, nil, mode.errno)
			}
			f := newHubFixture(t)
			root := filepath.Join(f.share, "projects", pid1)
			writeTree(t, root, map[string]string{".scion/project-id": pid1, ".scion/project-slug": "renamed\n"})
			writeTree(t, f.share, map[string]string{
				legacyHubProjectsDir + "/p1/.scion/settings.yaml":        idSettings(pid1),
				legacyHubProjectsDir + "/p1/.scion/project-id":           pid1,
				legacyHubProjectsDir + "/p1/.scion/project-slug":         "p1\n",
				legacyHubProjectsDir + "/p1/.scion/templates/t/agent.md": "tpl",
				legacyHubProjectsDir + "/p1/.scion/harness-configs/h/c":  "hc",
				legacyHubProjectsDir + "/p1/.scion/agents/a1/x":          "broker record",
				legacyHubProjectsDir + "/p1/.scion/other.txt":            "other",
				legacyHubProjectsDir + "/p1/main.go":                     "package main",
			})
			in := f.input(ProjectRef{ID: pid1, Slug: "p1"})
			rep, err := Run(context.Background(), in, opts(false))
			var b bytes.Buffer
			rep.WriteLog(&b)
			if err != nil || rep.Failed() {
				t.Fatalf("err=%v failed=%t\n%s", err, rep.Failed(), b.String())
			}
			for p, want := range map[string]string{
				".scion/settings.yaml":        idSettings(pid1),
				".scion/project-id":           pid1,
				".scion/project-slug":         "renamed\n",
				".scion/templates/t/agent.md": "tpl",
				".scion/harness-configs/h/c":  "hc",
				"workspace/main.go":           "package main",
			} {
				if got := readFile(t, filepath.Join(root, p)); got != want {
					t.Fatalf("%s = %q, want %q\n%s", p, got, want, b.String())
				}
			}
			assertAbsent(t, filepath.Join(root, ".scion/agents"))
			assertAbsent(t, filepath.Join(root, ".scion/other.txt"))
			if !strings.Contains(b.String(), "CONFLICT .") || strings.Contains(b.String(), "identity mismatch") {
				t.Fatalf("want a project-slug conflict and no identity mismatch:\n%s", b.String())
			}
			entries, err := os.ReadDir(filepath.Join(root, "workspace"))
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), ".scion") {
					t.Fatalf("%s landed in workspace/\n%s", e.Name(), b.String())
				}
			}
			again, err := Run(context.Background(), in, opts(false))
			if err != nil || again.Moves() != 0 || again.Failed() {
				t.Fatalf("re-run: err=%v moves=%d failed=%t", err, again.Moves(), again.Failed())
			}
		})
	}
}

// Git source on the copy path: the per-entry backup
// .scion/settings.yaml.migrated-<ts> never reaches workspace/.
func TestRun_GitSeedBackupNotCarriedIntoWorkspace(t *testing.T) {
	forceRename(t, nil, syscall.EINVAL)
	f := newHubFixture(t)
	root := filepath.Join(f.share, "projects", pid1)
	writeTree(t, root, map[string]string{".scion/settings.yaml": "hub's new seed"})
	writeTree(t, f.share, map[string]string{
		legacyHubProjectsDir + "/p1/.git/HEAD":            "ref",
		legacyHubProjectsDir + "/p1/.scion/settings.yaml": idSettings(pid1),
		legacyHubProjectsDir + "/p1/main.go":              "package main",
	})
	rep, err := Run(context.Background(), f.input(ProjectRef{ID: pid1, Slug: "p1", Git: true}), opts(false))
	if err != nil || rep.Failed() {
		t.Fatalf("err=%v failed=%t", err, rep.Failed())
	}
	if readFile(t, filepath.Join(root, ".scion/settings.yaml")) != "hub's new seed" {
		t.Fatal("target settings overwritten")
	}
	err = filepath.WalkDir(filepath.Join(root, "workspace"), func(p string, d os.DirEntry, err error) error {
		if err == nil && strings.Contains(d.Name(), MigratedSuffix) {
			t.Fatalf("seed backup %s landed in workspace/", p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// fakeLock is a ProvisionLock that serializes on a mutex, records the state
// dir it was given, and can hand the section an already-lost context.
type fakeLock struct {
	mu       sync.Mutex
	dirs     []string
	lose     bool
	inLock   bool
	observed func()
}

func (l *fakeLock) lock(ws Tree, section func(context.Context) error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.dirs = append(l.dirs, ws.String())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if l.lose {
		cancel()
	}
	l.inLock = true
	defer func() { l.inLock = false }()
	if l.observed != nil {
		l.observed()
	}
	return section(ctx)
}

// Steps 4/1/2 run inside the project's provisioning lock, taken on
// <ProjectRoot>/provision created with the leaf contract; a provisioner
// waiting on the same lock only ever sees the complete workspace.
func TestRun_WorkspaceMovesHoldProvisioningLock(t *testing.T) {
	forceRename(t, nil, syscall.EINVAL) // NFS: the long copy path
	f := newHubFixture(t)
	files := map[string]string{legacyHubProjectsDir + "/p1/.scion/project-id": pid1}
	for i := 0; i < 40; i++ {
		files[fmt.Sprintf("%s/p1/d%02d/f.txt", legacyHubProjectsDir, i)] = "x"
	}
	writeTree(t, f.share, files)
	ws := filepath.Join(f.share, "projects", pid1, "workspace")
	lk := &fakeLock{}
	var seen map[string]string
	var wg sync.WaitGroup
	lk.observed = func() {
		if _, err := os.Lstat(ws); !os.IsNotExist(err) {
			t.Errorf("workspace touched before the lock was held")
		}
		wg.Add(1)
		go func() { // a provisioner waiting on the same lock
			defer wg.Done()
			lk.mu.Lock()
			defer lk.mu.Unlock()
			seen = snapshot(t, ws)
		}()
	}
	in := f.input(ProjectRef{ID: pid1, Slug: "p1"})
	in.ProvisionLock = lk.lock
	rep, err := Run(context.Background(), in, opts(false))
	wg.Wait()
	if err != nil || rep.Failed() {
		t.Fatalf("err=%v failed=%t", err, rep.Failed())
	}
	// The lock is taken for the project's workspace (the hub derives the
	// lock directories from it through pkg/provision).
	if len(lk.dirs) != 1 || lk.dirs[0] != ws {
		t.Fatalf("lock workspaces = %v, want [%s]", lk.dirs, ws)
	}
	if final := snapshot(t, ws); !reflect.DeepEqual(seen, final) || len(final) < 41 {
		t.Fatalf("the waiting provisioner saw %d entries, final %d", len(seen), len(final))
	}
	if countNotes(rep.Projects[0].Notes, "agent starts for this project waited") != 1 {
		t.Fatalf("notes = %v", rep.Projects[0].Notes)
	}
	// No lock for a project without legacy content.
	g := newHubFixture(t)
	lk2 := &fakeLock{}
	in2 := g.input(ProjectRef{ID: pid1, Slug: "p1"})
	in2.ProvisionLock = lk2.lock
	if _, err := Run(context.Background(), in2, opts(false)); err != nil || len(lk2.dirs) != 0 {
		t.Fatalf("err=%v lock taken %d times without legacy content", err, len(lk2.dirs))
	}
	assertAbsent(t, filepath.Join(g.share, "projects", pid1, "provision"))
}

// A lost lock stops the migration before any write: the project is
// pending with the reason, the legacy tree is intact, the object-storage cleanup keeps objects.
func TestRun_LostProvisioningLockStops(t *testing.T) {
	f := newHubFixture(t)
	writeTree(t, f.share, map[string]string{legacyHubProjectsDir + "/p1/.scion/project-id": pid1, legacyHubProjectsDir + "/p1/a": "a"})
	before := snapshot(t, filepath.Join(f.share, legacyHubProjectsDir, "p1"))
	f.objects.objects[objectPrefix(pid1)+"a"] = true
	in := f.input(ProjectRef{ID: pid1, Slug: "p1"})
	in.ProvisionLock = (&fakeLock{lose: true}).lock
	rep, err := Run(context.Background(), in, opts(false))
	if err != nil || !rep.Failed() || len(rep.PendingProjects()) != 1 {
		t.Fatalf("err=%v failed=%t", err, rep.Failed())
	}
	var b bytes.Buffer
	rep.WriteLog(&b)
	if !strings.Contains(b.String(), "lost the project's provisioning lock") {
		t.Fatalf("log:\n%s", b.String())
	}
	if got := snapshot(t, filepath.Join(f.share, legacyHubProjectsDir, "p1")); !reflect.DeepEqual(got, before) {
		t.Fatal("the legacy tree changed under a lost lock")
	}
	assertAbsent(t, filepath.Join(f.share, "projects", pid1, "workspace"))
	if rep.Objects[0].Outcome != ObjectsKept {
		t.Fatalf("objects = %s", rep.Objects[0].Outcome)
	}
}

// the object-storage cleanup keeps the object copy when there was no legacy content and the
// target workspace is empty: it may be the only copy.
func TestRun_M3KeepsObjectsWithoutAnyWorkspaceContent(t *testing.T) {
	f := newHubFixture(t)
	f.objects.objects[objectPrefix(pid1)+"only-copy"] = true
	rep, err := Run(context.Background(), f.input(ProjectRef{ID: pid1, Slug: "p1"}), opts(false))
	if err != nil || rep.Failed() {
		t.Fatalf("err=%v failed=%t", err, rep.Failed())
	}
	if rep.Objects[0].Outcome != ObjectsKept || len(f.objects.deletes) != 0 {
		t.Fatalf("objects=%s deletes=%v", rep.Objects[0].Outcome, f.objects.deletes)
	}
	// A populated target (provisioned earlier) lets the object-storage cleanup delete.
	writeTree(t, f.share, map[string]string{"projects/" + pid1 + "/workspace/x": "x"})
	rep, _ = Run(context.Background(), f.input(ProjectRef{ID: pid1, Slug: "p1"}), opts(false))
	if rep.Objects[0].Outcome != ObjectsDeleted {
		t.Fatalf("objects = %s", rep.Objects[0].Outcome)
	}
}

// One matching and one mismatching marker: the mismatch wins.
func TestRun_MixedMarkersMismatchWins(t *testing.T) {
	f := newHubFixture(t)
	legacyKey := strings.TrimPrefix(projectkeys.LegacyConfigHubProjectIDKey, "hub.")
	writeTree(t, f.share, map[string]string{
		legacyHubProjectsDir + "/p1/.scion/project-id":    pid1,
		legacyHubProjectsDir + "/p1/.scion/settings.yaml": "hub:\n  " + legacyKey + ": " + pid2 + "\n",
		legacyHubProjectsDir + "/p1/a":                    "a",
	})
	rep, err := Run(context.Background(), f.input(ProjectRef{ID: pid1, Slug: "p1"}), opts(false))
	if err != nil || !rep.Failed() {
		t.Fatalf("err=%v failed=%t", err, rep.Failed())
	}
	assertAbsent(t, filepath.Join(f.share, "projects", pid1))
}

// countNotes counts the notes containing sub.
func countNotes(notes []string, sub string) int {
	n := 0
	for _, note := range notes {
		if strings.Contains(note, sub) {
			n++
		}
	}
	return n
}

// The migration only reads the hub/broker workspace records (identity
// markers): a run leaves existing records byte-identical and creates none
// (the hub no longer writes them; the migration only reads them).
func TestRun_WritesNoWorkspaceRecords(t *testing.T) {
	f := newHubFixture(t)
	writeTree(t, f.global, map[string]string{
		"projects/p1/notes.txt": "n",
		"hub-workspaces/p1":     pid1,
		"broker-workspaces/p1":  pid1,
	})
	writeTree(t, f.share, map[string]string{legacyHubProjectsDir + "/p2/.scion/project-id": pid2, legacyHubProjectsDir + "/p2/a": "a"})
	before := map[string]map[string]string{
		"hub":    snapshot(t, filepath.Join(f.global, "hub-workspaces")),
		"broker": snapshot(t, filepath.Join(f.global, "broker-workspaces")),
	}
	rep, err := Run(context.Background(), f.input(ProjectRef{ID: pid1, Slug: "p1"}, ProjectRef{ID: pid2, Slug: "p2"}), opts(false))
	if err != nil || rep.Failed() || rep.Moves() == 0 {
		t.Fatalf("err=%v failed=%t moves=%d", err, rep.Failed(), rep.Moves())
	}
	after := map[string]map[string]string{
		"hub":    snapshot(t, filepath.Join(f.global, "hub-workspaces")),
		"broker": snapshot(t, filepath.Join(f.global, "broker-workspaces")),
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("workspace records changed:\nbefore %v\nafter  %v", before, after)
	}
	assertAbsent(t, filepath.Join(f.global, "hub-workspaces", "p2"))
	assertAbsent(t, filepath.Join(f.global, "broker-workspaces", "p2"))
}

// gitRepoFiles builds a legacy git workspace with several .git entries.
func gitRepoFiles() map[string]string {
	files := map[string]string{
		legacyHubProjectsDir + "/p1/.scion/project-id": pid1,
		legacyHubProjectsDir + "/p1/main.go":           "package main",
		legacyHubProjectsDir + "/p1/.git/HEAD":         "ref: refs/heads/main",
	}
	for i := 0; i < 10; i++ {
		files[fmt.Sprintf("%s/p1/.git/objects/%02d/obj", legacyHubProjectsDir, i)] = "blob"
	}
	return files
}

// .git is copied last under GitMigratingName and renamed
// in only when complete. Interrupted inside .git: workspace/ has no .git
// (a first start refuses: not empty, no marker, no .git), only the temp
// dir; the re-run resumes, renames it to .git, and the result is the full
// repository.
func TestRun_GitCopiedLastAndRenamedInWhenComplete(t *testing.T) {
	forceRename(t, nil, syscall.EINVAL) // NFS: the copy path
	f := newHubFixture(t)
	writeTree(t, f.share, gitRepoFiles())
	ws := filepath.Join(f.share, "projects", pid1, "workspace")
	in := f.input(ProjectRef{ID: pid1, Slug: "p1", Git: true})

	lk := &fakeLock{}
	var cancel context.CancelFunc
	in.ProvisionLock = func(w Tree, section func(context.Context) error) error {
		return lk.lock(w, func(ctx context.Context) error {
			c, cf := context.WithCancel(ctx)
			cancel = cf
			defer cf()
			return section(c)
		})
	}
	seen := 0
	old := copyEntryHook
	copyEntryHook = func(rel string) {
		if strings.HasPrefix(rel, ".git/objects/") {
			seen++
			if seen == 3 && cancel != nil {
				cancel() // the lock is lost mid-.git copy
			}
		}
		if rel == "main.go" {
			// Everything else is copied before .git starts.
			if _, err := os.Lstat(filepath.Join(ws, GitMigratingName)); !os.IsNotExist(err) {
				t.Errorf(".git copy started before the other entries")
			}
		}
	}
	t.Cleanup(func() { copyEntryHook = old })

	rep, err := Run(context.Background(), in, opts(false))
	if err != nil || !rep.Failed() || len(rep.PendingProjects()) != 1 {
		t.Fatalf("interrupted run: err=%v failed=%t", err, rep.Failed())
	}
	assertAbsent(t, filepath.Join(ws, ".git")) // a provisioner refuses, never adopts
	if _, err := os.Stat(filepath.Join(ws, GitMigratingName)); err != nil {
		t.Fatalf("temp dir missing after the interruption: %v", err)
	}
	if readFile(t, filepath.Join(f.share, legacyHubProjectsDir, "p1/.git/HEAD")) != "ref: refs/heads/main" {
		t.Fatal("source not left in place after the interruption")
	}
	var b bytes.Buffer
	rep.WriteLog(&b)
	if !strings.Contains(b.String(), "lost the project's provisioning lock") || !strings.Contains(b.String(), "interrupted, run again before starting agents") {
		t.Fatalf("log:\n%s", b.String())
	}

	// Re-run: resumes into the temp dir and renames it in.
	copyEntryHook = old
	in.ProvisionLock = (&fakeLock{}).lock
	rep, err = Run(context.Background(), in, opts(false))
	b.Reset()
	rep.WriteLog(&b)
	if err != nil || rep.Failed() {
		t.Fatalf("re-run: err=%v failed=%t\n%s", err, rep.Failed(), b.String())
	}
	assertAbsent(t, filepath.Join(ws, GitMigratingName))
	if readFile(t, filepath.Join(ws, ".git/HEAD")) != "ref: refs/heads/main" || readFile(t, filepath.Join(ws, ".git/objects/09/obj")) != "blob" {
		t.Fatal("the renamed-in .git is not complete")
	}
	if strings.Contains(b.String(), "CONFLICT") {
		t.Fatalf("a resumed run must not report conflicts:\n%s", b.String())
	}
}

// A run stopped by the run's own context (timeout, shutdown) is reported
// as such, not as a lost lock.
func TestRun_RunStoppedIsNotReportedAsLostLock(t *testing.T) {
	forceRename(t, nil, syscall.EINVAL)
	f := newHubFixture(t)
	writeTree(t, f.share, gitRepoFiles())
	in := f.input(ProjectRef{ID: pid1, Slug: "p1", Git: true})
	ctx, cancel := context.WithCancel(context.Background())
	in.ProvisionLock = func(w Tree, section func(context.Context) error) error {
		return section(ctx) // the lock context derives from the run context
	}
	old := copyEntryHook
	copyEntryHook = func(rel string) {
		if rel == "main.go" {
			cancel()
		}
	}
	t.Cleanup(func() { copyEntryHook = old; cancel() })
	rep, _ := Run(ctx, in, opts(false))
	var b bytes.Buffer
	rep.WriteLog(&b)
	if !strings.Contains(b.String(), "the migration run stopped") || strings.Contains(b.String(), "lost the project's provisioning lock") {
		t.Fatalf("log:\n%s", b.String())
	}
}

// A copy into a hard-linked source's target is chowned: the new file is
// never hard-linked.
func TestRun_CopyOfHardLinkedSourceIsChowned(t *testing.T) {
	forceRename(t, nil, syscall.EINVAL)
	f := newHubFixture(t)
	writeTree(t, f.share, map[string]string{legacyHubProjectsDir + "/p1/.scion/project-id": pid1, legacyHubProjectsDir + "/p1/a.txt": "A"})
	if err := os.Link(filepath.Join(f.share, legacyHubProjectsDir, "p1/a.txt"), filepath.Join(f.share, legacyHubProjectsDir, "p1/b.txt")); err != nil {
		t.Fatal(err)
	}
	chowned := recordChowns(t, nil)
	in := f.input(ProjectRef{ID: pid1, Slug: "p1"})
	in.Workspace.UID, in.Workspace.GID = os.Getuid(), os.Getgid()
	rep, err := Run(context.Background(), in, opts(false))
	if err != nil || rep.Failed() {
		t.Fatalf("err=%v failed=%t", err, rep.Failed())
	}
	got := strings.Join(chowned(), "\n")
	for _, n := range []string{"workspace/a.txt", "workspace/b.txt"} {
		if !strings.Contains(got, n) {
			t.Fatalf("%s not chowned; chowned:\n%s", n, got)
		}
	}
	for _, e := range rep.Projects[0].OwnershipErrors() {
		if strings.Contains(e.Error(), "hard-linked") {
			t.Fatalf("a fresh copy reported as hard-linked: %v", e)
		}
	}
}

// Provisioning lock litter and the temp .git dir do not make a workspace
// "populated" for the object-storage cleanup.
func TestRun_M3IgnoresLockLitter(t *testing.T) {
	f := newHubFixture(t)
	writeTree(t, f.share, map[string]string{
		"projects/" + pid1 + "/workspace/.scion-provision.lock/owner": "x",
		"projects/" + pid1 + "/workspace/.scion-provisioned":          "x",
		"projects/" + pid1 + "/workspace/" + GitMigratingName + "/x":  "x",
		"projects/" + pid1 + "/workspace/" + CopyTempPrefix + "ab12":  "x",
	})
	f.objects.objects[objectPrefix(pid1)+"a"] = true
	rep, err := Run(context.Background(), f.input(ProjectRef{ID: pid1, Slug: "p1"}), opts(false))
	if err != nil || rep.Objects[0].Outcome != ObjectsKept {
		t.Fatalf("err=%v objects=%s", err, rep.Objects[0].Outcome)
	}
}

// A truncated file in the temp .git (from a
// killed earlier run) differs from the source: the temp dir is NOT renamed
// into .git, the project stays pending, the object-storage cleanup keeps its objects.
func TestRun_TruncatedFileInTempGitBlocksRename(t *testing.T) {
	forceRename(t, nil, syscall.EINVAL)
	f := newHubFixture(t)
	writeTree(t, f.share, gitRepoFiles())
	ws := filepath.Join(f.share, "projects", pid1, "workspace")
	writeTree(t, ws, map[string]string{GitMigratingName + "/objects/05/obj": "bl"}) // truncated "blob"
	f.objects.objects[objectPrefix(pid1)+"a"] = true
	rep, err := Run(context.Background(), f.input(ProjectRef{ID: pid1, Slug: "p1", Git: true}), opts(false))
	if err != nil || !rep.Failed() || len(rep.PendingProjects()) != 1 {
		t.Fatalf("err=%v failed=%t", err, rep.Failed())
	}
	assertAbsent(t, filepath.Join(ws, ".git"))
	if readFile(t, filepath.Join(ws, GitMigratingName, "objects/05/obj")) != "bl" {
		t.Fatal("the temp dir must be left for inspection")
	}
	var b bytes.Buffer
	rep.WriteLog(&b)
	if !strings.Contains(b.String(), ".git copy incomplete") || rep.Objects[0].Outcome != ObjectsKept {
		t.Fatalf("objects=%s log:\n%s", rep.Objects[0].Outcome, b.String())
	}
	if readFile(t, filepath.Join(f.share, legacyHubProjectsDir, "p1/.git/HEAD")) != "ref: refs/heads/main" {
		t.Fatal("source must stay in place")
	}
}

// A run killed in the middle of copying a file leaves no partial file
// under its final name (only a temp file); the next run removes the temp
// litter and completes; litter never counts as workspace content.
func TestRun_KilledCopyLeavesNoPartialFinalFile(t *testing.T) {
	forceRename(t, nil, syscall.EINVAL)
	f := newHubFixture(t)
	writeTree(t, f.share, map[string]string{
		legacyHubProjectsDir + "/p1/.scion/project-id": pid1,
		legacyHubProjectsDir + "/p1/big.bin":           strings.Repeat("x", 4096),
	})
	ws := filepath.Join(f.share, "projects", pid1, "workspace")
	old := copyCrashHook
	t.Cleanup(func() { copyCrashHook = old })
	copyCrashHook = func(name string) error {
		if name == "big.bin" {
			return errSimulatedKill
		}
		return nil
	}
	in := f.input(ProjectRef{ID: pid1, Slug: "p1"})
	rep, _ := Run(context.Background(), in, opts(false))
	copyCrashHook = old
	if !rep.Failed() {
		t.Fatal("the killed run must not succeed")
	}
	assertAbsent(t, filepath.Join(ws, "big.bin"))
	entries, err := os.ReadDir(ws)
	if err != nil {
		t.Fatal(err)
	}
	litter := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), CopyTempPrefix) {
			litter++
		}
	}
	if litter != 1 {
		t.Fatalf("want one temp file left by the kill, got %d (%v)", litter, entries)
	}
	if rep.Projects[0].WorkspacePopulated {
		t.Fatal("temp litter counted as workspace content")
	}
	// The next run cleans the litter and completes.
	rep, err = Run(context.Background(), in, opts(false))
	if err != nil || rep.Failed() {
		t.Fatalf("re-run: err=%v failed=%t", err, rep.Failed())
	}
	if readFile(t, filepath.Join(ws, "big.bin")) != strings.Repeat("x", 4096) {
		t.Fatal("not copied on the re-run")
	}
	entries, _ = os.ReadDir(ws)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), CopyTempPrefix) {
			t.Fatalf("temp litter left: %s", e.Name())
		}
	}
	if third, err := Run(context.Background(), in, opts(false)); err != nil || third.Moves() != 0 || third.Failed() {
		t.Fatalf("third run: err=%v moves=%d failed=%t", err, third.Moves(), third.Failed())
	}
}

// A legacy tree holding only .scion is kept
// aside once its configuration is copied; re-runs neither take the lock
// again nor report conflicts after the target settings are edited.
func TestRun_ScionOnlyTreeKeptAside(t *testing.T) {
	f := newHubFixture(t)
	writeTree(t, f.share, map[string]string{legacyHubProjectsDir + "/p1/.scion/settings.yaml": idSettings(pid1)})
	lk := &fakeLock{}
	in := f.input(ProjectRef{ID: pid1, Slug: "p1"})
	in.ProvisionLock = lk.lock
	rep, err := Run(context.Background(), in, opts(false))
	if err != nil || rep.Failed() {
		t.Fatalf("run 1: err=%v failed=%t", err, rep.Failed())
	}
	assertAbsent(t, filepath.Join(f.share, legacyHubProjectsDir, "p1"))
	if readFile(t, filepath.Join(f.share, legacyHubProjectsDir, "p1"+backupSuffix, ".scion/settings.yaml")) != idSettings(pid1) {
		t.Fatal("the legacy tree was not kept aside")
	}
	// The operator edits the project configuration.
	if err := os.WriteFile(filepath.Join(f.share, "projects", pid1, ".scion/settings.yaml"), []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for run := 2; run <= 3; run++ {
		rep, err := Run(context.Background(), in, opts(false))
		var b bytes.Buffer
		rep.WriteLog(&b)
		if err != nil || rep.Failed() || rep.Moves() != 0 || strings.Contains(b.String(), "CONFLICT") {
			t.Fatalf("run %d: err=%v failed=%t moves=%d\n%s", run, err, rep.Failed(), rep.Moves(), b.String())
		}
	}
	if len(lk.dirs) != 1 {
		t.Fatalf("lock taken %d times; re-runs must not lock", len(lk.dirs))
	}
}

// keepAside negatives: a broker's copy-only .scion-only tree is never kept
// aside; a tree with content takes the normal move; a dry run is
// read-only.
func TestRun_KeepAsideNegatives(t *testing.T) {
	t.Run("copy-only", func(t *testing.T) {
		f := newHubFixture(t)
		writeTree(t, f.global, map[string]string{"projects/p1/.scion/settings.yaml": idSettings(pid1), "projects/p1/.scion/agents/a/x": "x"})
		before := snapshot(t, filepath.Join(f.global, "projects/p1"))
		if rep, err := Run(context.Background(), f.input(ProjectRef{ID: pid1, Slug: "p1"}), opts(false)); err != nil || rep.Failed() {
			t.Fatalf("err=%v failed=%t", err, rep.Failed())
		}
		if got := snapshot(t, filepath.Join(f.global, "projects/p1")); !reflect.DeepEqual(got, before) {
			t.Fatal("a broker's tree was changed")
		}
	})
	t.Run("content-tree", func(t *testing.T) {
		f := newHubFixture(t)
		writeTree(t, f.share, map[string]string{legacyHubProjectsDir + "/p1/.scion/settings.yaml": idSettings(pid1), legacyHubProjectsDir + "/p1/a.txt": "A"})
		rep, err := Run(context.Background(), f.input(ProjectRef{ID: pid1, Slug: "p1"}), opts(false))
		if err != nil || rep.Failed() {
			t.Fatalf("err=%v failed=%t", err, rep.Failed())
		}
		if readFile(t, filepath.Join(f.share, "projects", pid1, "workspace/a.txt")) != "A" {
			t.Fatal("content not moved by the workspace step")
		}
	})
	t.Run("dry-run", func(t *testing.T) {
		f := newHubFixture(t)
		writeTree(t, f.share, map[string]string{legacyHubProjectsDir + "/p1/.scion/settings.yaml": idSettings(pid1)})
		before := snapshot(t, f.share)
		rep, err := Run(context.Background(), f.input(ProjectRef{ID: pid1, Slug: "p1"}), opts(true))
		if err != nil || rep.Moves() == 0 {
			t.Fatalf("err=%v moves=%d", err, rep.Moves())
		}
		if got := snapshot(t, f.share); !reflect.DeepEqual(got, before) {
			t.Fatal("a dry run changed the share")
		}
	})
}
