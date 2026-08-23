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
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression tests for the #591 authorization bypass, covering the call sites
// converted in handlers_agents_core.go, handlers_projects_core.go and
// handlers_agent_create_helpers.go.
//
// The absence of exactly this test class is what allowed the bug: every one of
// these handlers had a guard that was skipped in silence for any caller that
// was not a UserIdentity, and the user-path tests all passed throughout. So
// each converted site gets a test asserting that an agent-authenticated caller
// is now denied on a request that previously succeeded, plus a broker-
// authenticated denial, plus positive tests that the agent flows we intend to
// keep still work.
//
// Test naming: everything file-local is prefixed bypassAgents so it cannot
// collide with the parallel conversion work in other handler files.

// ============================================================================
// Fixture
// ============================================================================

// bypassAgentsFixture is the world these tests reason about, mirroring the
// narrated scenario in design §5.3:
//
//	project (P1), owned by owner
//	  caller   — the agent presenting the token in most tests
//	  sibling  — a project peer of caller; NOT a descendant of it
//	  child    — a descendant of caller (caller.ID in its ancestry)
//	other (P2)
//	  stranger — an agent in a project the caller has nothing to do with
//
// The sibling/child distinction is load-bearing: the ancestry bypass grants
// caller full access to child, so a denial test written against child would
// pass for the wrong reason.
type bypassAgentsFixture struct {
	srv      *Server
	store    store.Store
	owner    *store.User
	proj     *store.Project
	other    *store.Project
	caller   *store.Agent
	sibling  *store.Agent
	child    *store.Agent
	stranger *store.Agent
	broker   *store.RuntimeBroker
	// brokerSecret is the HMAC key for broker-authenticated requests.
	brokerSecret []byte
	brokerAuthID string
}

// bypassAgentsServer builds a server that accepts all three identity kinds:
// the dev user token, agent JWTs, and HMAC-signed broker requests. The stock
// testServer has no broker auth, and the #591 bypass admitted brokers as well
// as agents, so the tests need a server where a broker caller can actually
// reach a handler.
func bypassAgentsServer(t *testing.T) (*Server, store.Store) {
	t.Helper()
	s, err := newTestStore(":memory:")
	if err != nil {
		t.Skipf("skipping: test store unavailable (%v)", err)
	}
	require.NoError(t, s.Migrate(context.Background()))

	cfg := DefaultServerConfig()
	cfg.DevAuthToken = testDevToken
	cfg.DevUserConfig = DevUserConfig{
		Username:    "dev",
		DisplayName: "Development User",
		Email:       "dev@localhost",
	}
	cfg.BrokerAuthConfig = DefaultBrokerAuthConfig()
	srv, err := New(cfg, s)
	require.NoError(t, err)
	srv.SetHubID("test-hub-id")
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	return srv, s
}

func bypassAgentsSetup(t *testing.T) *bypassAgentsFixture {
	t.Helper()
	srv, s := bypassAgentsServer(t)
	ctx := context.Background()
	f := &bypassAgentsFixture{srv: srv, store: s}

	f.owner = &store.User{
		ID:          tid("bypass-owner"),
		Email:       "bypass-owner@example.com",
		DisplayName: "Bypass Owner",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, f.owner))

	f.proj = &store.Project{
		ID:      tid("bypass-p1"),
		Name:    "Bypass P1",
		Slug:    "bypass-p1",
		OwnerID: f.owner.ID,
	}
	require.NoError(t, s.CreateProject(ctx, f.proj))

	f.other = &store.Project{
		ID:      tid("bypass-p2"),
		Name:    "Bypass P2",
		Slug:    "bypass-p2",
		OwnerID: f.owner.ID,
	}
	require.NoError(t, s.CreateProject(ctx, f.other))

	// An auto-provide broker, so that agent creation can resolve a broker and
	// the create tests exercise the authorization gate rather than dying at
	// broker selection.
	f.brokerSecret = []byte("bypass-secret-key-32-bytes-ok!!")
	f.brokerAuthID = uuid.New().String()
	f.broker = &store.RuntimeBroker{
		ID:          f.brokerAuthID,
		Name:        "bypass-broker",
		Slug:        "bypass-broker",
		Status:      store.BrokerStatusOnline,
		AutoProvide: true,
		Created:     time.Now(),
		Updated:     time.Now(),
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, f.broker))
	require.NoError(t, s.CreateBrokerSecret(ctx, &store.BrokerSecret{
		BrokerID:  f.broker.ID,
		SecretKey: f.brokerSecret,
		Algorithm: store.BrokerSecretAlgorithmHMACSHA256,
		Status:    store.BrokerSecretStatusActive,
	}))
	for _, p := range []*store.Project{f.proj, f.other} {
		require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
			ProjectID:  p.ID,
			BrokerID:   f.broker.ID,
			BrokerName: f.broker.Name,
			Status:     store.BrokerStatusOnline,
		}))
		p.DefaultRuntimeBrokerID = f.broker.ID
		require.NoError(t, s.UpdateProject(ctx, p))
	}

	mk := func(name, projectID string, ancestry []string) *store.Agent {
		a := &store.Agent{
			ID:        tid(name),
			Slug:      tid(name),
			Name:      name,
			ProjectID: projectID,
			Phase:     string(state.PhaseRunning),
			CreatedBy: f.owner.ID,
			OwnerID:   f.owner.ID,
			Ancestry:  ancestry,
		}
		require.NoError(t, s.CreateAgent(ctx, a))
		return a
	}
	f.caller = mk("bypass-caller", f.proj.ID, []string{f.owner.ID})
	f.sibling = mk("bypass-sibling", f.proj.ID, []string{f.owner.ID})
	f.child = mk("bypass-child", f.proj.ID, []string{f.owner.ID, tid("bypass-caller")})
	f.stranger = mk("bypass-stranger", f.other.ID, []string{f.owner.ID})

	return f
}

// token mints an agent JWT for the calling agent with the given scopes.
// ScopeProjectRead is always included because the baseline role grants it to
// every agent; without it, checkAgentReadScope rejects read requests before
// the handler-level authorization these tests are designed to exercise.
func (f *bypassAgentsFixture) token(t *testing.T, scopes ...AgentTokenScope) string {
	t.Helper()
	svc := f.srv.GetAgentTokenService()
	require.NotNil(t, svc)
	allScopes := append([]AgentTokenScope{ScopeProjectRead}, scopes...)
	tok, err := svc.GenerateAgentToken(f.caller.ID, f.caller.ProjectID, allScopes, nil)
	require.NoError(t, err)
	return tok
}

// asAgent issues a request carrying the calling agent's token.
func (f *bypassAgentsFixture) asAgent(t *testing.T, method, path string, body interface{}, scopes ...AgentTokenScope) *httptest.ResponseRecorder {
	t.Helper()
	return doRequestWithAgentToken(t, f.srv, method, path, body, f.token(t, scopes...))
}

