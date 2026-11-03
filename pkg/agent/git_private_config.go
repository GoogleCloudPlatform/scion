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

package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// maxPrivateGitConfigSize caps how much of a base .git/config is copied into the
// per-agent private copy. A real repository config is a few KiB; the cap bounds
// a pathological base (the broker never reads this copy back) rather than
// defining a security boundary.
const maxPrivateGitConfigSize = 1 << 20 // 1 MiB

// tmpConfigClose closes the written temp config before the rename. It is a
// package var only so a test can inject a close error and confirm the write path
// fails closed; it is otherwise exactly (*os.File).Close.
var tmpConfigClose = func(f *os.File) error { return f.Close() }

// seedCheckedPrivateGitConfig checks every repository config layer the agent's
// git reads raw (util.CheckSeedRepoConfigLayers: the common and the agent's own
// config.worktree, cross-checked against git) and then seeds the private copy
// of .git/config (seedPrivateGitConfig). The agent's own worktree admin dir is
// resolved from broker-trusted data only (util.TrustedWorktreeAdminName, the
// same resolution the runtime uses to bind it). When the workspace is a linked
// worktree (its .git is not a directory) and that name does not resolve, the
// seed refuses, as the runtime does before binding git metadata; otherwise an
// unresolved name means there is no own worktree layer and only the common
// layers are checked. Then every submodule admin dir and gitlink the agent's git
// can reach is checked (util.CheckSeedSubmodules). A refusal, or anything that
// cannot be checked, is returned before any file is written.
func seedCheckedPrivateGitConfig(ctx context.Context, repoRoot, workspace, agentDir, branch string) (string, error) {
	if agentDir == "" {
		return "", fmt.Errorf("seed private git config: empty agent dir")
	}
	gitDir := filepath.Join(repoRoot, ".git")
	// The repository's .git must be a real directory: every path the checks
	// below derive from it then matches the one git walks.
	if fi, err := os.Lstat(gitDir); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("seed private git config: the repository's .git must be a real directory, not a symlink or gitfile (%s)", gitDir)
	}
	adminName, ok := util.TrustedWorktreeAdminName(gitDir, workspace)
	if !ok && workspace != "" {
		// Without a resolved admin name, the workspace's .git may only be absent
		// (no own worktree layer) or a real directory (the main checkout).
		fi, err := os.Lstat(filepath.Join(workspace, ".git"))
		if (err != nil && !errors.Is(err, fs.ErrNotExist)) || (err == nil && !fi.IsDir()) {
			return "", fmt.Errorf("seed private git config: cannot resolve the broker-recorded worktree admin directory for workspace %q against %q; refusing to seed the repository config", workspace, gitDir)
		}
	}
	if err := util.CheckSeedRepoConfigLayers(ctx, gitDir, adminName); err != nil {
		return "", fmt.Errorf("seed private git config: %w", err)
	}
	// Only after the layers pass: the submodule check runs git against them.
	wt := workspace
	if wt == "" {
		wt = repoRoot
	}
	if err := util.CheckSeedSubmodules(ctx, gitDir, wt, adminName); err != nil {
		return "", fmt.Errorf("seed private git config: %w", err)
	}
	return seedPrivateGitConfig(filepath.Join(gitDir, "config"), agentDir, branch)
}

