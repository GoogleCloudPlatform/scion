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
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

// scion hub test-identity (ptone/scion#4240): issue, re-issue tokens for,
// mint web-login codes for, list and delete hub test identities, tear down
// a run, and (human only) manage workload issuer delegations.
//
// Without a hub profile the commands use the CLI's usual hub credential
// and are human only. With a target-hub profile (hub_profile.go) they act
// as the profile's issuer delegation, keep issued identities in the run
// cache, and also run in agent mode, but only against a target whose
// endpoint is not the agent's own hub. delegate is the human consent step
// on the target: human mode only, never with a profile. web-code is not
// available with a profile (it is not in hubProfileAllowed).
//
// A token or web-login code is a secret. Without a profile it is written
// only to the file named by --out, created exclusively (never overwriting
// an existing path or following a symlink) with mode 0600; with a profile
// a token goes only to the run cache (0600, exclusive, symlinks refused).
// Standard output and standard error carry only non-secret facts: the
// identity's ID, email, role, expiry, the code ID and the output file path.

var hubTestIdentityCmd = &cobra.Command{
	Use:   "test-identity",
	Short: "Manage hub test identities (short-lived synthetic users)",
	Long: `Manage hub test identities: short-lived synthetic member or viewer
users for testing on a test hub.

The hub must run with --enable-test-identities, and you need the
test_identity.issue permission: an admin session, or a hub-bound access
token carrying the test_identity:issue scope (the intended credential for
CI and operators, set in SCION_HUB_TOKEN).

The access token of a test identity is written only to the file named by
--out, which must not exist yet; it is created with mode 0600. The token is
never printed.

With a target-hub profile (see 'scion hub profile'), the commands act as the
profile's issuer delegation on another test hub, issued identities go to the
run cache (issue --as <alias> --run <label>), 'teardown --run <label>'
removes a whole run, and the commands also run in agent mode, but only
against a profile for another hub. 'delegate' is the human consent step on
the target hub.

Examples:
  # Issue a member identity; its token goes to ./fixture.token
  scion hub test-identity issue --role member --ttl 30m --purpose "e2e run" --out ./fixture.token

  # Issue a new token for the same identity (before it expires)
  scion hub test-identity token <id> --out ./fixture-2.token

  # List your live test identities
  scion hub test-identity list

  # Mint a single-use web-login code for a browser session; it goes to ./code.txt
  scion hub test-identity web-code <id> --out ./code.txt

  # Delete a test identity when the run is done
  scion hub test-identity delete <id>

  # With a target-hub profile: issue into the run cache, then tear the run down
  scion --hub-profile t1 hub test-identity issue --as owner --run r1
  scion --hub-profile t1 hub test-identity teardown --run r1`,
}

var hubTestIdentityIssueCmd = &cobra.Command{
	Use:   "issue --out FILE",
	Short: "Issue a test identity and write its token to a file",
	Long: `Issue a new test identity and write its access token to --out.

--out must name a path that does not exist (an existing file or a symlink
is refused). The file is created with mode 0600. Only the identity's ID,
email, role and expiry are printed.

--ttl is the token lifetime (default 30m on the hub); the token never
outlives the identity. --lifetime is the identity lifetime (default 1h on
the hub, at most 8h).

Examples:
  scion hub test-identity issue --out ./fixture.token
  scion hub test-identity issue --role viewer --ttl 15m --lifetime 2h --purpose "nightly" --out /tmp/viewer.token`,
	Args: cobra.NoArgs,
	RunE: runHubTestIdentityIssue,
}

var hubTestIdentityTokenCmd = &cobra.Command{
	Use:   "token [ID] [--out FILE]",
	Short: "Issue a new token for a live test identity",
	Long: `Issue a new access token for a live test identity you issued, and
write it to --out (which must not exist; created with mode 0600).

With a hub profile and --as <alias> (and --run), the token of that cached
identity is re-issued through the profile's issuer delegation and stored in
the run cache instead (no ID, no --out).

Examples:
  scion hub test-identity token 6f1c... --out ./fixture-2.token
  scion hub test-identity token 6f1c... --ttl 10m --out ./short.token
  scion --hub-profile t1 hub test-identity token --as owner --run r1`,
	Args: cobra.MaximumNArgs(1),
	RunE: runHubTestIdentityToken,
}

