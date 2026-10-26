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
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
)

// FilesPresence is what AgentFilesPresence established about an agent's
// broker-side files. Only FilesAbsent is a proof; the zero value is
// FilesUnknown, so an unset result never counts as absent.
type FilesPresence int

const (
	// FilesUnknown means absence could not be established.
	FilesUnknown FilesPresence = iota
	// FilesAbsent means none of the agent's files exist.
	FilesAbsent
	// FilesPresent means at least one of the agent's files exists.
	FilesPresent
)

func (p FilesPresence) String() string {
	switch p {
	case FilesAbsent:
		return "absent"
	case FilesPresent:
		return "present"
	}
	return "unknown"
}

// AgentFilesPresence reports whether any of the broker-side files that
// deleteAgentFiles removes for agentName in the project at projectPath
// still exists. It checks exactly that set: the in-project agent
// directory (which holds a linked checkout's worktree), the split-storage
// agent directory, the state directory resolved from hubProjectID, the
// worktree-per-agent shared-base worktree, the global project's per-agent
// workspace, a sharer-registry entry, a git worktree entry for one of the
// agent's own worktree paths, and a branch named after the agent (the last
// three skipped for an empty-per-agent workspace, as delete skips them).
//
// It changes nothing. Any error other than a path not existing, and a
// project path that does not resolve to an existing project directory,
// gives FilesUnknown.
func AgentFilesPresence(agentName, projectPath, hubProjectID string) FilesPresence {
	if _, err := checkAgentDirContained(projectPath, agentName, false); err != nil {
		return FilesUnknown
	}
	projectDir, err := config.GetResolvedProjectDir(projectPath)
	if err != nil {
		return FilesUnknown
	}
	if _, err := os.Stat(projectDir); err != nil {
		return FilesUnknown
	}
	layout := resolveAgentProjectLayout(projectDir)
	agentsDir := filepath.Join(projectDir, "agents")
	var externalAgentDir string
	if layout.externalAgentsRoot != "" {
		externalAgentDir = filepath.Join(layout.externalAgentsRoot, agentName)
	}
	dirs := []string{
		filepath.Join(agentsDir, agentName),
		externalAgentDir,
		hubStateDirToRemove(projectDir, agentName, hubProjectID, externalAgentDir),
	}
	var sharedBaseWorktree string
	if layout.sharedBaseIsRepo {
		sharedBaseWorktree = filepath.Join(layout.sharedBase, "worktrees", agentName)
		dirs = append(dirs, sharedBaseWorktree)
	}
	if config.IsGlobalProjectDir(projectDir) {
		dirs = append(dirs, filepath.Join(projectDir, "workspace", agentName))
	}
	for _, d := range dirs {
		if d == "" {
			continue
		}
		switch p := pathPresence(d); p {
		case FilesAbsent:
		default:
			return p
		}
	}
	if layout.repoRoot == "" || persistedEmptyPerAgent([]string{agentsDir}, externalAgentDir, agentName) {
		return FilesAbsent
	}
	if _, _, found, err := provision.FindBranchForAgent(layout.repoRoot, projectDir, agentName); err != nil {
		return FilesUnknown
	} else if found {
		return FilesPresent
	}
	own := []string{filepath.Join(agentsDir, agentName, "workspace")}
	if sharedBaseWorktree != "" {
		own = append(own, sharedBaseWorktree)
	}
	if p := worktreeEntryPresence(layout.repoRoot, own); p != FilesAbsent {
		return p
	}
	return branchPresence(layout.repoRoot, api.Slugify(agentName))
}

// pathPresence stats one path without following a final symlink.
func pathPresence(p string) FilesPresence {
	_, err := os.Lstat(p)
	switch {
	case err == nil:
		return FilesPresent
	case errors.Is(err, fs.ErrNotExist):
		return FilesAbsent
	}
	return FilesUnknown
}

// worktreeEntryPresence reports whether git lists a worktree entry for one
// of paths in the repository at repoRoot (an entry can outlive its
// directory, which deleteAgentFiles removes as well).
func worktreeEntryPresence(repoRoot string, paths []string) FilesPresence {
	out, err := exec.Command("git", "-C", repoRoot, "worktree", "list", "--porcelain").Output()
	if err != nil {
		return FilesUnknown
	}
	want := map[string]bool{}
	for _, p := range paths {
		want[filepath.Clean(p)] = true
		if dir, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
			want[filepath.Join(dir, filepath.Base(p))] = true
		}
	}
	for _, line := range strings.Split(strings.ReplaceAll(string(out), "\r\n", "\n"), "\n") {
		if wt, ok := strings.CutPrefix(line, "worktree "); ok && want[filepath.Clean(wt)] {
			return FilesPresent
		}
	}
	return FilesAbsent
}

// branchPresence reports whether the branch exists in the repository at
// repoRoot. git show-ref exits 1 for a missing ref; anything else that is
// not success is unknown.
func branchPresence(repoRoot, branch string) FilesPresence {
	err := exec.Command("git", "-C", repoRoot, "show-ref", "--verify", "--quiet", "refs/heads/"+branch).Run()
	if err == nil {
		return FilesPresent
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return FilesAbsent
	}
	return FilesUnknown
}
