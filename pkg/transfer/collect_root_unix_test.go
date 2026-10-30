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

package transfer

import (
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
)

// collectRootTree writes a tree with nested directories, an excluded .git
// directory, an extra-excluded file, an in-tree link and a FIFO.
func collectRootTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range map[string]string{
		"a.txt":         "a\n",
		"sub/b.txt":     "b\n",
		"sub/deep/c.sh": "#!/bin/sh\n",
		".git/HEAD":     "ref: x\n",
		"skip.tmp":      "excluded\n",
	} {
		writeReadTestFile(t, dir, rel, content)
	}
	if err := os.Chmod(filepath.Join(dir, "sub/deep/c.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a.txt", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "sub", "fifo"), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	return dir
}

func TestCollectFilesFromRoot_MatchesCollectFiles(t *testing.T) {
	dir := collectRootTree(t)
	want, err := CollectFiles(dir, []string{"*.tmp"})
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	var got []FileInfo
	within(t, func() { got, err = CollectFilesFromRoot(root, dir, []string{"*.tmp"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("CollectFilesFromRoot = %+v\nCollectFiles = %+v", got, want)
	}
	if len(got) != 3 {
		t.Errorf("collected %d files, want 3 (a.txt, sub/b.txt, sub/deep/c.sh)", len(got))
	}
}

// The walk goes through the root: a directory swapped in at the path after
// the root was opened is not walked.
func TestCollectFilesFromRoot_DoesNotReopenThePath(t *testing.T) {
	dir := collectRootTree(t)
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	moved := dir + ".moved"
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(moved) })
	writeReadTestFile(t, dir, "other.txt", "swapped in\n")
	got, err := CollectFilesFromRoot(root, dir, []string{"*.tmp"})
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, fi := range got {
		paths = append(paths, fi.Path)
	}
	if want := []string{"a.txt", "sub/b.txt", "sub/deep/c.sh"}; !reflect.DeepEqual(paths, want) {
		t.Errorf("collected %v, want the original tree %v", paths, want)
	}
}
