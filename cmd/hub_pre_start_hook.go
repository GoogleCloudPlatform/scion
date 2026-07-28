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
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/spf13/cobra"
)

// hubHookCmd is the parent command for `scion hub hook`.
var hubHookCmd = &cobra.Command{
	Use:     "hook",
	Aliases: []string{"psh"},
	Short:   "Manage hub-scoped pre-start hooks",
	Long: `Manage the hub-wide shell script that runs before every agent starts.

A hub-scoped pre-start hook is a named shell script staged at
$HOME/.scion/hooks/pre-start.d/30-project-custom before an agent container
starts. Exactly one hub hook may be active at a time; creating or activating a
new hook automatically archives the previous one.

The hub hook is a fallback: a project that has its own active pre-start hook
uses that hook instead. Only projects with no active project-scoped hook get
the hub hook.

If the hook script exits non-zero, agent startup is aborted.

Managing hub hooks requires hub administrator privileges.

Examples:
  scion hub hook list
  scion hub hook create --name "Baseline tools" --script setup.sh
  cat setup.sh | scion hub hook create --name "Baseline tools" --script -
  scion hub hook show <id-or-slug>
  scion hub hook update <id-or-slug> --script new-setup.sh
  scion hub hook activate <id-or-slug>
  scion hub hook delete <id-or-slug>`,
}

var hubHookListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List hub-scoped pre-start hooks",
	Args:    cobra.NoArgs,
	RunE:    runHubHookList,
}

var hubHookShowCmd = &cobra.Command{
	Use:   "show <id-or-slug>",
	Short: "Show details of a hub-scoped pre-start hook",
	Args:  cobra.ExactArgs(1),
	RunE:  runHubHookShow,
}

var hubHookCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new hub-scoped pre-start hook (archives any current active hook)",
	Args:  cobra.NoArgs,
	RunE:  runHubHookCreate,
}

var hubHookUpdateCmd = &cobra.Command{
	Use:   "update <id-or-slug>",
	Short: "Update an existing hub-scoped pre-start hook",
	Args:  cobra.ExactArgs(1),
	RunE:  runHubHookUpdate,
}

var hubHookActivateCmd = &cobra.Command{
	Use:   "activate <id-or-slug>",
	Short: "Activate an archived hub-scoped pre-start hook",
	Long: `Mark an archived hub-scoped pre-start hook as active. The current active hub
hook (if any) is automatically archived.`,
	Args: cobra.ExactArgs(1),
	RunE: runHubHookActivate,
}

var hubHookDeleteCmd = &cobra.Command{
	Use:     "delete <id-or-slug>",
	Aliases: []string{"rm", "remove"},
	Short:   "Delete an archived hub-scoped pre-start hook",
	Long: `Delete a hub-scoped pre-start hook. Archived hooks may be deleted directly.
Deleting the active hook requires --force, and the Hub still refuses the delete
when other hub hooks exist — activate a different hook first in that case.

Note: deleting a hub hook only prevents it from being applied to future agents.
Agents already created continue to run the hook script on every restart until
they are recreated (the script is baked into the agent's applied configuration
at creation time).`,
	Args: cobra.ExactArgs(1),
	RunE: runHubHookDelete,
}

// Flags for create/update/delete.
var (
	hubHookName        string
	hubHookSlug        string
	hubHookDescription string
	hubHookScript      string // file path or "-" for stdin
	hubHookForce       bool
)

func init() {
	hubHookCmd.AddCommand(hubHookListCmd)
	hubHookCmd.AddCommand(hubHookShowCmd)
	hubHookCmd.AddCommand(hubHookCreateCmd)
	hubHookCmd.AddCommand(hubHookUpdateCmd)
	hubHookCmd.AddCommand(hubHookActivateCmd)
	hubHookCmd.AddCommand(hubHookDeleteCmd)

	// Create flags.
	hubHookCreateCmd.Flags().StringVar(&hubHookName, "name", "", "Human-readable name for the hook (required)")
	hubHookCreateCmd.Flags().StringVar(&hubHookSlug, "slug", "", "URL-safe slug (derived from name if omitted)")
	hubHookCreateCmd.Flags().StringVar(&hubHookDescription, "description", "", "Optional description")
	hubHookCreateCmd.Flags().StringVar(&hubHookScript, "script", "", `Path to shell script file, or "-" to read from stdin (required)`)
	_ = hubHookCreateCmd.MarkFlagRequired("name")
	_ = hubHookCreateCmd.MarkFlagRequired("script")

	// Update flags.
	hubHookUpdateCmd.Flags().StringVar(&hubHookName, "name", "", "New name for the hook")
	hubHookUpdateCmd.Flags().StringVar(&hubHookDescription, "description", "", "New description")
	hubHookUpdateCmd.Flags().StringVar(&hubHookScript, "script", "", `Path to updated shell script, or "-" to read from stdin`)

	// Delete flags.
	hubHookDeleteCmd.Flags().BoolVar(&hubHookForce, "force", false, "Allow deleting the currently active hub hook")
}

