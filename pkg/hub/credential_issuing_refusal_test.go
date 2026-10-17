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
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// credentialIssuingSpyKey marks a request context whose store reads
// credentialIssuingSpyStore counts. Only reads for a marked request
// are counted, so background server work cannot affect the count.
type credentialIssuingSpyKey struct{}

// credentialIssuingSpyStore counts the store reads a request to the
// credential-issuing endpoint makes to resolve its owner, its target
// project, the caller's bindings and the token count.
type credentialIssuingSpyStore struct {
	store.Store
	calls atomic.Int32
}

func (s *credentialIssuingSpyStore) count(ctx context.Context) {
	if ctx.Value(credentialIssuingSpyKey{}) != nil {
		s.calls.Add(1)
	}
}

// DB and Dialect forward to the wrapped store, so New() runs its
// migrations and startup checks against the real store.
func (s *credentialIssuingSpyStore) DB() *sql.DB {
	if p, ok := s.Store.(interface{ DB() *sql.DB }); ok {
		return p.DB()
	}
	return nil
}

func (s *credentialIssuingSpyStore) Dialect() string {
	if p, ok := s.Store.(interface{ Dialect() string }); ok {
		return p.Dialect()
	}
	return ""
}

func (s *credentialIssuingSpyStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	s.count(ctx)
	return s.Store.GetUser(ctx, id)
}

func (s *credentialIssuingSpyStore) GetProject(ctx context.Context, id string) (*store.Project, error) {
	s.count(ctx)
	return s.Store.GetProject(ctx, id)
}

func (s *credentialIssuingSpyStore) ListRoleBindingsForPrincipals(ctx context.Context, principals []store.PrincipalRef, scopeTypes []string, scopeIDs []string) ([]*store.RoleBinding, error) {
	s.count(ctx)
	return s.Store.ListRoleBindingsForPrincipals(ctx, principals, scopeTypes, scopeIDs)
}

func (s *credentialIssuingSpyStore) CountUserAccessTokens(ctx context.Context, userID string) (int, error) {
	s.count(ctx)
	return s.Store.CountUserAccessTokens(ctx, userID)
}

func (s *credentialIssuingSpyStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	s.count(ctx)
	return s.Store.WithTx(ctx, fn)
}

// credentialIssuingSpyServer returns a test server on a spy store, a project
// whose owner is ownerID, and the spy.
func credentialIssuingSpyServer(t *testing.T, projectID, ownerID string) (*Server, *credentialIssuingSpyStore) {
	t.Helper()
	base, err := newTestStore(t, ":memory:")
	require.NoError(t, err)
	spy := &credentialIssuingSpyStore{Store: base}
	srv, _ := testServerOnMigratedStore(t, spy)
	rs4Project(t, base, projectID, ownerID)
	rs4AddProjectRole(t, base, ownerID, projectID, store.ProjectRoleOwner)
	return srv, spy
}

// postCredentialIssuingAs sends POST /api/v1/auth/tokens through the hub's route
// table (route guard, then handler) with identity and cred installed as the
// authentication middleware would install them, and the spy marker set.
func postCredentialIssuingAs(t *testing.T, srv *Server, identity Identity, cred CredentialContext, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/tokens", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	ctx := context.WithValue(req.Context(), credentialIssuingSpyKey{}, true)
	if identity != nil {
		ctx = contextWithIdentity(ctx, identity)
		if user, ok := identity.(UserIdentity); ok {
			ctx = context.WithValue(ctx, userContextKey{}, user)
		}
		ctx = contextWithCredentialContext(ctx, cred)
	}
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req.WithContext(ctx))
	return rec
}

