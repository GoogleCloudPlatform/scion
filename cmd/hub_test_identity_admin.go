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
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

// Test identities with admin roles on test-designated hubs
// (ptone/scion#4240, admin tier phase A4):
//
//   - issue --admin-role hub-admin|super-admin --purpose ... --out FILE
//     issues one identity holding the real hub-admin or super-admin role;
//   - revoke-all --reason ... [--dry-run] is the emergency stop: it
//     revokes every such identity on the hub;
//   - residue ID prints what one such identity left on the hub.
//
// All three are human mode only and never run with a hub profile:
// revoke-all and residue are absent from agentAllowed and
// hubProfileAllowed, and issue refuses --admin-role in agent mode and with
// a profile. The hub enforces the rest: issuance needs a live human
// super-admin; revoke-all and residue need a human super-admin on an
// interactive session.
//
// The token is a secret. It is written only to --out (created with
// O_CREATE|O_EXCL, mode 0600, symlinks and symlinked directories refused)
// and never reaches standard output, standard error or an error message.

// Admin roles accepted by --admin-role.
const (
	testAdminRoleHubAdmin   = "hub-admin"
	testAdminRoleSuperAdmin = "super-admin"
)

// Purpose bounds, mirroring the hub (8 to 200 characters), checked before
// the token file is created so a refused purpose costs nothing.
const (
	testAdminPurposeMinRunes = 8
	testAdminPurposeMaxRunes = 200
)

// errTestAdminSuperAdminRequired is returned when the hub refuses
// revoke-all or residue because the caller is not a human super-admin on
// an interactive session (403).
var errTestAdminSuperAdminRequired = errors.New("this command requires a human super-admin signed in interactively ('scion hub auth login'); access tokens, test identities and other roles are refused")

// errTestAdminTierOff is returned when the hub answers 404 to an admin
// route: the hub did not start with --test-mode=admin or above.
var errTestAdminTierOff = errors.New("test identities with admin roles are not enabled on this hub (it must start with --test-mode=admin:<hub id> or super-admin:<hub id>)")

var hubTestIdentityRevokeAllCmd = &cobra.Command{
	Use:   "revoke-all --reason TEXT [--dry-run]",
	Short: "Revoke every test identity with an admin role on this hub (emergency stop)",
	Long: `Revoke every test identity with an admin role (test hub-admin and test
super-admin) on the hub. Each one is deleted, or, when it owns agents or is
a project's last owner, suspended with its role bindings removed. Its
tokens stop working at once. Afterwards, restart the hub without
--test-mode=admin or super-admin.

This is the emergency stop for test-designated hubs. Only a human
super-admin signed in interactively may run it: the hub refuses access
tokens, test identities and every other role.

--reason is required. It is printed with the result as the operator's
record only: the hub does NOT receive or store the reason. Its audit
records who revoked each identity, and when; keep the reason in your own
runbook log.

--dry-run changes nothing: it reads the hub's health summary and prints
whether the admin tier is on and how many identities with admin roles are
live, then stops without calling revoke-all. Run it first. It does not
check that you are a super-admin (reading the health summary needs only
hub.health.read); the real call does.

Not available in agent mode or with a hub profile.

Examples:
  scion hub test-identity revoke-all --reason "INC-123 stop test admins" --dry-run
  scion hub test-identity revoke-all --reason "INC-123 stop test admins"`,
	Args: cobra.NoArgs,
	RunE: runHubTestIdentityRevokeAll,
}

var hubTestIdentityResidueCmd = &cobra.Command{
	Use:   "residue ID",
	Short: "Print what a test identity with an admin role left on the hub",
	Long: `Print the residue report of one test identity with an admin role, live
or already purged: the grants and resources stamped with its issuance
(which a purge removes) and the changes it made to rows without its stamp,
for example a user it suspended (reported, never reverted). The report
holds IDs and audit facts only, never a token.

One identity per call. Only a human super-admin signed in interactively may
read it. Not available in agent mode or with a hub profile.

Examples:
  scion hub test-identity residue 6f1c...
  scion hub test-identity residue 6f1c... --json`,
	Args: cobra.ExactArgs(1),
	RunE: runHubTestIdentityResidue,
}

// Flag values of the admin commands.
var (
	testIdentityAdminRole   string
	testIdentityReason      string
	testIdentityDryRun      bool
	testIdentityResidueJSON bool
	testIdentityRevokeJSON  bool
)

