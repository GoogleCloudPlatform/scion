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

// OpenNoFollow refuses a symlink at rel (to a file or a directory) with
// ErrReparsePoint. Control: a real file and directory open, and a
// group-readable file opens too (no privacy decision), with CheckPrivate
// on its handle refusing it.
func TestOpenNoFollow_RefusesSymlinkMakesNoPrivacyDecision(t *testing.T) {
	tr, dir := openTestTree(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "f"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "f"), filepath.Join(dir, "flink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "dlink")); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"flink", "dlink"} {
		if f, err := tr.OpenNoFollow(rel); !errors.Is(err, ErrReparsePoint) {
			if f != nil {
				_ = f.Close()
			}
			t.Errorf("OpenNoFollow(%s) = %v, want ErrReparsePoint", rel, err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "d"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "open"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "open"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"d", "open"} {
		f, err := tr.OpenNoFollow(rel)
		if err != nil {
			t.Fatalf("control: OpenNoFollow(%s): %v", rel, err)
		}
		owned, err := OwnedByCurrentUser(f)
		if err != nil || !owned {
			t.Errorf("control: OwnedByCurrentUser(%s) = %v, %v", rel, owned, err)
		}
		if rel == "open" {
			if err := CheckPrivate(f); !errors.Is(err, ErrNotPrivate) {
				t.Errorf("CheckPrivate on the handle of a 0644 file = %v, want ErrNotPrivate", err)
			}
		}
		_ = f.Close()
	}
}
