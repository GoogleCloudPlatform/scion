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
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/spf13/cobra"
)

var (
	triggerGitHubUnmatchedReply string
	triggerGitHubUnmatchedText  string

	triggerGitHubPrimaryListInstallation  int64
	triggerGitHubPrimarySetProject        string
	triggerGitHubPrimarySetInstallation   int64
	triggerGitHubPrimaryUnsetInstallation int64
)

var triggerGitHubAdminHubContextFn = triggerGitHubAdminHubContext

func triggerGitHubAdminHubContext() (*HubContext, context.Context, context.CancelFunc, error) {
	hubCtx, err := requireHub(projectPath)
	if err != nil {
		return nil, nil, nil, err
	}
	if !isTriggerJSONOutput() {
		PrintUsingHub(hubCtx.Endpoint)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	return hubCtx, ctx, cancel, nil
}

var triggerGitHubCmd = &cobra.Command{
	Use:   "github",
	Short: "Inspect and configure GitHub App trigger integration",
	Long:  `Inspect GitHub App trigger readiness, project association, repository routing, and source settings, and manage hub-level primary repository routing.`,
}

var triggerGitHubStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show GitHub App trigger readiness, association, routing, and settings",
	Long:  `Display the GitHub App's trigger readiness, this project's installation association, repository routing (primary project, sibling projects, or stale_primary), and unmatched-command settings. Hub administrators also see Hub-level GitHub App webhook status.`,
	Args:  cobra.NoArgs,
	RunE:  runTriggerGitHubStatus,
}

var triggerGitHubSettingsCmd = &cobra.Command{
	Use:   "settings",
	Short: "View or update project GitHub trigger source settings",
	Long: `View or update GitHub trigger source settings for the current project.

With no flags, displays the current settings. Pass --unmatched-command-reply
and/or --unmatched-command-text to update them (unspecified fields keep their
existing values; pass --unmatched-command-text "" to reset to the default text).`,
	Args: cobra.NoArgs,
	RunE: runTriggerGitHubSettings,
}

var triggerGitHubPrimaryCmd = &cobra.Command{
	Use:   "primary",
	Short: "Manage primary project routing per GitHub repository (hub admin)",
	Long:  `List, set, or unset the primary project that receives GitHub trigger webhooks when multiple projects share a repository.`,
}

var triggerGitHubPrimaryListCmd = &cobra.Command{
	Use:   "list",
	Short: "List repository primary-project routing entries",
	Args:  cobra.NoArgs,
	RunE:  runTriggerGitHubPrimaryList,
}

var triggerGitHubPrimarySetCmd = &cobra.Command{
	Use:   "set <owner/repo>",
	Short: "Set the primary project for a GitHub repository",
	Args:  cobra.ExactArgs(1),
	RunE:  runTriggerGitHubPrimarySet,
}

var triggerGitHubPrimaryUnsetCmd = &cobra.Command{
	Use:   "unset <owner/repo>",
	Short: "Remove the primary project routing entry for a GitHub repository",
	Args:  cobra.ExactArgs(1),
	RunE:  runTriggerGitHubPrimaryUnset,
}

type triggerGitHubAssociationOutput struct {
	InstallationID *int64                            `json:"installation_id,omitempty"`
	Status         *hubclient.GitHubAppProjectStatus `json:"status,omitempty"`
}

type triggerGitHubSourceSettingsOutput struct {
	UnmatchedCommandReply bool   `json:"unmatched_command_reply"`
	UnmatchedCommandText  string `json:"unmatched_command_text"`
}

type triggerGitHubStatusOutput struct {
	ProjectID   string                                    `json:"project_id"`
	ProjectName string                                    `json:"project_name,omitempty"`
	GitRemote   string                                    `json:"git_remote,omitempty"`
	Association *triggerGitHubAssociationOutput           `json:"association,omitempty"`
	Readiness   hubclient.GitHubTriggersProjectReadiness  `json:"readiness"`
	App         *hubclient.GitHubAppConfig                `json:"app,omitempty"`
	Routing     hubclient.ProjectGitHubTriggerRoutingView `json:"routing"`
	Warnings    []string                                  `json:"warnings,omitempty"`
	Settings    triggerGitHubSourceSettingsOutput         `json:"settings"`
}

