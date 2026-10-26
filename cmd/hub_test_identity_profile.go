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
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubprofile"
)

// Test identity commands with a target-hub profile, teardown, and the
// human-only delegate commands (ptone/scion#4240).

var hubTestIdentityTeardownCmd = &cobra.Command{
	Use:   "teardown --run LABEL",
	Short: "Delete every test identity of a run and remove its cache (hub profile)",
	Long: `Delete every test identity of a run on the profile's target hub, and
remove the run's identity cache.

It deletes each cached identity of the run, then any identity the profile's
issuer delegation still lists whose purpose names the run (an identity
whose cache file was never written). An identity that is already gone
counts as done. When everything is gone the run's cache directory is
removed; otherwise the command lists what is left and exits non-zero (run
it again).

With --purge each identity is purged (its agents, schedules, groups and
projects go with it) instead of deleted.

Requires a hub profile (--hub-profile or SCION_HUB_PROFILE).

Examples:
  scion --hub-profile t1 hub test-identity teardown --run r1
  scion --hub-profile t1 hub test-identity teardown --run r1 --purge`,
	Args: cobra.NoArgs,
	RunE: runHubTestIdentityTeardown,
}

var hubTestIdentityIssueAdminCmd = &cobra.Command{
	Use:   "issue-admin --admin-role ROLE --purpose TEXT --as ALIAS --run LABEL",
	Short: "Issue a test hub-admin or test super-admin through a hub profile's admin delegation",
	Long: `Issue an admin-kind test identity (a test hub-admin or a test super-admin)
on the profile's target hub, through the profile's admin issuer delegation
of --admin-role, and store it in the run cache as --as.

The target must run the test admin tier (--test-mode=admin, or super-admin
for a test super-admin; systemd test hubs only), and the profile must hold
the admin delegation of that role (--issuer-delegation hub-admin=<id> or
super-admin=<id>) and the target's admin issuer service account
(--admin-issuer-sa). This command needs a hub profile; it never uses your
own login. In agent mode it runs only with a profile for another hub. It is
the runner's command, acting through a delegation; a human operator issuing
on their own session uses 'issue --admin-role' without a profile.

The token goes only to the run cache (mode 0600); only the identity's ID,
email, kind, role, expiry and the cache file are printed. --purpose is required
by the hub (8 to 200 characters with the run tag the CLI adds).

Lifetimes are the kind's: a test hub-admin lives at most 60 minutes (token
at most 30), a test super-admin at most 30 minutes (token at most 15). The
token of a cached admin identity is re-issued through the same delegation
('token --as <alias>'), never past the identity's lifetime. A web-login code
('web-code <id> --admin-role <role>') needs the target's admin web session
setting. The delegation cannot list or delete admin identities: they end at
their expiry (teardown drops them from the cache), or a human super-admin
revokes them.

Examples:
  scion --hub-profile t9 hub test-identity issue-admin --admin-role hub-admin --purpose "admin journey" --as admin1 --run r1
  scion --hub-profile t9 hub test-identity issue-admin --admin-role super-admin --purpose "super journey" --as super1 --run r1 --lifetime 15m`,
	Args: cobra.NoArgs,
	RunE: runHubTestIdentityIssueAdmin,
}

var hubTestIdentityDelegateCmd = &cobra.Command{
	Use:   "delegate",
	Short: "Manage workload issuer delegations (human only)",
	Long: `Manage workload issuer delegations on a test hub.

A delegation lets one pinned workload identity (the target hub's issuer
service account, by its numeric unique id, which must be in the hub's
startup allowlist server.hub.test_identities.workload_issuers) issue test
identities on your behalf, within your own live authority: member and
viewer identities (--scope member, the default), or on a hub running the
admin tiers test hub-admins (--scope hub-admin) or one test super-admin
(--scope super-admin). An admin scope needs a live super-admin session and
an allowlist entry with max_tier admin. It is the human consent step: these commands
take your interactive login on the hub ('scion hub auth login'), never an
access token, an agent token or a hub profile, and are not available in
agent mode.

Examples:
  scion --hub https://t1.example.com hub test-identity delegate create --workload-sub 1234567890 --ttl 8h --label "uat window"
  scion --hub https://t1.example.com hub test-identity delegate create --workload-sub 1234567891 --scope hub-admin --ttl 2h --admin-cap 1
  scion --hub https://t1.example.com hub test-identity delegate list
  scion --hub https://t1.example.com hub test-identity delegate revoke <id>`,
}

