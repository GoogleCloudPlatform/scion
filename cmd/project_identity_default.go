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
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/spf13/cobra"
)

// Annotation keys of the identity defaults in the resolved-settings
// response. They mirror the hub's project setting keys.
const (
	identityDefaultModeKey = "scion.io/default-gcp-identity-mode"
	identityDefaultSAIDKey = "scion.io/default-gcp-identity-service-account-id"
)

// identityDefaultRolesHint is appended to a 403 from the settings PUT.
const identityDefaultRolesHint = "Changing a project's default GCP identity needs the project.update permission, " +
	"which the project-owner and project-admin roles hold; ask a project owner to grant you the admin role. " +
	"Agents cannot change project settings."

var (
	identityDefaultProfile string
	identityDefaultJSON    bool
)

// resolveProjectForIdentityDefault is a variable so tests can point the
// identity default commands at a fake Hub.
var resolveProjectForIdentityDefault = resolveLinkedProjectForSA

var projectIdentityCmd = &cobra.Command{
	Use:   "identity",
	Short: "Manage the project's GCP identity settings",
}

var projectIdentityDefaultCmd = &cobra.Command{
	Use:   "default",
	Short: "Show or change the default GCP identity for new agents",
	Long: `Show or change the default GCP identity the Hub gives a new agent when
the create request names none.

The Hub picks the identity from the first rung that applies:
  1. request      the identity named at create (--service-account)
  2. per-profile  the project's default account for the agent's broker profile
  3. project      the project default (an account, passthrough or block)
  4. hub          the hub default, set by a hub admin
  5. runtime      the broker's runtime default when nothing else is set

Accounts may be named by id, email or display name. The Hub resolves the
reference and the command stores the account id, so a stored default keeps
working after the account is renamed. A reference that matches more than
one account, or none, is refused before anything is written.

Each write changes only the identity field it names; every other project
setting keeps its stored value.

Examples:
  scion project identity default show
  scion project identity default set worker@my-project.iam.gserviceaccount.com
  scion project identity default set passthrough
  scion project identity default set --profile gke "Worker SA"
  scion project identity default unset --profile gke
  scion project identity default unset`,
}

var projectIdentityDefaultShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Show the default GCP identity ladder and where each rung is set",
	Args:  cobra.NoArgs,
	RunE:  runIdentityDefaultShow,
}

var projectIdentityDefaultSetCmd = &cobra.Command{
	Use:   "set ID|EMAIL|NAME|passthrough|block",
	Short: "Set the project default, or a per-profile default, GCP identity",
	Long: `Set the project default GCP identity, or with --profile the default
account for agents running under one broker profile.

The project default is an account (id, email or display name), or one of
the modes passthrough or block. A per-profile default is always an
account. The words passthrough and block are read as modes, and a bare
assign is refused (it needs an account); to name an account whose display
name is one of these words, use its id or email.

Setting the project default replaces both the mode and the account: a
mode clears the stored account, and an account sets the mode to assign.
Setting a per-profile default adds or replaces that profile's entry and
keeps the entries of other profiles. It reads the stored map and writes it
back whole, so two concurrent per-profile writes are last-writer-wins.`,
	Args: cobra.ExactArgs(1),
	RunE: runIdentityDefaultSet,
}

var projectIdentityDefaultUnsetCmd = &cobra.Command{
	Use:   "unset",
	Short: "Clear the project default, or a per-profile default, GCP identity",
	Long: `Clear the project default GCP identity (mode and account), so new agents
fall through to the hub default, or with --profile remove that profile's
default account and keep the entries of other profiles.`,
	Args: cobra.NoArgs,
	RunE: runIdentityDefaultUnset,
}

func init() {
	projectCmd.AddCommand(projectIdentityCmd)
	projectIdentityCmd.AddCommand(projectIdentityDefaultCmd)
	projectIdentityDefaultCmd.AddCommand(projectIdentityDefaultShowCmd)
	projectIdentityDefaultCmd.AddCommand(projectIdentityDefaultSetCmd)
	projectIdentityDefaultCmd.AddCommand(projectIdentityDefaultUnsetCmd)

	for _, c := range []*cobra.Command{projectIdentityDefaultShowCmd, projectIdentityDefaultSetCmd, projectIdentityDefaultUnsetCmd} {
		c.Flags().BoolVar(&identityDefaultJSON, "json", false, "Output in JSON format")
	}
	projectIdentityDefaultSetCmd.Flags().StringVar(&identityDefaultProfile, "profile", "", "Broker profile whose default account to set")
	projectIdentityDefaultUnsetCmd.Flags().StringVar(&identityDefaultProfile, "profile", "", "Broker profile whose default account to remove")
}

