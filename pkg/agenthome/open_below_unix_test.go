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

package agenthome

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// openBelowFixture is a root with a real nested directory a/b holding
// file.txt, and an outside directory holding a marker file.
func openBelowFixture(t *testing.T) (root, outside string) {
	t.Helper()
	base := t.TempDir()
	root = filepath.Join(base, "root")
	outside = filepath.Join(base, "outside")
	for _, d := range []string{filepath.Join(root, "a", "b"), outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "a", "b", "file.txt"), []byte("inside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "file.txt"), []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, outside
}

// OpenBelow returns the directory below root, and a read through it stays
// in that directory.
func TestOpenBelowOpensRealDirectory(t *testing.T) {
	root, _ := openBelowFixture(t)
	for _, rel := range []string{"a/b", filepath.Join("a", "b"), "a/./b"} {
		h, err := OpenBelow(root, rel)
		if err != nil {
			t.Fatalf("OpenBelow(%q): %v", rel, err)
		}
		data, err := h.ReadFile("file.txt", 1<<10)
		_ = h.Close()
		if err != nil || string(data) != "inside" {
			t.Fatalf("ReadFile via %q = %q, %v", rel, data, err)
		}
	}
	for _, rel := range []string{"", "."} {
		h, err := OpenBelow(root, rel)
		if err != nil {
			t.Fatalf("OpenBelow(%q): %v", rel, err)
		}
		data, err := h.ReadFile("a/b/file.txt", 1<<10)
		_ = h.Close()
		if err != nil || string(data) != "inside" {
			t.Fatalf("ReadFile below root via %q = %q, %v", rel, data, err)
		}
	}
}

// A symbolic link at the first or at a nested component of rel is refused.
func TestOpenBelowRefusesLinkedComponent(t *testing.T) {
	for name, linkAt := range map[string]string{"first": "x", "nested": filepath.Join("a", "x")} {
		t.Run(name, func(t *testing.T) {
			root, outside := openBelowFixture(t)
			if err := os.Symlink(outside, filepath.Join(root, linkAt)); err != nil {
				t.Fatal(err)
			}
			h, err := OpenBelow(root, linkAt)
			if err == nil {
				_ = h.Close()
				t.Fatal("OpenBelow followed a linked component")
			}
			if !errors.Is(err, ErrLinkComponent) {
				t.Fatalf("error = %v, want ErrLinkComponent", err)
			}
		})
	}
}

// A component that is a file is refused with ErrNotDirectory; a missing one
// satisfies fs.ErrNotExist; an absolute rel or one with ".." is refused.
func TestOpenBelowRefusesNonDirectoryMissingAndNonLocal(t *testing.T) {
	root, _ := openBelowFixture(t)
	if _, err := OpenBelow(root, "a/b/file.txt"); !errors.Is(err, ErrNotDirectory) {
		t.Errorf("file component: error = %v, want ErrNotDirectory", err)
	}
	if _, err := OpenBelow(root, "a/missing"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing component: error = %v, want fs.ErrNotExist", err)
	}
	for _, rel := range []string{"../outside", "a/../../outside", "/etc"} {
		if h, err := OpenBelow(root, rel); err == nil {
			_ = h.Close()
			t.Errorf("OpenBelow accepted %q", rel)
		}
	}
}
