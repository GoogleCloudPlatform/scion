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
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

// projectHookCmd is the parent command for `scion project hook`.
var projectHookCmd = &cobra.Command{
	Use:     "hook",
	Aliases: []string{"psh"},
	Short:   "Manage project pre-start hooks",
	Long: `Manage shell scripts that run before every agent starts in the project.

A pre-start hook is a named shell script staged at
$HOME/.scion/hooks/pre-start.d/30-project-custom before the agent container
starts. Exactly one hook may be active per project at a time; creating or
activating a new hook automatically archives the previous one.

If the hook script exits non-zero, agent startup is aborted.

The optional [project] argument accepts a project name, slug, or UUID.
When omitted, the project is inferred from the current directory's Hub link.

Examples:
  scion project hook list
  scion project hook create --name "Install tools" --script setup.sh
  scion project hook show <id-or-slug>
  scion project hook update <id-or-slug> --script new-setup.sh
  scion project hook activate <id-or-slug>
  scion project hook delete <id-or-slug>`,
}

var projectHookListCmd = &cobra.Command{
	Use:     "list [project]",
	Aliases: []string{"ls"},
	Short:   "List pre-start hooks for a project",
	Args:    cobra.MaximumNArgs(1),
	RunE:    runProjectHookList,
}

var projectHookShowCmd = &cobra.Command{
	Use:   "show <id-or-slug> [project]",
	Short: "Show details of a pre-start hook",
	Args:  cobra.RangeArgs(1, 2),
	RunE:  runProjectHookShow,
}

var projectHookCreateCmd = &cobra.Command{
	Use:   "create [project]",
	Short: "Create a new pre-start hook (archives any current active hook)",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runProjectHookCreate,
}

var projectHookUpdateCmd = &cobra.Command{
	Use:   "update <id-or-slug> [project]",
	Short: "Update an existing pre-start hook",
	Args:  cobra.RangeArgs(1, 2),
	RunE:  runProjectHookUpdate,
}

var projectHookActivateCmd = &cobra.Command{
	Use:   "activate <id-or-slug> [project]",
	Short: "Activate an archived pre-start hook",
	Long: `Mark an archived pre-start hook as active. The current active hook (if any)
is automatically archived.`,
	Args: cobra.RangeArgs(1, 2),
	RunE: runProjectHookActivate,
}

var projectHookDeleteCmd = &cobra.Command{
	Use:     "delete <id-or-slug> [project]",
	Aliases: []string{"rm", "remove"},
	Short:   "Delete an archived pre-start hook",
	Long: `Delete a pre-start hook. Only archived hooks may be deleted. To delete
the active hook, first activate a different hook (or there is no replacement),
then delete it.

Note: deleting a hook only prevents it from being applied to future agents.
Agents already created continue to run the hook script on every restart until
they are recreated (the script is baked into the agent's applied configuration
at creation time).`,
	Args: cobra.RangeArgs(1, 2),
	RunE: runProjectHookDelete,
}

// Flags for create/update.
var (
	projectHookName        string
	projectHookSlug        string
	projectHookDescription string
	projectHookScript      string // file path or "-" for stdin
)

func init() {
	projectCmd.AddCommand(projectHookCmd)
	projectHookCmd.AddCommand(projectHookListCmd)
	projectHookCmd.AddCommand(projectHookShowCmd)
	projectHookCmd.AddCommand(projectHookCreateCmd)
	projectHookCmd.AddCommand(projectHookUpdateCmd)
	projectHookCmd.AddCommand(projectHookActivateCmd)
	projectHookCmd.AddCommand(projectHookDeleteCmd)

	// Create flags.
	projectHookCreateCmd.Flags().StringVar(&projectHookName, "name", "", "Human-readable name for the hook (required)")
	projectHookCreateCmd.Flags().StringVar(&projectHookSlug, "slug", "", "URL-safe slug (derived from name if omitted)")
	projectHookCreateCmd.Flags().StringVar(&projectHookDescription, "description", "", "Optional description")
	projectHookCreateCmd.Flags().StringVar(&projectHookScript, "script", "", `Path to shell script file, or "-" to read from stdin (required)`)
	_ = projectHookCreateCmd.MarkFlagRequired("name")
	_ = projectHookCreateCmd.MarkFlagRequired("script")

	// Update flags.
	projectHookUpdateCmd.Flags().StringVar(&projectHookName, "name", "", "New name for the hook")
	projectHookUpdateCmd.Flags().StringVar(&projectHookDescription, "description", "", "New description")
	projectHookUpdateCmd.Flags().StringVar(&projectHookScript, "script", "", `Path to updated shell script, or "-" to read from stdin`)
}

