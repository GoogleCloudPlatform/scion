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

package transfer

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"testing"
)

func writeTreeT(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func readT(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

func absentT(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s exists (err=%v), want absent", p, err)
	}
}

func symlinkT(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
}

// plantedBucketEntries are entries an agent could have put in the bucket
// (or an upload from before the shared exclude list left there). None may
// ever be applied, with or without git data.
var plantedBucketEntries = map[string]string{
	".git/config":                          "[core]\n\thooksPath = /tmp/evil\n",
	".git/config.worktree":                 "[core]\n\tfsmonitor = evil\n",
	".git/hooks/pre-commit":                "#!/bin/sh\nevil\n",
	".git/info/attributes":                 "* filter=evil\n",
	".git/modules/sub/config":              "[core]\n\tsshCommand = evil\n",
	".git/modules/sub/hooks/post-checkout": "#!/bin/sh\nevil\n",
	".git/worktrees/wt/gitdir":             "/host/path\n",
	".git/objects/info/alternates":         "/host/other/objects\n",
	".git/objects/info/http-alternates":    "https://evil.example/objects\n",
	"sub/.git/config":                      "[core]\n\thooksPath = /tmp/evil\n",
	"vendor/lib/.git/hooks/pre-commit":     "#!/bin/sh\nevil\n",
	".scion/settings.yaml":                 "evil: true\n",
	"proj/.scion/settings.yaml":            "evil: true\n",
	"proj/.scion/templates/t/x.yaml":       "evil: true\n",
	".DS_Store":                            "x",
	// Case and invisible-character variants of the same entries.
	".GIT/config":                    "[core]\n\thooksPath = /tmp/evil\n",
	".Git/hooks/pre-commit":          "#!/bin/sh\nevil\n",
	"sub/.GIT/config":                "[core]\n",
	".git/objects/INFO/alternates":   "/host/other/objects\n",
	".git/HOOKS/post-checkout":       "#!/bin/sh\nevil\n",
	".SCION/settings.yaml":           "evil: true\n",
	"proj/.Scion/settings.yaml":      "evil: true\n",
	"proj/.\u017fcion/settings.yaml": "evil: true\n",
	".g\u200cit/config":              "[core]\n",
}

func TestApplyStagedWorkspace_ContentSyncSemantics(t *testing.T) {
	staging, dest := t.TempDir(), t.TempDir()
	writeTreeT(t, staging, map[string]string{
		"new.txt":       "new",
		"changed.txt":   "after",
		"same.txt":      "same",
		"dir/deep/a.go": "package a\n",
	})
	if err := os.MkdirAll(filepath.Join(staging, "emptydir"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTreeT(t, dest, map[string]string{
		"changed.txt":          "before",
		"same.txt":             "same",
		"stale.txt":            "gone",
		"dir/stale.go":         "gone",
		".git/config":          "[core]\n\tbare = false\n",
		".git/refs/heads/main": "local\n",
		".scion/project-id":    "local-id\n",
		"proj/.scion/settings": "local\n",
		"sub/.git":             "gitdir: /host/x\n",
	})
	symlinkT(t, "same.txt", filepath.Join(dest, "kept-link"))
	sameBefore, err := os.Stat(filepath.Join(dest, "same.txt"))
	if err != nil {
		t.Fatal(err)
	}

	stats, err := ApplyStagedWorkspace(staging, dest, StagedApplyOptions{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	for rel, want := range map[string]string{"new.txt": "new", "changed.txt": "after", "same.txt": "same", "dir/deep/a.go": "package a\n"} {
		if got := readT(t, filepath.Join(dest, rel)); got != want {
			t.Errorf("%s = %q, want %q", rel, got, want)
		}
	}
	sameAfter, _ := os.Stat(filepath.Join(dest, "same.txt"))
	if !os.SameFile(sameBefore, sameAfter) {
		t.Error("unchanged same.txt was rewritten")
	}
	if fi, err := os.Stat(filepath.Join(dest, "emptydir")); err != nil || !fi.IsDir() {
		t.Errorf("emptydir not created: %v", err)
	}
	absentT(t, filepath.Join(dest, "stale.txt"))
	absentT(t, filepath.Join(dest, "dir/stale.go"))
	// Excluded paths and symlinks on the destination are never removed.
	for rel, want := range map[string]string{
		".git/config":          "[core]\n\tbare = false\n",
		".git/refs/heads/main": "local\n",
		".scion/project-id":    "local-id\n",
		"proj/.scion/settings": "local\n",
		"sub/.git":             "gitdir: /host/x\n",
	} {
		if got := readT(t, filepath.Join(dest, rel)); got != want {
			t.Errorf("%s = %q, want %q (excluded paths must be kept)", rel, got, want)
		}
	}
	if _, err := os.Lstat(filepath.Join(dest, "kept-link")); err != nil {
		t.Errorf("symlink removed: %v", err)
	}
	if stats.Written != 3 || stats.Deleted != 2 {
		t.Errorf("stats = %+v, want Written 3, Deleted 2", stats)
	}
}

func TestApplyStagedWorkspace_RefusesPlantedEntries(t *testing.T) {
	for _, gitData := range []bool{false, true} {
		staging, dest := t.TempDir(), t.TempDir()
		writeTreeT(t, staging, plantedBucketEntries)
		writeTreeT(t, staging, map[string]string{"ok.txt": "ok"})
		writeTreeT(t, dest, map[string]string{".git/config": "[core]\n\tbare = false\n"})

		stats, err := ApplyStagedWorkspace(staging, dest, StagedApplyOptions{GitData: gitData})
		if err != nil {
			t.Fatalf("gitData=%v: apply: %v", gitData, err)
		}
		for rel := range plantedBucketEntries {
			if rel == ".git/config" {
				if got := readT(t, filepath.Join(dest, rel)); got != "[core]\n\tbare = false\n" {
					t.Errorf("gitData=%v: receiver's .git/config replaced: %q", gitData, got)
				}
				continue
			}
			absentT(t, filepath.Join(dest, rel))
		}
		if got := readT(t, filepath.Join(dest, "ok.txt")); got != "ok" {
			t.Errorf("ok.txt = %q", got)
		}
		if stats.Refused == 0 {
			t.Errorf("gitData=%v: Refused = 0, want the planted entries counted", gitData)
		}
	}
}

func TestApplyStagedWorkspace_GitData(t *testing.T) {
	staging, dest := t.TempDir(), t.TempDir()
	writeTreeT(t, staging, map[string]string{
		".git/HEAD":                "ref: refs/heads/main\n",
		".git/packed-refs":         "abc refs/tags/v1\n",
		".git/index":               "DIRC",
		".git/refs/heads/main":     "agent-commit\n",
		".git/objects/ab/cdef":     "object",
		".git/objects/pack/p.pack": "pack",
		".git/config":              "[core]\n\thooksPath = /tmp/evil\n",
		"a.txt":                    "a",
	})
	writeTreeT(t, dest, map[string]string{
		".git/config":           "[core]\n\tbare = false\n",
		".git/hooks/pre-commit": "#!/bin/sh\nmine\n",
		".git/refs/heads/main":  "old\n",
		".git/refs/heads/other": "packed or deleted by the source\n",
		".git/packed-refs":      "stale\n",
		".git/objects/zz/old":   "receiver's object",
	})

	if _, err := ApplyStagedWorkspace(staging, dest, StagedApplyOptions{GitData: true}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	for rel, want := range map[string]string{
		".git/HEAD":                "ref: refs/heads/main\n",
		".git/packed-refs":         "abc refs/tags/v1\n",
		".git/index":               "DIRC",
		".git/refs/heads/main":     "agent-commit\n",
		".git/objects/ab/cdef":     "object",
		".git/objects/pack/p.pack": "pack",
		".git/config":              "[core]\n\tbare = false\n",
		".git/hooks/pre-commit":    "#!/bin/sh\nmine\n",
		".git/objects/zz/old":      "receiver's object", // objects are never deleted
		"a.txt":                    "a",
	} {
		if got := readT(t, filepath.Join(dest, rel)); got != want {
			t.Errorf("%s = %q, want %q", rel, got, want)
		}
	}
	// The ref state is one unit: a ref the source no longer has is removed.
	absentT(t, filepath.Join(dest, ".git/refs/heads/other"))

	// Without GitData the same staging leaves .git alone.
	dest2 := t.TempDir()
	stats, err := ApplyStagedWorkspace(staging, dest2, StagedApplyOptions{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	absentT(t, filepath.Join(dest2, ".git"))
	if stats.Refused == 0 {
		t.Error("staged git data not counted as refused without GitData")
	}
}

func TestApplyStagedWorkspace_GitDataNotIntoGitFileOrLink(t *testing.T) {
	staging := t.TempDir()
	writeTreeT(t, staging, map[string]string{".git/refs/heads/main": "x\n"})

	// A worktree's .git file is left untouched.
	dest := t.TempDir()
	writeTreeT(t, dest, map[string]string{".git": "gitdir: /host/repo/.git/worktrees/a\n"})
	stats, err := ApplyStagedWorkspace(staging, dest, StagedApplyOptions{GitData: true})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !stats.GitDataSkipped {
		t.Error("GitDataSkipped not set for a .git file")
	}
	if got := readT(t, filepath.Join(dest, ".git")); got != "gitdir: /host/repo/.git/worktrees/a\n" {
		t.Errorf(".git file changed: %q", got)
	}

	// A .git symlink is neither followed nor replaced.
	dest, outside := t.TempDir(), t.TempDir()
	symlinkT(t, outside, filepath.Join(dest, ".git"))
	stats, err = ApplyStagedWorkspace(staging, dest, StagedApplyOptions{GitData: true})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !stats.GitDataSkipped {
		t.Error("GitDataSkipped not set for a .git symlink")
	}
	absentT(t, filepath.Join(outside, "refs"))
}

// TestApplyStagedWorkspace_DoesNotWriteThroughLinks covers the destination
// links rclone's local backend writes through: a symlinked directory, a
// symlinked file, and a link inside the workspace aimed at .git/hooks.
func TestApplyStagedWorkspace_DoesNotWriteThroughLinks(t *testing.T) {
	staging, dest, outside := t.TempDir(), t.TempDir(), t.TempDir()
	writeTreeT(t, outside, map[string]string{"secret": "outside"})
	writeTreeT(t, staging, map[string]string{
		"dir/new.txt":  "agent",
		"file.txt":     "agent",
		"d/pre-commit": "#!/bin/sh\nagent\n",
	})
	writeTreeT(t, dest, map[string]string{".git/config": "[core]\n"})
	if err := os.MkdirAll(filepath.Join(dest, ".git/hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	symlinkT(t, outside, filepath.Join(dest, "dir"))
	symlinkT(t, filepath.Join(outside, "secret"), filepath.Join(dest, "file.txt"))
	symlinkT(t, ".git/hooks", filepath.Join(dest, "d"))

	if _, err := ApplyStagedWorkspace(staging, dest, StagedApplyOptions{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	absentT(t, filepath.Join(outside, "new.txt"))
	if got := readT(t, filepath.Join(outside, "secret")); got != "outside" {
		t.Errorf("outside/secret = %q, written through a link", got)
	}
	absentT(t, filepath.Join(dest, ".git/hooks/pre-commit"))
	for _, rel := range []string{"dir", "d"} {
		fi, err := os.Lstat(filepath.Join(dest, rel))
		if err != nil || !fi.IsDir() {
			t.Errorf("%s is not a real directory after apply: %v %v", rel, fi, err)
		}
	}
	fi, err := os.Lstat(filepath.Join(dest, "file.txt"))
	if err != nil || !fi.Mode().IsRegular() {
		t.Errorf("file.txt is not a regular file after apply: %v %v", fi, err)
	}
	if got := readT(t, filepath.Join(dest, "dir/new.txt")); got != "agent" {
		t.Errorf("dir/new.txt = %q", got)
	}
}

func TestApplyStagedWorkspace_TypeConflictFails(t *testing.T) {
	staging, dest := t.TempDir(), t.TempDir()
	writeTreeT(t, staging, map[string]string{"dir/f.txt": "f"})
	writeTreeT(t, dest, map[string]string{"dir": "a file"})
	if _, err := ApplyStagedWorkspace(staging, dest, StagedApplyOptions{}); !errors.Is(err, ErrStagedApplyConflict) {
		t.Fatalf("err = %v, want ErrStagedApplyConflict", err)
	}
}

func TestApplyStagedWorkspace_KeepsExistingMode(t *testing.T) {
	staging, dest := t.TempDir(), t.TempDir()
	writeTreeT(t, staging, map[string]string{"run.sh": "#!/bin/sh\nnew\n", "new.sh": "x"})
	if err := os.WriteFile(filepath.Join(dest, "run.sh"), []byte("#!/bin/sh\nold\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyStagedWorkspace(staging, dest, StagedApplyOptions{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if fi, _ := os.Stat(filepath.Join(dest, "run.sh")); fi.Mode().Perm() != 0o755 {
		t.Errorf("run.sh mode = %v, want 0755 kept", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(filepath.Join(dest, "new.sh")); fi.Mode().Perm() != 0o644 {
		t.Errorf("new.sh mode = %v, want the staged 0644", fi.Mode().Perm())
	}
	entries, _ := os.ReadDir(dest)
	for _, e := range entries {
		if len(e.Name()) > 9 && e.Name()[:10] == ".sync-tmp-" {
			t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
}

// TestApplyStagedWorkspace_LargeFilesStreamed covers files larger than one
// copy chunk: a new one is written, an identical one is not rewritten, and
// one differing only in its last byte is rewritten.
func TestApplyStagedWorkspace_LargeFilesStreamed(t *testing.T) {
	staging, dest := t.TempDir(), t.TempDir()
	big := make([]byte, 3*copyChunk+17)
	for i := range big {
		big[i] = byte(i * 7)
	}
	changed := append([]byte(nil), big...)
	changed[len(changed)-1] ^= 0xff
	for name, data := range map[string][]byte{"new.bin": big, "same.bin": big, "changed.bin": changed} {
		if err := os.WriteFile(filepath.Join(staging, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for name, data := range map[string][]byte{"same.bin": big, "changed.bin": big} {
		if err := os.WriteFile(filepath.Join(dest, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sameBefore, _ := os.Stat(filepath.Join(dest, "same.bin"))

	stats, err := ApplyStagedWorkspace(staging, dest, StagedApplyOptions{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if stats.Written != 2 {
		t.Errorf("Written = %d, want 2 (new.bin, changed.bin)", stats.Written)
	}
	sameAfter, _ := os.Stat(filepath.Join(dest, "same.bin"))
	if !os.SameFile(sameBefore, sameAfter) {
		t.Error("identical large file was rewritten")
	}
	for name, want := range map[string][]byte{"new.bin": big, "changed.bin": changed} {
		got, err := os.ReadFile(filepath.Join(dest, name))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s: content differs (err %v)", name, err)
		}
	}
}

// TestApplyStagedWorkspace_RefStateNeedsRepository checks that ref state
// is removed only when the staging directory carries a repository
// (.git/HEAD): staged objects alone never delete the receiver's refs.
func TestApplyStagedWorkspace_RefStateNeedsRepository(t *testing.T) {
	staging, dest := t.TempDir(), t.TempDir()
	writeTreeT(t, staging, map[string]string{".git/objects/ab/cd": "o"})
	writeTreeT(t, dest, map[string]string{".git/HEAD": "ref: refs/heads/main\n", ".git/refs/heads/main": "x\n"})
	if _, err := ApplyStagedWorkspace(staging, dest, StagedApplyOptions{GitData: true}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := readT(t, filepath.Join(dest, ".git/refs/heads/main")); got != "x\n" {
		t.Errorf("refs/heads/main = %q, want kept", got)
	}
}

// TestApplyStagedWorkspace_RefusesReservedAlias stands in for a
// name-folding filesystem with a hard link: a content path whose target is
// the destination's own .git or .scion file under another name is refused.
func TestApplyStagedWorkspace_RefusesReservedAlias(t *testing.T) {
	for _, reserved := range []string{".git", ".scion"} {
		staging, dest := t.TempDir(), t.TempDir()
		writeTreeT(t, dest, map[string]string{reserved: "pointer\n"})
		if err := os.Link(filepath.Join(dest, reserved), filepath.Join(dest, "alias")); err != nil {
			t.Skipf("hard links unsupported: %v", err)
		}
		writeTreeT(t, staging, map[string]string{"alias": "agent\n"})
		_, err := ApplyStagedWorkspace(staging, dest, StagedApplyOptions{})
		if !errors.Is(err, ErrReservedEntry) {
			t.Fatalf("%s: err = %v, want ErrReservedEntry", reserved, err)
		}
		if got := readT(t, filepath.Join(dest, reserved)); got != "pointer\n" {
			t.Errorf("%s changed: %q", reserved, got)
		}
	}
}

// TestApplyStagedWorkspace_RemovesEmptiedDirectories checks that content
// directories the source no longer has are removed once empty, while a
// directory still holding an excluded entry stays.
func TestApplyStagedWorkspace_RemovesEmptiedDirectories(t *testing.T) {
	staging, dest := t.TempDir(), t.TempDir()
	writeTreeT(t, staging, map[string]string{"keep/a.txt": "a"})
	writeTreeT(t, dest, map[string]string{
		"keep/a.txt":        "a",
		"gone/deep/x.txt":   "x",
		"held/.scion/local": "kept",
	})
	if _, err := ApplyStagedWorkspace(staging, dest, StagedApplyOptions{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	absentT(t, filepath.Join(dest, "gone"))
	if got := readT(t, filepath.Join(dest, "held/.scion/local")); got != "kept" {
		t.Errorf("held/.scion/local = %q", got)
	}
}

func TestApplyStagedWorkspace_ShallowValidated(t *testing.T) {
	good := "0123456789abcdef0123456789abcdef01234567\n" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n"
	for name, tc := range map[string]struct {
		content string
		applied bool
	}{
		"valid":      {good, true},
		"short id":   {"0123\n", false},
		"upper hex":  {"0123456789ABCDEF0123456789abcdef01234567\n", false},
		"no newline": {"0123456789abcdef0123456789abcdef01234567", false},
		"other text": {"[core]\n\thooksPath = /tmp/x\n", false},
	} {
		staging, dest := t.TempDir(), t.TempDir()
		writeTreeT(t, staging, map[string]string{".git/HEAD": "ref: refs/heads/main\n", ".git/shallow": tc.content})
		if _, err := ApplyStagedWorkspace(staging, dest, StagedApplyOptions{GitData: true}); err != nil {
			t.Fatalf("%s: apply: %v", name, err)
		}
		_, err := os.Stat(filepath.Join(dest, ".git/shallow"))
		if (err == nil) != tc.applied {
			t.Errorf("%s: shallow applied = %v, want %v", name, err == nil, tc.applied)
		}
	}
}

// foldingListing stands in for a case-insensitive filesystem's directory
// listing: the destination holds one file under two names (a hard link),
// and the listing shows only the on-disk spelling, hiding the other name.
type foldingListing struct {
	fs.FS
	hidden map[string]bool
}

func (f foldingListing) ReadDir(name string) ([]fs.DirEntry, error) {
	entries, err := fs.ReadDir(f.FS, name)
	if err != nil {
		return nil, err
	}
	out := entries[:0]
	for _, e := range entries {
		if !f.hidden[path.Join(name, e.Name())] {
			out = append(out, e)
		}
	}
	return out, nil
}

// TestApplyStagedWorkspace_CaseOnlyRenameKeepsFile: on a case-insensitive
// receiver, a source that renamed README.md to Readme.md (same content)
// must not lose the file to the deletion pass, in content or in the ref
// state. A hard link gives the destination the second name; the listing
// hides it, as the folding filesystem lists only the on-disk spelling.
func TestApplyStagedWorkspace_CaseOnlyRenameKeepsFile(t *testing.T) {
	staging, dest := t.TempDir(), t.TempDir()
	writeTreeT(t, staging, map[string]string{
		"Readme.md":            "same\n",
		".git/HEAD":            "ref: refs/heads/main\n",
		".git/refs/heads/Main": "abc\n",
	})
	writeTreeT(t, dest, map[string]string{
		"README.md":            "same\n",
		".git/refs/heads/main": "abc\n",
	})
	if err := os.Link(filepath.Join(dest, "README.md"), filepath.Join(dest, "Readme.md")); err != nil {
		t.Skipf("hard links unsupported: %v", err)
	}
	if err := os.Link(filepath.Join(dest, ".git/refs/heads/main"), filepath.Join(dest, ".git/refs/heads/Main")); err != nil {
		t.Fatal(err)
	}
	listing := func(r *os.Root) fs.FS {
		return foldingListing{FS: r.FS(), hidden: map[string]bool{"Readme.md": true, ".git/refs/heads/Main": true}}
	}
	if _, err := applyStagedWorkspace(staging, dest, StagedApplyOptions{GitData: true}, listing); err != nil {
		t.Fatalf("apply: %v", err)
	}
	for _, rel := range []string{"README.md", ".git/refs/heads/main"} {
		if _, err := os.Stat(filepath.Join(dest, rel)); err != nil {
			t.Errorf("%s removed: %v", rel, err)
		}
	}
}

// TestApplyStagedWorkspace_DeletedHardLinkRemoved checks the other side of
// the alias rule: a genuine hard link the source no longer has (listed under
// its own name) is removed like any extraneous file, in the same directory
// or another one.
func TestApplyStagedWorkspace_DeletedHardLinkRemoved(t *testing.T) {
	staging, dest := t.TempDir(), t.TempDir()
	writeTreeT(t, staging, map[string]string{"a.txt": "shared\n"})
	writeTreeT(t, dest, map[string]string{"a.txt": "shared\n"})
	if err := os.MkdirAll(filepath.Join(dest, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(dest, "a.txt"), filepath.Join(dest, "b.txt")); err != nil {
		t.Skipf("hard links unsupported: %v", err)
	}
	if err := os.Link(filepath.Join(dest, "a.txt"), filepath.Join(dest, "sub/c.txt")); err != nil {
		t.Fatal(err)
	}
	stats, err := ApplyStagedWorkspace(staging, dest, StagedApplyOptions{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := readT(t, filepath.Join(dest, "a.txt")); got != "shared\n" {
		t.Errorf("a.txt = %q", got)
	}
	absentT(t, filepath.Join(dest, "b.txt"))
	absentT(t, filepath.Join(dest, "sub/c.txt"))
	if stats.Deleted != 2 {
		t.Errorf("Deleted = %d, want 2", stats.Deleted)
	}
}

// TestApplyStagedWorkspace_LockFilesNeverTouched checks that lock files are
// neither applied from staging nor removed from the destination.
func TestApplyStagedWorkspace_LockFilesNeverTouched(t *testing.T) {
	staging, dest := t.TempDir(), t.TempDir()
	writeTreeT(t, staging, map[string]string{
		".git/HEAD":                 "ref: refs/heads/main\n",
		".git/refs/heads/main":      "abc\n",
		".git/refs/heads/main.lock": "agent's lock\n",
		".git/index.lock":           "agent's lock\n",
	})
	writeTreeT(t, dest, map[string]string{".git/refs/heads/other.lock": "receiver's live lock\n"})
	if _, err := ApplyStagedWorkspace(staging, dest, StagedApplyOptions{GitData: true}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	absentT(t, filepath.Join(dest, ".git/refs/heads/main.lock"))
	absentT(t, filepath.Join(dest, ".git/index.lock"))
	if got := readT(t, filepath.Join(dest, ".git/refs/heads/other.lock")); got != "receiver's live lock\n" {
		t.Errorf("receiver's lock = %q", got)
	}
}
