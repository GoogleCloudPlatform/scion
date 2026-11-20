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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// TestCheckTree: CheckTree accepts a tree of plain files and directories
// (Python bytecode ignored) and refuses, by its path relative to the
// checked directory, the first symbolic link, special file or hard-linked
// file, never following a link out of the source.
func TestCheckTree(t *testing.T) {
	tests := []struct {
		name       string
		setup      func(t *testing.T, src, outside string)
		wantPath   string
		wantReason string
	}{
		{
			name: "plain tree",
			setup: func(t *testing.T, src, _ string) {
				mustWrite(t, filepath.Join(src, "a", "b", "f.txt"))
				mustWrite(t, filepath.Join(src, "__pycache__", "x.pyc"))
				mustWrite(t, filepath.Join(src, "mod.pyc"))
			},
		},
		{
			name: "link to an outside file",
			setup: func(t *testing.T, src, outside string) {
				mustWrite(t, filepath.Join(src, "a", "ok.txt"))
				mustSymlink(t, filepath.Join(outside, "secret"), filepath.Join(src, "a", "b", "link"))
			},
			wantPath:   "a/b/link",
			wantReason: skipLink,
		},
		{
			name: "link to an outside directory",
			setup: func(t *testing.T, src, outside string) {
				mustSymlink(t, outside, filepath.Join(src, "dir-link"))
			},
			wantPath:   "dir-link",
			wantReason: skipLink,
		},
		{
			name: "dangling link",
			setup: func(t *testing.T, src, outside string) {
				mustSymlink(t, filepath.Join(outside, "missing"), filepath.Join(src, "dangling"))
			},
			wantPath:   "dangling",
			wantReason: skipLink,
		},
		{
			name: "hard-linked file",
			setup: func(t *testing.T, src, outside string) {
				if err := os.MkdirAll(filepath.Join(src, "d"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(filepath.Join(outside, "secret"), filepath.Join(src, "d", "hard")); err != nil {
					t.Fatal(err)
				}
			},
			wantPath:   "d/hard",
			wantReason: skipHardLink,
		},
		{
			name: "fifo",
			setup: func(t *testing.T, src, _ string) {
				if err := os.MkdirAll(src, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := unix.Mkfifo(filepath.Join(src, "fifo"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantPath:   "fifo",
			wantReason: skipSpecial,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			outside := filepath.Join(base, "outside")
			mustWrite(t, filepath.Join(outside, "secret"))
			src := filepath.Join(base, "root", "tpl", "home")
			if err := os.MkdirAll(src, 0o755); err != nil {
				t.Fatal(err)
			}
			tc.setup(t, src, outside)
			h, err := OpenBelow(filepath.Join(base, "root"), "tpl")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = h.Close() }()

			err = CheckTree(h, "home")
			if tc.wantPath == "" {
				if err != nil {
					t.Fatalf("CheckTree() = %v, want nil", err)
				}
				return
			}
			var refused *RefusedEntryError
			if !errors.As(err, &refused) {
				t.Fatalf("CheckTree() = %v, want *RefusedEntryError", err)
			}
			if refused.Path != tc.wantPath || refused.Reason != tc.wantReason {
				t.Errorf("refused = {%q, %q}, want {%q, %q}", refused.Path, refused.Reason, tc.wantPath, tc.wantReason)
			}
			if msg := err.Error(); strings.Contains(msg, base) || !strings.Contains(msg, tc.wantPath) {
				t.Errorf("message %q must name %q and no host path", msg, tc.wantPath)
			}
		})
	}
}

// TestCheckTree_LinkedRoot: a srcRel that is itself a link is refused as a
// link component, never followed.
func TestCheckTree_LinkedRoot(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	mustWrite(t, filepath.Join(outside, "secret"))
	if err := os.MkdirAll(filepath.Join(base, "tpl"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, outside, filepath.Join(base, "tpl", "home"))
	h, err := OpenBelow(base, "tpl")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Close() }()
	if err := CheckTree(h, "home"); !errors.Is(err, ErrLinkComponent) {
		t.Fatalf("CheckTree() = %v, want ErrLinkComponent", err)
	}
}

func mustWrite(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}
