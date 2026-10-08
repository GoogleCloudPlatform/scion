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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// healthyPolicyResp is a summary with nothing wrong: a healthy hub, one
// online broker, zero dispatch counts and no agents.
func healthyPolicyResp() *HealthSummaryResponse {
	return &HealthSummaryResponse{
		Hub: HealthSummaryHub{Status: HealthStatusHealthy, InstanceID: "inst-1", Checks: map[string]string{"database": "healthy"}},
		Brokers: HealthSummaryBrokers{Items: []HealthSummaryBroker{
			{ID: "b-ok", Name: "ok", Status: store.BrokerStatusOnline},
		}, Total: 1},
		Agents:       &HealthSummaryAgents{ByPhase: []HealthPhaseCount{}, Problems: []HealthAgentGroup{}},
		Dispatch:     &HealthSummaryDispatch{},
		Integrations: []HealthSummaryIntegration{},
	}
}

func attentionKinds(items []HealthAttentionItem) []string {
	out := []string{}
	for _, it := range items {
		out = append(out, it.Kind)
	}
	return out
}

func attentionMessages(items []HealthAttentionItem) []string {
	out := []string{}
	for _, it := range items {
		out = append(out, it.Message)
	}
	return out
}

func TestDeriveHealthSummaryStatus_Rules(t *testing.T) {
	cases := []struct {
		name       string
		mutate     func(r *HealthSummaryResponse)
		wantStatus string
		wantItems  []HealthAttentionItem
	}{
		{
			name:       "all healthy",
			mutate:     func(*HealthSummaryResponse) {},
			wantStatus: HealthStatusHealthy,
			wantItems:  []HealthAttentionItem{},
		},
		{
			name: "critical hub check is critical",
			mutate: func(r *HealthSummaryResponse) {
				r.Hub.Status = HealthStatusUnhealthy
				r.Hub.Checks = map[string]string{"database": "unhealthy"}
			},
			wantStatus: HealthStatusUnhealthy,
			wantItems: []HealthAttentionItem{{
				Severity: HealthAttentionCritical, Kind: HealthAttentionHubCheck,
				Subject: HealthAttentionSubject{Type: HealthSubjectHub, ID: "inst-1"},
				Message: "Hub check database is not healthy on this instance",
			}},
		},
		{
			name: "non-critical hub check is a warning and keeps its raw value out",
			mutate: func(r *HealthSummaryResponse) {
				r.Hub.Status = HealthStatusDegraded
				r.Hub.Checks = map[string]string{"database": "healthy", "colocated_broker": "unhealthy: registration failed"}
			},
			wantStatus: HealthStatusDegraded,
			wantItems: []HealthAttentionItem{{
				Severity: HealthAttentionWarning, Kind: HealthAttentionHubCheck,
				Subject: HealthAttentionSubject{Type: HealthSubjectHub, ID: "inst-1"},
				Message: "Hub check colocated_broker is not healthy on this instance",
			}},
		},
		{
			name: "offline broker degrades",
			mutate: func(r *HealthSummaryResponse) {
				r.Brokers.Items = append(r.Brokers.Items, HealthSummaryBroker{ID: "b-off", Name: "off", Status: store.BrokerStatusOffline})
			},
			wantStatus: HealthStatusDegraded,
			wantItems: []HealthAttentionItem{{
				Severity: HealthAttentionWarning, Kind: HealthAttentionBrokerOffline,
				Subject: HealthAttentionSubject{Type: HealthSubjectBroker, ID: "b-off", Name: "off"},
				Message: "Runtime broker off is offline",
			}},
		},
		{
			name: "broker in another non-online status degrades",
			mutate: func(r *HealthSummaryResponse) {
				r.Brokers.Items[0].Status = "degraded"
			},
			wantStatus: HealthStatusDegraded,
			wantItems: []HealthAttentionItem{{
				Severity: HealthAttentionWarning, Kind: HealthAttentionBrokerOffline,
				Subject: HealthAttentionSubject{Type: HealthSubjectBroker, ID: "b-ok", Name: "ok"},
				Message: "Runtime broker ok is not online",
			}},
		},
		{
			name: "online broker reporting degraded health degrades",
			mutate: func(r *HealthSummaryResponse) {
				r.Brokers.Items[0].Health = &HealthBrokerSelf{Status: HealthStatusDegraded, Checks: map[string]string{
					"runtime": "unavailable", "nfs_mounts": "healthy",
				}}
			},
			wantStatus: HealthStatusDegraded,
			wantItems: []HealthAttentionItem{{
				Severity: HealthAttentionWarning, Kind: HealthAttentionBrokerDegraded,
				Subject: HealthAttentionSubject{Type: HealthSubjectBroker, ID: "b-ok", Name: "ok"},
				Message: "Runtime broker ok reports degraded (runtime: unavailable)",
			}},
		},
		{
			name: "online broker reporting unhealthy health degrades, never unhealthy",
			mutate: func(r *HealthSummaryResponse) {
				r.Brokers.Items[0].Health = &HealthBrokerSelf{Status: HealthStatusUnhealthy, Checks: map[string]string{}}
			},
			wantStatus: HealthStatusDegraded,
			wantItems: []HealthAttentionItem{{
				Severity: HealthAttentionWarning, Kind: HealthAttentionBrokerDegraded,
				Subject: HealthAttentionSubject{Type: HealthSubjectBroker, ID: "b-ok", Name: "ok"},
				Message: "Runtime broker ok reports unhealthy",
			}},
		},
		{
			name: "broker health not reported is neutral",
			mutate: func(r *HealthSummaryResponse) {
				r.Brokers.Items[0].Health = nil
			},
			wantStatus: HealthStatusHealthy,
			wantItems:  []HealthAttentionItem{},
		},
		{
			name: "unhealthy NFS share degrades",
			mutate: func(r *HealthSummaryResponse) {
				r.Brokers.Items[0].WorkspaceStorage = &HealthBrokerStorage{Backend: "nfs", NFSHealthy: boolPtr(false)}
			},
			wantStatus: HealthStatusDegraded,
			wantItems: []HealthAttentionItem{{
				Severity: HealthAttentionWarning, Kind: HealthAttentionBrokerNFS,
				Subject: HealthAttentionSubject{Type: HealthSubjectBroker, ID: "b-ok", Name: "ok"},
				Message: "Runtime broker ok reports its NFS workspace storage unhealthy",
			}},
		},
		{
			name: "healthy NFS share and local storage are neutral",
			mutate: func(r *HealthSummaryResponse) {
				r.Brokers.Items[0].WorkspaceStorage = &HealthBrokerStorage{Backend: "nfs", NFSHealthy: boolPtr(true)}
				r.Brokers.Items = append(r.Brokers.Items, HealthSummaryBroker{
					ID: "b-local", Name: "local", Status: store.BrokerStatusOnline,
					WorkspaceStorage: &HealthBrokerStorage{Backend: "local"},
				})
			},
			wantStatus: HealthStatusHealthy,
			wantItems:  []HealthAttentionItem{},
		},
		{
			name: "offline broker with stale problems gets one item",
			mutate: func(r *HealthSummaryResponse) {
				r.Brokers.Items[0].Status = store.BrokerStatusOffline
				r.Brokers.Items[0].Health = &HealthBrokerSelf{Status: HealthStatusDegraded, Checks: map[string]string{}}
				r.Brokers.Items[0].WorkspaceStorage = &HealthBrokerStorage{Backend: "nfs", NFSHealthy: boolPtr(false)}
			},
			wantStatus: HealthStatusDegraded,
			wantItems: []HealthAttentionItem{{
				Severity: HealthAttentionWarning, Kind: HealthAttentionBrokerOffline,
				Subject: HealthAttentionSubject{Type: HealthSubjectBroker, ID: "b-ok", Name: "ok"},
				Message: "Runtime broker ok is offline",
			}},
		},
		{
			name: "broker list not reported is a warning only",
			mutate: func(r *HealthSummaryResponse) {
				r.Brokers = HealthSummaryBrokers{Items: []HealthSummaryBroker{}, NotReported: true}
			},
			wantStatus: HealthStatusHealthy,
			wantItems: []HealthAttentionItem{{
				Severity: HealthAttentionWarning, Kind: HealthAttentionHubCheck,
				Subject: HealthAttentionSubject{Type: HealthSubjectHub, ID: "inst-1"},
				Message: "Runtime broker data not available",
			}},
		},
		{
			name: "stuck messages degrade",
			mutate: func(r *HealthSummaryResponse) {
				r.Dispatch.StuckMessages = 3
			},
			wantStatus: HealthStatusDegraded,
			wantItems: []HealthAttentionItem{{
				Severity: HealthAttentionWarning, Kind: HealthAttentionDispatch,
				Subject: HealthAttentionSubject{Type: HealthSubjectDispatch},
				Message: "3 agent messages stuck pending delivery",
			}},
		},
		{
			name: "stuck broker dispatch degrades",
			mutate: func(r *HealthSummaryResponse) {
				r.Dispatch.StuckBrokerDispatch = 1
			},
			wantStatus: HealthStatusDegraded,
			wantItems: []HealthAttentionItem{{
				Severity: HealthAttentionWarning, Kind: HealthAttentionDispatch,
				Subject: HealthAttentionSubject{Type: HealthSubjectDispatch},
				Message: "1 broker dispatch stuck in progress",
			}},
		},
		{
			name: "failed dispatches alone are neutral",
			mutate: func(r *HealthSummaryResponse) {
				r.Dispatch.FailedBrokerDispatch1h = 7
			},
			wantStatus: HealthStatusHealthy,
			wantItems:  []HealthAttentionItem{},
		},
		{
			name: "null dispatch is a warning only",
			mutate: func(r *HealthSummaryResponse) {
				r.Dispatch = nil
			},
			wantStatus: HealthStatusHealthy,
			wantItems: []HealthAttentionItem{{
				Severity: HealthAttentionWarning, Kind: HealthAttentionDispatch,
				Subject: HealthAttentionSubject{Type: HealthSubjectDispatch},
				Message: "Dispatch data not available",
			}},
		},
		{
			name: "unhealthy integration degrades",
			mutate: func(r *HealthSummaryResponse) {
				r.Integrations = []HealthSummaryIntegration{{Name: "slack", Health: HealthStatusUnhealthy}}
			},
			wantStatus: HealthStatusDegraded,
			wantItems: []HealthAttentionItem{{
				Severity: HealthAttentionWarning, Kind: HealthAttentionIntegration,
				Subject: HealthAttentionSubject{Type: HealthSubjectIntegration, ID: "slack", Name: "slack"},
				Message: "Integration slack is unhealthy",
			}},
		},
		{
			name: "degraded integration is a warning only",
			mutate: func(r *HealthSummaryResponse) {
				r.Integrations = []HealthSummaryIntegration{{Name: "slack", Health: HealthStatusDegraded}}
			},
			wantStatus: HealthStatusHealthy,
			wantItems: []HealthAttentionItem{{
				Severity: HealthAttentionWarning, Kind: HealthAttentionIntegration,
				Subject: HealthAttentionSubject{Type: HealthSubjectIntegration, ID: "slack", Name: "slack"},
				Message: "Integration slack is degraded",
			}},
		},
		{
			name: "integration not reported or not managed is neutral",
			mutate: func(r *HealthSummaryResponse) {
				r.Integrations = []HealthSummaryIntegration{
					{Name: "a", Health: healthIntegrationUnknown, Reason: healthIntegrationTimedOutReason},
					{Name: "b", Health: healthIntegrationUnknown, Reason: healthIntegrationNotManagedReason},
					{Name: "c", Health: HealthStatusHealthy},
				}
			},
			wantStatus: HealthStatusHealthy,
			wantItems:  []HealthAttentionItem{},
		},
		{
			name: "null agents skips the ratio and warns",
			mutate: func(r *HealthSummaryResponse) {
				r.Agents = nil
			},
			wantStatus: HealthStatusHealthy,
			wantItems: []HealthAttentionItem{{
				Severity: HealthAttentionWarning, Kind: HealthAttentionAgents,
				Subject: HealthAttentionSubject{Type: HealthSubjectAgents},
				Message: "Agent data not available",
			}},
		},
		{
			name: "offline agents are warnings only",
			mutate: func(r *HealthSummaryResponse) {
				r.Agents.Considered = 2
				r.Agents.Problems = []HealthAgentGroup{{Kind: HealthAgentGroupOffline, Count: 1, Items: []HealthAgentRef{
					{ID: "a1", Name: "worker", ProjectID: "p1", ProjectSlug: "proj"},
				}}}
			},
			wantStatus: HealthStatusHealthy,
			wantItems: []HealthAttentionItem{{
				Severity: HealthAttentionWarning, Kind: HealthAttentionAgents,
				Subject: HealthAttentionSubject{Type: HealthSubjectAgent, ID: "a1", Name: "worker", ProjectID: "p1"},
				Message: "Agent proj / worker is offline",
			}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := healthyPolicyResp()
			tc.mutate(r)
			status, items := deriveHealthSummaryStatus(r)
			assert.Equal(t, tc.wantStatus, status)
			assert.Equal(t, tc.wantItems, items)
		})
	}
}