var hubTestIdentityListCmd = &cobra.Command{
	Use:   "list",
	Short: "List your test identities",
	Long: `List the test identities you issued (every identity for an admin
session). Only live identities are listed unless --include-expired is set.

Examples:
  scion hub test-identity list
  scion hub test-identity list --include-expired --limit 20
  scion hub test-identity list --json`,
	Args: cobra.NoArgs,
	RunE: runHubTestIdentityList,
}

var hubTestIdentityDeleteCmd = &cobra.Command{
	Use:   "delete [ID]",
	Short: "Delete a test identity",
	Long: `Delete a test identity. Its role bindings and group memberships go
with it and its tokens stop working at once.

The delete is refused while the identity still owns agents, or is the
last owner of a project: delete those first, or use --purge. Deleting an
identity that no longer exists succeeds with a notice.

With --purge the hub also removes what the identity created, directly or
through its agents: its agents (with their files on the broker), schedules,
groups and projects. Only resources the hub recorded as created under the
identity are removed. A project that has another owner, or resources from
someone else, is kept (the identity's own access to it is removed). Agents
the identity owns without that record are listed as unproven and never
removed. When the purge cannot finish (for example a broker is offline) it
lists what is left and exits non-zero; run it again to finish.

With a hub profile and --as <alias> (and --run), the cached identity is
deleted and removed from the run cache (no ID).

Examples:
  scion hub test-identity delete 6f1c...
  scion hub test-identity delete 6f1c... --purge
  scion --hub-profile t1 hub test-identity delete --as owner --run r1`,
	Args: cobra.MaximumNArgs(1),
	RunE: runHubTestIdentityDelete,
}

var hubTestIdentityWebCodeCmd = &cobra.Command{
	Use:   "web-code ID --out FILE",
	Short: "Mint a single-use web-login code for a browser session as a test identity",
	Long: `Mint a single-use web-login code for a live test identity you issued, and
write it to --out. A browser (for example a Playwright test) redeems the
code with POST /auth/test-identity/redeem and then holds a web session as
the test identity until the identity expires.

The code is valid for 5 minutes and can be redeemed once. Minting a new
code replaces any earlier code that was not redeemed yet. The hub must run
with test identities enabled at the member level and carry the test
infrastructure marker ("scion server test-infra mark"), which holds the
hub-local web session key; otherwise the hub answers that web sessions are
not available.

--out must name a path that does not exist yet: an existing file, a
symlink, or a path under a symlinked directory is refused. The file is
created with mode 0600 and holds only the code. The code is never printed;
only the identity ID, the code ID, the code expiry and the file path are.

Examples:
  scion hub test-identity web-code 6f1c... --out ./code.txt
  scion hub test-identity web-code 6f1c... --out /tmp/run-42/code.txt --json`,
	Args: cobra.ExactArgs(1),
	RunE: runHubTestIdentityWebCode,
}

// Flag values.
var (
	testIdentityRole           string
	testIdentityTTL            time.Duration
	testIdentityLifetime       time.Duration
	testIdentityPurpose        string
	testIdentityOut            string
	testIdentityJSON           bool
	testIdentityListLimit      int
	testIdentityIncludeExpired bool
	testIdentityPurge          bool
)

