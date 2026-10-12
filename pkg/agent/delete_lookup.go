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
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// LocalAgentDirExists reports whether agentName has an agent directory that a
// local delete (DeleteAgentFiles) would remove, in any of the layouts an
// agent's state directory can use:
//
//   - the project's own agents root, <project>/agents/<name> (this is also
//     where a hub-native project keeps its agents);
//   - the project's external agents root
//     (config.GetGitProjectExternalAgentsDir), where a shared-workspace
//     agent of a project with a project-id marker keeps its state;
//   - the global agents root.
//
// A directory counts whether or not it holds a scion-agent.json, so a
// directory kept by stale-directory cleanup is found too. It only reads the
// filesystem. An agentName that is not a single path element is an error.
func LocalAgentDirExists(agentName, projectPath string) (bool, error) {
	if agentName == "" || agentName == "." || agentName == ".." || filepath.Base(agentName) != agentName || filepath.Clean(agentName) != agentName {
		return false, fmt.Errorf("agent %q is not a single path element", agentName)
	}

	var roots []string
	if projectDir, err := config.GetResolvedProjectDir(projectPath); err == nil {
		roots = append(roots, filepath.Join(projectDir, "agents"))
		if extDir, err := config.GetGitProjectExternalAgentsDir(projectDir); err == nil && extDir != "" {
			roots = append(roots, extDir)
		}
	}
	if globalDir, err := config.GetGlobalAgentsDir(); err == nil {
		roots = append(roots, globalDir)
	}

	for _, root := range roots {
		if _, err := os.Stat(filepath.Join(root, agentName)); err == nil {
			return true, nil
		}
	}
	return false, nil
}
