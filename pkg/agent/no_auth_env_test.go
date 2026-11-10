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
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/stagedsecrets"
)

const testNoSessionBus = "unix:path=/home/scion/.scion/harness/no-session-bus"

func TestNoAuthModeActive(t *testing.T) {
	cases := []struct {
		name   string
		noAuth bool
		cfg    *config.HarnessNoAuthConfig
		want   bool
	}{
		{"drop-to-shell", true, &config.HarnessNoAuthConfig{Behavior: "drop-to-shell"}, true},
		{"allow", true, &config.HarnessNoAuthConfig{Behavior: "allow"}, true},
		{"not requested", false, &config.HarnessNoAuthConfig{Behavior: "drop-to-shell"}, false},
		{"no no_auth block", true, nil, false},
		{"other behavior", true, &config.HarnessNoAuthConfig{Behavior: "show-setup-instructions"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := noAuthModeActive(tc.noAuth, tc.cfg); got != tc.want {
				t.Errorf("noAuthModeActive = %t, want %t", got, tc.want)
			}
		})
	}
}

// TestApplyNoAuthEnv_ReservedNamesDropped (HC7): no_auth.env goes through the
// same reserved-name filter as env-type secrets: SCION_* (including the
// staged-token marker), GCE_METADATA_* and hub-only keys are dropped with a
// warning and never reach the container env.
func TestApplyNoAuthEnv_ReservedNamesDropped(t *testing.T) {
	reserved := []string{
		"SCION_HUB_ENDPOINT",
		stagedsecrets.HubTokenStagedEnvVar,
		"GCE_METADATA_HOST",
		"TZ",
		api.EnvGitTokenRemote,
		"SCION_GIT_CLONE_URL",
	}
	cfg := &config.HarnessNoAuthConfig{Behavior: "drop-to-shell", Env: map[string]string{
		"DBUS_SESSION_BUS_ADDRESS": testNoSessionBus,
	}}
	for _, k := range reserved {
		cfg.Env[k] = "from-no-auth-env"
	}

	env, warnings := applyNoAuthEnv([]string{"PATH=/usr/bin"}, cfg, true, "agent")

	if v, ok := envValue(env, "DBUS_SESSION_BUS_ADDRESS"); !ok || v != testNoSessionBus {
		t.Errorf("DBUS_SESSION_BUS_ADDRESS = %q (set=%t), want %q", v, ok, testNoSessionBus)
	}
	for _, k := range reserved {
		if v, ok := envValue(env, k); ok {
			t.Errorf("reserved key %s reached the env with %q", k, v)
		}
		if !slices.ContainsFunc(warnings, func(w string) bool { return strings.Contains(w, " sets "+k+", ") }) {
			t.Errorf("no warning names reserved key %s; warnings: %v", k, warnings)
		}
	}
}

func TestApplyNoAuthEnv(t *testing.T) {
	cfg := &config.HarnessNoAuthConfig{Behavior: "drop-to-shell", Env: map[string]string{
		"DBUS_SESSION_BUS_ADDRESS": testNoSessionBus,
		"EMPTY":                    "",
		"KEPT":                     "from-no-auth-env",
	}}

	t.Run("inactive leaves env unchanged", func(t *testing.T) {
		in := []string{"PATH=/usr/bin"}
		env, warnings := applyNoAuthEnv(in, cfg, false, "agent")
		if !slices.Equal(env, in) || warnings != nil {
			t.Errorf("got env %v warnings %v, want %v and none", env, warnings, in)
		}
	})

	t.Run("existing key wins", func(t *testing.T) {
		env, _ := applyNoAuthEnv([]string{"KEPT=from-settings"}, cfg, true, "agent")
		if v, _ := envValue(env, "KEPT"); v != "from-settings" {
			t.Errorf("KEPT = %q, want the value already set", v)
		}
		if n := strings.Count(strings.Join(env, "\n"), "KEPT="); n != 1 {
			t.Errorf("KEPT appears %d times, want 1: %v", n, env)
		}
	})

	t.Run("empty value dropped with warning", func(t *testing.T) {
		env, warnings := applyNoAuthEnv(nil, cfg, true, "agent")
		if _, ok := envValue(env, "EMPTY"); ok {
			t.Error("EMPTY reached the env")
		}
		if !slices.ContainsFunc(warnings, func(w string) bool { return strings.Contains(w, " sets EMPTY to ") }) {
			t.Errorf("no warning for EMPTY: %v", warnings)
		}
	})
}

