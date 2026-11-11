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

//go:build unix

package privatefs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// ReplaceFollowing on unix is the ssh-config writer's POSIX sequence: a
// new file gets newMode, an existing file keeps its mode, and a symlinked
// leaf is followed (the target is replaced, the link kept).
func TestReplaceFollowing_UnixKeepsModeAndFollowsLink(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config")
	if err := ReplaceFollowing(p, []byte("one"), 0o600, ".tmp-"); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("new file mode %v", fi.Mode().Perm())
	}
	if err := os.Chmod(p, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceFollowing(p, []byte("two"), 0o600, ".tmp-"); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o640 {
		t.Fatalf("existing file mode not kept: %v", fi.Mode().Perm())
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(p, link); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceFollowing(link, []byte("three"), 0o600, ".tmp-"); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link not kept: %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != "three" {
		t.Fatalf("target content %q", b)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Fatalf("temporary files left: %v", entries)
	}
}

// The one-name rule on unix (the relaxed ssh path): a target with a second
// name is refused and left untouched. Control: an ordinary target with the
// same mode is replaced.
func TestReplaceFollowing_UnixRefusesHardLinkedTarget(t *testing.T) {
	dir := t.TempDir()
	linked := filepath.Join(dir, "linked")
	ordinary := filepath.Join(dir, "ordinary")
	for _, p := range []string{linked, ordinary} {
		if err := os.WriteFile(p, []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Link(linked, filepath.Join(t.TempDir(), "second")); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceFollowing(linked, []byte("new"), 0o600, ".tmp-"); !errors.Is(err, ErrHardLinked) {
		t.Fatalf("hard-linked target: %v, want ErrHardLinked", err)
	}
	if b, _ := os.ReadFile(linked); string(b) != "old" {
		t.Fatalf("the hard-linked file changed: %q", b)
	}
	if err := ReplaceFollowing(ordinary, []byte("new"), 0o600, ".tmp-"); err != nil {
		t.Fatalf("control: %v", err)
	}
}

// FollowLinks is a no-op on unix: the tree opens and MkdirAll follows a
// symlinked directory component, as without it.
func TestTree_FollowLinksUnix(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	tr, err := OpenTree(root, TreeOptions{FollowLinks: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tr.Close() }()
	if err := tr.MkdirAll("link/sub", Private); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(real, "sub")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("sub: %v %v", fi, err)
	}
}

// CheckReplaceTarget (unix): a hard-linked target is refused, an ordinary
// or missing one passes, and nothing is written either way.
func TestCheckReplaceTarget_Unix(t *testing.T) {
	dir := t.TempDir()
	linked := filepath.Join(dir, "linked")
	ordinary := filepath.Join(dir, "ordinary")
	for _, p := range []string{linked, ordinary} {
		if err := os.WriteFile(p, []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Link(linked, filepath.Join(t.TempDir(), "second")); err != nil {
		t.Fatal(err)
	}
	if err := CheckReplaceTarget(linked); !errors.Is(err, ErrHardLinked) {
		t.Fatalf("hard-linked target: %v, want ErrHardLinked", err)
	}
	for _, p := range []string{ordinary, filepath.Join(dir, "missing")} {
		if err := CheckReplaceTarget(p); err != nil {
			t.Fatalf("control %s: %v", p, err)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Fatalf("CheckReplaceTarget wrote something: %v", entries)
	}
}
