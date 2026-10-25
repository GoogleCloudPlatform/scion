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

package relayserver

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// TestStartRefusesPlaintextNonLoopbackHop: a separate relay refuses to
// start with an internal listener that would serve plaintext on a
// non-loopback address, or a plaintext hub internal URL to a non-loopback
// host, and registers nothing.
func TestStartRefusesPlaintextNonLoopbackHop(t *testing.T) {
	for _, tc := range []struct {
		name, setting string
		mod           func(*Config)
	}{
		{name: "wildcard listener", setting: "server.hub.conduit.internal_listen", mod: func(c *Config) { c.InternalListen = ":0" }},
		{name: "unspecified listener", setting: "server.hub.conduit.internal_listen", mod: func(c *Config) { c.InternalListen = "0.0.0.0:0" }},
		{name: "plaintext hub internal URL", setting: "server.relay.hub_internal_url", mod: func(c *Config) { c.HubInternalURL = "http://192.0.2.20:8080" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := relaytest.NewWorld(t)
			cfg := testConfig(t, w, newFakeExperiment(expAnswer{on: true}))
			tc.mod(&cfg)
			_, err := Start(context.Background(), cfg)
			if !errors.Is(err, config.ErrPlaintextNonLoopback) || !strings.Contains(err.Error(), tc.setting) {
				t.Fatalf("Start = %v, want an ErrPlaintextNonLoopback refusal naming %s", err, tc.setting)
			}
			relays, err := w.Store.ListRelays(context.Background())
			if err != nil || len(relays) != 0 {
				t.Fatalf("relay rows %v (%v); want none", relays, err)
			}
		})
	}
}

// TestStartServesInternalTLS: with InternalTLS the internal listener
// serves TLS on any address and the relay registers an https endpoint.
// The required self-check probes that endpoint with the hop client TLS
// (operator CA, shared server name), so the start proves the TLS path.
func TestStartServesInternalTLS(t *testing.T) {
	const name = "scion-relay.internal"
	files := relaytest.NewHopTLSFiles(t, name)
	srvTLS, err := relay.NewHopServerTLS(files.CertFile, files.KeyFile, nil)
	if err != nil {
		t.Fatal(err)
	}
	cliTLS, err := relay.NewHopClientTLS(files.CAFile, name)
	if err != nil {
		t.Fatal(err)
	}
	w := relaytest.NewWorld(t)
	cfg := testConfig(t, w, newFakeExperiment(expAnswer{on: true}))
	cfg.InternalListen = "0.0.0.0:0" // allowed: the listener serves TLS
	cfg.InternalTLS = srvTLS
	cfg.HopTLS = cliTLS
	cfg.RequireSelfCheck = true
	startServer(t, cfg)
	relays, err := w.Store.ListRelays(context.Background())
	if err != nil || len(relays) != 1 {
		t.Fatalf("relay rows %v (%v); want one", relays, err)
	}
	if ep := relays[0].InternalEndpoint; !strings.HasPrefix(ep, "https://") {
		t.Fatalf("registered internal endpoint %q, want https", ep)
	}
}
