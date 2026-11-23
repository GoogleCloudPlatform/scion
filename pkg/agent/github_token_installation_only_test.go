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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

func TestBuildTrackedAgentEnv_GitHubTokenInstallationOnlyBlocksHostAndConfigLayers(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_host_pat_1")
	t.Setenv("GH_TOKEN", "ghp_host_pat_2")
	t.Setenv("GH_ENTERPRISE_TOKEN", "ghp_host_pat_3")
	t.Setenv("GITHUB_ENTERPRISE_TOKEN", "ghp_host_pat_4")
	t.Setenv("SCION_USER_GITHUB_TOKEN", "true")
	t.Setenv("SAFE_HOST_PASSTHROUGH", "safe_host_val")

	rawBundleEnv := map[string]string{
		"GITHUB_TOKEN": "ghp_bundle_pat",
		"GH_TOKEN":     "ghp_bundle_pat_2",
		"BUNDLE_OK":    "bundle_val",
	}
	scionCfg := &api.ScionConfig{
		Env: map[string]string{
			"GITHUB_TOKEN":            "",                // empty-value host passthrough
			"GH_TOKEN":                "",                // empty-value host passthrough
			"GH_ENTERPRISE_TOKEN":     "",                // empty-value host passthrough
			"GITHUB_ENTERPRISE_TOKEN": "ghp_literal_pat", // literal value
			"SCION_USER_GITHUB_TOKEN": "",                // empty-value host passthrough
			"EXPANDED_PAT":            "${GITHUB_TOKEN}", // ${VAR} host expansion
			"SAFE_HOST_PASSTHROUGH":   "",                // allowed host passthrough
		},
	}
	defaultEnv := map[string]string{
		"GH_TOKEN":   "ghp_default_pat",
		"DEFAULT_OK": "default_val",
	}

	// 1. Without a Hub-minted token in extraEnv: no blocked GitHub key reaches the container.
	tracked, warnings, _, _ := buildTrackedAgentEnv(
		rawBundleEnv, scionCfg, nil, nil, nil, nil, defaultEnv, true, true,
	)
	envMap := envSliceToMap(tracked.Slice())
	for _, blocked := range []string{
		"GITHUB_TOKEN",
		"GH_TOKEN",
		"GH_ENTERPRISE_TOKEN",
		"GITHUB_ENTERPRISE_TOKEN",
		"SCION_USER_GITHUB_TOKEN",
	} {
		if v, ok := envMap[blocked]; ok {
			t.Errorf("blocked key %q leaked into container env with value %q", blocked, v)
		}
	}
	if v := envMap["EXPANDED_PAT"]; v != "" {
		t.Errorf("EXPANDED_PAT leaked host GITHUB_TOKEN via expansion: %q", v)
	}
	if envMap["SAFE_HOST_PASSTHROUGH"] != "safe_host_val" {
		t.Errorf("SAFE_HOST_PASSTHROUGH = %q, want safe_host_val", envMap["SAFE_HOST_PASSTHROUGH"])
	}
	if envMap["BUNDLE_OK"] != "bundle_val" {
		t.Errorf("BUNDLE_OK = %q, want bundle_val", envMap["BUNDLE_OK"])
	}
	if envMap["DEFAULT_OK"] != "default_val" {
		t.Errorf("DEFAULT_OK = %q, want default_val", envMap["DEFAULT_OK"])
	}
	// Warnings must never leak host token values.
	for _, w := range warnings {
		if strings.Contains(w, "ghp_host_pat") {
			t.Errorf("warning leaked host PAT value: %q", w)
		}
	}

	// 2. With a Hub-minted App installation token in extraEnv: only that token arrives.
	extraEnv := map[string]string{
		"GITHUB_TOKEN": "ghs_hub_minted_installation_token",
	}
	trackedWithApp, _, _, _ := buildTrackedAgentEnv(
		rawBundleEnv, scionCfg, extraEnv, nil, extraEnv, nil, defaultEnv, true, true,
	)
	envMapWithApp := envSliceToMap(trackedWithApp.Slice())
	if envMapWithApp["GITHUB_TOKEN"] != "ghs_hub_minted_installation_token" {
		t.Errorf("GITHUB_TOKEN = %q, want ghs_hub_minted_installation_token", envMapWithApp["GITHUB_TOKEN"])
	}
	if _, ok := envMapWithApp["GH_TOKEN"]; ok {
		t.Errorf("GH_TOKEN must still be blocked when Hub minted GITHUB_TOKEN")
	}
}

func TestResolveAuthEnvOverlayWithPolicy_GitHubTokenInstallationOnlySkipsSettingsHCEnv(t *testing.T) {
	settings := &config.VersionedSettings{
		HarnessConfigs: map[string]config.HarnessConfigEntry{
			"claude": {
				Env: map[string]string{
					"GITHUB_TOKEN": "ghp_settings_pat",
					"GH_TOKEN":     "ghp_settings_pat_2",
					"SAFE_SETTING": "ok",
				},
			},
		},
	}

	opts := &api.StartOptions{
		BrokerMode:                  true,
		GitHubTokenInstallationOnly: true,
		Env:                         map[string]string{},
	}
	overlay, _, settingsHCEnv, _ := resolveAuthEnvOverlayWithPolicy(opts, settings, "", "claude")
	if _, ok := opts.Env["GITHUB_TOKEN"]; ok {
		t.Errorf("opts.Env must not receive settings GITHUB_TOKEN when GitHubTokenInstallationOnly=true")
	}
	if _, ok := opts.Env["GH_TOKEN"]; ok {
		t.Errorf("opts.Env must not receive settings GH_TOKEN when GitHubTokenInstallationOnly=true")
	}
	if _, ok := overlay["GITHUB_TOKEN"]; ok {
		t.Errorf("overlay must not receive settings GITHUB_TOKEN when GitHubTokenInstallationOnly=true")
	}
	if _, ok := settingsHCEnv["GITHUB_TOKEN"]; ok {
		t.Errorf("settingsHCEnv must not receive GITHUB_TOKEN when GitHubTokenInstallationOnly=true")
	}
	if opts.Env["SAFE_SETTING"] != "ok" {
		t.Errorf("opts.Env[SAFE_SETTING] = %q, want ok", opts.Env["SAFE_SETTING"])
	}
}

func envSliceToMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if ok {
			m[k] = v
		}
	}
	return m
}
