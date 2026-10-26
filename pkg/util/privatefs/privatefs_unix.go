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

package privatefs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// The unix implementation is the POSIX call sequence scion used before
// this package existed, unchanged: modes 0600/0700, O_NOFOLLOW on the
// leaf, the Perm&077 and owner-uid checks on the opened file.

func isSeparator(c byte) bool { return c == '/' }

func isAbsLike(rel string) bool { return filepath.IsAbs(rel) }

func fileMode(p Protection) os.FileMode {
	if p == Private {
		return 0o600
	}
	return 0o666
}

func dirMode(p Protection) os.FileMode {
	if p == Private {
		return 0o700
	}
	return 0o777
}

// Describe returns how p protects a file, for user-facing text.
func Describe(p Protection) string {
	if p == Private {
		return "mode 0600"
	}
	return "default permissions"
}

// Tree is a private directory tree rooted at a trusted anchor.
type Tree struct {
	anchor string
}

// OpenTree opens the tree rooted at anchor, which must be a directory
// (links at or above it are followed). With opt.CreateAnchor, a missing
// anchor is created with its parents (os.MkdirAll, mode 0700).
func OpenTree(anchor string, opt TreeOptions) (*Tree, error) {
	abs, err := filepath.Abs(anchor)
	if err != nil {
		return nil, err
	}
	if opt.CreateAnchor {
		if err := os.MkdirAll(abs, 0o700); err != nil {
			return nil, err
		}
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, &fs.PathError{Op: "open tree", Path: abs, Err: syscall.ENOTDIR}
	}
	return &Tree{anchor: abs}, nil
}

// Anchor returns the tree's anchor path.
func (t *Tree) Anchor() string { return t.anchor }

// Close releases the tree.
func (t *Tree) Close() error { return nil }

func (t *Tree) path(rel string) (string, error) {
	comps, err := splitRel(rel)
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{t.anchor}, comps...)...), nil
}

// MkdirAll creates rel and any missing parents with p (os.MkdirAll).
// Existing directories are left as they are.
func (t *Tree) MkdirAll(rel string, p Protection) error {
	path, err := t.path(rel)
	if err != nil {
		return err
	}
	return os.MkdirAll(path, dirMode(p))
}

// MkdirAllEach creates every rel with p, as MkdirAll does. (The Windows
// implementation first verifies every existing component of every rel
// before creating anything; on unix nothing below the anchor is checked,
// so there is nothing to verify first.)
func (t *Tree) MkdirAllEach(p Protection, rels ...string) error {
	for _, rel := range rels {
		if err := t.MkdirAll(rel, p); err != nil {
			return err
		}
	}
	return nil
}

// Create opens rel for reading and writing, creating it with p if it does
// not exist (excl: it must not exist). It never follows a symlink at rel.
// A Private file is also chmodded to 0600, so the umask cannot loosen it
// and an existing file is tightened.
func (t *Tree) Create(rel string, excl bool, p Protection) (*os.File, error) {
	path, err := t.path(rel)
	if err != nil {
		return nil, err
	}
	flags := os.O_RDWR | os.O_CREATE | syscall.O_NOFOLLOW | syscall.O_CLOEXEC
	if excl {
		flags |= os.O_EXCL
	}
	f, err := os.OpenFile(path, flags, fileMode(p))
	if err != nil {
		return nil, linkErr(path, err)
	}
	if p == Private {
		if err := f.Chmod(0o600); err != nil {
			_ = f.Close()
			return nil, err
		}
	}
	return f, nil
}

// OpenPrivate opens the existing file rel, never following a symlink, and
// verifies it with CheckPrivate. Opened for reading only, a file that is
// not private is refused. Opened for writing, a file owned by the current
// user is repaired (chmod 0600) before it is returned, and one owned by
// someone else is refused. flag is O_RDONLY, O_WRONLY or O_RDWR, optionally
// with O_TRUNC (applied after the check).
func (t *Tree) OpenPrivate(rel string, flag int) (*os.File, error) {
	path, err := t.path(rel)
	if err != nil {
		return nil, err
	}
	if flag&^(os.O_RDONLY|os.O_WRONLY|os.O_RDWR|os.O_TRUNC) != 0 || flag&(os.O_WRONLY|os.O_RDWR) == os.O_WRONLY|os.O_RDWR {
		return nil, &fs.PathError{Op: "open private", Path: path, Err: syscall.EINVAL}
	}
	write := flag&(os.O_WRONLY|os.O_RDWR) != 0
	// O_NONBLOCK: opening a FIFO planted at rel returns at once (or fails)
	// instead of blocking, so the regular-file check below refuses it. It
	// changes nothing for a regular file.
	f, err := os.OpenFile(path, (flag&^os.O_TRUNC)|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, linkErr(path, err)
	}
	if err := verifyOrRepair(f, write); err != nil {
		_ = f.Close()
		return nil, err
	}
	if write && flag&os.O_TRUNC != 0 {
		if err := f.Truncate(0); err != nil {
			_ = f.Close()
			return nil, err
		}
	}
	return f, nil
}

func verifyOrRepair(f *os.File, write bool) error {
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("refusing %s: it is not a regular file", f.Name())
	}
	err = CheckPrivate(f)
	if err == nil || !write {
		return err
	}
	owned, oerr := OwnedByCurrentUser(f)
	if oerr != nil || !owned {
		return err
	}
	return f.Chmod(0o600)
}

