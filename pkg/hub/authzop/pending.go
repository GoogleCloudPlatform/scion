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

package authzop

// Area names the catalog area file that owns an operation or a pending
// entry point.
type Area string

const (
	AreaAgents           Area = "agents"
	AreaProjects         Area = "projects"
	AreaMessaging        Area = "messaging"
	AreaChat             Area = "chat"
	AreaIdentity         Area = "identity"
	AreaHub              Area = "hub"
	AreaCatalogResources Area = "catalog_resources"
	AreaBrokers          Area = "brokers"
	AreaMaterial         Area = "material"
)

// ValidAreas is the closed set of catalog areas.
var ValidAreas = map[Area]bool{
	AreaAgents:           true,
	AreaProjects:         true,
	AreaMessaging:        true,
	AreaChat:             true,
	AreaIdentity:         true,
	AreaHub:              true,
	AreaCatalogResources: true,
	AreaBrokers:          true,
	AreaMaterial:         true,
}

// PendingEntry identifies one live entry point that has no catalog
// operation yet, and so no recorded bearer disposition.
type PendingEntry struct {
	Kind    EntryPointKind
	Pattern string
	Method  string
	Variant string
	Area    Area
}

// PendingBearerDispositions lists every live entry point whose catalog
// operation, and with it the bearer disposition, is recorded by a later
// batch. An entry is removed when its operation is catalogued. An entry
// must name a live entry point and must not also be catalogued.
//
// HTTP patterns use "{name}" placeholders. Non-HTTP patterns use
// "<function>:<detail>", where <function> is declared in pkg/hub.
var PendingBearerDispositions = []PendingEntry{
	// --- agents ---
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/cloud-logs", Method: "GET", Area: AreaAgents},
	{Kind: EntryPointSSE, Pattern: "/api/v1/agents/{id}/cloud-logs/stream", Method: "GET", Area: AreaAgents},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/groups", Method: "GET", Area: AreaAgents},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/keys", Method: "POST", Area: AreaAgents},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/logs", Method: "GET", Area: AreaAgents},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/message", Method: "POST", Area: AreaAgents},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/message-logs", Method: "GET", Area: AreaAgents},
	{Kind: EntryPointSSE, Pattern: "/api/v1/agents/{id}/message-logs/stream", Method: "GET", Area: AreaAgents},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/messages", Method: "GET", Area: AreaAgents},
	{Kind: EntryPointSSE, Pattern: "/api/v1/agents/{id}/messages/stream", Method: "GET", Area: AreaAgents},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/metrics/summary", Method: "GET", Area: AreaAgents},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/ports", Method: "DELETE", Area: AreaAgents},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/ports", Method: "POST", Area: AreaAgents},
	{Kind: EntryPointWebSocket, Pattern: "/api/v1/agents/{id}/ports/tunnel", Method: "GET", Area: AreaAgents},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/ports/{port}", Method: "DELETE", Area: AreaAgents},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/status", Method: "POST", Area: AreaAgents},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/workspace", Method: "GET", Area: AreaAgents},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/workspace/sync-from", Method: "POST", Area: AreaAgents},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/workspace/sync-to", Method: "POST", Area: AreaAgents},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/workspace/sync-to/finalize", Method: "POST", Area: AreaAgents},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{id}/agents", Method: "GET", Area: AreaAgents},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{id}/agents", Method: "POST", Area: AreaAgents},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{id}/agents/stop-all", Method: "POST", Area: AreaAgents},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{id}/agents/{agentId}", Method: "DELETE", Area: AreaAgents},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{id}/agents/{agentId}", Method: "GET", Area: AreaAgents},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{id}/agents/{agentId}", Method: "PATCH", Area: AreaAgents},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{id}/agents/{agentId}/cloud-logs", Method: "GET", Area: AreaAgents},
	{Kind: EntryPointSSE, Pattern: "/api/v1/projects/{id}/agents/{agentId}/cloud-logs/stream", Method: "GET", Area: AreaAgents},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{id}/agents/{agentId}/keys", Method: "POST", Area: AreaAgents},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{id}/agents/{agentId}/logs", Method: "GET", Area: AreaAgents},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{id}/agents/{agentId}/message", Method: "POST", Area: AreaAgents},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{id}/agents/{agentId}/message-logs", Method: "GET", Area: AreaAgents},
	{Kind: EntryPointSSE, Pattern: "/api/v1/projects/{id}/agents/{agentId}/message-logs/stream", Method: "GET", Area: AreaAgents},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{id}/agents/{agentId}/status", Method: "POST", Area: AreaAgents},
	// --- projects ---
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/artifacts", Method: "GET", Area: AreaProjects},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/artifacts/shared/{token}", Method: "GET", Area: AreaProjects},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{id}/clone", Method: "POST", Area: AreaProjects},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{id}/gcp-service-accounts", Method: "GET", Area: AreaProjects},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{id}/gcp-service-accounts/mint", Method: "POST", Area: AreaProjects},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{id}/gcp-service-accounts/{saId}", Method: "DELETE", Area: AreaProjects},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{id}/gcp-service-accounts/{saId}", Method: "GET", Area: AreaProjects},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{id}/gcp-service-accounts/{saId}/verify", Method: "POST", Area: AreaProjects},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{id}/members/principals/{type}/{principalId}", Method: "DELETE", Area: AreaProjects},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{id}/members/principals/{type}/{principalId}", Method: "PUT", Area: AreaProjects},
	{Kind: EntryPointSSE, Pattern: "/api/v1/projects/{id}/message-logs/stream", Method: "GET", Area: AreaProjects},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{id}/providers", Method: "POST", Area: AreaProjects},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{id}/providers/{brokerId}", Method: "DELETE", Area: AreaProjects},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{id}/schedules/{scheduleId}/history", Method: "GET", Area: AreaProjects},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{id}/schedules/{scheduleId}/pause", Method: "POST", Area: AreaProjects},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{id}/schedules/{scheduleId}/resume", Method: "POST", Area: AreaProjects},
	{Kind: EntryPointSchedulerJob, Pattern: "executeSchedule:dispatch_agent", Area: AreaProjects},
	{Kind: EntryPointSchedulerJob, Pattern: "executeSchedule:message", Area: AreaProjects},
	{Kind: EntryPointSchedulerJob, Pattern: "fireEvent:dispatch_agent", Area: AreaProjects},
	{Kind: EntryPointSchedulerJob, Pattern: "fireEvent:message", Area: AreaProjects},
	// --- messaging ---
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/conversations", Method: "GET", Area: AreaMessaging},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/conversations", Method: "POST", Area: AreaMessaging},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/conversations/", Method: "POST", Area: AreaMessaging},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/conversations/resolve", Method: "GET", Area: AreaMessaging},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/conversations/{id}", Method: "GET", Area: AreaMessaging},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/conversations/{id}/default-agent", Method: "PUT", Area: AreaMessaging},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/conversations/{id}/leave", Method: "POST", Area: AreaMessaging},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/conversations/{id}/messages", Method: "GET", Area: AreaMessaging},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/conversations/{id}/messages/{messageId}", Method: "GET", Area: AreaMessaging},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/conversations/{id}/participants", Method: "POST", Area: AreaMessaging},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/gcs/object", Method: "GET", Area: AreaMessaging},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/message-channels", Method: "GET", Area: AreaMessaging},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/messages", Method: "GET", Area: AreaMessaging},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/messages/read-all", Method: "POST", Area: AreaMessaging},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/messages/{id}", Method: "GET", Area: AreaMessaging},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/messages/{id}/read", Method: "POST", Area: AreaMessaging},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/messaging/capabilities", Method: "GET", Area: AreaMessaging},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/messaging/targets/resolve", Method: "GET", Area: AreaMessaging},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/notifications", Method: "GET", Area: AreaMessaging},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/notifications/ack-all", Method: "POST", Area: AreaMessaging},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/notifications/subscriptions", Method: "GET", Area: AreaMessaging},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/notifications/subscriptions", Method: "POST", Area: AreaMessaging},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/notifications/subscriptions/bulk", Method: "POST", Area: AreaMessaging},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/notifications/subscriptions/bulk-delete", Method: "POST", Area: AreaMessaging},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/notifications/subscriptions/{id}", Method: "DELETE", Area: AreaMessaging},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/notifications/subscriptions/{id}", Method: "PATCH", Area: AreaMessaging},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/notifications/templates", Method: "GET", Area: AreaMessaging},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/notifications/templates", Method: "POST", Area: AreaMessaging},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/notifications/templates/{id}", Method: "DELETE", Area: AreaMessaging},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/notifications/{id}/ack", Method: "POST", Area: AreaMessaging},
	// --- chat ---
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/chat/conversations/{id}/interagent", Method: "GET", Area: AreaChat},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/chat/conversations/{id}/promote", Method: "POST", Area: AreaChat},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/chat/conversations/{key}/messages/{messageId}", Method: "DELETE", Area: AreaChat},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/chat/conversations/{key}/messages/{messageId}", Method: "PUT", Area: AreaChat},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/chat/conversations/{topicKey}/messages", Method: "POST", Area: AreaChat},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/chat/conversations/{topicKey}/mute", Method: "PUT", Area: AreaChat},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/chat/conversations/{topicKey}/pin", Method: "PUT", Area: AreaChat},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/chat/conversations/{topicKey}/read", Method: "GET", Area: AreaChat},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/chat/conversations/{topicKey}/read", Method: "POST", Area: AreaChat},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/chat/conversations/{topicKey}/typing", Method: "POST", Area: AreaChat},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/chat/conversations/{topicKey}/unread", Method: "POST", Area: AreaChat},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/chat/presence", Method: "POST", Area: AreaChat},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/chat/spaces/{projectId}/emoji", Method: "PUT", Area: AreaChat},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/chat/spaces/{projectId}/members", Method: "GET", Area: AreaChat},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/chat/spaces/{projectId}/read", Method: "POST", Area: AreaChat},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/chat/spaces/{projectId}/threads", Method: "POST", Area: AreaChat},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/chat/topics/{topicId}", Method: "DELETE", Area: AreaChat},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/chat/topics/{topicId}", Method: "PATCH", Area: AreaChat},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/chat/user-prefs", Method: "GET", Area: AreaChat},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/chat/user-prefs", Method: "PUT", Area: AreaChat},
	// --- identity ---
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/access-constraint-previews", Method: "POST", Area: AreaIdentity},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/access-constraint-previews/{jobId}", Method: "GET", Area: AreaIdentity},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/access-constraint-previews/{jobId}/result", Method: "GET", Area: AreaIdentity},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/access-constraints/{id}/affected-principals", Method: "GET", Area: AreaIdentity},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/access-constraints/{id}/audit", Method: "GET", Area: AreaIdentity},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/allow-list/domains", Method: "GET", Area: AreaIdentity},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/allow-list/import", Method: "POST", Area: AreaIdentity},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/effective-access", Method: "GET", Area: AreaIdentity},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/effective-access", Method: "HEAD", Area: AreaIdentity},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/invites", Method: "POST", Area: AreaIdentity},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/invites/stats", Method: "GET", Area: AreaIdentity},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/invites/{id}/revoke", Method: "POST", Area: AreaIdentity},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/roles/export", Method: "HEAD", Area: AreaIdentity},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/roles/{id}/export", Method: "HEAD", Area: AreaIdentity},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/auth/admin-status", Method: "GET", Area: AreaIdentity},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/auth/invite/redeem", Method: "POST", Area: AreaIdentity},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/auth/me", Method: "GET", Area: AreaIdentity},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/auth/scopes", Method: "GET", Area: AreaIdentity},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/authz/explain", Method: "POST", Area: AreaIdentity},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/discord/link/verify", Method: "POST", Area: AreaIdentity},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/groups/{id}/members", Method: "GET", Area: AreaIdentity},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/groups/{id}/members/{type}/{memberId}", Method: "GET", Area: AreaIdentity},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/principals/agent/{id}", Method: "GET", Area: AreaIdentity},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/principals/group/{id}", Method: "GET", Area: AreaIdentity},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/principals/user/{id}", Method: "GET", Area: AreaIdentity},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/teams/link/verify", Method: "POST", Area: AreaIdentity},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/telegram/link/verify", Method: "POST", Area: AreaIdentity},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/users/me/groups", Method: "GET", Area: AreaIdentity},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/users/me/injected-skills", Method: "GET", Area: AreaIdentity},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/users/me/injected-skills", Method: "POST", Area: AreaIdentity},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/users/me/injected-skills", Method: "PUT", Area: AreaIdentity},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/users/me/injected-skills/{id}", Method: "DELETE", Area: AreaIdentity},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/users/me/templates", Method: "GET", Area: AreaIdentity},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/users/me/templates", Method: "POST", Area: AreaIdentity},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/users/me/templates/{id}", Method: "DELETE", Area: AreaIdentity},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/users/me/templates/{id}", Method: "GET", Area: AreaIdentity},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/users/me/templates/{id}", Method: "PUT", Area: AreaIdentity},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/users/me/templates/{id}/download", Method: "GET", Area: AreaIdentity},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/users/me/templates/{id}/finalize", Method: "POST", Area: AreaIdentity},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/users/me/templates/{id}/upload", Method: "POST", Area: AreaIdentity},
	// --- hub ---
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/integrations/available", Method: "GET", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/integrations/{name}/config", Method: "PUT", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/integrations/{name}/health", Method: "GET", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/integrations/{name}/install", Method: "POST", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/integrations/{name}/restart", Method: "POST", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/integrations/{name}/update", Method: "POST", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/integrations/{name}/update/{id}", Method: "GET", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/lifecycle-hooks", Method: "POST", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/lifecycle-hooks/{id}", Method: "DELETE", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/lifecycle-hooks/{id}", Method: "PUT", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/limits/{id}/entitlements", Method: "GET", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/maintenance", Method: "GET", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/maintenance/operations/{key}/runs", Method: "GET", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/maintenance/operations/{key}/runs/{runId}", Method: "GET", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/project-defaults", Method: "PATCH", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/project-defaults", Method: "POST", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/project-defaults", Method: "PUT", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/server-config", Method: "PATCH", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/server-config", Method: "POST", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/conduit/grant-keys", Method: "GET", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/experiments", Method: "GET", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/hub/settings/injected-skills", Method: "GET", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/hub/settings/injected-skills", Method: "PUT", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/metrics/session/{id}", Method: "GET", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/pre-start-hooks", Method: "GET", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/pre-start-hooks", Method: "POST", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/pre-start-hooks/{id}", Method: "DELETE", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/pre-start-hooks/{id}", Method: "GET", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/pre-start-hooks/{id}", Method: "PUT", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/pre-start-hooks/{id}/activate", Method: "POST", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/system/apple-dns", Method: "GET", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/system/apple-dns", Method: "POST", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/system/check", Method: "GET", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/system/fs/list", Method: "GET", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/system/fs/mkdir", Method: "POST", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/system/fs/validate-path", Method: "POST", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/system/identity", Method: "PUT", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/system/images/build", Method: "POST", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/system/images/pull", Method: "POST", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/system/init", Method: "POST", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/system/registry", Method: "PUT", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/system/runtime", Method: "GET", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/system/runtime", Method: "PUT", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/system/status", Method: "GET", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/system/workstation-settings", Method: "PATCH", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/usage/me", Method: "GET", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/metrics", Method: "GET", Area: AreaHub},
	// --- catalog_resources ---
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/harness-configs/{id}", Method: "PATCH", Area: AreaCatalogResources},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/harness-configs/{id}/check-image", Method: "POST", Area: AreaCatalogResources},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/harness-configs/{id}/clone", Method: "POST", Area: AreaCatalogResources},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/harness-configs/{id}/download", Method: "GET", Area: AreaCatalogResources},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/harness-configs/{id}/files", Method: "GET", Area: AreaCatalogResources},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/harness-configs/{id}/files", Method: "POST", Area: AreaCatalogResources},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/harness-configs/{id}/files/{path}", Method: "DELETE", Area: AreaCatalogResources},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/harness-configs/{id}/files/{path}", Method: "GET", Area: AreaCatalogResources},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/harness-configs/{id}/files/{path}", Method: "PUT", Area: AreaCatalogResources},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/harness-configs/{id}/finalize", Method: "POST", Area: AreaCatalogResources},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/harness-configs/{id}/image-status", Method: "GET", Area: AreaCatalogResources},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/harness-configs/{id}/local-image", Method: "DELETE", Area: AreaCatalogResources},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/harness-configs/{id}/pull-image", Method: "POST", Area: AreaCatalogResources},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/harness-configs/{id}/reimport", Method: "POST", Area: AreaCatalogResources},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/harness-configs/{id}/upload", Method: "POST", Area: AreaCatalogResources},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/harness-configs/{id}/validate", Method: "GET", Area: AreaCatalogResources},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/skill-registries/{id}", Method: "PATCH", Area: AreaCatalogResources},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/skill-registries/{id}/pin", Method: "POST", Area: AreaCatalogResources},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/skill-registries/{id}/pins", Method: "GET", Area: AreaCatalogResources},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/skill-registries/{id}/unpin", Method: "POST", Area: AreaCatalogResources},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/skills/resolve", Method: "POST", Area: AreaCatalogResources},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/skills/{id}/download", Method: "GET", Area: AreaCatalogResources},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/skills/{id}/files/{path}", Method: "GET", Area: AreaCatalogResources},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/skills/{id}/files/{path}", Method: "PUT", Area: AreaCatalogResources},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/skills/{id}/finalize", Method: "POST", Area: AreaCatalogResources},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/skills/{id}/resolve", Method: "GET", Area: AreaCatalogResources},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/skills/{id}/upload", Method: "POST", Area: AreaCatalogResources},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/skills/{id}/versions", Method: "GET", Area: AreaCatalogResources},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/skills/{id}/versions", Method: "POST", Area: AreaCatalogResources},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/skills/{id}/versions/{vid}", Method: "GET", Area: AreaCatalogResources},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/skills/{id}/versions/{vid}/deprecate", Method: "POST", Area: AreaCatalogResources},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/templates/{id}", Method: "PATCH", Area: AreaCatalogResources},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/templates/{id}/clone", Method: "POST", Area: AreaCatalogResources},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/templates/{id}/download", Method: "GET", Area: AreaCatalogResources},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/templates/{id}/files", Method: "GET", Area: AreaCatalogResources},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/templates/{id}/files", Method: "POST", Area: AreaCatalogResources},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/templates/{id}/files/{path}", Method: "DELETE", Area: AreaCatalogResources},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/templates/{id}/files/{path}", Method: "GET", Area: AreaCatalogResources},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/templates/{id}/files/{path}", Method: "PUT", Area: AreaCatalogResources},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/templates/{id}/finalize", Method: "POST", Area: AreaCatalogResources},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/templates/{id}/upload", Method: "POST", Area: AreaCatalogResources},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/templates/{id}/validate", Method: "GET", Area: AreaCatalogResources},
	// --- brokers ---
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/brokers/{id}/rotate-secret", Method: "POST", Area: AreaBrokers},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/runtime-brokers/{id}", Method: "DELETE", Area: AreaBrokers},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/runtime-brokers/{id}", Method: "PATCH", Area: AreaBrokers},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/runtime-brokers/{id}/env", Method: "GET", Area: AreaBrokers},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/runtime-brokers/{id}/env/{key}", Method: "DELETE", Area: AreaBrokers},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/runtime-brokers/{id}/env/{key}", Method: "GET", Area: AreaBrokers},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/runtime-brokers/{id}/env/{key}", Method: "PUT", Area: AreaBrokers},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/runtime-brokers/{id}/heartbeat", Method: "POST", Area: AreaBrokers},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/runtime-brokers/{id}/projects", Method: "GET", Area: AreaBrokers},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/runtime-brokers/{id}/secrets", Method: "GET", Area: AreaBrokers},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/runtime-brokers/{id}/secrets/{key}", Method: "DELETE", Area: AreaBrokers},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/runtime-brokers/{id}/secrets/{key}", Method: "GET", Area: AreaBrokers},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/runtime-brokers/{id}/secrets/{key}", Method: "PATCH", Area: AreaBrokers},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/runtime-brokers/{id}/secrets/{key}", Method: "PUT", Area: AreaBrokers},
	// --- material ---
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/env/{key}", Method: "DELETE", Area: AreaMaterial},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/env/{key}", Method: "PUT", Area: AreaMaterial},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/gcp-service-accounts/mint", Method: "POST", Area: AreaMaterial},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/secrets/{key}", Method: "PATCH", Area: AreaMaterial},
}

