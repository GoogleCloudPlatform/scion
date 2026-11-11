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

// Package wintest holds test helpers for the Windows private-file tests.
//
// Symlink and junction creation FAILS the test when it does not work; it
// never skips. A skipped symlink test reports green while testing
// nothing. TestNoSkipOnSymlinkPaths in
// pkg/util/privatefs enforces that no test calling these helpers skips.
package wintest

import (
	"os"
	"os/exec"
	"runtime"
	"testing"
)

const privilegeHint = "the Windows test job must run with SeCreateSymbolicLinkPrivilege or Developer Mode; symlink and junction tests never skip, so this is a job configuration failure, not a skip"

// MustSymlink creates newname as a symbolic link to oldname, or fails the
// test.
func MustSymlink(t testing.TB, oldname, newname string) {
	t.Helper()
	if err := os.Symlink(oldname, newname); err != nil {
		t.Fatalf("symlink creation failed: %s: %v", privilegeHint, err)
	}
}

// MustJunction creates newname as a directory junction to the directory
// oldname (mklink /J, which needs no privilege), or fails the test. It
// fails on any OS other than Windows, which has no junctions.
func MustJunction(t testing.TB, oldname, newname string) {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Fatalf("junctions exist only on Windows (GOOS=%s)", runtime.GOOS)
	}
	out, err := exec.Command("cmd", "/c", "mklink", "/J", newname, oldname).CombinedOutput()
	if err != nil {
		t.Fatalf("junction creation failed (mklink /J %s %s): %v: %s", newname, oldname, err, out)
	}
	fi, err := os.Lstat(newname)
	if err != nil {
		t.Fatalf("junction %s not created: %v", newname, err)
	}
	if fi.Mode().IsDir() && fi.Mode()&os.ModeIrregular == 0 && fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("junction %s reads as a plain directory (mode %v)", newname, fi.Mode())
	}
}
