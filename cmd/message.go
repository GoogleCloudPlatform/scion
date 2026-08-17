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
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
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
var msgChannel string
var msgThreadID string
var msgCC []string

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
  group[a,b,...]     Send to multiple recipients (Hub mode only)

If --broadcast is used, the recipient can be omitted and the message will be sent to all running agents.

Examples:
  scion message my-agent "Please review the PR"
  scion message user:alice "I need clarification on the auth module"
  scion message "group[agent:reviewer,user:alice,deploy-bot]" "Release v2 is ready"`,
	Args:              cobra.MinimumNArgs(1),
	ValidArgsFunction: getAgentNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		var agentName string
		var userRecipient string
		var groupRecipients []messages.GroupRecipient
		var message string

		if msgBroadcast || msgAll {
			if len(args) > 0 && messages.IsGroupRecipient(args[0]) {
				return fmt.Errorf("group[] recipients cannot be combined with --broadcast or --all")
			}
			message = strings.Join(args, " ")
		} else {
			if len(args) < 2 {
				return fmt.Errorf("recipient and message are required unless --broadcast is used")
			}
			recipient := args[0]
			message = strings.Join(args[1:], " ")

			if messages.IsGroupRecipient(recipient) {
				parsed, err := messages.ParseGroupRecipient(recipient)
				if err != nil {
					return fmt.Errorf("invalid group recipient: %w", err)
				}
				groupRecipients = parsed
			} else if strings.HasPrefix(recipient, "user:") {
				userRecipient = recipient
			} else if strings.Contains(recipient, "@") && !strings.HasPrefix(recipient, "agent:") {
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

		// Validate --thread-id requires --channel
		if msgThreadID != "" && msgChannel == "" {
			return fmt.Errorf("--thread-id requires --channel to be set")
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

		// Validate --cc restrictions: parse first so empty-string values
		// (e.g. --cc "") are handled correctly instead of triggering
		// false-positive validation errors.
		parsedCC := parseCCFlag(msgCC)
		if len(parsedCC) > 0 {
			if msgBroadcast || msgAll {
				return fmt.Errorf("--cc cannot be combined with --broadcast or --all")
			}
			if msgRaw {
				return fmt.Errorf("--cc cannot be combined with --raw")
			}
			if msgIn != "" || msgAt != "" {
				return fmt.Errorf("--cc cannot be combined with --in or --at")
			}
			if userRecipient != "" {
				return fmt.Errorf("--cc cannot be used with user recipients")
			}
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

		// Validate group recipient restrictions
		if len(groupRecipients) > 0 {
			if msgBroadcast || msgAll {
				return fmt.Errorf("group[] recipients cannot be combined with --broadcast or --all")
			}
			if msgRaw {
				return fmt.Errorf("--raw cannot be used with group[] recipients")
			}
			if msgIn != "" || msgAt != "" {
				return fmt.Errorf("--in/--at cannot be used with group[] recipients")
			}
			if msgNotify {
				return fmt.Errorf("--notify cannot be used with group[] recipients")
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
		if len(msgAttach) > 0 && (msgIn != "" || msgAt != "") {
			return fmt.Errorf("--attach cannot be combined with --in or --at")
		}

		// Validate attachment file paths exist
		for _, p := range msgAttach {
			resolved := resolveAttachmentPath(p)
			if resolved == "" {
				return fmt.Errorf("attachment %q: path is outside allowed roots (/workspace, /scion-volumes)", p)
			}
			info, err := os.Stat(resolved)
			if err != nil {
				return fmt.Errorf("attachment %q: %w", p, err)
			}
			if !info.Mode().IsRegular() {
				if info.IsDir() {
					return fmt.Errorf("attachment %q: is a directory, not a regular file", p)
				}
				return fmt.Errorf("attachment %q: is not a regular file", p)
			}
		}

		// Check if Hub should be used
		var hubCtx *HubContext
		var err error
		if len(groupRecipients) > 0 {
			// Group recipients: skip sync (multiple recipients, no single agent)
			hubCtx, err = CheckHubAvailabilityWithOptions(projectPath, true)
		} else if userRecipient != "" {
			// User recipient: skip sync (no agent involved)
			hubCtx, err = CheckHubAvailabilityWithOptions(projectPath, true)
		} else if msgAll {
			// Cross-project operation: skip sync
			hubCtx, err = CheckHubAvailabilityWithOptions(projectPath, true)
		} else if msgBroadcast {
			// Grove-scoped broadcast: no specific agent
			hubCtx, err = CheckHubAvailability(projectPath)
		} else {
			// Single agent: exclude target from sync requirements
			hubCtx, err = CheckHubAvailabilityForAgent(projectPath, agentName, true)
		}
		if err != nil {
			return err
		}

		// Group recipients require Hub mode
		if len(groupRecipients) > 0 && hubCtx == nil {
			return fmt.Errorf("group[] recipients require Hub mode (use 'scion hub enable' first)")
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

		// --cc requires Hub mode
		if len(parsedCC) > 0 && hubCtx == nil {
			return fmt.Errorf("--cc requires Hub mode (use 'scion hub enable' first)")
		}

		// Stage attachments to shared volume (after Hub mode confirmed)
		if len(msgAttach) > 0 && hubCtx != nil {
			staged, err := stageAttachments(msgAttach)
			if err != nil {
				return fmt.Errorf("attachment staging failed: %w", err)
			}
			msgAttach = staged
		}

		// Group-targeted messages: fan out to each recipient
		if len(groupRecipients) > 0 {
			return sendGroupMessageViaHub(hubCtx, groupRecipients, message, msgInterrupt)
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

		// --attach requires Hub mode: attachments are delivered through Hub
		// storage, while local mode writes plain text to the agent terminal
		// and cannot transfer files.
		if len(msgAttach) > 0 {
			return fmt.Errorf("--attach requires Hub mode (use 'scion hub enable' first); in local mode, include the file contents in the message text")
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
	msg.Channel = msgChannel
	msg.ThreadID = msgThreadID
	return msg
}

func sendMessageViaHub(hubCtx *HubContext, agentName string, message string, interrupt bool, broadcast bool, all bool, notify bool, wake bool) error {
	if !isJSONOutput() {
		PrintUsingHub(hubCtx.Endpoint)
	}

	// Resolve sender identity for structured messages
	sender := resolveSenderIdentity(hubCtx)

	// Validate --channel against registered channels
	if msgChannel != "" {
		if err := validateChannel(hubCtx, msgChannel); err != nil {
			return err
		}
	}

	// Grove-scoped broadcast: send via Hub broadcast endpoint.
	if broadcast && !all {
		projectID, err := GetProjectID(hubCtx)
		if err != nil {
			return wrapHubError(err)
		}
		agentSvc := hubCtx.Client.ProjectAgents(projectID)

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		msg := buildStructuredMessage(sender, "", message)
		msg.Broadcasted = true
		bcastResp, err := agentSvc.BroadcastMessage(ctx, msg, interrupt)
		if err != nil {
			return wrapHubError(fmt.Errorf("failed to broadcast message via Hub: %w", err))
		}

		if !isJSONOutput() {
			printBroadcastAccepted(bcastResp)
		}
		return nil
	}

	// Global broadcast (--all): fan-out at client level across projects.
	// Each project doesn't have a global broadcast endpoint, so we list all
	// running agents and send individually.
	// TODO: upgrade to P3 model (targeting breakdown, DELIVERY_FAILED notifications)
	// once a global broadcast endpoint exists.
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
				if _, err := agentSvc.SendStructuredMessage(ctx, name, msg, interrupt, false, false); err != nil {
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
	if _, err := agentSvc.SendStructuredMessage(ctx, agentName, msg, interrupt, notify, wake); err != nil {
		return wrapHubError(fmt.Errorf("failed to send message to agent '%s' via Hub: %w", agentName, err))
	}

	if !isJSONOutput() {
		fmt.Printf("Message delivered to agent '%s'.\n", agentName)
		if notify {
			fmt.Printf("Subscribed to notifications for agent '%s'.\n", agentName)
		}
	}

	// @mention and --cc fan-out: send TypeMention messages to mentioned agents
	var mentionNames []string
	// Parse @mentions from message body
	mentionNames = append(mentionNames, extractMentions(message)...)
	// Parse --cc flag
	mentionNames = append(mentionNames, parseCCFlag(msgCC)...)
	if len(mentionNames) > 0 {
		sendMentionMessages(hubCtx, sender, "agent:"+agentName, message, mentionNames, agentSvc)
	}

	return nil
}

func printBroadcastAccepted(resp *hubclient.BroadcastResponse) {
	if resp == nil {
		fmt.Println("Broadcast accepted.")
		return
	}
	if resp.Targeted == 0 {
		if resp.Skipped > 0 {
			fmt.Printf("No running agents to broadcast to (%d agents skipped).\n", resp.Skipped)
		} else {
			fmt.Println("No running agents found to broadcast to.")
		}
		return
	}
	if resp.Skipped > 0 {
		phases := make([]string, 0, len(resp.SkippedBreakdown))
		for phase := range resp.SkippedBreakdown {
			phases = append(phases, phase)
		}
		sort.Strings(phases)
		parts := make([]string, 0, len(phases))
		for _, phase := range phases {
			parts = append(parts, fmt.Sprintf("%d %s", resp.SkippedBreakdown[phase], phase))
		}
		fmt.Printf("Broadcast accepted (%d running agents targeted, %d skipped: %s).\n",
			resp.Targeted, resp.Skipped, strings.Join(parts, ", "))
	} else {
		fmt.Printf("Broadcast accepted (%d running agents targeted).\n", resp.Targeted)
	}
}

func sendOutboundMessageViaHub(hubCtx *HubContext, userRecipient string, message string, urgent bool) error {
	if !isJSONOutput() {
		PrintUsingHub(hubCtx.Endpoint)
	}

	// Validate --channel against registered channels
	if msgChannel != "" {
		if err := validateChannel(hubCtx, msgChannel); err != nil {
			return err
		}
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
		Recipient:   userRecipient,
		Msg:         message,
		Type:        "instruction",
		Urgent:      urgent,
		Attachments: msgAttach,
		Channel:     msgChannel,
		ThreadID:    msgThreadID,
	}

	if err := agentSvc.SendOutboundMessage(ctx, senderAgent, outMsg); err != nil {
		return wrapHubError(fmt.Errorf("failed to send message to %s: %w", userRecipient, err))
	}

	if !isJSONOutput() {
		fmt.Printf("Message sent to %s via Hub.\n", userRecipient)
	}
	return nil
}

func sendGroupMessageViaHub(hubCtx *HubContext, recipients []messages.GroupRecipient, message string, interrupt bool) error {
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

	// Build the recipients string once before the fan-out loop.
	recipientStrs := make([]string, len(recipients))
	for i, r := range recipients {
		recipientStrs[i] = r.String()
	}
	recipientsStr := messages.FormatGroupRecipients(sender, recipientStrs)

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
		go func(idx int, recip messages.GroupRecipient) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			recipStr := recip.String()
			switch recip.Kind {
			case messages.RecipientAgent:
				slug := api.Slugify(recip.Name)
				msg := buildStructuredMessage(sender, "agent:"+slug, message)
				msg.Type = messages.TypeGroupSet
				msg.Recipients = recipientsStr
				msg.Metadata = map[string]string{"group_id": groupID}
				if _, err := agentSvc.SendStructuredMessage(ctx, slug, msg, interrupt, false, false); err != nil {
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
					Recipient:   userRecip,
					Msg:         message,
					Type:        messages.TypeGroupSet,
					Urgent:      interrupt,
					Attachments: msgAttach,
					Channel:     msgChannel,
					ThreadID:    msgThreadID,
					Metadata:    map[string]string{"recipients": recipientsStr, "group_id": groupID},
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
		fmt.Printf("Group delivery complete: %d/%d delivered.\n", delivered, len(recipients))
	}

	// @mention and --cc fan-out for group messages: mentioned agents that are
	// not already group recipients receive a TypeMention notification.
	// This runs regardless of partial delivery — mention recipients are
	// independent of the group.
	var mentionNames []string
	mentionNames = append(mentionNames, extractMentions(message)...)
	mentionNames = append(mentionNames, parseCCFlag(msgCC)...)
	if len(mentionNames) > 0 {
		// Build a mention source that reflects the group
		recipientStrs := make([]string, len(recipients))
		for i, r := range recipients {
			recipientStrs[i] = r.String()
		}
		mentionSource := "group[" + strings.Join(recipientStrs, ",") + "]"

		// Collect group recipient slugs to exclude from mentions
		groupSlugs := make(map[string]bool)
		for _, r := range recipients {
			if r.Kind == messages.RecipientAgent {
				groupSlugs[api.Slugify(r.Name)] = true
			}
		}

		// Filter out names already in the group
		var filtered []string
		for _, name := range mentionNames {
			if !groupSlugs[api.Slugify(name)] {
				filtered = append(filtered, name)
			}
		}

		if len(filtered) > 0 {
			sendMentionMessages(hubCtx, sender, mentionSource, message, filtered, agentSvc)
		}
	}

	if delivered == 0 {
		return fmt.Errorf("group delivery failed: 0/%d recipients received the message", len(recipients))
	}
	if delivered < len(recipients) {
		return fmt.Errorf("group delivery partially failed: %d/%d delivered", delivered, len(recipients))
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

var messageChannelsCmd = &cobra.Command{
	Use:   "channels",
	Short: "List available message channels",
	Long:  "Lists the registered message broker channels that can be targeted with --channel.",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		hubCtx, err := CheckHubAvailabilityWithOptions(projectPath, true)
		if err != nil {
			return err
		}
		if hubCtx == nil {
			return fmt.Errorf("listing message channels requires Hub mode (use 'scion hub enable' first)")
		}
		if !isJSONOutput() {
			PrintUsingHub(hubCtx.Endpoint)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		channels, err := hubCtx.Client.Messages().ListChannels(ctx)
		if err != nil {
			return wrapHubError(err)
		}

		if isJSONOutput() {
			return outputJSON(channels)
		}

		if len(channels) == 0 {
			fmt.Println("No message channels registered.")
			return nil
		}

		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "NAME\tSTATUS\tTYPE")
		for _, ch := range channels {
			chType := "broker"
			if ch.Observer {
				chType = "observer"
			}
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", ch.Name, ch.Status, chType)
		}
		return tw.Flush()
	},
}

// validateChannel checks that the given channel name is registered with the Hub.
func validateChannel(hubCtx *HubContext, channel string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	channels, err := hubCtx.Client.Messages().ListChannels(ctx)
	if err != nil {
		return wrapHubError(fmt.Errorf("failed to list channels: %w", err))
	}

	for _, ch := range channels {
		if ch.Name == channel {
			return nil
		}
	}

	available := make([]string, len(channels))
	for i, ch := range channels {
		available[i] = ch.Name
	}

	if len(available) == 0 {
		return fmt.Errorf("channel %q is not registered; no channels are currently available", channel)
	}
	return fmt.Errorf("channel %q is not registered; available channels: %s", channel, strings.Join(available, ", "))
}

// extractMentions delegates to the shared messages.ExtractMentions.
func extractMentions(text string) []string {
	return messages.ExtractMentions(text)
}

// parseCCFlag delegates to the shared messages.ParseCCFlags. The --cc flag is
// repeatable and each occurrence may itself be a comma-separated list.
func parseCCFlag(cc []string) []string {
	return messages.ParseCCFlags(cc)
}

// maxMentionRecipients is an alias for the shared constant.
const maxMentionRecipients = messages.MaxMentionRecipients

// sendMentionMessages resolves @mentions and --cc names against project agents
// and sends TypeMention messages to each resolved agent. The primary recipient
// is excluded from mentions. Unresolved names produce stderr warnings but do
// not fail the primary send.
func sendMentionMessages(hubCtx *HubContext, sender, primaryRecipient, messageText string, mentionNames []string, agentSvc hubclient.AgentService) {
	if len(mentionNames) == 0 {
		return
	}

	// List project agents for resolution
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := agentSvc.List(ctx, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not list project agents for @mention resolution: %s\n", err)
		return
	}
	if resp == nil {
		return
	}

	// Build lookup map of known agents (slug -> slug with original case)
	knownAgents := make(map[string]string, len(resp.Agents))
	for _, a := range resp.Agents {
		knownAgents[strings.ToLower(a.Name)] = a.Name
	}

	// Determine the primary recipient's slug for dedup
	primarySlug := strings.ToLower(strings.TrimPrefix(primaryRecipient, "agent:"))

	// Resolve mentions and deduplicate
	var resolved []string
	seen := make(map[string]bool)
	seen[primarySlug] = true // skip primary recipient

	for _, name := range mentionNames {
		if len(resolved) >= maxMentionRecipients {
			fmt.Fprintf(os.Stderr, "Warning: too many @mentions; only the first %d will receive mention notifications\n", maxMentionRecipients)
			break
		}
		lower := strings.ToLower(name)
		if seen[lower] {
			continue
		}
		seen[lower] = true

		slug, ok := knownAgents[lower]
		if !ok {
			fmt.Fprintf(os.Stderr, "Warning: @%s does not match any agent in this project; skipping mention\n", name)
			continue
		}
		resolved = append(resolved, slug)
	}

	if len(resolved) == 0 {
		return
	}

	// Send TypeMention to each resolved agent
	var wg sync.WaitGroup
	for _, slug := range resolved {
		wg.Add(1)
		go func(agentSlug string) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			mentionMsg := messages.NewMention(sender, "agent:"+agentSlug, messageText, primaryRecipient)
			if _, err := agentSvc.SendStructuredMessage(ctx, agentSlug, mentionMsg, false, false, false); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to send mention to @%s: %s\n", agentSlug, err)
				return
			}
			if !isJSONOutput() {
				fmt.Fprintf(os.Stderr, "Mention notification sent to @%s.\n", agentSlug)
			}
		}(slug)
	}
	wg.Wait()
}

func init() {
	messageCmd.Flags().BoolVarP(&msgInterrupt, "interrupt", "i", false, "Interrupt the harness before sending the message")
	messageCmd.Flags().BoolVarP(&msgBroadcast, "broadcast", "b", false, "Send the message to all running agents in the current project")
	messageCmd.Flags().BoolVarP(&msgAll, "all", "a", false, "Send the message to all running agents across all projects")
	messageCmd.Flags().StringVar(&msgIn, "in", "", "Schedule message delivery after a duration (e.g. 30m, 1h)")
	messageCmd.Flags().StringVar(&msgAt, "at", "", "Schedule message delivery at an absolute time (ISO 8601, e.g. 2026-02-28T14:00:00Z)")
	messageCmd.Flags().BoolVar(&msgPlain, "plain", false, "Mark for plain-text delivery (message still flows as structured JSON internally)")
	messageCmd.Flags().BoolVar(&msgRaw, "raw", false, "Send literal bytes via tmux send-keys with no trailing Enter (supports control keys like arrows and Escape)")
	messageCmd.Flags().StringArrayVar(&msgAttach, "attach", nil, "Attach file path(s), repeatable; use paths under /workspace or /scion-volumes (bare relative paths resolve to /workspace). Absolute paths outside these roots are silently dropped on delivery.")
	messageCmd.Flags().BoolVar(&msgNotify, "notify", false, "Subscribe to notifications for the target agent (completed, waiting for input, etc.)")
	messageCmd.Flags().BoolVarP(&msgWake, "wake", "w", false, "Resume a suspended agent before delivering the message")
	messageCmd.Flags().StringVar(&msgChannel, "channel", "", "Target a specific message channel (e.g. telegram, gchat, web)")
	messageCmd.Flags().StringVar(&msgThreadID, "thread-id", "", "Target a specific thread within the channel")
	messageCmd.Flags().StringArrayVar(&msgCC, "cc", nil, "CC an additional agent (repeatable; also accepts a comma-separated list); each receives a mention notification")
	messageCmd.AddCommand(messageChannelsCmd)
	rootCmd.AddCommand(messageCmd)
}

// resolveAttachmentPath resolves a relative or absolute attachment path.
// Relative paths are resolved relative to /workspace. Absolute paths outside
// /workspace and /scion-volumes are filtered out with a warning. Returns ""
// for filtered paths.
func resolveAttachmentPath(p string) string {
	if !filepath.IsAbs(p) {
		p = filepath.Join("/workspace", p)
	}

	// Resolve symlinks to prevent directory traversal via symlinks
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		// If we can't resolve (e.g., file doesn't exist yet), fall back to Clean
		resolved = filepath.Clean(p)
	}

	if strings.HasPrefix(resolved, "/workspace/") || resolved == "/workspace" ||
		strings.HasPrefix(resolved, "/scion-volumes/") || resolved == "/scion-volumes" {
		return resolved
	}

	fmt.Fprintf(os.Stderr, "Warning: attachment path %q is outside allowed roots "+
		"(/workspace, /scion-volumes); skipping\n", p)
	return ""
}

// copyFile copies the file at src to dst, preserving permissions.
func copyFile(src, dst string) (err error) {
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = srcFile.Close() }()

	srcInfo, err := srcFile.Stat()
	if err != nil {
		return err
	}

	dstFile, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, srcInfo.Mode())
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := dstFile.Close(); err == nil {
			err = closeErr
		}
	}()

	_, err = io.Copy(dstFile, srcFile)
	return err
}

// uniqueDest returns a unique destination path in dir for basename. If basename
// already exists in dir, appends _1, _2, etc. before the extension.
func uniqueDest(dir, basename string) (string, error) {
	dest := filepath.Join(dir, basename)
	_, err := os.Stat(dest)
	if err != nil {
		if os.IsNotExist(err) {
			return dest, nil
		}
		return "", err
	}

	ext := filepath.Ext(basename)
	name := strings.TrimSuffix(basename, ext)
	for i := 1; ; i++ {
		candidate := filepath.Join(dir, fmt.Sprintf("%s_%d%s", name, i, ext))
		_, err := os.Stat(candidate)
		if err != nil {
			if os.IsNotExist(err) {
				return candidate, nil
			}
			return "", err
		}
	}
}

// stageAttachments copies attachment files to the scratchpad shared volume
// and returns the new paths. Returns an error if the scratchpad is not
// available — attachments require shared storage for cross-agent delivery.
func stageAttachments(paths []string) (staged []string, err error) {
	scratchpad := "/scion-volumes/scratchpad"

	// Check scratchpad availability — hard error if absent
	if _, err := os.Stat(scratchpad); os.IsNotExist(err) {
		return nil, fmt.Errorf("scratchpad volume not available at %s; "+
			"attachments require a scratchpad shared volume for cross-agent "+
			"file transfer. Create one with: scion shared-dir create scratchpad",
			scratchpad)
	}

	// Determine agent slug for per-agent directory
	agentSlug := os.Getenv("SCION_AGENT_NAME")
	if agentSlug == "" {
		agentSlug = "_user"
	}

	// Generate per-message staging directory under agent slug
	msgID := api.NewUUID()
	stageDir := filepath.Join(scratchpad, ".attachments", agentSlug, msgID)
	if err := os.MkdirAll(stageDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create attachment staging directory: %w", err)
	}

	// Clean up staging directory if we return an error
	defer func() {
		if err != nil {
			_ = os.RemoveAll(stageDir)
		}
	}()

	staged = make([]string, 0, len(paths))
	for _, p := range paths {
		// Resolve path
		resolved := resolveAttachmentPath(p)
		if resolved == "" {
			continue // filtered out (warning already printed)
		}

		// Validate file exists and is a regular file
		info, err := os.Stat(resolved)
		if err != nil {
			return nil, fmt.Errorf("attachment %q: %w", p, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("attachment %q: not a regular file", p)
		}

		// Copy to staging directory (handle duplicate basenames)
		dest, err := uniqueDest(stageDir, filepath.Base(resolved))
		if err != nil {
			return nil, fmt.Errorf("failed to determine destination for attachment %q: %w", p, err)
		}
		if err := copyFile(resolved, dest); err != nil {
			return nil, fmt.Errorf("failed to stage attachment %q: %w", p, err)
		}
		staged = append(staged, dest)
	}

	return staged, nil
}