// TestCredentialIssuingEndpoint_NonSessionCallersGetOneRefusal pins the
// credential rule of the credential-issuing endpoint: every authenticated
// caller that is not a user holding an interactive session or dev
// credential gets one refusal, byte for byte: 403 with the fixed message
// and the CREDENTIAL_MANAGEMENT session-only reason. The refusal does not
// depend on the identity type, the credential kind (known, unknown or
// empty) or the requested target (an existing project, a missing project
// or the hub), and no owner, project, binding or token-count read runs
// before it.
func TestCredentialIssuingEndpoint_NonSessionCallersGetOneRefusal(t *testing.T) {
	projectID := tid("tcr-project")
	ownerID := tid("tcr-owner")
	srv, spy := credentialIssuingSpyServer(t, projectID, ownerID)

	user := NewAuthenticatedUser(ownerID, ownerID+"@test.com", "Owner", "member", string(ClientTypeAPI))
	callers := []struct {
		name     string
		identity Identity
		cred     CredentialContext
	}{
		{"user with access token", user, CredentialContext{Kind: CredentialKindUAT, ID: "uat-1"}},
		{"user with federation credential", user, CredentialContext{Kind: CredentialKindFederation, Type: "federated_user"}},
		{"user on behalf of broker", user, CredentialContext{Kind: CredentialKindBroker, ID: ownerID, Type: "user"}},
		{"user with agent credential", user, CredentialContext{Kind: CredentialKindAgentJWT, ID: "agent-jwt"}},
		{"user with delegated credential kind", user, CredentialContext{Kind: CredentialKindDelegatedAgent, ID: "dc-1"}},
		{"user with hub delivery kind", user, CredentialContext{Kind: CredentialKindHubDelivery}},
		{"user with empty credential kind", user, CredentialContext{}},
		{"user with unknown credential kind", user, CredentialContext{Kind: CredentialKind("not-a-kind")}},
		{"agent", cpmAgentIdentity(tid("tcr-agent"), projectID, nil), CredentialContext{Kind: CredentialKindAgentJWT, ID: "agent-jwt"}},
		{"agent with session credential kind", cpmAgentIdentity(tid("tcr-agent"), projectID, nil), CredentialContext{Kind: CredentialKindInteractive}},
		{"federated agent", NewFederatedAgentIdentity("https://peer.test", tid("tcr-fed-agent"), projectID, "a", "", nil, nil), CredentialContext{Kind: CredentialKindFederation}},
		{"broker", NewBrokerIdentity(tid("tcr-broker")), CredentialContext{Kind: CredentialKindBroker, ID: "broker-1"}},
		{"broker with unknown credential kind", NewBrokerIdentity(tid("tcr-broker")), CredentialContext{Kind: CredentialKind("not-a-kind")}},
	}
	targets := []struct {
		name string
		body map[string]interface{}
	}{
		{"existing project", map[string]interface{}{"name": "x", "projectId": projectID, "scopes": []string{"agent:read"}}},
		{"missing project", map[string]interface{}{"name": "x", "projectId": tid("tcr-missing"), "scopes": []string{"agent:read"}}},
		{"hub", map[string]interface{}{"name": "x", "boundary": map[string]string{"kind": "hub"}, "scopes": []string{"agent:read"}}},
	}

	var want []byte
	for _, c := range callers {
		for _, target := range targets {
			label := c.name + " / " + target.name
			before := spy.calls.Load()
			rec := postCredentialIssuingAs(t, srv, c.identity, c.cred, target.body)
			requireSessionOnlyRefusal(t, rec, authzop.ReasonCredentialManagement, label)
			assert.Equal(t, before, spy.calls.Load(), "%s: no store read may run before the refusal", label)
			if want == nil {
				want = rec.Body.Bytes()
				var env struct {
					Error struct {
						Message string `json:"message"`
					} `json:"error"`
				}
				require.NoError(t, json.Unmarshal(want, &env), string(want))
				assert.Equal(t, ErrUATCredentialDenied.Error(), env.Error.Message, "fixed refusal message")
				continue
			}
			assert.Equal(t, string(want), rec.Body.String(), "%s: refusal body must be identical", label)
		}
	}
}

