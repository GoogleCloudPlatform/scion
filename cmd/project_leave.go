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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/spf13/cobra"
)

// projectLeaveCmd is the minimal 'scion project leave' (ptone/scion#7494,
// phase A): it leaves the project after a confirmation. The dry-run
// display and the agent options come later.
var projectLeaveCmd = &cobra.Command{
	Use:   "leave <project>",
	Short: "Leave a project you were added to directly",
	Long: `Remove your own direct roles on a project.

The <project> argument is required and can be a project name, slug, or ID;
the current project is never used by default.

After you leave, your agents in the project are stopped and put on hold
unless you keep access through a group or your hub role, and your access
tokens limited to this project are revoked. To come back, a project owner or admin must add
you again.

The last owner of a project cannot leave (transfer ownership first), and
membership through a group cannot be left here.

Requires Hub connectivity and the hub.project_self_leave experiment.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		projectRef := args[0]

		resolvedPath, _, err := config.ResolveProjectPath(projectPath)
		if err != nil {
			return fmt.Errorf("failed to resolve project path: %w", err)
		}
		settings, err := config.LoadSettings(resolvedPath)
		if err != nil {
			return fmt.Errorf("failed to load settings: %w", err)
		}
		client, uat, err := getHubClientUAT(settings)
		if err != nil {
			return err
		}

		ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
		defer cancel()

		project, err := resolveProjectByNameOrID(ctx, client, projectRef, uat)
		if err != nil {
			return projectLeaveResolveError(projectRef, err)
		}
		interactive := projectLeaveStdinIsTerminal()
		confirmed := hubsync.ConfirmAction(fmt.Sprintf("Leave project %q?", project.Name), false, autoConfirm)
		return runProjectLeave(ctx, client.Projects(), project, leaveConfirmationOf(confirmed, interactive), cmd.OutOrStdout())
	},
}

// projectLeaveNotMember is the error for a project the caller cannot
// leave because they are not a member, or it does not exist: the hub gives
// one answer for both.
func projectLeaveNotMember(ref string) error {
	return fmt.Errorf("not a member of %s (or it doesn't exist)", ref)
}

// projectLeaveResolveError maps a failure to resolve ref. Only a project
// that is not found (a hub 404, or the resolver finding no match) reads as
// not a member, because the hub answers a non-member with the same 404 as
// a missing project. Every other error (authentication, a server error, an
// ambiguous name, a token without the lookup scopes) is returned wrapped,
// so it is not hidden behind "not a member".
func projectLeaveResolveError(ref string, err error) error {
	var notFound *projectNotFoundError
	if apiclient.IsNotFoundError(err) || errors.As(err, &notFound) {
		return projectLeaveNotMember(ref)
	}
	return fmt.Errorf("failed to resolve project %s: %w", ref, err)
}

// projectLeaveStdinIsTerminal reports whether stdin is a terminal, so a
// declined prompt can be told from a run with no prompt at all. Tests
// replace it.
var projectLeaveStdinIsTerminal = util.IsTerminal

// leaveConfirmation is the outcome of the leave confirmation.
type leaveConfirmation int

const (
	// leaveConfirmed: -y, or "yes" at the prompt.
	leaveConfirmed leaveConfirmation = iota
	// leaveDeclined: the user answered no at an interactive prompt.
	leaveDeclined
	// leaveNotConfirmable: stdin is not a terminal and -y was not given,
	// so there was no prompt to answer.
	leaveNotConfirmable
)

// leaveConfirmationOf classifies ConfirmAction's answer: a "no" on a
// terminal is a decision; without a terminal no question was asked.
func leaveConfirmationOf(confirmed, interactive bool) leaveConfirmation {
	switch {
	case confirmed:
		return leaveConfirmed
	case interactive:
		return leaveDeclined
	default:
		return leaveNotConfirmable
	}
}

// errProjectLeaveCancelled is returned when the user declines the prompt:
// the command exits non-zero, and no -y hint is shown because the user
// chose not to leave.
var errProjectLeaveCancelled = errors.New("leave cancelled")

// runProjectLeave leaves project through svc once confirmed, and prints
// the outcome to out.
func runProjectLeave(ctx context.Context, svc hubclient.ProjectService, project *hubclient.Project, confirm leaveConfirmation, out io.Writer) error {
	switch confirm {
	case leaveConfirmed:
	case leaveDeclined:
		return errProjectLeaveCancelled
	default:
		return newUsageError("leaving %s was not confirmed; pass -y to confirm", project.Name)
	}
	res, err := svc.Leave(ctx, project.ID, false)
	if err != nil {
		if apiclient.IsNotFoundError(err) {
			return projectLeaveNotMember(project.Name)
		}
		return fmt.Errorf("failed to leave project: %w", err)
	}

	if isJSONOutput() {
		return outputJSON(ActionResult{
			Status:  "success",
			Command: "project leave",
			Message: fmt.Sprintf("Left project %q", project.Name),
			Details: map[string]interface{}{
				"projectId":       res.ProjectID,
				"removedRoles":    res.RemovedRoles,
				"remainingAccess": res.RemainingAccess,
				"agents":          res.Agents.Count,
				"revokedTokens":   res.ProjectBoundTokens.Revoked,
				"willBeHeld":      res.WillBeHeld,
			},
		})
	}

	_, _ = fmt.Fprintf(out, "Left project %s\n", project.Name)
	switch res.RemainingAccess {
	case "group":
		_, _ = fmt.Fprintln(out, "You keep access through a group; only your direct roles were removed.")
	case "hub":
		_, _ = fmt.Fprintln(out, "You keep access through your hub role; only your direct roles were removed.")
	}
	if res.WillBeHeld {
		_, _ = fmt.Fprintf(out, "%d of your agents in the project will be stopped and put on hold.\n", res.Agents.Count)
	}
	if n := res.ProjectBoundTokens.Revoked; n > 0 {
		_, _ = fmt.Fprintf(out, "%d access tokens limited to this project were revoked.\n", n)
	}
	return nil
}

func init() {
	projectLeaveCmd.Flags().BoolVarP(&autoConfirm, "yes", "y", false, "Answer Yes to the confirmation prompt (required to confirm when stdin is not a terminal)")
	projectCmd.AddCommand(projectLeaveCmd)
}
