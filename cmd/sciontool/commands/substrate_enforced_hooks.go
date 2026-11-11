/*
Copyright 2026 The Scion Authors.
*/

package commands

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

// enforcedHooksDirModeBits are the permission bits fixupEnforcedHooksDirChain
// strips from every pre-existing ancestor of hooks.EnforcedHooksDir: group-
// and other-write. This is the opposite direction from fixupRootfsForScion's
// own "/" handling (which only ever widens 0700 up to 0755) — here a
// pre-existing directory that is too PERMISSIVE (writable by the workload,
// or owned by something other than root) is the failure mode, since
// LifecycleManager's enforced-mode ownership check (pkg/sciontool/hooks.
// DecideExecAsRoot) treats that exact condition as "drop, don't trust".
const enforcedHooksDirModeBits = 0o022

// fixupEnforcedHooksDirChain hardens every EXISTING ancestor of
// hooks.EnforcedHooksDir (typically "/" and "/run" — see that constant's own
// doc comment for why it lives under /run) to be root-owned and free of
// group/other-write bits, and does the same for the hooks dir itself if
// bootstrap has already created it.
//
// This exists because mkdirAllTracked (pkg/sciontool/substrate's bootstrap
// file writer) only ever chmods directories IT creates — a pre-existing
// ancestor (a real mount point, or an image-layer directory) keeps whatever
// mode and owner it already had. If the actor's rootfs or a future image
// ever produces a group/world-writable or non-root-owned "/" or "/run" (the
// same class of pre-snapshot oddity fixupRootfsForScion's own doc comment
// describes for "/" and $HOME), a hook that is otherwise perfectly
// root-owned and protected would still, correctly, be dropped by
// DecideExecAsRoot: this fixup exists only to make that not spuriously
// happen, never to make the drop path more permissive. If a mode bit still
// can't be corrected here (e.g. a read-only rootfs), the result is DROP, not
// trust — DecideExecAsRoot fails closed on whatever it actually observes at
// hook-execution time, regardless of whether this fixup ran or succeeded.
//
// Symlinks: each entry's open uses O_NOFOLLOW, which refuses a symlinked
// FINAL component only (the kernel fails such an O_DIRECTORY|O_NOFOLLOW open
// with ENOTDIR). Every ancestor is fixed up in its OWN iteration, so a
// symlinked ancestor is refused — logged and left untouched — when it is the
// entry being opened. A deeper entry's open still traverses a symlinked
// ancestor by name; that is acceptable because this fixup only ever tightens
// ownership and mode bits, and the authoritative enforcement is
// DecideExecAsRoot at hook-execution time, which fails closed on whatever it
// actually observes.
func fixupEnforcedHooksDirChain() {
	for _, dir := range enforcedHooksDirAncestors() {
		fixupEnforcedHooksDirEntry(dir)
	}
}

// enforcedHooksDirAncestors returns every path from "/" down to and
// including hooks.EnforcedHooksDir itself, e.g. for "/run/scion/hooks":
// ["/", "/run", "/run/scion", "/run/scion/hooks"].
func enforcedHooksDirAncestors() []string {
	clean := filepath.Clean(hooks.EnforcedHooksDir)
	// parentDirs (substrate_rootfs.go) already returns every ancestor from
	// "/" down to (not including) clean itself; append clean to also fix up
	// the hooks dir's own mode/ownership once bootstrap has created it.
	return append(parentDirs(clean), clean)
}

// fixupEnforcedHooksDirEntry corrects one ancestor: root ownership and no
// group/other-write bits. A missing entry is not an error — bootstrap
// creates the hooks dir itself fresh, root-owned, 0755 (see
// pkg/sciontool/substrate's writeBootstrapFile), so there is nothing to fix
// until at least one bootstrap has run. An entry that is itself a symlink or
// a non-directory is refused (logged, left alone), never chmod/chowned
// through; O_NOFOLLOW guards only this final component, not its parents.
//
// The entry is opened ONCE with O_NOFOLLOW|O_DIRECTORY and the fstat and the
// fchown/fchmod all act on that fd, so they operate on the exact inode we
// opened. A by-name os.Lstat followed by a by-name os.Chown/os.Chmod would
// re-resolve the path each time: an entry swapped for a symlink between the
// stat and the chmod would then be followed (os.Lchown cannot help — Chmod
// still follows the path, and Linux has no lchmod). This fixup exists because
// a chain entry may legitimately be non-root-owned or group/other-writable,
// so that race is real and must be closed, not assumed away.
func fixupEnforcedHooksDirEntry(path string) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		switch {
		case errors.Is(err, os.ErrNotExist):
			// Missing entry: nothing to fix until bootstrap has created it.
			return
		case errors.Is(err, syscall.ELOOP):
			log.Error("fixupEnforcedHooksDirChain: %s: symlink loop or too many links; refusing to touch it", path)
			return
		case errors.Is(err, syscall.ENOTDIR):
			log.Error("fixupEnforcedHooksDirChain: %s is a symlink or not a directory; refusing to touch it", path)
			return
		default:
			log.Error("fixupEnforcedHooksDirChain: failed to open %s without following symlinks: %v", path, err)
			return
		}
	}
	defer func() { _ = f.Close() }()

	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		log.Error("fixupEnforcedHooksDirChain: failed to stat %s: %v", path, err)
		return
	}

	if st.Uid != 0 || st.Gid != 0 {
		if err := f.Chown(0, 0); err != nil {
			log.Error("fixupEnforcedHooksDirChain: failed to chown %s to root: %v", path, err)
		} else {
			log.Info("fixupEnforcedHooksDirChain: chowned %s to root:root", path)
		}
	}
	if perm := os.FileMode(st.Mode).Perm(); perm&enforcedHooksDirModeBits != 0 {
		newPerm := perm &^ enforcedHooksDirModeBits
		if err := f.Chmod(newPerm); err != nil {
			log.Error("fixupEnforcedHooksDirChain: failed to chmod %s to %#o: %v", path, newPerm, err)
		} else {
			log.Info("fixupEnforcedHooksDirChain: chmod %s from %#o to %#o (stripped group/other write)", path, perm, newPerm)
		}
	}
}
