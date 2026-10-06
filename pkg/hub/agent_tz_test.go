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

package hub

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestChooseAgentTZ_Chain(t *testing.T) {
	storageUser := agentTZ{TZ: "America/Denver", Source: TZSourceUser}

	tests := []struct {
		name            string
		ac              *store.AgentAppliedConfig
		storage         agentTZ
		hubDefault      string
		forGatherAnswer bool
		want            agentTZ
	}{
		{
			name:       "explicit pin beats storage and hub default",
			ac:         &store.AgentAppliedConfig{ExplicitTimezone: "Europe/Paris"},
			storage:    storageUser,
			hubDefault: "Asia/Tokyo",
			want:       agentTZ{TZ: "Europe/Paris", Source: TZSourceExplicit},
		},
		{
			name:       "adopted legacy pin is labelled legacy",
			ac:         &store.AgentAppliedConfig{ExplicitTimezone: "Europe/Paris", ExplicitTimezoneLegacy: true},
			storage:    storageUser,
			hubDefault: "Asia/Tokyo",
			want:       agentTZ{TZ: "Europe/Paris", Source: TZSourceLegacy},
		},
		{
			name:       "storage beats hub default",
			ac:         &store.AgentAppliedConfig{},
			storage:    storageUser,
			hubDefault: "Asia/Tokyo",
			want:       storageUser,
		},
		{
			name:       "storage source label is passed through",
			ac:         &store.AgentAppliedConfig{},
			storage:    agentTZ{TZ: "Asia/Kathmandu", Source: TZSourceProgeny},
			hubDefault: "Asia/Tokyo",
			want:       agentTZ{TZ: "Asia/Kathmandu", Source: TZSourceProgeny},
		},
		{
			name:       "hub default when no pin and no storage",
			ac:         &store.AgentAppliedConfig{},
			hubDefault: "Asia/Tokyo",
			want:       agentTZ{TZ: "Asia/Tokyo", Source: TZSourceHubDefault},
		},
		{
			name: "nothing gives no TZ",
			ac:   &store.AgentAppliedConfig{},
			want: agentTZ{TZ: "", Source: TZSourceNone},
		},
		{
			name:            "nothing answers a gather need with UTC",
			ac:              &store.AgentAppliedConfig{},
			forGatherAnswer: true,
			want:            agentTZ{TZ: "UTC", Source: TZSourceNone},
		},
		{
			name:            "gather answer does not override a lower rung",
			ac:              &store.AgentAppliedConfig{},
			hubDefault:      "Asia/Tokyo",
			forGatherAnswer: true,
			want:            agentTZ{TZ: "Asia/Tokyo", Source: TZSourceHubDefault},
		},
		{
			name:       "nil applied config falls through to storage",
			ac:         nil,
			storage:    storageUser,
			hubDefault: "Asia/Tokyo",
			want:       storageUser,
		},
		{
			name:       "unpinned flag alone does not block lower rungs",
			ac:         &store.AgentAppliedConfig{ExplicitTimezoneUnpinned: true},
			hubDefault: "Asia/Tokyo",
			want:       agentTZ{TZ: "Asia/Tokyo", Source: TZSourceHubDefault},
		},
		{
			name: "unpinned with no rungs gives no TZ",
			ac:   &store.AgentAppliedConfig{ExplicitTimezoneUnpinned: true},
			want: agentTZ{TZ: "", Source: TZSourceNone},
		},
		{
			name:       "env TZ is not a rung",
			ac:         &store.AgentAppliedConfig{Env: map[string]string{"TZ": "Europe/Paris"}},
			hubDefault: "Asia/Tokyo",
			want:       agentTZ{TZ: "Asia/Tokyo", Source: TZSourceHubDefault},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := chooseAgentTZ(tt.ac, tt.storage, tt.hubDefault, tt.forGatherAnswer)
			if got != tt.want {
				t.Fatalf("chooseAgentTZ() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestAgentAppliedConfig_ExplicitTimezoneJSON checks the three fields
// round-trip through JSON (the store re-marshals AppliedConfig) and are
// omitted when zero, so records written before them are unchanged.
func TestAgentAppliedConfig_ExplicitTimezoneJSON(t *testing.T) {
	empty, err := json.Marshal(store.AgentAppliedConfig{})
	if err != nil {
		t.Fatalf("marshal empty: %v", err)
	}
	for _, key := range []string{"explicitTimezone", "explicitTimezoneLegacy", "explicitTimezoneUnpinned"} {
		if strings.Contains(string(empty), key) {
			t.Errorf("empty config JSON %s contains %q; want omitted", empty, key)
		}
	}

	in := store.AgentAppliedConfig{
		ExplicitTimezone:         "Asia/Kathmandu",
		ExplicitTimezoneLegacy:   true,
		ExplicitTimezoneUnpinned: true,
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out store.AgentAppliedConfig
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal %s: %v", data, err)
	}
	if out.ExplicitTimezone != in.ExplicitTimezone || out.ExplicitTimezoneLegacy != in.ExplicitTimezoneLegacy || out.ExplicitTimezoneUnpinned != in.ExplicitTimezoneUnpinned {
		t.Fatalf("round trip = %+v, want the three timezone fields of %+v", out, in)
	}
}
