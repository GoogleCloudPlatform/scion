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
	"strings"

	"github.com/ptone/scion-agent/pkg/agent"
	"github.com/ptone/scion-agent/pkg/config"
	"github.com/ptone/scion-agent/pkg/runtime"
	"github.com/spf13/cobra"
)

var msgInterrupt bool
var msgBroadcast bool
var msgAll bool

// messageCmd represents the message command
var messageCmd = &cobra.Command{
	Use:     "message [agent] <message>",
	Aliases: []string{"msg"},
	Short:   "Send a message to an agent's harness",
	Long: `Sends a message to a running agent's harness by enqueuing it into the tmux session.
If --broadcast is used, the agent name can be omitted and the message will be sent to all running agents.`,
	Args:              cobra.MinimumNArgs(1),
	ValidArgsFunction: getAgentNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		var agentName string
		var message string

		if msgBroadcast || msgAll {
			message = strings.Join(args, " ")
		} else {
			if len(args) < 2 {
				return fmt.Errorf("agent name and message are required unless --broadcast is used")
			}
			agentName = args[0]
			message = strings.Join(args[1:], " ")
		}

		ctx := context.Background()

		effectiveProfile := profile
		if !(msgBroadcast || msgAll) && effectiveProfile == "" {
			effectiveProfile = agent.GetSavedProfile(agentName, grovePath)
		}

		rt := runtime.GetRuntime(grovePath, effectiveProfile)
		mgr := agent.NewManager(rt)

		var targets []string
		if msgBroadcast || msgAll {
			filters := map[string]string{
				"scion.agent": "true",
			}

			if !msgAll {
				projectDir, _ := config.GetResolvedProjectDir(grovePath)
				if projectDir != "" {
					filters["scion.grove_path"] = projectDir
					filters["scion.grove"] = config.GetGroveName(projectDir)
				}
			}

			agents, err := mgr.List(ctx, filters)
			if err != nil {
				return err
			}
			for _, a := range agents {
				status := strings.ToLower(a.ContainerStatus)
				if strings.HasPrefix(status, "up") || status == "running" {
					targets = append(targets, a.Name)
				}
			}
		} else {
			targets = []string{agentName}
		}

		if len(targets) == 0 {
			if msgBroadcast || msgAll {
				fmt.Println("No running agents found to broadcast to.")
				return nil
			}
			return fmt.Errorf("agent '%s' not found or not running", agentName)
		}

		for _, target := range targets {
			fmt.Printf("Sending message to agent '%s'...\n", target)
			if err := mgr.Message(ctx, target, message, msgInterrupt); err != nil {
				if msgBroadcast || msgAll {
					fmt.Printf("Warning: failed to send message to agent '%s': %s\n", target, err)
					continue
				}
				return err
			}
		}

		return nil
	},
}

func init() {
	messageCmd.Flags().BoolVarP(&msgInterrupt, "interrupt", "i", false, "Interrupt the harness before sending the message")
	messageCmd.Flags().BoolVarP(&msgBroadcast, "broadcast", "b", false, "Send the message to all running agents in the current grove")
	messageCmd.Flags().BoolVarP(&msgAll, "all", "a", false, "Send the message to all running agents across all groves")
	rootCmd.AddCommand(messageCmd)
}