// asAgentWithHeaders issues a request carrying the calling agent's token with additional headers.
func (f *bypassAgentsFixture) asAgentWithHeaders(t *testing.T, method, path string, body interface{}, headers map[string]string, scopes ...AgentTokenScope) *httptest.ResponseRecorder {
	t.Helper()
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		require.NoError(t, err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(bodyBytes))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Scion-Agent-Token", f.token(t, scopes...))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// asBroker issues an HMAC-signed broker request. Brokers implement neither
// UserIdentity nor AgentIdentity, so before #591 they slipped through every one
// of these guards exactly as agents did.
func (f *bypassAgentsFixture) asBroker(t *testing.T, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		require.NoError(t, err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := "bypass-nonce-" + uuid.New().String()
	req.Header.Set(HeaderBrokerID, f.broker.ID)
	req.Header.Set(HeaderTimestamp, timestamp)
	req.Header.Set(HeaderNonce, nonce)

	svc := f.srv.brokerAuthService
	require.NotNil(t, svc, "broker auth service must be configured for broker tests")
	mac := hmac.New(sha256.New, f.brokerSecret)
	mac.Write(svc.buildCanonicalString(req, timestamp, nonce))
	req.Header.Set(HeaderSignature, base64.StdEncoding.EncodeToString(mac.Sum(nil)))

	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// ============================================================================
// Denials — one per converted site, agent-authenticated
// ============================================================================

// TestBypassAgents_Denials is the core regression suite. Every request below
// succeeded before the conversion.
func TestBypassAgents_Denials(t *testing.T) {
	t.Run("getAgent cross-project is 404, not 403", func(t *testing.T) {
		// getAgent keeps its project-isolation check ahead of s.authorize
		// precisely so this stays a 404: a 403 would confirm that an agent with
		// this ID exists in some other project (design §4.2). This test is the
		// guard against a later cleanup "simplifying" the ordering.
		f := bypassAgentsSetup(t)
		rec := f.asAgent(t, http.MethodGet, "/api/v1/agents/"+f.stranger.ID, nil)
		assert.Equal(t, http.StatusNotFound, rec.Code,
			"cross-project read must 404 and not disclose existence; got: %s", rec.Body.String())
	})

	t.Run("updateAgent on a project peer is denied", func(t *testing.T) {
		// updateAgent had no authorization of any kind: any authenticated
		// caller could rewrite any agent's name, labels, config and GCP
		// identity, hub-wide and cross-project.
		f := bypassAgentsSetup(t)
		rec := f.asAgent(t, http.MethodPatch, "/api/v1/agents/"+f.sibling.ID,
			map[string]interface{}{"name": "hijacked"})
		assert.Equal(t, http.StatusForbidden, rec.Code,
			"agent must not update a project peer; got: %s", rec.Body.String())

		got, err := f.store.GetAgent(context.Background(), f.sibling.ID)
		require.NoError(t, err)
		assert.NotEqual(t, "hijacked", got.Name, "the denied update must not have been applied")
	})

	t.Run("updateAgent cross-project is denied", func(t *testing.T) {
		f := bypassAgentsSetup(t)
		rec := f.asAgent(t, http.MethodPatch, "/api/v1/agents/"+f.stranger.ID,
			map[string]interface{}{"name": "hijacked"})
		assert.Equal(t, http.StatusForbidden, rec.Code,
			"agent must not update an agent in another project; got: %s", rec.Body.String())
	})

	t.Run("performAgentDelete on a project peer is denied", func(t *testing.T) {
		f := bypassAgentsSetup(t)
		rec := f.asAgent(t, http.MethodDelete, "/api/v1/agents/"+f.sibling.ID, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code,
			"agent must not delete a project peer; got: %s", rec.Body.String())

		got, err := f.store.GetAgent(context.Background(), f.sibling.ID)
		require.NoError(t, err)
		assert.True(t, got.DeletedAt.IsZero(), "the denied delete must not have been applied")
	})

	t.Run("updateProject on the agent's own project is denied", func(t *testing.T) {
		// Read-class access to its own project is granted by the Part 2
		// baseline; mutation deliberately is not (design §5.5).
		f := bypassAgentsSetup(t)
		rec := f.asAgent(t, http.MethodPatch, "/api/v1/projects/"+f.proj.ID,
			map[string]interface{}{"name": "renamed-by-agent"})
		assert.Equal(t, http.StatusForbidden, rec.Code,
			"agent must not rename its own project; got: %s", rec.Body.String())

		got, err := f.store.GetProject(context.Background(), f.proj.ID)
		require.NoError(t, err)
		assert.NotEqual(t, "renamed-by-agent", got.Name)
	})

	t.Run("deleteProject on the agent's own project is denied", func(t *testing.T) {
		f := bypassAgentsSetup(t)
		rec := f.asAgent(t, http.MethodDelete, "/api/v1/projects/"+f.proj.ID, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code,
			"agent must not delete the project it lives in; got: %s", rec.Body.String())

		_, err := f.store.GetProject(context.Background(), f.proj.ID)
		assert.NoError(t, err, "project must still exist after the denied delete")
	})

	t.Run("lifecycle action without ScopeAgentLifecycle is denied", func(t *testing.T) {
		// The scope is template-administered (design §5.2): an agent whose
		// template did not grant it holds no lifecycle authority even over a
		// peer in its own project.
		f := bypassAgentsSetup(t)
		rec := f.asAgent(t, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%s/agents/%s/restart", f.proj.ID, f.sibling.ID), nil,
			ScopeAgentStatusUpdate)
		assert.Equal(t, http.StatusForbidden, rec.Code,
			"agent without the lifecycle scope must not restart a peer; got: %s", rec.Body.String())
	})

	t.Run("lifecycle action cross-project is denied even with the scope", func(t *testing.T) {
		f := bypassAgentsSetup(t)
		rec := f.asAgent(t, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%s/agents/%s/restart", f.other.ID, f.stranger.ID), nil,
			ScopeAgentLifecycle)
		assert.Equal(t, http.StatusForbidden, rec.Code,
			"the lifecycle scope is confined to the agent's own project; got: %s", rec.Body.String())
	})

	t.Run("cross-project create via the project route is denied", func(t *testing.T) {
		// The headline hole: POST /api/v1/projects/{id}/agents had no gate at
		// all, so any agent token created an agent in any project — and this is
		// the route the CLI uses for create/start/sync (design §3.3).
		f := bypassAgentsSetup(t)
		rec := f.asAgent(t, http.MethodPost, "/api/v1/projects/"+f.other.ID+"/agents",
			CreateAgentRequest{Name: "cross-project-agent"}, ScopeAgentCreate)
		assert.Equal(t, http.StatusForbidden, rec.Code,
			"agent must not create an agent in another project; got: %s", rec.Body.String())

		_, err := f.store.GetAgentBySlug(context.Background(), f.other.ID, "cross-project-agent")
		assert.Equal(t, store.ErrNotFound, err, "no agent may have been created in the other project")
	})

	t.Run("create via the project route without ScopeAgentCreate is denied", func(t *testing.T) {
		f := bypassAgentsSetup(t)
		rec := f.asAgent(t, http.MethodPost, "/api/v1/projects/"+f.proj.ID+"/agents",
			CreateAgentRequest{Name: "unscoped-agent"}, ScopeAgentStatusUpdate)
		assert.Equal(t, http.StatusForbidden, rec.Code,
			"agent without the create scope must be denied on the project route too; got: %s",
			rec.Body.String())
	})

	t.Run("GCP passthrough is denied to a non-user caller", func(t *testing.T) {
		// The passthrough check is a hand-rolled broker-ownership comparison,
		// left hand-rolled on purpose (ptone/scion#596). An agent cannot own a
		// broker, so there is nothing for the comparison to consult — before
		// the fix it simply skipped the block and got passthrough, exposing the
		// broker's own GCP identity to the agent container.
		f := bypassAgentsSetup(t)
		rec := f.asAgent(t, http.MethodPost, "/api/v1/projects/"+f.proj.ID+"/agents",
			CreateAgentRequest{
				Name:        "passthrough-agent",
				GCPIdentity: &GCPIdentityAssignment{MetadataMode: store.GCPMetadataModePassthrough},
			}, ScopeAgentCreate)
		assert.Equal(t, http.StatusForbidden, rec.Code,
			"non-user caller must not obtain GCP metadata passthrough; got: %s", rec.Body.String())
	})

	t.Run("assigning a GCP service account from another project is rejected", func(t *testing.T) {
		// §5.5: an agent may not assign a service account it has no read access
		// to. The project-confinement check is what enforces that here — it runs
		// ahead of the ActionRead authorization, so an out-of-project service
		// account is refused before the authorization question is even asked.
		f := bypassAgentsSetup(t)
		sa := bypassAgentsCreateSA(t, f, f.other.ID, true)

		rec := f.asAgent(t, http.MethodPost, "/api/v1/projects/"+f.proj.ID+"/agents",
			CreateAgentRequest{
				Name: "sa-agent",
				GCPIdentity: &GCPIdentityAssignment{
					MetadataMode:     store.GCPMetadataModeAssign,
					ServiceAccountID: sa.ID,
				},
			}, ScopeAgentCreate)
		assert.Equal(t, http.StatusBadRequest, rec.Code,
			"agent must not assign a service account scoped to another project; got: %s",
			rec.Body.String())
		assert.Contains(t, rec.Body.String(), msgSANotAvailableInProject)
	})

	t.Run("assigning an unverified GCP service account is rejected", func(t *testing.T) {
		f := bypassAgentsSetup(t)
		sa := bypassAgentsCreateSA(t, f, f.proj.ID, false)

		rec := f.asAgent(t, http.MethodPost, "/api/v1/projects/"+f.proj.ID+"/agents",
			CreateAgentRequest{
				Name: "sa-agent",
				GCPIdentity: &GCPIdentityAssignment{
					MetadataMode:     store.GCPMetadataModeAssign,
					ServiceAccountID: sa.ID,
				},
			}, ScopeAgentCreate)
		assert.Equal(t, http.StatusBadRequest, rec.Code,
			"agent must not assign an unverified service account; got: %s", rec.Body.String())
		assert.Contains(t, rec.Body.String(), "not verified")
	})
}

// bypassAgentsCreateSA registers a project-scoped GCP service account.
func bypassAgentsCreateSA(t *testing.T, f *bypassAgentsFixture, scopeID string, verified bool) *store.GCPServiceAccount {
	t.Helper()
	sa := &store.GCPServiceAccount{
		ID:        uuid.New().String(),
		Scope:     store.ScopeProject,
		ScopeID:   scopeID,
		Email:     fmt.Sprintf("sa-%s@proj.iam.gserviceaccount.com", uuid.New().String()[:8]),
		ProjectID: "gcp-proj",
		CreatedBy: f.owner.ID,
		Verified:  verified,
		CreatedAt: time.Now(),
	}
	require.NoError(t, f.store.CreateGCPServiceAccount(context.Background(), sa))
	return sa
}

// TestBypassAgents_UpdateAgentServiceAccountChecks covers the PATCH twin of the
// create path's service-account checks.
//
// Hardening create alone is not enough: PATCH assigns a service account too, and
// "create the agent with no identity, then PATCH one in" needs nothing more than
// update rights on the agent — which the creator holds by definition. Without
// these two checks the PATCH route is a complete bypass of the create route's.
func TestBypassAgents_UpdateAgentServiceAccountChecks(t *testing.T) {
	// GCP identity may only be patched while the agent is still in the "created"
	// phase, so these run against a freshly created, undispatched agent.
	pendingAgent := func(t *testing.T, f *bypassAgentsFixture) *store.Agent {
		t.Helper()
		a := &store.Agent{
			ID:        uuid.New().String(),
			Slug:      "bypass-pending",
			Name:      "bypass-pending",
			ProjectID: f.proj.ID,
			Phase:     string(state.PhaseCreated),
			CreatedBy: f.owner.ID,
			OwnerID:   f.owner.ID,
		}
		require.NoError(t, f.store.CreateAgent(context.Background(), a))
		return a
	}

	patchSA := func(t *testing.T, f *bypassAgentsFixture, agentID, saID string) *httptest.ResponseRecorder {
		t.Helper()
		return doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, "/api/v1/agents/"+agentID,
			map[string]interface{}{
				"gcp_identity": map[string]interface{}{
					"metadata_mode":      store.GCPMetadataModeAssign,
					"service_account_id": saID,
				},
			})
	}

	t.Run("service account from another project is rejected", func(t *testing.T) {
		f := bypassAgentsSetup(t)
		a := pendingAgent(t, f)
		sa := bypassAgentsCreateSA(t, f, f.other.ID, true)
		rec := patchSA(t, f, a.ID, sa.ID)
		require.Equal(t, http.StatusBadRequest, rec.Code,
			"PATCH must apply the same project confinement as create; got: %s", rec.Body.String())
		assert.Contains(t, rec.Body.String(), msgSANotAvailableInProject)

		got, err := f.store.GetAgent(context.Background(), a.ID)
		require.NoError(t, err)
		if got.AppliedConfig != nil {
			assert.Nil(t, got.AppliedConfig.GCPIdentity,
				"the rejected service account must not have been attached")
		}
	})

	t.Run("unverified service account is rejected", func(t *testing.T) {
		f := bypassAgentsSetup(t)
		a := pendingAgent(t, f)
		sa := bypassAgentsCreateSA(t, f, f.proj.ID, false)
		rec := patchSA(t, f, a.ID, sa.ID)
		require.Equal(t, http.StatusBadRequest, rec.Code,
			"PATCH must apply the same verification requirement as create; got: %s", rec.Body.String())
		assert.Contains(t, rec.Body.String(), "not verified")
	})

	t.Run("verified in-project service account is still accepted", func(t *testing.T) {
		f := bypassAgentsSetup(t)
		a := pendingAgent(t, f)
		sa := bypassAgentsCreateSA(t, f, f.proj.ID, true)
		rec := patchSA(t, f, a.ID, sa.ID)
		require.Equal(t, http.StatusOK, rec.Code,
			"the legitimate assignment must keep working; got: %s", rec.Body.String())

		got, err := f.store.GetAgent(context.Background(), a.ID)
		require.NoError(t, err)
		require.NotNil(t, got.AppliedConfig)
		require.NotNil(t, got.AppliedConfig.GCPIdentity)
		assert.Equal(t, sa.ID, got.AppliedConfig.GCPIdentity.ServiceAccountID)
	})

	t.Run("a user who can update the agent but cannot read the service account is denied", func(t *testing.T) {
		// The live arm of the PATCH authorization call, and the reason it is
		// worth having even though it constrains no agent today.
		//
		// The caller owns the agent, so ActionUpdate on it is granted by owner
		// bypass. The service account is in the same project but was created by
		// someone else, and the caller is neither an admin nor a project
		// owner/admin, so nothing grants read on it. Update rights on an agent
		// must not be convertible into the use of a service account the caller
		// cannot see — which is exactly what create already refuses.
		f := bypassAgentsSetup(t)

		updater := &store.User{
			ID:          tid("bypass-updater"),
			Email:       "updater@example.com",
			DisplayName: "Agent Updater",
			Role:        store.UserRoleMember,
			Status:      "active",
			Created:     time.Now(),
		}
		require.NoError(t, f.store.CreateUser(context.Background(), updater))

		owned := &store.Agent{
			ID:        uuid.New().String(),
			Slug:      "bypass-updater-agent",
			Name:      "bypass-updater-agent",
			ProjectID: f.proj.ID,
			Phase:     string(state.PhaseCreated),
			CreatedBy: updater.ID,
			OwnerID:   updater.ID,
		}
		require.NoError(t, f.store.CreateAgent(context.Background(), owned))

		// Created by the project owner, not by the caller.
		sa := bypassAgentsCreateSA(t, f, f.proj.ID, true)

		body := map[string]interface{}{
			"gcp_identity": map[string]interface{}{
				"metadata_mode":      store.GCPMetadataModeAssign,
				"service_account_id": sa.ID,
			},
		}

		// Precondition: the caller really can update this agent, so the denial
		// below is attributable to the service account and not to the agent.
		rec := doRequestAsUser(t, f.srv, updater, http.MethodPatch, "/api/v1/agents/"+owned.ID,
			map[string]interface{}{"task_summary": "mine to update"})
		require.Equal(t, http.StatusOK, rec.Code,
			"precondition: the caller must have update rights on its own agent; got: %s", rec.Body.String())

		rec = doRequestAsUser(t, f.srv, updater, http.MethodPatch, "/api/v1/agents/"+owned.ID, body)
		assert.Equal(t, http.StatusForbidden, rec.Code,
			"update rights on an agent must not confer use of an unreadable service account; got: %s",
			rec.Body.String())

		got, err := f.store.GetAgent(context.Background(), owned.ID)
		require.NoError(t, err)
		if got.AppliedConfig != nil {
			assert.Nil(t, got.AppliedConfig.GCPIdentity)
		}
	})

	t.Run("an agent cannot PATCH a service account onto a project peer", func(t *testing.T) {
		// The authorization half of §4.5: even a well-formed, in-project,
		// verified assignment must fail for a caller with no update rights.
		f := bypassAgentsSetup(t)
		a := pendingAgent(t, f)
		sa := bypassAgentsCreateSA(t, f, f.proj.ID, true)
		rec := f.asAgent(t, http.MethodPatch, "/api/v1/agents/"+a.ID,
			map[string]interface{}{
				"gcp_identity": map[string]interface{}{
					"metadata_mode":      store.GCPMetadataModeAssign,
					"service_account_id": sa.ID,
				},
			})
		assert.Equal(t, http.StatusForbidden, rec.Code,
			"agent must not attach a GCP identity to a peer; got: %s", rec.Body.String())
	})
}

// ============================================================================
// Denials — broker-authenticated
// ============================================================================

// TestBypassAgents_BrokerCallerDenied covers the identity kind that is easy to
// forget. brokerIdentityImpl implements neither UserIdentity nor AgentIdentity,
// so an HMAC-signed broker request skipped every one of these guards. Unlike an
// agent, a broker has no project and so no read baseline either — it should be
// denied everywhere here, reads included.
func TestBypassAgents_BrokerCallerDenied(t *testing.T) {
	f := bypassAgentsSetup(t)

	cases := []struct {
		name   string
		method string
		path   string
		body   interface{}
	}{
		{"getAgent", http.MethodGet, "/api/v1/agents/" + f.sibling.ID, nil},
		{"updateAgent", http.MethodPatch, "/api/v1/agents/" + f.sibling.ID,
			map[string]interface{}{"name": "broker-hijacked"}},
		{"deleteAgent", http.MethodDelete, "/api/v1/agents/" + f.sibling.ID, nil},
		{"updateProject", http.MethodPatch, "/api/v1/projects/" + f.proj.ID,
			map[string]interface{}{"name": "broker-renamed"}},
		{"deleteProject", http.MethodDelete, "/api/v1/projects/" + f.proj.ID, nil},
		{"createProjectAgent", http.MethodPost, "/api/v1/projects/" + f.proj.ID + "/agents",
			CreateAgentRequest{Name: "broker-made-agent"}},
		{"createAgent", http.MethodPost, "/api/v1/agents",
			CreateAgentRequest{Name: "broker-made-agent-2", ProjectID: f.proj.ID}},
		{"agentLifecycle", http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%s/agents/%s/restart", f.proj.ID, f.sibling.ID), nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.asBroker(t, tc.method, tc.path, tc.body)
			assert.Contains(t, []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound},
				rec.Code,
				"broker-authenticated caller must not be authorized here; got %d: %s",
				rec.Code, rec.Body.String())
		})
	}

	// Nothing above may have taken effect.
	ctx := context.Background()
	sib, err := f.store.GetAgent(ctx, f.sibling.ID)
	require.NoError(t, err)
	assert.NotEqual(t, "broker-hijacked", sib.Name)
	assert.True(t, sib.DeletedAt.IsZero())
	proj, err := f.store.GetProject(ctx, f.proj.ID)
	require.NoError(t, err)
	assert.NotEqual(t, "broker-renamed", proj.Name)
	_, err = f.store.GetAgentBySlug(ctx, f.proj.ID, "broker-made-agent")
	assert.Equal(t, store.ErrNotFound, err)
}

