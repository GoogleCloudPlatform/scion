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
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/spf13/cobra"
)

var (
	convKind    string
	convSurface string
	convProject string
	convJSON    bool
	convLimit   int

	convMsgLimit  int
	convMsgBefore string
	convMsgAfter  string
	convMsgJSON   bool

	convCreateJSON bool

	convGetJSON        bool
	convGetMessageJSON bool

	convParticipantsJSON bool

	convCatchUpSince string
	convCatchUpJSON  bool
)

// conversationCmd is the top-level command for conversation management.
var conversationCmd = &cobra.Command{
	Use:     "conversation",
	Aliases: []string{"conv"},
	Short:   "Manage conversations",
	Long: `View and manage conversations you participate in.

Conversations require Hub mode. Enable with 'scion hub enable <endpoint>'.

Commands:
  scion conversation list                       List your conversations
  scion conversation messages <conv-ref>        View messages in a conversation
  scion conversation get <conv-ref>             Get conversation details
  scion conversation get-message <ref> <msg-id> Get a message from a conversation
  scion conversation create <name>              Create a new group conversation
  scion conversation set-default <ref> <agent>  Set default agent for a conversation

Conversation references:
  conv:<uuid>     Direct conversation ID
  @<agent-name>   Agent DM conversation
  #<thread-name>  Named thread conversation`,
	RunE: runConversationList,
}

// conversationListCmd lists conversations.
var conversationListCmd = &cobra.Command{
	Use:   "list",
	Short: "List conversations you participate in",
	Long: `List conversations you participate in.

Examples:
  scion conversation list
  scion conversation list --kind group
  scion conversation list --surface discord --json
  scion conversation list --limit 10`,
	RunE: runConversationList,
}

// conversationMessagesCmd shows messages in a conversation.
var conversationMessagesCmd = &cobra.Command{
	Use:   "messages <conversation-ref>",
	Short: "View messages in a conversation",
	Long: `View messages in a conversation.

Conversation references:
  conv:<uuid>     Direct conversation ID
  @<agent-name>   Agent DM conversation
  #<thread-name>  Named thread conversation

Examples:
  scion conversation messages conv:a1b2c3d4-...
  scion conversation messages @my-agent --limit 50
  scion conversation messages #design-thread --json`,
	Args: cobra.ExactArgs(1),
	RunE: runConversationMessages,
}

// conversationCreateCmd creates a new conversation.
var conversationCreateCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Create a new group conversation",
	Long: `Create a new group conversation.

Examples:
  scion conversation create "Design Discussion"
  scion conversation create "Sprint Planning" --project <project-id>
  scion conversation create "Debug Thread" --json`,
	Args: cobra.ExactArgs(1),
	RunE: runConversationCreate,
}

// conversationGetCmd gets conversation details.
var conversationGetCmd = &cobra.Command{
	Use:   "get <conversation-ref>",
	Short: "Get conversation details",
	Long: `Get details of a single conversation including participants.

Examples:
  scion conversation get conv:a1b2c3d4-...
  scion conversation get @my-agent --json`,
	Args: cobra.ExactArgs(1),
	RunE: runConversationGet,
}

// conversationGetMessageCmd gets a message from a conversation.
var conversationGetMessageCmd = &cobra.Command{
	Use:   "get-message <conversation-ref> <message-id>",
	Short: "Get a specific message by ID from a conversation",
	Long: `Get a specific message by its ID from a conversation.

Examples:
  scion conversation get-message conv:a1b2c3d4-... msg-uuid-here
  scion conversation get-message conv:a1b2c3d4-... msg-uuid-here --json`,
	Args: cobra.ExactArgs(2),
	RunE: runConversationGetMessage,
}

// conversationSetDefaultCmd sets the default agent for a conversation.
var conversationSetDefaultCmd = &cobra.Command{
	Use:   "set-default <conversation-ref> <agent-id>",
	Short: "Set the default agent for a conversation",
	Long: `Set the default agent for a conversation.

Examples:
  scion conversation set-default conv:a1b2c3d4-... my-agent-uuid
  scion conversation set-default @my-agent other-agent-uuid`,
	Args: cobra.ExactArgs(2),
	RunE: runConversationSetDefault,
}