var hubTestIdentityDelegateCreateCmd = &cobra.Command{
	Use:   "create --workload-sub SUB",
	Short: "Create a workload issuer delegation",
	Long: `Create a workload issuer delegation with one scope: member (default),
hub-admin or super-admin. The delegation id it prints is not a secret: it
is useless without a token for the pinned workload identity. Give a member
delegation to the runner's hub profile with '--issuer-delegation
member=<id>', and an admin delegation with '--issuer-delegation
hub-admin=<id>' or 'super-admin=<id>' (plus --admin-issuer-sa); the runner
then issues with 'scion hub test-identity issue-admin'.

Lifetimes (--ttl): member default 8h, at most 7 days; hub-admin default 4h,
at most 8h; super-admin default 1h, at most 2h.`,
	Args: cobra.NoArgs,
	RunE: runHubTestIdentityDelegateCreate,
}

var hubTestIdentityDelegateListCmd = &cobra.Command{
	Use:   "list",
	Short: "List your workload issuer delegations",
	Args:  cobra.NoArgs,
	RunE:  runHubTestIdentityDelegateList,
}

var hubTestIdentityDelegateRevokeCmd = &cobra.Command{
	Use:   "revoke ID",
	Short: "Revoke a workload issuer delegation",
	Args:  cobra.ExactArgs(1),
	RunE:  runHubTestIdentityDelegateRevoke,
}

var (
	// testIdentityIssueAdminRole is issue-admin's --admin-role. It is not
	// shared with A4's 'issue --admin-role' (testIdentityAdminRole) or
	// web-code's (testIdentityWebCodeAdminRole), so no command reads another
	// command's flag value.
	testIdentityIssueAdminRole string
	// testIdentityWebCodeAdminRole is web-code's --admin-role.
	testIdentityWebCodeAdminRole string
	testIdentityTeardownPurge    bool
	delegateWorkloadSub          string
	delegateTTL                  time.Duration
	delegateMemberCap            int
	delegateAdminCap             int
	delegateScope                string
	delegateLabel                string
	delegateJSON                 bool
)

func init() {
	hubTestIdentityCmd.AddCommand(hubTestIdentityTeardownCmd, hubTestIdentityDelegateCmd, hubTestIdentityIssueAdminCmd)
	af := hubTestIdentityIssueAdminCmd.Flags()
	af.StringVar(&testIdentityIssueAdminRole, "admin-role", "", `Admin kind to issue: "hub-admin" or "super-admin" (selects the profile's delegation of that tier)`)
	af.StringVar(&testIdentityPurpose, "purpose", "", "Purpose recorded on the identity and in the audit log (required by the hub)")
	af.DurationVar(&testIdentityTTL, "ttl", 0, "Token lifetime, e.g. 10m (default: the kind's; at most 30m hub-admin, 15m super-admin)")
	af.DurationVar(&testIdentityLifetime, "lifetime", 0, "Identity lifetime, e.g. 30m (default: the kind's; at most 60m hub-admin, 30m super-admin)")
	af.BoolVar(&testIdentityJSON, "json", false, "Print the identity as JSON (never the token)")
	_ = hubTestIdentityIssueAdminCmd.MarkFlagRequired("admin-role")
	hubTestIdentityTeardownCmd.Flags().BoolVar(&testIdentityTeardownPurge, "purge", false, "Purge each identity (with what it created) instead of deleting it")
	hubTestIdentityTeardownCmd.Flags().BoolVar(&testIdentityJSON, "json", false, "Output in JSON format")

	hubTestIdentityDelegateCmd.AddCommand(hubTestIdentityDelegateCreateCmd, hubTestIdentityDelegateListCmd, hubTestIdentityDelegateRevokeCmd)
	f := hubTestIdentityDelegateCreateCmd.Flags()
	f.StringVar(&delegateWorkloadSub, "workload-sub", "", "Numeric unique id of the workload identity (the target's issuer service account)")
	f.DurationVar(&delegateTTL, "ttl", 0, "Delegation lifetime, e.g. 2h (default: the scope's: member 8h, hub-admin 4h, super-admin 1h; at most 7 days, 8h and 2h)")
	f.StringVar(&delegateScope, "scope", delegateScopeMember, "Delegated scope: member, hub-admin or super-admin")
	f.IntVar(&delegateMemberCap, "member-cap", 0, "Live test identities a member delegation may hold at once, 1-5 (default: 5)")
	f.IntVar(&delegateAdminCap, "admin-cap", 0, "Live admin identities an admin delegation may hold at once: hub-admin 1-2, super-admin 1 (default: the maximum)")
	f.StringVar(&delegateLabel, "label", "", "Short label for the test window")
	f.BoolVar(&delegateJSON, "json", false, "Output in JSON format")
	_ = hubTestIdentityDelegateCreateCmd.MarkFlagRequired("workload-sub")
	hubTestIdentityDelegateListCmd.Flags().BoolVar(&delegateJSON, "json", false, "Output in JSON format")
}