// TestDeriveHealthSummaryStatus_AgentErrorRatio covers the 5% boundary.
func TestDeriveHealthSummaryStatus_AgentErrorRatio(t *testing.T) {
	cases := []struct {
		name                string
		errored, considered int
		wantStatus          string
		wantRatioItem       string
	}{
		{"zero considered", 0, 0, HealthStatusHealthy, ""},
		{"none errored", 0, 40, HealthStatusHealthy, ""},
		{"just below 5%", 1, 21, HealthStatusHealthy, ""},
		{"just below 5% at scale", 49, 1000, HealthStatusHealthy, ""},
		{"exactly 5%", 1, 20, HealthStatusDegraded, "Error or crashed: 1 of 20 agents (5%)"},
		{"exactly 5% at scale", 50, 1000, HealthStatusDegraded, "Error or crashed: 50 of 1000 agents (5%)"},
		{"above 5%", 2, 39, HealthStatusDegraded, "Error or crashed: 2 of 39 agents (5.1%)"},
		{"all errored", 3, 3, HealthStatusDegraded, "Error or crashed: 3 of 3 agents (100%)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := healthyPolicyResp()
			r.Agents.Errored = tc.errored
			r.Agents.Considered = tc.considered
			status, items := deriveHealthSummaryStatus(r)
			assert.Equal(t, tc.wantStatus, status)
			if tc.wantRatioItem == "" {
				assert.Empty(t, items)
				return
			}
			assert.Equal(t, []HealthAttentionItem{{
				Severity: HealthAttentionWarning, Kind: HealthAttentionAgents,
				Subject: HealthAttentionSubject{Type: HealthSubjectAgents},
				Message: tc.wantRatioItem,
			}}, items)
		})
	}
	assert.InDelta(t, agentErrorDegradedRatio, 1.0/agentErrorRatioDenominator, 1e-12,
		"the integer comparison must match the ratio")
}

