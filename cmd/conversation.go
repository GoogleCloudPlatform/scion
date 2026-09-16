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
	"text/tabwriter"
	"time"

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

	convGetJSON bool

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

func init() {
	rootCmd.AddCommand(conversationCmd)
	conversationCmd.AddCommand(conversationListCmd)
	conversationCmd.AddCommand(conversationMessagesCmd)
	conversationCmd.AddCommand(conversationCreateCmd)
	conversationCmd.AddCommand(conversationGetCmd)
	conversationCmd.AddCommand(conversationSetDefaultCmd)

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
}

func runConversationList(cmd *cobra.Command, args []string) error {
	if convJSON {
		outputFormat = "json"
	}

	_, client, err := requireHubClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
		shortID := conv.ID
		if len(shortID) > 12 {
			shortID = shortID[:12]
		}
		name := conv.DisplayName
		if len(name) > 20 {
			name = name[:17] + "..."
		}
		defaultAgent := ""
		if conv.DefaultAgentID != nil {
			defaultAgent = *conv.DefaultAgentID
			if len(defaultAgent) > 12 {
				defaultAgent = defaultAgent[:12]
			}
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

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
		from := msg.Sender
		if len(from) > 20 {
			from = from[:17] + "..."
		}
		body := msg.Msg
		if len(body) > 60 {
			body = body[:57] + "..."
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", timeStr, from, body)
	}
	return tw.Flush()
}

func runConversationCreate(cmd *cobra.Command, args []string) error {
	if convCreateJSON {
		outputFormat = "json"
	}

	_, client, err := requireHubClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req := &hubclient.CreateConversationRequest{
		DisplayName: args[0],
		ProjectID:   convProject,
		Kind:        "group",
	}

	conv, err := client.Conversations().Create(ctx, req)
	if err != nil {
		return fmt.Errorf("failed to create conversation: %w", err)
	}

	if isJSONOutput() {
		return outputJSON(conv)
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

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
			shortID := p.PrincipalID
			if len(shortID) > 12 {
				shortID = shortID[:12]
			}
			_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\n", p.PrincipalKind, shortID, p.Role)
		}
		_ = tw.Flush()
	}
	return nil
}

func runConversationSetDefault(cmd *cobra.Command, args []string) error {
	_, client, err := requireHubClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