func init() {
	hubTestIdentityCmd.AddCommand(hubTestIdentityRevokeAllCmd)
	hubTestIdentityCmd.AddCommand(hubTestIdentityResidueCmd)

	hubTestIdentityIssueCmd.Flags().StringVar(&testIdentityAdminRole, "admin-role", "", `Issue a test identity with an admin role: "hub-admin" or "super-admin" (needs --purpose and --out; human mode only, no hub profile)`)

	hubTestIdentityRevokeAllCmd.Flags().StringVar(&testIdentityReason, "reason", "", "Why every test identity with an admin role is revoked (required; printed with the result only, not sent to or stored by the hub)")
	hubTestIdentityRevokeAllCmd.Flags().BoolVar(&testIdentityDryRun, "dry-run", false, "Show what would be revoked and change nothing")
	hubTestIdentityRevokeAllCmd.Flags().BoolVar(&testIdentityRevokeJSON, "json", false, "Output in JSON format")

	hubTestIdentityResidueCmd.Flags().BoolVar(&testIdentityResidueJSON, "json", false, "Output in JSON format")
}

// testAdminIssueOptions are the inputs of issueTestAdminIdentity.
type testAdminIssueOptions struct {
	AdminRole string
	TTL       time.Duration
	Lifetime  time.Duration
	Purpose   string
	Out       string
	JSON      bool
}

// refuseTestAdminContext refuses a command for test identities with admin
// roles in agent mode or with a hub profile.
func refuseTestAdminContext(what string) error {
	if resolveMode() == ModeAgent {
		return fmt.Errorf("%s is not available in agent mode: test identities with admin roles are managed by a human operator", what)
	}
	if hubProfileSelected() {
		hint := ""
		if what == "issue --admin-role" {
			hint = " (a runner acting through an admin issuer delegation uses 'issue-admin')"
		}
		return fmt.Errorf("%s is not available with a hub profile: it uses your own hub sign-in on the hub itself%s", what, hint)
	}
	return nil
}

// runHubTestIdentityAdminIssue is issue with --admin-role.
func runHubTestIdentityAdminIssue(cmd *cobra.Command) error {
	if err := refuseTestAdminContext("issue --admin-role"); err != nil {
		return err
	}
	if cmd.Flags().Changed("role") {
		return newUsageError("--role and --admin-role cannot be combined: --role is for member and viewer identities")
	}
	_, client, err := loadHubClient()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()
	return issueTestAdminIdentity(ctx, cmd.OutOrStdout(), cmd.ErrOrStderr(), client.TestIdentities(), testAdminIssueOptions{
		AdminRole: testIdentityAdminRole, TTL: testIdentityTTL, Lifetime: testIdentityLifetime,
		Purpose: testIdentityPurpose, Out: testIdentityOut, JSON: testIdentityJSON,
	})
}

// issueTestAdminIdentity validates every input, creates the token file
// exclusively, issues the identity and writes only its token to the file.
// Nothing is sent when an input or the file is refused.
func issueTestAdminIdentity(ctx context.Context, out, errOut io.Writer, svc hubclient.TestIdentityService, opts testAdminIssueOptions) error {
	role := opts.AdminRole
	if role != testAdminRoleHubAdmin && role != testAdminRoleSuperAdmin {
		return newUsageError(`invalid --admin-role %q: must be "hub-admin" or "super-admin"`, opts.AdminRole)
	}
	purpose := strings.TrimSpace(opts.Purpose)
	if n := utf8.RuneCountInString(purpose); n < testAdminPurposeMinRunes || n > testAdminPurposeMaxRunes {
		return newUsageError("--purpose is required with --admin-role: %d to %d characters", testAdminPurposeMinRunes, testAdminPurposeMaxRunes)
	}
	ttl, err := wholeSeconds("ttl", opts.TTL)
	if err != nil {
		return err
	}
	lifetime, err := wholeSeconds("lifetime", opts.Lifetime)
	if err != nil {
		return err
	}
	if opts.Out == "" {
		return newUsageError("--out is required: the token is written only to a file")
	}
	if err := refuseSymlinkedParents(opts.Out); err != nil {
		return err
	}
	file, err := createTokenFile(opts.Out)
	if err != nil {
		return err
	}
	resp, err := svc.IssueAdmin(ctx, &hubclient.IssueTestAdminIdentityRequest{
		AdminRole: role, Purpose: purpose, LifetimeSeconds: lifetime, TokenTTLSeconds: ttl,
	})
	if err != nil {
		file.abandon()
		if apiclient.IsNotFoundError(err) {
			return fmt.Errorf("failed to issue a test identity with the %s role: %w (for super-admin the hub needs --test-mode=super-admin)", role, errTestAdminTierOff)
		}
		return fmt.Errorf("failed to issue a test identity with the %s role: %w", role, err)
	}
	if err := testAdminTokenWrite(file, resp.AccessToken); err != nil {
		// Admin identities have no re-issue: the identity must be revoked.
		_, _ = fmt.Fprintf(errOut, "Test identity %s was issued, but its token could not be written. Its token cannot be re-issued; remove it with \"scion hub test-identity delete %s --purge\".\n", resp.Identity.ID, resp.Identity.ID)
		return fmt.Errorf("failed to write --out %s: %w", file.path, err)
	}
	return printTestAdminIdentityToken(out, resp, file.path, opts.JSON)
}