// TestDeriveHealthSummaryStatus_AgentGroups: one item per listed agent in
// group order, plus an overflow item beyond the reference cap. Error and
// crashed agents below the ratio do not change the status.
func TestDeriveHealthSummaryStatus_AgentGroups(t *testing.T) {
	r := healthyPolicyResp()
	r.Agents.Errored = 2
	r.Agents.Considered = 100
	r.Agents.Problems = []HealthAgentGroup{
		{Kind: HealthAgentGroupErrored, Count: 3, Items: []HealthAgentRef{
			{ID: "e1", Name: "e1", ProjectID: "p1", ProjectSlug: "alpha"},
			{ID: "e2", Name: "e2", ProjectID: "p2"},
		}},
		{Kind: HealthAgentGroupCrashed, Count: 1, Items: []HealthAgentRef{{ID: "c1", Name: "c1", ProjectID: "p1", ProjectSlug: "alpha"}}},
	}
	status, items := deriveHealthSummaryStatus(r)
	assert.Equal(t, HealthStatusHealthy, status)
	assert.Equal(t, []string{
		"Agent alpha / e1 is in the error phase",
		"Agent e2 is in the error phase",
		"1 more agent in the error phase",
		"Agent alpha / c1 has crashed",
	}, attentionMessages(items))
	assert.Equal(t, HealthAttentionSubject{Type: HealthSubjectAgents}, items[2].Subject)
	assert.Equal(t, HealthAttentionSubject{Type: HealthSubjectAgent, ID: "c1", Name: "c1", ProjectID: "p1"}, items[3].Subject)
}