// ReadPrivate reads the regular file rel, never following a symlink, and
// refuses it unless CheckPrivate accepts it. At most max bytes are read
// (no limit when max <= 0).
func (t *Tree) ReadPrivate(rel string, max int64) ([]byte, error) {
	f, err := t.OpenPrivate(rel, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if max <= 0 {
		return io.ReadAll(f)
	}
	return io.ReadAll(io.LimitReader(f, max))
}

// WriteFile writes data to rel in place (os.WriteFile with mode 0600 for
// Private): an existing file keeps its mode.
func (t *Tree) WriteFile(rel string, data []byte, p Protection) error {
	path, err := t.path(rel)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, fileMode(p))
}

// CreateTemp creates a new file in the directory relDir ("" for the
// anchor), named by pattern as os.CreateTemp does, and returns it with its
// path relative to the anchor. A Private file is chmodded to 0600.
func (t *Tree) CreateTemp(relDir, pattern string, p Protection) (*os.File, string, error) {
	dir := t.anchor
	if relDir != "" {
		var err error
		if dir, err = t.path(relDir); err != nil {
			return nil, "", err
		}
	}
	if strings.ContainsRune(pattern, '/') {
		return nil, "", &fs.PathError{Op: "createtemp", Path: pattern, Err: errors.New("pattern contains a path separator")}
	}
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, "", err
	}
	if p == Private {
		if err := f.Chmod(0o600); err != nil {
			_ = f.Close()
			_ = os.Remove(f.Name())
			return nil, "", err
		}
	}
	name := filepath.Base(f.Name())
	if relDir != "" {
		name = relDir + "/" + name
	}
	return f, name, nil
}

// Rename renames oldRel to newRel (os.Rename, which replaces a symlink at
// newRel itself and never writes through it).
func (t *Tree) Rename(oldRel, newRel string) error {
	oldPath, err := t.path(oldRel)
	if err != nil {
		return err
	}
	newPath, err := t.path(newRel)
	if err != nil {
		return err
	}
	return os.Rename(oldPath, newPath)
}

// RenameOpen renames the private file open as f to newRel: os.Rename of
// f's name, today's unix behaviour. f stays open. What is enforced: f must
// pass CheckPrivate (fstat on the handle), otherwise it is refused with
// ErrNotPrivate and nothing is renamed. f need not come from this tree.
func (t *Tree) RenameOpen(f *os.File, newRel string) error {
	if err := CheckPrivate(f); err != nil {
		return &fs.PathError{Op: "rename", Path: f.Name() + " -> " + filepath.Join(t.anchor, newRel), Err: err}
	}
	newPath, err := t.path(newRel)
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), newPath)
}

// RemoveOpen removes the file open as f (created by this tree): os.Remove
// of f's name, today's unix behaviour. f stays open.
func (t *Tree) RemoveOpen(f *os.File) error {
	return os.Remove(f.Name())
}

// Remove removes rel (os.Remove, which removes a symlink itself).
func (t *Tree) Remove(rel string) error {
	path, err := t.path(rel)
	if err != nil {
		return err
	}
	return os.Remove(path)
}

// OpenDir opens the directory rel for reading, never following a symlink
// at rel, so its privacy can be checked and its entries read on the
// handle.
func (t *Tree) OpenDir(rel string) (*os.File, error) {
	path, err := t.path(rel)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, linkErr(path, err)
	}
	return f, nil
}

// linkErr wraps the ELOOP that O_NOFOLLOW returns for a symlink in
// ErrReparsePoint.
func linkErr(path string, err error) error {
	if errors.Is(err, syscall.ELOOP) {
		return fmt.Errorf("refusing %s: %w", path, ErrReparsePoint)
	}
	return err
}

// CheckPrivate verifies, on the open file, that it is owned by the current
// user and grants nothing to group or others (Perm&077 == 0).
func CheckPrivate(f *os.File) error {
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	return checkInfo(f.Name(), fi)
}

// currentUID is the uid that CheckPrivate, OwnedByCurrentUser and
// CheckNoForeignWriters compare a file's owner with: the real uid, one
// source for all three (a variable so tests can check that they agree).
var currentUID = os.Getuid

// checkInfo is CheckPrivate's predicate on a stat result.
func checkInfo(name string, fi fs.FileInfo) error {
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: %s is group- or world-accessible (mode %04o)", ErrNotPrivate, name, fi.Mode().Perm())
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != currentUID() {
		return fmt.Errorf("%w: %s is owned by another user", ErrNotPrivate, name)
	}
	return nil
}

// OwnedByCurrentUser reports whether the open file is owned by the
// current user (currentUID, as CheckPrivate).
func OwnedByCurrentUser(f *os.File) (bool, error) {
	fi, err := f.Stat()
	if err != nil {
		return false, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return false, fmt.Errorf("cannot read the owner of %s", f.Name())
	}
	return int(st.Uid) == currentUID(), nil
}

// CheckNoForeignWriters verifies, on the open file, that it is owned by the
// current user and is not writable by group or others (Perm&022 == 0).
func CheckNoForeignWriters(f *os.File) error {
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%w: %s is group- or world-writable (mode %04o)", ErrNotPrivate, f.Name(), fi.Mode().Perm())
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != currentUID() {
		return fmt.Errorf("%w: %s is owned by another user", ErrNotPrivate, f.Name())
	}
	return nil
}
