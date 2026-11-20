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
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/envpolicy"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

func TestBundleEnvLayer_LiteralValuesAndPolicy(t *testing.T) {
	t.Setenv("HOME", "/broker/host/home")
	t.Setenv("BAR", "broker-host-bar")

	raw := map[string]string{
		"FOO":                               "${HOME}/x",
		"DOLLAR_LETTER":                     "$HOME/y",
		"DOLLAR_DIGITS":                     "$100",
		"BAR":                               "",
		"PLAIN":                             "ok",
		"SCION_HOOK_WITHHELD_ENV":           "FORGED",
		"SCION_EXTRA_PATH":                  "/evil/bin",
		"SCION_LIVENESS_HARNESS_ROOT_DEPTH": "3",
		"BAD,KEY":                           "bad",
		" SPACED ":                          "bad",
	}

	entries, warnings := BundleEnvLayer(raw)

	wantEntries := map[string]string{
		"FOO":                           "${HOME}/x",
		"DOLLAR_LETTER":                 "$HOME/y",
		"DOLLAR_DIGITS":                 "$100",
		"BAR":                           "",
		"PLAIN":                         "ok",
		envpolicy.HintLivenessRootDepth: "3",
	}
	if !maps.Equal(entries, wantEntries) {
		t.Errorf("BundleEnvLayer entries = %v, want %v", entries, wantEntries)
	}

	joined := strings.Join(warnings, "\n")
	for _, wantSubstr := range []string{
		"harness-config env values are not expanded",
		"SCION_HOOK_WITHHELD_ENV",
		"SCION_EXTRA_PATH",
		"SCION_LIVENESS_HARNESS_ROOT_DEPTH",
		"BAD,KEY",
		" SPACED ",
	} {
		if !strings.Contains(joined, wantSubstr) {
			t.Errorf("expected warnings to mention %q; got:\n%s", wantSubstr, joined)
		}
	}
	if strings.Contains(joined, "DOLLAR_DIGITS") {
		t.Errorf("DOLLAR_DIGITS ($100) should not trigger variable-syntax warning; got:\n%s", joined)
	}
	if strings.Contains(joined, "/broker/host/home") || strings.Contains(joined, "broker-host-bar") {
		t.Errorf("warnings must never contain broker host values; got:\n%s", joined)
	}
}

func TestBuildAgentEnvWithBundle_LiteralAndHubFill(t *testing.T) {
	t.Setenv("HOME", "/broker/host/home")
	t.Setenv("BAR", "broker-host-bar")
	t.Setenv("HUB_FILLED", "broker-host-filled")

	bundle := map[string]string{
		"FOO":        "${HOME}/x",
		"BAR":        "",
		"HUB_FILLED": "",
	}
	extra := map[string]string{
		"HUB_FILLED": "from-hub",
	}

	envSlice, warnings, missingKeys, _, provenance := buildAgentEnvWithBundle(bundle, nil, extra, nil, true)
	sort.Strings(missingKeys)

	envMap := make(map[string]string, len(envSlice))
	for _, kv := range envSlice {
		if k, v, ok := strings.Cut(kv, "="); ok {
			envMap[k] = v
		}
	}

	if envMap["FOO"] != "${HOME}/x" {
		t.Errorf("FOO = %q, want literal %q", envMap["FOO"], "${HOME}/x")
	}
	if _, ok := envMap["BAR"]; ok {
		t.Errorf("BAR = %q, want omitted (must never read broker host env)", envMap["BAR"])
	}
	if envMap["HUB_FILLED"] != "from-hub" {
		t.Errorf("HUB_FILLED = %q, want %q", envMap["HUB_FILLED"], "from-hub")
	}
	if !slices.Equal(missingKeys, []string{"BAR"}) {
		t.Errorf("missingKeys = %v, want [BAR]", missingKeys)
	}
	if provenance["FOO"] != envOriginBundle {
		t.Errorf("provenance[FOO] = %v, want envOriginBundle", provenance["FOO"])
	}
	if provenance["HUB_FILLED"] != envOriginCaller {
		t.Errorf("provenance[HUB_FILLED] = %v, want envOriginCaller", provenance["HUB_FILLED"])
	}
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "harness-config env values are not expanded") {
		t.Errorf("expected not-expanded warning; got:\n%s", joined)
	}
	if !strings.Contains(joined, "'BAR' has no value") {
		t.Errorf("expected missing-key warning for BAR; got:\n%s", joined)
	}
}

