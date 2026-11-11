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
	"strings"
	"text/tabwriter"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

// Project-scoped chat thread groups: scion conversation group ...
// (ptone/scion#4424, design section 4). A thread group is a named set of a
// project's group conversations (threads).

var (
	convGroupProject    string
	convGroupListJSON   bool
	convGroupGetJSON    bool
	convGroupCreateJSON bool

	// convCreateGroup is --group on scion conversation create.
	convCreateGroup string
)

// threadGroupRefHelp describes the <group> argument.
const threadGroupRefHelp = `A <group> is a thread group ID, or a group name resolved within the
project (--project <id>, defaulting to the current project). Names are
compared case-insensitively, as the hub compares them. An unknown or
ambiguous name is an error; an ID outside the project is looked up
directly.`

// conversationGroupCmd is the parent of the thread group subcommands.
var conversationGroupCmd = &cobra.Command{
	Use:   "group",
	Short: "Manage project thread groups",
	Long: `Manage the thread groups of a project.

A thread group is a named set of a project's group conversations
(threads), shared by every member of the project. A thread is in at most
one group: adding it to another group moves it.

Commands:
  scion conversation group list                      List the project's thread groups
  scion conversation group get <group>               Show a thread group and its threads
  scion conversation group create <name>             Create a thread group
  scion conversation group rename <group> <new-name> Rename a thread group
  scion conversation group delete <group>            Delete a thread group (users only)
  scion conversation group add <group> <conv-ref>    Add a thread to a thread group
  scion conversation group remove <group> <conv-ref> Remove a thread from a thread group

A <group> is a thread group ID, or a group name resolved within the project
given by the subcommand's --project <id> (defaulting to the current project).
An unknown or ambiguous name is an error.`,
}

var conversationGroupListCmd = &cobra.Command{
	Use:   "list",
	Short: "List the project's thread groups",
	Long: `List the thread groups of a project, sorted by name.

--project defaults to the current project, as for scion conversation create.

Examples:
  scion conversation group list
  scion conversation group list --project <project-id> --json`,
	Args: cobra.NoArgs,
	RunE: runConversationGroupList,
}

var conversationGroupGetCmd = &cobra.Command{
	Use:   "get <group>",
	Short: "Show a thread group and its threads",
	Long: `Show a thread group and its member threads.

` + threadGroupRefHelp + `

Examples:
  scion conversation group get Release
  scion conversation group get 6f1c2b9e-... --json`,
	Args: cobra.ExactArgs(1),
	RunE: runConversationGroupGet,
}

var conversationGroupCreateCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Create a thread group",
	Long: `Create a thread group in a project.

A name is 1 to 100 characters after trimming, with no control characters,
and unique in the project (case-insensitively). --project defaults to the
current project, as for scion conversation create.

Examples:
  scion conversation group create Release
  scion conversation group create "Design reviews" --project <project-id> --json`,
	Args: cobra.ExactArgs(1),
	RunE: runConversationGroupCreate,
}

var conversationGroupRenameCmd = &cobra.Command{
	Use:   "rename <group> <new-name>",
	Short: "Rename a thread group",
	Long: `Rename a thread group.

` + threadGroupRefHelp + `

Examples:
  scion conversation group rename Release "Release 2.0"`,
	Args: cobra.ExactArgs(2),
	RunE: runConversationGroupRename,
}

var conversationGroupDeleteCmd = &cobra.Command{
	Use:   "delete <group>",
	Short: "Delete a thread group",
	Long: `Delete a thread group. Its threads become ungrouped; no thread is deleted.

Only users may delete a thread group; the command is not available in
agent mode, and the hub refuses an agent.

` + threadGroupRefHelp + `

Examples:
  scion conversation group delete Release
  scion conversation group delete Release --yes`,
	Args: cobra.ExactArgs(1),
	RunE: runConversationGroupDelete,
}

var conversationGroupAddCmd = &cobra.Command{
	Use:   "add <group> <conversation-ref>",
	Short: "Add a thread to a thread group",
	Long: `Add a thread (a group conversation of the project) to a thread group.
A thread already in another group of the project moves to this one.

` + threadGroupRefHelp + `

Conversation references:
  conv:<uuid>     Direct conversation ID
  #<thread-name>  Named thread conversation

Examples:
  scion conversation group add Release #release-notes
  scion conversation group add Release conv:a1b2c3d4-...`,
	Args: cobra.ExactArgs(2),
	RunE: runConversationGroupAdd,
}

