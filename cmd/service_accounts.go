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
	"fmt"
	"os"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/spf13/cobra"
)

// Root-level `scion service-accounts`, P5 item B.
//
// WHY A SECOND PLACE TO SAY THIS. `scion project service-accounts` is nested
// under a project and takes its scope from there, so it cannot name a
// hub-scoped account: those belong to no project. This group takes scope from
// the root --global flag instead, exactly as `scion templates` does, which
// makes hub-scoped accounts addressable from a shell that is not inside any
// project.
//
// The nested group keeps working unchanged. It is not deprecated here and its
// behaviour is not retuned; the two share the hubclient service and differ only
// in how they decide scope.
//
// NO mint SUBCOMMAND. Minting draws on a project's quota and has no hub-scoped
// form, so a root-level `mint --global` would be a command with nothing to do.
// It stays at `scion project service-accounts mint`, where the project is part
// of the address rather than a flag.
//
// NOT IN agentAllowed, and that is a decision rather than an omission. Agent
// mode is an explicit allow-list (cli_mode.go), so this group is absent there by
// default and this comment exists so the default reads as chosen. Agents CONSUME
// service account identity through the metadata server; they do not administer
// registrations. The flat by-id route also answers 404 to any agent, because an
// agent carries no user identity -- see the noIdentity arm in
// handlers_gcp_identity_scoped.go -- so `show`, `verify` and `remove` would be
// commands that cannot succeed for the caller running them.

// saScopeFromGlobalFlag maps the root --global flag onto the SERVICE ACCOUNT
// scope vocabulary, whose scopes are "hub", "project" and "user".
//
// THIS IS THE ONLY PLACE THE MAPPING IS WRITTEN, and it is a mapping rather
// than a rename. --global is a PRESENTATION word, chosen so this group reads
// like `scion templates --global`. "hub" is a DOMAIN word. They are different
// words because they denote different things: templates really do have a scope
// called "global", distinct from the hub scope service accounts use, and
// pkg/hubclient refuses "global" outright for exactly that reason.
//
// The obvious "simplification" is to teach the client to accept both spellings.
// Do not. Once both are correct the invariant stops being falsifiable: nobody
// can ever again grep for a site that conflated template-global with SA-hub,
// because the client absorbed it (sa-arch). Presentation-to-domain translation
// belongs here, at the CLI boundary, and nowhere deeper.
//
// See templateScopeFromGlobalFlag in templates.go for the other half. The two
// spellings meet in exactly one place in this repository: the test that asserts
// they diverge.
func saScopeFromGlobalFlag() string {
	if globalMode {
		return store.ScopeHub
	}
	return store.ScopeProject
}

var serviceAccountsCmd = &cobra.Command{
	Use:     "service-accounts",
	Aliases: []string{"service-account", "sas"},
	Short:   "Manage GCP service accounts at hub or project scope",
	Long: `Manage GCP service accounts registered with the Hub.

Scope comes from the root --global flag, the same way it does for
templates:

  --global   hub-scoped accounts, which belong to no project and are
             assignable from every project
  (default)  the accounts registered to the current project

Service accounts are registered with the Hub and used to give agents
transparent GCP identity via metadata server emulation. No key material
is stored — the Hub impersonates the SA at token-generation time.

Examples:
  scion service-accounts list --global
  scion service-accounts list
  scion service-accounts list --assignable
  scion service-accounts show <id> --global
  scion service-accounts verify <id> --global
  scion service-accounts remove <id> --global`,
}

var (
	saGlobalListAssignable bool
	saGlobalAddProjectID   string
	saGlobalAddName        string
)