// profileIssueOutput is the --json output of an issue with a profile. It
// has no token field.
type profileIssueOutput struct {
	Alias          string              `json:"alias"`
	Run            string              `json:"run"`
	Profile        string              `json:"profile"`
	IdentityView   profileIdentityView `json:"identity"`
	CacheFile      string              `json:"cacheFile"`
	TokenExpiresAt time.Time           `json:"tokenExpiresAt"`
}

// profileIdentityView is the non-secret view of a cache entry.
type profileIdentityView struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Kind      string    `json:"kind"`
	Role      string    `json:"role"`
	ExpiresAt time.Time `json:"expiresAt"`
}

func viewOf(e *hubprofile.CacheEntry) profileIdentityView {
	return profileIdentityView{ID: e.IdentityID, Email: e.Email, Kind: e.Kind, Role: e.Role, ExpiresAt: e.ExpiresAt}
}

// printProfileEntry prints the non-secret facts of a cache entry. It never
// prints e.Token.
func printProfileEntry(out io.Writer, verb string, e *hubprofile.CacheEntry, cacheFile string, asJSON bool) error {
	if asJSON {
		return writeJSONTo(out, profileIssueOutput{
			Alias: e.Alias, Run: e.Run, Profile: e.Profile, IdentityView: viewOf(e),
			CacheFile: cacheFile, TokenExpiresAt: e.TokenExpiresAt,
		})
	}
	_, _ = fmt.Fprintf(out, "%s test identity %s as %q (run %s, profile %s)\n", verb, e.IdentityID, e.Alias, e.Run, e.Profile)
	_, _ = fmt.Fprintf(out, "  Email:         %s\n", e.Email)
	_, _ = fmt.Fprintf(out, "  Kind:          %s\n", e.Kind)
	_, _ = fmt.Fprintf(out, "  Role:          %s\n", e.Role)
	_, _ = fmt.Fprintf(out, "  Expires:       %s\n", formatTestIdentityTime(e.ExpiresAt))
	_, _ = fmt.Fprintf(out, "  Token expires: %s\n", formatTestIdentityTime(e.TokenExpiresAt))
	_, _ = fmt.Fprintf(out, "  Cache file:    %s %s\n", cacheFile, tokenFileProtection())
	return nil
}

func cacheFileOf(home string, e *hubprofile.CacheEntry) string {
	dir, err := hubprofile.RunDir(home, e.Profile, e.Run)
	if err != nil {
		return ""
	}
	return filepath.Join(dir, e.Alias+".json")
}

// runProfileTestIdentityIssue issues through the profile's issuer
// delegation and caches the identity as --as in --run.
func runProfileTestIdentityIssue(cmd *cobra.Command) error {
	if hubProfileAs == "" {
		return newUsageError("with a hub profile, name the new identity with --as <alias> (and --run <label>); its token goes to the run cache")
	}
	if testIdentityOut != "" {
		return newUsageError("--out does not apply with a hub profile: the token goes only to the run cache")
	}
	ttl, err := wholeSeconds("ttl", testIdentityTTL)
	if err != nil {
		return err
	}
	lifetime, err := wholeSeconds("lifetime", testIdentityLifetime)
	if err != nil {
		return err
	}
	s, err := openHubProfileIssuerSession()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()
	e, err := hubprofile.IssueToCache(ctx, s, hubprofile.IssueRequest{
		Alias: hubProfileAs, Role: testIdentityRole, Purpose: testIdentityPurpose, Lifetime: lifetime, TokenTTL: ttl,
	})
	if err != nil {
		return err
	}
	home, _ := hubprofile.HomeDir()
	return printProfileEntry(cmd.OutOrStdout(), "Issued", e, cacheFileOf(home, e), testIdentityJSON)
}

