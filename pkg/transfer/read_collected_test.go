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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// collectOne collects dir and returns its root and the entry for rel.
func collectOne(t *testing.T, dir, rel string) (*os.Root, FileInfo) {
	t.Helper()
	files, err := CollectFiles(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	for _, f := range files {
		if f.Path == rel {
			return root, f
		}
	}
	t.Fatalf("%s not collected from %v", rel, files)
	return nil, FileInfo{}
}

func writeReadTestFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestReadCollectedFile_Unchanged pins that an unchanged collected file
// is returned byte for byte.
func TestReadCollectedFile_Unchanged(t *testing.T) {
	dir := t.TempDir()
	writeReadTestFile(t, dir, "sub/a.txt", "hello\n")
	root, fi := collectOne(t, dir, "sub/a.txt")

	data, err := ReadCollectedFile(root, fi)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello\n" {
		t.Errorf("got %q, want %q", data, "hello\n")
	}
}

// TestReadCollectedFile_ChangedAfterCollect pins that a file whose size or
// content changed after collection is refused.
func TestReadCollectedFile_ChangedAfterCollect(t *testing.T) {
	for name, replacement := range map[string]string{
		"same size": "HELLO\n",
		"larger":    "hello, world\n",
		"smaller":   "hi\n",
		"empty":     "",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeReadTestFile(t, dir, "a.txt", "hello\n")
			root, fi := collectOne(t, dir, "a.txt")

			writeReadTestFile(t, dir, "a.txt", replacement)
			data, err := ReadCollectedFile(root, fi)
			if err == nil || !strings.Contains(err.Error(), "changed since it was collected") {
				t.Fatalf("err = %v (data %q), want a changed-since-collected error", err, data)
			}
		})
	}
}

// TestReadCollectedFile_Removed pins that a removed file is an error.
func TestReadCollectedFile_Removed(t *testing.T) {
	dir := t.TempDir()
	writeReadTestFile(t, dir, "a.txt", "hello\n")
	root, fi := collectOne(t, dir, "a.txt")
	if err := os.Remove(filepath.Join(dir, "a.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCollectedFile(root, fi); err == nil {
		t.Fatal("ReadCollectedFile succeeded for a removed file")
	}
}
