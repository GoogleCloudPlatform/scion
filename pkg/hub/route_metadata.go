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

// RouteClassification categorizes routes by their authentication/authorization model.
type RouteClassification string

const (
	RoutePublic        RouteClassification = "public"        // No auth required
	RouteAuthenticated RouteClassification = "authenticated" // Identity required, no specific permission
	RoutePolicy        RouteClassification = "policy"        // Permission-checked via AuthzRequest/Decide
	RouteHubAdmin      RouteClassification = "hub-admin"     // Unscoped local platform admin only
	RouteWorkstation   RouteClassification = "workstation"   // Workstation token required
	RouteBrokerHMAC    RouteClassification = "broker-hmac"   // Broker HMAC authentication
	RouteAgentToken    RouteClassification = "agent-token"   // Agent JWT required
	RouteWebhook       RouteClassification = "webhook"       // Webhook signature verification
)

// RouteMetadata describes the authorization requirements for a single route.
type RouteMetadata struct {
	// Pattern is the HTTP method+path registered on the mux (e.g., "GET /api/v1/agents").
	// For routes registered without a method prefix, Pattern is just the path.
	Pattern string

	// RouteID is a stable, unique identifier for this route (e.g., "agents.list", "projects.create").
	RouteID string

	// Classification determines which authorization model applies.
	Classification RouteClassification

	// Permission is the canonical permission ID required (from pkg/hub/permissions registry).
	// Only meaningful when Classification == RoutePolicy.
	Permission string

	// Resource is the resource type from the permission registry (e.g., "agent", "project").
	// Only meaningful when Classification == RoutePolicy.
	Resource string

	// Action is the action from the permission registry (e.g., "read", "create", "update").
	// Only meaningful when Classification == RoutePolicy.
	Action string
}

