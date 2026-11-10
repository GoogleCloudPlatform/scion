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
	"slices"
	"time"

	"golang.org/x/sys/unix"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/dirfd"
)

// fdSource is a treeSource over a directory descriptor. Every entry is
// classified with fstatat(AT_SYMLINK_NOFOLLOW) and opened with O_NOFOLLOW,
// so a link in the source is never followed.
type fdSource struct {
	fd   int
	mode fs.FileMode
}

// newFDSource takes ownership of fd, an O_DIRECTORY|O_NOFOLLOW descriptor.
func newFDSource(fd int) (*fdSource, error) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	return &fdSource{fd: fd, mode: fs.FileMode(st.Mode & 0o777)}, nil
}

// openSource opens the directory srcRel below src as a tree source; no
// component is followed through a link.
func openSource(src *Home, srcRel string) (*fdSource, error) {
	parts, err := splitRel(srcRel)
	if err != nil {
		return nil, err
	}
	fd, err := src.walk(parts, false, 0)
	if err != nil {
		return nil, err
	}
	return newFDSource(fd)
}

func (s *fdSource) dirMode() fs.FileMode { return s.mode }

func (s *fdSource) entries() ([]srcEntry, error) {
	// A fresh descriptor, so listing never moves s.fd's offset.
	lfd, err := unix.Openat(s.fd, ".", dirOpenFlags, 0)
	if err != nil {
		return nil, err
	}
	d := os.NewFile(uintptr(lfd), ".")
	names, err := d.Readdirnames(-1)
	_ = d.Close()
	if err != nil {
		return nil, err
	}
	slices.Sort(names)
	out := make([]srcEntry, 0, len(names))
	for _, name := range names {
		var st unix.Stat_t
		if err := unix.Fstatat(s.fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue
			}
			return nil, err
		}
		e := srcEntry{name: name, mode: fs.FileMode(st.Mode & 0o777), size: st.Size, dev: uint64(st.Dev), ino: uint64(st.Ino)}
		switch st.Mode & unix.S_IFMT {
		case unix.S_IFDIR:
			if SkipCopyName(name, true) {
				continue
			}
			e.kind = kindDir
		case unix.S_IFREG:
			if SkipCopyName(name, false) {
				continue
			}
			if st.Nlink == 1 {
				e.kind = kindFile
			}
		}
		out = append(out, e)
	}
	return out, nil
}

// sameEntry reports whether st is the entry e was listed as.
func sameEntry(st *unix.Stat_t, e srcEntry) bool {
	return uint64(st.Dev) == e.dev && uint64(st.Ino) == e.ino
}

