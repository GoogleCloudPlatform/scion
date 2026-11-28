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

package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/initenv"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/procreap"
)

const (
	defaultContainerWorkspaceRoot = "/workspace"
	defaultContainerRepoRoot      = "/repo-root"
)

var errSafeDirectoryPrivilegeDropRequired = errors.New(
	"configureGitSafeDirectories: privilege drop required but uid/gid were not both set; refusing to run git as root")

// runConfigureGitSafeDirectories is RunInit's seam onto
// configureGitSafeDirectoriesWithPolicy. Production never reassigns it.
var runConfigureGitSafeDirectories = configureGitSafeDirectoriesWithPolicy

// scopedSafeDirectoryEntries returns the scoped git safe.directory entries for
// root: root itself and "<root>/*" (Git 2.46+ prefix match for subdirectories
// such as worktrees and submodules). Returns nil if root is empty, not an
// absolute path, or "/" (which would produce "/*", effectively a global
// wildcard).
func scopedSafeDirectoryEntries(root string) []string {
	if root == "" {
		return nil
	}
	clean := filepath.Clean(root)
	if !filepath.IsAbs(clean) || clean == "/" {
		return nil
	}
	return []string{clean, clean + "/*"}
}

// isPathUnderRoot reports whether path equals root or is nested beneath it.
func isPathUnderRoot(path, root string) bool {
	if path == "" || root == "" {
		return false
	}
	cleanPath := filepath.Clean(path)
	cleanRoot := filepath.Clean(root)
	return cleanPath == cleanRoot || strings.HasPrefix(cleanPath, cleanRoot+"/")
}

// agentSafeDirectoryPaths computes the scoped safe.directory entries for the
// agent container's global git config (~/.gitconfig).
//
// Container workspace layouts:
//   - Standard / shared-plain / clone-per-agent: workspace at /workspace (or
//     SCION_WORKSPACE_PATH).
//   - worktree-per-agent (Kubernetes NFS): SCION_WORKSPACE_PATH is
//     /repo-root/worktrees/<agent>, with shared git metadata at /repo-root/.git.
//   - worktree-per-agent (Docker/Podman/Apple): SCION_WORKSPACE_PATH is unset,
//     cwd is under /repo-root (e.g. /repo-root/.scion/agents/<name>/workspace
//     or /repo-root/worktrees/<name>), and /repo-root is mounted.
//   - Outside-root registered worktree (Docker/Podman/Apple): workspace is at
//     /workspace and shared git metadata is mounted at /repo-root/.git.
//
// Entries are always scoped to the mounted project-storage/workspace root(s)
// and never include "*" or "/*".
func agentSafeDirectoryPaths(getenv func(string) string, cwd string, dirExists func(string) bool) []string {
	var out []string
	seen := make(map[string]bool)
	addRoot := func(root string) {
		for _, entry := range scopedSafeDirectoryEntries(root) {
			if !seen[entry] {
				seen[entry] = true
				out = append(out, entry)
			}
		}
	}

	ws := getenv("SCION_WORKSPACE_PATH")
	if ws != "" {
		if isPathUnderRoot(ws, defaultContainerRepoRoot) {
			addRoot(defaultContainerRepoRoot)
		} else {
			addRoot(ws)
		}
	} else {
		addRoot(defaultContainerWorkspaceRoot)
	}

	if isPathUnderRoot(cwd, defaultContainerRepoRoot) || (ws == "" && dirExists != nil && dirExists(defaultContainerRepoRoot)) {
		addRoot(defaultContainerRepoRoot)
	}

	return out
}

func pathIsDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// configureGitSafeDirectoriesWithPolicy adds scoped safe.directory entries to
// the agent's global ~/.gitconfig so git works in project-storage trees and
// worktrees owned by another UID (ptone/scion#6656). It never modifies any
// repository's .git/config and never configures safe.directory="*".
func configureGitSafeDirectoriesWithPolicy(agentHome string, uid, gid int, requirePrivilegeDrop bool, policy initenv.Policy) error {
	if agentHome == "" {
		return nil
	}
	if !pathIsDir(agentHome) {
		log.Debug("Skipping git safe.directory setup: agent home %s is not a directory", agentHome)
		return nil
	}

	cwd, _ := os.Getwd()
	entries := agentSafeDirectoryPaths(os.Getenv, cwd, pathIsDir)
	if len(entries) == 0 {
		return nil
	}

	gitconfigPath := filepath.Join(agentHome, ".gitconfig")
	if fi, err := os.Stat(gitconfigPath); err == nil && !fi.Mode().IsRegular() {
		log.Warn("Refusing existing %s: not a regular file (mode %v); skipping git safe.directory configuration.", gitconfigPath, fi.Mode())
		return nil
	}

	var configureCmd func(cmd *exec.Cmd)
	switch {
	case uid > 0 && gid > 0:
		configureCmd = func(cmd *exec.Cmd) { configureGitCommandWithPolicy(cmd, uid, gid, agentHome, policy) }
	case requirePrivilegeDrop:
		return errSafeDirectoryPrivilegeDropRequired
	default:
		configureCmd = func(cmd *exec.Cmd) { cmd.Env = policy.StartupChildEnv("/root") }
	}

	// Read existing safe.directory entries so repeated starts stay idempotent
	// without disturbing any entries the user or template already configured.
	existing := make(map[string]bool)
	{
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cmd, err := newInitGitCommandContext(ctx, "config", "--file", gitconfigPath, "--null", "--get-all", "safe.directory")
		if err != nil {
			cancel()
			return fmt.Errorf("failed to resolve a trusted git binary: %w", err)
		}
		configureCmd(cmd)
		out, cerr := procreap.CombinedOutputManaged(cmd)
		cancel()
		if cerr == nil {
			for _, val := range strings.Split(string(out), "\x00") {
				if val != "" {
					existing[val] = true
				}
			}
		} else {
			var exitErr *exec.ExitError
			// Exit code 1 means the key or file does not exist yet.
			if !errors.As(cerr, &exitErr) || exitErr.ExitCode() != 1 {
				return fmt.Errorf("failed to read safe.directory from %s: %w", gitconfigPath, cerr)
			}
		}
	}

	for _, entry := range entries {
		if existing[entry] {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cmd, err := newInitGitCommandContext(ctx, "config", "--file", gitconfigPath, "--add", "safe.directory", entry)
		if err != nil {
			cancel()
			return fmt.Errorf("failed to resolve a trusted git binary: %w", err)
		}
		configureCmd(cmd)
		out, cerr := procreap.CombinedOutputManaged(cmd)
		cancel()
		if cerr != nil {
			return fmt.Errorf("failed to add safe.directory %q to %s: %s (%w)", entry, gitconfigPath, strings.TrimSpace(string(out)), cerr)
		}
		existing[entry] = true
	}

	return nil
}
