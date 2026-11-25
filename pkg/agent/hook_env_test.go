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
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/envpolicy"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

func TestSanitizeHarnessConfigEnv(t *testing.T) {
	raw := map[string]string{
		"ORDINARY_KEY":                      "ok",
		"PATH=/opt/custom":                  ":/usr/bin:/bin",
		"BAD,KEY":                           "comma",
		" SPACED ":                          "spaces",
		"SCION_HOOK_WITHHELD_ENV":           "FORGED",
		"SCION_EXTRA_PATH":                  "/evil/bin",
		"SCION_AGENT_NAME":                  "spoofed",
		"SCION_${UNSET_TEST_VAR}SMUGGLED":   "bad",
		"SCION_LIVENESS_HARNESS_ROOT_DEPTH": "2",
		"SCION_AUTO_EXPOSE_PORTS":           "true",
		"SCION_AUTO_EXPOSE_MODE":            "denylist",
		envpolicy.HintAutoExposeMode:        "allowlist", // explicit hint must not be clobbered by legacy rewrite
	}

	got, warnings := sanitizeHarnessConfigEnv(raw, "bundle", true)

	if got["ORDINARY_KEY"] != "ok" {
		t.Errorf("ORDINARY_KEY = %q, want %q", got["ORDINARY_KEY"], "ok")
	}
	if got[envpolicy.HintLivenessRootDepth] != "2" {
		t.Errorf("%s = %q, want %q", envpolicy.HintLivenessRootDepth, got[envpolicy.HintLivenessRootDepth], "2")
	}
	if got[envpolicy.HintAutoExposePorts] != "true" {
		t.Errorf("%s = %q, want %q", envpolicy.HintAutoExposePorts, got[envpolicy.HintAutoExposePorts], "true")
	}
	if got[envpolicy.HintAutoExposeMode] != "allowlist" {
		t.Errorf("%s = %q, want explicit %q preserved over legacy rewrite", envpolicy.HintAutoExposeMode, got[envpolicy.HintAutoExposeMode], "allowlist")
	}
	for _, refused := range []string{
		"PATH=/opt/custom",
		"BAD,KEY",
		" SPACED ",
		"SCION_HOOK_WITHHELD_ENV",
		"SCION_EXTRA_PATH",
		"SCION_AGENT_NAME",
		"SCION_SMUGGLED",
		"SCION_${UNSET_TEST_VAR}SMUGGLED",
		"SCION_LIVENESS_HARNESS_ROOT_DEPTH",
		"SCION_AUTO_EXPOSE_PORTS",
		"SCION_AUTO_EXPOSE_MODE",
	} {
		if _, ok := got[refused]; ok {
			t.Errorf("sanitized map unexpectedly contains %q", refused)
		}
	}
	// 3 invalid + 4 refused + 3 rewritten = 10 warnings.
	if len(warnings) != 10 {
		t.Errorf("len(warnings) = %d, want 10; warnings = %v", len(warnings), warnings)
	}
}