// TestDeriveHealthSummaryStatus_Ordering: critical hub checks, other hub
// checks, brokers, integrations, dispatch, the ratio item, then agents.
func TestDeriveHealthSummaryStatus_Ordering(t *testing.T) {
	r := healthyPolicyResp()
	r.Hub.Status = HealthStatusUnhealthy
	r.Hub.Checks = map[string]string{
		"database": "unhealthy", "workspace_storage": "unhealthy: mount not available",
		"colocated_broker": "unhealthy: registration pending", "audit": "degraded",
	}
	r.Brokers.Items = []HealthSummaryBroker{
		{ID: "b1", Name: "b1", Status: store.BrokerStatusOffline},
		{ID: "b2", Name: "b2", Status: store.BrokerStatusOnline, Health: &HealthBrokerSelf{Status: HealthStatusDegraded}},
		{ID: "b3", Name: "b3", Status: store.BrokerStatusOnline, WorkspaceStorage: &HealthBrokerStorage{Backend: "nfs", NFSHealthy: boolPtr(false)}},
	}
	r.Integrations = []HealthSummaryIntegration{{Name: "chat", Health: HealthStatusUnhealthy}}
	r.Dispatch = &HealthSummaryDispatch{StuckMessages: 1, StuckBrokerDispatch: 1}
	r.Agents.Errored = 1
	r.Agents.Considered = 1
	r.Agents.Problems = []HealthAgentGroup{
		{Kind: HealthAgentGroupErrored, Count: 1, Items: []HealthAgentRef{{ID: "e", Name: "e"}}},
		{Kind: HealthAgentGroupCrashed, Count: 1, Items: []HealthAgentRef{{ID: "c", Name: "c"}}},
		{Kind: HealthAgentGroupOffline, Count: 1, Items: []HealthAgentRef{{ID: "o", Name: "o"}}},
	}

	status, items := deriveHealthSummaryStatus(r)
	assert.Equal(t, HealthStatusUnhealthy, status)
	assert.Equal(t, []string{
		"Hub check database is not healthy on this instance",
		"Hub check workspace_storage is not healthy on this instance",
		"Hub check audit is not healthy on this instance",
		"Hub check colocated_broker is not healthy on this instance",
		"Runtime broker b1 is offline",
		"Runtime broker b2 reports degraded",
		"Runtime broker b3 reports its NFS workspace storage unhealthy",
		"Integration chat is unhealthy",
		"1 agent message stuck pending delivery",
		"1 broker dispatch stuck in progress",
		"Error or crashed: 1 of 1 agents (100%)",
		"Agent e is in the error phase",
		"Agent c has crashed",
		"Agent o is offline",
	}, attentionMessages(items))
	assert.Equal(t, HealthAttentionCritical, items[0].Severity)
	assert.Equal(t, HealthAttentionCritical, items[1].Severity)
	for _, it := range items[2:] {
		assert.Equal(t, HealthAttentionWarning, it.Severity, it.Message)
	}
	assert.Equal(t, []string{
		HealthAttentionHubCheck, HealthAttentionHubCheck, HealthAttentionHubCheck, HealthAttentionHubCheck,
		HealthAttentionBrokerOffline, HealthAttentionBrokerDegraded, HealthAttentionBrokerNFS,
		HealthAttentionIntegration, HealthAttentionDispatch, HealthAttentionDispatch,
		HealthAttentionAgents, HealthAttentionAgents, HealthAttentionAgents, HealthAttentionAgents,
	}, attentionKinds(items))

	// The result does not depend on map iteration order.
	for i := 0; i < 20; i++ {
		_, again := deriveHealthSummaryStatus(r)
		require.Equal(t, items, again)
	}
}

