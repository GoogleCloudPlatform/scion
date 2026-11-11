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

package agent

import (
	"os"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// peerSecretEnvNames are both env spellings of
// server.hub.conduit.peer_secret.
var peerSecretEnvNames = []string{
	"SCION_SERVER_HUB_CONDUIT_PEERSECRET",
	"SCION_SERVER_HUB_CONDUIT_PEER_SECRET",
}

// TestPeerSecretNeverReachesAnAgent (design 3.9, criterion 30): with the
// relay-peer secret in the host env under either spelling, no part of an
// agent's environment carries its value: not a passthrough of the name,
// not an expansion of it in a value or a key, in local or broker mode, and
// not the env a broker-launched process inherits.
func TestPeerSecretNeverReachesAnAgent(t *testing.T) {
	const peerSecret = "relay-peer-secret-for-agent-env-tests-0123456789abcdef"
	for _, name := range peerSecretEnvNames {
		t.Run(name, func(t *testing.T) {
			for _, other := range peerSecretEnvNames {
				t.Setenv(other, "")
			}
			t.Setenv(name, peerSecret)
			cfg := &api.ScionConfig{Env: map[string]string{
				name:              "",
				"PEER_BRACED":     "${" + name + "}",
				"PEER_BARE":       "$" + name,
				"PEER_MIXED":      "prefix-${" + name + "}-suffix",
				"${" + name + "}": "key-expansion",
			}}
			for _, brokerMode := range []bool{false, true} {
				env, warnings, _, _ := buildAgentEnv(cfg, nil, nil, brokerMode)
				for _, e := range append(append([]string(nil), env...), warnings...) {
					if strings.Contains(e, peerSecret) {
						t.Errorf("brokerMode=%v: the peer secret value reached the agent: %q", brokerMode, strings.SplitN(e, "=", 2)[0])
					}
				}
				got := envToMap(env)
				for _, absent := range append([]string{"PEER_BRACED", "PEER_BARE"}, peerSecretEnvNames...) {
					if v, ok := got[absent]; ok && v != "" {
						t.Errorf("brokerMode=%v: %s is set in the agent env", brokerMode, absent)
					}
				}
			}

			for _, kv := range util.EnvWithoutAgentRefusedNames(os.Environ()) {
				if strings.Contains(kv, peerSecret) {
					t.Errorf("a broker-launched process would inherit the peer secret under %q", strings.SplitN(kv, "=", 2)[0])
				}
			}
		})
	}
}
