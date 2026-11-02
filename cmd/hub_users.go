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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

// hubUsersProvisionNameF is the display-name flag of hub users provision.
const hubUsersProvisionNameF = "display-name"

var (
	hubUsersOutputJSON    bool
	hubUsersProvisionName string
	hubUsersProvisionNote string
	hubUsersInviteFile    string
	hubUsersInviteNote    string
	hubUsersInviteProject []string
)

// Client-side limits of hub users invite; the hub enforces the same caps.
// The command never splits a list into several requests, so one run is
// one report.
const (
	hubUsersInviteMaxEmails      = 1000
	hubUsersInviteMaxMemberships = 2000
)

var hubUsersCmd = &cobra.Command{
	Use:   "users",
	Short: "Administer hub users",
	Long: `Administer hub user records.

Examples:
  scion hub users provision alice@example.com --display-name "Alice Smith"
  scion hub users invite alice@example.com bob@example.com --project web=project-member`,
}

var hubUsersInviteCmd = &cobra.Command{
	Use:   "invite [EMAIL...]",
	Short: "Invite users, optionally with project roles",
	Long: `Invite one or more people to the hub in one request.

Addresses come from the arguments (a comma-separated list is also accepted)
and from --file: a path, or - for stdin. The file holds one address per
line, or CSV with an optional second column holding a note. Blank lines and
lines starting with # are skipped. --note applies to every address without
its own note.

--project PROJECT=ROLE[,ROLE...] gives every invited address the named
project roles; repeat it for more projects. PROJECT is a project name or ID;
ROLE is a role name (project-member, project-admin, project-owner or a
custom project role) or a role ID. You need the authority to grant those
roles through the project members page; if any requested role cannot be
granted, the hub refuses the whole request and invites no one.

Hub roles cannot be set here: the hub role is assigned at first sign-in from
the hub's configured policy.

The command prints one line per address and a summary. It exits non-zero
when the request is refused or when some address or membership did not
complete; running the same command again is safe and retries only what is
missing. At most 1000 addresses, and at most 2000 memberships (addresses
times projects), per request.

Requires the user.invite permission (hub admins hold it).

Examples:
  scion hub users invite alice@example.com bob@example.com
  scion hub users invite --file people.csv --note "Q3 cohort"
  cat list.txt | scion hub users invite --file - --project web=project-member
  scion hub users invite carol@example.com --project web=project-member,reviewer --json`,
	RunE: runHubUsersInvite,
}

var hubUsersProvisionCmd = &cobra.Command{
	Use:   "provision EMAIL",
	Short: "Pre-register a user",
	Long: `Pre-register a user on the hub.

This creates the same pending (invited) record as an admin invite, with an
optional display name. The person still signs in through a configured
sign-in provider; the record holds no credential. The role is assigned at
first sign-in from the hub's configured policy.

Running the command again with the same details is safe: it reports that
the user is already pre-registered.

Requires the user.invite permission (hub admins hold it), with an interactive
sign-in (scion hub auth login) or, where no interactive login is stored, a
hub-boundary user access token carrying user:invite (SCION_HUB_TOKEN); it is
not available on a hub running with dev auth.

Examples:
  scion hub users provision alice@example.com
  scion hub users provision bob@example.com --display-name "Bob" --note "Contractor, Q3"
  scion hub users provision carol@example.com --json`,
	Args: cobra.ExactArgs(1),
	RunE: runHubUsersProvision,
}

func init() {
	hubCmd.AddCommand(hubUsersCmd)
	hubUsersCmd.AddCommand(hubUsersProvisionCmd)
	hubUsersCmd.AddCommand(hubUsersInviteCmd)

	hubUsersProvisionCmd.Flags().StringVar(&hubUsersProvisionName, hubUsersProvisionNameF, "", "Display name to store for the user")
	hubUsersProvisionCmd.Flags().StringVar(&hubUsersProvisionNote, "note", "", "Optional admin note")
	hubUsersInviteCmd.Flags().StringVar(&hubUsersInviteFile, "file", "", "Read addresses from a file (one per line, or CSV email,note); - reads stdin")
	hubUsersInviteCmd.Flags().StringVar(&hubUsersInviteNote, "note", "", "Admin note for addresses without their own note")
	hubUsersInviteCmd.Flags().StringArrayVar(&hubUsersInviteProject, "project", nil, "Project roles for every invited address, as PROJECT=ROLE[,ROLE...]; repeatable")
	hubUsersCmd.PersistentFlags().BoolVar(&hubUsersOutputJSON, "json", false, "Output in JSON format")
}

