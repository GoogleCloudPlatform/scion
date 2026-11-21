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

// templateImportURLTimeout bounds a Hub URL import request.
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
  scion templates import-url https://github.com/org/repo/tree/main/.scion/templates --name my-template`,
	Args: cobra.ExactArgs(1),
	RunE: runTemplatesImportURL,
}

func runTemplatesImportURL(cmd *cobra.Command, args []string) error {
	sourceURL := args[0]
	scope, _ := cmd.Flags().GetString("scope")
	names, _ := cmd.Flags().GetStringSlice("name")

	switch scope {
	case "", "project":
		scope = "project"
	default:
		return newUsageError("--scope must be global, project or user")
	}

	hubCtx, err := requireHubWith(projectPath, scope == "global" || scope == "user")
	if err != nil {
		return err
	}
	PrintUsingHub(hubCtx.Endpoint)

	var scopeID string
	if scope == "project" {
		scopeID, err = GetProjectID(hubCtx)
		if err != nil || scopeID == "" {
			if err != nil {
				return fmt.Errorf("failed to resolve project ID for --scope project: %w", err)
			}
			return newUsageError("no project context resolved for --scope project")
		}
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), templateImportURLTimeout)
	defer cancel()

	return executeTemplatesImportURL(ctx, hubCtx.Client.Templates(), scope, scopeID, sourceURL, names)
}

func executeTemplatesImportURL(
	ctx context.Context,
	svc hubclient.TemplateService,
	scope, scopeID, sourceURL string,
	names []string,
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
		return annotateImportURLError(err, scope, false)
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
	templatesImportURLCmd.Flags().String("scope", "", "Target scope (project, global, user)")
	templatesImportURLCmd.Flags().StringSlice("name", nil, "Import only the named template(s) (repeatable)")
}
