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
	"errors"
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
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/spf13/cobra"
)

var msgInterrupt bool
var msgIn string
var msgAt string
var msgPlain bool
var msgAttach []string
var msgNotify bool
var msgWake bool
var msgChannel string
var msgThreadID string
var msgCC []string
var msgBodyFile string

// errRawFlagRemoved is returned for any use of the removed --raw flag.
var errRawFlagRemoved = fmt.Errorf("--raw has been removed from 'scion message': raw keystroke delivery through messages is no longer supported; " +
	"send keystrokes with 'scion keys <agent-name> <keystrokes>' instead (see 'scion keys --help')")

// emitDeprecationWarning prints a deprecation notice to stderr.
func emitDeprecationWarning(flag, replacement string) {
	fmt.Fprintf(os.Stderr, "Warning: --%s is deprecated: %s\n", flag, replacement)
}

// deprecationReplacements maps deprecated message-command flags to their
// replacement guidance. Tests assert that every replacement mentioning a
// conversation-reference form (@<, conv:, #<) is documented in Long.
var deprecationReplacements = []struct {
	Flag    string
	Message string
}{
	{"plain", "--plain is deprecated and will be removed"},
	{"notify", "use 'scion notifications subscribe' instead"},
	{"in", "use 'scion schedule create --in' instead"},
	{"at", "use 'scion schedule create --at' instead"},
	{"channel", "address the conversation with conv:<uuid> (see 'scion conversation list'), or use @<name> to message an agent directly"},
	{"thread-id", "address the conversation with conv:<uuid> (see 'scion conversation list'); for user: recipients on the web channel, the Hub rejects a thread ID that does not match an existing conversation"},
	{"cc", "--cc is deprecated and will be removed"},
}

// emitDeprecationWarnings checks all deprecated flags and emits warnings
// for any that were explicitly set.
func emitDeprecationWarnings(cmd *cobra.Command) {
	for _, d := range deprecationReplacements {
		if cmd.Flags().Changed(d.Flag) {
			emitDeprecationWarning(d.Flag, d.Message)
		}
	}
}

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
  @<agent-name>      Send to an agent's conversation (preferred)
  @<email>           Send to a user by email (global DM)
  conv:<uuid>        Send to a conversation by ID
  #<thread>          Send to a named thread

Message body can be provided as:
  - Positional arguments: scion message agent "hello world"
  - File: scion message agent --body-file msg.txt
  - Stdin: echo "hello" | scion message agent -
           (or: scion message agent --body-file -)

For --body-file and stdin, trailing CR/LF characters are trimmed; all other text,
including interior newlines, is sent exactly as read.

The shell expands backticks and $(...) inside double-quoted arguments before
scion runs, executing them and splicing in their output. To send code or
shell snippets verbatim, use --body-file or stdin with a quoted heredoc:

  scion message my-agent - <<'EOF'
  Run ` + "`make test`" + ` and check $(pwd)
  EOF