func runHubUsersProvision(cmd *cobra.Command, args []string) error {
	_, client, err := loadHubClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	req := &hubclient.ProvisionUserRequest{Email: args[0]}
	if cmd.Flags().Changed(hubUsersProvisionNameF) {
		name := hubUsersProvisionName
		req.DisplayName = &name
	}
	if cmd.Flags().Changed("note") {
		note := hubUsersProvisionNote
		req.Note = &note
	}
	return provisionHubUser(ctx, cmd.OutOrStdout(), client.Users(), req, hubUsersOutputJSON)
}

// provisionHubUser sends one provisioning request and prints the result.
func provisionHubUser(ctx context.Context, out io.Writer, users hubclient.UserService, req *hubclient.ProvisionUserRequest, asJSON bool) error {
	resp, err := users.Provision(ctx, req)
	if err != nil {
		var pe *hubclient.ProvisionError
		if errors.As(err, &pe) {
			return fmt.Errorf("failed to provision %s: %s", req.Email, provisionErrorText(pe))
		}
		return fmt.Errorf("failed to provision %s: %w", req.Email, err)
	}

	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(resp)
	}

	if resp.Created {
		_, _ = fmt.Fprintf(out, "Provisioned %s (status %s).\n", resp.User.Email, resp.User.Status)
	} else {
		_, _ = fmt.Fprintf(out, "%s is already pre-registered with these details (status %s).\n", resp.User.Email, resp.User.Status)
	}
	if resp.User.ID != "" {
		_, _ = fmt.Fprintf(out, "User ID: %s\n", resp.User.ID)
	}
	for _, w := range resp.Warnings {
		_, _ = fmt.Fprintf(out, "Warning: %s\n", provisionWarningText(w))
	}
	return nil
}

// provisionErrorText describes a 409 or 422 provisioning response.
func provisionErrorText(pe *hubclient.ProvisionError) string {
	switch pe.Reason {
	case hubclient.ProvisionReasonPendingUserExists:
		if pe.UserID != "" {
			return fmt.Sprintf("a pending record for this email exists with different details (user ID %s)", pe.UserID)
		}
		return "a pending record for this email exists with different details"
	case hubclient.ProvisionReasonSuspendedUserExists:
		return "this email belongs to a suspended user"
	case hubclient.ProvisionReasonUserExists:
		return "user already exists"
	default:
		return pe.Message
	}
}

// provisionWarningText describes an advisory provisioning warning.
func provisionWarningText(w string) string {
	switch w {
	case "reserved_identity":
		return "this email is a reserved platform identity; sign-in will be refused"
	case "domain_not_authorized":
		return "this email is outside the hub's authorized domains; sign-in will be refused"
	case "sign_in_currently_blocked_by_access_mode":
		return "the hub's access mode currently blocks all sign-ins (domain_restricted with no domains)"
	default:
		return w
	}
}

// hubUsersInviteOptions are the inputs of one hub users invite run.
type hubUsersInviteOptions struct {
	Args     []string
	File     string
	Stdin    io.Reader
	Note     string
	Projects []string
	JSON     bool
}

// inviteProjectSpec is one parsed --project flag.
type inviteProjectSpec struct {
	Arg   string
	Roles []string
}

func runHubUsersInvite(cmd *cobra.Command, args []string) error {
	_, client, err := loadHubClient()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 90*time.Second)
	defer cancel()
	return inviteHubUsers(ctx, cmd.OutOrStdout(), client, hubUsersInviteOptions{
		Args:     args,
		File:     hubUsersInviteFile,
		Stdin:    cmd.InOrStdin(),
		Note:     hubUsersInviteNote,
		Projects: hubUsersInviteProject,
		JSON:     hubUsersOutputJSON,
	})
}

