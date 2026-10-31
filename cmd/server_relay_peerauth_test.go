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
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// TestRelayPeerAuthOptions_NoSessionSecret: a relay's relay-peer auth
// options never carry the session secret or the signing ring, which are
// hub-only, even when the session secret is set in the relay's
// environment; they come from the conduit settings alone.
func TestRelayPeerAuthOptions_NoSessionSecret(t *testing.T) {
	t.Setenv(sessionSecretEnv, "session-secret-0123456789abcdefghijklmn")
	t.Setenv(legacySessionSecretEnv, "legacy-session-secret-0123456789abcdef")
	cfg := &config.GlobalConfig{}
	cfg.Hub.Conduit.PeerSecret = "peer-secret-0123456789abcdefghijklmnopqr"
	opts, err := relayPeerAuthOptions(cfg, "relay-a", false)
	if err != nil {
		t.Fatal(err)
	}
	if opts.SharedSecret != "" {
		t.Error("the relay's peer auth options carry the session secret")
	}
	if opts.SigningRing != nil {
		t.Error("the relay's peer auth options carry the signing ring")
	}
	if opts.PeerSecret == "" {
		t.Error("the relay's peer auth options lack the peer secret")
	}
}

// TestWarnRelaySessionSecret: a relay with a session secret configured
// logs the warning; without one it logs nothing.
func TestWarnRelaySessionSecret(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	t.Setenv(sessionSecretEnv, "")
	t.Setenv(legacySessionSecretEnv, "")
	warnRelaySessionSecret()
	if buf.Len() != 0 {
		t.Fatalf("warned without a session secret: %s", buf.String())
	}
	t.Setenv(sessionSecretEnv, "session-secret-0123456789abcdefghijklmn")
	warnRelaySessionSecret()
	if !strings.Contains(buf.String(), "session secret is set on --role relay") {
		t.Fatalf("no warning with a session secret: %q", buf.String())
	}
}