var conversationGroupRemoveCmd = &cobra.Command{
	Use:   "remove <group> <conversation-ref>",
	Short: "Remove a thread from a thread group",
	Long: `Remove a thread from a thread group. The thread is not deleted.

` + threadGroupRefHelp + `

Conversation references:
  conv:<uuid>     Direct conversation ID
  #<thread-name>  Named thread conversation

Examples:
  scion conversation group remove Release #release-notes`,
	Args: cobra.ExactArgs(2),
	RunE: runConversationGroupRemove,
}

func init() {
	conversationCmd.AddCommand(conversationGroupCmd)
	conversationGroupCmd.AddCommand(conversationGroupListCmd)
	conversationGroupCmd.AddCommand(conversationGroupGetCmd)
	conversationGroupCmd.AddCommand(conversationGroupCreateCmd)
	conversationGroupCmd.AddCommand(conversationGroupRenameCmd)
	conversationGroupCmd.AddCommand(conversationGroupDeleteCmd)
	conversationGroupCmd.AddCommand(conversationGroupAddCmd)
	conversationGroupCmd.AddCommand(conversationGroupRemoveCmd)

	conversationGroupListCmd.Flags().StringVar(&convGroupProject, "project", "", "Project ID (defaults to current project)")
	conversationGroupListCmd.Flags().BoolVar(&convGroupListJSON, "json", false, "Output in JSON format")

	conversationGroupGetCmd.Flags().BoolVar(&convGroupGetJSON, "json", false, "Output in JSON format")

	// Every group subcommand takes the same local --project <id>, so the
	// flag means a project ID throughout the group (as on conversation
	// create); on the item subcommands it selects the project names are
	// resolved in.
	for _, c := range []*cobra.Command{conversationGroupGetCmd, conversationGroupRenameCmd,
		conversationGroupDeleteCmd, conversationGroupAddCmd, conversationGroupRemoveCmd} {
		c.Flags().StringVar(&convGroupProject, "project", "", "Project ID to resolve a group name in (defaults to current project)")
	}

	conversationGroupCreateCmd.Flags().StringVar(&convGroupProject, "project", "", "Project ID (defaults to current project)")
	conversationGroupCreateCmd.Flags().BoolVar(&convGroupCreateJSON, "json", false, "Output in JSON format")

	// --yes is also a root persistent flag; it is repeated here, bound to
	// the same variable, so delete's own help shows it (as hub projects
	// delete does).
	conversationGroupDeleteCmd.Flags().BoolVarP(&autoConfirm, "yes", "y", false, "Answer Yes to the confirmation prompt (required to confirm when stdin is not a terminal)")

	conversationCreateCmd.Flags().StringVar(&convCreateGroup, "group", "", "File the new conversation into this thread group (ID or name in the project)")
}

// threadGroupProjectHint adds the project resolve error to err when the hub
// refused the call because no projectId was sent, as conversation create
// does: the local hint is then the actionable message.
func threadGroupProjectHint(err, resolveErr error) error {
	var apiErr *apiclient.APIError
	if resolveErr != nil && errors.As(err, &apiErr) && apiErr.IsBadRequest() &&
		strings.Contains(apiErr.Message, "projectId is required") {
		return fmt.Errorf("%w (%v)", err, resolveErr)
	}
	return err
}

// threadGroupLabel describes a group in an error or confirmation message.
func threadGroupLabel(g hubclient.ThreadGroup) string {
	return fmt.Sprintf("%q (%s)", g.Name, g.ID)
}