// testAdminTokenWrite writes the issued token to its file. It is a
// variable only so a test can make the write fail after issuance.
var testAdminTokenWrite = func(f *tokenFile, token string) error { return f.write(token) }

// testAdminIdentityOutput is the --json output of issue --admin-role. It
// has no token field.
type testAdminIdentityOutput struct {
	Identity       hubclient.TestAdminIdentity `json:"identity"`
	TokenFile      string                      `json:"tokenFile"`
	TokenExpiresAt time.Time                   `json:"tokenExpiresAt"`
}

// printTestAdminIdentityToken prints the non-secret facts of an admin
// issuance. It never prints resp.AccessToken.
func printTestAdminIdentityToken(out io.Writer, resp *hubclient.TestAdminIdentityTokenResponse, path string, asJSON bool) error {
	absPath := path
	if abs, err := filepath.Abs(path); err == nil {
		absPath = abs
	}
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(testAdminIdentityOutput{Identity: resp.Identity, TokenFile: absPath, TokenExpiresAt: resp.TokenExpiresAt})
	}
	id := resp.Identity
	_, _ = fmt.Fprintf(out, "Issued test identity %s\n", id.ID)
	_, _ = fmt.Fprintf(out, "  Admin role:    %s\n", id.AdminRole)
	_, _ = fmt.Fprintf(out, "  Kind:          %s\n", id.Kind)
	_, _ = fmt.Fprintf(out, "  Email:         %s\n", id.Email)
	_, _ = fmt.Fprintf(out, "  Issuance ID:   %s\n", id.IssuanceID)
	_, _ = fmt.Fprintf(out, "  Expires:       %s\n", formatTestIdentityTime(id.ExpiresAt))
	_, _ = fmt.Fprintf(out, "  Token expires: %s (no re-issue)\n", formatTestIdentityTime(resp.TokenExpiresAt))
	if id.Purpose != "" {
		_, _ = fmt.Fprintf(out, "  Purpose:       %s\n", id.Purpose)
	}
	_, _ = fmt.Fprintf(out, "  Token file:    %s %s\n", absPath, tokenFileProtection())
	return nil
}

// testAdminRefusal maps the hub's answer to revoke-all or residue onto a
// named error: 403 is errTestAdminSuperAdminRequired, 404 on revoke-all is
// errTestAdminTierOff.
func testAdminRefusal(what string, err error, notFoundIsTierOff bool) error {
	switch {
	case apiclient.IsForbiddenError(err) || apiclient.IsUnauthorizedError(err):
		return fmt.Errorf("%s refused: %w (hub: %v)", what, errTestAdminSuperAdminRequired, err)
	case notFoundIsTierOff && apiclient.IsNotFoundError(err):
		return fmt.Errorf("%s: %w", what, errTestAdminTierOff)
	}
	return fmt.Errorf("%s failed: %w", what, err)
}

func runHubTestIdentityRevokeAll(cmd *cobra.Command, _ []string) error {
	if err := refuseTestAdminContext("revoke-all"); err != nil {
		return err
	}
	if err := checkTestAdminReason(testIdentityReason); err != nil {
		return err
	}
	_, client, err := loadHubClient()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
	defer cancel()
	return revokeAllTestAdminIdentities(ctx, cmd.OutOrStdout(), client, testAdminRevokeAllOptions{
		Reason: testIdentityReason, DryRun: testIdentityDryRun, JSON: testIdentityRevokeJSON,
	})
}

// checkTestAdminReason refuses an empty --reason.
func checkTestAdminReason(reason string) error {
	if strings.TrimSpace(reason) == "" {
		return newUsageError("--reason is required: say why every test identity with an admin role is revoked")
	}
	return nil
}

// testAdminRevokeAllOptions are the inputs of revokeAllTestAdminIdentities.
type testAdminRevokeAllOptions struct {
	Reason string
	DryRun bool
	JSON   bool
}

