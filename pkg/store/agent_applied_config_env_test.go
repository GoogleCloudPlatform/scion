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
)

// TestAgentAppliedConfigMarshalJSONHidesEnvByDefault is the guard for the
// structural env-redaction choke point: AppliedConfig.Env must never reach a
// JSON encoding unless ResponseView has explicitly been called first. This is
// what lets a brand-new hub handler serialize a store.Agent (or an
// AgentAppliedConfig embedded in any other response type) without
// independently re-deriving the env-redaction rule -- forgetting to opt in
// fails closed (no env at all), never open.
func TestAgentAppliedConfigMarshalJSONHidesEnvByDefault(t *testing.T) {
	cfg := &AgentAppliedConfig{
		Image: "some-image",
		Env: map[string]string{
			"PLAIN_VAR":    "plain-value",
			"GITHUB_TOKEN": "ghp_should_never_be_marshaled",
		},
	}

	t.Run("direct marshal of a freshly constructed config omits env", func(t *testing.T) {
		data, err := json.Marshal(cfg)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var decoded map[string]interface{}
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if _, ok := decoded["env"]; ok {
			t.Errorf("env must not be present in the default marshal, got: %s", data)
		}
		// Unrelated fields still marshal normally -- this isn't a blanket
		// suppression of the whole type, only of Env.
		if decoded["image"] != "some-image" {
			t.Errorf("expected image to still marshal, got: %s", data)
		}
	})

	t.Run("marshal after ResponseView(false) still omits env", func(t *testing.T) {
		view := cfg.ResponseView(false)
		data, err := json.Marshal(view)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var decoded map[string]interface{}
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if _, ok := decoded["env"]; ok {
			t.Errorf("env must not be present when canAttach is false, got: %s", data)
		}
	})

	t.Run("marshal after ResponseView(true) includes env minus GITHUB_TOKEN", func(t *testing.T) {
		view := cfg.ResponseView(true)
		data, err := json.Marshal(view)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var decoded struct {
			Env map[string]string `json:"env"`
		}
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if decoded.Env["PLAIN_VAR"] != "plain-value" {
			t.Errorf("expected PLAIN_VAR to be present, got: %s", data)
		}
		if _, ok := decoded.Env["GITHUB_TOKEN"]; ok {
			t.Errorf("GITHUB_TOKEN must never be marshaled, even with canAttach true, got: %s", data)
		}
	})

	t.Run("ResponseView does not mutate the original config", func(t *testing.T) {
		original := &AgentAppliedConfig{Env: map[string]string{"PLAIN_VAR": "v"}}
		_ = original.ResponseView(true)
		data, err := json.Marshal(original)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var decoded map[string]interface{}
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if _, ok := decoded["env"]; ok {
			t.Errorf("calling ResponseView on a copy must not flip visibility on the original, got: %s", data)
		}
	})

	t.Run("a nil config's ResponseView is nil", func(t *testing.T) {
		var nilCfg *AgentAppliedConfig
		if got := nilCfg.ResponseView(true); got != nil {
			t.Errorf("expected nil, got %+v", got)
		}
	})

	t.Run("an agent embedding the config also hides env by default", func(t *testing.T) {
		// Regression guard for the case that actually matters in the hub: a
		// handler that serializes a *store.Agent (or a type that embeds one,
		// like AgentWithCapabilities) without calling redactedAgentCopy /
		// ResponseView must still get no Env, because AppliedConfig keeps its
		// own MarshalJSON method regardless of what wraps it.
		agent := &Agent{
			ID:            "agent-1",
			AppliedConfig: cfg,
		}
		data, err := json.Marshal(agent)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var decoded struct {
			AppliedConfig map[string]interface{} `json:"appliedConfig"`
		}
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if _, ok := decoded.AppliedConfig["env"]; ok {
			t.Errorf("a store.Agent serialized without going through the response-view gate must not expose env, got: %s", data)
		}
	})
}
