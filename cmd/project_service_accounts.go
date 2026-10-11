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
	"io"
	"os"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/spf13/cobra"
)

var saOutputJSON bool

var projectServiceAccountsCmd = &cobra.Command{
	Use:     "service-accounts",
	Aliases: []string{"sa"},
	Short:   "Manage GCP service accounts for a project",
	Long: `Manage GCP service accounts registered for use by agents in a project.

Service accounts are registered with the Hub and used to provide agents
with transparent GCP identity via metadata server emulation. No key
material is stored — the Hub impersonates the SA at token-generation time.

Examples:
  scion project service-accounts list
  scion project service-accounts show <id|email|name>
  scion project service-accounts add agent-worker@project.iam.gserviceaccount.com --gcp-project my-project
  scion project service-accounts verify <id>
  scion project service-accounts remove <id>`,
}

var saAddCmd = &cobra.Command{
	Use:   "add EMAIL",
	Short: "Register a GCP service account",
	Long: `Register a GCP service account for use by agents in this project.

The Hub will verify it can impersonate this service account via the
IAM Credentials API. The Hub's own service account must have
roles/iam.serviceAccountTokenCreator on the target SA.

Examples:
  scion project service-accounts add agent-worker@my-project.iam.gserviceaccount.com --gcp-project my-project
  scion project service-accounts add agent-worker@my-project.iam.gserviceaccount.com --gcp-project my-project --name "Worker SA"`,
	Args: gcpProjectArgs(cobra.ExactArgs(1)),
	RunE: runSAAdd,
}

var saListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List registered GCP service accounts",
	Long: `List all GCP service accounts registered for this project.

Examples:
  scion project service-accounts list
  scion project service-accounts list --json`,
	Args: cobra.NoArgs,
	RunE: runSAList,
}

var saShowCmd = &cobra.Command{
	Use:     "show ID|EMAIL|NAME",
	Aliases: []string{"get", "describe", "status"},
	Short:   "Show a service account's status in this project",
	Long: `Show one GCP service account as seen from this project: identity,
verification, mapping per Kubernetes broker profile, Workload Identity
binding, the defaults that point at it, the agents using it, and the
next step to make it usable.

The account may be named by id, email (case-insensitive) or display
name, as on agent create. A name that matches more than one account is
refused with the candidate ids. Use the id or the email for a name
that contains "/".

Mappings are owned by the broker and shown read-only. When the broker
reports them, each mapping shows the Kubernetes service account, the
namespace, the source (explicitly mapped or discovered by annotation),
an ambiguous or incomplete-report marker, and the report's age. A
profile shows "not mapped" only when its report is complete and recent;
otherwise it shows "unknown" with the reason. The Workload Identity
binding is not checked by the hub and shows as unknown.

Examples:
  scion project service-accounts show <id>
  scion project service-accounts show worker@my-project.iam.gserviceaccount.com
  scion project service-accounts show "Worker SA" --json`,
	Args: cobra.ExactArgs(1),
	RunE: runSAShow,
}

var saRemoveCmd = &cobra.Command{
	Use:     "remove ID",
	Aliases: []string{"rm", "delete"},
	Short:   "Remove a GCP service account registration",
	Long: `Remove a registered GCP service account from this project.

This does not delete the service account in GCP — it only removes the
registration from the Hub.

Examples:
  scion project service-accounts remove <id>`,
	Args: cobra.ExactArgs(1),
	RunE: runSARemove,
}

var saVerifyCmd = &cobra.Command{
	Use:   "verify ID",
	Short: "Verify the Hub can impersonate a service account",
	Long: `Verify that the Hub can generate tokens for a registered service account.

This calls the IAM Credentials API to confirm the Hub's identity has
roles/iam.serviceAccountTokenCreator on the target SA.

Examples:
  scion project service-accounts verify <id>`,
	Args: cobra.ExactArgs(1),
	RunE: runSAVerify,
}

var saMintCmd = &cobra.Command{
	Use:   "mint",
	Short: "Mint a new GCP service account in the Hub's project",
	Long: `Create a new GCP service account in the Hub's own GCP project.

The minted SA is permissionless by default — no IAM roles are granted.
The Hub automatically configures itself to impersonate the SA for token
generation. You can later grant IAM permissions on your own GCP projects.

Examples:
  scion project service-accounts mint
  scion project service-accounts mint --account-id my-pipeline
  scion project service-accounts mint --account-id my-pipeline --name "My Pipeline SA"`,
	Args: cobra.NoArgs,
	RunE: runSAMint,
}

var (
	saProjectID   string
	saDisplayName string
	saMintID      string
)

