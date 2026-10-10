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

package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/shareddirs"
)

// NFSProjectHostPath returns the host path of projectID's NFS project
// directory, <MountRoot>/<shareID>/<SubPathRoot>/<projectID>, and the host
// base of the share it is on (<MountRoot>/<shareID>). The directory holds
// the project's workspace, shared-dirs, provisioning state (provision/),
// worktrees and per-agent directories.
//
// The path is derived from nfsBackend.Resolve, the same computation that
// gives agent pods their workspace subPath, so the layout has one source.
// It is a pure computation with no I/O. It returns an error unless the
// result passes nfsProjectPathGuard.
func NFSProjectHostPath(cfg *config.V1NFSConfig, projectID string) (projectDir, hostBase string, err error) {
	if cfg == nil {
		return "", "", errors.New("NFS config is nil")
	}
	if !shareddirs.ValidProjectID(projectID) {
		return "", "", fmt.Errorf("invalid project ID %q", projectID)
	}
	res, err := NewNFSBackend(cfg).Resolve(ResolveInput{ProjectID: projectID})
	if err != nil {
		return "", "", err
	}
	// The share host base must be a proper subdirectory of the mount root
	// (a share ID such as ".." or "" would otherwise move it to or above
	// the mount root).
	if !filepath.IsAbs(cfg.MountRoot) || !pathStrictlyUnder(res.HostBase, cfg.MountRoot) {
		return "", "", fmt.Errorf("NFS share host base %q is not strictly under mount root %q", res.HostBase, cfg.MountRoot)
	}
	subPathRoot, err := config.ResolveSubPathRoot(cfg.SubPathRoot)
	if err != nil {
		return "", "", err
	}
	// Resolve gives <hostBase>/<subPathRoot>/<projectID>/workspace.
	projectDir = filepath.Dir(res.HostPath)
	if err := nfsProjectPathGuard(projectDir, res.HostBase, subPathRoot, projectID); err != nil {
		return "", "", err
	}
	return projectDir, res.HostBase, nil
}

// nfsProjectPathGuard checks, lexically, that projectDir is exactly
// <hostBase>/<subPathRoot>/<projectID>: hostBase is absolute and not the
// filesystem root, no path segment is empty, "." or "..", and projectDir is
// strictly under both hostBase and <hostBase>/<subPathRoot> (compared by
// whole path components, so ".../projects-other" is not under
// ".../projects").
func nfsProjectPathGuard(projectDir, hostBase, subPathRoot, projectID string) error {
	if hostBase == "" || !filepath.IsAbs(hostBase) {
		return fmt.Errorf("NFS host base %q is not an absolute path", hostBase)
	}
	cleanBase := filepath.Clean(hostBase)
	if cleanBase == string(filepath.Separator) {
		return errors.New("NFS host base is the filesystem root")
	}
	for _, seg := range append(strings.Split(subPathRoot, "/"), projectID) {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("NFS project path has an empty or relative segment (subpath_root %q, project %q)", subPathRoot, projectID)
		}
	}
	want := filepath.Join(cleanBase, subPathRoot, projectID)
	if filepath.Clean(projectDir) != want {
		return fmt.Errorf("NFS project path %q is not %q", projectDir, want)
	}
	if err := ValidateNotExportRoot(want, cleanBase); err != nil {
		return err
	}
	if !pathStrictlyUnder(want, filepath.Join(cleanBase, subPathRoot)) {
		return fmt.Errorf("NFS project path %q is not under the subpath root", want)
	}
	return nil
}

// pathStrictlyUnder reports whether target is below root by whole path
// components and not equal to it. Both are cleaned first.
func pathStrictlyUnder(target, root string) bool {
	t, r := filepath.Clean(target), filepath.Clean(root)
	if t == r {
		return false
	}
	return strings.HasPrefix(t, strings.TrimSuffix(r, string(filepath.Separator))+string(filepath.Separator))
}

