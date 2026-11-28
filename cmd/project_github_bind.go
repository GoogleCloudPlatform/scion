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
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

var projectGitHubBindInstallationID int64

var projectGitHubBindCmd = &cobra.Command{
	Use:   "bind",
	Short: "Bind a GitHub App installation to the project (hub admin)",
	Long: `Bind a GitHub App installation to the current project as a hub administrator.

When --installation is omitted, scion queries the Hub's installations and
auto-selects the single active installation whose account matches the
project's GitHub remote owner. If zero or multiple active installations match,
scion exits with an error listing candidates.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		client, projectID, err := resolveProjectForGitHub()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
		defer cancel()
		return runProjectGitHubBind(ctx, client.Projects(), client.GitHubApp(), projectID, projectGitHubBindInstallationID, cmd.OutOrStdout())
	},
}

func runProjectGitHubBind(
	ctx context.Context,
	projects hubclient.ProjectService,
	githubApp hubclient.GitHubAppService,
	projectID string,
	installationID int64,
	out io.Writer,
) error {
	targetID := installationID
	accountLogin := ""

	if targetID <= 0 {
		info, err := projects.GetGitHub(ctx, projectID)
		if err != nil {
			return wrapHubError(err)
		}
		if !info.Remote.OnAppHost || info.Remote.Owner == "" {
			return fmt.Errorf("project git remote is not a repository on the configured GitHub host (%s)", info.App.WebHost)
		}

		installations, err := githubApp.ListInstallations(ctx, &hubclient.ListGitHubInstallationsOptions{
			Status: "active",
		})
		if err != nil {
			return wrapHubError(err)
		}

		candidates := filterMatchingActiveInstallations(installations, info.Remote.Owner)
		if len(candidates) == 0 {
			return fmt.Errorf("no active GitHub App installation found matching owner %q; install the GitHub App on %q or pass --installation <id>", info.Remote.Owner, info.Remote.Owner)
		}
		if len(candidates) > 1 {
			var lines []string
			for _, c := range candidates {
				lines = append(lines, fmt.Sprintf("  - %d (%s, %s)", c.InstallationID, stripControlChars(c.AccountLogin), stripControlChars(c.AccountType)))
			}
			return fmt.Errorf("multiple active GitHub App installations match owner %q; pass --installation <id> to choose one:\n%s", info.Remote.Owner, strings.Join(lines, "\n"))
		}
		targetID = candidates[0].InstallationID
		accountLogin = candidates[0].AccountLogin
	}

	resp, err := projects.BindGitHubInstallation(ctx, projectID, targetID)
	if err != nil {
		return wrapHubError(err)
	}

	if isJSONOutput() {
		return outputJSON(resp)
	}

	if accountLogin != "" {
		_, _ = fmt.Fprintf(out, "Bound GitHub App installation %s (#%d) to project %s.\n", stripControlChars(accountLogin), resp.InstallationID, resp.ProjectID)
	} else {
		_, _ = fmt.Fprintf(out, "Bound GitHub App installation #%d to project %s.\n", resp.InstallationID, resp.ProjectID)
	}
	return nil
}

func init() {
	projectGitHubBindCmd.Flags().Int64Var(&projectGitHubBindInstallationID, "installation", 0, "GitHub App installation ID to bind (auto-selected if omitted)")
}
