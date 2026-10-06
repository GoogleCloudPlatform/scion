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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

// Join token lifetime bounds accepted by the hub (hub.MinJoinTokenTTLSeconds
// and hub.MaxJoinTokenTTLSeconds). They are checked here too so that an
// out-of-range --ttl fails before any request is sent.
const (
	minBrokerJoinTokenTTL = 5 * time.Minute
	maxBrokerJoinTokenTTL = 24 * time.Hour
)

var (
	hubBrokersJoinTokenTTL  time.Duration
	hubBrokersJoinTokenJSON bool
)

var hubBrokersJoinTokenCmd = &cobra.Command{
	Use:   "join-token",
	Short: "Manage broker join tokens",
	Long:  `Manage join tokens used to connect a broker host to the Hub without a Hub user credential on that host.`,
}

var hubBrokersJoinTokenCreateCmd = &cobra.Command{
	Use:   "create <broker-name>",
	Short: "Create a broker and a join token for it",
	Long: `Create a join token for the named broker.

If no broker with this name exists, the broker is created and you become its
owner. If a broker with this name exists, its owner or a super-admin can issue
a new token for it; anyone else is refused. A new token makes any earlier
unused token for that broker stop working. The broker's settings
(auto-provide, labels, GCP host identity) are never changed, and the broker is
not added to any project.

Redeeming a token replaces the broker's credentials. If a host has already
joined as this broker, it is disconnected once the new token is used.

The token is single use. Run 'scion runtime-broker join' on the broker host
with the token in SCION_BROKER_JOIN_TOKEN to finish the connection.

The token is printed on stdout and the instructions on stderr, so
  TOKEN=$(scion hub brokers join-token create my-broker 2>/dev/null)
captures only the token.

Requires the broker.create permission (hub member or admin). Minting with a
user access token is not supported; sign in with 'scion hub auth login'.

Examples:
  # Create a token valid for the default lifetime (1h)
  scion hub brokers join-token create build-host-3

  # Create a token valid for 30 minutes, as JSON
  scion hub brokers join-token create build-host-3 --ttl 30m --json`,
	Args: cobra.ExactArgs(1),
	RunE: runHubBrokersJoinTokenCreate,
}

func init() {
	hubBrokersCmd.AddCommand(hubBrokersJoinTokenCmd)
	hubBrokersJoinTokenCmd.AddCommand(hubBrokersJoinTokenCreateCmd)

	hubBrokersJoinTokenCreateCmd.Flags().DurationVar(&hubBrokersJoinTokenTTL, "ttl", 0, "Token lifetime, between 5m and 24h (default: the hub default, 1h)")
	hubBrokersJoinTokenCreateCmd.Flags().BoolVar(&hubBrokersJoinTokenJSON, "json", false, "Output in JSON format")
}

// brokerJoinTokenOutput is the --json output of 'hub brokers join-token create'.
type brokerJoinTokenOutput struct {
	BrokerID    string `json:"brokerId"`
	BrokerName  string `json:"brokerName"`
	JoinToken   string `json:"joinToken"`
	ExpiresAt   string `json:"expiresAt"`
	HubEndpoint string `json:"hubEndpoint"`
	Reissued    bool   `json:"reissued"`
}

// brokerJoinTokenTTLSeconds converts --ttl to the request value: 0 when
// unset (the hub default), else whole seconds within the accepted range.
func brokerJoinTokenTTLSeconds(ttl time.Duration) (int, error) {
	if ttl == 0 {
		return 0, nil
	}
	if ttl < minBrokerJoinTokenTTL || ttl > maxBrokerJoinTokenTTL {
		return 0, fmt.Errorf("--ttl must be between %s and %s", minBrokerJoinTokenTTL, maxBrokerJoinTokenTTL)
	}
	return int(ttl / time.Second), nil
}

func runHubBrokersJoinTokenCreate(cmd *cobra.Command, args []string) error {
	brokerName := args[0]
	ttlSeconds, err := brokerJoinTokenTTLSeconds(hubBrokersJoinTokenTTL)
	if err != nil {
		return err
	}

	resolvedPath, _, err := config.ResolveProjectPath(projectPath)
	if err != nil {
		return fmt.Errorf("failed to resolve project path: %w", err)
	}
	settings, err := config.LoadSettings(resolvedPath)
	if err != nil {
		return fmt.Errorf("failed to load settings: %w", err)
	}
	endpoint := GetHubEndpoint(settings)

	client, err := getHubClient(settings)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := client.RuntimeBrokers().Create(ctx, &hubclient.CreateBrokerRequest{
		Name:                brokerName,
		JoinTokenTTLSeconds: ttlSeconds,
		PreserveSettings:    true,
		Labels: map[string]string{
			"scion.io/broker-role": "remote",
		},
	})
	if err != nil {
		return fmt.Errorf("failed to create join token: %w", err)
	}

	out := brokerJoinTokenOutput{
		BrokerID:    resp.BrokerID,
		BrokerName:  brokerName,
		JoinToken:   resp.JoinToken,
		ExpiresAt:   resp.ExpiresAt,
		HubEndpoint: endpoint,
		Reissued:    resp.Reissued,
	}
	return writeBrokerJoinTokenOutput(cmd.OutOrStdout(), cmd.ErrOrStderr(), out, hubBrokersJoinTokenJSON)
}

// writeBrokerJoinTokenOutput prints the token to stdout, alone on a line,
// and the instructions to stderr. With asJSON, stdout carries a single JSON
// object and nothing is written to stderr.
func writeBrokerJoinTokenOutput(stdout, stderr io.Writer, out brokerJoinTokenOutput, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	_, _ = fmt.Fprintf(stderr, "Join token for broker '%s' (ID %s) expires %s (single use).\n", out.BrokerName, out.BrokerID, out.ExpiresAt)
	if out.Reissued {
		_, _ = fmt.Fprintln(stderr, "The previous unused join token for this broker no longer works.")
	}
	_, _ = fmt.Fprintln(stderr, "On the host run:")
	_, _ = fmt.Fprintf(stderr, "  SCION_HUB_ENDPOINT=%s SCION_BROKER_JOIN_TOKEN=<token> scion runtime-broker join --broker-id %s\n", out.HubEndpoint, out.BrokerID)
	_, err := fmt.Fprintln(stdout, out.JoinToken)
	return err
}
