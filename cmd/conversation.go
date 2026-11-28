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
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

var (
	convKind    string
	convSurface string
	convProject string
	convJSON    bool
	convLimit   int
	convAll     bool

	convMsgLimit  int
	convMsgAll    bool
	convMsgBefore string
	convMsgAfter  string
	convMsgJSON   bool
	convMsgFull   bool

	convCreateJSON bool

	convGetJSON                    bool
	convGetMessageJSON             bool
	convGetMessageBody             bool
	convGetMessageSaveAttachments  string
	convGetMessageAttachments      []string
	convGetMessageForce            bool
	convGetMessageIncludePublished bool

	convParticipantsJSON bool

	convCatchUpSince string
	convCatchUpLimit int
	convCatchUpAll   bool
	convCatchUpJSON  bool
	convCatchUpFull  bool
)

// conversationCmd is the top-level command for conversation management.
var conversationCmd = &cobra.Command{
	Use:     "conversation",
	Aliases: []string{"conv"},
	Short:   "Manage conversations",
	Long: `View and manage conversations you participate in.

Conversations go through the hub (start the local one with 'scion server start',
or add a remote one with 'scion hub add <name> <url>' and select it with
'scion use --hub <name>').

Commands:
  scion conversation list                       List your conversations
  scion conversation messages <conv-ref>        View messages in a conversation
  scion conversation get <conv-ref>             Get conversation details
  scion conversation get-message <ref> <msg-id> Get a message from a conversation
  scion conversation create <name>              Create a new group conversation
  scion conversation set-default <ref> <agent>  Set default agent for a conversation
  scion conversation group ...                  Manage project thread groups

Conversation references:
  conv:<uuid>     Direct conversation ID (full 36-character UUID)
  <uuid>          Bare 36-character conversation ID
  @<agent-name>   Agent DM conversation
  #<thread-name>  Named thread conversation`,
	Args: cobra.NoArgs,
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
  scion conversation list --limit 10
  scion conversation list --all`,
	Args: cobra.NoArgs,
	RunE: runConversationList,
}

// conversationMessagesCmd shows messages in a conversation.
var conversationMessagesCmd = &cobra.Command{
	Use:   "messages <conversation-ref>",
	Short: "View messages in a conversation",
	Long: `View messages in a conversation, in chronological order.

Pass --full to print complete untruncated message bodies and message IDs.

Conversation references:
  conv:<uuid>     Direct conversation ID (full 36-character UUID)
  <uuid>          Bare 36-character conversation ID
  @<agent-name>   Agent DM conversation
  #<thread-name>  Named thread conversation

Examples:
  scion conversation messages conv:a1b2c3d4-...
  scion conversation messages a1b2c3d4-... --full
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

A name is 1 to 100 characters after trimming, must start with an ASCII
letter or digit (a-z, A-Z, 0-9), and may contain only ASCII letters,
digits, spaces, underscores (_), and hyphens (-).

Examples:
  scion conversation create "Design Discussion"
  scion conversation create "Sprint Planning" --project <project-id>
  scion conversation create "Debug Thread" --json
  scion conversation create "release-notes" --group Release

--group files the new conversation into a thread group of the same project
(a group ID, or a name resolved within the project).`,
	Args: cobra.ExactArgs(1),
	RunE: runConversationCreate,
}

// conversationGetCmd gets conversation details.
var conversationGetCmd = &cobra.Command{
	Use:   "get <conversation-ref>",
	Short: "Get conversation details",
	Long: `Get details of a single conversation including participants.

The MSG MODE column shows each agent participant's message mode (see
scion set-message-mode --help) in the conversation's project; for a
conversation without a project (a DM or legacy group), in the calling
agent's project. It shows "-" for users, for any agent outside that
project, and for every agent when a user reads a conversation without a
project.

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
  scion conversation get-message conv:a1b2c3d4-... msg-uuid-here --json
  scion conversation get-message conv:a1b2c3d4-... msg-uuid-here --body

--body prints only the message's body (msg.Msg) to stdout, with no added
bytes, and exits 0. It is the fetch command a large-DM offload stub names
(ptone/scion#2257) — it is silent to make "> file" redirection exact.`,
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

The MSG MODE column shows each agent participant's message mode (see
scion set-message-mode --help) in the conversation's project; for a
conversation without a project (a DM or legacy group), in the calling
agent's project. It shows "-" for users, for any agent outside that
project, and for every agent when a user reads a conversation without a
project.

