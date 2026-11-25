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

package runtimebroker

import (
	"context"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

func TestStaticCapabilities_AdvertisesGitHubTokenInstallationOnly(t *testing.T) {
	caps := StaticCapabilities(nil, "docker")
	if !caps.GitHubTokenInstallationOnly {
		t.Fatalf("StaticCapabilities.GitHubTokenInstallationOnly = false, want true")
	}
}

func TestAltEnvKeySatisfiedWithPolicy_GitHubTokenInstallationOnly(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_host_pat")
	t.Setenv("GH_TOKEN", "ghp_host_pat_2")
	t.Setenv("OTHER_AUTH_KEY", "other_val")

	// When installation-only is false, workstation process env satisfies GITHUB_TOKEN.
	if !altEnvKeySatisfiedWithPolicy(nil, []string{"GITHUB_TOKEN"}, true, false) {
		t.Errorf("expected GITHUB_TOKEN satisfied from host env when installationOnly=false")
	}
	// When installation-only is true, host env must NOT satisfy blocked GitHub keys.
	if altEnvKeySatisfiedWithPolicy(nil, []string{"GITHUB_TOKEN", "GH_TOKEN"}, true, true) {
		t.Errorf("blocked GitHub keys must not be satisfied from host env when installationOnly=true")
	}
	// Hub-minted ResolvedEnv still satisfies it.
	if !altEnvKeySatisfiedWithPolicy(map[string]string{"GITHUB_TOKEN": "ghs_app"}, []string{"GITHUB_TOKEN"}, true, true) {
		t.Errorf("Hub-minted ResolvedEnv GITHUB_TOKEN must still satisfy altEnvKeySatisfiedWithPolicy")
	}
	// Unrelated host env keys still work on workstation brokers.
	if !altEnvKeySatisfiedWithPolicy(nil, []string{"OTHER_AUTH_KEY"}, true, true) {
		t.Errorf("unrelated host env key should still be satisfied on workstation broker")
	}
}

func TestBuildStartContext_GitHubTokenInstallationOnlyStripsConfigAndInlineEnv(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)
	inline := &api.ScionConfig{
		Env: map[string]string{
			"GITHUB_TOKEN": "ghp_inline_pat",
			"GH_TOKEN":     "ghp_inline_pat_2",
			"KEEP_INLINE":  "yes",
		},
	}
	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:                        "inst-agent",
		AgentID:                     "agent-1",
		Slug:                        "inst-agent",
		ProjectPath:                 t.TempDir(),
		Operation:                   opCreate,
		GitHubTokenInstallationOnly: true,
		ResolvedEnv: map[string]string{
			"GITHUB_TOKEN": "ghs_hub_minted_app",
		},
		Config: &CreateAgentConfig{
			Env: []string{
				"GITHUB_TOKEN=ghp_config_pat",
				"GH_TOKEN=ghp_config_pat_2",
				"KEEP_CFG=yes",
			},
		},
		InlineConfig: inline,
	})
	if err != nil {
		t.Fatalf("buildStartContext: %v", err)
	}
	if !sc.Opts.GitHubTokenInstallationOnly {
		t.Fatalf("sc.Opts.GitHubTokenInstallationOnly = false, want true")
	}
	if sc.Opts.Env["GITHUB_TOKEN"] != "ghs_hub_minted_app" {
		t.Errorf("sc.Opts.Env[GITHUB_TOKEN] = %q, want ghs_hub_minted_app (Config.Env must not override)", sc.Opts.Env["GITHUB_TOKEN"])
	}
	if _, ok := sc.Opts.Env["GH_TOKEN"]; ok {
		t.Errorf("sc.Opts.Env[GH_TOKEN] must be stripped")
	}
	if sc.Opts.Env["KEEP_CFG"] != "yes" {
		t.Errorf("sc.Opts.Env[KEEP_CFG] = %q, want yes", sc.Opts.Env["KEEP_CFG"])
	}
	if sc.Opts.InlineConfig == nil || sc.Opts.InlineConfig.Env["KEEP_INLINE"] != "yes" {
		t.Fatalf("sc.Opts.InlineConfig.Env missing KEEP_INLINE: %+v", sc.Opts.InlineConfig)
	}
	if _, ok := sc.Opts.InlineConfig.Env["GITHUB_TOKEN"]; ok {
		t.Errorf("sc.Opts.InlineConfig.Env[GITHUB_TOKEN] must be stripped")
	}
	if _, ok := sc.Opts.InlineConfig.Env["GH_TOKEN"]; ok {
		t.Errorf("sc.Opts.InlineConfig.Env[GH_TOKEN] must be stripped")
	}
	// Caller's InlineConfig must not be mutated.
	if inline.Env["GITHUB_TOKEN"] != "ghp_inline_pat" {
		t.Errorf("caller inline.Env was mutated: %+v", inline.Env)
	}
}

func TestExtractRequiredEnvKeys_GitHubTokenInstallationOnlyOmitsBlockedKeys(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)

	req := CreateAgentRequest{
		Name:                        "inst-only-blocked",
		ProjectPath:                 t.TempDir(),
		GitHubTokenInstallationOnly: true,
		RequiredSecrets: []api.RequiredSecret{
			{Key: "GITHUB_TOKEN", Description: "GitHub PAT", Type: "environment"},
			{Key: "GH_TOKEN", Description: "GH CLI PAT", Type: "environment"},
		},
	}
	keys, secretInfo, _, _, err := srv.extractRequiredEnvKeysChecked(req, "", "")
	if err != nil {
		t.Fatalf("extractRequiredEnvKeysChecked: %v", err)
	}
	for _, k := range keys {
		if api.IsBlockedInstallationGitHubEnvKey(k) {
			t.Errorf("blocked key %q remained in required keys under GitHubTokenInstallationOnly", k)
		}
	}
	for _, blocked := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if _, ok := secretInfo[blocked]; ok {
			t.Errorf("blocked key %q remained in secretInfo under GitHubTokenInstallationOnly", blocked)
		}
	}
}
