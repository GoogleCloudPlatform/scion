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

//go:build !no_sqlite

package cmd

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	relayModeTestPeerSecret    = "peer-secret-for-startup-tests-0123456789"
	relayModeTestSessionSecret = "session-secret-for-startup-tests-01234"
)

// TestValidateConduitPeerSecret (design §3.9, criterion 30): peer_secret
// is required in external relay mode and on --role relay, and a hub refuses
// one equal to its session secret. Errors name the setting, never a value.
func TestValidateConduitPeerSecret(t *testing.T) {
	external := config.HubConduitConfig{RelayMode: config.ConduitRelayModeExternal}
	withSecret := func(c config.HubConduitConfig, s string) config.HubConduitConfig { c.PeerSecret = s; return c }
	for _, tc := range []struct {
		name      string
		conduit   config.HubConduitConfig
		relayRole bool
		session   string
		wantErr   string
	}{
		{name: "in-process without peer secret", conduit: config.HubConduitConfig{}},
		{name: "external without peer secret", conduit: external, wantErr: "server.hub.conduit.peer_secret is required"},
		{name: "relay role without peer secret", relayRole: true, wantErr: "server.hub.conduit.peer_secret is required on --role relay"},
		{name: "external with peer secret", conduit: withSecret(external, relayModeTestPeerSecret), session: relayModeTestSessionSecret},
		{name: "relay role with peer secret", conduit: withSecret(config.HubConduitConfig{}, relayModeTestPeerSecret), relayRole: true},
		{name: "equal to the session secret", conduit: withSecret(external, relayModeTestSessionSecret), session: relayModeTestSessionSecret, wantErr: "must differ from the session secret"},
		{name: "equal to the session secret, in-process", conduit: withSecret(config.HubConduitConfig{}, relayModeTestSessionSecret), session: relayModeTestSessionSecret, wantErr: "must differ from the session secret"},
		{name: "equal to the session secret after the trailing newline is trimmed", conduit: withSecret(external, relayModeTestSessionSecret+"\n"), session: relayModeTestSessionSecret, wantErr: "must differ from the session secret"},
		{name: "a second trailing newline is part of the secret", conduit: withSecret(external, relayModeTestSessionSecret+"\n\n"), session: relayModeTestSessionSecret},
		{name: "only a newline is unset (external)", conduit: withSecret(external, "\n"), wantErr: "server.hub.conduit.peer_secret is required"},
		{name: "only a newline is unset (relay role)", conduit: withSecret(config.HubConduitConfig{}, "\n"), relayRole: true, wantErr: "server.hub.conduit.peer_secret is required on --role relay"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateConduitPeerSecret(tc.conduit, tc.relayRole, tc.session)
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.NotContains(t, err.Error(), relayModeTestPeerSecret)
			assert.NotContains(t, err.Error(), relayModeTestSessionSecret)
		})
	}
}

// TestValidateServerPreflight_RelayMode: the hub's startup preflight
// applies the relay-mode rules with the configured session secret, and
// rejects a weak peer secret.
func TestValidateServerPreflight_RelayMode(t *testing.T) {
	t.Cleanup(resetServerFlags)
	for _, tc := range []struct {
		name    string
		conduit config.HubConduitConfig
		wantErr string
	}{
		{name: "external without peer secret", conduit: config.HubConduitConfig{RelayMode: "external"}, wantErr: "server.hub.conduit.peer_secret is required"},
		{name: "peer secret equal to the session secret", conduit: config.HubConduitConfig{RelayMode: "external", PeerSecret: relayModeTestSessionSecret}, wantErr: "must differ from the session secret"},
		{name: "peer secret too short", conduit: config.HubConduitConfig{RelayMode: "external", PeerSecret: "short"}, wantErr: "server.hub.conduit.peer_secret"},
		{name: "peer secret only a newline", conduit: config.HubConduitConfig{RelayMode: "external", PeerSecret: "\n"}, wantErr: "server.hub.conduit.peer_secret is required"},
		{name: "unknown relay mode", conduit: config.HubConduitConfig{RelayMode: "sidecar"}, wantErr: "server.hub.conduit.relay_mode"},
		{name: "external with a peer secret", conduit: config.HubConduitConfig{RelayMode: "external", PeerSecret: relayModeTestPeerSecret}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetServerFlags()
			enableHub = true
			t.Setenv("SCION_SERVER_SESSION_SECRET", relayModeTestSessionSecret)
			cfg := &config.GlobalConfig{}
			cfg.Hub.Conduit = tc.conduit
			err := validateServerPreflight(cfg)
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.NotContains(t, err.Error(), relayModeTestSessionSecret)
		})
	}
}

// TestStartConduit_ExternalRelayNeedsExperiment: external relay mode with
// hub.conduit off is a startup error.
func TestStartConduit_ExternalRelayNeedsExperiment(t *testing.T) {
	t.Cleanup(resetServerFlags)
	srv := conduitHubServer(t, false)
	cfg := &config.GlobalConfig{}
	cfg.Hub.Conduit = config.HubConduitConfig{RelayMode: "external", PeerSecret: relayModeTestPeerSecret}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	t.Cleanup(func() { cancel(); wg.Wait() })
	err := startConduit(ctx, cancel, cfg, srv, "https://hub.example.com", &wg, make(chan error, 1))
	require.ErrorIs(t, err, errConduitExternalNeedsExperiment)

	// In-process mode with the experiment off still starts nothing.
	cfg.Hub.Conduit = config.HubConduitConfig{}
	assert.NoError(t, startConduit(ctx, cancel, cfg, srv, "https://hub.example.com", &wg, make(chan error, 1)))
}

// TestStartConduit_ExternalRelayMode: with hub.conduit on, external relay
// mode starts the external runtime instead of a relay, never traps
// SIGTERM (there is nothing to drain), and any failure is fatal, outside
// hosted HA too.
func TestStartConduit_ExternalRelayMode(t *testing.T) {
	t.Cleanup(resetServerFlags)
	srv := conduitHubServer(t, true)
	cfg := &config.GlobalConfig{}
	cfg.Hub.Conduit = config.HubConduitConfig{RelayMode: "external", PeerSecret: relayModeTestPeerSecret}

	prevExt, prevRelay, prevNotify := startConduitExternalFn, startConduitRelayFn, conduitSignalNotify
	t.Cleanup(func() {
		startConduitExternalFn, startConduitRelayFn, conduitSignalNotify = prevExt, prevRelay, prevNotify
	})
	relayStarted, trapped := false, false
	startConduitRelayFn = func(context.Context, *config.GlobalConfig, *hub.Server, string, *sync.WaitGroup, chan<- error, bool) error {
		relayStarted = true
		return nil
	}
	conduitSignalNotify = func(chan<- os.Signal, ...os.Signal) { trapped = true }

	var extErr error
	externalCalls := 0
	startConduitExternalFn = func(context.Context, *config.GlobalConfig, *hub.Server) error {
		externalCalls++
		return extErr
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	t.Cleanup(func() { cancel(); wg.Wait() })

	require.NoError(t, startConduit(ctx, cancel, cfg, srv, "https://hub.example.com", &wg, make(chan error, 1)))
	assert.Equal(t, 1, externalCalls)
	assert.False(t, relayStarted, "no in-process relay in external mode")
	assert.False(t, trapped, "SIGTERM is not trapped without a relay")

	extErr = errors.New("boom")
	err := startConduit(ctx, cancel, cfg, srv, "https://hub.example.com", &wg, make(chan error, 1))
	require.Error(t, err, "a failure is fatal outside hosted HA too")
	assert.Contains(t, err.Error(), "conduit external relay mode")
}
