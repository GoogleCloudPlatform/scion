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
// list and delete hub test identities. Available in human mode only: the
// "hub" subtree is not in agentAllowed (cli_mode.go), since issuance needs
// a user or issuer credential that agents do not hold.
//
// A token is a secret. It is written only to the file named by --out,
// created exclusively (never overwriting an existing path or following a
// symlink) with mode 0600. Standard output and standard error carry only
// non-secret facts: the identity's ID, email, role, expiry and the token
// file path.

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

Examples:
  # Issue a member identity; its token goes to ./fixture.token
  scion hub test-identity issue --role member --ttl 30m --purpose "e2e run" --out ./fixture.token

  # Issue a new token for the same identity (before it expires)
  scion hub test-identity token <id> --out ./fixture-2.token

  # List your live test identities
  scion hub test-identity list

  # Delete a test identity when the run is done
  scion hub test-identity delete <id>`,
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
	Use:   "token ID --out FILE",
	Short: "Issue a new token for a live test identity",
	Long: `Issue a new access token for a live test identity you issued, and
write it to --out (which must not exist; created with mode 0600).

Examples:
  scion hub test-identity token 6f1c... --out ./fixture-2.token
  scion hub test-identity token 6f1c... --ttl 10m --out ./short.token`,
	Args: cobra.ExactArgs(1),
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
	Use:   "delete ID",
	Short: "Delete a test identity",
	Long: `Delete a test identity. Its role bindings and group memberships go
with it and its tokens stop working at once.

The delete is refused while the identity still owns agents, or is the
last owner of a project: delete those first. Deleting an identity that no
longer exists succeeds with a notice.

Examples:
  scion hub test-identity delete 6f1c...`,
	Args: cobra.ExactArgs(1),
	RunE: runHubTestIdentityDelete,
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
)

func init() {
	hubCmd.AddCommand(hubTestIdentityCmd)
	hubTestIdentityCmd.AddCommand(hubTestIdentityIssueCmd)
	hubTestIdentityCmd.AddCommand(hubTestIdentityTokenCmd)
	hubTestIdentityCmd.AddCommand(hubTestIdentityListCmd)
	hubTestIdentityCmd.AddCommand(hubTestIdentityDeleteCmd)

	hubTestIdentityIssueCmd.Flags().StringVar(&testIdentityRole, "role", "member", `Role of the identity: "member" or "viewer"`)
	hubTestIdentityIssueCmd.Flags().DurationVar(&testIdentityTTL, "ttl", 0, "Token lifetime, e.g. 30m (default: the hub's, 30m)")
	hubTestIdentityIssueCmd.Flags().DurationVar(&testIdentityLifetime, "lifetime", 0, "Identity lifetime, e.g. 2h (default: the hub's, 1h; at most 8h)")
	hubTestIdentityIssueCmd.Flags().StringVar(&testIdentityPurpose, "purpose", "", "Short label recorded on the identity and in the audit log")
	hubTestIdentityIssueCmd.Flags().StringVar(&testIdentityOut, "out", "", "File to write the token to (required; must not exist; created 0600)")
	hubTestIdentityIssueCmd.Flags().BoolVar(&testIdentityJSON, "json", false, "Print the identity as JSON (never the token)")
	_ = hubTestIdentityIssueCmd.MarkFlagRequired("out")

	hubTestIdentityTokenCmd.Flags().DurationVar(&testIdentityTTL, "ttl", 0, "Token lifetime, e.g. 30m (default: the hub's, 30m)")
	hubTestIdentityTokenCmd.Flags().StringVar(&testIdentityOut, "out", "", "File to write the token to (required; must not exist; created 0600)")
	hubTestIdentityTokenCmd.Flags().BoolVar(&testIdentityJSON, "json", false, "Print the identity as JSON (never the token)")
	_ = hubTestIdentityTokenCmd.MarkFlagRequired("out")

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

// testIdentityTokenOptions are the inputs of reissueTestIdentityToken.
type testIdentityTokenOptions struct {
	ID   string
	TTL  time.Duration
	Out  string
	JSON bool
}

func runHubTestIdentityIssue(cmd *cobra.Command, _ []string) error {
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
	_, client, err := loadHubClient()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()
	return reissueTestIdentityToken(ctx, cmd.OutOrStdout(), cmd.ErrOrStderr(), client.TestIdentities(), testIdentityTokenOptions{
		ID: args[0], TTL: testIdentityTTL, Out: testIdentityOut, JSON: testIdentityJSON,
	})
}

func runHubTestIdentityList(cmd *cobra.Command, _ []string) error {
	_, client, err := loadHubClient()
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
	_, client, err := loadHubClient()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()
	return deleteTestIdentity(ctx, cmd.OutOrStdout(), cmd.ErrOrStderr(), client.TestIdentities(), args[0])
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

// write writes the token and closes the file.
func (t *tokenFile) write(token string) error {
	if _, err := io.WriteString(t.f, token+"\n"); err != nil {
		_ = t.f.Close()
		return err
	}
	if err := t.f.Sync(); err != nil {
		_ = t.f.Close()
		return err
	}
	return t.f.Close()
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
		_ = os.Remove(file.path)
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
		_ = os.Remove(file.path)
		_, _ = fmt.Fprintf(errOut, "A token for test identity %s was issued, but could not be written; run this command again with a new --out.\n", opts.ID)
		return fmt.Errorf("failed to write --out %s: %w", file.path, err)
	}
	return printTestIdentityToken(out, "New token for", resp, file.path, opts.JSON)
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
			fmt.Fprintf(&b, "test identity %s is the last owner of these projects; delete them first (a later release adds a purge that removes them):\n", id)
			for _, p := range projects {
				fmt.Fprintf(&b, "  project %s (%s)\n", p.Name, p.ID)
			}
		}
		return errors.New(strings.TrimRight(b.String(), "\n"))
	}
	return fmt.Errorf("failed to delete test identity %s: %w", id, err)
}