// ============================================================================
// Positive tests — the flows the fix must NOT break
// ============================================================================

// TestBypassAgents_LegitimateFlowsStillWork matters as much as the denials. A
// conversion that denies everything would pass every test above; these are what
// distinguish a fix from an outage. If one of these fails, the change is wrong.
func TestBypassAgents_LegitimateFlowsStillWork(t *testing.T) {
	t.Run("agent reads itself", func(t *testing.T) {
		// Self-read is NOT covered by the ancestry bypass — an agent does not
		// appear in its own ancestry — so this passes only via the Part 2
		// read-class project baseline (design §6).
		f := bypassAgentsSetup(t)
		rec := f.asAgent(t, http.MethodGet, "/api/v1/agents/"+f.caller.ID, nil)
		assert.Equal(t, http.StatusOK, rec.Code,
			"an agent must be able to read itself; got: %s", rec.Body.String())
	})

	t.Run("agent reads a project peer", func(t *testing.T) {
		f := bypassAgentsSetup(t)
		rec := f.asAgent(t, http.MethodGet, "/api/v1/agents/"+f.sibling.ID, nil)
		assert.Equal(t, http.StatusOK, rec.Code,
			"an agent must be able to read a peer in its own project; got: %s", rec.Body.String())
	})

	t.Run("agent deletes its own descendant", func(t *testing.T) {
		// Upstream #1097 gates agent DELETE on ScopeAgentLifecycle + project
		// isolation. A legitimate agent-to-agent delete carries the lifecycle
		// scope; the test verifies the ancestry bypass still lets the call
		// through once that prerequisite is met.
		f := bypassAgentsSetup(t)
		rec := f.asAgent(t, http.MethodDelete, "/api/v1/agents/"+f.child.ID, nil, ScopeAgentLifecycle)
		assert.NotEqual(t, http.StatusForbidden, rec.Code,
			"an agent must still be able to delete its own descendant; got %d: %s",
			rec.Code, rec.Body.String())
	})

	t.Run("agent updates its own descendant", func(t *testing.T) {
		f := bypassAgentsSetup(t)
		rec := f.asAgent(t, http.MethodPatch, "/api/v1/agents/"+f.child.ID,
			map[string]interface{}{"taskSummary": "still fine"})
		assert.NotEqual(t, http.StatusForbidden, rec.Code,
			"an agent must still be able to update its own descendant; got %d: %s",
			rec.Code, rec.Body.String())
	})

	t.Run("scoped create in the agent's own project via the project route", func(t *testing.T) {
		// This is the route the CLI uses. It gained a gate in this change, so
		// it is the likeliest place to have over-tightened.
		f := bypassAgentsSetup(t)
		rec := f.asAgent(t, http.MethodPost, "/api/v1/projects/"+f.proj.ID+"/agents",
			CreateAgentRequest{Name: "legit-sub-agent"}, ScopeAgentCreate)
		require.Equal(t, http.StatusCreated, rec.Code,
			"an agent holding ScopeAgentCreate must still create sub-agents in its own project; got: %s",
			rec.Body.String())

		var resp CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		require.NotNil(t, resp.Agent)
		assert.Equal(t, f.caller.ID, resp.Agent.CreatedBy,
			"attribution must survive the conversion: CreatedBy is still the calling agent")
		assert.Contains(t, resp.Agent.Ancestry, f.caller.ID,
			"ancestry must still be threaded from the calling agent")
	})

	t.Run("scoped create in the agent's own project via the unscoped route", func(t *testing.T) {
		f := bypassAgentsSetup(t)
		rec := f.asAgent(t, http.MethodPost, "/api/v1/agents",
			CreateAgentRequest{Name: "legit-sub-agent-2", ProjectID: f.proj.ID}, ScopeAgentCreate)
		assert.Equal(t, http.StatusCreated, rec.Code,
			"the pre-existing gated create route must behave exactly as before; got: %s",
			rec.Body.String())
	})

	t.Run("scoped lifecycle on a project peer", func(t *testing.T) {
		// Peers, not just descendants — resolved as Q3 in the design. The
		// request may still fail downstream on broker availability, which is
		// not an authorization outcome, so this asserts only that it is not
		// denied.
		f := bypassAgentsSetup(t)
		rec := f.asAgent(t, http.MethodPost,
			fmt.Sprintf("/api/v1/projects/%s/agents/%s/restart", f.proj.ID, f.sibling.ID), nil,
			ScopeAgentLifecycle)
		assert.NotEqual(t, http.StatusForbidden, rec.Code,
			"an agent holding ScopeAgentLifecycle must still act on a peer in its own project; got %d: %s",
			rec.Code, rec.Body.String())
		assert.NotEqual(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("project owner retains full access", func(t *testing.T) {
		// The conversion must not change the user path at all.
		f := bypassAgentsSetup(t)
		rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPatch,
			"/api/v1/projects/"+f.proj.ID, map[string]interface{}{"name": "Renamed By Owner"})
		assert.Equal(t, http.StatusOK, rec.Code,
			"the project owner must still be able to update the project; got: %s", rec.Body.String())

		rec = doRequestAsUser(t, f.srv, f.owner, http.MethodPatch,
			"/api/v1/agents/"+f.sibling.ID, map[string]interface{}{"taskSummary": "owner edit"})
		assert.Equal(t, http.StatusOK, rec.Code,
			"the agent's owner must still be able to update it; got: %s", rec.Body.String())
	})
}

// ============================================================================
// Cross-route parity — the regression guard for the drift class itself
// ============================================================================

// TestBypassAgents_CreateRouteParity asserts that the same request body gets
// the same verdict on both create routes.
//
// It is deliberately written as a table over routes rather than as two suites:
// the failure being guarded against is not any one of today's four
// divergences, it is that the two routes drift at all. A fifth divergence
// introduced later fails this test without anyone having to remember to extend
// it — provided new cases are added to the table rather than to one route.
func TestBypassAgents_CreateRouteParity(t *testing.T) {
	cases := []struct {
		name     string
		gcp      *GCPIdentityAssignment
		wantCode int
		why      string
	}{
		{
			name:     "block with a service account id",
			gcp:      &GCPIdentityAssignment{MetadataMode: store.GCPMetadataModeBlock, ServiceAccountID: "some-sa"},
			wantCode: http.StatusBadRequest,
			why:      "a service account alongside block is a contradictory request",
		},
		{
			name:     "passthrough with a service account id",
			gcp:      &GCPIdentityAssignment{MetadataMode: store.GCPMetadataModePassthrough, ServiceAccountID: "some-sa"},
			wantCode: http.StatusBadRequest,
			why:      "a service account alongside passthrough is a contradictory request",
		},
		{
			name:     "unrecognised metadata mode",
			gcp:      &GCPIdentityAssignment{MetadataMode: "totally-bogus"},
			wantCode: http.StatusBadRequest,
			why: "an unknown mode must be rejected, never normalised to block: " +
				"it propagates verbatim to the broker and sidecar, which fail OPEN on it (design §8.4)",
		},
		{
			name:     "assign with no service account id",
			gcp:      &GCPIdentityAssignment{MetadataMode: store.GCPMetadataModeAssign},
			wantCode: http.StatusBadRequest,
			why:      "assign without a service account cannot be satisfied",
		},
		{
			name:     "empty gcp_identity object",
			gcp:      &GCPIdentityAssignment{},
			wantCode: http.StatusBadRequest,
			why: "the project-default-drop case: an empty mode used to match no case in the " +
				"config switch and skip the else branch that applies the project default, " +
				"silently producing an agent with no identity",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, route := range []string{"unscoped", "project-scoped"} {
				t.Run(route, func(t *testing.T) {
					f := bypassAgentsSetup(t)
					var rec *httptest.ResponseRecorder
					switch route {
					case "unscoped":
						rec = doRequest(t, f.srv, http.MethodPost, "/api/v1/agents",
							CreateAgentRequest{
								Name:        "parity-agent",
								ProjectID:   f.proj.ID,
								GCPIdentity: tc.gcp,
							})
					default:
						rec = doRequest(t, f.srv, http.MethodPost,
							"/api/v1/projects/"+f.proj.ID+"/agents",
							CreateAgentRequest{
								Name:        "parity-agent",
								GCPIdentity: tc.gcp,
							})
					}
					assert.Equal(t, tc.wantCode, rec.Code,
						"%s route: %s; got: %s", route, tc.why, rec.Body.String())
				})
			}
		})
	}
}

// TestBypassAgents_ProjectDefaultGCPIdentityOnBothRoutes covers the other half
// of the §3.3 project-default finding: with the project configured for
// "assign", both create routes must actually apply it.
func TestBypassAgents_ProjectDefaultGCPIdentityOnBothRoutes(t *testing.T) {
	setup := func(t *testing.T) (*bypassAgentsFixture, *store.GCPServiceAccount) {
		t.Helper()
		f := bypassAgentsSetup(t)
		ctx := context.Background()
		sa := &store.GCPServiceAccount{
			ID:        tid("bypass-default-sa"),
			Scope:     store.ScopeProject,
			ScopeID:   f.proj.ID,
			Email:     "default-sa@proj.iam.gserviceaccount.com",
			ProjectID: "gcp-proj",
			CreatedBy: f.owner.ID,
			Verified:  true,
			CreatedAt: time.Now(),
		}
		require.NoError(t, f.store.CreateGCPServiceAccount(ctx, sa))

		proj, err := f.store.GetProject(ctx, f.proj.ID)
		require.NoError(t, err)
		if proj.Annotations == nil {
			proj.Annotations = map[string]string{}
		}
		proj.Annotations[projectSettingDefaultGCPIdentityMode] = store.GCPMetadataModeAssign
		proj.Annotations[projectSettingDefaultGCPIdentitySAID] = sa.ID
		require.NoError(t, f.store.UpdateProject(ctx, proj))
		return f, sa
	}

	assertApplied := func(t *testing.T, f *bypassAgentsFixture, sa *store.GCPServiceAccount, rec *httptest.ResponseRecorder) {
		t.Helper()
		require.Equal(t, http.StatusCreated, rec.Code, "create failed: %s", rec.Body.String())
		var resp CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		require.NotNil(t, resp.Agent)
		require.NotNil(t, resp.Agent.AppliedConfig)
		require.NotNil(t, resp.Agent.AppliedConfig.GCPIdentity,
			"the project's configured default GCP identity must be applied, not silently dropped")
		assert.Equal(t, store.GCPMetadataModeAssign, resp.Agent.AppliedConfig.GCPIdentity.MetadataMode)
		assert.Equal(t, sa.ID, resp.Agent.AppliedConfig.GCPIdentity.ServiceAccountID)
	}

	t.Run("unscoped route applies the project default", func(t *testing.T) {
		f, sa := setup(t)
		rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents",
			CreateAgentRequest{Name: "default-agent-a", ProjectID: f.proj.ID})
		assertApplied(t, f, sa, rec)
	})

	t.Run("project route applies the project default", func(t *testing.T) {
		f, sa := setup(t)
		rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/projects/"+f.proj.ID+"/agents",
			CreateAgentRequest{Name: "default-agent-b"})
		assertApplied(t, f, sa, rec)
	})

	// The route by which the default used to be dropped. Sending an explicit
	// empty gcp_identity is now a rejected request on both routes rather than a
	// silent nil identity — the validation rejects rather than normalises, so
	// the caller is told, instead of receiving an agent that quietly lacks the
	// identity the project configured.
	t.Run("empty gcp_identity is rejected rather than silently dropping the default", func(t *testing.T) {
		f, _ := setup(t)
		for name, path := range map[string]string{
			"unscoped":       "/api/v1/agents",
			"project-scoped": "/api/v1/projects/" + f.proj.ID + "/agents",
		} {
			t.Run(name, func(t *testing.T) {
				body := CreateAgentRequest{Name: "empty-gcp-agent", GCPIdentity: &GCPIdentityAssignment{}}
				if name == "unscoped" {
					body.ProjectID = f.proj.ID
				}
				rec := doRequest(t, f.srv, http.MethodPost, path, body)
				require.Equal(t, http.StatusBadRequest, rec.Code,
					"expected rejection, got: %s", rec.Body.String())

				_, err := f.store.GetAgentBySlug(context.Background(), f.proj.ID, "empty-gcp-agent")
				assert.Equal(t, store.ErrNotFound, err,
					"no agent may be created with a silently-dropped identity")
			})
		}
	})
}

