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

// EntryPointExemption documents a route or entry point that does not map to an
// authorization operation. Each exemption is typed, scoped, owned, and
// rationalized. Invalid or stale exemptions fail CI.
type EntryPointExemption struct {
	// Pattern is the route pattern or entry point identifier (matches
	// routeMetadataTable keys or non-HTTP entry point names).
	Pattern string

	// Kind classifies the exemption.
	Kind ExemptionKind

	// Reason explains why this entry point is exempt from operation mapping.
	Reason string

	// Owner identifies the reviewer or team responsible for this exemption.
	Owner string
}

// MutationClassification maps a discovered security-relevant mutation call
// site to its cataloged operation or reviewed exemption.
type MutationClassification struct {
	// File is the source file path relative to the project root.
	File string

	// Function is the enclosing function name.
	Function string

	// Symbol is the mutation method/function name.
	Symbol string

	// OperationID maps to a Catalog operation (empty if Exemption is set).
	OperationID OperationID

	// Exemption documents why this site is exempt from operation mapping.
	// Exactly one of OperationID or Exemption must be set.
	Exemption *MutationExemption
}

// MutationExemption documents why a specific mutation call site is exempt.
type MutationExemption struct {
	Kind   ExemptionKind
	Reason string
	Scope  string
}

// SecurityMutationSymbols is the closed set of method/function names that
// represent security-relevant mutations and external effects. The
// authorization audit scanner discovers call sites matching these symbols
// and requires each to be classified.
//
// Scanner boundary: the scanner walks all .go files (excluding _test.go)
// under pkg/hub/ and pkg/store/ recursively. Methods in extras/, pkg/ent/,
// and pkg/k8s/ are outside the trust boundary.
var SecurityMutationSymbols = map[string]string{
	// Authority mutations — role bindings
	"CreateRoleBinding":              "grant-authority",
	"DeleteRoleBinding":              "revoke-authority",
	"DeleteRoleBindingsForPrincipal": "revoke-authority",
	"DeleteRoleBindingsForScope":     "revoke-authority",

	// Role definition mutations
	"CreateRoleDefinition":                  "create-resource",
	"UpdateRoleDefinition":                  "update-resource",
	"DeleteRoleDefinition":                  "delete-resource",
	"UpdateSystemRoleDefinitionPermissions": "change-authority",

	// Group mutations
	"CreateGroup":           "create-resource",
	"UpdateGroup":           "update-resource",
	"DeleteGroup":           "delete-resource",
	"AddGroupMember":        "grant-authority",
	"RemoveGroupMember":     "revoke-authority",
	"RemoveChildGroupEdge":  "revoke-authority",
	"UpdateGroupMemberRole": "change-authority",
	// Group-membership cleanup on principal delete and at startup
	// (ptone/scion#2769): rows of deleted users and agents.
	"DeleteGroupMembershipsForUser":   "revoke-authority",
	"DeleteGroupMembershipsForAgents": "revoke-authority",
	"DeleteOrphanedGroupMemberships":  "revoke-authority",

	// Access constraint mutations
	"CreateAccessConstraint": "tighten-boundary",
	"UpdateAccessConstraint": "change-authority",
	"DeleteAccessConstraint": "relax-boundary",

	// User lifecycle mutations
	"CreateUser": "create-resource",
	"UpdateUser": "change-principal-status",
	"DeleteUser": "delete-resource",

	// Credential/token mutations — user access tokens
	"CreateUserAccessToken": "mint-credential",
	"RevokeUserAccessToken": "revoke-authority",
	"DeleteUserAccessToken": "revoke-authority",

	// Credential mutations — agent credentials
	"CreateAgentCredential":         "mint-credential",
	"RevokeAgentCredential":         "revoke-authority",
	"RevokeAgentCredentialsByAgent": "revoke-authority",

	// Secret operations
	"CreateSecret":               "create-resource",
	"UpdateSecret":               "update-resource",
	"UpdateSecretValueIfVersion": "update-resource",
	"UpsertSecret":               "create-resource",
	"DeleteSecret":               "delete-resource",
	"DeleteSecretsByScope":       "delete-resource",
	"GetSecretValue":             "read-secret",

	// Broker secret operations
	"CreateBrokerSecret":     "create-resource",
	"UpdateBrokerSecret":     "update-resource",
	"DeleteBrokerSecret":     "delete-resource",
	"CreateJoinToken":        "mint-credential",
	"UpsertJoinToken":        "mint-credential",
	"DeleteJoinToken":        "delete-resource",
	"ConsumeJoinToken":       "delete-resource",
	"DeleteExpiredJoinToken": "delete-resource",
	"CleanExpiredJoinTokens": "delete-resource",

	// Invite code operations
	"CreateInviteCode": "mint-credential",
	"RevokeInviteCode": "revoke-authority",
	"DeleteInviteCode": "delete-resource",

	// GCP service account mutations (store-level)
	"CreateGCPServiceAccount": "assign-credential",
	"DeleteGCPServiceAccount": "delete-resource",
	"UpdateGCPServiceAccount": "update-resource",

	// GCP IAM external effects
	"GenerateAccessToken":  "mint-credential",
	"CreateServiceAccount": "emit-external",
	"DeleteServiceAccount": "emit-external",
	"SetIAMPolicy":         "emit-external",

	// Project lifecycle
	"DeleteProject": "delete-resource",

	// Agent lifecycle
	"DeleteAgent": "delete-resource",
	// FinalizeAgentDeletion is the delete engine's one-transaction soft or
	// hard delete (with cascade). It replaced the handler's DeleteAgent call.
	"FinalizeAgentDeletion": "delete-resource",
}

// Catalog is the authoritative operation catalog. Every externally reachable
// high-risk entry point maps to exactly one OperationSpec here or to an
// EntryPointExemption. The catalog is validated by TestCatalogValidation.
//
// Operations are organized by domain. Each spec uses the frozen
// OperationSpec vocabulary from CT1.
var Catalog = concatOperations(
	agentOperations,
	projectOperations,
	messagingOperations,
	chatOperations,
	identityOperations,
	hubOperations,
	catalogResourceOperations,
	brokerOperations,
	materialOperations,
)