func init() {
	hubCmd.AddCommand(hubTestIdentityCmd)
	hubTestIdentityCmd.AddCommand(hubTestIdentityIssueCmd)
	hubTestIdentityCmd.AddCommand(hubTestIdentityTokenCmd)
	hubTestIdentityCmd.AddCommand(hubTestIdentityListCmd)
	hubTestIdentityCmd.AddCommand(hubTestIdentityDeleteCmd)
	hubTestIdentityCmd.AddCommand(hubTestIdentityWebCodeCmd)
	hubTestIdentityDeleteCmd.Flags().BoolVar(&testIdentityPurge, "purge", false, "Also remove the agents, schedules, groups and projects the identity created")

	hubTestIdentityIssueCmd.Flags().StringVar(&testIdentityRole, "role", "member", `Role of the identity: "member" or "viewer"`)
	hubTestIdentityIssueCmd.Flags().DurationVar(&testIdentityTTL, "ttl", 0, "Token lifetime, e.g. 30m (default: the hub's, 30m)")
	hubTestIdentityIssueCmd.Flags().DurationVar(&testIdentityLifetime, "lifetime", 0, "Identity lifetime, e.g. 2h (default: the hub's, 1h; at most 8h)")
	hubTestIdentityIssueCmd.Flags().StringVar(&testIdentityPurpose, "purpose", "", "Short label recorded on the identity and in the audit log")
	hubTestIdentityIssueCmd.Flags().StringVar(&testIdentityOut, "out", "", "File to write the token to (required without a hub profile; must not exist; created 0600)")
	hubTestIdentityIssueCmd.Flags().BoolVar(&testIdentityJSON, "json", false, "Print the identity as JSON (never the token)")

	hubTestIdentityTokenCmd.Flags().DurationVar(&testIdentityTTL, "ttl", 0, "Token lifetime, e.g. 30m (default: the hub's, 30m)")
	hubTestIdentityTokenCmd.Flags().StringVar(&testIdentityOut, "out", "", "File to write the token to (required unless --as names a cached identity; must not exist; created 0600)")
	hubTestIdentityTokenCmd.Flags().BoolVar(&testIdentityJSON, "json", false, "Print the identity as JSON (never the token)")

	hubTestIdentityWebCodeCmd.Flags().StringVar(&testIdentityOut, "out", "", "File to write the code to (required; must not exist; created 0600)")
	hubTestIdentityWebCodeCmd.Flags().BoolVar(&testIdentityJSON, "json", false, "Print the code ID, expiry and file as JSON (never the code)")
	_ = hubTestIdentityWebCodeCmd.MarkFlagRequired("out")

	hubTestIdentityListCmd.Flags().IntVar(&testIdentityListLimit, "limit", 0, "Maximum number of identities to list (default: the hub's, 100)")
	hubTestIdentityListCmd.Flags().BoolVar(&testIdentityIncludeExpired, "include-expired", false, "Also list expired identities")
	hubTestIdentityListCmd.Flags().BoolVar(&testIdentityJSON, "json", false, "Output in JSON format")
}

// testIdentityIssueOptions are the inputs of issueTestIdentity.
type testIdentityIssueOptions struct {
	Role     string
	TTL      time.Duration
	Lifetime time.Duration
	Purpose  string
	Out      string
	JSON     bool
}

// testIdentityWebCodeOptions are the inputs of mintTestIdentityWebCode.
type testIdentityWebCodeOptions struct {
	ID   string
	Out  string
	JSON bool
}

// testIdentityTokenOptions are the inputs of reissueTestIdentityToken.
type testIdentityTokenOptions struct {
	ID   string
	TTL  time.Duration
	Out  string
	JSON bool
}

func runHubTestIdentityIssue(cmd *cobra.Command, _ []string) error {
	if hubProfileSelected() {
		return runProfileTestIdentityIssue(cmd)
	}
	if testIdentityOut == "" {
		return newUsageError("--out is required: the token is written only to a file")
	}
	_, client, err := loadHubClient()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()
	return issueTestIdentity(ctx, cmd.OutOrStdout(), cmd.ErrOrStderr(), client.TestIdentities(), testIdentityIssueOptions{
		Role: testIdentityRole, TTL: testIdentityTTL, Lifetime: testIdentityLifetime,
		Purpose: testIdentityPurpose, Out: testIdentityOut, JSON: testIdentityJSON,
	})
}

func runHubTestIdentityToken(cmd *cobra.Command, args []string) error {
	if hubProfileSelected() && hubProfileAs != "" {
		return runProfileTestIdentityReissue(cmd, args)
	}
	if len(args) != 1 {
		return newUsageError("name the test identity: token ID --out FILE")
	}
	if testIdentityOut == "" {
		return newUsageError("--out is required: the token is written only to a file")
	}
	client, err := testIdentityIssuerClient()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()
	return reissueTestIdentityToken(ctx, cmd.OutOrStdout(), cmd.ErrOrStderr(), client.TestIdentities(), testIdentityTokenOptions{
		ID: args[0], TTL: testIdentityTTL, Out: testIdentityOut, JSON: testIdentityJSON,
	})
}