func TestTrackedAgentEnv_AllLayersAndPrecedence(t *testing.T) {
	bundleEnv := map[string]string{
		"BUNDLE_ONLY":            "from-bundle",
		"GITHUB_TOKEN":           "",
		"INLINE_WIN":             "from-bundle",
		"LEGACY_STALE":           "new-bundle-val",
		"MY${UNSET_TEST_VAR}KEY": "expanded-key-val",
	}
	settingsHCEnv := map[string]string{
		"SETTINGS_ONLY":    "from-settings",
		"SETTINGS_HUB_WIN": "from-settings",
		"SETTINGS_EMPTY":   "",
	}
	inlineEnv := map[string]string{
		"INLINE_WIN": "from-inline",
	}
	callerEnv := map[string]string{
		"GITHUB_TOKEN":     "ghp_from_hub",
		"SETTINGS_HUB_WIN": "from-hub",
		"OPERATOR_ONLY":    "keep",
	}

	optsEnv := make(map[string]string, len(callerEnv)+len(settingsHCEnv))
	for k, v := range callerEnv {
		optsEnv[k] = v
	}
	for k, v := range settingsHCEnv {
		if _, exists := optsEnv[k]; !exists {
			optsEnv[k] = v
		}
	}

	scionCfg := &api.ScionConfig{
		Env: map[string]string{
			"INLINE_WIN":   "from-inline",
			"LEGACY_STALE": "old-persisted-val",
			"OWN_PERSIST":  "from-own",
		},
	}
	defaultEnv := map[string]string{
		"DEFAULT_ONLY": "from-default",
	}

	tracked, _, _, _ := buildTrackedAgentEnv(bundleEnv, scionCfg, optsEnv, settingsHCEnv, callerEnv, inlineEnv, defaultEnv, true, false)

	// Post-build mutations: no-auth sidecar (HC) and broker-system override.
	noAuthCfg := &config.HarnessNoAuthConfig{
		Behavior: "drop-to-shell",
		Env: map[string]string{
			"NO_AUTH_ONLY":  "from-no-auth",
			"OPERATOR_ONLY": "no-auth-loses",
		},
	}
	applyNoAuthEnvTracked(tracked, noAuthCfg, true, "test-agent")
	tracked.Set(envLaunchID, "run-123", envOriginBrokerSystem)

	wantWithheld := []string{
		"BUNDLE_ONLY",
		"LEGACY_STALE",
		"MY${UNSET_TEST_VAR}KEY",
		"NO_AUTH_ONLY",
		"SETTINGS_ONLY",
	}
	gotWithheld := tracked.HarnessConfigKeys()
	if !slices.Equal(gotWithheld, wantWithheld) {
		t.Errorf("HarnessConfigKeys() = %v, want %v", gotWithheld, wantWithheld)
	}

	// Verify broker-system overwrite clears withholding and Delete removes it.
	tracked.Set("NO_AUTH_ONLY", "overridden-by-broker", envOriginBrokerSystem)
	if slices.Contains(tracked.HarnessConfigKeys(), "NO_AUTH_ONLY") {
		t.Errorf("NO_AUTH_ONLY remained withheld after envOriginBrokerSystem overwrite")
	}
	tracked.Delete("BUNDLE_ONLY")
	if slices.Contains(tracked.HarnessConfigKeys(), "BUNDLE_ONLY") {
		t.Errorf("BUNDLE_ONLY remained withheld after Delete")
	}
}

