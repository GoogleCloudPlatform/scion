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

package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/util/privatefs"
)

// The Windows half of the private-file split (the legacy migration owner
// check, mkdirLikeLegacyRoot and the settings backups). Mode bits mean
// nothing on Windows (an existing file's Perm reads 0666 or 0444), so every
// decision here is made on an open handle through privatefs: the owner SID
// for the owner check, the real DACL for the root and the backups.

// currentUserOwns reports whether the migration may act on path: true only
// when the object at path is owned by the current user's SID. It is opened
// through privatefs.Tree.OpenNoFollow anchored at its directory, so a
// reparse point at path is refused (an error: the caller skips it, as unix
// skips a symlink it finds with Lstat), and the owner is read on that same
// handle. info is not used: its Sys() carries no owner on Windows.
func currentUserOwns(path string, _ os.FileInfo) (bool, error) {
	tree, err := privatefs.OpenTree(filepath.Dir(path), privatefs.TreeOptions{})
	if err != nil {
		return false, err
	}
	defer func() { _ = tree.Close() }()
	f, err := tree.OpenNoFollow(filepath.Base(path))
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	return privatefs.OwnedByCurrentUser(f)
}

// sourceProtection is the protection a copy of the existing object open as
// f gets (mkdirLikeLegacyRoot, the settings backups): Inherit only when f
// is shown not to be private,
// else Private. Protection follows the source's real ACL, never its
// meaningless Perm; see protectionForCheck for the error mapping.
func sourceProtection(f *os.File) privatefs.Protection {
	return protectionForCheck(privatefs.CheckPrivate(f))
}

// protectionForCheck maps a privatefs.CheckPrivate result to the copy's
// protection: Inherit for an error wrapping privatefs.ErrNotPrivate, and
// Private when the check passes or fails with any other error. Note what
// ErrNotPrivate covers: privatefs classifies a security descriptor or
// owner it cannot read (GetSecurityInfo or the owner lookup failing) as
// ErrNotPrivate, so that case yields Inherit ("Private if it passes, else
// Inherit"). Only the errors privatefs
// does not wrap that way (the DACL or an ACE cannot be read, the process
// identity cannot be built) fail closed to Private.
func protectionForCheck(err error) privatefs.Protection {
	if errors.Is(err, privatefs.ErrNotPrivate) {
		return privatefs.Inherit
	}
	return privatefs.Private
}

// mkdirLikeLegacyRoot creates the canonical root scionHome/canonicalName
// in a Tree anchored at scionHome: Private when the legacy
// root scionHome/legacyName passes CheckPrivate, else Inherit. An existing
// canonical root is left as it is.
func mkdirLikeLegacyRoot(scionHome, legacyName, canonicalName string, _ os.FileInfo) error {
	tree, err := privatefs.OpenTree(scionHome, privatefs.TreeOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tree.Close() }()
	src, err := tree.OpenDir(legacyName)
	if err != nil {
		return err
	}
	p := sourceProtection(src)
	_ = src.Close()
	return tree.MkdirAll(canonicalName, p)
}

// copySettingsBackup writes backupPath, in the directory of path, as a new
// file holding a copy of the settings file at path. The source is
// opened ONCE: the protection decision (sourceProtection: Private when it
// passes CheckPrivate, else Inherit) and the read are made on that same
// handle, so whatever object is actually read decides its own protection
// (no check-then-open). A partial copy is deleted
// through its own handle.
func copySettingsBackup(path, backupPath string, _ os.FileInfo) error {
	if filepath.Dir(backupPath) != filepath.Dir(path) {
		return fmt.Errorf("settings backup %s is not in the directory of %s", backupPath, path)
	}
	tree, err := privatefs.OpenTree(filepath.Dir(path), privatefs.TreeOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tree.Close() }()
	// GENERIC_READ includes READ_CONTROL, so the DACL is read on this
	// handle too.
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	return copyBackupFrom(tree, src, filepath.Base(backupPath))
}

// copyBackupFrom creates name in tree, exclusively, as a copy of the open
// file src, with the protection src's own handle earns (sourceProtection).
// It takes no source path: the decision cannot come from another object
// than the bytes copied.
func copyBackupFrom(tree *privatefs.Tree, src *os.File, name string) error {
	p := sourceProtection(src)
	data, err := io.ReadAll(src)
	if err != nil {
		return err
	}
	f, err := tree.Create(name, true, p)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = tree.RemoveOpen(f)
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = tree.RemoveOpen(f)
		_ = f.Close()
		return err
	}
	return f.Close()
}
