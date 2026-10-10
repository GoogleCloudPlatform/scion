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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestFleetHubStatus(t *testing.T) {
	cases := []struct {
		name  string
		fleet HealthSummaryHubFleet
		want  string
	}{
		{"no live instance", HealthSummaryHubFleet{}, HealthStatusUnhealthy},
		{"1 of 1 healthy", HealthSummaryHubFleet{Live: 1, Healthy: 1}, HealthStatusHealthy},
		{"1 of 1 unhealthy", HealthSummaryHubFleet{Live: 1, Unhealthy: 1}, HealthStatusUnhealthy},
		{"1 of 1 degraded", HealthSummaryHubFleet{Live: 1, Degraded: 1}, HealthStatusDegraded},
		{"1 of 2 unhealthy", HealthSummaryHubFleet{Live: 2, Healthy: 1, Unhealthy: 1}, HealthStatusDegraded},
		{"2 of 2 unhealthy", HealthSummaryHubFleet{Live: 2, Unhealthy: 2}, HealthStatusUnhealthy},
		{"1 of 3 unhealthy", HealthSummaryHubFleet{Live: 3, Healthy: 2, Unhealthy: 1}, HealthStatusDegraded},
		{"2 of 3 unhealthy", HealthSummaryHubFleet{Live: 3, Healthy: 1, Unhealthy: 2}, HealthStatusUnhealthy},
		{"2 of 4 unhealthy", HealthSummaryHubFleet{Live: 4, Healthy: 2, Unhealthy: 2}, HealthStatusDegraded},
		{"3 of 3 degraded", HealthSummaryHubFleet{Live: 3, Degraded: 3}, HealthStatusDegraded},
		{"3 of 3 healthy", HealthSummaryHubFleet{Live: 3, Healthy: 3}, HealthStatusHealthy},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, fleetHubStatus(tc.fleet))
		})
	}
}

