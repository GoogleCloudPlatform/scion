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

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/spf13/cobra"
)

// keysCmd represents the keys command
var keysCmd = &cobra.Command{
	Use:   "keys <agent-name> <keystrokes>",
	Short: "Send raw keystrokes to an agent's terminal",
	Long: `Sends literal bytes to an agent's terminal via tmux send-keys,
with no trailing Enter. Supports control keys like arrows, Escape, etc.

This is useful for interacting with interactive TUI applications running
inside an agent's terminal session.

Examples:
  scion keys my-agent "Escape"
  scion keys my-agent "C-c"
  scion keys my-agent "Up Up Enter"`,
	Args: cobra.ExactArgs(2),
	ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) == 0 {
			return getAgentNames(cmd, args, toComplete)
		}
		return nil, cobra.ShellCompDirectiveNoFileComp
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		agentName := args[0]
		keystrokes := strings.Join(args[1:], " ")

		ctx := context.Background()

		rt := runtime.GetRuntime(projectPath, profile)
		mgr := agent.NewManager(rt)
		defer mgr.Close()

		fmt.Printf("Sending raw keys to agent '%s'...\n", agentName)
		return mgr.MessageRaw(ctx, agentName, "", keystrokes)
	},
}

func init() {
	rootCmd.AddCommand(keysCmd)
}