func init() {
	projectCmd.AddCommand(projectServiceAccountsCmd)
	projectServiceAccountsCmd.AddCommand(saAddCmd)
	projectServiceAccountsCmd.AddCommand(saListCmd)
	projectServiceAccountsCmd.AddCommand(saShowCmd)
	projectServiceAccountsCmd.AddCommand(saRemoveCmd)
	projectServiceAccountsCmd.AddCommand(saVerifyCmd)
	projectServiceAccountsCmd.AddCommand(saMintCmd)

	saAddCmd.Flags().StringVar(&saProjectID, "gcp-project", "", "GCP project ID (required)")
	saAddCmd.Flags().StringVar(&saDisplayName, "name", "", "Display name for the service account")
	_ = saAddCmd.MarkFlagRequired("gcp-project")

	saMintCmd.Flags().StringVar(&saMintID, "account-id", "", "Custom account ID (will be prefixed with scion-)")
	saMintCmd.Flags().StringVar(&saDisplayName, "name", "", "Display name for the service account")

	saListCmd.Flags().BoolVar(&saOutputJSON, "json", false, "Output in JSON format")
	addSAAssignStatusFlags(saListCmd)
	saShowCmd.Flags().BoolVar(&saOutputJSON, "json", false, "Output in JSON format")
}

// resolveProjectForSA is a variable so tests can point the service-account
// commands at a fake Hub.
var resolveProjectForSA = resolveLinkedProjectForSA

// resolveLinkedProjectForSA resolves the project ID and creates a hub client
// for SA operations.
func resolveLinkedProjectForSA() (hubclient.Client, string, error) {
	settings, client, err := loadHubClient()
	if err != nil {
		return nil, "", err
	}

	projectID := ""
	if settings.Hub != nil && settings.Hub.ProjectID != "" {
		projectID = settings.Hub.ProjectID
	}
	if projectID == "" {
		return nil, "", fmt.Errorf("project not linked to Hub. Use 'scion hub link' first")
	}

	return client, projectID, nil
}

func runSAAdd(cmd *cobra.Command, args []string) error {
	email := args[0]

	client, projectID, err := resolveProjectForSA()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req := &hubclient.CreateGCPServiceAccountRequest{
		Scope:       store.ScopeProject,
		ScopeID:     projectID,
		Email:       email,
		ProjectID:   saProjectID,
		DisplayName: saDisplayName,
	}

	sa, err := client.GCPServiceAccounts().Create(ctx, req)
	if err != nil {
		return fmt.Errorf("failed to register service account: %w", err)
	}
	printSAWarnings(sa.Warnings)

	if isJSONOutput() {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(sa)
	}

	fmt.Printf("Registered service account: %s\n", sa.Email)
	fmt.Printf("  ID:       %s\n", sa.ID)
	fmt.Printf("  Project:  %s\n", sa.ProjectID)
	if sa.DisplayName != "" {
		fmt.Printf("  Name:     %s\n", sa.DisplayName)
	}
	fmt.Printf("  Verified: %v\n", sa.Verified)

	return nil
}

func runSAList(cmd *cobra.Command, args []string) error {
	if err := checkSAAssignStatusFlags(); err != nil {
		return err
	}
	client, projectID, err := resolveProjectForSA()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// ListForProject, not the hub-inclusive variant, because this command
	// answers "what is registered to this project" -- and hub-scoped accounts
	// are registered to the hub, not here.
	//
	// The other set, the ASSIGNABLE one, belongs to a picker: omitting hub scope
	// there would silently hide accounts the user may assign. This command is
	// not that, and the root-level `scion service-accounts list --global`
	// command is where hub-scoped accounts are listed.
	opts := hubclient.ListForProject(projectID)
	setSAAssignStatusOptions(opts)
	sas, warnings, err := client.GCPServiceAccounts().ListWithWarnings(ctx, opts)
	if err != nil {
		return fmt.Errorf("failed to list service accounts: %w", err)
	}
	printSAWarnings(warnings)

	if saOutputJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(sas)
	}

	if len(sas) == 0 {
		fmt.Println("No GCP service accounts registered for this project.")
		fmt.Println("Use 'scion project service-accounts add' to register one.")
		return nil
	}

	// GCP PROJECT, not the Scion project: sa.ProjectID is where the service
	// account lives in GCP. Every row here is registered to the Scion project
	// this command was run in, so printing that would print one repeated value.
	fmt.Printf("GCP Service Accounts (%d):\n", len(sas))
	assign := saAssignStatusRequested()
	fmt.Printf("%-36s  %-45s  %-20s  %-8s  %s\n", "ID", "EMAIL", "GCP PROJECT", "VERIFIED", saAssignLast(assign, "MAPPED", "ASSIGN"))
	fmt.Printf("%-36s  %-45s  %-20s  %-8s  %s\n",
		"------------------------------------",
		"---------------------------------------------",
		"--------------------",
		"--------",
		saAssignLast(assign, "------", "------"))
	for _, sa := range sas {
		verified := "no"
		if sa.Verified {
			verified = "yes"
		}
		fmt.Printf("%-36s  %-45s  %-20s  %-8s  %s\n",
			sa.ID,
			truncate(sa.Email, 45),
			truncate(sa.ProjectID, 20),
			verified,
			saAssignLast(assign, saMappedColumn(sa.Mapping), saAssignColumn(sa.AssignStatus)))
	}
	printSAAssignFooter(assign, sas)

	return nil
}