// ============================================================================
// Broker dispatch — canDispatchToBroker
// ============================================================================

// bypassAgentsBrokerCtx returns a context carrying a genuine broker identity,
// obtained by running a correctly signed broker request through the real
// authentication middleware.
//
// It is done this way rather than by hand-constructing an identity on purpose: a
// hand-built one would prove only that the switch in canDispatchToBroker has a
// default arm, not that real broker traffic actually lands in it.
func bypassAgentsBrokerCtx(t *testing.T, f *bypassAgentsFixture) context.Context {
	t.Helper()
	svc := f.srv.brokerAuthService
	require.NotNil(t, svc)

	var captured context.Context
	var inner http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Context()
		w.WriteHeader(http.StatusOK)
	})
	handler := UnifiedAuthMiddleware(AuthConfig{
		Mode:          "production",
		BrokerAuthSvc: svc,
	})(BrokerAuthMiddleware(svc)(inner))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/runtime-brokers/bypass-broker/heartbeat", nil)
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := "bypass-dispatch-" + uuid.New().String()
	req.Header.Set(HeaderBrokerID, f.broker.ID)
	req.Header.Set(HeaderTimestamp, timestamp)
	req.Header.Set(HeaderNonce, nonce)
	mac := hmac.New(sha256.New, f.brokerSecret)
	mac.Write(svc.buildCanonicalString(req, timestamp, nonce))
	req.Header.Set(HeaderSignature, base64.StdEncoding.EncodeToString(mac.Sum(nil)))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "signed broker request should authenticate")
	require.NotNil(t, captured)

	ident := GetIdentityFromContext(captured)
	require.NotNil(t, ident, "middleware must place a broker identity in the context")
	require.Equal(t, "broker", ident.Type())
	return captured
}

