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
	"io/fs"
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/util/privatefs"
)

// preservedAtomicMode is the mode writeFileAtomic asks for. The unix "keep
// the existing file's mode" rule does not apply on Windows: an existing
// file's Perm reads 0666 (or 0444) whatever its ACL, so copying it would
// mean nothing. writeFileAtomic writes ordinary files, mode 0644, which
// privatefs.ForMode maps to Inherit; writeFileAtomicMode then keeps an
// existing owner-only file private from its real ACL (see
// atomicProtection).
func preservedAtomicMode(string) os.FileMode {
	return 0644
}

// atomicProtection is the protection writeFileAtomicMode gives the new
// file. Private
// when mode is Private (privatefs.ForMode). Otherwise the existing target
// decides, as for the copies in mkdirLikeLegacyRoot and the settings
// backups: it is opened with
// Tree.OpenNoFollow in a Tree anchored at its directory and checked with
// privatefs.CheckPrivate on that handle, so a user-tightened file stays
// private, and protectionForCheck maps the result (Inherit only for
// ErrNotPrivate, which includes a security descriptor or owner privatefs
// cannot read; any other check error is Private). The mode's protection
// (Inherit) is kept when there is nothing to decide from: no target, a
// reparse point at the target (refused by OpenNoFollow and never followed:
// the write then replaces the link itself, not its target), or a directory
// the Tree cannot be opened on (the write itself reports that). Any other
// failure to open the target is Private (fail closed).
//
// The decision is made on the handle and the write goes through its own
// handle, as before; the target is not checked again by path in between.
func atomicProtection(targetPath string, mode os.FileMode) privatefs.Protection {
	p := privatefs.ForMode(mode)
	if p == privatefs.Private {
		return p
	}
	tree, err := privatefs.OpenTree(filepath.Dir(targetPath), privatefs.TreeOptions{})
	if err != nil {
		return p
	}
	defer func() { _ = tree.Close() }()
	f, err := tree.OpenNoFollow(filepath.Base(targetPath))
	switch {
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, privatefs.ErrReparsePoint):
		return p
	case err != nil:
		return privatefs.Private
	}
	defer func() { _ = f.Close() }()
	return protectionForCheck(privatefs.CheckPrivate(f))
}

// writeFileAtomicMode is writeFileAtomic with the protection of the new file
// given by atomicProtection:
//
//   - Private (mode grants nothing to group or others, or the existing
//     target passes CheckPrivate): the temporary file is created in a
//     privatefs Tree anchored at the target's directory with the owner-only
//     ACL set at creation, and renamed over the target through its
//     still-open handle. The result is private whatever the old
//     file's ACL was (a too-open file is replaced: an implicit repair).
//   - Inherit: the temporary file is created by createTempFile and gets the
//     directory's inherited ACL, as any new file there would. No Chmod is
//     called: on Windows it only sets the read-only attribute.
func writeFileAtomicMode(targetPath string, data []byte, mode os.FileMode) error {
	if atomicProtection(targetPath, mode) == privatefs.Private {
		return writeFileAtomicPrivate(targetPath, data)
	}
	dir := filepath.Dir(targetPath)
	tmp, err := createTempFile(dir, "."+filepath.Base(targetPath)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file for %s: %w: %w", targetPath, errAtomicTempCreate, err)
	}
	tmpName := tmp.Name()
	renamed := false
	defer func() {
		_ = tmp.Close()
		if !renamed {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write temp file for %s: %w", targetPath, err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync temp file for %s: %w", targetPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file for %s: %w", targetPath, err)
	}
	if beforeAtomicRename != nil {
		if err := beforeAtomicRename(tmpName, targetPath); err != nil {
			return fmt.Errorf("replace %s: %w", targetPath, err)
		}
	}
	if err := os.Rename(tmpName, targetPath); err != nil {
		return fmt.Errorf("replace %s: %w", targetPath, err)
	}
	renamed = true
	return syncDir(dir)
}

// writeFileAtomicPrivate is the Private case of writeFileAtomicMode. The
// temporary file is renamed, and on failure deleted, through its own
// handle; nothing acts on its name.
func writeFileAtomicPrivate(targetPath string, data []byte) error {
	dir := filepath.Dir(targetPath)
	tree, err := privatefs.OpenTree(dir, privatefs.TreeOptions{})
	if err != nil {
		return fmt.Errorf("create temp file for %s: %w: %w", targetPath, errAtomicTempCreate, err)
	}
	defer func() { _ = tree.Close() }()
	tmp, _, err := tree.CreateTemp("", "."+filepath.Base(targetPath)+".tmp-*", privatefs.Private)
	if err != nil {
		return fmt.Errorf("create temp file for %s: %w: %w", targetPath, errAtomicTempCreate, err)
	}
	renamed := false
	defer func() {
		if !renamed {
			_ = tree.RemoveOpen(tmp)
		}
		_ = tmp.Close()
	}()
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write temp file for %s: %w", targetPath, err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync temp file for %s: %w", targetPath, err)
	}
	if beforeAtomicRename != nil {
		if err := beforeAtomicRename(tmp.Name(), targetPath); err != nil {
			return fmt.Errorf("replace %s: %w", targetPath, err)
		}
	}
	if err := tree.RenameOpen(tmp, filepath.Base(targetPath)); err != nil {
		return fmt.Errorf("replace %s: %w", targetPath, err)
	}
	renamed = true
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", targetPath, err)
	}
	return syncDir(dir)
}

// syncDir would fsync dir so a completed rename survives a crash. Windows
// has no directory fsync (FlushFileBuffers needs write access, which a
// directory handle cannot have), so there is nothing to do: NTFS journals
// the rename itself.
func syncDir(string) error {
	return nil
}