// PendingBearerOperations lists catalogued operations whose bearer
// disposition is recorded by a later batch. A listed operation carries no
// Bearer value, and Validate does not apply the credentials agreement rule
// to it until its disposition is recorded.
var PendingBearerOperations = []OperationID{
	"access.constraint.create",
	"access.constraint.delete",
	"access.constraint.read",
	"access.constraint.update",
	"agent.list",
	"agent.message.send",
	"agent.portaccess",
	"agent.setmessagemode",
	"agent.stopall",
	"agent.update",
	"broker.read",
	"chat.access",
	"env.read",
	"gcp.identity.assign",
	"gcp.identity.create",
	"gcp.identity.delete",
	"gcp.identity.mint",
	"gcp.identity.read",
	"gcp.identity.verify",
	"group.create",
	"group.delete",
	"group.member.add",
	"group.member.remove",
	"group.read",
	"group.update",
	"harnessconfig.create",
	"harnessconfig.delete",
	"hub.adminmode.update",
	"hub.allowlist.update",
	"hub.authreset",
	"hub.config.read",
	"hub.config.update",
	"hub.diagnostics.read",
	"hub.experiments.update",
	"hub.githubapp.read",
	"hub.githubapp.update",
	"hub.health.read",
	"hub.integrations.read",
	"hub.lifecyclehooks.read",
	"hub.maintenance.execute",
	"hub.messaging.update",
	"hub.metrics.read",
	"hub.projectdefaults.read",
	"hub.scheduler.read",
	"hub.teamsmanifest.read",
	"hub.validate.execute",
	"project.lifecycle.create",
	"project.list",
	"project.register",
	"quota.create",
	"quota.delete",
	"quota.read",
	"quota.update",
	"role.binding.create",
	"role.binding.delete",
	"role.binding.read",
	"role.definition.create",
	"role.definition.delete",
	"role.definition.update",
	"role.read",
	"schedule.event.create",
	"schedule.event.delete",
	"schedule.event.read",
	"schedule.event.update",
	"secret.read",
	"secret.write",
	"skill.create",
	"skill.read",
	"skill.register",
	"template.create",
	"template.delete",
	"template.read",
	"user.admin.invite",
	"user.admin.suspend",
	"user.read",
	"user.update",
}

var pendingBearerOperationSet = func() map[OperationID]bool {
	m := make(map[OperationID]bool, len(PendingBearerOperations))
	for _, id := range PendingBearerOperations {
		m[id] = true
	}
	return m
}()

// IsPendingBearerOperation reports whether id is listed in
// PendingBearerOperations.
func IsPendingBearerOperation(id OperationID) bool {
	return pendingBearerOperationSet[id]
}