func TestStart_BundleEnvNotPersistedAndReResolvedOnRestart(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	t.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	if err := os.MkdirAll(hcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeHC := func(bundleEnvYAML string) {
		t.Helper()
		content := strings.Join([]string{
			"harness: generic",
			"user: scion",
			"image: test-image:latest",
			"command:",
			"  base: [\"echo\"]",
			"env:",
			bundleEnvYAML,
			"",
		}, "\n")
		if err := os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeHC("  BUNDLE_KEY: v1\n  BUNDLE_LITERAL: \"${HOME}/sub\"")

	tplDir := filepath.Join(globalScionDir, "templates", "default")
	if err := os.MkdirAll(tplDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	settings := strings.Join([]string{
		"schema_version: \"1\"",
		"active_profile: local",
		"profiles:",
		"  local:",
		"    runtime: docker",
		"harness_configs:",
		"  test-harness:",
		"    image: test-image:latest",
		"    env:",
		"      SETTINGS_KEY: from-settings",
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

	startOpts := api.StartOptions{
		Name:          "persist-test",
		ProjectPath:   projectScionDir,
		Template:      "default",
		HarnessConfig: "test-harness",
		BrokerMode:    true,
		NoAuth:        true,
		HarnessAuth:   "none", // exercises Start's scion-agent.json rewrite path too
		InlineConfig: &api.ScionConfig{
			Env: map[string]string{
				"INLINE_OWN": "from-inline",
			},
		},
	}

	// First start.
	if _, err := mgr.Start(context.Background(), startOpts); err != nil {
		t.Fatalf("first Start failed: %v", err)
	}
	env1 := runEnvMap(captured.Env)
	if env1["BUNDLE_KEY"] != "v1" {
		t.Errorf("first start BUNDLE_KEY = %q, want v1", env1["BUNDLE_KEY"])
	}
	if env1["BUNDLE_LITERAL"] != "${HOME}/sub" {
		t.Errorf("first start BUNDLE_LITERAL = %q, want literal ${HOME}/sub", env1["BUNDLE_LITERAL"])
	}
	if env1["SETTINGS_KEY"] != "from-settings" {
		t.Errorf("first start SETTINGS_KEY = %q, want from-settings", env1["SETTINGS_KEY"])
	}
	if env1["INLINE_OWN"] != "from-inline" {
		t.Errorf("first start INLINE_OWN = %q, want from-inline", env1["INLINE_OWN"])
	}

	// Check scion-agent.json on disk: must NOT contain bundle or settings-hc env,
	// and MUST keep INLINE_OWN.
	agentJSONPath := filepath.Join(projectScionDir, "agents", "persist-test", "scion-agent.json")
	data, err := os.ReadFile(agentJSONPath)
	if err != nil {
		t.Fatalf("read scion-agent.json: %v", err)
	}
	var persisted api.ScionConfig
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("unmarshal scion-agent.json: %v", err)
	}
	if persisted.Env["INLINE_OWN"] != "from-inline" {
		t.Errorf("persisted INLINE_OWN = %q, want from-inline", persisted.Env["INLINE_OWN"])
	}
	for _, forbidden := range []string{"BUNDLE_KEY", "BUNDLE_LITERAL", "SETTINGS_KEY"} {
		if v, ok := persisted.Env[forbidden]; ok {
			t.Errorf("scion-agent.json must not persist %s=%q", forbidden, v)
		}
	}

	// Edit the harness-config bundle between starts and restart.
	writeHC("  BUNDLE_KEY: v2\n  BUNDLE_ADDED: fresh-on-restart")
	restartOpts := api.StartOptions{
		Name:          "persist-test",
		ProjectPath:   projectScionDir,
		Template:      "default",
		HarnessConfig: "test-harness",
		BrokerMode:    true,
		NoAuth:        true,
	}
	if _, err := mgr.Start(context.Background(), restartOpts); err != nil {
		t.Fatalf("restart Start failed: %v", err)
	}
	env2 := runEnvMap(captured.Env)
	if env2["BUNDLE_KEY"] != "v2" {
		t.Errorf("restart BUNDLE_KEY = %q, want v2", env2["BUNDLE_KEY"])
	}
	if env2["BUNDLE_ADDED"] != "fresh-on-restart" {
		t.Errorf("restart BUNDLE_ADDED = %q, want fresh-on-restart", env2["BUNDLE_ADDED"])
	}
	if _, ok := env2["BUNDLE_LITERAL"]; ok {
		t.Errorf("restart BUNDLE_LITERAL = %q, want absent after removal from bundle", env2["BUNDLE_LITERAL"])
	}
	if env2["INLINE_OWN"] != "from-inline" {
		t.Errorf("restart INLINE_OWN = %q, want from-inline", env2["INLINE_OWN"])
	}
	for _, wantWithheld := range []string{"BUNDLE_ADDED", "BUNDLE_KEY", "SETTINGS_KEY"} {
		if !slices.Contains(captured.HookWithheldEnv, wantWithheld) {
			t.Errorf("restart HookWithheldEnv = %v, missing %q", captured.HookWithheldEnv, wantWithheld)
		}
	}
	if slices.Contains(captured.HookWithheldEnv, "INLINE_OWN") {
		t.Errorf("restart HookWithheldEnv = %v, should not withhold INLINE_OWN", captured.HookWithheldEnv)
	}

	// Delete the harness-config dir so Start cannot re-resolve it on the next
	// restart: bundle env must NOT be delivered (no persisted bundle env) and
	// therefore cannot leak to hooks.
	if err := os.RemoveAll(hcDir); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Start(context.Background(), restartOpts); err != nil {
		t.Fatalf("restart with unresolved hcDir failed: %v", err)
	}
	env3 := runEnvMap(captured.Env)
	for _, removed := range []string{"BUNDLE_KEY", "BUNDLE_ADDED", "BUNDLE_LITERAL"} {
		if v, ok := env3[removed]; ok {
			t.Errorf("unresolved-hcDir restart unexpectedly delivered bundle key %s=%q", removed, v)
		}
	}
	if env3["INLINE_OWN"] != "from-inline" {
		t.Errorf("unresolved-hcDir restart lost INLINE_OWN = %q", env3["INLINE_OWN"])
	}
}

func TestStart_LegacyPersistedHarnessConfigEnvDemotedInMemory(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	t.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	if err := os.MkdirAll(hcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hcConfig := strings.Join([]string{
		"harness: generic",
		"user: scion",
		"image: test-image:latest",
		"command:",
		"  base: [\"echo\"]",
		"env:",
		"  BUNDLE_SAME: from-bundle",
		"  BUNDLE_STALE: \"${OLD_VAR}\"",
		"  SCION_EXTRA_PATH: /evil/from-bundle",
		"  SCION_LIVENESS_HARNESS_ROOT_DEPTH: \"2\"",
		"  HARNESS_HINT_AUTO_EXPOSE_PORTS: \"true\"",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte(hcConfig), 0o644); err != nil {
		t.Fatal(err)
	}

	tplDir := filepath.Join(globalScionDir, "templates", "default")
	if err := os.MkdirAll(tplDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	settings := strings.Join([]string{
		"schema_version: \"1\"",
		"active_profile: local",
		"profiles:",
		"  local:",
		"    runtime: docker",
		"harness_configs:",
		"  test-harness:",
		"    env:",
		"      SETTINGS_SAME: from-settings",
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

	// Provision agent normally, then rewrite scion-agent.json to simulate a
	// legacy pre-P3 file carrying baked-in bundle/settings env plus a stale
	// expanded bundle value and an inline key.
	if _, err := mgr.Start(context.Background(), api.StartOptions{
		Name:          "legacy-agent",
		ProjectPath:   projectScionDir,
		Template:      "default",
		HarnessConfig: "test-harness",
		BrokerMode:    true,
		NoAuth:        true,
	}); err != nil {
		t.Fatalf("initial Start failed: %v", err)
	}

	agentJSONPath := filepath.Join(projectScionDir, "agents", "legacy-agent", "scion-agent.json")
	rawBefore, err := os.ReadFile(agentJSONPath)
	if err != nil {
		t.Fatal(err)
	}
	var legacyCfg api.ScionConfig
	if err := json.Unmarshal(rawBefore, &legacyCfg); err != nil {
		t.Fatal(err)
	}
	legacyCfg.Env = map[string]string{
		"BUNDLE_SAME":                       "from-bundle",
		"BUNDLE_STALE":                      "expanded-old-val",
		"SETTINGS_SAME":                     "from-settings",
		"INLINE_OWN":                        "from-inline",
		"SCION_EXTRA_PATH":                  "/evil/from-bundle",
		"SCION_LIVENESS_HARNESS_ROOT_DEPTH": "2",
		"SCION_AUTO_EXPOSE_PORTS":           "true",
	}
	legacyBytes, err := json.MarshalIndent(&legacyCfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agentJSONPath, legacyBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	// Restart against the legacy file.
	if _, err := mgr.Start(context.Background(), api.StartOptions{
		Name:          "legacy-agent",
		ProjectPath:   projectScionDir,
		Template:      "default",
		HarnessConfig: "test-harness",
		BrokerMode:    true,
		NoAuth:        true,
	}); err != nil {
		t.Fatalf("legacy restart Start failed: %v", err)
	}

	// File on disk must remain untouched (demotion is in memory only).
	rawAfter, err := os.ReadFile(agentJSONPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(rawAfter) != string(legacyBytes) {
		t.Errorf("legacy scion-agent.json was modified on disk")
	}

	// Workload env keeps all expected values (including stale expanded value),
	// refuses legacy persisted SCION_EXTRA_PATH, and delivers the hints only
	// under HARNESS_HINT_* (not under the legacy SCION_* names).
	env := runEnvMap(captured.Env)
	if env["BUNDLE_SAME"] != "from-bundle" {
		t.Errorf("BUNDLE_SAME = %q, want from-bundle", env["BUNDLE_SAME"])
	}
	if env["BUNDLE_STALE"] != "expanded-old-val" {
		t.Errorf("BUNDLE_STALE = %q, want expanded-old-val", env["BUNDLE_STALE"])
	}
	if env["SETTINGS_SAME"] != "from-settings" {
		t.Errorf("SETTINGS_SAME = %q, want from-settings", env["SETTINGS_SAME"])
	}
	if env["INLINE_OWN"] != "from-inline" {
		t.Errorf("INLINE_OWN = %q, want from-inline", env["INLINE_OWN"])
	}
	if env[envpolicy.HintLivenessRootDepth] != "2" {
		t.Errorf("%s = %q, want 2", envpolicy.HintLivenessRootDepth, env[envpolicy.HintLivenessRootDepth])
	}
	if env[envpolicy.HintAutoExposePorts] != "true" {
		t.Errorf("%s = %q, want true", envpolicy.HintAutoExposePorts, env[envpolicy.HintAutoExposePorts])
	}
	for _, legacyScionKey := range []string{
		"SCION_EXTRA_PATH",
		"SCION_LIVENESS_HARNESS_ROOT_DEPTH",
		"SCION_AUTO_EXPOSE_PORTS",
	} {
		if v, ok := env[legacyScionKey]; ok {
			t.Errorf("legacy persisted key %s=%q survived as own env instead of being demoted", legacyScionKey, v)
		}
	}

	// All harness-config keys (including stale BUNDLE_STALE and rewritten/migrated
	// hints) are withheld from hooks; INLINE_OWN is not.
	wantWithheld := []string{
		"BUNDLE_SAME",
		"BUNDLE_STALE",
		envpolicy.HintAutoExposePorts,
		envpolicy.HintLivenessRootDepth,
		"SETTINGS_SAME",
	}
	if !slices.Equal(captured.HookWithheldEnv, wantWithheld) {
		t.Errorf("HookWithheldEnv = %v, want %v", captured.HookWithheldEnv, wantWithheld)
	}
}

func runEnvMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}
	return m
}