func runTriggerGitHubStatus(cmd *cobra.Command, args []string) error {
	hubCtx, projectID, ctx, cancel, err := triggerHubContextFn()
	if err != nil {
		return err
	}
	defer cancel()

	settings, err := hubCtx.Client.Projects().GetGitHubTriggerSettings(ctx, projectID)
	if err != nil {
		return wrapHubError(err)
	}

	out := triggerGitHubStatusOutput{
		ProjectID: projectID,
		Readiness: settings.Readiness,
		Routing:   settings.Routing,
		Warnings:  settings.Warnings,
		Settings: triggerGitHubSourceSettingsOutput{
			UnmatchedCommandReply: settings.UnmatchedCommandReply,
			UnmatchedCommandText:  settings.UnmatchedCommandText,
		},
	}

	if proj, pErr := hubCtx.Client.Projects().Get(ctx, projectID); pErr == nil && proj != nil {
		out.ProjectName = proj.Name
		out.GitRemote = proj.GitRemote
		if proj.GitHubInstallationID != nil || proj.GitHubAppStatus != nil {
			out.Association = &triggerGitHubAssociationOutput{
				InstallationID: proj.GitHubInstallationID,
				Status:         proj.GitHubAppStatus,
			}
		}
	}

	// Best-effort admin enrichment from GET /api/v1/github-app. Non-admin callers
	// receive 403/401, which is ignored so project members still see project status.
	if hubCtx.CredentialKind != hubsync.CredentialKindAgentToken {
		if appCfg, aErr := hubCtx.Client.GitHubApp().GetConfig(ctx); aErr == nil && appCfg != nil {
			out.App = appCfg
		} else if aErr != nil && !apiclient.IsForbiddenError(aErr) && !apiclient.IsUnauthorizedError(aErr) && !apiclient.IsNotFoundError(aErr) {
			return wrapHubError(aErr)
		}
	}

	if isTriggerJSONOutput() {
		return outputTriggerJSON(out)
	}

	printTriggerGitHubStatus(&out)
	return nil
}

