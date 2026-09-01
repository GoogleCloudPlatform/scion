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
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
)

var routePermissionClassifications = map[string]string{
	"/healthz":                                  "public:health",
	"/readyz":                                   "public:health",
	"/metrics":                                  "public:metrics",
	"/api/v1/auth/login":                        "public:auth",
	"/api/v1/auth/token":                        "public:auth",
	"/api/v1/auth/refresh":                      "public:auth",
	"/api/v1/auth/validate":                     "public:auth",
	"/api/v1/auth/admin-status":                 "authenticated:user",
	"/api/v1/auth/logout":                       "authenticated:user",
	"/api/v1/auth/me":                           "authenticated:user",
	"/api/v1/auth/tokens":                       "authenticated:user-token",
	"/api/v1/auth/tokens/":                      "authenticated:user-token",
	"/api/v1/auth/scopes":                       "authenticated:user-token",
	"/api/v1/auth/providers":                    "public:auth",
	"/api/v1/auth/invite/redeem":                "public:invite",
	"/api/v1/auth/cli/authorize":                "public:cli-oauth",
	"/api/v1/auth/cli/token":                    "public:cli-oauth",
	"/api/v1/auth/cli/device":                   "public:cli-oauth",
	"/api/v1/auth/cli/device/token":             "public:cli-oauth",
	"/api/v1/agents":                            "policy:agent",
	"/api/v1/agents/":                           "policy:agent",
	"/api/v1/projects":                          "policy:project",
	"/api/v1/projects/register":                 "policy:project-register",
	"/api/v1/projects/":                         "policy:project-subroute",
	"/api/v1/groves":                            "policy:legacy-project",
	"/api/v1/groves/register":                   "policy:legacy-project-register",
	"/api/v1/groves/":                           "policy:legacy-project-subroute",
	"/api/v1/runtime-brokers":                   "policy:broker",
	"/api/v1/runtime-brokers/":                  "policy:broker",
	"/api/v1/templates":                         "policy:template",
	"/api/v1/templates/":                        "policy:template",
	"/api/v1/gcp-service-accounts":              "policy:gcp-service-account",
	"/api/v1/gcp-service-accounts/":             "policy:gcp-service-account",
	"/api/v1/skills":                            "policy:skill",
	"/api/v1/skills/":                           "policy:skill",
	"/api/v1/skill-registries":                  "hub-admin:skill-registry",
	"/api/v1/skill-registries/":                 "hub-admin:skill-registry",
	"/api/v1/harness-configs":                   "policy:harness-config",
	"/api/v1/harness-configs/":                  "policy:harness-config",
	"/api/v1/pre-start-hooks":                   "policy:pre-start-hook",
	"/api/v1/pre-start-hooks/":                  "policy:pre-start-hook",
	"/api/v1/users":                             "policy:user",
	"/api/v1/users/":                            "policy:user",
	"/api/v1/env":                               "policy:env",
	"/api/v1/env/":                              "policy:env",
	"/api/v1/secrets":                           "policy:secret",
	"/api/v1/secrets/":                          "policy:secret",
	"/api/v1/metrics/session/":                  "authenticated:session-metrics",
	"/api/v1/groups":                            "policy:group",
	"/api/v1/groups/":                           "policy:group",
	"/api/v1/policies":                          "authenticated:policy-gone",
	"/api/v1/policies/":                         "authenticated:policy-gone",
	"/api/v1/users/me/groups":                   "authenticated:principal",
	"/api/v1/principals/":                       "authenticated:principal",
	"/api/v1/users/me/injected-skills":          "authenticated:injected-skills",
	"/api/v1/users/me/injected-skills/":         "authenticated:injected-skills",
	"/api/v1/users/me/templates":                "authenticated:user-templates",
	"/api/v1/users/me/templates/":               "authenticated:user-templates",
	"/api/v1/hub/settings/injected-skills":      "authenticated:injected-skills",
	"/api/v1/brokers":                           "broker-hmac:registration",
	"/api/v1/brokers/join":                      "broker-hmac:registration",
	"/api/v1/brokers/":                          "broker-hmac:broker",
	"/api/v1/broker/inbound":                    "broker-hmac:inbound",
	"/api/v1/broker/projects":                   "broker-hmac:projects",
	"/api/v1/admin/maintenance":                 "hub-admin:maintenance",
	"/api/v1/admin/maintenance/operations":      "hub-admin:maintenance",
	"/api/v1/admin/maintenance/operations/":     "hub-admin:maintenance",
	"/api/v1/admin/maintenance/migrations/":     "hub-admin:maintenance",
	"/api/v1/admin/maintenance/check-updates":   "hub-admin:maintenance",
	"/api/v1/admin/maintenance/restart":         "hub-admin:maintenance",
	"/api/v1/admin/scheduler":                   "hub-admin:scheduler",
	"/api/v1/admin/allow-list":                  "hub-admin:allow-list",
	"/api/v1/admin/allow-list/":                 "hub-admin:allow-list",
	"/api/v1/admin/users/invite/bulk":           "hub-admin:invite",
	"/api/v1/admin/users/invite":                "hub-admin:invite",
	"/api/v1/admin/invites":                     "hub-admin:invite",
	"/api/v1/admin/invites/":                    "hub-admin:invite",
	"/api/v1/admin/server-config/schema":        "hub-admin:server-config",
	"/api/v1/admin/server-config/sections/":     "hub-admin:server-config",
	"/api/v1/admin/server-config":               "hub-admin:server-config",
	"/api/v1/admin/project-defaults":            "hub-admin:project-defaults",
	"/api/v1/admin/agents/reset-auth-all":       "hub-admin:agent-reset",
	"/api/v1/admin/gcp-quota":                   "hub-admin:gcp-quota",
	"/api/v1/admin/lifecycle-hooks":             "hub-admin:lifecycle-hook",
	"/api/v1/admin/lifecycle-hooks/":            "hub-admin:lifecycle-hook",
	"/api/v1/admin/validate-resources":          "hub-admin:resource-validation",
	"/api/v1/admin/integrations":                "hub-admin:integration",
	"/api/v1/admin/integrations/teams/manifest": "hub-admin:integration",
	"/api/v1/admin/integrations/":               "hub-admin:integration",
	"/api/v1/admin/diagnostics/logs/stream":     "hub-admin:diagnostics",
	"/api/v1/admin/diagnostics/logs":            "hub-admin:diagnostics",
	"/api/v1/admin/health/summary":              "hub-admin:health",
	"/api/v1/admin/messaging/divergence":        "hub-admin:diagnostics",
	"/api/v1/metrics/":                          "hub-admin:metrics-dashboard",
	"/api/v1/admin/metrics-dashboard":           "hub-admin:metrics-dashboard",
	"/api/v1/notifications":                     "authenticated:notifications",
	"/api/v1/notifications/":                    "authenticated:notifications",
	"/api/v1/messages":                          "authenticated:messages",
	"/api/v1/messages/":                         "authenticated:messages",
	"/api/v1/message-channels":                  "authenticated:messages",
	"/api/v1/chat/prefs":                        "policy:chat",
	"/api/v1/chat/threads":                      "policy:chat",
	"/api/v1/chat/threads/":                     "policy:chat",
	"/api/v1/chat/spaces":                       "policy:chat",
	"/api/v1/chat/spaces/":                      "policy:chat",
	"/api/v1/chat/conversations/":               "policy:chat",
	"/api/v1/chat/topics/":                      "policy:chat",
	"/api/v1/chat/dms":                          "policy:chat",
	"/api/v1/chat/user-prefs":                   "authenticated:chat-prefs",
	"/api/v1/chat/presence":                     "authenticated:chat-presence",
	"/api/v1/chat/search":                       "policy:chat",
	"/api/v1/chat/attachments":                  "policy:chat-attachment",
	"/api/v1/chat/attachments/":                 "policy:chat-attachment",
	"/api/v1/runtime-brokers/connect":           "broker-hmac:control-channel",
	"/api/v1/agent/gcp-token":                   "agent-token:gcp-token",
	"/api/v1/agent/gcp-identity-token":          "agent-token:gcp-token",
	"/api/v1/settings/public":                   "public:settings",
	"/api/v1/github-app":                        "hub-admin:github-app",
	"/api/v1/github-app/installations":          "hub-admin:github-app",
	"/api/v1/github-app/installations/":         "hub-admin:github-app",
	"/api/v1/github-app/installations/discover": "hub-admin:github-app",
	"/api/v1/github-app/sync-permissions":       "hub-admin:github-app",
	"/api/v1/telegram/link":                     "authenticated:account-link",
	"/api/v1/telegram/link/verify":              "authenticated:account-link",
	"/api/v1/telegram/link/status":              "authenticated:account-link",
	"/api/v1/discord/link":                      "authenticated:account-link",
	"/api/v1/discord/link/verify":               "authenticated:account-link",
	"/api/v1/discord/link/status":               "authenticated:account-link",
	"/api/v1/teams/link":                        "authenticated:account-link",
	"/api/v1/teams/link/verify":                 "authenticated:account-link",
	"/api/v1/teams/link/status":                 "authenticated:account-link",
	"/api/v1/resources/import":                  "policy:resource-import",
	"/api/v1/resources/discover":                "policy:resource-import",
	"/api/v1/skills/discover-directory":         "policy:skill",
	"/api/v1/webhooks/github":                   "webhook-signature:github",
	"/github-app/setup":                         "public:github-app",
	"/api/v1/system/identity":                   "workstation:system",
	"/api/v1/system/status":                     "workstation:system",
	"/api/v1/system/check":                      "workstation:system",
	"/api/v1/system/runtime":                    "workstation:system",
	"/api/v1/system/init":                       "workstation:system",
	"/api/v1/system/images/pull":                "workstation:system",
	"/api/v1/system/images/build":               "workstation:system",
	"/api/v1/system/apple-dns":                  "workstation:system",
	"/api/v1/system/registry":                   "workstation:system",
	"/api/v1/system/workstation-settings":       "workstation:system",
	"GET /.well-known/openid-configuration":     "public:oidc",
	"GET /.well-known/jwks.json":                "public:oidc",
	"POST /api/v1/agent/identity-token":         "agent-token:identity-token",
	"/api/v1/system/fs/list":                    "workstation:filesystem",
	"/api/v1/system/fs/mkdir":                   "workstation:filesystem",
	"/api/v1/system/fs/validate-path":           "workstation:filesystem",
	"/api/v1/authz/explain":                     "authenticated:authz-explain",
	"/api/v1/admin/limits":                      "hub-admin:quota",
	"/api/v1/admin/limits/":                     "hub-admin:quota",
	"/api/v1/admin/entitlements/":               "hub-admin:quota",
	"/api/v1/admin/usage":                       "hub-admin:quota",
	"/api/v1/admin/usage/":                      "hub-admin:quota",
	"/api/v1/usage/me":                          "authenticated:quota-usage",
	// Role management (PR-C1)
	"/api/v1/admin/roles":          "hub-admin:role",
	"/api/v1/admin/roles/":         "hub-admin:role",
	"/api/v1/admin/role-bindings":  "hub-admin:role_binding",
	"/api/v1/admin/role-bindings/": "hub-admin:role_binding",
	"/api/v1/admin/permissions":    "hub-admin:role",

	// Access Constraints (AC1)
	"/api/v1/admin/access-constraints":  "hub-admin:access_constraint",
	"/api/v1/admin/access-constraints/": "hub-admin:access_constraint",

	// Access Constraint Previews (B7)
	"/api/v1/admin/access-constraint-previews":  "hub-admin:access_constraint",
	"/api/v1/admin/access-constraint-previews/": "hub-admin:access_constraint",
}