// runHubTestIdentityIssueAdmin issues an admin-kind identity through the
// profile's admin delegation of --admin-role and caches it as --as in
// --run. It needs a hub profile: there is no path through the CLI's own
// hub credential.
func runHubTestIdentityIssueAdmin(cmd *cobra.Command, _ []string) error {
	if !hubProfileSelected() {
		return fmt.Errorf("issue-admin needs a hub profile (--hub-profile or %s) holding an admin issuer delegation", hubprofile.EnvHubProfile)
	}
	role := strings.TrimSpace(testIdentityIssueAdminRole)
	if !hubprofile.IsAdminTier(role) {
		return newUsageError("invalid --admin-role %q: must be %q or %q", testIdentityIssueAdminRole, hubprofile.TierHubAdmin, hubprofile.TierSuperAdmin)
	}
	if hubProfileAs == "" {
		return newUsageError("name the new identity with --as <alias> (and --run <label>); its token goes to the run cache")
	}
	if strings.TrimSpace(testIdentityPurpose) == "" {
		return newUsageError("--purpose is required: the hub records it on every admin-kind identity")
	}
	ttl, err := wholeSeconds("ttl", testIdentityTTL)
	if err != nil {
		return err
	}
	lifetime, err := wholeSeconds("lifetime", testIdentityLifetime)
	if err != nil {
		return err
	}
	s, err := openHubProfileIssuerSession()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()
	e, err := hubprofile.IssueAdminToCache(ctx, s, hubprofile.AdminIssueRequest{
		Alias: hubProfileAs, AdminRole: role, Purpose: testIdentityPurpose, Lifetime: lifetime, TokenTTL: ttl,
	})
	if err != nil {
		return err
	}
	home, _ := hubprofile.HomeDir()
	return printProfileEntry(cmd.OutOrStdout(), "Issued", e, cacheFileOf(home, e), testIdentityJSON)
}

// runProfileTestIdentityReissue re-issues the token of the --as identity.
func runProfileTestIdentityReissue(cmd *cobra.Command, args []string) error {
	if testIdentityOut != "" {
		return newUsageError("--out does not apply with --as: the token goes only to the run cache")
	}
	ttl, err := wholeSeconds("ttl", testIdentityTTL)
	if err != nil {
		return err
	}
	s, err := openHubProfileSession(true)
	if err != nil {
		return err
	}
	if len(args) == 1 && args[0] != s.Entry().IdentityID {
		return newUsageError("ID %s is not the identity cached as %q (%s)", args[0], hubProfileAs, s.Entry().IdentityID)
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()
	e, err := s.Reissue(ctx, ttl)
	if err != nil {
		return err
	}
	home, _ := hubprofile.HomeDir()
	return printProfileEntry(cmd.OutOrStdout(), "New token for", e, cacheFileOf(home, e), testIdentityJSON)
}

// runProfileTestIdentityDeleteAlias deletes the --as identity through the
// issuer delegation and removes it from the run cache.
func runProfileTestIdentityDeleteAlias(cmd *cobra.Command, args []string) error {
	s, err := openHubProfileSession(true)
	if err != nil {
		return err
	}
	e := s.Entry()
	if len(args) == 1 && args[0] != e.IdentityID {
		return newUsageError("ID %s is not the identity cached as %q (%s)", args[0], hubProfileAs, e.IdentityID)
	}
	if e.AdminTier() != "" {
		return fmt.Errorf("identity %q (kind %s) is not deleted through a delegation: it expires at %s, a human super-admin can revoke it, and 'teardown --run %s' drops it from the cache", e.Alias, e.Kind, formatTestIdentityTime(e.ExpiresAt), e.Run)
	}
	client, err := s.IssuerClient()
	if err != nil {
		return err
	}
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
	if testIdentityPurge {
		ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Minute)
		defer cancel()
		err = purgeTestIdentity(ctx, out, errOut, client.TestIdentities(), e.IdentityID)
	} else {
		ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
		defer cancel()
		err = deleteTestIdentity(ctx, out, errOut, client.TestIdentities(), e.IdentityID)
	}
	if err != nil {
		return err
	}
	home, _ := hubprofile.HomeDir()
	if err := hubprofile.RemoveEntry(home, e.Profile, e.Run, e.Alias); err != nil {
		return fmt.Errorf("test identity %s is gone, but its cache file was not removed: %w", e.IdentityID, err)
	}
	_, _ = fmt.Fprintf(out, "Removed %q from run %s.\n", e.Alias, e.Run)
	return nil
}