// testAdminRevokeAllOutput is the --json output of revoke-all.
type testAdminRevokeAllOutput struct {
	DryRun bool   `json:"dryRun"`
	Reason string `json:"reason"`
	// AdminTierEnabled, SuperAdminEnabled and LiveIdentities are the
	// dry run's reading of the health summary; LiveIdentities is nil when
	// the hub could not count them.
	AdminTierEnabled  *bool                                `json:"adminTierEnabled,omitempty"`
	SuperAdminEnabled *bool                                `json:"superAdminEnabled,omitempty"`
	LiveIdentities    *int                                 `json:"liveIdentities,omitempty"`
	Revoked           []hubclient.RevokedTestAdminIdentity `json:"revoked,omitempty"`
	Count             int                                  `json:"count"`
}

// revokeAllTestAdminIdentities runs revoke-all, or with DryRun only reads
// the health summary and never calls revoke-all.
func revokeAllTestAdminIdentities(ctx context.Context, out io.Writer, client hubclient.Client, opts testAdminRevokeAllOptions) error {
	if err := checkTestAdminReason(opts.Reason); err != nil {
		return err
	}
	reason := strings.TrimSpace(opts.Reason)
	if opts.DryRun {
		return dryRunRevokeAllTestAdminIdentities(ctx, out, client, reason, opts.JSON)
	}
	resp, err := client.TestIdentities().RevokeAllAdmin(ctx)
	if err != nil {
		if apiclient.IsForbiddenError(err) || apiclient.IsUnauthorizedError(err) || apiclient.IsNotFoundError(err) {
			return testAdminRefusal("revoke-all", err, true)
		}
		// The hub revokes one identity at a time, so a failure (for example
		// 503 when its audit sink cannot be written) can come after some
		// were already revoked. Running it again is safe.
		return fmt.Errorf("revoke-all failed: %w; some identities may already be revoked: check with \"scion hub test-identity revoke-all --reason ... --dry-run\" and run revoke-all again (it is safe to repeat)", err)
	}
	if opts.JSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		revoked := resp.Revoked
		if revoked == nil {
			revoked = []hubclient.RevokedTestAdminIdentity{}
		}
		return enc.Encode(testAdminRevokeAllOutput{Reason: reason, Revoked: revoked, Count: resp.Count})
	}
	_, _ = fmt.Fprintf(out, "Revoked %d test identities with admin roles.\n", resp.Count)
	_, _ = fmt.Fprintf(out, "  Reason: %s (not stored by the hub)\n", reason)
	for _, r := range resp.Revoked {
		_, _ = fmt.Fprintf(out, "  %s %s: %s\n", r.Kind, r.ID, r.Outcome)
	}
	_, _ = fmt.Fprintln(out, "Next: restart the hub without --test-mode=admin or super-admin.")
	return nil
}

// dryRunRevokeAllTestAdminIdentities reports what revoke-all would act on,
// from the read-only health summary. It sends no write.
func dryRunRevokeAllTestAdminIdentities(ctx context.Context, out io.Writer, client hubclient.Client, reason string, asJSON bool) error {
	sum, err := client.HealthSummary(ctx)
	if err != nil {
		return fmt.Errorf("revoke-all --dry-run could not read the hub health summary (needs hub.health.read): %w", err)
	}
	h := sum.Hub
	if asJSON {
		admin, super := h.TestAdminIdentitiesEnabled, h.TestSuperAdminIdentitiesEnabled
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(testAdminRevokeAllOutput{
			DryRun: true, Reason: reason, AdminTierEnabled: &admin, SuperAdminEnabled: &super, LiveIdentities: h.LiveTestAdminIdentities,
		})
	}
	live := "unknown (the hub could not count them)"
	if h.LiveTestAdminIdentities != nil {
		live = fmt.Sprintf("%d", *h.LiveTestAdminIdentities)
	}
	_, _ = fmt.Fprintln(out, "Dry run: nothing was changed. Your super-admin authority was not checked.")
	_, _ = fmt.Fprintf(out, "  Admin tier enabled:       %s\n", yesNo(h.TestAdminIdentitiesEnabled))
	_, _ = fmt.Fprintf(out, "  Super-admin kind enabled: %s\n", yesNo(h.TestSuperAdminIdentitiesEnabled))
	_, _ = fmt.Fprintf(out, "  Live identities:          %s\n", live)
	_, _ = fmt.Fprintf(out, "  Reason:                   %s (not stored by the hub)\n", reason)
	if !h.TestAdminIdentitiesEnabled {
		_, _ = fmt.Fprintln(out, "revoke-all would answer that the admin tier is not enabled on this hub.")
		return nil
	}
	_, _ = fmt.Fprintln(out, "Without --dry-run, revoke-all deletes (or suspends) every live test identity with an admin role.")
	return nil
}

