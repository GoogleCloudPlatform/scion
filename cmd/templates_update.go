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
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

var templatesUpdateCmd = &cobra.Command{
	Use:   "update [name]",
	Short: "Refresh a Hub template from its source URL",
	Long: `Refreshes a Hub template from its stored source URL, replacing its files with
the latest version of the source.

Only templates imported from a GitHub folder URL can be refreshed. Built-in
templates and templates created without a source URL are skipped.

If --url is provided, it overrides (and updates) the stored source URL.
Use --all to refresh every template that has a GitHub source URL.

Examples:
  scion templates update my-template
  scion templates update my-template --url https://github.com/org/repo/tree/main/.scion/templates/my-template
  scion templates update --all`,
	Args: cobra.MaximumNArgs(1),
	RunE: runTemplatesUpdate,
}

func runTemplatesUpdate(cmd *cobra.Command, args []string) error {
	urlOverride, _ := cmd.Flags().GetString("url")
	all, _ := cmd.Flags().GetBool("all")
	scope, _ := cmd.Flags().GetString("scope")

	if len(args) == 0 && !all {
		return newUsageError("specify a template name or use --all")
	}
	if all && len(args) > 0 {
		return newUsageError("specify a template name or --all, not both")
	}
	if all && urlOverride != "" {
		return newUsageError("--all and --url cannot be used together")
	}
	switch scope {
	case "", "global", "project", "user":
	default:
		return newUsageError("--scope must be global, project or user")
	}

	var gp string
	if projectPath != "" {
		resolved, err := config.GetResolvedProjectDir(projectPath)
		if err != nil {
			return fmt.Errorf("failed to resolve project path %q: %w", projectPath, err)
		}
		gp = resolved
	} else if projectDir, err := config.GetResolvedProjectDir(""); err == nil {
		gp = projectDir
	}

	hubCtx, err := CheckHubAvailabilityWithOptions(gp, true)
	if err != nil {
		return err
	}
	if hubCtx == nil {
		return fmt.Errorf("hub is not available, the update command requires a hub connection")
	}

	PrintUsingHub(hubCtx.Endpoint)

	if all {
		// Each template gets its own deadline, so one slow source does not
		// use up the time of the templates after it.
		return updateAllTemplates(cmd.Context(), hubCtx.Client.Templates(), scope, templateUpdateTimeout)
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), templateUpdateTimeout)
	defer cancel()
	return updateSingleTemplate(ctx, hubCtx.Client.Templates(), args[0], urlOverride, scope)
}

// templateUpdateTimeout bounds one template refresh (and, with --all, each
// template's refresh separately).
const templateUpdateTimeout = 5 * time.Minute

// templateListTimeout bounds each page of the template list.
const templateListTimeout = time.Minute

// templateSourceRefreshable reports whether a stored template source URL is
// one the Hub can refresh from: an https URL on github.com with no username
// or password. This matches the Web UI; the Hub applies the full check.
func templateSourceRefreshable(sourceURL string) bool {
	u, err := url.Parse(strings.TrimSpace(sourceURL))
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Scheme, "https") && strings.EqualFold(u.Hostname(), "github.com") && u.User == nil
}

