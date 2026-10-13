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
	"os"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store/relaydb"
)

const relayRoleTestPeerSecret = "peer-secret-for-relay-role-tests-0123456789"

// relayRoleTestConfig is a complete --role relay configuration.
func relayRoleTestConfig() *config.GlobalConfig {
	cfg := &config.GlobalConfig{}
	cfg.Hub.Host, cfg.Hub.Port = "127.0.0.1", 18443
	cfg.Hub.Conduit.PeerSecret = relayRoleTestPeerSecret
	cfg.Hub.Conduit.PeerAuth = config.ConduitPeerAuthHMAC
	cfg.Hub.Conduit.InstanceID = "relay-7"
	cfg.Hub.Conduit.InternalListen = "127.0.0.1:0"
	cfg.Hub.Conduit.InternalAdvertise = "http://relay-7.internal:9811"
	cfg.Relay.HubInternalURL = "http://hub-internal.example:9811"
	cfg.Relay.PublicURL = "https://relay-7.example.com"
	cfg.Database.Driver, cfg.Database.URL = "sqlite", "relay.db"
	return cfg
}

// TestServerRoleFlag: --role accepts hub and relay only.
func TestServerRoleFlag(t *testing.T) {
	assert.NoError(t, validateServerRole("hub"))
	assert.NoError(t, validateServerRole("relay"))
	assert.ErrorContains(t, validateServerRole("broker"), "invalid --role")
}

// TestValidateRelayRoleConfig: --role relay requires peer_secret, the hub
// internal URL, its public URL and the internal listener; errors name the
// setting, never a value.
func TestValidateRelayRoleConfig(t *testing.T) {
	require.NoError(t, validateRelayRoleConfig(relayRoleTestConfig()))
	for name, tc := range map[string]struct {
		mod     func(*config.GlobalConfig)
		wantErr string
	}{
		"no peer secret":       {func(c *config.GlobalConfig) { c.Hub.Conduit.PeerSecret = "" }, "server.hub.conduit.peer_secret is required on --role relay"},
		"weak peer secret":     {func(c *config.GlobalConfig) { c.Hub.Conduit.PeerSecret = "short" }, "peer_secret"},
		"no hub internal url":  {func(c *config.GlobalConfig) { c.Relay.HubInternalURL = "" }, "server.relay.hub_internal_url is required"},
		"no public url":        {func(c *config.GlobalConfig) { c.Relay.PublicURL = "" }, "server.relay.public_url is required"},
		"no internal listener": {func(c *config.GlobalConfig) { c.Hub.Conduit.InternalListen = "" }, "server.hub.conduit.internal_listen is required"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := relayRoleTestConfig()
			tc.mod(cfg)
			err := validateRelayRoleConfig(cfg)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.NotContains(t, err.Error(), relayRoleTestPeerSecret)
		})
	}
}

// TestRunRelayRoleChecksConfigBeforeTheDatabase: an incomplete
// configuration fails before the database is opened.
func TestRunRelayRoleChecksConfigBeforeTheDatabase(t *testing.T) {
	prev := openRelayDB
	t.Cleanup(func() { openRelayDB = prev })
	openRelayDB = func(context.Context, relaydb.Options) (*relaydb.DB, error) {
		t.Fatal("the database was opened")
		return nil, nil
	}
	cfg := relayRoleTestConfig()
	cfg.Hub.Conduit.PeerSecret = ""
	err := runRelayRole(context.Background(), cfg)
	assert.ErrorContains(t, err, "peer_secret is required on --role relay")
}

// TestRelayServerConfigReadsRelaySettings: the relay's configuration comes
// from server.relay, server.hub and server.hub.conduit, its database access
// from the opened relay database, and its relay-peer authenticator from
// peer_secret.
func TestRelayServerConfigReadsRelaySettings(t *testing.T) {
	cfg := relayRoleTestConfig()
	db := &relaydb.DB{Settings: &relaydb.SettingsReader{}}
	rcfg, err := relayServerConfig(context.Background(), cfg, db)
	require.NoError(t, err)
	assert.Equal(t, "relay-7", rcfg.InstanceID)
	assert.Equal(t, "http://hub-internal.example:9811", rcfg.HubInternalURL)
	assert.Equal(t, "https://relay-7.example.com", rcfg.PublicURL)
	assert.Equal(t, "127.0.0.1:18443", rcfg.PublicListen)
	assert.Equal(t, "127.0.0.1:0", rcfg.InternalListen)
	assert.Equal(t, "http://relay-7.internal:9811", rcfg.Advertise(nil), "server.hub.conduit.internal_advertise")
	assert.NotNil(t, rcfg.PeerAuth)
	assert.Same(t, db.Settings, rcfg.Experiment)
	assert.Nil(t, rcfg.Events, "no LISTEN source on SQLite")

	opts := relayDatabaseOptions(cfg)
	assert.Equal(t, "sqlite", opts.Driver)
	assert.Equal(t, "relay.db", opts.DSN)
}

// TestRelayRoleNeverReadsTheSessionSecret: the relay composition root reads
// no session secret spelling and passes none to the relay-peer
// authenticator.
func TestRelayRoleNeverReadsTheSessionSecret(t *testing.T) {
	src, err := os.ReadFile("server_relay.go")
	require.NoError(t, err)
	for _, forbidden := range []string{`sessionSecretValue`, `webSessionSecret`, `SESSION_SECRET`, `SharedSecret`, `SharedSigningSecret`} {
		assert.NotRegexp(t, regexp.MustCompile(regexp.QuoteMeta(forbidden)), string(src), forbidden)
	}
}
