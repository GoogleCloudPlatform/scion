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

package hubclient

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// healthSummaryPath is the admin health summary endpoint behind the
// health dashboard.
const healthSummaryPath = "/api/v1/admin/health/summary"

// HealthSummary is the response of GET /api/v1/admin/health/summary: the
// hub's composite health, with the overall status and the ranked "Needs
// attention" list computed by the hub. It mirrors the hub's wire JSON;
// the hub owns the meaning of every field, and clients render it without
// re-deriving status or attention.
//
// The route needs the hub.health.read permission and a user identity (an
// interactive session or a hub-boundary access token holding
// hub_health:read). Other callers get 403. A hub instance that cannot read
// its database answers 503 with a fixed message.
type HealthSummary struct {
	// Status is healthy, degraded or unhealthy.
	Status      string    `json:"status"`
	GeneratedAt time.Time `json:"generated_at"`
	// Attention is the ranked "Needs attention" list.
	Attention []HealthAttentionItem `json:"attention"`
	Hub       HealthSummaryHub      `json:"hub"`
	Brokers   HealthSummaryBrokers  `json:"runtime_brokers"`
	// Agents is nil when the hub could not read the agent aggregate.
	Agents *HealthSummaryAgents `json:"agents"`
	// Dispatch is nil when the hub could not count dispatch health.
	Dispatch *HealthSummaryDispatch `json:"dispatch"`
	// Integrations is empty unless IntegrationsDetail is true.
	Integrations       []HealthSummaryIntegration     `json:"integrations"`
	IntegrationsDetail bool                           `json:"integrations_detail"`
	IntegrationCounts  HealthSummaryIntegrationCounts `json:"integration_counts"`
	// ServiceAccountCheck is set only while the service account assignment
	// check cannot run on a live hub instance.
	ServiceAccountCheck *HealthSummarySACheck `json:"service_account_check,omitempty"`
	// HubInstances is nil when the hub-instance registry could not be read.
	HubInstances *HealthSummaryHubInstances `json:"hub_instances"`
	Links        *HealthSummaryLinks        `json:"links,omitempty"`

	// Raw is the response body exactly as the hub sent it, so callers can
	// pass the summary through without dropping fields this client does
	// not model.
	Raw json.RawMessage `json:"-"`
}

// HealthAttentionItem is one entry of the "Needs attention" list.
type HealthAttentionItem struct {
	// Severity is critical or warning.
	Severity string                 `json:"severity"`
	Kind     string                 `json:"kind"`
	Subject  HealthAttentionSubject `json:"subject"`
	// Message is a fixed sentence composed by the hub.
	Message string `json:"message"`
}

