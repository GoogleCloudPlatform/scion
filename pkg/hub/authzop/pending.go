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
	// Response classifies what the response body can carry (see
	// ResponseSensitivity). HTTP-like entry points only.
	Response ResponseSensitivity
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
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/agents/{id}/groups", Method: "GET", Area: AreaAgents},
	// --- projects ---
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/artifacts/shared/{token}", Method: "GET", Area: AreaProjects},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{id}/gcp-service-accounts/mint", Method: "POST", Area: AreaProjects},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{id}/gcp-service-accounts/{saId}", Method: "DELETE", Area: AreaProjects},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{id}/gcp-service-accounts/{saId}", Method: "GET", Area: AreaProjects},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{id}/gcp-service-accounts/{saId}/status", Method: "GET", Area: AreaProjects},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{id}/gcp-service-accounts/{saId}/verify", Method: "POST", Area: AreaProjects},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{id}/providers", Method: "POST", Area: AreaProjects},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/projects/{id}/providers/{brokerId}", Method: "DELETE", Area: AreaProjects},
	// --- messaging ---
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/gcs/object", Method: "GET", Variant: "direct_or_projectless", Area: AreaMessaging},
	// --- chat ---
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/chat/attachments/{id}", Method: "GET", Variant: "noproject", Area: AreaChat},
	// --- identity ---
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/groups/{id}/members", Method: "GET", Area: AreaIdentity},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/groups/{id}/members/{type}/{memberId}", Method: "GET", Area: AreaIdentity},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/principals/agent/{id}", Method: "GET", Area: AreaIdentity},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/principals/group/{id}", Method: "GET", Area: AreaIdentity},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/principals/user/{id}", Method: "GET", Area: AreaIdentity},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/users/me/visitor-dms", Method: "GET", Area: AreaIdentity},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/users/me/visitor-grants", Method: "GET", Area: AreaIdentity},
	// --- hub ---
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/delegation-adoption", Method: "GET", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/delegation-adoption/commits", Method: "POST", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/delegation-adoption/previews", Method: "POST", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/maintenance/operations/{key}/runs", Method: "GET", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/admin/maintenance/operations/{key}/runs/{runId}", Method: "GET", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/conduit/grant-keys", Method: "GET", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/experiments", Method: "GET", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/profiling", Method: "GET", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/hub/settings/injected-skills", Method: "GET", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/metrics/session/{id}", Method: "GET", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/pre-start-hooks", Method: "GET", Area: AreaHub},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/pre-start-hooks/{id}", Method: "GET", Area: AreaHub},
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
	{Kind: EntryPointHTTPRoute, Pattern: "/metrics", Method: "GET", Area: AreaHub},
	// --- catalog_resources ---
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/harness-configs/{id}/local-image", Method: "DELETE", Area: AreaCatalogResources},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/harness-configs/{id}/pull-image", Method: "POST", Area: AreaCatalogResources},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/skills/resolve", Method: "POST", Variant: "ref=registry", Area: AreaCatalogResources, Response: ResponseCredential},
	// --- brokers ---
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/brokers/{id}/rotate-secret", Method: "POST", Area: AreaBrokers, Response: ResponseCredential},
	// --- material ---
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/env/{key}", Method: "DELETE", Area: AreaMaterial},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/env/{key}", Method: "PUT", Area: AreaMaterial, Response: ResponseCredential},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/gcp-service-accounts/mint", Method: "POST", Area: AreaMaterial},
	{Kind: EntryPointHTTPRoute, Pattern: "/api/v1/secrets/{key}", Method: "PATCH", Area: AreaMaterial},
}

// PendingBearerOperations lists catalogued operations whose bearer
// disposition is recorded by a later batch. A listed operation carries no
// Bearer value, and Validate does not apply the credentials agreement rule
// to it until its disposition is recorded.
var PendingBearerOperations = []OperationID{
	"agent.message.send",
	"env.hub.list",
	"env.read",
	"gcp.identity.assign",
	"gcp.identity.create",
	"gcp.identity.delete",
	"gcp.identity.mint",
	"gcp.identity.read",
	"gcp.identity.verify",
	"group.read",
	"hub.adminmode.read",
	"hub.adminmode.update",
	"hub.maintenance.execute",
	"secret.read",
	"secret.write",
	"user.read",
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