// TestBypassAgents_CanDispatchToBroker pins the broker-selection filter.
//
// This one is not an endpoint guard, so it does not answer in status codes — it
// is the predicate that decides which brokers a caller's agent may be placed on.
// Its old `no user identity → allow` branch admitted every non-user caller, which
// is the widest form of the #591 bug: it did not merely skip a check, it treated
// the absence of a recognisable caller as proof of permission.
//
// Note the nil case is *inverted*, not deleted. GetIdentityFromContext returns a
// literal nil interface for unauthenticated requests, and handing that to
// CheckAccess panics on identity.Type() — a 500, not a deny.
func TestBypassAgents_CanDispatchToBroker(t *testing.T) {
	// A broker that is NOT auto-provide: auto-provide short-circuits to allow
	// before the caller is ever examined, so it would mask every case here.
	newRestrictedBroker := func(t *testing.T, f *bypassAgentsFixture, linkTo string) *store.RuntimeBroker {
		t.Helper()
		b := &store.RuntimeBroker{
			ID:          uuid.New().String(),
			Name:        "restricted-" + uuid.New().String()[:8],
			Slug:        "restricted-" + uuid.New().String()[:8],
			Status:      store.BrokerStatusOnline,
			AutoProvide: false,
			CreatedBy:   f.owner.ID,
			Created:     time.Now(),
			Updated:     time.Now(),
		}
		require.NoError(t, f.store.CreateRuntimeBroker(context.Background(), b))
		if linkTo != "" {
			require.NoError(t, f.store.AddProjectProvider(context.Background(), &store.ProjectProvider{
				ProjectID:  linkTo,
				BrokerID:   b.ID,
				BrokerName: b.Name,
				Status:     store.BrokerStatusOnline,
			}))
		}
		return b
	}

	agentCtx := func(f *bypassAgentsFixture, projectID string, scopes ...AgentTokenScope) context.Context {
		return contextWithIdentity(context.Background(), &agentIdentityWrapper{&AgentTokenClaims{
			Claims:    jwt.Claims{Subject: f.caller.ID},
			ProjectID: projectID,
			Scopes:    scopes,
		}})
	}

	t.Run("unauthenticated caller is denied", func(t *testing.T) {
		f := bypassAgentsSetup(t)
		b := newRestrictedBroker(t, f, f.proj.ID)
		assert.False(t, f.srv.canDispatchToBroker(context.Background(), b),
			"an empty context must deny, not allow — and must not panic")
	})

	t.Run("unauthenticated caller is denied even on an auto-provide broker", func(t *testing.T) {
		// AutoProvide is a property of the broker, not a licence for anonymous
		// callers: the identity nil-check sits ahead of it.
		f := bypassAgentsSetup(t)
		assert.False(t, f.srv.canDispatchToBroker(context.Background(), f.broker))
	})

	t.Run("broker-typed caller is denied", func(t *testing.T) {
		// Deliberate behaviour change. CheckAccess already answered broker
		// identities with "unknown identity type"; the fail-open branch was the
		// only thing letting them past, and it let everything past.
		f := bypassAgentsSetup(t)
		b := newRestrictedBroker(t, f, f.proj.ID)
		ctx := bypassAgentsBrokerCtx(t, f)
		assert.False(t, f.srv.canDispatchToBroker(ctx, b),
			"a broker-typed caller must not select a broker for dispatch")
	})

	t.Run("agent with the create scope may dispatch within its own project", func(t *testing.T) {
		f := bypassAgentsSetup(t)
		b := newRestrictedBroker(t, f, f.proj.ID)
		assert.True(t, f.srv.canDispatchToBroker(agentCtx(f, f.proj.ID, ScopeAgentCreate), b),
			"this is the flow the gate exists to preserve: broker selection completing "+
				"a create that authorizeAgentCreate already allowed")
	})

	t.Run("agent without the create scope is denied", func(t *testing.T) {
		f := bypassAgentsSetup(t)
		b := newRestrictedBroker(t, f, f.proj.ID)
		assert.False(t, f.srv.canDispatchToBroker(agentCtx(f, f.proj.ID, ScopeAgentStatusUpdate), b))
	})

	t.Run("agent is denied a broker that does not serve its project", func(t *testing.T) {
		f := bypassAgentsSetup(t)
		b := newRestrictedBroker(t, f, f.other.ID)
		assert.False(t, f.srv.canDispatchToBroker(agentCtx(f, f.proj.ID, ScopeAgentCreate), b),
			"holding the create scope must not reach brokers outside the agent's project")
	})

	t.Run("auto-provide brokers stay open to authenticated callers", func(t *testing.T) {
		// Unchanged behaviour, pinned because combo hub-broker deployments
		// depend on it and the identity switch sits behind it.
		f := bypassAgentsSetup(t)
		assert.True(t, f.srv.canDispatchToBroker(agentCtx(f, f.proj.ID), f.broker))
	})

	t.Run("user path is unchanged", func(t *testing.T) {
		// The owner of the broker may dispatch; an unrelated member may not.
		// Whatever CheckAccess decided before, it still decides.
		f := bypassAgentsSetup(t)
		b := newRestrictedBroker(t, f, f.proj.ID)

		ownerCtx := contextWithIdentity(context.Background(),
			NewAuthenticatedUser(f.owner.ID, f.owner.Email, f.owner.DisplayName, string(f.owner.Role), "cli"))
		assert.True(t, f.srv.canDispatchToBroker(ownerCtx, b),
			"the broker's creator must still be able to dispatch to it")

		stranger := &store.User{
			ID:          tid("bypass-stranger-user"),
			Email:       "stranger@example.com",
			DisplayName: "Stranger",
			Role:        store.UserRoleMember,
			Status:      "active",
			Created:     time.Now(),
		}
		require.NoError(t, f.store.CreateUser(context.Background(), stranger))
		strangerCtx := contextWithIdentity(context.Background(),
			NewAuthenticatedUser(stranger.ID, stranger.Email, stranger.DisplayName, string(stranger.Role), "cli"))
		assert.False(t, f.srv.canDispatchToBroker(strangerCtx, b),
			"an unrelated user must not dispatch to someone else's non-auto-provide broker")
	})

	t.Run("dev identity takes the user path", func(t *testing.T) {
		// "dev" is a user kind, not an unknown type: it must be evaluated by
		// policy and must not fall into the default deny arm. The identity is
		// stubbed because no concrete dev-typed identity exists in the package —
		// the type string is what the switch keys on.
		f := bypassAgentsSetup(t)
		b := newRestrictedBroker(t, f, f.proj.ID)
		devCtx := contextWithIdentity(context.Background(), &bypassAgentsDevIdentity{
			UserIdentity: NewAuthenticatedUser(f.owner.ID, f.owner.Email, f.owner.DisplayName,
				string(f.owner.Role), "cli"),
		})
		assert.True(t, f.srv.canDispatchToBroker(devCtx, b),
			"a dev caller must be evaluated by policy, not rejected as an unknown type")
	})
}

