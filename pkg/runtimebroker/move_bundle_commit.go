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

package runtimebroker

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/GoogleCloudPlatform/scion/pkg/util/rootopen"
)

// Target-side staging, commit, and imported marker logic for move-bundle
// imports (design §3.5 "Target commit", §3.8).

const (
	moveBundleStageInfix     = ".bundle-"
	moveBundleStageSuffixLen = 32
	moveBundleMarkerPrefix   = ".scion-move-bundle-"
)

// errMoveBundleCommitCrash is returned by a test crash hook to simulate a
// process abort mid-commit (skipping cleanup so .bundle-* siblings remain).
var errMoveBundleCommitCrash = errors.New("simulated move-bundle commit crash")

// testHookMoveBundleCommit, when set (tests only), is called at commit steps
// ("before_workspace_commit", "after_workspace_entry", "before_home_commit",
// "after_home_entry", "before_marker").
var testHookMoveBundleCommit func(step string) error

func moveBundleCommitHook(step string) error {
	if testHookMoveBundleCommit == nil {
		return nil
	}
	return testHookMoveBundleCommit(step)
}

// newMoveBundleStageName returns a fresh sibling staging directory name
// ".<base>.bundle-<32hex>" for target directory base name base.
func newMoveBundleStageName(base string) (string, error) {
	var b [moveBundleStageSuffixLen / 2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "." + base + moveBundleStageInfix + hex.EncodeToString(b[:]), nil
}

// moveBundleImportedMarkerName returns ".scion-move-bundle-<rid>".
func moveBundleImportedMarkerName(rid string) string {
	return moveBundleMarkerPrefix + rid
}

// hasMoveBundleImportedMarker reports whether <agentDir>/.scion-move-bundle-<rid>
// already exists.
func hasMoveBundleImportedMarker(agentDir, rid string) (bool, error) {
	root, err := os.OpenRoot(agentDir)
	if err != nil {
		return false, err
	}
	defer func() { _ = root.Close() }()
	_, err = root.Lstat(moveBundleImportedMarkerName(rid))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// writeMoveBundleImportedMarker writes <agentDir>/.scion-move-bundle-<rid>
// with "imported\n" using O_CREATE|O_EXCL through rootopen.OpenFileForWrite.
func writeMoveBundleImportedMarker(agentDir, rid string) error {
	root, err := os.OpenRoot(agentDir)
	if err != nil {
		return fmt.Errorf("open agent dir for imported marker: %w", err)
	}
	defer func() { _ = root.Close() }()
	f, err := rootopen.OpenFileForWrite(root, moveBundleImportedMarkerName(rid), 0o600, false)
	if err != nil {
		return fmt.Errorf("create imported marker: %w", err)
	}
	_, writeErr := f.WriteString("imported\n")
	closeErr := f.Close()
	if writeErr != nil {
		return fmt.Errorf("write imported marker: %w", writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close imported marker: %w", closeErr)
	}
	return nil
}

// commitMoveBundleWorkspace moves every top-level entry of sibling stageName
// into workspaceDir (both children of filepath.Dir(workspaceDir)) using
// os.Root.Rename (fd-relative renameat). If any top-level entry already exists
// in workspaceDir, commit fails closed without overwriting it.
func commitMoveBundleWorkspace(workspaceDir, stageName string) error {
	parent := filepath.Dir(workspaceDir)
	wsBase := filepath.Base(workspaceDir)

	root, err := os.OpenRoot(parent)
	if err != nil {
		return fmt.Errorf("open workspace parent: %w", err)
	}
	defer func() { _ = root.Close() }()

	if err := moveBundleCommitHook("before_workspace_commit"); err != nil {
		return err
	}

	entries, err := fs.ReadDir(root.FS(), stageName)
	if err != nil {
		return fmt.Errorf("read workspace staging dir: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)

	// First pass: reject workspace identity entries (e.g. root ".scion") and
	// verify no top-level name already exists in workspaceDir before renaming
	// anything, so a conflict never leaves partial writes behind.
	for _, name := range names {
		if transfer.IsWorkspaceIdentityName(name) {
			return fmt.Errorf("workspace commit refused: entry %q is a reserved workspace identity name", name)
		}
		dst := wsBase + "/" + name
		if _, err := root.Lstat(dst); err == nil {
			return fmt.Errorf("workspace commit conflict: entry %q already exists", name)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("stat workspace target %q: %w", name, err)
		}
	}

	// Second pass: rename every validated top-level entry into workspaceDir.
	for _, name := range names {
		dst := wsBase + "/" + name
		if err := root.Rename(stageName+"/"+name, dst); err != nil {
			return fmt.Errorf("rename workspace entry %q: %w", name, err)
		}
		if err := moveBundleCommitHook("after_workspace_entry"); err != nil {
			return err
		}
	}
	return nil
}

// commitMoveBundleHome merges sibling stageName into homeDir (both children of
// filepath.Dir(homeDir)) with provision-rendered files winning:
//   - If a path does not exist in homeDir, it is renamed into place atomically
//     (including whole new subdirectories).
//   - If a path already exists in homeDir and both are real directories (not
//     symlinks), merge recurses into the directory.
//   - Otherwise (a provision-rendered file or symlink already exists, or types
//     differ), the staged entry is skipped and counted.
func commitMoveBundleHome(homeDir, stageName string) (int64, error) {
	parent := filepath.Dir(homeDir)
	homeBase := filepath.Base(homeDir)

	root, err := os.OpenRoot(parent)
	if err != nil {
		return 0, fmt.Errorf("open home parent: %w", err)
	}
	defer func() { _ = root.Close() }()

	if err := moveBundleCommitHook("before_home_commit"); err != nil {
		return 0, err
	}

	return mergeMoveBundleDir(root, stageName, homeBase)
}

func mergeMoveBundleDir(root *os.Root, srcDir, dstDir string) (int64, error) {
	entries, err := fs.ReadDir(root.FS(), srcDir)
	if err != nil {
		return 0, fmt.Errorf("read staged home dir: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)

	var skipped int64
	for _, name := range names {
		srcPath := srcDir + "/" + name
		dstPath := dstDir + "/" + name

		dstInfo, dstErr := root.Lstat(dstPath)
		if errors.Is(dstErr, fs.ErrNotExist) {
			if err := root.Rename(srcPath, dstPath); err != nil {
				return skipped, fmt.Errorf("rename home entry %q: %w", name, err)
			}
			if err := moveBundleCommitHook("after_home_entry"); err != nil {
				return skipped, err
			}
			continue
		}
		if dstErr != nil {
			return skipped, fmt.Errorf("stat home entry %q: %w", name, dstErr)
		}

		srcInfo, srcErr := root.Lstat(srcPath)
		if srcErr != nil {
			return skipped, fmt.Errorf("stat staged home entry %q: %w", name, srcErr)
		}

		// Both real directories (neither a symlink): recurse to merge children.
		srcIsDir := srcInfo.IsDir() && srcInfo.Mode()&fs.ModeSymlink == 0
		dstIsDir := dstInfo.IsDir() && dstInfo.Mode()&fs.ModeSymlink == 0
		if srcIsDir && dstIsDir {
			subSkipped, err := mergeMoveBundleDir(root, srcPath, dstPath)
			skipped += subSkipped
			if err != nil {
				return skipped, err
			}
			continue
		}

		// Provision-rendered entry wins. If the staged entry is a directory
		// colliding with a provision-rendered file/symlink, count all leaf
		// entries inside it as skipped; otherwise count 1.
		if srcIsDir {
			n, countErr := countDirEntries(root.FS(), srcPath)
			if countErr != nil {
				return skipped, countErr
			}
			skipped += n
		} else {
			skipped++
		}
	}
	return skipped, nil
}

func countDirEntries(fsys fs.FS, dir string) (int64, error) {
	var n int64
	err := fs.WalkDir(fsys, dir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if p == dir {
			return nil
		}
		if !d.IsDir() {
			n++
		}
		return nil
	})
	if err != nil {
		return n, fmt.Errorf("count skipped home entries: %w", err)
	}
	if n == 0 {
		// Empty staged dir collided with a non-dir in home.
		n = 1
	}
	return n, nil
}

// moveBundleCommitLchown is the lchown function used after committing imported
// bundle trees to target directories that require a specific NFS owner
// (e.g., Kubernetes NFS workspace and home leaves). Replaced in unit tests to
// record and assert target ownership without requiring root privileges.
var moveBundleCommitLchown = os.Lchown

// chownMoveBundleTree applies uid:gid ownership recursively to rootDir and all
// entries inside it without following symlinks. In non-root environments
// (such as unprivileged unit tests), EPERM is tolerated so local tests that do
// not stub moveBundleCommitLchown do not fail.
func chownMoveBundleTree(rootDir string, uid, gid int) error {
	if uid <= 0 && gid <= 0 {
		return nil
	}
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		return fmt.Errorf("open committed tree %q for chown: %w", filepath.Base(rootDir), err)
	}
	defer func() { _ = root.Close() }()

	nonRoot := os.Geteuid() != 0
	return fs.WalkDir(root.FS(), ".", func(p string, _ fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		target := rootDir
		if p != "." {
			target = filepath.Join(rootDir, filepath.FromSlash(p))
		}
		if err := moveBundleCommitLchown(target, uid, gid); err != nil {
			if errors.Is(err, fs.ErrNotExist) || (nonRoot && errors.Is(err, fs.ErrPermission)) {
				return nil
			}
			return fmt.Errorf("chown %q to %d:%d: %w", p, uid, gid, err)
		}
		return nil
	})
}

// isMoveBundleStageName reports whether name is ".<base>.bundle-<32hex>".
func isMoveBundleStageName(name, base string) bool {
	suffix, ok := strings.CutPrefix(name, "."+base+moveBundleStageInfix)
	if !ok || len(suffix) != moveBundleStageSuffixLen {
		return false
	}
	for _, c := range suffix {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
