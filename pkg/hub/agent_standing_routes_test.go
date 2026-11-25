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
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
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
	skillSlug       string
	globalSkillID   string
	templateID      string
	globalTplID     string
	harnessConfigID string
	brokerID        string
	artifactID      string
}

// agentStandingProbe describes one agent-callable credential, token, or
// secret-issuing HTTP route (and optional authzop Variant) that must refuse an
// orphaned agent.
type agentStandingProbe struct {
	// key is the canonical "METHOD /path" pattern matching authzop / routeMetadataTable.
	key string
	// variant is the authzop EntryPoint.Variant (empty when the entry point has no Variant).
	variant string
	// label distinguishes multiple probes sharing one key when variant is empty.
	label   string
	method  string
	path    func(f *standingCoverageFixture, agent *store.Agent) string
	body    func(f *standingCoverageFixture) any
	wantDen func(t *testing.T, rec *httptest.ResponseRecorder, label string)
}

func (p agentStandingProbe) variantKey() string {
	return normalizeStandingRouteVariantKey(p.key, p.method, p.variant)
}

func (p agentStandingProbe) name() string {
	if p.variant != "" {
		return p.key + " [" + p.variant + "]"
	}
	if p.label != "" {
		return p.key + " [" + p.label + "]"
	}
	return p.key
}

// nonAgentCredentialRoute describes a ResponseCredential route (or variant)
// that must refuse even an active agent token with 401 or 403.
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