Examples:
  scion conversation participants conv:a1b2c3d4-...
  scion conversation participants @my-agent --json`,
	Args: cobra.ExactArgs(1),
	RunE: runConversationParticipants,
}

// conversationJoinCmd adds a participant to a conversation.
var conversationJoinCmd = &cobra.Command{
	Use:   "join <conversation-ref> <principal-kind> <principal>",
	Short: "Add a participant to a conversation",
	Long: `Add a participant to a conversation.

The principal argument accepts a user ID, email, or handle (display name,
hyphenated display-name slug, or email local part) for kind "user", and an
agent ID, slug, or name for kind "agent".

Examples:
  scion conversation join conv:a1b2c3d4-... agent my-agent
  scion conversation join conv:a1b2c3d4-... agent 0b56e8d0-...
  scion conversation join #design-thread user alice@example.com
  scion conversation join #design-thread user alice-smith`,
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
	Long: `Show recent messages in a conversation, in chronological order. Defaults to the last hour.

Pass --full to print complete untruncated message bodies and message IDs.

Examples:
  scion conversation catch-up conv:a1b2c3d4-...
  scion conversation catch-up a1b2c3d4-... --full
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
		cmd.Flags().BoolVar(&convAll, "all", false, "Return all conversations without a limit")
	}

	// Messages flags
	conversationMessagesCmd.Flags().IntVar(&convMsgLimit, "limit", 25, "Maximum number of messages to show")
	conversationMessagesCmd.Flags().BoolVar(&convMsgAll, "all", false, "Return all messages without a limit")
	conversationMessagesCmd.Flags().StringVar(&convMsgBefore, "before", "", "Show messages before this time (RFC3339)")
	conversationMessagesCmd.Flags().StringVar(&convMsgAfter, "after", "", "Show messages after this time (RFC3339)")
	conversationMessagesCmd.Flags().BoolVar(&convMsgJSON, "json", false, "Output in JSON format")
	conversationMessagesCmd.Flags().BoolVar(&convMsgFull, "full", false, "Show full untruncated message text and message IDs")

	// Create flags
	conversationCreateCmd.Flags().StringVar(&convProject, "project", "", "Project ID (defaults to current project)")
	conversationCreateCmd.Flags().BoolVar(&convCreateJSON, "json", false, "Output in JSON format")

	// Get flags
	conversationGetCmd.Flags().BoolVar(&convGetJSON, "json", false, "Output in JSON format")
	conversationGetMessageCmd.Flags().BoolVar(&convGetMessageJSON, "json", false, "Output in JSON format")
	conversationGetMessageCmd.Flags().BoolVar(&convGetMessageBody, "body", false, "Print only the message body to stdout (no added bytes)")
	conversationGetMessageCmd.Flags().StringVar(&convGetMessageSaveAttachments, "save-attachments", "", "Save message attachments into this directory")
	conversationGetMessageCmd.Flags().StringArrayVar(&convGetMessageAttachments, "attachment", nil, "Save only the named attachment (exact ID or name; repeatable)")
	conversationGetMessageCmd.Flags().BoolVar(&convGetMessageForce, "force", false, "Overwrite existing files when saving attachments")
	conversationGetMessageCmd.Flags().BoolVar(&convGetMessageIncludePublished, "include-published", false, "Download attachments even when already published as an artifact")

	// Participants flags
	conversationParticipantsCmd.Flags().BoolVar(&convParticipantsJSON, "json", false, "Output in JSON format")

	// Catch-up flags
	conversationCatchUpCmd.Flags().StringVar(&convCatchUpSince, "since", "1h", "Show messages from this duration ago (e.g. 30m, 2h)")
	conversationCatchUpCmd.Flags().IntVar(&convCatchUpLimit, "limit", 50, "Maximum number of messages to show")
	conversationCatchUpCmd.Flags().BoolVar(&convCatchUpAll, "all", false, "Return all messages without a limit")
	conversationCatchUpCmd.Flags().BoolVar(&convCatchUpJSON, "json", false, "Output in JSON format")
	conversationCatchUpCmd.Flags().BoolVar(&convCatchUpFull, "full", false, "Show full untruncated message text and message IDs")
}

func runConversationList(cmd *cobra.Command, args []string) error {
	if err := validateListLimitFlags(cmd, convLimit, convAll, "limit", "all"); err != nil {
		return err
	}

	if convJSON {
		outputFormat = "json"
	}

	settings, client, err := requireHubClient()
	if err != nil {
		return err
	}

	ctx, cancel := listRequestContext(cmdContext(cmd), convAll)
	defer cancel()

	limit := convLimit
	if convAll {
		limit = 0
	}

	opts := &hubclient.ListConversationsOptions{
		Kind:    convKind,
		Surface: convSurface,
		Limit:   limit,
	}

	// Review round 1 finding #2 / design §3.2 addendum: project_id keeps
	// its existing narrowing semantics (it drops every conversation whose
	// ProjectID doesn't match, including every DM, which has ProjectID ==
	// nil), so it must stay opt-in via an explicit --project. The
	// hub-context default instead drives the purely additive
	// include_project_groups union, which can only add groups the caller
	// wasn't already listing — it never narrows out DMs or other
	// projects' conversations. Unlike create, an unresolved project here
	// is not an error: it just falls back to today's behavior (nothing
	// sent, so the caller's participations plus canonical DMs).
	if convProject != "" {
		opts.ProjectID = convProject
	} else if resolved, resolveErr := resolveProjectID(settings, ""); resolveErr == nil {
		opts.IncludeProjectGroups = resolved
	}

	result, err := client.Conversations().List(ctx, opts)
	if err != nil {
		return fmt.Errorf("failed to list conversations: %w", err)
	}

	if isJSONOutput() {
		if err := outputJSON(result); err != nil {
			return err
		}
		printListTruncationNotice(os.Stderr, len(result.Conversations), result.Total, result.Truncated, "conversations", "limit", "all")
		return nil
	}

	if len(result.Conversations) == 0 {
		fmt.Println("No conversations found.")
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tKIND\tSURFACE\tNAME\tDEFAULT AGENT\tLAST ACTIVITY")
	// IDs are printed in full so a listed ID can be passed back as
	// conv:<id> (ptone/scion#7131).
	for _, conv := range result.Conversations {
		name := truncateRunes(conversationListName(conv), 20, true)
		defaultAgent := ""
		if conv.DefaultAgentID != nil {
			defaultAgent = *conv.DefaultAgentID
		}
		lastActivity := clitime.Ago(conv.LastActivityAt)
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			conv.ID, conv.Kind, conv.Surface, name, defaultAgent, lastActivity)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	printListTruncationNotice(os.Stderr, len(result.Conversations), result.Total, result.Truncated, "conversations", "limit", "all")
	return nil
}

func runConversationMessages(cmd *cobra.Command, args []string) error {
	if err := validateListLimitFlags(cmd, convMsgLimit, convMsgAll, "limit", "all"); err != nil {
		return err
	}

	if convMsgJSON {
		outputFormat = "json"
	}

	settings, client, err := requireHubClient()
	if err != nil {
		return err
	}

	ctx, cancel := listRequestContext(cmdContext(cmd), convMsgAll)
	defer cancel()

	conversationID, err := resolveConversationRef(ctx, client, optionalProjectID(settings), args[0])
	if err != nil {
		return err
	}

	pageRes, err := fetchListPages(ctx, convMsgLimit, convMsgAll, 200, func(ctx context.Context, pageLimit int, cursor string) ([]store.Message, string, int, error) {
		opts := &hubclient.ConversationMessagesOptions{
			Limit:  pageLimit,
			Cursor: cursor,
			Before: convMsgBefore,
			After:  convMsgAfter,
		}
		res, err := client.Conversations().ListMessages(ctx, conversationID, opts)
		if err != nil {
			return nil, "", 0, err
		}
		return res.Items, res.NextCursor, res.TotalCount, nil
	})
	if err != nil {
		return fmt.Errorf("failed to list messages: %w", err)
	}

	if isJSONOutput() {
		if err := outputJSON(messageListJSONOutput(pageRes)); err != nil {
			return err
		}
		printListTruncationNotice(os.Stderr, len(pageRes.Items), pageRes.TotalCount, pageRes.Truncated, "messages", "limit", "all")
		return nil
	}

	if len(pageRes.Items) == 0 {
		fmt.Println("No messages found.")
		return nil
	}

	items := chronologicalMessages(pageRes.Items)
	label := newUserLabeler(ctx, client)
	if convMsgFull {
		if err := writeConversationMessagesFull(os.Stdout, items, label); err != nil {
			return err
		}
		printListTruncationNotice(os.Stderr, len(pageRes.Items), pageRes.TotalCount, pageRes.Truncated, "messages", "limit", "all")
		return nil
	}
	if err := writeConversationMessagesTable(os.Stdout, items, label, false); err != nil {
		return err
	}
	printListTruncationNotice(os.Stderr, len(pageRes.Items), pageRes.TotalCount, pageRes.Truncated, "messages", "limit", "all")
	return nil
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

	// --group resolves within the same project the conversation is created
	// in (ptone/scion#4424, design section 4); the hub also requires the
	// group's project to equal the conversation's.
	var group *hubclient.ThreadGroup
	if convCreateGroup != "" {
		group, err = resolveThreadGroup(ctx, client, convProject, projectResolveErr, convCreateGroup)
		if err != nil {
			return err
		}
		req.ThreadGroupID = group.ID
	}

	conv, err := client.Conversations().Create(ctx, req)
	if err != nil {
		if req.ThreadGroupID != "" && isThreadGroupsDisabledCreateError(err) {
			return fmt.Errorf("failed to create conversation: %w", &hubclient.ThreadGroupsNotEnabledError{Err: err})
		}
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
		// get/messages/participants` or see it in `scion conversation list`.
		fmt.Fprintf(os.Stderr, "warning: created conversation %s but could not add you as a participant; "+
			"it won't appear in `scion conversation list` or be readable via `scion conversation get` yet\n", conv.ID)
	}

	fmt.Printf("Conversation created: %s\n", conv.ID)
	fmt.Printf("  Name:    %s\n", conv.DisplayName)
	fmt.Printf("  Kind:    %s\n", conv.Kind)
	fmt.Printf("  Surface: %s\n", conv.Surface)
	if group != nil {
		fmt.Printf("  Group:   %s\n", group.Name)
	}
	return nil
}

