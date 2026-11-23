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

func TestGitHubTokenSerializerStrip_AllBlockedKeysWithheldForEveryViewer(t *testing.T) {
	blockedEnv := map[string]string{
		"GITHUB_TOKEN":            "ghp_1",
		"GH_TOKEN":                "ghp_2",
		"GH_ENTERPRISE_TOKEN":     "ghp_3",
		"GITHUB_ENTERPRISE_TOKEN": "ghp_4",
		"SCION_USER_GITHUB_TOKEN": "true",
		"SAFE_VAR":                "keep_me",
	}

	ac := &AgentAppliedConfig{
		Env: blockedEnv,
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

	// PendingConfig also strips every blocked GitHub key even for canAttach=true.
	pending := &AgentPendingConfig{
		Config: map[string]json.RawMessage{
			"env": json.RawMessage(`{"GITHUB_TOKEN":"ghp_1","GH_TOKEN":"ghp_2","GH_ENTERPRISE_TOKEN":"ghp_3","GITHUB_ENTERPRISE_TOKEN":"ghp_4","SCION_USER_GITHUB_TOKEN":"true","SAFE_VAR":"keep_me"}`),
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
	if len(decodedPending.Config.Env) != 1 || decodedPending.Config.Env["SAFE_VAR"] != "keep_me" {
		t.Errorf("pendingConfig.env = %v, want only SAFE_VAR=keep_me", decodedPending.Config.Env)
	}
}