// The fleet rule and its attention items, through the policy, from
// registry rows (the acceptance table of the fleet hub status).
func TestDeriveHealthSummaryStatus_FleetRule(t *testing.T) {
	healthy := func(id string) store.HubInstance {
		return policyRow(id, HealthStatusHealthy, time.Second, map[string]string{"database": "healthy"})
	}
	unhealthy := func(id string) store.HubInstance {
		return policyRow(id, HealthStatusUnhealthy, time.Second, map[string]string{"database": "unhealthy"})
	}
	hubSubject := HealthAttentionSubject{Type: HealthSubjectHub}
	instance := func(id string) HealthAttentionSubject {
		return HealthAttentionSubject{Type: HealthSubjectHub, ID: id, Name: "hub-" + id}
	}
	cases := []struct {
		name       string
		rows       []store.HubInstance
		readFailed bool
		wantStatus string
		wantItems  []HealthAttentionItem
	}{
		{
			name:       "1 of 3 unhealthy is degraded with warning items only",
			rows:       []store.HubInstance{healthy("1"), healthy("2"), unhealthy("3")},
			wantStatus: HealthStatusDegraded,
			wantItems: []HealthAttentionItem{
				{Severity: HealthAttentionWarning, Kind: HealthAttentionHubInstance, Subject: hubSubject, Message: "2 of 3 hub instances healthy"},
				{Severity: HealthAttentionWarning, Kind: HealthAttentionHubCheck, Subject: instance("3"), Message: "Hub check database is not healthy on instance hub-3"},
			},
		},
		{
			name:       "2 of 3 unhealthy is unhealthy with critical items",
			rows:       []store.HubInstance{healthy("1"), unhealthy("2"), unhealthy("3")},
			wantStatus: HealthStatusUnhealthy,
			wantItems: []HealthAttentionItem{
				{Severity: HealthAttentionCritical, Kind: HealthAttentionHubInstance, Subject: hubSubject, Message: "1 of 3 hub instances healthy"},
				{Severity: HealthAttentionCritical, Kind: HealthAttentionHubCheck, Subject: instance("2"), Message: "Hub check database is not healthy on instance hub-2"},
				{Severity: HealthAttentionCritical, Kind: HealthAttentionHubCheck, Subject: instance("3"), Message: "Hub check database is not healthy on instance hub-3"},
			},
		},
		{
			name:       "1 of 1 unhealthy is unhealthy",
			rows:       []store.HubInstance{unhealthy("1")},
			wantStatus: HealthStatusUnhealthy,
			wantItems: []HealthAttentionItem{
				{Severity: HealthAttentionCritical, Kind: HealthAttentionHubInstance, Subject: hubSubject, Message: "0 of 1 hub instances healthy"},
				{Severity: HealthAttentionCritical, Kind: HealthAttentionHubCheck, Subject: instance("1"), Message: "Hub check database is not healthy on instance hub-1"},
			},
		},
		{
			name:       "1 of 2 unhealthy is degraded",
			rows:       []store.HubInstance{healthy("1"), unhealthy("2")},
			wantStatus: HealthStatusDegraded,
			wantItems: []HealthAttentionItem{
				{Severity: HealthAttentionWarning, Kind: HealthAttentionHubInstance, Subject: hubSubject, Message: "1 of 2 hub instances healthy"},
				{Severity: HealthAttentionWarning, Kind: HealthAttentionHubCheck, Subject: instance("2"), Message: "Hub check database is not healthy on instance hub-2"},
			},
		},
		{
			name:       "0 live with no rows is unhealthy",
			rows:       nil,
			wantStatus: HealthStatusUnhealthy,
			wantItems: []HealthAttentionItem{
				{Severity: HealthAttentionCritical, Kind: HealthAttentionHubInstance, Subject: hubSubject, Message: "No hub instance is reporting"},
			},
		},
		{
			name:       "0 live with only stopped rows is unhealthy",
			rows:       []store.HubInstance{stoppedPolicyRow("1", time.Minute)},
			wantStatus: HealthStatusUnhealthy,
			wantItems: []HealthAttentionItem{
				{Severity: HealthAttentionCritical, Kind: HealthAttentionHubInstance, Subject: hubSubject, Message: "No hub instance is reporting"},
			},
		},
		{
			name: "0 live with a stale row is unhealthy and the stale row is a warning",
			rows: []store.HubInstance{
				policyRow("1", HealthStatusHealthy, time.Minute, nil),
			},
			wantStatus: HealthStatusUnhealthy,
			wantItems: []HealthAttentionItem{
				{Severity: HealthAttentionCritical, Kind: HealthAttentionHubInstance, Subject: hubSubject, Message: "No hub instance is reporting"},
				{Severity: HealthAttentionWarning, Kind: HealthAttentionHubInstance, Subject: instance("1"), Message: "Hub instance hub-1 stopped reporting"},
			},
		},
		{
			name: "a stale instance is a warning for 15 min and changes no status",
			rows: []store.HubInstance{
				healthy("1"),
				// Last reported unhealthy, but stale: outside the ratio.
				policyRow("2", HealthStatusUnhealthy, 15*time.Minute, map[string]string{"database": "unhealthy"}),
			},
			wantStatus: HealthStatusHealthy,
			wantItems: []HealthAttentionItem{
				{Severity: HealthAttentionWarning, Kind: HealthAttentionHubInstance, Subject: instance("2"), Message: "Hub instance hub-2 stopped reporting"},
			},
		},
		{
			name: "a stale instance past 15 min adds nothing",
			rows: []store.HubInstance{
				healthy("1"),
				policyRow("2", HealthStatusUnhealthy, 15*time.Minute+time.Second, map[string]string{"database": "unhealthy"}),
			},
			wantStatus: HealthStatusHealthy,
			wantItems:  []HealthAttentionItem{},
		},
		{
			name:       "a stopped instance adds nothing",
			rows:       []store.HubInstance{healthy("1"), stoppedPolicyRow("2", time.Minute)},
			wantStatus: HealthStatusHealthy,
			wantItems:  []HealthAttentionItem{},
		},
		{
			name:       "a registry read failure is a warning and changes no status",
			readFailed: true,
			wantStatus: HealthStatusHealthy,
			wantItems: []HealthAttentionItem{
				{Severity: HealthAttentionWarning, Kind: HealthAttentionHubInstance, Subject: hubSubject, Message: "Hub instance data not available"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := healthyPolicyResp()
			if tc.readFailed {
				r.Hub = buildHealthSummaryFleetHub(nil, time.Time{}, false)
				r.HubInstances = nil
			} else {
				setPolicyFleet(r, tc.rows...)
			}
			status, items := deriveHealthSummaryStatus(r)
			assert.Equal(t, tc.wantStatus, status)
			assert.Equal(t, tc.wantItems, items)
		})
	}
}

func TestBuildHealthSummaryFleetHub(t *testing.T) {
	t.Run("registry read failed", func(t *testing.T) {
		hub := buildHealthSummaryFleetHub(nil, time.Time{}, false)
		assert.Equal(t, HubStatusUnknown, hub.Status)
		assert.Nil(t, hub.Instances)
		assert.Empty(t, hub.Version)
		assert.NotNil(t, hub.UnhealthyChecks, "never null")
		assert.Empty(t, hub.UnhealthyChecks)
	})

	t.Run("version agreed by the live instances", func(t *testing.T) {
		stale := policyRow("3", HealthStatusHealthy, time.Minute, nil)
		stale.Version = "v0"
		hub := buildHealthSummaryFleetHub([]store.HubInstance{
			policyRow("1", HealthStatusHealthy, time.Second, nil),
			policyRow("2", HealthStatusHealthy, time.Second, nil),
			stale,
		}, policyNow, true)
		assert.Equal(t, "v1", hub.Version, "a stale instance's version does not count")
		assert.Equal(t, HealthStatusHealthy, hub.Status)
		require.NotNil(t, hub.Instances)
		assert.Equal(t, HealthSummaryHubFleet{Live: 2, Healthy: 2}, *hub.Instances)
	})

	t.Run("mixed versions", func(t *testing.T) {
		other := policyRow("2", HealthStatusHealthy, time.Second, nil)
		other.Version = "v2"
		hub := buildHealthSummaryFleetHub([]store.HubInstance{
			policyRow("1", HealthStatusHealthy, time.Second, nil), other,
		}, policyNow, true)
		assert.Equal(t, HubVersionMixed, hub.Version)
	})

	t.Run("no live instance", func(t *testing.T) {
		hub := buildHealthSummaryFleetHub([]store.HubInstance{stoppedPolicyRow("1", time.Minute)}, policyNow, true)
		assert.Empty(t, hub.Version)
		assert.Equal(t, HealthStatusUnhealthy, hub.Status)
		require.NotNil(t, hub.Instances)
		assert.Equal(t, HealthSummaryHubFleet{}, *hub.Instances)
	})

	t.Run("checks tagged with their instance, live only, normalised and ordered", func(t *testing.T) {
		b := policyRow("b", HealthStatusUnhealthy, time.Second, map[string]string{
			"database": "unhealthy: dial failed", "audit_log_writer": "degraded: recent write failures", "workspace_storage": "healthy",
		})
		a := policyRow("a", HealthStatusDegraded, time.Second, map[string]string{"colocated_broker": "unhealthy: registration pending"})
		staleRow := policyRow("c", HealthStatusUnhealthy, time.Minute, map[string]string{"database": "unhealthy"})
		hub := buildHealthSummaryFleetHub([]store.HubInstance{b, staleRow, a}, policyNow, true)
		assert.Equal(t, []HealthSummaryHubCheck{
			{InstanceID: "b", InstanceLabel: "hub-b", Name: "database", Value: "unhealthy"},
			{InstanceID: "b", InstanceLabel: "hub-b", Name: "audit_log_writer", Value: "degraded"},
			{InstanceID: "a", InstanceLabel: "hub-a", Name: "colocated_broker", Value: "unhealthy"},
		}, hub.UnhealthyChecks)
		require.NotNil(t, hub.Instances)
		assert.Equal(t, HealthSummaryHubFleet{Live: 2, Degraded: 1, Unhealthy: 1}, *hub.Instances)
		assert.Equal(t, HealthStatusDegraded, hub.Status, "1 of 2 unhealthy")
	})

	t.Run("an instance without a label is named by its ID", func(t *testing.T) {
		r := policyRow("x", HealthStatusDegraded, time.Second, map[string]string{"audit_log_writer": "degraded"})
		r.Label = ""
		hub := buildHealthSummaryFleetHub([]store.HubInstance{r}, policyNow, true)
		require.Len(t, hub.UnhealthyChecks, 1)
		assert.Equal(t, "x", hub.UnhealthyChecks[0].InstanceLabel)
	})
}

// The service account check section comes from the live rows' check
// sa_assign_check: present when a live instance reports it, naming those
// instances; a stale or stopped instance's report does not count.
func TestHealthSummarySACheckFromRows(t *testing.T) {
	sa := map[string]string{"database": "healthy", saAssignCheckName: "degraded"}
	staleSA := policyRow("3", HealthStatusDegraded, time.Minute, sa)
	got := healthSummarySACheck(buildHealthSummaryHubInstances([]store.HubInstance{
		policyRow("2", HealthStatusDegraded, time.Second, sa),
		policyRow("1", HealthStatusHealthy, time.Second, map[string]string{"database": "healthy"}),
		policyRow("0", HealthStatusDegraded, time.Second, sa),
		staleSA,
	}, policyNow, "1"))
	require.NotNil(t, got)
	assert.Equal(t, HealthStatusDegraded, got.Status)
	assert.Equal(t, saAssignCheckDiagCause, got.Cause)
	assert.Equal(t, saAssignCheckDiagRemedy, got.Remedy)
	assert.Equal(t, saAssignCheckDiagDocsURL, got.DocsURL)
	assert.Equal(t, []string{"hub-0", "hub-2"}, got.Instances)

	assert.Nil(t, healthSummarySACheck(nil), "registry read failed")
	assert.Nil(t, healthSummarySACheck(buildHealthSummaryHubInstances([]store.HubInstance{
		policyRow("1", HealthStatusHealthy, time.Second, map[string]string{"database": "healthy"}),
		staleSA,
	}, policyNow, "1")), "only a stale instance reports it")
}
