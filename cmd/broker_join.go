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
	"os"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth/adcsource"
	"github.com/GoogleCloudPlatform/scion/pkg/version"
	"github.com/spf13/cobra"
)

const (
	// envBrokerJoinToken holds the join token for 'runtime-broker join'.
	envBrokerJoinToken = "SCION_BROKER_JOIN_TOKEN"
	// envBrokerID holds the broker ID for 'runtime-broker join'.
	envBrokerID = "SCION_BROKER_ID"
	// brokerJoinTokenPrefix matches hub.JoinTokenPrefix; the cmd package
	// does not import the hub server package for one constant.
	brokerJoinTokenPrefix = "scion_join_"
)

var (
	brokerJoinBrokerID string
)

var brokerJoinCmd = &cobra.Command{
	Use:   "join",
	Short: "Join this host to the Hub with a join token",
	Long: `Join this host to the Hub as an existing broker, using a join token.

A user with access to the Hub creates the broker and a join token on their own
machine with 'scion hub brokers join-token create <broker-name>'. This command
then runs on the broker host. It needs no Hub user credential: the join token
is the only credential it sends.

The join token is read from the SCION_BROKER_JOIN_TOKEN environment variable.
There is no command-line flag for the token.

The Hub endpoint is resolved as for 'register': SCION_HUB_ENDPOINT, hub.endpoint
in settings, or --hub.

This command will:
1. Check whether the local broker server is running (a warning only, so the
   host can join before 'scion runtime-broker start')
2. Exchange the join token for broker credentials
3. Save the credentials and the broker ID for future authentication

It does not add the broker to any project. Use 'scion runtime-broker provide'
for that.

Examples:
  SCION_HUB_ENDPOINT=https://hub.example.com \
  SCION_BROKER_JOIN_TOKEN=<token> \
  scion runtime-broker join --broker-id <broker-id>`,
	RunE: runBrokerJoin,
}

func init() {
	brokerCmd.AddCommand(brokerJoinCmd)

	brokerJoinCmd.Flags().StringVar(&brokerJoinBrokerID, "broker-id", "", "ID of the broker to join as (default: SCION_BROKER_ID)")
	brokerJoinCmd.Flags().StringVar(&brokerHubName, "name", "", "Name for this hub connection (derived from endpoint if not specified)")
	brokerJoinCmd.Flags().StringVar(&brokerTransportMode, "transport-mode", "", "Transport auth mode: 'iap' or 'cloudrun_invoker' (overrides SCION_TRANSPORT_MODE)")
	brokerJoinCmd.Flags().StringVar(&brokerTransportAudience, "transport-audience", "", "Transport auth OIDC audience (overrides SCION_TRANSPORT_AUDIENCE)")
	brokerJoinCmd.Flags().IntVar(&brokerPort, "port", 0, brokerPortFlagUsage)
}

// resolveBrokerJoinToken returns the join token from SCION_BROKER_JOIN_TOKEN,
// trimmed of surrounding whitespace. A value without the scion_join_ prefix
// is rejected here, before any request is sent.
func resolveBrokerJoinToken() (string, error) {
	token := strings.TrimSpace(os.Getenv(envBrokerJoinToken))
	if token == "" {
		return "", fmt.Errorf("no join token: set %s", envBrokerJoinToken)
	}
	if !strings.HasPrefix(token, brokerJoinTokenPrefix) {
		return "", fmt.Errorf("the value in %s is not a join token (expected the %s prefix)", envBrokerJoinToken, brokerJoinTokenPrefix)
	}
	return token, nil
}

// resolveBrokerJoinBrokerID returns the broker ID from --broker-id, else
// SCION_BROKER_ID.
func resolveBrokerJoinBrokerID() (string, error) {
	id := strings.TrimSpace(brokerJoinBrokerID)
	if id == "" {
		id = strings.TrimSpace(os.Getenv(envBrokerID))
	}
	if id == "" {
		return "", fmt.Errorf("broker ID is required: pass --broker-id or set %s", envBrokerID)
	}
	return id, nil
}

