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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

// userCmd is the parent command for `scion user` operations.
var userCmd = &cobra.Command{
	Use:   "user",
	Short: "Manage user settings",
	Long:  `Commands for managing per-user Hub settings such as injected skills.`,
}

// userSkillsCmd is the parent group for `scion user skills` subcommands.
var userSkillsCmd = &cobra.Command{
	Use:   "skills",
	Short: "Manage injected skills for the current user",
	Long: `Manage the list of skills that are automatically injected into every agent
you provision, regardless of the project.

User-scope injected skills are applied after hub-scope skills but before
project-scope skills (template > project > user > hub).

Examples:
  scion user skills list
  scion user skills add skill://my-skill
  scion user skills add skill://my-skill@1.2 --as alias --optional
  scion user skills remove <id>
  scion user skills remove skill://my-skill`,
}

// userSkillsListCmd implements `scion user skills list`.
var userSkillsListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List injected skills for the current user",
	Args:    cobra.NoArgs,
	RunE:    runUserSkillsList,
}

// userSkillsAddCmd implements `scion user skills add <uri>`.
var userSkillsAddCmd = &cobra.Command{
	Use:   "add <uri>",
	Short: "Add a skill to your injected-skills list",
	Long: `Add a skill URI to your personal injected-skills list.

Examples:
  scion user skills add skill://my-skill
  scion user skills add skill://my-skill@1.2 --as alias --optional
  scion user skills add --from-directory https://github.com/org/repo/tree/main/skills`,
	Args: cobra.RangeArgs(0, 1),
	RunE: runUserSkillsAdd,
}

// userSkillsRemoveCmd implements `scion user skills remove <id|uri>`.
var userSkillsRemoveCmd = &cobra.Command{
	Use:     "remove <id|uri>",
	Aliases: []string{"rm", "delete"},
	Short:   "Remove a skill from your injected-skills list",
	Long: `Remove an entry from your personal injected-skills list.

The entry can be identified by its UUID or by the full skill URI.

Examples:
  scion user skills remove <uuid>
  scion user skills remove skill://my-skill`,
	Args: cobra.ExactArgs(1),
	RunE: runUserSkillsRemove,
}

// Flags for user skills add command.
var (
	userSkillsAs       string
	userSkillsOptional bool
	userSkillsFromDir  string
)

func init() {
	rootCmd.AddCommand(userCmd)
	userCmd.AddCommand(userSkillsCmd)
	userSkillsCmd.AddCommand(userSkillsListCmd)
	userSkillsCmd.AddCommand(userSkillsAddCmd)
	userSkillsCmd.AddCommand(userSkillsRemoveCmd)

	userSkillsAddCmd.Flags().StringVar(&userSkillsAs, "as", "", "Alias for the skill (SkillAs)")
	userSkillsAddCmd.Flags().BoolVar(&userSkillsOptional, "optional", false, "Mark the skill as optional (failure does not abort provisioning)")
	userSkillsAddCmd.Flags().StringVar(&userSkillsFromDir, "from-directory", "",
		"GitHub directory URL to discover skills from")
}

// resolveUserSkillsService returns an InjectedSkillsService for the current user.
func resolveUserSkillsService() (hubclient.InjectedSkillsService, error) {
	hubCtx, err := CheckHubAvailabilityWithOptions(projectPath, true)
	if err != nil {
		return nil, fmt.Errorf("hub connection required: %w", err)
	}
	if hubCtx == nil {
		return nil, fmt.Errorf("hub is not enabled; configure hub.endpoint to use user skills")
	}
	return hubCtx.Client.UserInjectedSkills(), nil
}

func runUserSkillsList(cmd *cobra.Command, args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	svc, err := resolveUserSkillsService()
	if err != nil {
		return err
	}

	list, err := svc.List(ctx)
	if err != nil {
		return fmt.Errorf("failed to list user injected skills: %w", err)
	}

	if isJSONOutput() {
		return outputJSON(list)
	}

	if len(list.Entries) == 0 {
		fmt.Println("No injected skills configured for your account.")
		return nil
	}

	printSkillInjectionTable(list.Entries)
	return nil
}

func runUserSkillsAdd(cmd *cobra.Command, args []string) error {
	if userSkillsFromDir != "" {
		if userSkillsAs != "" || userSkillsOptional {
			return fmt.Errorf("--as and --optional cannot be used with --from-directory (they apply only to single-skill add)")
		}
		if len(args) > 0 {
			return fmt.Errorf("cannot combine a skill URI argument with --from-directory; choose one")
		}
		return runUserSkillsFromDirectory(cmd, userSkillsFromDir)
	}

	if len(args) == 0 {
		return fmt.Errorf("skill URI or --from-directory is required")
	}

	skillURI := args[0]

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

	svc, err := resolveUserSkillsService()
	if err != nil {
		return err
	}

	as := userSkillsAs
	optional := userSkillsOptional

	entry, err := svc.Add(ctx, &hubclient.AddInjectedSkillRequest{
		SkillURI: skillURI,
		SkillAs:  as,
		Optional: optional,
	})
	if err != nil {
		if apiclient.IsUnauthorizedError(err) {
			return fmt.Errorf("not authorized to modify user injected skills")
		}
		return fmt.Errorf("failed to add user injected skill: %w", err)
	}

	if isJSONOutput() {
		return outputJSON(entry)
	}

	fmt.Printf("Added injected skill (ID: %s)\n", entry.ID)
	return nil
}

func runUserSkillsRemove(cmd *cobra.Command, args []string) error {
	skillRef := args[0]

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	svc, err := resolveUserSkillsService()
	if err != nil {
		return err
	}

	entryID, err := resolveInjectedSkillEntryID(ctx, svc, skillRef)
	if err != nil {
		return err
	}

	if err := svc.Remove(ctx, entryID); err != nil {
		if apiclient.IsUnauthorizedError(err) {
			return fmt.Errorf("not authorized to modify user injected skills")
		}
		return fmt.Errorf("failed to remove user injected skill: %w", err)
	}

	if isJSONOutput() {
		return outputJSON(map[string]string{"removed": entryID})
	}

	fmt.Printf("Removed injected skill (ID: %s)\n", entryID)
	return nil
}

func runUserSkillsFromDirectory(cmd *cobra.Command, dirURL string) error {
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
		return fmt.Errorf("hub is not enabled; configure hub.endpoint to use user skills")
	}

	// Fix 3: Separate context for discover phase (30s).
	parentCtx := cmd.Context()
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	discoverCtx, discoverCancel := context.WithTimeout(parentCtx, 30*time.Second)
	defer discoverCancel()

	result, err := hubCtx.Client.DiscoverSkillsDirectory(discoverCtx, hubclient.DiscoverSkillsDirectoryRequest{
		SourceURL: dirURL,
		// No ProjectID for user scope.
	})
	if err != nil {
		return fmt.Errorf("skill discovery failed: %w", err)
	}
	if len(result.Skills) == 0 {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "No skills found at the given URL.")
		return nil
	}

	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Discovered %d skill(s):\n", len(result.Skills))
	for _, s := range result.Skills {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  %s  (%s)\n", s.URI, s.Name)
	}
	if len(result.Skipped) > 0 {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  (%d folder(s) skipped)\n", len(result.Skipped))
	}

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

	svc := hubCtx.Client.UserInjectedSkills()
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
