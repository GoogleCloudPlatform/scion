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
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/agenthome"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// Retained files of soft-deleted agents (ptone/scion#4744).
//
// A soft delete that keeps the agent's files leaves the agent directory on
// the runtime host and marks agent-info.json with phase "deleted"
// (MarkAgentSoftDeleted). Once the soft-delete retention window has passed,
// the hub asks the runtime host to remove those files. Before removing
// anything, the host checks with CheckRetainedAgentFiles that the files are
// still the retained files of a soft-deleted agent: a live agent's files
// (no mark) and directories that are symlinks are refused.

// softDeletedPhase is the agent-info.json phase MarkAgentSoftDeleted writes.
const softDeletedPhase = "deleted"

// retainedAgentInfoMaxSize bounds how much of agent-info.json is read.
const retainedAgentInfoMaxSize = 1 << 20

var (
	// ErrRetainedFilesNotMarked is returned when the agent's agent-info.json
	// is missing, unreadable or does not carry the soft-delete mark.
	ErrRetainedFilesNotMarked = errors.New("agent files carry no soft-delete mark")
	// ErrRetainedFilesSymlink is returned when one of the agent's
	// directories is a symlink rather than a directory.
	ErrRetainedFilesSymlink = errors.New("agent directory is a symlink")
)

// CheckRetainedAgentFiles reports whether agentName's files in projectPath
// are the retained files of a soft-deleted agent, which may be removed once
// the soft-delete window has passed. It returns nil only when agent-info.json
// is a regular file carrying phase "deleted", and none of the agent's
// directories (the agent directory under the project's agents root, the
// external agent directory of a git project, and the agent home) is a
// symlink. Errors name no host path.
//
// The caller holds the agent-files lock, so the files cannot be
// re-provisioned between this check and the removal.
func CheckRetainedAgentFiles(agentName, projectPath string) error {
	projectDir, err := config.GetResolvedProjectDir(projectPath)
	if err != nil {
		return fmt.Errorf("retained files: %w: %v", ErrAgentProjectUnresolved, err)
	}
	if _, err := checkAgentDirContained(projectDir, agentName, false); err != nil {
		return fmt.Errorf("retained files: %w", err)
	}

	dirs := []string{filepath.Join(projectDir, "agents", agentName)}
	if extDir, err := config.GetGitProjectExternalAgentsDir(projectDir); err == nil && extDir != "" {
		dirs = append(dirs, filepath.Join(extDir, agentName))
	}
	agentHome := config.GetAgentHomePath(projectDir, agentName)
	dirs = append(dirs, agentHome)
	for _, d := range dirs {
		info, err := os.Lstat(d)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("retained files: %w", ErrRetainedFilesNotMarked)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return ErrRetainedFilesSymlink
		}
	}

	if !retainedAgentInfoMarked(filepath.Join(agentHome, "agent-info.json")) {
		return ErrRetainedFilesNotMarked
	}
	return nil
}

// retainedAgentInfoMarked reports whether path is a regular file holding an
// agent-info.json with the soft-delete phase.
func retainedAgentInfoMarked(path string) bool {
	// Read confined to the agent home, path's directory
	// (agenthome.ReadFileAt).
	data, err := agenthome.ReadFileAt(filepath.Dir(path), filepath.Base(path), retainedAgentInfoMaxSize)
	if err != nil {
		return false
	}
	var ai api.AgentInfo
	if err := json.Unmarshal(data, &ai); err != nil {
		return false
	}
	return ai.Phase == softDeletedPhase
}