// TestDeriveHealthSummaryStatus_NoRawErrorText: values of hub checks and
// other free text never reach a message; messages are fixed templates
// filled with identifiers and counts.
func TestDeriveHealthSummaryStatus_NoRawErrorText(t *testing.T) {
	const secret = "dial tcp 10.0.0.1:5432: password=hunter2"
	r := healthyPolicyResp()
	r.Hub.Status = HealthStatusUnhealthy
	r.Hub.Checks = map[string]string{"database": "unhealthy: " + secret, "colocated_broker": secret}
	r.Hub.UnhealthyChecks = []string{"database: unhealthy: " + secret}
	r.Brokers.Items = append(r.Brokers.Items, HealthSummaryBroker{ID: "b2", Name: "b2", Status: secret})
	r.Integrations = []HealthSummaryIntegration{{Name: "chat", Health: HealthStatusUnhealthy, Reason: secret}}
	r.Dispatch = nil
	r.Agents = nil

	_, items := deriveHealthSummaryStatus(r)
	require.NotEmpty(t, items)
	for _, it := range items {
		for _, frag := range []string{"hunter2", "dial tcp", "10.0.0.1", "password"} {
			assert.False(t, strings.Contains(it.Message, frag), "message %q carries raw text", it.Message)
		}
	}
}

