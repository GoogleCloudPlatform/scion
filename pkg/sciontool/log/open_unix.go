//go:build unix

/*
Copyright 2025 The Scion Authors.
*/

package log

import (
	"fmt"
	"os"
	"syscall"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/dirfd"
)

// openLogFileNoFollow opens path for appending, the same way the historical
// os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, mode) call did,
// except:
//   - it never follows a symlink at any component of path, not just the
//     final one (see the dirfd package doc comment) — a workload that owns
//     an intermediate directory such as $HOME can otherwise redirect the
//     open into an arbitrary directory even though the leaf itself is
//     never a symlink;
//   - O_NONBLOCK keeps a FIFO planted at path from blocking this open (and
//     therefore every later log line, since write() holds mu while this
//     runs) forever waiting for a reader that will never come;
//   - it refuses to write to anything that isn't a regular file with
//     exactly one link, so a hardlink to a root-owned file (which passes a
//     bare "is this a regular file" check, since a hardlink IS a regular
//     file) is refused instead of silently appended to.
//
// This process runs as root for its whole lifetime on Substrate (see
// fixupRootfsForScion's doc comment in cmd/sciontool/commands), and logPath
// normally lives under $HOME, which the scion user can write to. A symlink,
// hardlink, FIFO, or other non-regular entry here now surfaces as an
// open/stat error instead, which callers already treat the same way a
// missing/unwritable log path has always been treated: fall back to
// /tmp/agent.log, or (if that also fails) drop the line silently rather
// than block startup on logging.
func openLogFileNoFollow(path string, mode os.FileMode) (*os.File, error) {
	dirFd, leaf, err := dirfd.OpenParentNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = dirfd.Close(dirFd) }()

	f, err := dirfd.OpenAt(dirFd, leaf,
		syscall.O_APPEND|syscall.O_CREAT|syscall.O_WRONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, mode)
	if err != nil {
		return nil, err
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		_ = f.Close()
		return nil, err
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFREG || st.Nlink != 1 {
		_ = f.Close()
		return nil, fmt.Errorf("log path %s is not a single-link regular file", path)
	}
	return f, nil
}