func init() {
	rootCmd.AddCommand(serviceAccountsCmd)

	serviceAccountsCmd.AddCommand(saGlobalListCmd)
	serviceAccountsCmd.AddCommand(saGlobalShowCmd)
	serviceAccountsCmd.AddCommand(saGlobalVerifyCmd)
	serviceAccountsCmd.AddCommand(saGlobalRemoveCmd)
	serviceAccountsCmd.AddCommand(saGlobalAddCmd)

	saGlobalListCmd.Flags().BoolVar(&saOutputJSON, "json", false, "Output in JSON format")
	saGlobalListCmd.Flags().BoolVar(&saGlobalListAssignable, "assignable", false,
		"List the accounts assignable to an agent in this project: its own plus every hub-scoped account")

	saGlobalAddCmd.Flags().StringVar(&saGlobalAddProjectID, "gcp-project", "", "GCP project ID (required)")
	saGlobalAddCmd.Flags().StringVar(&saGlobalAddName, "name", "", "Display name for the service account")
	_ = saGlobalAddCmd.MarkFlagRequired("gcp-project")
}

var saGlobalListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List GCP service accounts at the selected scope",
	Long: `List GCP service accounts.

Without --global this lists the accounts REGISTERED TO the current
project. With --global it lists hub-scoped accounts, which belong to no
project.

--assignable asks a different question: which accounts could be assigned
to an agent in this project. That is the project's own accounts plus
every hub-scoped one, because hub-scoped accounts are assignable from
everywhere. A picker that asked the first question would silently omit
accounts the user is permitted to assign.

Examples:
  scion service-accounts list --global
  scion service-accounts list
  scion service-accounts list --assignable
  scion service-accounts list --global --json`,
	Args: cobra.NoArgs,
	RunE: runSAScopedList,
}

var saGlobalShowCmd = &cobra.Command{
	Use:     "show ID",
	Aliases: []string{"get", "describe"},
	Short:   "Show one GCP service account",
	Long: `Show the details of a single registered GCP service account.

Examples:
  scion service-accounts show <id> --global
  scion service-accounts show <id>`,
	Args: cobra.ExactArgs(1),
	RunE: runSAScopedShow,
}

var saGlobalVerifyCmd = &cobra.Command{
	Use:   "verify ID",
	Short: "Verify the Hub can impersonate a service account",
	Long: `Re-run the Hub's impersonation check against a registered account.

This calls the IAM Credentials API to confirm the Hub's identity holds
roles/iam.serviceAccountTokenCreator on the target SA.

Examples:
  scion service-accounts verify <id> --global`,
	Args: cobra.ExactArgs(1),
	RunE: runSAScopedVerify,
}

var saGlobalRemoveCmd = &cobra.Command{
	Use:     "remove ID",
	Aliases: []string{"rm", "delete"},
	Short:   "Remove a GCP service account registration",
	Long: `Remove a registered GCP service account.

This does not delete the service account in GCP — it removes the
registration from the Hub.

Removing a hub-scoped account affects every project, so the Hub checks
authority against the account itself rather than against any one
project's membership. Expect a refusal unless you hold it.

Examples:
  scion service-accounts remove <id> --global`,
	Args: cobra.ExactArgs(1),
	RunE: runSAScopedRemove,
}

var saGlobalAddCmd = &cobra.Command{
	Use:   "add EMAIL",
	Short: "Register an existing GCP service account",
	Long: `Register an existing GCP service account with the Hub.

Hub-scoped registration is NOT ENABLED on the Hub today. Running this
with --global reaches the Hub and returns the Hub's own refusal, which
is deliberate: the command exists so the refusal is visible and
explains itself, rather than the flag combination silently not being a
thing.

Examples:
  scion service-accounts add worker@my-proj.iam.gserviceaccount.com --gcp-project my-proj`,
	Args: cobra.ExactArgs(1),
	RunE: runSAScopedAdd,
}

// saScopeContext is a resolved scope plus a client that can address it.
type saScopeContext struct {
	client hubclient.Client

	// scope is store.ScopeHub or store.ScopeProject.
	scope string

	// scopeID is the Scion project ID at project scope, and empty at hub scope
	// -- the Hub resolves its own ID and rejects a client-supplied one.
	scopeID string
}