func runHubTestIdentityResidue(cmd *cobra.Command, args []string) error {
	if err := refuseTestAdminContext("residue"); err != nil {
		return err
	}
	_, client, err := loadHubClient()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
	defer cancel()
	return printTestAdminResidue(ctx, cmd.OutOrStdout(), client.TestIdentities(), args[0], testIdentityResidueJSON)
}

// printTestAdminResidue reads and prints the residue report of one
// identity.
func printTestAdminResidue(ctx context.Context, out io.Writer, svc hubclient.TestIdentityService, id string, asJSON bool) error {
	if strings.TrimSpace(id) == "" {
		return newUsageError("name one test identity: residue ID")
	}
	rep, err := svc.AdminResidue(ctx, id)
	if err != nil {
		if apiclient.IsNotFoundError(err) {
			return fmt.Errorf("residue of %s: not a test identity with an admin role on this hub, now or in the past, or the admin tier is not enabled: %w", id, err)
		}
		return testAdminRefusal("residue of "+id, err, false)
	}
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	_, _ = fmt.Fprintf(out, "Residue of test identity %s (issuance %s)\n", rep.IdentityID, rep.IssuanceID)
	_, _ = fmt.Fprintf(out, "Stamped grants (removed by a purge): %d\n", len(rep.StampedGrants))
	for _, g := range rep.StampedGrants {
		_, _ = fmt.Fprintf(out, "  %s\n", describeResidueGrant(g))
	}
	r := rep.StampedResources
	_, _ = fmt.Fprintf(out, "Stamped resources (removed by a purge): %d agents, %d schedules, %d groups, %d projects\n",
		len(r.Agents), len(r.Schedules), len(r.Groups), len(r.Projects))
	for _, list := range []struct {
		name string
		ids  []string
	}{{"agent", r.Agents}, {"schedule", r.Schedules}, {"group", r.Groups}, {"project", r.Projects}} {
		for _, rid := range list.ids {
			_, _ = fmt.Fprintf(out, "  %s %s\n", list.name, rid)
		}
	}
	_, _ = fmt.Fprintf(out, "Changes to rows without its stamp (reported, never reverted): %d\n", len(rep.UnstampedChanges))
	for _, c := range rep.UnstampedChanges {
		_, _ = fmt.Fprintf(out, "  %s %s %s by %s %s at %s\n", c.MutationType, c.TargetType, c.TargetID, c.ActorKind, c.ActorID, formatTestIdentityTime(c.Timestamp))
	}
	if len(rep.KeptRoles) > 0 {
		_, _ = fmt.Fprintf(out, "Custom roles kept by a purge (bound outside the issuance): %d\n", len(rep.KeptRoles))
		for _, g := range rep.KeptRoles {
			_, _ = fmt.Fprintf(out, "  %s\n", describeResidueGrant(g))
		}
	}
	if rep.Truncated {
		_, _ = fmt.Fprintln(out, "(a list hit its bound; more residue exists than is shown)")
	}
	if rep.Limit != "" {
		_, _ = fmt.Fprintf(out, "Limit: %s\n", rep.Limit)
	}
	return nil
}

// describeResidueGrant renders one stamped grant on one line.
func describeResidueGrant(g hubclient.TestAdminResidueGrant) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s", g.Kind, g.ID)
	if g.Name != "" {
		fmt.Fprintf(&b, " name=%s", g.Name)
	}
	if g.RoleDefinitionID != "" {
		fmt.Fprintf(&b, " role=%s", g.RoleDefinitionID)
	}
	if g.PrincipalType != "" || g.PrincipalID != "" {
		fmt.Fprintf(&b, " principal=%s:%s", g.PrincipalType, g.PrincipalID)
	}
	if g.ScopeType != "" || g.ScopeID != "" {
		fmt.Fprintf(&b, " scope=%s:%s", g.ScopeType, g.ScopeID)
	}
	if g.GroupID != "" {
		fmt.Fprintf(&b, " group=%s", g.GroupID)
	}
	if g.ExpiresAt != nil {
		fmt.Fprintf(&b, " expires=%s", formatTestIdentityTime(*g.ExpiresAt))
	}
	return b.String()
}