// routeMetadataTable maps every registered mux pattern to its authorization metadata.
// Every route in registerRoutes() MUST have an entry here; the route coverage test
// enforces this invariant.
var routeMetadataTable = map[string]RouteMetadata{
	// -------------------------------------------------------------------------
	// Public: Health, metrics, and unauthenticated endpoints
	// -------------------------------------------------------------------------
	"/healthz": {
		Pattern: "/healthz", RouteID: "health.liveness",
		Classification: RoutePublic,
	},
	"/readyz": {
		Pattern: "/readyz", RouteID: "health.readiness",
		Classification: RoutePublic,
	},
	"/metrics": {
		Pattern: "/metrics", RouteID: "metrics.prometheus",
		Classification: RoutePublic,
	},

	// -------------------------------------------------------------------------
	// Public: Auth endpoints (login, token, providers, etc.)
	// -------------------------------------------------------------------------
	"/api/v1/auth/login": {
		Pattern: "/api/v1/auth/login", RouteID: "auth.login",
		Classification: RoutePublic,
	},
	"/api/v1/auth/token": {
		Pattern: "/api/v1/auth/token", RouteID: "auth.token",
		Classification: RoutePublic,
	},
	"/api/v1/auth/refresh": {
		Pattern: "/api/v1/auth/refresh", RouteID: "auth.refresh",
		Classification: RoutePublic,
	},
	"/api/v1/auth/validate": {
		Pattern: "/api/v1/auth/validate", RouteID: "auth.validate",
		Classification: RoutePublic,
	},
	"/api/v1/auth/providers": {
		Pattern: "/api/v1/auth/providers", RouteID: "auth.providers",
		Classification: RoutePublic,
	},
	"/api/v1/auth/invite/redeem": {
		Pattern: "/api/v1/auth/invite/redeem", RouteID: "auth.invite.redeem",
		Classification: RoutePublic,
	},
	"/api/v1/auth/cli/authorize": {
		Pattern: "/api/v1/auth/cli/authorize", RouteID: "auth.cli.authorize",
		Classification: RoutePublic,
	},
	"/api/v1/auth/cli/token": {
		Pattern: "/api/v1/auth/cli/token", RouteID: "auth.cli.token",
		Classification: RoutePublic,
	},
	"/api/v1/auth/cli/device": {
		Pattern: "/api/v1/auth/cli/device", RouteID: "auth.cli.device",
		Classification: RoutePublic,
	},
	"/api/v1/auth/cli/device/token": {
		Pattern: "/api/v1/auth/cli/device/token", RouteID: "auth.cli.device.token",
		Classification: RoutePublic,
	},
	"/api/v1/settings/public": {
		Pattern: "/api/v1/settings/public", RouteID: "settings.public",
		Classification: RoutePublic,
	},
	"/github-app/setup": {
		Pattern: "/github-app/setup", RouteID: "github-app.setup",
		Classification: RoutePublic,
	},

	// -------------------------------------------------------------------------
	// Public: OIDC discovery (method-prefixed)
	// -------------------------------------------------------------------------
	"GET /.well-known/openid-configuration": {
		Pattern: "GET /.well-known/openid-configuration", RouteID: "oidc.discovery",
		Classification: RoutePublic,
	},
	"GET /.well-known/jwks.json": {
		Pattern: "GET /.well-known/jwks.json", RouteID: "oidc.jwks",
		Classification: RoutePublic,
	},

	// -------------------------------------------------------------------------
	// Authenticated: User session endpoints
	// -------------------------------------------------------------------------
	"/api/v1/auth/logout": {
		Pattern: "/api/v1/auth/logout", RouteID: "auth.logout",
		Classification: RouteAuthenticated,
	},
	"/api/v1/auth/me": {
		Pattern: "/api/v1/auth/me", RouteID: "auth.me",
		Classification: RouteAuthenticated,
	},
	"/api/v1/auth/tokens": {
		Pattern: "/api/v1/auth/tokens", RouteID: "auth.tokens.list",
		Classification: RouteAuthenticated,
	},
	"/api/v1/auth/tokens/": {
		Pattern: "/api/v1/auth/tokens/", RouteID: "auth.tokens.byId",
		Classification: RouteAuthenticated,
	},
	"/api/v1/metrics/session/": {
		Pattern: "/api/v1/metrics/session/", RouteID: "metrics.session",
		Classification: RouteAuthenticated,
	},
	"/api/v1/users/me/groups": {
		Pattern: "/api/v1/users/me/groups", RouteID: "users.me.groups",
		Classification: RouteAuthenticated,
	},
	"/api/v1/principals/": {
		Pattern: "/api/v1/principals/", RouteID: "principals.byId",
		Classification: RouteAuthenticated,
	},
	"/api/v1/users/me/injected-skills": {
		Pattern: "/api/v1/users/me/injected-skills", RouteID: "users.me.injectedSkills",
		Classification: RouteAuthenticated,
	},
	"/api/v1/users/me/injected-skills/": {
		Pattern: "/api/v1/users/me/injected-skills/", RouteID: "users.me.injectedSkills.byId",
		Classification: RouteAuthenticated,
	},
	"/api/v1/users/me/templates": {
		Pattern: "/api/v1/users/me/templates", RouteID: "users.me.templates",
		Classification: RouteAuthenticated,
	},
	"/api/v1/users/me/templates/": {
		Pattern: "/api/v1/users/me/templates/", RouteID: "users.me.templates.byId",
		Classification: RouteAuthenticated,
	},
	"/api/v1/notifications": {
		Pattern: "/api/v1/notifications", RouteID: "notifications.list",
		Classification: RouteAuthenticated,
	},
	"/api/v1/notifications/": {
		Pattern: "/api/v1/notifications/", RouteID: "notifications.byId",
		Classification: RouteAuthenticated,
	},
	"/api/v1/messages": {
		Pattern: "/api/v1/messages", RouteID: "messages.list",
		Classification: RouteAuthenticated,
	},
	"/api/v1/messages/": {
		Pattern: "/api/v1/messages/", RouteID: "messages.byId",
		Classification: RouteAuthenticated,
	},
	"/api/v1/message-channels": {
		Pattern: "/api/v1/message-channels", RouteID: "messageChannels.list",
		Classification: RouteAuthenticated,
	},
	"/api/v1/chat/user-prefs": {
		Pattern: "/api/v1/chat/user-prefs", RouteID: "chat.userPrefs",
		Classification: RouteAuthenticated,
	},
	"/api/v1/chat/presence": {
		Pattern: "/api/v1/chat/presence", RouteID: "chat.presence",
		Classification: RouteAuthenticated,
	},

	// -------------------------------------------------------------------------
	// Authenticated: Account linking
	// -------------------------------------------------------------------------
	"/api/v1/telegram/link": {
		Pattern: "/api/v1/telegram/link", RouteID: "telegram.link",
		Classification: RouteAuthenticated,
	},
	"/api/v1/telegram/link/verify": {
		Pattern: "/api/v1/telegram/link/verify", RouteID: "telegram.link.verify",
		Classification: RouteAuthenticated,
	},
	"/api/v1/telegram/link/status": {
		Pattern: "/api/v1/telegram/link/status", RouteID: "telegram.link.status",
		Classification: RouteAuthenticated,
	},
	"/api/v1/discord/link": {
		Pattern: "/api/v1/discord/link", RouteID: "discord.link",
		Classification: RouteAuthenticated,
	},
	"/api/v1/discord/link/verify": {
		Pattern: "/api/v1/discord/link/verify", RouteID: "discord.link.verify",
		Classification: RouteAuthenticated,
	},
	"/api/v1/discord/link/status": {
		Pattern: "/api/v1/discord/link/status", RouteID: "discord.link.status",
		Classification: RouteAuthenticated,
	},
	"/api/v1/teams/link": {
		Pattern: "/api/v1/teams/link", RouteID: "teams.link",
		Classification: RouteAuthenticated,
	},
	"/api/v1/teams/link/verify": {
		Pattern: "/api/v1/teams/link/verify", RouteID: "teams.link.verify",
		Classification: RouteAuthenticated,
	},
	"/api/v1/teams/link/status": {
		Pattern: "/api/v1/teams/link/status", RouteID: "teams.link.status",
		Classification: RouteAuthenticated,
	},

	// -------------------------------------------------------------------------
	// Policy: Agents
	// -------------------------------------------------------------------------
	"/api/v1/agents": {
		Pattern: "/api/v1/agents", RouteID: "agents.list",
		Classification: RoutePolicy,
		Permission:     "agent.read", Resource: "agent", Action: "read",
	},
	"/api/v1/agents/": {
		Pattern: "/api/v1/agents/", RouteID: "agents.byId",
		Classification: RoutePolicy,
		Permission:     "agent.read", Resource: "agent", Action: "read",
	},

	// -------------------------------------------------------------------------
	// Policy: Projects
	// -------------------------------------------------------------------------
	"/api/v1/projects": {
		Pattern: "/api/v1/projects", RouteID: "projects.list",
		Classification: RoutePolicy,
		Permission:     "project.read", Resource: "project", Action: "read",
	},
	"/api/v1/projects/register": {
		Pattern: "/api/v1/projects/register", RouteID: "projects.register",
		Classification: RoutePolicy,
		Permission:     "project.register", Resource: "project", Action: "register",
	},
	"/api/v1/projects/": {
		Pattern: "/api/v1/projects/", RouteID: "projects.byId",
		Classification: RoutePolicy,
		Permission:     "project.read", Resource: "project", Action: "read",
	},

	// -------------------------------------------------------------------------
	// Policy: Legacy groves (aliases for projects)
	// -------------------------------------------------------------------------
	"/api/v1/groves": {
		Pattern: "/api/v1/groves", RouteID: "groves.list",
		Classification: RoutePolicy,
		Permission:     "project.read", Resource: "project", Action: "read",
	},
	"/api/v1/groves/register": {
		Pattern: "/api/v1/groves/register", RouteID: "groves.register",
		Classification: RoutePolicy,
		Permission:     "project.register", Resource: "project", Action: "register",
	},
	"/api/v1/groves/": {
		Pattern: "/api/v1/groves/", RouteID: "groves.byId",
		Classification: RoutePolicy,
		Permission:     "project.read", Resource: "project", Action: "read",
	},

	// -------------------------------------------------------------------------
	// Policy: Runtime brokers
	// -------------------------------------------------------------------------
	"/api/v1/runtime-brokers": {
		Pattern: "/api/v1/runtime-brokers", RouteID: "runtimeBrokers.list",
		Classification: RoutePolicy,
		Permission:     "broker.read", Resource: "broker", Action: "read",
	},
	"/api/v1/runtime-brokers/": {
		Pattern: "/api/v1/runtime-brokers/", RouteID: "runtimeBrokers.byId",
		Classification: RoutePolicy,
		Permission:     "broker.read", Resource: "broker", Action: "read",
	},

	// -------------------------------------------------------------------------
	// Policy: Templates
	// -------------------------------------------------------------------------
	"/api/v1/templates": {
		Pattern: "/api/v1/templates", RouteID: "templates.list",
		Classification: RoutePolicy,
		Permission:     "template.read", Resource: "template", Action: "read",
	},
	"/api/v1/templates/": {
		Pattern: "/api/v1/templates/", RouteID: "templates.byId",
		Classification: RoutePolicy,
		Permission:     "template.read", Resource: "template", Action: "read",
	},

	// -------------------------------------------------------------------------
	// Policy: GCP service accounts
	// -------------------------------------------------------------------------
	"/api/v1/gcp-service-accounts": {
		Pattern: "/api/v1/gcp-service-accounts", RouteID: "gcpServiceAccounts.list",
		Classification: RoutePolicy,
		Permission:     "gcp_service_account.read", Resource: "gcp_service_account", Action: "read",
	},
	"/api/v1/gcp-service-accounts/": {
		Pattern: "/api/v1/gcp-service-accounts/", RouteID: "gcpServiceAccounts.byId",
		Classification: RoutePolicy,
		Permission:     "gcp_service_account.read", Resource: "gcp_service_account", Action: "read",
	},

	// -------------------------------------------------------------------------
	// Policy: Skills
	// -------------------------------------------------------------------------
	"/api/v1/skills": {
		Pattern: "/api/v1/skills", RouteID: "skills.list",
		Classification: RoutePolicy,
		Permission:     "skill.read", Resource: "skill", Action: "read",
	},
	"/api/v1/skills/": {
		Pattern: "/api/v1/skills/", RouteID: "skills.byId",
		Classification: RoutePolicy,
		Permission:     "skill.read", Resource: "skill", Action: "read",
	},
	"/api/v1/skills/discover-directory": {
		Pattern: "/api/v1/skills/discover-directory", RouteID: "skills.discoverDirectory",
		Classification: RoutePolicy,
		Permission:     "skill.read", Resource: "skill", Action: "read",
	},

	// -------------------------------------------------------------------------
	// Policy: Skill registries
	// -------------------------------------------------------------------------
	"/api/v1/skill-registries": {
		Pattern: "/api/v1/skill-registries", RouteID: "skillRegistries.list",
		Classification: RoutePolicy,
		Permission:     "skill.read", Resource: "skill", Action: "read",
	},
	"/api/v1/skill-registries/": {
		Pattern: "/api/v1/skill-registries/", RouteID: "skillRegistries.byId",
		Classification: RoutePolicy,
		Permission:     "skill.read", Resource: "skill", Action: "read",
	},

	// -------------------------------------------------------------------------
	// Policy: Harness configs
	// -------------------------------------------------------------------------
	"/api/v1/harness-configs": {
		Pattern: "/api/v1/harness-configs", RouteID: "harnessConfigs.list",
		Classification: RoutePolicy,
		Permission:     "harness_config.read", Resource: "harness_config", Action: "read",
	},
	"/api/v1/harness-configs/": {
		Pattern: "/api/v1/harness-configs/", RouteID: "harnessConfigs.byId",
		Classification: RoutePolicy,
		Permission:     "harness_config.read", Resource: "harness_config", Action: "read",
	},

	// -------------------------------------------------------------------------
	// Authenticated: Pre-start hooks (hub-scoped)
	// GET is open to any authenticated user (non-admins see redacted scripts);
	// POST/PUT/DELETE require admin — enforced by the handler.
	// -------------------------------------------------------------------------
	"/api/v1/pre-start-hooks": {
		Pattern: "/api/v1/pre-start-hooks", RouteID: "preStartHooks.list",
		Classification: RouteAuthenticated,
	},
	"/api/v1/pre-start-hooks/": {
		Pattern: "/api/v1/pre-start-hooks/", RouteID: "preStartHooks.byId",
		Classification: RouteAuthenticated,
	},

	// -------------------------------------------------------------------------
	// Policy: Users
	// -------------------------------------------------------------------------
	"/api/v1/users": {
		Pattern: "/api/v1/users", RouteID: "users.list",
		Classification: RoutePolicy,
		Permission:     "user.read", Resource: "user", Action: "read",
	},
	"/api/v1/users/": {
		Pattern: "/api/v1/users/", RouteID: "users.byId",
		Classification: RoutePolicy,
		Permission:     "user.read", Resource: "user", Action: "read",
	},

	// -------------------------------------------------------------------------
	// Policy: Environment variables and secrets
	// -------------------------------------------------------------------------
	"/api/v1/env": {
		Pattern: "/api/v1/env", RouteID: "env.list",
		Classification: RoutePolicy,
		Permission:     "project.read", Resource: "project", Action: "read",
	},
	"/api/v1/env/": {
		Pattern: "/api/v1/env/", RouteID: "env.byKey",
		Classification: RoutePolicy,
		Permission:     "project.read", Resource: "project", Action: "read",
	},
	"/api/v1/secrets": {
		Pattern: "/api/v1/secrets", RouteID: "secrets.list",
		Classification: RoutePolicy,
		Permission:     "project.read", Resource: "project", Action: "read",
	},
	"/api/v1/secrets/": {
		Pattern: "/api/v1/secrets/", RouteID: "secrets.byKey",
		Classification: RoutePolicy,
		Permission:     "project.read", Resource: "project", Action: "read",
	},

	// -------------------------------------------------------------------------
	// Policy: Groups
	// -------------------------------------------------------------------------
	"/api/v1/groups": {
		Pattern: "/api/v1/groups", RouteID: "groups.list",
		Classification: RoutePolicy,
		Permission:     "group.read", Resource: "group", Action: "read",
	},
	"/api/v1/groups/": {
		Pattern: "/api/v1/groups/", RouteID: "groups.byId",
		Classification: RoutePolicy,
		Permission:     "group.read", Resource: "group", Action: "read",
	},

	// -------------------------------------------------------------------------
	// Policy: Chat
	// -------------------------------------------------------------------------
	"/api/v1/chat/prefs": {
		Pattern: "/api/v1/chat/prefs", RouteID: "chat.prefs",
		Classification: RoutePolicy,
		Permission:     "project.read", Resource: "project", Action: "read",
	},
	"/api/v1/chat/threads": {
		Pattern: "/api/v1/chat/threads", RouteID: "chat.threads.list",
		Classification: RoutePolicy,
		Permission:     "project.read", Resource: "project", Action: "read",
	},
	"/api/v1/chat/threads/": {
		Pattern: "/api/v1/chat/threads/", RouteID: "chat.threads.byId",
		Classification: RoutePolicy,
		Permission:     "project.read", Resource: "project", Action: "read",
	},
	"/api/v1/chat/spaces": {
		Pattern: "/api/v1/chat/spaces", RouteID: "chat.spaces.list",
		Classification: RoutePolicy,
		Permission:     "project.read", Resource: "project", Action: "read",
	},
	"/api/v1/chat/spaces/": {
		Pattern: "/api/v1/chat/spaces/", RouteID: "chat.spaces.byId",
		Classification: RoutePolicy,
		Permission:     "project.read", Resource: "project", Action: "read",
	},
	"/api/v1/chat/conversations/": {
		Pattern: "/api/v1/chat/conversations/", RouteID: "chat.conversations.byId",
		Classification: RoutePolicy,
		Permission:     "project.read", Resource: "project", Action: "read",
	},
	"/api/v1/chat/topics/": {
		Pattern: "/api/v1/chat/topics/", RouteID: "chat.topics.byId",
		Classification: RoutePolicy,
		Permission:     "project.read", Resource: "project", Action: "read",
	},
	"/api/v1/chat/dms": {
		Pattern: "/api/v1/chat/dms", RouteID: "chat.dms",
		Classification: RoutePolicy,
		Permission:     "project.read", Resource: "project", Action: "read",
	},
	"/api/v1/chat/search": {
		Pattern: "/api/v1/chat/search", RouteID: "chat.search",
		Classification: RoutePolicy,
		Permission:     "project.read", Resource: "project", Action: "read",
	},
	"/api/v1/chat/attachments": {
		Pattern: "/api/v1/chat/attachments", RouteID: "chat.attachments.list",
		Classification: RoutePolicy,
		Permission:     "project.read", Resource: "project", Action: "read",
	},
	"/api/v1/chat/attachments/": {
		Pattern: "/api/v1/chat/attachments/", RouteID: "chat.attachments.byId",
		Classification: RoutePolicy,
		Permission:     "project.read", Resource: "project", Action: "read",
	},

	// -------------------------------------------------------------------------
	// Policy: Resource import/discover
	// -------------------------------------------------------------------------
	"/api/v1/resources/import": {
		Pattern: "/api/v1/resources/import", RouteID: "resources.import",
		Classification: RoutePolicy,
		Permission:     "template.create", Resource: "template", Action: "create",
	},
	"/api/v1/resources/discover": {
		Pattern: "/api/v1/resources/discover", RouteID: "resources.discover",
		Classification: RoutePolicy,
		Permission:     "template.read", Resource: "template", Action: "read",
	},

	// -------------------------------------------------------------------------
	// Hub admin: Policies
	// -------------------------------------------------------------------------
	"/api/v1/policies": {
		Pattern: "/api/v1/policies", RouteID: "policies.list",
		Classification: RouteHubAdmin,
	},
	"/api/v1/policies/": {
		Pattern: "/api/v1/policies/", RouteID: "policies.byId",
		Classification: RouteHubAdmin,
	},

	// -------------------------------------------------------------------------
	// Authenticated: Authorization explain endpoint
	// -------------------------------------------------------------------------
	"/api/v1/authz/explain": {
		Pattern: "/api/v1/authz/explain", RouteID: "authz.explain",
		Classification: RouteAuthenticated,
	},

	// -------------------------------------------------------------------------
	// Authenticated: Hub injected skills
	// GET is open to any authenticated user; PUT requires admin — enforced by handler.
	// -------------------------------------------------------------------------
	"/api/v1/hub/settings/injected-skills": {
		Pattern: "/api/v1/hub/settings/injected-skills", RouteID: "hub.settings.injectedSkills",
		Classification: RouteAuthenticated,
	},

	// -------------------------------------------------------------------------
	// Hub admin: System administration
	// -------------------------------------------------------------------------
	"/api/v1/admin/maintenance": {
		Pattern: "/api/v1/admin/maintenance", RouteID: "admin.maintenance",
		Classification: RouteHubAdmin,
	},
	"/api/v1/admin/maintenance/operations": {
		Pattern: "/api/v1/admin/maintenance/operations", RouteID: "admin.maintenance.operations",
		Classification: RouteHubAdmin,
	},
	"/api/v1/admin/maintenance/operations/": {
		Pattern: "/api/v1/admin/maintenance/operations/", RouteID: "admin.maintenance.operations.byId",
		Classification: RouteHubAdmin,
	},
	"/api/v1/admin/maintenance/migrations/": {
		Pattern: "/api/v1/admin/maintenance/migrations/", RouteID: "admin.maintenance.migrations.byId",
		Classification: RouteHubAdmin,
	},
	"/api/v1/admin/maintenance/check-updates": {
		Pattern: "/api/v1/admin/maintenance/check-updates", RouteID: "admin.maintenance.checkUpdates",
		Classification: RouteHubAdmin,
	},
	"/api/v1/admin/maintenance/restart": {
		Pattern: "/api/v1/admin/maintenance/restart", RouteID: "admin.maintenance.restart",
		Classification: RouteHubAdmin,
	},
	"/api/v1/admin/scheduler": {
		Pattern: "/api/v1/admin/scheduler", RouteID: "admin.scheduler",
		Classification: RouteHubAdmin,
	},
	"/api/v1/admin/allow-list": {
		Pattern: "/api/v1/admin/allow-list", RouteID: "admin.allowList",
		Classification: RouteHubAdmin,
	},
	"/api/v1/admin/allow-list/": {
		Pattern: "/api/v1/admin/allow-list/", RouteID: "admin.allowList.byEmail",
		Classification: RouteHubAdmin,
	},
	"/api/v1/admin/users/invite/bulk": {
		Pattern: "/api/v1/admin/users/invite/bulk", RouteID: "admin.users.invite.bulk",
		Classification: RouteHubAdmin,
	},
	"/api/v1/admin/users/invite": {
		Pattern: "/api/v1/admin/users/invite", RouteID: "admin.users.invite",
		Classification: RouteHubAdmin,
	},
	"/api/v1/admin/invites": {
		Pattern: "/api/v1/admin/invites", RouteID: "admin.invites",
		Classification: RouteHubAdmin,
	},
	"/api/v1/admin/invites/": {
		Pattern: "/api/v1/admin/invites/", RouteID: "admin.invites.byId",
		Classification: RouteHubAdmin,
	},
	"/api/v1/admin/server-config/schema": {
		Pattern: "/api/v1/admin/server-config/schema", RouteID: "admin.serverConfig.schema",
		Classification: RouteHubAdmin,
	},
	"/api/v1/admin/server-config/sections/": {
		Pattern: "/api/v1/admin/server-config/sections/", RouteID: "admin.serverConfig.sections.byId",
		Classification: RouteHubAdmin,
	},
	"/api/v1/admin/server-config": {
		Pattern: "/api/v1/admin/server-config", RouteID: "admin.serverConfig",
		Classification: RouteHubAdmin,
	},
	"/api/v1/admin/project-defaults": {
		Pattern: "/api/v1/admin/project-defaults", RouteID: "admin.projectDefaults",
		Classification: RouteHubAdmin,
	},
	"/api/v1/admin/agents/reset-auth-all": {
		Pattern: "/api/v1/admin/agents/reset-auth-all", RouteID: "admin.agents.resetAuthAll",
		Classification: RouteHubAdmin,
	},
	"/api/v1/admin/gcp-quota": {
		Pattern: "/api/v1/admin/gcp-quota", RouteID: "admin.gcpQuota",
		Classification: RouteHubAdmin,
	},
	"/api/v1/admin/lifecycle-hooks": {
		Pattern: "/api/v1/admin/lifecycle-hooks", RouteID: "admin.lifecycleHooks",
		Classification: RouteHubAdmin,
	},
	"/api/v1/admin/lifecycle-hooks/": {
		Pattern: "/api/v1/admin/lifecycle-hooks/", RouteID: "admin.lifecycleHooks.byId",
		Classification: RouteHubAdmin,
	},
	"/api/v1/admin/validate-resources": {
		Pattern: "/api/v1/admin/validate-resources", RouteID: "admin.validateResources",
		Classification: RouteHubAdmin,
	},
	"/api/v1/admin/integrations": {
		Pattern: "/api/v1/admin/integrations", RouteID: "admin.integrations",
		Classification: RouteHubAdmin,
	},
	"/api/v1/admin/integrations/teams/manifest": {
		Pattern: "/api/v1/admin/integrations/teams/manifest", RouteID: "admin.integrations.teamsManifest",
		Classification: RouteHubAdmin,
	},
	"/api/v1/admin/integrations/": {
		Pattern: "/api/v1/admin/integrations/", RouteID: "admin.integrations.byName",
		Classification: RouteHubAdmin,
	},
	"/api/v1/admin/diagnostics/logs/stream": {
		Pattern: "/api/v1/admin/diagnostics/logs/stream", RouteID: "admin.diagnostics.logsStream",
		Classification: RouteHubAdmin,
	},
	"/api/v1/admin/diagnostics/logs": {
		Pattern: "/api/v1/admin/diagnostics/logs", RouteID: "admin.diagnostics.logs",
		Classification: RouteHubAdmin,
	},
	"/api/v1/admin/health/summary": {
		Pattern: "/api/v1/admin/health/summary", RouteID: "admin.health.summary",
		Classification: RouteHubAdmin,
	},
	"/api/v1/metrics/": {
		Pattern: "/api/v1/metrics/", RouteID: "admin.metricsDashboard",
		Classification: RouteHubAdmin,
	},
	"/api/v1/admin/metrics-dashboard": {
		Pattern: "/api/v1/admin/metrics-dashboard", RouteID: "admin.metricsDashboard.legacy",
		Classification: RouteHubAdmin,
	},

	// -------------------------------------------------------------------------
	// Hub admin: GitHub App
	// -------------------------------------------------------------------------
	"/api/v1/github-app": {
		Pattern: "/api/v1/github-app", RouteID: "githubApp.config",
		Classification: RouteHubAdmin,
	},
	"/api/v1/github-app/installations": {
		Pattern: "/api/v1/github-app/installations", RouteID: "githubApp.installations",
		Classification: RouteHubAdmin,
	},
	"/api/v1/github-app/installations/": {
		Pattern: "/api/v1/github-app/installations/", RouteID: "githubApp.installations.byId",
		Classification: RouteHubAdmin,
	},
	"/api/v1/github-app/installations/discover": {
		Pattern: "/api/v1/github-app/installations/discover", RouteID: "githubApp.installations.discover",
		Classification: RouteHubAdmin,
	},
	"/api/v1/github-app/sync-permissions": {
		Pattern: "/api/v1/github-app/sync-permissions", RouteID: "githubApp.syncPermissions",
		Classification: RouteHubAdmin,
	},

	// -------------------------------------------------------------------------
	// Broker HMAC: Registration and lifecycle
	// -------------------------------------------------------------------------
	"/api/v1/brokers": {
		Pattern: "/api/v1/brokers", RouteID: "brokers.list",
		Classification: RouteBrokerHMAC,
	},
	"/api/v1/brokers/join": {
		Pattern: "/api/v1/brokers/join", RouteID: "brokers.join",
		Classification: RouteBrokerHMAC,
	},
	"/api/v1/brokers/": {
		Pattern: "/api/v1/brokers/", RouteID: "brokers.byId",
		Classification: RouteBrokerHMAC,
	},
	"/api/v1/broker/inbound": {
		Pattern: "/api/v1/broker/inbound", RouteID: "broker.inbound",
		Classification: RouteBrokerHMAC,
	},
	"/api/v1/broker/projects": {
		Pattern: "/api/v1/broker/projects", RouteID: "broker.projects",
		Classification: RouteBrokerHMAC,
	},
	"/api/v1/runtime-brokers/connect": {
		Pattern: "/api/v1/runtime-brokers/connect", RouteID: "runtimeBrokers.connect",
		Classification: RouteBrokerHMAC,
	},

	// -------------------------------------------------------------------------
	// Agent token: GCP identity and OIDC
	// -------------------------------------------------------------------------
	"/api/v1/agent/gcp-token": {
		Pattern: "/api/v1/agent/gcp-token", RouteID: "agent.gcpToken",
		Classification: RouteAgentToken,
	},
	"/api/v1/agent/gcp-identity-token": {
		Pattern: "/api/v1/agent/gcp-identity-token", RouteID: "agent.gcpIdentityToken",
		Classification: RouteAgentToken,
	},
	"POST /api/v1/agent/identity-token": {
		Pattern: "POST /api/v1/agent/identity-token", RouteID: "agent.identityToken",
		Classification: RouteAgentToken,
	},

	// -------------------------------------------------------------------------
	// Workstation: System endpoints
	// -------------------------------------------------------------------------
	"/api/v1/system/identity": {
		Pattern: "/api/v1/system/identity", RouteID: "system.identity",
		Classification: RouteWorkstation,
	},
	"/api/v1/system/status": {
		Pattern: "/api/v1/system/status", RouteID: "system.status",
		Classification: RouteWorkstation,
	},
	"/api/v1/system/check": {
		Pattern: "/api/v1/system/check", RouteID: "system.check",
		Classification: RouteWorkstation,
	},
	"/api/v1/system/runtime": {
		Pattern: "/api/v1/system/runtime", RouteID: "system.runtime",
		Classification: RouteWorkstation,
	},
	"/api/v1/system/init": {
		Pattern: "/api/v1/system/init", RouteID: "system.init",
		Classification: RouteWorkstation,
	},
	"/api/v1/system/images/pull": {
		Pattern: "/api/v1/system/images/pull", RouteID: "system.images.pull",
		Classification: RouteWorkstation,
	},
	"/api/v1/system/images/build": {
		Pattern: "/api/v1/system/images/build", RouteID: "system.images.build",
		Classification: RouteWorkstation,
	},
	"/api/v1/system/apple-dns": {
		Pattern: "/api/v1/system/apple-dns", RouteID: "system.appleDns",
		Classification: RouteWorkstation,
	},
	"/api/v1/system/registry": {
		Pattern: "/api/v1/system/registry", RouteID: "system.registry",
		Classification: RouteWorkstation,
	},
	"/api/v1/system/workstation-settings": {
		Pattern: "/api/v1/system/workstation-settings", RouteID: "system.workstationSettings",
		Classification: RouteWorkstation,
	},
	"/api/v1/system/fs/list": {
		Pattern: "/api/v1/system/fs/list", RouteID: "system.fs.list",
		Classification: RouteWorkstation,
	},
	"/api/v1/system/fs/mkdir": {
		Pattern: "/api/v1/system/fs/mkdir", RouteID: "system.fs.mkdir",
		Classification: RouteWorkstation,
	},
	"/api/v1/system/fs/validate-path": {
		Pattern: "/api/v1/system/fs/validate-path", RouteID: "system.fs.validatePath",
		Classification: RouteWorkstation,
	},

	// -------------------------------------------------------------------------
	// Webhook: GitHub webhook
	// -------------------------------------------------------------------------
	"/api/v1/webhooks/github": {
		Pattern: "/api/v1/webhooks/github", RouteID: "webhooks.github",
		Classification: RouteWebhook,
	},
}

