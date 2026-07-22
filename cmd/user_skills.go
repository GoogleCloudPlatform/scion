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
	"time"

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
  scion user skills add scion://my-skill
  scion user skills add scion://my-skill@1.2 --as alias --optional
  scion user skills remove <id>
  scion user skills remove scion://my-skill`,
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
  scion user skills add scion://my-skill
  scion user skills add scion://my-skill@1.2 --as alias --optional`,
	Args: cobra.ExactArgs(1),
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
  scion user skills remove scion://my-skill`,
	Args: cobra.ExactArgs(1),
	RunE: runUserSkillsRemove,
}

// Flags for user skills add command.
var (
	userSkillsAs       string
	userSkillsOptional bool
)

func init() {
	rootCmd.AddCommand(userCmd)
	userCmd.AddCommand(userSkillsCmd)
	userSkillsCmd.AddCommand(userSkillsListCmd)
	userSkillsCmd.AddCommand(userSkillsAddCmd)
	userSkillsCmd.AddCommand(userSkillsRemoveCmd)

	userSkillsAddCmd.Flags().StringVar(&userSkillsAs, "as", "", "Alias for the skill (SkillAs)")
	userSkillsAddCmd.Flags().BoolVar(&userSkillsOptional, "optional", false, "Mark the skill as optional (failure does not abort provisioning)")
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
	skillURI := args[0]

	if !isSkillURI(skillURI) {
		return fmt.Errorf("skill URI is required (expected format containing ://), got %q", skillURI)
	}

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
