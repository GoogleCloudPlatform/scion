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
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

// recordChowns replaces fchownat with a recorder that returns err (nil
// performs the real call) and returns the chowned host paths.
func recordChowns(t *testing.T, err error) func() []string {
	t.Helper()
	var mu sync.Mutex
	var paths []string
	old := fchownat
	fchownat = func(dirfd int, name string, uid, gid, flags int) error {
		dir, rerr := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", dirfd))
		if rerr != nil {
			t.Errorf("readlink dirfd: %v", rerr)
		}
		mu.Lock()
		paths = append(paths, filepath.Join(dir, name))
		mu.Unlock()
		if err != nil {
			return err
		}
		return old(dirfd, name, uid, gid, flags)
	}
	t.Cleanup(func() { fchownat = old })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := append([]string(nil), paths...)
		sort.Strings(out)
		return out
	}
}

func self() *Ownership { return &Ownership{UID: os.Getuid(), GID: os.Getgid()} }

func mode(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode()
}

// Workspace: a renamed tree is chowned entry by entry (links as links) and
// keeps its modes.
func TestNormalize_WorkspaceRenameChownsKeepsModes(t *testing.T) {
	base := t.TempDir()
	writeTree(t, base, map[string]string{"old/proj/a.txt": "A", "old/proj/d/b.txt": "B", "old/proj/l": "->a.txt"})
	chowned := recordChowns(t, nil)
	s := spec(base, "old/proj", "projects/u1/workspace")
	s.Normalize = NormalizeWorkspace
	o := opts(false)
	o.Owner = self()
	res := Move(s, o)
	if res.Outcome != OutcomeRenamed || res.Err != nil || res.OwnershipErr != nil {
		t.Fatalf("got %s err=%v ownership=%v", res.Outcome, res.Err, res.OwnershipErr)
	}
	ws := filepath.Join(base, "projects/u1/workspace")
	want := []string{ws, filepath.Join(ws, "a.txt"), filepath.Join(ws, "d"), filepath.Join(ws, "d/b.txt"), filepath.Join(ws, "l")}
	sort.Strings(want)
	if got := chowned(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("chowned:\n%v\nwant\n%v", got, want)
	}
	if m := mode(t, filepath.Join(ws, "a.txt")); m.Perm() != 0o644 {
		t.Fatalf("workspace file mode widened: %v", m)
	}
	if m := mode(t, filepath.Join(ws, "d")); m.Perm() != 0o755 || m&os.ModeSetgid != 0 {
		t.Fatalf("workspace dir mode changed: %v", m)
	}
}

// Shared dir: copied entries get the leaf contract (dirs setgid 2775,
// files group rw) and are chowned; the source and its .migrated copy and
// pre-existing target entries are never chowned.
func TestNormalize_SharedDirCopyAppliesLeafContract(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	writeTree(t, src, map[string]string{"cfg/shared-dirs/s/a.txt": "A", "cfg/shared-dirs/s/d/b.txt": "B"})
	writeTree(t, dst, map[string]string{"projects/u1/shared-dirs/s/agent.txt": "agent's"})
	chowned := recordChowns(t, nil)
	s := Spec{Step: StepSharedDirs, Source: Tree{Base: src, Rel: "cfg/shared-dirs/s"}, Target: Tree{Base: dst, Rel: "projects/u1/shared-dirs/s"}, NoRename: true, Normalize: NormalizeSharedDir}
	o := opts(false)
	o.Owner = self()
	res := Move(s, o)
	if res.Outcome != OutcomeMerged || res.Err != nil || res.OwnershipErr != nil {
		t.Fatalf("got %s err=%v ownership=%v", res.Outcome, res.Err, res.OwnershipErr)
	}
	leaf := filepath.Join(dst, "projects/u1/shared-dirs/s")
	want := []string{filepath.Join(leaf, "a.txt"), filepath.Join(leaf, "d"), filepath.Join(leaf, "d/b.txt")}
	var got []string
	for _, p := range chowned() {
		// Temp files are chowned before placement (and then placed).
		if !strings.HasPrefix(filepath.Base(p), CopyTempPrefix) {
			got = append(got, p)
		}
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("chowned:\n%v\nwant\n%v", got, want)
	}
	if m := mode(t, filepath.Join(leaf, "d")); m.Perm() != 0o775 || m&os.ModeSetgid == 0 {
		t.Fatalf("shared-dir dir mode = %v, want setgid 0775", m)
	}
	if m := mode(t, filepath.Join(leaf, "d/b.txt")); m.Perm() != 0o664 {
		t.Fatalf("shared-dir file mode = %v, want 0664", m)
	}
	if m := mode(t, filepath.Join(leaf, "agent.txt")); m.Perm() != 0o644 {
		t.Fatalf("pre-existing target file touched: %v", m)
	}
}

