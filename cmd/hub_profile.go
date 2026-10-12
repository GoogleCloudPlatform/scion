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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubprofile"
)

// CLI target-hub profiles (ptone/scion#4240). A profile names another test
// hub and how to reach it: endpoint, transport, the target's issuer and
// IAP service accounts, and its issuer delegation ids. With a profile
// selected (--hub-profile or SCION_HUB_PROFILE):
//
//   - the bearer comes only from the profile: the issuer delegation (an ID
//     token minted in-process as the target's issuer service account) or,
//     with --as, a test identity cached for the run. OAuth credentials,
//     ~/.scion/scion-token, SCION_AUTH_TOKEN and SCION_HUB_TOKEN are never
//     read, and a command with no applicable profile credential fails;
//   - only the commands in hubProfileAllowed run. They reach the target
//     only through the profile client and read no local project settings;
//     every other command is refused before it runs;
//   - a profile whose endpoint names the agent's own hub (SCION_HUB_ENDPOINT,
//     SCION_HUB_URL) is refused; the comparison is by parsed host name and
//     port (hubprofile.CheckNotOwnHub) and does not resolve DNS aliases.

var (
	hubProfileFlag string // --hub-profile
	hubProfileAs   string // --as
	hubProfileRun  string // --run
)

// hubProfileAllowed lists the commands (dot-separated paths, as in
// agentAllowed) that run with a profile selected. Parents are listed
// explicitly. Each one builds its client through openHubProfileClient (or
// needs none) and reads no local project state under a profile.
var hubProfileAllowed = map[string]bool{
	"hub":                        true,
	"hub.status":                 true,
	"hub.profile":                true,
	"hub.profile.add":            true,
	"hub.profile.list":           true,
	"hub.profile.show":           true,
	"hub.profile.remove":         true,
	"hub.test-identity":          true,
	"hub.test-identity.issue":    true,
	"hub.test-identity.token":    true,
	"hub.test-identity.list":     true,
	"hub.test-identity.delete":   true,
	"hub.test-identity.teardown": true,
	"hub.projects":               true,
	"hub.projects.info":          true,
	"hub.projects.create":        true,
	"hub.projects.delete":        true,
	"artifact":                   true,
	"artifact.publish":           true,
	"artifact.get":               true,
	"artifact.versions":          true,
	"artifact.delete":            true,
	"help":                       true,
	"global-flags":               true,
	"version":                    true,
	"completion":                 true,
	"__complete":                 true,
	"__completeNoDesc":           true,
	// hub.test-identity.delegate is deliberately absent: the delegation
	// routes take a human session, which a profile never uses.
}

// hubProfileNoSession are the allowed commands that manage profiles
// themselves and open no session.
var hubProfileNoSession = map[string]bool{
	"hub.profile": true, "hub.profile.add": true, "hub.profile.list": true,
	"hub.profile.show": true, "hub.profile.remove": true,
	"help": true, "global-flags": true, "version": true, "completion": true,
}

// hubProfileAgentRuntime are the agent-mode commands (agentAllowed) that
// run only against a target-hub profile whose endpoint is not the agent's
// own hub (ptone/scion#4240). hub.profile.list is not here: it reads only the local
// profile directory and contacts no hub.
var hubProfileAgentRuntime = map[string]bool{
	"hub.status":                 true,
	"hub.profile.add":            true,
	"hub.profile.show":           true,
	"hub.profile.remove":         true,
	"hub.test-identity.issue":    true,
	"hub.test-identity.token":    true,
	"hub.test-identity.list":     true,
	"hub.test-identity.delete":   true,
	"hub.test-identity.teardown": true,
}

// commandPath returns cmd's dot-separated path below the root.
func commandPath(cmd *cobra.Command) string {
	var parts []string
	for c := cmd; c != nil && c.HasParent(); c = c.Parent() {
		parts = append([]string{c.Name()}, parts...)
	}
	return strings.Join(parts, ".")
}

// hubProfileName returns the selected profile name: --hub-profile, else
// SCION_HUB_PROFILE, else "".
func hubProfileName() string {
	if hubProfileFlag != "" {
		return hubProfileFlag
	}
	return strings.TrimSpace(os.Getenv(hubprofile.EnvHubProfile))
}

