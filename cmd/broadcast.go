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
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/spf13/cobra"
)

var (
	bcastAll        bool
	bcastInterrupt  bool
	bcastVisibility string
)

// broadcastCmd represents the broadcast command
var broadcastCmd = &cobra.Command{
	Use:   "broadcast <message>",
	Short: "Send a message to all running agents",
	Long: `Sends a message to all running agents in the current project (default)
or across all projects (with --all).

Project-scoped broadcast uses the Hub's BroadcastMessage endpoint.
Global broadcast (--all) performs client-side fan-out to all running agents.

Examples:
  scion broadcast "Deploy is starting, pause your work"
  scion broadcast --all "System maintenance in 10 minutes"
  scion broadcast --interrupt "Stop immediately and report status"`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		// Validate --visibility if provided.
		if bcastVisibility != "" {
			switch bcastVisibility {
			case "normal", "verbose", "full":
				// valid
			default:
				return fmt.Errorf("invalid --visibility value %q: must be one of: normal, verbose, full", bcastVisibility)
			}
		}

		message := strings.Join(args, " ")

		// Check Hub availability
		var hubCtx *HubContext
		var err error
		if bcastAll {
			hubCtx, err = CheckHubAvailabilityWithOptions(projectPath, true)
		} else {
			hubCtx, err = CheckHubAvailability(projectPath)
		}
		if err != nil {
			return err
		}

		if hubCtx != nil {
			return broadcastViaHub(hubCtx, message)
		}

		// Local mode fallback
		return broadcastLocal(message)
	},
}

func broadcastViaHub(hubCtx *HubContext, message string) error {
	if !isJSONOutput() {
		PrintUsingHub(hubCtx.Endpoint)
	}

	sender := resolveSenderIdentity(hubCtx)

	// Project-scoped broadcast
	if !bcastAll {
		projectID, err := GetProjectID(hubCtx)
		if err != nil {
			return wrapHubError(err)
		}
		agentSvc := hubCtx.Client.ProjectAgents(projectID)

		ctx, cancel := context.WithTimeout(context.Background(), broadcastHubTimeout)
		defer cancel()

		msg := buildBroadcastMessage(sender, message)
		if err := messaging.ValidateLegacyMessage(msg); err != nil {
			return fmt.Errorf("message validation failed: %w", err)
		}
		bcastResp, err := agentSvc.BroadcastMessage(ctx, msg, bcastInterrupt)
		if err != nil {
			return wrapHubError(fmt.Errorf("failed to broadcast message via Hub: %w", err))
		}

		if !isJSONOutput() {
			printBroadcastAccepted(bcastResp)
		}
		return nil
	}

	// Global broadcast (--all): fan-out at client level across projects.
	agentSvc := hubCtx.Client.Agents()

	ctx, cancel := context.WithTimeout(context.Background(), broadcastHubTimeout)
	defer cancel()

	resp, err := agentSvc.List(ctx, &hubclient.ListAgentsOptions{Phase: "running"})
	if err != nil {
		return wrapHubError(fmt.Errorf("failed to list agents via Hub: %w", err))
	}
	if resp == nil {
		return fmt.Errorf("failed to list agents via Hub: server returned empty response")
	}

	if len(resp.Agents) == 0 {
		fmt.Println("No running agents found to broadcast to.")
		return nil
	}

	if !isJSONOutput() {
		fmt.Printf("Broadcasting message to %d agents...\n", len(resp.Agents))
	}

	var wg sync.WaitGroup
	for _, a := range resp.Agents {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), broadcastHubSendTimeout)
			defer cancel()

			msg := buildBroadcastMessage(sender, message)
			msg.Recipient = "agent:" + name
			if _, err := agentSvc.SendStructuredMessage(ctx, name, msg, bcastInterrupt, false, false); err != nil {
				fmt.Printf("Warning: failed to send message to agent '%s' via Hub: %s\n", name, err)
				return
			}
			if !isJSONOutput() {
				fmt.Printf("Message delivered to agent '%s' via Hub.\n", name)
			}
		}(a.Name)
	}
	wg.Wait()
	return nil
}

// Timeouts for broadcast operations. Separate deadlines so that when a timeout
// fires it names exactly one operation, not "something was slow."
const (
	broadcastHubTimeout     = 30 * time.Second // project-scoped Hub broadcast or global Hub list
	broadcastHubSendTimeout = 30 * time.Second // per-agent send in global Hub fan-out
	broadcastListTimeout    = 15 * time.Second // local agent list
	broadcastMessageTimeout = 30 * time.Second // local per-agent message; generous: --interrupt adds work
)

func broadcastLocal(message string) error {
	rt := runtime.GetRuntime(projectPath, profile)
	mgr := agent.NewManager(rt)
	defer mgr.Close()

	filters := map[string]string{
		"scion.agent": "true",
	}

	if !bcastAll {
		projectDir, _ := config.GetResolvedProjectDir(projectPath)
		if projectDir != "" {
			filters["scion.project_path"] = projectDir
			filters["scion.project"] = config.GetProjectName(projectDir)
		}
	}

	listCtx, listCancel := context.WithTimeout(context.Background(), broadcastListTimeout)
	defer listCancel()

	agents, err := mgr.List(listCtx, filters)
	if err != nil {
		return err
	}

	var targets []string
	for _, a := range agents {
		if a.Phase == string(state.PhaseRunning) {
			targets = append(targets, a.Name)
		}
	}

	if len(targets) == 0 {
		fmt.Println("No running agents found to broadcast to.")
		return nil
	}

	fmt.Printf("Broadcasting message to %d agents...\n", len(targets))
	var wg sync.WaitGroup
	for _, target := range targets {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			sendCtx, sendCancel := context.WithTimeout(context.Background(), broadcastMessageTimeout)
			defer sendCancel()
			if err := mgr.Message(sendCtx, name, "", message, bcastInterrupt); err != nil {
				fmt.Printf("Warning: failed to send message to agent '%s': %s\n", name, err)
				return
			}
			fmt.Printf("Message delivered to agent '%s'.\n", name)
		}(target)
	}
	wg.Wait()
	return nil
}

// buildBroadcastMessage constructs a StructuredMessage for broadcast.
func buildBroadcastMessage(sender, message string) *messages.StructuredMessage {
	msg := messages.NewInstruction(sender, "", message)
	msg.Broadcasted = true
	msg.Urgent = bcastInterrupt
	if bcastVisibility != "" {
		msg.Visibility = bcastVisibility
	}
	return msg
}

func init() {
	broadcastCmd.Flags().BoolVarP(&bcastAll, "all", "a", false, "Send to all running agents across all projects (global broadcast)")
	broadcastCmd.Flags().BoolVarP(&bcastInterrupt, "interrupt", "i", false, "Interrupt the harness before sending the message")
	broadcastCmd.Flags().StringVar(&bcastVisibility, "visibility", "", "Message visibility: normal, verbose, or full")
	rootCmd.AddCommand(broadcastCmd)
}