// openHubProfileIssuerSession opens the selected profile as its issuer
// delegation. --as, if given, names an alias the caller handles itself.
func openHubProfileIssuerSession() (*hubprofile.Session, error) {
	home, err := hubprofile.HomeDir()
	if err != nil {
		return nil, err
	}
	p, err := hubprofile.Load(home, hubProfileName())
	if err != nil {
		return nil, err
	}
	return hubprofile.Open(hubprofile.Options{Home: home, Profile: p, Run: hubProfileRunLabel()}, hubProfileDeps())
}

func runHubTestIdentityTeardown(cmd *cobra.Command, _ []string) error {
	if !hubProfileSelected() {
		return fmt.Errorf("teardown needs a hub profile (--hub-profile or %s)", hubprofile.EnvHubProfile)
	}
	if hubProfileRunLabel() == "" {
		return newUsageError("--run <label> is required (or %s)", hubprofile.EnvHubRun)
	}
	if hubProfileAs != "" {
		return newUsageError("--as does not apply to teardown: it removes every identity of the run")
	}
	s, err := openHubProfileIssuerSession()
	if err != nil {
		return err
	}
	timeout := 2 * time.Minute
	if testIdentityTeardownPurge {
		timeout = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
	defer cancel()
	rep, err := hubprofile.Teardown(ctx, s, testIdentityTeardownPurge)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	failMsg := fmt.Sprintf("teardown of run %s did not complete; run it again", rep.Run)
	if testIdentityJSON {
		if err := writeJSONTo(out, rep); err != nil {
			return err
		}
		if !rep.Complete {
			return &jsonReportedError{msg: failMsg}
		}
		return nil
	}
	for _, it := range rep.Items {
		label := it.IdentityID
		if it.Alias != "" {
			label = fmt.Sprintf("%s (%s)", it.IdentityID, it.Alias)
		}
		if label == "" {
			label = "-"
		}
		line := fmt.Sprintf("  %s: %s", label, it.Outcome)
		if it.Detail != "" {
			line += " (" + it.Detail + ")"
		}
		_, _ = fmt.Fprintln(out, line)
	}
	if !rep.Complete {
		return errors.New(failMsg)
	}
	_, _ = fmt.Fprintf(out, "Run %s of profile %s torn down; its cache was removed.\n", rep.Run, rep.Profile)
	return nil
}

// --- delegate (human only) ---

// delegationClient returns the client for the delegate commands. They take
// a human session, which a hub profile never uses.
func delegationClient() (hubclient.Client, error) {
	if hubProfileSelected() {
		return nil, errors.New("delegate commands take your interactive login on the hub, never a hub profile")
	}
	_, client, err := loadHubClient()
	return client, err
}

// Delegate --scope values and the hub scope each maps to.
const (
	delegateScopeMember     = "member"
	delegateScopeHubAdmin   = "hub-admin"
	delegateScopeSuperAdmin = "super-admin"
)

var delegateScopes = map[string]string{
	delegateScopeMember:     "test_identity:issue",
	delegateScopeHubAdmin:   "test_identity:issue_admin",
	delegateScopeSuperAdmin: "test_identity:issue_super_admin",
}

// delegateAdminCapMax is the largest --admin-cap per admin scope (the hub
// enforces the same bounds).
var delegateAdminCapMax = map[string]int{delegateScopeHubAdmin: 2, delegateScopeSuperAdmin: 1}

func runHubTestIdentityDelegateCreate(cmd *cobra.Command, _ []string) error {
	ttl, err := wholeSeconds("ttl", delegateTTL)
	if err != nil {
		return err
	}
	scope, ok := delegateScopes[delegateScope]
	if !ok {
		return newUsageError("invalid --scope %q: must be member, hub-admin or super-admin", delegateScope)
	}
	if delegateScope == delegateScopeMember {
		if delegateAdminCap != 0 {
			return newUsageError("--admin-cap applies to the hub-admin and super-admin scopes only")
		}
		if delegateMemberCap < 0 || delegateMemberCap > 5 {
			return newUsageError("invalid --member-cap %d: must be 1-5", delegateMemberCap)
		}
	} else {
		if delegateMemberCap != 0 {
			return newUsageError("--member-cap applies to the member scope only; use --admin-cap")
		}
		if maxCap := delegateAdminCapMax[delegateScope]; delegateAdminCap < 0 || delegateAdminCap > maxCap {
			return newUsageError("invalid --admin-cap %d: must be 1-%d for --scope %s", delegateAdminCap, maxCap, delegateScope)
		}
	}
	client, err := delegationClient()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()
	d, err := client.TestIdentityDelegations().Create(ctx, &hubclient.CreateTestIdentityDelegationRequest{
		WorkloadSub: strings.TrimSpace(delegateWorkloadSub), Scopes: []string{scope},
		TTLSeconds: ttl, MemberCap: delegateMemberCap, AdminCap: delegateAdminCap, Label: delegateLabel,
	})
	if err != nil {
		return fmt.Errorf("failed to create the delegation: %w", err)
	}
	out := cmd.OutOrStdout()
	if delegateJSON {
		return writeJSONTo(out, d)
	}
	_, _ = fmt.Fprintf(out, "Created delegation %s\n", d.ID)
	_, _ = fmt.Fprintf(out, "  Workload sub: %s\n", d.WorkloadSub)
	_, _ = fmt.Fprintf(out, "  Scope:        %s\n", d.Scope)
	if d.AdminCap > 0 {
		_, _ = fmt.Fprintf(out, "  Admin cap:    %d\n", d.AdminCap)
	} else {
		_, _ = fmt.Fprintf(out, "  Member cap:   %d\n", d.MemberCap)
	}
	_, _ = fmt.Fprintf(out, "  Expires:      %s\n", formatTestIdentityTime(d.ExpiresAt))
	_, _ = fmt.Fprintf(out, "Add it to the runner's hub profile with --issuer-delegation %s=%s\n", delegateScope, d.ID)
	if delegateScope != delegateScopeMember {
		_, _ = fmt.Fprintln(out, "An admin delegation also needs the target's admin issuer service account in the profile (--admin-issuer-sa).")
	}
	return nil
}

func runHubTestIdentityDelegateList(cmd *cobra.Command, _ []string) error {
	client, err := delegationClient()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()
	resp, err := client.TestIdentityDelegations().List(ctx)
	if err != nil {
		return fmt.Errorf("failed to list delegations: %w", err)
	}
	out := cmd.OutOrStdout()
	if delegateJSON {
		return writeJSONTo(out, resp)
	}
	if len(resp.Items) == 0 {
		_, _ = fmt.Fprintln(out, "No delegations.")
		return nil
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tWORKLOAD SUB\tSCOPE\tCAP\tLIVE\tEXPIRES\tLABEL")
	for _, d := range resp.Items {
		live := "no"
		if d.Live {
			live = "yes"
		}
		identityCap := d.MemberCap
		if d.AdminCap > 0 {
			identityCap = d.AdminCap
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\t%s\n", d.ID, d.WorkloadSub, d.Scope, identityCap, live, formatTestIdentityTime(d.ExpiresAt), d.Label)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if resp.Truncated {
		_, _ = fmt.Fprintln(out, "(more delegations exist)")
	}
	return nil
}

func runHubTestIdentityDelegateRevoke(cmd *cobra.Command, args []string) error {
	client, err := delegationClient()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()
	if err := client.TestIdentityDelegations().Revoke(ctx, args[0]); err != nil {
		if apiclient.IsNotFoundError(err) {
			return fmt.Errorf("delegation %s not found", args[0])
		}
		return fmt.Errorf("failed to revoke delegation %s: %w", args[0], err)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Revoked delegation %s.\n", args[0])
	return nil
}
