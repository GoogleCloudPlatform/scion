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

// TestAgentStanding_RouteGuardEnforcesDefaultAndGuard proves that routeGuard
// structurally enforces delegation standing on RouteAgentToken routes whose
// policy is AgentStandingGuard or unset (fail-closed), even when the wrapped
// handler itself performs no standing check.
func TestAgentStanding_RouteGuardEnforcesDefaultAndGuard(t *testing.T) {
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

// agentStandingProbe describes one agent-callable credential, token, or
// secret-issuing HTTP route that must refuse an orphaned agent.
type agentStandingProbe struct {
	// key is the canonical "METHOD /path" pattern matching authzop / routeMetadataTable.
	key     string
	method  string
	path    func(f *msFixture, agent *store.Agent, grantID string) string
	body    func() any
	want403 func(t *testing.T, rec *httptest.ResponseRecorder, label string)
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
//  2. Every authzop Catalog or Exemption HTTP route with ResponseCredential
//     under /api/v1/agent/ or /api/v1/agents/ must either be in the behavioral
//     probe list or in the explicit non-agent-callable exclusion list (and
//     excluded routes are probed to confirm agent tokens are refused).
//  3. Every route in the behavioral probe list is exercised through the full
//     HTTP stack (UnifiedAuthMiddleware -> routeGuard -> handler) with an
//     orphaned agent (suspended root user, no AgentHold), a removed root user,
//     and a child agent of a suspended root user, asserting 403 Forbidden.
func TestAgentStanding_CredentialRoutesCoverage(t *testing.T) {
	assertForbidden := func(t *testing.T, rec *httptest.ResponseRecorder, label string) {
		t.Helper()
		require.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", label, rec.Body.String())
	}

	probes := []agentStandingProbe{
		{
			key:    "POST /api/v1/agent/gcp-token",
			method: http.MethodPost,
			path:   func(*msFixture, *store.Agent, string) string { return "/api/v1/agent/gcp-token" },
			body:   func() any { return map[string]any{} },
			want403: func(t *testing.T, rec *httptest.ResponseRecorder, label string) {
				assertForbidden(t, rec, label)
				assert.Contains(t, rec.Body.String(), "Insufficient permissions", label)
			},
		},
		{
			key:    "POST /api/v1/agent/gcp-identity-token",
			method: http.MethodPost,
			path:   func(*msFixture, *store.Agent, string) string { return "/api/v1/agent/gcp-identity-token" },
			body:   func() any { return map[string]string{"audience": "https://api.example.com"} },
			want403: func(t *testing.T, rec *httptest.ResponseRecorder, label string) {
				assertForbidden(t, rec, label)
				assert.Contains(t, rec.Body.String(), "Insufficient permissions", label)
			},
		},
		{
			key:    "POST /api/v1/agent/identity-token",
			method: http.MethodPost,
			path:   func(*msFixture, *store.Agent, string) string { return "/api/v1/agent/identity-token" },
			body:   func() any { return map[string]string{"audience": "https://api.example.com"} },
			want403: func(t *testing.T, rec *httptest.ResponseRecorder, label string) {
				assertForbidden(t, rec, label)
				assert.Contains(t, rec.Body.String(), "Insufficient permissions", label)
			},
		},
		{
			key:     "POST /api/v1/agent/secrets",
			method:  http.MethodPost,
			path:    func(*msFixture, *store.Agent, string) string { return "/api/v1/agent/secrets" },
			body:    func() any { return map[string]any{"keys": []string{"API_KEY"}} },
			want403: assertForbidden,
		},
		{
			key:    "POST /api/v1/agents/{id}/token/refresh",
			method: http.MethodPost,
			path: func(_ *msFixture, a *store.Agent, _ string) string {
				return "/api/v1/agents/" + a.ID + "/token/refresh"
			},
			body:    func() any { return nil },
			want403: assertForbidden,
		},
		{
			key:    "POST /api/v1/agents/{id}/refresh-token",
			method: http.MethodPost,
			path: func(_ *msFixture, a *store.Agent, _ string) string {
				return "/api/v1/agents/" + a.ID + "/refresh-token"
			},
			body: func() any { return nil },
			want403: func(t *testing.T, rec *httptest.ResponseRecorder, label string) {
				assertForbidden(t, rec, label)
				assert.Contains(t, rec.Body.String(), "Insufficient permissions", label)
			},
		},
		{
			key:    "POST /api/v1/agents/{id}/delegations/{grantId}/exchange",
			method: http.MethodPost,
			path: func(_ *msFixture, a *store.Agent, grantID string) string {
				return "/api/v1/agents/" + a.ID + "/delegations/" + grantID + "/exchange"
			},
			body: func() any { return map[string]any{"audience": "https://hub.example.test"} },
			want403: func(t *testing.T, rec *httptest.ResponseRecorder, label string) {
				assertForbidden(t, rec, label)
				assert.Contains(t, rec.Body.String(), errCodeGrantAgentChanged, label)
			},
		},
		{
			key:    "GET /api/v1/agents/{id}/secrets/{key}",
			method: http.MethodGet,
			path: func(_ *msFixture, a *store.Agent, _ string) string {
				return "/api/v1/agents/" + a.ID + "/secrets/API_KEY"
			},
			body:    func() any { return nil },
			want403: assertForbidden,
		},
		{
			key:    "POST /api/v1/agents",
			method: http.MethodPost,
			path:   func(*msFixture, *store.Agent, string) string { return "/api/v1/agents" },
			body: func() any {
				return map[string]any{"name": "sub-agent-standing-test"}
			},
			want403: assertForbidden,
		},
		{
			key:    "POST /api/v1/projects/{projectId}/agents",
			method: http.MethodPost,
			path: func(f *msFixture, _ *store.Agent, _ string) string {
				return "/api/v1/projects/" + f.projectID + "/agents"
			},
			body: func() any {
				return map[string]any{"name": "sub-agent-standing-test"}
			},
			want403: assertForbidden,
		},
	}

	probeByKey := make(map[string]agentStandingProbe, len(probes))
	for _, p := range probes {
		require.NotContains(t, probeByKey, p.key, "duplicate probe key %s", p.key)
		probeByKey[p.key] = p
	}

	// Explicit exempt RouteAgentToken routes (non-credential routes).
	exemptAgentTokenRoutes := map[string]string{
		"/api/v1/agent/decisions":  "reads and records agent decisions; issues no credentials, tokens, or secrets",
		"/api/v1/agent/decisions/": "reads and records agent decisions; issues no credentials, tokens, or secrets",
		"/api/v1/conduit":          "WebSocket session endpoint (agent, broker, user); issues no credentials or tokens",
	}

	// Explicit ResponseCredential routes under /api/v1/agents/ that do not admit
	// agent JWTs at all (verified below).
	nonAgentCallableCredentialRoutes := map[string]string{
		"POST /api/v1/agents/{id}/workspace/sync-from": "user/broker-only workspace sync; refuses agent tokens",
		"POST /api/v1/agents/{id}/workspace/sync-to":   "user/broker-only workspace sync; refuses agent tokens",
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
				if _, ok := probeByKey[key]; !ok {
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

	// --- Structural Check 2: every authzop ResponseCredential agent route is accounted for ---
	t.Run("every authzop ResponseCredential agent route is covered", func(t *testing.T) {
		isAgentPath := func(pattern string) bool {
			_, p, ok := strings.Cut(pattern, " ")
			if !ok {
				p = pattern
			}
			return strings.HasPrefix(p, "/api/v1/agent/") ||
				p == "/api/v1/agents" ||
				strings.HasPrefix(p, "/api/v1/agents/") ||
				p == "/api/v1/projects/{projectId}/agents"
		}

		discovered := make(map[string]bool)
		for _, op := range authzop.Catalog {
			for _, ep := range op.EntryPoints {
				if ep.Kind != authzop.EntryPointHTTPRoute || ep.Response != authzop.ResponseCredential {
					continue
				}
				if !isAgentPath(ep.Pattern) {
					continue
				}
				discovered[normalizeStandingRouteKey(ep.Pattern, ep.Method)] = true
			}
		}
		for _, ex := range authzop.EntryPointExemptions {
			if ex.Response != authzop.ResponseCredential || !isAgentPath(ex.Pattern) {
				continue
			}
			discovered[normalizeStandingRouteKey(ex.Pattern, http.MethodPost)] = true
		}

		var uncovered []string
		for key := range discovered {
			if _, ok := probeByKey[key]; ok {
				continue
			}
			if nonAgentCallableCredentialRoutes[key] != "" {
				continue
			}
			uncovered = append(uncovered, key)
		}
		sort.Strings(uncovered)
		require.Empty(t, uncovered, "authzop ResponseCredential agent routes missing from standing coverage probes")

		for key := range nonAgentCallableCredentialRoutes {
			if !discovered[key] {
				t.Errorf("stale nonAgentCallableCredentialRoutes entry %q", key)
			}
		}
	})

	// Helper to create a server + msFixture with OIDC, secret backend, and
	// agent delegation enabled so every credential route reaches its standing check.
	newFullFixture := func(t *testing.T, name string) (*msFixture, string, string, map[string]string) {
		t.Helper()
		srv := testOIDCServerWithRoutes(t)
		t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
		srv.SetSecretBackend(secret.NewLocalBackend(srv.store, "test-hub-id", "test-secret"))
		cur := srv.experimentRegistry()
		var active []experiments.Experiment
		for _, e := range cur.All() {
			if e.Name == experiments.AgentDelegation {
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

		// Seed active delegation grants for agentA and childC so the exchange
		// route passes grant lookup and reaches the standing check.
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
		return f, tokA, tokC, grants
	}

	// --- Behavioral Check 1: suspended root user (no AgentHold) refused on every route ---
	t.Run("suspended root user refused across every agent credential route", func(t *testing.T) {
		f, tokA, tokC, grants := newFullFixture(t, "cov-susp")
		ctx := context.Background()

		u, err := f.s.GetUser(ctx, f.userID)
		require.NoError(t, err)
		u.Status = store.UserStatusSuspended
		require.NoError(t, f.s.UpdateUser(ctx, u))
		require.False(t, f.held(f.agentA.ID))
		require.False(t, f.held(f.childC.ID))
		requireStandingReason(t, f.srv.agentStanding(ctx, f.agentA.ID), standingReasonRootInactive)
		requireStandingReason(t, f.srv.agentStanding(ctx, f.childC.ID), standingReasonRootInactive)

		for _, p := range probes {
			t.Run(p.key+" (direct)", func(t *testing.T) {
				body := p.body()
				if m, ok := body.(map[string]any); ok && p.key == "POST /api/v1/agents" {
					m["projectId"] = f.projectID
				}
				rec := doRequestWithAgentToken(t, f.srv, p.method, p.path(f, f.agentA, grants[f.agentA.ID]), body, tokA)
				p.want403(t, rec, p.key+" (direct)")
			})
			t.Run(p.key+" (child)", func(t *testing.T) {
				body := p.body()
				if m, ok := body.(map[string]any); ok && p.key == "POST /api/v1/agents" {
					m["projectId"] = f.projectID
				}
				rec := doRequestWithAgentToken(t, f.srv, p.method, p.path(f, f.childC, grants[f.childC.ID]), body, tokC)
				p.want403(t, rec, p.key+" (child)")
			})
		}
	})

	// --- Behavioral Check 2: removed root user (no AgentHold) refused on every route ---
	t.Run("removed root user refused across every agent credential route", func(t *testing.T) {
		f, tokA, _, grants := newFullFixture(t, "cov-rem")
		ctx := context.Background()

		f.dropBindings(f.userID)
		require.False(t, f.held(f.agentA.ID))
		requireStandingReason(t, f.srv.agentStanding(ctx, f.agentA.ID), standingReasonRootNotAdmitted)

		for _, p := range probes {
			t.Run(p.key, func(t *testing.T) {
				body := p.body()
				if m, ok := body.(map[string]any); ok && p.key == "POST /api/v1/agents" {
					m["projectId"] = f.projectID
				}
				rec := doRequestWithAgentToken(t, f.srv, p.method, p.path(f, f.agentA, grants[f.agentA.ID]), body, tokA)
				p.want403(t, rec, p.key)
			})
		}
	})

	// --- Behavioral Check 3: excluded non-agent-callable routes refuse even an active agent ---
	t.Run("non-agent-callable credential routes refuse agent tokens", func(t *testing.T) {
		f, tokA, _, _ := newFullFixture(t, "cov-nonagent")
		for _, path := range []string{
			"/api/v1/agents/" + f.agentA.ID + "/workspace/sync-from",
			"/api/v1/agents/" + f.agentA.ID + "/workspace/sync-to",
		} {
			rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, path, nil, tokA)
			require.True(t, rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden,
				"%s: expected 401/403 for agent token, got %d: %s", path, rec.Code, rec.Body.String())
		}
	})
}
