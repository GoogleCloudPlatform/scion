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
	"github.com/spf13/cobra"
)

// startCmd represents the start command
var startCmd = &cobra.Command{
	Use:     "start <agent-name> [task...]",
	Aliases: []string{"run"},
	Short:   "Launch a new scion agent",
	Long: `Provision and launch a new isolated LLM agent to perform a specific task.
The agent will be created from a template and run in a detached container.

The agent-name is required as the first argument. All subsequent arguments 
form the task prompt. If no task arguments are provided, the agent will 
look for a prompt.md file in its root directory.`,
	Args:              cobra.MinimumNArgs(1),
	ValidArgsFunction: getAgentNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		return RunAgent(cmd, args, false)
	},
}

func init() {

	rootCmd.AddCommand(startCmd)

	startCmd.Flags().StringVarP(&templateName, "type", "t", "", "Template to use")

	startCmd.Flags().StringVarP(&agentImage, "image", "i", "", "Container image to use (overrides template)")

	startCmd.Flags().BoolVar(&noAuth, "no-auth", false, "Disable authentication propagation")

	startCmd.Flags().BoolVarP(&attach, "attach", "a", false, "Attach to the agent TTY after starting")

	startCmd.Flags().StringVarP(&branch, "branch", "b", "", "Git branch to use for the agent workspace")

	startCmd.Flags().StringVarP(&workspace, "workspace", "w", "", "Host path to mount as /workspace")

	startCmd.Flags().StringVar(&runtimeBrokerID, "broker", "", "Preferred runtime broker ID or name")

	// Template resolution flags for Hub mode (Section 9.4)
	startCmd.Flags().BoolVar(&uploadTemplate, "upload-template", false, "Automatically upload local template to Hub if not found")
	startCmd.Flags().BoolVar(&noUpload, "no-upload", false, "Fail if template requires upload (never prompt)")
	startCmd.Flags().StringVar(&templateScope, "template-scope", "grove", "Scope for uploaded template (global, grove, user)")

}