// conversationParticipantsCmd lists participants in a conversation.
var conversationParticipantsCmd = &cobra.Command{
	Use:   "participants <conversation-ref>",
	Short: "List participants in a conversation",
	Long: `List participants in a conversation.

Examples:
  scion conversation participants conv:a1b2c3d4-...
  scion conversation participants @my-agent --json`,
	Args: cobra.ExactArgs(1),
	RunE: runConversationParticipants,
}

// conversationJoinCmd adds a participant to a conversation.
var conversationJoinCmd = &cobra.Command{
	Use:   "join <conversation-ref> <principal-kind> <principal-id>",
	Short: "Add a participant to a conversation",
	Long: `Add a participant to a conversation.

Examples:
  scion conversation join conv:a1b2c3d4-... agent 0b56e8d0-...
  scion conversation join #design-thread user a1b2c3d4-...`,
	Args: cobra.ExactArgs(3),
	RunE: runConversationJoin,
}

// conversationLeaveCmd removes the caller from a conversation.
var conversationLeaveCmd = &cobra.Command{
	Use:   "leave <conversation-ref>",
	Short: "Leave a conversation",
	Long: `Leave a conversation. Removes the caller from the conversation.

Examples:
  scion conversation leave conv:a1b2c3d4-...
  scion conversation leave #design-thread`,
	Args: cobra.ExactArgs(1),
	RunE: runConversationLeave,
}

// conversationCatchUpCmd shows recent messages in a conversation.
var conversationCatchUpCmd = &cobra.Command{
	Use:   "catch-up <conversation-ref>",
	Short: "Show recent messages in a conversation",
	Long: `Show recent messages in a conversation. Defaults to the last hour.

Examples:
  scion conversation catch-up conv:a1b2c3d4-...
  scion conversation catch-up @my-agent --since 30m
  scion conversation catch-up #design-thread --since 2h --json`,
	Args: cobra.ExactArgs(1),
	RunE: runConversationCatchUp,
}

func init() {
	rootCmd.AddCommand(conversationCmd)
	conversationCmd.AddCommand(conversationListCmd)
	conversationCmd.AddCommand(conversationMessagesCmd)
	conversationCmd.AddCommand(conversationCreateCmd)
	conversationCmd.AddCommand(conversationGetCmd)
	conversationCmd.AddCommand(conversationGetMessageCmd)
	conversationCmd.AddCommand(conversationSetDefaultCmd)
	conversationCmd.AddCommand(conversationParticipantsCmd)
	conversationCmd.AddCommand(conversationJoinCmd)
	conversationCmd.AddCommand(conversationLeaveCmd)
	conversationCmd.AddCommand(conversationCatchUpCmd)

	// List flags (on both parent and list subcommand)
	for _, cmd := range []*cobra.Command{conversationCmd, conversationListCmd} {
		cmd.Flags().StringVar(&convKind, "kind", "", "Filter by kind (direct, group)")
		cmd.Flags().StringVar(&convSurface, "surface", "", "Filter by surface (native, discord, slack, etc.)")
		cmd.Flags().StringVar(&convProject, "project", "", "Filter by project ID")
		cmd.Flags().BoolVar(&convJSON, "json", false, "Output in JSON format")
		cmd.Flags().IntVar(&convLimit, "limit", 50, "Maximum number of conversations to show")
	}

	// Messages flags
	conversationMessagesCmd.Flags().IntVar(&convMsgLimit, "limit", 25, "Maximum number of messages to show")
	conversationMessagesCmd.Flags().StringVar(&convMsgBefore, "before", "", "Show messages before this time (RFC3339)")
	conversationMessagesCmd.Flags().StringVar(&convMsgAfter, "after", "", "Show messages after this time (RFC3339)")
	conversationMessagesCmd.Flags().BoolVar(&convMsgJSON, "json", false, "Output in JSON format")

	// Create flags
	conversationCreateCmd.Flags().StringVar(&convProject, "project", "", "Project ID (defaults to current project)")
	conversationCreateCmd.Flags().BoolVar(&convCreateJSON, "json", false, "Output in JSON format")

	// Get flags
	conversationGetCmd.Flags().BoolVar(&convGetJSON, "json", false, "Output in JSON format")
	conversationGetMessageCmd.Flags().BoolVar(&convGetMessageJSON, "json", false, "Output in JSON format")

	// Participants flags
	conversationParticipantsCmd.Flags().BoolVar(&convParticipantsJSON, "json", false, "Output in JSON format")

	// Catch-up flags
	conversationCatchUpCmd.Flags().StringVar(&convCatchUpSince, "since", "1h", "Show messages from this duration ago (e.g. 30m, 2h)")
	conversationCatchUpCmd.Flags().BoolVar(&convCatchUpJSON, "json", false, "Output in JSON format")
}