// normalizeStandingRouteVariantKey canonicalizes an authzop entry point into
// "METHOD /path" (when variant is empty) or "METHOD /path [variant]" so
// distinct variants of the same route are tracked independently by the census.
func normalizeStandingRouteVariantKey(pattern, method, variant string) string {
	base := normalizeStandingRouteKey(pattern, method)
	if variant != "" {
		return base + " [" + variant + "]"
	}
	return base
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
			key:    "GET /api/v1/conduit",
			method: http.MethodGet,
			path:   func(*standingCoverageFixture, *store.Agent) string { return "/api/v1/conduit" },
			body:   func(*standingCoverageFixture) any { return nil },
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
			key:     "GET /api/v1/env",
			variant: "scope=hub",
			method:  http.MethodGet,
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
			key:     "GET /api/v1/env/{key}",
			variant: "scope=hub",
			method:  http.MethodGet,
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
			key:     "POST /api/v1/skills/resolve",
			variant: "ref=skill",
			method:  http.MethodPost,
			path:    func(*standingCoverageFixture, *store.Agent) string { return "/api/v1/skills/resolve" },
			body: func(f *standingCoverageFixture) any {
				return ResolveSkillsRequest{
					Skills:    []ResolveSkillRef{{URI: "skill://" + f.skillSlug}},
					ProjectID: f.projectID,
				}
			},
			wantDen: assertForbidden,
		},
		{
			key:     "POST /api/v1/skills/resolve",
			variant: "ref=registry",
			method:  http.MethodPost,
			path:    func(*standingCoverageFixture, *store.Agent) string { return "/api/v1/skills/resolve" },
			body: func(f *standingCoverageFixture) any {
				return ResolveSkillsRequest{
					Skills:    []ResolveSkillRef{{URI: "skill://other-registry/my-skill"}},
					ProjectID: f.projectID,
				}
			},
			wantDen: assertForbidden,
		},
		{
			key:     "POST /api/v1/skills/resolve",
			variant: "ref=gh,project=named",
			method:  http.MethodPost,
			path:    func(*standingCoverageFixture, *store.Agent) string { return "/api/v1/skills/resolve" },
			body: func(f *standingCoverageFixture) any {
				return ResolveSkillsRequest{
					Skills:    []ResolveSkillRef{{URI: "gh://acme/private-repo/SKILL.md@main"}},
					ProjectID: f.projectID,
				}
			},
			wantDen: assertForbidden,
		},
		{
			key:     "POST /api/v1/skills/resolve",
			variant: "ref=gh,project=none",
			method:  http.MethodPost,
			path:    func(*standingCoverageFixture, *store.Agent) string { return "/api/v1/skills/resolve" },
			body: func(*standingCoverageFixture) any {
				return ResolveSkillsRequest{
					Skills: []ResolveSkillRef{{URI: "gh://acme/public-repo/SKILL.md@main"}},
				}
			},
			wantDen: assertForbidden,
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
			key:    "POST /api/v1/projects/{projectId}/webhook-endpoints/{id}/rotate-secret",
			method: http.MethodPost,
			path: func(f *standingCoverageFixture, _ *store.Agent) string {
				return "/api/v1/projects/" + f.projectID + "/webhook-endpoints/00000000-0000-4000-8000-000000000001/rotate-secret"
			},
			body: func(*standingCoverageFixture) any {
				return nil
			},
			wantDen: assertForbidden,
		},
		{
			key:    "POST /api/v1/templates",
			method: http.MethodPost,
			path:   func(*standingCoverageFixture, *store.Agent) string { return "/api/v1/templates" },
			body: func(f *standingCoverageFixture) any {
				return map[string]any{
					"name":    "standing-test-tpl",
					"scope":   "project",
					"scopeId": f.projectID,
					"files":   []map[string]any{{"path": "scion-agent.yaml", "size": 10}},
				}
			},
			wantDen: assertForbidden,
		},
		{
			key:    "POST /api/v1/templates/{id}/upload",
			method: http.MethodPost,
			path: func(f *standingCoverageFixture, _ *store.Agent) string {
				return "/api/v1/templates/" + f.templateID + "/upload"
			},
			body: func(*standingCoverageFixture) any {
				return map[string]any{
					"files": []map[string]any{{"path": "scion-agent.yaml", "size": 10}},
				}
			},
			wantDen: assertForbidden,
		},
		{
			key:     "POST /api/v1/skills",
			variant: "scope=project",
			method:  http.MethodPost,
			path:    func(*standingCoverageFixture, *store.Agent) string { return "/api/v1/skills" },
			body: func(f *standingCoverageFixture) any {
				return map[string]any{"name": "standing-test-skill", "scope": "project", "scopeId": f.projectID}
			},
			wantDen: assertForbidden,
		},
		{
			key:    "POST /api/v1/skills/{id}/versions",
			method: http.MethodPost,
			path: func(f *standingCoverageFixture, _ *store.Agent) string {
				return "/api/v1/skills/" + f.skillID + "/versions"
			},
			body: func(*standingCoverageFixture) any {
				return map[string]any{
					"version": "2.0.0",
					"files":   []map[string]any{{"path": "SKILL.md", "size": 10}},
				}
			},
			wantDen: assertForbidden,
		},
		{
			key:    "POST /api/v1/skills/{id}/upload",
			method: http.MethodPost,
			path: func(f *standingCoverageFixture, _ *store.Agent) string {
				return "/api/v1/skills/" + f.skillID + "/upload"
			},
			body: func(*standingCoverageFixture) any {
				return map[string]any{
					"version": "1.0.0",
					"files":   []map[string]any{{"path": "SKILL.md", "size": 10}},
				}
			},
			wantDen: assertForbidden,
		},
		{
			key:    "POST /api/v1/artifacts/{id}/versions/{seq}/view",
			method: http.MethodPost,
			path: func(f *standingCoverageFixture, _ *store.Agent) string {
				return "/api/v1/artifacts/" + f.artifactID + "/versions/1/view"
			},
			body:    func(*standingCoverageFixture) any { return nil },
			wantDen: assertNotFound,
		},
	}

	probeBaseKeys := make(map[string]bool, len(probes))
	probeVariantKeys := make(map[string]bool, len(probes))
	for _, p := range probes {
		probeBaseKeys[p.key] = true
		probeVariantKeys[p.variantKey()] = true
	}

	// Explicit exempt RouteAgentToken routes (non-credential routes).
	exemptAgentTokenRoutes := map[string]string{
		"GET /api/v1/agent/hub":          "returns only the hub's non-secret hub_id identifier; issues no credentials, tokens, or secrets",
		"/api/v1/agent/decisions":        "reads and records agent decisions; issues no credentials, tokens, or secrets",
		"/api/v1/agent/decisions/":       "reads and records agent decisions; issues no credentials, tokens, or secrets",
		"/api/v1/agent/secret-requests":  "lists and withdraws the calling agent's secret request metadata; never reads, writes, or returns secret values",
		"/api/v1/agent/secret-requests/": "lists and withdraws the calling agent's secret request metadata; never reads, writes, or returns secret values",
		"/api/v1/agent/command-requests": "lists the calling agent's own command request metadata and run status; never executes commands or issues credentials",
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
			path:   func(f *standingCoverageFixture) string { return "/api/v1/brokers/" + f.brokerID + "/rotate-secret" },
		},
		"POST /api/v1/brokers/join": {
			reason: "broker-HMAC join flow (one-time join token); ignores/refuses agent tokens",
			method: http.MethodPost,
			path:   func(*standingCoverageFixture) string { return "/api/v1/brokers/join" },
			body: func(f *standingCoverageFixture) any {
				return map[string]any{"brokerId": f.brokerID, "joinToken": "invalid-token"}
			},
		},
		"GET /api/v1/runtime-brokers/{id}/env": {
			reason: "broker/user material read; refuses agent tokens",
			method: http.MethodGet,
			path:   func(f *standingCoverageFixture) string { return "/api/v1/runtime-brokers/" + f.brokerID + "/env" },
		},
		"GET /api/v1/runtime-brokers/{id}/env/{key}": {
			reason: "broker/user material read; refuses agent tokens",
			method: http.MethodGet,
			path: func(f *standingCoverageFixture) string {
				return "/api/v1/runtime-brokers/" + f.brokerID + "/env/API_KEY"
			},
		},
		"PUT /api/v1/runtime-brokers/{id}/env/{key}": {
			reason: "broker/user material write; refuses agent tokens",
			method: http.MethodPut,
			path: func(f *standingCoverageFixture) string {
				return "/api/v1/runtime-brokers/" + f.brokerID + "/env/API_KEY"
			},
			body: func(*standingCoverageFixture) any { return map[string]any{"value": "v"} },
		},
		"POST /api/v1/runtime-brokers/{id}/agents/{agentId}/workspace-download": {
			reason: "broker-only workspace download; refuses agent tokens",
			method: http.MethodPost,
			path: func(f *standingCoverageFixture) string {
				return "/api/v1/runtime-brokers/" + f.brokerID + "/agents/" + f.agentA.ID + "/workspace-download"
			},
		},
		"POST /api/v1/runtime-brokers/{id}/agents/{agentId}/move-bundle/{rid}/upload-url": {
			reason: "broker-only move-bundle upload URL; refuses agent tokens",
			method: http.MethodPost,
			path: func(f *standingCoverageFixture) string {
				return "/api/v1/runtime-brokers/" + f.brokerID + "/agents/" + f.agentA.ID + "/move-bundle/rid-test/upload-url"
			},
		},
		"POST /api/v1/runtime-brokers/{id}/agents/{agentId}/move-bundle/{rid}/download-urls": {
			reason: "broker-only move-bundle download URLs; refuses agent tokens",
			method: http.MethodPost,
			path: func(f *standingCoverageFixture) string {
				return "/api/v1/runtime-brokers/" + f.brokerID + "/agents/" + f.agentA.ID + "/move-bundle/rid-test/download-urls"
			},
		},
		"GET /api/v1/env [scope=user]": {
			reason: "user-scoped env read; refuses agent tokens",
			method: http.MethodGet,
			path:   func(*standingCoverageFixture) string { return "/api/v1/env?scope=user" },
		},
		"GET /api/v1/env/{key} [scope=user]": {
			reason: "user-scoped env read; refuses agent tokens",
			method: http.MethodGet,
			path:   func(*standingCoverageFixture) string { return "/api/v1/env/API_KEY?scope=user" },
		},
		"PUT /api/v1/env/{key}": {
			reason: "agents have read-only env access; PUT refused",
			method: http.MethodPut,
			path:   func(*standingCoverageFixture) string { return "/api/v1/env/API_KEY" },
			body: func(f *standingCoverageFixture) any {
				return map[string]any{"value": "v", "scope": "project", "scopeId": f.projectID}
			},
		},
		"PUT /api/v1/env/{key} [scope=hub]": {
			reason: "agents have read-only hub env access; PUT refused",
			method: http.MethodPut,
			path:   func(*standingCoverageFixture) string { return "/api/v1/env/API_KEY" },
			body: func(*standingCoverageFixture) any {
				return map[string]any{"value": "v", "scope": "hub"}
			},
		},
		"PUT /api/v1/env/{key} [scope=user]": {
			reason: "user-scoped env write; refuses agent tokens",
			method: http.MethodPut,
			path:   func(*standingCoverageFixture) string { return "/api/v1/env/API_KEY" },
			body: func(*standingCoverageFixture) any {
				return map[string]any{"value": "v", "scope": "user"}
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
			reason: "users only (agentAdminForbiddenMessage); refuses agent tokens even on agent-owned artifact",
			method: http.MethodPost,
			path:   func(f *standingCoverageFixture) string { return "/api/v1/artifacts/" + f.artifactID + "/links" },
			body:   func(*standingCoverageFixture) any { return map[string]any{} },
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
		"POST /api/v1/skills [scope=global]": {
			reason: "agents cannot create global skills; refuses agent tokens",
			method: http.MethodPost,
			path:   func(*standingCoverageFixture) string { return "/api/v1/skills" },
			body:   func(*standingCoverageFixture) any { return map[string]any{"name": "g-skill", "scope": "global"} },
		},
		"POST /api/v1/skills [scope=user]": {
			reason: "agents cannot create user-scoped skills; refuses agent tokens",
			method: http.MethodPost,
			path:   func(*standingCoverageFixture) string { return "/api/v1/skills" },
			body:   func(*standingCoverageFixture) any { return map[string]any{"name": "u-skill", "scope": "user"} },
		},
		"POST /api/v1/skills/{id}/versions [scope=global]": {
			reason: "agents lack global skill.update scope; refuses agent tokens",
			method: http.MethodPost,
			path:   func(f *standingCoverageFixture) string { return "/api/v1/skills/" + f.globalSkillID + "/versions" },
			body:   func(*standingCoverageFixture) any { return map[string]any{"version": "1.0.1"} },
		},
		"POST /api/v1/skills/{id}/upload [scope=global]": {
			reason: "agents lack global skill.update scope; refuses agent tokens",
			method: http.MethodPost,
			path:   func(f *standingCoverageFixture) string { return "/api/v1/skills/" + f.globalSkillID + "/upload" },
			body:   func(*standingCoverageFixture) any { return map[string]any{} },
		},
		"POST /api/v1/templates [scope=global]": {
			reason: "agents cannot create global templates; refuses agent tokens",
			method: http.MethodPost,
			path:   func(*standingCoverageFixture) string { return "/api/v1/templates" },
			body:   func(*standingCoverageFixture) any { return map[string]any{"name": "g-tpl", "scope": "global"} },
		},
		"POST /api/v1/templates/{id}/upload [scope=global]": {
			reason: "agents lack global template.update scope; refuses agent tokens",
			method: http.MethodPost,
			path:   func(f *standingCoverageFixture) string { return "/api/v1/templates/" + f.globalTplID + "/upload" },
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
				matched := probeBaseKeys[key]
				if !matched && !strings.Contains(pattern, " ") {
					matched = probeBaseKeys[http.MethodGet+" "+pattern]
				}
				if !matched {
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

	// --- Structural Check 2: every authzop ResponseCredential HTTP route+variant is accounted for ---
	t.Run("every authzop ResponseCredential route is covered", func(t *testing.T) {
		discovered := make(map[string]bool)
		publicExemptions := make(map[string]string)

		for _, op := range authzop.Catalog {
			for _, ep := range op.EntryPoints {
				if ep.Kind != authzop.EntryPointHTTPRoute || ep.Response != authzop.ResponseCredential {
					continue
				}
				discovered[normalizeStandingRouteVariantKey(ep.Pattern, ep.Method, ep.Variant)] = true
			}
		}
		for _, ex := range authzop.EntryPointExemptions {
			if ex.Response != authzop.ResponseCredential {
				continue
			}
			key := normalizeStandingRouteVariantKey(ex.Pattern, http.MethodPost, "")
			discovered[key] = true
			if ex.Kind == authzop.ExemptionPublicEndpoint {
				publicExemptions[key] = ex.Pattern
			}
		}
		for _, ep := range authzop.PendingBearerDispositions {
			if ep.Kind != authzop.EntryPointHTTPRoute || ep.Response != authzop.ResponseCredential {
				continue
			}
			discovered[normalizeStandingRouteVariantKey(ep.Pattern, ep.Method, ep.Variant)] = true
		}

		var uncovered []string
		for key := range discovered {
			if probeVariantKeys[key] {
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
		require.Empty(t, uncovered, "authzop ResponseCredential routes/variants missing from standing coverage census")

		for key, spec := range nonAgentCallableCredentialRoutes {
			if spec.reason == "" {
				t.Errorf("nonAgentCallableCredentialRoutes[%q] missing reason", key)
			}
			if !discovered[key] {
				t.Errorf("stale nonAgentCallableCredentialRoutes entry %q", key)
			}
			if probeVariantKeys[key] {
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
		stor, err := storage.NewLocal(storage.Config{Provider: storage.ProviderLocal, Bucket: "b", LocalPath: t.TempDir()})
		require.NoError(t, err)
		srv.SetStorage(stor)
		artDB, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "artifacts.db")+"?_pragma=busy_timeout(5000)")
		require.NoError(t, err)
		t.Cleanup(func() { _ = artDB.Close() })
		artStore := artifacts.NewStore(artDB, "sqlite")
		require.NoError(t, artStore.Init(context.Background()))
		srv.SetArtifactStore(artStore)
		srv.testIdentities.enabled = true
		srv.testAdminGate.hubAdmin = true
		cur := srv.experimentRegistry()
		var active []experiments.Experiment
		for _, e := range cur.All() {
			if e.Name == experiments.AgentDelegation ||
				e.Name == experiments.Triggers ||
				e.Name == experiments.Artifacts ||
				e.Name == experiments.FederationProjectVisitors ||
				e.Name == conduitExperiment {
				e.Default = true
			}
			active = append(active, e)
		}
		reg, err := experiments.NewRegistry(active, cur.Retired())
		require.NoError(t, err)
		srv.setExperimentRegistry(reg)

		conduitSecret := make([]byte, 32)
		_, err = rand.Read(conduitSecret)
		require.NoError(t, err)
		conduitPeerAuth, err := relay.NewHMACPeerAuthFromSecret(relay.HMACPeerAuthConfig{Secret: conduitSecret, SelfID: "standing-cov-" + name})
		require.NoError(t, err)
		require.NoError(t, srv.StartConduitRelay(context.Background(), ConduitRelayOptions{
			InstanceID: "standing-cov-" + name,
			PeerAuth:   conduitPeerAuth,
			Store:      entadapter.NewConduitRegistryStore(enttest.NewClient(t)),
		}))
		t.Cleanup(func() {
			sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			srv.shutdownConduitRelay(sctx)
		})

		f := newMSFixtureOn(t, name, srv, srv.store)
		ctx := context.Background()
		// Root the fixture at a project admin with full-role agents so
		// admin-gated project write routes (skill create/versions/upload,
		// template create/upload) reach authorization/standing rather than
		// failing earlier on a member role denial.
		f.dropBindings(f.userID)
		f.addMember(f.userID, store.ProjectRoleAdmin)
		for _, ag := range []*store.Agent{f.agentA, f.childC} {
			ag.AppliedConfig = &store.AgentAppliedConfig{AgentRole: string(AgentRoleFull)}
			require.NoError(t, f.s.UpdateAgent(ctx, ag))
		}
		scopes := append(append([]AgentTokenScope{}, ScopesForRole(AgentRoleFull)...), ScopeIdentityToken)
		tokA := f.agentToken(f.agentA, scopes...)
		tokC := f.agentToken(f.childC, scopes...)

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

		// Seed project-scoped skill (with published version and storage file),
		// template, and harness config so download and resolve routes succeed
		// with 200 for a healthy agent and refuse after standing loss.
		skillID := tid(name + "-skill")
		skillSlug := "standing-skill-" + name
		skillStoragePath := "skills/project/" + f.projectID + "/" + skillSlug
		skillContent := []byte("---\nname: " + skillSlug + "\n---\n# Standing Test\n")
		_, err = stor.Upload(ctx, skillStoragePath+"/1.0.0/SKILL.md", bytes.NewReader(skillContent), storage.UploadOptions{})
		require.NoError(t, err)
		require.NoError(t, f.s.CreateSkill(ctx, &store.Skill{
			ID:            skillID,
			Name:          skillSlug,
			Slug:          skillSlug,
			Scope:         store.SkillScopeProject,
			ScopeID:       f.projectID,
			Status:        "active",
			StoragePath:   skillStoragePath,
			StorageBucket: "b",
		}))
		require.NoError(t, f.s.CreateSkillVersion(ctx, &store.SkillVersion{
			ID:      tid(name + "-skill-v1"),
			SkillID: skillID,
			Version: "1.0.0",
			Status:  store.SkillVersionStatusPublished,
			Files: []store.TemplateFile{{
				Path: "SKILL.md",
				Size: int64(len(skillContent)),
				Hash: "sha256:placeholder",
			}},
			Created: time.Now().UTC(),
		}))
		globalSkillID := tid(name + "-global-skill")
		require.NoError(t, f.s.CreateSkill(ctx, &store.Skill{
			ID:     globalSkillID,
			Name:   "standing-global-skill-" + name,
			Slug:   "standing-global-skill-" + name,
			Scope:  store.SkillScopeGlobal,
			Status: "active",
		}))

		tplID := tid(name + "-tpl")
		tplSlug := "standing-tpl-" + name
		tplStoragePath := "templates/project/" + f.projectID + "/" + tplSlug
		tplContent := []byte("harness: claude\n")
		_, err = stor.Upload(ctx, tplStoragePath+"/scion-agent.yaml", bytes.NewReader(tplContent), storage.UploadOptions{})
		require.NoError(t, err)
		require.NoError(t, f.s.CreateTemplate(ctx, &store.Template{
			ID:            tplID,
			Name:          tplSlug,
			Slug:          tplSlug,
			Scope:         store.TemplateScopeProject,
			ScopeID:       f.projectID,
			Status:        store.TemplateStatusActive,
			StoragePath:   tplStoragePath,
			StorageBucket: "b",
			Files: []store.TemplateFile{{
				Path: "scion-agent.yaml",
				Size: int64(len(tplContent)),
				Hash: "sha256:placeholder",
			}},
		}))
		globalTplID := tid(name + "-global-tpl")
		require.NoError(t, f.s.CreateTemplate(ctx, &store.Template{
			ID:     globalTplID,
			Name:   "standing-global-tpl-" + name,
			Slug:   "standing-global-tpl-" + name,
			Scope:  store.TemplateScopeGlobal,
			Status: store.TemplateStatusActive,
		}))

		hcID := tid(name + "-hc")
		hcSlug := "standing-hc-" + name
		hcStoragePath := "harness-configs/project/" + f.projectID + "/" + hcSlug
		hcContent := []byte("harness: claude\n")
		_, err = stor.Upload(ctx, hcStoragePath+"/config.yaml", bytes.NewReader(hcContent), storage.UploadOptions{})
		require.NoError(t, err)
		require.NoError(t, f.s.CreateHarnessConfig(ctx, &store.HarnessConfig{
			ID:            hcID,
			Name:          hcSlug,
			Slug:          hcSlug,
			Harness:       "claude",
			Scope:         store.HarnessConfigScopeProject,
			ScopeID:       f.projectID,
			Status:        store.HarnessConfigStatusActive,
			StoragePath:   hcStoragePath,
			StorageBucket: "b",
			Files: []store.TemplateFile{{
				Path: "config.yaml",
				Size: int64(len(hcContent)),
				Hash: "sha256:placeholder",
			}},
		}))

		brokerID := tid(name + "-broker")
		require.NoError(t, f.s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
			ID:     brokerID,
			Name:   "standing-broker-" + name,
			Slug:   "standing-broker-" + name,
			Status: store.BrokerStatusOnline,
		}))

		// Seed an HTML artifact owned by agentA so /versions/1/view succeeds
		// with 200 for a healthy agent (and refuses after standing loss) while
		// /links hits the explicit agent-refusal 403.
		agentArtID := tid(name + "-agent-art")
		artContent := []byte("<!doctype html><html><body>ok</body></html>")
		artSum := sha256.Sum256(artContent)
		artDigest := hex.EncodeToString(artSum[:])
		_, err = stor.Upload(ctx, artifacts.BlobPath("test-hub-id", artDigest), bytes.NewReader(artContent), storage.UploadOptions{ContentType: "text/html"})
		require.NoError(t, err)
		now := time.Now().UTC()
		require.NoError(t, artStore.CreatePublished(ctx,
			&artifacts.Artifact{
				ID: agentArtID, ScopeKind: artifacts.ScopeKindProject, ScopeRef: f.projectID,
				OwnerKind: artifacts.PrincipalKindAgent, OwnerRef: f.agentA.ID, Title: "agent-art",
				CreatedAt: now, UpdatedAt: now,
			},
			&artifacts.Version{
				ID: tid(name + "-agent-art-v1"), ArtifactID: agentArtID, Seq: 1, Kind: artifacts.VersionKindPublish,
				EntryPath: "index.html", TotalBytes: int64(len(artContent)), FileCount: 1, CreatedAt: now, State: artifacts.VersionStateReady,
			},
			[]artifacts.File{{VersionID: tid(name + "-agent-art-v1"), Path: "index.html", Size: int64(len(artContent)), SHA256: artDigest, MediaType: "text/html"}},
			nil,
		))

		return &standingCoverageFixture{
			msFixture:       f,
			tokA:            tokA,
			tokC:            tokC,
			grants:          grants,
			skillID:         skillID,
			skillSlug:       skillSlug,
			globalSkillID:   globalSkillID,
			templateID:      tplID,
			globalTplID:     globalTplID,
			harnessConfigID: hcID,
			brokerID:        brokerID,
			artifactID:      agentArtID,
		}
	}

	assertHealthyControls := func(t *testing.T, f *standingCoverageFixture) {
		t.Helper()
		rec := doRequestWithAgentToken(t, f.srv, http.MethodGet, "/api/v1/env?scope=project&scopeId="+f.projectID, nil, f.tokA)
		require.Equal(t, http.StatusOK, rec.Code, "control: active agent must reach GET /api/v1/env: %s", rec.Body.String())

		for _, dl := range []struct {
			name string
			path string
		}{
			{"skill download", "/api/v1/skills/" + f.skillID + "/download"},
			{"template download", "/api/v1/templates/" + f.templateID + "/download"},
			{"harness-config download", "/api/v1/harness-configs/" + f.harnessConfigID + "/download"},
		} {
			dlRec := doRequestWithAgentToken(t, f.srv, http.MethodGet, dl.path, nil, f.tokA)
			require.Equal(t, http.StatusOK, dlRec.Code, "control: active agent must reach %s: %s", dl.name, dlRec.Body.String())
			var dlResp DownloadResponse
			require.NoError(t, json.NewDecoder(dlRec.Body).Decode(&dlResp), dl.name)
			require.NotEmpty(t, dlResp.Files, "control: %s must return download URLs", dl.name)
		}

		resRec := doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/skills/resolve", ResolveSkillsRequest{
			Skills:    []ResolveSkillRef{{URI: "skill://" + f.skillSlug}},
			ProjectID: f.projectID,
		}, f.tokA)
		require.Equal(t, http.StatusOK, resRec.Code, "control: active agent must reach POST /api/v1/skills/resolve: %s", resRec.Body.String())
		var resResp ResolveSkillsResponse
		require.NoError(t, json.NewDecoder(resRec.Body).Decode(&resResp))
		require.Len(t, resResp.Resolved, 1, "control: skill:// resolve must return 1 resolved skill")
		require.NotEmpty(t, resResp.Resolved[0].Files, "control: skill:// resolve must return signed file URLs")
		assert.Empty(t, resResp.Errors)

		viewRec := doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/artifacts/"+f.artifactID+"/versions/1/view", nil, f.tokA)
		require.Equal(t, http.StatusOK, viewRec.Code, "control: active agent must reach POST /api/v1/artifacts/{id}/versions/{seq}/view: %s", viewRec.Body.String())

		conduitRec := doRequestWithAgentToken(t, f.srv, http.MethodGet, "/api/v1/conduit", nil, f.tokA)
		require.Equal(t, http.StatusBadRequest, conduitRec.Code, "control: active agent must pass standing check and reach WebSocket upgrade check on GET /api/v1/conduit: %s", conduitRec.Body.String())
		assert.Contains(t, conduitRec.Body.String(), "WebSocket upgrade required")

		// Positive controls for admin-rooted project skill and template write
		// routes (direct and child agents) so refusal cases below prove loss of
		// standing rather than an earlier role or scope denial.
		for _, caller := range []struct {
			label   string
			tok     string
			suffix  string
			version string
		}{
			{"direct", f.tokA, "direct", "1.1.0"},
			{"child", f.tokC, "child", "1.2.0"},
		} {
			skCreateRec := doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/skills", map[string]any{
				"name": "ctrl-skill-" + caller.suffix, "scope": "project", "scopeId": f.projectID,
			}, caller.tok)
			require.Equal(t, http.StatusCreated, skCreateRec.Code, "control (%s): active admin-rooted agent must create project skill: %s", caller.label, skCreateRec.Body.String())

			skVerRec := doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/skills/"+f.skillID+"/versions", map[string]any{
				"version": caller.version,
				"files":   []map[string]any{{"path": "SKILL.md", "size": 10}},
			}, caller.tok)
			require.Equal(t, http.StatusCreated, skVerRec.Code, "control (%s): active admin-rooted agent must publish project skill version: %s", caller.label, skVerRec.Body.String())
			var skVerResp PublishVersionResponse
			require.NoError(t, json.NewDecoder(skVerRec.Body).Decode(&skVerResp))
			require.NotEmpty(t, skVerResp.UploadURLs, "control (%s): skill version publish must return upload URLs", caller.label)

			skUpRec := doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/skills/"+f.skillID+"/upload", map[string]any{
				"version": "1.0.0",
				"files":   []map[string]any{{"path": "SKILL.md", "size": 10}},
			}, caller.tok)
			require.Equal(t, http.StatusOK, skUpRec.Code, "control (%s): active admin-rooted agent must request project skill upload URLs: %s", caller.label, skUpRec.Body.String())
			var skUpResp UploadResponse
			require.NoError(t, json.NewDecoder(skUpRec.Body).Decode(&skUpResp))
			require.NotEmpty(t, skUpResp.UploadURLs, "control (%s): skill upload must return upload URLs", caller.label)

			tplCreateRec := doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/templates", map[string]any{
				"name":    "ctrl-tpl-" + caller.suffix,
				"scope":   "project",
				"scopeId": f.projectID,
				"files":   []map[string]any{{"path": "scion-agent.yaml", "size": 10}},
			}, caller.tok)
			require.Equal(t, http.StatusCreated, tplCreateRec.Code, "control (%s): active admin-rooted agent must create project template: %s", caller.label, tplCreateRec.Body.String())
			var tplCreateResp CreateTemplateResponse
			require.NoError(t, json.NewDecoder(tplCreateRec.Body).Decode(&tplCreateResp))
			require.NotEmpty(t, tplCreateResp.UploadURLs, "control (%s): template create must return upload URLs", caller.label)

			tplUpRec := doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/templates/"+f.templateID+"/upload", map[string]any{
				"files": []map[string]any{{"path": "scion-agent.yaml", "size": 10}},
			}, caller.tok)
			require.Equal(t, http.StatusOK, tplUpRec.Code, "control (%s): active admin-rooted agent must request project template upload URLs: %s", caller.label, tplUpRec.Body.String())
			var tplUpResp UploadResponse
			require.NoError(t, json.NewDecoder(tplUpRec.Body).Decode(&tplUpResp))
			require.NotEmpty(t, tplUpResp.UploadURLs, "control (%s): template upload must return upload URLs", caller.label)
		}
	}

	// --- Behavioral Check 1: suspended root user (no AgentHold) refused on every route ---
	t.Run("suspended root user refused across every agent credential route", func(t *testing.T) {
		f := newFullFixture(t, "cov-susp")
		ctx := context.Background()

		// Positive controls: active agent succeeds on env, downloads, and skill:// resolve before suspension.
		assertHealthyControls(t, f)

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

		// Positive controls before root-user removal (covers the review-2 skill:// resolve + download cases).
		assertHealthyControls(t, f)

		f.dropBindings(f.userID)
		require.False(t, f.held(f.agentA.ID))
		require.False(t, f.held(f.childC.ID))
		requireStandingReason(t, f.srv.agentStanding(ctx, f.agentA.ID), standingReasonRootNotAdmitted)
		requireStandingReason(t, f.srv.agentStanding(ctx, f.childC.ID), standingReasonRootNotAdmitted)

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
					rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden,
					"%s: expected 401/403 for active agent token, got %d: %s",
					key, rec.Code, rec.Body.String())
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

	// --- Behavioral Check 5: soft-deleted agent refused by standing check on admin-rooted skill/template write routes ---
	t.Run("soft-deleted agent refused on admin-rooted skill and template write routes", func(t *testing.T) {
		f := newFullFixture(t, "cov-softdel")
		ctx := context.Background()

		// Positive controls before soft-deletion confirm CheckAccess allows both agents.
		assertHealthyControls(t, f)

		// Soft-delete agentA without revoking its token or placing an AgentHold.
		// UnifiedAuthMiddleware and CheckAccess (delegation ceiling) still pass
		// for agentA because its admin root is active and its delegation edge is
		// active; only the handler's agentStandingForbidden check refuses it.
		a, err := f.s.GetAgent(ctx, f.agentA.ID)
		require.NoError(t, err)
		a.DeletedAt = time.Now().UTC()
		require.NoError(t, f.s.UpdateAgent(ctx, a))
		require.False(t, f.held(f.agentA.ID))
		requireStandingReason(t, f.srv.agentStanding(ctx, f.agentA.ID), standingReasonAgentDeleted)

		writeProbes := []struct {
			name   string
			method string
			path   string
			body   any
		}{
			{
				name:   "POST /api/v1/skills [scope=project]",
				method: http.MethodPost,
				path:   "/api/v1/skills",
				body:   map[string]any{"name": "softdel-skill", "scope": "project", "scopeId": f.projectID},
			},
			{
				name:   "POST /api/v1/skills/{id}/versions",
				method: http.MethodPost,
				path:   "/api/v1/skills/" + f.skillID + "/versions",
				body:   map[string]any{"version": "2.0.0", "files": []map[string]any{{"path": "SKILL.md", "size": 10}}},
			},
			{
				name:   "POST /api/v1/skills/{id}/upload",
				method: http.MethodPost,
				path:   "/api/v1/skills/" + f.skillID + "/upload",
				body:   map[string]any{"version": "1.0.0", "files": []map[string]any{{"path": "SKILL.md", "size": 10}}},
			},
			{
				name:   "POST /api/v1/templates [scope=project]",
				method: http.MethodPost,
				path:   "/api/v1/templates",
				body:   map[string]any{"name": "softdel-tpl", "scope": "project", "scopeId": f.projectID, "files": []map[string]any{{"path": "scion-agent.yaml", "size": 10}}},
			},
			{
				name:   "POST /api/v1/templates/{id}/upload",
				method: http.MethodPost,
				path:   "/api/v1/templates/" + f.templateID + "/upload",
				body:   map[string]any{"files": []map[string]any{{"path": "scion-agent.yaml", "size": 10}}},
			},
		}

		for _, wp := range writeProbes {
			t.Run(wp.name+" (direct)", func(t *testing.T) {
				rec := doRequestWithAgentToken(t, f.srv, wp.method, wp.path, wp.body, f.tokA)
				require.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", wp.name, rec.Body.String())
				assert.Contains(t, rec.Body.String(), "Insufficient permissions", wp.name)
			})
			t.Run(wp.name+" (child)", func(t *testing.T) {
				rec := doRequestWithAgentToken(t, f.srv, wp.method, wp.path, wp.body, f.tokC)
				require.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", wp.name, rec.Body.String())
			})
		}
	})
}
