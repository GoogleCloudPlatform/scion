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
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

// resolveProjectForGitHub returns the Hub client and resolved project ID for
// 'scion project github' subcommands. Overridden in tests.
var resolveProjectForGitHub = resolveLinkedProjectForSA

var projectGitHubCmd = &cobra.Command{
	Use:   "github",
	Short: "Inspect and manage the project's GitHub App integration",
	Long: `Inspect and manage the GitHub App installation and effective token
permissions for the current project on the Hub.

Examples:
  scion project github status
  scion project github bind
  scion project github bind --installation 12345
  scion project github unbind`,
}

// filterMatchingActiveInstallations returns active GitHub App installations
// whose account_login matches owner (case-insensitively).
func filterMatchingActiveInstallations(installations []hubclient.GitHubInstallation, owner string) []hubclient.GitHubInstallation {
	var matched []hubclient.GitHubInstallation
	for _, inst := range installations {
		if inst.Status != "active" {
			continue
		}
		if inst.AccountLogin != "" && strings.EqualFold(inst.AccountLogin, owner) {
			matched = append(matched, inst)
		}
	}
	return matched
}

func init() {
	projectCmd.AddCommand(projectGitHubCmd)
	projectGitHubCmd.AddCommand(projectGitHubStatusCmd)
	projectGitHubCmd.AddCommand(projectGitHubBindCmd)
	projectGitHubCmd.AddCommand(projectGitHubUnbindCmd)
}