// resolveProjectHookClient resolves the Hub client and project ID, then returns
// a ProjectPreStartHookService.
func resolveProjectHookClient(ctx context.Context, projectArg string) (hubclient.ProjectPreStartHookService, error) {
	hubCtx, err := CheckHubAvailabilityWithOptions(projectPath, true)
	if err != nil {
		return nil, fmt.Errorf("hub connection required: %w", err)
	}
	if hubCtx == nil {
		return nil, fmt.Errorf("hub is not enabled; configure hub.endpoint to use project pre-start hooks")
	}

	var projectID string
	if projectArg != "" {
		project, err := resolveProjectByNameOrID(ctx, hubCtx.Client, projectArg)
		if err != nil {
			return nil, fmt.Errorf("could not resolve project %q: %w", projectArg, err)
		}
		projectID = project.ID
	} else {
		projectID, err = GetProjectID(hubCtx)
		if err != nil {
			return nil, fmt.Errorf("could not determine project ID: %w", err)
		}
	}

	return hubCtx.Client.ProjectPreStartHooks(projectID), nil
}

// scriptMaxBytes is the client-side read limit, mirroring the server's 64 KB
// enforcement. We read one extra byte to detect over-limit files early so the
// CLI can emit a clear error before sending anything to the server.
const scriptMaxBytes = 64 * 1024

// readScriptContent reads script content from a file path or stdin ("-").
// It enforces a 64 KB client-side limit to avoid buffering huge inputs.
func readScriptContent(path string) (string, error) {
	var r io.Reader
	if path == "-" {
		r = os.Stdin
	} else {
		f, err := os.Open(path)
		if err != nil {
			return "", fmt.Errorf("open script file: %w", err)
		}
		defer func() { _ = f.Close() }()
		r = f
	}
	// Read at most scriptMaxBytes+1 so we can detect over-limit files without
	// buffering the whole thing.
	data, err := io.ReadAll(io.LimitReader(r, scriptMaxBytes+1))
	if err != nil {
		return "", fmt.Errorf("read script: %w", err)
	}
	if len(data) > scriptMaxBytes {
		return "", fmt.Errorf("script size exceeds the 64 KB limit")
	}
	return string(data), nil
}

// resolveHookID resolves a slug-or-ID to a hook UUID by listing if needed.
func resolveHookID(ctx context.Context, svc hubclient.ProjectPreStartHookService, idOrSlug string) (string, error) {
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
	return "", fmt.Errorf("no pre-start hook found with slug or name %q", idOrSlug)
}

// splitHookArgs separates the optional project argument from the hook ref argument.
// When two args are present: args[0]=hookRef, args[1]=project.
// When one arg is present: it's the hookRef; project comes from current directory.
func splitHookArgs(args []string) (hookRef, projectArg string) {
	switch len(args) {
	case 1:
		return args[0], ""
	case 2:
		return args[0], args[1]
	}
	return "", ""
}

