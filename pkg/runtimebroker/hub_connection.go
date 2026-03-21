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

package runtimebroker

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/templatecache"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// ConnectionStatus represents the state of a hub connection.
type ConnectionStatus string

const (
	// ConnectionStatusConnected indicates the connection is active and healthy.
	ConnectionStatusConnected ConnectionStatus = "connected"
	// ConnectionStatusDisconnected indicates the connection is not active.
	ConnectionStatusDisconnected ConnectionStatus = "disconnected"
	// ConnectionStatusError indicates the connection encountered an error.
	ConnectionStatusError ConnectionStatus = "error"
)

// HubConnection encapsulates all per-hub state for a single hub connection.
type HubConnection struct {
	Name        string // "local", "prod", "hub-scion-dev"
	HubEndpoint string
	BrokerID    string
	AuthMode    brokercredentials.AuthMode

	Credentials *brokercredentials.BrokerCredentials
	SecretKey   []byte // decoded from Credentials.SecretKey

	HubClient      hubclient.Client
	Hydrator       *templatecache.Hydrator
	Heartbeat      *HeartbeatService
	ControlChannel *ControlChannelClient

	// IsColocated indicates this connection is to a co-located hub running in
	// the same process. Co-located connections skip HTTP heartbeat because an
	// internal database heartbeat loop handles status updates directly.
	IsColocated bool

	// TemplatesDir is the local filesystem path to the templates directory
	// (e.g., ~/.scion/templates). When set on a co-located connection, the
	// broker resolves templates directly from this directory instead of
	// going through the Hub API → storage → cache hydration round-trip.
	TemplatesDir string

	Status ConnectionStatus
	mu     sync.RWMutex
}

// GetStatus returns the current connection status.
func (hc *HubConnection) GetStatus() ConnectionStatus {
	hc.mu.RLock()
	defer hc.mu.RUnlock()
	return hc.Status
}

// setStatus updates the connection status.
func (hc *HubConnection) setStatus(status ConnectionStatus) {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	hc.Status = status
}

// Start starts the heartbeat and control channel services for this connection.
func (hc *HubConnection) Start(ctx context.Context, server *Server) error {
	hasValidCredentials := hc.Credentials != nil && hc.Credentials.SecretKey != ""

	// Start heartbeat service if enabled.
	// Co-located connections skip HTTP heartbeat because an internal
	// database heartbeat loop handles status updates directly.
	if server.config.HeartbeatEnabled && !hc.IsColocated && hc.HubClient != nil && hc.BrokerID != "" {
		if !hasValidCredentials {
			slog.Warn("Skipping heartbeat for connection: no valid credentials", "name", hc.Name)
		} else {
			interval := server.config.HeartbeatInterval
			if interval <= 0 {
				interval = DefaultHeartbeatInterval
			}

			groveFilter := server.buildGroveFilterForHub(hc.HubEndpoint)

			hc.Heartbeat = NewHeartbeatService(
				hc.HubClient.RuntimeBrokers(),
				hc.BrokerID,
				interval,
				server.manager,
				groveFilter,
				logging.Subsystem("broker.heartbeat"),
			)
			hc.Heartbeat.auxiliaryManagers = server.getAuxiliaryManagers
			hc.Heartbeat.SetVersion(server.version)
			hc.Heartbeat.Start(ctx)
			slog.Info("Heartbeat started for hub connection", "name", hc.Name, "interval", interval)
		}
	}

	// Start control channel if enabled
	if server.config.ControlChannelEnabled && hc.HubEndpoint != "" && hc.BrokerID != "" {
		if !hasValidCredentials {
			slog.Warn("Skipping control channel for connection: no valid credentials", "name", hc.Name)
		} else {
			ccConfig := ControlChannelConfig{
				HubEndpoint:         hc.HubEndpoint,
				BrokerID:            hc.BrokerID,
				SecretKey:           hc.SecretKey,
				Version:             server.version,
				ReconnectInitial:    1 * time.Second,
				ReconnectMax:        60 * time.Second,
				ReconnectMultiplier: 2.0,
				PingInterval:        30 * time.Second,
				PongWait:            60 * time.Second,
				WriteWait:           10 * time.Second,
				Debug:               server.config.Debug,
			}

			hc.ControlChannel = NewControlChannelClient(ccConfig, server.Handler(), server, hc.Name, logging.Subsystem("broker.control-channel"))
			go func() {
				if err := hc.ControlChannel.Connect(ctx); err != nil {
					if ctx.Err() != nil {
						slog.Info("Control channel stopped", "name", hc.Name)
					} else {
						slog.Error("Control channel error", "name", hc.Name, "error", err)
					}
				}
			}()
			slog.Info("Connecting to Hub control channel", "name", hc.Name, "endpoint", hc.HubEndpoint)
		}
	}

	hc.setStatus(ConnectionStatusConnected)
	return nil
}

