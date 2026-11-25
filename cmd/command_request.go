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
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/msgaction"
)

const (
	errMsgCommandRequestAgentsOnly = "this command is for agents; it is available only in agent mode"
	errMsgCommandRequestHumansOnly = "this command is only available to human users"
)

var (
	commandRequestsClientFactory = func() (hubclient.Client, error) {
		_, client, err := requireHubClient()
		return client, err
	}
	commandApproveIsTerminal = func() bool {
		return term.IsTerminal(int(os.Stdin.Fd()))
	}
	commandApproveStdin io.Reader = os.Stdin
)

var commandCmd = &cobra.Command{
	Use:   "command",
	Short: "Manage human-approved command requests",
}

func init() {
	commandCmd.AddCommand(newCommandRequestCmd())
	rootCmd.AddCommand(commandCmd)
}

func newCommandRequestCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "request",
		Short: "List or approve command requests",
		Long: `Inspect or approve command execution requests.

Commands:
  scion command request list [--key <key>] [--json]              (agent mode)
  scion command request approve <id> [--confirm-sha256 <hex>]    (human)`,
	}
	cmd.AddCommand(
		newCommandRequestListCmd(),
		newCommandRequestApproveCmd(),
	)
	return cmd
}

func newCommandRequestListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List command requests made by this agent",
		Args:  cobra.NoArgs,
		RunE:  runCommandRequestList,
	}
	cmd.Flags().String("key", "", "Filter by command request key")
	cmd.Flags().Bool("json", false, "Output in JSON format")
	return cmd
}

func newCommandRequestApproveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "approve <id>",
		Short: "Approve a pending command request and run it in your execution container",
		Long: `Review and approve a pending command request (human users only).

On an interactive terminal, prints the exact script, working directory, timeout,
and target execution container, then prompts for [y/N] confirmation.
Non-interactively, requires --confirm-sha256 <full 64-char hex> matching the
request's canonical command_sha256.`,
		Args: cobra.ExactArgs(1),
		RunE: runCommandRequestApprove,
	}
	cmd.Flags().String("confirm-sha256", "", "Full 64-character hex command_sha256 for non-interactive confirmation")
	cmd.Flags().Bool("json", false, "Output in JSON format")
	return cmd
}

func commandRequestsWantJSON(cmd *cobra.Command) bool {
	if isJSONOutput() {
		return true
	}
	v, err := cmd.Flags().GetBool("json")
	return err == nil && v
}

func runCommandRequestList(cmd *cobra.Command, _ []string) error {
	if resolveMode() != ModeAgent {
		return errors.New(errMsgCommandRequestAgentsOnly)
	}
	keyFilter, _ := cmd.Flags().GetString("key")

	client, err := commandRequestsClientFactory()
	if err != nil {
		return err
	}

	baseCtx := cmd.Context()
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	ctx, cancel := context.WithTimeout(baseCtx, 30*time.Second)
	defer cancel()

	items := make([]messages.CommandRequestView, 0)
	cursor := ""
	for {
		page, err := client.CommandRequests().List(ctx, &hubclient.ListAgentCommandRequestsOptions{
			Key:    strings.TrimSpace(keyFilter),
			Cursor: cursor,
			Limit:  100,
		})
		if err != nil {
			return formatCommandRequestAPIError(err)
		}
		items = append(items, page.Items...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}

	if commandRequestsWantJSON(cmd) {
		return writeJSONTo(cmd.OutOrStdout(), messages.AgentCommandRequestListResponse{
			Items: items,
		})
	}

	if len(items) == 0 {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "No command requests.")
		return nil
	}

	writeCommandRequestsListTable(cmd.OutOrStdout(), items)
	return nil
}

func writeCommandRequestsListTable(out io.Writer, items []messages.CommandRequestView) {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tKEY\tVER\tSTATUS\tRUN\tEXIT\tAGE")
	for _, it := range items {
		runStatus := "-"
		exitStr := "-"
		if it.Run != nil {
			runStatus = safeText(it.Run.Status)
			if it.Run.Reason != "" {
				runStatus += " (" + safeText(it.Run.Reason) + ")"
			}
			if it.Run.ExitCode != nil {
				exitStr = fmt.Sprintf("%d", *it.Run.ExitCode)
			}
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\tv%d\t%s\t%s\t%s\t%s\n",
			safeText(it.RowID),
			safeText(it.Key),
			it.Version,
			safeText(it.Status),
			runStatus,
			exitStr,
			clitime.Ago(it.CreatedAt),
		)
	}
	_ = tw.Flush()
}

