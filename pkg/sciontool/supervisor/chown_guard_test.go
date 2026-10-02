/*
Copyright 2026 The Scion Authors.
*/

package supervisor

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/util/fsutil"
)

// The full table of rejected critical-system-path names, the device-boundary
// walk behavior, and the mountinfo-based bind-source check are all tested
// directly against pkg/util/fsutil (TestCheckRoot_*, TestChownTree_*,
// TestCheckMountSourceReader_*), which is what chownRecursive delegates to.
// Tests here only cover this call site's own wiring, and never invoke the
// recursive chown on anything other than a t.TempDir() tree.

// TestChownRecursive_RejectsSymlinkToCriticalPath proves the guard resolves
// symlinks before checking, so a home directory that is itself a symlink
// pointing at a critical system path cannot slip through. The symlink lives
// under t.TempDir(); only its target names a critical path, and resolution
// happens inside CheckRoot before any walk starts.
func TestChownRecursive_RejectsSymlinkToCriticalPath(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "home-link")
	if err := os.Symlink("/etc", link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	err := chownRecursive(link, os.Getuid(), os.Getgid(), true)
	if !errors.Is(err, fsutil.ErrCriticalSystemPath) {
		t.Fatalf("chownRecursive(%q) = %v, want ErrCriticalSystemPath (resolves to /etc)", link, err)
	}
}

// TestChownRecursive_RejectsFilesystemRootLookalike covers a temp dir laid out
// like a filesystem root (etc/passwd, usr/bin, and a proc marker) even
// though its own path carries no critical-path name.
func TestChownRecursive_RejectsFilesystemRootLookalike(t *testing.T) {
	dir := t.TempDir()
	mustMkdirAll(t, filepath.Join(dir, "etc"))
	mustWriteFile(t, filepath.Join(dir, "etc", "passwd"), "root:x:0:0:root:/root:/bin/sh\n")
	mustMkdirAll(t, filepath.Join(dir, "usr", "bin"))
	mustMkdirAll(t, filepath.Join(dir, "proc"))

	before, err := os.Lstat(filepath.Join(dir, "etc", "passwd"))
	if err != nil {
		t.Fatalf("lstat before: %v", err)
	}
	beforeStat := before.Sys().(*syscall.Stat_t)

	err = chownRecursive(dir, os.Getuid()+1, os.Getgid()+1, true)
	if !errors.Is(err, fsutil.ErrFilesystemRootLookalike) {
		t.Fatalf("chownRecursive(%q) = %v, want ErrFilesystemRootLookalike", dir, err)
	}

	after, err := os.Lstat(filepath.Join(dir, "etc", "passwd"))
	if err != nil {
		t.Fatalf("lstat after: %v", err)
	}
	afterStat := after.Sys().(*syscall.Stat_t)
	if afterStat.Uid != beforeStat.Uid || afterStat.Gid != beforeStat.Gid {
		t.Errorf("ownership changed despite refusal: before uid=%d gid=%d, after uid=%d gid=%d",
			beforeStat.Uid, beforeStat.Gid, afterStat.Uid, afterStat.Gid)
	}
}

// TestChownRecursive_ToleratesSingleRealMarker proves the heuristic requires
// more than one marker, so a home directory that happens to contain exactly
// one of fsutil's root-lookalike markers (here, a top-level "boot/"
// directory) is not falsely rejected. The full per-marker table and the
// exactly-at-threshold boundary are covered once, directly, in
// pkg/util/fsutil; this is a thin wiring check that chownRecursive doesn't
// somehow apply a different threshold at this call site.
func TestChownRecursive_ToleratesSingleRealMarker(t *testing.T) {
	dir := t.TempDir()
	mustMkdirAll(t, filepath.Join(dir, "boot"))

	if err := chownRecursive(dir, os.Getuid(), os.Getgid(), true); err != nil {
		t.Fatalf("chownRecursive(%q) = %v, want nil (exactly one real marker: boot)", dir, err)
	}
}

// TestChownRecursive_AllowsOrdinaryHomeDir proves the guard is not
// overbroad: a normal home directory must still be chowned successfully.
func TestChownRecursive_AllowsOrdinaryHomeDir(t *testing.T) {
	dir := t.TempDir()
	mustMkdirAll(t, filepath.Join(dir, ".claude"))
	mustWriteFile(t, filepath.Join(dir, ".bashrc"), "# rc\n")

	if err := chownRecursive(dir, os.Getuid(), os.Getgid(), true); err != nil {
		t.Fatalf("chownRecursive(%q) = %v, want nil", dir, err)
	}
}

// TestChownRecursive_ChecksMountSource proves chownRecursive actually calls
// checkMountSource and respects its refusal, without needing a real mount:
// it stubs the package variable to simulate a critical bind source and
// confirms both that the error propagates and that nothing under root was
// touched (the check runs before the walk).
func TestChownRecursive_ChecksMountSource(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	mustWriteFile(t, path, "hi\n")
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat before: %v", err)
	}
	beforeStat := before.Sys().(*syscall.Stat_t)

	orig := checkMountSource
	checkMountSource = func(root string) error { return fsutil.ErrCriticalMountSource }
	defer func() { checkMountSource = orig }()

	err = chownRecursive(dir, os.Getuid(), os.Getgid(), true)
	if !errors.Is(err, fsutil.ErrCriticalMountSource) {
		t.Fatalf("chownRecursive(%q) = %v, want it to propagate the stubbed ErrCriticalMountSource", dir, err)
	}

	after, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat after: %v", err)
	}
	afterStat := after.Sys().(*syscall.Stat_t)
	if afterStat.Uid != beforeStat.Uid || afterStat.Gid != beforeStat.Gid {
		t.Errorf("ownership changed despite the stubbed mount-source refusal: before uid=%d gid=%d, after uid=%d gid=%d",
			beforeStat.Uid, beforeStat.Gid, afterStat.Uid, afterStat.Gid)
	}
}

// TestChownRecursive_InvalidRootNeverReachesMountSource proves the ordering
// chownRecursive's own comment describes: fsutil.CheckRoot runs before
// checkMountSource, so a root CheckRoot already refuses is never looked up
// in the mount table at all. Stubbing checkMountSource to return nil (the "I
// was never called, nothing to object to" answer) and then asserting the
// call still never happened is what actually locks in the ordering:
// asserting only the returned error's type would pass just as well if the
// two checks ran in the other order and checkMountSource's own (different)
// refusal reached the caller instead.
func TestChownRecursive_InvalidRootNeverReachesMountSource(t *testing.T) {
	var mountSourceCalls int
	orig := checkMountSource
	checkMountSource = func(root string) error {
		mountSourceCalls++
		return nil
	}
	defer func() { checkMountSource = orig }()

	const criticalRoot = "/etc"
	err := chownRecursive(criticalRoot, os.Getuid(), os.Getgid(), true)
	if !errors.Is(err, fsutil.ErrCriticalSystemPath) {
		t.Fatalf("chownRecursive(%q) = %v, want fsutil.ErrCriticalSystemPath from CheckRoot", criticalRoot, err)
	}
	if mountSourceCalls != 0 {
		t.Errorf("expected checkMountSource to never run for a root CheckRoot already refuses, got %d call(s)", mountSourceCalls)
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
