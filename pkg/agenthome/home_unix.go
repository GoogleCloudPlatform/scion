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

//go:build unix

package agenthome

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/dirfd"
)

const dirOpenFlags = unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_RDONLY | unix.O_CLOEXEC

// Home is an agent home opened once; see the package documentation. A Home
// must not be used after Close, and Close must not race other calls.
type Home struct {
	path string
	fd   int
}

// Open opens the agent home at path, the broker-side path of the home.
// path itself is opened with a plain directory open (links in the home's
// own path, which lies in the broker-owned agent directory, are followed);
// everything below it is resolved through the returned Home.
func Open(path string) (*Home, error) {
	fd, err := unix.Open(path, unix.O_DIRECTORY|unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	return &Home{path: path, fd: fd}, nil
}

// Path returns the broker-side path the home was opened with.
func (h *Home) Path() string { return h.path }

// Close releases the home's directory descriptor.
func (h *Home) Close() error {
	if h.fd < 0 {
		return nil
	}
	err := unix.Close(h.fd)
	h.fd = -1
	return err
}

// componentErr turns an openat error on the directory component name (in
// the directory dirFd refers to) into the error this package reports for
// it. With O_DIRECTORY|O_NOFOLLOW a link is reported as ELOOP or ENOTDIR
// depending on the platform, so ENOTDIR is told apart with an lstat.
func componentErr(dirFd int, name string, err error) error {
	switch {
	case errors.Is(err, unix.ELOOP):
		return ErrLinkComponent
	case errors.Is(err, unix.ENOTDIR):
		var st unix.Stat_t
		if unix.Fstatat(dirFd, name, &st, unix.AT_SYMLINK_NOFOLLOW) == nil && st.Mode&unix.S_IFMT == unix.S_IFLNK {
			return ErrLinkComponent
		}
		return ErrNotDirectory
	}
	return err
}

// walk opens the directory parts (relative to the home) with no component
// followed through a link, creating missing components with mode when
// create is set. The caller owns the returned descriptor.
func (h *Home) walk(parts []string, create bool, mode os.FileMode) (int, error) {
	fd, err := unix.Openat(h.fd, ".", dirOpenFlags, 0)
	if err != nil {
		return -1, pathErr("open", h.path, nil, err)
	}
	for i, name := range parts {
		child, oerr := unix.Openat(fd, name, dirOpenFlags, 0)
		if errors.Is(oerr, unix.ENOENT) && create {
			if merr := unix.Mkdirat(fd, name, uint32(mode.Perm())); merr != nil && !errors.Is(merr, unix.EEXIST) {
				_ = unix.Close(fd)
				return -1, pathErr("mkdir", h.path, parts[:i+1], merr)
			}
			child, oerr = unix.Openat(fd, name, dirOpenFlags, 0)
		}
		if oerr != nil {
			cerr := componentErr(fd, name, oerr)
			_ = unix.Close(fd)
			return -1, pathErr("open", h.path, parts[:i+1], cerr)
		}
		_ = unix.Close(fd)
		fd = child
	}
	return fd, nil
}

// MkdirAll makes rel a chain of real directories below the home, creating
// missing ones with mode (subject to the umask, like os.MkdirAll). An
// existing component that is a link or not a directory is refused.
func (h *Home) MkdirAll(rel string, mode os.FileMode) error {
	parts, err := splitRel(rel)
	if err != nil {
		return err
	}
	fd, err := h.walk(parts, true, mode)
	if err != nil {
		return err
	}
	return unix.Close(fd)
}

func noChown(fd, uid, gid int) error { return nil }

// WriteFile writes data to rel with mode (not subject to the umask). The
// parent directory must exist. The content is written to a new temporary
// file and renamed over rel, so whatever entry is at rel (a link included)
// is replaced, never written through.
func (h *Home) WriteFile(rel string, data []byte, mode os.FileMode) error {
	parent, leaf, err := splitLeaf(rel)
	if err != nil {
		return err
	}
	fd, err := h.walk(parent, false, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	full := filepath.Join(h.path, filepath.FromSlash(rel))
	if err := dirfd.WriteAtNoFollowWithChown(fd, leaf, full, data, mode, 0, 0, dirfd.ReplaceLeaf, noChown); err != nil {
		return pathErr("write", h.path, append(parent, leaf), err)
	}
	return nil
}

// ReadFile reads the regular file rel, up to max bytes, without following
// a link at any component or at the leaf. A missing file satisfies
// errors.Is(err, fs.ErrNotExist).
func (h *Home) ReadFile(rel string, max int64) ([]byte, error) {
	parent, leaf, err := splitLeaf(rel)
	if err != nil {
		return nil, err
	}
	fd, err := h.walk(parent, false, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(fd) }()
	data, err := dirfd.ReadAtNoFollow(fd, leaf, max)
	if err != nil {
		return nil, pathErr("read", h.path, append(parent, leaf), err)
	}
	return data, nil
}

// openParentIfPresent opens rel's parent for a removal. present is false
// (with a nil error) when a component of the parent does not exist, so
// there is nothing to remove.
func (h *Home) openParentIfPresent(parent []string) (fd int, present bool, err error) {
	fd, err = h.walk(parent, false, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return -1, false, nil
		}
		return -1, false, err
	}
	return fd, true, nil
}

// Remove removes the entry rel: a file or link is unlinked (a link itself,
// never its target), an empty directory is removed. A missing entry or a
// missing parent is not an error; a link at a parent component is refused.
func (h *Home) Remove(rel string) error {
	parent, leaf, err := splitLeaf(rel)
	if err != nil {
		return err
	}
	fd, present, err := h.openParentIfPresent(parent)
	if err != nil || !present {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	err = unix.Unlinkat(fd, leaf, 0)
	if errors.Is(err, unix.EISDIR) || errors.Is(err, unix.EPERM) {
		// unlinkat without AT_REMOVEDIR refuses a directory (EISDIR on
		// Linux, EPERM elsewhere); retry as a directory removal.
		if derr := unix.Unlinkat(fd, leaf, unix.AT_REMOVEDIR); derr == nil || !errors.Is(derr, unix.ENOTDIR) {
			err = derr
		}
	}
	if err != nil && !errors.Is(err, unix.ENOENT) {
		return pathErr("remove", h.path, append(parent, leaf), err)
	}
	return nil
}

// RemoveAll removes rel and everything below it without following a link:
// a link at rel is removed itself, a directory is emptied through its own
// descriptor (dirfd.RemoveContentsNoFollow, which never descends through a
// link) and then removed. A missing entry or a missing parent is not an
// error; a link at a parent component is refused. The home itself cannot
// be removed.
func (h *Home) RemoveAll(rel string) error {
	parent, leaf, err := splitLeaf(rel)
	if err != nil {
		return err
	}
	fd, present, err := h.openParentIfPresent(parent)
	if err != nil || !present {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	if err := removeEntryAt(fd, leaf); err != nil {
		return pathErr("remove", h.path, append(parent, leaf), err)
	}
	return nil
}

// removeEntryAt removes name in the directory pfd refers to, never
// following a link. A missing entry is not an error.
func removeEntryAt(pfd int, name string) error {
	var st unix.Stat_t
	if err := unix.Fstatat(pfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		if err := dirfd.UnlinkAt(pfd, name); err != nil && !errors.Is(err, unix.ENOENT) {
			return err
		}
		return nil
	}
	cfd, err := unix.Openat(pfd, name, dirOpenFlags, 0)
	if err != nil {
		// Replaced since the stat (for example by a link): refuse rather
		// than follow.
		return componentErr(pfd, name, err)
	}
	dir := os.NewFile(uintptr(cfd), name)
	var firstErr error
	_, err = dirfd.RemoveContentsNoFollow(dir, nil, func(n string, e error) {
		if firstErr == nil {
			firstErr = fmt.Errorf("%s/%s: %w", name, n, e)
		}
	})
	_ = dir.Close()
	if err != nil {
		return err
	}
	if firstErr != nil {
		return firstErr
	}
	if err := unix.Unlinkat(pfd, name, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	return nil
}

// InstallTree replaces dstRel in the home with a copy of the directory
// srcRel in src, a broker-owned tree. Only directories and regular files
// are copied (links and special files in the source are skipped, never
// followed); directories get mode 0755 and files 0644, or 0755 when any
// execute bit is set in the source. The copy is built under a temporary
// name in dstRel's parent (created when missing, mode 0755), every entry
// created relative to its parent's descriptor; whatever is at dstRel is
// then removed the way RemoveAll removes it, and the copy is renamed into
// place. Exceeding lim aborts with a *LimitError and leaves dstRel as it
// was.
func (h *Home) InstallTree(src *os.Root, srcRel, dstRel string, lim Limits) error {
	parent, leaf, err := splitLeaf(dstRel)
	if err != nil {
		return err
	}
	fi, err := src.Lstat(srcRel)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("agenthome: %s is not a directory", srcRel)
	}
	pfd, err := h.walk(parent, true, 0o755)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(pfd) }()

	tmp, err := tempName(leaf)
	if err != nil {
		return err
	}
	if err := unix.Mkdirat(pfd, tmp, 0o700); err != nil {
		return pathErr("mkdir", h.path, append(parent, tmp), err)
	}
	installed := false
	defer func() {
		if !installed {
			_ = removeEntryAt(pfd, tmp)
		}
	}()
	tfd, err := unix.Openat(pfd, tmp, dirOpenFlags, 0)
	if err != nil {
		return pathErr("open", h.path, append(parent, tmp), componentErr(pfd, tmp, err))
	}
	b := &treeBudget{lim: lim}
	err = copyTreeInto(src, filepath.Clean(srcRel), tfd, b, 0)
	_ = unix.Close(tfd)
	if err != nil {
		return err
	}
	if err := removeEntryAt(pfd, leaf); err != nil {
		return pathErr("remove", h.path, append(parent, leaf), err)
	}
	if err := dirfd.RenameAt(pfd, tmp, leaf); err != nil {
		return pathErr("rename", h.path, append(parent, leaf), err)
	}
	installed = true
	return nil
}

// copyTreeInto copies the contents of the directory srcRel in src into the
// directory dstFd refers to.
func copyTreeInto(src *os.Root, srcRel string, dstFd int, b *treeBudget, depth int) error {
	if err := unix.Fchmod(dstFd, 0o755); err != nil {
		return err
	}
	entries, err := readDirSorted(src, srcRel)
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		s := filepath.Join(srcRel, name)
		fi, err := src.Lstat(s)
		if err != nil {
			return err
		}
		switch {
		case fi.IsDir():
			if err := b.depth(depth + 1); err != nil {
				return err
			}
			if err := b.entry(); err != nil {
				return err
			}
			if err := unix.Mkdirat(dstFd, name, 0o700); err != nil {
				return err
			}
			cfd, err := unix.Openat(dstFd, name, dirOpenFlags, 0)
			if err != nil {
				return componentErr(dstFd, name, err)
			}
			err = copyTreeInto(src, s, cfd, b, depth+1)
			_ = unix.Close(cfd)
			if err != nil {
				return err
			}
		case fi.Mode().IsRegular():
			if err := b.entry(); err != nil {
				return err
			}
			if err := copyFileInto(src, s, dstFd, name, copyMode(fi.Mode()), b); err != nil {
				return err
			}
		default:
			// Links and special files are skipped; they still count.
			if err := b.entry(); err != nil {
				return err
			}
		}
	}
	return nil
}

// copyFileInto copies the regular file s in src to name in the directory
// dstFd refers to, created with O_EXCL|O_NOFOLLOW.
func copyFileInto(src *os.Root, s string, dstFd int, name string, mode os.FileMode, b *treeBudget) error {
	in, err := src.Open(s)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	if st, err := in.Stat(); err != nil {
		return err
	} else if !st.Mode().IsRegular() {
		return fmt.Errorf("agenthome: %s is not a regular file", s)
	}
	out, err := dirfd.CreateExclAt(dstFd, name, 0o600)
	if err != nil {
		return err
	}
	var r io.Reader = in
	if rem := b.remainingBytes(); rem >= 0 {
		r = io.LimitReader(in, rem+1)
	}
	n, err := io.Copy(out, r)
	if err == nil {
		err = b.addBytes(n)
	}
	if err == nil {
		err = out.Chmod(mode)
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}