// isThreadGroupsDisabledCreateError reports whether err is an older hub's
// 400 for a threadGroupId sent while its thread groups experiment is off
// (ptone/scion#5546 removed it). --group normally lists the
// project's groups first, which already reports such a hub; this covers a
// group ID used with no resolved project.
func isThreadGroupsDisabledCreateError(err error) bool {
	var apiErr *apiclient.APIError
	return errors.As(err, &apiErr) && apiErr.IsBadRequest() &&
		strings.Contains(apiErr.Message, "thread groups are not enabled")
}

func runConversationGet(cmd *cobra.Command, args []string) error {
	if convGetJSON {
		outputFormat = "json"
	}

	settings, client, err := requireHubClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	conversationID, err := resolveConversationRef(ctx, client, optionalProjectID(settings), args[0])
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
	fmt.Printf("Created:        %s\n", clitime.Format(conv.CreatedAt, clitime.Full))
	fmt.Printf("Last Activity:  %s\n", clitime.Format(conv.LastActivityAt, clitime.Full))

	writeConversationGetParticipants(os.Stdout, conv.Participants)
	return nil
}

// participantModeCell is the MSG MODE cell for a participant: the agent's
// message mode, or "-" for a user or an agent whose mode the hub did not
// report (an agent outside the conversation's project, or any agent when a
// user reads a conversation without a project).
func participantModeCell(p hubclient.ConversationParticipantDetail) string {
	if p.MessageMode == "" {
		return "-"
	}
	return p.MessageMode
}

