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
	"encoding/json"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// TestWithHubAgentDefaults covers the broker-side half of the Gap 2c plumbing:
// what the hub sent must reach the provisioning context, and what it did not
// send must leave the context untouched so no new rung fires.
func TestWithHubAgentDefaults(t *testing.T) {
	tests := []struct {
		name string
		cfg  *CreateAgentConfig
		want *api.HubAgentDefaults
	}{
		{
			name: "nil config",
			cfg:  nil,
		},
		{
			name: "config without hub defaults (file-mode hub, local dispatch, or old hub)",
			cfg:  &CreateAgentConfig{Task: "t"},
		},
		{
			name: "empty hub defaults are treated as absent",
			cfg:  &CreateAgentConfig{HubAgentDefaults: &api.HubAgentDefaults{}},
		},
		{
			name: "limits reach the context",
			cfg: &CreateAgentConfig{HubAgentDefaults: &api.HubAgentDefaults{
				MaxTurns:      50,
				MaxModelCalls: 200,
				MaxDuration:   "2h",
			}},
			want: &api.HubAgentDefaults{MaxTurns: 50, MaxModelCalls: 200, MaxDuration: "2h"},
		},
		{
			name: "resources alone are enough to carry",
			cfg: &CreateAgentConfig{HubAgentDefaults: &api.HubAgentDefaults{
				Resources: &api.ResourceSpec{Limits: api.ResourceList{CPU: "2"}},
			}},
			want: &api.HubAgentDefaults{Resources: &api.ResourceSpec{Limits: api.ResourceList{CPU: "2"}}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := api.HubAgentDefaultsFromContext(withHubAgentDefaults(context.Background(), tc.cfg))
			if tc.want == nil {
				if got != nil {
					t.Fatalf("want nothing on the context, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatal("want hub defaults on the context, got nil")
			}
			if got.MaxTurns != tc.want.MaxTurns || got.MaxModelCalls != tc.want.MaxModelCalls ||
				got.MaxDuration != tc.want.MaxDuration {
				t.Errorf("limits: want %+v, got %+v", tc.want, got)
			}
			if (got.Resources == nil) != (tc.want.Resources == nil) {
				t.Fatalf("resources: want %+v, got %+v", tc.want.Resources, got.Resources)
			}
			if got.Resources != nil && *got.Resources != *tc.want.Resources {
				t.Errorf("resources: want %+v, got %+v", *tc.want.Resources, *got.Resources)
			}
		})
	}
}

// TestCreateAgentConfig_DecodesHubAgentDefaults pins the receiving end of the
// wire against the JSON the hub actually emits. The hub's struct is a separate
// type in a package this one cannot import, so this is the broker-side half of
// the compatibility pair (pkg/hub has the other).
func TestCreateAgentConfig_DecodesHubAgentDefaults(t *testing.T) {
	const fromHub = `{"harnessConfig":"claude-vertex","hubAgentDefaults":{"maxTurns":50,` +
		`"maxModelCalls":200,"maxDuration":"2h","resources":{"limits":{"cpu":"2"},"disk":"10Gi"}}}`

	var cfg CreateAgentConfig
	if err := json.Unmarshal([]byte(fromHub), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.HubAgentDefaults == nil {
		t.Fatal("hubAgentDefaults did not decode")
	}
	if cfg.HubAgentDefaults.MaxTurns != 50 || cfg.HubAgentDefaults.MaxModelCalls != 200 ||
		cfg.HubAgentDefaults.MaxDuration != "2h" {
		t.Errorf("limits decoded wrong: %+v", cfg.HubAgentDefaults)
	}
	if cfg.HubAgentDefaults.Resources == nil ||
		cfg.HubAgentDefaults.Resources.Limits.CPU != "2" ||
		cfg.HubAgentDefaults.Resources.Disk != "10Gi" {
		t.Errorf("resources decoded wrong: %+v", cfg.HubAgentDefaults.Resources)
	}

	// A payload from a hub that predates the field must leave it nil, not
	// zero-valued — that is what keeps the broker's own tiers in charge.
	var old CreateAgentConfig
	if err := json.Unmarshal([]byte(`{"harnessConfig":"claude-vertex"}`), &old); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if old.HubAgentDefaults != nil {
		t.Errorf("want nil for a payload without the key, got %+v", old.HubAgentDefaults)
	}
}
