//go:build linux

/*
Copyright 2026 The Scion Authors.
*/

package commands

import (
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// TestChownHomeEntryNoFollow_Linux_FDPathUsesAtEmptyPath confirms two things
// a build-tag mistake could otherwise hide silently: that a GOOS=linux build
// actually links this file's chownHomeEntryNoFollow (via linuxFDPathTestHook,
// a symbol that exists only in init_setuid_linux.go — see that symbol's own
// doc comment) rather than init_setuid_other.go's by-name implementation,
// and that the real per-entry chown it issues is the single-fd
// AT_EMPTY_PATH call on the already-opened entry (so no path is
// re-resolved between open and chown), never a by-name
// AT_SYMLINK_NOFOLLOW one. It also serves as the required proof that a
// symlink entry's preserved behaviour (the link itself is chowned, its
// target is never followed or chowned) holds under the fd-based chown.
func TestChownHomeEntryNoFollow_Linux_FDPathUsesAtEmptyPath(t *testing.T) {
	dir := t.TempDir()
	groupPath := filepath.Join(dir, "group")
	passwdPath := filepath.Join(dir, "passwd")
	if err := os.WriteFile(groupPath, []byte("scion:x:2000:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(passwdPath, []byte("scion:x:2000:2000:Scion:/home/scion:/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	homeDir := filepath.Join(dir, "home")
	if err := os.MkdirAll(homeDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// A symlink entry, whose target must never be followed or chowned.
	symlinkVictim := t.TempDir()
	if err := os.Chmod(symlinkVictim, 0o750); err != nil {
		t.Fatal(err)
	}
	wantSymlinkVictim, err := os.Lstat(symlinkVictim)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(symlinkVictim, filepath.Join(homeDir, "link")); err != nil {
		t.Fatal(err)
	}

	// A normal entry, chowned as usual (positive control alongside the
	// symlink).
	if err := os.WriteFile(filepath.Join(homeDir, "normal"), []byte("skel"), 0o644); err != nil {
		t.Fatal(err)
	}

	orig := directSetUIDAtChownAt
	type recordedCall struct {
		name  string
		flags int
	}
	var calls []recordedCall
	directSetUIDAtChownAt = func(dirFd int, name string, uid, gid, flags int) error {
		calls = append(calls, recordedCall{name: name, flags: flags})
		return orig(dirFd, name, uid, gid, flags)
	}
	t.Cleanup(func() { directSetUIDAtChownAt = orig })

	var fdPathNames []string
	origHook := linuxFDPathTestHook
	linuxFDPathTestHook = func(name string) {
		fdPathNames = append(fdPathNames, name)
	}
	t.Cleanup(func() { linuxFDPathTestHook = origHook })

	self := strconv.Itoa(os.Getuid())
	selfGID := strconv.Itoa(os.Getgid())
	if err := directSetUIDAt("scion", self, selfGID, groupPath, passwdPath, homeDir, true); err != nil {
		t.Fatalf("directSetUIDAt() = %v, want nil", err)
	}

	if len(fdPathNames) == 0 {
		t.Fatal("linuxFDPathTestHook never fired — a GOOS=linux build must link init_setuid_linux.go's fd-based chownHomeEntryNoFollow, not init_setuid_other.go's by-name one")
	}
	wantAttempted := map[string]bool{"link": false, "normal": false}
	for _, n := range fdPathNames {
		if _, ok := wantAttempted[n]; ok {
			wantAttempted[n] = true
		}
	}
	for name, saw := range wantAttempted {
		if !saw {
			t.Errorf("entry %q was never attempted via the fd path", name)
		}
	}

	// Every per-entry call (i.e. every chown call other than the home
	// directory's own "." call) must be the single-fd AT_EMPTY_PATH form:
	// an empty name relative to that entry's own already-open descriptor,
	// never a by-name AT_SYMLINK_NOFOLLOW call relative to the home
	// directory's fd.
	sawEntryCall := false
	for _, c := range calls {
		if c.name == "." {
			continue
		}
		sawEntryCall = true
		if c.name != "" {
			t.Errorf("per-entry chown call carried name %q, want \"\" (AT_EMPTY_PATH relative to the entry's own fd)", c.name)
		}
		if c.flags != unix.AT_EMPTY_PATH {
			t.Errorf("per-entry chown call flags = %#x, want AT_EMPTY_PATH (%#x)", c.flags, unix.AT_EMPTY_PATH)
		}
	}
	if !sawEntryCall {
		t.Fatal("no per-entry chown call was recorded")
	}

	// The symlink's target must never be followed or chowned.
	gotSymlinkVictim, err := os.Lstat(symlinkVictim)
	if err != nil {
		t.Fatal(err)
	}
	if gotSymlinkVictim.Mode() != wantSymlinkVictim.Mode() {
		t.Errorf("symlink target mode changed: got %v, want %v (must never be followed)", gotSymlinkVictim.Mode(), wantSymlinkVictim.Mode())
	}
	if gotSymlinkVictim.Sys().(*syscall.Stat_t).Uid != wantSymlinkVictim.Sys().(*syscall.Stat_t).Uid {
		t.Error("symlink target owner changed; must never be followed or chowned")
	}
}