// identityDefaultRung is one rung of the default GCP identity ladder as
// shown by `show`. State is "set", "not_set", "unknown" or "n/a".
type identityDefaultRung struct {
	Rung               string `json:"rung"`
	Source             string `json:"source"`
	State              string `json:"state"`
	Profile            string `json:"profile,omitempty"`
	Mode               string `json:"mode,omitempty"`
	ServiceAccountID   string `json:"serviceAccountId,omitempty"`
	ServiceAccountName string `json:"serviceAccountName,omitempty"`
	Note               string `json:"note,omitempty"`
}

type identityDefaultLadder struct {
	ProjectID string                `json:"projectId"`
	Rungs     []identityDefaultRung `json:"rungs"`
}

func identityDefaultsOutputJSON() bool {
	return identityDefaultJSON || isJSONOutput()
}

func runIdentityDefaultShow(cmd *cobra.Command, args []string) error {
	client, projectID, err := resolveProjectForIdentityDefault()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resolved, err := client.Projects().GetResolvedSettings(ctx, projectID)
	if err != nil {
		return fmt.Errorf("failed to read project settings: %w", err)
	}

	ladder := buildIdentityDefaultLadder(projectID, resolved, identityDefaultAccountNames(ctx, client, projectID))
	if identityDefaultsOutputJSON() {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(ladder)
	}
	printIdentityDefaultLadder(os.Stdout, ladder)
	return nil
}

// identityDefaultAccountNames maps the ids of the accounts visible in the
// project to their display names. It is best effort: names only decorate
// the ids, so a failed list returns an empty map.
func identityDefaultAccountNames(ctx context.Context, client hubclient.Client, projectID string) map[string]string {
	names := map[string]string{}
	accounts, err := client.GCPServiceAccounts().List(ctx, &hubclient.ListGCPServiceAccountsOptions{
		Scope:            store.ScopeProject,
		ScopeID:          projectID,
		IncludeHubScoped: true,
	})
	if err != nil {
		return names
	}
	for _, sa := range accounts {
		if sa.DisplayName != "" {
			names[sa.ID] = sa.DisplayName
		}
	}
	return names
}

// buildIdentityDefaultLadder reports each rung of the ladder and where it is
// configured. It describes the rungs; it does not claim which one wins for a
// given agent, because that depends on the agent's profile and broker.
func buildIdentityDefaultLadder(projectID string, resolved *hubclient.ResolvedProjectSettings, names map[string]string) identityDefaultLadder {
	ps := resolved.Project
	if ps == nil {
		ps = &hubclient.ProjectSettings{}
	}
	rungs := []identityDefaultRung{{
		Rung:   "request",
		Source: "agent create request",
		State:  "n/a",
		Note:   "set per agent with --service-account; wins over every default",
	}}

	if len(ps.DefaultGCPIdentityServiceAccountIDByProfile) == 0 {
		rungs = append(rungs, identityDefaultRung{
			Rung:   "per-profile",
			Source: "project setting defaultGCPIdentityServiceAccountIDByProfile",
			State:  "not_set",
		})
	}
	for _, profile := range slices.Sorted(maps.Keys(ps.DefaultGCPIdentityServiceAccountIDByProfile)) {
		id := ps.DefaultGCPIdentityServiceAccountIDByProfile[profile]
		rungs = append(rungs, identityDefaultRung{
			Rung:               "per-profile",
			Source:             "project setting defaultGCPIdentityServiceAccountIDByProfile",
			State:              "set",
			Profile:            profile,
			Mode:               store.GCPMetadataModeAssign,
			ServiceAccountID:   id,
			ServiceAccountName: names[id],
			Note:               "applies to agents running under this profile",
		})
	}

	project := identityDefaultRung{
		Rung:   "project",
		Source: "project settings defaultGCPIdentityMode / defaultGCPIdentityServiceAccountID",
		State:  "not_set",
	}
	if ps.DefaultGCPIdentityMode != "" {
		project.State = "set"
		project.Mode = ps.DefaultGCPIdentityMode
		switch ps.DefaultGCPIdentityMode {
		case store.GCPMetadataModeAssign:
			project.ServiceAccountID = ps.DefaultGCPIdentityServiceAccountID
			project.ServiceAccountName = names[ps.DefaultGCPIdentityServiceAccountID]
			if project.ServiceAccountID == "" {
				project.Note = "assign without an account; agents get block"
			}
		case store.GCPMetadataModeBlock:
			project.Note = "an explicit block stops the ladder here"
		}
	}
	rungs = append(rungs, project)

	hub := identityDefaultRung{
		Rung:   "hub",
		Source: "hub setting agent_defaults.default_gcp_identity_mode",
		State:  "unknown",
	}
	if entry, ok := resolved.Settings[identityDefaultModeKey]; ok {
		switch entry.HubDefault {
		case hubclient.ResolvedHubDefaultPresent:
			hub.State = "set"
			hub.Mode, _ = entry.HubValue.(string)
			if hub.Mode == store.GCPMetadataModeAssign {
				if sa, ok := resolved.Settings[identityDefaultSAIDKey]; ok && sa.HubDefault == hubclient.ResolvedHubDefaultPresent {
					hub.ServiceAccountID, _ = sa.HubValue.(string)
					hub.ServiceAccountName = names[hub.ServiceAccountID]
				}
			}
			if hub.Mode == store.GCPMetadataModePassthrough {
				hub.Note = "hub passthrough applies only on the hub's embedded broker; elsewhere the runtime default applies"
			}
		case hubclient.ResolvedHubDefaultAbsent:
			hub.State = "not_set"
		}
	}
	if project.State == "set" {
		hub.Note = joinNote("not reached while the project default is set", hub.Note)
	}
	rungs = append(rungs, hub)

	rungs = append(rungs, identityDefaultRung{
		Rung:   "runtime",
		Source: "broker runtime",
		State:  "n/a",
		Note:   "used when no rung above applies: block, or passthrough on Kubernetes",
	})

	return identityDefaultLadder{ProjectID: projectID, Rungs: rungs}
}