Examples:
  scion message my-agent "Please review the PR"
  scion message @my-agent "Please review the PR"
  scion message user:alice "I need clarification on the auth module"
  scion message "group[agent:reviewer,user:alice,deploy-bot]" "Release v2 is ready"
  scion message my-agent --body-file /path/to/message.txt
  echo "message with backticks" | scion message my-agent -`,
	Args: func(cmd *cobra.Command, args []string) error {
		// --raw is removed. Reject it here, in argument validation, so the
		// command fails before any hook, project resolution or network call
		// runs: a --raw invocation never reaches the wire.
		if cmd.Flags().Changed("raw") {
			return errRawFlagRemoved
		}
		// --body-file provides the message, so we only need recipient
		bodyFile, _ := cmd.Flags().GetString("body-file")
		if bodyFile != "" {
			if len(args) < 1 {
				return fmt.Errorf("recipient is required")
			}
			if len(args) > 1 {
				return fmt.Errorf("--body-file and positional message arguments are mutually exclusive")
			}
			return nil
		}
		if len(args) < 2 {
			return fmt.Errorf("recipient and message are required unless --body-file is used")
		}
		return nil
	},
	ValidArgsFunction: getAgentNames,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Refuse removed flags with actionable errors.
		// In agent mode, scion broadcast is not available (not in agentAllowed),
		// so the error must not recommend it — tell agents to address recipients
		// explicitly instead.
		if cmd.Flags().Changed("broadcast") {
			if resolveMode() == ModeAgent {
				return fmt.Errorf("--broadcast has been removed from 'scion message'; broadcasting is not available in agent mode — address your recipients explicitly (e.g. @agent-name)")
			}
			return fmt.Errorf("--broadcast has been removed from 'scion message'; use 'scion broadcast' instead")
		}
		if cmd.Flags().Changed("all") {
			if resolveMode() == ModeAgent {
				return fmt.Errorf("--all has been removed from 'scion message'; broadcasting is not available in agent mode — address your recipients explicitly (e.g. @agent-name)")
			}
			return fmt.Errorf("--all has been removed from 'scion message'; use 'scion broadcast --all' instead")
		}

		// Emit deprecation warnings for any deprecated flags in use.
		// Deprecated flags still work — they warn AND succeed.
		emitDeprecationWarnings(cmd)

		var agentName string
		var userRecipient string
		var groupRecipients []messages.GroupRecipient
		var convRef *messaging.Reference // S4 conversation reference (conv:, @, #)
		var message string

		{
			if len(args) < 1 {
				return fmt.Errorf("recipient is required")
			}
			recipient := args[0]
			if len(args) > 1 {
				message = strings.Join(args[1:], " ")
			}

			// Try parsing as an S4 conversation reference first.
			// This catches conv:<uuid>, @<agent-slug>, @<email>, #<thread>.
			if ref, err := messaging.ParseReference(recipient); err == nil {
				// DEF-138: conv:<uuid> and #<thread> are now fully supported.
				// Delivery routing through explicit conversation assertion
				// (P-1..P-3) means the conversation_id survives to the
				// persisting writer. The gate that previously rejected these
				// two kinds is removed.
				convRef = ref
			} else if strings.HasPrefix(recipient, "conv:") || strings.HasPrefix(recipient, "#") {
				// Looks like a conversation reference but failed to parse.
				// Parse-failure-denies: fail loudly, do not fall through to legacy paths.
				return fmt.Errorf("invalid conversation reference: %w", err)
			} else if strings.HasPrefix(recipient, "@") {
				// @ prefix is exclusively a conversation reference in the new grammar.
				// A bare email without leading @ falls through to the legacy path below.
				return fmt.Errorf("invalid conversation reference: %w", err)
			} else if messages.IsGroupRecipient(recipient) {
				parsed, err := messages.ParseGroupRecipient(recipient)
				if err != nil {
					return fmt.Errorf("invalid group recipient: %w", err)
				}
				groupRecipients = parsed
			} else if strings.HasPrefix(recipient, "user:") {
				userRecipient = recipient
			} else if strings.Contains(recipient, "@") && !strings.HasPrefix(recipient, "agent:") {
				// Legacy bare email — treat as user recipient for backward compat.
				userRecipient = "user:" + recipient
			} else {
				// Strip optional "agent:" prefix for backwards compatibility
				agentName = api.Slugify(strings.TrimPrefix(recipient, "agent:"))
			}
		}

		// Validate --body-file conflicts
		if msgBodyFile != "" && len(args) > 1 {
			return fmt.Errorf("--body-file and positional message arguments are mutually exclusive")
		}

		// Resolve body from --body-file or stdin
		var err error
		message, err = resolveMessageBody(msgBodyFile, message)
		if err != nil {
			return err
		}

		// Ensure we have a message body
		if message == "" {
			return fmt.Errorf("message body is empty; provide a message via positional args, --body-file, or pipe to stdin with '-'")
		}

		// Validate scheduling flags
		if msgIn != "" && msgAt != "" {
			return fmt.Errorf("--in and --at are mutually exclusive")
		}

		// Validate --thread-id requires --channel
		if msgThreadID != "" && msgChannel == "" {
			return fmt.Errorf("--thread-id requires --channel to be set")
		}

		// Validate --cc restrictions: parse first so empty-string values
		// (e.g. --cc "") are handled correctly instead of triggering
		// false-positive validation errors.
		parsedCC := parseCCFlag(msgCC)
		if len(parsedCC) > 0 {
			if msgIn != "" || msgAt != "" {
				return fmt.Errorf("--cc cannot be combined with --in or --at")
			}
			if userRecipient != "" {
				return fmt.Errorf("--cc cannot be used with user recipients")
			}
		}

		// Validate user-recipient restrictions
		if userRecipient != "" {
			if msgIn != "" || msgAt != "" {
				return fmt.Errorf("--in/--at cannot be used with user recipients")
			}
		}

		// Validate group recipient restrictions
		if len(groupRecipients) > 0 {
			if msgIn != "" || msgAt != "" {
				return fmt.Errorf("--in/--at cannot be used with group[] recipients")
			}
			if msgNotify {
				return fmt.Errorf("--notify cannot be used with group[] recipients")
			}
		}

		// Validate --wake restrictions
		if msgWake {
			if msgIn != "" || msgAt != "" {
				return fmt.Errorf("--wake cannot be combined with --in or --at")
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

		// Cross-project detection: when running as an agent and --project
		// specifies a different project than the sender's own, the --project
		// value is the TARGET project, not the sender's working context.
		// Set up hub context using the agent's own project and capture the
		// target project slug for cross-project resolution.
		//
		// When --project matches the agent's own project (by slug or ID),
		// skip cross-project detection to preserve normal same-project
		// sending — which works even when CPM is disabled. Without this
		// guard, the cross-project resolver would reject at Hub-off before
		// lookup, breaking ordinary same-project sends with explicit --project.
		var crossProjectTarget string
		senderProjectPath := projectPath
		hasAgentTarget := agentName != "" || (convRef != nil && convRef.Kind == messaging.RefAgent)
		if hasAgentTarget {
			crossProjectTarget = detectCrossProjectTarget(cmd)
			if crossProjectTarget != "" {
				senderProjectPath = "" // let hub context resolve from agent's own config
			}
		}

		// conv:<id> with explicit --project mismatch in agent mode: reject.
		// The conversation ID already identifies its project context;
		// reinterpreting the sender context via --project would be silently
		// wrong. This intentionally does NOT reuse detectCrossProjectTarget:
		// that helper treats an explicitly empty --project ("") as
		// same-project, but this check must still reject it, matching
		// pre-existing behavior.
		if convRef != nil && convRef.Kind == messaging.RefConversation &&
			os.Getenv("SCION_AGENT_NAME") != "" && cmd.Flags().Changed("project") {
			ownProjectSlug := os.Getenv("SCION_PROJECT")
			ownProjectID := os.Getenv("SCION_PROJECT_ID")
			isSameProject := (ownProjectSlug != "" && projectPath == ownProjectSlug) ||
				(ownProjectID != "" && projectPath == ownProjectID)
			if !isSameProject {
				return fmt.Errorf("--project cannot be used with conv: references; the conversation already identifies its project context")
			}
		}

		// Check if Hub should be used
		var hubCtx *HubContext
		if convRef != nil {
			// Conversation references require Hub mode for resolution
			hubCtx, err = CheckHubAvailabilityWithOptions(senderProjectPath, true)
		} else if len(groupRecipients) > 0 {
			// Group recipients: skip sync (multiple recipients, no single agent)
			hubCtx, err = CheckHubAvailabilityWithOptions(senderProjectPath, true)
		} else if userRecipient != "" {
			// User recipient: skip sync (no agent involved)
			hubCtx, err = CheckHubAvailabilityWithOptions(senderProjectPath, true)
		} else if crossProjectTarget != "" {
			// Cross-project: set up hub from agent's own project
			hubCtx, err = CheckHubAvailabilityWithOptions(senderProjectPath, true)
		} else {
			// Single agent: exclude target from sync requirements
			hubCtx, err = CheckHubAvailabilityForAgent(senderProjectPath, agentName, true)
		}
		if err != nil {
			return err
		}

		// Conversation references require Hub mode
		if convRef != nil && hubCtx == nil {
			return fmt.Errorf("conversation references require Hub mode (use 'scion hub enable' first)")
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

		// Cross-project agent addressing: intercept @agent and bare agent
		// names BEFORE the generic convRef dispatch. When crossProjectTarget
		// is set, both forms route through the cross-project send path.
		if hubCtx != nil && crossProjectTarget != "" {
			if convRef != nil && convRef.Kind == messaging.RefAgent {
				return sendCrossProjectMessage(hubCtx, crossProjectTarget, convRef.Value, message, msgInterrupt, msgWake, msgAttach)
			}
			if agentName != "" {
				return sendCrossProjectMessage(hubCtx, crossProjectTarget, agentName, message, msgInterrupt, msgWake, msgAttach)
			}
		}

		// Conversation-reference messages: resolve and send via Hub
		if convRef != nil {
			return sendMessageViaConversation(hubCtx, convRef, message, msgInterrupt, msgWake, msgAttach)
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
			return sendMessageViaHub(hubCtx, agentName, message, msgInterrupt, msgNotify, msgWake)
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

		fmt.Printf("Sending message to agent '%s'...\n", agentName)
		if err := mgr.Message(ctx, agentName, "", message, msgInterrupt); err != nil {
			return err
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
// plain and urgent are passed explicitly rather than read from the
// message-command flag globals; channel and thread ID still come from the
// deprecated --channel/--thread-id globals.
func buildStructuredMessage(sender, recipient, message string, attachments []string, plain, urgent bool) *messages.StructuredMessage {
	msg := messages.NewInstruction(sender, recipient, message)
	msg.Plain = plain
	msg.Urgent = urgent
	if len(attachments) > 0 {
		msg.Attachments = attachments
	}
	msg.Channel = msgChannel
	msg.ThreadID = msgThreadID
	return msg
}

// agentMessageSendError renders a failed agent-message send. A 404 with the
// hub's agent_not_found code means the recipient does not exist (deleted,
// reaped, or misspelled), so the error states that plainly and names the
// agent; the hub's own message is kept as the cause. Other 404s (e.g. a
// stale project ID, "Project not found", or a bare 404 from an older hub or
// a proxy) are not about the agent, so they and all other failures keep the
// generic wording. Both go through wrapHubError, which (for a 404) adds no
// local-only hint, and Execute prints no Usage block for hub failures; the
// command exits 1.
func agentMessageSendError(agentName string, err error) error {
	var apiErr *apiclient.APIError
	if errors.As(err, &apiErr) && apiErr.IsNotFound() && apiErr.Code == apiclient.ErrCodeAgentNotFound {
		return wrapHubError(fmt.Errorf("agent '%s' not found; message not sent: %w", agentName, err))
	}
	return wrapHubError(fmt.Errorf("failed to send message to agent '%s' via Hub: %w", agentName, err))
}

func sendMessageViaHub(hubCtx *HubContext, agentName string, message string, interrupt bool, notify bool, wake bool) error {
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

	msg := buildStructuredMessage(sender, "agent:"+agentName, message, msgAttach, msgPlain, interrupt)
	// Validate through the new envelope choke point (Phase 7, AC-8).
	if err := messaging.ValidateLegacyMessage(msg); err != nil {
		return fmt.Errorf("message validation failed: %w", err)
	}

	// Server-side @mention fan-out: the hub parses the body itself and
	// delivers each mention, so the CLI sends a single request. It still
	// sends an explicit mentions list — unioned with --cc — so a hub that
	// only fans out from the explicit field still delivers it. Filtering
	// self and the primary here matters because such a hub does not
	// exclude the sender.
	mentions := filterMentionNames(
		messages.DedupMentionNames(extractMentions(message), parseCCFlag(msgCC)),
		strings.TrimPrefix(sender, "agent:"), agentName)

	resp, err := agentSvc.SendStructuredMessageWithOptions(ctx, agentName, msg, hubclient.SendMessageOptions{
		Interrupt: interrupt,
		Notify:    notify,
		Wake:      wake,
		Mentions:  mentions,
	})
	if err != nil {
		return agentMessageSendError(agentName, err)
	}

	if isJSONOutput() {
		if resp != nil {
			return outputJSON(resp)
		}
		return nil
	}
	// Mention outcomes print before the confirmation so the last line
	// reports the send itself; a skipped mention never means a failed send.
	if resp != nil {
		printMentionResults(resp.MentionResults)
	}
	if resp != nil && resp.Status == "deferred" {
		// Design agent-reincarnate §3.7: the recipient is mid-`scion
		// reincarnate`. The message was saved to history, not dropped.
		fmt.Printf("agent %s is reincarnating; message saved to history and will be seen on catch-up (message %s).\n", agentName, resp.MessageID)
	} else {
		fmt.Printf("Message delivered to agent '%s'.\n", agentName)
	}
	if notify {
		fmt.Printf("Subscribed to notifications for agent '%s'.\n", agentName)
	}

	return nil
}

