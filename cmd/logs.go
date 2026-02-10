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
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ptone/scion-agent/pkg/agent"
	"github.com/ptone/scion-agent/pkg/runtime"
	"github.com/spf13/cobra"
)

// logsCmd represents the logs command
var logsCmd = &cobra.Command{
	Use:               "logs <agent>",
	Short:             "Get logs of an agent",
	Args:              cobra.ExactArgs(1),
	ValidArgsFunction: getAgentNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		agentName := args[0]

		// Check if Hub is enabled - logs command is not yet supported with Hub
		hubCtx, err := CheckHubAvailabilitySimple(grovePath)
		if err != nil {
			return err
		}
		if hubCtx != nil {
			return fmt.Errorf("logs command is not yet supported when using Hub integration\n\nTo view logs locally, use: scion --no-hub logs %s", agentName)
		}

		effectiveProfile := profile
		if effectiveProfile == "" {
			effectiveProfile = agent.GetSavedRuntime(agentName, grovePath)
		}

		rt := runtime.GetRuntime(grovePath, effectiveProfile)

		// Find the agent to get its grove path
		agents, err := rt.List(context.Background(), map[string]string{
			"scion.agent": "true",
			"scion.name":  agentName,
		})
		if err != nil {
			return fmt.Errorf("failed to find agent %s: %w", agentName, err)
		}
		if len(agents) == 0 {
			return fmt.Errorf("agent %s not found", agentName)
		}

		a := agents[0]
		if a.GrovePath == "" {
			return fmt.Errorf("agent %s has no grove path configured", agentName)
		}

		agentLogPath := filepath.Join(a.GrovePath, "agents", agentName, "home", "agent.log")
		if _, err := os.Stat(agentLogPath); os.IsNotExist(err) {
			return fmt.Errorf("log file not found: %s\n\nThe agent may not have started yet or does not produce logs", agentLogPath)
		}

		data, err := os.ReadFile(agentLogPath)
		if err != nil {
			return fmt.Errorf("failed to read log file: %w", err)
		}

		fmt.Print(string(data))
		return nil
	},
}

func init() {
	rootCmd.AddCommand(logsCmd)
}