func runCommandRequestApprove(cmd *cobra.Command, args []string) error {
	if resolveMode() == ModeAgent {
		return errors.New(errMsgCommandRequestHumansOnly)
	}
	reqID := strings.TrimSpace(args[0])
	if reqID == "" {
		return newUsageError("command request <id> is required")
	}
	confirmHash, _ := cmd.Flags().GetString("confirm-sha256")
	confirmHash = strings.ToLower(strings.TrimSpace(confirmHash))

	client, err := commandRequestsClientFactory()
	if err != nil {
		return err
	}

	baseCtx := cmd.Context()
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	ctx, cancel := context.WithTimeout(baseCtx, 30*time.Second)
	defer cancel()

	detail, err := client.CommandRequests().Get(ctx, reqID)
	if err != nil {
		return formatCommandRequestAPIError(err)
	}

	out := cmd.OutOrStdout()
	containerLabel := "your execution container"
	if detail.ExecContainer != nil && detail.ExecContainer.AgentName != "" {
		containerLabel = safeText(detail.ExecContainer.AgentName)
		if detail.ExecContainer.ProjectName != "" {
			containerLabel += " (" + safeText(detail.ExecContainer.ProjectName) + ")"
		}
	}

	if !commandRequestsWantJSON(cmd) {
		printCommandRequestConfirmationSummary(out, detail, containerLabel)
	}

	if commandApproveIsTerminal() {
		if confirmHash != "" {
			if confirmHash != strings.ToLower(detail.Request.CommandSHA256) {
				return fmt.Errorf("--confirm-sha256 does not match command request hash %s", detail.Request.CommandSHA256)
			}
		} else {
			_, _ = fmt.Fprintf(out, "Run this in %s? [y/N] ", containerLabel)
			reader := bufio.NewReader(commandApproveStdin)
			line, rerr := reader.ReadString('\n')
			if rerr != nil && !errors.Is(rerr, io.EOF) {
				return fmt.Errorf("reading confirmation: %w", rerr)
			}
			ans := strings.ToLower(strings.TrimSpace(line))
			if ans != "y" && ans != "yes" {
				return errors.New("approval cancelled")
			}
		}
	} else {
		if confirmHash == "" {
			return newUsageError("non-interactive approval requires --confirm-sha256 %s", detail.Request.CommandSHA256)
		}
		if confirmHash != strings.ToLower(detail.Request.CommandSHA256) {
			return fmt.Errorf("--confirm-sha256 does not match command request hash %s", detail.Request.CommandSHA256)
		}
	}

	resp, err := client.CommandRequests().Approve(ctx, reqID, &messages.ApproveCommandRequestBody{
		RowID:          detail.Request.RowID,
		CommandSHA256:  detail.Request.CommandSHA256,
		Surface:        msgaction.CommandRequestSurfaceCLI,
		IdempotencyKey: uuid.NewString(),
	})
	if err != nil {
		return formatCommandRequestAPIError(err)
	}

	if commandRequestsWantJSON(cmd) {
		return writeJSONTo(out, resp)
	}
	_, _ = fmt.Fprintf(out, "Approved %s v%d (run %s, status %s).\n",
		safeText(resp.Request.Key), resp.Request.Version, safeText(resp.Run.RunID), safeText(resp.Run.Status))
	return nil
}

func printCommandRequestConfirmationSummary(out io.Writer, detail *messages.CommandRequestDetailResponse, containerLabel string) {
	req := detail.Request
	_, _ = fmt.Fprintf(out, "Command request: %s v%d (%s)\n", safeText(req.Key), req.Version, safeText(req.RowID))
	_, _ = fmt.Fprintf(out, "Requested by:    %s\n", safeText(req.AgentName))
	if req.Description != "" {
		_, _ = fmt.Fprintf(out, "Description:     %s\n", safeText(req.Description))
	}
	wd := req.Workdir
	if wd == "" {
		wd = "$HOME"
	}
	_, _ = fmt.Fprintf(out, "Workdir:         %s\n", safeText(wd))
	if strings.HasPrefix(req.Workdir, "/workspace") {
		_, _ = fmt.Fprintln(out, "WARNING:         workdir is under /workspace (agent-writable)")
	}
	_, _ = fmt.Fprintf(out, "Timeout:         %ds\n", req.TimeoutS)
	_, _ = fmt.Fprintf(out, "Container:       %s", containerLabel)
	if detail.ExecContainer != nil && detail.ExecContainer.State != "" {
		_, _ = fmt.Fprintf(out, " [%s]", safeText(detail.ExecContainer.State))
	}
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintf(out, "SHA-256:         %s\n", safeText(req.CommandSHA256))
	_, _ = fmt.Fprintln(out, "--- script ---")
	_, _ = fmt.Fprint(out, req.Script)
	if !strings.HasSuffix(req.Script, "\n") {
		_, _ = fmt.Fprintln(out)
	}
	_, _ = fmt.Fprintln(out, "--- end script ---")
}

func formatCommandRequestAPIError(err error) error {
	if err == nil {
		return nil
	}
	var apiErr *apiclient.APIError
	if !errors.As(err, &apiErr) {
		return err
	}
	if apiErr.Code == "experiment_disabled" || apiErr.StatusCode == http.StatusNotFound {
		if msg := strings.TrimSpace(apiErr.Message); msg != "" && apiErr.Code != "not_found" {
			return errors.New(safeText(msg))
		}
		if apiErr.Code == "experiment_disabled" {
			return errors.New(errMsgCommandRequestsExperimentRequired)
		}
	}
	if msg := strings.TrimSpace(apiErr.Message); msg != "" {
		return errors.New(safeText(msg))
	}
	return err
}
