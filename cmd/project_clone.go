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

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

var (
	cloneProjectName      string
	cloneProjectSlug      string
	cloneAsTemplate       bool
	cloneProjectGitRemote string
)

var projectCloneCmd = &cobra.Command{
	Use:   "clone <source-slug-or-id>",
	Short: "Clone a project's configuration into a new project",
	Long: `Clone a project by creating a new project pre-populated with the source
project's configuration.

The <source-slug-or-id> argument identifies the project to clone (name, slug,
or UUID).

Copies: settings, labels, environment variables, injected skills, pre-start
hook, project harness configs and templates.

Does not copy: secrets, agents, history, or chat integrations.

The new project gets a fresh ID, the caller as owner, and private visibility.
If --name is omitted, defaults to "<source-name> copy".

Requires Hub connectivity.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		sourceRef := args[0]

		resolvedPath, _, err := config.ResolveProjectPath(projectPath)
		if err != nil {
			return fmt.Errorf("failed to resolve project path: %w", err)
		}

		settings, err := config.LoadSettings(resolvedPath)
		if err != nil {
			return fmt.Errorf("failed to load settings: %w", err)
		}

		client, err := getHubClient(settings)
		if err != nil {
			return err
		}

		ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
		defer cancel()

		source, err := resolveProjectByNameOrID(ctx, client, sourceRef)
		if err != nil {
			return fmt.Errorf("failed to find project: %w", err)
		}

		name := cloneProjectName
		if name == "" {
			name = source.Name + " copy"
		}

		cloned, err := client.Projects().Clone(ctx, source.ID, hubclient.CloneProjectRequest{
			Name:       name,
			Slug:       cloneProjectSlug,
			AsTemplate: cloneAsTemplate,
			GitRemote:  cloneProjectGitRemote,
		})
		if err != nil {
			return fmt.Errorf("failed to clone project: %w", err)
		}

		if isJSONOutput() {
			return outputJSON(ActionResult{
				Status:  "success",
				Command: "project clone",
				Message: fmt.Sprintf("Cloned project %q → %q", source.Name, cloned.Name),
				Details: map[string]interface{}{
					"id":        cloned.ID,
					"name":      cloned.Name,
					"slug":      cloned.Slug,
					"source_id": source.ID,
				},
			})
		}

		fmt.Printf("Cloned project %q → %q (slug: %s)\n", source.Name, cloned.Name, cloned.Slug)
		return nil
	},
}

func init() {
	projectCloneCmd.Flags().StringVar(&cloneProjectName, "name", "", "Name for the cloned project (default: \"<source> copy\")")
	projectCloneCmd.Flags().StringVar(&cloneProjectSlug, "slug", "", "Explicit slug for the cloned project (default: auto-generated from name)")
	projectCloneCmd.Flags().BoolVar(&cloneAsTemplate, "as-template", false, "Mark the cloned project as a template (admin-only)")
	projectCloneCmd.Flags().StringVar(&cloneProjectGitRemote, "git-remote", "", "Override the git remote URL for the cloned project")
	projectCmd.AddCommand(projectCloneCmd)
}