// conversationModeHint is printed under the conversation get participants
// table when an agent participant has a restricted message mode.
const conversationModeHint = "Agent-to-agent delivery requires compatible message modes; see `scion set-message-mode --help`."

// writeConversationGetParticipants writes the participants section of
// `scion conversation get`, including each agent's message mode
// (ptone/scion#4411).
func writeConversationGetParticipants(w io.Writer, participants []hubclient.ConversationParticipantDetail) {
	if len(participants) == 0 {
		return
	}
	_, _ = fmt.Fprintln(w, "\nParticipants:")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "  KIND\tID\tROLE\tMSG MODE")
	restricted := false
	for _, p := range participants {
		_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", p.PrincipalKind, p.PrincipalID, p.Role, participantModeCell(p))
		switch p.MessageMode {
		case "", "project", "hub":
		default:
			restricted = true
		}
	}
	_ = tw.Flush()
	if restricted {
		_, _ = fmt.Fprintln(w, "\n"+conversationModeHint)
	}
}

// writeConversationParticipantsTable writes the `scion conversation
// participants` table, including each agent's message mode.
func writeConversationParticipantsTable(w io.Writer, participants []hubclient.ConversationParticipantDetail) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "KIND\tID\tROLE\tMSG MODE\tJOINED")
	for _, p := range participants {
		joined := clitime.Ago(p.JoinedAt)
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", p.PrincipalKind, p.PrincipalID, p.Role, participantModeCell(p), joined)
	}
	return tw.Flush()
}

