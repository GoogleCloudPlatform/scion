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
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// TestSettingsHC_BuildAgentEnv_Golden pins settings harness-config env
// behavior through resolveAuthEnvOverlayWithPolicy + buildAgentEnv and
// compares the pre-P3 shape (settings-hc also merged into finalScionCfg.Env
// underneath inline env) against the P3 shape (finalScionCfg.Env holds only
// inline env) to show that dropping settings-hc from persisted config is
// neutral for all non-auth-filtered keys.
func TestSettingsHC_BuildAgentEnv_Golden(t *testing.T) {
	settingsEnv := map[string]string{
		"SETTINGS_ONLY":             "from-settings",
		"SETTINGS_HUB_WIN":          "from-settings",
		"SETTINGS_INLINE_COLLIDE":   "from-settings",
		"SETTINGS_EMPTY":            "",
		"SETTINGS_EMPTY_HUB_FILLED": "",
	}
	settings := &config.VersionedSettings{
		SchemaVersion: "1",
		ActiveProfile: "local",
		Profiles: map[string]config.V1ProfileConfig{
			"local": {Runtime: "docker"},
		},
		HarnessConfigs: map[string]config.HarnessConfigEntry{
			"test-harness": {
				Harness: "generic",
				Env:     settingsEnv,
			},
		},
	}

	opts := api.StartOptions{
		Name:          "golden-settings-hc",
		HarnessConfig: "test-harness",
		Profile:       "local",
		BrokerMode:    true,
		Env: map[string]string{
			"HUB_ONLY":                  "from-hub",
			"SETTINGS_HUB_WIN":          "from-hub",
			"SETTINGS_EMPTY_HUB_FILLED": "from-hub",
		},
	}

	_, _, _, _ = resolveAuthEnvOverlayWithPolicy(&opts, settings, "local", "test-harness")

	inlineEnv := map[string]string{
		"INLINE_ONLY":             "from-inline",
		"SETTINGS_INLINE_COLLIDE": "from-inline",
	}
	// Pre-P3 shape: ProvisionAgent merged settings-hc underneath inline env
	// into scion-agent.json / finalScionCfg.Env.
	legacyMergedCfgEnv := maps.Clone(settingsEnv)
	maps.Copy(legacyMergedCfgEnv, inlineEnv)

	defaultEnv := map[string]string{
		"DEFAULT_ONLY": "from-default",
	}

	run := func(cfgEnv map[string]string) (map[string]string, []string) {
		envSlice, _, missingKeys, _ := buildAgentEnv(&api.ScionConfig{Env: cfgEnv}, opts.Env, defaultEnv, true)
		sort.Strings(missingKeys)
		return runEnvMap(envSlice), missingKeys
	}

	gotOld, missingOld := run(legacyMergedCfgEnv)
	gotNew, missingNew := run(inlineEnv)

	want := map[string]string{
		"DEFAULT_ONLY":              "from-default",
		"HUB_ONLY":                  "from-hub",
		"INLINE_ONLY":               "from-inline",
		"SETTINGS_EMPTY_HUB_FILLED": "from-hub",
		"SETTINGS_HUB_WIN":          "from-hub",
		"SETTINGS_INLINE_COLLIDE":   "from-settings",
		"SETTINGS_ONLY":             "from-settings",
	}
	wantMissing := []string{"SETTINGS_EMPTY"}

	if !maps.Equal(gotNew, want) {
		t.Errorf("new-shape buildAgentEnv env = %v, want %v", gotNew, want)
	}
	if !slices.Equal(missingNew, wantMissing) {
		t.Errorf("new-shape buildAgentEnv missingKeys = %v, want %v", missingNew, wantMissing)
	}
	if !maps.Equal(gotNew, gotOld) {
		t.Errorf("P3 shape diverged from pre-P3 shape: new=%v, old=%v", gotNew, gotOld)
	}
	if !slices.Equal(missingNew, missingOld) {
		t.Errorf("P3 missingKeys diverged from pre-P3 shape: new=%v, old=%v", missingNew, missingOld)
	}
}

