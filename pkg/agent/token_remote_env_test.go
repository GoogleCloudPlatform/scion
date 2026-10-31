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
	"bytes"
	"log/slog"
	"strings"
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

// For a hub-dispatched agent the in-container clone URL comes only from the
// broker's env: a config-layer or harness-config value is dropped.
func TestCloneURL_BrokerModeOnlyFromBroker(t *testing.T) {
	const otherURL = "file:///srv/other/repo"
	env, _, _, dropped := buildAgentEnv(&api.ScionConfig{Env: map[string]string{"SCION_GIT_CLONE_URL": otherURL}}, nil, nil, true)
	if v, ok := envListToMap(env)["SCION_GIT_CLONE_URL"]; ok {
		t.Errorf("config-layer clone URL reached the container: %q", v)
	}
	if len(dropped) != 1 || dropped[0].Key != "SCION_GIT_CLONE_URL" {
		t.Errorf("dropped = %+v, want the config-layer clone URL", dropped)
	}
	settings := &config.VersionedSettings{
		HarnessConfigs: map[string]config.HarnessConfigEntry{
			"claude-cfg": {Harness: "claude", Env: map[string]string{"SCION_GIT_CLONE_URL": otherURL}},
		},
	}
	opts := api.StartOptions{BrokerMode: true}
	_, _ = resolveAuthEnvOverlay(&opts, settings, "", "claude-cfg")
	if v, ok := opts.Env["SCION_GIT_CLONE_URL"]; ok {
		t.Errorf("harness-config clone URL merged into opts.Env: %q", v)
	}
}

// A dropped hub-only value is never echoed: the log line and the warning
// name the key and the layer only. The git keys point to the project's git
// settings on the hub, not to a hub environment variable.
func TestWarnDroppedBrokerEnv_NeverLogsValue(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	const cloneURL = "https://mirror.example/org/repo.git"
	const tokenRemote = "mirror.example/org/repo"
	warnings := warnDroppedBrokerEnv("agent-1", nil, []droppedBrokerEnv{
		{Key: "SCION_GIT_CLONE_URL", Value: cloneURL, Layer: envLayerConfig},
		{Key: api.EnvGitTokenRemote, Value: tokenRemote, Layer: envLayerHarnessConfigEntry},
		{Key: "TZ", Value: "Europe/Paris", Layer: envLayerConfig},
	})
	if len(warnings) != 3 {
		t.Fatalf("want 3 warnings, got %v", warnings)
	}
	logged := buf.String()
	for _, v := range []string{cloneURL, tokenRemote, "mirror.example", "Europe/Paris"} {
		if strings.Contains(logged, v) {
			t.Errorf("the log echoes a dropped value %q:\n%s", v, logged)
		}
		for _, w := range warnings {
			if strings.Contains(w, v) {
				t.Errorf("warning %q echoes a dropped value %q", w, v)
			}
		}
	}
	for _, key := range []string{"SCION_GIT_CLONE_URL", api.EnvGitTokenRemote, "TZ"} {
		if !strings.Contains(logged, key) {
			t.Errorf("the log does not name the dropped key %s:\n%s", key, logged)
		}
	}
	for i, w := range warnings[:2] {
		if !strings.Contains(w, "the project's git settings on the hub") || strings.Contains(w, "hub environment variable") {
			t.Errorf("warning %d guidance = %q, want the project's git settings", i, w)
		}
	}
	if !strings.Contains(warnings[2], "hub environment variable") {
		t.Errorf("TZ guidance = %q, want a hub environment variable", warnings[2])
	}
}