// writeMessageBody writes msg.Msg to w with no added bytes — no trailing
// newline, no label — so a large-DM offload stub's fetch command
// (ptone/scion#2257, `scion conversation get-message ... --body`) reproduces
// exactly the persisted bytes when redirected to a file. Extracted from
// runConversationGetMessage for direct unit testing.
func writeMessageBody(w io.Writer, msg *store.Message) error {
	_, err := io.WriteString(w, msg.Msg)
	return err
}

func runConversationGetMessage(cmd *cobra.Command, args []string) error {
	if convGetMessageBody && convGetMessageSaveAttachments != "" {
		return errors.New("--body cannot be combined with --save-attachments")
	}
	if len(convGetMessageAttachments) > 0 && convGetMessageSaveAttachments == "" {
		return errors.New("--attachment requires --save-attachments")
	}
	if convGetMessageForce && convGetMessageSaveAttachments == "" {
		return errors.New("--force requires --save-attachments")
	}
	if convGetMessageIncludePublished && convGetMessageSaveAttachments == "" {
		return errors.New("--include-published requires --save-attachments")
	}

	if convGetMessageJSON {
		outputFormat = "json"
	}

	settings, client, err := requireHubClient()
	if err != nil {
		return err
	}

	timeout := 30 * time.Second
	if convGetMessageSaveAttachments != "" {
		timeout = 5 * time.Minute
	}
	ctx, cancel := context.WithTimeout(cmdContext(cmd), timeout)
	defer cancel()

	conversationID, err := resolveConversationRef(ctx, client, optionalProjectID(settings), args[0])
	if err != nil {
		return err
	}

	msg, err := client.Conversations().GetMessage(ctx, conversationID, args[1])
	if err != nil {
		return fmt.Errorf("failed to get message: %w", err)
	}

	if convGetMessageBody {
		return writeMessageBody(cmd.OutOrStdout(), msg)
	}

	var saveAnnotations []messageAttachmentJSON
	var saveErr error
	if convGetMessageSaveAttachments != "" {
		saveAnnotations, saveErr = saveMessageAttachments(
			ctx,
			client.Conversations(),
			cmd.ErrOrStderr(),
			conversationID,
			msg,
			convGetMessageSaveAttachments,
			convGetMessageAttachments,
			convGetMessageForce,
			convGetMessageIncludePublished,
		)
	}

	if isJSONOutput() {
		if convGetMessageSaveAttachments != "" && saveAnnotations != nil {
			if jerr := outputMessageWithSaveJSON(cmd.OutOrStdout(), msg, saveAnnotations); jerr != nil {
				return jerr
			}
			return saveErr
		}
		if saveErr != nil {
			return saveErr
		}
		return outputJSON(msg)
	}

	if err := writeGetMessageText(ctx, client, cmd.OutOrStdout(), msg); err != nil {
		return err
	}
	return saveErr
}

func writeGetMessageText(ctx context.Context, client hubclient.Client, out io.Writer, msg *store.Message) error {
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
		dispatchedAt = clitime.Format(*msg.DispatchedAt, clitime.Full)
	}
	dispatchFailureReason := ""
	if msg.DispatchFailureReason != nil {
		dispatchFailureReason = *msg.DispatchFailureReason
	}

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(tw, "ID\t%s\n", msg.ID)
	_, _ = fmt.Fprintf(tw, "CONVERSATION ID\t%s\n", msg.ConversationID)
	_, _ = fmt.Fprintf(tw, "PROJECT ID\t%s\n", msg.ProjectID)
	_, _ = fmt.Fprintf(tw, "AGENT ID\t%s\n", msg.AgentID)
	_, _ = fmt.Fprintf(tw, "SENDER\t%s\n", msg.Sender)
	_, _ = fmt.Fprintf(tw, "SENDER ID\t%s\n", msg.SenderID)
	_, _ = fmt.Fprintf(tw, "SENDER PROJECT ID\t%s\n", senderProjectID)
	// A message sent with an agent delegated credential also names the
	// user who authorized it and the grant; other messages print no rows.
	if msg.AuthorizingUserID != "" {
		_, _ = fmt.Fprintf(tw, "AUTHORIZED BY\t%s\n", newUserLabeler(ctx, client)(msg.AuthorizingUserID))
		_, _ = fmt.Fprintf(tw, "AUTHORIZING USER ID\t%s\n", msg.AuthorizingUserID)
		_, _ = fmt.Fprintf(tw, "DELEGATION GRANT ID\t%s\n", msg.DelegationGrantID)
	}
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
	_, _ = fmt.Fprintf(tw, "CREATED\t%s\n", clitime.Format(msg.CreatedAt, clitime.Full))
	_, _ = fmt.Fprintf(tw, "DISPATCH STATE\t%s\n", msg.DispatchState)
	_, _ = fmt.Fprintf(tw, "DISPATCHED AT\t%s\n", dispatchedAt)
	_, _ = fmt.Fprintf(tw, "DISPATCH FAILURE REASON\t%s\n", dispatchFailureReason)
	if err := tw.Flush(); err != nil {
		return err
	}
	return writeMessageAttachmentsText(out, msg.Attachments)
}

