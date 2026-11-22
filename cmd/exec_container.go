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
	"strings"
	"text/tabwriter"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

// execContainerCmd is the parent command for `scion exec-container` operations
// (design §4.5, ptone/scion#8817). Human-mode only (absent from agentAllowed
// in cmd/cli_mode.go) and gated off Windows in v1 (absent from windowsAllowed
// in cmd/platform_gate.go).
var execContainerCmd = &cobra.Command{
	Use:   "exec-container",
	Short: "Manage your default execution container",
	Long: `Inspect, set, or clear your default execution container on the Hub.

An execution container is an agent running the built-in shell harness (--harness shell)
that holds credentials you log into manually so approved commands can run
without exposing those credentials to LLM agents.

Examples:
  scion exec-container show
  scion exec-container set ops
  scion exec-container clear`,
}

var execContainerShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Show your registered execution container and its current state",
	Args:  cobra.NoArgs,
	RunE:  runExecContainerShow,
}

var execContainerSetCmd = &cobra.Command{
	Use:   "set <agent>",
	Short: "Register an execution container in the current project as your default execution container",
	Long: `Resolve <agent> (by name, slug, or UUID) within the current project and
register it as your default execution container on the Hub.

Name resolution happens locally against the current project; the Hub validates
that the target is an execution container you can attach to.`,
	Args: cobra.ExactArgs(1),
	RunE: runExecContainerSet,
}

var execContainerClearCmd = &cobra.Command{
	Use:   "clear",
	Short: "Clear your registered execution container",
	Args:  cobra.NoArgs,
	RunE:  runExecContainerClear,
}

func init() {
	rejectUnknownSubcommands(execContainerCmd)
	rootCmd.AddCommand(execContainerCmd)
	execContainerCmd.AddCommand(execContainerShowCmd)
	execContainerCmd.AddCommand(execContainerSetCmd)
	execContainerCmd.AddCommand(execContainerClearCmd)
}

func resolveExecContainerHubClient() (hubclient.Client, error) {
	hubCtx, err := requireHubWith(projectPath, true)
	if err != nil {
		return nil, fmt.Errorf("hub connection required: %w", err)
	}
	return hubCtx.Client, nil
}

func execContainerContext(cmd *cobra.Command) (context.Context, context.CancelFunc) {
	parent := context.Background()
	if cmd != nil && cmd.Context() != nil {
		parent = cmd.Context()
	}
	return context.WithTimeout(parent, 30*time.Second)
}

func runExecContainerShow(cmd *cobra.Command, _ []string) error {
	ctx, cancel := execContainerContext(cmd)
	defer cancel()

	client, err := resolveExecContainerHubClient()
	if err != nil {
		return err
	}

	status, err := client.Users().GetExecContainer(ctx)
	if err != nil {
		return err
	}

	if isJSONOutput() {
		return outputJSON(status)
	}
	printExecContainerStatus(cmd.OutOrStdout(), status)
	return nil
}

func runExecContainerSet(cmd *cobra.Command, args []string) error {
	ctx, cancel := execContainerContext(cmd)
	defer cancel()

	agentRef := strings.TrimSpace(args[0])
	if agentRef == "" {
		return fmt.Errorf("agent name or ID is required")
	}

	client, projectID, err := requireHubProjectClient()
	if err != nil {
		return err
	}

	agentID, err := resolveLocalProjectAgentID(ctx, client, projectID, agentRef)
	if err != nil {
		return err
	}

	status, err := client.Users().SetExecContainer(ctx, agentID)
	if err != nil {
		return err
	}

	if isJSONOutput() {
		return outputJSON(status)
	}
	printExecContainerStatus(cmd.OutOrStdout(), status)
	return nil
}

func runExecContainerClear(cmd *cobra.Command, _ []string) error {
	ctx, cancel := execContainerContext(cmd)
	defer cancel()

	client, err := resolveExecContainerHubClient()
	if err != nil {
		return err
	}

	if err := client.Users().ClearExecContainer(ctx); err != nil {
		return err
	}

	if isJSONOutput() {
		return outputJSON(map[string]any{"cleared": true})
	}
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Execution container cleared.")
	return nil
}

// resolveLocalProjectAgentID resolves an agent reference within projectID to
// its canonical agent ID, paging through all results so large projects are
// never truncated. Resolution precedence is exact ID, then slug, then name; an
// ambiguous name (multiple agents sharing the same name) returns an error.
// The Hub never resolves names on PUT /api/v1/users/me/exec-container
// (design §4.5).
func resolveLocalProjectAgentID(ctx context.Context, client hubclient.Client, projectID, ref string) (string, error) {
	var all []hubclient.Agent
	var cursor string
	seenCursors := make(map[string]bool)
	for {
		opts := &hubclient.ListAgentsOptions{
			Page: apiclient.PageOptions{
				Limit:  200,
				Cursor: cursor,
			},
		}
		resp, err := client.ProjectAgents(projectID).List(ctx, opts)
		if err != nil {
			return "", fmt.Errorf("failed to list agents in current project: %w", err)
		}
		all = append(all, resp.Agents...)
		next := resp.Page.NextCursor
		if next == "" || seenCursors[next] {
			break
		}
		seenCursors[next] = true
		cursor = next
	}

	// 1. Exact ID match.
	for _, a := range all {
		if strings.EqualFold(a.ID, ref) {
			return a.ID, nil
		}
	}
	// 2. Exact slug match.
	for _, a := range all {
		if a.Slug == ref {
			return a.ID, nil
		}
	}
	// 3. Exact name match (error if ambiguous).
	var nameMatches []hubclient.Agent
	for _, a := range all {
		if a.Name == ref {
			nameMatches = append(nameMatches, a)
		}
	}
	if len(nameMatches) == 1 {
		return nameMatches[0].ID, nil
	}
	if len(nameMatches) > 1 {
		choices := make([]string, 0, len(nameMatches))
		for _, a := range nameMatches {
			choices = append(choices, fmt.Sprintf("%s (%s)", a.Slug, a.ID))
		}
		return "", fmt.Errorf("agent name %q is ambiguous in current project (%s); specify the slug or UUID instead",
			ref, strings.Join(choices, ", "))
	}
	return "", fmt.Errorf("agent %q not found in current project", ref)
}

func printExecContainerStatus(out io.Writer, status *hubclient.ExecContainerStatus) {
	if status == nil || !status.Configured {
		_, _ = fmt.Fprintln(out, "Not configured")
		return
	}

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(tw, "STATE\t%s\n", status.State)
	if status.Reason != "" {
		_, _ = fmt.Fprintf(tw, "REASON\t%s\n", status.Reason)
	}
	if status.Agent != nil {
		name := status.Agent.Name
		if name == "" {
			name = status.Agent.Slug
		}
		_, _ = fmt.Fprintf(tw, "AGENT\t%s\n", name)
		_, _ = fmt.Fprintf(tw, "AGENT ID\t%s\n", status.Agent.ID)
		proj := status.Agent.ProjectName
		if proj == "" {
			proj = status.Agent.ProjectID
		} else if status.Agent.ProjectID != "" {
			proj = fmt.Sprintf("%s (%s)", proj, status.Agent.ProjectID)
		}
		_, _ = fmt.Fprintf(tw, "PROJECT\t%s\n", proj)
		_, _ = fmt.Fprintf(tw, "PHASE\t%s\n", status.Agent.Phase)
		if status.Agent.Activity != "" {
			_, _ = fmt.Fprintf(tw, "ACTIVITY\t%s\n", status.Agent.Activity)
		}
	}
	_ = tw.Flush()
}
