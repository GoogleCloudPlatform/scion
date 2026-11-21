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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

// templateImportURLTimeout bounds a Hub URL import or discover request.
const templateImportURLTimeout = 5 * time.Minute

var templatesImportURLCmd = &cobra.Command{
	Use:   "import-url <github-url>",
	Short: "Import templates from a GitHub URL through the Hub",
	Long: `Imports one or more templates from a GitHub folder URL directly through the Hub.

Import always overwrites templates with the same name in the target scope (skipping
templates whose content hash is unchanged). Credentials are resolved by the Hub from
the target scope's secrets (see docs: hosted/user/secrets.md); no GitHub token is
sent from the CLI.

Agents can only target their own project and can only create new templates.

Examples:
  scion templates import-url https://github.com/org/repo/tree/main/.scion/templates
  scion templates import-url https://github.com/org/repo/tree/main/.scion/templates --name my-template
  scion templates import-url https://github.com/org/repo/tree/main/.scion/templates --dry-run
  scion templates import-url https://github.com/org/repo/tree/main/.scion/templates --scope global`,
	Args: cobra.ExactArgs(1),
	RunE: runTemplatesImportURL,
}

func registerTemplatesImportURLFlags(cmd *cobra.Command) {
	cmd.Flags().String("scope", "", "Target scope (project, global, user)")
	cmd.Flags().StringSlice("name", nil, "Import only the named template(s) (repeatable)")
	cmd.Flags().Bool("dry-run", false, "Discover and list templates without importing")
}

// resolveImportURLScope implements the 5-step scope resolution rules for
// `scion templates import-url`:
//  1. Explicit --scope (global, project, or user); conflicts with root --global if not global.
//  2. Root --global selects global.
//  3. Agent mode defaults to project.
//  4. Human with a resolved project context defaults to project.
//  5. Otherwise fail with a usage error.
func resolveImportURLScope(flagScope string, globalFlag, agentMode, hasProject bool) (string, error) {
	switch flagScope {
	case "", "global", "project", "user":
	default:
		return "", newUsageError("--scope must be global, project or user")
	}

	if globalFlag {
		switch flagScope {
		case "", "global":
			return "global", nil
		default:
			return "", newUsageError("--global conflicts with --scope %s", flagScope)
		}
	}

	if flagScope != "" {
		return flagScope, nil
	}
	if agentMode {
		return "project", nil
	}
	if hasProject {
		return "project", nil
	}
	return "", newUsageError("specify --scope (no project context)")
}

func runTemplatesImportURL(cmd *cobra.Command, args []string) error {
	sourceURL := args[0]
	flagScope, _ := cmd.Flags().GetString("scope")
	names, _ := cmd.Flags().GetStringSlice("name")
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	agentMode := resolveMode() == ModeAgent

	// Validate explicit scope and --global conflicts before contacting the Hub.
	switch flagScope {
	case "", "global", "project", "user":
	default:
		return newUsageError("--scope must be global, project or user")
	}
	if globalMode && flagScope != "" && flagScope != "global" {
		return newUsageError("--global conflicts with --scope %s", flagScope)
	}

	preScope := flagScope
	if preScope == "" && globalMode {
		preScope = "global"
	}

	// Global and user scopes do not require a linked project.
	hubCtx, err := requireHubWith(projectPath, preScope == "global" || preScope == "user")
	if err != nil {
		return err
	}
	PrintUsingHub(hubCtx.Endpoint)

	var currentProject string
	if preScope != "global" && preScope != "user" {
		currentProject, _ = GetProjectID(hubCtx)
	}

	scope, err := resolveImportURLScope(flagScope, globalMode, agentMode, currentProject != "")
	if err != nil {
		return err
	}

	var scopeID string
	if scope == "project" {
		scopeID = currentProject
		if scopeID == "" {
			scopeID, err = GetProjectID(hubCtx)
			if err != nil {
				return fmt.Errorf("failed to resolve project ID for --scope project: %w", err)
			}
			if scopeID == "" {
				return newUsageError("no project context resolved for --scope project")
			}
		}
	}
	// For scope == "user", scopeID stays empty; the Hub forces it to the caller.

	ctx, cancel := context.WithTimeout(cmd.Context(), templateImportURLTimeout)
	defer cancel()

	if dryRun {
		return executeTemplatesImportURLDryRun(ctx, hubCtx.Client.Templates(), scope, scopeID, sourceURL, names, agentMode)
	}
	return executeTemplatesImportURL(ctx, hubCtx.Client.Templates(), scope, scopeID, sourceURL, names, agentMode)
}

func executeTemplatesImportURLDryRun(
	ctx context.Context,
	svc hubclient.TemplateService,
	scope, scopeID, sourceURL string,
	names []string,
	agentMode bool,
) error {
	req := &hubclient.DiscoverResourcesRequest{
		Kind:      "template",
		Scope:     scope,
		ScopeID:   scopeID,
		SourceURL: sourceURL,
	}

	resp, err := svc.DiscoverResources(ctx, req)
	if err != nil {
		return annotateImportURLError(err, scope, agentMode)
	}

	if len(names) > 0 {
		allowed := make(map[string]struct{}, len(names))
		for _, n := range names {
			allowed[n] = struct{}{}
		}
		filtered := make([]string, 0, len(resp.Resources))
		for _, r := range resp.Resources {
			if _, ok := allowed[r]; ok {
				filtered = append(filtered, r)
			}
		}
		resp = &hubclient.DiscoverResourcesResponse{
			Resources: filtered,
			Skipped:   resp.Skipped,
			Count:     len(filtered),
		}
	}

	if isJSONOutput() {
		return outputJSON(resp)
	}

	for _, name := range resp.Resources {
		fmt.Printf("Would import: %s\n", name)
	}
	fmt.Printf("Found %d template(s)\n", resp.Count)
	return nil
}

func executeTemplatesImportURL(
	ctx context.Context,
	svc hubclient.TemplateService,
	scope, scopeID, sourceURL string,
	names []string,
	agentMode bool,
) error {
	req := &hubclient.ImportResourcesRequest{
		Kind:      "template",
		Scope:     scope,
		ScopeID:   scopeID,
		SourceURL: sourceURL,
		Names:     names,
	}

	resp, err := svc.ImportResources(ctx, req)
	if err != nil {
		return annotateImportURLError(err, scope, agentMode)
	}

	if isJSONOutput() {
		if err := outputJSON(resp); err != nil {
			return err
		}
		if len(resp.Failed) > 0 {
			return fmt.Errorf("%d template(s) failed to import", len(resp.Failed))
		}
		return nil
	}

	for _, name := range resp.Imported {
		fmt.Printf("Imported: %s\n", name)
	}
	for _, f := range resp.Failed {
		fmt.Printf("%s: %s\n", f.Name, f.Reason)
	}
	fmt.Printf("Imported %d template(s)", resp.Count)
	if len(resp.Failed) > 0 {
		fmt.Printf(", %d failed", len(resp.Failed))
	}
	fmt.Println()

	if len(resp.Failed) > 0 {
		return fmt.Errorf("%d template(s) failed to import", len(resp.Failed))
	}
	return nil
}

func annotateImportURLError(err error, scope string, agentMode bool) error {
	var apiErr *apiclient.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			if agentMode && scope == "global" {
				return fmt.Errorf("%w (hint: global catalog writes need a human session or hub token)", err)
			}
			return fmt.Errorf("%w (hint: check your permissions for scope %q)", err, scope)
		}
	}
	return err
}

func init() {
	templatesCmd.AddCommand(templatesImportURLCmd)
	registerTemplatesImportURLFlags(templatesImportURLCmd)
}