func runConversationSetDefault(cmd *cobra.Command, args []string) error {
	settings, client, err := requireHubClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	conversationID, err := resolveConversationRef(ctx, client, optionalProjectID(settings), args[0])
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

	settings, client, err := requireHubClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	conversationID, err := resolveConversationRef(ctx, client, optionalProjectID(settings), args[0])
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

	return writeConversationParticipantsTable(os.Stdout, conv.Participants)
}

func runConversationJoin(cmd *cobra.Command, args []string) error {
	settings, client, err := requireHubClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	conversationID, err := resolveConversationRef(ctx, client, optionalProjectID(settings), args[0])
	if err != nil {
		return err
	}

	req := &hubclient.AddParticipantRequest{
		PrincipalKind: args[1],
		PrincipalID:   args[2],
	}

	participant, err := client.Conversations().AddParticipant(ctx, conversationID, req)
	if err != nil {
		return fmt.Errorf("failed to add participant: %w", err)
	}

	if participant != nil && participant.PrincipalID != "" && participant.PrincipalID != req.PrincipalID {
		fmt.Printf("Added %s %s (%s) to conversation %s\n", req.PrincipalKind, req.PrincipalID, participant.PrincipalID, conversationID)
	} else {
		fmt.Printf("Added %s %s to conversation %s\n", req.PrincipalKind, req.PrincipalID, conversationID)
	}
	return nil
}

func runConversationLeave(cmd *cobra.Command, args []string) error {
	settings, client, err := requireHubClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	conversationID, err := resolveConversationRef(ctx, client, optionalProjectID(settings), args[0])
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
	if err := validateListLimitFlags(cmd, convCatchUpLimit, convCatchUpAll, "limit", "all"); err != nil {
		return err
	}

	if convCatchUpJSON {
		outputFormat = "json"
	}

	settings, client, err := requireHubClient()
	if err != nil {
		return err
	}

	ctx, cancel := listRequestContext(cmdContext(cmd), convCatchUpAll)
	defer cancel()

	conversationID, err := resolveConversationRef(ctx, client, optionalProjectID(settings), args[0])
	if err != nil {
		return err
	}

	since, err := time.ParseDuration(convCatchUpSince)
	if err != nil {
		return newUsageError("invalid --since value %q: %w", convCatchUpSince, err)
	}

	afterTime := time.Now().UTC().Add(-since).Format(time.RFC3339)

	pageRes, err := fetchListPages(ctx, convCatchUpLimit, convCatchUpAll, 200, func(ctx context.Context, pageLimit int, cursor string) ([]store.Message, string, int, error) {
		opts := &hubclient.ConversationMessagesOptions{
			Limit:  pageLimit,
			Cursor: cursor,
			After:  afterTime,
		}
		res, err := client.Conversations().ListMessages(ctx, conversationID, opts)
		if err != nil {
			return nil, "", 0, err
		}
		return res.Items, res.NextCursor, res.TotalCount, nil
	})
	if err != nil {
		return fmt.Errorf("failed to list messages: %w", err)
	}

	if isJSONOutput() {
		if err := outputJSON(messageListJSONOutput(pageRes)); err != nil {
			return err
		}
		printListTruncationNotice(os.Stderr, len(pageRes.Items), pageRes.TotalCount, pageRes.Truncated, "messages", "limit", "all")
		return nil
	}

	if len(pageRes.Items) == 0 {
		fmt.Printf("No messages in the last %s.\n", convCatchUpSince)
		return nil
	}

	items := chronologicalMessages(pageRes.Items)
	label := newUserLabeler(ctx, client)
	if convCatchUpFull {
		if err := writeConversationMessagesFull(os.Stdout, items, label); err != nil {
			return err
		}
		printListTruncationNotice(os.Stderr, len(pageRes.Items), pageRes.TotalCount, pageRes.Truncated, "messages", "limit", "all")
		return nil
	}
	if err := writeConversationMessagesTable(os.Stdout, items, label, true); err != nil {
		return err
	}
	printListTruncationNotice(os.Stderr, len(pageRes.Items), pageRes.TotalCount, pageRes.Truncated, "messages", "limit", "all")
	return nil
}