// Root squash: chown fails with EPERM. The move still completes, the
// failure is reported (not a step failure), and a re-run moves nothing.
func TestNormalize_ChownEPERMIsReportedNotFatal(t *testing.T) {
	base := t.TempDir()
	writeTree(t, base, map[string]string{"old/proj/a.txt": "A"})
	recordChowns(t, unix.EPERM)
	s := spec(base, "old/proj", "projects/u1/workspace")
	s.Normalize = NormalizeWorkspace
	o := opts(false)
	o.Owner = &Ownership{UID: 1000, GID: 1000}
	res := Move(s, o)
	if res.Outcome != OutcomeRenamed || res.Err != nil || res.Failed() {
		t.Fatalf("got %s err=%v failed=%v, want renamed", res.Outcome, res.Err, res.Failed())
	}
	if res.OwnershipErr == nil || !strings.Contains(res.OwnershipErr.Error(), "operation not permitted") {
		t.Fatalf("ownership err = %v, want EPERM", res.OwnershipErr)
	}
	again := Move(s, o)
	if again.Moved() || again.Failed() {
		t.Fatalf("re-run: %s err=%v", again.Outcome, again.Err)
	}
}

func TestNormalize_NoOwnerNoChown(t *testing.T) {
	base := t.TempDir()
	writeTree(t, base, map[string]string{"old/proj/a.txt": "A"})
	chowned := recordChowns(t, nil)
	s := spec(base, "old/proj", "projects/u1/workspace")
	s.Normalize = NormalizeWorkspace
	if res := Move(s, opts(false)); res.Outcome != OutcomeRenamed {
		t.Fatalf("got %s err=%v", res.Outcome, res.Err)
	}
	if got := chowned(); len(got) != 0 {
		t.Fatalf("chowned without an owner: %v", got)
	}
}

// A hard-linked file in a renamed tree is not chowned (its inode is shared
// with a name outside the migrated set); the skip is reported.
func TestNormalize_HardLinkedFileNotChowned(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	writeTree(t, outside, map[string]string{"shared.txt": "s"})
	writeTree(t, base, map[string]string{"old/proj/a.txt": "A"})
	if err := os.Link(filepath.Join(outside, "shared.txt"), filepath.Join(base, "old/proj/linked.txt")); err != nil {
		t.Skipf("hard link across temp dirs: %v", err)
	}
	chowned := recordChowns(t, nil)
	s := spec(base, "old/proj", "projects/u1/workspace")
	s.Normalize = NormalizeWorkspace
	o := opts(false)
	o.Owner = self()
	res := Move(s, o)
	if res.Outcome != OutcomeRenamed || res.Err != nil {
		t.Fatalf("got %s err=%v", res.Outcome, res.Err)
	}
	for _, p := range chowned() {
		if strings.HasSuffix(p, "linked.txt") {
			t.Fatal("hard-linked file was chowned")
		}
	}
	if res.OwnershipErr == nil || !strings.Contains(res.OwnershipErr.Error(), "hard-linked") {
		t.Fatalf("ownership err = %v", res.OwnershipErr)
	}
}