func runHubTestIdentityWebCode(cmd *cobra.Command, args []string) error {
	_, client, err := loadHubClient()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()
	return mintTestIdentityWebCode(ctx, cmd.OutOrStdout(), client.TestIdentities(), testIdentityWebCodeOptions{
		ID: args[0], Out: testIdentityOut, JSON: testIdentityJSON,
	})
}

// testIdentityIssuerClient returns the client test identity commands act
// with: the profile's issuer delegation when a profile is selected (and
// --as is refused), the CLI's usual hub client otherwise.
func testIdentityIssuerClient() (hubclient.Client, error) {
	if hubProfileSelected() {
		if hubProfileAs != "" {
			return nil, errors.New("--as does not apply to this command: test identity commands act as the profile's issuer delegation")
		}
		s, err := openHubProfileSession(false)
		if err != nil {
			return nil, err
		}
		return s.IssuerClient()
	}
	_, client, err := loadHubClient()
	return client, err
}

func runHubTestIdentityList(cmd *cobra.Command, _ []string) error {
	client, err := testIdentityIssuerClient()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()
	return listTestIdentities(ctx, cmd.OutOrStdout(), client.TestIdentities(), &hubclient.ListTestIdentitiesOptions{
		Limit: testIdentityListLimit, IncludeExpired: testIdentityIncludeExpired,
	}, testIdentityJSON)
}

func runHubTestIdentityDelete(cmd *cobra.Command, args []string) error {
	if hubProfileSelected() && hubProfileAs != "" {
		return runProfileTestIdentityDeleteAlias(cmd, args)
	}
	if len(args) != 1 {
		return newUsageError("name the test identity: delete ID")
	}
	client, err := testIdentityIssuerClient()
	if err != nil {
		return err
	}
	if testIdentityPurge {
		// A purge waits for each agent's delete on its broker.
		ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Minute)
		defer cancel()
		return purgeTestIdentity(ctx, cmd.OutOrStdout(), cmd.ErrOrStderr(), client.TestIdentities(), args[0])
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()
	return deleteTestIdentity(ctx, cmd.OutOrStdout(), cmd.ErrOrStderr(), client.TestIdentities(), args[0])
}

// purgeTestIdentity purges a test identity and prints the hub's report.
// An incomplete purge prints what is left and returns an error.
func purgeTestIdentity(ctx context.Context, out, errOut io.Writer, svc hubclient.TestIdentityService, id string) error {
	report, err := svc.Purge(ctx, id)
	if report != nil {
		printTestIdentityPurgeReport(out, report)
	}
	if err != nil {
		if report != nil {
			return fmt.Errorf("purge of test identity %s did not complete; run the command again to finish", id)
		}
		return fmt.Errorf("failed to purge test identity %s: %w", id, err)
	}
	return nil
}

func printTestIdentityPurgeReport(out io.Writer, r *hubclient.TestIdentityPurgeReport) {
	d := r.Deleted
	_, _ = fmt.Fprintf(out, "Purge of test identity %s: removed %d agents, %d schedules, %d groups, %d projects.\n",
		r.IdentityID, d.Agents, d.Schedules, d.Groups, d.Projects)
	if r.IdentityDeleted {
		_, _ = fmt.Fprintf(out, "Deleted test identity %s.\n", r.IdentityID)
	} else if r.IdentityKeptReason != "" {
		_, _ = fmt.Fprintf(out, "Test identity %s was kept: %s.\n", r.IdentityID, r.IdentityKeptReason)
	}
	for _, p := range r.KeptProjects {
		line := fmt.Sprintf("  kept project %s (%s); the identity's access was removed", p.ID, p.Reason)
		if p.Ownerless {
			line += "; the project now has no owner"
		}
		_, _ = fmt.Fprintln(out, line)
	}
	for _, it := range r.NotDone {
		_, _ = fmt.Fprintf(out, "  not done: %s %s: %s %s\n", it.Type, it.ID, it.Status, it.Reason)
	}
	for _, it := range r.Unproven {
		_, _ = fmt.Fprintf(out, "  unproven: %s %s in project %s (not recorded as created by the identity; a real administrator must decide)\n", it.Type, it.ID, it.ProjectID)
	}
	if r.UnprovenTruncated {
		_, _ = fmt.Fprintln(out, "  more unproven agents exist than are listed")
	}
	if r.Truncated {
		_, _ = fmt.Fprintln(out, "  more resources remain; run the command again")
	}
}