// newUnauthenticatedHubClient returns a hub client that sends no user or
// broker credential. Transport auth (IAP / Cloud Run invoker) is still
// applied when configured, from the given mode and audience or the
// environment.
func newUnauthenticatedHubClient(endpoint, transportMode, transportAudience string) (hubclient.Client, error) {
	opts := []hubclient.Option{hubclient.WithTimeout(30 * time.Second)}
	src, mode, err := transportauth.ResolveBrokerTransport(transportMode, transportAudience, adcsource.New)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve transport auth: %w", err)
	}
	if src != nil {
		opts = append(opts, hubclient.WithTransportAuth(src, mode))
	}
	return hubclient.New(endpoint, opts...)
}

func runBrokerJoin(cmd *cobra.Command, args []string) error {
	brokerID, err := resolveBrokerJoinBrokerID()
	if err != nil {
		return err
	}
	token, err := resolveBrokerJoinToken()
	if err != nil {
		return err
	}

	gp := projectPath
	if gp == "" && globalMode {
		gp = "global"
	}
	resolvedPath, _, err := config.ResolveProjectPath(gp)
	if err != nil {
		return fmt.Errorf("failed to resolve project path: %w", err)
	}
	settings, err := config.LoadSettings(resolvedPath)
	if err != nil {
		return fmt.Errorf("failed to load settings: %w", err)
	}

	endpoint := GetHubEndpoint(settings)
	if endpoint == "" {
		return fmt.Errorf("hub endpoint not configured; configure via SCION_HUB_ENDPOINT, hub.endpoint in settings.yaml, or --hub flag")
	}

	// The broker server does not have to be running yet: a provisioning
	// script may join first and start the broker afterwards.
	port := resolveBrokerPort(cmd)
	if health, err := checkLocalBrokerServer(port); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: broker server not running on port %d; start it with 'scion runtime-broker start' after joining\n", port)
	} else {
		fmt.Fprintf(os.Stderr, "Broker server is running (status: %s, version: %s)\n", health.Status, health.Version)
	}

	hubName := brokerHubName
	if hubName == "" {
		hubName = brokercredentials.DeriveHubName(endpoint)
	}
	if hubName == "" {
		hubName = "default"
	}

	multiStore, migrated, migrationErr := initializeBrokerCredentialStore()
	if migrationErr != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to migrate legacy credentials: %v\n", migrationErr)
	} else if migrated {
		fmt.Fprintf(os.Stderr, "Migrated legacy credentials to %s\n", multiStore.Dir())
	}

	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return fmt.Errorf("failed to get global directory: %w", err)
	}

	client, err := newUnauthenticatedHubClient(endpoint, brokerTransportMode, brokerTransportAudience)
	if err != nil {
		return err
	}

	hostname, err := os.Hostname()
	if err != nil {
		hostname = "local-host"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	joined, err := completeJoinAndPersist(ctx, client, brokerJoinParams{
		Settings:          settings,
		Endpoint:          endpoint,
		HubName:           hubName,
		BrokerID:          brokerID,
		JoinToken:         token,
		Hostname:          hostname,
		TransportMode:     brokerTransportMode,
		TransportAudience: brokerTransportAudience,
		CredStore:         multiStore,
	})
	if err != nil {
		return err
	}
	if joined.SaveErr != nil {
		// The hub has already issued the credentials; without the saved
		// copy this host cannot authenticate, so this is a failure.
		return fmt.Errorf("broker %s joined the hub, but saving its credentials failed: %w", joined.BrokerID, joined.SaveErr)
	}

	persistBrokerHubSettings(os.Stderr, globalDir, endpoint, joined.BrokerID, hubName)

	fmt.Printf("Broker %s joined hub %s; credentials saved to %s\n", joined.BrokerID, endpoint, multiStore.Dir())
	return nil
}

