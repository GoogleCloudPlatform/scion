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

import "net/http"

// prefixSubRouteRow enumerates one concrete sub-route pattern and its accepted
// HTTP methods under a trailing-slash routeMetadataTable prefix.
//
// Every row is verified individually by:
//   - TestEntryPointsCoverRouteMetadata (pkg/hub/authzop, via AST parsing of
//     prefixSubRouteTable)
//   - TestBearerDisposition_EveryRoutePatternCovered (pkg/hub)
//   - TestPrefixSubRoutes_HandlerDispatchCompleteness (pkg/hub/authzop,
//     AST-checks the registered prefix handlers and their delegated s.handle*
//     callees for literal action switches, equality checks, and prefix/suffix
//     string matches not represented in prefixSubRouteTable)
//   - TestPrefixSubRoutes_LiveUndeclaredMethodCheck (pkg/hub, live HTTP probe
//     requiring undeclared standard methods on each row to return 404 or 405)
type prefixSubRouteRow struct {
	prefix  string
	pattern string
	methods []string
}

// prefixSubRouteTable enumerates every concrete (method, pattern) operation
// dispatched under the trailing-slash routeMetadataTable prefixes that are not
// covered by a dedicated table (agentSubRouteTable, projectSubRouteTable) or
// external service parser (pkg/artifacts/service.go).
var prefixSubRouteTable = []prefixSubRouteRow{
	// /api/v1/admin/access-constraint-previews/
	{prefix: "/api/v1/admin/access-constraint-previews/", pattern: "/api/v1/admin/access-constraint-previews/{jobId}", methods: []string{http.MethodGet}},
	{prefix: "/api/v1/admin/access-constraint-previews/", pattern: "/api/v1/admin/access-constraint-previews/{jobId}/result", methods: []string{http.MethodGet}},

	// /api/v1/admin/access-constraints/
	{prefix: "/api/v1/admin/access-constraints/", pattern: "/api/v1/admin/access-constraints/{id}", methods: []string{http.MethodGet, http.MethodPut, http.MethodDelete}},
	{prefix: "/api/v1/admin/access-constraints/", pattern: "/api/v1/admin/access-constraints/{id}/affected-principals", methods: []string{http.MethodGet}},
	{prefix: "/api/v1/admin/access-constraints/", pattern: "/api/v1/admin/access-constraints/{id}/audit", methods: []string{http.MethodGet}},

	// /api/v1/admin/allow-list/
	{prefix: "/api/v1/admin/allow-list/", pattern: "/api/v1/admin/allow-list/import", methods: []string{http.MethodPost}},
	{prefix: "/api/v1/admin/allow-list/", pattern: "/api/v1/admin/allow-list/domains", methods: []string{http.MethodGet}},
	{prefix: "/api/v1/admin/allow-list/", pattern: "/api/v1/admin/allow-list/{email}", methods: []string{http.MethodDelete}},

	// /api/v1/admin/entitlements/
	{prefix: "/api/v1/admin/entitlements/", pattern: "/api/v1/admin/entitlements/{id}", methods: []string{http.MethodGet, http.MethodPut, http.MethodDelete}},

	// /api/v1/admin/integrations/
	{prefix: "/api/v1/admin/integrations/", pattern: "/api/v1/admin/integrations/available", methods: []string{http.MethodGet}},
	{prefix: "/api/v1/admin/integrations/", pattern: "/api/v1/admin/integrations/{name}", methods: []string{http.MethodGet}},
	{prefix: "/api/v1/admin/integrations/", pattern: "/api/v1/admin/integrations/{name}/config", methods: []string{http.MethodPut}},
	{prefix: "/api/v1/admin/integrations/", pattern: "/api/v1/admin/integrations/{name}/restart", methods: []string{http.MethodPost}},
	{prefix: "/api/v1/admin/integrations/", pattern: "/api/v1/admin/integrations/{name}/health", methods: []string{http.MethodGet}},
	{prefix: "/api/v1/admin/integrations/", pattern: "/api/v1/admin/integrations/{name}/update", methods: []string{http.MethodPost}},
	{prefix: "/api/v1/admin/integrations/", pattern: "/api/v1/admin/integrations/{name}/update/{id}", methods: []string{http.MethodGet}},
	{prefix: "/api/v1/admin/integrations/", pattern: "/api/v1/admin/integrations/{name}/install", methods: []string{http.MethodPost}},

	// /api/v1/admin/invites/
	{prefix: "/api/v1/admin/invites/", pattern: "/api/v1/admin/invites/stats", methods: []string{http.MethodGet}},
	{prefix: "/api/v1/admin/invites/", pattern: "/api/v1/admin/invites/{id}", methods: []string{http.MethodGet, http.MethodDelete}},
	{prefix: "/api/v1/admin/invites/", pattern: "/api/v1/admin/invites/{id}/revoke", methods: []string{http.MethodPost}},

	// /api/v1/admin/lifecycle-hooks/
	{prefix: "/api/v1/admin/lifecycle-hooks/", pattern: "/api/v1/admin/lifecycle-hooks/{id}", methods: []string{http.MethodGet, http.MethodPut, http.MethodDelete}},

	// /api/v1/admin/limits/
	{prefix: "/api/v1/admin/limits/", pattern: "/api/v1/admin/limits/{id}", methods: []string{http.MethodGet, http.MethodPut, http.MethodDelete}},
	{prefix: "/api/v1/admin/limits/", pattern: "/api/v1/admin/limits/{id}/entitlements", methods: []string{http.MethodGet, http.MethodPost}},

	// /api/v1/admin/maintenance/migrations/
	{prefix: "/api/v1/admin/maintenance/migrations/", pattern: "/api/v1/admin/maintenance/migrations/{id}/run", methods: []string{http.MethodPost}},

	// /api/v1/admin/maintenance/operations/
	{prefix: "/api/v1/admin/maintenance/operations/", pattern: "/api/v1/admin/maintenance/operations/{id}", methods: []string{http.MethodGet}},
	{prefix: "/api/v1/admin/maintenance/operations/", pattern: "/api/v1/admin/maintenance/operations/{id}/run", methods: []string{http.MethodPost}},
	{prefix: "/api/v1/admin/maintenance/operations/", pattern: "/api/v1/admin/maintenance/operations/{key}/runs", methods: []string{http.MethodGet}},
	{prefix: "/api/v1/admin/maintenance/operations/", pattern: "/api/v1/admin/maintenance/operations/{key}/runs/{runId}", methods: []string{http.MethodGet}},

	// /api/v1/admin/role-bindings/
	{prefix: "/api/v1/admin/role-bindings/", pattern: "/api/v1/admin/role-bindings/user/{userId}", methods: []string{http.MethodGet}},
	{prefix: "/api/v1/admin/role-bindings/", pattern: "/api/v1/admin/role-bindings/{id}", methods: []string{http.MethodDelete}},

	// /api/v1/admin/roles/
	{prefix: "/api/v1/admin/roles/", pattern: "/api/v1/admin/roles/{id}", methods: []string{http.MethodGet, http.MethodPut, http.MethodDelete}},
	{prefix: "/api/v1/admin/roles/", pattern: "/api/v1/admin/roles/{id}/export", methods: []string{http.MethodGet, http.MethodHead}},
	{prefix: "/api/v1/admin/roles/", pattern: "/api/v1/admin/roles/{id}/duplicate", methods: []string{http.MethodPost}},

	// /api/v1/admin/server-config/sections/
	{prefix: "/api/v1/admin/server-config/sections/", pattern: "/api/v1/admin/server-config/sections/{id}", methods: []string{http.MethodDelete}},

	// /api/v1/admin/system-one/instances/
	{prefix: "/api/v1/admin/system-one/instances/", pattern: "/api/v1/admin/system-one/instances/{name}", methods: []string{http.MethodGet, http.MethodDelete}},
	{prefix: "/api/v1/admin/system-one/instances/", pattern: "/api/v1/admin/system-one/instances/{name}/probe", methods: []string{http.MethodPost}},
	{prefix: "/api/v1/admin/system-one/instances/", pattern: "/api/v1/admin/system-one/instances/{name}/projects", methods: []string{http.MethodGet}},

	// /api/v1/admin/usage/
	{prefix: "/api/v1/admin/usage/", pattern: "/api/v1/admin/usage/{limit}", methods: []string{http.MethodGet}},

	// /api/v1/attachment-deliveries/
	{prefix: "/api/v1/attachment-deliveries/", pattern: "/api/v1/attachment-deliveries/{token}", methods: []string{http.MethodGet}},

	// /api/v1/auth/tokens/
	{prefix: "/api/v1/auth/tokens/", pattern: "/api/v1/auth/tokens/{id}", methods: []string{http.MethodGet, http.MethodDelete}},
	{prefix: "/api/v1/auth/tokens/", pattern: "/api/v1/auth/tokens/{id}/revoke", methods: []string{http.MethodPost}},

	// /api/v1/chat/attachments/
	{prefix: "/api/v1/chat/attachments/", pattern: "/api/v1/chat/attachments/{id}", methods: []string{http.MethodGet}},

	// /api/v1/chat/conversations/
	{prefix: "/api/v1/chat/conversations/", pattern: "/api/v1/chat/conversations/{id}/messages", methods: []string{http.MethodGet, http.MethodPost}},
	{prefix: "/api/v1/chat/conversations/", pattern: "/api/v1/chat/conversations/{id}/messages/{messageId}", methods: []string{http.MethodPut, http.MethodDelete}},
	{prefix: "/api/v1/chat/conversations/", pattern: "/api/v1/chat/conversations/{id}/read", methods: []string{http.MethodGet, http.MethodPost}},
	{prefix: "/api/v1/chat/conversations/", pattern: "/api/v1/chat/conversations/{id}/unread", methods: []string{http.MethodPost}},
	{prefix: "/api/v1/chat/conversations/", pattern: "/api/v1/chat/conversations/{id}/typing", methods: []string{http.MethodPost}},
	{prefix: "/api/v1/chat/conversations/", pattern: "/api/v1/chat/conversations/{id}/interagent", methods: []string{http.MethodGet}},
	{prefix: "/api/v1/chat/conversations/", pattern: "/api/v1/chat/conversations/{id}/mute", methods: []string{http.MethodPut}},
	{prefix: "/api/v1/chat/conversations/", pattern: "/api/v1/chat/conversations/{id}/pin", methods: []string{http.MethodPut}},
	{prefix: "/api/v1/chat/conversations/", pattern: "/api/v1/chat/conversations/{id}/promote", methods: []string{http.MethodPost}},
	{prefix: "/api/v1/chat/conversations/", pattern: "/api/v1/chat/conversations/{id}/decisions", methods: []string{http.MethodGet}},
	{prefix: "/api/v1/chat/conversations/", pattern: "/api/v1/chat/conversations/{id}/scheduled", methods: []string{http.MethodGet, http.MethodPost}},
	{prefix: "/api/v1/chat/conversations/", pattern: "/api/v1/chat/conversations/{id}/scheduled/{scheduledId}", methods: []string{http.MethodDelete}},
	{prefix: "/api/v1/chat/conversations/", pattern: "/api/v1/chat/conversations/{id}/scheduled/{scheduledId}/send-now", methods: []string{http.MethodPost}},
	{prefix: "/api/v1/chat/conversations/", pattern: "/api/v1/chat/conversations/{id}/scheduled/{scheduledId}/dismiss", methods: []string{http.MethodPost}},

	// /api/v1/chat/spaces/
	{prefix: "/api/v1/chat/spaces/", pattern: "/api/v1/chat/spaces/{id}/threads", methods: []string{http.MethodGet, http.MethodPost}},
	{prefix: "/api/v1/chat/spaces/", pattern: "/api/v1/chat/spaces/{id}/members", methods: []string{http.MethodGet}},
	{prefix: "/api/v1/chat/spaces/", pattern: "/api/v1/chat/spaces/{id}/read", methods: []string{http.MethodPost}},
	{prefix: "/api/v1/chat/spaces/", pattern: "/api/v1/chat/spaces/{id}/emoji", methods: []string{http.MethodPut}},

	// /api/v1/chat/stars/
	{prefix: "/api/v1/chat/stars/", pattern: "/api/v1/chat/stars/state", methods: []string{http.MethodGet}},
	{prefix: "/api/v1/chat/stars/", pattern: "/api/v1/chat/stars/{messageId}", methods: []string{http.MethodGet, http.MethodPut, http.MethodDelete}},

	// /api/v1/chat/topics/
	{prefix: "/api/v1/chat/topics/", pattern: "/api/v1/chat/topics/{id}", methods: []string{http.MethodGet, http.MethodPatch, http.MethodDelete}},

	// /api/v1/command-requests/
	{prefix: "/api/v1/command-requests/", pattern: "/api/v1/command-requests/from-message", methods: []string{http.MethodPost}},
	{prefix: "/api/v1/command-requests/", pattern: "/api/v1/command-requests/{id}", methods: []string{http.MethodGet}},
	{prefix: "/api/v1/command-requests/", pattern: "/api/v1/command-requests/{id}/approve", methods: []string{http.MethodPost}},
	{prefix: "/api/v1/command-requests/", pattern: "/api/v1/command-requests/{id}/output", methods: []string{http.MethodGet}},
	{prefix: "/api/v1/command-requests/", pattern: "/api/v1/command-requests/{id}/dismiss", methods: []string{http.MethodPost}},

	// /api/v1/conversations/
	{prefix: "/api/v1/conversations/", pattern: "/api/v1/conversations/", methods: []string{http.MethodPost}},
	{prefix: "/api/v1/conversations/", pattern: "/api/v1/conversations/{id}", methods: []string{http.MethodGet}},
	{prefix: "/api/v1/conversations/", pattern: "/api/v1/conversations/{id}/messages", methods: []string{http.MethodGet}},
	{prefix: "/api/v1/conversations/", pattern: "/api/v1/conversations/{id}/messages/{messageId}", methods: []string{http.MethodGet}},
	{prefix: "/api/v1/conversations/", pattern: "/api/v1/conversations/{id}/messages/{messageId}/attachments/{attachmentId}", methods: []string{http.MethodGet}},
	{prefix: "/api/v1/conversations/", pattern: "/api/v1/conversations/{id}/watch", methods: []string{http.MethodGet}},
	{prefix: "/api/v1/conversations/", pattern: "/api/v1/conversations/{id}/default-agent", methods: []string{http.MethodPut}},
	{prefix: "/api/v1/conversations/", pattern: "/api/v1/conversations/{id}/participants", methods: []string{http.MethodPost}},
	{prefix: "/api/v1/conversations/", pattern: "/api/v1/conversations/{id}/leave", methods: []string{http.MethodPost}},

	// /api/v1/decision-prompts/
	{prefix: "/api/v1/decision-prompts/", pattern: "/api/v1/decision-prompts/{messageId}/answers", methods: []string{http.MethodPost}},
	{prefix: "/api/v1/decision-prompts/", pattern: "/api/v1/decision-prompts/{messageId}/dismiss", methods: []string{http.MethodPost}},

	// /api/v1/decisions/
	{prefix: "/api/v1/decisions/", pattern: "/api/v1/decisions/{rowId}", methods: []string{http.MethodGet}},
	{prefix: "/api/v1/decisions/", pattern: "/api/v1/decisions/{rowId}/answer", methods: []string{http.MethodPost}},

	// /api/v1/env/
	{prefix: "/api/v1/env/", pattern: "/api/v1/env/{key}", methods: []string{http.MethodGet, http.MethodPut, http.MethodDelete}},

	// /api/v1/gcp-service-accounts/
	{prefix: "/api/v1/gcp-service-accounts/", pattern: "/api/v1/gcp-service-accounts/{id}", methods: []string{http.MethodGet, http.MethodDelete}},
	{prefix: "/api/v1/gcp-service-accounts/", pattern: "/api/v1/gcp-service-accounts/{id}/verify", methods: []string{http.MethodPost}},

	// /api/v1/groups/
	{prefix: "/api/v1/groups/", pattern: "/api/v1/groups/{id}", methods: []string{http.MethodGet, http.MethodPatch, http.MethodDelete}},
	{prefix: "/api/v1/groups/", pattern: "/api/v1/groups/{id}/members", methods: []string{http.MethodGet, http.MethodPost}},
	{prefix: "/api/v1/groups/", pattern: "/api/v1/groups/{id}/members/{memberType}/{memberId}", methods: []string{http.MethodGet, http.MethodDelete}},
	{prefix: "/api/v1/groups/", pattern: "/api/v1/groups/{id}/usage/budgets", methods: []string{http.MethodGet}},

	// /api/v1/harness-configs/
	{prefix: "/api/v1/harness-configs/", pattern: "/api/v1/harness-configs/{id}", methods: []string{http.MethodGet, http.MethodPut, http.MethodPatch, http.MethodDelete}},
	{prefix: "/api/v1/harness-configs/", pattern: "/api/v1/harness-configs/{id}/upload", methods: []string{http.MethodPost}},
	{prefix: "/api/v1/harness-configs/", pattern: "/api/v1/harness-configs/{id}/finalize", methods: []string{http.MethodPost}},
	{prefix: "/api/v1/harness-configs/", pattern: "/api/v1/harness-configs/{id}/download", methods: []string{http.MethodGet}},
	{prefix: "/api/v1/harness-configs/", pattern: "/api/v1/harness-configs/{id}/clone", methods: []string{http.MethodPost}},
	{prefix: "/api/v1/harness-configs/", pattern: "/api/v1/harness-configs/{id}/validate", methods: []string{http.MethodGet}},
	{prefix: "/api/v1/harness-configs/", pattern: "/api/v1/harness-configs/{id}/check-image", methods: []string{http.MethodPost}},
	{prefix: "/api/v1/harness-configs/", pattern: "/api/v1/harness-configs/{id}/image-status", methods: []string{http.MethodGet}},
	{prefix: "/api/v1/harness-configs/", pattern: "/api/v1/harness-configs/{id}/local-image", methods: []string{http.MethodDelete}},
	{prefix: "/api/v1/harness-configs/", pattern: "/api/v1/harness-configs/{id}/pull-image", methods: []string{http.MethodPost}},
	{prefix: "/api/v1/harness-configs/", pattern: "/api/v1/harness-configs/{id}/reimport", methods: []string{http.MethodPost}},
	{prefix: "/api/v1/harness-configs/", pattern: "/api/v1/harness-configs/{id}/files", methods: []string{http.MethodGet, http.MethodPost}},
	{prefix: "/api/v1/harness-configs/", pattern: "/api/v1/harness-configs/{id}/files/{path}", methods: []string{http.MethodGet, http.MethodPut, http.MethodDelete}},

	// /api/v1/messages/
	{prefix: "/api/v1/messages/", pattern: "/api/v1/messages/read-all", methods: []string{http.MethodPost}},
	{prefix: "/api/v1/messages/", pattern: "/api/v1/messages/{id}", methods: []string{http.MethodGet}},
	{prefix: "/api/v1/messages/", pattern: "/api/v1/messages/{id}/read", methods: []string{http.MethodPost}},

	// /api/v1/metrics/
	{prefix: "/api/v1/metrics/", pattern: "/api/v1/metrics/{name}", methods: []string{http.MethodGet}},

	// /api/v1/notifications/
	{prefix: "/api/v1/notifications/", pattern: "/api/v1/notifications/ack-all", methods: []string{http.MethodPost}},
	{prefix: "/api/v1/notifications/", pattern: "/api/v1/notifications/{id}/ack", methods: []string{http.MethodPost}},
	{prefix: "/api/v1/notifications/", pattern: "/api/v1/notifications/subscriptions", methods: []string{http.MethodGet, http.MethodPost}},
	{prefix: "/api/v1/notifications/", pattern: "/api/v1/notifications/subscriptions/{id}", methods: []string{http.MethodPatch, http.MethodDelete}},
	{prefix: "/api/v1/notifications/", pattern: "/api/v1/notifications/subscriptions/bulk", methods: []string{http.MethodPost}},
	{prefix: "/api/v1/notifications/", pattern: "/api/v1/notifications/subscriptions/bulk-delete", methods: []string{http.MethodPost}},
	{prefix: "/api/v1/notifications/", pattern: "/api/v1/notifications/templates", methods: []string{http.MethodGet, http.MethodPost}},
	{prefix: "/api/v1/notifications/", pattern: "/api/v1/notifications/templates/{id}", methods: []string{http.MethodDelete}},

	// /api/v1/pending-secrets/
	{prefix: "/api/v1/pending-secrets/", pattern: "/api/v1/pending-secrets/{id}/approve", methods: []string{http.MethodPost}},
	{prefix: "/api/v1/pending-secrets/", pattern: "/api/v1/pending-secrets/{id}/reject", methods: []string{http.MethodPost}},
	{prefix: "/api/v1/pending-secrets/", pattern: "/api/v1/pending-secrets/{id}/reveal", methods: []string{http.MethodPost}},

	// /api/v1/runtime-brokers/
	{prefix: "/api/v1/runtime-brokers/", pattern: "/api/v1/runtime-brokers/{id}", methods: []string{http.MethodGet, http.MethodPatch, http.MethodDelete}},
	{prefix: "/api/v1/runtime-brokers/", pattern: "/api/v1/runtime-brokers/{id}/env", methods: []string{http.MethodGet}},
	{prefix: "/api/v1/runtime-brokers/", pattern: "/api/v1/runtime-brokers/{id}/env/{key}", methods: []string{http.MethodGet, http.MethodPut, http.MethodDelete}},
	{prefix: "/api/v1/runtime-brokers/", pattern: "/api/v1/runtime-brokers/{id}/secrets", methods: []string{http.MethodGet}},
	{prefix: "/api/v1/runtime-brokers/", pattern: "/api/v1/runtime-brokers/{id}/secrets/{key}", methods: []string{http.MethodGet, http.MethodPut, http.MethodPatch, http.MethodDelete}},
	{prefix: "/api/v1/runtime-brokers/", pattern: "/api/v1/runtime-brokers/{id}/settings", methods: []string{http.MethodGet, http.MethodPut}},
	{prefix: "/api/v1/runtime-brokers/", pattern: "/api/v1/runtime-brokers/{id}/conversion-plan", methods: []string{http.MethodGet}},
	{prefix: "/api/v1/runtime-brokers/", pattern: "/api/v1/runtime-brokers/{id}/agents/{agentId}/launch", methods: []string{http.MethodPost}},
	{prefix: "/api/v1/runtime-brokers/", pattern: "/api/v1/runtime-brokers/{id}/agents/{agentId}/deliver", methods: []string{http.MethodPost}},
	{prefix: "/api/v1/runtime-brokers/", pattern: "/api/v1/runtime-brokers/{id}/agents/{agentId}/workspace-download", methods: []string{http.MethodPost}},
	{prefix: "/api/v1/runtime-brokers/", pattern: "/api/v1/runtime-brokers/{id}/agents/{agentId}/move-bundle/{rid}/upload-url", methods: []string{http.MethodPost}},
	{prefix: "/api/v1/runtime-brokers/", pattern: "/api/v1/runtime-brokers/{id}/agents/{agentId}/move-bundle/{rid}/download-urls", methods: []string{http.MethodPost}},
	{prefix: "/api/v1/runtime-brokers/", pattern: "/api/v1/runtime-brokers/{id}/heartbeat", methods: []string{http.MethodPost}},
	{prefix: "/api/v1/runtime-brokers/", pattern: "/api/v1/runtime-brokers/{id}/message-failures", methods: []string{http.MethodPost}},
	{prefix: "/api/v1/runtime-brokers/", pattern: "/api/v1/runtime-brokers/{id}/projects", methods: []string{http.MethodGet}},

	// /api/v1/secret-requests/
	{prefix: "/api/v1/secret-requests/", pattern: "/api/v1/secret-requests/{messageId}", methods: []string{http.MethodGet}},
	{prefix: "/api/v1/secret-requests/", pattern: "/api/v1/secret-requests/{messageId}/fulfill", methods: []string{http.MethodPost}},
	{prefix: "/api/v1/secret-requests/", pattern: "/api/v1/secret-requests/{messageId}/dismiss", methods: []string{http.MethodPost}},

	// /api/v1/secrets/
	{prefix: "/api/v1/secrets/", pattern: "/api/v1/secrets/{key}", methods: []string{http.MethodGet, http.MethodPut, http.MethodPatch, http.MethodDelete}},

	// /api/v1/skill-registries/
	{prefix: "/api/v1/skill-registries/", pattern: "/api/v1/skill-registries/{id}", methods: []string{http.MethodGet, http.MethodPut, http.MethodPatch, http.MethodDelete}},
	{prefix: "/api/v1/skill-registries/", pattern: "/api/v1/skill-registries/{id}/pin", methods: []string{http.MethodPost}},
	{prefix: "/api/v1/skill-registries/", pattern: "/api/v1/skill-registries/{id}/pins", methods: []string{http.MethodGet}},
	{prefix: "/api/v1/skill-registries/", pattern: "/api/v1/skill-registries/{id}/unpin", methods: []string{http.MethodPost}},

	// /api/v1/skills/
	{prefix: "/api/v1/skills/", pattern: "/api/v1/skills/resolve", methods: []string{http.MethodPost}},
	{prefix: "/api/v1/skills/", pattern: "/api/v1/skills/{id}", methods: []string{http.MethodGet, http.MethodPatch, http.MethodDelete}},
	{prefix: "/api/v1/skills/", pattern: "/api/v1/skills/{id}/versions", methods: []string{http.MethodGet, http.MethodPost}},
	{prefix: "/api/v1/skills/", pattern: "/api/v1/skills/{id}/versions/{vid}", methods: []string{http.MethodGet}},
	{prefix: "/api/v1/skills/", pattern: "/api/v1/skills/{id}/versions/{vid}/deprecate", methods: []string{http.MethodPost}},
	{prefix: "/api/v1/skills/", pattern: "/api/v1/skills/{id}/upload", methods: []string{http.MethodPost}},
	{prefix: "/api/v1/skills/", pattern: "/api/v1/skills/{id}/finalize", methods: []string{http.MethodPost}},
	{prefix: "/api/v1/skills/", pattern: "/api/v1/skills/{id}/download", methods: []string{http.MethodGet}},
	{prefix: "/api/v1/skills/", pattern: "/api/v1/skills/{id}/resolve", methods: []string{http.MethodGet}},
	{prefix: "/api/v1/skills/", pattern: "/api/v1/skills/{id}/files/{path}", methods: []string{http.MethodGet, http.MethodPut}},

	// /api/v1/templates/
	{prefix: "/api/v1/templates/", pattern: "/api/v1/templates/{id}", methods: []string{http.MethodGet, http.MethodPut, http.MethodPatch, http.MethodDelete}},
	{prefix: "/api/v1/templates/", pattern: "/api/v1/templates/{id}/upload", methods: []string{http.MethodPost}},
	{prefix: "/api/v1/templates/", pattern: "/api/v1/templates/{id}/finalize", methods: []string{http.MethodPost}},
	{prefix: "/api/v1/templates/", pattern: "/api/v1/templates/{id}/download", methods: []string{http.MethodGet}},
	{prefix: "/api/v1/templates/", pattern: "/api/v1/templates/{id}/clone", methods: []string{http.MethodPost}},
	{prefix: "/api/v1/templates/", pattern: "/api/v1/templates/{id}/validate", methods: []string{http.MethodGet}},
	{prefix: "/api/v1/templates/", pattern: "/api/v1/templates/{id}/reimport", methods: []string{http.MethodPost}},
	{prefix: "/api/v1/templates/", pattern: "/api/v1/templates/{id}/files", methods: []string{http.MethodGet, http.MethodPost}},
	{prefix: "/api/v1/templates/", pattern: "/api/v1/templates/{id}/files/{path}", methods: []string{http.MethodGet, http.MethodPut, http.MethodDelete}},

	// /api/v1/thread-groups/
	{prefix: "/api/v1/thread-groups/", pattern: "/api/v1/thread-groups/{id}", methods: []string{http.MethodGet, http.MethodPatch, http.MethodDelete}},
	{prefix: "/api/v1/thread-groups/", pattern: "/api/v1/thread-groups/{id}/threads", methods: []string{http.MethodPost}},
	{prefix: "/api/v1/thread-groups/", pattern: "/api/v1/thread-groups/{id}/threads/{conversationId}", methods: []string{http.MethodDelete}},

	// /api/v1/users/
	{prefix: "/api/v1/users/", pattern: "/api/v1/users/{id}", methods: []string{http.MethodGet, http.MethodPatch, http.MethodDelete}},
	{prefix: "/api/v1/users/", pattern: "/api/v1/users/{id}/revoke-sessions", methods: []string{http.MethodPost}},
	{prefix: "/api/v1/users/", pattern: "/api/v1/users/{id}/delete-blockers", methods: []string{http.MethodGet}},
	{prefix: "/api/v1/users/", pattern: "/api/v1/users/{id}/deletion", methods: []string{http.MethodGet}},
	{prefix: "/api/v1/users/", pattern: "/api/v1/users/{id}/usage", methods: []string{http.MethodGet}},

	// /api/v1/users/me/injected-skills/
	{prefix: "/api/v1/users/me/injected-skills/", pattern: "/api/v1/users/me/injected-skills/{id}", methods: []string{http.MethodDelete}},

	// /api/v1/users/me/templates/
	{prefix: "/api/v1/users/me/templates/", pattern: "/api/v1/users/me/templates/{id}", methods: []string{http.MethodGet, http.MethodPut, http.MethodDelete}},
	{prefix: "/api/v1/users/me/templates/", pattern: "/api/v1/users/me/templates/{id}/upload", methods: []string{http.MethodPost}},
	{prefix: "/api/v1/users/me/templates/", pattern: "/api/v1/users/me/templates/{id}/finalize", methods: []string{http.MethodPost}},
	{prefix: "/api/v1/users/me/templates/", pattern: "/api/v1/users/me/templates/{id}/download", methods: []string{http.MethodGet}},
}

// enumeratedPrefixSet returns the set of trailing-slash prefixes covered
// row-by-row by prefixSubRouteTable.
func enumeratedPrefixSet() map[string]bool {
	out := make(map[string]bool)
	for _, row := range prefixSubRouteTable {
		out[row.prefix] = true
	}
	return out
}