// displaySourceURL returns sourceURL for printing. The display never shows
// credentials embedded in a source string: http(s) and builtin:// URLs are
// shown without any username, password, query or fragment, and any other
// source is shown as a fixed label.
func displaySourceURL(sourceURL string) string {
	u, err := url.Parse(strings.TrimSpace(sourceURL))
	if err != nil || u.Opaque != "" || u.Host == "" {
		return "(non-web source)"
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "builtin":
	default:
		return "(non-web source)"
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.RawFragment = ""
	return u.String()
}

func updateSingleTemplate(ctx context.Context, svc hubclient.TemplateService, name, urlOverride, scope string) error {
	resp, err := svc.List(ctx, &hubclient.ListTemplatesOptions{
		Name:   name,
		Scope:  scope,
		Status: "active",
	})
	if err != nil {
		return fmt.Errorf("failed to search Hub: %w", err)
	}

	var matches []*hubclient.Template
	for i := range resp.Templates {
		t := &resp.Templates[i]
		if t.Name == name || t.Slug == name {
			matches = append(matches, t)
		}
	}
	if len(matches) == 0 {
		return fmt.Errorf("template %q not found on Hub", name)
	}
	if len(matches) > 1 {
		scopes := make([]string, len(matches))
		for i, m := range matches {
			scopes[i] = m.Scope
		}
		return fmt.Errorf("template %q exists in several scopes (%s), use --scope to choose one",
			name, strings.Join(scopes, ", "))
	}
	match := matches[0]

	if urlOverride == "" {
		if match.SourceURL == "" {
			return fmt.Errorf("template %q has no stored source URL, use --url to specify one", name)
		}
		if !templateSourceRefreshable(match.SourceURL) {
			return fmt.Errorf("template %q was not imported from a GitHub URL and cannot be refreshed from source, use --url to specify one", name)
		}
	}

	if !isJSONOutput() {
		shown := urlOverride
		if shown == "" {
			shown = match.SourceURL
		}
		fmt.Printf("Updating %q from %s...\n", name, displaySourceURL(shown))
	}

	result, err := svc.Reimport(ctx, match.ID, urlOverride)
	if err != nil {
		return fmt.Errorf("reimport failed: %w", err)
	}

	if isJSONOutput() {
		return outputJSON(ActionResult{
			Status:  "success",
			Command: "templates update",
			Message: fmt.Sprintf("Updated %d template(s)", result.Count),
			Details: map[string]interface{}{
				"name":     name,
				"imported": result.Templates,
				"count":    result.Count,
			},
		})
	}

	fmt.Printf("Updated %d template(s): %v\n", result.Count, result.Templates)
	return nil
}

func updateAllTemplates(ctx context.Context, svc hubclient.TemplateService, scope string, perTemplate time.Duration) error {
	var templates []hubclient.Template
	opts := &hubclient.ListTemplatesOptions{Scope: scope, Status: "active"}
	for {
		listCtx, cancel := context.WithTimeout(ctx, templateListTimeout)
		resp, err := svc.List(listCtx, opts)
		cancel()
		if err != nil {
			return fmt.Errorf("failed to list templates: %w", err)
		}
		templates = append(templates, resp.Templates...)
		if resp.Page.NextCursor == "" {
			break
		}
		opts.Page.Cursor = resp.Page.NextCursor
	}

	var updated, skipped, failed int
	jsonOut := isJSONOutput()
	for _, t := range templates {
		if !templateSourceRefreshable(t.SourceURL) {
			skipped++
			continue
		}
		if !jsonOut {
			fmt.Printf("Updating %q (%s) from %s...\n", t.Name, t.Scope, displaySourceURL(t.SourceURL))
		}
		tctx, cancel := context.WithTimeout(ctx, perTemplate)
		_, err := svc.Reimport(tctx, t.ID, "")
		cancel()
		if err != nil {
			if !jsonOut {
				fmt.Printf("  Failed: %s\n", err)
			}
			failed++
			continue
		}
		if !jsonOut {
			fmt.Printf("  Done.\n")
		}
		updated++
	}

	msg := fmt.Sprintf("Updated %d, skipped %d (no GitHub source URL), failed %d", updated, skipped, failed)
	if jsonOut {
		status := "success"
		if failed > 0 {
			status = "warn"
		}
		return outputJSON(ActionResult{
			Status:  status,
			Command: "templates update --all",
			Message: msg,
			Details: map[string]interface{}{"updated": updated, "skipped": skipped, "failed": failed},
		})
	}

	fmt.Printf("\n%s\n", msg)
	if failed > 0 {
		return fmt.Errorf("%d template(s) failed to update", failed)
	}
	return nil
}

func init() {
	templatesCmd.AddCommand(templatesUpdateCmd)
	templatesUpdateCmd.Flags().String("url", "", "Override (and store) the template's source URL")
	templatesUpdateCmd.Flags().Bool("all", false, "Update all templates that have a GitHub source URL")
	templatesUpdateCmd.Flags().String("scope", "", "Only consider templates in this scope (global, project, user)")
}
