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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestApplyBrokerAgentConfig_RecordsHarnessConfigSource: the broker-reported
// harness-config provenance (ptone/scion#620) lands on the hub agent record,
// and an older broker that omits it leaves the recorded value alone.
func TestApplyBrokerAgentConfig_RecordsHarnessConfigSource(t *testing.T) {
	var info RemoteAgentInfo
	if err := json.Unmarshal([]byte(`{"id":"a","slug":"a","containerId":"c","name":"a","status":"running","harnessConfig":"claude","harnessConfigSource":"broker-local"}`), &info); err != nil {
		t.Fatal(err)
	}
	agent := &store.Agent{AppliedConfig: &store.AgentAppliedConfig{HarnessConfigID: "hc-1"}}
	applyBrokerAgentConfig(agent, &info)
	if got := agent.AppliedConfig.HarnessConfigSource; got != "broker-local" {
		t.Errorf("HarnessConfigSource = %q, want broker-local", got)
	}

	applyBrokerAgentConfig(agent, &RemoteAgentInfo{Name: "a"})
	if got := agent.AppliedConfig.HarnessConfigSource; got != "broker-local" {
		t.Errorf("older broker response cleared HarnessConfigSource: got %q", got)
	}
}
