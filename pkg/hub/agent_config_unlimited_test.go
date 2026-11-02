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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
)

// TestUnlimitedCountChanges: only a change of a count limit to 0 counts; an
// echo of a stored 0, an unset key and a value do not (ptone/scion#5605).
func TestUnlimitedCountChanges(t *testing.T) {
	zero, five := api.IntPtr(0), api.IntPtr(5)
	for _, tc := range []struct {
		name      string
		req, base *api.ScionConfig
		want      []string
	}{
		{name: "nil request", req: nil, base: &api.ScionConfig{MaxTurns: five}},
		{name: "unset keys", req: &api.ScionConfig{}, base: &api.ScionConfig{MaxTurns: five}},
		{name: "a value", req: &api.ScionConfig{MaxTurns: api.IntPtr(7)}, base: &api.ScionConfig{MaxTurns: five}},
		{name: "change from a value", req: &api.ScionConfig{MaxTurns: zero}, base: &api.ScionConfig{MaxTurns: five}, want: []string{"config.max_turns"}},
		{name: "change from unset", req: &api.ScionConfig{MaxModelCalls: zero}, base: &api.ScionConfig{}, want: []string{"config.max_model_calls"}},
		{name: "change with no base", req: &api.ScionConfig{MaxTurns: zero, MaxModelCalls: zero}, base: nil, want: []string{"config.max_turns", "config.max_model_calls"}},
		{name: "echo of a stored zero", req: &api.ScionConfig{MaxTurns: zero}, base: &api.ScionConfig{MaxTurns: api.IntPtr(0)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, unlimitedCountChanges(tc.req, tc.base))
		})
	}
}

// TestUnlimitedCountCreateWarning: a create whose dispatched inline config
// sets a count limit to 0 warns only when the broker lacks
// unlimitedCountLimits.
func TestUnlimitedCountCreateWarning(t *testing.T) {
	cfg := &api.ScionConfig{MaxTurns: api.IntPtr(0), MaxModelCalls: api.IntPtr(0)}
	got := unlimitedCountCreateWarning(cfg, &store.BrokerCapabilities{ClearedFields: true})
	assert.Contains(t, got, "config.max_turns, config.max_model_calls: this agent's runtime broker keeps the inherited limit")
	assert.Contains(t, unlimitedCountCreateWarning(cfg, nil), "config.max_turns")
	assert.Empty(t, unlimitedCountCreateWarning(cfg, &store.BrokerCapabilities{UnlimitedCountLimits: true}))
	assert.Empty(t, unlimitedCountCreateWarning(&api.ScionConfig{MaxTurns: api.IntPtr(3)}, nil))
	assert.Empty(t, unlimitedCountCreateWarning(nil, nil))
}

// TestCapabilitiesFromStrings_UnlimitedCountLimits: the hub parses the
// broker capability in either spelling.
func TestCapabilitiesFromStrings_UnlimitedCountLimits(t *testing.T) {
	for _, name := range []string{"unlimitedCountLimits", "unlimited_count_limits", " UnlimitedCountLimits "} {
		assert.True(t, capabilitiesFromStrings([]string{name}).UnlimitedCountLimits, name)
	}
	assert.False(t, capabilitiesFromStrings([]string{"clearedFields"}).UnlimitedCountLimits)
	assert.True(t, brokerStartSupportFrom(&store.BrokerCapabilities{UnlimitedCountLimits: true}).UnlimitedCountLimits)
}
