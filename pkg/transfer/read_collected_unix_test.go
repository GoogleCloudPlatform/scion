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

//go:build !windows

package transfer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReadCollectedFile_ReplacedBySymlink pins that a collected regular
// file replaced by a symlink after collection is refused, whether the link
// points outside the collected directory or at identical content inside it.
func TestReadCollectedFile_ReplacedBySymlink(t *testing.T) {
	const content = "hello\n"
	outside := t.TempDir()
	writeReadTestFile(t, outside, "other.txt", content)

	for name, target := range map[string]func(t *testing.T, dir string) string{
		"outside absolute": func(*testing.T, string) string { return filepath.Join(outside, "other.txt") },
		"outside relative": func(t *testing.T, dir string) string {
			rel, err := filepath.Rel(dir, filepath.Join(outside, "other.txt"))
			if err != nil {
				t.Fatal(err)
			}
			return rel
		},
		"inside same content": func(t *testing.T, dir string) string {
			writeReadTestFile(t, dir, "copy.txt", content)
			return "copy.txt"
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeReadTestFile(t, dir, "a.txt", content)
			root, fi := collectOne(t, dir, "a.txt")

			link := filepath.Join(dir, "a.txt")
			if err := os.Remove(link); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target(t, dir), link); err != nil {
				t.Fatal(err)
			}
			// The link resolves, and to a file with the collected content.
			if data, err := os.ReadFile(link); err != nil || string(data) != content {
				t.Fatalf("link does not resolve to the test content: %q, %v", data, err)
			}

			data, err := ReadCollectedFile(root, fi)
			if err == nil || !strings.Contains(err.Error(), "no longer a regular file") {
				t.Fatalf("err = %v (data %q), want a no-longer-regular error", err, data)
			}
		})
	}
}

// TestReadCollectedFile_ParentReplacedBySymlink pins the two remaining
// guards when a parent directory of a collected file is swapped for a
// symlink: a link that leaves the collected directory is refused by
// os.Root, and a link that stays inside it is resolved, so the content is
// then checked against the collected hash.
func TestReadCollectedFile_ParentReplacedBySymlink(t *testing.T) {
	const content = "hello\n"
	cases := map[string]struct {
		setup   func(t *testing.T, dir, outside string) string // returns the link target for sub
		wantErr []string                                       // nil means the read succeeds
	}{
		"outside directory, same content": {
			setup: func(t *testing.T, dir, outside string) string {
				writeReadTestFile(t, outside, "a.txt", content)
				return outside
			},
			// os.Root refuses the link that leaves the root.
			wantErr: []string{"failed to read sub/a.txt", "path escapes from parent"},
		},
		"inside directory, different content": {
			setup: func(t *testing.T, dir, outside string) string {
				writeReadTestFile(t, dir, "other/a.txt", "HELLO\n")
				return "other"
			},
			wantErr: []string{"changed since it was collected"},
		},
		"inside directory, same content": {
			setup: func(t *testing.T, dir, outside string) string {
				writeReadTestFile(t, dir, "other/a.txt", content)
				return "other"
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			outside := t.TempDir()
			dir := t.TempDir()
			writeReadTestFile(t, dir, "sub/a.txt", content)
			root, fi := collectOne(t, dir, "sub/a.txt")

			target := tc.setup(t, dir, outside)
			sub := filepath.Join(dir, "sub")
			if err := os.RemoveAll(sub); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, sub); err != nil {
				t.Fatal(err)
			}
			// The swapped path resolves on the plain filesystem.
			if _, err := os.Stat(filepath.Join(sub, "a.txt")); err != nil {
				t.Fatalf("swapped path does not resolve: %v", err)
			}

			data, err := ReadCollectedFile(root, fi)
			if tc.wantErr == nil {
				if err != nil || string(data) != content {
					t.Fatalf("got %q, %v; want %q", data, err, content)
				}
				return
			}
			if err == nil {
				t.Fatalf("read succeeded with %q, want an error containing %q", data, tc.wantErr)
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("err = %v (data %q), want it to contain %q", err, data, want)
				}
			}
		})
	}
}