func (s *fdSource) sub(e srcEntry) (treeSource, error) {
	cfd, err := unix.Openat(s.fd, e.name, dirOpenFlags, 0)
	if err != nil {
		return nil, componentErr(s.fd, e.name, err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(cfd, &st); err != nil || !sameEntry(&st, e) {
		_ = unix.Close(cfd)
		if err == nil {
			err = fmt.Errorf("agenthome: %s changed while being copied", e.name)
		}
		return nil, err
	}
	return newFDSource(cfd)
}

func (s *fdSource) open(e srcEntry) (io.ReadCloser, error) {
	fd, err := unix.Openat(s.fd, e.name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 || !sameEntry(&st, e) {
		_ = unix.Close(fd)
		if err == nil {
			err = fmt.Errorf("agenthome: %s changed while being copied", e.name)
		}
		return nil, err
	}
	return os.NewFile(uintptr(fd), e.name), nil
}

func (s *fdSource) close() error {
	if s.fd < 0 {
		return nil
	}
	err := unix.Close(s.fd)
	s.fd = -1
	return err
}

// InstallTreeFrom replaces dstRel in the home with a copy of the directory
// srcRel in src, a directory the caller opened from a trusted root with
// OpenBelow (or an accessor built on it), so no component on the way to
// the source was followed through a link. srcRel is walked the same way.
// The source is read through descriptors only: links, special files and
// regular files with more than one hard link are skipped (never followed;
// each counts toward lim.MaxFiles), and SkipCopyName entries are left out.
// Modes follow homeFileMode and homeDirMode. The destination is handled as
// InstallTree handles it: the copy is built under a temporary name in
// dstRel's parent (created when missing, mode 0755; a linked parent is
// refused), whatever is at dstRel is removed without following a link,
// and the copy is renamed into place. Any error, a *LimitError included,
// leaves dstRel as it was.
func (h *Home) InstallTreeFrom(src *Home, srcRel, dstRel string, lim Limits) error {
	s, err := openSource(src, srcRel)
	if err != nil {
		return err
	}
	defer func() { _ = s.close() }()
	return h.installTree(s, dstRel, lim)
}

// InstallTreeFS is InstallTreeFrom for a directory srcRel in an fs.FS
// (the embedded platform skills).
func (h *Home) InstallTreeFS(src fs.FS, srcRel string, dstRel string, lim Limits) error {
	s, err := newFSSource(src, srcRel)
	if err != nil {
		return err
	}
	return h.installTree(s, dstRel, lim)
}

func (h *Home) installTree(src treeSource, dstRel string, lim Limits) error {
	parent, leaf, err := splitLeaf(dstRel)
	if err != nil {
		return err
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
	err = copyInto(src, tfd, &treeBudget{lim: lim}, 0)
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

// copyInto copies src into the new, empty directory dstFd refers to.
func copyInto(src treeSource, dstFd int, b *treeBudget, depth int) error {
	entries, err := src.entries()
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := b.entry(); err != nil {
			return err
		}
		switch e.kind {
		case kindDir:
			if err := b.depth(depth + 1); err != nil {
				return err
			}
			if err := unix.Mkdirat(dstFd, e.name, 0o700); err != nil {
				return err
			}
			cfd, err := unix.Openat(dstFd, e.name, dirOpenFlags, 0)
			if err != nil {
				return componentErr(dstFd, e.name, err)
			}
			err = copySubdir(src, e, cfd, b, depth+1)
			_ = unix.Close(cfd)
			if err != nil {
				return err
			}
		case kindFile:
			if err := copyFileTo(src, e, dstFd, e.name, b, false); err != nil {
				return err
			}
		}
	}
	return unix.Fchmod(dstFd, uint32(homeDirMode(src.dirMode())))
}

func copySubdir(src treeSource, e srcEntry, dstFd int, b *treeBudget, depth int) error {
	child, err := src.sub(e)
	if err != nil {
		return err
	}
	defer func() { _ = child.close() }()
	return copyInto(child, dstFd, b, depth)
}

// copyFileTo copies the kindFile entry e of src to name, created with
// O_CREAT|O_EXCL|O_NOFOLLOW in the directory dstFd refers to, with mode
// homeFileMode. With sync set the file is fsynced before it is closed.
func copyFileTo(src treeSource, e srcEntry, dstFd int, name string, b *treeBudget, sync bool) error {
	in, err := src.open(e)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := dirfd.CreateExclAt(dstFd, name, 0o600)
	if err != nil {
		return err
	}
	err = copyBudgeted(out, in, b)
	if err == nil {
		err = out.Chmod(homeFileMode(e.mode))
	}
	if err == nil && sync {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

// OverlayTreeFrom copies the directory srcRel in src (opened and read as
// for InstallTreeFrom) into dstRel ("." for the home itself), merging:
// destination entries that are not in the source are kept. dstRel and its
// parents are created when missing (mode 0755); a linked component is
// refused. Below dstRel:
//   - a source directory reuses an existing real directory (its mode is
//     kept) or creates one with homeDirMode; a link at its place is
//     removed, never followed, and replaced by a directory; any other
//     entry there is an error;
//   - a source file is written to a temporary file in its directory
//     (O_CREAT|O_EXCL|O_NOFOLLOW, mode homeFileMode, fsynced) and renamed
//     over the entry, so a link there is replaced, never written through;
//     a directory there is an error.
//
// The source as listed before the copy is charged against the limits; a
// limit found then writes nothing. A source that changes during the copy
// can still fail midway with files already renamed in place (each file
// all or nothing); callers treat the error as fatal.
func (h *Home) OverlayTreeFrom(src *Home, srcRel, dstRel string, lim Limits) error {
	s, err := openSource(src, srcRel)
	if err != nil {
		return err
	}
	defer func() { _ = s.close() }()
	if err := preWalk(s, &treeBudget{lim: lim}, 0); err != nil {
		return err
	}
	parts, err := splitRel(dstRel)
	if err != nil {
		return err
	}
	dfd, err := h.walk(parts, true, 0o755)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(dfd) }()
	return overlayInto(s, dfd, &treeBudget{lim: lim}, 0)
}

func overlayInto(src treeSource, dstFd int, b *treeBudget, depth int) error {
	entries, err := src.entries()
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := b.entry(); err != nil {
			return err
		}
		switch e.kind {
		case kindDir:
			if err := b.depth(depth + 1); err != nil {
				return err
			}
			cfd, err := openOrMakeDirAt(dstFd, e.name, homeDirMode(e.mode))
			if err != nil {
				return err
			}
			err = overlaySubdir(src, e, cfd, b, depth+1)
			_ = unix.Close(cfd)
			if err != nil {
				return err
			}
		case kindFile:
			if err := overlayFileAt(src, e, dstFd, b); err != nil {
				return err
			}
		}
	}
	return nil
}

func overlaySubdir(src treeSource, e srcEntry, dstFd int, b *treeBudget, depth int) error {
	child, err := src.sub(e)
	if err != nil {
		return err
	}
	defer func() { _ = child.close() }()
	return overlayInto(child, dstFd, b, depth)
}

// openOrMakeDirAt opens the directory name in the directory pfd refers to,
// creating it with mode when missing and replacing a link there (removed,
// never followed). Any other non-directory entry is an error.
func openOrMakeDirAt(pfd int, name string, mode fs.FileMode) (int, error) {
	created := false
	var st unix.Stat_t
	err := unix.Fstatat(pfd, name, &st, unix.AT_SYMLINK_NOFOLLOW)
	switch {
	case errors.Is(err, unix.ENOENT):
		created = true
	case err != nil:
		return -1, err
	case st.Mode&unix.S_IFMT == unix.S_IFLNK:
		if err := dirfd.UnlinkAt(pfd, name); err != nil && !errors.Is(err, unix.ENOENT) {
			return -1, err
		}
		created = true
	case st.Mode&unix.S_IFMT != unix.S_IFDIR:
		return -1, fmt.Errorf("agenthome: %s exists and is not a directory", name)
	}
	if created {
		if err := unix.Mkdirat(pfd, name, 0o700); err != nil {
			return -1, err
		}
	}
	cfd, err := unix.Openat(pfd, name, dirOpenFlags, 0)
	if err != nil {
		return -1, componentErr(pfd, name, err)
	}
	if created {
		if err := unix.Fchmod(cfd, uint32(mode)); err != nil {
			_ = unix.Close(cfd)
			return -1, err
		}
	}
	return cfd, nil
}

// overlayFileAt installs the kindFile entry e of src as e.name in the
// directory dstFd refers to, through a temporary file renamed over it.
func overlayFileAt(src treeSource, e srcEntry, dstFd int, b *treeBudget) error {
	var st unix.Stat_t
	if err := unix.Fstatat(dstFd, e.name, &st, unix.AT_SYMLINK_NOFOLLOW); err == nil && st.Mode&unix.S_IFMT == unix.S_IFDIR {
		return fmt.Errorf("agenthome: %s exists and is a directory", e.name)
	}
	tmp, err := tempName(e.name)
	if err != nil {
		return err
	}
	if err := copyFileTo(src, e, dstFd, tmp, b, true); err != nil {
		_ = dirfd.UnlinkAt(dstFd, tmp)
		return err
	}
	if err := dirfd.RenameAt(dstFd, tmp, e.name); err != nil {
		_ = dirfd.UnlinkAt(dstFd, tmp)
		return err
	}
	return nil
}

// statInfo is the fs.FileInfo Lstat returns.
type statInfo struct {
	name string
	st   unix.Stat_t
}

func (i *statInfo) Name() string { return i.name }
func (i *statInfo) Size() int64  { return i.st.Size }
func (i *statInfo) Mode() fs.FileMode {
	m := fs.FileMode(i.st.Mode & 0o777)
	switch i.st.Mode & unix.S_IFMT {
	case unix.S_IFDIR:
		m |= fs.ModeDir
	case unix.S_IFLNK:
		m |= fs.ModeSymlink
	case unix.S_IFIFO:
		m |= fs.ModeNamedPipe
	case unix.S_IFSOCK:
		m |= fs.ModeSocket
	case unix.S_IFCHR:
		m |= fs.ModeDevice | fs.ModeCharDevice
	case unix.S_IFBLK:
		m |= fs.ModeDevice
	}
	return m
}
func (i *statInfo) ModTime() time.Time { return time.Unix(i.st.Mtim.Unix()) }
func (i *statInfo) IsDir() bool        { return i.st.Mode&unix.S_IFMT == unix.S_IFDIR }
func (i *statInfo) Sys() any           { return &i.st }

// Lstat describes the entry rel without following a link at the leaf; a
// linked parent component is refused. A missing entry satisfies
// errors.Is(err, fs.ErrNotExist).
func (h *Home) Lstat(rel string) (fs.FileInfo, error) {
	parent, leaf, err := splitLeaf(rel)
	if err != nil {
		return nil, err
	}
	fd, err := h.walk(parent, false, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(fd) }()
	info := &statInfo{name: leaf}
	if err := unix.Fstatat(fd, leaf, &info.st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, pathErr("lstat", h.path, append(parent, leaf), err)
	}
	return info, nil
}

// ReadDir lists the directory rel below the home ("." for the home itself)
// in name order. rel is walked with no component followed (a linked
// component is refused with ErrLinkComponent), and each entry is described
// with fstatat(AT_SYMLINK_NOFOLLOW): a link is reported as a link
// (fs.ModeSymlink) and never followed. Only names and metadata are read.
func (h *Home) ReadDir(rel string) ([]fs.FileInfo, error) {
	parts, err := splitRel(rel)
	if err != nil {
		return nil, err
	}
	fd, err := h.walk(parts, false, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(fd) }()
	lfd, err := unix.Openat(fd, ".", dirOpenFlags, 0)
	if err != nil {
		return nil, pathErr("open", h.path, parts, err)
	}
	d := os.NewFile(uintptr(lfd), ".")
	names, err := d.Readdirnames(-1)
	_ = d.Close()
	if err != nil {
		return nil, pathErr("readdir", h.path, parts, err)
	}
	slices.Sort(names)
	out := make([]fs.FileInfo, 0, len(names))
	for _, name := range names {
		info := &statInfo{name: name}
		if err := unix.Fstatat(fd, name, &info.st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue
			}
			return nil, pathErr("lstat", h.path, append(parts, name), err)
		}
		out = append(out, info)
	}
	return out, nil
}
