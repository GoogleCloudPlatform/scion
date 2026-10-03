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
	"bytes"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
)

// provisionerOwnedFiles are the bundled provisioner scripts that belong to the
// harness bundle rather than the operator. capture_auth.py is included
// because it calls into the vendored scion_harness.py and must stay in step
// with it. Like config.yaml, these scripts are refreshed from the bundled
// copy during non-force seeding so that provisioner fixes reach existing
// nodes. Other user files are preserved.
var provisionerOwnedFiles = map[string]bool{
	"provision.py":     true,
	"scion_harness.py": true,
	"capture_auth.py":  true,
}

// isProvisionerOwnedFile reports whether relPath, relative to the
// harness-config root, is a bundled provisioner script.
func isProvisionerOwnedFile(relPath string) bool {
	return provisionerOwnedFiles[filepath.ToSlash(filepath.Clean(relPath))]
}

// provisionerScriptRefresh describes the outcome of refreshProvisionerScript.
type provisionerScriptRefresh int

const (
	// provisionerScriptUnchanged means the target already matches the bundle.
	provisionerScriptUnchanged provisionerScriptRefresh = iota
	// provisionerScriptCreated means the target did not exist.
	provisionerScriptCreated
	// provisionerScriptReplaced means an existing regular file differed from
	// the bundle.
	provisionerScriptReplaced
	// provisionerScriptUserManaged means the target is a symlink or another
	// non-regular file. It is left untouched.
	provisionerScriptUserManaged
)

// inspectProvisionerScript compares targetPath with the bundled data without
// writing anything. Symlinks are never followed.
func inspectProvisionerScript(targetPath string, data []byte) (provisionerScriptRefresh, error) {
	info, err := os.Lstat(targetPath)
	if os.IsNotExist(err) {
		return provisionerScriptCreated, nil
	}
	if err != nil {
		return 0, fmt.Errorf("stat %s: %w", targetPath, err)
	}
	if !info.Mode().IsRegular() {
		return provisionerScriptUserManaged, nil
	}
	current, err := os.ReadFile(targetPath)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", targetPath, err)
	}
	if bytes.Equal(current, data) {
		return provisionerScriptUnchanged, nil
	}
	return provisionerScriptReplaced, nil
}

// writeFileAtomic writes data to targetPath through a temporary file in the
// same directory followed by a rename, so readers never see a partial file.
// It preserves the mode of an existing regular file and uses 0644 otherwise.
func writeFileAtomic(targetPath string, data []byte) error {
	mode := os.FileMode(0644)
	if info, err := os.Lstat(targetPath); err == nil && info.Mode().IsRegular() {
		mode = info.Mode().Perm()
	}
	dir := filepath.Dir(targetPath)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(targetPath)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file for %s: %w", targetPath, err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write temp file for %s: %w", targetPath, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("chmod temp file for %s: %w", targetPath, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temp file for %s: %w", targetPath, err)
	}
	if err := os.Rename(tmpName, targetPath); err != nil {
		cleanup()
		return fmt.Errorf("replace %s: %w", targetPath, err)
	}
	return nil
}

// refreshProvisionerScript makes targetPath match the bundled data. A
// symlinked or otherwise non-regular target is treated as user-managed and
// left alone; the link is neither replaced nor written through.
func refreshProvisionerScript(targetPath string, data []byte) (provisionerScriptRefresh, error) {
	result, err := inspectProvisionerScript(targetPath, data)
	if err != nil {
		return 0, err
	}
	switch result {
	case provisionerScriptCreated, provisionerScriptReplaced:
		if err := writeFileAtomic(targetPath, data); err != nil {
			return 0, err
		}
	}
	return result, nil
}

// seedHarnessConfigFile seeds one non-config.yaml file of a harness-config.
// With force, every file is overwritten. Without force, provisioner-owned
// scripts are refreshed from the bundle and other existing files are kept.
func seedHarnessConfigFile(srcFS fs.FS, basePath, relPath, targetPath string, force bool) error {
	if force || !isProvisionerOwnedFile(relPath) {
		return seedFileFromGenericFS(srcFS, basePath, relPath, targetPath, force, false)
	}
	data, err := fs.ReadFile(srcFS, path.Join(filepath.ToSlash(basePath), filepath.ToSlash(relPath)))
	if err != nil {
		return nil // not in the bundle; nothing to seed
	}
	result, err := refreshProvisionerScript(targetPath, data)
	if err != nil {
		return err
	}
	if result == provisionerScriptUserManaged {
		slog.Warn("Skipping refresh of user-managed provisioner script (symlink or non-regular file)",
			"path", targetPath)
	}
	return nil
}