// TestNoAuthEnv_OnlyInNoAuthMode (HC3): through Start, the env of a harness
// config's no-auth-env.yaml is in the container env for a no-auth start and
// absent for api-key and auth-file starts of the same harness config, and a
// bundle without the file adds no env. Dropping the merge in Start (mutant
// M-H2) fails the no-auth case.
func TestNoAuthEnv_OnlyInNoAuthMode(t *testing.T) {
	cases := []struct {
		name        string
		harnessAuth string
		env         map[string]string
		secrets     []api.ResolvedSecret
		noSidecar   bool
		wantNoAuth  bool
		wantBus     string // "" means absent
	}{
		{
			name:        "no-auth",
			harnessAuth: "none",
			wantNoAuth:  true,
			wantBus:     testNoSessionBus,
		},
		{
			name:        "no-auth keeps an explicit value",
			harnessAuth: "none",
			env:         map[string]string{"DBUS_SESSION_BUS_ADDRESS": "unix:path=/run/user/1000/bus"},
			wantNoAuth:  true,
			wantBus:     "unix:path=/run/user/1000/bus",
		},
		{
			name:        "no-auth without no-auth-env.yaml",
			harnessAuth: "none",
			noSidecar:   true,
			wantNoAuth:  true,
		},
		{
			name: "api-key",
			env:  map[string]string{"COPILOT_GITHUB_TOKEN": "test-token"},
		},
		{
			name:        "auth-file",
			harnessAuth: "auth-file",
			secrets: []api.ResolvedSecret{
				{Name: "COPILOT_CONFIG", Type: "file", Target: "~/.copilot/config.json", Value: `{"logged_in_users":[]}`},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			t.Chdir(tmpDir)
			t.Setenv("HOME", tmpDir)

			globalScionDir := filepath.Join(tmpDir, ".scion")
			hcDir := filepath.Join(globalScionDir, "harness-configs", "noauth-env-test")
			if err := os.MkdirAll(hcDir, 0755); err != nil {
				t.Fatal(err)
			}
			hcYAML := `harness: noauth-env-test
user: scion
image: test-image:latest
provisioner:
  type: container-script
  command: ["python3", "provision.py"]
no_auth:
  behavior: drop-to-shell
  message: sign in
auth:
  default_type: api-key
  types:
    api-key:
      required_env:
        - any_of: ["COPILOT_GITHUB_TOKEN"]
    auth-file:
      required_files:
        - name: COPILOT_CONFIG
          type: file
          target_suffix: "/.copilot/config.json"
          field: CopilotConfigFile
  autodetect:
    env:
      COPILOT_GITHUB_TOKEN: api-key
    files:
      COPILOT_CONFIG: auth-file
`
			if err := os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte(hcYAML), 0644); err != nil {
				t.Fatal(err)
			}
			if !tc.noSidecar {
				sidecar := "env:\n  DBUS_SESSION_BUS_ADDRESS: " + testNoSessionBus + "\n"
				if err := os.WriteFile(filepath.Join(hcDir, config.NoAuthEnvSidecarFile), []byte(sidecar), 0644); err != nil {
					t.Fatal(err)
				}
			}
			tplDir := filepath.Join(globalScionDir, "templates", "default")
			if err := os.MkdirAll(tplDir, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "noauth-env-test"}`), 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`), 0644); err != nil {
				t.Fatal(err)
			}

			projectScionDir := filepath.Join(tmpDir, "project", ".scion")
			agentName := "noauth-env-agent"
			agentDir := filepath.Join(projectScionDir, "agents", agentName)
			if err := os.MkdirAll(filepath.Join(agentDir, "home"), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{"harness_config": "noauth-env-test"}`), 0644); err != nil {
				t.Fatal(err)
			}

			var captured runtime.RunConfig
			mockRT := &runtime.MockRuntime{
				ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
					return []api.AgentInfo{}, nil
				},
				RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
					captured = cfg
					return "mock-id", nil
				},
			}

			if _, err := NewManager(mockRT).Start(context.Background(), api.StartOptions{
				Name:            agentName,
				ProjectPath:     projectScionDir,
				BrokerMode:      true,
				HarnessAuth:     tc.harnessAuth,
				Env:             tc.env,
				ResolvedSecrets: tc.secrets,
			}); err != nil {
				t.Fatalf("Start failed: %v", err)
			}

			if captured.NoAuth != tc.wantNoAuth {
				t.Errorf("RunConfig.NoAuth = %t, want %t", captured.NoAuth, tc.wantNoAuth)
			}
			got, set := envValue(captured.Env, "DBUS_SESSION_BUS_ADDRESS")
			switch {
			case tc.wantBus == "" && set:
				t.Errorf("DBUS_SESSION_BUS_ADDRESS = %q in a %s start, want absent", got, tc.name)
			case tc.wantBus != "" && got != tc.wantBus:
				t.Errorf("DBUS_SESSION_BUS_ADDRESS = %q (set=%t), want %q", got, set, tc.wantBus)
			}
		})
	}
}