// hubProfileRunLabel returns --run, else SCION_HUB_RUN.
func hubProfileRunLabel() string {
	if hubProfileRun != "" {
		return hubProfileRun
	}
	return strings.TrimSpace(os.Getenv(hubprofile.EnvHubRun))
}

// hubProfileSelected reports whether a profile is selected.
func hubProfileSelected() bool { return hubProfileName() != "" }

// checkHubProfileGate runs from root's PersistentPreRunE. With a profile
// selected it refuses every command outside hubProfileAllowed, and
// --hub-profile together with --hub or --no-hub. Without one it refuses
// --as and --run. In agent mode it applies the target-profile rule to the
// commands in hubProfileAgentRuntime.
func checkHubProfileGate(cmd *cobra.Command, mode CLIMode) error {
	path := commandPath(cmd)
	selected := hubProfileSelected()
	if !selected {
		if hubProfileAs != "" && profileFlagIsOurs(cmd, "as") {
			return fmt.Errorf("--as needs a hub profile (--hub-profile or %s)", hubprofile.EnvHubProfile)
		}
		if hubProfileRun != "" && profileFlagIsOurs(cmd, "run") {
			return fmt.Errorf("--run needs a hub profile (--hub-profile or %s)", hubprofile.EnvHubProfile)
		}
		if mode == ModeAgent && hubProfileAgentRuntime[path] && !hubProfileNoSession[path] {
			return fmt.Errorf("'%s' in agent mode runs only against a target-hub profile: select one with --hub-profile or %s (a profile for another hub, never this agent's own)", cmd.CommandPath(), hubprofile.EnvHubProfile)
		}
		return nil
	}
	if err := hubprofile.ValidateName("profile name", hubProfileName()); err != nil {
		return err
	}
	if !hubProfileAllowed[path] {
		return fmt.Errorf("'%s' is not available with a hub profile (%q): with a profile selected, only the target-hub commands run (hub status, hub profile, hub test-identity, hub projects, artifact publish/get/versions/delete); unset %s or drop --hub-profile to use it", cmd.CommandPath(), hubProfileName(), hubprofile.EnvHubProfile)
	}
	if hubEndpoint != "" || noHub {
		return errors.New("--hub and --no-hub cannot be used with a hub profile: the profile names the hub")
	}
	if mode == ModeAgent && hubProfileAgentRuntime[path] && !hubProfileNoSession[path] {
		// The session itself refuses the agent's own hub (hubprofile.Open);
		// check here too, so the refusal comes before any work.
		if err := checkProfileNotOwnHub(hubProfileName()); err != nil {
			return err
		}
	}
	return nil
}

// profileFlagIsOurs reports whether name on cmd is the profile flag of the
// hub or artifact tree (not a same-named local flag) and was given.
func profileFlagIsOurs(cmd *cobra.Command, name string) bool {
	f := cmd.Flags().Lookup(name)
	if f == nil || !f.Changed {
		return false
	}
	return f == hubCmd.PersistentFlags().Lookup(name) || f == artifactCmd.PersistentFlags().Lookup(name)
}

// checkProfileNotOwnHub loads a profile and refuses it when its endpoint is
// the agent's own hub.
func checkProfileNotOwnHub(name string) error {
	home, err := hubprofile.HomeDir()
	if err != nil {
		return err
	}
	p, err := hubprofile.Load(home, name)
	if err != nil {
		return err
	}
	if err := hubprofile.CheckNotOwnHub(p.Endpoint, os.Getenv); err != nil {
		return fmt.Errorf("hub profile %q: %w", name, err)
	}
	return nil
}

// hubProfileDeps are the session dependencies; tests replace them.
var hubProfileDeps = func() hubprofile.Deps { return hubprofile.Deps{} }