func runConversationList(cmd *cobra.Command, args []string) error {
	if convJSON {
		outputFormat = "json"
	}

	_, client, err := requireHubClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	opts := &hubclient.ListConversationsOptions{
		Kind:      convKind,
		Surface:   convSurface,
		ProjectID: convProject,
		Limit:     convLimit,
	}

	result, err := client.Conversations().List(ctx, opts)
	if err != nil {
		return fmt.Errorf("failed to list conversations: %w", err)
	}

	if isJSONOutput() {
		return outputJSON(result)
	}

	if len(result.Conversations) == 0 {
		fmt.Println("No conversations found.")
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tKIND\tSURFACE\tNAME\tDEFAULT AGENT\tLAST ACTIVITY")
	for _, conv := range result.Conversations {
		shortID := truncateRunes(conv.ID, 12, false)
		name := truncateRunes(conv.DisplayName, 20, true)
		defaultAgent := ""
		if conv.DefaultAgentID != nil {
			defaultAgent = truncateRunes(*conv.DefaultAgentID, 12, false)
		}
		lastActivity := formatTimeAgo(conv.LastActivityAt)
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			shortID, conv.Kind, conv.Surface, name, defaultAgent, lastActivity)
	}
	return tw.Flush()
}

func runConversationMessages(cmd *cobra.Command, args []string) error {
	if convMsgJSON {
		outputFormat = "json"
	}

	_, client, err := requireHubClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	conversationID, err := resolveConversationRef(ctx, client, args[0])
	if err != nil {
		return err
	}

	opts := &hubclient.ConversationMessagesOptions{
		Limit:  convMsgLimit,
		Before: convMsgBefore,
		After:  convMsgAfter,
	}

	result, err := client.Conversations().ListMessages(ctx, conversationID, opts)
	if err != nil {
		return fmt.Errorf("failed to list messages: %w", err)
	}

	if isJSONOutput() {
		return outputJSON(result)
	}

	if len(result.Items) == 0 {
		fmt.Println("No messages found.")
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "TIME\tFROM\tMESSAGE")
	for _, msg := range result.Items {
		timeStr := msg.CreatedAt.Format("15:04:05")
		from := truncateRunes(msg.Sender, 20, true)
		body := truncateRunes(msg.Msg, 60, true)
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", timeStr, from, body)
	}
	return tw.Flush()
}

