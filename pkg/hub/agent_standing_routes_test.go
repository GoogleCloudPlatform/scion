//go:build !no_sqlite

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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAgentStanding_RouteGuardPolicies proves that routeGuard structurally
// enforces delegation standing on RouteAgentToken routes whose policy is
// AgentStandingGuard or unset (fail-closed), while passing AgentStandingHandler
// and AgentStandingExempt through to the wrapped handler.
func TestAgentStanding_RouteGuardPolicies(t *testing.T) {
	for _, tc := range []struct {
		name              string
		policy            AgentStandingPolicy
		reason            string
		expectOrphanBlock bool
	}{
		{
			name:              "AgentStandingGuard refuses orphaned agent before handler runs",
			policy:            AgentStandingGuard,
			expectOrphanBlock: true,
		},
		{
			name:              "unset AgentStanding fails closed and refuses orphaned agent",
			policy:            "",
			expectOrphanBlock: true,
		},
		{
			name:              "AgentStandingHandler defers standing check to handler",
			policy:            AgentStandingHandler,
			reason:            "handler checks rate limit before standing",
			expectOrphanBlock: false,
		},
		{
			name:              "AgentStandingExempt allows request through to handler",
			policy:            AgentStandingExempt,
			reason:            "non-credential route",
			expectOrphanBlock: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newMSFixture(t, "rg-"+string(tc.policy))
			ctx := context.Background()

			called := 0
			meta := RouteMetadata{
				Pattern:             "POST /api/v1/agent/test-synthetic-token",
				RouteID:             "agent.testSyntheticToken",
				Classification:      RouteAgentToken,
				AgentStanding:       tc.policy,
				AgentStandingReason: tc.reason,
			}
			guarded := f.srv.routeGuard(meta, func(w http.ResponseWriter, _ *http.Request) {
				called++
				w.WriteHeader(http.StatusOK)
			})

			callAsAgent := func(agent *store.Agent) *httptest.ResponseRecorder {
				req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/test-synthetic-token", nil)
				req = req.WithContext(withStandingMemo(contextWithIdentity(req.Context(), f.agentIdentity(agent))))
				rec := httptest.NewRecorder()
				guarded(rec, req)
				return rec
			}

			// Control: agent in good standing reaches the handler.
			rec := callAsAgent(f.agentA)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.Equal(t, 1, called)

			// Suspend the root user without placing an AgentHold on agentA
			// (the ptone/scion#7831 orphaned-agent condition).
			u, err := f.s.GetUser(ctx, f.userID)
			require.NoError(t, err)
			u.Status = store.UserStatusSuspended
			require.NoError(t, f.s.UpdateUser(ctx, u))
			require.False(t, f.held(f.agentA.ID))
			requireStandingReason(t, f.srv.agentStanding(ctx, f.agentA.ID), standingReasonRootInactive)

			rec = callAsAgent(f.agentA)
			if tc.expectOrphanBlock {
				require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
				assert.Equal(t, 1, called, "handler must not execute for an orphaned agent")
			} else {
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				assert.Equal(t, 2, called)
			}
		})
	}
}

// standingCoverageFixture bundles the server, agents, tokens, and seeded
// project-scoped resources needed to drive every agent-callable credential
// route through the full HTTP stack.
type standingCoverageFixture struct {
	*msFixture
	tokA            string
	tokC            string
	grants          map[string]string
	skillID         string
	templateID      string
	harnessConfigID string
}

// agentStandingProbe describes one agent-callable credential, token, or
// secret-issuing HTTP route that must refuse an orphaned agent.
type agentStandingProbe struct {
	// key is the canonical "METHOD /path" pattern matching authzop / routeMetadataTable.
	key string
	// label distinguishes multiple probes sharing one key (e.g. scope=project vs scope=hub).
	label   string
	method  string
	path    func(f *standingCoverageFixture, agent *store.Agent) string
	body    func(f *standingCoverageFixture) any
	wantDen func(t *testing.T, rec *httptest.ResponseRecorder, label string)
}

func (p agentStandingProbe) name() string {
	if p.label != "" {
		return p.key + " [" + p.label + "]"
	}
	return p.key
}

// nonAgentCredentialRoute describes a ResponseCredential route that must
// refuse even an active agent token.
type nonAgentCredentialRoute struct {
	reason string
	method string
	path   func(f *standingCoverageFixture) string
	body   func(f *standingCoverageFixture) any
}

// normalizeStandingRouteKey canonicalizes a routeMetadataTable pattern or
// authzop entry point into "METHOD /path" (defaulting methodless patterns to POST).
func normalizeStandingRouteKey(pattern, method string) string {
	if pm, rest, ok := strings.Cut(pattern, " "); ok {
		return pm + " " + rest
	}
	if method == "" {
		method = http.MethodPost
	}
	return method + " " + pattern
}