func printTriggerGitHubStatus(out *triggerGitHubStatusOutput) {
	fmt.Println("App Readiness:")
	readyLabel := "not ready"
	if out.Readiness.Ready {
		readyLabel = "ready"
	}
	fmt.Printf("  Status:              %s\n", readyLabel)
	if out.Readiness.AppSlug != "" {
		fmt.Printf("  App Slug:            %s\n", triggerTerminalText(out.Readiness.AppSlug, false))
	}
	if out.App != nil {
		if out.App.AppID != 0 {
			fmt.Printf("  App ID:              %d\n", out.App.AppID)
		}
		fmt.Printf("  Configured:          %t\n", out.App.Configured)
		webhooksEnabled := out.App.WebhooksEnabled
		hasWebhookSecret := out.App.HasWebhookSecret
		if out.App.Triggers != nil {
			webhooksEnabled = out.App.Triggers.WebhooksEnabled
			hasWebhookSecret = out.App.Triggers.HasWebhookSecret
		}
		fmt.Printf("  Webhooks Enabled:    %t\n", webhooksEnabled)
		if hasWebhookSecret {
			fmt.Printf("  Webhook Secret:      configured\n")
		} else {
			fmt.Printf("  Webhook Secret:      missing\n")
		}
	}
	if len(out.Readiness.MissingEvents) > 0 {
		fmt.Printf("  Missing Events:      %s\n", formatTriggerQuotedList(out.Readiness.MissingEvents))
	}
	if len(out.Readiness.MissingPermissions) > 0 {
		fmt.Printf("  Missing Permissions: %s\n", formatTriggerQuotedList(out.Readiness.MissingPermissions))
	}
	if out.Readiness.CheckedAt != nil && !out.Readiness.CheckedAt.IsZero() {
		fmt.Printf("  Checked At:          %s\n", clitime.Format(*out.Readiness.CheckedAt, clitime.Full))
	}
	if out.Readiness.Error != "" {
		fmt.Printf("  Error:               %s\n", triggerTerminalText(out.Readiness.Error, false))
	}

	fmt.Println()
	fmt.Println("Project Association:")
	if out.ProjectName != "" {
		fmt.Printf("  Project:             %s (%s)\n",
			triggerTerminalText(out.ProjectName, false),
			triggerTerminalText(out.ProjectID, false))
	} else {
		fmt.Printf("  Project:             %s\n", triggerTerminalText(out.ProjectID, false))
	}
	if out.GitRemote != "" {
		fmt.Printf("  Git Remote:          %s\n", triggerTerminalText(out.GitRemote, false))
	} else {
		fmt.Printf("  Git Remote:          (none)\n")
	}
	if out.Association != nil && out.Association.InstallationID != nil && *out.Association.InstallationID > 0 {
		fmt.Printf("  Installation ID:     %d\n", *out.Association.InstallationID)
	} else {
		fmt.Printf("  Installation ID:     (not associated)\n")
	}
	if out.Association != nil && out.Association.Status != nil {
		st := out.Association.Status
		if st.State != "" {
			fmt.Printf("  State:               %s\n", triggerTerminalText(st.State, false))
		}
		if st.ErrorCode != "" {
			fmt.Printf("  Error Code:          %s\n", triggerTerminalText(st.ErrorCode, false))
		}
		if st.ErrorMessage != "" {
			fmt.Printf("  Error Message:       %s\n", triggerTerminalText(st.ErrorMessage, false))
		}
	}

	fmt.Println()
	fmt.Println("Repository Routing:")
	routingStatus := out.Routing.Status
	if routingStatus == "" {
		if out.Routing.PrimaryProject != nil {
			routingStatus = "ok"
		} else {
			routingStatus = "none"
		}
	}
	if routingStatus == "stale_primary" {
		fmt.Printf("  Status:              stale_primary (primary project is no longer a candidate; failing open to all candidate projects)\n")
	} else {
		fmt.Printf("  Status:              %s\n", triggerTerminalText(routingStatus, false))
	}
	if out.Routing.PrimaryProject != nil {
		label := formatProjectRef(out.Routing.PrimaryProject.Name, out.Routing.PrimaryProject.ID)
		if out.Routing.IsPrimary {
			label += " (this project)"
		}
		fmt.Printf("  Primary Project:     %s\n", label)
	} else {
		fmt.Printf("  Primary Project:     none\n")
	}
	if len(out.Routing.OtherProjects) > 0 {
		sanitized := make([]string, len(out.Routing.OtherProjects))
		for i, name := range out.Routing.OtherProjects {
			sanitized[i] = triggerTerminalText(name, false)
		}
		fmt.Printf("  Other Projects:      %s\n", strings.Join(sanitized, ", "))
	} else {
		fmt.Printf("  Other Projects:      none\n")
	}
	if len(out.Warnings) > 0 {
		fmt.Printf("  Warnings:            %s\n", formatTriggerQuotedList(out.Warnings))
	}

	fmt.Println()
	fmt.Println("Source Settings:")
	printTriggerGitHubSettingsFields(out.Settings.UnmatchedCommandReply, out.Settings.UnmatchedCommandText)
}

func printTriggerGitHubSettingsFields(reply bool, text string) {
	replyLabel := "off"
	if reply {
		replyLabel = "on"
	}
	fmt.Printf("  Unmatched Command Reply: %s\n", replyLabel)
	if text == "" {
		fmt.Printf("  Unmatched Command Text:  (default)\n")
	} else {
		fmt.Printf("  Unmatched Command Text:  %s\n", triggerTerminalText(text, false))
	}
}

func parseOnOffFlag(raw string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "on", "true":
		return true, nil
	case "off", "false":
		return false, nil
	default:
		return false, newUsageError("invalid --unmatched-command-reply value %q (must be on or off)", raw)
	}
}

