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

package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/ptone/scion-agent/pkg/config"
	"github.com/ptone/scion-agent/pkg/util"
	"github.com/spf13/cobra"
)

var cdwCmd = &cobra.Command{
	Use:   "cdw <agent-name|branch-name>",
	Short: "Change directory to an agent's workspace or a branch's worktree",
	Long: `Resolves the path to a worktree and changes into (starts a new shell in that directory).
First checks for an agent by name and enters its workspace if found.
Then checks for any git worktree checked out to the specified branch.`,
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: getAgentNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		var targetPath string

		// 1. Check for Agent
		projectDir, _ := config.GetResolvedProjectDir(grovePath)

		// Check project grove
		if projectDir != "" {
			agentDir := filepath.Join(projectDir, "agents", name)
			workspace := filepath.Join(agentDir, "workspace")
			if _, err := os.Stat(filepath.Join(workspace, ".git")); err == nil {
				targetPath = workspace
			}
		}

		// Check global grove if not found
		if targetPath == "" {
			globalAgentsDir, _ := config.GetGlobalAgentsDir()
			if globalAgentsDir != "" {
				agentDir := filepath.Join(globalAgentsDir, name)
				workspace := filepath.Join(agentDir, "workspace")
				if _, err := os.Stat(filepath.Join(workspace, ".git")); err == nil {
					targetPath = workspace
				}
			}
		}

		// 2. Check for Branch Worktree if not found
		if targetPath == "" && util.IsGitRepo() {
			path, err := util.FindWorktreeByBranch(name)
			if err == nil && path != "" {
				targetPath = path
			}
		}

		// Not found
		if targetPath == "" {
			return fmt.Errorf("no agent worktree found for '%s'. 'cdw' is only valid for git-based agents that use worktrees for workspace isolation.", name)
		}

		// Change directory
		if err := os.Chdir(targetPath); err != nil {
			return fmt.Errorf("error changing directory to '%s': %v", targetPath, err)
		}

		// Get shell
		shell := os.Getenv("SHELL")
		if shell == "" {
			shell = "bash" // fallback
		}

		shellPath, err := exec.LookPath(shell)
		if err != nil {
			// Try /bin/bash or /bin/sh if LookPath fails
			if _, err := os.Stat("/bin/bash"); err == nil {
				shellPath = "/bin/bash"
			} else if _, err := os.Stat("/bin/sh"); err == nil {
				shellPath = "/bin/sh"
			} else {
				return fmt.Errorf("error finding shell '%s': %v", shell, err)
			}
		}

		// Execute shell
		// We use syscall.Exec to replace the current process with the shell
		err = syscall.Exec(shellPath, []string{shell}, os.Environ())
		if err != nil {
			return fmt.Errorf("error executing shell '%s': %v", shellPath, err)
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(cdwCmd)
}