func joinNote(a, b string) string {
	if b == "" {
		return a
	}
	return a + "; " + b
}

func printIdentityDefaultLadder(w io.Writer, l identityDefaultLadder) {
	_, _ = fmt.Fprintf(w, "Default GCP identity for new agents in project %s\n", l.ProjectID)
	_, _ = fmt.Fprintln(w, "(the first rung that applies to an agent wins)")
	for i, r := range l.Rungs {
		label := r.Rung
		if r.Profile != "" {
			label = fmt.Sprintf("%s %q", r.Rung, r.Profile)
		}
		_, _ = fmt.Fprintf(w, "  %d. %-24s %s\n", i+1, label, identityDefaultRungValue(r))
		_, _ = fmt.Fprintf(w, "     %-24s source: %s\n", "", r.Source)
		if r.Note != "" {
			_, _ = fmt.Fprintf(w, "     %-24s %s\n", "", r.Note)
		}
	}
}

func identityDefaultRungValue(r identityDefaultRung) string {
	switch r.State {
	case "not_set":
		return "not set"
	case "unknown":
		return "unknown"
	case "n/a":
		return "-"
	}
	if r.ServiceAccountID == "" {
		return r.Mode
	}
	v := r.Mode + " " + r.ServiceAccountID
	if r.ServiceAccountName != "" {
		v += fmt.Sprintf(" (%s)", r.ServiceAccountName)
	}
	return v
}

// identityDefaultMode returns the mode a set argument names, or "" when
// the argument is an account reference.
func identityDefaultMode(arg string) string {
	switch strings.ToLower(arg) {
	case store.GCPMetadataModePassthrough:
		return store.GCPMetadataModePassthrough
	case store.GCPMetadataModeBlock:
		return store.GCPMetadataModeBlock
	}
	return ""
}

func runIdentityDefaultSet(cmd *cobra.Command, args []string) error {
	ref := strings.TrimSpace(args[0])
	if ref == "" {
		return errors.New("an account id, email or name, or passthrough or block, is required")
	}
	if strings.EqualFold(ref, store.GCPMetadataModeAssign) {
		return errors.New("assign needs an account: pass the account's id, email or name instead")
	}
	mode := identityDefaultMode(ref)
	if identityDefaultProfile != "" && mode != "" {
		return fmt.Errorf("a per-profile default is an account; %s can only be set as the project default (omit --profile)", mode)
	}

	client, projectID, err := resolveProjectForIdentityDefault()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	update := &hubclient.ProjectGCPIdentityDefaultsUpdate{}
	switch {
	case mode != "":
		empty := ""
		update.Mode = &mode
		update.ServiceAccountID = &empty
	default:
		saID, err := resolveIdentityDefaultAccount(ctx, client, projectID, ref)
		if err != nil {
			return err
		}
		if identityDefaultProfile != "" {
			current, err := readIdentityDefaults(ctx, client, projectID)
			if err != nil {
				return err
			}
			byProfile := maps.Clone(current.DefaultGCPIdentityServiceAccountIDByProfile)
			if byProfile == nil {
				byProfile = map[string]string{}
			}
			byProfile[identityDefaultProfile] = saID
			update.ServiceAccountIDByProfile = &byProfile
		} else {
			assign := store.GCPMetadataModeAssign
			update.Mode = &assign
			update.ServiceAccountID = &saID
		}
	}

	return writeIdentityDefaults(ctx, client, projectID, update)
}