// saMappedColumn renders the MAPPED column: mapped profiles over the
// Kubernetes profiles that reported their mappings, "-" when none reported
// (no Kubernetes profiles, or an older Hub that sends no summary).
func saMappedColumn(m *hubclient.GCPServiceAccountMappingSummary) string {
	if m == nil || m.ReportedProfiles == 0 {
		return "-"
	}
	return fmt.Sprintf("%d/%d", m.MappedProfiles, m.ReportedProfiles)
}

func runSAShow(cmd *cobra.Command, args []string) error {
	client, projectID, err := resolveProjectForSA()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st, err := client.GCPServiceAccounts().Status(ctx, projectID, args[0])
	if err != nil {
		if apiclient.IsNotFoundError(err) {
			// A Hub without the status route answers 404 too, which would
			// otherwise read as "no such account".
			return fmt.Errorf("failed to read service account status: %w\n"+
				"No account %q is visible in this project, or the Hub is older and does not support "+
				"this command (try 'scion project service-accounts list')", err, args[0])
		}
		return fmt.Errorf("failed to read service account status: %w", err)
	}

	if saOutputJSON || isJSONOutput() {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(st)
	}

	printSAStatus(os.Stdout, st)
	return nil
}

// saMappingStateText renders a mapping state for humans.
var saMappingStateText = map[string]string{
	"mapped":       "mapped",
	"not_mapped":   "not mapped",
	"unknown":      "unknown",
	"not_reported": "not reported",
}

// saMappingText renders one profile's mapping line after its state: the
// Kubernetes ServiceAccount, namespace and source when mapped, then the
// reason a state is unknown, the ambiguous and incomplete markers, and the
// report age.
func saMappingText(m hubclient.GCPServiceAccountProfileMapping, state string) string {
	if m.KubernetesServiceAccount != "" {
		detail := "KSA " + m.KubernetesServiceAccount
		if m.Namespace != "" {
			detail += ", namespace " + m.Namespace
		}
		if m.Source != "" {
			detail += ", " + m.Source
		}
		state += " (" + detail + ")"
	}
	var notes []string
	switch m.UnknownReason {
	case "":
	case "report_stale":
		notes = append(notes, "report stale")
	case "report_incomplete":
		// The incomplete note below covers a report that names its reason;
		// one that does not still says it is incomplete.
		if !m.Incomplete {
			notes = append(notes, "report incomplete: unknown reason")
		}
	case "report_old_version":
		notes = append(notes, "the broker's report is too old a version to tell")
	case "report_missing":
		notes = append(notes, "no stored report from the broker")
	default:
		notes = append(notes, strings.ReplaceAll(m.UnknownReason, "_", " "))
	}
	if m.Ambiguous {
		notes = append(notes, "ambiguous: more than one Kubernetes service account is annotated with it")
	}
	if m.Incomplete {
		reason := m.IncompleteReason
		if reason == "" {
			reason = "unknown reason"
		}
		notes = append(notes, "report incomplete: "+strings.ReplaceAll(reason, "_", " "))
	}
	if m.ReportedAt != nil && !m.ReportedAt.IsZero() {
		notes = append(notes, "reported "+clitime.Ago(*m.ReportedAt))
	}
	if len(notes) > 0 {
		state += "; " + strings.Join(notes, "; ")
	}
	return state
}