// HealthAttentionSubject is what an attention item is about.
type HealthAttentionSubject struct {
	Type      string `json:"type"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	ProjectID string `json:"project_id,omitempty"`
}

// HealthSummaryHub is the hub fleet section of the health summary.
type HealthSummaryHub struct {
	Status           string `json:"status"`
	InstanceID       string `json:"instance_id"`
	Version          string `json:"version"`
	ConnectedBrokers int    `json:"connected_brokers"`
	ActiveAgents     int    `json:"active_agents"`
	Projects         int    `json:"projects"`
	// Instances is nil when the registry could not be read.
	Instances                       *HealthSummaryHubFleet  `json:"instances"`
	UnhealthyChecks                 []HealthSummaryHubCheck `json:"unhealthy_checks"`
	TestIdentitiesEnabled           bool                    `json:"test_identities_enabled"`
	TestAdminIdentitiesEnabled      bool                    `json:"test_admin_identities_enabled"`
	TestSuperAdminIdentitiesEnabled bool                    `json:"test_super_admin_identities_enabled"`
	// LiveTestAdminIdentities is nil when the count could not be read.
	LiveTestAdminIdentities        *int `json:"live_test_admin_identities"`
	TestIdentityWebSessionsEnabled bool `json:"test_identity_web_sessions_enabled"`
}

// HealthSummaryHubFleet counts the live hub instances by status.
type HealthSummaryHubFleet struct {
	Live      int `json:"live"`
	Healthy   int `json:"healthy"`
	Degraded  int `json:"degraded"`
	Unhealthy int `json:"unhealthy"`
}

// HealthSummaryHubCheck is one non-healthy check of one live hub instance.
type HealthSummaryHubCheck struct {
	InstanceID    string `json:"instance_id"`
	InstanceLabel string `json:"instance_label"`
	Name          string `json:"name"`
	Value         string `json:"value"`
}

// HealthSummaryBrokers is the runtime broker section of the health summary.
type HealthSummaryBrokers struct {
	Items       []HealthSummaryBroker `json:"items"`
	Total       int                   `json:"total"`
	Truncated   bool                  `json:"truncated"`
	NotReported bool                  `json:"not_reported,omitempty"`
}

// HealthSummaryBroker is one runtime broker in the health summary.
type HealthSummaryBroker struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version string `json:"version"`
	// Status is broker liveness: online, offline or degraded.
	Status           string               `json:"status"`
	LastHeartbeat    *time.Time           `json:"last_heartbeat"`
	Runtime          *HealthBrokerRuntime `json:"runtime"`
	WorkspaceStorage *HealthBrokerStorage `json:"workspace_storage"`
	// Health is the broker's last self-reported health; nil when the
	// broker never reported it.
	Health *HealthBrokerSelf  `json:"health"`
	Agents HealthBrokerAgents `json:"agents"`
}

// HealthBrokerSelf is a runtime broker's self-reported health.
type HealthBrokerSelf struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

// HealthBrokerAgents holds per-broker agent counts.
type HealthBrokerAgents struct {
	Running   int `json:"running"`
	Attention int `json:"attention"`
}

// HealthBrokerRuntime is the runtime a broker places agents on by default.
type HealthBrokerRuntime struct {
	Type    string `json:"type"`
	Profile string `json:"profile"`
}

// HealthBrokerStorage is a broker's reported workspace storage.
type HealthBrokerStorage struct {
	Backend    string `json:"backend"`
	NFSHealthy *bool  `json:"nfs_healthy,omitempty"`
}

// HealthSummaryAgents is the agents section of the health summary.
type HealthSummaryAgents struct {
	Total      int                `json:"total"`
	Active     int                `json:"active"`
	Errored    int                `json:"errored"`
	Considered int                `json:"considered"`
	ByPhase    []HealthPhaseCount `json:"by_phase"`
	Problems   []HealthAgentGroup `json:"problems"`
}

// HealthPhaseCount is the number of agents in one phase.
type HealthPhaseCount struct {
	Phase string `json:"phase"`
	Count int    `json:"count"`
}

// HealthAgentGroup is one kind of agent problem (errored, crashed or
// offline). Count is the true total; Items is capped by the hub.
type HealthAgentGroup struct {
	Kind  string           `json:"kind"`
	Count int              `json:"count"`
	Items []HealthAgentRef `json:"items"`
}

// HealthAgentRef identifies one agent in a problem group.
type HealthAgentRef struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	ProjectID   string `json:"project_id"`
	ProjectSlug string `json:"project_slug"`
	BrokerID    string `json:"broker_id"`
}

// HealthSummaryDispatch holds dispatch pipeline counters.
type HealthSummaryDispatch struct {
	StuckMessages          int `json:"stuck_messages"`
	StuckBrokerDispatch    int `json:"stuck_broker_dispatch"`
	FailedBrokerDispatch1h int `json:"failed_broker_dispatch_1h"`
}

// HealthSummaryIntegration is one chat or messaging integration.
type HealthSummaryIntegration struct {
	Name       string     `json:"name"`
	Platform   string     `json:"platform"`
	Health     string     `json:"health"`
	Connected  bool       `json:"connected"`
	Version    string     `json:"version"`
	Reason     string     `json:"reason,omitempty"`
	ManagedBy  []string   `json:"managed_by,omitempty"`
	ReportedAt *time.Time `json:"reported_at,omitempty"`
}

// HealthSummaryIntegrationCounts aggregates the integrations by health.
type HealthSummaryIntegrationCounts struct {
	Total     int `json:"total"`
	Healthy   int `json:"healthy"`
	Degraded  int `json:"degraded"`
	Unhealthy int `json:"unhealthy"`
	Unknown   int `json:"unknown"`
}

// HealthSummarySACheck reports that the service account assignment check
// cannot run on at least one live hub instance.
type HealthSummarySACheck struct {
	Status    string   `json:"status"`
	Cause     string   `json:"cause"`
	Remedy    string   `json:"remedy"`
	DocsURL   string   `json:"docs_url"`
	Instances []string `json:"instances"`
}

// HealthSummaryHubInstances lists the hub instances seen in the last hour.
type HealthSummaryHubInstances struct {
	AsOf      time.Time           `json:"as_of"`
	Items     []HealthHubInstance `json:"items"`
	Live      int                 `json:"live"`
	Total     int                 `json:"total"`
	Truncated bool                `json:"truncated"`
}

// HealthHubInstance is one hub instance (process).
type HealthHubInstance struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Version string `json:"version"`
	// State is live, stale or stopped.
	State                 string                         `json:"state"`
	Serving               bool                           `json:"serving"`
	StartedAt             time.Time                      `json:"started_at"`
	LastSeen              time.Time                      `json:"last_seen"`
	StoppedAt             *time.Time                     `json:"stopped_at"`
	Status                string                         `json:"status"`
	Checks                map[string]string              `json:"checks"`
	Database              *HealthHubInstanceDB           `json:"database"`
	IntegrationCounts     HealthSummaryIntegrationCounts `json:"integration_counts"`
	IntegrationsTruncated bool                           `json:"integrations_truncated,omitempty"`
	Integrations          []HealthHubInstanceIntegration `json:"integrations,omitempty"`
}

// HealthHubInstanceIntegration is one integration as one hub instance
// reported it.
type HealthHubInstanceIntegration struct {
	Name      string `json:"name"`
	Health    string `json:"health"`
	Connected bool   `json:"connected"`
	Version   string `json:"version"`
}

// HealthHubInstanceDB is one hub instance's database connection pool.
type HealthHubInstanceDB struct {
	PoolActive         int   `json:"pool_active"`
	PoolIdle           int   `json:"pool_idle"`
	PoolMax            int   `json:"pool_max"`
	PoolWaitCountTotal int64 `json:"pool_wait_count_total"`
}

// HealthSummaryLinks holds operator-configured Health page links.
type HealthSummaryLinks struct {
	MonitoringDashboard string `json:"monitoring_dashboard,omitempty"`
}

// HealthSummary fetches GET /api/v1/admin/health/summary. A 403 (caller
// lacks hub.health.read or is not a user identity) and a 503 (the serving
// hub instance cannot read its database) are returned as *apiclient.APIError,
// so callers can tell them apart by status code.
func (c *client) HealthSummary(ctx context.Context) (*HealthSummary, error) {
	resp, err := c.get(ctx, healthSummaryPath, nil)
	if err != nil {
		return nil, err
	}
	raw, err := apiclient.DecodeRequired[json.RawMessage](resp)
	if err != nil {
		return nil, err
	}
	var summary HealthSummary
	if err := json.Unmarshal(*raw, &summary); err != nil {
		return nil, fmt.Errorf("failed to decode health summary: %w", err)
	}
	summary.Raw = *raw
	return &summary, nil
}
