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

//go:build windows

package privatefs

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// replaceHook counts the calls of testHookAfterLinkCheckOpen and keeps the
// first error of the replacing writer.
type replaceHook struct {
	calls int
	err   error
}

// checkReplaced fails the test if the replacing writer failed. The one
// exception is a volume without a POSIX-semantics rename: renameHandle then
// falls back to the classic rename, which refuses to replace a file that
// is open elsewhere. Only that refusal skips the test (with the error);
// any other error is a failure.
func (h *replaceHook) checkReplaced(t *testing.T) {
	t.Helper()
	if h.err == nil {
		return
	}
	var pe *fs.PathError
	if errors.As(h.err, &pe) && pe.Op == "rename" &&
		(errors.Is(h.err, windows.ERROR_ACCESS_DENIED) ||
			errors.Is(h.err, windows.ERROR_NOT_SUPPORTED) ||
			errors.Is(h.err, windows.ERROR_INVALID_PARAMETER)) {
		t.Skipf("no POSIX-semantics rename over an open file on this volume: %v", h.err)
	}
	t.Fatalf("replacing the file during the check: %v", h.err)
}

// replaceDuringCheck installs testHookAfterLinkCheckOpen so that, on the
// calls for which replace returns true, the file is replaced through a
// second tree with WriteFile (a POSIX-semantics rename over the name the
// check holds open). The name then leaves the open handle, and its
// NumberOfLinks reads 0.
func replaceDuringCheck(t *testing.T, dir, rel string, replace func(call int) bool) *replaceHook {
	t.Helper()
	other := treeAt(t, dir)
	t.Cleanup(func() { _ = other.Close() })
	h := &replaceHook{}
	inHook := false
	testHookAfterLinkCheckOpen = func(string) {
		if inHook {
			return
		}
		inHook = true
		defer func() { inHook = false }()
		h.calls++
		if h.err != nil || !replace(h.calls) {
			return
		}
		h.err = other.WriteFile(rel, []byte("other writer"), Private)
	}
	t.Cleanup(func() { testHookAfterLinkCheckOpen = nil })
	return h
}

func writeTarget(t *testing.T, dir, rel string) {
	t.Helper()
	tr := treeAt(t, dir)
	defer func() { _ = tr.Close() }()
	if err := tr.WriteFile(rel, []byte("old"), Private); err != nil {
		t.Fatal(err)
	}
}

func TestCheckReplaceTarget_WindowsTargetReplacedWhileCheckedIsReopened(t *testing.T) {
	// CheckReplaceTarget refuses a parent directory that other users can
	// change before it opens the target, so the directory is made private
	// (replaceDir), as in the other CheckReplaceTarget tests.
	dir := replaceDir(t)
	writeTarget(t, dir, "config")
	h := replaceDuringCheck(t, dir, "config", func(call int) bool { return call == 1 })
	err := CheckReplaceTarget(filepath.Join(dir, "config"))
	h.checkReplaced(t)
	if err != nil {
		t.Fatalf("CheckReplaceTarget after a concurrent replace: %v", err)
	}
	if h.calls != 2 {
		t.Fatalf("link check opened the target %d times, want 2 (one retry)", h.calls)
	}
}

func TestCheckReplaceTarget_WindowsTargetKeepsChangingIsRefused(t *testing.T) {
	// CheckReplaceTarget refuses a parent directory that other users can
	// change before it opens the target, so the directory is made private
	// (replaceDir), as in the other CheckReplaceTarget tests.
	dir := replaceDir(t)
	writeTarget(t, dir, "config")
	h := replaceDuringCheck(t, dir, "config", func(int) bool { return true })
	err := CheckReplaceTarget(filepath.Join(dir, "config"))
	h.checkReplaced(t)
	if !errors.Is(err, ErrReplacedConcurrently) {
		t.Fatalf("got %v, want ErrReplacedConcurrently", err)
	}
	if errors.Is(err, ErrHardLinked) {
		t.Fatalf("a replaced file is not a hard link: %v", err)
	}
	if h.calls != replacedAttempts {
		t.Fatalf("link check opened the target %d times, want %d", h.calls, replacedAttempts)
	}
}

func TestTreeCreate_WindowsTargetReplacedWhileCheckedIsReopened(t *testing.T) {
	dir := t.TempDir()
	writeTarget(t, dir, "data")
	h := replaceDuringCheck(t, dir, "data", func(call int) bool { return call == 1 })
	tr := treeAt(t, dir)
	defer func() { _ = tr.Close() }()
	f, err := tr.Create("data", false, Inherit)
	h.checkReplaced(t)
	if err != nil {
		t.Fatalf("Create after a concurrent replace: %v", err)
	}
	defer func() { _ = f.Close() }()
	if h.calls != 2 {
		t.Fatalf("link check opened the target %d times, want 2 (one retry)", h.calls)
	}
	if b, _ := io.ReadAll(f); string(b) != "other writer" {
		t.Fatalf("Create returned a handle on %q, want the replacement", b)
	}
}

func TestOpenPrivate_WindowsTargetReplacedWhileCheckedIsReopened(t *testing.T) {
	dir := t.TempDir()
	writeTarget(t, dir, "secret")
	h := replaceDuringCheck(t, dir, "secret", func(call int) bool { return call == 1 })
	tr := treeAt(t, dir)
	defer func() { _ = tr.Close() }()
	f, err := tr.OpenPrivate("secret", os.O_RDWR)
	h.checkReplaced(t)
	if err != nil {
		t.Fatalf("OpenPrivate after a concurrent replace: %v", err)
	}
	defer func() { _ = f.Close() }()
	if h.calls != 2 {
		t.Fatalf("link check opened the target %d times, want 2 (one retry)", h.calls)
	}
	if b, _ := io.ReadAll(f); string(b) != "other writer" {
		t.Fatalf("OpenPrivate returned a handle on %q, want the replacement", b)
	}
}

func TestTreeCreatePrivate_WindowsTargetReplacedWhileCheckedIsReopened(t *testing.T) {
	dir := t.TempDir()
	writeTarget(t, dir, "secret")
	h := replaceDuringCheck(t, dir, "secret", func(call int) bool { return call == 1 })
	tr := treeAt(t, dir)
	defer func() { _ = tr.Close() }()
	f, err := tr.Create("secret", false, Private)
	h.checkReplaced(t)
	if err != nil {
		t.Fatalf("Create(Private) after a concurrent replace: %v", err)
	}
	defer func() { _ = f.Close() }()
	if h.calls != 2 {
		t.Fatalf("link check opened the target %d times, want 2 (one retry)", h.calls)
	}
	if b, _ := io.ReadAll(f); string(b) != "other writer" {
		t.Fatalf("Create(Private) returned a handle on %q, want the replacement", b)
	}
}