// bypassAgentsDevIdentity is a UserIdentity reporting Type() == "dev", the arm
// the ruling calls out as easy to drop from the switch.
type bypassAgentsDevIdentity struct {
	UserIdentity
}

func (d *bypassAgentsDevIdentity) Type() string { return "dev" }

// TestBypassAgents_UnauthenticatedDenied is the floor. authorize() answers 401
// rather than 403 for a caller with no identity at all, and no converted site
// may be reachable without authentication.
func TestBypassAgents_UnauthenticatedDenied(t *testing.T) {
	f := bypassAgentsSetup(t)

	for _, tc := range []struct {
		name   string
		method string
		path   string
	}{
		{"getAgent", http.MethodGet, "/api/v1/agents/" + f.sibling.ID},
		{"updateAgent", http.MethodPatch, "/api/v1/agents/" + f.sibling.ID},
		{"deleteAgent", http.MethodDelete, "/api/v1/agents/" + f.sibling.ID},
		{"updateProject", http.MethodPatch, "/api/v1/projects/" + f.proj.ID},
		{"deleteProject", http.MethodDelete, "/api/v1/projects/" + f.proj.ID},
		{"createProjectAgent", http.MethodPost, "/api/v1/projects/" + f.proj.ID + "/agents"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequestNoAuth(t, f.srv, tc.method, tc.path, nil)
			assert.Contains(t,
				[]int{http.StatusUnauthorized, http.StatusForbidden},
				rec.Code, "unauthenticated caller must be rejected; got %d: %s",
				rec.Code, rec.Body.String())
		})
	}
}

// ============================================================================
// Broker dispatch — checkBrokerDispatchAccess
// ============================================================================

