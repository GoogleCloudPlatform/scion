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
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"cloud.google.com/go/compute/metadata"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relayrole/relayserver"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store/relaydb"
)

// scion server --role relay: a separate conduit relay process (design §3.9
// "Separate relay"). This file only composes it: relayserver runs the
// relay, relaydb is its database access (the registry store, the
// hub.conduit reader and the revocation events), and relay builds its
// relay-peer authenticator. It never reads the session secret. A test in
// pkg/conduit/relayrole/relayserver checks the imports of this file.

// Server roles (--role).
const (
	serverRoleHub   = "hub"
	serverRoleRelay = "relay"
)

// serverRole is --role.
var serverRole = serverRoleHub

// validateServerRole checks --role.
func validateServerRole(role string) error {
	switch role {
	case serverRoleHub, serverRoleRelay:
		return nil
	}
	return fmt.Errorf("invalid --role %q: want hub or relay", role)
}

// validateRelayRoleConfig checks the configuration a separate relay needs.
// Errors name the setting, never a value.
func validateRelayRoleConfig(cfg *config.GlobalConfig) error {
	if err := validateConduitPeerSecret(cfg.Hub.Conduit, true, ""); err != nil {
		return err
	}
	if err := config.ValidatePeerSecret(cfg.Hub.Conduit.PeerSecretValue()); err != nil {
		return err
	}
	if err := cfg.Hub.Conduit.Validate(); err != nil {
		return err
	}
	if err := cfg.Relay.Validate(); err != nil {
		return err
	}
	switch {
	case cfg.Relay.HubInternalURL == "":
		return errors.New("server.relay.hub_internal_url is required on --role relay")
	case cfg.Relay.PublicURL == "":
		return errors.New("server.relay.public_url is required on --role relay")
	case cfg.Hub.Conduit.InternalListen == "":
		return errors.New("server.hub.conduit.internal_listen is required on --role relay")
	}
	return nil
}

// relayDatabaseOptions is the relay's database connection input.
func relayDatabaseOptions(cfg *config.GlobalConfig) relaydb.Options {
	return relaydb.Options{
		Driver:        cfg.Database.Driver,
		DSN:           cfg.Database.URL,
		MaxOpenConns:  cfg.Database.MaxOpenConns,
		MaxIdleConns:  cfg.Database.MaxIdleConns,
		EventSubjects: append(append([]string(nil), relayserver.RevocationSubjects...), relayserver.SettingsUpdatedSubject),
	}
}

// openRelayDB opens the relay's database access (a var for tests).
var openRelayDB = relaydb.Open

// relayServerConfig builds the relay's configuration from cfg. It reads
// the relay and conduit settings and builds the relay-peer authenticator
// from server.hub.conduit.peer_secret; there is no session secret input.
func relayServerConfig(ctx context.Context, cfg *config.GlobalConfig, db *relaydb.DB) (relayserver.Config, error) {
	if err := validateRelayRoleConfig(cfg); err != nil {
		return relayserver.Config{}, err
	}
	id := relay.InstanceID(cfg.Hub.Conduit.InstanceID)
	onGCP := metadata.OnGCE()
	opts := relay.ConduitPeerAuthOptions{
		Mode:            cfg.Hub.Conduit.PeerAuthMode(),
		OnGCP:           onGCP,
		SelfID:          id,
		PeerSecret:      cfg.Hub.Conduit.PeerSecretValue(),
		Audience:        cfg.Hub.Conduit.PeerAudience,
		ServiceAccounts: cfg.Hub.Conduit.PeerServiceAccounts,
	}
	if onGCP {
		if email, err := metadata.EmailWithContext(ctx, "default"); err == nil {
			opts.OwnServiceAccount = email
		}
	}
	auth, mode, err := relay.NewConduitPeerAuth(opts)
	if err != nil {
		return relayserver.Config{}, err
	}
	slog.Info("Conduit relay-peer authentication", "mode", mode)
	podIP := os.Getenv("POD_IP")
	return relayserver.Config{
		InstanceID:     id,
		PublicURL:      cfg.Relay.PublicURL,
		HubInternalURL: cfg.Relay.HubInternalURL,
		PublicListen:   net.JoinHostPort(cfg.Hub.Host, strconv.Itoa(cfg.Hub.Port)),
		InternalListen: cfg.Hub.Conduit.InternalListen,
		Advertise: func(bound net.Addr) string {
			return conduitAdvertiseEndpoint(cfg.Hub.Conduit.InternalAdvertise, bound, podIP)
		},
		PeerAuth:         auth,
		Registry:         db.Registry,
		Experiment:       db.Settings,
		Events:           db.Events(),
		RecheckInterval:  conduitAuthzRecheckIntervalSetting(cfg),
		ReconnectWindow:  conduitReconnectWindowSetting(cfg),
		LifetimeCap:      conduitLifetimeCapSetting(cfg),
		RequireSelfCheck: hostedMode && isHADeployment(cfg),
	}, nil
}

// runRelayRole runs scion server --role relay until a signal or a fatal
// relay error.
func runRelayRole(ctx context.Context, cfg *config.GlobalConfig) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := validateRelayRoleConfig(cfg); err != nil {
		return err
	}
	db, err := openRelayDB(ctx, relayDatabaseOptions(cfg))
	if err != nil {
		return err
	}
	defer db.Close()
	rcfg, err := relayServerConfig(ctx, cfg, db)
	if err != nil {
		return err
	}
	srv, err := relayserver.Start(ctx, rcfg)
	if err != nil {
		return err
	}
	runErr := srv.Wait(ctx)
	grace, capped := conduitShutdownDrainSetting(cfg)
	timeout := 30 * time.Second
	if capped {
		timeout = grace
	}
	sctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_ = srv.Shutdown(sctx, capped, relay.ShutdownDrainCap(grace))
	return runErr
}