// Stop stops the heartbeat and control channel services for this connection.
func (hc *HubConnection) Stop() {
	if hc.ControlChannel != nil {
		slog.Info("Stopping control channel for connection", "name", hc.Name)
		hc.ControlChannel.Close()
		hc.ControlChannel = nil
	}
	if hc.Heartbeat != nil {
		slog.Info("Stopping heartbeat for connection", "name", hc.Name)
		hc.Heartbeat.Stop()
		hc.Heartbeat = nil
	}
	hc.setStatus(ConnectionStatusDisconnected)
}

// Reinitialize updates credentials and restarts services for this connection.
func (hc *HubConnection) Reinitialize(ctx context.Context, server *Server, creds *brokercredentials.BrokerCredentials) error {
	// Stop existing services
	hc.Stop()

	// Update credentials
	hc.Credentials = creds
	hc.BrokerID = creds.BrokerID
	hc.HubEndpoint = creds.HubEndpoint
	hc.AuthMode = creds.AuthMode

	// Decode secret key
	secretKey, err := base64.StdEncoding.DecodeString(creds.SecretKey)
	if err != nil {
		hc.setStatus(ConnectionStatusError)
		return fmt.Errorf("failed to decode secret key: %w", err)
	}
	hc.SecretKey = secretKey

	// Create new Hub client
	opts := buildHubClientOpts(creds, secretKey)
	client, err := hubclient.New(creds.HubEndpoint, opts...)
	if err != nil {
		hc.setStatus(ConnectionStatusError)
		return fmt.Errorf("failed to create Hub client: %w", err)
	}
	hc.HubClient = client

	// Rebuild hydrator using shared cache
	if server.cache != nil {
		hc.Hydrator = templatecache.NewHydrator(server.cache, client)
	}

	slog.Info("Hub connection reinitialized", "name", hc.Name, "brokerID", creds.BrokerID)

	// Restart services
	return hc.Start(ctx, server)
}

// buildHubClientOpts creates hub client options from credentials.
func buildHubClientOpts(creds *brokercredentials.BrokerCredentials, secretKey []byte) []hubclient.Option {
	var opts []hubclient.Option

	switch creds.AuthMode {
	case brokercredentials.AuthModeDevAuth:
		opts = append(opts, hubclient.WithAutoDevAuth())
		slog.Info("Hub client using auto dev authentication", "name", creds.Name)
	case brokercredentials.AuthModeBearer:
		// Bearer mode could use a token from the credentials, but currently
		// there's no token field in BrokerCredentials for bearer auth.
		// Fall through to HMAC for now.
		fallthrough
	default:
		// Default to HMAC auth
		if len(secretKey) > 0 {
			opts = append(opts, hubclient.WithHMACAuth(creds.BrokerID, secretKey))
			slog.Info("Hub client using HMAC authentication", "name", creds.Name, "brokerID", creds.BrokerID)
		} else {
			opts = append(opts, hubclient.WithAutoDevAuth())
			slog.Info("Hub client using auto dev authentication (no secret key)", "name", creds.Name)
		}
	}

	return opts
}