// openHubProfileSession opens the selected profile. With asAlias the
// session acts as the --as identity (required); otherwise as the issuer
// delegation, and --as is refused.
func openHubProfileSession(asAlias bool) (*hubprofile.Session, error) {
	name := hubProfileName()
	if name == "" {
		return nil, errors.New("no hub profile selected")
	}
	home, err := hubprofile.HomeDir()
	if err != nil {
		return nil, err
	}
	p, err := hubprofile.Load(home, name)
	if err != nil {
		return nil, err
	}
	opts := hubprofile.Options{Home: home, Profile: p, Run: hubProfileRunLabel()}
	if asAlias {
		if hubProfileAs == "" {
			return nil, fmt.Errorf("with hub profile %q this command acts as a test identity: name it with --as <alias> (and --run <label>); the issuer credential is accepted only on test identity routes", name)
		}
		opts.Alias = hubProfileAs
	} else if hubProfileAs != "" {
		return nil, errors.New("--as does not apply to this command")
	}
	return hubprofile.Open(opts, hubProfileDeps())
}

// openHubProfileClient returns a target client for a general command: it
// acts as the --as identity.
func openHubProfileClient() (hubclient.Client, error) {
	s, err := openHubProfileSession(true)
	if err != nil {
		return nil, err
	}
	return s.Client()
}

// --- scion hub profile ---

var hubProfileCmd = &cobra.Command{
	Use:   "profile",
	Short: "Manage target-hub profiles for test identities on another test hub",
	Long: `Manage target-hub profiles: named descriptions of another test hub,
used to issue and use test identities there.

A profile is stored in ~/.scion/hub-profiles/<name>.yaml and holds only
non-secret fields. Tokens are minted in memory when a command runs and are
never written to the profile, the environment or the terminal.

Select a profile with --hub-profile <name> or SCION_HUB_PROFILE. With a
profile selected, the bearer comes only from the profile (its issuer
delegation, or a test identity named with --as), never from a login, an
agent token or SCION_HUB_TOKEN, and only target-hub commands run.

Examples:
  scion hub profile add t1 --endpoint https://t1.example.com --transport iap \
    --iap-audience 1234.apps.googleusercontent.com \
    --uat-sa t1-uat@proj.iam.gserviceaccount.com \
    --issuer-sa t1-issuer@proj.iam.gserviceaccount.com \
    --issuer-delegation member=<delegation id>
  scion hub profile list
  scion hub profile show t1
  scion hub profile remove t1`,
}

var hubProfileAddCmd = &cobra.Command{
	Use:   "add NAME",
	Short: "Add a target-hub profile",
	Long: `Add a target-hub profile. Every field is non-secret.

--transport iap sends an ID token for --uat-sa (the target's IAP service
account) with --iap-audience in Proxy-Authorization. --transport none sends
no transport credential.

--issuer-sa is the target's issuer service account; issuer ID tokens are
minted as this account for the target's issuer audience (read from the
target's GET /api/v1/test-infra/status). --issuer-delegation member=<id>
names the delegation a human issuer created on the target. Only the member
tier is supported.

Tokens are minted in-process from the agent's assigned service account (the
metadata server in "assign" mode) through IAM Credentials generateIdToken,
so the assigned account needs roles/iam.serviceAccountOpenIdTokenCreator on
both target accounts.

A profile whose endpoint names this agent's own hub (SCION_HUB_ENDPOINT or
SCION_HUB_URL) is refused, and every profile is refused while either of
those is set but is not an absolute URL. The check compares parsed host
names and ports (loopback and container bridge names count as one host);
it does not resolve DNS aliases, so another name for the same hub is not
detected. The target hub's own delegation remains the boundary.`,
	Args: cobra.ExactArgs(1),
	RunE: runHubProfileAdd,
}

var hubProfileListCmd = &cobra.Command{
	Use:   "list",
	Short: "List target-hub profiles",
	Args:  cobra.NoArgs,
	RunE:  runHubProfileList,
}

var hubProfileShowCmd = &cobra.Command{
	Use:   "show NAME",
	Short: "Show a target-hub profile and its cached runs",
	Args:  cobra.ExactArgs(1),
	RunE:  runHubProfileShow,
}

