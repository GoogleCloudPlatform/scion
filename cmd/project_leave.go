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
are revoked when you keep no access. To come back, a project owner or admin
must add you again.

When you keep no access and have agents in the project, those agents are
stopped and put on hold. Choose what happens to them: --delete-agents deletes
them before leaving and waits until the Hub confirms each delete (nothing is
left if a delete fails or cannot be confirmed), --keep-agents leaves them on
hold. A terminal session asks; with -y or without a terminal, one of the two
flags is required.

The last owner of a project cannot leave (transfer ownership first), and
membership through a group cannot be left here.

Leaving needs an interactive login session: a user access token is refused
(403 forbidden). Requires Hub connectivity and the hub.project_self_leave
experiment.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		opts := leaveOptions{
			dryRun:      leaveDryRun,
			json:        leaveJSON || isJSONOutput(),
			agents:      leaveAgentFlagChoice(leaveDeleteAgents, leaveKeepAgents),
			yes:         autoConfirm,
			interactive: projectLeaveStdinIsTerminal(),
		}
		out := cmd.OutOrStdout()
		err := runProjectLeaveCommand(cmd.Context(), args[0], opts, out, cmd.ErrOrStderr())
		if err != nil && opts.json {
			return reportLeaveErrorJSON(out, err)
		}
		return err
	},
}

// runProjectLeaveCommand resolves the project and runs the leave. Every
// error it returns is reported by the caller, as JSON with --json.
func runProjectLeaveCommand(ctx context.Context, projectRef string, opts leaveOptions, out, errOut io.Writer) error {
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

	resolveCtx, cancel := context.WithTimeout(ctx, leaveCallTimeout)
	defer cancel()
	project, err := resolveProjectByNameOrID(resolveCtx, client, projectRef, uat)
	if err != nil {
		return projectLeaveResolveError(projectRef, err)
	}

	agents := client.ProjectAgents(project.ID)
	lio := leaveIO{
		out:    out,
		errOut: errOut,
		prompt: func(q string, allowDelete bool) (leaveAgentChoice, error) {
			return promptLeaveAgentChoice(os.Stdin, os.Stderr, q, allowDelete)
		},
		confirm:     func(q string) bool { return hubsync.ConfirmAction(q, false, false) },
		deleteAgent: leaveAgentDeleter(agents),
	}
	return runProjectLeave(ctx, client.Projects(), project, opts, lio)
}

// leaveAgentDeleter is the production delete seam: scion delete's path
// (deleteViaHubAndWait), so a delete the hub accepted (202) is polled and
// counts only once it is confirmed. onAccepted runs on a 202, before the
// poll.
func leaveAgentDeleter(agents hubclient.AgentService) func(ctx context.Context, agentID string, onAccepted func()) (hubDeleteOutcome, error) {
	return func(ctx context.Context, agentID string, onAccepted func()) (hubDeleteOutcome, error) {
		return deleteViaHubAndWait(ctx, agents, agentID, &hubclient.DeleteAgentOptions{DeleteFiles: true, RemoveBranch: true}, onAccepted)
	}
}

// leaveError is an error with a code for the --json output: the code, a
// message and optional details.
type leaveError struct {
	code    string
	msg     string
	details map[string]interface{}
	cause   error
}

func (e *leaveError) Error() string { return e.msg }
func (e *leaveError) Unwrap() error { return e.cause }

