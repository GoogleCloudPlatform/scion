//go:build linux

/*
Copyright 2026 The Scion Authors.
*/

package commands

import (
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

// linuxFDPathTestHook, when non-nil, is invoked once per $HOME entry by
// this file's chownHomeEntryNoFollow, with the entry's name, right
// alongside homeEntryChownTestHook. It exists solely so a linux-only test
// can confirm this file — not init_setuid_other.go's by-name
// implementation — is what a GOOS=linux build actually links: that test
// references this symbol directly, so it fails to compile at all (rather
// than silently passing against the wrong implementation) if the two
// files' build tags were ever accidentally swapped. Always nil in
// production; unexported — this package's own tests are the only thing
// that may set it.
var linuxFDPathTestHook func(name string)

// chownHomeEntryNoFollow chowns one $HOME entry via a single already-open,
// no-follow file descriptor: openat(homeDirFd, name,
// O_PATH|O_NOFOLLOW|O_CLOEXEC) resolves name exactly once, and that same fd
// is then both fstat'd (for the hardlinked-regular-file guard below) and
// chowned (directSetUIDAtChownAt(fd, "", uid, gid, AT_EMPTY_PATH)) —
// closing the check/use window a separate Fstatat(name) + Fchownat(name)
// pair would otherwise leave open for a workload racing a rename or
// symlink-swap of that name in between the two calls. O_PATH|O_NOFOLLOW
// succeeds without following a symlink or requiring its target to exist,
// and AT_EMPTY_PATH on an O_NOFOLLOW-opened descriptor chowns the entry
// itself — a symlink has its own link chowned, never whatever it points
// at — so this preserves the by-name path's (init_setuid_other.go) existing
// symlink behaviour exactly, while removing the TOCTOU window for every
// other entry type.
//
// Under requirePrivilegeDrop, a regular file with more than one hard link
// is skipped (logged, non-fatal, the same as every other per-entry chown
// failure here): hard-linking only needs write access to the directory a
// link is created in, not ownership of the target, so a workload could
// otherwise plant a hardlink to an unrelated (possibly root-owned) file it
// does not itself own and have it handed over by this chown pass. The
// guard is restricted to regular files because a directory legitimately
// has Nlink >= 2 (from its own "." entry and every subdirectory's ".."),
// and a symlink is classified by its own link count, never its target's,
// because the fd this checks was never resolved past it. Unenforced mode
// keeps the base behaviour of chowning every entry unconditionally.
//
// O_PATH and fchownat's AT_EMPTY_PATH are Linux-specific kernel features
// with no portable equivalent, which is why this implementation is built
// only for GOOS=linux; see init_setuid_other.go for every other platform.
func chownHomeEntryNoFollow(homeDirFd int, homeDir, name string, uid, gid int, requirePrivilegeDrop bool) {
	fd, operr := unix.Openat(homeDirFd, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if operr != nil {
		log.Debug("Failed to open %s: %v", filepath.Join(homeDir, name), operr)
		return
	}
	defer func() { _ = unix.Close(fd) }()

	if requirePrivilegeDrop {
		var st unix.Stat_t
		if serr := unix.Fstat(fd, &st); serr != nil {
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
	if linuxFDPathTestHook != nil {
		linuxFDPathTestHook(name)
	}
	if err := directSetUIDAtChownAt(fd, "", uid, gid, unix.AT_EMPTY_PATH); err != nil {
		log.Debug("Failed to chown %s: %v", filepath.Join(homeDir, name), err)
	}
}
