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

// Package fsutil provides input validation and a safe recursive-chown
// implementation for callers that must change ownership of a directory tree
// computed from configuration, an environment variable, or another
// resolution step whose output isn't otherwise verified to be the intended
// per-agent/per-workspace directory. It is a stdlib-only leaf package with
// no dependency on any other scion package, so it can be imported from
// dependency-constrained callers (e.g. pkg/provision).
package fsutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Sentinel errors returned (wrapped) by CheckRoot and CheckMountSource.
// Callers that need to distinguish the reason use errors.Is.
var (
	// ErrEmptyRoot is returned when the given root is the empty string.
	ErrEmptyRoot = errors.New("chown root must not be empty")
	// ErrRelativeRoot is returned when the given root is not an absolute path.
	ErrRelativeRoot = errors.New("chown root must be an absolute path")
	// ErrCriticalSystemPath is returned when the given root names (or
	// resolves, via symlinks, to) one of criticalSystemPaths.
	ErrCriticalSystemPath = errors.New("chown root is a critical system path")
	// ErrHostRootLookalike is returned when the given root's contents look
	// like a filesystem root even though its path name does not.
	ErrHostRootLookalike = errors.New("chown root looks like a filesystem root")
	// ErrCriticalMountSource is returned by CheckMountSource when root is a
	// mount point whose bind source names a critical system directory.
	ErrCriticalMountSource = errors.New("chown root's mount source is a critical system path")
)

// criticalSystemPaths are absolute paths that must never be the root of a
// recursive ownership change, even if an upstream resolution bug computes
// one of them as the target. Operating on any of these would rewrite
// ownership across an entire host or container filesystem instead of one
// workspace or home directory.
var criticalSystemPaths = map[string]bool{
	"/":       true,
	"/bin":    true,
	"/boot":   true,
	"/dev":    true,
	"/etc":    true,
	"/home":   true,
	"/lib":    true,
	"/lib32":  true,
	"/lib64":  true,
	"/libx32": true,
	"/opt":    true,
	"/proc":   true,
	"/root":   true,
	"/run":    true,
	"/sbin":   true,
	"/srv":    true,
	"/sys":    true,
	"/usr":    true,
	"/var":    true,
}

// hostRootSignals are relative markers effectively only ever found together
// at the root of a real Unix filesystem. A legitimate per-agent workspace or
// home directory should never contain more than one of these by coincidence.
var hostRootSignals = []string{
	filepath.Join("etc", "passwd"),
	filepath.Join("usr", "bin"),
	"proc",
	filepath.Join("var", "log"),
	"boot",
}

// hostRootSignalThreshold is the number of hostRootSignals that must be
// present under a directory before it is treated as "looks like a
// filesystem root" and refused as a chown root.
const hostRootSignalThreshold = 2

// looksLikeHostRoot reports whether root appears to be a whole Unix
// filesystem root (host or container) rather than a real per-agent/per-user
// directory, based on the presence of hostRootSignals.
func looksLikeHostRoot(root string) bool {
	matches := 0
	for _, rel := range hostRootSignals {
		if _, err := os.Lstat(filepath.Join(root, rel)); err == nil {
			matches++
			if matches >= hostRootSignalThreshold {
				return true
			}
		}
	}
	return false
}

// CheckRoot validates that root is safe to use as the root of a recursive
// ownership change. It is the single point of input validation shared by
// every recursive-chown call site in this codebase; callers should not
// re-implement any part of this policy.
//
// Policy (all four checks must pass):
//  1. root must not be empty (ErrEmptyRoot).
//  2. root must be an absolute path (ErrRelativeRoot) — a relative root
//     depends on the caller's working directory, which is exactly the kind
//     of ambient, unverified state this check exists to distrust.
//  3. root must not name a critical system path (ErrCriticalSystemPath),
//     checked after filepath.Clean and, on a best-effort basis, after
//     resolving symlinks with filepath.EvalSymlinks.
//  4. root's contents must not look like a filesystem root
//     (ErrHostRootLookalike), even when its path name gives no indication —
//     see looksLikeHostRoot.
//
// CheckRoot does not touch the filesystem beyond stat-ing root and a small,
// fixed set of paths under it (check 4) and resolving symlinks in root's own
// path (check 3); it never walks a tree.
func CheckRoot(root string) error {
	if root == "" {
		return ErrEmptyRoot
	}
	if !filepath.IsAbs(root) {
		return fmt.Errorf("%w: %q", ErrRelativeRoot, root)
	}
	cleaned := filepath.Clean(root)
	if criticalSystemPaths[cleaned] {
		return fmt.Errorf("%w: %q", ErrCriticalSystemPath, root)
	}
	if resolved, err := filepath.EvalSymlinks(cleaned); err == nil {
		if resolved = filepath.Clean(resolved); criticalSystemPaths[resolved] {
			return fmt.Errorf("%w: %q resolves to %q", ErrCriticalSystemPath, root, resolved)
		}
	}
	if looksLikeHostRoot(root) {
		return fmt.Errorf("%w: %q (found %d+ host-root markers)", ErrHostRootLookalike, root, hostRootSignalThreshold)
	}
	return nil
}