// printSAStatus writes the status view's sections.
func printSAStatus(w io.Writer, st *hubclient.GCPServiceAccountStatus) {
	// Output errors are ignored, as for the other printers in this package.
	p := func(format string, a ...interface{}) { _, _ = fmt.Fprintf(w, format, a...) }

	name := st.Account.DisplayName
	if name == "" {
		name = st.Account.Email
	}
	p("Service account: %s\n", name)
	p("  ID:     %s\n", st.Account.ID)
	p("  Email:  %s\n", st.Account.Email)
	p("  Scope:  %s\n", st.Account.Scope)

	p("\nVerification:\n")
	status := st.Verification.Status
	if st.Verification.VerifiedAt != nil && !st.Verification.VerifiedAt.IsZero() {
		status += fmt.Sprintf(" (checked %s)", clitime.Ago(*st.Verification.VerifiedAt))
	}
	p("  Status: %s\n", status)
	if st.Verification.Error != "" {
		p("  Error:  %s\n", st.Verification.Error)
	}

	p("\nMapping (Kubernetes broker profiles, broker-owned):\n")
	if len(st.Mappings) == 0 {
		p("  no Kubernetes broker profiles in this project\n")
	}
	for _, m := range st.Mappings {
		state := saMappingStateText[m.State]
		if state == "" {
			state = m.State
		}
		p("  %-30s  %s\n", m.BrokerName+"/"+m.Profile, saMappingText(m, state))
	}

	p("\nWorkload Identity binding:\n")
	binding := strings.ReplaceAll(st.WorkloadIdentityBinding.State, "_", " ")
	if st.WorkloadIdentityBinding.Reason != "" {
		binding += fmt.Sprintf(" (%s)", st.WorkloadIdentityBinding.Reason)
	}
	p("  %s\n", binding)

	p("\nDefault for:\n")
	if len(st.DefaultFor) == 0 {
		p("  none\n")
	}
	for _, d := range st.DefaultFor {
		switch d.Kind {
		case "profile":
			p("  profile %s\n", d.Profile)
		default:
			p("  %s default\n", d.Kind)
		}
	}

	p("\nAgents using it (%d):\n", st.Agents.Count)
	if st.Agents.Count == 0 {
		p("  none\n")
	}
	for _, n := range st.Agents.Names {
		p("  %s\n", n)
	}
	if more := st.Agents.Count - len(st.Agents.Names); more > 0 {
		p("  ... and %d more\n", more)
	}

	p("\nNext step:\n")
	p("  %s\n", st.NextStep.Message)
}

func runSARemove(cmd *cobra.Command, args []string) error {
	saID := args[0]

	client, projectID, err := resolveProjectForSA()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ref := hubclient.ProjectScopedRef(projectID, saID)
	if err := client.GCPServiceAccounts().Delete(ctx, ref); err != nil {
		return fmt.Errorf("failed to remove service account: %w", err)
	}

	fmt.Printf("Removed service account %s\n", saID)
	return nil
}

func runSAMint(cmd *cobra.Command, args []string) error {
	client, projectID, err := resolveProjectForSA()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	req := &hubclient.MintGCPServiceAccountRequest{
		AccountID:   saMintID,
		DisplayName: saDisplayName,
	}

	sa, err := client.GCPServiceAccounts().Mint(ctx, projectID, req)
	if err != nil {
		return fmt.Errorf("failed to mint service account: %w", err)
	}
	printSAWarnings(sa.Warnings)

	if isJSONOutput() {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(sa)
	}

	fmt.Printf("Minted service account: %s\n", sa.Email)
	fmt.Printf("  ID:        %s\n", sa.ID)
	fmt.Printf("  Project:   %s\n", sa.ProjectID)
	if sa.DisplayName != "" {
		fmt.Printf("  Name:      %s\n", sa.DisplayName)
	}
	fmt.Printf("  Verified:  %v\n", sa.Verified)
	fmt.Printf("  Managed:   %v\n", sa.Managed)

	return nil
}

func runSAVerify(cmd *cobra.Command, args []string) error {
	saID := args[0]

	client, projectID, err := resolveProjectForSA()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sa, err := client.GCPServiceAccounts().Verify(ctx, hubclient.ProjectScopedRef(projectID, saID))
	if err != nil {
		return fmt.Errorf("verification failed: %w", err)
	}
	printSAWarnings(sa.Warnings)

	if isJSONOutput() {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(sa)
	}

	fmt.Printf("Service account verified: %s\n", sa.Email)
	fmt.Printf("  ID:          %s\n", sa.ID)
	fmt.Printf("  Project:     %s\n", sa.ProjectID)
	fmt.Printf("  Verified:    %v\n", sa.Verified)
	fmt.Printf("  Verified At: %s\n", clitime.Format(sa.VerifiedAt, clitime.Full))

	return nil
}

// printSAWarnings prints the Hub's advisory service account warnings (for
// example a GSA no Kubernetes broker profile maps) to stderr, so they never
// mix with --json output on stdout.
func printSAWarnings(warnings []string) {
	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "Warning: %s\n", w)
	}
}