// CleanupNFSProject removes projectID's NFS project directory
// <MountRoot>/<shareID>/<SubPathRoot>/<projectID> (see NFSProjectHostPath)
// with everything in it: workspace, shared-dirs, provision/, worktrees and
// agent directories.
//
// Guards: the path must pass nfsProjectPathGuard. On disk, the subpath root
// must resolve (through any symlinks) to a directory strictly under the
// resolved share host base, and the project directory itself must be a
// real directory, not a symlink. Anything else is refused with an error and
// nothing is removed.
//
// Idempotent: a missing project directory, subpath root or share host base
// is success (nothing to remove).
func CleanupNFSProject(cfg *config.V1NFSConfig, projectID string) error {
	if cfg == nil {
		return fmt.Errorf("CleanupNFSProject: %w: NFS config is nil", ErrNFSCleanupRefused)
	}
	if projectID == "" {
		return fmt.Errorf("CleanupNFSProject: %w: projectID is required", ErrNFSCleanupRefused)
	}
	if len(cfg.Shares) == 0 {
		return fmt.Errorf("CleanupNFSProject: %w: no NFS shares configured", ErrNFSCleanupRefused)
	}
	projectDir, hostBase, err := NFSProjectHostPath(cfg, projectID)
	if err != nil {
		return fmt.Errorf("CleanupNFSProject: %w: %w", ErrNFSCleanupRefused, err)
	}

	// Resolve symlinks on the share base and the project's parent so a
	// symlinked subpath root cannot redirect the removal outside the share.
	realBase, err := filepath.EvalSymlinks(hostBase)
	if errors.Is(err, os.ErrNotExist) {
		slog.Debug("CleanupNFSProject: share host base does not exist; nothing to remove",
			"project_id", projectID, "host_base", hostBase)
		return nil
	}
	if err != nil {
		return fmt.Errorf("CleanupNFSProject: resolve share host base: %w", err)
	}
	realParent, err := filepath.EvalSymlinks(filepath.Dir(projectDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("CleanupNFSProject: resolve subpath root: %w", err)
	}
	if !pathStrictlyUnder(realParent, realBase) {
		return fmt.Errorf("CleanupNFSProject: %w: subpath root resolves to %q, outside the share %q", ErrNFSCleanupRefused, realParent, realBase)
	}
	target := filepath.Join(realParent, filepath.Base(projectDir))
	if !pathStrictlyUnder(target, realBase) {
		return fmt.Errorf("CleanupNFSProject: %w: %q is not under the share %q", ErrNFSCleanupRefused, target, realBase)
	}

	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		slog.Debug("CleanupNFSProject: project subtree does not exist (already cleaned)",
			"project_id", projectID, "path", projectDir)
		return nil
	}
	if err != nil {
		return fmt.Errorf("CleanupNFSProject: stat %s: %w", target, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("CleanupNFSProject: %w: %s is not a directory (mode %s)", ErrNFSCleanupRefused, target, info.Mode().Type())
	}

	slog.Info("CleanupNFSProject: removing project subtree",
		"project_id", projectID, "path", target)
	// os.RemoveAll unlinks symlinks inside the tree without following them.
	if err := os.RemoveAll(target); err != nil {
		return fmt.Errorf("CleanupNFSProject: remove %s: %w", target, err)
	}
	slog.Info("CleanupNFSProject: project subtree removed",
		"project_id", projectID, "path", target)
	return nil
}

// ErrNFSCleanupRefused marks a CleanupNFSProject error where a guard refused
// the removal (bad config, invalid ID, symlink, path outside the share).
// Retrying cannot help with such errors.
var ErrNFSCleanupRefused = errors.New("NFS project cleanup refused")

// CleanupNFSProjectRetry runs CleanupNFSProject and, if the removal itself
// fails (for example ENOTEMPTY because an agent pod was still terminating and
// writing into the tree), runs it once more after retryDelay. A guard refusal
// (ErrNFSCleanupRefused) is not retried. The wait ends early when ctx is
// done, and then the retry is skipped. It returns the last error; callers log
// it and never fail anything on it. It can block for retryDelay plus two
// removals, so callers on a request path run it in the background with a
// context of its own.
func CleanupNFSProjectRetry(ctx context.Context, cfg *config.V1NFSConfig, projectID string, retryDelay time.Duration) error {
	err := CleanupNFSProject(cfg, projectID)
	if err == nil || errors.Is(err, ErrNFSCleanupRefused) {
		return err
	}
	slog.Warn("CleanupNFSProject: removal failed; retrying once",
		"project_id", projectID, "retry_in", retryDelay, "error", err)
	t := time.NewTimer(retryDelay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("%w (retry skipped: %v)", err, ctx.Err())
	case <-t.C:
	}
	return CleanupNFSProject(cfg, projectID)
}