// projectLeaveNotMember is the error for a project the caller cannot
// resolve: the hub gives non-members the same answer as a missing project.
func projectLeaveNotMember(ref string) error {
	return &leaveError{code: "not_found", msg: fmt.Sprintf("not a member of %s (or it doesn't exist)", ref)}
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

// reportLeaveErrorJSON writes err as {"error": {"code", "message",
// "details"}} on out and returns a jsonReportedError, so the command exits
// non-zero with nothing else printed.
func reportLeaveErrorJSON(out io.Writer, err error) error {
	body := map[string]interface{}{"code": "error", "message": err.Error()}
	var le *leaveError
	var ue *usageError
	switch {
	case errors.As(err, &le):
		body["code"] = le.code
		if len(le.details) > 0 {
			body["details"] = le.details
		}
	case errors.As(err, &ue):
		body["code"] = "usage"
	case errors.Is(err, errProjectLeaveCancelled):
		body["code"] = "cancelled"
	}
	if werr := writeJSONTo(out, map[string]interface{}{"error": body}); werr != nil {
		return werr
	}
	return &jsonReportedError{msg: err.Error()}
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
// service, so tests can drive it. errOut takes the consequences under
// --json when a prompt is shown.
type leaveIO struct {
	out         io.Writer
	errOut      io.Writer
	prompt      func(question string, allowDelete bool) (leaveAgentChoice, error)
	confirm     func(question string) bool
	deleteAgent func(ctx context.Context, agentID string, onAccepted func()) (hubDeleteOutcome, error)
}

// errProjectLeaveCancelled is returned when the user declines or cancels
// at a prompt: the command exits non-zero, and no -y hint is shown because
// the user chose not to leave.
var errProjectLeaveCancelled = errors.New("leave cancelled")

// leaveCallTimeout bounds each hub call; prompts are not under it.
const leaveCallTimeout = 30 * time.Second

// countNoun is "1 agent" or "2 agents".
func countNoun(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// runProjectLeave runs the dry run, prints its consequences, settles the
// agents, and leaves project through svc. Nothing is deleted or left until
// the leave is confirmed; with dryRun nothing changes at all.
func runProjectLeave(ctx context.Context, svc hubclient.ProjectService, project *hubclient.Project, opts leaveOptions, lio leaveIO) error {
	dry, err := leaveCall(ctx, svc, project.ID, true)
	if err != nil {
		return leaveFailure(project, err)
	}
	if opts.dryRun {
		if opts.json {
			return writeJSONTo(lio.out, dry)
		}
		printLeaveConsequences(lio.out, project, dry)
		return nil
	}

	holdsAgents := dry.WillBeHeld
	choice := opts.agents
	// Every run that asks something on a terminal (the agent choice or the
	// yes/no) shows the consequences first.
	prompts := !opts.yes && opts.interactive
	switch {
	case !opts.json:
		printLeaveConsequences(lio.out, project, dry)
	case prompts && lio.errOut != nil:
		// The JSON on stdout stays the result alone; whoever answers the
		// prompt sees what leaving changes.
		printLeaveConsequences(lio.errOut, project, dry)
	}

	question := fmt.Sprintf("Leave project %q?", project.Name)
	switch {
	case holdsAgents && choice == leaveAgentsUnset:
		if opts.yes || !opts.interactive {
			return newUsageError("you have %s in %s that would be put on hold; pass --delete-agents or --keep-agents", countNoun(dry.Agents.Count, "agent"), project.Name)
		}
		listed := !dry.Agents.Truncated && len(dry.Agents.Items) >= dry.Agents.Count
		q := fmt.Sprintf("Leave project %q: delete your %s first, keep them on hold, or cancel?", project.Name, countNoun(dry.Agents.Count, "agent"))
		if !listed {
			q = fmt.Sprintf("Leave project %q: your %s are more than can be listed, so they cannot be deleted here; keep them on hold, or cancel?", project.Name, countNoun(dry.Agents.Count, "agent"))
		}
		choice, err = lio.prompt(q, listed)
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
		return leaveFailure(project, err)
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
// stops at the first agent whose delete is not confirmed. A 202 counts only
// once the poll confirms it; a failed, not-taken, unobservable or timed-out
// delete stops the leave. A 404 on the DELETE is an agent already gone. A
// list the hub cut short cannot be deleted in full, so it is refused before
// any delete.
func deleteLeaveAgents(ctx context.Context, agents hubclient.LeaveProjectAgents, lio leaveIO) ([]string, error) {
	if agents.Truncated || len(agents.Items) < agents.Count {
		return nil, &leaveError{code: "agent_delete_refused",
			msg: fmt.Sprintf("you have %s in the project, more than can be listed here; delete them first, or pass --keep-agents", countNoun(agents.Count, "agent"))}
	}
	deleted := make([]string, 0, len(agents.Items))
	for _, a := range agents.Items {
		label := leaveAgentLabel(a)
		callCtx, cancel := context.WithTimeout(ctx, leaveCallTimeout)
		outcome, err := lio.deleteAgent(callCtx, a.ID, func() {
			if lio.errOut != nil {
				_, _ = fmt.Fprintf(lio.errOut, "Waiting for the Hub to delete %s...\n", label)
			}
		})
		cancel()
		var cause error
		switch {
		case err != nil && apiclient.IsNotFoundError(err):
		case err != nil:
			cause = err
		case !outcome.Confirmed():
			if cause = hubDeleteFailure(label, outcome, "the agent was kept"); cause == nil {
				cause = errors.New(hubDeletePendingMessage(outcome, ""))
			}
		}
		if cause != nil {
			return deleted, leaveDeleteError(deleted, label, cause)
		}
		deleted = append(deleted, label)
	}
	return deleted, nil
}

// leaveDeleteError is the agent_delete_failed error: it names the agents
// already deleted and the one that was not, and says the project was not
// left.
func leaveDeleteError(deleted []string, failed string, cause error) error {
	msg := fmt.Sprintf("failed to delete agent %s, so the project was not left: %v", failed, cause)
	if len(deleted) > 0 {
		msg = fmt.Sprintf("deleted %s; %s", strings.Join(deleted, ", "), msg)
	}
	if deleted == nil {
		deleted = []string{}
	}
	return &leaveError{code: "agent_delete_failed", msg: msg, cause: cause,
		details: map[string]interface{}{"deleted": deleted, "failed": failed}}
}

// leaveAgentLabel names an agent of a leave result: its name, else its ID.
func leaveAgentLabel(a hubclient.LeaveProjectAgentItem) string {
	if a.Name != "" {
		return a.Name
	}
	return a.ID
}

// leaveFailure maps a failed leave request (dry run or real). The project
// was already resolved, so a 404 here is not "it doesn't exist": the hub's
// leave route answers "route not found" while hub.project_self_leave is
// off, "Project not found" to a caller with no direct role on it, and a hub
// without the route answers "Project route not found". A 403 whose
// details.credential is session_required is the session-only gate refusing
// an access token. A 409
// keeps the hub's code and message (last_owner gets the way out added; a
// group-only refusal lists the readable groups).
func leaveFailure(project *hubclient.Project, err error) error {
	var apiErr *apiclient.APIError
	if !errors.As(err, &apiErr) {
		return fmt.Errorf("failed to leave project: %w", err)
	}
	switch {
	case apiErr.StatusCode == 403 && apiErr.Details["credential"] == "session_required":
		return &leaveError{code: apiErr.Code, details: apiErr.Details, cause: err,
			msg: fmt.Sprintf("cannot leave %s: leaving needs an interactive login session (scion hub auth login); access tokens are refused", project.Name)}
	case apiErr.StatusCode == 404 && apiErr.Message == "Project route not found":
		return &leaveError{code: "unsupported", cause: err,
			msg: fmt.Sprintf("cannot leave %s: this Hub does not support project leave (it may be an older version)", project.Name)}
	case apiErr.StatusCode == 404 && apiErr.Message == "route not found":
		return &leaveError{code: "experiment_disabled", cause: err,
			msg: fmt.Sprintf("cannot leave %s: project self-leave (hub.project_self_leave) is not enabled on this Hub", project.Name)}
	case apiErr.StatusCode == 404:
		return &leaveError{code: "not_found", cause: err,
			msg: fmt.Sprintf("cannot leave %s: you have no direct role on it", project.Name)}
	case apiErr.StatusCode == 409:
		msg := fmt.Sprintf("cannot leave %s: %s", project.Name, apiErr.Message)
		if apiErr.Code == "last_owner" {
			msg += ". Transfer ownership to another member first, or delete the project."
		}
		if groups := leaveGroupNames(apiErr.Details); len(groups) > 0 {
			msg += " Groups: " + strings.Join(groups, ", ")
		}
		return &leaveError{code: apiErr.Code, msg: msg, details: apiErr.Details, cause: err}
	default:
		return &leaveError{code: apiErr.Code, msg: fmt.Sprintf("failed to leave project: %v", err), details: apiErr.Details, cause: err}
	}
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
	if dry.WillBeHeld {
		_, _ = fmt.Fprintf(out, "  Your %s in the project will be stopped and put on hold, and stay on hold if you are added back:\n", countNoun(dry.Agents.Count, "agent"))
		for _, a := range dry.Agents.Items {
			_, _ = fmt.Fprintf(out, "    - %s (%s)\n", leaveAgentLabel(a), a.Phase)
		}
		if dry.Agents.Truncated || len(dry.Agents.Items) < dry.Agents.Count {
			_, _ = fmt.Fprintf(out, "    ... and %d more\n", dry.Agents.Count-len(dry.Agents.Items))
		}
	}
	if n := dry.ProjectBoundTokens.Revoked; n > 0 {
		_, _ = fmt.Fprintf(out, "  %s limited to this project will be revoked.\n", countNoun(n, "access token"))
	}
}

// printLeaveResult prints the outcome of the real leave.
func printLeaveResult(out io.Writer, project *hubclient.Project, res *hubclient.LeaveProjectResult, deleted []string) {
	if len(deleted) > 0 {
		_, _ = fmt.Fprintf(out, "Deleted %s: %s\n", countNoun(len(deleted), "agent"), strings.Join(deleted, ", "))
	}
	_, _ = fmt.Fprintf(out, "Left project %s\n", project.Name)
	switch res.RemainingAccess {
	case "group":
		_, _ = fmt.Fprintln(out, "You keep access through a group; only your direct roles were removed.")
	case "hub":
		_, _ = fmt.Fprintln(out, "You keep access through your hub role; only your direct roles were removed.")
	}
	if res.WillBeHeld {
		_, _ = fmt.Fprintf(out, "Your %s in the project will be stopped and put on hold.\n", countNoun(res.Agents.Count, "agent"))
	}
	if n := res.ProjectBoundTokens.Revoked; n > 0 {
		_, _ = fmt.Fprintf(out, "%s limited to this project were revoked.\n", countNoun(n, "access token"))
	}
}

// promptLeaveAgentChoice asks whether to delete the agents, keep them on
// hold, or cancel, reading the answer from in and writing the question to
// out. Without allowDelete the choices are keep and cancel, and a delete
// answer cancels. An end of input with no answer, or an unknown answer,
// cancels. A read error cancels and is returned, so the command exits
// non-zero without leaving.
func promptLeaveAgentChoice(in io.Reader, out io.Writer, question string, allowDelete bool) (leaveAgentChoice, error) {
	choices := "[d]elete / [k]eep / [c]ancel"
	if !allowDelete {
		choices = "[k]eep / [c]ancel"
	}
	_, _ = fmt.Fprintf(out, "%s %s: ", question, choices)
	input, err := bufio.NewReader(in).ReadString('\n')
	if err != nil {
		_, _ = fmt.Fprintln(out)
		if !errors.Is(err, io.EOF) {
			return leaveAgentsCancel, fmt.Errorf("reading the answer: %w", err)
		}
	}
	choice := parseLeaveAgentAnswer(input)
	if choice == leaveAgentsDelete && !allowDelete {
		return leaveAgentsCancel, nil
	}
	return choice, nil
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
	projectLeaveCmd.Flags().BoolVar(&leaveDeleteAgents, "delete-agents", false, "Delete your agents in the project before leaving (waits until the Hub confirms each delete)")
	projectLeaveCmd.Flags().BoolVar(&leaveKeepAgents, "keep-agents", false, "Leave your agents in the project stopped and on hold")
	projectLeaveCmd.Flags().BoolVar(&leaveDryRun, "dry-run", false, "Show what leaving would change, and change nothing")
	projectLeaveCmd.Flags().BoolVar(&leaveJSON, "json", false, "Output the leave result, or the error, as JSON")
	projectLeaveCmd.MarkFlagsMutuallyExclusive("delete-agents", "keep-agents")
	projectCmd.AddCommand(projectLeaveCmd)
}
