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
	"slices"
	"strings"
	"testing"
)

func TestNormalizeDir_TemporaryFileCollision(t *testing.T) {
	for _, tc := range []struct {
		name     string
		kind     string
		filename string
		mode     os.FileMode
	}{
		{name: "regular_file", kind: "file"},
		{name: "directory", kind: "directory"},
		{name: "symlink", kind: "symlink"},
		{name: "dangling_symlink", kind: "dangling_symlink"},
		{name: "hardlink", kind: "hardlink"},
		{name: "no_collision"},
		{name: "executable", mode: 0755},
		{name: "long_filename", filename: strings.Repeat("a", 246) + ".txt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			name := tc.filename
			if name == "" {
				name = "config.yaml"
			}
			path := filepath.Join(dir, name)
			mode := tc.mode
			if mode == 0 {
				mode = 0640
			}
			if err := os.WriteFile(path, []byte("hello\r\nworld\r\n"), mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}

			sibling := path + ".tmp"
			outside := filepath.Join(t.TempDir(), "untouched.txt")
			const kept = "keep this content\n"
			switch tc.kind {
			case "file":
				if err := os.WriteFile(sibling, []byte(kept), 0644); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(sibling, 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(sibling, "keep.txt"), []byte(kept), 0644); err != nil {
					t.Fatal(err)
				}
			case "symlink", "dangling_symlink", "hardlink":
				if tc.kind != "dangling_symlink" {
					if err := os.WriteFile(outside, []byte(kept), 0644); err != nil {
						t.Fatal(err)
					}
				}
				link := os.Symlink
				if tc.kind == "hardlink" {
					link = os.Link
				}
				if err := link(outside, sibling); err != nil {
					t.Fatal(err)
				}
			}

			if err := NormalizeDir(dir); err != nil {
				t.Errorf("NormalizeDir: %v", err)
			}
			checkContent := func(path, want string) {
				t.Helper()
				got, err := os.ReadFile(path)
				if err != nil {
					t.Errorf("read %s: %v", filepath.Base(path), err)
				} else if string(got) != want {
					t.Errorf("%s = %q, want %q", filepath.Base(path), got, want)
				}
			}
			checkContent(path, "hello\nworld\n")
			if info, err := os.Lstat(path); err != nil {
				t.Error(err)
			} else if !info.Mode().IsRegular() || info.Mode().Perm() != mode {
				t.Errorf("normalized file mode = %v, want regular file with %v", info.Mode(), mode)
			}
			switch tc.kind {
			case "file", "hardlink":
				checkContent(sibling, kept)
			case "directory":
				checkContent(filepath.Join(sibling, "keep.txt"), kept)
			case "symlink", "dangling_symlink":
				if target, err := os.Readlink(sibling); err != nil || target != outside {
					t.Errorf("sibling symlink = %q, %v; want %q", target, err, outside)
				}
			}
			switch tc.kind {
			case "symlink", "hardlink":
				checkContent(outside, kept)
			case "dangling_symlink":
				if _, err := os.Lstat(outside); !os.IsNotExist(err) {
					t.Errorf("dangling symlink target was created: %v", err)
				}
			}

			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			var gotNames []string
			for _, entry := range entries {
				gotNames = append(gotNames, entry.Name())
			}
			wantNames := []string{name}
			if tc.kind != "" {
				wantNames = append(wantNames, name+".tmp")
			}
			if !slices.Equal(gotNames, wantNames) {
				t.Errorf("directory entries = %v, want %v", gotNames, wantNames)
			}
		})
	}
}
