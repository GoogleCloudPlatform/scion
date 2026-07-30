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
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

// projectSkillsCmd is the parent group for `scion project skills` subcommands.
var projectSkillsCmd = &cobra.Command{
	Use:   "skills",
	Short: "Manage injected skills for a project",
	Long: `Manage the list of skills that are automatically injected into every agent
provisioned in a project.

Injected skills are installed alongside template-declared skills. The union of
hub, user, project, and template skills is resolved at provisioning time.

The optional [project] argument accepts a project name, slug, or UUID.
When omitted, the project is inferred from the current directory's Hub link.

Examples:
  scion project skills list
  scion project skills list my-project
  scion project skills add skill://my-skill
  scion project skills add my-project skill://my-skill --as my-alias --optional
  scion project skills remove <id>
  scion project skills remove my-project skill://my-skill`,
}

// projectSkillsListCmd implements `scion project skills list [project]`.
var projectSkillsListCmd = &cobra.Command{
	Use:     "list [project]",
	Aliases: []string{"ls"},
	Short:   "List injected skills for a project",
	Args:    cobra.MaximumNArgs(1),
	RunE:    runProjectSkillsList,
}

// projectSkillsAddCmd implements `scion project skills add [project] <uri>`.
var projectSkillsAddCmd = &cobra.Command{
	Use:   "add [project] <uri>",
	Short: "Add a skill to the project injected-skills list",
	Long: `Add a skill URI to the project's injected-skills list.

The first argument may be omitted if the project can be inferred from the
current directory's Hub link. When both arguments are present, the first
is the project name/slug/UUID and the second is the skill URI.

Examples:
  scion project skills add skill://my-skill
  scion project skills add my-project skill://my-skill
  scion project skills add my-project skill://my-skill@1.2 --as alias --optional
  scion project skills add --from-directory https://github.com/org/repo/tree/main/skills`,
	Args: cobra.RangeArgs(0, 2),
	RunE: runProjectSkillsAdd,
}

// projectSkillsRemoveCmd implements `scion project skills remove [project] <id|uri>`.
var projectSkillsRemoveCmd = &cobra.Command{
	Use:     "remove [project] <id|uri>",
	Aliases: []string{"rm", "delete"},
	Short:   "Remove a skill from the project injected-skills list",
	Long: `Remove an entry from the project's injected-skills list.

The entry can be identified by its UUID or by the full skill URI.
When a URI is provided, the list is fetched first and the matching entry
is removed.

Examples:
  scion project skills remove <uuid>
  scion project skills remove skill://my-skill
  scion project skills remove my-project <uuid>
  scion project skills remove my-project skill://my-skill`,
	Args: cobra.RangeArgs(1, 2),
	RunE: runProjectSkillsRemove,
}

// Flags shared by add commands.
var (
	projectSkillsAs       string
	projectSkillsOptional bool
	projectSkillsFromDir  string
)

func init() {
	projectCmd.AddCommand(projectSkillsCmd)
	projectSkillsCmd.AddCommand(projectSkillsListCmd)
	projectSkillsCmd.AddCommand(projectSkillsAddCmd)
	projectSkillsCmd.AddCommand(projectSkillsRemoveCmd)

	projectSkillsAddCmd.Flags().StringVar(&projectSkillsAs, "as", "", "Alias for the skill (SkillAs)")
	projectSkillsAddCmd.Flags().BoolVar(&projectSkillsOptional, "optional", false, "Mark the skill as optional (failure does not abort provisioning)")
	projectSkillsAddCmd.Flags().StringVar(&projectSkillsFromDir, "from-directory", "",
		"GitHub directory URL to discover skills from (e.g. https://github.com/org/repo/tree/main/skills)")
}