func runProjectHookList(cmd *cobra.Command, args []string) error {
	projectArg := ""
	if len(args) == 1 {
		projectArg = args[0]
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	svc, err := resolveProjectHookClient(ctx, projectArg)
	if err != nil {
		return err
	}

	list, err := svc.List(ctx)
	if err != nil {
		return fmt.Errorf("list pre-start hooks: %w", err)
	}

	if isJSONOutput() {
		return outputJSON(list)
	}

	if len(list.Hooks) == 0 {
		fmt.Println("No pre-start hooks configured for this project.")
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tSLUG\tNAME\tSTATUS\tCREATED")
	for _, h := range list.Hooks {
		created := h.Created.Format("2006-01-02")
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", h.ID, h.Slug, h.Name, h.Status, created)
	}
	return tw.Flush()
}

func runProjectHookShow(cmd *cobra.Command, args []string) error {
	hookRef, projectArg := splitHookArgs(args)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	svc, err := resolveProjectHookClient(ctx, projectArg)
	if err != nil {
		return err
	}

	hookID, err := resolveHookID(ctx, svc, hookRef)
	if err != nil {
		return err
	}

	hook, err := svc.Get(ctx, hookID)
	if err != nil {
		if apiclient.IsNotFoundError(err) {
			return fmt.Errorf("hook %q not found", hookRef)
		}
		return fmt.Errorf("get hook: %w", err)
	}

	if isJSONOutput() {
		return outputJSON(hook)
	}

	fmt.Printf("ID:          %s\n", hook.ID)
	fmt.Printf("Name:        %s\n", hook.Name)
	fmt.Printf("Slug:        %s\n", hook.Slug)
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

func runProjectHookCreate(cmd *cobra.Command, args []string) error {
	projectArg := ""
	if len(args) == 1 {
		projectArg = args[0]
	}

	scriptContent, err := readScriptContent(projectHookScript)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	svc, err := resolveProjectHookClient(ctx, projectArg)
	if err != nil {
		return err
	}

	hook, err := svc.Create(ctx, &hubclient.CreateProjectPreStartHookRequest{
		Name:        projectHookName,
		Slug:        projectHookSlug,
		Description: projectHookDescription,
		Script:      scriptContent,
	})
	if err != nil {
		if apiclient.IsUnauthorizedError(err) || apiclient.IsForbiddenError(err) {
			return fmt.Errorf("not authorized to manage pre-start hooks for this project")
		}
		return fmt.Errorf("create pre-start hook: %w", err)
	}

	if isJSONOutput() {
		return outputJSON(hook)
	}

	fmt.Printf("Created pre-start hook %q (ID: %s, status: %s)\n", hook.Name, hook.ID, hook.Status)
	return nil
}

func runProjectHookUpdate(cmd *cobra.Command, args []string) error {
	hookRef, projectArg := splitHookArgs(args)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	svc, err := resolveProjectHookClient(ctx, projectArg)
	if err != nil {
		return err
	}

	hookID, err := resolveHookID(ctx, svc, hookRef)
	if err != nil {
		return err
	}

	req := &hubclient.UpdateProjectPreStartHookRequest{}

	if cmd.Flags().Changed("name") {
		v := projectHookName
		req.Name = &v
	}
	if cmd.Flags().Changed("description") {
		v := projectHookDescription
		req.Description = &v
	}
	if cmd.Flags().Changed("script") {
		scriptContent, err := readScriptContent(projectHookScript)
		if err != nil {
			return err
		}
		req.Script = &scriptContent
	}

	hook, err := svc.Update(ctx, hookID, req)
	if err != nil {
		if apiclient.IsUnauthorizedError(err) || apiclient.IsForbiddenError(err) {
			return fmt.Errorf("not authorized to manage pre-start hooks for this project")
		}
		return fmt.Errorf("update pre-start hook: %w", err)
	}

	if isJSONOutput() {
		return outputJSON(hook)
	}

	fmt.Printf("Updated pre-start hook %q (ID: %s)\n", hook.Name, hook.ID)
	return nil
}

func runProjectHookActivate(cmd *cobra.Command, args []string) error {
	hookRef, projectArg := splitHookArgs(args)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	svc, err := resolveProjectHookClient(ctx, projectArg)
	if err != nil {
		return err
	}

	hookID, err := resolveHookID(ctx, svc, hookRef)
	if err != nil {
		return err
	}

	hook, err := svc.Activate(ctx, hookID)
	if err != nil {
		if apiclient.IsUnauthorizedError(err) || apiclient.IsForbiddenError(err) {
			return fmt.Errorf("not authorized to manage pre-start hooks for this project")
		}
		return fmt.Errorf("activate pre-start hook: %w", err)
	}

	if isJSONOutput() {
		return outputJSON(hook)
	}

	fmt.Printf("Activated pre-start hook %q (ID: %s)\n", hook.Name, hook.ID)
	return nil
}

func runProjectHookDelete(cmd *cobra.Command, args []string) error {
	hookRef, projectArg := splitHookArgs(args)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	svc, err := resolveProjectHookClient(ctx, projectArg)
	if err != nil {
		return err
	}

	hookID, err := resolveHookID(ctx, svc, hookRef)
	if err != nil {
		return err
	}

	if err := svc.Delete(ctx, hookID); err != nil {
		if apiclient.IsUnauthorizedError(err) || apiclient.IsForbiddenError(err) {
			return fmt.Errorf("not authorized to manage pre-start hooks for this project")
		}
		return fmt.Errorf("delete pre-start hook: %w", err)
	}

	if isJSONOutput() {
		return outputJSON(map[string]string{"deleted": hookID})
	}

	fmt.Printf("Deleted pre-start hook (ID: %s)\n", hookID)
	return nil
}