var hubProfileRemoveCmd = &cobra.Command{
	Use:   "remove NAME",
	Short: "Remove a target-hub profile",
	Long: `Remove a target-hub profile. A profile with cached runs is refused:
tear them down first ('scion hub test-identity teardown --run <label>'), or
pass --remove-cache to delete the cache files (the identities stay on the
target hub until they expire).`,
	Args: cobra.ExactArgs(1),
	RunE: runHubProfileRemove,
}

var (
	hubProfileAddEndpoint    string
	hubProfileAddTransport   string
	hubProfileAddIAPAudience string
	hubProfileAddIssuerSA    string
	hubProfileAddUATSA       string
	hubProfileAddDelegations []string
	hubProfileAddReplace     bool
	hubProfileRemoveCache    bool
	hubProfileJSON           bool
)

func init() {
	rootCmd.PersistentFlags().StringVar(&hubProfileFlag, "hub-profile", "", "Target-hub profile to use (overrides SCION_HUB_PROFILE); see 'scion hub profile'")
	// --as and --run apply only with a hub profile, and only to the command
	// trees a profile can use (hub and artifact), so they do not appear on
	// unrelated commands such as start or create.
	for _, c := range []*cobra.Command{hubCmd, artifactCmd} {
		c.PersistentFlags().StringVar(&hubProfileAs, "as", "", "Only with a hub profile: act as this cached test identity of the run")
		c.PersistentFlags().StringVar(&hubProfileRun, "run", "", "Only with a hub profile: the run label of cached test identities (overrides SCION_HUB_RUN)")
	}

	hubCmd.AddCommand(hubProfileCmd)
	hubProfileCmd.AddCommand(hubProfileAddCmd, hubProfileListCmd, hubProfileShowCmd, hubProfileRemoveCmd)

	f := hubProfileAddCmd.Flags()
	f.StringVar(&hubProfileAddEndpoint, "endpoint", "", "Target hub URL (https)")
	f.StringVar(&hubProfileAddTransport, "transport", hubprofile.TransportIAP, `Transport credential: "iap" or "none"`)
	f.StringVar(&hubProfileAddIAPAudience, "iap-audience", "", "IAP OAuth client id (transport iap)")
	f.StringVar(&hubProfileAddIssuerSA, "issuer-sa", "", "The target's issuer service account email")
	f.StringVar(&hubProfileAddUATSA, "uat-sa", "", "The target's IAP service account email (transport iap)")
	f.StringSliceVar(&hubProfileAddDelegations, "issuer-delegation", nil, "Issuer delegation on the target, tier=id (only member=<id>)")
	f.BoolVar(&hubProfileAddReplace, "replace", false, "Replace an existing profile of the same name")
	_ = hubProfileAddCmd.MarkFlagRequired("endpoint")

	hubProfileListCmd.Flags().BoolVar(&hubProfileJSON, "json", false, "Output in JSON format")
	hubProfileShowCmd.Flags().BoolVar(&hubProfileJSON, "json", false, "Output in JSON format")
	hubProfileRemoveCmd.Flags().BoolVar(&hubProfileRemoveCache, "remove-cache", false, "Also delete the profile's cached runs")
}

// agentModeProfileCheck applies the agent-mode target-profile rule to the profile commands
// that name their profile positionally: in agent mode the profile must
// name another hub.
func agentModeProfileCheck(endpoint string) error {
	if resolveMode() != ModeAgent {
		return nil
	}
	return hubprofile.CheckNotOwnHub(endpoint, os.Getenv)
}

func runHubProfileAdd(cmd *cobra.Command, args []string) error {
	delegations, err := hubprofile.ParseDelegations(hubProfileAddDelegations)
	if err != nil {
		return asUsageError(err)
	}
	p := &hubprofile.Profile{
		Name: args[0], Endpoint: hubProfileAddEndpoint, Transport: hubProfileAddTransport,
		IAPAudience: hubProfileAddIAPAudience, IssuerServiceAccount: hubProfileAddIssuerSA,
		UATServiceAccount: hubProfileAddUATSA, IssuerDelegations: delegations,
	}
	if err := p.Validate(); err != nil {
		return asUsageError(err)
	}
	// Refuse the agent's own hub in every mode: a target-hub profile
	// always names another hub.
	if err := hubprofile.CheckNotOwnHub(p.Endpoint, os.Getenv); err != nil {
		return err
	}
	home, err := hubprofile.HomeDir()
	if err != nil {
		return err
	}
	if err := hubprofile.Save(home, p, hubProfileAddReplace); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Saved hub profile %q (%s).\n", p.Name, p.Endpoint)
	return nil
}

