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

//go:build !no_sqlite

package hub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// agentTokenRefusalCase is one agent-token refusal: the token sent as
// X-Scion-Agent-Token, the run header, the agent id for a path, and the
// cause the refusal is logged with ("" for a run-scope refusal, which the
// run-scope checker logs itself).
type agentTokenRefusalCase struct {
	name      string
	cause     string
	agentID   string
	token     string
	runHeader string
}

// agentTokenRefusalCases enforces run scope on srv and stores an agent and
// a token for each refusal cause UnifiedAuthMiddleware answers.
func agentTokenRefusalCases(t *testing.T, srv *Server, s store.Store, project *store.Project) []agentTokenRefusalCase {
	t.Helper()
	ctx := context.Background()
	srv.authConfig.AgentRunScope = testRunScopeChecker(agentRunScopeEnforce, time.Now().Add(-time.Hour), s)
	var cases []agentTokenRefusalCase

	// bound stores an agent and a token bound to its current run.
	bound := func(name string) (*store.Agent, string) {
		agent := runScopeAgent(t, s, project.ID, name, "run-"+name)
		return agent, signRunToken(t, srv, s, agent, agent.RunID, recordRun(agent.RunID))
	}

	revoked, revokedTok := bound("refusal-revoked")
	claims, err := srv.agentTokenService.ValidateAgentToken(revokedTok)
	require.NoError(t, err)
	cred, err := s.GetAgentCredentialByJTIHash(ctx, hashJTI(claims.ID))
	require.NoError(t, err)
	require.NoError(t, s.RevokeAgentCredential(ctx, cred.ID, "test", "explicit"))
	cases = append(cases, agentTokenRefusalCase{name: "revoked", cause: "revoked", agentID: revoked.ID, token: revokedTok})

	held, heldTok := bound("refusal-held")
	_, err = s.CreateAgentHolds(ctx, []*store.AgentHold{{
		AgentID:           held.ID,
		ProjectID:         held.ProjectID,
		Cause:             store.AgentHoldCauseOwnerAccessEnded,
		RootPrincipalType: store.AgentHoldRootUser,
		RootPrincipalID:   tid("user-cred-test"),
		Trigger:           store.MembershipLossTriggerMemberRemove,
		ActorKind:         "system",
		ActorID:           "hub",
		CorrelationID:     "test",
	}})
	require.NoError(t, err)
	cases = append(cases, agentTokenRefusalCase{name: "held", cause: "held", agentID: held.ID, token: heldTok})

	subjectTok, _, err := srv.agentTokenService.SignAgentToken(AgentTokenGrant{
		AgentID: "not-an-agent", ProjectID: project.ID, Scopes: ScopesForRole(AgentRoleFull),
	}, "")
	require.NoError(t, err)
	cases = append(cases, agentTokenRefusalCase{name: "invalid subject", cause: "invalid_subject", agentID: revoked.ID, token: subjectTok})

	cases = append(cases, agentTokenRefusalCase{name: "invalid token", cause: "invalid_token", agentID: revoked.ID, token: "not-a-token"})

	superseded := runScopeAgent(t, s, project.ID, "refusal-superseded", "run-refusal-superseded")
	cases = append(cases, agentTokenRefusalCase{
		name: "run superseded", agentID: superseded.ID,
		token: signRunToken(t, srv, s, superseded, "run-old", recordRun("run-old")),
	})

	mismatch, mismatchTok := bound("refusal-run-header")
	cases = append(cases, agentTokenRefusalCase{name: "run header", agentID: mismatch.ID, token: mismatchTok, runHeader: "run-elsewhere"})

	return cases
}

// testOuterHeader is set by withTestOuterHeader, outside every hub
// middleware.
const (
	testOuterHeader      = "X-Test-Outer"
	testOuterHeaderValue = "kept"
)

func withTestOuterHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(testOuterHeader, testOuterHeaderValue)
		next.ServeHTTP(w, r)
	})
}

// serveAuditRoute sends a GET for the constraint audit route through h,
// with an Origin the test server's CORS allows, and returns the response.
func serveAuditRoute(h http.Handler, set func(*http.Request)) rawResponse {
	req := httptest.NewRequest(http.MethodGet, constraintAuditAuthTestPath, nil)
	req.Header.Set("Origin", "https://outer.example")
	set(req)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rawOf(rec)
}

// TestUniformRefusalResponsesOnAuditRoute: on the constraint audit route
// every agent-token refusal gets the response a missing constraint gets:
// status, the full header map and body. Headers set before
// authentication are kept and nothing authentication set is.
func TestUniformRefusalResponsesOnAuditRoute(t *testing.T) {
	srv, s, _, project := setupCredentialTestServer(t)
	cases := agentTokenRefusalCases(t, srv, s, project)
	h := withTestOuterHeader(srv.Handler())

	want := serveAuditRoute(h, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+testDevToken) })
	require.Equal(t, http.StatusNotFound, want.Status, want.Body)
	require.Equal(t, testOuterHeaderValue, want.Header.Get(testOuterHeader))
	require.NotEmpty(t, want.Header.Get("Access-Control-Allow-Origin"))
	require.Empty(t, want.Header.Values("WWW-Authenticate"))

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := serveAuditRoute(h, func(r *http.Request) {
				r.Header.Set("X-Scion-Agent-Token", c.token)
				if c.runHeader != "" {
					r.Header.Set(AgentRunIDHeader, c.runHeader)
				}
			})
			assert.Equal(t, want, got)
			assert.Empty(t, got.Header.Values("WWW-Authenticate"))
		})
	}

	t.Run("development middleware", func(t *testing.T) {
		notFound := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			NotFound(w, "Access Constraint")
		})
		dh := withTestOuterHeader(DevAuthMiddleware(testDevToken, srv.config.DevUserConfig)(notFound))
		want := serveAuditRoute(dh, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+testDevToken) })
		require.Equal(t, http.StatusNotFound, want.Status, want.Body)
		require.Equal(t, testOuterHeaderValue, want.Header.Get(testOuterHeader))

		got := serveAuditRoute(dh, func(r *http.Request) { r.Header.Set("X-Scion-Agent-Token", "not-a-token") })
		assert.Equal(t, want, got)
		assert.Empty(t, got.Header.Values("WWW-Authenticate"))
	})
}