// TestStart_SettingsHC_UnusedAuthKeyDropped pins the one auth-filter
// interaction of stopping settings-hc persistence: when Start's auth filter
// (run.go) drops an unused auth-candidate key (here ANTHROPIC_VERTEX_PROJECT_ID
// and empty CLOUD_ML_REGION under claude api-key auth) from opts.Env, it is no
// longer re-injected (or host-passed-through) from persisted scion-agent.json,
// while used auth keys and ordinary settings-hc keys still reach the container.
func TestStart_SettingsHC_UnusedAuthKeyDropped(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	t.Setenv("HOME", tmpDir)
	t.Setenv("CLOUD_ML_REGION", "host-region-should-not-pass-through")

	globalScionDir := filepath.Join(tmpDir, ".scion")
	hcDir := filepath.Join(globalScionDir, "harness-configs", "claude")
	if err := os.MkdirAll(hcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hcConfig := strings.Join([]string{
		"harness: claude",
		"user: scion",
		"image: test-image:latest",
		"provisioner:",
		"  type: container-script",
		"  interface_version: 1",
		"  command: [\"python3\", \"/home/scion/.scion/harness/provision.py\"]",
		"auth_selected_type: api-key",
		"auth:",
		"  default_type: api-key",
		"  types:",
		"    api-key:",
		"      required_env:",
		"        - any_of: [\"ANTHROPIC_API_KEY\"]",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte(hcConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hcDir, "provision.py"), []byte("#!/usr/bin/env python3\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	tplDir := filepath.Join(globalScionDir, "templates", "default")
	if err := os.MkdirAll(tplDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "claude"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	settings := strings.Join([]string{
		"schema_version: \"1\"",
		"active_profile: local",
		"profiles:",
		"  local:",
		"    runtime: docker",
		"harness_configs:",
		"  claude:",
		"    env:",
		"      ANTHROPIC_API_KEY: sk-from-settings",
		"      ANTHROPIC_VERTEX_PROJECT_ID: unused-vertex-proj",
		"      CLOUD_ML_REGION: \"\"",
		"      SETTINGS_REGULAR: regular-val",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(settings), 0o644); err != nil {
		t.Fatal(err)
	}

	projectScionDir := filepath.Join(tmpDir, "project", ".scion")
	if err := os.MkdirAll(projectScionDir, 0o755); err != nil {
		t.Fatal(err)
	}

	var captured runtime.RunConfig
	mgr := NewManager(&runtime.MockRuntime{
		ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
			return nil, nil
		},
		RunFunc: func(_ context.Context, cfg runtime.RunConfig) (string, error) {
			captured = cfg
			return "mock-id", nil
		},
	})

	if _, err := mgr.Start(context.Background(), api.StartOptions{
		Name:          "unused-auth-key-test",
		ProjectPath:   projectScionDir,
		Template:      "default",
		HarnessConfig: "claude",
		HarnessAuth:   "api-key",
		BrokerMode:    true,
	}); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	env := runEnvMap(captured.Env)
	if env["SETTINGS_REGULAR"] != "regular-val" {
		t.Errorf("SETTINGS_REGULAR = %q, want regular-val", env["SETTINGS_REGULAR"])
	}
	gotAPIKey := env["ANTHROPIC_API_KEY"]
	if gotAPIKey == "" && captured.ResolvedAuth != nil {
		gotAPIKey = captured.ResolvedAuth.EnvVars["ANTHROPIC_API_KEY"]
	}
	if gotAPIKey != "sk-from-settings" {
		t.Errorf("ANTHROPIC_API_KEY = %q, want sk-from-settings", gotAPIKey)
	}
	for _, unusedKey := range []string{"ANTHROPIC_VERTEX_PROJECT_ID", "CLOUD_ML_REGION"} {
		if v, ok := env[unusedKey]; ok {
			t.Errorf("unused auth-candidate key %s=%q unexpectedly reached RunConfig.Env", unusedKey, v)
		}
		if captured.ResolvedAuth != nil {
			if v, ok := captured.ResolvedAuth.EnvVars[unusedKey]; ok {
				t.Errorf("unused auth-candidate key %s=%q unexpectedly reached ResolvedAuth.EnvVars", unusedKey, v)
			}
		}
	}
}