// guarded looks up the route metadata for a pattern and wraps the handler with
// the declarative route guard. If the pattern has no entry in routeMetadataTable,
// it returns a handler that fails closed with 500.
func (s *Server) guarded(pattern string, handler http.HandlerFunc) http.HandlerFunc {
	meta, ok := routeMetadataTable[pattern]
	if !ok {
		return func(w http.ResponseWriter, r *http.Request) {
			writeError(w, http.StatusInternalServerError, ErrCodeRuntimeError, "route not in metadata table: "+pattern, nil)
		}
	}
	return s.routeGuard(meta, handler)
}

// routeGuard wraps a handler with declarative authorization derived from route metadata.
// It runs BEFORE the handler. The handler's own authorization checks remain as defense-in-depth.
func (s *Server) routeGuard(meta RouteMetadata, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch meta.Classification {
		case RoutePublic:
			// No guard — pass through
			next(w, r)
		case RouteAuthenticated:
			identity := GetIdentityFromContext(r.Context())
			if identity == nil {
				writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "authentication required", nil)
				return
			}
			next(w, r)
		case RoutePolicy:
			// Policy routes support multiple auth models (user, agent, broker) and
			// some support anonymous access for public resources (e.g. skills).
			// The handler performs per-resource authorization with full context.
			// The declarative guard classifies the route; enforcement stays in
			// the handler where resource IDs, ownership, and visibility are known.
			next(w, r)
		case RouteHubAdmin:
			// Delegate to existing requireAdmin — already handles scoped/federated rejection
			if _, ok := s.requireAdmin(w, r); !ok {
				return
			}
			next(w, r)
		case RouteWorkstation:
			// Delegate to existing requireWorkstation check
			s.requireWorkstation(http.HandlerFunc(next)).ServeHTTP(w, r)
		case RouteBrokerHMAC:
			// Broker routes are already wrapped with broker auth middleware — pass through
			next(w, r)
		case RouteAgentToken:
			// Agent token routes verify identity in middleware — just ensure identity present
			identity := GetIdentityFromContext(r.Context())
			if identity == nil {
				writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "authentication required", nil)
				return
			}
			next(w, r)
		case RouteWebhook:
			// Webhook routes verify signatures in the handler — pass through
			next(w, r)
		default:
			// Fail closed for unknown classification
			writeError(w, http.StatusInternalServerError, ErrCodeRuntimeError, "route misconfigured", nil)
		}
	}
}