func runConversationCreate(cmd *cobra.Command, args []string) error {
	if convCreateJSON {
		outputFormat = "json"
	}

	settings, client, err := requireHubClient()
	if err != nil {
		return err
	}

	// §3.6: --project documents "defaults to current project" but previously
	// sent "" as-is, relying entirely on the server's agent-token fallback —
	// which only applies to agent callers, so a human CLI user with no
	// --project got a NULL-project group with no web audience. Fill it from
	// the resolved hub context (flag > hub-linked project > local project),
	// the same resolution every other project-scoped command uses. If none
	// resolves, leave it empty: the server-side agent-token fallback (or,
	// for a user identity with neither, a clear "projectId is required"
	// error) still applies.
	var projectResolveErr error
	if convProject == "" {
		if resolved, resolveErr := resolveProjectID(settings, convProject); resolveErr == nil {
			convProject = resolved
		} else {
			projectResolveErr = resolveErr
		}
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	req := &hubclient.CreateConversationRequest{
		DisplayName: args[0],
		ProjectID:   convProject,
		Kind:        "group",
	}

	conv, err := client.Conversations().Create(ctx, req)
	if err != nil {
		// If we couldn't resolve a project locally and the server's 400 is
		// specifically the projectId-required guard, the resolveProjectID
		// error ("Use --project flag or link this project with 'scion hub
		// link'") is the more actionable message — surface both. Checking
		// the message (not just "any 400") matters: an agent whose local
		// settings don't resolve a project but whose token does still hits
		// projectResolveErr != nil, and a 400 for an unrelated reason (e.g.
		// an invalid name) must not be misreported as a project problem.
		var apiErr *apiclient.APIError
		if projectResolveErr != nil && errors.As(err, &apiErr) && apiErr.IsBadRequest() &&
			strings.Contains(apiErr.Message, "projectId is required") {
			return fmt.Errorf("failed to create conversation: %w (%v)", err, projectResolveErr)
		}
		return fmt.Errorf("failed to create conversation: %w", err)
	}

	if isJSONOutput() {
		return outputJSON(conv)
	}

	if len(conv.Participants) == 0 {
		// See handleCreateConversation's AddParticipant-failure path
		// (pkg/hub/handlers_conversations.go): the conversation exists and
		// is visible on the web, but until a participant row exists the
		// creator can't read it back via `scion conversation
		// show/messages/participants` or see it in `scion conversation list`.
		fmt.Fprintf(os.Stderr, "warning: created conversation %s but could not add you as a participant; "+
			"it won't appear in `scion conversation list` or be readable via `scion conversation show` yet\n", conv.ID)
	}

	fmt.Printf("Conversation created: %s\n", conv.ID)
	fmt.Printf("  Name:    %s\n", conv.DisplayName)
	fmt.Printf("  Kind:    %s\n", conv.Kind)
	fmt.Printf("  Surface: %s\n", conv.Surface)
	return nil
}

func runConversationGet(cmd *cobra.Command, args []string) error {
	if convGetJSON {
		outputFormat = "json"
	}

	_, client, err := requireHubClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	conversationID, err := resolveConversationRef(ctx, client, args[0])
	if err != nil {
		return err
	}

	conv, err := client.Conversations().Get(ctx, conversationID)
	if err != nil {
		return fmt.Errorf("failed to get conversation: %w", err)
	}

	if isJSONOutput() {
		return outputJSON(conv)
	}

	fmt.Printf("ID:             %s\n", conv.ID)
	fmt.Printf("Kind:           %s\n", conv.Kind)
	fmt.Printf("Surface:        %s\n", conv.Surface)
	fmt.Printf("Name:           %s\n", conv.DisplayName)
	if conv.DefaultAgentID != nil {
		fmt.Printf("Default Agent:  %s\n", *conv.DefaultAgentID)
	}
	fmt.Printf("Created:        %s\n", conv.CreatedAt.Format(time.RFC3339))
	fmt.Printf("Last Activity:  %s\n", conv.LastActivityAt.Format(time.RFC3339))

	if len(conv.Participants) > 0 {
		fmt.Println("\nParticipants:")
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "  KIND\tID\tROLE")
		for _, p := range conv.Participants {
			shortID := truncateRunes(p.PrincipalID, 12, false)
			_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\n", p.PrincipalKind, shortID, p.Role)
		}
		_ = tw.Flush()
	}
	return nil
}