func TestFormatPercent(t *testing.T) {
	assert.Equal(t, "5", formatPercent(1, 20))
	assert.Equal(t, "4.9", formatPercent(49, 1000))
	assert.Equal(t, "4.9", formatPercent(499, 10000), "rounded down, never up to the threshold")
	assert.Equal(t, "33.3", formatPercent(1, 3))
	assert.Equal(t, "0", formatPercent(0, 0))
}

// TestOmitHealthSummaryIntegrationDetail: identity-free aggregate items
// replace the per-integration items in place.
func TestOmitHealthSummaryIntegrationDetail(t *testing.T) {
	r := healthyPolicyResp()
	r.Brokers.Items[0].Status = store.BrokerStatusOffline
	r.Integrations = []HealthSummaryIntegration{
		{Name: "a-chat", Health: HealthStatusUnhealthy, Version: "v1"},
		{Name: "b-chat", Health: HealthStatusDegraded},
		{Name: "c-chat", Health: HealthStatusUnhealthy},
	}
	r.Dispatch.StuckMessages = 1
	r.IntegrationsDetail = true
	r.IntegrationCounts = healthSummaryIntegrationCounts(r.Integrations)
	r.Status, r.Attention = deriveHealthSummaryStatus(r)

	omitHealthSummaryIntegrationDetail(r)
	assert.Equal(t, []HealthSummaryIntegration{}, r.Integrations)
	assert.False(t, r.IntegrationsDetail)
	assert.Equal(t, HealthSummaryIntegrationCounts{Total: 3, Degraded: 1, Unhealthy: 2}, r.IntegrationCounts)
	assert.Equal(t, HealthStatusDegraded, r.Status)
	assert.Equal(t, []string{
		"Runtime broker ok is offline",
		"2 integrations unhealthy",
		"1 integration degraded",
		"1 agent message stuck pending delivery",
	}, attentionMessages(r.Attention))
	for _, it := range r.Attention {
		if it.Kind == HealthAttentionIntegration {
			assert.Equal(t, HealthAttentionSubject{Type: HealthSubjectIntegration}, it.Subject)
		}
	}
}