// resolveHubHookClient resolves the Hub client and returns a
// HubPreStartHookService. Unlike project hooks, no project ID is involved:
// hub hooks live at the hub scope and require hub administrator privileges.
func resolveHubHookClient() (hubclient.HubPreStartHookService, error) {
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

	return client.HubPreStartHooks(), nil
}

// resolveHubHookID resolves a slug-or-ID to a hook UUID by listing if needed.
func resolveHubHookID(ctx context.Context, svc hubclient.HubPreStartHookService, idOrSlug string) (string, error) {
	// If it already looks like a UUID, use it directly.
	if isUUIDLike(idOrSlug) {
		return idOrSlug, nil
	}
	// Otherwise treat it as a slug and search the list.
	list, err := svc.List(ctx)
	if err != nil {
		return "", fmt.Errorf("list hooks to resolve slug: %w", err)
	}
	for _, h := range list.Hooks {
		if h.Slug == idOrSlug || h.Name == idOrSlug {
			return h.ID, nil
		}
	}
	return "", fmt.Errorf("no hub pre-start hook found with slug or name %q", idOrSlug)
}

func runHubHookList(cmd *cobra.Command, args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	svc, err := resolveHubHookClient()
	if err != nil {
		return err
	}

	list, err := svc.List(ctx)
	if err != nil {
		if apiclient.IsUnauthorizedError(err) || apiclient.IsForbiddenError(err) {
			return fmt.Errorf("not authorized to view hub pre-start hooks; run 'scion hub auth login'")
		}
		return fmt.Errorf("list hub pre-start hooks: %w", err)
	}

	if isJSONOutput() {
		return outputJSON(list)
	}

	if len(list.Hooks) == 0 {
		fmt.Println("No hub-scoped pre-start hooks configured.")
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSLUG\tNAME\tSTATUS\tCREATED")
	for _, h := range list.Hooks {
		created := h.Created.Format("2006-01-02")
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", h.ID, h.Slug, h.Name, h.Status, created)
	}
	return tw.Flush()
}

func runHubHookShow(cmd *cobra.Command, args []string) error {
	hookRef := args[0]

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	svc, err := resolveHubHookClient()
	if err != nil {
		return err
	}

	hookID, err := resolveHubHookID(ctx, svc, hookRef)
	if err != nil {
		return err
	}

	hook, err := svc.Get(ctx, hookID)
	if err != nil {
		if apiclient.IsNotFoundError(err) {
			return fmt.Errorf("hub hook %q not found", hookRef)
		}
		if apiclient.IsUnauthorizedError(err) || apiclient.IsForbiddenError(err) {
			return fmt.Errorf("not authorized to view hub pre-start hooks; run 'scion hub auth login'")
		}
		return fmt.Errorf("get hub hook: %w", err)
	}

	if isJSONOutput() {
		return outputJSON(hook)
	}

	fmt.Printf("ID:          %s\n", hook.ID)
	fmt.Printf("Name:        %s\n", hook.Name)
	fmt.Printf("Slug:        %s\n", hook.Slug)
	fmt.Printf("Scope:       %s\n", store.PreStartHookScopeHub)
	fmt.Printf("Status:      %s\n", hook.Status)
	if hook.Description != "" {
		fmt.Printf("Description: %s\n", hook.Description)
	}
	fmt.Printf("Created:     %s\n", hook.Created.Format(time.RFC3339))
	fmt.Printf("Updated:     %s\n", hook.Updated.Format(time.RFC3339))
	fmt.Println("Script:")
	fmt.Println(strings.Repeat("-", 60))
	fmt.Println(hook.Script)
	return nil
}

func runHubHookCreate(cmd *cobra.Command, args []string) error {
	scriptContent, err := readScriptContent(hubHookScript)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	svc, err := resolveHubHookClient()
	if err != nil {
		return err
	}

	hook, err := svc.Create(ctx, &hubclient.CreateProjectPreStartHookRequest{
		Name:        hubHookName,
		Slug:        hubHookSlug,
		Description: hubHookDescription,
		Script:      scriptContent,
	})
	if err != nil {
		if apiclient.IsUnauthorizedError(err) || apiclient.IsForbiddenError(err) {
			return fmt.Errorf("not authorized to manage hub pre-start hooks (hub administrator required)")
		}
		return fmt.Errorf("create hub pre-start hook: %w", err)
	}

	if isJSONOutput() {
		return outputJSON(hook)
	}

	fmt.Printf("Created hub pre-start hook %q (ID: %s, status: %s)\n", hook.Name, hook.ID, hook.Status)
	return nil
}

func runHubHookUpdate(cmd *cobra.Command, args []string) error {
	hookRef := args[0]

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	svc, err := resolveHubHookClient()
	if err != nil {
		return err
	}

	hookID, err := resolveHubHookID(ctx, svc, hookRef)
	if err != nil {
		return err
	}

	req := &hubclient.UpdateProjectPreStartHookRequest{}

	if cmd.Flags().Changed("name") {
		v := hubHookName
		req.Name = &v
	}
	if cmd.Flags().Changed("description") {
		v := hubHookDescription
		req.Description = &v
	}
	if cmd.Flags().Changed("script") {
		scriptContent, err := readScriptContent(hubHookScript)
		if err != nil {
			return err
		}
		req.Script = &scriptContent
	}

	hook, err := svc.Update(ctx, hookID, req)
	if err != nil {
		if apiclient.IsUnauthorizedError(err) || apiclient.IsForbiddenError(err) {
			return fmt.Errorf("not authorized to manage hub pre-start hooks (hub administrator required)")
		}
		return fmt.Errorf("update hub pre-start hook: %w", err)
	}

	if isJSONOutput() {
		return outputJSON(hook)
	}

	fmt.Printf("Updated hub pre-start hook %q (ID: %s)\n", hook.Name, hook.ID)
	return nil
}

func runHubHookActivate(cmd *cobra.Command, args []string) error {
	hookRef := args[0]

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	svc, err := resolveHubHookClient()
	if err != nil {
		return err
	}

	hookID, err := resolveHubHookID(ctx, svc, hookRef)
	if err != nil {
		return err
	}

	hook, err := svc.Activate(ctx, hookID)
	if err != nil {
		if apiclient.IsUnauthorizedError(err) || apiclient.IsForbiddenError(err) {
			return fmt.Errorf("not authorized to manage hub pre-start hooks (hub administrator required)")
		}
		return fmt.Errorf("activate hub pre-start hook: %w", err)
	}

	if isJSONOutput() {
		return outputJSON(hook)
	}

	fmt.Printf("Activated hub pre-start hook %q (ID: %s)\n", hook.Name, hook.ID)
	return nil
}

func runHubHookDelete(cmd *cobra.Command, args []string) error {
	hookRef := args[0]

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	svc, err := resolveHubHookClient()
	if err != nil {
		return err
	}

	hookID, err := resolveHubHookID(ctx, svc, hookRef)
	if err != nil {
		return err
	}

	// Deleting the currently active hub hook removes the hub-wide fallback for
	// every project, so require an explicit --force.
	if !hubHookForce {
		hook, err := svc.Get(ctx, hookID)
		if err != nil {
			if apiclient.IsNotFoundError(err) {
				return fmt.Errorf("hub hook %q not found", hookRef)
			}
			if apiclient.IsUnauthorizedError(err) || apiclient.IsForbiddenError(err) {
				return fmt.Errorf("not authorized to view hub pre-start hooks; run 'scion hub auth login'")
			}
			return fmt.Errorf("get hub hook: %w", err)
		}
		if hook.Status == store.ProjectPreStartHookStatusActive {
			return fmt.Errorf("hub hook %q is active; re-run with --force to delete it", hookRef)
		}
	}

	if err := svc.Delete(ctx, hookID); err != nil {
		if apiclient.IsUnauthorizedError(err) || apiclient.IsForbiddenError(err) {
			return fmt.Errorf("not authorized to manage hub pre-start hooks (hub administrator required)")
		}
		return fmt.Errorf("delete hub pre-start hook: %w", err)
	}

	if isJSONOutput() {
		return outputJSON(map[string]string{"deleted": hookID})
	}

	fmt.Printf("Deleted hub pre-start hook (ID: %s)\n", hookID)
	fmt.Println("Note: agents created before this delete keep running the hook on every " +
		"restart until they are recreated.")
	return nil
}
