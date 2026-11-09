//go:build unix

/*
Copyright 2026 The Scion Authors.
*/

package hooks

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/dirfd"
)

// writeProvisionStatusInMemOutputs writes data to path, whose parent is
// memOutputsDir, for a non-root writer (see provisionStatusCreatesDir). The
// directory is created when missing (openMemOutputsDir) and checked
// (checkMemOutputsDir) before the leaf is written through the directory's
// own descriptor, so nothing resolved by path after the checks can redirect
// the write.
func writeProvisionStatusInMemOutputs(path string, data []byte) error {
	dir, created, err := openMemOutputsDir()
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	if err := checkMemOutputsDir(dir, created); err != nil {
		return err
	}
	return dirfd.WriteAtNoFollowWithChown(int(dir.Fd()), filepath.Base(path), path, data, 0o600, 0, 0, dirfd.ReplaceLeaf, dirfd.Fchown)
}

// openMemOutputsDir opens memOutputsDir, creating it when it does not
// exist, and reports whether this call created it. Nothing is followed and
// nothing is resolved by path after the root:
//
//   - The in-memory root and every ancestor are opened component by
//     component with O_NOFOLLOW (dirfd.OpenParentNoFollow), so a symbolic
//     link anywhere up to and including the root is refused, and a missing
//     root is an error, never created.
//   - The outputs directory is created with mkdirat(2) relative to that
//     root descriptor, mode 0700. Only this call's own successful mkdirat
//     counts as created; EEXIST (whatever the existing entry is) means it
//     was not, and the entry is never changed here.
//   - It is then opened with O_NOFOLLOW|O_DIRECTORY relative to the same
//     root descriptor. A symbolic link or other non-directory there is
//     ErrProvisionStatusDirUnsafe.
func openMemOutputsDir() (dir *os.File, created bool, err error) {
	outputs := memOutputsDir()
	rootFd, _, err := dirfd.OpenParentNoFollow(outputs)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = dirfd.Close(rootFd) }()

	// An existing name gives EEXIST even when the root is not writable or
	// is read-only (Linux checks existence before write permission), so a
	// directory a root init already prepared is still used.
	switch err := unix.Mkdirat(rootFd, memOutputsName, 0o700); {
	case err == nil:
		created = true
	case errors.Is(err, syscall.EEXIST):
	default:
		return nil, false, fmt.Errorf("mkdir %s: %w", outputs, err)
	}

	// Residual window: between this call's own mkdirat and the open below,
	// any process that can write the in-memory root (an emptyDir, typically
	// writable by every uid in the pod) could replace the new entry. A
	// symbolic link or non-directory put there is refused by the open. A
	// replacement directory must still pass checkMemOutputsDir (owned by the
	// writer, no wider than 0700) before anything is written; one from
	// another uid fails the owner check, so the worst case is a skipped
	// status write.
	const flags = syscall.O_DIRECTORY | syscall.O_NOFOLLOW | syscall.O_RDONLY
	dir, err = dirfd.OpenAt(rootFd, memOutputsName, flags, 0)
	switch {
	case err == nil:
		return dir, created, nil
	case errors.Is(err, syscall.ELOOP), errors.Is(err, syscall.ENOTDIR):
		return nil, false, fmt.Errorf("%w: %s is not a real directory", ErrProvisionStatusDirUnsafe, outputs)
	default:
		return nil, false, fmt.Errorf("open %s: %w", outputs, err)
	}
}

// checkMemOutputsDir requires the open outputs directory to be owned by the
// writer's effective uid with a mode no wider than 0700; otherwise the
// status write is skipped with ErrProvisionStatusDirUnsafe, and the
// directory is left exactly as it is. A directory this call created gets
// mode 0700 set explicitly, whatever the umask removed at creation.
func checkMemOutputsDir(dir *os.File, created bool) error {
	fi, err := dir.Stat()
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("stat %s: no owner information", dir.Name())
	}
	if uid := provisionStatusGeteuid(); int(st.Uid) != uid {
		return fmt.Errorf("%w: %s is owned by uid %d, not the writer's uid %d", ErrProvisionStatusDirUnsafe, memOutputsDir(), st.Uid, uid)
	}
	perm := fi.Mode().Perm()
	if perm&0o077 != 0 {
		return fmt.Errorf("%w: %s has mode %#o, wider than 0700", ErrProvisionStatusDirUnsafe, memOutputsDir(), perm)
	}
	if created && perm != 0o700 {
		if err := dir.Chmod(0o700); err != nil {
			return fmt.Errorf("chmod %s: %w", memOutputsDir(), err)
		}
	}
	return nil
}
