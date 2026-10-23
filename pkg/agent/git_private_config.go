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
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// maxPrivateGitConfigSize caps how much of a base .git/config is copied into the
// per-agent private copy. A real repository config is a few KiB; the cap bounds
// a pathological base (the broker never reads this copy back) rather than
// defining a security boundary.
const maxPrivateGitConfigSize = 1 << 20 // 1 MiB

// seedPrivateGitConfig writes a per-agent private copy of the repository's
// .git/config into the agent's (broker-owned, agent-scoped) state dir and
// returns its path, for the runtime to bind read-only as a single file over the
// container's .git/config.
//
// It reads the base config (absent → empty), strips credential-bearing keys
// (util.RedactGitConfigSecrets), and writes the result atomically with an
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
	gitDir := filepath.Join(agentDir, "git")
	if err := os.MkdirAll(gitDir, 0700); err != nil {
		return "", fmt.Errorf("seed private git config: %w", err)
	}

	raw, err := readBaseGitConfig(baseConfigPath)
	if err != nil {
		return "", fmt.Errorf("seed private git config: %w", err)
	}
	redacted, stripped, err := util.RedactGitConfigSecrets(raw)
	if err != nil {
		return "", fmt.Errorf("seed private git config: redact base config: %w", err)
	}
	if len(stripped) > 0 {
		util.Debugf("seedPrivateGitConfig: stripped credential-bearing keys from the private copy: %v", stripped)
	}

	// Drop any pre-existing upstream for the seed branch and push.autoSetupRemote
	// from the base before appending our own, so the copy never ends up with
	// duplicate single-valued branch.<b>.remote/merge keys. A base that already
	// carries them is the normal pre-upgrade state (an agent that ran `git push -u`
	// while the config was writable, or a host user's upstream); duplicates make
	// bare `git push` fail with "multiple upstream branches".
	redacted, err = util.StripGitConfigKeys(redacted, func(section, subsection string, hasSubsection bool, name string) bool {
		if section == "push" && !hasSubsection && name == "autosetupremote" {
			return true
		}
		if branch != "" && section == "branch" && hasSubsection && subsection == branch {
			return true
		}
		return false
	})
	if err != nil {
		return "", fmt.Errorf("seed private git config: strip stale upstream: %w", err)
	}

	// The repository config is read-only to the agent (the copy is bound
	// read-only), so `git config --local` and any config write cannot persist.
	// Pre-seed the agent's own branch upstream and push.autoSetupRemote so a BARE
	// `git push` (no arguments) finds the upstream already set and exits 0; without
	// the seed a bare `git push` fails with "no upstream branch" (exit 128).
	// `git push -u <origin> <branch>` exits 0 and reaches the remote with or without
	// the seed; under the read-only .git it cannot persist the upstream and git
	// reports an error locking the config (an error, not a warning), but the push
	// still succeeds. Personal preferences go in the agent's writable global git
	// config in the agent home.
	redacted = append(redacted, privateConfigUpstreamSeed(branch)...)

	tmp, err := os.CreateTemp(gitDir, "config.*.tmp") // O_EXCL
	if err != nil {
		return "", fmt.Errorf("seed private git config: temp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return "", fmt.Errorf("seed private git config: chmod: %w", err)
	}
	if _, err := tmp.Write(redacted); err != nil {
		tmp.Close()
		return "", fmt.Errorf("seed private git config: write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("seed private git config: close: %w", err)
	}
	dest := filepath.Join(gitDir, "config")
	if err := os.Rename(tmpName, dest); err != nil {
		return "", fmt.Errorf("seed private git config: rename: %w", err)
	}
	return dest, nil
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

// readBaseGitConfig reads up to maxPrivateGitConfigSize bytes of the base
// .git/config. A missing file is not an error (returns empty); a file larger
// than the cap is.
func readBaseGitConfig(path string) ([]byte, error) {
	// Open with O_NOFOLLOW so a symlink at the base .git/config path cannot
	// redirect the broker's read elsewhere, and confirm via fstat that the opened
	// file is a regular file (not a FIFO/device). A missing file is not an error.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("base git config %s is not a regular file", path)
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxPrivateGitConfigSize+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxPrivateGitConfigSize {
		return nil, fmt.Errorf("base git config %s exceeds %d bytes", path, maxPrivateGitConfigSize)
	}
	return raw, nil
}
