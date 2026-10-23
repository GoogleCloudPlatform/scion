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
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fixedNow is the clock of every test run: .migrated-20261023T010203Z.
func fixedNow() time.Time { return time.Date(2026, 10, 23, 1, 2, 3, 0, time.UTC) }

const backupSuffix = MigratedSuffix + "20261023T010203Z"

func opts(dry bool) Options { return Options{DryRun: dry, Now: fixedNow} }

// writeTree creates files (path -> content) below root; a value starting
// with "->" makes a symlink to the rest, a trailing "/" in a key a dir.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, content := range files {
		full := filepath.Join(root, p)
		if strings.HasSuffix(p, "/") {
			if err := os.MkdirAll(full, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(content, "->") {
			if err := os.Symlink(strings.TrimPrefix(content, "->"), full); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// snapshot maps every path below root (not following symlinks) to a
// description of its type and content.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) && p == root {
				return filepath.SkipDir
			}
			return err
		}
		rel, _ := filepath.Rel(root, p)
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, _ := os.Readlink(p)
			out[rel] = "link:" + target
		case info.IsDir():
			out[rel] = "dir:" + info.Mode().Perm().String()
		default:
			b, _ := os.ReadFile(p)
			out[rel] = "file:" + info.Mode().Perm().String() + ":" + string(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func assertAbsent(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Lstat(p); !os.IsNotExist(err) {
		t.Fatalf("%s: want absent, got err=%v", p, err)
	}
}

func spec(base, src, dst string) Spec {
	return Spec{Step: StepHubProjects, Source: Tree{Base: base, Rel: src}, Target: Tree{Base: base, Rel: dst}}
}

func TestMove_RenamesIntoAbsentTarget(t *testing.T) {
	base := t.TempDir()
	writeTree(t, base, map[string]string{"old/proj/a.txt": "A", "old/proj/sub/b.txt": "B"})

	res := Move(spec(base, "old/proj", "projects/u1/workspace"), opts(false))
	if res.Outcome != OutcomeRenamed || res.Err != nil {
		t.Fatalf("got %s err=%v, want renamed", res.Outcome, res.Err)
	}
	assertAbsent(t, filepath.Join(base, "old/proj"))
	if got := readFile(t, filepath.Join(base, "projects/u1/workspace/sub/b.txt")); got != "B" {
		t.Fatalf("b.txt = %q", got)
	}
	// Intermediates the move created get the EnsureLeaf intermediate mode.
	fi, err := os.Stat(filepath.Join(base, "projects/u1"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSetgid == 0 || fi.Mode().Perm() != 0o755 {
		t.Fatalf("intermediate mode = %v, want setgid 0755", fi.Mode())
	}
}

// An existing empty target may be the source of a running agent's mount:
// it is copied into, never replaced by a rename.
func TestMove_CopiesIntoExistingEmptyTarget(t *testing.T) {
	base := t.TempDir()
	writeTree(t, base, map[string]string{"old/proj/a.txt": "A", "projects/u1/workspace/": ""})
	var before syscall.Stat_t
	if err := syscall.Stat(filepath.Join(base, "projects/u1/workspace"), &before); err != nil {
		t.Fatal(err)
	}
	res := Move(spec(base, "old/proj", "projects/u1/workspace"), opts(false))
	if res.Outcome != OutcomeMerged || res.Err != nil || res.Copied != 1 {
		t.Fatalf("got %s err=%v copied=%d, want merged", res.Outcome, res.Err, res.Copied)
	}
	var after syscall.Stat_t
	if err := syscall.Stat(filepath.Join(base, "projects/u1/workspace"), &after); err != nil {
		t.Fatal(err)
	}
	if before.Ino != after.Ino {
		t.Fatal("the existing target directory was replaced")
	}
	if got := readFile(t, filepath.Join(base, "projects/u1/workspace/a.txt")); got != "A" {
		t.Fatalf("a.txt = %q", got)
	}
}

// A rename into an absent target gives the renamed top-level directory the
// leaf contract (setgid 2775) when the target is a Mount leaf.
func TestMove_RenameAppliesLeafContract(t *testing.T) {
	base := t.TempDir()
	writeTree(t, base, map[string]string{"old/proj/a.txt": "A"})
	s := spec(base, "old/proj", "projects/u1/workspace")
	s.TargetIsLeaf = true
	s.PrepareTarget = func() error { t.Fatal("PrepareTarget must not run for a rename"); return nil }
	res := Move(s, opts(false))
	if res.Outcome != OutcomeRenamed || res.Err != nil {
		t.Fatalf("got %s err=%v", res.Outcome, res.Err)
	}
	if m := mode(t, filepath.Join(base, "projects/u1/workspace")); m.Perm() != 0o775 || m&os.ModeSetgid == 0 {
		t.Fatalf("renamed leaf mode = %v, want setgid 0775", m)
	}
	if m := mode(t, filepath.Join(base, "projects/u1/workspace/a.txt")); m.Perm() != 0o644 {
		t.Fatalf("workspace file mode changed: %v", m)
	}
}

// forceRename replaces the renameat2 seam: before is run first (to create
// a target concurrently), then errno is returned, or the real call runs
// when errno is 0.
func forceRename(t *testing.T, before func(), errno syscall.Errno) {
	t.Helper()
	old := renameat2
	renameat2 = func(od int, on string, nd int, nn string, flags uint) error {
		if before != nil {
			before()
		}
		if errno != 0 {
			return errno
		}
		return old(od, on, nd, nn, flags)
	}
	t.Cleanup(func() { renameat2 = old })
}

// A target created concurrently, between the absent check and the rename,
// is never replaced: atomic NOREPLACE fails with EEXIST and the tree is
// copied into the existing directory (its inode survives).
func TestMove_TargetCreatedConcurrentlyIsNotReplaced(t *testing.T) {
	base := t.TempDir()
	writeTree(t, base, map[string]string{"old/proj/a.txt": "A"})
	ws := filepath.Join(base, "projects/u1/workspace")
	var ino uint64
	forceRename(t, func() {
		if ino != 0 {
			return
		}
		if err := os.Mkdir(ws, 0o2775); err != nil {
			t.Fatal(err)
		}
		var st syscall.Stat_t
		if err := syscall.Stat(ws, &st); err != nil {
			t.Fatal(err)
		}
		ino = st.Ino
	}, 0)
	res := Move(spec(base, "old/proj", "projects/u1/workspace"), opts(false))
	if res.Outcome != OutcomeMerged || res.Err != nil {
		t.Fatalf("got %s err=%v, want merged", res.Outcome, res.Err)
	}
	var st syscall.Stat_t
	if err := syscall.Stat(ws, &st); err != nil || st.Ino != ino {
		t.Fatalf("the concurrently created target was replaced (err=%v)", err)
	}
	if readFile(t, filepath.Join(ws, "a.txt")) != "A" {
		t.Fatal("not copied")
	}
}

// NFS: every rename flag is rejected (EINVAL). A directory is then never
// renamed: an absent target is created through PrepareTarget and copied
// into, and the source is kept as .migrated. An empty target created
// concurrently is not replaced.
func TestMove_NoAtomicRenameCopiesDirectories(t *testing.T) {
	base := t.TempDir()
	writeTree(t, base, map[string]string{"old/proj/a.txt": "A", "old/proj/d/b.txt": "B"})
	ws := filepath.Join(base, "projects/u1/workspace")
	forceRename(t, nil, syscall.EINVAL)
	prepared := 0
	s := spec(base, "old/proj", "projects/u1/workspace")
	var ino uint64
	s.PrepareTarget = func() error {
		prepared++
		if err := os.MkdirAll(ws, 0o2775); err != nil {
			return err
		}
		var st syscall.Stat_t
		if err := syscall.Stat(ws, &st); err != nil {
			return err
		}
		ino = st.Ino
		return nil
	}
	res := Move(s, opts(false))
	if res.Outcome != OutcomeMerged || res.Err != nil || res.Copied != 2 || prepared != 1 {
		t.Fatalf("got %s err=%v copied=%d prepared=%d, want merged", res.Outcome, res.Err, res.Copied, prepared)
	}
	var st syscall.Stat_t
	if err := syscall.Stat(ws, &st); err != nil || st.Ino != ino {
		t.Fatalf("the prepared target was replaced (err=%v)", err)
	}
	if readFile(t, filepath.Join(base, "old/proj"+backupSuffix, "a.txt")) != "A" {
		t.Fatal("source not kept as .migrated")
	}
	if readFile(t, filepath.Join(ws, "d/b.txt")) != "B" {
		t.Fatal("not copied")
	}
}

// NFS file placement: linkat, never a plain rename. A target file created
// concurrently (before the link) is not overwritten: a CONFLICT, and the
// source is kept aside.
func TestMove_NoAtomicRenameFileNeverOverwrites(t *testing.T) {
	base := t.TempDir()
	writeTree(t, base, map[string]string{"src/settings.yaml": "legacy seed", "dst/": ""})
	target := filepath.Join(base, "dst/settings.yaml")
	forceRename(t, func() {
		if _, err := os.Lstat(target); os.IsNotExist(err) {
			if err := os.WriteFile(target, []byte("NEW SEED"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}, syscall.EINVAL)
	res := Move(Spec{Step: StepSeed, Source: Tree{Base: base, Rel: "src/settings.yaml"}, Target: Tree{Base: base, Rel: "dst/settings.yaml"}}, opts(false))
	if res.Failed() || len(res.Conflicts) != 1 {
		t.Fatalf("got %s err=%v conflicts=%v, want one conflict", res.Outcome, res.Err, res.Conflicts)
	}
	if readFile(t, target) != "NEW SEED" {
		t.Fatal("target overwritten")
	}
	if readFile(t, filepath.Join(base, "src/settings.yaml"+backupSuffix)) != "legacy seed" {
		t.Fatal("source not kept aside")
	}
}

// NFS file placement into an absent target links the file (same inode)
// and removes the source name.
func TestMove_NoAtomicRenameFileLinks(t *testing.T) {
	base := t.TempDir()
	writeTree(t, base, map[string]string{"src/settings.yaml": "seed", "dst/": ""})
	var before syscall.Stat_t
	if err := syscall.Stat(filepath.Join(base, "src/settings.yaml"), &before); err != nil {
		t.Fatal(err)
	}
	forceRename(t, nil, syscall.EINVAL)
	res := Move(Spec{Step: StepSeed, Source: Tree{Base: base, Rel: "src/settings.yaml"}, Target: Tree{Base: base, Rel: "dst/settings.yaml"}}, opts(false))
	if res.Outcome != OutcomeRenamed || res.Err != nil {
		t.Fatalf("got %s err=%v", res.Outcome, res.Err)
	}
	var after syscall.Stat_t
	if err := syscall.Stat(filepath.Join(base, "dst/settings.yaml"), &after); err != nil || after.Ino != before.Ino {
		t.Fatalf("not placed by link (err=%v)", err)
	}
	assertAbsent(t, filepath.Join(base, "src/settings.yaml"))
}

// A10: the target is never overwritten; conflicts are reported; missing
// files are copied; the source is kept as <name>.migrated-<ts>.
func TestMove_MergeNeverOverwrites(t *testing.T) {
	base := t.TempDir()
	writeTree(t, base, map[string]string{
		"old/proj/new.txt":                "from-source",
		"old/proj/clash.txt":              "source-version",
		"old/proj/same.txt":               "same",
		"old/proj/dir/inner.txt":          "inner",
		"old/proj/fileisdir/x":            "x",
		"projects/u1/workspace/clash.txt": "TARGET-version",
		"projects/u1/workspace/same.txt":  "same",
		"projects/u1/workspace/fileisdir": "a file in the target",
	})
	before := snapshot(t, filepath.Join(base, "projects/u1/workspace"))

	res := Move(spec(base, "old/proj", "projects/u1/workspace"), opts(false))
	if res.Outcome != OutcomeMerged || res.Err != nil {
		t.Fatalf("got %s err=%v, want merged", res.Outcome, res.Err)
	}
	ws := filepath.Join(base, "projects/u1/workspace")
	// Every file that was in the target is byte-for-byte unchanged.
	after := snapshot(t, ws)
	for p, v := range before {
		if after[p] != v {
			t.Fatalf("target %s changed: %q -> %q", p, v, after[p])
		}
	}
	if got := readFile(t, filepath.Join(ws, "new.txt")); got != "from-source" {
		t.Fatalf("new.txt = %q", got)
	}
	if got := readFile(t, filepath.Join(ws, "dir/inner.txt")); got != "inner" {
		t.Fatalf("dir/inner.txt = %q", got)
	}
	sort.Strings(res.Conflicts)
	if want := []string{"clash.txt", "fileisdir"}; !reflect.DeepEqual(res.Conflicts, want) {
		t.Fatalf("conflicts = %v, want %v", res.Conflicts, want)
	}
	if res.Copied != 2 || res.Identical != 1 {
		t.Fatalf("copied=%d identical=%d, want 2 and 1", res.Copied, res.Identical)
	}
	assertAbsent(t, filepath.Join(base, "old/proj"))
	backup := filepath.Join(base, "old/proj"+backupSuffix)
	if res.Backup != backup {
		t.Fatalf("backup = %q, want %q", res.Backup, backup)
	}
	if got := readFile(t, filepath.Join(backup, "clash.txt")); got != "source-version" {
		t.Fatalf("source's clash.txt not kept in the backup: %q", got)
	}
}

// A10: a second run reports 0 moves and changes nothing.
func TestMove_SecondRunMovesNothing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
	}{
		{"rename", map[string]string{"old/proj/a.txt": "A"}},
		{"merge", map[string]string{"old/proj/a.txt": "A", "projects/u1/workspace/b.txt": "B"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			writeTree(t, base, tc.files)
			first := Move(spec(base, "old/proj", "projects/u1/workspace"), opts(false))
			if !first.Moved() || first.Err != nil {
				t.Fatalf("first run: %s err=%v", first.Outcome, first.Err)
			}
			snap := snapshot(t, base)
			second := Move(spec(base, "old/proj", "projects/u1/workspace"), Options{Now: func() time.Time { return fixedNow().Add(time.Hour) }})
			if second.Moved() || second.Outcome != OutcomeNothing || second.Err != nil {
				t.Fatalf("second run: %s err=%v, want nothing", second.Outcome, second.Err)
			}
			if got := snapshot(t, base); !reflect.DeepEqual(got, snap) {
				t.Fatalf("second run changed the tree:\nbefore %v\nafter  %v", snap, got)
			}
		})
	}
}

func TestMove_DryRunChangesNothing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  Outcome
		copy  int
	}{
		{"rename-absent", map[string]string{"old/proj/a.txt": "A"}, OutcomeWouldRename, 0},
		{"existing-empty", map[string]string{"old/proj/a.txt": "A", "projects/u1/workspace/": ""}, OutcomeWouldMerge, 1},
		{"merge", map[string]string{"old/proj/a.txt": "A", "old/proj/d/c.txt": "C", "old/proj/b.txt": "x", "projects/u1/workspace/b.txt": "B"}, OutcomeWouldMerge, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			writeTree(t, base, tc.files)
			snap := snapshot(t, base)
			prepared := false
			s := spec(base, "old/proj", "projects/u1/workspace")
			s.PrepareTarget = func() error { prepared = true; return nil }
			res := Move(s, opts(true))
			if res.Outcome != tc.want || res.Err != nil {
				t.Fatalf("got %s err=%v, want %s", res.Outcome, res.Err, tc.want)
			}
			if res.Copied != tc.copy {
				t.Fatalf("copied = %d, want %d", res.Copied, tc.copy)
			}
			if prepared {
				t.Fatal("dry run called PrepareTarget")
			}
			if got := snapshot(t, base); !reflect.DeepEqual(got, snap) {
				t.Fatalf("dry run changed the tree:\nbefore %v\nafter  %v", snap, got)
			}
		})
	}
}

// Symlinks inside the source are never followed or copied: a link to
// content outside the tree must not bring that content into the target.
func TestMove_SymlinksInSourceAreRefused(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	writeTree(t, outside, map[string]string{"secret.txt": "SECRET", "secretdir/s.txt": "SECRET"})
	writeTree(t, base, map[string]string{
		"old/proj/ok.txt":                    "ok",
		"old/proj/link.txt":                  "->" + filepath.Join(outside, "secret.txt"),
		"old/proj/sub/linkdir":               "->" + filepath.Join(outside, "secretdir"),
		"projects/u1/workspace/existing.txt": "keep",
	})
	res := Move(spec(base, "old/proj", "projects/u1/workspace"), opts(false))
	if res.Outcome != OutcomeMerged || res.Err != nil {
		t.Fatalf("got %s err=%v, want merged", res.Outcome, res.Err)
	}
	sort.Strings(res.Refused)
	if want := []string{"link.txt (symlink)", "sub/linkdir (symlink)"}; !reflect.DeepEqual(res.Refused, want) {
		t.Fatalf("refused = %v, want %v", res.Refused, want)
	}
	ws := filepath.Join(base, "projects/u1/workspace")
	assertAbsent(t, filepath.Join(ws, "link.txt"))
	assertAbsent(t, filepath.Join(ws, "sub/linkdir"))
	for p, v := range snapshot(t, ws) {
		if strings.Contains(v, "SECRET") || strings.HasPrefix(v, "link:") {
			t.Fatalf("target holds %s = %q", p, v)
		}
	}
}

func TestMove_SpecialFilesAreRefused(t *testing.T) {
	base := t.TempDir()
	writeTree(t, base, map[string]string{"old/proj/ok.txt": "ok", "projects/u1/workspace/x": "x"})
	if err := syscall.Mkfifo(filepath.Join(base, "old/proj/fifo"), 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	res := Move(spec(base, "old/proj", "projects/u1/workspace"), opts(false))
	if res.Outcome != OutcomeMerged || res.Err != nil {
		t.Fatalf("got %s err=%v", res.Outcome, res.Err)
	}
	if want := []string{"fifo (special file)"}; !reflect.DeepEqual(res.Refused, want) {
		t.Fatalf("refused = %v, want %v", res.Refused, want)
	}
	assertAbsent(t, filepath.Join(base, "projects/u1/workspace/fifo"))
}

// A symlink at the source itself, at a source path component or at a
// target path component refuses the move; nothing is written through it.
func TestMove_SymlinkedPathsAreRefused(t *testing.T) {
	t.Run("source-is-symlink", func(t *testing.T) {
		base := t.TempDir()
		outside := t.TempDir()
		writeTree(t, outside, map[string]string{"a.txt": "A"})
		writeTree(t, base, map[string]string{"old/proj": "->" + outside})
		res := Move(spec(base, "old/proj", "projects/u1/workspace"), opts(false))
		if res.Outcome != OutcomeFailed || res.Err == nil {
			t.Fatalf("got %s err=%v, want failed", res.Outcome, res.Err)
		}
		assertAbsent(t, filepath.Join(base, "projects"))
		if got := snapshot(t, outside); got["a.txt"] != "file:-rw-r--r--:A" {
			t.Fatalf("outside changed: %v", got)
		}
	})
	t.Run("source-component-is-symlink", func(t *testing.T) {
		base := t.TempDir()
		outside := t.TempDir()
		writeTree(t, outside, map[string]string{"proj/a.txt": "A"})
		writeTree(t, base, map[string]string{"old": "->" + outside})
		res := Move(spec(base, "old/proj", "projects/u1/workspace"), opts(false))
		if res.Outcome != OutcomeFailed || res.Err == nil {
			t.Fatalf("got %s err=%v, want failed", res.Outcome, res.Err)
		}
		assertAbsent(t, filepath.Join(base, "projects"))
	})
	t.Run("target-component-is-symlink", func(t *testing.T) {
		base := t.TempDir()
		outside := t.TempDir()
		writeTree(t, base, map[string]string{"old/proj/a.txt": "A", "projects": "->" + outside})
		res := Move(spec(base, "old/proj", "projects/u1/workspace"), opts(false))
		if res.Outcome != OutcomeFailed || res.Err == nil {
			t.Fatalf("got %s err=%v, want failed", res.Outcome, res.Err)
		}
		if got := snapshot(t, outside); len(got) != 1 {
			t.Fatalf("wrote through the symlink: %v", got)
		}
		if readFile(t, filepath.Join(base, "old/proj/a.txt")) != "A" {
			t.Fatal("source changed")
		}
	})
	t.Run("target-is-symlink", func(t *testing.T) {
		base := t.TempDir()
		outside := t.TempDir()
		writeTree(t, base, map[string]string{"old/proj/a.txt": "A", "projects/u1/workspace": "->" + outside})
		res := Move(spec(base, "old/proj", "projects/u1/workspace"), opts(false))
		if res.Outcome != OutcomeFailed || res.Err == nil {
			t.Fatalf("got %s err=%v, want failed", res.Outcome, res.Err)
		}
		if got := snapshot(t, outside); len(got) != 1 {
			t.Fatalf("wrote through the symlink: %v", got)
		}
	})
	t.Run("target-subdir-is-symlink", func(t *testing.T) {
		base := t.TempDir()
		outside := t.TempDir()
		writeTree(t, base, map[string]string{"old/proj/d/a.txt": "A", "projects/u1/workspace/d": "->" + outside})
		res := Move(spec(base, "old/proj", "projects/u1/workspace"), opts(false))
		if res.Err != nil {
			t.Fatalf("err=%v", res.Err)
		}
		if want := []string{"d"}; !reflect.DeepEqual(res.Conflicts, want) {
			t.Fatalf("conflicts = %v, want %v", res.Conflicts, want)
		}
		if got := snapshot(t, outside); len(got) != 1 {
			t.Fatalf("wrote through the symlink: %v", got)
		}
	})
}

func TestMove_TargetIsAFile(t *testing.T) {
	base := t.TempDir()
	writeTree(t, base, map[string]string{"old/proj/a.txt": "A", "projects/u1/workspace": "file"})
	res := Move(spec(base, "old/proj", "projects/u1/workspace"), opts(false))
	if res.Outcome != OutcomeFailed || res.Err == nil {
		t.Fatalf("got %s err=%v, want failed", res.Outcome, res.Err)
	}
	if readFile(t, filepath.Join(base, "projects/u1/workspace")) != "file" || readFile(t, filepath.Join(base, "old/proj/a.txt")) != "A" {
		t.Fatal("a refused move changed something")
	}
}

func TestMove_NothingToMove(t *testing.T) {
	base := t.TempDir()
	writeTree(t, base, map[string]string{"old/empty/": ""})
	for _, src := range []string{"old/missing", "old/empty", "nope/deeper/missing"} {
		res := Move(spec(base, src, "projects/u1/workspace"), opts(false))
		if res.Outcome != OutcomeNothing || res.Err != nil || res.Moved() {
			t.Fatalf("%s: got %s err=%v, want nothing", src, res.Outcome, res.Err)
		}
	}
	assertAbsent(t, filepath.Join(base, "projects"))
}

func TestMove_NoRenameUsesPrepareTargetAndCopies(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	writeTree(t, src, map[string]string{"cfg/shared-dirs/scratch/a.txt": "A"})
	prepared := 0
	s := Spec{
		Step:     StepSharedDirs,
		Source:   Tree{Base: src, Rel: "cfg/shared-dirs/scratch"},
		Target:   Tree{Base: dst, Rel: "projects/u1/shared-dirs/scratch"},
		NoRename: true,
		PrepareTarget: func() error {
			prepared++
			return os.MkdirAll(filepath.Join(dst, "projects/u1/shared-dirs/scratch"), 0o2775)
		},
	}
	res := Move(s, opts(false))
	if res.Outcome != OutcomeMerged || res.Err != nil {
		t.Fatalf("got %s err=%v, want merged", res.Outcome, res.Err)
	}
	if prepared != 1 {
		t.Fatalf("PrepareTarget called %d times", prepared)
	}
	if readFile(t, filepath.Join(dst, "projects/u1/shared-dirs/scratch/a.txt")) != "A" {
		t.Fatal("not copied")
	}
	if readFile(t, filepath.Join(src, "cfg/shared-dirs/scratch"+backupSuffix, "a.txt")) != "A" {
		t.Fatal("source not kept as .migrated copy")
	}
}

func TestMove_CopyKeepsPermissionsAndMtime(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	writeTree(t, src, map[string]string{"p/run.sh": "#!/bin/sh"})
	p := filepath.Join(src, "p/run.sh")
	if err := os.Chmod(p, 0o750); err != nil {
		t.Fatal(err)
	}
	mtime := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	s := Spec{Step: StepLocalProjects, Source: Tree{Base: src, Rel: "p"}, Target: Tree{Base: dst, Rel: "w"}, NoRename: true}
	res := Move(s, opts(false))
	if res.Outcome != OutcomeMerged || res.Err != nil {
		t.Fatalf("got %s err=%v", res.Outcome, res.Err)
	}
	fi, err := os.Stat(filepath.Join(dst, "w/run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o750 || !fi.ModTime().Equal(mtime) {
		t.Fatalf("mode %v mtime %v, want 0750 and %v", fi.Mode().Perm(), fi.ModTime(), mtime)
	}
}

func TestMove_Entries(t *testing.T) {
	base := t.TempDir()
	writeTree(t, base, map[string]string{
		"old/proj/.scion/settings.yaml":    "hub: x",
		"old/proj/.scion/project-id":       "u1",
		"old/proj/.scion/templates/t.yaml": "repo",
		"projects/u1/.scion/project-id":    "u1",
	})
	s := Spec{Step: StepSeed, Source: Tree{Base: base, Rel: "old/proj/.scion"}, Target: Tree{Base: base, Rel: "projects/u1/.scion"}, Entries: []string{"settings.yaml", "project-id"}}
	res := Move(s, opts(false))
	if res.Failed() || !res.Moved() {
		t.Fatalf("got %s err=%v", res.Outcome, res.Err)
	}
	if readFile(t, filepath.Join(base, "projects/u1/.scion/settings.yaml")) != "hub: x" {
		t.Fatal("settings.yaml not moved")
	}
	assertAbsent(t, filepath.Join(base, "old/proj/.scion/settings.yaml"))
	// project-id was identical in the target: the source copy is kept aside.
	if readFile(t, filepath.Join(base, "old/proj/.scion/project-id"+backupSuffix)) != "u1" {
		t.Fatal("identical project-id not kept as .migrated copy")
	}
	if readFile(t, filepath.Join(base, "old/proj/.scion/templates/t.yaml")) != "repo" {
		t.Fatal("an unlisted entry moved")
	}
	if len(res.Entries) != 2 || res.Entries[1].Identical != 1 {
		t.Fatalf("entries = %+v", res.Entries)
	}
}

func TestMove_FileConflictNeverOverwrites(t *testing.T) {
	base := t.TempDir()
	writeTree(t, base, map[string]string{"src/settings.yaml": "source", "dst/settings.yaml": "TARGET"})
	res := Move(Spec{Step: StepSeed, Source: Tree{Base: base, Rel: "src/settings.yaml"}, Target: Tree{Base: base, Rel: "dst/settings.yaml"}}, opts(false))
	if res.Failed() {
		t.Fatalf("err=%v", res.Err)
	}
	if want := []string{"."}; !reflect.DeepEqual(res.Conflicts, want) {
		t.Fatalf("conflicts = %v", res.Conflicts)
	}
	if readFile(t, filepath.Join(base, "dst/settings.yaml")) != "TARGET" {
		t.Fatal("target overwritten")
	}
	if readFile(t, filepath.Join(base, "src/settings.yaml"+backupSuffix)) != "source" {
		t.Fatal("source not kept")
	}
}

func TestMove_RejectsBadTrees(t *testing.T) {
	base := t.TempDir()
	for _, s := range []Spec{
		{Source: Tree{Base: "rel", Rel: "a"}, Target: Tree{Base: base, Rel: "b"}},
		{Source: Tree{Base: base, Rel: "../a"}, Target: Tree{Base: base, Rel: "b"}},
		{Source: Tree{Base: base, Rel: "a"}, Target: Tree{Base: base, Rel: ""}},
		{Source: Tree{Base: base, Rel: "a"}, Target: Tree{Base: base, Rel: "/abs"}},
		{Source: Tree{Base: base, Rel: "a"}, Target: Tree{Base: base, Rel: "b"}, Entries: []string{"../x"}},
	} {
		if res := Move(s, opts(false)); res.Outcome != OutcomeFailed || res.Err == nil {
			t.Fatalf("%+v: got %s, want failed", s, res.Outcome)
		}
	}
}

func TestMove_BackupNameTaken(t *testing.T) {
	base := t.TempDir()
	writeTree(t, base, map[string]string{"old/proj/a.txt": "A", "old/proj" + backupSuffix + "/x": "x", "projects/u1/workspace/b": "b"})
	res := Move(spec(base, "old/proj", "projects/u1/workspace"), opts(false))
	if res.Outcome != OutcomeFailed || res.Err == nil {
		t.Fatalf("got %s err=%v, want failed", res.Outcome, res.Err)
	}
	if readFile(t, filepath.Join(base, "old/proj/a.txt")) != "A" {
		t.Fatal("source must stay in place when it cannot be renamed aside")
	}
}

func TestMove_CopyOnlyLeavesSourceAndHonoursExclude(t *testing.T) {
	base := t.TempDir()
	writeTree(t, base, map[string]string{"src/a.txt": "A", "src/skip/x": "x", "src/keep/y": "y"})
	snap := snapshot(t, filepath.Join(base, "src"))
	s := Spec{Step: StepLocalProjects, Source: Tree{Base: base, Rel: "src"}, Target: Tree{Base: base, Rel: "dst"}, CopyOnly: true, Exclude: []string{"skip"}}
	dry := Move(s, opts(true))
	if dry.Outcome != OutcomeWouldCopy || dry.Copied != 2 || !dry.Moved() {
		t.Fatalf("dry: %s copied=%d", dry.Outcome, dry.Copied)
	}
	assertAbsent(t, filepath.Join(base, "dst"))
	res := Move(s, opts(false))
	if res.Outcome != OutcomeCopied || res.Copied != 2 || res.Err != nil {
		t.Fatalf("got %s copied=%d err=%v", res.Outcome, res.Copied, res.Err)
	}
	if got := snapshot(t, filepath.Join(base, "src")); !reflect.DeepEqual(got, snap) {
		t.Fatal("copy-only changed the source")
	}
	assertAbsent(t, filepath.Join(base, "dst/skip"))
	if readFile(t, filepath.Join(base, "dst/keep/y")) != "y" {
		t.Fatal("not copied")
	}
	again := Move(s, opts(false))
	if again.Moved() || again.Identical != 2 {
		t.Fatalf("re-run: %s copied=%d identical=%d", again.Outcome, again.Copied, again.Identical)
	}
}

func TestExcludeUnder(t *testing.T) {
	got := excludeUnder([]string{"agents", "agents/x", "a/b/c", "./a/d", "b"}, "a")
	if want := []string{"b/c", "d"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// A done context stops a copy at the next entry: the move fails and the
// source stays in place.
func TestMove_CancelledContextStopsCopy(t *testing.T) {
	base := t.TempDir()
	writeTree(t, base, map[string]string{"src/a.txt": "A", "src/b.txt": "B", "dst/x": "x"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	o := opts(false)
	o.Context = ctx
	res := Move(Spec{Step: StepLocalProjects, Source: Tree{Base: base, Rel: "src"}, Target: Tree{Base: base, Rel: "dst"}}, o)
	if res.Outcome != OutcomeFailed || !errors.Is(res.Err, context.Canceled) {
		t.Fatalf("got %s err=%v, want failed with context.Canceled", res.Outcome, res.Err)
	}
	if readFile(t, filepath.Join(base, "src/a.txt")) != "A" {
		t.Fatal("source not left in place")
	}
}