func TestStart_HookWithheldEnv_EndToEnd(t *testing.T) {
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
		"  BUNDLE_ONLY: from-bundle",
		"  \"BPATH=/opt/bundle\": \":/usr/bin\"",
		"  COLLIDE_KEY: from-bundle",
		"  GITHUB_TOKEN: \"\"",
		"  GOOGLE_CLOUD_PROJECT: bundle-proj",
		"  INLINE_WIN: from-bundle",
		"  \"MY${UNSET_TEST_VAR}KEY\": from-expanded-key",
		"  SCION_HOOK_WITHHELD_ENV: FORGED_BUNDLE",
		"  SCION_EXTRA_PATH: /evil/bundle",
		"  SCION_LIVENESS_HARNESS_ROOT_DEPTH: \"2\"",
		"env_template:",
		"  TPL_ONLY: \"agent-{{ .AgentName }}\"",
		"  \"PATH=/opt/custom\": \":/usr/bin:/bin\"",
		"  SCION_HOOK_WITHHELD_ENV: FORGED_TPL",
		"  SCION_EXTRA_PATH: /evil/tpl",
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

	// Settings harness_configs.<name>.env collides with bundle COLLIDE_KEY,
	// adds SETTINGS_ONLY, and attempts to set reserved SCION_* keys.
	// If callerEnv were snapshotted after resolveAuthEnvOverlay, COLLIDE_KEY
	// and SETTINGS_ONLY would look caller-supplied and drop out of HookWithheldEnv.
	settings := strings.Join([]string{
		"schema_version: \"1\"",
		"active_profile: local",
		"profiles:",
		"  local:",
		"    runtime: docker",
		"harness_configs:",
		"  test-harness:",
		"    env:",
		"      COLLIDE_KEY: from-settings",
		"      \"PATHS=/opt/settings\": \":/usr/bin\"",
		"      SETTINGS_ONLY: from-settings",
		"      SCION_HOOK_WITHHELD_ENV: FORGED_SETTINGS",
		"      SCION_EXTRA_PATH: /evil/settings",
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

	info, err := mgr.Start(context.Background(), api.StartOptions{
		Name:          "hook-env-test",
		ProjectPath:   projectScionDir,
		Template:      "default",
		HarnessConfig: "test-harness",
		BrokerMode:    true,
		NoAuth:        true,
		Env: map[string]string{
			"GITHUB_TOKEN":  "ghp_from_hub",
			"OPERATOR_ONLY": "operator-val",
		},
		InlineConfig: &api.ScionConfig{
			Env: map[string]string{
				"INLINE_WIN": "from-inline",
			},
		},
		ResolvedSecrets: []api.ResolvedSecret{
			{Type: "environment", Target: "GOOGLE_CLOUD_PROJECT", Value: "secret-proj"},
		},
	})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// RunConfig.HookWithheldEnv carries the broker-side RunConfig.Env harness-config
	// provenance; env_template keys (TPL_ONLY) are tracked directly by each runtime
	// from Harness.GetEnv() during emission.
	wantWithheld := []string{
		"BUNDLE_ONLY",
		"COLLIDE_KEY",
		"GOOGLE_CLOUD_PROJECT",
		envpolicy.HintLivenessRootDepth,
		"MY${UNSET_TEST_VAR}KEY",
		"SETTINGS_ONLY",
	}
	if !slices.Equal(captured.HookWithheldEnv, wantWithheld) {
		t.Errorf("RunConfig.HookWithheldEnv = %v, want %v (RunConfig.Env=%v)", captured.HookWithheldEnv, wantWithheld, captured.Env)
	}

	envMap := make(map[string]string, len(captured.Env))
	for _, e := range captured.Env {
		if k, v, ok := strings.Cut(e, "="); ok {
			envMap[k] = v
		}
	}
	if envMap["BUNDLE_ONLY"] != "from-bundle" {
		t.Errorf("RunConfig.Env[BUNDLE_ONLY] = %q, want %q", envMap["BUNDLE_ONLY"], "from-bundle")
	}
	if envMap["COLLIDE_KEY"] != "from-settings" {
		t.Errorf("RunConfig.Env[COLLIDE_KEY] = %q, want %q", envMap["COLLIDE_KEY"], "from-settings")
	}
	if envMap["SETTINGS_ONLY"] != "from-settings" {
		t.Errorf("RunConfig.Env[SETTINGS_ONLY] = %q, want %q", envMap["SETTINGS_ONLY"], "from-settings")
	}
	if envMap["GOOGLE_CLOUD_PROJECT"] != "bundle-proj" {
		t.Errorf("RunConfig.Env[GOOGLE_CLOUD_PROJECT] = %q, want %q", envMap["GOOGLE_CLOUD_PROJECT"], "bundle-proj")
	}
	if envMap["GITHUB_TOKEN"] != "ghp_from_hub" {
		t.Errorf("RunConfig.Env[GITHUB_TOKEN] = %q, want %q", envMap["GITHUB_TOKEN"], "ghp_from_hub")
	}
	if envMap["INLINE_WIN"] != "from-inline" {
		t.Errorf("RunConfig.Env[INLINE_WIN] = %q, want %q", envMap["INLINE_WIN"], "from-inline")
	}
	if envMap["MY${UNSET_TEST_VAR}KEY"] != "from-expanded-key" {
		t.Errorf("RunConfig.Env[MY${UNSET_TEST_VAR}KEY] = %q, want %q", envMap["MY${UNSET_TEST_VAR}KEY"], "from-expanded-key")
	}
	if envMap[envpolicy.HintLivenessRootDepth] != "2" {
		t.Errorf("RunConfig.Env[%s] = %q, want %q", envpolicy.HintLivenessRootDepth, envMap[envpolicy.HintLivenessRootDepth], "2")
	}
	for _, refused := range []string{
		"SCION_HOOK_WITHHELD_ENV",
		"SCION_EXTRA_PATH",
		"SCION_LIVENESS_HARNESS_ROOT_DEPTH",
		"BPATH",
		"BPATH=/opt/bundle",
		"PATH",
		"PATH=/opt/custom",
		"PATHS",
		"PATHS=/opt/settings",
	} {
		if v, ok := envMap[refused]; ok {
			t.Errorf("RunConfig.Env unexpectedly contains refused/invalid key %s=%q", refused, v)
		}
	}
	if captured.Harness == nil {
		t.Fatalf("captured.Harness is nil")
	}
	tplEnv := captured.Harness.GetEnv("hook-env-test", "/home/scion", "scion")
	if tplEnv["TPL_ONLY"] != "agent-hook-env-test" {
		t.Errorf("Harness.GetEnv()[TPL_ONLY] = %q, want %q", tplEnv["TPL_ONLY"], "agent-hook-env-test")
	}
	for _, badKey := range []string{"PATH", "PATH=/opt/custom"} {
		if v, ok := tplEnv[badKey]; ok {
			t.Errorf("Harness.GetEnv() unexpectedly contains invalid key %s=%q", badKey, v)
		}
	}

	// Verify start warnings were recorded for refused SCION_* keys and legacy rewrite.
	joinedWarnings := strings.Join(info.Warnings, "\n")
	for _, substr := range []string{"SCION_HOOK_WITHHELD_ENV", "SCION_EXTRA_PATH", "SCION_LIVENESS_HARNESS_ROOT_DEPTH", envpolicy.HintLivenessRootDepth} {
		if !strings.Contains(joinedWarnings, substr) {
			t.Errorf("expected start warnings to mention %q; got:\n%s", substr, joinedWarnings)
		}
	}
}

// TestStart_HookWithheldEnv_NoAuthSidecar verifies that keys added from a
// bundle's no-auth-env.yaml sidecar (including PATH) are recorded with
// harness-config provenance and appear in RunConfig.HookWithheldEnv in no-auth
// mode, while caller overrides stay visible and non-no-auth starts omit them.
func TestStart_HookWithheldEnv_NoAuthSidecar(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	t.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	hcDir := filepath.Join(globalScionDir, "harness-configs", "noauth-hc")
	if err := os.MkdirAll(hcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hcYAML := strings.Join([]string{
		"harness: generic",
		"user: scion",
		"image: test-image:latest",
		"command:",
		"  base: [\"echo\"]",
		"no_auth:",
		"  behavior: drop-to-shell",
		"  message: sign in",
		"auth:",
		"  default_type: api-key",
		"  types:",
		"    api-key:",
		"      required_env:",
		"        - any_of: [\"TEST_API_KEY\"]",
		"  autodetect:",
		"    env:",
		"      TEST_API_KEY: api-key",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte(hcYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	sidecar := strings.Join([]string{
		"env:",
		"  PATH: /workspace/bin:/usr/bin",
		"  DBUS_SESSION_BUS_ADDRESS: unix:path=/tmp/no-bus",
		"  CALLER_WINS: from-no-auth",
		"  \"BAD,KEY\": comma",
		"  SCION_RESERVED: bad",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(hcDir, config.NoAuthEnvSidecarFile), []byte(sidecar), 0o644); err != nil {
		t.Fatal(err)
	}

	tplDir := filepath.Join(globalScionDir, "templates", "default")
	if err := os.MkdirAll(tplDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "noauth-hc"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte("schema_version: \"1\"\nactive_profile: local\nprofiles:\n  local:\n    runtime: docker\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	projectScionDir := filepath.Join(tmpDir, "project", ".scion")
	if err := os.MkdirAll(projectScionDir, 0o755); err != nil {
		t.Fatal(err)
	}

	var captured runtime.RunConfig
	mgr := NewManager(&runtime.MockRuntime{
		ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil },
		RunFunc: func(_ context.Context, cfg runtime.RunConfig) (string, error) {
			captured = cfg
			return "mock-id", nil
		},
	})

	t.Run("no-auth active withholds sidecar keys", func(t *testing.T) {
		info, err := mgr.Start(context.Background(), api.StartOptions{
			Name:          "noauth-agent",
			ProjectPath:   projectScionDir,
			Template:      "default",
			HarnessConfig: "noauth-hc",
			BrokerMode:    true,
			HarnessAuth:   "none",
			Env: map[string]string{
				"CALLER_WINS": "from-caller",
			},
		})
		if err != nil {
			t.Fatalf("Start failed: %v", err)
		}
		envMap := runEnvMap(captured.Env)
		if envMap["PATH"] != "/workspace/bin:/usr/bin" {
			t.Errorf("Env[PATH] = %q, want /workspace/bin:/usr/bin", envMap["PATH"])
		}
		if envMap["DBUS_SESSION_BUS_ADDRESS"] != "unix:path=/tmp/no-bus" {
			t.Errorf("Env[DBUS_SESSION_BUS_ADDRESS] = %q, want unix:path=/tmp/no-bus", envMap["DBUS_SESSION_BUS_ADDRESS"])
		}
		if envMap["CALLER_WINS"] != "from-caller" {
			t.Errorf("Env[CALLER_WINS] = %q, want from-caller", envMap["CALLER_WINS"])
		}
		if _, ok := envMap["BAD,KEY"]; ok {
			t.Errorf("invalid key BAD,KEY reached RunConfig.Env")
		}
		wantWithheld := []string{"DBUS_SESSION_BUS_ADDRESS", "PATH"}
		if !slices.Equal(captured.HookWithheldEnv, wantWithheld) {
			t.Errorf("RunConfig.HookWithheldEnv = %v, want %v", captured.HookWithheldEnv, wantWithheld)
		}
		joined := strings.Join(info.Warnings, "\n")
		if !strings.Contains(joined, "BAD,KEY") {
			t.Errorf("expected warning for invalid key BAD,KEY; got:\n%s", joined)
		}
	})

	t.Run("api-key start omits sidecar keys", func(t *testing.T) {
		_, err := mgr.Start(context.Background(), api.StartOptions{
			Name:          "apikey-agent",
			ProjectPath:   projectScionDir,
			Template:      "default",
			HarnessConfig: "noauth-hc",
			BrokerMode:    true,
			Env: map[string]string{
				"TEST_API_KEY": "sk-test",
			},
		})
		if err != nil {
			t.Fatalf("Start failed: %v", err)
		}
		envMap := runEnvMap(captured.Env)
		for _, k := range []string{"PATH", "DBUS_SESSION_BUS_ADDRESS"} {
			if _, ok := envMap[k]; ok {
				t.Errorf("non-no-auth start unexpectedly has %s in RunConfig.Env", k)
			}
			if slices.Contains(captured.HookWithheldEnv, k) {
				t.Errorf("non-no-auth start unexpectedly withholds %s", k)
			}
		}
	})
}

// TestStart_HookWithheldEnv_RestartEmptyTemplateDivergentEnvTemplate verifies
// that on restart with opts.Template == "", env_template provenance and
// warnings come strictly from the harness resolved by harness.Resolve (the
// emitter), never from a separate template-path lookup.
func TestStart_HookWithheldEnv_RestartEmptyTemplateDivergentEnvTemplate(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	t.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	// Global harness-config "div-hc" has TPL_FROM_GLOBAL and SCION_FROM_GLOBAL.
	globalHCDir := filepath.Join(globalScionDir, "harness-configs", "div-hc")
	if err := os.MkdirAll(globalHCDir, 0o755); err != nil {
		t.Fatal(err)
	}
	globalHC := strings.Join([]string{
		"harness: generic",
		"user: scion",
		"image: test-image:latest",
		"command:",
		"  base: [\"echo\"]",
		"env_template:",
		"  TPL_FROM_GLOBAL: global-val",
		"  SCION_FROM_GLOBAL: bad",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(globalHCDir, "config.yaml"), []byte(globalHC), 0o644); err != nil {
		t.Fatal(err)
	}

	// Local template "local-tpl" bundles a DIFFERENT "div-hc" with
	// TPL_FROM_LOCAL_TPL and SCION_FROM_LOCAL_TPL.
	tplDir := filepath.Join(globalScionDir, "templates", "local-tpl")
	tplHCDir := filepath.Join(tplDir, "harness-configs", "div-hc")
	if err := os.MkdirAll(tplHCDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "div-hc"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	tplHC := strings.Join([]string{
		"harness: generic",
		"user: scion",
		"image: test-image:latest",
		"command:",
		"  base: [\"echo\"]",
		"env_template:",
		"  TPL_FROM_LOCAL_TPL: local-tpl-val",
		"  SCION_FROM_LOCAL_TPL: bad",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(tplHCDir, "config.yaml"), []byte(tplHC), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte("schema_version: \"1\"\nactive_profile: local\nprofiles:\n  local:\n    runtime: docker\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Pre-existing agent referencing template "local-tpl" and harness_config "div-hc".
	projectScionDir := filepath.Join(tmpDir, "project", ".scion")
	agentName := "div-agent"
	agentDir := filepath.Join(projectScionDir, "agents", agentName)
	if err := os.MkdirAll(filepath.Join(agentDir, "home"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{"harness_config":"div-hc","info":{"template":"local-tpl"}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	var captured runtime.RunConfig
	mgr := NewManager(&runtime.MockRuntime{
		ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil },
		RunFunc: func(_ context.Context, cfg runtime.RunConfig) (string, error) {
			captured = cfg
			return "mock-id", nil
		},
	})

	// Restart with opts.Template == "" (the broker restart shape).
	info, err := mgr.Start(context.Background(), api.StartOptions{
		Name:          agentName,
		ProjectPath:   projectScionDir,
		Template:      "",
		HarnessConfig: "div-hc",
		BrokerMode:    true,
		NoAuth:        true,
	})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	if captured.Harness == nil {
		t.Fatalf("captured.Harness is nil")
	}
	emittedTpl := captured.Harness.GetEnv(agentName, "/home/scion", "scion")
	if emittedTpl["TPL_FROM_GLOBAL"] != "global-val" {
		t.Errorf("Harness.GetEnv()[TPL_FROM_GLOBAL] = %q, want %q", emittedTpl["TPL_FROM_GLOBAL"], "global-val")
	}
	if _, ok := emittedTpl["TPL_FROM_LOCAL_TPL"]; ok {
		t.Errorf("Harness.GetEnv() unexpectedly emitted TPL_FROM_LOCAL_TPL")
	}
	// Warnings must come from the emitting bundle (global), not the non-emitting
	// template-bundled copy.
	joined := strings.Join(info.Warnings, "\n")
	if !strings.Contains(joined, "SCION_FROM_GLOBAL") {
		t.Errorf("expected warning for SCION_FROM_GLOBAL from emitting bundle; got:\n%s", joined)
	}
	if strings.Contains(joined, "SCION_FROM_LOCAL_TPL") {
		t.Errorf("unexpected warning for SCION_FROM_LOCAL_TPL from non-emitting bundle; got:\n%s", joined)
	}
	// RunConfig.HookWithheldEnv must not contain stale env_template keys from
	// a separate lookup (runtimes withhold env_template directly from GetEnv).
	if slices.Contains(captured.HookWithheldEnv, "TPL_FROM_LOCAL_TPL") {
		t.Errorf("RunConfig.HookWithheldEnv unexpectedly contains non-emitting TPL_FROM_LOCAL_TPL: %v", captured.HookWithheldEnv)
	}
}

// TestEnvOrigin_AllValuesClassified ensures every envOrigin constant has an
// explicit classification in isHarnessConfig().
func TestEnvOrigin_AllValuesClassified(t *testing.T) {
	cases := []struct {
		origin envOrigin
		wantHC bool
	}{
		{envOriginNone, false},
		{envOriginBundle, true},
		{envOriginSettingsHC, true},
		{envOriginNoAuthHC, true},
		{envOriginLegacyHC, true},
		{envOriginConfigOwn, false},
		{envOriginCaller, false},
		{envOriginDefault, false},
		{envOriginBrokerSystem, false},
	}
	for _, tc := range cases {
		if got := tc.origin.isHarnessConfig(); got != tc.wantHC {
			t.Errorf("envOrigin(%d).isHarnessConfig() = %t, want %t", tc.origin, got, tc.wantHC)
		}
	}
}

// TestStart_AgentEnvUsesTrackedBuilder is a structural guard verifying that
// AgentManager.Start builds RunConfig.Env and RunConfig.HookWithheldEnv from
// trackedAgentEnv and never bypasses it via raw slice mutators.
func TestStart_AgentEnvUsesTrackedBuilder(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "run.go", nil, 0)
	if err != nil {
		t.Fatalf("parse run.go: %v", err)
	}

	var startDecl *ast.FuncDecl
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == "Start" && fn.Recv != nil {
			startDecl = fn
			break
		}
	}
	if startDecl == nil {
		t.Fatalf("could not find (*AgentManager).Start in run.go")
	}

	forbiddenCalls := map[string]bool{
		"buildAgentEnv":           true,
		"buildAgentEnvWithBundle": true,
		"applyNoAuthEnv":          true,
		"withLaunchIDEnv":         true,
		"withInitialTaskEnv":      true,
		"enforceWorkspaceGitEnv":  true,
		"withEnvValue":            true,
	}
	var sawBuildTracked, sawApplyNoAuthTracked, sawHarnessConfigKeys bool
	ast.Inspect(startDecl.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			if forbiddenCalls[fn.Name] {
				t.Errorf("Start() calls raw env helper %s; use trackedAgentEnv instead", fn.Name)
			}
			if fn.Name == "buildTrackedAgentEnv" {
				sawBuildTracked = true
			}
			if fn.Name == "applyNoAuthEnvTracked" {
				sawApplyNoAuthTracked = true
			}
		case *ast.SelectorExpr:
			if fn.Sel.Name == "HarnessConfigKeys" {
				sawHarnessConfigKeys = true
			}
		}
		return true
	})

	if !sawBuildTracked {
		t.Error("Start() did not call buildTrackedAgentEnv")
	}
	if !sawApplyNoAuthTracked {
		t.Error("Start() did not call applyNoAuthEnvTracked")
	}
	if !sawHarnessConfigKeys {
		t.Error("Start() did not derive RunConfig.HookWithheldEnv from trackedAgentEnv.HarnessConfigKeys()")
	}
}