func runTriggerGitHubSettings(cmd *cobra.Command, args []string) error {
	replyChanged := cmd.Flags().Changed("unmatched-command-reply")
	textChanged := cmd.Flags().Changed("unmatched-command-text")

	var desiredReply bool
	if replyChanged {
		v, err := parseOnOffFlag(triggerGitHubUnmatchedReply)
		if err != nil {
			return err
		}
		desiredReply = v
	}

	hubCtx, projectID, ctx, cancel, err := triggerHubContextFn()
	if err != nil {
		return err
	}
	defer cancel()

	var result *hubclient.ProjectGitHubTriggerSettings
	if !replyChanged && !textChanged {
		result, err = hubCtx.Client.Projects().GetGitHubTriggerSettings(ctx, projectID)
		if err != nil {
			return wrapHubError(err)
		}
	} else {
		req := &hubclient.UpdateProjectGitHubTriggerSettingsRequest{}
		if !replyChanged || !textChanged {
			current, gErr := hubCtx.Client.Projects().GetGitHubTriggerSettings(ctx, projectID)
			if gErr != nil {
				return wrapHubError(gErr)
			}
			req.UnmatchedCommandReply = current.UnmatchedCommandReply
			req.UnmatchedCommandText = current.UnmatchedCommandText
		}
		if replyChanged {
			req.UnmatchedCommandReply = desiredReply
		}
		if textChanged {
			req.UnmatchedCommandText = triggerGitHubUnmatchedText
		}
		result, err = hubCtx.Client.Projects().UpdateGitHubTriggerSettings(ctx, projectID, req)
		if err != nil {
			return wrapHubError(err)
		}
	}

	out := triggerGitHubSourceSettingsOutput{
		UnmatchedCommandReply: result.UnmatchedCommandReply,
		UnmatchedCommandText:  result.UnmatchedCommandText,
	}
	if isTriggerJSONOutput() {
		return outputTriggerJSON(out)
	}

	if replyChanged || textChanged {
		fmt.Println("Updated GitHub trigger source settings:")
	} else {
		fmt.Println("GitHub trigger source settings:")
	}
	printTriggerGitHubSettingsFields(out.UnmatchedCommandReply, out.UnmatchedCommandText)
	return nil
}

func parseOwnerRepoArg(raw string) (string, string, error) {
	trimmed := strings.TrimSpace(raw)
	parts := strings.Split(trimmed, "/")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return "", "", newUsageError("invalid repository %q: expected <owner>/<repo>", raw)
	}
	owner := strings.TrimSpace(parts[0])
	repo := strings.TrimSpace(parts[1])
	if strings.ContainsAny(owner, " \t\r\n") || strings.ContainsAny(repo, " \t\r\n") {
		return "", "", newUsageError("invalid repository %q: expected <owner>/<repo>", raw)
	}
	return owner, repo, nil
}

func runTriggerGitHubPrimaryList(cmd *cobra.Command, args []string) error {
	var instFilter *int64
	if cmd.Flags().Changed("installation") {
		if triggerGitHubPrimaryListInstallation <= 0 {
			return newUsageError("--installation must be a positive integer")
		}
		id := triggerGitHubPrimaryListInstallation
		instFilter = &id
	}

	hubCtx, ctx, cancel, err := triggerGitHubAdminHubContextFn()
	if err != nil {
		return err
	}
	defer cancel()

	entries, err := hubCtx.Client.GitHubApp().ListRepoRouting(ctx, instFilter)
	if err != nil {
		return wrapHubError(err)
	}
	if entries == nil {
		entries = []hubclient.GitHubRepoRoutingEntry{}
	}

	if isTriggerJSONOutput() {
		return outputTriggerJSON(entries)
	}

	if len(entries) == 0 {
		fmt.Println("No repository routing entries found.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "INSTALLATION\tREPO\tSTATUS\tPRIMARY\tPROJECTS")
	for _, entry := range entries {
		primaryStr := formatRoutingPrimaryColumn(entry)
		projectsStr := formatRoutingProjectsColumn(entry.Projects)
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n",
			entry.InstallationID,
			triggerTerminalText(entry.Repo, false),
			triggerTerminalText(entry.Status, false),
			primaryStr,
			projectsStr,
		)
	}
	return w.Flush()
}

func formatProjectRef(name, id string) string {
	cleanID := triggerTerminalText(id, false)
	if name == "" {
		return cleanID
	}
	return fmt.Sprintf("%s (%s)", triggerTerminalText(name, false), cleanID)
}

func formatRoutingPrimaryColumn(entry hubclient.GitHubRepoRoutingEntry) string {
	if entry.PrimaryProjectID == nil || *entry.PrimaryProjectID == "" {
		return "-"
	}
	primaryID := *entry.PrimaryProjectID
	for _, p := range entry.Projects {
		if p.ID == primaryID {
			return formatProjectRef(p.Name, p.ID)
		}
	}
	return triggerTerminalText(primaryID, false)
}

func formatRoutingProjectsColumn(projects []hubclient.GitHubRepoRoutingProject) string {
	if len(projects) == 0 {
		return "-"
	}
	names := make([]string, len(projects))
	for i, p := range projects {
		label := p.Name
		if label == "" {
			label = p.ID
		}
		label = triggerTerminalText(label, false)
		if p.HasGitHubTriggers {
			label += "*"
		}
		names[i] = label
	}
	return strings.Join(names, ", ")
}

func resolveProjectRefForRouting(ctx context.Context, projects hubclient.ProjectService, ref string) (*hubclient.Project, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, newUsageError("--project is required")
	}

	if p, err := projects.Get(ctx, ref); err == nil && p != nil {
		return p, nil
	} else if err != nil {
		var apiErr *apiclient.APIError
		if !apiclient.IsNotFoundError(err) && (!errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest) {
			return nil, wrapHubError(err)
		}
	}

	if resp, err := projects.List(ctx, &hubclient.ListProjectsOptions{Slug: ref}); err == nil && resp != nil && len(resp.Projects) > 0 {
		if len(resp.Projects) == 1 {
			p := resp.Projects[0]
			return &p, nil
		}
		return nil, fmt.Errorf("project %q matched multiple projects; pass a project ID", ref)
	}

	if resp, err := projects.List(ctx, &hubclient.ListProjectsOptions{Name: ref}); err == nil && resp != nil && len(resp.Projects) > 0 {
		if len(resp.Projects) == 1 {
			p := resp.Projects[0]
			return &p, nil
		}
		return nil, fmt.Errorf("project %q matched multiple projects; pass a project ID", ref)
	}

	return nil, fmt.Errorf("project %q not found", ref)
}

