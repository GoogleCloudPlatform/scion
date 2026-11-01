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

package maintenance

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ReconcilePluginsAfterRollback puts the broker plugins back in lockstep
// after the boot-failure guard rolled the hub back. The guard
// (scion-hub-rollback.sh) swaps only the current/previous links, so the
// plugin directory still holds the plugins of the release that failed.
// Call it at startup, before plugins are loaded.
//
// With <install_root>/rollback.marker {from, to} and current pointing at
// releases/<to>, each plugin file X in releases/<to>/plugins/ replaces
// pluginDir/X, with a copy and one rename, only when pluginDir/X is
// exactly releases/<from>/plugins/X (same SHA-256): the failed release's
// build. A plugin rebuilt or reinstalled by hand differs and is never
// touched, nor is one that is missing from pluginDir. Once replaced,
// pluginDir/X equals releases/<to>/plugins/X, so later boots change
// nothing.
//
// It returns the replaced paths. A missing marker, a marker that is not
// about the current release, or a release without a plugins directory is
// not an error: there is nothing to do. Release directories, their plugins
// directories and every file compared must be real (not symlinks), so
// nothing outside the install root or the plugin directory is read or
// written.
func ReconcilePluginsAfterRollback(installRoot, pluginDir string) ([]string, error) {
	if installRoot == "" || pluginDir == "" {
		return nil, nil
	}
	m, err := ReadRollbackMarker(installRoot)
	if err != nil || m == nil {
		return nil, err
	}
	for _, r := range []string{m.From, m.To} {
		if !releaseDirRE.MatchString(r) || strings.HasSuffix(r, partialSuffix) {
			return nil, fmt.Errorf("rollback marker names an invalid release %q", r)
		}
	}
	if m.From == m.To {
		return nil, nil
	}
	releases := filepath.Join(installRoot, releasesDirName)
	if target, err := os.Readlink(filepath.Join(installRoot, currentLinkName)); err != nil || filepath.Base(target) != m.To {
		return nil, nil // the hub has moved on since the rollback
	}
	if !isRealDir(filepath.Join(releases, m.To)) || !isRealDir(filepath.Join(releases, m.From)) {
		return nil, nil
	}
	toDir := filepath.Join(releases, m.To, pluginsSubdir)
	fromDir := filepath.Join(releases, m.From, pluginsSubdir)
	if !isRealDir(toDir) || !isRealDir(fromDir) || !isRealDir(pluginDir) {
		return nil, nil
	}

	entries, err := os.ReadDir(toDir)
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	var replaced []string
	var errs []error
	for _, e := range entries {
		file := e.Name()
		if name, ok := strings.CutPrefix(file, PluginBinaryPrefix); !ok || !pluginNameRE.MatchString(name) {
			continue
		}
		installed := filepath.Join(pluginDir, file)
		want, err := sha256Regular(filepath.Join(toDir, file))
		if err != nil {
			continue
		}
		have, err := sha256Regular(installed)
		if err != nil || have == want {
			continue // not installed, not a regular file, or already in lockstep
		}
		failed, err := sha256Regular(filepath.Join(fromDir, file))
		if err != nil || have != failed {
			continue // not the failed release's build: leave it
		}
		if err := replacePlugin(filepath.Join(toDir, file), installed); err != nil {
			errs = append(errs, err)
			continue
		}
		replaced = append(replaced, installed)
	}
	return replaced, errors.Join(errs...)
}

// replacePlugin copies src to a hidden temporary file next to dst and
// renames it over dst.
func replacePlugin(src, dst string) error {
	tmp := filepath.Join(filepath.Dir(dst), "."+filepath.Base(dst)+".reconcile")
	_ = os.Remove(tmp)
	if err := copyExecutable(src, tmp); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace %s: %w", dst, err)
	}
	return nil
}

// isRealDir reports whether p is a directory and not a symlink.
func isRealDir(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.IsDir()
}

// sha256Regular returns the SHA-256 of p, which must be a regular file and
// not a symlink. Only the file that was checked is read.
func sha256Regular(p string) (string, error) {
	before, err := os.Lstat(p)
	if err != nil {
		return "", err
	}
	if !before.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", p)
	}
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !os.SameFile(before, opened) {
		return "", fmt.Errorf("%s changed while it was read", p)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