// chronologicalMessages returns a copy of msgs sorted from oldest to newest
// by CreatedAt, preserving relative order when timestamps are equal.
func chronologicalMessages(msgs []store.Message) []store.Message {
	sorted := append([]store.Message(nil), msgs...)
	slices.SortStableFunc(sorted, func(a, b store.Message) int {
		return a.CreatedAt.Compare(b.CreatedAt)
	})
	return sorted
}

// writeConversationMessagesTable writes conversation messages in table format
// with TIME, FROM, ATT (attachment count, blank when 0), and MESSAGE columns.
func writeConversationMessagesTable(w io.Writer, items []store.Message, label func(string) string, showDeferred bool) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "TIME\tFROM\tATT\tMESSAGE")
	for i := range items {
		msg := &items[i]
		timeStr := clitime.Format(msg.CreatedAt, clitime.Clock)
		from := truncateRunes(msg.Sender, 20, true)
		attCount := ""
		if len(msg.Attachments) > 0 {
			attCount = fmt.Sprintf("%d", len(msg.Attachments))
		}
		body := truncateRunes(msg.Msg, 60, true)
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", timeStr, from, attCount, body)
		if authorizedBy := messageAuthorizedBy(msg, label); authorizedBy != "" {
			_, _ = fmt.Fprintf(tw, "\t\t\t%s\n", authorizedBy)
		}
		if showDeferred {
			// A deferred message was never dispatched, so its attachments never
			// reached this agent's home (ptone/scion#4596). The hub lists them
			// only to a reader.
			for _, a := range msg.DeferredAttachments {
				_, _ = fmt.Fprintf(tw, "\t\t\t%s\n", messaging.AttachmentNotDeliveredLine(a.Name, a.Size, "the message was deferred"))
			}
		}
	}
	return tw.Flush()
}