// resolveThreadGroup resolves a <group> argument to a thread group:
//   - groups of projectID whose ID equals ref, or whose name has the same
//     name key as ref (store.ThreadGroupNameKey, the hub's own uniqueness
//     key), are candidates; exactly one resolves, more than one is an
//     ambiguity error naming each;
//   - a UUID that matches no group of projectID is fetched directly, so a
//     group of another project is still reachable by ID (the hub
//     authorizes it);
//   - otherwise an unknown name is an error.
//
// A UUID with no resolved project skips the list. An empty projectID for a
// name lets the hub default it to an agent caller's project.
func resolveThreadGroup(ctx context.Context, client hubclient.Client, projectID string, resolveErr error, ref string) (*hubclient.ThreadGroup, error) {
	_, uuidErr := uuid.Parse(ref)
	isID := uuidErr == nil
	if isID && projectID == "" {
		g, err := client.ThreadGroups().Get(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("failed to get thread group: %w", err)
		}
		return g, nil
	}

	groups, err := client.ThreadGroups().List(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve thread group %q: %w", ref, threadGroupProjectHint(err, resolveErr))
	}
	key := store.ThreadGroupNameKey(ref)
	var matches []hubclient.ThreadGroup
	for _, g := range groups {
		if (isID && g.ID == ref) || store.ThreadGroupNameKey(g.Name) == key {
			matches = append(matches, g)
		}
	}
	switch {
	case len(matches) == 1:
		return &matches[0], nil
	case len(matches) > 1:
		labels := make([]string, len(matches))
		for i, g := range matches {
			labels[i] = threadGroupLabel(g)
		}
		return nil, fmt.Errorf("thread group %q is ambiguous: it matches %s; use the group ID", ref, strings.Join(labels, ", "))
	case isID:
		g, err := client.ThreadGroups().Get(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("failed to get thread group: %w", err)
		}
		return g, nil
	}
	where := "this project"
	if projectID != "" {
		where = "project " + projectID
	}
	return nil, fmt.Errorf("no thread group named %q in %s", ref, where)
}

// threadGroupCommand is the hub client and project of a thread group
// command.
type threadGroupCommand struct {
	client hubclient.Client
	// projectID is the flag, else the hub-linked or local project, as
	// conversation create resolves it. It is "" when none resolves; the hub
	// then defaults it for an agent caller.
	projectID string
	// resolveErr is why projectID did not resolve, for the hint added to
	// the hub's "projectId is required" answer.
	resolveErr error
}

// newThreadGroupCommand loads the hub client and resolves the project for
// a thread group command.
func newThreadGroupCommand(projectFlag string) (*threadGroupCommand, error) {
	settings, client, err := requireHubClient()
	if err != nil {
		return nil, err
	}
	projectID, resolveErr := resolveProjectID(settings, projectFlag)
	return &threadGroupCommand{client: client, projectID: projectID, resolveErr: resolveErr}, nil
}

// resolve resolves a <group> argument within the command's project.
func (c *threadGroupCommand) resolve(ctx context.Context, ref string) (*hubclient.ThreadGroup, error) {
	return resolveThreadGroup(ctx, c.client, c.projectID, c.resolveErr, ref)
}

func runConversationGroupList(cmd *cobra.Command, args []string) error {
	if convGroupListJSON {
		outputFormat = "json"
	}
	tg, err := newThreadGroupCommand(convGroupProject)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	groups, err := tg.client.ThreadGroups().List(ctx, tg.projectID)
	if err != nil {
		return fmt.Errorf("failed to list thread groups: %w", threadGroupProjectHint(err, tg.resolveErr))
	}
	if isJSONOutput() {
		// The same shape as the hub's GET /api/v1/thread-groups body.
		return outputJSON(struct {
			ThreadGroups []hubclient.ThreadGroup `json:"threadGroups"`
		}{ThreadGroups: groups})
	}
	return writeThreadGroupList(os.Stdout, groups)
}

// writeThreadGroupList writes the text output of group list.
func writeThreadGroupList(w io.Writer, groups []hubclient.ThreadGroup) error {
	if len(groups) == 0 {
		_, _ = fmt.Fprintln(w, "No thread groups found.")
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NAME\tTHREADS\tID")
	for _, g := range groups {
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%s\n", g.Name, len(g.Threads), g.ID)
	}
	return tw.Flush()
}

func runConversationGroupGet(cmd *cobra.Command, args []string) error {
	if convGroupGetJSON {
		outputFormat = "json"
	}
	tg, err := newThreadGroupCommand(convGroupProject)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	g, err := tg.resolve(ctx, args[0])
	if err != nil {
		return err
	}
	if isJSONOutput() {
		return outputJSON(g)
	}
	return writeThreadGroupDetail(os.Stdout, g)
}

// threadGroupThreadName is the NAME cell of a member thread.
func threadGroupThreadName(t hubclient.ThreadGroupThread) string {
	if t.Name == "" {
		return "-"
	}
	return t.Name
}

// writeThreadGroupDetail writes the text output of group get: the group's
// fields, then its member threads.
func writeThreadGroupDetail(w io.Writer, g *hubclient.ThreadGroup) error {
	_, _ = fmt.Fprintf(w, "ID:          %s\n", g.ID)
	_, _ = fmt.Fprintf(w, "Name:        %s\n", g.Name)
	_, _ = fmt.Fprintf(w, "Project:     %s\n", g.ProjectID)
	_, _ = fmt.Fprintf(w, "Created by:  %s (%s)\n", g.CreatedBy, g.CreatedByKind)
	_, _ = fmt.Fprintf(w, "Created:     %s\n", clitime.Format(g.CreatedAt, clitime.Full))
	_, _ = fmt.Fprintf(w, "Updated:     %s\n", clitime.Format(g.UpdatedAt, clitime.Full))
	if len(g.Threads) == 0 {
		_, _ = fmt.Fprintln(w, "\nNo threads in this group.")
		return nil
	}
	_, _ = fmt.Fprintln(w, "\nThreads:")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "  NAME\tCONVERSATION ID\tADDED")
	for _, t := range g.Threads {
		_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\n", threadGroupThreadName(t), t.ConversationID, clitime.Ago(t.AddedAt))
	}
	return tw.Flush()
}

