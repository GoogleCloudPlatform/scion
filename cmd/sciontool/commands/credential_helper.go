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

package commands

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

// credentialHelperCmd implements a git credential helper that returns fresh
// GitHub tokens. When GitHub App token refresh is enabled, it reads the token
// from the refreshable token file (written by the background refresh loop).
// If the token file is missing or stale, it attempts an on-demand refresh
// from the Hub.
//
// Usage in git config:
//
//	git config credential.helper '!sciontool credential-helper'
var credentialHelperCmd = &cobra.Command{
	Use:    "credential-helper",
	Short:  "Git credential helper for GitHub App tokens",
	Hidden: true,
	Run: func(cmd *cobra.Command, args []string) {
		runCredentialHelper()
	},
}

func init() {
	rootCmd.AddCommand(credentialHelperCmd)
}

func runCredentialHelper() {
	// If GitHub App is not enabled, fall back to GITHUB_TOKEN env var
	if !hub.IsGitHubAppEnabled() {
		token := os.Getenv("GITHUB_TOKEN")
		if token != "" {
			fmt.Printf("username=oauth2\npassword=%s\n", token)
		}
		return
	}

	// Try reading from the token file first (fastest path)
	tokenPath := hub.GitHubTokenPath()
	token := hub.ReadGitHubTokenFile(tokenPath)

	// Check whether the cached token is expired. If so, discard it and
	// force an on-demand refresh from the Hub rather than returning a
	// stale token that will be rejected by GitHub.
	if token != "" && hub.IsGitHubTokenExpired(tokenPath) {
		log.Debug("credential-helper: cached token is expired, forcing on-demand refresh")
		token = ""
	}

	// If no valid token in file, try on-demand refresh from Hub
	if token == "" {
		hubClient := hub.NewClient()
		if hubClient != nil && hubClient.IsConfigured() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			newToken, newExpiry, err := hubClient.RefreshGitHubToken(ctx)
			if err != nil {
				log.Error("credential-helper: on-demand GitHub token refresh failed: %v", err)
			} else {
				token = newToken
				// Write the refreshed token and expiry to the file for future use.
				// Writing the expiry prevents the next credential-helper invocation
				// (git often calls twice per push) from redundantly refreshing.
				if writeErr := hub.WriteGitHubTokenFile(tokenPath, newToken); writeErr != nil {
					log.Error("credential-helper: failed to write token file: %v", writeErr)
				}
				if expiryErr := hub.WriteGitHubTokenExpiry(tokenPath, newExpiry); expiryErr != nil {
					log.Error("credential-helper: failed to write token expiry file: %v", expiryErr)
				}
				_ = os.Setenv("GITHUB_TOKEN", newToken)
				log.Debug("credential-helper: on-demand refresh succeeded, expires: %s", newExpiry.Format(time.RFC3339))
			}
		}
	}

	// Fall back to GITHUB_TOKEN env var if still empty
	if token == "" {
		token = os.Getenv("GITHUB_TOKEN")
	}

	if token != "" {
		fmt.Printf("username=oauth2\npassword=%s\n", token)
	}
}
