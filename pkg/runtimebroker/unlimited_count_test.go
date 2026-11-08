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
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// TestApplyInlineConfigUpdate_UnlimitedCountReplacesPersisted: the start
// merge writes an inline count limit of 0 ("no limit") over the agent's
// persisted limit, and an unset inline limit keeps it (ptone/scion#5605).
func TestApplyInlineConfigUpdate_UnlimitedCountReplacesPersisted(t *testing.T) {
	srv, _ := newTestServerWithProvisionCapture()
	projectDir := filepath.Join(t.TempDir(), ".scion")
	agentName := "unlimited-agent"
	cfgPath := writeAgentConfig(t, projectDir, agentName, api.ScionConfig{MaxTurns: api.IntPtr(5), MaxModelCalls: api.IntPtr(6)})

	srv.applyInlineConfigUpdate(context.Background(), agentName, projectDir, &api.ScionConfig{MaxTurns: api.IntPtr(0)}, false)

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var got api.ScionConfig
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if !api.IsUnlimitedCount(got.MaxTurns) {
		t.Errorf("max_turns = %s, want 0 (no limit): %s", api.FormatCountLimit(got.MaxTurns), data)
	}
	if !api.IntPtrEqual(got.MaxModelCalls, api.IntPtr(6)) {
		t.Errorf("max_model_calls = %s, want the persisted 6: %s", api.FormatCountLimit(got.MaxModelCalls), data)
	}
}

// TestStaticCapabilities_UnlimitedCountLimits: this broker advertises that it
// honours an explicit count limit of 0, so the hub accepts Unlimited for its
// agents.
func TestStaticCapabilities_UnlimitedCountLimits(t *testing.T) {
	if !StaticCapabilities(nil, "").UnlimitedCountLimits {
		t.Fatal("StaticCapabilities: UnlimitedCountLimits = false")
	}
	found := false
	for _, n := range StaticCapabilityNames(nil) {
		if n == "unlimitedCountLimits" {
			found = true
		}
	}
	if !found {
		t.Fatal("StaticCapabilityNames lacks unlimitedCountLimits")
	}
}