// wholeSeconds converts a flag duration to the API's whole seconds. A
// zero duration means "the hub default".
func wholeSeconds(flag string, d time.Duration) (int64, error) {
	if d == 0 {
		return 0, nil
	}
	if d < time.Second || d%time.Second != 0 {
		return 0, newUsageError("invalid --%s %s: must be a whole number of seconds, at least 1s", flag, d)
	}
	return int64(d / time.Second), nil
}

// tokenFile is a token output file created exclusively before the token is
// requested, so a refused path costs no token and no other process can
// substitute the file between the check and the write.
type tokenFile struct {
	path string
	f    *os.File
}

// createTokenFile creates path for writing a token: exclusively
// (O_CREATE|O_EXCL, so an existing file and any symlink, even a dangling
// one, are refused) with mode 0600.
func createTokenFile(path string) (*tokenFile, error) {
	if path == "" {
		return nil, newUsageError("--out is required: the token is written only to a file")
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("refusing --out %s: it is a symlink; name a new file", path)
		}
		return nil, fmt.Errorf("refusing --out %s: it already exists; name a new file", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("cannot use --out %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("refusing --out %s: it already exists; name a new file", path)
		}
		return nil, fmt.Errorf("cannot create --out %s: %w", path, err)
	}
	// The umask can only narrow 0600; set it exactly anyway.
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("cannot set mode 0600 on --out %s: %w", path, err)
	}
	return &tokenFile{path: path, f: f}, nil
}

// abandon closes and removes the file, which holds no token yet.
func (t *tokenFile) abandon() {
	_ = t.f.Close()
	_ = os.Remove(t.path)
}

// write writes the token and a newline, and closes the file.
func (t *tokenFile) write(token string) error {
	return t.writeExact(token + "\n")
}

// writeExact writes content as is, syncs and closes the file. On any
// failure it closes and removes the file, so no partial secret is left.
func (t *tokenFile) writeExact(content string) error {
	_, err := io.WriteString(t.f, content)
	if err == nil {
		err = t.f.Sync()
	}
	if cerr := t.f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(t.path)
	}
	return err
}

// testIdentityOutput is the --json output of issue and token. It has no
// token field.
type testIdentityOutput struct {
	Identity       hubclient.TestIdentity `json:"identity"`
	TokenFile      string                 `json:"tokenFile"`
	TokenExpiresAt time.Time              `json:"tokenExpiresAt"`
}

func formatTestIdentityTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

// printTestIdentityToken prints the non-secret facts of an issue or token
// response. It never prints resp.AccessToken.
func printTestIdentityToken(out io.Writer, verb string, resp *hubclient.TestIdentityTokenResponse, path string, asJSON bool) error {
	absPath := path
	if abs, err := filepath.Abs(path); err == nil {
		absPath = abs
	}
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(testIdentityOutput{Identity: resp.Identity, TokenFile: absPath, TokenExpiresAt: resp.TokenExpiresAt})
	}
	id := resp.Identity
	_, _ = fmt.Fprintf(out, "%s test identity %s\n", verb, id.ID)
	_, _ = fmt.Fprintf(out, "  Email:         %s\n", id.Email)
	_, _ = fmt.Fprintf(out, "  Role:          %s\n", id.Role)
	_, _ = fmt.Fprintf(out, "  Expires:       %s\n", formatTestIdentityTime(id.ExpiresAt))
	_, _ = fmt.Fprintf(out, "  Token expires: %s\n", formatTestIdentityTime(resp.TokenExpiresAt))
	if id.Purpose != "" {
		_, _ = fmt.Fprintf(out, "  Purpose:       %s\n", id.Purpose)
	}
	_, _ = fmt.Fprintf(out, "  Token file:    %s (mode 0600)\n", absPath)
	return nil
}

