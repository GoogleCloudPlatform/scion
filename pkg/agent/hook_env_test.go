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

func TestComputeHookWithheldEnv_AllLayersAndPrecedence(t *testing.T) {
	bundleEnv := map[string]string{
		"BUNDLE_ONLY":            "from-bundle",
		"GITHUB_TOKEN":           "",
		"INLINE_WIN":             "from-bundle",
		"GOOGLE_CLOUD_PROJECT":   "bundle-proj",
		"EMPTY_AUTH_OR_SECRET":   "",
		"MY${UNSET_TEST_VAR}KEY": "expanded-key-val",
	}
	settingsHCEnv := map[string]string{
		"SETTINGS_ONLY":    "from-settings",
		"SETTINGS_HUB_WIN": "from-settings",
		"SETTINGS_EMPTY":   "",
	}
	envTemplateEnv := map[string]string{
		"TPL_ONLY":       "from-tpl",
		"TPL_AUTH_WIN":   "from-tpl",
		"TPL_SECRET_WIN": "from-tpl",
		"TPL_HUB_WIN":    "from-tpl",
		"TPL_INLINE_WIN": "from-tpl",
	}
	inlineEnv := map[string]string{
		"INLINE_WIN":     "from-inline",
		"TPL_INLINE_WIN": "from-inline",
	}
	callerEnv := map[string]string{
		"GITHUB_TOKEN":     "ghp_from_hub",
		"SETTINGS_HUB_WIN": "from-hub",
		"TPL_HUB_WIN":      "from-hub",
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

	agentEnv, _, _, _, provenance := buildAgentEnvWithBundle(bundleEnv, &api.ScionConfig{Env: inlineEnv}, optsEnv, nil, true)

	baseInput := hookWithheldInput{
		bundleEnv:      bundleEnv,
		envTemplateEnv: envTemplateEnv,
		settingsHCEnv:  settingsHCEnv,
		agentEnv:       agentEnv,
		provenance:     provenance,
		callerEnv:      callerEnv,
		inlineEnv:      inlineEnv,
		resolvedAuth: &api.ResolvedAuth{
			EnvVars: map[string]string{"TPL_AUTH_WIN": "from-auth"},
		},
		resolvedSecrets: []api.ResolvedSecret{
			{Type: "environment", Target: "TPL_SECRET_WIN", Value: "from-secret"},
		},
	}

	// On docker/podman/container/k8s/cloudrun, Harness.GetEnv() (env_template)
	// is emitted after ResolvedAuth and wins on collision, so TPL_AUTH_WIN is
	// withheld. On substrate, ResolvedAuth wins over Harness.GetEnv() and
	// cfg.Env, so TPL_AUTH_WIN is not withheld. On cloudrun-sandbox,
	// Harness.GetEnv() wins over cfg.Env (so TPL_HUB_WIN and TPL_INLINE_WIN are
	// withheld), while ResolvedAuth wins over Harness.GetEnv().
	runtimeCases := []struct {
		runtimeName string
		want        []string
	}{
		{
			runtimeName: "docker",
			want:        []string{"BUNDLE_ONLY", "GOOGLE_CLOUD_PROJECT", "MY${UNSET_TEST_VAR}KEY", "SETTINGS_ONLY", "TPL_AUTH_WIN", "TPL_ONLY"},
		},
		{
			runtimeName: "substrate",
			want:        []string{"BUNDLE_ONLY", "GOOGLE_CLOUD_PROJECT", "MY${UNSET_TEST_VAR}KEY", "SETTINGS_ONLY", "TPL_ONLY"},
		},
		{
			runtimeName: "cloudrun-sandbox",
			want:        []string{"BUNDLE_ONLY", "GOOGLE_CLOUD_PROJECT", "MY${UNSET_TEST_VAR}KEY", "SETTINGS_ONLY", "TPL_HUB_WIN", "TPL_INLINE_WIN", "TPL_ONLY"},
		},
	}

	for _, tc := range runtimeCases {
		t.Run(tc.runtimeName, func(t *testing.T) {
			in := baseInput
			in.runtimeName = tc.runtimeName
			got := computeHookWithheldEnv(in)
			if !slices.Equal(got, tc.want) {
				t.Errorf("computeHookWithheldEnv(%s) = %v, want %v (agentEnv=%v)", tc.runtimeName, got, tc.want, agentEnv)
			}
		})
	}
}

// TestComputeHookWithheldEnv_AllLayerPairsPerRuntime verifies every unordered
// pair of the 7 environment layers (bundle, env_template, settings, hub, own,
// auth, secrets) across every runtime family, asserting both the final winning
// layer and whether computeHookWithheldEnv withholds the key.
func TestComputeHookWithheldEnv_AllLayerPairsPerRuntime(t *testing.T) {
	type layer string
	const (
		layerBundle      layer = "bundle"
		layerEnvTemplate layer = "env_template"
		layerSettings    layer = "settings"
		layerHub         layer = "hub"
		layerOwn         layer = "own"
		layerAuth        layer = "auth"
		layerSecrets     layer = "secrets"
	)

	isHarnessConfigLayer := func(l layer) bool {
		return l == layerBundle || l == layerEnvTemplate || l == layerSettings
	}

	// expectedWinner returns which of layers a and b supplies the final
	// container value on runtimeName, combining buildAgentEnvWithBundle's
	// intra-ConfigEnv precedence (bundle < own < settings < hub) with each
	// runtime's inter-source precedence.
	expectedWinner := func(runtimeName string, a, b layer) layer {
		// Intra-ConfigEnv rank (higher wins within RunConfig.Env).
		cfgRank := map[layer]int{
			layerBundle:   1,
			layerOwn:      2,
			layerSettings: 3,
			layerHub:      4,
		}
		_, aInCfg := cfgRank[a]
		_, bInCfg := cfgRank[b]
		if aInCfg && bInCfg {
			if cfgRank[a] > cfgRank[b] {
				return a
			}
			return b
		}

		// Runtime-level source rank per runtime family.
		toRuntimeSrc := func(l layer) runtime.EnvSource {
			switch l {
			case layerEnvTemplate:
				return runtime.EnvSourceHarnessTemplate
			case layerAuth:
				return runtime.EnvSourceResolvedAuth
			case layerSecrets:
				return runtime.EnvSourceResolvedSecret
			default:
				return runtime.EnvSourceConfigEnv
			}
		}
		sa, sb := toRuntimeSrc(a), toRuntimeSrc(b)
		switch runtimeName {
		case "substrate":
			// HarnessTemplate < ConfigEnv < ResolvedAuth < ResolvedSecret
			rank := map[runtime.EnvSource]int{
				runtime.EnvSourceHarnessTemplate: 1,
				runtime.EnvSourceConfigEnv:       2,
				runtime.EnvSourceResolvedAuth:    3,
				runtime.EnvSourceResolvedSecret:  4,
			}
			if rank[sa] > rank[sb] {
				return a
			}
			return b
		case "cloudrun-sandbox":
			// Non-colliding secrets win over auth/tpl, but secrets colliding
			// with ConfigEnv are skipped; otherwise ConfigEnv < Tpl < Auth.
			if (sa == runtime.EnvSourceResolvedSecret && sb == runtime.EnvSourceConfigEnv) ||
				(sb == runtime.EnvSourceResolvedSecret && sa == runtime.EnvSourceConfigEnv) {
				if sa == runtime.EnvSourceConfigEnv {
					return a
				}
				return b
			}
			rank := map[runtime.EnvSource]int{
				runtime.EnvSourceConfigEnv:       1,
				runtime.EnvSourceHarnessTemplate: 2,
				runtime.EnvSourceResolvedAuth:    3,
				runtime.EnvSourceResolvedSecret:  4,
			}
			if rank[sa] > rank[sb] {
				return a
			}
			return b
		default:
			// docker, podman, container, kubernetes, cloudrun:
			// ResolvedAuth < HarnessTemplate < ResolvedSecret (non-colliding) < ConfigEnv
			rank := map[runtime.EnvSource]int{
				runtime.EnvSourceResolvedAuth:    1,
				runtime.EnvSourceHarnessTemplate: 2,
				runtime.EnvSourceResolvedSecret:  3,
				runtime.EnvSourceConfigEnv:       4,
			}
			if rank[sa] > rank[sb] {
				return a
			}
			return b
		}
	}

	allLayers := []layer{layerBundle, layerEnvTemplate, layerSettings, layerHub, layerOwn, layerAuth, layerSecrets}
	runtimes := []string{"docker", "podman", "container", "kubernetes", "cloudrun", "cloudrun-sandbox", "substrate"}

	const key = "PAIR_TEST_KEY"
	for _, rtName := range runtimes {
		t.Run(rtName, func(t *testing.T) {
			for i := 0; i < len(allLayers); i++ {
				for j := i + 1; j < len(allLayers); j++ {
					la, lb := allLayers[i], allLayers[j]
					t.Run(string(la)+"_vs_"+string(lb), func(t *testing.T) {
						var bundleEnv, tplEnv, settingsEnv, callerEnv, inlineEnv map[string]string
						var auth *api.ResolvedAuth
						var secrets []api.ResolvedSecret

						addLayer := func(l layer) {
							switch l {
							case layerBundle:
								bundleEnv = map[string]string{key: "from-bundle"}
							case layerEnvTemplate:
								tplEnv = map[string]string{key: "from-env-template"}
							case layerSettings:
								settingsEnv = map[string]string{key: "from-settings"}
							case layerHub:
								callerEnv = map[string]string{key: "from-hub"}
							case layerOwn:
								inlineEnv = map[string]string{key: "from-own"}
							case layerAuth:
								auth = &api.ResolvedAuth{Method: "api-key", EnvVars: map[string]string{key: "from-auth"}}
							case layerSecrets:
								secrets = []api.ResolvedSecret{{Type: "environment", Target: key, Value: "from-secret"}}
							}
						}
						addLayer(la)
						addLayer(lb)

						// Assemble opts.Env as Start does: callerEnv, with settingsEnv filling absent keys.
						optsEnv := make(map[string]string)
						for k, v := range callerEnv {
							optsEnv[k] = v
						}
						for k, v := range settingsEnv {
							if _, exists := optsEnv[k]; !exists {
								optsEnv[k] = v
							}
						}
						var cfg *api.ScionConfig
						if len(inlineEnv) > 0 {
							cfg = &api.ScionConfig{Env: inlineEnv}
						}
						agentEnv, _, _, _, provenance := buildAgentEnvWithBundle(bundleEnv, cfg, optsEnv, nil, true)

						gotWithheld := computeHookWithheldEnv(hookWithheldInput{
							runtimeName:     rtName,
							bundleEnv:       bundleEnv,
							envTemplateEnv:  tplEnv,
							settingsHCEnv:   settingsEnv,
							agentEnv:        agentEnv,
							provenance:      provenance,
							callerEnv:       callerEnv,
							inlineEnv:       inlineEnv,
							resolvedAuth:    auth,
							resolvedSecrets: secrets,
						})

						winner := expectedWinner(rtName, la, lb)
						wantWithheld := isHarnessConfigLayer(winner)
						isWithheld := slices.Contains(gotWithheld, key)
						if isWithheld != wantWithheld {
							t.Errorf("runtime=%s pair=(%s,%s) winner=%s: withheld=%v, want %v (gotWithheld=%v)",
								rtName, la, lb, winner, isWithheld, wantWithheld, gotWithheld)
						}
					})
				}
			}
		})
	}
}

func TestComputeHookWithheldEnv_EmptyLayers(t *testing.T) {
	got := computeHookWithheldEnv(hookWithheldInput{agentEnv: []string{"FOO=bar"}})
	if len(got) != 0 {
		t.Errorf("computeHookWithheldEnv(empty) = %v, want empty", got)
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

	wantWithheld := []string{
		"BUNDLE_ONLY",
		"COLLIDE_KEY",
		"GOOGLE_CLOUD_PROJECT",
		envpolicy.HintLivenessRootDepth,
		"MY${UNSET_TEST_VAR}KEY",
		"SETTINGS_ONLY",
		"TPL_ONLY",
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
	if captured.Harness != nil {
		tplEnv := captured.Harness.GetEnv("hook-env-test", "/home/scion", "scion")
		for _, badKey := range []string{"PATH", "PATH=/opt/custom"} {
			if v, ok := tplEnv[badKey]; ok {
				t.Errorf("Harness.GetEnv() unexpectedly contains invalid key %s=%q", badKey, v)
			}
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