// EntryPointExemptions documents routes and entry points that do not map to
// authorization operations. Each exemption is typed, scoped, owned, and
// rationalized. The CI gate validates that every exemption references a real
// route and that no route is both cataloged and exempted.
var EntryPointExemptions = []EntryPointExemption{
	// Public endpoints — no authorization required
	{Pattern: "/healthz", Kind: ExemptionPublicEndpoint, Reason: "Liveness probe, no authorization", Owner: "route_metadata.go"},
	{Pattern: "/readyz", Kind: ExemptionPublicEndpoint, Reason: "Readiness probe, no authorization", Owner: "route_metadata.go"},
	{Pattern: "/metrics", Kind: ExemptionPublicEndpoint, Reason: "Prometheus metrics, no authorization", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/auth/login", Kind: ExemptionPublicEndpoint, Reason: "Auth login flow, pre-authentication", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/auth/token", Kind: ExemptionPublicEndpoint, Reason: "Auth token exchange, pre-authentication", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/auth/refresh", Kind: ExemptionPublicEndpoint, Reason: "Auth token refresh, pre-authentication", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/auth/validate", Kind: ExemptionPublicEndpoint, Reason: "Token validation, pre-authentication", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/auth/providers", Kind: ExemptionPublicEndpoint, Reason: "Auth provider list, public configuration", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/auth/invite/redeem", Kind: ExemptionPublicEndpoint, Reason: "Invite redemption, pre-authentication", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/auth/cli/authorize", Kind: ExemptionPublicEndpoint, Reason: "CLI auth flow, pre-authentication", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/auth/cli/token", Kind: ExemptionPublicEndpoint, Reason: "CLI token exchange, pre-authentication", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/auth/cli/device", Kind: ExemptionPublicEndpoint, Reason: "CLI device auth flow, pre-authentication", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/auth/cli/device/token", Kind: ExemptionPublicEndpoint, Reason: "CLI device token exchange, pre-authentication", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/auth/integrations/google/exchange", Kind: ExemptionPublicEndpoint, Reason: "GE Google credential exchange, pre-authentication", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/settings/public", Kind: ExemptionPublicEndpoint, Reason: "Public settings, no secrets", Owner: "route_metadata.go"},
	{Pattern: "/github-app/setup", Kind: ExemptionPublicEndpoint, Reason: "GitHub App setup callback, pre-authentication", Owner: "route_metadata.go"},
	{Pattern: "GET /.well-known/openid-configuration", Kind: ExemptionPublicEndpoint, Reason: "OIDC discovery, public standard", Owner: "route_metadata.go"},
	{Pattern: "GET /.well-known/jwks.json", Kind: ExemptionPublicEndpoint, Reason: "OIDC JWKS, public standard", Owner: "route_metadata.go"},

	// Authentication-only endpoints — identity required but no resource-level authorization
	{Pattern: "/api/v1/auth/me", Kind: ExemptionAuthenticationOnly, Reason: "Read own identity, self-service", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/auth/admin-status", Kind: ExemptionAuthenticationOnly, Reason: "Check own admin status, self-service", Owner: "route_metadata.go"},
	// /api/v1/auth/tokens: GET is self-service (exempted); POST is cataloged as credential.token.create.
	// /api/v1/auth/tokens/: GET is self-service (exempted); POST {id}/revoke and DELETE {id} are both
	// cataloged as credential.token.revoke (RS4/A5: one operation, two entry points).
	// The catalog uses method-specific entry points; the route metadata uses the base pattern for both.
	{Pattern: "/api/v1/auth/scopes", Kind: ExemptionAuthenticationOnly, Reason: "List available scopes, self-service", Owner: "route_metadata.go"},
	// Conversation management API — inline authorization via participant checks.
	{Pattern: "/api/v1/conversations", Kind: ExemptionAuthenticationOnly, Reason: "Conversation list/create, inline participant-based authorization", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/conversations/", Kind: ExemptionAuthenticationOnly, Reason: "Conversation by ID, inline participant-based authorization", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/conversations/resolve", Kind: ExemptionAuthenticationOnly, Reason: "Conversation resolution, inline authorization", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/gcp-service-accounts/mint", Kind: ExemptionAuthenticationOnly, Reason: "Hub-scope GCP SA minting, inline policy check in handler", Owner: "route_metadata.go"},
	// Cross-project messaging — inline authorization.
	{Pattern: "/api/v1/messaging/capabilities", Kind: ExemptionAuthenticationOnly, Reason: "Messaging capabilities query, authenticated read-only", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/messaging/targets/resolve", Kind: ExemptionAuthenticationOnly, Reason: "Messaging target resolution, inline policy check", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/metrics/session/", Kind: ExemptionAuthenticationOnly, Reason: "Session metrics, self-service", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/users/me/groups", Kind: ExemptionAuthenticationOnly, Reason: "List own group memberships, self-service", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/principals/", Kind: ExemptionAuthenticationOnly, Reason: "Resolve principal display name, self-service", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/users/me/injected-skills", Kind: ExemptionAuthenticationOnly, Reason: "Manage own injected skills, self-service", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/users/me/injected-skills/", Kind: ExemptionAuthenticationOnly, Reason: "Manage own injected skill by ID, self-service", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/users/me/templates", Kind: ExemptionAuthenticationOnly, Reason: "Manage own templates, self-service", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/users/me/templates/", Kind: ExemptionAuthenticationOnly, Reason: "Manage own template by ID, self-service", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/notifications", Kind: ExemptionAuthenticationOnly, Reason: "List own notifications, self-service", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/notifications/", Kind: ExemptionAuthenticationOnly, Reason: "Manage own notification by ID, self-service", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/messages", Kind: ExemptionAuthenticationOnly, Reason: "List own messages, self-service", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/messages/", Kind: ExemptionAuthenticationOnly, Reason: "Manage own message by ID, self-service", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/gcs/object", Kind: ExemptionAuthenticationOnly, Reason: "gs:// link fetch, inline message-visibility-based authorization", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/conduit/grant-keys", Kind: ExemptionAuthenticationOnly, Reason: "Conduit grant public keys, authenticated read-only, experiment-gated", Owner: "route_metadata.go"},
	// Artifact share links (hub.artifacts experiment): no handler behaviour
	// yet, the route answers 404; a catalog operation replaces this
	// exemption when share links land (ptone/scion#3202).
	{Pattern: "/api/v1/artifacts/shared/", Kind: ExemptionPublicEndpoint, Reason: "Artifact share links (token-only by design; still behind the auth middleware until token access ships), experiment-gated, answers 404 with no handler behaviour yet; replaced by a catalog operation when the handler lands (ptone/scion#3202)", Owner: "route_metadata.go"},
	// Artifact views (hub.artifacts experiment): reads of one version's
	// files under a short-lived view capability minted after the caller's
	// read access was checked; the artifact service verifies the
	// capability on every request and never uses a session here.
	{Pattern: "/api/v1/artifacts/view/", Kind: ExemptionPublicEndpoint, Reason: "Artifact view capability (minted on POST /api/v1/artifacts/{id}/versions/{seq}/view after artifact.read; HMAC over artifact, version and expiry, verified by the artifact service on every GET/HEAD), experiment-gated", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/message-channels", Kind: ExemptionAuthenticationOnly, Reason: "List own message channels, self-service", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/chat/user-prefs", Kind: ExemptionAuthenticationOnly, Reason: "Chat preferences, self-service", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/chat/presence", Kind: ExemptionAuthenticationOnly, Reason: "Chat presence, self-service", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/telegram/link", Kind: ExemptionInternalOnly, Reason: "Chat account link registration, broker-authenticated", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/telegram/link/verify", Kind: ExemptionAuthenticationOnly, Reason: "Account linking verification, self-service", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/telegram/link/status", Kind: ExemptionInternalOnly, Reason: "Chat account link status, broker-authenticated", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/discord/link", Kind: ExemptionInternalOnly, Reason: "Chat account link registration, broker-authenticated", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/discord/link/verify", Kind: ExemptionAuthenticationOnly, Reason: "Account linking verification, self-service", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/discord/link/status", Kind: ExemptionInternalOnly, Reason: "Chat account link status, broker-authenticated", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/teams/link", Kind: ExemptionInternalOnly, Reason: "Chat account link registration, broker-authenticated", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/teams/link/verify", Kind: ExemptionAuthenticationOnly, Reason: "Account linking verification, self-service", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/teams/link/status", Kind: ExemptionInternalOnly, Reason: "Chat account link status, broker-authenticated", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/authz/explain", Kind: ExemptionAuthenticationOnly, Reason: "Authorization explain for self, self-service diagnostic", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/hub/settings/injected-skills", Kind: ExemptionHubAdmin, Reason: "Hub injected skills; GET is open, PUT requires hub-admin (enforced in handler via requireAdmin). Admin mutation — operation contract deferred to AH1.", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/pre-start-hooks", Kind: ExemptionHubAdmin, Reason: "Pre-start hooks; GET is open, POST/PUT/DELETE require hub-admin (enforced in handler via requireAdmin). Admin mutation — operation contract deferred to AH1.", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/pre-start-hooks/", Kind: ExemptionHubAdmin, Reason: "Pre-start hooks by ID; admin enforcement in handler via requireAdmin. Admin mutation — operation contract deferred to AH1.", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/admin/delegation-adoption", Kind: ExemptionHubAdmin, Reason: "Delegation-provenance adoption status; hub system admin on a session or local development credential only (route guard plus handler check). No registered permission, so it cannot be delegated.", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/admin/delegation-adoption/previews", Kind: ExemptionHubAdmin, Reason: "Delegation-provenance adoption preview; read-only planning, hub system admin on a session or local development credential only.", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/admin/delegation-adoption/commits", Kind: ExemptionHubAdmin, Reason: "Delegation-provenance adoption commit (adopt or revert); hub system admin on a session or local development credential only, rechecked in the commit transaction; per-hop mutation audit. Operation contract deferred, as for the other hub-admin exemptions.", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/usage/me", Kind: ExemptionAuthenticationOnly, Reason: "Own usage statistics, self-service", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/experiments", Kind: ExemptionAuthenticationOnly, Reason: "Resolved experiments map for signed-in callers, no resource-level authorization", Owner: "route_metadata.go"},

	// Workstation endpoints — workstation token authentication
	{Pattern: "/api/v1/system/identity", Kind: ExemptionInternalOnly, Reason: "Workstation system endpoint, workstation-token auth", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/system/status", Kind: ExemptionInternalOnly, Reason: "Workstation system endpoint, workstation-token auth", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/system/check", Kind: ExemptionInternalOnly, Reason: "Workstation system endpoint, workstation-token auth", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/system/runtime", Kind: ExemptionInternalOnly, Reason: "Workstation system endpoint, workstation-token auth", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/system/init", Kind: ExemptionInternalOnly, Reason: "Workstation system endpoint, workstation-token auth", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/system/images/pull", Kind: ExemptionInternalOnly, Reason: "Workstation system endpoint, workstation-token auth", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/system/images/build", Kind: ExemptionInternalOnly, Reason: "Workstation system endpoint, workstation-token auth", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/system/apple-dns", Kind: ExemptionInternalOnly, Reason: "Workstation system endpoint, workstation-token auth", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/system/registry", Kind: ExemptionInternalOnly, Reason: "Workstation system endpoint, workstation-token auth", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/system/workstation-settings", Kind: ExemptionInternalOnly, Reason: "Workstation system endpoint, workstation-token auth", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/system/fs/list", Kind: ExemptionInternalOnly, Reason: "Workstation system endpoint, workstation-token auth", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/system/fs/mkdir", Kind: ExemptionInternalOnly, Reason: "Workstation system endpoint, workstation-token auth", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/system/fs/validate-path", Kind: ExemptionInternalOnly, Reason: "Workstation system endpoint, workstation-token auth", Owner: "route_metadata.go"},

	// Broker HMAC endpoints — broker authentication
	{Pattern: "/api/v1/brokers", Kind: ExemptionInternalOnly, Reason: "Broker registration, broker-HMAC auth", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/brokers/join", Kind: ExemptionInternalOnly, Reason: "Broker join, broker-HMAC auth", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/brokers/", Kind: ExemptionInternalOnly, Reason: "Broker by ID, broker-HMAC auth", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/broker/callback", Kind: ExemptionInternalOnly, Reason: "Broker callback, broker-HMAC auth", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/broker/inbound", Kind: ExemptionInternalOnly, Reason: "Broker message inbound, broker-HMAC auth; per-message authz via authorizeAgentMessage", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/broker/inbound/routed", Kind: ExemptionInternalOnly, Reason: "Broker routed message inbound, broker-HMAC auth; per-agent authorization via routing resolution", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/broker/projects", Kind: ExemptionInternalOnly, Reason: "Broker project list, broker-HMAC auth", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/runtime-brokers/connect", Kind: ExemptionInternalOnly, Reason: "Runtime broker WebSocket connect, broker-HMAC auth", Owner: "route_metadata.go"},

	// Agent token endpoints — agent JWT authentication
	// /api/v1/agent/gcp-token: cataloged as gcp.identity.mint (agent-JWT entry point).
	{Pattern: "/api/v1/agent/gcp-identity-token", Kind: ExemptionInternalOnly, Reason: "Agent GCP identity token, agent-JWT auth", Owner: "route_metadata.go"},
	{Pattern: "POST /api/v1/agent/identity-token", Kind: ExemptionInternalOnly, Reason: "Agent OIDC identity token, agent-JWT auth", Owner: "route_metadata.go"},
	{Pattern: "POST /api/v1/agent/secrets", Kind: ExemptionInternalOnly, Reason: "Agent secret fetch, agent-JWT auth", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/conduit", Kind: ExemptionInternalOnly, Reason: "Agent conduit session, agent-JWT auth (own agent row only), experiment-gated", Owner: "route_metadata.go"},

	// Webhook endpoints — signature verification
	{Pattern: "/api/v1/webhooks/github", Kind: ExemptionInternalOnly, Reason: "GitHub webhook, signature-verified", Owner: "route_metadata.go"},

	// GitHub App admin endpoints — fully converted to catalog operations
	// (hub.githubapp.read, hub.githubapp.update) with method-aware route guard
	// enforcement. Exemptions removed; see Catalog OperationSpec entries.

	// Access constraint preview endpoints — hub-admin, access_constraint.admin permission
	{Pattern: "/api/v1/admin/access-constraint-previews", Kind: ExemptionHubAdmin, Reason: "Access constraint previews, hub-admin with access_constraint.admin; PR #1445 B5 governance", Owner: "route_metadata.go"},
	{Pattern: "/api/v1/admin/access-constraint-previews/", Kind: ExemptionHubAdmin, Reason: "Access constraint preview by ID, hub-admin with access_constraint.admin; PR #1445 B5 governance", Owner: "route_metadata.go"},
	{Pattern: "GET /api/v1/admin/access-constraints/{id}/audit", Kind: ExemptionAuthenticationOnly, Reason: "Live access-constraint history, inline resource-scoped hub.audit.read check with privacy-preserving not-found denial", Owner: "handlers_access_constraints.go"},
	{Pattern: "/api/v1/admin/effective-access", Kind: ExemptionAuthenticationOnly, Reason: "Admin effective-access composition, inline hub.audit.read check", Owner: "route_metadata.go"},
}

// MutationClassifications maps every discovered security-relevant mutation
// call site to either a catalog operation or a reviewed exemption. This table
// is the R3 exit-gate artifact: every scanner-discovered site must appear
// here, and every entry here must still be discoverable by the scanner.
//
// Organized by source file for reviewability.
var MutationClassifications = []MutationClassification{
	// -----------------------------------------------------------------------
	// pkg/hub/project_membership_service.go — RS1 bounded domain service
	// Mutations moved from handlers to the service in RS1. Handlers now
	// delegate to the service and never directly mutate RoleBindings.
	// -----------------------------------------------------------------------
	{File: "pkg/hub/project_membership_service.go", Function: "AddMember", Symbol: "CreateRoleBinding", OperationID: "project.membership.add"},
	{File: "pkg/hub/project_membership_service.go", Function: "UpdateMemberRole", Symbol: "CreateRoleBinding", OperationID: "project.membership.update"},
	{File: "pkg/hub/project_membership_service.go", Function: "UpdateMemberRole", Symbol: "DeleteRoleBinding", OperationID: "project.membership.update"},
	{File: "pkg/hub/project_membership_service.go", Function: "RemoveMember", Symbol: "DeleteRoleBinding", OperationID: "project.membership.remove"},
	{File: "pkg/hub/project_membership_service.go", Function: "TransferOwnership", Symbol: "CreateRoleBinding", OperationID: "project.membership.transfer"},
	{File: "pkg/hub/project_membership_service.go", Function: "replaceBindingTx", Symbol: "CreateRoleBinding", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "RS1 one-binding invariant: atomic binding replacement used by AddMember/UpdateMemberRole/TransferOwnership; always called from a governed service method", Scope: "pkg/hub/project_membership_service.go"}},
	{File: "pkg/hub/project_membership_service.go", Function: "replaceBindingTx", Symbol: "DeleteRoleBinding", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "RS1 one-binding invariant: atomic binding replacement cleanup; always called from a governed service method", Scope: "pkg/hub/project_membership_service.go"}},
	{File: "pkg/hub/project_membership_service.go", Function: "MigrateMultiRoleBindings", Symbol: "DeleteRoleBinding", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "RS1 R-3 pre-constraint migration: removes duplicate bindings keeping highest authority; idempotent, admin-only, runs within transaction", Scope: "pkg/hub/project_membership_service.go"}},
	// ptone/scion#2529 P1: applyRolePlanTx is the purpose-named delete-then-
	// create step for project_membership_set.go's SetMemberRoles (the atomic
	// "set roles for principal" engine backing PUT/DELETE
	// .../members/principals/{type}/{id}) — the only place that engine calls
	// tx.CreateRoleBinding/tx.DeleteRoleBinding, keeping every direct
	// role-binding mutation call enumerable in this one file per RS1 O-3
	// (rs1_extended_test.go TestRS1_AST_BypassPathsDocumented). SetMemberRoles
	// runs the credential gate and CanDelegate pre-transaction, and
	// re-evaluates governance / custom-role authority (including the F1
	// role_binding.* structural guard) and the actor-authority-change check
	// under the project lock before calling applyRolePlanTx; the last-owner
	// guard runs on the post-state afterwards in the same transaction — the
	// same way AddMember/UpdateMemberRole/TransferOwnership govern
	// replaceBindingTx above. Review r1 F3: this replaces the earlier
	// generic txCreateRoleBinding/txDeleteRoleBinding forwarders, which were
	// reusable primitives that left the governed call site invisible to this
	// catalog; applyRolePlanTx is a single-purpose step, so this entry
	// covers exactly what it does. A dedicated OperationID (e.g.
	// project.membership.set) is deferred: wiring one requires a
	// route_metadata.go entry, which is out of scope for P1.
	{File: "pkg/hub/project_membership_service.go", Function: "applyRolePlanTx", Symbol: "CreateRoleBinding", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "governed delete-then-create step for SetMemberRoles (project_membership_set.go): credential gate and CanDelegate run pre-transaction; governance/custom-role authority, the role_binding.* guard and an actor-authority-change check are re-evaluated under the project lock inside WithTx before this call; the last-owner guard is enforced on the post-state in the same transaction and a violation rolls back every mutation", Scope: "pkg/hub/project_membership_service.go"}},
	{File: "pkg/hub/project_membership_service.go", Function: "applyRolePlanTx", Symbol: "DeleteRoleBinding", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "governed delete-then-create step for SetMemberRoles (project_membership_set.go): credential gate and CanDelegate run pre-transaction; governance/custom-role authority, the role_binding.* guard and an actor-authority-change check are re-evaluated under the project lock inside WithTx before this call; the last-owner guard is enforced on the post-state in the same transaction and a violation rolls back every mutation", Scope: "pkg/hub/project_membership_service.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/handlers_roles.go — role/binding CRUD
	// -----------------------------------------------------------------------
	{File: "pkg/hub/handlers_roles.go", Function: "createRoleBinding", Symbol: "CreateRoleBinding", OperationID: "role.binding.create"},
	{File: "pkg/hub/handlers_roles.go", Function: "deleteRoleBinding", Symbol: "DeleteRoleBinding", OperationID: "role.binding.delete"},
	{File: "pkg/hub/handlers_roles.go", Function: "createRoleDefinition", Symbol: "CreateRoleDefinition", OperationID: "role.definition.create"},
	{File: "pkg/hub/handlers_roles.go", Function: "importRoleDefinitions", Symbol: "CreateRoleDefinition", OperationID: "role.definition.create"},
	{File: "pkg/hub/handlers_roles.go", Function: "duplicateRoleDefinition", Symbol: "CreateRoleDefinition", OperationID: "role.definition.create"},
	{File: "pkg/hub/handlers_roles.go", Function: "updateRoleDefinition", Symbol: "UpdateRoleDefinition", OperationID: "role.definition.update"},
	{File: "pkg/hub/handlers_roles.go", Function: "deleteRoleDefinition", Symbol: "DeleteRoleDefinition", OperationID: "role.definition.delete"},

	// -----------------------------------------------------------------------
	// pkg/hub/access_constraint_governance*.go — B5 transactional governance
	// PR #1445 moved store mutations from handlers to the governance layer.
	// Audited creates run in the dedicated transactional helper; updates,
	// deletes, compensations, and role-binding changes remain in the core file.
	// -----------------------------------------------------------------------
	{File: "pkg/hub/access_constraint_governance_auditevent.go", Function: "createAccessConstraintWithAudit", Symbol: "CreateAccessConstraint", OperationID: "access.constraint.create"},
	{File: "pkg/hub/access_constraint_governance.go", Function: "CommitBoundaryChange", Symbol: "UpdateAccessConstraint", OperationID: "access.constraint.update"},
	{File: "pkg/hub/access_constraint_governance.go", Function: "CommitBoundaryChange", Symbol: "DeleteAccessConstraint", OperationID: "access.constraint.delete"},
	{File: "pkg/hub/access_constraint_governance.go", Function: "compensateAuditFailure", Symbol: "CreateAccessConstraint", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Governance compensating action: restores constraint after audit failure", Scope: "pkg/hub/access_constraint_governance.go"}},
	{File: "pkg/hub/access_constraint_governance.go", Function: "compensateAuditFailure", Symbol: "UpdateAccessConstraint", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Governance compensating action: restores constraint after audit failure", Scope: "pkg/hub/access_constraint_governance.go"}},
	{File: "pkg/hub/access_constraint_governance.go", Function: "compensateAuditFailure", Symbol: "DeleteAccessConstraint", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Governance compensating action: restores constraint after audit failure", Scope: "pkg/hub/access_constraint_governance.go"}},
	{File: "pkg/hub/access_constraint_governance.go", Function: "ReplaceRoleBinding", Symbol: "CreateRoleBinding", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "B5 §5 atomic role binding replacement; exported GovernanceService method reachable only from access_constraint.admin-guarded handler paths; enforces lockout invariant independently on admin-role downgrades", Scope: "pkg/hub/access_constraint_governance.go"}},
	{File: "pkg/hub/access_constraint_governance.go", Function: "ReplaceRoleBinding", Symbol: "DeleteRoleBinding", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "B5 §5 atomic role binding replacement; exported GovernanceService method reachable only from access_constraint.admin-guarded handler paths; enforces lockout invariant independently on admin-role downgrades", Scope: "pkg/hub/access_constraint_governance.go"}},
	{File: "pkg/hub/access_constraint_governance.go", Function: "ReplaceRoleBinding", Symbol: "DeleteRoleBinding", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "B5 §5 atomic role binding replacement compensating delete: rolls back newly created binding when old-binding deletion fails, preventing dual-active-binding inconsistency", Scope: "pkg/hub/access_constraint_governance.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/access_constraint_recovery.go — constraint recovery operations
	// -----------------------------------------------------------------------
	{File: "pkg/hub/access_constraint_recovery.go", Function: "RecoverAll", Symbol: "UpdateAccessConstraint", Exemption: &MutationExemption{Kind: ExemptionHubAdmin, Reason: "Constraint recovery: re-enables constraints disabled by audit failure", Scope: "pkg/hub/access_constraint_recovery.go"}},
	{File: "pkg/hub/access_constraint_recovery.go", Function: "rollbackDisable", Symbol: "UpdateAccessConstraint", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Constraint recovery rollback: restores constraint on disable failure", Scope: "pkg/hub/access_constraint_recovery.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/handlers_groups.go — group CRUD and membership
	// -----------------------------------------------------------------------
	{File: "pkg/hub/handlers_groups.go", Function: "createGroup", Symbol: "CreateGroup", OperationID: "group.create"},
	{File: "pkg/hub/handlers_groups.go", Function: "createGroup", Symbol: "AddGroupMember", OperationID: "group.create"},
	{File: "pkg/hub/handlers_groups.go", Function: "updateGroup", Symbol: "UpdateGroup", OperationID: "group.update"},
	{File: "pkg/hub/handlers_groups.go", Function: "deleteGroup", Symbol: "DeleteGroup", OperationID: "group.delete"},
	{File: "pkg/hub/handlers_groups.go", Function: "addGroupMember", Symbol: "AddGroupMember", OperationID: "group.member.add"},
	{File: "pkg/hub/handlers_groups.go", Function: "removeGroupMember", Symbol: "RemoveGroupMember", OperationID: "group.member.remove"},

	// -----------------------------------------------------------------------
	// pkg/hub/handlers_gcp_identity.go — GCP service account operations
	// -----------------------------------------------------------------------
	{File: "pkg/hub/handlers_gcp_identity.go", Function: "createGCPServiceAccount", Symbol: "CreateGCPServiceAccount", OperationID: "gcp.identity.create"},
	{File: "pkg/hub/handlers_gcp_identity.go", Function: "deleteGCPServiceAccount", Symbol: "DeleteGCPServiceAccount", OperationID: "gcp.identity.delete"},
	{File: "pkg/hub/handlers_gcp_identity.go", Function: "handleAgentGCPToken", Symbol: "GenerateAccessToken", OperationID: "gcp.identity.mint"},
	{File: "pkg/hub/handlers_gcp_identity.go", Function: "mintGCPServiceAccount", Symbol: "CreateGCPServiceAccount", OperationID: "gcp.identity.create"},
	{File: "pkg/hub/handlers_gcp_identity.go", Function: "mintGCPServiceAccount", Symbol: "CreateServiceAccount", OperationID: "gcp.identity.create"},
	{File: "pkg/hub/handlers_gcp_identity.go", Function: "mintGCPServiceAccount", Symbol: "DeleteServiceAccount", OperationID: "gcp.identity.create"},
	{File: "pkg/hub/handlers_gcp_identity.go", Function: "mintGCPServiceAccount", Symbol: "DeleteServiceAccount", OperationID: "gcp.identity.create"},
	{File: "pkg/hub/handlers_gcp_identity.go", Function: "mintGCPServiceAccount", Symbol: "DeleteServiceAccount", OperationID: "gcp.identity.create"},
	{File: "pkg/hub/handlers_gcp_identity.go", Function: "mintGCPServiceAccount", Symbol: "SetIAMPolicy", OperationID: "gcp.identity.create"},
	{File: "pkg/hub/handlers_gcp_identity.go", Function: "mintGCPServiceAccount", Symbol: "SetIAMPolicy", OperationID: "gcp.identity.create"},
	// applyGCPVerificationResult persists every verification outcome: the verify
	// routes (gcp.identity.verify) and the auto-verify step of both create
	// handlers (gcp.identity.create) reach it after their own authorization.
	{File: "pkg/hub/handlers_gcp_identity.go", Function: "applyGCPVerificationResult", Symbol: "UpdateGCPServiceAccount", OperationID: "gcp.identity.verify"},

	// -----------------------------------------------------------------------
	// pkg/hub/handlers_gcp_identity_scoped.go — hub-scoped GCP identity
	// -----------------------------------------------------------------------
	{File: "pkg/hub/handlers_gcp_identity_scoped.go", Function: "createHubScopedGCPServiceAccount", Symbol: "CreateGCPServiceAccount", OperationID: "gcp.identity.create"},
	{File: "pkg/hub/handlers_gcp_identity_scoped.go", Function: "deleteGCPServiceAccountByID", Symbol: "DeleteGCPServiceAccount", OperationID: "gcp.identity.delete"},
	{File: "pkg/hub/handlers_gcp_identity_scoped.go", Function: "mintHubScopedGCPServiceAccount", Symbol: "SetIAMPolicy", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Hub-scope GCP SA mint: sets IAM policy on new service account", Scope: "pkg/hub/handlers_gcp_identity_scoped.go"}},
	{File: "pkg/hub/handlers_gcp_identity_scoped.go", Function: "mintHubScopedGCPServiceAccount", Symbol: "SetIAMPolicy", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Hub-scope GCP SA mint: sets IAM policy on new service account", Scope: "pkg/hub/handlers_gcp_identity_scoped.go"}},
	{File: "pkg/hub/handlers_gcp_identity_scoped.go", Function: "mintHubScopedGCPServiceAccount", Symbol: "CreateGCPServiceAccount", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Hub-scope GCP SA mint: creates GCP service account record", Scope: "pkg/hub/handlers_gcp_identity_scoped.go"}},
	{File: "pkg/hub/handlers_gcp_identity_scoped.go", Function: "mintHubScopedGCPServiceAccount", Symbol: "CreateServiceAccount", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Hub-scope GCP SA mint: creates IAM service account via GCP API", Scope: "pkg/hub/handlers_gcp_identity_scoped.go"}},
	{File: "pkg/hub/handlers_gcp_identity_scoped.go", Function: "mintHubScopedGCPServiceAccount", Symbol: "DeleteServiceAccount", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Hub-scope GCP SA mint: rollback cleanup on failure", Scope: "pkg/hub/handlers_gcp_identity_scoped.go"}},
	{File: "pkg/hub/handlers_gcp_identity_scoped.go", Function: "mintHubScopedGCPServiceAccount", Symbol: "DeleteServiceAccount", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Hub-scope GCP SA mint: rollback cleanup on failure", Scope: "pkg/hub/handlers_gcp_identity_scoped.go"}},
	{File: "pkg/hub/handlers_gcp_identity_scoped.go", Function: "mintHubScopedGCPServiceAccount", Symbol: "DeleteServiceAccount", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Hub-scope GCP SA mint: rollback cleanup on failure", Scope: "pkg/hub/handlers_gcp_identity_scoped.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/useraccesstoken.go — user access token CRUD
	// -----------------------------------------------------------------------
	{File: "pkg/hub/useraccesstoken.go", Function: "CreateTokenWithParams", Symbol: "CreateUserAccessToken", OperationID: "credential.token.create"},
	{File: "pkg/hub/useraccesstoken.go", Function: "RevokeToken", Symbol: "RevokeUserAccessToken", OperationID: "credential.token.revoke"},
	{File: "pkg/hub/useraccesstoken.go", Function: "DeleteToken", Symbol: "DeleteUserAccessToken", OperationID: "credential.token.revoke"},

	// -----------------------------------------------------------------------
	// pkg/hub/handlers_users_core.go — user management
	// -----------------------------------------------------------------------
	{File: "pkg/hub/handlers_users_core.go", Function: "deleteUser", Symbol: "DeleteUser", OperationID: "user.admin.delete"},
	{File: "pkg/hub/handlers_users_core.go", Function: "deleteUser", Symbol: "DeleteGroupMembershipsForUser", OperationID: "user.admin.delete"},
	{File: "pkg/hub/handlers_users_core.go", Function: "guardAndCascadeUserRoleBindingsTx", Symbol: "DeleteRoleBindingsForPrincipal", OperationID: "user.admin.delete"},
	{File: "pkg/hub/handlers_users_core.go", Function: "guardAndCascadeUserRoleBindingsTx", Symbol: "DeleteRoleBinding", OperationID: "user.admin.delete"},
	{File: "pkg/hub/handlers_users_core.go", Function: "updateUser", Symbol: "UpdateUser", OperationID: "user.update"},
	// pkg/hub/user_delete_data.go — deleted user's user-scope secrets when no secret backend is configured (ptone/scion#2769)
	{File: "pkg/hub/user_delete_data.go", Function: "removeUserScopedSecretRowsWithoutBackend", Symbol: "GetSecretValue", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Reads a deleted user's user-scope secret row only to tell a value stored in the hub database from an external reference; never returned; runs after user.admin.delete or the allow-list delete commits, or from the startup sweep for users that no longer exist", Scope: "pkg/hub/user_delete_data.go"}},
	{File: "pkg/hub/user_delete_data.go", Function: "removeUserScopedSecretRowsWithoutBackend", Symbol: "DeleteSecret", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Deletes a deleted user's user-scope secret rows whose value is stored in the hub database when no secret backend is configured; runs after user.admin.delete or the allow-list delete commits, or from the startup sweep for users that no longer exist", Scope: "pkg/hub/user_delete_data.go"}},
	{File: "pkg/hub/handlers_users_core.go", Function: "createSuperAdminBindingTx", Symbol: "CreateRoleBinding", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Super-admin binding creation inside single atomic WithTx in updateUser; caller checks user.promote + CanDelegate; uses SystemReconcileCreatedBy sentinel", Scope: "pkg/hub/handlers_users_core.go"}},
	{File: "pkg/hub/handlers_users_core.go", Function: "deleteSuperAdminBindingTx", Symbol: "DeleteRoleBinding", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Super-admin binding deletion inside single atomic WithTx in updateUser; caller checks user.promote + CanDelegate from canonical binding state; guarded by checkLastSuperAdminTx with serialization lock, self-lockout re-check, and full error propagation (R4-fix)", Scope: "pkg/hub/handlers_users_core.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/handlers_roles.go — generic role-binding delete with super-admin guard (R6)
	// -----------------------------------------------------------------------
	{File: "pkg/hub/handlers_roles.go", Function: "deleteSystemSuperAdminBinding", Symbol: "DeleteRoleBinding", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Super-admin binding deletion via generic DELETE endpoint inside atomic WithTx; guarded by CanDelegate, self-lockout, checkLastSuperAdminTx with serialization lock, and transactional audit (R6)", Scope: "pkg/hub/handlers_roles.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/handlers_agents_core.go — agent lifecycle
	// -----------------------------------------------------------------------
	{File: "pkg/hub/handlers_agents_core.go", Function: "deleteFailedCreateRow", Symbol: "DeleteAgent", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Agent create rollback, deletes on creation failure", Scope: "pkg/hub/handlers_agents_core.go"}},
	{File: "pkg/hub/handlers_agents_core.go", Function: "handleAgentTokenRefresh", Symbol: "RevokeAgentCredential", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Agent token refresh, agent-JWT auth; old credential revoked on refresh", Scope: "pkg/hub/handlers_agents_core.go"}},
	{File: "pkg/hub/handlers_agents_core.go", Function: "ensureHostSARecord", Symbol: "CreateGCPServiceAccount", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Host SA record creation during agent assignment, broker-HMAC authenticated", Scope: "pkg/hub/handlers_agents_core.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/agent_create_tx.go — agent create rollback
	// -----------------------------------------------------------------------
	{File: "pkg/hub/agent_create_tx.go", Function: "compensateAgentCreate", Symbol: "DeleteAgent", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Agent create rollback transaction, deletes the agent row on creation failure", Scope: "pkg/hub/agent_create_tx.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/agent_delete_engine.go — agent delete engine (ptone/scion#2483)
	// -----------------------------------------------------------------------
	{File: "pkg/hub/agent_delete_engine.go", Function: "finalizeAgentDeletion", Symbol: "FinalizeAgentDeletion", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Delete engine's terminal soft or hard delete. The engine only runs under a claim taken by performAgentDelete, which authorizes agent.delete on the target agent (authorizeAgentTargetAction) for both the agent and the project-scoped DELETE routes; the write is CAS-guarded by that claim", Scope: "pkg/hub/agent_delete_engine.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/handlers_agent_create_helpers.go
	// -----------------------------------------------------------------------
	{File: "pkg/hub/handlers_agent_create_helpers.go", Function: "handleExistingAgent", Symbol: "DeleteAgent", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Existing agent cleanup during create, route-guarded by agent.create path", Scope: "pkg/hub/handlers_agent_create_helpers.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/handlers_agent_lifecycle.go
	// -----------------------------------------------------------------------
	{File: "pkg/hub/handlers_agent_lifecycle.go", Function: "revokeSuspendedCredentials", Symbol: "RevokeAgentCredentialsByAgent", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Agent suspend revokes credentials (reached only from suspendAgent), route-guarded by agent.update permission", Scope: "pkg/hub/handlers_agent_lifecycle.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/agent_credential_revoke.go — shared best-effort revoke helper
	// -----------------------------------------------------------------------
	{File: "pkg/hub/membership_loss.go", Function: "holdAgentsTx", Symbol: "RevokeAgentCredentialsByAgent", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Hub-internal membership loss processor: revokes the credentials of agents it holds after their root user's project access ended, in the same transaction as the hold; reached only from the outbox processor and reconciler, never from a request handler", Scope: "pkg/hub/membership_loss.go"}},
	{File: "pkg/hub/membership_loss.go", Function: "membershipRestoreHook", Symbol: "RevokeAgentCredentialsByAgent", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Restore transaction hook: revokes the credentials of a restored agent it holds because its root user is not admitted to the project; runs inside the already route-guarded restore transaction", Scope: "pkg/hub/membership_loss.go"}},
	{File: "pkg/hub/agent_credential_revoke.go", Function: "revokeAgentCredentials", Symbol: "RevokeAgentCredentialsByAgent", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Shared revoke helper (reached via revokeAgentCredentialsBestEffort) called from create/launch dispatch and handler cleanup paths that are themselves already route-guarded, and from the broker-HMAC-authenticated launch report endpoint; mirrors the existing delete and suspend revoke exemptions", Scope: "pkg/hub/agent_credential_revoke.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/handlers_projects_core.go — project lifecycle
	// -----------------------------------------------------------------------
	{File: "pkg/hub/handlers_projects_core.go", Function: "createProject", Symbol: "DeleteProject", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Project create rollback, deletes on creation failure", Scope: "pkg/hub/handlers_projects_core.go"}},
	{File: "pkg/hub/handlers_projects_core.go", Function: "createProject", Symbol: "DeleteProject", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Project create rollback, deletes on creation failure", Scope: "pkg/hub/handlers_projects_core.go"}},
	{File: "pkg/hub/handlers_projects_core.go", Function: "createProject", Symbol: "DeleteProject", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Project create rollback, deletes on creation failure", Scope: "pkg/hub/handlers_projects_core.go"}},
	{File: "pkg/hub/handlers_projects_core.go", Function: "createProject", Symbol: "DeleteRoleBindingsForScope", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Project create rollback, cleans up bindings on failure", Scope: "pkg/hub/handlers_projects_core.go"}},
	{File: "pkg/hub/handlers_projects_core.go", Function: "createProject", Symbol: "DeleteRoleBindingsForScope", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Project create rollback, cleans up bindings on failure", Scope: "pkg/hub/handlers_projects_core.go"}},
	{File: "pkg/hub/handlers_projects_core.go", Function: "createProjectGroup", Symbol: "CreateGroup", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Project create sub-step: creates project groups", Scope: "pkg/hub/handlers_projects_core.go"}},
	{File: "pkg/hub/handlers_projects_core.go", Function: "createProjectMembersGroup", Symbol: "AddGroupMember", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Project create sub-step: adds creator to members group", Scope: "pkg/hub/handlers_projects_core.go"}},
	{File: "pkg/hub/handlers_projects_core.go", Function: "createProjectMembersGroup", Symbol: "CreateGroup", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Project create sub-step: creates members group", Scope: "pkg/hub/handlers_projects_core.go"}},
	{File: "pkg/hub/handlers_projects_core.go", Function: "createProjectOwnerRoleBinding", Symbol: "CreateRoleBinding", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Project create sub-step: creates owner role binding", Scope: "pkg/hub/handlers_projects_core.go"}},
	// RS3: deleteProject handler now delegates to ProjectDeletionService.
	// Cascade mutations are in the service's cascadeSecurityState method.
	{File: "pkg/hub/project_deletion_service.go", Function: "cascadeSecurityState", Symbol: "DeleteRoleBindingsForScope", OperationID: "project.lifecycle.delete"},
	{File: "pkg/hub/project_deletion_service.go", Function: "cascadeSecurityState", Symbol: "DeleteGroup", OperationID: "project.lifecycle.delete"},
	{File: "pkg/hub/project_deletion_service.go", Function: "cascadeSecurityState", Symbol: "DeleteSecretsByScope", OperationID: "project.lifecycle.delete"},
	{File: "pkg/hub/project_deletion_service.go", Function: "cascadeSecurityState", Symbol: "DeleteGCPServiceAccount", OperationID: "project.lifecycle.delete"},
	{File: "pkg/hub/project_deletion_service.go", Function: "Delete", Symbol: "DeleteProject", OperationID: "project.lifecycle.delete"},
	{File: "pkg/hub/handlers_projects_core.go", Function: "handleProjectRegister", Symbol: "DeleteProject", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Project register rollback, deletes on failure", Scope: "pkg/hub/handlers_projects_core.go"}},
	{File: "pkg/hub/handlers_projects_core.go", Function: "migrateProjectSlug", Symbol: "UpdateGroup", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Project slug migration, updates group names", Scope: "pkg/hub/handlers_projects_core.go"}},
	{File: "pkg/hub/handlers_projects_core.go", Function: "migrateProjectSlug", Symbol: "UpdateGroup", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Project slug migration, updates group names", Scope: "pkg/hub/handlers_projects_core.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/project_clone.go
	// -----------------------------------------------------------------------
	{File: "pkg/hub/project_clone.go", Function: "handleProjectClone", Symbol: "DeleteProject", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Project clone rollback, deletes on failure", Scope: "pkg/hub/project_clone.go"}},
	{File: "pkg/hub/project_clone.go", Function: "handleProjectClone", Symbol: "DeleteRoleBindingsForScope", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Project clone rollback, cleans up bindings on failure", Scope: "pkg/hub/project_clone.go"}},
	{File: "pkg/hub/project_clone.go", Function: "cloneProjectGCPServiceAccounts", Symbol: "CreateGCPServiceAccount", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Project clone sub-step: clones GCP service account associations to target project", Scope: "pkg/hub/project_clone.go"}},
	{File: "pkg/hub/project_clone.go", Function: "cloneProjectGCPServiceAccounts", Symbol: "DeleteGCPServiceAccount", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Project clone rollback, deletes cloned GCP service accounts on failure", Scope: "pkg/hub/project_clone.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/handlers_auth.go — auth flow user provisioning
	// -----------------------------------------------------------------------
	{File: "pkg/hub/handlers_auth.go", Function: "provisionUser", Symbol: "CreateUser", Exemption: &MutationExemption{Kind: ExemptionAuthenticationOnly, Reason: "User provisioning during auth login flow, pre-authorization", Scope: "pkg/hub/handlers_auth.go"}},
	{File: "pkg/hub/sign_in_policy.go", Function: "applyLiveSignInPolicy", Symbol: "UpdateUser", Exemption: &MutationExemption{Kind: ExemptionAuthenticationOnly, Reason: "User record update during sign-in; shared by every sign-in path (web login, GE exchange, external bearer) via provisionUser and the Google identity resolver", Scope: "pkg/hub/sign_in_policy.go"}},
	{File: "pkg/hub/handlers_auth.go", Function: "ensureSuperAdminRoleBinding", Symbol: "CreateRoleBinding", Exemption: &MutationExemption{Kind: ExemptionAuthenticationOnly, Reason: "Idempotent super-admin binding during authorized user provisioning", Scope: "pkg/hub/handlers_auth.go"}},
	{File: "pkg/hub/handlers_auth.go", Function: "handleAuthRefresh", Symbol: "UpdateUser", Exemption: &MutationExemption{Kind: ExemptionAuthenticationOnly, Reason: "User last-login update during token refresh", Scope: "pkg/hub/handlers_auth.go"}},
	{File: "pkg/hub/handlers_auth.go", Function: "deleteSuperAdminRoleBinding", Symbol: "DeleteRoleBinding", Exemption: &MutationExemption{Kind: ExemptionHubAdmin, Reason: "Super-admin self-demotion, hub-admin operation", Scope: "pkg/hub/handlers_auth.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/google_identity_resolver.go — shared Google identity resolution,
	// used by both the GE credential exchange (ge_exchange.go) and the
	// external-bearer auth path (auth_external_bearer.go). Extracted from
	// ge_exchange.go's former resolveLocalUser/provisionNewUser.
	// -----------------------------------------------------------------------
	{File: "pkg/hub/google_identity_resolver.go", Function: "Resolve", Symbol: "DeleteUser", Exemption: &MutationExemption{Kind: ExemptionAuthenticationOnly, Reason: "Google identity resolution: orphan user cleanup after concurrent binding race", Scope: "pkg/hub/google_identity_resolver.go"}},
	{File: "pkg/hub/google_identity_resolver.go", Function: "provisionNewUser", Symbol: "CreateUser", Exemption: &MutationExemption{Kind: ExemptionAuthenticationOnly, Reason: "Google identity resolution: new user provisioning (GE exchange and external-bearer)", Scope: "pkg/hub/google_identity_resolver.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/web.go — OAuth/session middleware
	// -----------------------------------------------------------------------
	{File: "pkg/hub/web.go", Function: "handleOAuthCallback", Symbol: "CreateUser", Exemption: &MutationExemption{Kind: ExemptionAuthenticationOnly, Reason: "OAuth callback user provisioning", Scope: "pkg/hub/web.go"}},
	{File: "pkg/hub/web.go", Function: "handleOAuthCallback", Symbol: "UpdateUser", Exemption: &MutationExemption{Kind: ExemptionAuthenticationOnly, Reason: "OAuth callback user record update", Scope: "pkg/hub/web.go"}},
	{File: "pkg/hub/web.go", Function: "proxyAuthMiddleware", Symbol: "CreateUser", Exemption: &MutationExemption{Kind: ExemptionAuthenticationOnly, Reason: "Proxy auth user provisioning", Scope: "pkg/hub/web.go"}},
	{File: "pkg/hub/web.go", Function: "proxyAuthMiddleware", Symbol: "UpdateUser", Exemption: &MutationExemption{Kind: ExemptionAuthenticationOnly, Reason: "Proxy auth user record update", Scope: "pkg/hub/web.go"}},
	{File: "pkg/hub/web.go", Function: "sessionToBearerMiddleware", Symbol: "GenerateAccessToken", Exemption: &MutationExemption{Kind: ExemptionAuthenticationOnly, Reason: "Session-to-bearer token conversion middleware", Scope: "pkg/hub/web.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/handlers_test_login.go — dev/test login
	// -----------------------------------------------------------------------
	{File: "pkg/hub/handlers_test_login.go", Function: "handleTestLogin", Symbol: "CreateUser", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Development-only test login endpoint", Scope: "pkg/hub/handlers_test_login.go"}},
	{File: "pkg/hub/handlers_test_login.go", Function: "handleTestLogin", Symbol: "UpdateUser", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Development-only test login endpoint", Scope: "pkg/hub/handlers_test_login.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/admin_allow_list.go — hub admin: allow-list management
	// -----------------------------------------------------------------------
	{File: "pkg/hub/admin_allow_list.go", Function: "handleAdminAllowListAdd", Symbol: "CreateUser", Exemption: &MutationExemption{Kind: ExemptionHubAdmin, Reason: "Admin allow-list add, hub-admin operation", Scope: "pkg/hub/admin_allow_list.go"}},
	{File: "pkg/hub/admin_allow_list.go", Function: "handleAdminAllowListByEmail", Symbol: "DeleteGroupMembershipsForUser", Exemption: &MutationExemption{Kind: ExemptionHubAdmin, Reason: "Admin allow-list remove, hub-admin operation; removes the invited user's group memberships in the same WithTx, before DeleteUser (ON DELETE SET NULL would otherwise orphan them; ptone/scion#2769)", Scope: "pkg/hub/admin_allow_list.go"}},
	{File: "pkg/hub/admin_allow_list.go", Function: "handleAdminAllowListByEmail", Symbol: "DeleteUser", Exemption: &MutationExemption{Kind: ExemptionHubAdmin, Reason: "Admin allow-list remove, hub-admin operation; runs inside WithTx with the same last-project-owner guard and role-binding cascade as user.admin.delete (guardAndCascadeUserRoleBindingsTx) and owned-agents check (checkUserOwnsNoAgentsTx)", Scope: "pkg/hub/admin_allow_list.go"}},
	{File: "pkg/hub/admin_allow_list.go", Function: "handleAdminAllowListImport", Symbol: "CreateUser", Exemption: &MutationExemption{Kind: ExemptionHubAdmin, Reason: "Admin allow-list import, hub-admin operation", Scope: "pkg/hub/admin_allow_list.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/admin_invites.go — hub admin: invite management
	// -----------------------------------------------------------------------
	{File: "pkg/hub/admin_invites.go", Function: "handleAdminInviteDelete", Symbol: "DeleteInviteCode", OperationID: "user.admin.invite"},
	{File: "pkg/hub/admin_invites.go", Function: "handleAdminInviteRevoke", Symbol: "RevokeInviteCode", OperationID: "user.admin.invite"},

	// -----------------------------------------------------------------------
	// pkg/hub/admin_user_invite.go — hub admin: user invite
	// -----------------------------------------------------------------------
	{File: "pkg/hub/admin_user_invite.go", Function: "handleAdminUserInvite", Symbol: "CreateUser", OperationID: "user.admin.invite"},
	{File: "pkg/hub/admin_user_invite.go", Function: "handleAdminUserInviteBulk", Symbol: "CreateUser", OperationID: "user.admin.invite"},

	// -----------------------------------------------------------------------
	// pkg/hub/handlers_chat_secrets.go — chat integration secrets
	// -----------------------------------------------------------------------
	{File: "pkg/hub/handlers_chat_secrets.go", Function: "HasChatIntegrationSecret", Symbol: "GetSecretValue", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Chat integration secret check, route-guarded by hub admin", Scope: "pkg/hub/handlers_chat_secrets.go"}},
	{File: "pkg/hub/handlers_chat_secrets.go", Function: "LoadChatIntegrationSecret", Symbol: "GetSecretValue", Exemption: &MutationExemption{Kind: ExemptionRouteGuarded, Reason: "Chat integration secret load, route-guarded by hub admin", Scope: "pkg/hub/handlers_chat_secrets.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/handlers_github_app.go — GitHub App admin
	// -----------------------------------------------------------------------
	{File: "pkg/hub/handlers_github_app.go", Function: "loadGitHubAppSecret", Symbol: "GetSecretValue", Exemption: &MutationExemption{Kind: ExemptionHubAdmin, Reason: "GitHub App secret read, hub-admin operation", Scope: "pkg/hub/handlers_github_app.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/hub_secrets.go — shared Hub-scoped secret persistence
	// -----------------------------------------------------------------------
	{File: "pkg/hub/hub_secrets.go", Function: "setHubSecret", Symbol: "UpsertSecret", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Shared Hub-scoped secret persistence, called only from route-guarded chat integration and GitHub App admin paths", Scope: "pkg/hub/hub_secrets.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/invite_service.go — invite code creation
	// -----------------------------------------------------------------------
	{File: "pkg/hub/invite_service.go", Function: "CreateInvite", Symbol: "CreateInviteCode", OperationID: "user.admin.invite"},

	// -----------------------------------------------------------------------
	// pkg/hub/seed.go — server startup seed/reconciliation
	// -----------------------------------------------------------------------
	{File: "pkg/hub/seed.go", Function: "ReconcileSuperAdminBindings", Symbol: "CreateRoleBinding", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Server startup: reconcile super-admin role bindings", Scope: "pkg/hub/seed.go"}},
	{File: "pkg/hub/seed.go", Function: "ReconcileSuperAdminBindings", Symbol: "DeleteRoleBinding", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Server startup: reconcile super-admin role bindings", Scope: "pkg/hub/seed.go"}},
	{File: "pkg/hub/seed.go", Function: "ReconcileSuperAdminBindings", Symbol: "UpdateUser", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Server startup: promote/demote super-admin users", Scope: "pkg/hub/seed.go"}},
	{File: "pkg/hub/seed.go", Function: "ReconcileSuperAdminBindings", Symbol: "UpdateUser", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Server startup: promote/demote super-admin users", Scope: "pkg/hub/seed.go"}},
	{File: "pkg/hub/seed.go", Function: "clearProjectMembersGroupOwners", Symbol: "UpdateGroup", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Server startup: clear legacy Group.OwnerID on project members groups (ptone/scion#2599); only ever removes an owner, never grants", Scope: "pkg/hub/seed.go"}},
	{File: "pkg/hub/seed.go", Function: "backfillProjectOwnerRoleBindings", Symbol: "CreateRoleBinding", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Server startup: backfill project owner role bindings", Scope: "pkg/hub/seed.go"}},
	{File: "pkg/hub/seed.go", Function: "backfillSuperAdminBinding", Symbol: "CreateRoleBinding", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Server startup: backfill super-admin role binding for admin users (hub-members/hub-viewer grants go through syncHubRoleGrants)", Scope: "pkg/hub/seed.go"}},
	{File: "pkg/hub/seed.go", Function: "CleanupRedundantHubMemberBindings", Symbol: "DeleteRoleBinding", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Server startup: transactional removal of redundant unconditional system-created direct hub-member bindings; verified active canonical group binding before any delete; fail-closed on missing group binding or delete error", Scope: "pkg/hub/seed.go"}},
	{File: "pkg/hub/seed.go", Function: "ensureDevUserRoleBinding", Symbol: "CreateRoleBinding", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Server startup: ensure dev user role binding", Scope: "pkg/hub/seed.go"}},
	{File: "pkg/hub/seed.go", Function: "deleteHubViewerBindingsTx", Symbol: "DeleteRoleBinding", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Hub role grant reconciliation (syncHubRoleGrants): hub-viewer binding removal for non-viewer roles; caller authorizes (user.promote on PATCH inside single atomic WithTx, authentication on login, server startup on backfill)", Scope: "pkg/hub/seed.go"}},
	{File: "pkg/hub/seed.go", Function: "ensureHubMembershipTx", Symbol: "AddGroupMember", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Canonical hub-members group membership grant (single implementation); used by syncHubRoleGrants (PATCH role inside WithTx, login paths, startup backfill); caller authorizes", Scope: "pkg/hub/seed.go"}},
	{File: "pkg/hub/seed.go", Function: "ensureHubViewerBindingTx", Symbol: "CreateRoleBinding", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Hub role grant reconciliation (syncHubRoleGrants): hub-viewer binding creation for viewers; caller authorizes (user.promote on PATCH inside single atomic WithTx, authentication on login, server startup on backfill)", Scope: "pkg/hub/seed.go"}},
	{File: "pkg/hub/seed.go", Function: "ensureHubViewerBindingTx", Symbol: "DeleteRoleBinding", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Hub role grant reconciliation (syncHubRoleGrants): replacement of time-limited (expired, scheduled or expiring) hub-viewer binding with an unconditional one for viewers; caller authorizes (user.promote on PATCH inside single atomic WithTx, authentication on login, server startup on backfill)", Scope: "pkg/hub/seed.go"}},
	{File: "pkg/hub/seed.go", Function: "removeHubMembershipTx", Symbol: "RemoveGroupMember", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Canonical hub-members group removal when a user's role is viewer; used by syncHubRoleGrants (PATCH role inside WithTx, login paths, startup backfill); counterpart to ensureHubMembershipTx; caller authorizes", Scope: "pkg/hub/seed.go"}},
	{File: "pkg/hub/seed.go", Function: "reconcileBuiltInRole", Symbol: "CreateRoleDefinition", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Server startup: create built-in role definition", Scope: "pkg/hub/seed.go"}},
	{File: "pkg/hub/seed.go", Function: "reconcileBuiltInRole", Symbol: "UpdateSystemRoleDefinitionPermissions", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Server startup: reconcile built-in role permissions", Scope: "pkg/hub/seed.go"}},
	{File: "pkg/hub/seed.go", Function: "removeProjectMembersGroupParentEdges", Symbol: "RemoveChildGroupEdge", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Server startup: remove child-group edges that name a system-managed project members group (not a valid child group); only ever removes, never grants; audited", Scope: "pkg/hub/seed.go"}},
	{File: "pkg/hub/seed.go", Function: "removeProjectMembersGroupRoleBindings", Symbol: "DeleteRoleBinding", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Server startup: remove role bindings whose principal is a system-managed project members group (not a valid role-binding principal); only ever removes, never grants; audited", Scope: "pkg/hub/seed.go"}},
	{File: "pkg/hub/seed.go", Function: "seedDefaultGroupsAndBindings", Symbol: "CreateGroup", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Server startup: seed default groups", Scope: "pkg/hub/seed.go"}},
	{File: "pkg/hub/seed.go", Function: "seedDevUser", Symbol: "CreateUser", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Server startup: seed dev user", Scope: "pkg/hub/seed.go"}},
	{File: "pkg/hub/seed.go", Function: "seedHubMemberRoleBinding", Symbol: "CreateRoleBinding", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Server startup: seed hub member role binding", Scope: "pkg/hub/seed.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/server.go — server infrastructure
	// -----------------------------------------------------------------------
	{File: "pkg/hub/server.go", Function: "sweepOrphanedGroupMemberships", Symbol: "DeleteOrphanedGroupMemberships", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Server startup: delete group memberships whose user and agent are both NULL (principal deleted, ON DELETE SET NULL); such rows are always orphans and carry no principal; idempotent, every startup (ptone/scion#2769)", Scope: "pkg/hub/server.go"}},
	{File: "pkg/hub/server.go", Function: "RecordAgentCredential", Symbol: "CreateAgentCredential", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Agent credential provisioning during agent create, server infrastructure", Scope: "pkg/hub/server.go"}},
	{File: "pkg/hub/server.go", Function: "a2aBridgeSweepHandler", Symbol: "GenerateAccessToken", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Background job: A2A bridge sweep generates GCP tokens", Scope: "pkg/hub/server.go"}},
	{File: "pkg/hub/server.go", Function: "backupSigningKeyToStore", Symbol: "UpdateSecret", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "OIDC signing key backup, server infrastructure", Scope: "pkg/hub/server.go"}},
	{File: "pkg/hub/server.go", Function: "backupSigningKeyToStore", Symbol: "UpsertSecret", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "OIDC signing key backup, server infrastructure", Scope: "pkg/hub/server.go"}},
	{File: "pkg/hub/server.go", Function: "ensureSigningKey", Symbol: "DeleteSecret", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "OIDC signing key rotation, server infrastructure", Scope: "pkg/hub/server.go"}},
	{File: "pkg/hub/server.go", Function: "ensureSigningKey", Symbol: "GetSecretValue", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "OIDC signing key load, server infrastructure", Scope: "pkg/hub/server.go"}},
	{File: "pkg/hub/server.go", Function: "ensureSigningKey", Symbol: "GetSecretValue", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "OIDC signing key load, server infrastructure", Scope: "pkg/hub/server.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/oidckeys.go — OIDC key infrastructure
	// -----------------------------------------------------------------------
	{File: "pkg/hub/oidckeys.go", Function: "backupKeyToStore", Symbol: "UpdateSecret", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "OIDC keyset backup, cryptographic infrastructure", Scope: "pkg/hub/oidckeys.go"}},
	{File: "pkg/hub/oidckeys.go", Function: "backupKeyToStore", Symbol: "UpsertSecret", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "OIDC keyset backup, cryptographic infrastructure", Scope: "pkg/hub/oidckeys.go"}},
	{File: "pkg/hub/oidckeys.go", Function: "casCreateKeyInStore", Symbol: "CreateSecret", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "OIDC keyset CAS create, cryptographic infrastructure", Scope: "pkg/hub/oidckeys.go"}},
	{File: "pkg/hub/oidckeys.go", Function: "casCreateKeyInStore", Symbol: "GetSecretValue", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "OIDC keyset CAS read, cryptographic infrastructure", Scope: "pkg/hub/oidckeys.go"}},
	{File: "pkg/hub/oidckeys.go", Function: "loadKeysetFromDB", Symbol: "GetSecretValue", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "OIDC keyset load, cryptographic infrastructure", Scope: "pkg/hub/oidckeys.go"}},
	{File: "pkg/hub/oidckeys.go", Function: "loadOrCreateKey", Symbol: "GetSecretValue", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "OIDC keyset load-or-create, cryptographic infrastructure", Scope: "pkg/hub/oidckeys.go"}},
	{File: "pkg/hub/oidckeys.go", Function: "saveKeysetToDB", Symbol: "UpsertSecret", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "OIDC keyset save, cryptographic infrastructure", Scope: "pkg/hub/oidckeys.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/conduit_grants.go — Conduit grant signing key ring
	// -----------------------------------------------------------------------
	{File: "pkg/hub/conduit_grants.go", Function: "Create", Symbol: "CreateSecret", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Conduit grant key ring bootstrap, cryptographic infrastructure", Scope: "pkg/hub/conduit_grants.go"}},
	{File: "pkg/hub/conduit_grants.go", Function: "CompareAndSwap", Symbol: "UpdateSecretValueIfVersion", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Conduit grant key ring rotation (compare-and-swap), cryptographic infrastructure", Scope: "pkg/hub/conduit_grants.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/lifecycle_hook_executor.go — pre-start hook execution
	// -----------------------------------------------------------------------
	{File: "pkg/hub/lifecycle_hook_executor.go", Function: "resolveIdentityAndToken", Symbol: "GenerateAccessToken", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Pre-start hook: generates GCP token for hook execution", Scope: "pkg/hub/lifecycle_hook_executor.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/maintenance_executors.go — maintenance operations
	// -----------------------------------------------------------------------
	{File: "pkg/hub/maintenance_executors.go", Function: "Run", Symbol: "GetSecretValue", Exemption: &MutationExemption{Kind: ExemptionHubAdmin, Reason: "Maintenance executor reads secrets, hub-admin operation", Scope: "pkg/hub/maintenance_executors.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/system_identity.go
	// -----------------------------------------------------------------------
	{File: "pkg/hub/system_identity.go", Function: "updateDevUserRecord", Symbol: "UpdateUser", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Dev mode user record update, system identity management", Scope: "pkg/hub/system_identity.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/gcp_token_cache.go / gcp_token_iam.go — GCP token infrastructure
	// -----------------------------------------------------------------------
	{File: "pkg/hub/gcp_token_cache.go", Function: "GenerateAccessToken", Symbol: "GenerateAccessToken", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "GCP token cache, delegates to IAM GenerateAccessToken", Scope: "pkg/hub/gcp_token_cache.go"}},
	{File: "pkg/hub/gcp_token_iam.go", Function: "GenerateAccessToken", Symbol: "GenerateAccessToken", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "GCP IAM token generation, infrastructure implementation", Scope: "pkg/hub/gcp_token_iam.go"}},
	{File: "pkg/hub/gcp_token_iam.go", Function: "VerifyImpersonation", Symbol: "GenerateAccessToken", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "GCP impersonation verification, infrastructure implementation", Scope: "pkg/hub/gcp_token_iam.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/gcs_link_source.go — gs:// link per-request storage client
	// -----------------------------------------------------------------------
	{File: "pkg/hub/gcs_link_source.go", Function: "gcsObjectSourceFor", Symbol: "GenerateAccessToken", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "gs:// link per-request token mint, reached only after the gcs endpoint's own authorization steps have passed", Scope: "pkg/hub/gcs_link_source.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/brokerauth.go — broker authentication infrastructure
	// -----------------------------------------------------------------------
	{File: "pkg/hub/brokerauth.go", Function: "CompleteBrokerJoin", Symbol: "CreateBrokerSecret", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Broker join completion, broker-HMAC auth infrastructure", Scope: "pkg/hub/brokerauth.go"}},
	{File: "pkg/hub/brokerauth.go", Function: "CompleteBrokerJoin", Symbol: "DeleteBrokerSecret", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Broker join completion, broker-HMAC auth infrastructure", Scope: "pkg/hub/brokerauth.go"}},
	{File: "pkg/hub/brokerauth.go", Function: "CompleteBrokerJoin", Symbol: "ConsumeJoinToken", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Broker join completion: single-use join token consumed in the join transaction, broker-HMAC auth infrastructure", Scope: "pkg/hub/brokerauth.go"}},
	{File: "pkg/hub/server.go", Function: "brokerJoinTokenCleanupHandler", Symbol: "CleanExpiredJoinTokens", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Scheduled removal of expired broker join tokens, no caller", Scope: "pkg/hub/server.go"}},
	{File: "pkg/hub/brokerauth.go", Function: "classifyUnconsumedJoinToken", Symbol: "DeleteExpiredJoinToken", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Broker join completion, expired join token cleanup, broker-HMAC auth infrastructure", Scope: "pkg/hub/brokerauth.go"}},
	{File: "pkg/hub/brokerauth.go", Function: "createBrokerRegistration", Symbol: "UpsertJoinToken", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Broker registration, broker-HMAC auth infrastructure", Scope: "pkg/hub/brokerauth.go"}},
	{File: "pkg/hub/brokerauth.go", Function: "GenerateAndStoreSecret", Symbol: "CreateBrokerSecret", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Broker secret generation, broker-HMAC auth infrastructure", Scope: "pkg/hub/brokerauth.go"}},
	{File: "pkg/hub/brokerauth.go", Function: "RotateBrokerSecret", Symbol: "UpdateBrokerSecret", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Broker secret rotation, broker-HMAC auth infrastructure", Scope: "pkg/hub/brokerauth.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/handlers_runtime_brokers.go — broker deregistration cleanup
	// -----------------------------------------------------------------------
	{File: "pkg/hub/handlers_runtime_brokers.go", Function: "deleteRuntimeBroker", Symbol: "DeleteBrokerSecret", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Broker deregistration cleanup: delete HMAC secret for removed broker", Scope: "pkg/hub/handlers_runtime_brokers.go"}},
	{File: "pkg/hub/handlers_runtime_brokers.go", Function: "deleteRuntimeBroker", Symbol: "DeleteJoinToken", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Broker deregistration cleanup: delete join token for removed broker", Scope: "pkg/hub/handlers_runtime_brokers.go"}},

	// -----------------------------------------------------------------------
	// pkg/hub/brokerclient.go / controlchannel_client.go / httpdispatcher.go
	// — agent delete dispatch infrastructure
	// -----------------------------------------------------------------------
	{File: "pkg/hub/brokerclient.go", Function: "DeleteAgent", Symbol: "DeleteAgent", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Broker client agent delete dispatch, infrastructure adapter", Scope: "pkg/hub/brokerclient.go"}},
	{File: "pkg/hub/controlchannel_client.go", Function: "DeleteAgent", Symbol: "DeleteAgent", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Control channel agent delete dispatch, infrastructure adapter", Scope: "pkg/hub/controlchannel_client.go"}},
	{File: "pkg/hub/controlchannel_client.go", Function: "DeleteAgent", Symbol: "DeleteAgent", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Control channel agent delete dispatch, infrastructure adapter", Scope: "pkg/hub/controlchannel_client.go"}},
	{File: "pkg/hub/httpdispatcher.go", Function: "DeleteAgent", Symbol: "DeleteAgent", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "HTTP dispatcher agent delete, infrastructure adapter", Scope: "pkg/hub/httpdispatcher.go"}},
	{File: "pkg/hub/httpdispatcher.go", Function: "DispatchAgentDelete", Symbol: "DeleteAgent", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "HTTP dispatcher agent delete dispatch, infrastructure adapter", Scope: "pkg/hub/httpdispatcher.go"}},
	{File: "pkg/hub/httpdispatcher.go", Function: "deletePreviousRuns", Symbol: "DeleteAgent", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "HTTP dispatcher run-scoped delete of an agent's previous runs, part of DispatchAgentDelete, infrastructure adapter", Scope: "pkg/hub/httpdispatcher.go"}},
	{File: "pkg/hub/move_dispatch.go", Function: "DispatchAgentDeleteLocalOnly", Symbol: "DeleteAgent", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "move dispatcher source-broker local-only agent delete during reincarnate --broker, infrastructure adapter", Scope: "pkg/hub/move_dispatch.go"}},

	// -----------------------------------------------------------------------
	// pkg/store/entadapter/ — store layer implementation
	// -----------------------------------------------------------------------
	{File: "pkg/store/entadapter/composite.go", Function: "DeleteAgent", Symbol: "DeleteAgent", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store adapter: composite DeleteAgent implementation", Scope: "pkg/store/entadapter"}},
	{File: "pkg/store/entadapter/composite.go", Function: "DeleteAgent", Symbol: "DeleteAgent", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store adapter: composite DeleteAgent re-enters itself on the transactional store so the cascade and the row delete commit together (ptone/scion#2769)", Scope: "pkg/store/entadapter"}},
	{File: "pkg/store/entadapter/composite.go", Function: "DeleteAgent", Symbol: "DeleteGroupMembershipsForAgents", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store adapter: DeleteAgent removes the agent's group memberships before the agent row (ON DELETE SET NULL would orphan them; ptone/scion#2769)", Scope: "pkg/store/entadapter"}},
	{File: "pkg/store/entadapter/composite.go", Function: "DeleteProject", Symbol: "DeleteGroupMembershipsForAgents", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store adapter: DeleteProject removes its agents' group memberships before the bulk agent delete (ptone/scion#2769)", Scope: "pkg/store/entadapter"}},
	{File: "pkg/store/entadapter/composite.go", Function: "PurgeDeletedAgents", Symbol: "DeleteGroupMembershipsForAgents", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store adapter: PurgeDeletedAgents removes each batch's still-eligible agents' group memberships before the delete, in the purge transaction (ptone/scion#2769)", Scope: "pkg/store/entadapter"}},
	{File: "pkg/store/entadapter/agent_deletion_finalize.go", Function: "finalizeAgentDeletionOnce", Symbol: "DeleteGroupMembershipsForAgents", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store adapter: finalize-hard removes the agent's group memberships before tx.Agent.Delete(), in the same transaction; the caller (FinalizeAgentDeletion via the delete engine) authorizes (ptone/scion#2769)", Scope: "pkg/store/entadapter"}},
	{File: "pkg/store/entadapter/composite.go", Function: "DeleteProject", Symbol: "DeleteProject", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store adapter: composite DeleteProject implementation", Scope: "pkg/store/entadapter"}},
	{File: "pkg/store/entadapter/composite.go", Function: "DeleteProject", Symbol: "DeleteProject", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store adapter: composite DeleteProject re-enters itself on the transactional store so the cascade and the row deletes commit together (ptone/scion#2769)", Scope: "pkg/store/entadapter"}},
	{File: "pkg/store/entadapter/secret_store.go", Function: "UpsertSecret", Symbol: "CreateSecret", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store adapter: UpsertSecret delegates to CreateSecret", Scope: "pkg/store/entadapter"}},
	{File: "pkg/store/entadapter/secret_store.go", Function: "UpsertSecret", Symbol: "UpdateSecret", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store adapter: UpsertSecret delegates to UpdateSecret", Scope: "pkg/store/entadapter"}},

	// -----------------------------------------------------------------------
	// pkg/store/storetest/ — store interface conformance test fixtures
	// -----------------------------------------------------------------------
	{File: "pkg/store/storetest/domains.go", Function: "AgentDomain", Symbol: "DeleteAgent", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: agent domain setup", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains.go", Function: "GCPServiceAccountDomain", Symbol: "CreateGCPServiceAccount", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: GCP SA domain setup", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains.go", Function: "GCPServiceAccountDomain", Symbol: "CreateGCPServiceAccount", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: GCP SA domain setup", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains.go", Function: "GCPServiceAccountDomain", Symbol: "CreateGCPServiceAccount", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: GCP SA domain setup", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains.go", Function: "GCPServiceAccountDomain", Symbol: "DeleteGCPServiceAccount", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: GCP SA domain teardown", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains.go", Function: "GCPServiceAccountDomain", Symbol: "UpdateGCPServiceAccount", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: GCP SA domain update", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains.go", Function: "GroupDomain", Symbol: "CreateGroup", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: group domain setup", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains.go", Function: "GroupDomain", Symbol: "CreateGroup", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: group domain setup", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains.go", Function: "GroupDomain", Symbol: "CreateGroup", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: group domain setup", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains.go", Function: "GroupDomain", Symbol: "DeleteGroup", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: group domain teardown", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains.go", Function: "GroupDomain", Symbol: "UpdateGroup", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: group domain update", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains.go", Function: "seedGCPScopeMix", Symbol: "CreateGCPServiceAccount", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: GCP scope seeding", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_agent_hold.go", Function: "AgentHoldConformance", Symbol: "DeleteAgent", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: hard delete of an agent in a conformance case", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_delegation_descendants.go", Function: "DelegationDescendantsConformance", Symbol: "DeleteAgent", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: hard delete of an agent in a conformance case", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_delegation_descendants.go", Function: "DelegationDescendantsConformance", Symbol: "DeleteAgent", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: hard delete of an agent in a conformance case", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_group.go", Function: "GroupChildEdgeRemovalConformance", Symbol: "AddGroupMember", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: child-group edge removal conformance edge setup", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_group.go", Function: "GroupChildEdgeRemovalConformance", Symbol: "CreateGroup", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: child-group edge removal conformance group setup", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_group.go", Function: "GroupChildEdgeRemovalConformance", Symbol: "RemoveChildGroupEdge", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: child-group edge removal conformance under test", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_group.go", Function: "GroupDirectParentsConformance", Symbol: "AddGroupMember", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: direct-parent group conformance edge setup", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_group.go", Function: "GroupDirectParentsConformance", Symbol: "CreateGroup", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: direct-parent group conformance group setup", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_project_broker.go", Function: "BrokerJoinTokenDomain", Symbol: "CreateJoinToken", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: broker join token domain setup", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_project_broker.go", Function: "BrokerJoinTokenDomain", Symbol: "DeleteJoinToken", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: broker join token domain teardown", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_project_broker.go", Function: "BrokerSecretDomain", Symbol: "CreateBrokerSecret", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: broker secret domain setup", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_project_broker.go", Function: "BrokerSecretDomain", Symbol: "DeleteBrokerSecret", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: broker secret domain teardown", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_project_broker.go", Function: "BrokerSecretDomain", Symbol: "UpdateBrokerSecret", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: broker secret domain update", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_project_broker.go", Function: "ProjectDomain", Symbol: "DeleteProject", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: project domain teardown", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_secret_template.go", Function: "SecretDomain", Symbol: "CreateSecret", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: secret domain setup", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_secret_template.go", Function: "SecretDomain", Symbol: "CreateSecret", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: secret domain setup", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_secret_template.go", Function: "SecretDomain", Symbol: "CreateSecret", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: secret domain setup", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_secret_template.go", Function: "SecretDomain", Symbol: "DeleteSecret", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: secret domain teardown", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_secret_template.go", Function: "SecretDomain", Symbol: "UpdateSecret", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: secret domain update", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_group_membership.go", Function: "GroupMembershipCleanupConformance", Symbol: "CreateUser", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: group-membership cleanup conformance user setup", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_group_membership.go", Function: "GroupMembershipCleanupConformance", Symbol: "CreateGroup", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: group-membership cleanup conformance group setup", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_group_membership.go", Function: "GroupMembershipCleanupConformance", Symbol: "AddGroupMember", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: group-membership cleanup conformance membership setup", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_group_membership.go", Function: "GroupMembershipCleanupConformance", Symbol: "DeleteGroupMembershipsForUser", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: group-membership cleanup conformance user cleanup", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_group_membership.go", Function: "GroupMembershipCleanupConformance", Symbol: "DeleteGroupMembershipsForUser", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: group-membership cleanup conformance user cleanup (idempotent re-run)", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_group_membership.go", Function: "GroupMembershipCleanupConformance", Symbol: "DeleteGroupMembershipsForAgents", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: group-membership cleanup conformance agent cleanup", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_group_membership.go", Function: "GroupMembershipCleanupConformance", Symbol: "DeleteGroupMembershipsForAgents", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: group-membership cleanup conformance agent cleanup (empty ids)", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_group_membership.go", Function: "GroupMembershipCleanupConformance", Symbol: "DeleteUser", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: group-membership cleanup conformance orphan creation", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_group_membership.go", Function: "GroupMembershipCleanupConformance", Symbol: "DeleteAgent", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: group-membership cleanup conformance orphan creation", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_group_membership.go", Function: "GroupMembershipCleanupConformance", Symbol: "DeleteOrphanedGroupMemberships", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: group-membership cleanup conformance sweep", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_group_membership.go", Function: "GroupMembershipCleanupConformance", Symbol: "DeleteOrphanedGroupMemberships", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: group-membership cleanup conformance sweep (idempotent re-run)", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_user.go", Function: "InviteCodeDomain", Symbol: "CreateInviteCode", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: invite code domain setup", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_user.go", Function: "InviteCodeDomain", Symbol: "DeleteInviteCode", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: invite code domain teardown", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_user.go", Function: "UserDomain", Symbol: "CreateUser", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: user domain setup", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_user.go", Function: "UserDomain", Symbol: "CreateUser", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: user domain setup", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_user.go", Function: "UserDomain", Symbol: "CreateUser", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: user domain setup", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_user.go", Function: "UserDomain", Symbol: "CreateUser", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: user domain setup", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_user.go", Function: "UserDomain", Symbol: "CreateUser", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: user domain setup", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_user.go", Function: "UserDomain", Symbol: "DeleteUser", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: user domain teardown", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_user.go", Function: "UserDomain", Symbol: "UpdateUser", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: user domain update", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_user.go", Function: "UserTerminalWorkspaceConformance", Symbol: "CreateUser", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: terminal-workspace conformance user setup", Scope: "pkg/store/storetest"}},
	{File: "pkg/store/storetest/domains_user.go", Function: "UserTerminalWorkspaceConformance", Symbol: "DeleteUser", Exemption: &MutationExemption{Kind: ExemptionInternalOnly, Reason: "Store test fixture: terminal-workspace conformance cascade-delete teardown", Scope: "pkg/store/storetest"}},
}

// concatOperations joins the per-area operation lists into one catalog.
func concatOperations(areas ...[]OperationSpec) []OperationSpec {
	var n int
	for _, a := range areas {
		n += len(a)
	}
	out := make([]OperationSpec, 0, n)
	for _, a := range areas {
		out = append(out, a...)
	}
	return out
}

// CatalogOperationIDs returns the set of all operation IDs in the catalog.
func CatalogOperationIDs() map[OperationID]bool {
	ids := make(map[OperationID]bool, len(Catalog))
	for _, spec := range Catalog {
		ids[spec.ID] = true
	}
	return ids
}

// CatalogBasePermissions returns the set of all base permissions referenced
// by catalog operations.
func CatalogBasePermissions() map[string][]OperationID {
	perms := make(map[string][]OperationID)
	for _, spec := range Catalog {
		if spec.BasePermission != "" {
			perms[spec.BasePermission] = append(perms[spec.BasePermission], spec.ID)
		}
	}
	return perms
}