// issueTestIdentity issues a test identity and writes its token to opts.Out.
func issueTestIdentity(ctx context.Context, out, errOut io.Writer, svc hubclient.TestIdentityService, opts testIdentityIssueOptions) error {
	role := strings.TrimSpace(opts.Role)
	if role == "" {
		role = "member"
	}
	if role != "member" && role != "viewer" {
		return newUsageError(`invalid --role %q: must be "member" or "viewer"`, opts.Role)
	}
	ttl, err := wholeSeconds("ttl", opts.TTL)
	if err != nil {
		return err
	}
	lifetime, err := wholeSeconds("lifetime", opts.Lifetime)
	if err != nil {
		return err
	}
	file, err := createTokenFile(opts.Out)
	if err != nil {
		return err
	}
	resp, err := svc.Issue(ctx, &hubclient.IssueTestIdentityRequest{
		Role: role, Purpose: opts.Purpose, LifetimeSeconds: lifetime, TokenTTLSeconds: ttl,
	})
	if err != nil {
		file.abandon()
		return fmt.Errorf("failed to issue test identity: %w", err)
	}
	if err := file.write(resp.AccessToken); err != nil {
		_, _ = fmt.Fprintf(errOut, "Test identity %s was issued, but its token could not be written; run \"scion hub test-identity token %s --out <file>\" for a new one, or delete it.\n", resp.Identity.ID, resp.Identity.ID)
		return fmt.Errorf("failed to write --out %s: %w", file.path, err)
	}
	return printTestIdentityToken(out, "Issued", resp, file.path, opts.JSON)
}

// reissueTestIdentityToken issues a new token for a live test identity and
// writes it to opts.Out.
func reissueTestIdentityToken(ctx context.Context, out, errOut io.Writer, svc hubclient.TestIdentityService, opts testIdentityTokenOptions) error {
	ttl, err := wholeSeconds("ttl", opts.TTL)
	if err != nil {
		return err
	}
	file, err := createTokenFile(opts.Out)
	if err != nil {
		return err
	}
	resp, err := svc.Token(ctx, opts.ID, &hubclient.TestIdentityTokenRequest{TokenTTLSeconds: ttl})
	if err != nil {
		file.abandon()
		return fmt.Errorf("failed to issue a token for test identity %s: %w", opts.ID, err)
	}
	if err := file.write(resp.AccessToken); err != nil {
		_, _ = fmt.Fprintf(errOut, "A token for test identity %s was issued, but could not be written; run this command again with a new --out.\n", opts.ID)
		return fmt.Errorf("failed to write --out %s: %w", file.path, err)
	}
	return printTestIdentityToken(out, "New token for", resp, file.path, opts.JSON)
}

// refuseSymlinkedParents refuses path when any directory component of it
// is a symlink, so the file cannot land somewhere other than the directory
// named. Components are checked on the absolute path; a component that does
// not exist is left to the exclusive create, which then fails.
func refuseSymlinkedParents(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("cannot use --out %s: %w", path, err)
	}
	dir := filepath.Dir(abs)
	for {
		fi, err := os.Lstat(dir)
		if err == nil && fi.Mode()&os.ModeSymlink != 0 {
			hint := ""
			if resolved, rerr := filepath.EvalSymlinks(filepath.Dir(abs)); rerr == nil {
				hint = fmt.Sprintf(" (use the resolved directory %s)", resolved)
			}
			return fmt.Errorf("refusing --out %s: directory %s is a symlink; name a path without symlinked directories%s", path, dir, hint)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil
		}
		dir = parent
	}
}

// testIdentityWebCodeOutput is the --json output of web-code. It has no
// code field.
type testIdentityWebCodeOutput struct {
	IdentityID string    `json:"identityId"`
	CodeID     string    `json:"codeId"`
	ExpiresAt  time.Time `json:"expiresAt"`
	CodeFile   string    `json:"codeFile"`
}