func resolveInstallationForRepoSet(
	ctx context.Context,
	ghApp hubclient.GitHubAppService,
	owner, repo string,
	proj *hubclient.Project,
) (int64, error) {
	entries, err := ghApp.ListRepoRouting(ctx, nil)
	if err != nil {
		return 0, wrapHubError(err)
	}

	targetRepo := strings.ToLower(owner + "/" + repo)
	var matching []hubclient.GitHubRepoRoutingEntry
	for _, entry := range entries {
		if strings.EqualFold(entry.Repo, targetRepo) {
			matching = append(matching, entry)
		}
	}

	if len(matching) == 1 {
		return matching[0].InstallationID, nil
	}
	if len(matching) > 1 {
		var projMatches []int64
		for _, entry := range matching {
			for _, p := range entry.Projects {
				if p.ID == proj.ID {
					projMatches = append(projMatches, entry.InstallationID)
					break
				}
			}
		}
		if len(projMatches) == 1 {
			return projMatches[0], nil
		}
		ids := make([]string, len(matching))
		for i, entry := range matching {
			ids[i] = strconv.FormatInt(entry.InstallationID, 10)
		}
		sort.Strings(ids)
		return 0, newUsageError("multiple installations match %s (%s); pass --installation <id>",
			owner+"/"+repo, strings.Join(ids, ", "))
	}

	if proj != nil && proj.GitHubInstallationID != nil && *proj.GitHubInstallationID > 0 {
		return *proj.GitHubInstallationID, nil
	}
	return 0, newUsageError("could not determine installation ID for %s; pass --installation <id>", owner+"/"+repo)
}