// TestAgentStanding_CredentialRoutesCoverage structurally guards against a new
// agent credential/token-issuing route silently skipping delegation standing
// (ptone/scion#8104):
//
//  1. Every RouteAgentToken route in routeMetadataTable must either be in the
//     behavioral probe list below (when AgentStandingGuard/AgentStandingHandler)
//     or in the explicit exempt list (when AgentStandingExempt).
//  2. Every authzop Catalog, Exemption, or Pending HTTP route with
//     ResponseCredential (regardless of path prefix) must be in one of:
//     - the behavioral standing probe list (exercised for orphaned agents),
//     - the explicit non-agent-callable list (probed to confirm active agent
//     tokens are refused), or
//     - the explicit pre-auth public endpoint list (verified to be RoutePublic).
//  3. Every route in the behavioral probe list is exercised through the full
//     HTTP stack (UnifiedAuthMiddleware -> routeGuard -> handler) with an
//     orphaned agent (suspended root user, no AgentHold), a removed root user,
//     and a child agent of a suspended root user, asserting refusal.
func TestAgentStanding_CredentialRoutesCoverage(t *testing.T) {
	assertForbidden := func(t *testing.T, rec *httptest.ResponseRecorder, label string) {
		t.Helper()
		require.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", label, rec.Body.String())
	}
	assertNotFound := func(t *testing.T, rec *httptest.ResponseRecorder, label string) {
		t.Helper()
		require.Equal(t, http.StatusNotFound, rec.Code, "%s: %s", label, rec.Body.String())
	}

	probes := []agentStandingProbe{
		{
			key:    "POST /api/v1/agent/gcp-token",
			method: http.MethodPost,
			path:   func(*standingCoverageFixture, *store.Agent) string { return "/api/v1/agent/gcp-token" },
			body:   func(*standingCoverageFixture) any { return map[string]any{} },
			wantDen: func(t *testing.T, rec *httptest.ResponseRecorder, label string) {
				assertForbidden(t, rec, label)
				assert.Contains(t, rec.Body.String(), "Insufficient permissions", label)
			},
		},
		{
			key:    "POST /api/v1/agent/gcp-identity-token",
			method: http.MethodPost,
			path:   func(*standingCoverageFixture, *store.Agent) string { return "/api/v1/agent/gcp-identity-token" },
			body:   func(*standingCoverageFixture) any { return map[string]string{"audience": "https://api.example.com"} },
			wantDen: func(t *testing.T, rec *httptest.ResponseRecorder, label string) {
				assertForbidden(t, rec, label)
				assert.Contains(t, rec.Body.String(), "Insufficient permissions", label)
			},
		},
		{
			key:    "POST /api/v1/agent/identity-token",
			method: http.MethodPost,
			path:   func(*standingCoverageFixture, *store.Agent) string { return "/api/v1/agent/identity-token" },
			body:   func(*standingCoverageFixture) any { return map[string]string{"audience": "https://api.example.com"} },
			wantDen: func(t *testing.T, rec *httptest.ResponseRecorder, label string) {
				assertForbidden(t, rec, label)
				assert.Contains(t, rec.Body.String(), "Insufficient permissions", label)
			},
		},
		{
			key:     "POST /api/v1/agent/secrets",
			method:  http.MethodPost,
			path:    func(*standingCoverageFixture, *store.Agent) string { return "/api/v1/agent/secrets" },
			body:    func(*standingCoverageFixture) any { return map[string]any{"keys": []string{"API_KEY"}} },
			wantDen: assertForbidden,
		},
		{
			key:    "POST /api/v1/agents/{id}/token/refresh",
			method: http.MethodPost,
			path: func(_ *standingCoverageFixture, a *store.Agent) string {
				return "/api/v1/agents/" + a.ID + "/token/refresh"
			},
			body:    func(*standingCoverageFixture) any { return nil },
			wantDen: assertForbidden,
		},
		{
			key:    "POST /api/v1/agents/{id}/refresh-token",
			method: http.MethodPost,
			path: func(_ *standingCoverageFixture, a *store.Agent) string {
				return "/api/v1/agents/" + a.ID + "/refresh-token"
			},
			body: func(*standingCoverageFixture) any { return nil },
			wantDen: func(t *testing.T, rec *httptest.ResponseRecorder, label string) {
				assertForbidden(t, rec, label)
				assert.Contains(t, rec.Body.String(), "Insufficient permissions", label)
			},
		},
		{
			key:    "POST /api/v1/agents/{id}/delegations/{grantId}/exchange",
			method: http.MethodPost,
			path: func(f *standingCoverageFixture, a *store.Agent) string {
				return "/api/v1/agents/" + a.ID + "/delegations/" + f.grants[a.ID] + "/exchange"
			},
			body: func(*standingCoverageFixture) any { return map[string]any{"audience": "https://hub.example.test"} },
			wantDen: func(t *testing.T, rec *httptest.ResponseRecorder, label string) {
				assertForbidden(t, rec, label)
				assert.Contains(t, rec.Body.String(), errCodeGrantAgentChanged, label)
			},
		},
		{
			key:    "GET /api/v1/agents/{id}/secrets/{key}",
			method: http.MethodGet,
			path: func(_ *standingCoverageFixture, a *store.Agent) string {
				return "/api/v1/agents/" + a.ID + "/secrets/API_KEY"
			},
			body:    func(*standingCoverageFixture) any { return nil },
			wantDen: assertForbidden,
		},
		{
			key:    "POST /api/v1/agents",
			method: http.MethodPost,
			path:   func(*standingCoverageFixture, *store.Agent) string { return "/api/v1/agents" },
			body: func(f *standingCoverageFixture) any {
				return map[string]any{"name": "sub-agent-standing-test", "projectId": f.projectID}
			},
			wantDen: assertForbidden,
		},
		{
			key:    "POST /api/v1/projects/{projectId}/agents",
			method: http.MethodPost,
			path: func(f *standingCoverageFixture, _ *store.Agent) string {
				return "/api/v1/projects/" + f.projectID + "/agents"
			},
			body: func(*standingCoverageFixture) any {
				return map[string]any{"name": "sub-agent-standing-test"}
			},
			wantDen: assertForbidden,
		},
		{
			key:    "GET /api/v1/env",
			label:  "scope=project",
			method: http.MethodGet,
			path: func(f *standingCoverageFixture, _ *store.Agent) string {
				return "/api/v1/env?scope=project&scopeId=" + f.projectID
			},
			body:    func(*standingCoverageFixture) any { return nil },
			wantDen: assertForbidden,
		},
		{
			key:    "GET /api/v1/env",
			label:  "scope=hub",
			method: http.MethodGet,
			path: func(*standingCoverageFixture, *store.Agent) string {
				return "/api/v1/env?scope=hub"
			},
			body:    func(*standingCoverageFixture) any { return nil },
			wantDen: assertForbidden,
		},
		{
			key:    "GET /api/v1/env/{key}",
			label:  "scope=project",
			method: http.MethodGet,
			path: func(f *standingCoverageFixture, _ *store.Agent) string {
				return "/api/v1/env/API_KEY?scope=project&scopeId=" + f.projectID
			},
			body:    func(*standingCoverageFixture) any { return nil },
			wantDen: assertForbidden,
		},
		{
			key:    "GET /api/v1/env/{key}",
			label:  "scope=hub",
			method: http.MethodGet,
			path: func(*standingCoverageFixture, *store.Agent) string {
				return "/api/v1/env/API_KEY?scope=hub"
			},
			body:    func(*standingCoverageFixture) any { return nil },
			wantDen: assertForbidden,
		},
		{
			key:    "GET /api/v1/projects/{id}/env",
			method: http.MethodGet,
			path: func(f *standingCoverageFixture, _ *store.Agent) string {
				return "/api/v1/projects/" + f.projectID + "/env"
			},
			body:    func(*standingCoverageFixture) any { return nil },
			wantDen: assertForbidden,
		},
		{
			key:    "GET /api/v1/projects/{id}/env/{key}",
			method: http.MethodGet,
			path: func(f *standingCoverageFixture, _ *store.Agent) string {
				return "/api/v1/projects/" + f.projectID + "/env/API_KEY"
			},
			body:    func(*standingCoverageFixture) any { return nil },
			wantDen: assertForbidden,
		},
		{
			key:    "GET /api/v1/skills/{id}/download",
			method: http.MethodGet,
			path: func(f *standingCoverageFixture, _ *store.Agent) string {
				return "/api/v1/skills/" + f.skillID + "/download"
			},
			body:    func(*standingCoverageFixture) any { return nil },
			wantDen: assertNotFound,
		},
		{
			key:    "GET /api/v1/templates/{id}/download",
			method: http.MethodGet,
			path: func(f *standingCoverageFixture, _ *store.Agent) string {
				return "/api/v1/templates/" + f.templateID + "/download"
			},
			body:    func(*standingCoverageFixture) any { return nil },
			wantDen: assertNotFound,
		},
		{
			key:    "GET /api/v1/harness-configs/{id}/download",
			method: http.MethodGet,
			path: func(f *standingCoverageFixture, _ *store.Agent) string {
				return "/api/v1/harness-configs/" + f.harnessConfigID + "/download"
			},
			body:    func(*standingCoverageFixture) any { return nil },
			wantDen: assertNotFound,
		},
		{
			key:    "POST /api/v1/skills/resolve",
			method: http.MethodPost,
			path:   func(*standingCoverageFixture, *store.Agent) string { return "/api/v1/skills/resolve" },
			body: func(f *standingCoverageFixture) any {
				return ResolveSkillsRequest{
					Skills:    []ResolveSkillRef{{URI: "gh://acme/private-repo/SKILL.md@main"}},
					ProjectID: f.projectID,
				}
			},
			wantDen: func(t *testing.T, rec *httptest.ResponseRecorder, label string) {
				require.Equal(t, http.StatusOK, rec.Code, "%s: %s", label, rec.Body.String())
				var resp ResolveSkillsResponse
				require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp), label)
				assert.Empty(t, resp.Resolved, label)
				require.NotEmpty(t, resp.Errors, label)
				assert.Equal(t, "forbidden", resp.Errors[0].Code, "%s: %+v", label, resp.Errors)
			},
		},
		{
			key:    "POST /api/v1/projects/{projectId}/webhook-endpoints",
			method: http.MethodPost,
			path: func(f *standingCoverageFixture, _ *store.Agent) string {
				return "/api/v1/projects/" + f.projectID + "/webhook-endpoints"
			},
			body: func(*standingCoverageFixture) any {
				return map[string]any{"name": "standing-test-hook"}
			},
			wantDen: assertForbidden,
		},
		{
			key:    "POST /api/v1/templates",
			method: http.MethodPost,
			path:   func(*standingCoverageFixture, *store.Agent) string { return "/api/v1/templates" },
			body: func(f *standingCoverageFixture) any {
				return map[string]any{"name": "standing-test-tpl", "scope": "project", "scopeId": f.projectID}
			},
			wantDen: assertForbidden,
		},
		{
			key:    "POST /api/v1/skills",
			method: http.MethodPost,
			path:   func(*standingCoverageFixture, *store.Agent) string { return "/api/v1/skills" },
			body: func(f *standingCoverageFixture) any {
				return map[string]any{"name": "standing-test-skill", "scope": "project", "scopeId": f.projectID}
			},
			wantDen: assertForbidden,
		},
	}

	probeKeys := make(map[string]bool, len(probes))
	for _, p := range probes {
		probeKeys[p.key] = true
	}

	// Explicit exempt RouteAgentToken routes (non-credential routes).
	exemptAgentTokenRoutes := map[string]string{
		"/api/v1/agent/decisions":  "reads and records agent decisions; issues no credentials, tokens, or secrets",
		"/api/v1/agent/decisions/": "reads and records agent decisions; issues no credentials, tokens, or secrets",
		"/api/v1/conduit":          "WebSocket session endpoint (agent, broker, user); issues no credentials or tokens",
	}

	// Explicit pre-authentication public credential routes (ExemptionPublicEndpoint).
	// Verified structurally to be RoutePublic in routeMetadataTable.
	preAuthPublicCredentialRoutes := map[string]string{
		"POST /api/v1/auth/login":                        "pre-authentication login flow (RoutePublic)",
		"POST /api/v1/auth/token":                        "pre-authentication token exchange (RoutePublic)",
		"POST /api/v1/auth/refresh":                      "pre-authentication token refresh (RoutePublic)",
		"POST /api/v1/auth/cli/token":                    "pre-authentication CLI token exchange (RoutePublic)",
		"POST /api/v1/auth/cli/device":                   "pre-authentication CLI device flow (RoutePublic)",
		"POST /api/v1/auth/cli/device/token":             "pre-authentication CLI device token exchange (RoutePublic)",
		"POST /api/v1/auth/integrations/google/exchange": "pre-authentication Google credential exchange (RoutePublic)",
	}

	// Explicit ResponseCredential routes that do not admit agent tokens at all
	// (every entry is behaviorally probed below with an active agent token).
	nonAgentCallableCredentialRoutes := map[string]nonAgentCredentialRoute{
		"POST /api/v1/agents/{id}/workspace/sync-from": {
			reason: "user/broker-only workspace sync; refuses agent tokens",
			method: http.MethodPost,
			path: func(f *standingCoverageFixture) string {
				return "/api/v1/agents/" + f.agentA.ID + "/workspace/sync-from"
			},
		},
		"POST /api/v1/agents/{id}/workspace/sync-to": {
			reason: "user/broker-only workspace sync; refuses agent tokens",
			method: http.MethodPost,
			path:   func(f *standingCoverageFixture) string { return "/api/v1/agents/" + f.agentA.ID + "/workspace/sync-to" },
		},
		"POST /api/v1/auth/tokens": {
			reason: "user session only; refuses agent tokens",
			method: http.MethodPost,
			path:   func(*standingCoverageFixture) string { return "/api/v1/auth/tokens" },
			body:   func(*standingCoverageFixture) any { return map[string]any{"name": "t"} },
		},
		"POST /api/v1/auth/federation-token": {
			reason: "user session or UAT only; refuses agent tokens",
			method: http.MethodPost,
			path:   func(*standingCoverageFixture) string { return "/api/v1/auth/federation-token" },
			body:   func(*standingCoverageFixture) any { return map[string]any{"audience": "https://peer.example.com"} },
		},
		"POST /api/v1/admin/invites": {
			reason: "hub admin only; refuses agent tokens",
			method: http.MethodPost,
			path:   func(*standingCoverageFixture) string { return "/api/v1/admin/invites" },
			body:   func(*standingCoverageFixture) any { return map[string]any{"email": "x@example.com"} },
		},
		"POST /api/v1/brokers": {
			reason: "broker-HMAC auth only; refuses agent tokens",
			method: http.MethodPost,
			path:   func(*standingCoverageFixture) string { return "/api/v1/brokers" },
			body:   func(*standingCoverageFixture) any { return map[string]any{"name": "b"} },
		},
		"POST /api/v1/brokers/": {
			reason: "broker-HMAC auth only; refuses agent tokens",
			method: http.MethodPost,
			path:   func(*standingCoverageFixture) string { return "/api/v1/brokers/broker-1" },
		},
		"POST /api/v1/brokers/join": {
			reason: "broker-HMAC join flow (one-time join token); ignores/refuses agent tokens",
			method: http.MethodPost,
			path:   func(*standingCoverageFixture) string { return "/api/v1/brokers/join" },
			body: func(*standingCoverageFixture) any {
				return map[string]any{"brokerId": "broker-1", "joinToken": "invalid-token"}
			},
		},
		"GET /api/v1/runtime-brokers/{id}/env": {
			reason: "broker/user material read; refuses agent tokens",
			method: http.MethodGet,
			path:   func(*standingCoverageFixture) string { return "/api/v1/runtime-brokers/broker-1/env" },
		},
		"GET /api/v1/runtime-brokers/{id}/env/{key}": {
			reason: "broker/user material read; refuses agent tokens",
			method: http.MethodGet,
			path:   func(*standingCoverageFixture) string { return "/api/v1/runtime-brokers/broker-1/env/API_KEY" },
		},
		"PUT /api/v1/runtime-brokers/{id}/env/{key}": {
			reason: "broker/user material write; refuses agent tokens",
			method: http.MethodPut,
			path:   func(*standingCoverageFixture) string { return "/api/v1/runtime-brokers/broker-1/env/API_KEY" },
			body:   func(*standingCoverageFixture) any { return map[string]any{"value": "v"} },
		},
		"POST /api/v1/runtime-brokers/{id}/agents/{agentId}/workspace-download": {
			reason: "broker-only workspace download; refuses agent tokens",
			method: http.MethodPost,
			path: func(f *standingCoverageFixture) string {
				return "/api/v1/runtime-brokers/broker-1/agents/" + f.agentA.ID + "/workspace-download"
			},
		},
		"PUT /api/v1/env/{key}": {
			reason: "agents have read-only env access; PUT refused",
			method: http.MethodPut,
			path:   func(*standingCoverageFixture) string { return "/api/v1/env/API_KEY" },
			body: func(f *standingCoverageFixture) any {
				return map[string]any{"value": "v", "scope": "project", "scopeId": f.projectID}
			},
		},
		"PUT /api/v1/projects/{id}/env/{key}": {
			reason: "agents have read-only project env access; PUT refused",
			method: http.MethodPut,
			path:   func(f *standingCoverageFixture) string { return "/api/v1/projects/" + f.projectID + "/env/API_KEY" },
			body:   func(*standingCoverageFixture) any { return map[string]any{"value": "v"} },
		},
		"POST /api/v1/pending-secrets/{id}/reveal": {
			reason: "user session only; refuses agent tokens",
			method: http.MethodPost,
			path:   func(*standingCoverageFixture) string { return "/api/v1/pending-secrets/ps-1/reveal" },
		},
		"POST /api/v1/projects/register": {
			reason: "user/broker project registration; refuses agent tokens",
			method: http.MethodPost,
			path:   func(*standingCoverageFixture) string { return "/api/v1/projects/register" },
			body:   func(*standingCoverageFixture) any { return map[string]any{"name": "p"} },
		},
		"POST /api/v1/artifacts/{id}/links": {
			reason: "users only (agentAdminForbiddenMessage); refuses agent tokens",
			method: http.MethodPost,
			path:   func(*standingCoverageFixture) string { return "/api/v1/artifacts/art-1/links" },
			body:   func(*standingCoverageFixture) any { return map[string]any{} },
		},
		"POST /api/v1/artifacts/{id}/versions/{seq}/view": {
			reason: "artifact version view requires artifact service and CheckAccess; refuses agent without grant",
			method: http.MethodPost,
			path:   func(*standingCoverageFixture) string { return "/api/v1/artifacts/art-1/versions/1/view" },
		},
		"GET /api/v1/users/me/templates/{id}/download": {
			reason: "user-scoped template download; refuses agent tokens",
			method: http.MethodGet,
			path: func(f *standingCoverageFixture) string {
				return "/api/v1/users/me/templates/" + f.templateID + "/download"
			},
		},
		"POST /api/v1/users/me/templates": {
			reason: "user-scoped template create; refuses agent tokens",
			method: http.MethodPost,
			path:   func(*standingCoverageFixture) string { return "/api/v1/users/me/templates" },
			body:   func(*standingCoverageFixture) any { return map[string]any{"name": "t"} },
		},
		"POST /api/v1/users/me/templates/{id}/upload": {
			reason: "user-scoped template upload; refuses agent tokens",
			method: http.MethodPost,
			path: func(f *standingCoverageFixture) string {
				return "/api/v1/users/me/templates/" + f.templateID + "/upload"
			},
			body: func(*standingCoverageFixture) any { return map[string]any{} },
		},
		"POST /api/v1/skills/{id}/versions": {
			reason: "agents lack skill.update scope; refuses agent tokens",
			method: http.MethodPost,
			path:   func(f *standingCoverageFixture) string { return "/api/v1/skills/" + f.skillID + "/versions" },
			body:   func(*standingCoverageFixture) any { return map[string]any{"version": "1.0.1"} },
		},
		"POST /api/v1/skills/{id}/upload": {
			reason: "agents lack skill.update scope; refuses agent tokens",
			method: http.MethodPost,
			path:   func(f *standingCoverageFixture) string { return "/api/v1/skills/" + f.skillID + "/upload" },
			body:   func(*standingCoverageFixture) any { return map[string]any{} },
		},
		"POST /api/v1/templates/{id}/upload": {
			reason: "agents lack template.update scope; refuses agent tokens",
			method: http.MethodPost,
			path:   func(f *standingCoverageFixture) string { return "/api/v1/templates/" + f.templateID + "/upload" },
			body:   func(*standingCoverageFixture) any { return map[string]any{} },
		},
		"POST /api/v1/harness-configs": {
			reason: "agents lack harness_config.create scope; refuses agent tokens",
			method: http.MethodPost,
			path:   func(*standingCoverageFixture) string { return "/api/v1/harness-configs" },
			body: func(f *standingCoverageFixture) any {
				return map[string]any{"name": "hc", "harness": "claude", "scope": "project", "scopeId": f.projectID}
			},
		},
		"POST /api/v1/harness-configs/{id}/upload": {
			reason: "agents lack harness_config.update scope; refuses agent tokens",
			method: http.MethodPost,
			path: func(f *standingCoverageFixture) string {
				return "/api/v1/harness-configs/" + f.harnessConfigID + "/upload"
			},
			body: func(*standingCoverageFixture) any { return map[string]any{} },
		},
		"POST /api/v1/test-identities": {
			reason: "hub admin / user only; refuses agent tokens",
			method: http.MethodPost,
			path:   func(*standingCoverageFixture) string { return "/api/v1/test-identities" },
			body:   func(*standingCoverageFixture) any { return map[string]any{} },
		},
		"POST /api/v1/test-identities/admin": {
			reason: "hub admin / user only; refuses agent tokens",
			method: http.MethodPost,
			path:   func(*standingCoverageFixture) string { return "/api/v1/test-identities/admin" },
			body:   func(*standingCoverageFixture) any { return map[string]any{} },
		},
		"POST /api/v1/test-identities/{id}/token": {
			reason: "hub admin / user only; refuses agent tokens",
			method: http.MethodPost,
			path:   func(*standingCoverageFixture) string { return "/api/v1/test-identities/ti-1/token" },
			body:   func(*standingCoverageFixture) any { return map[string]any{} },
		},
		"POST /api/v1/test-identities/{id}/web-login-code": {
			reason: "hub admin / user only; refuses agent tokens",
			method: http.MethodPost,
			path:   func(*standingCoverageFixture) string { return "/api/v1/test-identities/ti-1/web-login-code" },
			body:   func(*standingCoverageFixture) any { return map[string]any{} },
		},
	}

	// --- Structural Check 1: every RouteAgentToken route is accounted for ---
	t.Run("every RouteAgentToken route is covered or explicitly exempt", func(t *testing.T) {
		seenExempt := make(map[string]bool)
		var missing []string
		for pattern, meta := range routeMetadataTable {
			if meta.Classification != RouteAgentToken {
				continue
			}
			switch meta.AgentStanding {
			case AgentStandingExempt:
				if exemptAgentTokenRoutes[pattern] == "" {
					missing = append(missing, pattern+" (marked AgentStandingExempt in routeMetadataTable but not listed in test exemptAgentTokenRoutes)")
				}
				seenExempt[pattern] = true
			case AgentStandingGuard, AgentStandingHandler:
				key := normalizeStandingRouteKey(pattern, http.MethodPost)
				if !probeKeys[key] {
					missing = append(missing, pattern+" (key "+key+" missing from behavioral standing probes)")
				}
			default:
				missing = append(missing, pattern+" (invalid AgentStanding policy "+string(meta.AgentStanding)+")")
			}
		}
		sort.Strings(missing)
		require.Empty(t, missing, "RouteAgentToken routes not covered by standing structural guard")

		for pattern := range exemptAgentTokenRoutes {
			if !seenExempt[pattern] {
				t.Errorf("stale exemptAgentTokenRoutes entry %q (not an AgentStandingExempt RouteAgentToken route)", pattern)
			}
		}
	})

	// --- Structural Check 2: every authzop ResponseCredential HTTP route is accounted for ---
	t.Run("every authzop ResponseCredential route is covered", func(t *testing.T) {
		discovered := make(map[string]bool)
		publicExemptions := make(map[string]string)

		for _, op := range authzop.Catalog {
			for _, ep := range op.EntryPoints {
				if ep.Kind != authzop.EntryPointHTTPRoute || ep.Response != authzop.ResponseCredential {
					continue
				}
				discovered[normalizeStandingRouteKey(ep.Pattern, ep.Method)] = true
			}
		}
		for _, ex := range authzop.EntryPointExemptions {
			if ex.Response != authzop.ResponseCredential {
				continue
			}
			key := normalizeStandingRouteKey(ex.Pattern, http.MethodPost)
			discovered[key] = true
			if ex.Kind == authzop.ExemptionPublicEndpoint {
				publicExemptions[key] = ex.Pattern
			}
		}
		for _, ep := range authzop.PendingBearerDispositions {
			if ep.Kind != authzop.EntryPointHTTPRoute || ep.Response != authzop.ResponseCredential {
				continue
			}
			discovered[normalizeStandingRouteKey(ep.Pattern, ep.Method)] = true
		}

		var uncovered []string
		for key := range discovered {
			if probeKeys[key] {
				continue
			}
			if _, ok := nonAgentCallableCredentialRoutes[key]; ok {
				continue
			}
			if preAuthPublicCredentialRoutes[key] != "" {
				pat, isPublicEx := publicExemptions[key]
				require.True(t, isPublicEx, "%s listed in preAuthPublicCredentialRoutes but not an ExemptionPublicEndpoint", key)
				meta, ok := routeMetadataTable[pat]
				require.True(t, ok, "%s (pattern %q) missing from routeMetadataTable", key, pat)
				require.Equal(t, RoutePublic, meta.Classification, "%s must be RoutePublic", key)
				continue
			}
			uncovered = append(uncovered, key)
		}
		sort.Strings(uncovered)
		require.Empty(t, uncovered, "authzop ResponseCredential routes missing from standing coverage census")

		for key, spec := range nonAgentCallableCredentialRoutes {
			if spec.reason == "" {
				t.Errorf("nonAgentCallableCredentialRoutes[%q] missing reason", key)
			}
			if !discovered[key] {
				t.Errorf("stale nonAgentCallableCredentialRoutes entry %q", key)
			}
			if probeKeys[key] {
				t.Errorf("route %q listed in both probes and nonAgentCallableCredentialRoutes", key)
			}
		}
		for key := range preAuthPublicCredentialRoutes {
			if !discovered[key] {
				t.Errorf("stale preAuthPublicCredentialRoutes entry %q", key)
			}
		}
	})

	// Helper to create a server + msFixture with OIDC, secret backend,
	// experiments, and project-scoped resources so every credential route
	// reaches its authorization/standing check.
	newFullFixture := func(t *testing.T, name string) *standingCoverageFixture {
		t.Helper()
		srv := testOIDCServerWithRoutes(t)
		t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
		srv.SetSecretBackend(secret.NewLocalBackend(srv.store, "test-hub-id", "test-secret"))
		cur := srv.experimentRegistry()
		var active []experiments.Experiment
		for _, e := range cur.All() {
			if e.Name == experiments.AgentDelegation || e.Name == experiments.Triggers {
				e.Default = true
			}
			active = append(active, e)
		}
		reg, err := experiments.NewRegistry(active, cur.Retired())
		require.NoError(t, err)
		srv.setExperimentRegistry(reg)

		f := newMSFixtureOn(t, name, srv, srv.store)
		scopes := append(append([]AgentTokenScope{}, ScopesForRole(AgentRoleFull)...), ScopeIdentityToken)
		tokA := f.agentToken(f.agentA, scopes...)
		tokC := f.agentToken(f.childC, scopes...)

		ctx := context.Background()
		grants := make(map[string]string, 2)
		for _, ag := range []*store.Agent{f.agentA, f.childC} {
			grantID := tid(name + "-grant-" + ag.Slug)
			require.NoError(t, f.s.CreateAgentDelegationGrant(ctx, &store.AgentDelegationGrant{
				ID:                      grantID,
				AgentID:                 ag.ID,
				AgentProjectID:          ag.ProjectID,
				AgentGeneration:         ag.Generation,
				IssuerUserID:            f.userID,
				Name:                    "standing-test-grant",
				Purpose:                 "standing route coverage test",
				BoundaryKind:            string(permissions.BoundaryKindProject),
				BoundaryProjectID:       ag.ProjectID,
				CeilingPermissionIDs:    []string{"project.read"},
				CeilingVersion:          int(permissions.CeilingVersionV1),
				MaxCredentialTTLSeconds: 300,
				IssuanceAuditID:         tid(name + "-audit-" + ag.Slug),
				ExpiresAt:               time.Now().UTC().Add(time.Hour),
			}))
			grants[ag.ID] = grantID
		}

		// Seed project-scoped skill, template, and harness config so download
		// and upload routes reach authorization rather than failing with 404
		// on missing rows.
		skillID := tid(name + "-skill")
		require.NoError(t, f.s.CreateSkill(ctx, &store.Skill{
			ID:      skillID,
			Name:    "standing-skill-" + name,
			Slug:    "standing-skill-" + name,
			Scope:   store.SkillScopeProject,
			ScopeID: f.projectID,
			Status:  "active",
		}))
		tplID := tid(name + "-tpl")
		require.NoError(t, f.s.CreateTemplate(ctx, &store.Template{
			ID:      tplID,
			Name:    "standing-tpl-" + name,
			Slug:    "standing-tpl-" + name,
			Scope:   store.TemplateScopeProject,
			ScopeID: f.projectID,
			Status:  store.TemplateStatusActive,
		}))
		hcID := tid(name + "-hc")
		require.NoError(t, f.s.CreateHarnessConfig(ctx, &store.HarnessConfig{
			ID:      hcID,
			Name:    "standing-hc-" + name,
			Slug:    "standing-hc-" + name,
			Harness: "claude",
			Scope:   store.HarnessConfigScopeProject,
			ScopeID: f.projectID,
			Status:  store.HarnessConfigStatusActive,
		}))

		return &standingCoverageFixture{
			msFixture:       f,
			tokA:            tokA,
			tokC:            tokC,
			grants:          grants,
			skillID:         skillID,
			templateID:      tplID,
			harnessConfigID: hcID,
		}
	}

	// --- Behavioral Check 1: suspended root user (no AgentHold) refused on every route ---
	t.Run("suspended root user refused across every agent credential route", func(t *testing.T) {
		f := newFullFixture(t, "cov-susp")
		ctx := context.Background()

		// Positive control: active agent can read project env before suspension.
		rec := doRequestWithAgentToken(t, f.srv, http.MethodGet, "/api/v1/env?scope=project&scopeId="+f.projectID, nil, f.tokA)
		require.Equal(t, http.StatusOK, rec.Code, "control: active agent must reach GET /api/v1/env: %s", rec.Body.String())

		u, err := f.s.GetUser(ctx, f.userID)
		require.NoError(t, err)
		u.Status = store.UserStatusSuspended
		require.NoError(t, f.s.UpdateUser(ctx, u))
		require.False(t, f.held(f.agentA.ID))
		require.False(t, f.held(f.childC.ID))
		requireStandingReason(t, f.srv.agentStanding(ctx, f.agentA.ID), standingReasonRootInactive)
		requireStandingReason(t, f.srv.agentStanding(ctx, f.childC.ID), standingReasonRootInactive)

		for _, p := range probes {
			t.Run(p.name()+" (direct)", func(t *testing.T) {
				rec := doRequestWithAgentToken(t, f.srv, p.method, p.path(f, f.agentA), p.body(f), f.tokA)
				p.wantDen(t, rec, p.name()+" (direct)")
			})
			t.Run(p.name()+" (child)", func(t *testing.T) {
				rec := doRequestWithAgentToken(t, f.srv, p.method, p.path(f, f.childC), p.body(f), f.tokC)
				p.wantDen(t, rec, p.name()+" (child)")
			})
		}
	})

	// --- Behavioral Check 2: removed root user (no AgentHold) refused on every route ---
	t.Run("removed root user refused across every agent credential route", func(t *testing.T) {
		f := newFullFixture(t, "cov-rem")
		ctx := context.Background()

		f.dropBindings(f.userID)
		require.False(t, f.held(f.agentA.ID))
		requireStandingReason(t, f.srv.agentStanding(ctx, f.agentA.ID), standingReasonRootNotAdmitted)

		for _, p := range probes {
			t.Run(p.name(), func(t *testing.T) {
				rec := doRequestWithAgentToken(t, f.srv, p.method, p.path(f, f.agentA), p.body(f), f.tokA)
				p.wantDen(t, rec, p.name())
			})
		}
	})

	// --- Behavioral Check 3: excluded non-agent-callable routes refuse even an active agent ---
	t.Run("non-agent-callable credential routes refuse agent tokens", func(t *testing.T) {
		f := newFullFixture(t, "cov-nonagent")
		allScopesTok := f.agentToken(f.agentA, allRegisteredAgentScopes()...)

		keys := make([]string, 0, len(nonAgentCallableCredentialRoutes))
		for k := range nonAgentCallableCredentialRoutes {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		for _, key := range keys {
			spec := nonAgentCallableCredentialRoutes[key]
			t.Run(key, func(t *testing.T) {
				var body any
				if spec.body != nil {
					body = spec.body(f)
				}
				rec := doRequestWithAgentToken(t, f.srv, spec.method, spec.path(f), body, allScopesTok)
				require.True(t,
					rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden || rec.Code == http.StatusNotFound,
					"%s: expected 401/403/404 for active agent token, got %d: %s", key, rec.Code, rec.Body.String())
			})
		}
	})

	// --- Behavioral Check 4: POST /api/v1/agent/identity-token rate limit runs before standing ---
	t.Run("identity-token rate limiter runs before standing check", func(t *testing.T) {
		f := newFullFixture(t, "cov-idtok-rl")
		ctx := context.Background()

		// Exhaust the per-agent identity-token rate limit while the agent is in
		// good standing, then suspend the root user and confirm the rate limit
		// still returns 429 before the handler evaluates standing.
		u, err := f.s.GetUser(ctx, f.userID)
		require.NoError(t, err)
		u.Status = store.UserStatusSuspended
		require.NoError(t, f.s.UpdateUser(ctx, u))

		var got429 bool
		for range 100 {
			rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/agent/identity-token",
				map[string]string{"audience": "https://api.example.com"}, f.tokA)
			if rec.Code == http.StatusTooManyRequests {
				got429 = true
				break
			}
			require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		}
		require.True(t, got429, "expected 429 from handler rate limiter before standing check on POST /api/v1/agent/identity-token")
	})
}