// TestBypassAgents_CheckBrokerDispatchAccess pins the response-writing twin of
// canDispatchToBroker, on the agent-creation path.
//
// This was the worst of the #591 sites, because it did not merely omit a deny —
// it wrote one. `if userIdent == nil { return true }`, commented "no user
// identity (e.g. broker-to-broker) — allow". GetUserIdentityFromContext returns
// nil for an agent caller too, so every agent that reached agent creation could
// place its new agent on ANY broker in the hub, including brokers serving other
// projects, regardless of scope.
//
// The two functions are one rule, and this is the copy that drifted, so the fix
// is delegation rather than a second transcription of the switch. These cases
// therefore assert the same verdicts as TestBypassAgents_CanDispatchToBroker
// plus the HTTP response, and they fail if the delegation is ever unwound into
// a copy that can drift again.
func TestBypassAgents_CheckBrokerDispatchAccess(t *testing.T) {
	newRestrictedBroker := func(t *testing.T, f *bypassAgentsFixture, linkTo string) *store.RuntimeBroker {
		t.Helper()
		b := &store.RuntimeBroker{
			ID:          uuid.New().String(),
			Name:        "restricted-" + uuid.New().String()[:8],
			Slug:        "restricted-" + uuid.New().String()[:8],
			Status:      store.BrokerStatusOnline,
			AutoProvide: false,
			CreatedBy:   f.owner.ID,
			Created:     time.Now(),
			Updated:     time.Now(),
		}
		require.NoError(t, f.store.CreateRuntimeBroker(context.Background(), b))
		if linkTo != "" {
			require.NoError(t, f.store.AddProjectProvider(context.Background(), &store.ProjectProvider{
				ProjectID:  linkTo,
				BrokerID:   b.ID,
				BrokerName: b.Name,
				Status:     store.BrokerStatusOnline,
			}))
		}
		return b
	}

	agentCtx := func(f *bypassAgentsFixture, projectID string, scopes ...AgentTokenScope) context.Context {
		return contextWithIdentity(context.Background(), &agentIdentityWrapper{&AgentTokenClaims{
			Claims:    jwt.Claims{Subject: f.caller.ID},
			ProjectID: projectID,
			Scopes:    scopes,
		}})
	}

	// check runs the gate and returns its verdict together with the response it
	// wrote, because "denied" here means both false and a 403 on the wire.
	check := func(f *bypassAgentsFixture, ctx context.Context, brokerID string) (bool, *httptest.ResponseRecorder) {
		rec := httptest.NewRecorder()
		return f.srv.checkBrokerDispatchAccess(ctx, rec, brokerID), rec
	}

	t.Run("agent may not dispatch to a broker serving another project", func(t *testing.T) {
		// The bypass, stated directly. Before the fix this returned true
		// without so much as loading the broker.
		f := bypassAgentsSetup(t)
		b := newRestrictedBroker(t, f, f.other.ID)

		ok, rec := check(f, agentCtx(f, f.proj.ID, ScopeAgentCreate), b.ID)
		assert.False(t, ok, "an agent must not place an agent on a broker outside its project")
		assert.Equal(t, http.StatusForbidden, rec.Code)
	})

	t.Run("agent without the create scope is denied", func(t *testing.T) {
		f := bypassAgentsSetup(t)
		b := newRestrictedBroker(t, f, f.proj.ID)

		ok, rec := check(f, agentCtx(f, f.proj.ID, ScopeAgentStatusUpdate), b.ID)
		assert.False(t, ok)
		assert.Equal(t, http.StatusForbidden, rec.Code)
	})

	t.Run("agent with the create scope may dispatch within its own project", func(t *testing.T) {
		// The flow the gate must preserve: broker selection completing a create
		// that authorizeAgentCreate already allowed.
		f := bypassAgentsSetup(t)
		b := newRestrictedBroker(t, f, f.proj.ID)

		ok, _ := check(f, agentCtx(f, f.proj.ID, ScopeAgentCreate), b.ID)
		assert.True(t, ok)
	})

	t.Run("broker-typed caller is denied", func(t *testing.T) {
		// The case the old comment claimed to be serving. No broker-to-broker
		// dispatch reaches here in the first place: both callers of this gate
		// run authorizeAgentCreate first, which denies broker identities.
		f := bypassAgentsSetup(t)
		b := newRestrictedBroker(t, f, f.proj.ID)

		ok, rec := check(f, bypassAgentsBrokerCtx(t, f), b.ID)
		assert.False(t, ok)
		assert.Equal(t, http.StatusForbidden, rec.Code)
	})

	t.Run("unauthenticated caller is denied", func(t *testing.T) {
		f := bypassAgentsSetup(t)
		b := newRestrictedBroker(t, f, f.proj.ID)

		ok, rec := check(f, context.Background(), b.ID)
		assert.False(t, ok, "an empty context must deny, not allow — and must not panic")
		assert.Equal(t, http.StatusForbidden, rec.Code)
	})

	t.Run("auto-provide brokers stay open to authenticated callers", func(t *testing.T) {
		// Combo hub-broker deployments depend on this, and it is the reason the
		// AutoProvide check sits ahead of the identity switch rather than inside
		// the user arm.
		f := bypassAgentsSetup(t)

		ok, _ := check(f, agentCtx(f, f.proj.ID), f.broker.ID)
		assert.True(t, ok, "an auto-provide broker must stay dispatchable")
	})

	t.Run("user path is unchanged", func(t *testing.T) {
		f := bypassAgentsSetup(t)
		b := newRestrictedBroker(t, f, f.proj.ID)

		ownerCtx := contextWithIdentity(context.Background(),
			NewAuthenticatedUser(f.owner.ID, f.owner.Email, f.owner.DisplayName, string(f.owner.Role), "cli"))
		ok, _ := check(f, ownerCtx, b.ID)
		assert.True(t, ok, "the broker's creator must still be able to dispatch to it")

		stranger := &store.User{
			ID:          tid("bypass-dispatch-stranger"),
			Email:       "dispatch-stranger@example.com",
			DisplayName: "Stranger",
			Role:        store.UserRoleMember,
			Status:      "active",
			Created:     time.Now(),
		}
		require.NoError(t, f.store.CreateUser(context.Background(), stranger))
		strangerCtx := contextWithIdentity(context.Background(),
			NewAuthenticatedUser(stranger.ID, stranger.Email, stranger.DisplayName, string(stranger.Role), "cli"))
		ok, rec := check(f, strangerCtx, b.ID)
		assert.False(t, ok, "an unrelated user must not dispatch to someone else's broker")
		assert.Equal(t, http.StatusForbidden, rec.Code)
	})

	t.Run("unknown broker is an error, not an allow", func(t *testing.T) {
		f := bypassAgentsSetup(t)

		ok, rec := check(f, agentCtx(f, f.proj.ID, ScopeAgentCreate), "no-such-broker")
		assert.False(t, ok)
		assert.NotEqual(t, http.StatusOK, rec.Code)
	})
}

// ============================================================================
// Cross-project agent denials — project settings, shared-dirs, hooks, logs
// ============================================================================

// TestBypassAgents_CrossProjectReadDenials extends the denial suite to the
// remaining project-scoped read endpoints that were converted in this PR.
// Each of these returned data before the fix; now they deny cross-project
// agents with a 404 (not 403) to avoid confirming project existence.
func TestBypassAgents_CrossProjectReadDenials(t *testing.T) {
	f := bypassAgentsSetup(t)

	cases := []struct {
		name     string
		method   string
		path     string
		body     interface{}
		wantCode int
	}{
		{"projectSettings", http.MethodGet, "/api/v1/projects/" + f.other.ID + "/settings", nil, http.StatusNotFound},
		{"sharedDirs", http.MethodGet, "/api/v1/projects/" + f.other.ID + "/shared-dirs", nil, http.StatusNotFound},
		{"preStartHooks", http.MethodGet, "/api/v1/projects/" + f.other.ID + "/pre-start-hooks", nil, http.StatusNotFound},
		{"settingsResolved", http.MethodGet, "/api/v1/projects/" + f.other.ID + "/settings/resolved", nil, http.StatusNotFound},
		{"agentLogs", http.MethodGet, "/api/v1/agents/" + f.stranger.ID + "/logs", nil, http.StatusNotFound},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.asAgent(t, tc.method, tc.path, tc.body)
			assert.Equal(t, tc.wantCode, rec.Code,
				"cross-project read must return %d, not disclose existence; got: %s",
				tc.wantCode, rec.Body.String())
		})
	}
}

// TestBypassAgents_CrossProjectBrokerDenials extends the broker denial table
// to the same endpoints.
func TestBypassAgents_CrossProjectBrokerDenials(t *testing.T) {
	f := bypassAgentsSetup(t)

	cases := []struct {
		name   string
		method string
		path   string
	}{
		{"projectSettings", http.MethodGet, "/api/v1/projects/" + f.proj.ID + "/settings"},
		{"sharedDirs", http.MethodGet, "/api/v1/projects/" + f.proj.ID + "/shared-dirs"},
		{"preStartHooks", http.MethodGet, "/api/v1/projects/" + f.proj.ID + "/pre-start-hooks"},
		{"settingsResolved", http.MethodGet, "/api/v1/projects/" + f.proj.ID + "/settings/resolved"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.asBroker(t, tc.method, tc.path, nil)
			assert.Contains(t, []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound},
				rec.Code,
				"broker-authenticated caller must not read %s; got %d: %s",
				tc.name, rec.Code, rec.Body.String())
		})
	}
}

// ============================================================================
// Status code assertions — 404-before-403 ordering
// ============================================================================