// writeConversationMessagesFull prints full untruncated messages in
// chronological order (ptone/scion#8469).
func writeConversationMessagesFull(w io.Writer, items []store.Message, label func(string) string) error {
	for i := range items {
		if i > 0 {
			if _, err := fmt.Fprintln(w); err != nil {
				return err
			}
		}
		msg := &items[i]
		header := fmt.Sprintf("[%s] %s", clitime.Format(msg.CreatedAt, clitime.Full), msg.Sender)
		if msg.ID != "" {
			header += fmt.Sprintf(" (id: %s)", msg.ID)
		}
		if _, err := fmt.Fprintln(w, header); err != nil {
			return err
		}
		if authorizedBy := messageAuthorizedBy(msg, label); authorizedBy != "" {
			if _, err := fmt.Fprintf(w, "  %s\n", authorizedBy); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(w, msg.Msg); err != nil {
			return err
		}
		if err := writeMessageAttachmentsText(w, msg.Attachments); err != nil {
			return err
		}
		for _, a := range msg.DeferredAttachments {
			if _, err := fmt.Fprintf(w, "  %s\n", messaging.AttachmentNotDeliveredLine(a.Name, a.Size, "the message was deferred")); err != nil {
				return err
			}
		}
	}
	return nil
}

// conversationListName is the NAME column of conversation list
// (ptone/scion#3499): DM:<peer> for a direct conversation whose peer the hub
// resolved, else the display name, else the linked thread name, else "-".
func conversationListName(conv hubclient.ConversationDetail) string {
	if conv.Kind == "direct" && conv.DMPeer != nil && conv.DMPeer.Name != "" {
		return "DM:" + conv.DMPeer.Name
	}
	if conv.DisplayName != "" {
		return conv.DisplayName
	}
	if conv.ThreadName != "" {
		return conv.ThreadName
	}
	return "-"
}

// optionalProjectID returns the resolved project ID from settings or "" if no
// project context is configured. Commands that also work in agent mode (where
// the hub infers the caller's project from its token) or via DM list fallback
// use this instead of failing early on an unresolved local project.
func optionalProjectID(settings *config.Settings) string {
	projectID, err := resolveProjectID(settings, "")
	if err != nil {
		return ""
	}
	return projectID
}

// resolveConversationRef resolves a conversation reference string to a conversation ID.
// Supports conv:<uuid> and bare 36-character <uuid> directly. For @agent it
// uses the hub's canonical conversation resolution endpoint, falling back to
// direct-conversation listing by DMPeer only when no project context is set
// (a human caller outside a linked project). For #thread it lists group
// conversations and matches by display name.
func resolveConversationRef(ctx context.Context, client hubclient.Client, projectID, refStr string) (string, error) {
	if len(refStr) == 36 {
		if id, err := uuid.Parse(refStr); err == nil {
			return id.String(), nil
		}
	}

	ref, err := messaging.ParseReference(refStr)
	if err != nil {
		if looksLikeTruncatedUUID(refStr) {
			return "", newUsageError("invalid conversation reference %q: truncated conversation IDs do not resolve; use a full 36-character UUID (conv:<uuid> or <uuid>): %w", refStr, err)
		}
		return "", newUsageError("invalid conversation reference %q: %w", refStr, err)
	}

	switch ref.Kind {
	case messaging.RefConversation:
		// conv:<uuid> — direct ID
		return ref.Value, nil

	case messaging.RefAgent:
		return resolveAgentConversationRef(ctx, client, projectID, ref)

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
		return "", newUsageError("unsupported conversation reference type: %s", refStr)
	}
}

// resolveAgentConversationRef resolves an @<agent> reference to the caller's
// direct conversation with that agent. It queries the hub's canonical read-only
// GET /api/v1/conversations/resolve endpoint (the same canonical agent + DM-key
// resolution path used by messaging). When a project context is in scope (either
// projectID or an agent caller's token project), a not-found result from resolve
// is authoritative and does not fall back to cross-project listing. Only when
// the hub reports that a project ID is required (a human caller outside a linked
// project) does it fall back to matching direct conversations by DMPeer (and
// DisplayName) from Conversations().List.
func resolveAgentConversationRef(ctx context.Context, client hubclient.Client, projectID string, ref *messaging.Reference) (string, error) {
	res, err := client.Messaging().ResolveConversation(ctx, ref.Raw, projectID)
	if err == nil {
		if res != nil && res.Exists && res.Conversation != nil && res.Conversation.ID != "" {
			return res.Conversation.ID, nil
		}
		if res != nil && !res.Exists && res.PeerAgent != nil {
			return "", fmt.Errorf("no direct conversation exists with @%s yet; send a message with 'scion message @%s ...' to start one, or run 'scion conversation list' to see existing conversations", ref.Value, ref.Value)
		}
		return "", fmt.Errorf("no direct conversation found for @%s (run 'scion conversation list' to see existing conversations, or pass conv:<uuid>)", ref.Value)
	}

	var apiErr *apiclient.APIError
	if errors.As(err, &apiErr) && apiErr.IsBadRequest() && strings.Contains(apiErr.Message, "Project ID is required") {
		return resolveAgentDMFromList(ctx, client, ref.Value)
	}
	return "", fmt.Errorf("failed to resolve conversation for @%s: %w", ref.Value, err)
}

func resolveAgentDMFromList(ctx context.Context, client hubclient.Client, agentRef string) (string, error) {
	convs, listErr := client.Conversations().List(ctx, &hubclient.ListConversationsOptions{
		Kind: "direct",
	})
	if listErr != nil {
		return "", fmt.Errorf("failed to list conversations for reference resolution: %w", listErr)
	}

	var matches []hubclient.ConversationDetail
	for _, conv := range convs.Conversations {
		peerMatch := conv.DMPeer != nil && conv.DMPeer.Kind == "agent" &&
			(conv.DMPeer.Name == agentRef || conv.DMPeer.ID == agentRef)
		displayMatch := conv.DisplayName == agentRef || conv.DisplayName == "@"+agentRef
		if peerMatch || displayMatch {
			matches = append(matches, conv)
		}
	}

	switch len(matches) {
	case 1:
		return matches[0].ID, nil
	case 0:
		return "", fmt.Errorf("no direct conversation found for @%s (specify --project <id> or pass conv:<uuid> from 'scion conversation list')", agentRef)
	default:
		ids := make([]string, 0, len(matches))
		for _, m := range matches {
			ids = append(ids, "conv:"+m.ID)
		}
		return "", fmt.Errorf("multiple direct conversations match @%s (%s); pass conv:<uuid> explicitly or specify --project", agentRef, strings.Join(ids, ", "))
	}
}

// looksLikeTruncatedUUID reports whether s looks like a shortened hex/UUID
// prefix (4 to 35 hex/hyphen characters, optionally ending in "..." or "…")
// rather than another reference kind.
func looksLikeTruncatedUUID(s string) bool {
	s = strings.TrimSuffix(strings.TrimSuffix(s, "..."), "…")
	if len(s) < 4 || len(s) >= 36 {
		return false
	}
	hasHex := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F'):
			hasHex = true
		case c == '-':
		default:
			return false
		}
	}
	return hasHex
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
