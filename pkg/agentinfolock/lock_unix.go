//go:build unix

/*
Copyright 2026 The Scion Authors.
*/

package agentinfolock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/dirfd"
	"golang.org/x/sys/unix"
)

// acquire opens the lock file and takes LOCK_EX, waiting up to opts.Wait.
// ok is false when the update must run unlocked; the reason is logged.
func acquire(lockPath string, opts Options) (unlock func(), ok bool) {
	f, err := openLockFile(lockPath, opts.NoFollowParent)
	if err != nil {
		var nr *notRegularError
		if errors.As(err, &nr) {
			opts.warnUnusable(problemNotRegular, nr.kind, lockPath, nil)
		} else {
			opts.warnUnusable(problemCannotOpen, "", lockPath, err)
		}
		return nil, false
	}
	wait := opts.wait()
	deadline := time.Now().Add(wait)
	poll := minPoll
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			_ = f.Close()
			opts.warnUnusable(problemCannotLock, "", lockPath, err)
			return nil, false
		}
		if !time.Now().Before(deadline) {
			_ = f.Close()
			opts.warnUnusable(problemTimedOut, "", lockPath, fmt.Errorf("waited %s", wait))
			return nil, false
		}
		time.Sleep(poll)
		if poll < maxPoll {
			poll = min(2*poll, maxPoll)
		}
	}
	clearWarned(lockPath)
	return func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		_ = f.Close()
	}, true
}

// notRegularError reports a lock path that is not a single-link regular
// file. kind is a plain word for the log: fifo, directory, symlink or other
// (a socket, a device, or a regular file with more than one link).
type notRegularError struct {
	kind string
}

func (e *notRegularError) Error() string {
	return "lock file is not a single-link regular file (" + e.kind + ")"
}

// fileKind names the file type in mode (an S_IFMT value) for the log.
func fileKind(mode uint32) string {
	switch mode & unix.S_IFMT {
	case unix.S_IFIFO:
		return "fifo"
	case unix.S_IFDIR:
		return "directory"
	case unix.S_IFLNK:
		return "symlink"
	}
	return "other"
}

// openLockFile opens (creating if needed) the lock file relative to its
// parent directory. The leaf is opened O_NOFOLLOW so a symlink is refused,
// and O_NONBLOCK so a FIFO cannot hang the open. Anything but a single-link
// regular file is refused after an fstat on the open fd.
//
// With noFollowParent the parent is opened through dirfd.OpenParentNoFollow,
// which refuses a symlink in any path component; otherwise it is opened by
// path, following symlinks.
//
// A new lock file is created at the default mode (0666 narrowed by the
// umask, normally 0644); its content is never read or written. An existing
// file the caller cannot open read-write is opened read-only, which is
// enough for flock on a local filesystem. A writer that cannot open it even
// read-only, or cannot flock a read-only fd (NFS emulates flock with POSIX
// locks, which need write access), writes unlocked and logs, as for any
// other unusable lock file.
func openLockFile(lockPath string, noFollowParent bool) (*os.File, error) {
	var dirFd int
	var leaf string
	if noFollowParent {
		fd, l, err := dirfd.OpenParentNoFollow(lockPath)
		if err != nil {
			return nil, err
		}
		dirFd, leaf = fd, l
	} else {
		fd, err := unix.Open(filepath.Dir(lockPath), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, &os.PathError{Op: "open", Path: filepath.Dir(lockPath), Err: err}
		}
		dirFd, leaf = fd, filepath.Base(lockPath)
	}
	defer func() { _ = unix.Close(dirFd) }()
	const base = unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC

	fd, err := unix.Openat(dirFd, leaf, base|unix.O_RDWR|unix.O_CREAT|unix.O_EXCL, 0o666)
	switch {
	case err == nil:
	case errors.Is(err, unix.EEXIST):
		fd, err = unix.Openat(dirFd, leaf, base|unix.O_RDWR, 0)
		if errors.Is(err, unix.EACCES) || errors.Is(err, unix.EROFS) {
			fd, err = unix.Openat(dirFd, leaf, base|unix.O_RDONLY, 0)
		}
	}
	if err != nil {
		// Name the type when the open failed because of what is there
		// (ELOOP for a symlink under O_NOFOLLOW, EISDIR, ENXIO for a
		// socket, ...). The lstat is only for the log.
		var st unix.Stat_t
		if serr := unix.Fstatat(dirFd, leaf, &st, unix.AT_SYMLINK_NOFOLLOW); serr == nil && st.Mode&unix.S_IFMT != unix.S_IFREG {
			return nil, &notRegularError{kind: fileKind(uint32(st.Mode))}
		}
		return nil, err
	}
	f := os.NewFile(uintptr(fd), lockPath)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = f.Close()
		return nil, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 {
		_ = f.Close()
		return nil, &notRegularError{kind: fileKind(uint32(st.Mode))}
	}
	return f, nil
}
