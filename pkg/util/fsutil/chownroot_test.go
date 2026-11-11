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

package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCheckRoot_RejectsEmpty(t *testing.T) {
	err := CheckRoot("")
	if !errors.Is(err, ErrEmptyRoot) {
		t.Fatalf("CheckRoot(\"\") = %v, want ErrEmptyRoot", err)
	}
}

// TestCheckRoot_RejectsRelativePaths proves a relative root is rejected
// before it ever reaches the critical-path map lookup (which only ever
// matches absolute, cleaned paths, so a relative root would otherwise
// silently pass).
func TestCheckRoot_RejectsRelativePaths(t *testing.T) {
	cases := []string{".", "..", "usr", "./x", "usr/bin", "../etc"}
	for _, root := range cases {
		t.Run(root, func(t *testing.T) {
			err := CheckRoot(root)
			if !errors.Is(err, ErrRelativeRoot) {
				t.Fatalf("CheckRoot(%q) = %v, want ErrRelativeRoot", root, err)
			}
		})
	}
}

func TestCheckRoot_RejectsCriticalSystemPaths(t *testing.T) {
	cases := []string{
		"/", "/bin", "/boot", "/dev", "/etc", "/home", "/lib", "/lib32",
		"/lib64", "/libx32", "/media", "/mnt", "/nix", "/opt", "/proc",
		"/root", "/run", "/sbin", "/snap", "/srv", "/sys", "/tmp", "/usr",
		"/var",
		// macOS.
		"/Applications", "/Library", "/private", "/System", "/Users", "/Volumes",
		// Trailing slash and non-clean forms must resolve the same way.
		"/usr/", "/usr/../usr", "//usr",
	}
	for _, root := range cases {
		t.Run(root, func(t *testing.T) {
			err := CheckRoot(root)
			if !errors.Is(err, ErrCriticalSystemPath) {
				t.Fatalf("CheckRoot(%q) = %v, want ErrCriticalSystemPath", root, err)
			}
		})
	}
}

// TestIsCriticalSystemPath covers the exported lookup other packages reuse
// instead of keeping their own copy of this list.
func TestIsCriticalSystemPath(t *testing.T) {
	for _, path := range []string{"/etc", "/usr", "/var/", "/usr/../usr"} {
		t.Run(path, func(t *testing.T) {
			if !IsCriticalSystemPath(path) {
				t.Errorf("IsCriticalSystemPath(%q) = false, want true", path)
			}
		})
	}
	if IsCriticalSystemPath("/not-a-critical-path") {
		t.Error("IsCriticalSystemPath(\"/not-a-critical-path\") = true, want false")
	}
}

func TestCheckRoot_RejectsSymlinkToCriticalPath(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "workspace-link")
	if err := os.Symlink("/etc", link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	err := CheckRoot(link)
	if !errors.Is(err, ErrCriticalSystemPath) {
		t.Fatalf("CheckRoot(%q) = %v, want ErrCriticalSystemPath (resolves to /etc)", link, err)
	}
}

func TestCheckRoot_AllowsOrdinaryDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := CheckRoot(dir); err != nil {
		t.Fatalf("CheckRoot(%q) = %v, want nil", dir, err)
	}
}

// TestCheckRoot_RejectsFilesystemRootLookalike covers a directory laid out like a
// filesystem root (etc/passwd, usr/bin, and a proc marker) even though its
// path carries no critical-path name at all.
func TestCheckRoot_RejectsFilesystemRootLookalike(t *testing.T) {
	dir := t.TempDir()
	mustMkdirAll(t, filepath.Join(dir, "etc"))
	mustWriteFile(t, filepath.Join(dir, "etc", "passwd"), "root:x:0:0:root:/root:/bin/sh\n")
	mustMkdirAll(t, filepath.Join(dir, "usr", "bin"))
	mustMkdirAll(t, filepath.Join(dir, "proc")) // stand-in for a procfs mount

	err := CheckRoot(dir)
	if !errors.Is(err, ErrFilesystemRootLookalike) {
		t.Fatalf("CheckRoot(%q) = %v, want ErrFilesystemRootLookalike", dir, err)
	}
}

// TestCheckRoot_ToleratesSingleRealMarker proves the heuristic requires more
// than one marker: a directory containing exactly one of filesystemRootSignals
// must still pass. Tabled over every signal, since each is checked
// independently and any single one of them alone must not trip the
// heuristic.
func TestCheckRoot_ToleratesSingleRealMarker(t *testing.T) {
	for _, signal := range filesystemRootSignals {
		t.Run(signal, func(t *testing.T) {
			dir := t.TempDir()
			mustMkdirAll(t, filepath.Join(dir, signal))
			if err := CheckRoot(dir); err != nil {
				t.Fatalf("CheckRoot(%q) = %v, want nil (exactly one real marker: %s)", dir, err, signal)
			}
		})
	}
}

// TestCheckRoot_RejectsExactlyTwoMarkers pins filesystemRootSignalThreshold's
// value at exactly 2, using a hardcoded marker count rather than deriving it
// from the constant itself (which would make the assertion trivially true
// for whatever the threshold happens to be, and unable to catch it
// changing). Every other lookalike test in this codebase uses 3-4 markers,
// so none of them would notice the threshold being raised to 3 (a directory
// with 3+ real markers still trips a threshold of 3). The threshold being
// lowered to 1 is separately caught by TestCheckRoot_ToleratesSingleRealMarker
// above, since a single real marker would then also trip it.
func TestCheckRoot_RejectsExactlyTwoMarkers(t *testing.T) {
	dir := t.TempDir()
	mustMkdirAll(t, filepath.Join(dir, filesystemRootSignals[0]))
	mustMkdirAll(t, filepath.Join(dir, filesystemRootSignals[1]))
	err := CheckRoot(dir)
	if !errors.Is(err, ErrFilesystemRootLookalike) {
		t.Fatalf("CheckRoot(%q) = %v, want ErrFilesystemRootLookalike (exactly 2 markers)", dir, err)
	}
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func mustWriteFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
