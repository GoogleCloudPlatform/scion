//go:build linux

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

package homeprep

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"golang.org/x/sys/unix"
)

// root is an open directory that every operation of this package stays
// beneath. Paths are relative to it, use "/" separators, and are resolved
// without following any symbolic link: through openat2 with
// RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS where the kernel allows it, else by a
// per-component O_NOFOLLOW walk. Operations on a final component
// (unlinkat, renameat, symlinkat, mkdirat) act on the parent's fd.
type root struct {
	fd   int
	name string // for messages only
}

// openat2Disabled forces the per-component walk. Tests set it to cover
// both paths.
var openat2Disabled = false

// openRoot opens dir as a root. dir itself must be a real directory, not a
// symbolic link.
func openRoot(dir string) (*root, error) {
	fd, err := unix.Open(dir, unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: dir, Err: err}
	}
	return &root{fd: fd, name: dir}, nil
}

func (r *root) close() {
	if r != nil && r.fd >= 0 {
		_ = unix.Close(r.fd)
		r.fd = -1
	}
}

// cleanRel validates a root-relative path: non-empty, relative, no "."
// or ".." components and no empty components after cleaning.
func cleanRel(rel string) (string, error) {
	if rel == "" || strings.HasPrefix(rel, "/") {
		return "", fmt.Errorf("invalid path %q: must be relative", rel)
	}
	c := path.Clean(rel)
	if c == "." || c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("invalid path %q: outside the home", rel)
	}
	for _, part := range strings.Split(c, "/") {
		if part == ".." || part == "." || part == "" {
			return "", fmt.Errorf("invalid path %q", rel)
		}
	}
	return c, nil
}

// open opens rel beneath the root with flags (O_NOFOLLOW and O_CLOEXEC are
// always added). A symbolic link in any component fails with ELOOP or
// EXDEV.
func (r *root) open(rel string, flags int, mode uint32) (int, error) {
	c, err := cleanRel(rel)
	if err != nil {
		return -1, err
	}
	flags |= unix.O_NOFOLLOW | unix.O_CLOEXEC
	if !openat2Disabled {
		fd, err := unix.Openat2(r.fd, c, &unix.OpenHow{
			Flags:   uint64(flags),
			Mode:    uint64(mode),
			Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS,
		})
		if !errors.Is(err, unix.ENOSYS) && !errors.Is(err, unix.EPERM) {
			if err != nil {
				return -1, &os.PathError{Op: "open", Path: c, Err: err}
			}
			return fd, nil
		}
	}
	dir, leaf, err := r.walkParent(c)
	if err != nil {
		return -1, err
	}
	defer closeUnlessRoot(r, dir)
	fd, err := unix.Openat(dir, leaf, flags, mode)
	if err != nil {
		return -1, &os.PathError{Op: "open", Path: c, Err: err}
	}
	return fd, nil
}

// walkParent opens the parent directory of the cleaned path c by a
// per-component O_NOFOLLOW walk and returns its fd and the final component.
// The returned fd is r.fd itself when c has one component; callers release
// it with closeUnlessRoot.
func (r *root) walkParent(c string) (int, string, error) {
	parts := strings.Split(c, "/")
	dir := r.fd
	for _, part := range parts[:len(parts)-1] {
		next, err := unix.Openat(dir, part, unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_RDONLY|unix.O_CLOEXEC, 0)
		closeUnlessRoot(r, dir)
		if err != nil {
			return -1, "", &os.PathError{Op: "open", Path: c, Err: err}
		}
		dir = next
	}
	return dir, parts[len(parts)-1], nil
}

func closeUnlessRoot(r *root, fd int) {
	if fd >= 0 && fd != r.fd {
		_ = unix.Close(fd)
	}
}

// parent returns an fd for the parent directory of rel and rel's final
// component. The fd must be released with closeUnlessRoot.
func (r *root) parent(rel string) (int, string, error) {
	c, err := cleanRel(rel)
	if err != nil {
		return -1, "", err
	}
	dir, leaf := path.Split(c)
	if dir == "" {
		return r.fd, leaf, nil
	}
	fd, err := r.open(strings.TrimSuffix(dir, "/"), unix.O_DIRECTORY|unix.O_RDONLY, 0)
	if err != nil {
		return -1, "", err
	}
	return fd, leaf, nil
}

// lstat stats rel without following a final symbolic link. Parents are
// resolved beneath the root with no symbolic links.
func (r *root) lstat(rel string) (unix.Stat_t, error) {
	var st unix.Stat_t
	dir, leaf, err := r.parent(rel)
	if err != nil {
		return st, err
	}
	defer closeUnlessRoot(r, dir)
	if err := unix.Fstatat(dir, leaf, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return st, &os.PathError{Op: "lstat", Path: rel, Err: err}
	}
	return st, nil
}

// readFile reads the regular file rel, at most max bytes. A symbolic link
// or other non-regular file fails.
func (r *root) readFile(rel string, max int64) ([]byte, error) {
	fd, err := r.open(rel, unix.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), rel)
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", rel)
	}
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("%s is larger than %d bytes", rel, max)
	}
	return data, nil
}

// writeFileAtomic writes data to rel through a uniquely named temporary
// file in the same directory, then renames it into place. tmpName is the
// temporary file's name. The rename replaces a regular file or a symbolic
// link at rel, never what a link points to.
func (r *root) writeFileAtomic(rel, tmpName string, data []byte, mode uint32) error {
	dir, leaf, err := r.parent(rel)
	if err != nil {
		return err
	}
	defer closeUnlessRoot(r, dir)
	fd, err := unix.Openat(dir, tmpName, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, mode)
	if err != nil {
		return &os.PathError{Op: "create", Path: tmpName, Err: err}
	}
	f := os.NewFile(uintptr(fd), tmpName)
	_, werr := f.Write(data)
	if werr == nil {
		werr = f.Sync()
	}
	if werr == nil {
		werr = unix.Fchmod(fd, mode)
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = unix.Unlinkat(dir, tmpName, 0)
		return werr
	}
	if err := unix.Renameat(dir, tmpName, dir, leaf); err != nil {
		_ = unix.Unlinkat(dir, tmpName, 0)
		return &os.PathError{Op: "rename", Path: rel, Err: err}
	}
	return nil
}

// mkdirAll creates the directories of rel that do not exist, as the
// current user with mode, never following a symbolic link. An existing
// component that is not a directory fails.
func (r *root) mkdirAll(rel string, mode uint32) error {
	c, err := cleanRel(rel)
	if err != nil {
		return err
	}
	dir := r.fd
	defer func() { closeUnlessRoot(r, dir) }()
	for _, part := range strings.Split(c, "/") {
		if err := unix.Mkdirat(dir, part, mode); err != nil && !errors.Is(err, unix.EEXIST) {
			return &os.PathError{Op: "mkdir", Path: c, Err: err}
		}
		next, err := unix.Openat(dir, part, unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			return &os.PathError{Op: "open", Path: c, Err: err}
		}
		closeUnlessRoot(r, dir)
		dir = next
	}
	return nil
}

// names lists the entries of the root directory.
func (r *root) names() ([]string, error) {
	fd, err := unix.Openat(r.fd, ".", unix.O_DIRECTORY|unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), r.name)
	defer func() { _ = f.Close() }()
	return f.Readdirnames(-1)
}
