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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// For a hub-dispatched (broker-mode) agent the git token remote comes only
// from the broker's env (the hub's GitClone): a value in the agent's config
// layer or a harness-config entry is dropped, so neither can bind the git
// token to another remote. Without the broker's value the container gets
// none and the clone gets no token.
func TestTokenRemote_BrokerModeOnlyFromHub(t *testing.T) {
	const otherRemote = "mirror.example/org/repo"
	const recorded = "github.com/org/repo"
	cfg := &api.ScionConfig{Env: map[string]string{api.EnvGitTokenRemote: otherRemote}}

	env, _, _, dropped := buildAgentEnv(cfg, nil, nil, true)
	if v, ok := envListToMap(env)[api.EnvGitTokenRemote]; ok {
		t.Errorf("config-layer token remote reached the container: %q", v)
	}
	if len(dropped) != 1 || dropped[0].Key != api.EnvGitTokenRemote {
		t.Errorf("dropped = %+v, want the config-layer token remote", dropped)
	}

	env, _, _, _ = buildAgentEnv(cfg, map[string]string{api.EnvGitTokenRemote: recorded}, nil, true)
	if got := envListToMap(env)[api.EnvGitTokenRemote]; got != recorded {
		t.Errorf("token remote = %q, want the broker's %q", got, recorded)
	}

	settings := &config.VersionedSettings{
		HarnessConfigs: map[string]config.HarnessConfigEntry{
			"claude-cfg": {Harness: "claude", Env: map[string]string{api.EnvGitTokenRemote: otherRemote}},
		},
	}
	opts := api.StartOptions{BrokerMode: true}
	_, hcDropped := resolveAuthEnvOverlay(&opts, settings, "", "claude-cfg")
	if v, ok := opts.Env[api.EnvGitTokenRemote]; ok {
		t.Errorf("harness-config token remote merged into opts.Env: %q", v)
	}
	if len(hcDropped) != 1 || hcDropped[0].Key != api.EnvGitTokenRemote {
		t.Errorf("harness-config dropped = %+v, want the token remote", hcDropped)
	}
}