func runTriggerGitHubPrimarySet(cmd *cobra.Command, args []string) error {
	owner, repo, err := parseOwnerRepoArg(args[0])
	if err != nil {
		return err
	}
	projectRef := strings.TrimSpace(triggerGitHubPrimarySetProject)
	if projectRef == "" {
		return newUsageError("--project is required")
	}

	instFlagSet := cmd.Flags().Changed("installation")
	if instFlagSet && triggerGitHubPrimarySetInstallation <= 0 {
		return newUsageError("--installation must be a positive integer")
	}

	hubCtx, ctx, cancel, err := triggerGitHubAdminHubContextFn()
	if err != nil {
		return err
	}
	defer cancel()

	proj, err := resolveProjectRefForRouting(ctx, hubCtx.Client.Projects(), projectRef)
	if err != nil {
		return err
	}

	instID := triggerGitHubPrimarySetInstallation
	if !instFlagSet {
		instID, err = resolveInstallationForRepoSet(ctx, hubCtx.Client.GitHubApp(), owner, repo, proj)
		if err != nil {
			return err
		}
	}

	entry, err := hubCtx.Client.GitHubApp().SetRepoPrimary(ctx, owner, repo, &hubclient.SetGitHubRepoPrimaryRequest{
		InstallationID:   instID,
		PrimaryProjectID: proj.ID,
	})
	if err != nil {
		return wrapHubError(err)
	}

	if isTriggerJSONOutput() {
		return outputTriggerJSON(entry)
	}

	fmt.Printf("Set primary project for %s (installation %d):\n",
		triggerTerminalText(entry.Repo, false), entry.InstallationID)
	fmt.Printf("  Primary:  %s\n", formatRoutingPrimaryColumn(*entry))
	fmt.Printf("  Status:   %s\n", triggerTerminalText(entry.Status, false))
	fmt.Printf("  Projects: %s\n", formatRoutingProjectsColumn(entry.Projects))
	return nil
}

func runTriggerGitHubPrimaryUnset(cmd *cobra.Command, args []string) error {
	owner, repo, err := parseOwnerRepoArg(args[0])
	if err != nil {
		return err
	}

	var instFilter *int64
	if cmd.Flags().Changed("installation") {
		if triggerGitHubPrimaryUnsetInstallation <= 0 {
			return newUsageError("--installation must be a positive integer")
		}
		id := triggerGitHubPrimaryUnsetInstallation
		instFilter = &id
	}

	hubCtx, ctx, cancel, err := triggerGitHubAdminHubContextFn()
	if err != nil {
		return err
	}
	defer cancel()

	if err := hubCtx.Client.GitHubApp().DeleteRepoPrimary(ctx, owner, repo, instFilter); err != nil {
		return wrapHubError(err)
	}

	repoLabel := strings.ToLower(owner + "/" + repo)
	details := map[string]any{"repo": repoLabel}
	if instFilter != nil {
		details["installation_id"] = *instFilter
	}
	return outputTriggerActionResult(ActionResult{
		Status:  "ok",
		Command: "trigger github primary unset",
		Message: fmt.Sprintf("Removed primary project routing for %s.", triggerTerminalText(repoLabel, false)),
		Details: details,
	})
}

func init() {
	triggerGitHubCmd.AddCommand(triggerGitHubStatusCmd)
	triggerGitHubCmd.AddCommand(triggerGitHubSettingsCmd)
	triggerGitHubCmd.AddCommand(triggerGitHubPrimaryCmd)

	triggerGitHubPrimaryCmd.AddCommand(triggerGitHubPrimaryListCmd)
	triggerGitHubPrimaryCmd.AddCommand(triggerGitHubPrimarySetCmd)
	triggerGitHubPrimaryCmd.AddCommand(triggerGitHubPrimaryUnsetCmd)

	triggerGitHubSettingsCmd.Flags().StringVar(&triggerGitHubUnmatchedReply, "unmatched-command-reply", "", "Reply when a /command mention matches no trigger rule (on or off)")
	triggerGitHubSettingsCmd.Flags().StringVar(&triggerGitHubUnmatchedText, "unmatched-command-text", "", "Custom suffix appended to unmatched-command replies (empty string resets to default)")

	triggerGitHubPrimaryListCmd.Flags().Int64Var(&triggerGitHubPrimaryListInstallation, "installation", 0, "Filter by GitHub App installation ID")

	triggerGitHubPrimarySetCmd.Flags().StringVar(&triggerGitHubPrimarySetProject, "project", "", "Primary project ID or slug (required)")
	triggerGitHubPrimarySetCmd.Flags().Int64Var(&triggerGitHubPrimarySetInstallation, "installation", 0, "GitHub App installation ID (auto-detected when unambiguous)")

	triggerGitHubPrimaryUnsetCmd.Flags().Int64Var(&triggerGitHubPrimaryUnsetInstallation, "installation", 0, "Limit removal to a specific GitHub App installation ID")
}