// resolveSAScope builds a client and decides the scope from --global.
//
// AT HUB SCOPE THERE IS NO PROJECT TO REQUIRE, and that is the point of the
// group: a caller outside any project must still be able to list and inspect
// hub-scoped accounts. So the hub branch loads settings for the endpoint and
// stops, where the project branch goes on to demand a linked project.
func resolveSAScope() (*saScopeContext, error) {
	scope := saScopeFromGlobalFlag()

	resolvedPath, _, err := config.ResolveProjectPath(projectPath)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve project path: %w", err)
	}

	settings, err := config.LoadSettings(resolvedPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load settings: %w", err)
	}

	client, err := getHubClient(settings)
	if err != nil {
		return nil, err
	}

	if scope == store.ScopeHub {
		return &saScopeContext{client: client, scope: scope}, nil
	}

	projectID := ""
	if settings.Hub != nil && settings.Hub.ProjectID != "" {
		projectID = settings.Hub.ProjectID
	}
	if projectID == "" {
		return nil, fmt.Errorf("project not linked to Hub. Use 'scion hub link' first, " +
			"or pass --global to work with hub-scoped service accounts")
	}

	return &saScopeContext{client: client, scope: scope, scopeID: projectID}, nil
}

// ref resolves an account ID to a by-id address at this scope.
//
// Hub scope is parentless and uses the flat route. Project scope names the
// SCION project -- sc.scopeID, never the GCP project the account lives in; see
// the GCPServiceAccountRef doc for why that distinction has teeth.
func (sc *saScopeContext) ref(id string) hubclient.GCPServiceAccountRef {
	if sc.scope == store.ScopeHub {
		return hubclient.HubScopedRef(id)
	}
	return hubclient.ProjectScopedRef(sc.scopeID, id)
}

