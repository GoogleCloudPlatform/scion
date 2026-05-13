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
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/spf13/cobra"
)

var msgInterrupt bool
var msgBroadcast bool
var msgAll bool
var msgIn string
var msgAt string
var msgPlain bool
var msgRaw bool
var msgAttach []string
var msgNotify bool
var msgWake bool

// messageCmd represents the message command
var messageCmd = &cobra.Command{
	Use:     "message [recipient] <message>",
	Aliases: []string{"msg"},
	Short:   "Send a message to an agent or user",
	Long: `Sends a message to a running agent's harness or to a user's inbox.

Recipients:
  <agent-name>       Send to an agent (default, same as agent:<name>)
  agent:<name>       Send to an agent explicitly
  user:<name>        Send to a user's inbox (Hub mode only)
  set[a,b,...]       Send to multiple recipients (Hub mode only)

If --broadcast is used, the recipient can be omitted and the message will be sent to all running agents.

Examples:
  scion message my-agent "Please review the PR"
  scion message user:alice "I need clarification on the auth module"
  scion message "set[agent:reviewer,user:alice,deploy-bot]" "Release v2 is ready"`,
	Args:              cobra.MinimumNArgs(1),
	ValidArgsFunction: getAgentNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		var agentName string
		var userRecipient string
		var setRecipients []messages.SetRecipient
		var message string

		if msgBroadcast || msgAll {
			if len(args) > 0 && messages.IsSetRecipient(args[0]) {
				return fmt.Errorf("set[] recipients cannot be combined with --broadcast or --all")
			}
			message = strings.Join(args, " ")
		} else {
			if len(args) < 2 {
				return fmt.Errorf("recipient and message are required unless --broadcast is used")
			}
			recipient := args[0]
			message = strings.Join(args[1:], " ")

			if messages.IsSetRecipient(recipient) {
				parsed, err := messages.ParseSetRecipient(recipient)
				if err != nil {
					return fmt.Errorf("invalid set recipient: %w", err)
				}
				setRecipients = parsed
			} else if strings.HasPrefix(recipient, "user:") {
				userRecipient = recipient
			} else if strings.Contains(recipient, "@") && !strings.HasPrefix(recipient, "agent:") {
				// Bare email address — treat as user recipient
				userRecipient = "user:" + recipient
			} else {
				// Strip optional "agent:" prefix for backwards compatibility
				agentName = api.Slugify(strings.TrimPrefix(recipient, "agent:"))
			}
		}

		// Validate scheduling flags
		if msgIn != "" && msgAt != "" {
			return fmt.Errorf("--in and --at are mutually exclusive")
		}
		if (msgIn != "" || msgAt != "") && (msgBroadcast || msgAll) {
			return fmt.Errorf("--in/--at cannot be combined with --broadcast or --all")
		}

		// Validate --raw restrictions
		if msgRaw {
			if msgBroadcast || msgAll {
				return fmt.Errorf("--raw cannot be combined with --broadcast or --all")
			}
			if msgPlain {
				return fmt.Errorf("--raw and --plain are mutually exclusive")
			}
			if msgIn != "" || msgAt != "" {
				return fmt.Errorf("--raw cannot be combined with --in or --at")
			}
			if len(msgAttach) > 0 {
				return fmt.Errorf("--raw cannot be combined with --attach")
			}
		}

		// Validate --notify restrictions
		if msgNotify && (msgBroadcast || msgAll) {
			return fmt.Errorf("--notify cannot be combined with --broadcast or --all")
		}

		// Validate user-recipient restrictions
		if userRecipient != "" {
			if msgBroadcast || msgAll {
				return fmt.Errorf("user recipients cannot be combined with --broadcast or --all")
			}
			if msgRaw {
				return fmt.Errorf("--raw cannot be used with user recipients")
			}
			if msgIn != "" || msgAt != "" {
				return fmt.Errorf("--in/--at cannot be used with user recipients")
			}
		}

		// Validate set[] recipient restrictions
		if len(setRecipients) > 0 {
			if msgBroadcast || msgAll {
				return fmt.Errorf("set[] recipients cannot be combined with --broadcast or --all")
			}
			if msgRaw {
				return fmt.Errorf("--raw cannot be used with set[] recipients")
			}
			if msgIn != "" || msgAt != "" {
				return fmt.Errorf("--in/--at cannot be used with set[] recipients")
			}
			if msgNotify {
				return fmt.Errorf("--notify cannot be used with set[] recipients")
			}
		}

		// Validate --wake restrictions
		if msgWake {
			if msgBroadcast || msgAll {
				return fmt.Errorf("--wake cannot be combined with --broadcast or --all")
			}
			if msgIn != "" || msgAt != "" {
				return fmt.Errorf("--wake cannot be combined with --in or --at")
			}
			if msgRaw {
				return fmt.Errorf("--wake cannot be combined with --raw")
			}
			if userRecipient != "" {
				return fmt.Errorf("--wake cannot be used with user recipients")
			}
		}

		// Validate attachments
		if len(msgAttach) > messages.MaxAttachments {
			return fmt.Errorf("too many attachments: %d (max %d)", len(msgAttach), messages.MaxAttachments)
		}

		// Check if Hub should be used
		var hubCtx *HubContext
		var err error
		if len(setRecipients) > 0 {
			// Set recipients: skip sync (multiple recipients, no single agent)
			hubCtx, err = CheckHubAvailabilityWithOptions(projectPath, true)
		} else if userRecipient != "" {
			// User recipient: skip sync (no agent involved)
			hubCtx, err = CheckHubAvailabilityWithOptions(projectPath, true)
		} else if msgAll {
			// Cross-grove operation: skip sync
			hubCtx, err = CheckHubAvailabilityWithOptions(projectPath, true)
		} else if msgBroadcast {
			// Grove-scoped broadcast: no specific agent
			hubCtx, err = CheckHubAvailability(projectPath)
		} else {
			// Single agent: exclude target from sync requirements
			hubCtx, err = CheckHubAvailabilityForAgent(projectPath, agentName, false)
		}
		if err != nil {
			return err
		}

		// Set recipients require Hub mode
		if len(setRecipients) > 0 && hubCtx == nil {
			return fmt.Errorf("set[] recipients require Hub mode (use 'scion hub enable' first)")
		}

		// User recipients require Hub mode
		if userRecipient != "" && hubCtx == nil {
			return fmt.Errorf("sending messages to users requires Hub mode (use 'scion hub enable' first)")
		}

		// Handle scheduled messages
		if msgIn != "" || msgAt != "" {
			if hubCtx == nil {
				return fmt.Errorf("scheduled messages require Hub mode (use 'scion hub enable' first)")
			}
			return scheduleMessageViaHub(hubCtx, agentName, message, msgInterrupt, msgPlain)
		}

		// --notify requires Hub mode
		if msgNotify && hubCtx == nil {
			return fmt.Errorf("--notify requires Hub mode (use 'scion hub enable' first)")
		}

		// Set-targeted messages: fan out to each recipient
		if len(setRecipients) > 0 {
			return sendSetMessageViaHub(hubCtx, setRecipients, message, msgInterrupt)
		}

		// User-targeted messages: route to outbound-message endpoint
		if userRecipient != "" {
			return sendOutboundMessageViaHub(hubCtx, userRecipient, message, msgInterrupt)
		}

		if hubCtx != nil {
			return sendMessageViaHub(hubCtx, agentName, message, msgInterrupt, msgBroadcast, msgAll, msgNotify, msgWake)
		}

		// --wake requires Hub mode
		if msgWake {
			return fmt.Errorf("--wake requires Hub mode (use 'scion hub enable' first)")
		}

		// Local mode — structured messages are only available in Hub mode,
		// so local mode continues to use plain text delivery.
		ctx := context.Background()

		rt := runtime.GetRuntime(projectPath, profile)
		mgr := agent.NewManager(rt)
		defer mgr.Close()

		// Raw mode: send literal bytes via send-keys with no trailing Enter
		if msgRaw {
			fmt.Printf("Sending raw keys to agent '%s'...\n", agentName)
			return mgr.MessageRaw(ctx, agentName, "", message)
		}

		var targets []string
		if msgBroadcast || msgAll {
			filters := map[string]string{
				"scion.agent": "true",
			}

			if !msgAll {
				projectDir, _ := config.GetResolvedProjectDir(projectPath)
				if projectDir != "" {
					filters["scion.project_path"] = projectDir
					filters["scion.project"] = config.GetProjectName(projectDir)
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

		if len(targets) > 1 {
			fmt.Printf("Broadcasting message to %d agents...\n", len(targets))
			var wg sync.WaitGroup
			for _, target := range targets {
				wg.Add(1)
				go func(name string) {
					defer wg.Done()
					if err := mgr.Message(ctx, name, "", message, msgInterrupt); err != nil {
						fmt.Printf("Warning: failed to send message to agent '%s': %s\n", name, err)
						return
					}
					fmt.Printf("Message delivered to agent '%s'.\n", name)
				}(target)
			}
			wg.Wait()
		} else {
			for _, target := range targets {
				fmt.Printf("Sending message to agent '%s'...\n", target)
				if err := mgr.Message(ctx, target, "", message, msgInterrupt); err != nil {
					if msgBroadcast || msgAll {
						fmt.Printf("Warning: failed to send message to agent '%s': %s\n", target, err)
						continue
					}
					return err
				}
			}
		}

		return nil
	},
}

// resolveSenderIdentity determines the sender identity string for structured messages.
// In agent context (SCION_AGENT_NAME set), returns "agent:<name>".
// In user context, queries Hub for the current user and returns "user:<displayName>".
func resolveSenderIdentity(hubCtx *HubContext) string {
	// Agent context
	if agentName := os.Getenv("SCION_AGENT_NAME"); agentName != "" {
		return "agent:" + agentName
	}

	// User context — try to resolve from Hub
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	user, err := hubCtx.Client.Auth().Me(ctx)
	if err == nil && user != nil {
		name := user.DisplayName
		if name == "" {
			name = user.Email
		}
		if name != "" {
			return "user:" + name
		}
	}

	return "user:unknown"
}

// buildStructuredMessage constructs a StructuredMessage from CLI parameters.
func buildStructuredMessage(sender, recipient, message string) *messages.StructuredMessage {
	msg := messages.NewInstruction(sender, recipient, message)
	msg.Plain = msgPlain
	msg.Raw = msgRaw
	msg.Urgent = msgInterrupt
	msg.Broadcasted = msgBroadcast || msgAll
	if len(msgAttach) > 0 {
		msg.Attachments = msgAttach
	}
	return msg
}

func sendMessageViaHub(hubCtx *HubContext, agentName string, message string, interrupt bool, broadcast bool, all bool, notify bool, wake bool) error {
	if !isJSONOutput() {
		PrintUsingHub(hubCtx.Endpoint)
	}

	// Resolve sender identity for structured messages
	sender := resolveSenderIdentity(hubCtx)

	// Grove-scoped broadcast: list running agents, then fan-out individually.
	if broadcast && !all {
		projectID, err := GetProjectID(hubCtx)
		if err != nil {
			return wrapHubError(err)
		}
		agentSvc := hubCtx.Client.ProjectAgents(projectID)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		resp, err := agentSvc.List(ctx, &hubclient.ListAgentsOptions{Phase: "running"})
		if err != nil {
			return wrapHubError(fmt.Errorf("failed to list agents via Hub: %w", err))
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
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()

				msg := buildStructuredMessage(sender, "agent:"+name, message)
				if err := agentSvc.SendStructuredMessage(ctx, name, msg, interrupt, false, false); err != nil {
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

	// Global broadcast (--all): fan-out at client level across groves.
	// Each grove doesn't have a global broadcast endpoint, so we list all
	// running agents and send individually.
	if all {
		agentSvc := hubCtx.Client.Agents()

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		resp, err := agentSvc.List(ctx, &hubclient.ListAgentsOptions{Phase: "running"})
		if err != nil {
			return wrapHubError(fmt.Errorf("failed to list agents via Hub: %w", err))
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
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()

				msg := buildStructuredMessage(sender, "agent:"+name, message)
				if err := agentSvc.SendStructuredMessage(ctx, name, msg, interrupt, false, false); err != nil {
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

	// Single agent: direct message
	projectID, err := GetProjectID(hubCtx)
	if err != nil {
		return wrapHubError(err)
	}
	agentSvc := hubCtx.Client.ProjectAgents(projectID)

	if !isJSONOutput() {
		fmt.Printf("Sending message to agent '%s'...\n", agentName)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	msg := buildStructuredMessage(sender, "agent:"+agentName, message)
	if err := agentSvc.SendStructuredMessage(ctx, agentName, msg, interrupt, notify, wake); err != nil {
		return wrapHubError(fmt.Errorf("failed to send message to agent '%s' via Hub: %w", agentName, err))
	}

	if !isJSONOutput() {
		fmt.Printf("Message sent to agent '%s' via Hub.\n", agentName)
		if notify {
			fmt.Printf("Subscribed to notifications for agent '%s'.\n", agentName)
		}
	}
	return nil
}

func sendOutboundMessageViaHub(hubCtx *HubContext, userRecipient string, message string, urgent bool) error {
	if !isJSONOutput() {
		PrintUsingHub(hubCtx.Endpoint)
	}

	// Determine the sending agent's name. This command is intended for use
	// by agents running inside containers, where SCION_AGENT_NAME is set.
	senderAgent := os.Getenv("SCION_AGENT_NAME")
	if senderAgent == "" {
		return fmt.Errorf("sending messages to users is only supported from within an agent container (SCION_AGENT_NAME not set)")
	}

	projectID, err := GetProjectID(hubCtx)
	if err != nil {
		return wrapHubError(err)
	}
	agentSvc := hubCtx.Client.ProjectAgents(projectID)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	outMsg := &hubclient.OutboundMessageRequest{
		Recipient: userRecipient,
		Msg:       message,
		Type:      "instruction",
		Urgent:    urgent,
	}

	if err := agentSvc.SendOutboundMessage(ctx, senderAgent, outMsg); err != nil {
		return wrapHubError(fmt.Errorf("failed to send message to %s: %w", userRecipient, err))
	}

	if !isJSONOutput() {
		fmt.Printf("Message sent to %s via Hub.\n", userRecipient)
	}
	return nil
}

func sendSetMessageViaHub(hubCtx *HubContext, recipients []messages.SetRecipient, message string, interrupt bool) error {
	if !isJSONOutput() {
		PrintUsingHub(hubCtx.Endpoint)
	}

	sender := resolveSenderIdentity(hubCtx)
	groupID := api.NewUUID()

	projectID, err := GetProjectID(hubCtx)
	if err != nil {
		return wrapHubError(err)
	}
	agentSvc := hubCtx.Client.ProjectAgents(projectID)

	if !isJSONOutput() {
		fmt.Printf("Sending message to %d recipients...\n", len(recipients))
	}

	type recipientResult struct {
		Recipient string `json:"recipient"`
		Status    string `json:"status"`
		Error     string `json:"error,omitempty"`
	}

	results := make([]recipientResult, len(recipients))
	var wg sync.WaitGroup

	for i, r := range recipients {
		wg.Add(1)
		go func(idx int, recip messages.SetRecipient) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			recipStr := recip.String()
			switch recip.Kind {
			case messages.RecipientAgent:
				slug := api.Slugify(recip.Name)
				msg := buildStructuredMessage(sender, "agent:"+slug, message)
				msg.Metadata = map[string]string{"group_id": groupID}
				if err := agentSvc.SendStructuredMessage(ctx, slug, msg, interrupt, false, false); err != nil {
					results[idx] = recipientResult{Recipient: recipStr, Status: "failed", Error: err.Error()}
					if !isJSONOutput() {
						fmt.Printf("  Failed: %s: %s\n", recipStr, err)
					}
					return
				}
				results[idx] = recipientResult{Recipient: recipStr, Status: "delivered"}
				if !isJSONOutput() {
					fmt.Printf("  Delivered: %s\n", recipStr)
				}

			case messages.RecipientUser:
				senderAgent := os.Getenv("SCION_AGENT_NAME")
				if senderAgent == "" {
					results[idx] = recipientResult{Recipient: recipStr, Status: "failed", Error: "sending to users requires agent context (SCION_AGENT_NAME not set)"}
					if !isJSONOutput() {
						fmt.Printf("  Failed: %s: agent context required\n", recipStr)
					}
					return
				}
				userRecip := recipStr
				if !strings.HasPrefix(userRecip, "user:") {
					userRecip = "user:" + recip.Name
				}
				outMsg := &hubclient.OutboundMessageRequest{
					Recipient: userRecip,
					Msg:       message,
					Type:      "instruction",
					Urgent:    interrupt,
				}
				if err := agentSvc.SendOutboundMessage(ctx, senderAgent, outMsg); err != nil {
					results[idx] = recipientResult{Recipient: recipStr, Status: "failed", Error: err.Error()}
					if !isJSONOutput() {
						fmt.Printf("  Failed: %s: %s\n", recipStr, err)
					}
					return
				}
				results[idx] = recipientResult{Recipient: recipStr, Status: "delivered"}
				if !isJSONOutput() {
					fmt.Printf("  Delivered: %s\n", recipStr)
				}
			}
		}(i, r)
	}
	wg.Wait()

	delivered := 0
	for _, r := range results {
		if r.Status == "delivered" {
			delivered++
		}
	}

	if !isJSONOutput() {
		fmt.Printf("Set delivery complete: %d/%d delivered.\n", delivered, len(recipients))
	}

	if delivered == 0 {
		return fmt.Errorf("set delivery failed: 0/%d recipients received the message", len(recipients))
	}
	if delivered < len(recipients) {
		return fmt.Errorf("set delivery partially failed: %d/%d delivered", delivered, len(recipients))
	}
	return nil
}

func scheduleMessageViaHub(hubCtx *HubContext, agentName string, message string, interrupt bool, plain bool) error {
	if !isJSONOutput() {
		PrintUsingHub(hubCtx.Endpoint)
	}

	projectID, err := GetProjectID(hubCtx)
	if err != nil {
		return wrapHubError(err)
	}

	req := &hubclient.CreateScheduledEventRequest{
		EventType: "message",
		AgentName: agentName,
		Message:   message,
		Interrupt: interrupt,
		Plain:     plain,
	}

	if msgIn != "" {
		req.FireIn = msgIn
	} else {
		req.FireAt = msgAt
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	evt, err := hubCtx.Client.ScheduledEvents(projectID).Create(ctx, req)
	if err != nil {
		return wrapHubError(fmt.Errorf("failed to schedule message: %w", err))
	}

	if !isJSONOutput() {
		fmt.Printf("Message to agent '%s' scheduled for %s\n", agentName, evt.FireAt.Format(time.RFC3339))
	}

	return nil
}

func init() {
	messageCmd.Flags().BoolVarP(&msgInterrupt, "interrupt", "i", false, "Interrupt the harness before sending the message")
	messageCmd.Flags().BoolVarP(&msgBroadcast, "broadcast", "b", false, "Send the message to all running agents in the current grove")
	messageCmd.Flags().BoolVarP(&msgAll, "all", "a", false, "Send the message to all running agents across all groves")
	messageCmd.Flags().StringVar(&msgIn, "in", "", "Schedule message delivery after a duration (e.g. 30m, 1h)")
	messageCmd.Flags().StringVar(&msgAt, "at", "", "Schedule message delivery at an absolute time (ISO 8601, e.g. 2026-02-28T14:00:00Z)")
	messageCmd.Flags().BoolVar(&msgPlain, "plain", false, "Mark for plain-text delivery (message still flows as structured JSON internally)")
	messageCmd.Flags().BoolVar(&msgRaw, "raw", false, "Send literal bytes via tmux send-keys with no trailing Enter (supports control keys like arrows and Escape)")
	messageCmd.Flags().StringArrayVar(&msgAttach, "attach", nil, "Attach file path(s), repeatable")
	messageCmd.Flags().BoolVar(&msgNotify, "notify", false, "Subscribe to notifications for the target agent (completed, waiting for input, etc.)")
	messageCmd.Flags().BoolVarP(&msgWake, "wake", "w", false, "Resume a suspended agent before delivering the message")
	rootCmd.AddCommand(messageCmd)
}
