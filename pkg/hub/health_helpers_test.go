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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// policyNow is the store clock the policy tests' registry rows are read at.
var policyNow = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

// policyRow is a registry row for instance id (label "hub-<id>") with the
// given status and checks, last written age before policyNow.
func policyRow(id, status string, age time.Duration, checks map[string]string) store.HubInstance {
	return store.HubInstance{
		ID: id, Label: "hub-" + id, Version: "v1", Status: status,
		StartedAt: policyNow.Add(-time.Hour), LastSeen: policyNow.Add(-age), Checks: checks,
	}
}

// stoppedPolicyRow is policyRow for an instance that stopped cleanly age
// before policyNow.
func stoppedPolicyRow(id string, age time.Duration) store.HubInstance {
	r := policyRow(id, HealthStatusHealthy, age, map[string]string{"database": "healthy"})
	at := policyNow.Add(-age)
	r.StoppedAt = &at
	return r
}

// setPolicyFleet sets the hub and hub_instances sections from registry rows
// with the handler's builders, served by instance "1".
func setPolicyFleet(r *HealthSummaryResponse, rows ...store.HubInstance) {
	r.Hub = buildHealthSummaryFleetHub(rows, policyNow, true)
	r.Hub.InstanceID = "1"
	r.HubInstances = buildHealthSummaryHubInstances(rows, policyNow, "1")
}

// healthyPolicyResp is a summary with nothing wrong: one live, healthy hub
// instance, one online broker, zero dispatch counts and no agents.
func healthyPolicyResp() *HealthSummaryResponse {
	r := &HealthSummaryResponse{
		Brokers: HealthSummaryBrokers{Items: []HealthSummaryBroker{
			{ID: "b-ok", Name: "ok", Status: store.BrokerStatusOnline},
		}, Total: 1},
		Agents:       &HealthSummaryAgents{ByPhase: []HealthPhaseCount{}, Problems: []HealthAgentGroup{}},
		Dispatch:     &HealthSummaryDispatch{},
		Integrations: []HealthSummaryIntegration{},
	}
	setPolicyFleet(r, policyRow("1", HealthStatusHealthy, time.Second, map[string]string{"database": "healthy"}))
	return r
}
