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

// routeAuthzManifest declares the authorization posture for every route
// registered in registerRoutes(). This variable is a lint-only artifact: it
// is not referenced at runtime by any Go code. Its sole consumer is
// hack/check-route-authz-manifest.sh, which lexically extracts the keys and
// compares them against the route registrations in server.go.
//
// Authorization postures:
//
//	public         — No authentication required (health checks, setup callbacks).
//	auth-flow      — Part of the authentication flow itself; skipped by auth middleware.
//	authenticated  — Requires valid user/agent session (via UnifiedAuthMiddleware).
//	admin          — Requires admin role (explicit user.Role() != "admin" check).
//	workstation    — Requires workstation mode (requireWorkstation middleware).
//	broker-hmac    — Authenticated via runtime broker HMAC signature.
//	webhook        — Authenticated via webhook signature (e.g. GitHub X-Hub-Signature-256).
//	agent-token    — Authenticated via agent JWT token.
//	oidc-public    — Public OIDC metadata endpoints (no auth, conditionally registered).
//
// When adding a new route:
//  1. Add its handler to registerRoutes() in server.go.
//  2. Add a manifest entry here declaring its authz posture.
//  3. Run make check-route-authz-manifest to verify.
//
// See hack/LINT-CONVENTIONS.md and ptone/scion#598.
var routeAuthzManifest = map[string]string{
	// ── Health and metrics ──────────────────────────────────────────────
	"/healthz": "public",        // No auth — health check
	"/readyz":  "public",        // No auth — readiness probe
	"/metrics": "authenticated", // Prometheus metrics — requires session

	// ── Authentication flow (skipped by UnifiedAuthMiddleware) ──────────
	"/api/v1/auth/login":            "auth-flow", // Web frontend OAuth token exchange
	"/api/v1/auth/token":            "auth-flow", // OAuth code exchange (unified)
	"/api/v1/auth/refresh":          "auth-flow", // Token refresh
	"/api/v1/auth/validate":         "auth-flow", // Token validation
	"/api/v1/auth/logout":           "auth-flow", // Logout
	"/api/v1/auth/providers":        "auth-flow", // OAuth provider discovery for CLI login
	"/api/v1/auth/cli/authorize":    "auth-flow", // CLI OAuth authorization URL
	"/api/v1/auth/cli/token":        "auth-flow", // CLI OAuth token exchange
	"/api/v1/auth/cli/device":       "auth-flow", // CLI device flow initiation
	"/api/v1/auth/cli/device/token": "auth-flow", // CLI device flow token polling

	// ── Auth-adjacent (require session despite living under /auth) ──────
	"/api/v1/auth/me":            "authenticated", // "Who am I?" — requires valid session
	"/api/v1/auth/tokens":        "authenticated", // API token management — requires user session
	"/api/v1/auth/tokens/":       "authenticated", // API token by ID — requires user session
	"/api/v1/auth/invite/redeem": "authenticated", // Invite redemption — requires user session

	// ── Agents ─────────────────────────────────────────────────────────
	"/api/v1/agents":  "authenticated", // List/create agents
	"/api/v1/agents/": "authenticated", // Agent by ID

	// ── Projects ───────────────────────────────────────────────────────
	"/api/v1/projects":          "authenticated", // List/create projects
	"/api/v1/projects/register": "authenticated", // Project registration
	"/api/v1/projects/":         "authenticated", // Project routes (by-id, nested resources)

	// ── Legacy grove aliases ───────────────────────────────────────────
	"/api/v1/groves":          "authenticated", // Legacy alias for /projects
	"/api/v1/groves/register": "authenticated", // Legacy alias for /projects/register
	"/api/v1/groves/":         "authenticated", // Legacy alias for /projects/

	// ── Runtime brokers ────────────────────────────────────────────────
	"/api/v1/runtime-brokers":         "authenticated", // List/create runtime brokers
	"/api/v1/runtime-brokers/":        "authenticated", // Runtime broker routes
	"/api/v1/runtime-brokers/connect": "broker-hmac",   // WebSocket control channel — broker HMAC

	// ── Templates ──────────────────────────────────────────────────────
	"/api/v1/templates":  "authenticated", // List/create templates
	"/api/v1/templates/": "authenticated", // Template by ID

	// ── GCP service accounts ───────────────────────────────────────────
	"/api/v1/gcp-service-accounts":  "authenticated", // List GCP service accounts
	"/api/v1/gcp-service-accounts/": "authenticated", // GCP service account by ID

	// ── Skills ─────────────────────────────────────────────────────────
	"/api/v1/skills":                    "authenticated", // List/create skills
	"/api/v1/skills/":                   "authenticated", // Skill by ID
	"/api/v1/skills/discover-directory": "authenticated", // Skill directory discovery

	// ── Skill registries ───────────────────────────────────────────────
	"/api/v1/skill-registries":  "authenticated", // List/create skill registries
	"/api/v1/skill-registries/": "authenticated", // Skill registry by ID

	// ── Harness configs ────────────────────────────────────────────────
	"/api/v1/harness-configs":  "authenticated", // List/create harness configs
	"/api/v1/harness-configs/": "authenticated", // Harness config by ID

	// ── Pre-start hooks (hub-scoped) ───────────────────────────────────
	"/api/v1/pre-start-hooks":  "authenticated", // List/create pre-start hooks
	"/api/v1/pre-start-hooks/": "authenticated", // Pre-start hook by ID

	// ── Users ──────────────────────────────────────────────────────────
	"/api/v1/users":  "authenticated", // List/create users
	"/api/v1/users/": "authenticated", // User by ID

	// ── Environment variables and secrets ──────────────────────────────
	"/api/v1/env":      "authenticated", // List/create env vars
	"/api/v1/env/":     "authenticated", // Env var by key
	"/api/v1/secrets":  "authenticated", // List/create secrets
	"/api/v1/secrets/": "authenticated", // Secret by key

	// ── Session metrics ────────────────────────────────────────────────
	"/api/v1/metrics/session/": "authenticated", // Session metrics (DB-backed)

	// ── Groups and policies (hub permissions) ──────────────────────────
	"/api/v1/groups":    "authenticated", // List/create groups
	"/api/v1/groups/":   "authenticated", // Group routes
	"/api/v1/policies":  "authenticated", // List/create policies
	"/api/v1/policies/": "authenticated", // Policy routes

	// ── Principal resolution ───────────────────────────────────────────
	"/api/v1/users/me/groups": "authenticated", // My groups
	"/api/v1/principals/":     "authenticated", // Principal routes

	// ── User-scoped injected skills ────────────────────────────────────
	"/api/v1/users/me/injected-skills":  "authenticated", // List/create user injected skills
	"/api/v1/users/me/injected-skills/": "authenticated", // User injected skill by ID

	// ── Hub-scoped injected skills ─────────────────────────────────────
	"/api/v1/hub/settings/injected-skills": "authenticated", // Hub injected skills

	// ── Broker registration ────────────────────────────────────────────
	"/api/v1/brokers":      "authenticated", // List/register brokers — requires session
	"/api/v1/brokers/join": "auth-flow",     // Broker join — uses join token, in isUnauthenticatedEndpoint
	"/api/v1/brokers/":     "authenticated", // Broker by ID routes

	// ── Broker plugin endpoints ────────────────────────────────────────
	"/api/v1/broker/inbound":  "broker-hmac", // Broker inbound messages — broker HMAC
	"/api/v1/broker/projects": "broker-hmac", // Broker project listing — broker HMAC

	// ── Admin system endpoints ─────────────────────────────────────────
	"/api/v1/admin/maintenance":                 "admin", // Maintenance mode — requires admin role
	"/api/v1/admin/maintenance/operations":      "admin", // Maintenance operations
	"/api/v1/admin/maintenance/operations/":     "admin", // Maintenance operation by ID
	"/api/v1/admin/maintenance/migrations/":     "admin", // Maintenance migrations
	"/api/v1/admin/maintenance/check-updates":   "admin", // Check for updates
	"/api/v1/admin/maintenance/restart":         "admin", // Restart hub
	"/api/v1/admin/scheduler":                   "admin", // Scheduler status
	"/api/v1/admin/allow-list":                  "admin", // Allow list management
	"/api/v1/admin/allow-list/":                 "admin", // Allow list by email
	"/api/v1/admin/users/invite/bulk":           "admin", // Bulk user invite
	"/api/v1/admin/users/invite":                "admin", // User invite
	"/api/v1/admin/invites":                     "admin", // List invites
	"/api/v1/admin/invites/":                    "admin", // Invite by ID
	"/api/v1/admin/server-config/schema":        "admin", // Server config schema
	"/api/v1/admin/server-config/sections/":     "admin", // Server config section reset
	"/api/v1/admin/server-config":               "admin", // Server config
	"/api/v1/admin/project-defaults":            "admin", // Project defaults
	"/api/v1/admin/agents/reset-auth-all":       "admin", // Reset all agent auth
	"/api/v1/admin/gcp-quota":                   "admin", // GCP quota management
	"/api/v1/admin/lifecycle-hooks":             "admin", // Lifecycle hooks
	"/api/v1/admin/lifecycle-hooks/":            "admin", // Lifecycle hook by ID
	"/api/v1/admin/validate-resources":          "admin", // Validate resources
	"/api/v1/admin/integrations":                "admin", // Integrations management
	"/api/v1/admin/integrations/teams/manifest": "admin", // Teams manifest download
	"/api/v1/admin/integrations/":               "admin", // Integration by name
	"/api/v1/admin/diagnostics/logs/stream":     "admin", // Diagnostics log stream
	"/api/v1/admin/diagnostics/logs":            "admin", // Diagnostics logs
	"/api/v1/admin/health/summary":              "admin", // Health summary

	// ── Metrics dashboard (intentionally not admin-only) ───────────────
	"/api/v1/metrics/":                "authenticated", // Metrics dashboard — any session
	"/api/v1/admin/metrics-dashboard": "authenticated", // Legacy metrics dashboard alias — any session

	// ── Notifications ──────────────────────────────────────────────────
	"/api/v1/notifications":  "authenticated", // List notifications
	"/api/v1/notifications/": "authenticated", // Notification routes

	// ── Messages ───────────────────────────────────────────────────────
	"/api/v1/messages":         "authenticated", // List messages
	"/api/v1/messages/":        "authenticated", // Message routes
	"/api/v1/message-channels": "authenticated", // Message channels

	// ── Native chat (conditionally registered) ─────────────────────────
	"/api/v1/chat/prefs":          "authenticated", // Chat preferences
	"/api/v1/chat/threads":        "authenticated", // Chat threads
	"/api/v1/chat/threads/":       "authenticated", // Chat thread routes
	"/api/v1/chat/spaces":         "authenticated", // Chat spaces
	"/api/v1/chat/spaces/":        "authenticated", // Chat space routes
	"/api/v1/chat/conversations/": "authenticated", // Chat conversation routes
	"/api/v1/chat/topics/":        "authenticated", // Chat topic routes
	"/api/v1/chat/dms":            "authenticated", // Chat direct messages
	"/api/v1/chat/user-prefs":     "authenticated", // Chat user preferences
	"/api/v1/chat/presence":       "authenticated", // Chat presence
	"/api/v1/chat/search":         "authenticated", // Chat search
	"/api/v1/chat/attachments":    "authenticated", // Chat attachments
	"/api/v1/chat/attachments/":   "authenticated", // Chat attachment by ID

	// ── Agent GCP identity ─────────────────────────────────────────────
	"/api/v1/agent/gcp-token":          "agent-token", // Agent GCP access token
	"/api/v1/agent/gcp-identity-token": "agent-token", // Agent GCP identity token

	// ── Public settings ────────────────────────────────────────────────
	"/api/v1/settings/public": "authenticated", // Public settings — requires session despite name

	// ── GitHub App integration ─────────────────────────────────────────
	"/api/v1/github-app":                        "authenticated", // GitHub App management
	"/api/v1/github-app/installations":          "authenticated", // GitHub App installations list
	"/api/v1/github-app/installations/":         "authenticated", // GitHub App installation by ID
	"/api/v1/github-app/installations/discover": "authenticated", // GitHub App discovery
	"/api/v1/github-app/sync-permissions":       "authenticated", // GitHub App permission sync

	// ── Platform account linking ───────────────────────────────────────
	"/api/v1/telegram/link":        "authenticated", // Telegram account linking
	"/api/v1/telegram/link/verify": "authenticated", // Telegram link verification
	"/api/v1/telegram/link/status": "authenticated", // Telegram link status
	"/api/v1/discord/link":         "authenticated", // Discord account linking
	"/api/v1/discord/link/verify":  "authenticated", // Discord link verification
	"/api/v1/discord/link/status":  "authenticated", // Discord link status
	"/api/v1/teams/link":           "authenticated", // Teams account linking
	"/api/v1/teams/link/verify":    "authenticated", // Teams link verification
	"/api/v1/teams/link/status":    "authenticated", // Teams link status

	// ── Resource import ────────────────────────────────────────────────
	"/api/v1/resources/import":   "authenticated", // Resource import (templates + harness-configs)
	"/api/v1/resources/discover": "authenticated", // Resource discovery

	// ── Webhooks ───────────────────────────────────────────────────────
	"/api/v1/webhooks/github": "webhook", // GitHub webhook — signature verification
	"/github-app/setup":       "public",  // GitHub App post-install callback — browser redirect

	// ── Workstation-only system endpoints ──────────────────────────────
	"/api/v1/system/identity":             "workstation", // System identity
	"/api/v1/system/status":               "workstation", // System status
	"/api/v1/system/check":                "workstation", // System check
	"/api/v1/system/runtime":              "workstation", // System runtime
	"/api/v1/system/init":                 "workstation", // System init
	"/api/v1/system/images/pull":          "workstation", // Image pull
	"/api/v1/system/images/build":         "workstation", // Image build
	"/api/v1/system/apple-dns":            "workstation", // Apple DNS helper
	"/api/v1/system/registry":             "workstation", // System registry
	"/api/v1/system/workstation-settings": "workstation", // Workstation settings

	// ── Workstation-only filesystem endpoints ──────────────────────────
	"/api/v1/system/fs/list":          "workstation", // Filesystem listing
	"/api/v1/system/fs/mkdir":         "workstation", // Filesystem mkdir
	"/api/v1/system/fs/validate-path": "workstation", // Filesystem path validation

	// ── OIDC Identity Provider (conditionally registered) ──────────────
	"GET /.well-known/openid-configuration": "oidc-public", // OIDC discovery document
	"GET /.well-known/jwks.json":            "oidc-public", // OIDC JSON Web Key Set
	"POST /api/v1/agent/identity-token":     "agent-token", // Agent OIDC identity token
}
