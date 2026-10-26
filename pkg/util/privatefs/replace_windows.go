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

//go:build windows

package privatefs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"

	"github.com/GoogleCloudPlatform/scion/pkg/util/pathcanon"
)

// ReplaceFollowing atomically replaces the file at path with data, for
// integrity-only files the user may manage through links, such as
// ~/.ssh/config (the follow-then-verify rule: follow the link, then decide
// on handles).
//
// Windows:
//   - path is resolved with pathcanon.Resolve, so a symlinked leaf (a
//     dotfile manager's link) is followed and the file it names is the one
//     replaced; the link itself is kept.
//   - The target's parent directory is opened (it pins the directory) and
//     must pass CheckNoForeignWriters on that handle: a parent another
//     principal can write to (or owns, other than SYSTEM or Administrators)
//     is refused, because that principal could swap the file after us.
//   - An existing target is opened relative to the parent's handle (a
//     reparse point there is refused). It must have one name (the one-name
//     rule) and be owned by the current user, SYSTEM or Administrators (the
//     CheckNoForeignWriters owner set); otherwise
//     it is refused. Such a target whose ACL is too open is replaced, not
//     refused.
//   - The new content goes to a temporary file created relative to the
//     parent's handle with the private descriptor, and is renamed over the
//     target through that handle. The old descriptor is never copied: a
//     writable ssh config is code execution via ProxyCommand. newMode is not
//     used; the result is always Private.
//
// Stated limit: the follow-then-verify rule protects the target, not the
// mutability of the path to it. Directories above the target's parent are
// not checked: a live swap is defeated by the handle-based operations
// above, and a swap between sessions needs a directory that passes the
// follow-then-verify rule, which another non-admin user cannot produce.
// (TreeOptions.Warn reports a followed directory link whose target's
// parent is writable by another principal.)
//
// tempPrefix names the temporary file (tempPrefix + leaf + "-<random>").
func ReplaceFollowing(path string, data []byte, newMode fs.FileMode, tempPrefix string) error {
	_ = newMode // POSIX only; see above
	if strings.ContainsAny(tempPrefix, `/\`) {
		return &fs.PathError{Op: "replace", Path: tempPrefix, Err: errors.New("temporary prefix contains a path separator")}
	}
	t, leaf, err := openReplaceTarget(path)
	if err != nil {
		return err
	}
	defer func() { _ = t.Close() }()

	f, _, err := t.CreateTemp("", tempPrefix+leaf+"-*", Private)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			_ = deleteHandle(windows.Handle(f.Fd()))
			_ = f.Close()
		}
	}()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	// The temporary file was created by this tree with the private
	// descriptor; rename it over the target through its open handle.
	if err := t.renameOpen(f, leaf); err != nil {
		return err
	}
	ok = true
	return f.Close()
}

// CheckReplaceTarget runs every check ReplaceFollowing makes before it
// writes, on the same handles, and writes nothing: path is resolved
// (following a symlinked leaf), its directory is opened and must pass
// CheckNoForeignWriters, and an existing target must not be a reparse
// point, must have one name (the one-name rule) and must be owned by the current
// user, SYSTEM or Administrators. Callers that write several files in one
// operation run it on every file first, so a refusal leaves no side
// effect. ReplaceFollowing
// still makes these checks itself; this is a preflight, not a
// replacement. Once the directory passes CheckNoForeignWriters, no other
// principal can change the target between this check and the write.
//
// A target whose directory does not exist yet passes: there is nothing to
// check, and the caller creates the directory (private) before writing.
// Run it BEFORE creating any directory, so a refusal creates nothing.
func CheckReplaceTarget(path string) error {
	t, _, err := openReplaceTarget(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return t.Close()
}

// openReplaceTarget resolves path, opens its directory as a tree, checks
// the directory with CheckNoForeignWriters and the existing target with
// checkReplaceTarget, and returns the tree and the target's leaf name. On
// error nothing is left open.
func openReplaceTarget(path string) (*Tree, string, error) {
	resolved, err := pathcanon.Resolve(path)
	if err != nil {
		return nil, "", err
	}
	dir, leaf := filepath.Dir(resolved), filepath.Base(resolved)
	if err := pathcanon.ValidName(leaf); err != nil {
		return nil, "", err
	}
	t, err := OpenTree(dir, TreeOptions{})
	if err != nil {
		return nil, "", err
	}
	parent, err := t.anchorFile()
	if err != nil {
		_ = t.Close()
		return nil, "", err
	}
	err = CheckNoForeignWriters(parent)
	_ = parent.Close()
	if err != nil {
		_ = t.Close()
		return nil, "", fmt.Errorf("refusing to write %s: its directory can be changed by another user: %w", path, err)
	}
	if err := t.checkReplaceTarget(leaf); err != nil {
		_ = t.Close()
		return nil, "", fmt.Errorf("refusing to write %s: %w", path, err)
	}
	return t, leaf, nil
}

// checkReplaceTarget applies the follow-then-verify rule to the existing
// file leaf directly below
// the anchor, if there is one: it must not be a reparse point, must have
// one name, and must be owned by the current user, SYSTEM or
// Administrators. Such a file that fails CheckNoForeignWriters only on its
// ACL is accepted here: the caller replaces it with a private one (the
// repair). A missing file is accepted (a new file).
func (t *Tree) checkReplaceTarget(leaf string) error {
	// A target whose name went after the open (errNoNames) is opened
	// again.
	return retryReplaced(t.display(leaf), func() error { return t.checkReplaceTargetOnce(leaf) })
}

// checkReplaceTargetOnce is one attempt of checkReplaceTarget.
func (t *Tree) checkReplaceTargetOnce(leaf string) error {
	h, _, err := t.openLeaf(leaf, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES, windows.FILE_OPEN,
		windows.FILE_NON_DIRECTORY_FILE, nil)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(h), t.display(leaf))
	defer func() { _ = f.Close() }()
	afterLinkCheckOpen(f.Name())
	if err := checkSingleLink(f); err != nil {
		return err
	}
	werr := CheckNoForeignWriters(f)
	if werr == nil {
		return nil
	}
	owned, err := ownerAllowed(f)
	if err != nil {
		return fmt.Errorf("%w (%v)", werr, err)
	}
	if !owned {
		return werr
	}
	// Owned by the current user (or SYSTEM or Administrators, see
	// CheckNoForeignWriters) but too open: replaced by a private file.
	return nil
}