// collectInviteAddresses gathers the addresses of the arguments (split on
// commas) and of the file, in that order.
func collectInviteAddresses(opts hubUsersInviteOptions) ([]hubclient.InviteBatchEmail, error) {
	var out []hubclient.InviteBatchEmail
	for _, arg := range opts.Args {
		for _, part := range strings.Split(arg, ",") {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, hubclient.InviteBatchEmail{Email: p})
			}
		}
	}
	if opts.File != "" {
		var r io.Reader
		if opts.File == "-" {
			if opts.Stdin == nil {
				return nil, fmt.Errorf("--file -: no standard input")
			}
			r = opts.Stdin
		} else {
			f, err := os.Open(opts.File)
			if err != nil {
				return nil, fmt.Errorf("failed to open %s: %w", opts.File, err)
			}
			defer func() { _ = f.Close() }()
			r = f
		}
		records, err := parseEmailRecords(r, true, false)
		if err != nil {
			return nil, fmt.Errorf("failed to read %s: %w", opts.File, err)
		}
		for _, rec := range records {
			out = append(out, hubclient.InviteBatchEmail(rec))
		}
	}
	if opts.Note != "" {
		for i := range out {
			if out[i].Note == "" {
				out[i].Note = opts.Note
			}
		}
	}
	return out, nil
}

// parseInviteProjectFlag parses PROJECT=ROLE[,ROLE...].
func parseInviteProjectFlag(v string) (inviteProjectSpec, error) {
	project, roles, ok := strings.Cut(v, "=")
	project = strings.TrimSpace(project)
	if !ok || project == "" {
		return inviteProjectSpec{}, fmt.Errorf("invalid --project %q: expected PROJECT=ROLE[,ROLE...]", v)
	}
	spec := inviteProjectSpec{Arg: project}
	for _, r := range strings.Split(roles, ",") {
		if r = strings.TrimSpace(r); r != "" {
			spec.Roles = append(spec.Roles, r)
		}
	}
	if len(spec.Roles) == 0 {
		return inviteProjectSpec{}, fmt.Errorf("invalid --project %q: name at least one role", v)
	}
	return spec, nil
}

// resolveInviteRoles maps role names or IDs to role definition IDs through
// the project's assignable roles. An unknown role, or one the caller
// cannot grant, is a local error; the hub still checks every role.
func resolveInviteRoles(projectArg string, wanted []string, roles []hubclient.AssignableProjectRole) ([]string, error) {
	var ids []string
	seen := map[string]bool{}
	for _, w := range wanted {
		var match *hubclient.AssignableProjectRole
		for i := range roles {
			if roles[i].ID == w || roles[i].Name == w {
				match = &roles[i]
				break
			}
		}
		if match == nil {
			names := make([]string, 0, len(roles))
			for _, r := range roles {
				names = append(names, r.Name)
			}
			sort.Strings(names)
			return nil, fmt.Errorf("project %s has no assignable role %q; roles: %s", projectArg, w, strings.Join(names, ", "))
		}
		if !match.Grantable {
			reason := match.Reason
			if reason == "" {
				reason = match.DenialCode
			}
			return nil, fmt.Errorf("you cannot grant role %s in project %s: %s", match.Name, projectArg, reason)
		}
		if !seen[match.ID] {
			seen[match.ID] = true
			ids = append(ids, match.ID)
		}
	}
	return ids, nil
}

