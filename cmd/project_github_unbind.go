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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

var projectGitHubUnbindCmd = &cobra.Command{
	Use:   "unbind",
	Short: "Remove the GitHub App installation association from the project",
	Long:  `Remove the GitHub App installation association and any hub-admin binding from the current project.`,
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		client, projectID, err := resolveProjectForGitHub()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
		defer cancel()
		return runProjectGitHubUnbind(ctx, client.Projects(), projectID, cmd.OutOrStdout())
	},
}

func runProjectGitHubUnbind(ctx context.Context, projects hubclient.ProjectService, projectID string, out io.Writer) error {
	if err := projects.UnbindGitHubInstallation(ctx, projectID); err != nil {
		return wrapHubError(err)
	}

	if isJSONOutput() {
		return outputJSON(ActionResult{
			Status:  "success",
			Command: "project github unbind",
			Message: fmt.Sprintf("Removed GitHub App installation from project %s.", projectID),
			Details: map[string]interface{}{
				"project_id": projectID,
			},
		})
	}

	_, _ = fmt.Fprintf(out, "Removed GitHub App installation from project %s.\n", projectID)
	return nil
}