// seedPrivateGitConfig writes a per-agent private copy of the repository's
// .git/config into the agent's (broker-owned, agent-scoped) state dir and
// returns its path, for the runtime to bind read-only as a single file over the
// container's .git/config.
//
// It reads the base config (absent → empty) and prepares the copy with one
// call to util.PrepareSeededGitConfig, which parses the base once, refuses a
// base that sets a key the repository config policy does not allow (the error
// names the key, never its value, and is returned as the start error), strips
// credential-bearing keys and the stale upstream keys below, and re-serializes
// what was checked. It appends the broker's own upstream pre-seed and writes
// the result atomically with an
// O_EXCL temp file + rename at <agentDir>/git/config, mode 0600, owned by
// this broker process. The broker never reads this file back as configuration;
// the runtime re-checks it (regular file, not a symlink, link count 1,
// broker-owned) immediately before binding.
//
// The returned path is always a fresh, broker-written regular file: any stale
// file (including one an earlier agent run left, or a symlink) is replaced by the
// rename.
func seedPrivateGitConfig(baseConfigPath, agentDir, branch string) (string, error) {
	if agentDir == "" {
		return "", fmt.Errorf("seed private git config: empty agent dir")
	}
	raw, err := readBaseGitConfig(baseConfigPath)
	if err != nil {
		return "", fmt.Errorf("seed private git config: %w", err)
	}
	redacted, stripped, err := util.PrepareSeededGitConfig(raw, util.RepoPolicyLinked, staleUpstreamKey(branch))
	if err != nil {
		return "", fmt.Errorf("seed private git config: %w", err)
	}
	if len(stripped) > 0 {
		util.Debugf("seedPrivateGitConfig: stripped credential-bearing keys from the private copy: %v", stripped)
	}

	// The repository config is read-only to the agent (the copy is bound
	// read-only), so `git config --local` and any config write cannot persist.
	// Pre-seed the agent's own branch upstream and push.autoSetupRemote so a BARE
	// `git push` (no arguments) finds the upstream already set and exits 0; without
	// the seed a bare `git push` fails with "no upstream branch" (exit 128).
	// `git push -u <origin> <branch>` exits 0 and reaches the remote with or without
	// the seed; under the read-only .git it cannot persist the upstream and git
	// reports an error locking the config, but the push still succeeds. Personal
	// preferences go in the agent's writable global git config in the agent home.
	redacted = append(redacted, privateConfigUpstreamSeed(branch)...)

	// Create the git dir only once the base config has been read, so a
	// refused read (for example ErrUnsupported off unix) creates nothing.
	gitDir := filepath.Join(agentDir, "git")
	if err := os.MkdirAll(gitDir, 0700); err != nil {
		return "", fmt.Errorf("seed private git config: %w", err)
	}
	tmp, err := os.CreateTemp(gitDir, "config.*.tmp") // O_EXCL
	if err != nil {
		return "", fmt.Errorf("seed private git config: temp: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup of the temp config; a no-op once renamed. The write path
	// below fails closed (an explicit, checked close before the rename), so on any
	// error the temp is removed and no config file is left behind.
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close() // chmod already failed; the close is best-effort and the temp is removed by the deferred cleanup
		return "", fmt.Errorf("seed private git config: chmod: %w", err)
	}
	if _, err := tmp.Write(redacted); err != nil {
		_ = tmp.Close() // write already failed; the close is best-effort and the temp is removed by the deferred cleanup
		return "", fmt.Errorf("seed private git config: write: %w", err)
	}
	// Explicit, checked close before the rename: a close error here can mean the
	// written bytes did not reach the file, so fail closed (no rename; the deferred
	// cleanup removes the temp). tmpConfigClose is a test seam.
	if err := tmpConfigClose(tmp); err != nil {
		return "", fmt.Errorf("seed private git config: close: %w", err)
	}
	dest := filepath.Join(gitDir, "config")
	if err := os.Rename(tmpName, dest); err != nil {
		return "", fmt.Errorf("seed private git config: rename: %w", err)
	}
	return dest, nil
}

// staleUpstreamKey returns the strip predicate for the seeded copy: it drops
// any pre-existing upstream for the seed branch and push.autoSetupRemote from the
// base before our own are appended, so the copy never ends up with duplicate
// single-valued branch.<b>.remote/merge keys. A base that already carries them is
// the normal pre-upgrade state (an agent that ran `git push -u` while the config
// was writable, or a host user's upstream); duplicates make bare `git push` fail
// with "multiple upstream branches".
func staleUpstreamKey(branch string) func(section, subsection string, hasSubsection bool, name string) bool {
	return func(section, subsection string, hasSubsection bool, name string) bool {
		if section == "push" && !hasSubsection && name == "autosetupremote" {
			return true
		}
		if branch != "" && section == "branch" && hasSubsection && subsection == branch {
			return true
		}
		return false
	}
}

// conservativeBranchName matches branch names that need no git-config escaping in
// a [branch "..."] subsection or a value: ordinary refname characters only.
var conservativeBranchName = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// privateConfigUpstreamSeed returns the git config text appended to the private
// read-only repository config so a BARE `git push` (no arguments) exits 0:
// push.autoSetupRemote=true, and (when branch is known) the agent's own branch
// upstream set to origin. With the branch upstream already set, a bare push finds
// it and needs no config write; without any upstream config a bare push fails with
// "no upstream branch". `git push -u` succeeds with or without this seed; the seed
// is for the bare-push flow. For an ordinary branch name the block is written in
// git's canonical form. A git refname may legally
// contain a double quote; such a name is escaped in both the subsection header and
// the merge value (a bare quote would otherwise break the [branch "..."] header
// and every key after it); git may attempt a one-time canonical rewrite of such a
// header, which the read-only .git prevents without failing the push. An empty
// branch appends only the push setting.
func privateConfigUpstreamSeed(branch string) []byte {
	var b strings.Builder
	// Disable automatic gc and maintenance: under the read-only .git root they
	// cannot write their lock files in the root and would print warnings on routine
	// commands. (Apple reads the repository config directly, so this does not apply
	// there; see the workspace guide.)
	b.WriteString("\n[gc]\n\tauto = 0\n[maintenance]\n\tauto = false\n")
	b.WriteString("[push]\n\tautoSetupRemote = true\n")
	if branch != "" {
		if conservativeBranchName.MatchString(branch) {
			b.WriteString("[branch \"")
			b.WriteString(branch)
			b.WriteString("\"]\n\tremote = origin\n\tmerge = refs/heads/")
			b.WriteString(branch)
			b.WriteString("\n")
		} else {
			b.WriteString("[branch \"")
			b.WriteString(util.EscapeGitConfigSubsection(branch))
			b.WriteString("\"]\n\tremote = origin\n\tmerge = ")
			b.WriteString(util.EscapeGitConfigValue("refs/heads/" + branch))
			b.WriteString("\n")
		}
	}
	return []byte(b.String())
}
