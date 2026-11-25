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

package store

import (
	"encoding/json"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

func TestGitHubTokenSerializerStrip_InstallationOnlyVsNonInstallation(t *testing.T) {
	blockedEnv := map[string]string{
		"GITHUB_TOKEN":            "ghp_1",
		"GH_TOKEN":                "ghp_2",
		"GH_ENTERPRISE_TOKEN":     "ghp_3",
		"GITHUB_ENTERPRISE_TOKEN": "ghp_4",
		"SCION_USER_GITHUB_TOKEN": "true",
		"SAFE_VAR":                "keep_me",
	}

	t.Run("installation_only_strips_all_blocked_keys", func(t *testing.T) {
		ac := &AgentAppliedConfig{
			GitHubTokenPreference: GitHubTokenPreferenceInstallation,
			Env:                   blockedEnv,
			InlineConfig: &api.ScionConfig{
				Env: blockedEnv,
			},
			CreateInputs: &AgentCreateInputs{
				InlineConfig: &api.ScionConfig{
					Env: blockedEnv,
				},
			},
		}

		view := ac.ResponseView(true)
		raw, err := json.Marshal(view)
		if err != nil {
			t.Fatalf("json.Marshal(ResponseView(true)): %v", err)
		}

		var decoded struct {
			Env          map[string]string `json:"env"`
			InlineConfig struct {
				Env map[string]string `json:"env"`
			} `json:"inlineConfig"`
			CreateInputs struct {
				InlineConfig struct {
					Env map[string]string `json:"env"`
				} `json:"inlineConfig"`
			} `json:"createInputs"`
		}
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("json.Unmarshal: %v", err)
		}

		for name, envMap := range map[string]map[string]string{
			"appliedConfig.env":              decoded.Env,
			"appliedConfig.inlineConfig.env": decoded.InlineConfig.Env,
			"createInputs.inlineConfig.env":  decoded.CreateInputs.InlineConfig.Env,
		} {
			if len(envMap) != 1 || envMap["SAFE_VAR"] != "keep_me" {
				t.Errorf("%s = %v, want only SAFE_VAR=keep_me", name, envMap)
			}
		}

		pending := &AgentPendingConfig{
			Config: map[string]json.RawMessage{
				"env": json.RawMessage(`{"GITHUB_TOKEN":"ghp_1","GH_TOKEN":"ghp_2","GH_ENTERPRISE_TOKEN":"ghp_3","GITHUB_ENTERPRISE_TOKEN":"ghp_4","SCION_USER_GITHUB_TOKEN":"true","SAFE_VAR":"keep_me"}`),
			},
		}
		pendingRaw, err := json.Marshal(pending.ResponseViewForGitHubPolicy(true, true))
		if err != nil {
			t.Fatalf("json.Marshal(pending.ResponseViewForGitHubPolicy(true, true)): %v", err)
		}
		var decodedPending struct {
			Config struct {
				Env map[string]string `json:"env"`
			} `json:"config"`
		}
		if err := json.Unmarshal(pendingRaw, &decodedPending); err != nil {
			t.Fatalf("unmarshal pending view: %v", err)
		}
		if len(decodedPending.Config.Env) != 1 || decodedPending.Config.Env["SAFE_VAR"] != "keep_me" {
			t.Errorf("pendingConfig.env = %v, want only SAFE_VAR=keep_me", decodedPending.Config.Env)
		}
	})

	t.Run("non_installation_strips_only_GITHUB_TOKEN_and_preserves_GH_keys", func(t *testing.T) {
		for _, pref := range []string{"", "user_preferred"} {
			ac := &AgentAppliedConfig{
				GitHubTokenPreference: pref,
				Env:                   blockedEnv,
				InlineConfig:          &api.ScionConfig{Env: blockedEnv},
				CreateInputs: &AgentCreateInputs{
					InlineConfig: &api.ScionConfig{Env: blockedEnv},
				},
			}
			raw, err := json.Marshal(ac.ResponseView(true))
			if err != nil {
				t.Fatalf("pref=%q json.Marshal: %v", pref, err)
			}
			var decoded struct {
				Env          map[string]string `json:"env"`
				InlineConfig struct {
					Env map[string]string `json:"env"`
				} `json:"inlineConfig"`
				CreateInputs struct {
					InlineConfig struct {
						Env map[string]string `json:"env"`
					} `json:"inlineConfig"`
				} `json:"createInputs"`
			}
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatalf("pref=%q json.Unmarshal: %v", pref, err)
			}
			for name, envMap := range map[string]map[string]string{
				"appliedConfig.env":              decoded.Env,
				"appliedConfig.inlineConfig.env": decoded.InlineConfig.Env,
				"createInputs.inlineConfig.env":  decoded.CreateInputs.InlineConfig.Env,
			} {
				if _, ok := envMap["GITHUB_TOKEN"]; ok {
					t.Errorf("pref=%q %s must strip GITHUB_TOKEN", pref, name)
				}
				if envMap["GH_TOKEN"] != "ghp_2" || envMap["GH_ENTERPRISE_TOKEN"] != "ghp_3" || envMap["SAFE_VAR"] != "keep_me" {
					t.Errorf("pref=%q %s = %v, want GH_TOKEN, GH_ENTERPRISE_TOKEN, SAFE_VAR preserved", pref, name, envMap)
				}
			}
		}

		pending := &AgentPendingConfig{
			Config: map[string]json.RawMessage{
				"env": json.RawMessage(`{"GITHUB_TOKEN":"ghp_1","GH_TOKEN":"ghp_2","GH_ENTERPRISE_TOKEN":"ghp_3","SAFE_VAR":"keep_me"}`),
			},
		}
		pendingRaw, err := json.Marshal(pending.ResponseView(true))
		if err != nil {
			t.Fatalf("json.Marshal(pending.ResponseView(true)): %v", err)
		}
		var decodedPending struct {
			Config struct {
				Env map[string]string `json:"env"`
			} `json:"config"`
		}
		if err := json.Unmarshal(pendingRaw, &decodedPending); err != nil {
			t.Fatalf("unmarshal pending view: %v", err)
		}
		if _, ok := decodedPending.Config.Env["GITHUB_TOKEN"]; ok {
			t.Errorf("pendingConfig.env must strip GITHUB_TOKEN")
		}
		if decodedPending.Config.Env["GH_TOKEN"] != "ghp_2" || decodedPending.Config.Env["GH_ENTERPRISE_TOKEN"] != "ghp_3" {
			t.Errorf("pendingConfig.env = %v, want GH_TOKEN and GH_ENTERPRISE_TOKEN preserved", decodedPending.Config.Env)
		}
	})
}
