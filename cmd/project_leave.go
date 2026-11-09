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
	"os"
	"sort"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/spf13/cobra"
)

var (
	leaveDeleteAgents bool
	leaveKeepAgents   bool
	leaveDryRun       bool
	leaveJSON         bool
)

// projectLeaveCmd is 'scion project leave' (ptone/scion#7494). It always
// asks the hub for a dry run first, prints what leaving changes, settles
// what happens to the caller's agents, and then leaves.
var projectLeaveCmd = &cobra.Command{
	Use:   "leave <project>",
	Short: "Leave a project you were added to directly",
	Long: `Remove your own direct roles on a project (owner, admin, member, viewer
or a custom role).

The <project> argument is required and can be a project name, slug, or ID;
the current project is never used by default.

The command first asks the Hub what leaving would change and prints it: the
roles removed, whether you keep access through a group or your hub role, your
agents in the project, and your access tokens limited to the project, which
are revoked. To come back, a project owner or admin must add you again.

When you keep no access and have agents in the project, those agents are
stopped and put on hold. Choose what happens to them: --delete-agents deletes
them before leaving (nothing is left if a delete fails), --keep-agents leaves
them on hold. A terminal session asks; with -y or without a terminal, one of
the two flags is required.

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

		resolveCtx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
		defer cancel()
		project, err := resolveProjectByNameOrID(resolveCtx, client, projectRef, uat)
		if err != nil {
			return projectLeaveResolveError(projectRef, err)
		}

		opts := leaveOptions{
			dryRun:      leaveDryRun,
			json:        leaveJSON || isJSONOutput(),
			agents:      leaveAgentFlagChoice(leaveDeleteAgents, leaveKeepAgents),
			yes:         autoConfirm,
			interactive: projectLeaveStdinIsTerminal(),
		}
		agents := client.ProjectAgents(project.ID)
		lio := leaveIO{
			out:     cmd.OutOrStdout(),
			prompt:  promptLeaveAgentChoice,
			confirm: func(q string) bool { return hubsync.ConfirmAction(q, false, false) },
			deleteAgent: func(ctx context.Context, agentID string) error {
				return agents.Delete(ctx, agentID, &hubclient.DeleteAgentOptions{DeleteFiles: true, RemoveBranch: true})
			},
		}
		return runProjectLeave(cmd.Context(), client.Projects(), project, opts, lio)
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

// leaveAgentChoice is what happens to the caller's agents in the project.
type leaveAgentChoice int

const (
	// leaveAgentsUnset: no flag given and no answer yet.
	leaveAgentsUnset leaveAgentChoice = iota
	// leaveAgentsDelete: delete the agents before leaving.
	leaveAgentsDelete
	// leaveAgentsKeep: leave them; the membership check holds them.
	leaveAgentsKeep
	// leaveAgentsCancel: the user cancelled at the prompt.
	leaveAgentsCancel
)

// leaveAgentFlagChoice maps the mutually exclusive agent flags.
func leaveAgentFlagChoice(deleteAgents, keepAgents bool) leaveAgentChoice {
	switch {
	case deleteAgents:
		return leaveAgentsDelete
	case keepAgents:
		return leaveAgentsKeep
	default:
		return leaveAgentsUnset
	}
}

// leaveOptions are the command's settings for one run.
type leaveOptions struct {
	dryRun      bool
	json        bool
	agents      leaveAgentChoice
	yes         bool
	interactive bool
}

// leaveIO is what runProjectLeave talks to besides the hub's project
// service, so tests can drive it.
type leaveIO struct {
	out         io.Writer
	prompt      func(question string) (leaveAgentChoice, error)
	confirm     func(question string) bool
	deleteAgent func(ctx context.Context, agentID string) error
}

// errProjectLeaveCancelled is returned when the user declines or cancels
// at a prompt: the command exits non-zero, and no -y hint is shown because
// the user chose not to leave.
var errProjectLeaveCancelled = errors.New("leave cancelled")

// leaveCallTimeout bounds each hub call; prompts are not under it.
const leaveCallTimeout = 30 * time.Second

// runProjectLeave runs the dry run, prints its consequences, settles the
// agents, and leaves project through svc. Nothing is deleted or left until
// the leave is confirmed; with dryRun nothing changes at all.
func runProjectLeave(ctx context.Context, svc hubclient.ProjectService, project *hubclient.Project, opts leaveOptions, lio leaveIO) error {
	dry, err := leaveCall(ctx, svc, project.ID, true)
	if err != nil {
		return leaveFailure(project, err, opts.json, lio.out)
	}
	if opts.dryRun {
		if opts.json {
			return writeJSONTo(lio.out, dry)
		}
		printLeaveConsequences(lio.out, project, dry)
		return nil
	}
	if !opts.json {
		printLeaveConsequences(lio.out, project, dry)
	}

	holdsAgents := dry.RemainingAccess == "none" && dry.Agents.Count > 0
	choice := opts.agents
	question := fmt.Sprintf("Leave project %q?", project.Name)
	switch {
	case holdsAgents && choice == leaveAgentsUnset:
		if opts.yes || !opts.interactive {
			return newUsageError("you have %d agents in %s that would be put on hold; pass --delete-agents or --keep-agents", dry.Agents.Count, project.Name)
		}
		choice, err = lio.prompt(fmt.Sprintf("Leave project %q: delete your %d agents first, keep them on hold, or cancel?", project.Name, dry.Agents.Count))
		if err != nil {
			return err
		}
		if choice != leaveAgentsDelete && choice != leaveAgentsKeep {
			return errProjectLeaveCancelled
		}
	case opts.yes:
	case opts.interactive:
		if !lio.confirm(question) {
			return errProjectLeaveCancelled
		}
	default:
		return newUsageError("leaving %s was not confirmed; pass -y to confirm", project.Name)
	}

	if !holdsAgents && choice == leaveAgentsDelete && !opts.json {
		_, _ = fmt.Fprintln(lio.out, "No agents will be put on hold, so --delete-agents deletes nothing.")
	}
	var deleted []string
	if holdsAgents && choice == leaveAgentsDelete {
		if deleted, err = deleteLeaveAgents(ctx, dry.Agents, lio); err != nil {
			return err
		}
	}

	res, err := leaveCall(ctx, svc, project.ID, false)
	if err != nil {
		return leaveFailure(project, err, opts.json, lio.out)
	}
	if opts.json {
		return writeJSONTo(lio.out, res)
	}
	printLeaveResult(lio.out, project, res, deleted)
	return nil
}

// leaveCall makes one leave request (dry run or real) under its own timeout.
func leaveCall(ctx context.Context, svc hubclient.ProjectService, projectID string, dryRun bool) (*hubclient.LeaveProjectResult, error) {
	callCtx, cancel := context.WithTimeout(ctx, leaveCallTimeout)
	defer cancel()
	return svc.Leave(callCtx, projectID, dryRun)
}

// deleteLeaveAgents deletes every agent of a dry run's list, in order, and
// stops at the first failure. An agent already gone counts as deleted. A
// list the hub cut short cannot be deleted in full, so it is refused
// before any delete.
func deleteLeaveAgents(ctx context.Context, agents hubclient.LeaveProjectAgents, lio leaveIO) ([]string, error) {
	if agents.Truncated || len(agents.Items) < agents.Count {
		return nil, fmt.Errorf("you have %d agents in the project, more than can be listed here; delete them first, or pass --keep-agents", agents.Count)
	}
	deleted := make([]string, 0, len(agents.Items))
	for _, a := range agents.Items {
		callCtx, cancel := context.WithTimeout(ctx, leaveCallTimeout)
		err := lio.deleteAgent(callCtx, a.ID)
		cancel()
		if err != nil && !apiclient.IsNotFoundError(err) {
			return deleted, fmt.Errorf("failed to delete agent %s, so the project was not left: %w", leaveAgentLabel(a), err)
		}
		deleted = append(deleted, leaveAgentLabel(a))
	}
	return deleted, nil
}

// leaveAgentLabel names an agent of a leave result: its name, else its ID.
func leaveAgentLabel(a hubclient.LeaveProjectAgentItem) string {
	if a.Name != "" {
		return a.Name
	}
	return a.ID
}

// leaveFailure reports a failed leave request. A 404 is "not a member";
// any other hub refusal keeps its code and message (the 409s carry the
// guidance: transfer ownership, or leave the group). With JSON output the
// error is written as {"error": {...}} and the command exits non-zero.
func leaveFailure(project *hubclient.Project, err error, asJSON bool, out io.Writer) error {
	var apiErr *apiclient.APIError
	isAPI := errors.As(err, &apiErr)
	if asJSON {
		body := map[string]interface{}{"code": "error", "message": err.Error()}
		if apiclient.IsNotFoundError(err) {
			body = map[string]interface{}{"code": "not_found", "message": projectLeaveNotMember(project.Name).Error()}
		} else if isAPI {
			body = map[string]interface{}{"code": apiErr.Code, "message": apiErr.Message}
			if len(apiErr.Details) > 0 {
				body["details"] = apiErr.Details
			}
		}
		if werr := writeJSONTo(out, map[string]interface{}{"error": body}); werr != nil {
			return werr
		}
		return &jsonReportedError{msg: fmt.Sprintf("%v", body["message"])}
	}
	if apiclient.IsNotFoundError(err) {
		return projectLeaveNotMember(project.Name)
	}
	if isAPI && apiErr.StatusCode == 409 {
		msg := fmt.Sprintf("cannot leave %s: %s", project.Name, apiErr.Message)
		if apiErr.Code == "last_owner" {
			msg += ". Transfer ownership to another member first, or delete the project."
		}
		if groups := leaveGroupNames(apiErr.Details); len(groups) > 0 {
			msg += " Groups: " + strings.Join(groups, ", ")
		}
		return errors.New(msg)
	}
	return fmt.Errorf("failed to leave project: %w", err)
}

// leaveGroupNames reads the group names of a membership_group_derived
// refusal's details (only groups the caller may read are listed).
func leaveGroupNames(details map[string]interface{}) []string {
	raw, ok := details["groups"].([]interface{})
	if !ok {
		return nil
	}
	var names []string
	for _, g := range raw {
		m, ok := g.(map[string]interface{})
		if !ok {
			continue
		}
		if name, _ := m["name"].(string); name != "" {
			names = append(names, name)
		} else if id, _ := m["id"].(string); id != "" {
			names = append(names, id)
		}
	}
	sort.Strings(names)
	return names
}

// printLeaveConsequences prints what a dry run says leaving changes.
func printLeaveConsequences(out io.Writer, project *hubclient.Project, dry *hubclient.LeaveProjectResult) {
	_, _ = fmt.Fprintf(out, "Leaving %s:\n", project.Name)
	if len(dry.RemovedRoles) > 0 {
		_, _ = fmt.Fprintf(out, "  Removes your direct roles: %s\n", strings.Join(dry.RemovedRoles, ", "))
	}
	switch dry.RemainingAccess {
	case "group":
		_, _ = fmt.Fprintln(out, "  You keep access through a group; only your direct roles are removed.")
	case "hub":
		_, _ = fmt.Fprintln(out, "  You keep access through your hub role; only your direct roles are removed.")
	default:
		_, _ = fmt.Fprintln(out, "  You lose access to the project, its agents, chat and files. To come back, an owner or admin must add you again.")
	}
	if dry.RemainingAccess == "none" && dry.Agents.Count > 0 {
		_, _ = fmt.Fprintf(out, "  %d of your agents in the project will be stopped and put on hold, and stay on hold if you are added back:\n", dry.Agents.Count)
		for _, a := range dry.Agents.Items {
			_, _ = fmt.Fprintf(out, "    - %s (%s)\n", leaveAgentLabel(a), a.Phase)
		}
		if dry.Agents.Truncated || len(dry.Agents.Items) < dry.Agents.Count {
			_, _ = fmt.Fprintf(out, "    ... and %d more\n", dry.Agents.Count-len(dry.Agents.Items))
		}
	}
	if n := dry.ProjectBoundTokens.Revoked; n > 0 {
		_, _ = fmt.Fprintf(out, "  %d access tokens limited to this project will be revoked.\n", n)
	}
}

// printLeaveResult prints the outcome of the real leave.
func printLeaveResult(out io.Writer, project *hubclient.Project, res *hubclient.LeaveProjectResult, deleted []string) {
	if len(deleted) > 0 {
		_, _ = fmt.Fprintf(out, "Deleted %d agents: %s\n", len(deleted), strings.Join(deleted, ", "))
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
}

// promptLeaveAgentChoice asks on a terminal whether to delete the agents,
// keep them on hold, or cancel. The prompt goes to stderr. An end of input
// or an unknown answer cancels.
func promptLeaveAgentChoice(question string) (leaveAgentChoice, error) {
	_, _ = fmt.Fprintf(os.Stderr, "%s [d]elete / [k]eep / [c]ancel: ", question)
	input, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && (input == "" || !errors.Is(err, io.EOF)) {
		_, _ = fmt.Fprintln(os.Stderr)
		return leaveAgentsCancel, nil
	}
	return parseLeaveAgentAnswer(input), nil
}

// parseLeaveAgentAnswer reads an answer to the agent prompt.
func parseLeaveAgentAnswer(input string) leaveAgentChoice {
	switch strings.ToLower(strings.TrimSpace(input)) {
	case "d", "delete":
		return leaveAgentsDelete
	case "k", "keep":
		return leaveAgentsKeep
	default:
		return leaveAgentsCancel
	}
}

func init() {
	projectLeaveCmd.Flags().BoolVarP(&autoConfirm, "yes", "y", false, "Answer Yes to the confirmation prompt (required to confirm when stdin is not a terminal)")
	projectLeaveCmd.Flags().BoolVar(&leaveDeleteAgents, "delete-agents", false, "Delete your agents in the project before leaving")
	projectLeaveCmd.Flags().BoolVar(&leaveKeepAgents, "keep-agents", false, "Leave your agents in the project stopped and on hold")
	projectLeaveCmd.Flags().BoolVar(&leaveDryRun, "dry-run", false, "Show what leaving would change, and change nothing")
	projectLeaveCmd.Flags().BoolVar(&leaveJSON, "json", false, "Output the leave result as JSON")
	projectLeaveCmd.MarkFlagsMutuallyExclusive("delete-agents", "keep-agents")
	projectCmd.AddCommand(projectLeaveCmd)
}
