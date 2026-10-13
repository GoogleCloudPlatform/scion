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
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

var (
	hubAuthFederationTokenAudience string
	hubAuthFederationTokenJSON     bool
)

// hubAuthFederationTokenCmd obtains a user access credential for a peer hub. It is a
// human-mode command: hub.auth is not in agentAllowed (cmd/cli_mode.go).
var hubAuthFederationTokenCmd = &cobra.Command{
	Use:   "federation-token --audience URL",
	Short: "Obtain a short-lived user access credential for a peer hub",
	Long: `Obtain a short-lived user access credential that names you to a peer hub,
and print it.

The Hub signs the credential with its OIDC key for the one audience given
(the peer hub's issuer URL). The audience must be listed in the Hub's
server.oidc.user_token_audiences setting, and the credential expires after
server.oidc.user_token_lifetime (15 minutes by default). A user access token
used for this command needs the federation_token:mint scope on a hub
boundary.

Experimental: the Hub answers only while the federation.project_visitors
experiment is on.

By default only the credential is printed, so it can be captured in a
variable or piped to a client. Use --json for the credential and its expiry.

Example:
  scion hub auth federation-token --audience https://peer-hub.example.com`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		_, client, err := loadHubClient()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return runHubAuthFederationToken(ctx, client.Auth(), hubAuthFederationTokenAudience, hubAuthFederationTokenJSON, cmd.OutOrStdout())
	},
}

func init() {
	hubAuthCmd.AddCommand(hubAuthFederationTokenCmd)
	hubAuthFederationTokenCmd.Flags().StringVar(&hubAuthFederationTokenAudience, "audience", "", "Issuer URL of the peer hub the credential is for (required)")
	hubAuthFederationTokenCmd.Flags().BoolVar(&hubAuthFederationTokenJSON, "json", false, "Output the credential and its expiry as JSON")
	_ = hubAuthFederationTokenCmd.MarkFlagRequired("audience")
}

// runHubAuthFederationToken requests a user access credential for the peer hub at audience
// and writes it to out: the bare token followed by a newline, or with
// jsonOut the token and its expiry as JSON.
func runHubAuthFederationToken(ctx context.Context, auth hubclient.AuthService, audience string, jsonOut bool, out io.Writer) error {
	audience = strings.TrimSpace(audience)
	if audience == "" {
		return newUsageError("--audience must name the peer hub's issuer URL")
	}
	resp, err := auth.FederationToken(ctx, &hubclient.FederationTokenRequest{Audience: audience})
	if err != nil {
		return fmt.Errorf("failed to obtain federation token: %w", err)
	}
	if jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(resp)
	}
	_, err = fmt.Fprintln(out, resp.Token)
	return err
}