// TestCredentialIssuingEndpoint_VisitorAndDelegatedRefusedBeforeLookup
// pins that a visitor and an agent delegated credential are refused at the
// credential-issuing endpoint with 403, and that no owner, project,
// binding or token-count read runs first, for an existing and a missing
// target project.
func TestCredentialIssuingEndpoint_VisitorAndDelegatedRefusedBeforeLookup(t *testing.T) {
	projectID := tid("tcrv-project")
	ownerID := tid("tcrv-owner")
	srv, spy := credentialIssuingSpyServer(t, projectID, ownerID)

	visitor := NewVisitorIdentity(ownerID, "https://issuer.test", "sub-1", "v@peer.test", "Visitor")
	delegated := newDelegatedAgentIdentity(
		&store.AgentDelegatedCredential{ID: tid("tcrv-cred")},
		&store.AgentDelegationGrant{ID: tid("tcrv-grant"), AgentID: tid("tcrv-agent"), AgentProjectID: projectID,
			IssuerUserID: ownerID, BoundaryKind: string(BoundaryKindHub)},
	)
	callers := []struct {
		name     string
		identity Identity
		cred     CredentialContext
	}{
		{"visitor", visitor, CredentialContext{Kind: CredentialKindFederation}},
		{"delegated agent credential", delegated, CredentialContext{Kind: CredentialKindDelegatedAgent, ID: "dc-1"}},
	}
	for _, c := range callers {
		for _, target := range []string{projectID, tid("tcrv-missing")} {
			before := spy.calls.Load()
			rec := postCredentialIssuingAs(t, srv, c.identity, c.cred,
				map[string]interface{}{"name": "x", "projectId": target, "scopes": []string{"agent:read"}})
			assert.Equal(t, http.StatusForbidden, rec.Code, "%s / %s: %s", c.name, target, rec.Body.String())
			assert.Equal(t, before, spy.calls.Load(), "%s / %s: no store read may run before the refusal", c.name, target)
		}
	}
}

// TestCredentialIssuingEndpoint_SessionAndDevCreate pins that a user holding an
// interactive session or dev credential creates a token (201), with the
// spy observing the owner and target reads, and that a request with no
// identity gets 401.
func TestCredentialIssuingEndpoint_SessionAndDevCreate(t *testing.T) {
	projectID := tid("tcrs-project")
	ownerID := tid("tcrs-owner")
	srv, spy := credentialIssuingSpyServer(t, projectID, ownerID)
	user := NewAuthenticatedUser(ownerID, ownerID+"@test.com", "Owner", "member", string(ClientTypeAPI))

	for _, kind := range []CredentialKind{CredentialKindInteractive, CredentialKindDev} {
		before := spy.calls.Load()
		rec := postCredentialIssuingAs(t, srv, user, CredentialContext{Kind: kind, ID: "s-1"},
			map[string]interface{}{"name": "tcrs-" + string(kind), "projectId": projectID, "scopes": []string{"agent:read"}})
		require.Equal(t, http.StatusCreated, rec.Code, "%s: %s", kind, rec.Body.String())
		assert.Greater(t, spy.calls.Load(), before, "%s: the spy observes the create path's reads", kind)
	}

	rec := postCredentialIssuingAs(t, srv, nil, CredentialContext{},
		map[string]interface{}{"name": "x", "projectId": projectID, "scopes": []string{"agent:read"}})
	assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
}

// TestCredentialIssuingEndpoint_RealCredentialsGetOneRefusal pins the same refusal for
// real credentials through the full middleware: a user access token and an
// agent token get the identical 403 body.
func TestCredentialIssuingEndpoint_RealCredentialsGetOneRefusal(t *testing.T) {
	srv, s, _, _, agentToken := setupAgentSecretTest(t)
	projectID := tid("tcrr-project")
	ownerID := tid("tcrr-owner")
	rs4Project(t, s, projectID, ownerID)
	rs4AddProjectRole(t, s, ownerID, projectID, store.ProjectRoleOwner)
	uatKey, _, err := srv.uatService.CreateTokenWithParams(rs4MintContext(ownerID), CreateTokenParams{
		UserID: ownerID, Name: "tcrr-" + tid("tok"), Boundary: projectBoundary(projectID), Scopes: []string{"agent:read"},
	})
	require.NoError(t, err)

	body := map[string]interface{}{"name": "x", "projectId": projectID, "scopes": []string{"agent:read"}}
	uatRec := doRequestWithUAT(t, srv, uatKey, http.MethodPost, "/api/v1/auth/tokens", body)
	agentRec := doRequestWithAgentToken(t, srv, http.MethodPost, "/api/v1/auth/tokens", body, agentToken)

	requireSessionOnlyRefusal(t, uatRec, authzop.ReasonCredentialManagement, "access token")
	requireSessionOnlyRefusal(t, agentRec, authzop.ReasonCredentialManagement, "agent token")
	assert.Equal(t, uatRec.Body.String(), agentRec.Body.String(), "refusal body must be identical")
}
