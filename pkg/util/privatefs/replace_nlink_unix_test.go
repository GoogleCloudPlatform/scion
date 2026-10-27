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

// setLinkCheckHook installs fn as testHookAfterLinkCheckOpen for the test
// and counts its calls.
func setLinkCheckHook(t *testing.T, fn func(name string, call int)) *int {
	t.Helper()
	calls := 0
	testHookAfterLinkCheckOpen = func(name string) {
		calls++
		fn(name, calls)
	}
	t.Cleanup(func() { testHookAfterLinkCheckOpen = nil })
	return &calls
}

// renameOver puts a new file with content over path, as a concurrent
// ReplaceFollowing does.
func renameOver(t *testing.T, path, content string) {
	t.Helper()
	tmp := path + ".new"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		t.Error(err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Error(err)
	}
}

// A target replaced between the open and the link-count read leaves the
// open handle on a file with no name (Nlink == 0). That is not a hard
// link: the path is opened again, and the new file passes the check.
func TestReplaceFollowing_TargetReplacedWhileCheckedIsReopened(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(p, []byte("old"), 0o640); err != nil {
		t.Fatal(err)
	}
	calls := setLinkCheckHook(t, func(name string, call int) {
		if call == 1 {
			renameOver(t, name, "other writer")
		}
	})
	// newMode (0644) differs from both the stale file's mode (0640) and
	// the replacement's (0600).
	if err := ReplaceFollowing(p, []byte("mine"), 0o644, ".tmp-"); err != nil {
		t.Fatalf("ReplaceFollowing after a concurrent replace: %v", err)
	}
	if *calls != 2 {
		t.Fatalf("link check opened the target %d times, want 2 (one retry)", *calls)
	}
	if b, _ := os.ReadFile(p); string(b) != "mine" {
		t.Fatalf("content %q", b)
	}
	// The mode comes from the file that was checked again (the
	// replacement), not from the stale file and not from newMode.
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, want the replacement's 0600", fi.Mode().Perm())
	}
}

// A target removed between the open and the link-count read is opened
// again; it is gone, so it is a new file.
func TestCheckReplaceTarget_TargetRemovedWhileCheckedIsReopened(t *testing.T) {
	p := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(p, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := setLinkCheckHook(t, func(name string, call int) {
		if err := os.Remove(name); err != nil {
			t.Error(err)
		}
	})
	if err := CheckReplaceTarget(p); err != nil {
		t.Fatalf("CheckReplaceTarget after a concurrent remove: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("link check opened the target %d times, want 1 (the retry finds no file)", *calls)
	}
}

// A target that is replaced on every attempt is refused after a bounded
// number of attempts, with ErrReplacedConcurrently and not ErrHardLinked.
func TestCheckReplaceTarget_TargetKeepsChangingIsRefused(t *testing.T) {
	p := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(p, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := setLinkCheckHook(t, func(name string, call int) {
		renameOver(t, name, "other writer")
	})
	err := CheckReplaceTarget(p)
	if !errors.Is(err, ErrReplacedConcurrently) {
		t.Fatalf("got %v, want ErrReplacedConcurrently", err)
	}
	if errors.Is(err, ErrHardLinked) {
		t.Fatalf("a replaced file is not a hard link: %v", err)
	}
	if *calls != replacedAttempts {
		t.Fatalf("link check opened the target %d times, want %d", *calls, replacedAttempts)
	}
	err = ReplaceFollowing(p, []byte("mine"), 0o600, ".tmp-")
	if !errors.Is(err, ErrReplacedConcurrently) {
		t.Fatalf("ReplaceFollowing: got %v, want ErrReplacedConcurrently", err)
	}
	if b, _ := os.ReadFile(p); string(b) != "other writer" {
		t.Fatalf("refused write changed the file: %q", b)
	}
}

// The one-name rule is unchanged: a target with a second name is refused
// with ErrHardLinked at once, without a retry.
func TestCheckReplaceTarget_HardLinkRefusedWithoutRetry(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(p, filepath.Join(dir, "second-name")); err != nil {
		t.Fatal(err)
	}
	calls := setLinkCheckHook(t, func(string, int) {})
	err := CheckReplaceTarget(p)
	if !errors.Is(err, ErrHardLinked) {
		t.Fatalf("got %v, want ErrHardLinked", err)
	}
	if errors.Is(err, ErrReplacedConcurrently) {
		t.Fatalf("a hard link is not a concurrent replace: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("link check opened the target %d times, want 1", *calls)
	}
}
