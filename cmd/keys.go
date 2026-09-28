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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
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

In Hub mode, keys are delivered to the agent's terminal the same way a
local agent's are — through the hub. When run by an agent, this only
works within the agent's own project; cross-project targets are refused.
A human operator using --project can still target other projects.

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
		// Resolve the agent exactly as `scion message` does: strip the
		// optional "agent:" prefix and slugify.
		agentName := api.Slugify(strings.TrimPrefix(args[0], "agent:"))
		keystrokes := strings.Join(args[1:], " ")

		hubCtx, err := CheckHubAvailabilityForAgent(projectPath, agentName, true)
		if err != nil {
			return err
		}

		if hubCtx != nil {
			return sendKeysViaHub(hubCtx, agentName, keystrokes)
		}

		// Local mode now resolves the agent name the same way `scion
		// message` does; otherwise unchanged.
		ctx := context.Background()

		rt := runtime.GetRuntime(projectPath, profile)
		mgr := agent.NewManager(rt)
		defer mgr.Close()

		fmt.Printf("Sending raw keys to agent '%s'...\n", agentName)
		return mgr.MessageRaw(ctx, agentName, "", keystrokes)
	},
}

// sendKeysViaHub delivers keystrokes to a hub-managed agent by reusing the
// same StructuredMessage-with-Raw=true path `scion message --raw` uses: it
// resolves the sender identity and project the way `scion message` does,
// builds the structured message via the shared helper, and sends it through
// the normal messaging-authorization gate. The broker honours Raw the same
// way regardless of which command set it.
func sendKeysViaHub(hubCtx *HubContext, agentName, keystrokes string) error {
	if !isJSONOutput() {
		PrintUsingHub(hubCtx.Endpoint)
	}

	sender := resolveSenderIdentity(hubCtx)

	projectID, err := GetProjectID(hubCtx)
	if err != nil {
		return wrapHubError(err)
	}
	agentSvc := hubCtx.Client.ProjectAgents(projectID)

	if !isJSONOutput() {
		fmt.Printf("Sending raw keys to agent '%s'...\n", agentName)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	msg := buildStructuredMessage(sender, "agent:"+agentName, keystrokes, nil, true, false, false)
	if err := messaging.ValidateLegacyMessage(msg); err != nil {
		return fmt.Errorf("message validation failed: %w", err)
	}
	if _, err := agentSvc.SendStructuredMessage(ctx, agentName, msg, false, false, false); err != nil {
		return wrapHubError(fmt.Errorf("failed to send keys to agent '%s' via Hub: %w", agentName, err))
	}

	if !isJSONOutput() {
		fmt.Printf("Keys delivered to agent '%s'.\n", agentName)
	}
	return nil
}

func init() {
	rootCmd.AddCommand(keysCmd)
}