func runConversationGetMessage(cmd *cobra.Command, args []string) error {
	if convGetMessageJSON {
		outputFormat = "json"
	}

	_, client, err := requireHubClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	conversationID, err := resolveConversationRef(ctx, client, args[0])
	if err != nil {
		return err
	}

	msg, err := client.Conversations().GetMessage(ctx, conversationID, args[1])
	if err != nil {
		return fmt.Errorf("failed to get message: %w", err)
	}

	if isJSONOutput() {
		return outputJSON(msg)
	}

	senderProjectID := ""
	if msg.SenderProjectID != nil {
		senderProjectID = *msg.SenderProjectID
	}
	recipientProjectID := ""
	if msg.RecipientProjectID != nil {
		recipientProjectID = *msg.RecipientProjectID
	}
	dispatchedAt := ""
	if msg.DispatchedAt != nil {
		dispatchedAt = msg.DispatchedAt.Format(time.RFC3339)
	}
	dispatchFailureReason := ""
	if msg.DispatchFailureReason != nil {
		dispatchFailureReason = *msg.DispatchFailureReason
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(tw, "ID\t%s\n", msg.ID)
	_, _ = fmt.Fprintf(tw, "CONVERSATION ID\t%s\n", msg.ConversationID)
	_, _ = fmt.Fprintf(tw, "PROJECT ID\t%s\n", msg.ProjectID)
	_, _ = fmt.Fprintf(tw, "AGENT ID\t%s\n", msg.AgentID)
	_, _ = fmt.Fprintf(tw, "SENDER\t%s\n", msg.Sender)
	_, _ = fmt.Fprintf(tw, "SENDER ID\t%s\n", msg.SenderID)
	_, _ = fmt.Fprintf(tw, "SENDER PROJECT ID\t%s\n", senderProjectID)
	_, _ = fmt.Fprintf(tw, "RECIPIENT\t%s\n", msg.Recipient)
	_, _ = fmt.Fprintf(tw, "RECIPIENT ID\t%s\n", msg.RecipientID)
	_, _ = fmt.Fprintf(tw, "RECIPIENT PROJECT ID\t%s\n", recipientProjectID)
	_, _ = fmt.Fprintf(tw, "MESSAGE\t%s\n", msg.Msg)
	_, _ = fmt.Fprintf(tw, "TYPE\t%s\n", msg.Type)
	_, _ = fmt.Fprintf(tw, "URGENT\t%t\n", msg.Urgent)
	_, _ = fmt.Fprintf(tw, "BROADCASTED\t%t\n", msg.Broadcasted)
	_, _ = fmt.Fprintf(tw, "READ\t%t\n", msg.Read)
	_, _ = fmt.Fprintf(tw, "GROUP ID\t%s\n", msg.GroupID)
	_, _ = fmt.Fprintf(tw, "CHANNEL\t%s\n", msg.Channel)
	_, _ = fmt.Fprintf(tw, "THREAD ID\t%s\n", msg.ThreadID)
	_, _ = fmt.Fprintf(tw, "CREATED\t%s\n", msg.CreatedAt.Format(time.RFC3339))
	_, _ = fmt.Fprintf(tw, "DISPATCH STATE\t%s\n", msg.DispatchState)
	_, _ = fmt.Fprintf(tw, "DISPATCHED AT\t%s\n", dispatchedAt)
	_, _ = fmt.Fprintf(tw, "DISPATCH FAILURE REASON\t%s\n", dispatchFailureReason)
	return tw.Flush()
}

func runConversationSetDefault(cmd *cobra.Command, args []string) error {
	_, client, err := requireHubClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	conversationID, err := resolveConversationRef(ctx, client, args[0])
	if err != nil {
		return err
	}

	agentID := args[1]

	if err := client.Conversations().SetDefaultAgent(ctx, conversationID, agentID); err != nil {
		return fmt.Errorf("failed to set default agent: %w", err)
	}

	fmt.Printf("Default agent set to %s for conversation %s.\n", agentID, conversationID)
	return nil
}

func runConversationParticipants(cmd *cobra.Command, args []string) error {
	if convParticipantsJSON {
		outputFormat = "json"
	}

	_, client, err := requireHubClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	conversationID, err := resolveConversationRef(ctx, client, args[0])
	if err != nil {
		return err
	}

	conv, err := client.Conversations().Get(ctx, conversationID)
	if err != nil {
		return fmt.Errorf("failed to get conversation: %w", err)
	}

	if isJSONOutput() {
		return outputJSON(conv.Participants)
	}

	if len(conv.Participants) == 0 {
		fmt.Println("No participants found.")
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "KIND\tID\tROLE\tJOINED")
	for _, p := range conv.Participants {
		joined := formatTimeAgo(p.JoinedAt)
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", p.PrincipalKind, p.PrincipalID, p.Role, joined)
	}
	return tw.Flush()
}

func runConversationJoin(cmd *cobra.Command, args []string) error {
	_, client, err := requireHubClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	conversationID, err := resolveConversationRef(ctx, client, args[0])
	if err != nil {
		return err
	}

	req := &hubclient.AddParticipantRequest{
		PrincipalKind: args[1],
		PrincipalID:   args[2],
	}

	if _, err := client.Conversations().AddParticipant(ctx, conversationID, req); err != nil {
		return fmt.Errorf("failed to add participant: %w", err)
	}

	fmt.Printf("Added %s %s to conversation %s\n", req.PrincipalKind, req.PrincipalID, conversationID)
	return nil
}

func runConversationLeave(cmd *cobra.Command, args []string) error {
	_, client, err := requireHubClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	conversationID, err := resolveConversationRef(ctx, client, args[0])
	if err != nil {
		return err
	}

	if err := client.Conversations().Leave(ctx, conversationID); err != nil {
		return fmt.Errorf("failed to leave conversation: %w", err)
	}

	fmt.Printf("Left conversation %s\n", conversationID)
	return nil
}

func runConversationCatchUp(cmd *cobra.Command, args []string) error {
	if convCatchUpJSON {
		outputFormat = "json"
	}

	_, client, err := requireHubClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	conversationID, err := resolveConversationRef(ctx, client, args[0])
	if err != nil {
		return err
	}

	since, err := time.ParseDuration(convCatchUpSince)
	if err != nil {
		return fmt.Errorf("invalid --since value %q: %w", convCatchUpSince, err)
	}

	afterTime := time.Now().UTC().Add(-since).Format(time.RFC3339)

	opts := &hubclient.ConversationMessagesOptions{
		After: afterTime,
	}

	result, err := client.Conversations().ListMessages(ctx, conversationID, opts)
	if err != nil {
		return fmt.Errorf("failed to list messages: %w", err)
	}

	if isJSONOutput() {
		return outputJSON(result)
	}

	if len(result.Items) == 0 {
		fmt.Printf("No messages in the last %s.\n", convCatchUpSince)
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "TIME\tFROM\tMESSAGE")
	for _, msg := range result.Items {
		timeStr := msg.CreatedAt.Format("15:04:05")
		from := truncateRunes(msg.Sender, 20, true)
		body := truncateRunes(msg.Msg, 60, true)
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", timeStr, from, body)
	}
	return tw.Flush()
}

// resolveConversationRef resolves a conversation reference string to a conversation ID.
// Supports conv:<uuid> directly. For @agent and #thread, it first lists the caller's
// conversations and tries to match.
func resolveConversationRef(ctx context.Context, client hubclient.Client, refStr string) (string, error) {
	ref, err := messaging.ParseReference(refStr)
	if err != nil {
		return "", fmt.Errorf("invalid conversation reference %q: %w", refStr, err)
	}

	switch ref.Kind {
	case messaging.RefConversation:
		// conv:<uuid> — direct ID
		return ref.Value, nil

	case messaging.RefAgent:
		// @<agent-name> — find direct conversation with this agent
		convs, listErr := client.Conversations().List(ctx, &hubclient.ListConversationsOptions{
			Kind: "direct",
		})
		if listErr != nil {
			return "", fmt.Errorf("failed to list conversations for reference resolution: %w", listErr)
		}
		// Look for a conversation that matches this agent name in participants or display name
		for _, conv := range convs.Conversations {
			if conv.DisplayName == ref.Value || conv.DisplayName == "@"+ref.Value {
				return conv.ID, nil
			}
		}
		return "", fmt.Errorf("no conversation found for @%s", ref.Value)

	case messaging.RefThread:
		// #<thread-name> — find group conversation by name
		convs, listErr := client.Conversations().List(ctx, &hubclient.ListConversationsOptions{
			Kind: "group",
		})
		if listErr != nil {
			return "", fmt.Errorf("failed to list conversations for reference resolution: %w", listErr)
		}
		for _, conv := range convs.Conversations {
			if conv.DisplayName == ref.Value {
				return conv.ID, nil
			}
		}
		return "", fmt.Errorf("no conversation found for #%s", ref.Value)

	default:
		return "", fmt.Errorf("unsupported conversation reference type: %s", refStr)
	}
}

// truncateRunes truncates a string to at most max runes, preserving multi-byte
// characters. When ellipsis is true and truncation occurs, the last 3 runes
// are replaced with "..." so the total visual length stays at max.
func truncateRunes(s string, max int, ellipsis bool) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if ellipsis && max > 3 {
		return string(runes[:max-3]) + "..."
	}
	return string(runes[:max])
}

// formatTimeAgo formats a time as a human-readable relative time string.
func formatTimeAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		m := int(d.Minutes())
		if m == 1 {
			return "1m ago"
		}
		return fmt.Sprintf("%dm ago", m)
	case d < 24*time.Hour:
		h := int(d.Hours())
		if h == 1 {
			return "1h ago"
		}
		return fmt.Sprintf("%dh ago", h)
	default:
		days := int(d.Hours() / 24)
		if days == 1 {
			return "1d ago"
		}
		return fmt.Sprintf("%dd ago", days)
	}
}