func runSAScopedList(cmd *cobra.Command, args []string) error {
	sc, err := resolveSAScope()
	if err != nil {
		return err
	}

	if saGlobalListAssignable && sc.scope == store.ScopeHub {
		return fmt.Errorf("--assignable asks which accounts an agent in a PROJECT could be " +
			"assigned; it has no meaning with --global, which already lists every hub-scoped account")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var opts *hubclient.ListGCPServiceAccountsOptions
	switch {
	case sc.scope == store.ScopeHub:
		opts = hubclient.ListHubScoped()
	case saGlobalListAssignable:
		opts = hubclient.ListForProjectIncludingHubScoped(sc.scopeID)
	default:
		opts = hubclient.ListForProject(sc.scopeID)
	}

	sas, err := sc.client.GCPServiceAccounts().List(ctx, opts)
	if err != nil {
		return fmt.Errorf("failed to list service accounts: %w", err)
	}

	if saOutputJSON || isJSONOutput() {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(sas)
	}

	if len(sas) == 0 {
		if sc.scope == store.ScopeHub {
			fmt.Println("No hub-scoped GCP service accounts registered.")
			return nil
		}
		fmt.Println("No GCP service accounts registered for this project.")
		fmt.Println("Use 'scion project service-accounts add' to register one.")
		return nil
	}

	fmt.Printf("GCP Service Accounts (%d):\n", len(sas))
	// SCOPE is a column here and not in the project-nested command because an
	// --assignable list genuinely mixes hub-scoped and project-scoped rows, and
	// the difference decides whether removing one affects every project.
	//
	// GCP PROJECT is the project the account lives in on GCP's side. It is not
	// the Scion project in the Hub's routes.
	fmt.Printf("%-36s  %-45s  %-8s  %-20s  %s\n", "ID", "EMAIL", "SCOPE", "GCP PROJECT", "VERIFIED")
	fmt.Printf("%-36s  %-45s  %-8s  %-20s  %s\n",
		"------------------------------------",
		"---------------------------------------------",
		"--------",
		"--------------------",
		"--------")
	for _, sa := range sas {
		verified := "no"
		if sa.Verified {
			verified = "yes"
		}
		fmt.Printf("%-36s  %-45s  %-8s  %-20s  %s\n",
			sa.ID,
			truncate(sa.Email, 45),
			truncate(sa.Scope, 8),
			truncate(sa.ProjectID, 20),
			verified)
	}

	return nil
}

func runSAScopedShow(cmd *cobra.Command, args []string) error {
	sc, err := resolveSAScope()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sa, err := sc.client.GCPServiceAccounts().Get(ctx, sc.ref(args[0]))
	if err != nil {
		return fmt.Errorf("failed to read service account: %w", err)
	}

	if saOutputJSON || isJSONOutput() {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(sa)
	}

	fmt.Printf("Service account: %s\n", sa.Email)
	fmt.Printf("  ID:          %s\n", sa.ID)
	fmt.Printf("  Scope:       %s\n", sa.Scope)
	fmt.Printf("  GCP Project: %s\n", sa.ProjectID)
	if sa.DisplayName != "" {
		fmt.Printf("  Name:        %s\n", sa.DisplayName)
	}
	fmt.Printf("  Verified:    %v\n", sa.Verified)
	if !sa.VerifiedAt.IsZero() {
		fmt.Printf("  Verified At: %s\n", sa.VerifiedAt.Format(time.RFC3339))
	}
	if sa.VerificationError != "" {
		fmt.Printf("  Error:       %s\n", sa.VerificationError)
	}
	fmt.Printf("  Managed:     %v\n", sa.Managed)

	// WHAT YOU MAY DO, FROM THE HUB'S ANSWER AND NOT FROM THE FACT THAT YOU CAN
	// SEE THIS. A hub-scoped account is readable by every hub member and
	// manageable by very few, so printing "you may remove this" because a row
	// came back would advertise an action that fails for the common caller.
	if sa.Capabilities != nil {
		fmt.Printf("  You may:     %v\n", sa.Capabilities.Actions)
	}

	return nil
}

func runSAScopedVerify(cmd *cobra.Command, args []string) error {
	sc, err := resolveSAScope()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sa, err := sc.client.GCPServiceAccounts().Verify(ctx, sc.ref(args[0]))
	if err != nil {
		return fmt.Errorf("verification failed: %w", err)
	}

	if saOutputJSON || isJSONOutput() {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(sa)
	}

	fmt.Printf("Service account verified: %s\n", sa.Email)
	fmt.Printf("  ID:          %s\n", sa.ID)
	fmt.Printf("  Scope:       %s\n", sa.Scope)
	fmt.Printf("  Verified:    %v\n", sa.Verified)
	if !sa.VerifiedAt.IsZero() {
		fmt.Printf("  Verified At: %s\n", sa.VerifiedAt.Format(time.RFC3339))
	}

	return nil
}

func runSAScopedRemove(cmd *cobra.Command, args []string) error {
	sc, err := resolveSAScope()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := sc.client.GCPServiceAccounts().Delete(ctx, sc.ref(args[0])); err != nil {
		return fmt.Errorf("failed to remove service account: %w", err)
	}

	fmt.Printf("Removed service account %s\n", args[0])
	return nil
}

func runSAScopedAdd(cmd *cobra.Command, args []string) error {
	sc, err := resolveSAScope()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// HUB SCOPE IS SENT, NOT PRE-EMPTED. The Hub holds hub-scoped creation shut
	// and says so in its own words. Refusing here instead would report a
	// different and less true reason, and would keep reporting it after the hold
	// is lifted.
	req := &hubclient.CreateGCPServiceAccountRequest{
		Scope:       sc.scope,
		ScopeID:     sc.scopeID,
		Email:       args[0],
		ProjectID:   saGlobalAddProjectID,
		DisplayName: saGlobalAddName,
	}

	sa, err := sc.client.GCPServiceAccounts().Create(ctx, req)
	if err != nil {
		return fmt.Errorf("failed to register service account: %w", err)
	}

	if saOutputJSON || isJSONOutput() {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(sa)
	}

	fmt.Printf("Registered service account: %s\n", sa.Email)
	fmt.Printf("  ID:          %s\n", sa.ID)
	fmt.Printf("  Scope:       %s\n", sa.Scope)
	fmt.Printf("  GCP Project: %s\n", sa.ProjectID)
	fmt.Printf("  Verified:    %v\n", sa.Verified)

	return nil
}
