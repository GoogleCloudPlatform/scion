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

package config

import (
	"os"
	"path/filepath"
	"syscall"
)

// The unix half of the Windows private-file split (the legacy migration
// owner check, mkdirLikeLegacyRoot and the settings backups). Each
// function is the POSIX code these sites ran
// before the split, unchanged; privatefile_windows.go decides the same
// questions on handles through privatefs.

// fileOwnerUID returns the uid that owns the file described by info. ok is
// false only if the platform does not expose *syscall.Stat_t.
func fileOwnerUID(info os.FileInfo) (int, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}

// currentUserOwns reports whether the migration may act on path, described
// by info (from the caller's Lstat or Stat): false when another uid owns
// it. The error is always nil on unix.
func currentUserOwns(_ string, info os.FileInfo) (bool, error) {
	if uid, ok := fileOwnerUID(info); ok && uid != geteuid() {
		return false, nil
	}
	return true, nil
}

// mkdirLikeLegacyRoot creates the canonical root scionHome/canonicalName
// with the permission bits of the legacy root (legacyInfo).
func mkdirLikeLegacyRoot(scionHome, _, canonicalName string, legacyInfo os.FileInfo) error {
	return os.MkdirAll(filepath.Join(scionHome, canonicalName), legacyInfo.Mode().Perm())
}

// copySettingsBackup writes backupPath as a new file holding a copy of the
// settings file at path, with path's permission bits (info, from Lstat).
func copySettingsBackup(path, backupPath string, info os.FileInfo) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(backupPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(backupPath)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(backupPath)
		return err
	}
	return f.Close()
}