// resolveProjectSkillsClient resolves a hub client and project ID from an
// optional project name/slug/UUID argument. When arg is empty, the project is
// inferred from the current directory's settings.
func resolveProjectSkillsClient(ctx context.Context, projectArg string) (hubclient.InjectedSkillsService, error) {
	hubCtx, err := CheckHubAvailabilityWithOptions(projectPath, true)
	if err != nil {
		return nil, fmt.Errorf("hub connection required: %w", err)
	}
	if hubCtx == nil {
		return nil, fmt.Errorf("hub is not enabled; configure hub.endpoint to use project skills")
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

	return hubCtx.Client.ProjectInjectedSkills(projectID), nil
}

// splitProjectSkillsArgs parses the optional [project] prefix from args.
// When the first arg looks like a skill URI (contains "://") or a UUID, it is
// treated as the skill reference with no explicit project. Otherwise the first
// arg is the project and the second (if present) is the skill reference.
//
// Returns (projectArg, skillRef). Either may be empty.
func splitProjectSkillsArgs(args []string) (projectArg, skillRef string) {
	if len(args) == 0 {
		return "", ""
	}
	if len(args) == 1 {
		// Heuristic: URIs and UUIDs are skill references, not project names.
		if isSkillURI(args[0]) || isUUIDLike(args[0]) {
			return "", args[0]
		}
		return args[0], ""
	}
	// Two args: first is project, second is the skill/id reference.
	return args[0], args[1]
}

// isSkillURI returns true when s looks like a skill URI (contains "://").
func isSkillURI(s string) bool {
	return strings.Contains(s, "://")
}

// isUUIDLike returns true when s is a standard 36-character UUID
// (8-4-4-4-12 with dashes). The length check is required because
// uuid.Parse also accepts URN ("urn:uuid:…", 45 chars), braced
// ("{uuid}", 38 chars), and 32-char hex-without-dashes forms that
// must not be treated as entry IDs.
func isUUIDLike(s string) bool {
	if len(s) != 36 {
		return false
	}
	_, err := uuid.Parse(s)
	return err == nil
}

func runProjectSkillsList(cmd *cobra.Command, args []string) error {
	projectArg := ""
	if len(args) == 1 {
		projectArg = args[0]
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	svc, err := resolveProjectSkillsClient(ctx, projectArg)
	if err != nil {
		return err
	}

	list, err := svc.List(ctx)
	if err != nil {
		return fmt.Errorf("failed to list project injected skills: %w", err)
	}

	if isJSONOutput() {
		return outputJSON(list)
	}

	if len(list.Entries) == 0 {
		fmt.Println("No injected skills configured for this project.")
		return nil
	}

	printSkillInjectionTable(list.Entries)
	return nil
}

func runProjectSkillsAdd(cmd *cobra.Command, args []string) error {
	if projectSkillsFromDir != "" {
		if projectSkillsAs != "" || projectSkillsOptional {
			return fmt.Errorf("--as and --optional cannot be used with --from-directory (they apply only to single-skill add)")
		}
		projectArg, skillRef := splitProjectSkillsArgs(args)
		if skillRef != "" {
			return fmt.Errorf("cannot combine a skill URI argument with --from-directory; choose one")
		}
		return runProjectSkillsFromDirectory(cmd, projectArg, projectSkillsFromDir)
	}

	if len(args) == 0 {
		return fmt.Errorf("skill URI or --from-directory is required")
	}

	projectArg, skillURI := splitProjectSkillsArgs(args)
	if skillURI == "" {
		return fmt.Errorf("skill URI is required (expected format containing ://), got %q", args[0])
	}

	normalized, err := api.NormalizeSkillURI(skillURI)
	if err != nil {
		return fmt.Errorf("invalid skill URI: %w", err)
	}
	if normalized != skillURI {
		fmt.Fprintf(cmd.ErrOrStderr(), "Note: URI transformed → %s\n", normalized)
	}
	skillURI = normalized

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	svc, err := resolveProjectSkillsClient(ctx, projectArg)
	if err != nil {
		return err
	}

	as := projectSkillsAs
	optional := projectSkillsOptional

	entry, err := svc.Add(ctx, &hubclient.AddInjectedSkillRequest{
		SkillURI: skillURI,
		SkillAs:  as,
		Optional: optional,
	})
	if err != nil {
		if apiclient.IsUnauthorizedError(err) {
			return fmt.Errorf("not authorized to modify this project's injected skills")
		}
		return fmt.Errorf("failed to add injected skill: %w", err)
	}

	if isJSONOutput() {
		return outputJSON(entry)
	}

	fmt.Printf("Added injected skill (ID: %s)\n", entry.ID)
	return nil
}

func runProjectSkillsRemove(cmd *cobra.Command, args []string) error {
	projectArg, skillRef := splitProjectSkillsArgs(args)
	if skillRef == "" {
		return fmt.Errorf("skill ID or URI is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	svc, err := resolveProjectSkillsClient(ctx, projectArg)
	if err != nil {
		return err
	}

	entryID, err := resolveInjectedSkillEntryID(ctx, svc, skillRef)
	if err != nil {
		return err
	}

	if err := svc.Remove(ctx, entryID); err != nil {
		if apiclient.IsUnauthorizedError(err) {
			return fmt.Errorf("not authorized to modify this project's injected skills")
		}
		return fmt.Errorf("failed to remove injected skill: %w", err)
	}

	if isJSONOutput() {
		return outputJSON(map[string]string{"removed": entryID})
	}

	fmt.Printf("Removed injected skill (ID: %s)\n", entryID)
	return nil
}

// resolveInjectedSkillEntryID resolves a skill reference to an entry UUID.
// If ref is already a UUID it is returned as-is. If it looks like a skill URI,
// the list is fetched and the first matching entry ID is returned.
func resolveInjectedSkillEntryID(ctx context.Context, svc hubclient.InjectedSkillsService, ref string) (string, error) {
	// If it is a URI, resolve by listing and matching.
	if isSkillURI(ref) {
		list, err := svc.List(ctx)
		if err != nil {
			return "", fmt.Errorf("failed to list entries to resolve URI: %w", err)
		}
		for _, e := range list.Entries {
			if e.SkillURI == ref {
				return e.ID, nil
			}
		}
		return "", fmt.Errorf("no injected skill with URI %q found", ref)
	}
	// Otherwise it must be a UUID; reject anything else to avoid a silent misrouted path segment.
	if !isUUIDLike(ref) {
		return "", fmt.Errorf("invalid skill entry ID: %q (expected UUID or skill URI)", ref)
	}
	return ref, nil
}

// printSkillInjectionTable prints a tab-formatted table of skill injection entries.
func printSkillInjectionTable(entries []api.SkillInjectionEntry) {
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tSKILL URI\tALIAS\tOPTIONAL\tORDER\tNAME")
	for _, e := range entries {
		alias := e.SkillAs
		if alias == "" {
			alias = "-"
		}
		optional := "no"
		if e.Optional {
			optional = "yes"
		}
		name := e.SkillName
		if name == "" && e.SkillSlug != "" {
			name = e.SkillSlug
		}
		if name == "" {
			name = "-"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\n",
			e.ID, e.SkillURI, alias, optional, e.SortOrder, name)
	}
	_ = tw.Flush()
}

// looksLikeGitHubDirectoryURL returns true if s appears to be a GitHub
// directory URL of the form https://github.com/org/repo/tree/<ref>/...
// This is a client-side pre-check; the server validates authoritatively.
func looksLikeGitHubDirectoryURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	return u.Scheme == "https" &&
		strings.EqualFold(u.Host, "github.com") &&
		strings.Contains(u.Path, "/tree/")
}

func runProjectSkillsFromDirectory(cmd *cobra.Command, projectArg, dirURL string) error {
	// Fix 1: Strip userinfo (e.g. token:secret@) before sending.
	if parsed, err := url.Parse(dirURL); err == nil && parsed.User != nil {
		parsed.User = nil
		dirURL = parsed.String()
	}

	// Fix 2: Client-side URL validation.
	if !looksLikeGitHubDirectoryURL(dirURL) {
		return fmt.Errorf("--from-directory must be an https://github.com/.../tree/<ref>/... URL")
	}

	// Fix 4: Split nil-error check.
	hubCtx, err := CheckHubAvailabilityWithOptions(projectPath, true)
	if err != nil {
		return fmt.Errorf("hub connection required: %w", err)
	}
	if hubCtx == nil {
		return fmt.Errorf("hub is not enabled; configure hub.endpoint to use project skills")
	}

	// Resolve project ID for auth token forwarding.
	// Fix 3: Use separate context for discover phase (30s).
	parentCtx := cmd.Context()
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	discoverCtx, discoverCancel := context.WithTimeout(parentCtx, 30*time.Second)
	defer discoverCancel()

	var projectID string
	if projectArg != "" {
		proj, err := resolveProjectByNameOrID(discoverCtx, hubCtx.Client, projectArg)
		if err != nil {
			return fmt.Errorf("could not resolve project %q: %w", projectArg, err)
		}
		projectID = proj.ID
	} else {
		projectID, err = GetProjectID(hubCtx)
		if err != nil {
			return fmt.Errorf("could not determine project ID: %w", err)
		}
	}

	// Discover skills.
	result, err := hubCtx.Client.DiscoverSkillsDirectory(discoverCtx, hubclient.DiscoverSkillsDirectoryRequest{
		SourceURL: dirURL,
		ProjectID: projectID,
	})
	if err != nil {
		return fmt.Errorf("skill discovery failed: %w", err)
	}
	if len(result.Skills) == 0 {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "No skills found at the given URL.")
		return nil
	}

	// Print discovered skills.
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Discovered %d skill(s):\n", len(result.Skills))
	for _, s := range result.Skills {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  %s  (%s)\n", s.URI, s.Name)
	}
	if len(result.Skipped) > 0 {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  (%d folder(s) skipped: not recognized as skills)\n", len(result.Skipped))
	}

	// TTY: prompt unless --yes/--non-interactive.
	if isInteractiveTerminal() && !autoConfirm {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Add all %d skill(s)? [Y/n] ", len(result.Skills))
		var answer string
		_, _ = fmt.Fscanln(cmd.InOrStdin(), &answer)
		if answer != "" && strings.ToLower(answer) != "y" && strings.ToLower(answer) != "yes" {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Aborted.")
			return nil
		}
	}

	// Fix 3: Fresh timeout for add phase (60s), starts after the user responds.
	addCtx, addCancel := context.WithTimeout(parentCtx, 60*time.Second)
	defer addCancel()

	// Add each skill.
	svc := hubCtx.Client.ProjectInjectedSkills(projectID)
	var addErrors []string
	for _, s := range result.Skills {
		_, err := svc.Add(addCtx, &hubclient.AddInjectedSkillRequest{SkillURI: s.URI})
		if err != nil {
			addErrors = append(addErrors, fmt.Sprintf("%s: %v", s.Name, err))
		}
	}
	if len(addErrors) > 0 {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Warning: %d of %d skills could not be added:\n", len(addErrors), len(result.Skills))
		for _, e := range addErrors {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "  %s\n", e)
		}
	}
	added := len(result.Skills) - len(addErrors)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Added %d of %d skill(s).\n", added, len(result.Skills))

	// Fix 6: Return error when all adds fail.
	if added == 0 && len(result.Skills) > 0 {
		return fmt.Errorf("all %d skill(s) failed to add", len(result.Skills))
	}

	return nil
}
