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

	"github.com/spf13/cobra"
)

var configSchemaCmd = &cobra.Command{
	Use:   "schema",
	Short: "Show the agent config schema for --config files",
	Long: `Display the available fields for inline agent configuration files.

These fields can be used in YAML or JSON files passed to --config when
starting or creating agents. Content fields (system_prompt, agent_instructions)
support inline text or file:// URIs for file references.

URI conventions:
  file:///absolute/path  — read from absolute file path
  file://relative/path   — read from path relative to the config file
  (no prefix)            — treated as inline content`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if isJSONOutput() {
			return outputJSON(configSchemaFields())
		}

		fmt.Println("Agent Config Schema (for use with --config)")
		fmt.Println("============================================")
		fmt.Println()

		for _, f := range configSchemaFields() {
			fmt.Printf("  %-24s %-12s %s\n", f.Name, f.Type, f.Description)
		}
		fmt.Println()
		fmt.Println("Content fields (system_prompt, agent_instructions) support:")
		fmt.Println("  - Inline text content")
		fmt.Println("  - file:///absolute/path  (absolute file reference)")
		fmt.Println("  - file://relative/path   (relative to config file)")
		return nil
	},
}

type schemaField struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Description string `json:"description"`
}

func configSchemaFields() []schemaField {
	return []schemaField{
		{Name: "harness_config", Type: "string", Description: "Named harness configuration to use"},
		{Name: "image", Type: "string", Description: "Container image"},
		{Name: "model", Type: "string", Description: "LLM model to use"},
		{Name: "user", Type: "string", Description: "Container unix user"},
		{Name: "auth_selected_type", Type: "string", Description: "Auth method (api-key, oauth-token, auth-file, vertex-ai)"},
		{Name: "task", Type: "string", Description: "Task prompt for the agent"},
		{Name: "branch", Type: "string", Description: "Git branch for the agent workspace"},
		{Name: "system_prompt", Type: "string", Description: "System prompt (inline text or file:// URI)"},
		{Name: "agent_instructions", Type: "string", Description: "Agent instructions (inline text or file:// URI)"},
		{Name: "env", Type: "map", Description: "Environment variables (key: value)"},
		{Name: "volumes", Type: "list", Description: "Volume mounts"},
		{Name: "detached", Type: "bool", Description: "Run in detached mode"},
		{Name: "max_turns", Type: "int", Description: "Maximum conversation turns"},
		{Name: "max_model_calls", Type: "int", Description: "Maximum model API calls"},
		{Name: "max_duration", Type: "string", Description: "Maximum duration (e.g., 30m, 1h)"},
		{Name: "resources", Type: "object", Description: "Compute resource requirements (requests/limits)"},
		{Name: "services", Type: "list", Description: "Sidecar service definitions"},
		{Name: "telemetry", Type: "object", Description: "Telemetry configuration"},
		{Name: "secrets", Type: "list", Description: "Required secrets declarations"},
		{Name: "hub", Type: "object", Description: "Hub connection settings"},
		{Name: "default_harness_config", Type: "string", Description: "Default harness-config name"},
	}
}

func init() {
	configCmd.AddCommand(configSchemaCmd)
}
