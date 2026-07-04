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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/spf13/cobra"
)

var execTimeout int

// execCmd represents the exec command
var execCmd = &cobra.Command{
	Use:   "exec <agent> -- <command> [args...]",
	Short: "Execute a command inside an agent container",
	Long: `Execute a command inside a running agent's container.

In hub mode, the command is dispatched through the hub to the runtime broker
that owns the agent. In local mode, the command runs directly on the local
container runtime (Docker, Podman, or Apple Container).

This works across all runtime backends — the CLI abstracts away the
differences between docker exec, podman exec, container exec, and kubectl exec.`,
	Args:              cobra.MinimumNArgs(1),
	ValidArgsFunction: getAgentNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		agentName := api.Slugify(args[0])

		// Everything after -- is the command to execute
		command := args[1:]
		if len(command) == 0 {
			return fmt.Errorf("no command specified")
		}

		// Check if Hub is enabled
		hubCtx, err := CheckHubAvailabilityForAgent(projectPath, agentName, false)
		if err != nil {
			return err
		}

		if hubCtx != nil {
			return execViaHub(hubCtx, agentName, command)
		}

		return execLocal(agentName, command)
	},
}

func execLocal(agentName string, command []string) error {
	rt := runtime.GetRuntime(projectPath, profile)
	output, err := rt.Exec(context.Background(), agentName, command)
	if err != nil {
		return fmt.Errorf("failed to execute command in agent '%s': %w", agentName, err)
	}
	fmt.Print(output)
	return nil
}

func execViaHub(hubCtx *HubContext, agentName string, command []string) error {
	PrintUsingHub(hubCtx.Endpoint)

	projectID, err := GetProjectID(hubCtx)
	if err != nil {
		return wrapHubError(err)
	}

	timeout := execTimeout
	if timeout <= 0 {
		timeout = 30
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()

	resp, err := hubCtx.Client.ProjectAgents(projectID).Exec(ctx, agentName, command, timeout)
	if err != nil {
		return wrapHubError(fmt.Errorf("failed to execute command in agent '%s': %w", agentName, err))
	}

	fmt.Print(resp.Output)
	if resp.ExitCode != 0 {
		os.Exit(resp.ExitCode)
	}
	return nil
}

func init() {
	execCmd.Flags().IntVarP(&execTimeout, "timeout", "t", 30, "Timeout in seconds for command execution")
	rootCmd.AddCommand(execCmd)
}