// sendCrossProjectMessage resolves a target agent in a foreign project and
// sends a message via the non-project-scoped agent message endpoint. The
// sender's authenticated identity is preserved; only target resolution
// crosses the project boundary.
func sendCrossProjectMessage(hubCtx *HubContext, targetProject, agentSlug, message string, interrupt, wake bool, attachments []string) error {
	if !isJSONOutput() {
		PrintUsingHub(hubCtx.Endpoint)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Step 1: Resolve the target agent in the foreign project.
	result, err := hubCtx.Client.Messaging().ResolveTarget(ctx, targetProject, agentSlug)
	if err != nil {
		return wrapHubError(fmt.Errorf("failed to resolve agent %q in project %q: %w", agentSlug, targetProject, err))
	}
	if result.Agent == nil {
		return fmt.Errorf("agent %q not found in project %q", agentSlug, targetProject)
	}

	if !isJSONOutput() {
		fmt.Printf("Sending cross-project message to agent '%s' in project '%s'...\n", agentSlug, targetProject)
	}

	// Step 2: Build the structured message with the sender's identity.
	sender := resolveSenderIdentity(hubCtx)
	msg := buildStructuredMessage(sender, "agent:"+agentSlug, message, attachments, msgPlain, interrupt)
	if err := messaging.ValidateLegacyMessage(msg); err != nil {
		return fmt.Errorf("message validation failed: %w", err)
	}

	// Step 3: Send via the non-project-scoped endpoint using the target's UUID.
	// ProjectAgents("") produces /api/v1/agents/{uuid}/message which bypasses
	// the project-scoped slug lookup and its project isolation check.
	agentSvc := hubCtx.Client.ProjectAgents("")
	resp, err := agentSvc.SendStructuredMessage(ctx, result.Agent.ID, msg, interrupt, false, wake)
	if err != nil {
		return wrapHubError(fmt.Errorf("failed to send cross-project message to agent '%s': %w", agentSlug, err))
	}

	// The hub fans body @mentions out for this path too, the same as any
	// other agent message; report the results the same way the other send
	// paths do.
	if isJSONOutput() {
		if resp != nil {
			return outputJSON(resp)
		}
		return nil
	}
	if resp != nil {
		printMentionResults(resp.MentionResults)
	}
	fmt.Printf("Message delivered to agent '%s' in project '%s'.\n", agentSlug, targetProject)

	return nil
}

// sendMessageViaConversation sends a message to a conversation reference.
//
// DEF-142 P5: the CLI passes conversation_ref in the outbound message request
// and the server resolves it inline (P3), routing through the existing DEF-138
// auth block. This eliminates the two-step resolve-then-send pattern that
// ResolveConversation existed for.
//
// Two dispatch paths remain:
//   - Agent context (SCION_AGENT_NAME set): all ref kinds go via the outbound
//     endpoint with conversation_ref. The server resolves + authorizes.
//   - Human CLI context: only @agent is supported. The message is sent via
//     SendStructuredMessage; the server derives the conversation from
//     sender/recipient principals (DEF-138 Rule 3).
func sendMessageViaConversation(hubCtx *HubContext, ref *messaging.Reference, message string, interrupt bool, wake bool, attachments []string) error {
	if !isJSONOutput() {
		PrintUsingHub(hubCtx.Endpoint)
	}

	projectID, err := GetProjectID(hubCtx)
	if err != nil {
		return wrapHubError(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	agentSvc := hubCtx.Client.ProjectAgents(projectID)

	// DEF-142 P5: when running in an agent context, send ALL ref kinds via
	// the outbound endpoint with conversation_ref. The server resolves the
	// ref inline (P3) and routes through the existing DEF-138 auth block.
	//
	// CPM cutover (#1693): conv: references now route through the outbound
	// endpoint like @email and #thread, eliminating the duplicate conversation
	// send API path. Wake and attachments are fully supported.
	senderAgent := os.Getenv("SCION_AGENT_NAME")
	if senderAgent != "" {
		// DEF-164: agent-to-agent messages use the structured message
		// endpoint, not the outbound (user-only) endpoint. The outbound
		// handler's resolveAgentDM creates conversation/participant rows
		// before the DEF-152 addressee derivation rejects non-user DMs —
		// routing through SendStructuredMessage avoids both the rejection
		// and the orphan rows.
		if ref.Kind == messaging.RefAgent {
			sender := "agent:" + senderAgent
			agentMsg := buildStructuredMessage(sender, "agent:"+ref.Value, message, attachments, msgPlain, interrupt)
			if err := messaging.ValidateLegacyMessage(agentMsg); err != nil {
				return fmt.Errorf("message validation failed: %w", err)
			}
			// Server-side @mention fan-out: no --cc here (not accepted on
			// this path), just the body's @mentions, filtered of the sender
			// and the primary for old-hub compatibility.
			mentions := filterMentionNames(extractMentions(message), senderAgent, ref.Value)
			resp, err := agentSvc.SendStructuredMessageWithOptions(ctx, ref.Value, agentMsg, hubclient.SendMessageOptions{
				Wake:     wake,
				Mentions: mentions,
			})
			if err != nil {
				return agentMessageSendError(ref.Value, err)
			}
			if isJSONOutput() {
				if resp != nil {
					return outputJSON(resp)
				}
				return nil
			}
			if resp != nil {
				printMentionResults(resp.MentionResults)
			}
			if resp != nil && resp.Status == "deferred" {
				// Design agent-reincarnate §3.7: the recipient is
				// mid-`scion reincarnate`. The message was saved to
				// history, not dropped.
				fmt.Printf("agent %s is reincarnating; message saved to history and will be seen on catch-up (message %s).\n", ref.Value, resp.MessageID)
			} else {
				fmt.Printf("Message delivered to agent '%s'.\n", ref.Value)
			}
			return nil
		}

		// All non-agent ref kinds (conv:, @email, #thread) route through
		// the outbound endpoint with conversation_ref. The server resolves
		// the ref inline and handles wake, attachments, and auth.
		outMsg := &hubclient.OutboundMessageRequest{
			Msg:             message,
			Type:            "instruction",
			Urgent:          interrupt,
			ConversationRef: ref.Raw,
			Attachments:     attachments,
			Wake:            wake,
		}
		if ref.Kind == messaging.RefEmail {
			outMsg.Recipient = "user:" + ref.Value
		}

		// DEF-51 principle: the validated probe must match the sent envelope
		// by construction. Fields the outbound path does not send (Channel,
		// ThreadID) are zero in both outMsg and probe.
		probe := &messages.StructuredMessage{
			Version:     messages.Version,
			Timestamp:   time.Now().UTC().Format(time.RFC3339),
			Sender:      senderAgent,
			Msg:         outMsg.Msg,
			Type:        outMsg.Type,
			Attachments: attachments,
		}
		if ref.Kind == messaging.RefEmail {
			probe.Recipient = outMsg.Recipient
		}
		if err := messaging.ValidateLegacyMessage(probe); err != nil {
			return fmt.Errorf("message validation failed: %w", err)
		}

		result, err := agentSvc.SendOutboundMessage(ctx, senderAgent, outMsg)
		if err != nil {
			return wrapHubError(fmt.Errorf("failed to send message to %s: %w", ref.Raw, err))
		}
		if isJSONOutput() {
			// JSON output with full result (MentionResults included, if any).
			// outputJSON encodes a nil *OutboundMessageResult as JSON null,
			// which is valid output and does not panic.
			return outputJSON(result)
		}
		// apiclient.DecodeResponse returns (nil, nil) on 204 No Content, so
		// result can be nil with a nil err; print the minimal confirmation
		// instead of dereferencing a nil result.
		if result == nil {
			fmt.Printf("Message dispatched to %s.\n", ref.Raw)
			return nil
		}
		// Mention outcomes first, so the confirmation is the last line.
		printMentionResults(result.MentionResults)
		// Distinguish accepted dispatch from confirmed delivery.
		switch result.Status {
		case "sent":
			fmt.Printf("Message sent to %s (message %s).\n", ref.Raw, result.MessageID)
		case "deferred":
			// Design agent-reincarnate §3.7: the recipient is mid-`scion
			// reincarnate`. The message was saved to history, not dropped.
			fmt.Printf("agent %s is reincarnating; message saved to history and will be seen on catch-up (message %s).\n", ref.Raw, result.MessageID)
		default:
			fmt.Printf("Message dispatched to %s (message %s, status: %s).\n", ref.Raw, result.MessageID, result.Status)
		}
		return nil
	}

	// Human CLI context — only @agent is supported without an agent identity.
	// @email, conv:<uuid>, and #<thread> require SCION_AGENT_NAME because the
	// server needs a sender principal to resolve the conversation.
	if ref.Kind == messaging.RefEmail {
		return fmt.Errorf("@<email> addressing requires an agent identity; it works inside an agent container where SCION_AGENT_NAME is set")
	}
	if ref.Kind != messaging.RefAgent {
		return fmt.Errorf("%s addressing requires an agent identity; it works inside an agent container where SCION_AGENT_NAME is set", ref.Raw)
	}

	// @agent from human CLI: build and validate, then send via the agent
	// message endpoint. The server derives the conversation from the
	// sender/recipient principals (DEF-138 Rule 3).
	sender := resolveSenderIdentity(hubCtx)
	agentMsg := buildStructuredMessage(sender, "agent:"+ref.Value, message, attachments, msgPlain, interrupt)
	if err := messaging.ValidateLegacyMessage(agentMsg); err != nil {
		return fmt.Errorf("message validation failed: %w", err)
	}

	if _, err := agentSvc.SendStructuredMessage(ctx, ref.Value, agentMsg, interrupt, false, wake); err != nil {
		return agentMessageSendError(ref.Value, err)
	}
	if !isJSONOutput() {
		fmt.Printf("Message delivered to agent '%s'.\n", ref.Value)
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

	// Determine the sending agent's name. User-targeted messages require an
	// agent identity so the server can attribute and route the message.
	senderAgent := os.Getenv("SCION_AGENT_NAME")
	if senderAgent == "" {
		return fmt.Errorf("user messaging requires an agent identity; it works inside an agent container where SCION_AGENT_NAME is set")
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

	result, err := agentSvc.SendOutboundMessage(ctx, senderAgent, outMsg)
	if err != nil {
		return wrapHubError(fmt.Errorf("failed to send message to %s: %w", userRecipient, err))
	}

	if isJSONOutput() {
		if result != nil {
			return outputJSON(result)
		}
		return nil
	}
	if result != nil {
		printMentionResults(result.MentionResults)
	}
	// #2026: name the conversation the message landed in when the hub
	// reports it, so a send with --channel/--thread-id shows where it went.
	if result != nil && result.ConversationID != "" {
		fmt.Printf("Message sent to %s via Hub (conversation %s).\n", userRecipient, result.ConversationID)
	} else {
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

	// A25.6 F2: the group[] fan-out prints its per-recipient status from the
	// send response, not an assumption. Report-7-gteam-2a: while a target
	// was reincarnating, the human CLI printed "Delivered" for it anyway —
	// the response was discarded. Mirror the single-recipient path's own
	// "Status == deferred" check (see sendMessageViaHub above).

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
				msg := buildStructuredMessage(sender, "agent:"+slug, message, msgAttach, msgPlain, interrupt)
				msg.Type = messages.TypeGroupSet
				msg.Recipients = recipientsStr
				msg.Metadata = map[string]string{"group_id": groupID}
				sendResp, err := agentSvc.SendStructuredMessage(ctx, slug, msg, interrupt, false, false)
				if err != nil {
					results[idx] = recipientResult{Recipient: recipStr, Status: "failed", Error: err.Error()}
					if !isJSONOutput() {
						fmt.Printf("  Failed: %s: %s\n", recipStr, err)
					}
					return
				}
				if sendResp != nil && sendResp.Status == "deferred" {
					results[idx] = recipientResult{Recipient: recipStr, Status: "deferred"}
					if !isJSONOutput() {
						fmt.Printf("  Deferred: %s (agent is reincarnating; saved)\n", recipStr)
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
				if _, err := agentSvc.SendOutboundMessage(ctx, senderAgent, outMsg); err != nil {
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
	deferred := 0
	failed := 0
	for _, r := range results {
		switch r.Status {
		case "delivered":
			delivered++
		case "deferred":
			deferred++
		default:
			failed++
		}
	}

	// A25.7 O1: honour --json for group sends the same way the
	// single-recipient paths do (outputJSON(result)) — the per-recipient
	// status, including "deferred", was previously only ever printed as
	// human text; --json produced no output at all.
	if isJSONOutput() {
		if err := outputJSON(results); err != nil {
			return err
		}
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

	// The delivery summary prints after the mention fan-out so it is the
	// last line; mention notes above it never change the delivery result.
	if !isJSONOutput() {
		if deferred > 0 {
			fmt.Printf("Group delivery complete: %d/%d delivered, %d deferred.\n", delivered, len(recipients), deferred)
		} else {
			fmt.Printf("Group delivery complete: %d/%d delivered.\n", delivered, len(recipients))
		}
	}

	// A25.6 F2: a deferred recipient is not a failure (design agent-reincarnate
	// §3.7 — the message is saved for catch-up, not dropped), so it must not
	// trip the partial-failure error below on its own.
	//
	// A25.7 O2: report delivered, deferred and failed counts explicitly. The
	// previous "%d/%d delivered" wording folded deferred into "delivered" for
	// this message only (the counts above were already separated), which
	// could describe e.g. 1 delivered + 1 deferred + 1 failed as "2/3
	// delivered" — technically true of the denominator, but it hides that a
	// real failure occurred.
	succeeded := delivered + deferred
	if succeeded == 0 {
		return fmt.Errorf("group delivery failed: 0 delivered, 0 deferred, %d failed (of %d total)", failed, len(recipients))
	}
	if succeeded < len(recipients) {
		return fmt.Errorf("group delivery partially failed: %d delivered, %d deferred, %d failed (of %d total)", delivered, deferred, failed, len(recipients))
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
		fmt.Printf("Message to agent '%s' scheduled for %s\n", agentName, clitime.Format(evt.FireAt, clitime.Full))
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

// extractMentions delegates to the shared messages.ExtractProseMentions, so
// an @-mention inside a fenced code block, an inline backtick span, or a
// quoted '>' line is not treated as an address. Agent bodies pasting logs or
// diffs are common enough that this matters for the CLI's own
// explicit-mentions list, same as it does for the server's body parsing.
func extractMentions(text string) []string {
	return messages.ExtractProseMentions(text)
}

// parseCCFlag delegates to the shared messages.ParseCCFlags. The --cc flag is
// repeatable and each occurrence may itself be a comma-separated list.
func parseCCFlag(cc []string) []string {
	return messages.ParseCCFlags(cc)
}

// maxMentionRecipients is an alias for the shared constant.
const maxMentionRecipients = messages.MaxMentionRecipients

// filterMentionNames drops any name equal (case-insensitively) to selfSlug or
// primarySlug. This matters against an old hub, which does not exclude the
// sender from an explicit mentions list the way the new hub's
// fanOutAgentMentions does.
func filterMentionNames(names []string, selfSlug, primarySlug string) []string {
	if len(names) == 0 {
		return nil
	}
	var out []string
	for _, name := range names {
		if selfSlug != "" && strings.EqualFold(name, selfSlug) {
			continue
		}
		if primarySlug != "" && strings.EqualFold(name, primarySlug) {
			continue
		}
		out = append(out, name)
	}
	return out
}

// printMentionResults prints one line per mention result to stderr,
// describing the outcome for names that were not cleanly delivered. Skipped
// entirely under --json output, where the caller includes the results in the
// JSON response instead.
func printMentionResults(results []messages.MentionResult) {
	if isJSONOutput() {
		return
	}
	for _, r := range results {
		switch r.Status {
		case "delivered":
			// AgentPhase covers every non-running phase alike (stopped,
			// suspended, errored, still starting, ...) — the hub delivers
			// a mention regardless of which one it is, so the wording here
			// must not claim it "resumes", which is only accurate for
			// stopped/suspended.
			if r.AgentPhase != "" && r.AgentPhase != "running" {
				fmt.Fprintf(os.Stderr, "@%s is %s; it will see this mention once it is running.\n", r.Slug, r.AgentPhase)
			} else {
				fmt.Fprintf(os.Stderr, "Mention notification sent to @%s.\n", r.Slug)
			}
		case "not_found":
			fmt.Fprintf(os.Stderr, "Note: @%s is not an agent in this project; no agent was notified.\n", r.Slug)
		case "unauthorized":
			fmt.Fprintf(os.Stderr, "Warning: mention to @%s was denied (message delivery not authorized)\n", r.Slug)
		case "suppressed":
			fmt.Fprintf(os.Stderr, "Warning: mention to @%s was suppressed by loop protection; retry later\n", r.Slug)
		case "rate_limited":
			fmt.Fprintf(os.Stderr, "Warning: mention to @%s was rate-limited; retry later\n", r.Slug)
		case "ambiguous":
			fmt.Fprintf(os.Stderr, "Warning: mention to @%s delivery is ambiguous; it may or may not have been delivered\n", r.Slug)
		case "timeout":
			fmt.Fprintf(os.Stderr, "Warning: mention to @%s timed out and was not attempted\n", r.Slug)
		default:
			if r.Error != "" {
				fmt.Fprintf(os.Stderr, "Warning: mention to @%s failed: %s\n", r.Slug, r.Error)
			} else {
				fmt.Fprintf(os.Stderr, "Warning: mention to @%s: %s\n", r.Slug, r.Status)
			}
		}
	}
}

// sendMentionMessages resolves @mentions and --cc names against project agents
// and sends TypeMention messages to each resolved agent. The primary recipient
// is excluded from mentions. Unresolved names produce a non-fatal stderr note
// and do not fail the primary send.
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

	// Build lookup map of known agents (slug -> slug with original case).
	// Keyed by Slug, not Name: a mention names the agent's slug (e.g.
	// "@my-agent"), which can differ from its display Name, so looking up
	// by Name missed or mis-resolved any agent whose Name and Slug diverge.
	knownAgents := make(map[string]string, len(resp.Agents))
	for _, a := range resp.Agents {
		knownAgents[strings.ToLower(a.Slug)] = a.Slug
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
			fmt.Fprintf(os.Stderr, "Note: @%s is not an agent in this project; no agent was notified.\n", name)
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

// resolveMessageBody determines the message body from flags or positional args.
// Priority: --body-file > positional args. A positional body of exactly "-",
// or --body-file -, reads the body from stdin.
//
// Newline rule (the same for every non-positional source): trailing CR/LF
// characters are trimmed, so the newline that echo, a heredoc, or an editor
// adds at end-of-file is not sent. Interior newlines and leading
// or trailing spaces are preserved exactly. Positional bodies are used as
// given.
func resolveMessageBody(bodyFile string, positionalBody string) (string, error) {
	if bodyFile != "" {
		if positionalBody != "" {
			return "", fmt.Errorf("--body-file and positional message arguments are mutually exclusive")
		}
		if bodyFile == "-" {
			return readMessageBody(os.Stdin, "stdin")
		}
		file, err := os.Open(bodyFile)
		if err != nil {
			return "", fmt.Errorf("failed to open body file: %w", err)
		}
		defer func() { _ = file.Close() }()
		return readMessageBody(file, "body file")
	}
	if positionalBody == "-" {
		return readMessageBody(os.Stdin, "stdin")
	}
	return positionalBody, nil
}

// readMessageBody reads up to messages.MaxMsgSize+1 bytes from r and rejects
// anything over messages.MaxMsgSize before trimming, so a body that is cut
// off at the read limit is never sent in truncated form (trimming first
// could pull an over-limit read back under the limit). It then trims
// trailing CR/LF characters. source names r in error messages.
func readMessageBody(r io.Reader, source string) (string, error) {
	data, err := io.ReadAll(io.LimitReader(r, int64(messages.MaxMsgSize)+1))
	if err != nil {
		return "", fmt.Errorf("failed to read message from %s: %w", source, err)
	}
	if len(data) > messages.MaxMsgSize {
		return "", fmt.Errorf("message body from %s exceeds maximum size of %d bytes", source, messages.MaxMsgSize)
	}
	return strings.TrimRight(string(data), "\r\n"), nil
}

func init() {
	// Retained flags (core message functionality)
	messageCmd.Flags().BoolVarP(&msgInterrupt, "interrupt", "i", false, "Interrupt the harness before sending the message")
	messageCmd.Flags().BoolVarP(&msgWake, "wake", "w", false, "Resume a suspended agent before delivering the message")
	messageCmd.Flags().StringArrayVar(&msgAttach, "attach", nil, "Attach file path(s), repeatable; use paths under /workspace or /scion-volumes (bare relative paths resolve to /workspace). Absolute paths outside these roots are silently dropped on delivery.")
	messageCmd.Flags().StringVar(&msgBodyFile, "body-file", "", "Read message body from a file instead of positional args ('-' reads stdin; trailing CR/LF characters are trimmed)")

	// Deprecated flags — still functional, emit warnings when used.
	// These flags are hidden from help output to guide users toward
	// the new subcommands, but they continue to work identically.
	messageCmd.Flags().BoolP("broadcast", "b", false, "Removed: use 'scion broadcast' instead")
	messageCmd.Flags().BoolP("all", "a", false, "Removed: use 'scion broadcast --all' instead")
	messageCmd.Flags().StringVar(&msgIn, "in", "", "Deprecated: use 'scion schedule create --in' instead")
	messageCmd.Flags().StringVar(&msgAt, "at", "", "Deprecated: use 'scion schedule create --at' instead")
	messageCmd.Flags().BoolVar(&msgPlain, "plain", false, "Deprecated: --plain is deprecated and will be removed")
	// --raw is removed; the flag is kept (hidden) only so its use gets
	// actionable guidance instead of an unknown-flag error. See Args.
	messageCmd.Flags().Bool("raw", false, "Removed: use 'scion keys' instead")
	messageCmd.Flags().BoolVar(&msgNotify, "notify", false, "Deprecated: use 'scion notifications subscribe' instead")
	messageCmd.Flags().StringVar(&msgChannel, "channel", "", "Deprecated: use conversation references instead")
	messageCmd.Flags().StringVar(&msgThreadID, "thread-id", "", "Deprecated: use conversation references instead")
	messageCmd.Flags().StringArrayVar(&msgCC, "cc", nil, "Deprecated: --cc is deprecated and will be removed")

	// Hide deprecated flags from help
	_ = messageCmd.Flags().MarkHidden("broadcast")
	_ = messageCmd.Flags().MarkHidden("all")
	_ = messageCmd.Flags().MarkHidden("in")
	_ = messageCmd.Flags().MarkHidden("at")
	_ = messageCmd.Flags().MarkHidden("plain")
	_ = messageCmd.Flags().MarkHidden("raw")
	_ = messageCmd.Flags().MarkHidden("notify")
	_ = messageCmd.Flags().MarkHidden("channel")
	_ = messageCmd.Flags().MarkHidden("thread-id")
	_ = messageCmd.Flags().MarkHidden("cc")

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
