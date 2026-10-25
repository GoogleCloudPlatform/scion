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
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// This file holds the hop transport test that needs the sqlite-only relay
// start helpers (resetForSkipTest, startRelayForSkipTest, stubHubRelayStart
// in server_conduit_peerauth_skip_test.go); the other hop TLS tests are in
// server_conduit_hop_tls_test.go and build with every tag set.

// TestStartConduitRelay_HopTransport: the in-process relay refuses to
// start with a plaintext internal listener on a non-loopback address; with
// the TLS settings it advertises https and hands the hop client TLS to the
// hub relay.
func TestStartConduitRelay_HopTransport(t *testing.T) {
	t.Cleanup(resetServerFlags)
	t.Run("wildcard listener without TLS is refused", func(t *testing.T) {
		resetForSkipTest(t)
		t.Setenv("SCION_SERVER_SESSION_SECRET", "conduit-test-signing-secret-0123456789")
		starts := stubHubRelayStart(t)
		cfg := &config.GlobalConfig{}
		cfg.Hub.Conduit.PeerAuth = config.ConduitPeerAuthHMAC
		cfg.Hub.Conduit.InternalListen = "0.0.0.0:0"
		_, err := startRelayForSkipTest(t, cfg, false)
		require.Error(t, err)
		assert.True(t, errors.Is(err, config.ErrPlaintextNonLoopback), "%v", err)
		assert.Empty(t, *starts, "no relay starts")
	})
	t.Run("TLS listener advertises https", func(t *testing.T) {
		resetForSkipTest(t)
		t.Setenv("SCION_SERVER_SESSION_SECRET", "conduit-test-signing-secret-0123456789")
		t.Setenv("POD_IP", "10.1.1.1")
		starts := stubHubRelayStart(t)
		files := relaytest.NewHopTLSFiles(t, hopTestServerName)
		cfg := &config.GlobalConfig{}
		cfg.Hub.Conduit = hopTestConduitConfig(files)
		cfg.Hub.Conduit.PeerAuth = config.ConduitPeerAuthHMAC
		cfg.Hub.Conduit.InternalListen = "0.0.0.0:0"
		_, err := startRelayForSkipTest(t, cfg, false)
		require.NoError(t, err)
		require.Len(t, *starts, 1)
		got := (*starts)[0]
		assert.True(t, strings.HasPrefix(got.InternalEndpoint, "https://10.1.1.1:"), "endpoint %q", got.InternalEndpoint)
		require.NotNil(t, got.HopTLS, "the hop client TLS reaches the hub relay")
		assert.Equal(t, hopTestServerName, got.HopTLS.ServerName)
		assert.NotNil(t, got.HopTLS.RootCAs)
	})
}