// brokerJoinParams are the inputs to completeJoinAndPersist.
type brokerJoinParams struct {
	// Settings supply the broker profiles and default profile sent to the hub.
	Settings *config.Settings
	// Endpoint is the hub endpoint recorded in the saved credentials.
	Endpoint string
	// HubName is the hub connection name the credentials are saved under.
	HubName   string
	BrokerID  string
	JoinToken string
	Hostname  string
	// TransportMode and TransportAudience come from flags; when empty, the
	// SCION_TRANSPORT_MODE / SCION_TRANSPORT_AUDIENCE values are recorded.
	TransportMode     string
	TransportAudience string
	CredStore         *brokercredentials.MultiStore
}

// brokerJoinResult is the outcome of completeJoinAndPersist.
type brokerJoinResult struct {
	// BrokerID is the broker ID returned by the hub.
	BrokerID string
	// SaveErr is set when the join succeeded but the credentials could not
	// be saved. Callers decide whether that is fatal.
	SaveErr error
}

// completeJoinAndPersist redeems a join token at POST /api/v1/brokers/join
// and saves the returned HMAC credentials under p.HubName. It is shared by
// 'runtime-broker register' (which mints and redeems in one process) and
// 'runtime-broker join' (which redeems a token minted elsewhere).
func completeJoinAndPersist(ctx context.Context, client hubclient.Client, p brokerJoinParams) (*brokerJoinResult, error) {
	joinReq := &hubclient.JoinBrokerRequest{
		BrokerID:         p.BrokerID,
		JoinToken:        p.JoinToken,
		Hostname:         p.Hostname,
		Version:          version.Version,
		Capabilities:     brokerRegistrationCapabilities(),
		Profiles:         buildBrokerProfiles(p.Settings),
		WorkspaceStorage: loadBrokerRegistrationWorkspaceStorage(),
		DefaultProfile:   brokerRegistrationDefaultProfile(p.Settings),
	}

	joinResp, err := client.RuntimeBrokers().Join(ctx, joinReq)
	if err != nil {
		return nil, fmt.Errorf("failed to complete broker join: %w", err)
	}

	// Resolve transport config from flags, then env
	transportMode := p.TransportMode
	if transportMode == "" {
		transportMode = os.Getenv(transportauth.EnvTransportMode)
	}
	transportAudience := p.TransportAudience
	if transportAudience == "" {
		transportAudience = os.Getenv(transportauth.EnvTransportAudience)
	}

	newCreds := &brokercredentials.BrokerCredentials{
		Name:              p.HubName,
		BrokerID:          joinResp.BrokerID,
		SecretKey:         joinResp.SecretKey,
		HubEndpoint:       p.Endpoint,
		AuthMode:          brokercredentials.AuthModeHMAC,
		RegisteredAt:      time.Now(),
		TransportMode:     transportMode,
		TransportAudience: transportAudience,
	}
	return &brokerJoinResult{
		BrokerID: joinResp.BrokerID,
		SaveErr:  p.CredStore.Save(newCreds),
	}, nil
}

// persistBrokerHubSettings records the hub endpoint, the broker ID and the
// hub connection entry in global settings. Failures are reported to w as
// warnings, as 'register' has always done.
func persistBrokerHubSettings(w io.Writer, globalDir, endpoint, brokerID, hubName string) {
	if endpoint != "" {
		if err := config.UpdateSetting(globalDir, "hub.endpoint", endpoint, true); err != nil {
			_, _ = fmt.Fprintf(w, "Warning: failed to save hub endpoint to global settings: %v\n", err)
		}
	}
	if err := config.UpdateSetting(globalDir, "hub.brokerId", brokerID, true); err != nil {
		_, _ = fmt.Fprintf(w, "Warning: failed to save broker ID: %v\n", err)
	}
	// Write hub_connections entry for this registration
	if err := config.UpdateSetting(globalDir, "hub_connections."+hubName+".endpoint", endpoint, true); err != nil {
		_, _ = fmt.Fprintf(w, "Warning: failed to save hub connection to settings: %v\n", err)
	}
}
