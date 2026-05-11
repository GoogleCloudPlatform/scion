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

// resumeCmd represents the resume command
var resumeCmd = &cobra.Command{
	Use:   "resume <agent-name>",
	Short: "Resume a stopped scion agent",
	Long: `Resume an existing stopped LLM agent.
The agent will be re-launched with the harness-specific resume flag,
preserving its previous state.

The agent-name is required as the first argument. Subsequent arguments
are optional and form a task prompt to be added to the resumed session
(if supported by the harness).`,
	Args:              cobra.MinimumNArgs(1),
	ValidArgsFunction: getAgentNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		return RunAgent(cmd, args, true)
	},
}

func init() {
	rootCmd.AddCommand(resumeCmd)
	resumeCmd.Flags().StringVarP(&templateName, "type", "t", "", "Template to use")
	resumeCmd.Flags().StringVarP(&agentImage, "image", "i", "", "Container image to use (overrides template)")
	resumeCmd.Flags().BoolVar(&noAuth, "no-auth", false, "Disable authentication propagation")
	resumeCmd.Flags().BoolVarP(&attach, "attach", "a", false, "Attach to the agent TTY after starting")

	resumeCmd.Flags().StringVar(&runtimeBrokerID, "broker", "", "Preferred runtime broker ID or name")

	// Template resolution flags for Hub mode (Section 9.4)
	resumeCmd.Flags().BoolVar(&uploadTemplate, "upload-template", false, "Automatically upload local template to Hub if not found")
	resumeCmd.Flags().BoolVar(&noUpload, "no-upload", false, "Fail if template requires upload (never prompt)")
	resumeCmd.Flags().StringVar(&templateScope, "template-scope", "project", "Scope for uploaded template (global, project, user)")


	// Telemetry override flags
	resumeCmd.Flags().BoolVar(&enableTelemetry, "enable-telemetry", false, "Explicitly enable telemetry for this agent")
	resumeCmd.Flags().BoolVar(&disableTelemetry, "disable-telemetry", false, "Explicitly disable telemetry for this agent")

	// Inline config flag
	resumeCmd.Flags().StringVar(&inlineConfigPath, "config", "", "Path to inline agent config file (YAML/JSON), or '-' for stdin")
}