// inviteHubUsers builds one batch invite request, sends it and prints the
// result. It returns an error when the request is refused or the result is
// not complete.
func inviteHubUsers(ctx context.Context, out io.Writer, client hubclient.Client, opts hubUsersInviteOptions) error {
	emails, err := collectInviteAddresses(opts)
	if err != nil {
		return err
	}
	if len(emails) == 0 {
		return fmt.Errorf("no addresses given: pass addresses as arguments or with --file")
	}
	if len(emails) > hubUsersInviteMaxEmails {
		return fmt.Errorf("%d addresses is more than %d per request; split the list", len(emails), hubUsersInviteMaxEmails)
	}
	if len(emails)*len(opts.Projects) > hubUsersInviteMaxMemberships {
		return fmt.Errorf("%d addresses times %d projects is more than %d memberships per request; split the list",
			len(emails), len(opts.Projects), hubUsersInviteMaxMemberships)
	}

	req := &hubclient.InviteBatchRequest{Emails: emails}
	labels := map[string]string{}
	for _, v := range opts.Projects {
		spec, err := parseInviteProjectFlag(v)
		if err != nil {
			return err
		}
		project, err := resolveProjectNameOrID(ctx, client, spec.Arg)
		if err != nil {
			return fmt.Errorf("project %s: %w", spec.Arg, err)
		}
		if _, dup := labels[project.ID]; dup {
			return fmt.Errorf("project %s is given more than once", spec.Arg)
		}
		roles, err := client.Projects().AssignableRoles(ctx, project.ID)
		if err != nil {
			return fmt.Errorf("failed to list roles of project %s: %w", spec.Arg, err)
		}
		ids, err := resolveInviteRoles(spec.Arg, spec.Roles, roles)
		if err != nil {
			return err
		}
		labels[project.ID] = spec.Arg
		req.Projects = append(req.Projects, hubclient.InviteBatchProject{ProjectID: project.ID, RoleDefinitionIDs: ids})
	}

	resp, err := client.Users().InviteBatch(ctx, req)
	if err != nil {
		var be *hubclient.InviteBatchError
		if errors.As(err, &be) && opts.JSON {
			enc := json.NewEncoder(out)
			enc.SetIndent("", "  ")
			_ = enc.Encode(struct {
				Error    string                                `json:"error"`
				Reason   string                                `json:"reason"`
				BatchID  string                                `json:"batchId,omitempty"`
				Projects []hubclient.InviteBatchProjectRefusal `json:"projects"`
			}{Error: be.Message, Reason: hubclient.InviteBatchReasonProjectsNotGrantable, BatchID: be.BatchID, Projects: be.Projects})
			return fmt.Errorf("invite refused: %s", be.Message)
		}
		var apiErr *apiclient.APIError
		if opts.JSON && errors.As(err, &apiErr) {
			enc := json.NewEncoder(out)
			enc.SetIndent("", "  ")
			_ = enc.Encode(struct {
				Error   string                 `json:"error"`
				Code    string                 `json:"code,omitempty"`
				Status  int                    `json:"status"`
				Details map[string]interface{} `json:"details,omitempty"`
			}{Error: apiErr.Message, Code: apiErr.Code, Status: apiErr.StatusCode, Details: apiErr.Details})
			return fmt.Errorf("failed to invite users: %w", err)
		}
		if errors.As(err, &be) {
			_, _ = fmt.Fprintln(out, "The hub refused the request; no one was invited. Refused project entries:")
			for _, r := range be.Projects {
				label := labels[r.ProjectID]
				if label == "" {
					label = r.ProjectID
				}
				line := fmt.Sprintf("  %s: %s", label, r.Reason)
				if r.RoleDefinitionID != "" {
					line += " (role " + r.RoleDefinitionID + ")"
				}
				if r.DenialCode != "" {
					line += " [" + r.DenialCode + "]"
				}
				_, _ = fmt.Fprintln(out, line)
			}
			return fmt.Errorf("invite refused: %s", be.Message)
		}
		return fmt.Errorf("failed to invite users: %w", err)
	}

	if opts.JSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(resp); err != nil {
			return err
		}
	} else {
		printInviteBatchResult(out, resp, labels)
	}
	if !resp.Complete {
		return fmt.Errorf("some addresses or memberships did not complete; run the same command again to retry them")
	}
	return nil
}

// printInviteBatchResult prints one line per address and a summary.
func printInviteBatchResult(out io.Writer, resp *hubclient.InviteBatchResponse, labels map[string]string) {
	for _, r := range resp.Results {
		addr := r.Email
		if addr == "" {
			addr = r.Input
		}
		line := fmt.Sprintf("%-16s %s", r.Status, addr)
		if r.Reason != "" && r.Status != hubclient.InviteBatchStatusCreated {
			line += "  (" + r.Reason
			if r.DuplicateOf != nil {
				line += fmt.Sprintf(" of entry %d", *r.DuplicateOf+1)
			}
			line += ")"
		}
		var ms []string
		for _, m := range r.Memberships {
			if m.Status == hubclient.InviteBatchMembershipSkipped {
				continue
			}
			label := labels[m.ProjectID]
			if label == "" {
				label = m.ProjectID
			}
			item := label + ": " + m.Status
			if m.Code != "" {
				item += " " + m.Code
			}
			ms = append(ms, item)
		}
		if len(ms) > 0 {
			line += "  [" + strings.Join(ms, "; ") + "]"
		}
		_, _ = fmt.Fprintln(out, line)
	}
	c := resp.Counts
	summary := fmt.Sprintf("%d addresses: %d created, %d already invited, %d already active, %d suspended",
		resp.Total, c.Created, c.AlreadyInvited, c.AlreadyActive, c.Suspended)
	if c.Exists > 0 {
		summary += fmt.Sprintf(", %d existing", c.Exists)
	}
	summary += fmt.Sprintf(", %d invalid, %d duplicate, %d failed", c.Invalid, c.Duplicate, c.Failed)
	if c.MembershipFailed > 0 {
		summary += fmt.Sprintf("; %d memberships failed", c.MembershipFailed)
	}
	_, _ = fmt.Fprintln(out, summary)
}