func runConversationGroupCreate(cmd *cobra.Command, args []string) error {
	if convGroupCreateJSON {
		outputFormat = "json"
	}
	tg, err := newThreadGroupCommand(convGroupProject)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	g, err := tg.client.ThreadGroups().Create(ctx, &hubclient.CreateThreadGroupRequest{
		ProjectID: tg.projectID,
		Name:      args[0],
	})
	if err != nil {
		return fmt.Errorf("failed to create thread group: %w", threadGroupProjectHint(err, tg.resolveErr))
	}
	if isJSONOutput() {
		return outputJSON(g)
	}
	fmt.Printf("Thread group created: %s\n", g.ID)
	fmt.Printf("  Name:    %s\n", g.Name)
	fmt.Printf("  Project: %s\n", g.ProjectID)
	return nil
}

func runConversationGroupRename(cmd *cobra.Command, args []string) error {
	tg, err := newThreadGroupCommand(convGroupProject)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	g, err := tg.resolve(ctx, args[0])
	if err != nil {
		return err
	}
	renamed, err := tg.client.ThreadGroups().Rename(ctx, g.ID, args[1])
	if err != nil {
		return fmt.Errorf("failed to rename thread group: %w", err)
	}
	fmt.Printf("Renamed thread group %s from %q to %q\n", renamed.ID, g.Name, renamed.Name)
	return nil
}

func runConversationGroupDelete(cmd *cobra.Command, args []string) error {
	tg, err := newThreadGroupCommand(convGroupProject)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	g, err := tg.resolve(ctx, args[0])
	if err != nil {
		return err
	}
	prompt := fmt.Sprintf("Delete thread group %s? Its %d thread(s) become ungrouped", threadGroupLabel(*g), len(g.Threads))
	if !hubsync.ConfirmAction(prompt, false, autoConfirm) {
		return fmt.Errorf("deletion cancelled")
	}
	if err := tg.client.ThreadGroups().Delete(ctx, g.ID); err != nil {
		return fmt.Errorf("failed to delete thread group: %w", err)
	}
	fmt.Printf("Deleted thread group %s\n", threadGroupLabel(*g))
	return nil
}

func runConversationGroupAdd(cmd *cobra.Command, args []string) error {
	tg, err := newThreadGroupCommand(convGroupProject)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	g, err := tg.resolve(ctx, args[0])
	if err != nil {
		return err
	}
	conversationID, err := resolveConversationRef(ctx, tg.client, args[1])
	if err != nil {
		return err
	}
	if _, err := tg.client.ThreadGroups().AddThread(ctx, g.ID, &hubclient.AddThreadGroupThreadRequest{
		ConversationID: conversationID,
	}); err != nil {
		return fmt.Errorf("failed to add thread to thread group: %w", err)
	}
	fmt.Printf("Added conversation %s to thread group %s\n", conversationID, threadGroupLabel(*g))
	return nil
}

func runConversationGroupRemove(cmd *cobra.Command, args []string) error {
	tg, err := newThreadGroupCommand(convGroupProject)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	g, err := tg.resolve(ctx, args[0])
	if err != nil {
		return err
	}
	conversationID, err := resolveConversationRef(ctx, tg.client, args[1])
	if err != nil {
		return err
	}
	if err := tg.client.ThreadGroups().RemoveThread(ctx, g.ID, conversationID); err != nil {
		return fmt.Errorf("failed to remove thread from thread group: %w", err)
	}
	fmt.Printf("Removed conversation %s from thread group %s\n", conversationID, threadGroupLabel(*g))
	return nil
}
