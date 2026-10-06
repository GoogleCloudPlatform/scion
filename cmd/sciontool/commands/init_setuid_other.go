//go:build !linux

/*
Copyright 2026 The Scion Authors.
*/

package commands

import (
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

// chownHomeEntryNoFollow chowns one $HOME entry by name, relative to
// homeDirFd: Fstatat(name, AT_SYMLINK_NOFOLLOW) classifies the entry
// without following it, then directSetUIDAtChownAt(name,
// AT_SYMLINK_NOFOLLOW) chowns it the same way — a symlink has its own link
// chowned, never its target. This is a check/use pair by name, rather than
// the single already-open fd init_setuid_linux.go's GOOS=linux
// implementation uses for the same purpose, because fchownat's
// AT_EMPTY_PATH extension (needed to chown an O_PATH fd directly) is a
// Linux-specific kernel feature with no portable equivalent in
// golang.org/x/sys/unix.
//
// Under requirePrivilegeDrop, a regular file with more than one hard link
// is skipped (logged, non-fatal, the same as every other per-entry chown
// failure here) for the same reason, and with the same type-guard
// rationale, as the linux implementation: see chownHomeEntryNoFollow's doc
// comment in init_setuid_linux.go. Unenforced mode keeps the base
// behaviour of chowning every entry unconditionally.
func chownHomeEntryNoFollow(homeDirFd int, homeDir, name string, uid, gid int, requirePrivilegeDrop bool) {
	if requirePrivilegeDrop {
		var st unix.Stat_t
		if serr := unix.Fstatat(homeDirFd, name, &st, unix.AT_SYMLINK_NOFOLLOW); serr != nil {
			log.Debug("Failed to stat %s: %v", filepath.Join(homeDir, name), serr)
			return
		}
		if st.Mode&unix.S_IFMT == unix.S_IFREG && st.Nlink != 1 {
			log.Debug("Skipping chown of %s: hardlinked regular file (link count %d)", filepath.Join(homeDir, name), st.Nlink)
			return
		}
	}

	if homeEntryChownTestHook != nil {
		homeEntryChownTestHook(name)
	}
	if err := directSetUIDAtChownAt(homeDirFd, name, uid, gid, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		log.Debug("Failed to chown %s: %v", filepath.Join(homeDir, name), err)
	}
}