func runHubProfileList(cmd *cobra.Command, _ []string) error {
	home, err := hubprofile.HomeDir()
	if err != nil {
		return err
	}
	names, err := hubprofile.List(home)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	var profiles []*hubprofile.Profile
	for _, n := range names {
		p, err := hubprofile.Load(home, n)
		if err != nil {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Warning: %v\n", err)
			continue
		}
		profiles = append(profiles, p)
	}
	if hubProfileJSON {
		if profiles == nil {
			profiles = []*hubprofile.Profile{}
		}
		return writeIndentedJSON(out, map[string]any{"profiles": profiles})
	}
	if len(profiles) == 0 {
		_, _ = fmt.Fprintln(out, "No hub profiles.")
		return nil
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "NAME\tENDPOINT\tTRANSPORT\tMEMBER DELEGATION")
	for _, p := range profiles {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", p.Name, p.Endpoint, p.Transport, valueOrNone(p.MemberDelegation()))
	}
	return w.Flush()
}

func runHubProfileShow(cmd *cobra.Command, args []string) error {
	home, err := hubprofile.HomeDir()
	if err != nil {
		return err
	}
	p, err := hubprofile.Load(home, args[0])
	if err != nil {
		return err
	}
	if err := agentModeProfileCheck(p.Endpoint); err != nil {
		return fmt.Errorf("hub profile %q: %w", p.Name, err)
	}
	runs, err := hubprofile.ListRuns(home, p.Name)
	if err != nil {
		return err
	}
	if runs == nil {
		runs = []string{}
	}
	out := cmd.OutOrStdout()
	if hubProfileJSON {
		return writeIndentedJSON(out, map[string]any{"profile": p, "runs": runs})
	}
	_, _ = fmt.Fprintf(out, "Profile:            %s\n", p.Name)
	_, _ = fmt.Fprintf(out, "Endpoint:           %s\n", p.Endpoint)
	_, _ = fmt.Fprintf(out, "Transport:          %s\n", p.Transport)
	if p.Transport == hubprofile.TransportIAP {
		_, _ = fmt.Fprintf(out, "IAP audience:       %s\n", p.IAPAudience)
		_, _ = fmt.Fprintf(out, "IAP account:        %s\n", p.UATServiceAccount)
	}
	_, _ = fmt.Fprintf(out, "Issuer account:     %s\n", valueOrNone(p.IssuerServiceAccount))
	_, _ = fmt.Fprintf(out, "Member delegation:  %s\n", valueOrNone(p.MemberDelegation()))
	_, _ = fmt.Fprintf(out, "Cached runs:        %s\n", valueOrNone(strings.Join(runs, ", ")))
	return nil
}

func runHubProfileRemove(cmd *cobra.Command, args []string) error {
	home, err := hubprofile.HomeDir()
	if err != nil {
		return err
	}
	if p, err := hubprofile.Load(home, args[0]); err == nil {
		if err := agentModeProfileCheck(p.Endpoint); err != nil {
			return fmt.Errorf("hub profile %q: %w", p.Name, err)
		}
	} else if resolveMode() == ModeAgent {
		return err
	}
	if err := hubprofile.Remove(home, args[0], hubProfileRemoveCache); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Removed hub profile %q.\n", args[0])
	return nil
}

func writeIndentedJSON(out io.Writer, v any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// hubProfileRequested reports whether a profile is selected, before or
// after flag parsing: the parsed flag, SCION_HUB_PROFILE, or a
// --hub-profile argument (Execute resolves the mode before cobra parses
// flags).
func hubProfileRequested() bool {
	if hubProfileSelected() {
		return true
	}
	for _, a := range os.Args[1:] {
		if a == "--" {
			break
		}
		if a == "--hub-profile" || (strings.HasPrefix(a, "--hub-profile=") && a != "--hub-profile=") {
			return true
		}
	}
	return false
}
