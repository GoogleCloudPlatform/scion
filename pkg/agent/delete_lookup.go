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
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// LocalAgentDirExists reports whether agentName has an agent directory that
// a local delete (DeleteAgentFiles) removes, in either layout an agent's
// state directory can use in the project projectPath resolves to:
//
//   - the project's own agents root, <project>/agents/<name> (for the global
//     project this is the global agents root, and a hub-native project keeps
//     its agents here too);
//   - the project's external agents root
//     (config.GetGitProjectExternalAgentsDir), where a shared-workspace
//     agent of a project with a project-id marker keeps its state.
//
// The global agents root is not checked for any other project:
// DeleteAgentFiles never removes from it then, so a directory found only
// there would be reported deleted while left in place.
//
// A directory counts whether or not it holds a scion-agent.json, so a
// directory kept by stale-directory cleanup is found too. It only reads the
// filesystem. agentName must pass the same single-path-element check
// DeleteAgentFiles applies, or an error is returned.
func LocalAgentDirExists(agentName, projectPath string) (bool, error) {
	if _, err := checkAgentDirContained(projectPath, agentName, false); err != nil {
		return false, err
	}

	// A project path that resolves to no project has no agent directory
	// DeleteAgentFiles would remove (it reports ErrAgentProjectUnresolved).
	projectDir, err := config.GetResolvedProjectDir(projectPath)
	if err != nil {
		return false, nil
	}
	roots := []string{filepath.Join(projectDir, "agents")}
	if extDir, err := config.GetGitProjectExternalAgentsDir(projectDir); err == nil && extDir != "" {
		roots = append(roots, extDir)
	}

	for _, root := range roots {
		if _, err := os.Stat(filepath.Join(root, agentName)); err == nil {
			return true, nil
		}
	}
	return false, nil
}
