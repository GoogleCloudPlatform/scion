//go:build unix

/*
Copyright 2026 The Scion Authors.
*/

package agentinfolock

import (
	"errors"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// acquire opens the lock file and takes LOCK_EX, waiting up to opts.Wait.
// ok is false when the update must run unlocked; the reason is logged.
func acquire(lockPath string, opts Options) (unlock func(), ok bool) {
	f, err := openLockFile(lockPath)
	if err != nil {
		var nr *notRegularError
		if errors.As(err, &nr) {
			opts.warnf("agent-info lock file is not a regular file (%s), writing without lock: %s", nr.kind, lockPath)
		} else {
			opts.warnf("agent-info lock file cannot be opened, writing without lock: %s: %v", lockPath, err)
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
			opts.warnf("agent-info lock file cannot be locked, writing without lock: %s: %v", lockPath, err)
			return nil, false
		}
		if !time.Now().Before(deadline) {
			_ = f.Close()
			opts.logf("timed out after %s waiting for lock %s, updating unlocked", wait, lockPath)
			return nil, false
		}
		time.Sleep(poll)
		if poll < maxPoll {
			poll = min(2*poll, maxPoll)
		}
	}
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
// A new lock file gets mode 0666: root (sciontool init), the container user
// and the broker uid must all be able to open it. Its content is never read
// or written. An existing file the caller cannot open read-write is opened
// read-only, which is enough for flock on a local filesystem.
func openLockFile(lockPath string) (*os.File, error) {
	dir, err := os.Open(filepath.Dir(lockPath))
	if err != nil {
		return nil, err
	}
	defer func() { _ = dir.Close() }()
	dirFd := int(dir.Fd())
	leaf := filepath.Base(lockPath)
	const base = unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC

	created := false
	fd, err := unix.Openat(dirFd, leaf, base|unix.O_RDWR|unix.O_CREAT|unix.O_EXCL, 0o666)
	switch {
	case err == nil:
		created = true
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
	if created {
		// The umask narrowed the create mode; widen it on the fd.
		_ = unix.Fchmod(fd, 0o666)
	}
	return f, nil
}