func TestRegisteredRoutesHavePermissionClassification(t *testing.T) {
	source, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("read server.go: %v", err)
	}

	re := regexp.MustCompile(`s\.mux\.Handle(?:Func)?\("([^"]+)"`)
	matches := re.FindAllStringSubmatch(string(source), -1)
	if len(matches) == 0 {
		t.Fatal("no registered routes found in server.go")
	}

	registered := make(map[string]bool, len(matches))
	for _, match := range matches {
		registered[match[1]] = true
	}

	var missing []string
	for route := range registered {
		if routePermissionClassifications[route] == "" {
			missing = append(missing, route)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("registered routes missing permission classification: %v", missing)
	}

	var stale []string
	for route := range routePermissionClassifications {
		if !registered[route] {
			stale = append(stale, route)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Fatalf("permission classifications for unregistered routes: %v", stale)
	}
}

func TestRegisteredRoutesHaveRouteMetadata(t *testing.T) {
	source, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("read server.go: %v", err)
	}

	re := regexp.MustCompile(`s\.mux\.Handle(?:Func)?\("([^"]+)"`)
	matches := re.FindAllStringSubmatch(string(source), -1)
	if len(matches) == 0 {
		t.Fatal("no registered routes found in server.go")
	}

	registered := make(map[string]bool, len(matches))
	for _, match := range matches {
		registered[match[1]] = true
	}

	var missing []string
	for route := range registered {
		meta, ok := routeMetadataTable[route]
		if !ok {
			missing = append(missing, route)
			continue
		}
		if meta.Classification == "" {
			missing = append(missing, route+" (empty classification)")
		}
		if meta.RouteID == "" {
			missing = append(missing, route+" (empty routeID)")
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("registered routes missing route metadata: %v", missing)
	}

	var stale []string
	for route := range routeMetadataTable {
		if !registered[route] {
			stale = append(stale, route)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Fatalf("route metadata for unregistered routes: %v", stale)
	}
}

func TestRouteMetadataPermissionsAreValid(t *testing.T) {
	// Build set of valid permission IDs from the registry.
	validPermissions := make(map[string]bool)
	for _, perm := range permissions.Registry {
		validPermissions[perm.ID] = true
	}

	var invalid []string
	for pattern, meta := range routeMetadataTable {
		if meta.Classification != RoutePolicy {
			continue
		}
		if meta.Permission == "" {
			invalid = append(invalid, pattern+": empty permission")
			continue
		}
		if !validPermissions[meta.Permission] {
			invalid = append(invalid, pattern+": unknown permission "+meta.Permission)
		}
		if meta.Resource == "" {
			invalid = append(invalid, pattern+": empty resource")
		}
		if meta.Action == "" {
			invalid = append(invalid, pattern+": empty action")
		}
	}
	sort.Strings(invalid)
	if len(invalid) > 0 {
		t.Fatalf("invalid route metadata permissions:\n%s", strings.Join(invalid, "\n"))
	}
}

func TestRouteGuardsDenyUnauthorized(t *testing.T) {
	srv := &Server{config: DefaultServerConfig(), mux: http.NewServeMux()}
	srv.registerRoutes()

	// Test representative routes from each classification.
	tests := []struct {
		name           string
		route          string
		classification RouteClassification
		identity       Identity
		wantStatus     int
	}{
		// RouteAuthenticated: no identity → 401
		{
			name:           "authenticated route denies unauthenticated",
			route:          "/api/v1/auth/logout",
			classification: RouteAuthenticated,
			identity:       nil,
			wantStatus:     http.StatusUnauthorized,
		},
		// RouteHubAdmin: non-admin user → 403
		// Uses a route without Permission (requireAdmin fallback) so this test
		// works without an authzService. Permission-based routes are tested in
		// TestRouteGuardOpsPermissions with a full server.
		{
			name:           "hub-admin route denies non-admin",
			route:          "/api/v1/admin/allow-list",
			classification: RouteHubAdmin,
			identity:       NewAuthenticatedUser("user-1", "user@example.com", "User", "member", "api"),
			wantStatus:     http.StatusForbidden,
		},
		// RouteHubAdmin: no identity → 401
		{
			name:           "hub-admin route denies unauthenticated",
			route:          "/api/v1/admin/allow-list",
			classification: RouteHubAdmin,
			identity:       nil,
			wantStatus:     http.StatusUnauthorized,
		},
		// RouteAgentToken: no identity → 401
		{
			name:           "agent-token route denies unauthenticated",
			route:          "/api/v1/agent/gcp-token",
			classification: RouteAgentToken,
			identity:       nil,
			wantStatus:     http.StatusUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			if tt.identity != nil {
				ctx = contextWithIdentity(ctx, tt.identity)
			}
			ctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
			defer cancel()

			req := httptest.NewRequest(http.MethodGet, tt.route, nil)
			req = req.WithContext(ctx)
			rr := httptest.NewRecorder()

			srv.mux.ServeHTTP(rr, req)

			if rr.Code != tt.wantStatus {
				t.Fatalf("route %s returned %d, want %d: %s", tt.route, rr.Code, tt.wantStatus, rr.Body.String())
			}
		})
	}
}

// allHubAdminRoutes returns all routes classified as hub-admin, sorted.
func allHubAdminRoutes() []string {
	routes := make([]string, 0)
	for route, classification := range routePermissionClassifications {
		if strings.HasPrefix(classification, "hub-admin:") {
			routes = append(routes, route)
		}
	}
	sort.Strings(routes)
	return routes
}

func TestHubAdminRoutesRejectScopedAdminUAT(t *testing.T) {
	srv := &Server{config: DefaultServerConfig(), mux: http.NewServeMux(), authzService: NewAuthzService(nil, nil)}
	srv.registerRoutes()

	admin := NewAuthenticatedUser("admin-uat", "admin-uat@example.com", "Admin UAT", "admin", "api")
	scopedAdmin := NewScopedUserIdentity(admin, "project-1", []string{"agent:create", "project:read", "policy:manage"})

	routes := allHubAdminRoutes()

	for _, route := range routes {
		t.Run(route, func(t *testing.T) {
			// Skip routes that have Permission set — they require an authzService
			// to exercise the Decide path. These are tested in
			// TestRouteGuardOpsPermissions with a full server.
			if meta, ok := routeMetadataTable[route]; ok && meta.Permission != "" {
				t.Skipf("skipping permission-based route %s (tested in TestRouteGuardOpsPermissions)", route)
				return
			}

			method, path, body := scopedAdminUATRouteRequest(route)
			ctx, cancel := context.WithTimeout(contextWithIdentity(context.Background(), scopedAdmin), 200*time.Millisecond)
			defer cancel()
			req := httptest.NewRequest(method, path, body)
			req.Header.Set("Content-Type", "application/json")
			req = req.WithContext(ctx)
			rr := httptest.NewRecorder()

			srv.mux.ServeHTTP(rr, req)

			if rr.Code != http.StatusForbidden {
				t.Fatalf("scoped admin UAT to %s %s returned %d, want 403: %s", method, path, rr.Code, rr.Body.String())
			}
		})
	}
}

func scopedAdminUATRouteRequest(route string) (string, string, *bytes.Reader) {
	method := http.MethodGet
	path := route
	body := ""

	switch route {
	case "/api/v1/admin/users/invite", "/api/v1/admin/users/invite/bulk",
		"/api/v1/admin/agents/reset-auth-all", "/api/v1/admin/maintenance/check-updates",
		"/api/v1/admin/maintenance/restart", "/api/v1/github-app/installations/discover",
		"/api/v1/github-app/sync-permissions":
		method = http.MethodPost
	case "/api/v1/admin/server-config", "/api/v1/admin/project-defaults":
		method = http.MethodPut
		body = "{}"
	case "/api/v1/hub/settings/injected-skills":
		method = http.MethodPut
		body = `{"user_defined":[]}`
	}

	if strings.HasSuffix(path, "/") {
		path += "scoped-admin-uat-test"
	}

	return method, path, bytes.NewReader([]byte(body))
}