func runIdentityDefaultUnset(cmd *cobra.Command, args []string) error {
	client, projectID, err := resolveProjectForIdentityDefault()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	update := &hubclient.ProjectGCPIdentityDefaultsUpdate{}
	if identityDefaultProfile != "" {
		current, err := readIdentityDefaults(ctx, client, projectID)
		if err != nil {
			return err
		}
		if _, ok := current.DefaultGCPIdentityServiceAccountIDByProfile[identityDefaultProfile]; !ok {
			statusf("No default account is set for profile %q; nothing changed.\n", identityDefaultProfile)
			return printIdentityDefaultsResult(current)
		}
		byProfile := maps.Clone(current.DefaultGCPIdentityServiceAccountIDByProfile)
		delete(byProfile, identityDefaultProfile)
		update.ServiceAccountIDByProfile = &byProfile
	} else {
		empty, emptyID := "", ""
		update.Mode = &empty
		update.ServiceAccountID = &emptyID
	}

	return writeIdentityDefaults(ctx, client, projectID, update)
}

// readIdentityDefaults reads the stored settings for a per-profile write.
// The per-profile map is replaced whole on PUT, so the new map is built from
// the stored one. The hub has no conditional PUT, so two concurrent
// per-profile writes are last-writer-wins. Project-default writes do not
// read first: they send their two fields outright.
func readIdentityDefaults(ctx context.Context, client hubclient.Client, projectID string) (*hubclient.ProjectSettings, error) {
	current, err := client.Projects().GetSettings(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("failed to read project settings: %w", err)
	}
	return current, nil
}

// resolveIdentityDefaultAccount resolves an account reference through the
// Hub's resolver (the same one --service-account uses) and returns the
// account id. An ambiguous or unknown reference is an error, so nothing is
// written for it.
func resolveIdentityDefaultAccount(ctx context.Context, client hubclient.Client, projectID, ref string) (string, error) {
	st, err := client.GCPServiceAccounts().Status(ctx, projectID, ref)
	if err != nil {
		if apiclient.IsNotFoundError(err) {
			return "", fmt.Errorf("no GCP service account %q is available in this project; nothing was changed "+
				"(list them with 'scion project service-accounts list')", ref)
		}
		return "", fmt.Errorf("could not resolve GCP service account %q; nothing was changed: %w", ref, err)
	}
	if st.Account.ID == "" {
		return "", fmt.Errorf("the Hub returned no id for GCP service account %q; nothing was changed", ref)
	}
	return st.Account.ID, nil
}

func writeIdentityDefaults(ctx context.Context, client hubclient.Client, projectID string, update *hubclient.ProjectGCPIdentityDefaultsUpdate) error {
	res, err := client.Projects().UpdateGCPIdentityDefaults(ctx, projectID, update)
	if err != nil {
		if apiclient.IsForbiddenError(err) {
			return fmt.Errorf("failed to update the default GCP identity: %w\n%s", err, identityDefaultRolesHint)
		}
		return fmt.Errorf("failed to update the default GCP identity: %w", err)
	}
	return printIdentityDefaultsResult(res)
}

// identityDefaultsResult is the JSON output of set and unset: the stored
// identity defaults after the write.
type identityDefaultsResult struct {
	Mode                      string            `json:"defaultGCPIdentityMode"`
	ServiceAccountID          string            `json:"defaultGCPIdentityServiceAccountID"`
	ServiceAccountIDByProfile map[string]string `json:"defaultGCPIdentityServiceAccountIDByProfile"`
}

func printIdentityDefaultsResult(ps *hubclient.ProjectSettings) error {
	if ps == nil {
		ps = &hubclient.ProjectSettings{}
	}
	out := identityDefaultsResult{
		Mode:                      ps.DefaultGCPIdentityMode,
		ServiceAccountID:          ps.DefaultGCPIdentityServiceAccountID,
		ServiceAccountIDByProfile: ps.DefaultGCPIdentityServiceAccountIDByProfile,
	}
	if out.ServiceAccountIDByProfile == nil {
		out.ServiceAccountIDByProfile = map[string]string{}
	}
	if identityDefaultsOutputJSON() {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	project := "not set"
	switch {
	case out.Mode == store.GCPMetadataModeAssign:
		project = "assign " + out.ServiceAccountID
	case out.Mode != "":
		project = out.Mode
	}
	fmt.Printf("Project default: %s\n", project)
	if len(out.ServiceAccountIDByProfile) == 0 {
		fmt.Println("Per-profile defaults: none")
		return nil
	}
	fmt.Println("Per-profile defaults:")
	for _, profile := range slices.Sorted(maps.Keys(out.ServiceAccountIDByProfile)) {
		fmt.Printf("  %s: %s\n", profile, out.ServiceAccountIDByProfile[profile])
	}
	return nil
}
