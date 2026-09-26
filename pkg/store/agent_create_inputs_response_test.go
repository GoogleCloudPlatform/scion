// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
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

// createInputsResponseConfig returns an applied config whose CreateInputs
// carries an explicit inline config with env, GITHUB_TOKEN and a telemetry
// header map, mirroring the InlineConfig fixture in
// TestAgentAppliedConfigMarshalJSONHidesEnvByDefault.
func createInputsResponseConfig() *AgentAppliedConfig {
	level := 3
	return &AgentAppliedConfig{
		Image: "some-image",
		CreateInputs: &AgentCreateInputs{
			HarnessConfig: "claude",
			ThinkingLevel: &level,
			InlineConfig: &api.ScionConfig{
				Env: map[string]string{
					"CREATE_PLAIN_VAR": "create-plain-value",
					"GITHUB_TOKEN":     "ghp_create_input_value",
				},
				Telemetry: &api.TelemetryConfig{
					Cloud: &api.TelemetryCloudConfig{
						Endpoint: "https://collector.example.com",
						Headers:  map[string]string{"Authorization": "Bearer create-input-value"},
					},
				},
			},
		},
	}
}

// decodeCreateInputs marshals view and decodes its createInputs block.
func decodeCreateInputs(t *testing.T, view *AgentAppliedConfig) (ci struct {
	HarnessConfig string `json:"harnessConfig"`
	InlineConfig  *struct {
		Env       map[string]string `json:"env"`
		Telemetry *struct {
			Cloud *struct {
				Endpoint string            `json:"endpoint"`
				Headers  map[string]string `json:"headers"`
			} `json:"cloud"`
		} `json:"telemetry"`
	} `json:"inlineConfig"`
}) {
	t.Helper()
	data, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded struct {
		CreateInputs *json.RawMessage `json:"createInputs"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.CreateInputs == nil {
		t.Fatalf("expected createInputs in the response view, got: %s", data)
	}
	if err := json.Unmarshal(*decoded.CreateInputs, &ci); err != nil {
		t.Fatalf("unmarshal createInputs: %v", err)
	}
	if ci.InlineConfig == nil {
		t.Fatalf("expected createInputs.inlineConfig in the response view, got: %s", data)
	}
	return ci
}

// TestAgentAppliedConfigResponseViewRedactsCreateInputs pins that the response
// view gives CreateInputs.InlineConfig the same treatment as InlineConfig:
// env and the telemetry header map are hidden unless the caller can attach,
// GITHUB_TOKEN is always stripped, and the original is never mutated.
func TestAgentAppliedConfigResponseViewRedactsCreateInputs(t *testing.T) {
	t.Run("canAttach false hides create-input env and the telemetry header map", func(t *testing.T) {
		ci := decodeCreateInputs(t, createInputsResponseConfig().ResponseView(false))
		if ci.InlineConfig.Env != nil {
			t.Errorf("expected createInputs.inlineConfig.env to be absent, got: %+v", ci.InlineConfig.Env)
		}
		if ci.InlineConfig.Telemetry == nil || ci.InlineConfig.Telemetry.Cloud == nil {
			t.Fatal("expected the telemetry cloud config to survive the response view")
		}
		if ci.InlineConfig.Telemetry.Cloud.Endpoint != "https://collector.example.com" {
			t.Errorf("expected the telemetry endpoint to be kept, got: %q", ci.InlineConfig.Telemetry.Cloud.Endpoint)
		}
		if ci.InlineConfig.Telemetry.Cloud.Headers != nil {
			t.Errorf("expected the telemetry header map to be absent, got: %+v", ci.InlineConfig.Telemetry.Cloud.Headers)
		}
		if ci.HarnessConfig != "claude" {
			t.Errorf("expected non-env create inputs to be kept, got harnessConfig %q", ci.HarnessConfig)
		}
	})

	t.Run("canAttach true shows create-input env minus GITHUB_TOKEN", func(t *testing.T) {
		ci := decodeCreateInputs(t, createInputsResponseConfig().ResponseView(true))
		if ci.InlineConfig.Env["CREATE_PLAIN_VAR"] != "create-plain-value" {
			t.Errorf("expected CREATE_PLAIN_VAR to be present, got: %+v", ci.InlineConfig.Env)
		}
		if _, ok := ci.InlineConfig.Env["GITHUB_TOKEN"]; ok {
			t.Errorf("GITHUB_TOKEN must never be marshaled in createInputs.inlineConfig.env, got: %+v", ci.InlineConfig.Env)
		}
		if ci.InlineConfig.Telemetry == nil || ci.InlineConfig.Telemetry.Cloud == nil ||
			ci.InlineConfig.Telemetry.Cloud.Headers["Authorization"] != "Bearer create-input-value" {
			t.Errorf("expected the telemetry header map to be present when canAttach is true, got: %+v", ci.InlineConfig.Telemetry)
		}
	})

	for _, canAttach := range []bool{false, true} {
		name := "the original is unchanged after ResponseView(false)"
		if canAttach {
			name = "the original is unchanged after ResponseView(true)"
		}
		t.Run(name, func(t *testing.T) {
			original := createInputsResponseConfig()
			view := original.ResponseView(canAttach)

			if view.CreateInputs == original.CreateInputs {
				t.Fatal("ResponseView must deep-copy CreateInputs instead of aliasing it")
			}
			if view.CreateInputs.ThinkingLevel == original.CreateInputs.ThinkingLevel {
				t.Error("ResponseView must not alias CreateInputs.ThinkingLevel")
			}
			inline := original.CreateInputs.InlineConfig
			if len(inline.Env) != 2 || inline.Env["GITHUB_TOKEN"] != "ghp_create_input_value" {
				t.Errorf("the original CreateInputs.InlineConfig.Env must be untouched, got: %+v", inline.Env)
			}
			if inline.Telemetry.Cloud.Headers["Authorization"] != "Bearer create-input-value" {
				t.Errorf("the original telemetry header map must be untouched, got: %+v", inline.Telemetry.Cloud.Headers)
			}
		})
	}

	t.Run("nil CreateInputs stays nil", func(t *testing.T) {
		view := (&AgentAppliedConfig{Image: "x"}).ResponseView(true)
		if view.CreateInputs != nil {
			t.Errorf("expected nil CreateInputs, got %+v", view.CreateInputs)
		}
	})
}