// mintTestIdentityWebCode mints a web-login code for a live test identity
// and writes only the code to opts.Out. The code never reaches out or an
// error.
func mintTestIdentityWebCode(ctx context.Context, out io.Writer, svc hubclient.TestIdentityService, opts testIdentityWebCodeOptions) error {
	if opts.Out == "" {
		return newUsageError("--out is required: the code is written only to a file")
	}
	if err := refuseSymlinkedParents(opts.Out); err != nil {
		return err
	}
	file, err := createTokenFile(opts.Out)
	if err != nil {
		return err
	}
	resp, err := svc.WebLoginCode(ctx, opts.ID)
	if err != nil {
		file.abandon()
		if apiclient.IsNotFoundError(err) {
			return fmt.Errorf("failed to mint a web-login code for test identity %s (not found, not one you issued, or web sessions are not available on this hub): %w", opts.ID, err)
		}
		return fmt.Errorf("failed to mint a web-login code for test identity %s: %w", opts.ID, err)
	}
	if err := file.writeExact(resp.Code); err != nil {
		return fmt.Errorf("failed to write --out %s; mint a new code with a new --out: %w", file.path, err)
	}
	absPath := file.path
	if abs, err := filepath.Abs(file.path); err == nil {
		absPath = abs
	}
	if opts.JSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(testIdentityWebCodeOutput{IdentityID: opts.ID, CodeID: resp.CodeID, ExpiresAt: resp.ExpiresAt, CodeFile: absPath})
	}
	_, _ = fmt.Fprintf(out, "Web-login code for test identity %s\n", opts.ID)
	_, _ = fmt.Fprintf(out, "  Code ID:       %s\n", resp.CodeID)
	_, _ = fmt.Fprintf(out, "  Expires:       %s (single use)\n", formatTestIdentityTime(resp.ExpiresAt))
	_, _ = fmt.Fprintf(out, "  Code file:     %s (mode 0600)\n", absPath)
	return nil
}

// listTestIdentities lists test identities as the hub returns them: the
// caller's own, or every identity for an admin session.
func listTestIdentities(ctx context.Context, out io.Writer, svc hubclient.TestIdentityService, opts *hubclient.ListTestIdentitiesOptions, asJSON bool) error {
	if opts != nil && opts.Limit < 0 {
		return newUsageError("invalid --limit %d: must be positive", opts.Limit)
	}
	resp, err := svc.List(ctx, opts)
	if err != nil {
		return fmt.Errorf("failed to list test identities: %w", err)
	}
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(resp)
	}
	if len(resp.Items) == 0 {
		_, _ = fmt.Fprintln(out, "No test identities.")
		return nil
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tEMAIL\tROLE\tLIVE\tEXPIRES\tPURPOSE")
	for _, it := range resp.Items {
		live := "no"
		if it.Live {
			live = "yes"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", it.ID, it.Email, it.Role, live, formatTestIdentityTime(it.ExpiresAt), it.Purpose)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if resp.Truncated {
		_, _ = fmt.Fprintln(out, "(more identities match; raise --limit to see them)")
	}
	return nil
}

// deleteTestIdentity deletes a test identity. A 404 (it no longer exists,
// or it is not one of the caller's) succeeds with a notice, so a teardown
// step can run more than once.
func deleteTestIdentity(ctx context.Context, out, errOut io.Writer, svc hubclient.TestIdentityService, id string) error {
	err := svc.Delete(ctx, id)
	if err == nil {
		_, _ = fmt.Fprintf(out, "Deleted test identity %s.\n", id)
		return nil
	}
	if apiclient.IsNotFoundError(err) {
		_, _ = fmt.Fprintf(errOut, "Notice: test identity %s was not found (already deleted, or not one you may delete); nothing to do.\n", id)
		return nil
	}
	if agents, projects, ok := hubclient.TestIdentityDeleteConflict(err); ok && (len(agents) > 0 || len(projects) > 0) {
		var b strings.Builder
		if len(agents) > 0 {
			fmt.Fprintf(&b, "test identity %s still owns agents; delete them first:\n", id)
			for _, a := range agents {
				fmt.Fprintf(&b, "  agent %s (%s) in project %s\n", a.Slug, a.ID, a.ProjectID)
			}
		}
		if len(projects) > 0 {
			fmt.Fprintf(&b, "test identity %s is the last owner of these projects; delete them first, or use --purge:\n", id)
			for _, p := range projects {
				fmt.Fprintf(&b, "  project %s (%s)\n", p.Name, p.ID)
			}
		}
		return errors.New(strings.TrimRight(b.String(), "\n"))
	}
	return fmt.Errorf("failed to delete test identity %s: %w", id, err)
}