// TestBypassAgents_404Before403Ordering pins the isolation check ordering:
// cross-project agent requests on log endpoints must receive exactly 404
// (not 403), preventing project/agent existence disclosure.
func TestBypassAgents_404Before403Ordering(t *testing.T) {
	f := bypassAgentsSetup(t)

	cases := []struct {
		name string
		path string
	}{
		{"agentLogs", "/api/v1/agents/" + f.stranger.ID + "/logs"},
		{"agentMessageLogs", "/api/v1/agents/" + f.stranger.ID + "/message-logs"},
		{"projectMessageLogs", "/api/v1/projects/" + f.other.ID + "/message-logs"},
		{"projectSettings", "/api/v1/projects/" + f.other.ID + "/settings"},
		{"settingsResolved", "/api/v1/projects/" + f.other.ID + "/settings/resolved"},
		{"sharedDirs", "/api/v1/projects/" + f.other.ID + "/shared-dirs"},
		{"preStartHooks", "/api/v1/projects/" + f.other.ID + "/pre-start-hooks"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.asAgent(t, http.MethodGet, tc.path, nil)
			assert.Equal(t, http.StatusNotFound, rec.Code,
				"cross-project agent must get exactly 404, not 403 or another code; got: %s",
				rec.Body.String())
		})
	}
}

// ============================================================================
// In-project agent success — read baseline reaches the handlers
// ============================================================================

// TestBypassAgents_InProjectReadSuccess asserts the in-project agent still
// reaches the handlers for settings, settings/resolved, shared-dirs,
// pre-start-hooks and message-logs. These prove the read baseline reaches the
// Resource literals these handlers build — a real outage risk if a literal
// ever loses its project linkage.
func TestBypassAgents_InProjectReadSuccess(t *testing.T) {
	f := bypassAgentsSetup(t)

	cases := []struct {
		name string
		path string
	}{
		{"settings", "/api/v1/projects/" + f.proj.ID + "/settings"},
		{"settingsResolved", "/api/v1/projects/" + f.proj.ID + "/settings/resolved"},
		{"sharedDirs", "/api/v1/projects/" + f.proj.ID + "/shared-dirs"},
		{"preStartHooks", "/api/v1/projects/" + f.proj.ID + "/pre-start-hooks"},
		{"projectMessageLogs", "/api/v1/projects/" + f.proj.ID + "/message-logs"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.asAgent(t, http.MethodGet, tc.path, nil)
			// message-logs returns 501 when logQueryService is nil, which is
			// past authorization. All others should return 200.
			assert.NotEqual(t, http.StatusForbidden, rec.Code,
				"in-project agent must not be denied on %s; got %d: %s",
				tc.name, rec.Code, rec.Body.String())
			assert.NotEqual(t, http.StatusUnauthorized, rec.Code,
				"in-project agent must not get 401 on %s; got: %s",
				tc.name, rec.Body.String())
			assert.NotEqual(t, http.StatusNotFound, rec.Code,
				"in-project agent must not get 404 on %s; got: %s",
				tc.name, rec.Body.String())
		})
	}
}

// ============================================================================
// addGroupMember — non-user role cap
// ============================================================================

// TestBypassAgents_AddGroupMember_RoleCap pins the cap introduced in this PR:
// agents with an addMember policy may add plain members but not admin/owner.
// This is a new authorization rule invented by this change and has zero
// coverage without this test.
func TestBypassAgents_AddGroupMember_RoleCap(t *testing.T) {
	setup := func(t *testing.T) (*bypassAgentsFixture, *store.Group) {
		t.Helper()
		f := bypassAgentsSetup(t)
		ctx := context.Background()

		// Create a project-scoped group that the agent can interact with.
		group := &store.Group{
			ID:        uuid.New().String(),
			Name:      "bypass-test-group",
			Slug:      "bypass-test-group",
			GroupType: store.GroupTypeExplicit,
			ProjectID: f.proj.ID,
			OwnerID:   f.owner.ID,
		}
		require.NoError(t, f.store.CreateGroup(ctx, group))

		// Create an addMember policy and bind it to the agent.
		policy := &store.Policy{
			ID:           uuid.New().String(),
			Name:         "Agent addMember on group",
			ScopeType:    "project",
			ScopeID:      f.proj.ID,
			ResourceType: "group",
			Actions:      []string{"addMember"},
			Effect:       "allow",
		}
		require.NoError(t, f.store.CreatePolicy(ctx, policy))
		require.NoError(t, f.store.AddPolicyBinding(ctx, &store.PolicyBinding{
			PolicyID:      policy.ID,
			PrincipalType: "agent",
			PrincipalID:   f.caller.ID,
		}))

		return f, group
	}

	t.Run("agent with addMember policy adds a plain member", func(t *testing.T) {
		f, group := setup(t)
		rec := f.asAgent(t, http.MethodPost,
			"/api/v1/groups/"+group.ID+"/members",
			AddGroupMemberRequest{
				MemberType: store.GroupMemberTypeUser,
				MemberID:   f.owner.ID,
				Role:       store.GroupMemberRoleMember,
			})
		assert.Equal(t, http.StatusCreated, rec.Code,
			"agent with addMember policy must be able to add a plain member; got: %s",
			rec.Body.String())
	})

	t.Run("agent with addMember policy cannot add admin", func(t *testing.T) {
		f, group := setup(t)
		rec := f.asAgent(t, http.MethodPost,
			"/api/v1/groups/"+group.ID+"/members",
			AddGroupMemberRequest{
				MemberType: store.GroupMemberTypeUser,
				MemberID:   f.owner.ID,
				Role:       store.GroupMemberRoleAdmin,
			})
		assert.Equal(t, http.StatusForbidden, rec.Code,
			"agent must not escalate to admin; got: %s", rec.Body.String())
	})

	t.Run("agent with addMember policy cannot add owner", func(t *testing.T) {
		f, group := setup(t)
		rec := f.asAgent(t, http.MethodPost,
			"/api/v1/groups/"+group.ID+"/members",
			AddGroupMemberRequest{
				MemberType: store.GroupMemberTypeUser,
				MemberID:   f.owner.ID,
				Role:       store.GroupMemberRoleOwner,
			})
		assert.Equal(t, http.StatusForbidden, rec.Code,
			"agent must not escalate to owner; got: %s", rec.Body.String())
	})
}

// ============================================================================
// PTY scope pinning — authorizeAgentLifecycle vs authorize
// ============================================================================

// TestBypassAgents_PTYScopePinning pins the choice of authorizeAgentLifecycle
// over authorize for handleAgentPTY. Using authorize here would compile, pass
// every test, and quietly swap the rule from ScopeAgentLifecycle to the
// project read baseline.
func TestBypassAgents_PTYScopePinning(t *testing.T) {
	wsHeaders := map[string]string{
		"Upgrade":    "websocket",
		"Connection": "upgrade",
	}

	t.Run("scoped agent on a project peer reaches past authz", func(t *testing.T) {
		// An agent with ScopeAgentLifecycle on a peer in its own project should
		// pass authorization. Without a runtime broker it gets 422, which proves
		// it made it past the auth check.
		f := bypassAgentsSetup(t)
		rec := f.asAgentWithHeaders(t, http.MethodGet,
			"/api/v1/agents/"+f.sibling.ID+"/pty", nil,
			wsHeaders, ScopeAgentLifecycle)
		assert.Equal(t, http.StatusUnprocessableEntity, rec.Code,
			"scoped agent should reach past authz to the no-runtime-broker check; got: %s",
			rec.Body.String())
	})

	t.Run("unscoped agent is denied with missing scope message", func(t *testing.T) {
		// An agent WITHOUT ScopeAgentLifecycle should get 403 with the specific
		// "Missing required scope" message, proving authorizeAgentLifecycle is
		// the gate rather than the read baseline's authorize.
		f := bypassAgentsSetup(t)
		rec := f.asAgentWithHeaders(t, http.MethodGet,
			"/api/v1/agents/"+f.sibling.ID+"/pty", nil,
			wsHeaders)
		assert.Equal(t, http.StatusForbidden, rec.Code,
			"unscoped agent must be denied; got: %s", rec.Body.String())
		assert.Contains(t, rec.Body.String(), string(ScopeAgentLifecycle),
			"denial must cite the missing scope to pin the authorizeAgentLifecycle choice")
	})

	t.Run("cross-project agent with scope is denied", func(t *testing.T) {
		f := bypassAgentsSetup(t)
		rec := f.asAgentWithHeaders(t, http.MethodGet,
			"/api/v1/agents/"+f.stranger.ID+"/pty", nil,
			wsHeaders, ScopeAgentLifecycle)
		assert.Equal(t, http.StatusForbidden, rec.Code,
			"cross-project agent must be denied even with the scope; got: %s",
			rec.Body.String())
	})
}
