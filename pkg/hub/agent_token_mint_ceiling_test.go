// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
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
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mintBrokerClient records reset-auth calls on top of the dispatcher mock.
type mintBrokerClient struct {
	*mockRuntimeBrokerClient
	resetAuthCalled bool
	resetAuthToken  string
}

func (m *mintBrokerClient) ResetAuthAgent(_ context.Context, _, _, _, _, token string) error {
	m.resetAuthCalled = true
	m.resetAuthToken = token
	return nil
}

// edgeReadErrStore fails every agent-delegate edge read.
type edgeReadErrStore struct {
	store.Store
}

func (s *edgeReadErrStore) GetDelegationEdgesForDelegate(ctx context.Context, delegateType, delegateID string) ([]*store.DelegationEdge, error) {
	if delegateType == store.DelegationPrincipalAgent {
		return nil, errors.New("injected delegation edge read fault")
	}
	return s.Store.GetDelegationEdgesForDelegate(ctx, delegateType, delegateID)
}

// mintFixture is a test server whose dispatcher mints through the server
// against a recording broker client.
type mintFixture struct {
	srv       *Server
	store     store.Store
	disp      *HTTPAgentDispatcher
	client    *mintBrokerClient
	projectID string
	brokerID  string
	userID    string
}

func newMintFixture(t *testing.T, name string) *mintFixture {
	t.Helper()
	srv, s := testServer(t)
	project := setupProjectWithBroker(t, s, name, name)
	client := &mintBrokerClient{mockRuntimeBrokerClient: &mockRuntimeBrokerClient{}}
	disp := NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default())
	disp.SetTokenGenerator(srv)
	srv.SetDispatcher(disp)
	userID := tid(name + "-user")
	createDCUser(t, s, userID, name+"-user@test.com", project.ID, store.ProjectRoleOwner)
	return &mintFixture{
		srv: srv, store: s, disp: disp, client: client,
		projectID: project.ID, brokerID: tid("broker-" + name), userID: userID,
	}
}

// agent stores an agent on the fixture broker, with ancestry [user].
func (f *mintFixture) agent(t *testing.T, slug string, role AgentRole, phase state.Phase) *store.Agent {
	t.Helper()
	a := &store.Agent{
		ID: tid(slug), Slug: slug, Name: slug, ProjectID: f.projectID, OwnerID: f.userID,
		RuntimeBrokerID: f.brokerID, Phase: string(phase), StateVersion: 1,
		Ancestry:      []string{f.userID},
		AppliedConfig: &store.AgentAppliedConfig{AgentRole: string(role)},
		Created:       time.Now(), Updated: time.Now(),
	}
	require.NoError(t, f.store.CreateAgent(context.Background(), a))
	return a
}

// edge records an active project edge for delegate with the given ceiling
// and provenance.
func (f *mintFixture) edge(t *testing.T, delegatorType, delegatorID, delegateID string, c store.EffectCeiling, p store.AuthorityProvenance) {
	t.Helper()
	require.NoError(t, f.store.CreateDelegationEdge(context.Background(), &store.DelegationEdge{
		DelegatorType: delegatorType, DelegatorID: delegatorID,
		DelegateType: store.DelegationPrincipalAgent, DelegateID: delegateID,
		ScopeType: store.RoleScopeProject, ScopeID: f.projectID,
		Role: string(AgentRoleFull), Active: true,
		AuthorityProvenance: p, EffectCeiling: c,
	}))
}

// tokenClaims validates a minted token.
func (f *mintFixture) tokenClaims(t *testing.T, token string) *AgentTokenClaims {
	t.Helper()
	require.NotEmpty(t, token)
	claims, err := f.srv.agentTokenService.ValidateAgentToken(token)
	require.NoError(t, err)
	return claims
}

// refresh calls the refresh handler with a token minted for agent and the
// given presented ancestry.
func (f *mintFixture) refresh(t *testing.T, agent *store.Agent, presentedAncestry []string) *httptest.ResponseRecorder {
	t.Helper()
	presented, err := f.srv.agentTokenService.GenerateAgentToken(agent.ID, f.projectID,
		[]AgentTokenScope{ScopeAgentStatusUpdate, ScopeAgentTokenRefresh}, presentedAncestry)
	require.NoError(t, err)
	claims := f.tokenClaims(t, presented)
	rec := httptest.NewRecorder()
	f.srv.handleAgentTokenRefresh(rec, buildAgentRefreshRequest(agent.ID, claims, "", false), agent.ID)
	return rec
}

func refreshedToken(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return body.Token
}

// issueDeniedAudits returns the agent_token_issue_denied records for agent.
func issueDeniedAudits(t *testing.T, s store.Store, agentID string) []*store.MutationAuditRecord {
	t.Helper()
	recs, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{
		MutationType: mutationTypeAgentTokenIssueDenied, TargetType: "agent", TargetID: agentID,
	})
	require.NoError(t, err)
	return recs
}

// assertIssueDeniedAudit asserts one record naming the site and cause, and
// no scope list.
func assertIssueDeniedAudit(t *testing.T, s store.Store, agentID, site, cause string) {
	t.Helper()
	recs := issueDeniedAudits(t, s, agentID)
	require.Len(t, recs, 1)
	var summary map[string]string
	require.NoError(t, json.Unmarshal([]byte(recs[0].AfterSummary), &summary))
	assert.Equal(t, map[string]string{"site": site, "deny_cause": cause}, summary)
	assert.NotContains(t, recs[0].AfterSummary, "scope")
}

func assertCeilingDenied(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, string(DeniedByDelegationCeiling), decodeTargetAPIError(t, rec).Details["denied_by"])
}

func readonlyCoverageCeiling() store.EffectCeiling {
	return boundedCeiling(sortedUniqueIDs(agentScopeCoverage(ScopesForRole(AgentRoleReadOnly)))...)
}

// Every mint site issues the same ceiled scope set, and no production caller
// of the role-only GenerateAgentToken remains.
func TestAllMintSitesUseCeiledHelper(t *testing.T) {
	f := newMintFixture(t, "allsites")
	ctx := context.Background()
	a := f.agent(t, "allsites-agent", AgentRoleFull, state.PhaseRunning)
	ceiling := readonlyCoverageCeiling()
	f.edge(t, store.DelegationPrincipalUser, f.userID, a.ID, ceiling, provSession)
	want := filterScopes(f.srv.authzService.mintCandidateScopes(a), ceiling, ScopeCeilings{})
	require.NotEmpty(t, want)
	require.Less(t, len(want), len(ScopesForRole(AgentRoleFull)), "the ceiling narrows the full scope set")

	require.NoError(t, f.disp.DispatchAgentCreate(ctx, a))
	assert.ElementsMatch(t, want, f.tokenClaims(t, f.client.lastCreateReq.AgentToken).Scopes, "create")

	require.NoError(t, f.disp.DispatchAgentStart(ctx, a, "", false))
	assert.ElementsMatch(t, want, f.tokenClaims(t, f.client.lastResolvedEnv["SCION_AUTH_TOKEN"]).Scopes, "start")

	require.NoError(t, f.disp.DispatchAgentRestart(ctx, a))
	assert.ElementsMatch(t, want, f.tokenClaims(t, f.client.lastRestartResolvedEnv["SCION_AUTH_TOKEN"]).Scopes, "restart")

	require.NoError(t, f.disp.DispatchAgentResetAuth(ctx, a))
	assert.ElementsMatch(t, want, f.tokenClaims(t, f.client.resetAuthToken).Scopes, "reset-auth")

	tok := refreshedToken(t, f.refresh(t, a, a.Ancestry))
	assert.ElementsMatch(t, want, f.tokenClaims(t, tok).Scopes, "refresh")

	// Grep pin: outside test files, GenerateAgentToken( appears only as the
	// two declarations, the interface method, and the token-service calls
	// inside the old helper and GenerateAgentTokenForAgent.
	allowed := map[string]bool{
		"agent_token_mint.go:return tokenService.GenerateAgentToken(agent.ID, agent.ProjectID, scopes, agent.Ancestry)": true,
		"server.go:return tokenService.GenerateAgentToken(agentID, projectID, scopes, ancestry)":                        true,
	}
	call := regexp.MustCompile(`\bGenerateAgentToken\(`)
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	var unexpected []string
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		data, err := os.ReadFile(file)
		require.NoError(t, err)
		for i, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if !call.MatchString(trimmed) || strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "func ") {
				continue
			}
			if strings.HasPrefix(trimmed, "GenerateAgentToken(agentID, projectID string") && file == "httpdispatcher.go" {
				continue // the interface method
			}
			if !allowed[file+":"+trimmed] {
				unexpected = append(unexpected, file+":"+itoa(i+1)+": "+trimmed)
			}
		}
	}
	assert.Empty(t, unexpected, "production GenerateAgentToken callers outside GenerateAgentTokenForAgent")
}

// Start of an agent with no edge after the backfill: 403 at the delegation
// ceiling, no broker request, and an agent_token_issue_denied record.
func TestDispatcherStartAbortsOnCeilingOrphaned(t *testing.T) {
	f := newMintFixture(t, "start-orphan")
	setBackfillCompleted(t, f.store)
	a := f.agent(t, "start-orphan-agent", AgentRoleFull, state.PhaseStopped)

	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/start", nil)
	assertCeilingDenied(t, rec)
	assert.False(t, f.client.startCalled, "no broker request")
	assertIssueDeniedAudit(t, f.store, a.ID, mintSiteStart, string(DenyCauseCeilingOrphaned))
}

// A lookup fault at the create mint: 503, no broker create, no agent row.
func TestDispatcherCreateAbortsOnCeilingLookupError(t *testing.T) {
	f := newMintFixture(t, "create-lookup")
	f.srv.authzService.store = &edgeReadErrStore{Store: f.store}

	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/projects/"+f.projectID+"/agents",
		CreateAgentRequest{Name: "lookup-child"})
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assert.Equal(t, ErrCodeUnavailable, decodeTargetAPIError(t, rec).Code)
	assert.False(t, f.client.createCalled, "no broker create")
	_, err := f.store.GetAgentBySlug(context.Background(), f.projectID, "lookup-child")
	assert.ErrorIs(t, err, store.ErrNotFound, "no agent row left")

	recs, _, err := f.store.ListMutationAudits(context.Background(), store.MutationAuditFilter{
		MutationType: mutationTypeAgentTokenIssueDenied,
	})
	require.NoError(t, err)
	require.Len(t, recs, 1)
	assert.JSONEq(t, `{"site":"create","deny_cause":"lookup_error"}`, recs[0].AfterSummary)
}

// Restart with no edge after the backfill returns the mapped error before
// any broker request.
func TestDispatcherRestartAbortsOnMintError(t *testing.T) {
	f := newMintFixture(t, "restart-orphan")
	setBackfillCompleted(t, f.store)
	a := f.agent(t, "restart-orphan-agent", AgentRoleFull, state.PhaseRunning)

	err := f.disp.DispatchAgentRestart(context.Background(), a)
	require.ErrorIs(t, err, ErrProvenanceMissing)
	assert.False(t, f.client.restartCalled, "no broker request")
	rec := httptest.NewRecorder()
	require.True(t, writeAgentTokenIssueError(rec, err))
	assertCeilingDenied(t, rec)
	assertIssueDeniedAudit(t, f.store, a.ID, mintSiteRestart, string(DenyCauseCeilingOrphaned))
	recs := issueDeniedAudits(t, f.store, a.ID)
	assert.Equal(t, mintAuditSystemActorKind, recs[0].ActorPrincipalKind, "hub-initiated restart")
	assert.Equal(t, mintAuditSystemActorID, recs[0].ActorPrincipalID)
}

// Refresh mints with the stored ancestry, not the presented token's.
func TestRefreshUsesStoredAncestry(t *testing.T) {
	f := newMintFixture(t, "refresh-anc")
	a := f.agent(t, "refresh-anc-agent", AgentRoleFull, state.PhaseRunning)
	f.edge(t, store.DelegationPrincipalUser, f.userID, a.ID, ceilPrincip, provSession)

	tok := refreshedToken(t, f.refresh(t, a, []string{f.userID, tid("refresh-anc-other")}))
	assert.Equal(t, a.Ancestry, f.tokenClaims(t, tok).Ancestry)
}

// After a stored role raise, the refreshed scopes stay within the edge
// ceiling.
func TestRefreshRespectsEdgeCeiling(t *testing.T) {
	f := newMintFixture(t, "refresh-ceil")
	ctx := context.Background()
	f.srv.authzService.mintDevAuthOverride = false
	a := f.agent(t, "refresh-ceil-agent", AgentRoleReadOnly, state.PhaseRunning)
	ceiling := readonlyCoverageCeiling()
	f.edge(t, store.DelegationPrincipalUser, f.userID, a.ID, ceiling, provSession)

	a.AppliedConfig.AgentRole = string(AgentRoleFull)
	require.NoError(t, f.store.UpdateAgent(ctx, a))

	scopes := f.tokenClaims(t, refreshedToken(t, f.refresh(t, a, a.Ancestry))).Scopes
	require.NotEmpty(t, scopes)
	for _, sc := range scopes {
		assert.True(t, ceilingAllowsScope(ceiling, sc), "scope %s outside the ceiling", sc)
	}
	assert.NotContains(t, scopes, ScopeAgentCreate)
}

// Refresh with no edge after the backfill is 403; an edge lookup fault is
// 503. Neither issues a token.
func TestRefreshNoEdgeDenies(t *testing.T) {
	f := newMintFixture(t, "refresh-noedge")
	setBackfillCompleted(t, f.store)
	a := f.agent(t, "refresh-noedge-agent", AgentRoleFull, state.PhaseRunning)

	rec := f.refresh(t, a, a.Ancestry)
	assertCeilingDenied(t, rec)
	assert.NotContains(t, rec.Body.String(), `"token"`)
	assertIssueDeniedAudit(t, f.store, a.ID, mintSiteRefresh, string(DenyCauseCeilingOrphaned))
}

func TestRefreshEdgeLookupError503(t *testing.T) {
	f := newMintFixture(t, "refresh-lookup")
	a := f.agent(t, "refresh-lookup-agent", AgentRoleFull, state.PhaseRunning)
	f.edge(t, store.DelegationPrincipalUser, f.userID, a.ID, ceilPrincip, provSession)
	f.srv.authzService.store = &edgeReadErrStore{Store: f.store}

	rec := f.refresh(t, a, a.Ancestry)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), `"token"`)
	assertIssueDeniedAudit(t, f.store, a.ID, mintSiteRefresh, mintErrorClassLookup)
}

// The dev-auth full override is narrowed by a bounded ceiling.
func TestDevAuthOverrideDoesNotExceedCeiling(t *testing.T) {
	f := newMintFixture(t, "devauth-ceil")
	require.True(t, f.srv.authzService.mintDevAuthOverride, "test server runs with dev auth")
	a := f.agent(t, "devauth-ceil-agent", AgentRoleReadOnly, state.PhaseRunning)
	ceiling := readonlyCoverageCeiling()
	f.edge(t, store.DelegationPrincipalUser, f.userID, a.ID, ceiling, provSession)

	token, err := f.srv.GenerateAgentTokenForAgent(context.Background(), a)
	require.NoError(t, err)
	assert.ElementsMatch(t, filterScopes(ScopesForRole(AgentRoleFull), ceiling, ScopeCeilings{}), f.tokenClaims(t, token).Scopes)
	assert.NotContains(t, f.tokenClaims(t, token).Scopes, ScopeAgentCreate)
}

// An explicit role-none agent is minted a token: without dev auth it holds
// no role scopes; with dev auth it is raised to full and bounded by the
// chain ceiling.
func TestRoleNoneMintIssuesRoleDerivedScopes(t *testing.T) {
	f := newMintFixture(t, "none-mint")
	ctx := context.Background()
	a := f.agent(t, "none-mint-agent", AgentRoleNone, state.PhaseRunning)
	ceiling := readonlyCoverageCeiling()
	f.edge(t, store.DelegationPrincipalUser, f.userID, a.ID, ceiling, provSession)

	f.srv.authzService.mintDevAuthOverride = false
	token, err := f.srv.GenerateAgentTokenForAgent(ctx, a)
	require.NoError(t, err)
	// No role scopes; the token service issues its default status scope for
	// an empty set.
	assert.Equal(t, []AgentTokenScope{ScopeAgentStatusUpdate}, f.tokenClaims(t, token).Scopes, "without dev auth")

	f.srv.authzService.mintDevAuthOverride = true
	token, err = f.srv.GenerateAgentTokenForAgent(ctx, a)
	require.NoError(t, err)
	assert.ElementsMatch(t, filterScopes(ScopesForRole(AgentRoleFull), ceiling, ScopeCeilings{}), f.tokenClaims(t, token).Scopes)
}

// A grandchild mints within its parent's current edge ceiling after that
// edge is replaced by a narrower one; the GCP token scope follows the chain.
func TestMintUsesChainCeiling(t *testing.T) {
	f := newMintFixture(t, "chain-mint")
	ctx := context.Background()
	f.srv.authzService.mintDevAuthOverride = false
	saID := tid("chain-mint-sa")

	parent := f.agent(t, "chain-mint-parent", AgentRoleFull, state.PhaseRunning)
	child := f.agent(t, "chain-mint-child", AgentRoleFull, state.PhaseRunning)
	child.Ancestry = []string{f.userID, parent.ID}
	child.AppliedConfig.GCPIdentity = &store.GCPIdentityConfig{MetadataMode: store.GCPMetadataModeAssign, ServiceAccountID: saID}
	require.NoError(t, f.store.UpdateAgent(ctx, child))

	broad := boundedCeiling(sortedUniqueIDs(append(agentScopeCoverage(ScopesForRole(AgentRoleFull)), "gcp_service_account.assign"))...)
	f.edge(t, store.DelegationPrincipalUser, f.userID, parent.ID, broad, provSession)
	f.edge(t, store.DelegationPrincipalAgent, parent.ID, child.ID, broad, provAgent)

	token, err := f.srv.GenerateAgentTokenForAgent(ctx, child)
	require.NoError(t, err)
	before := f.tokenClaims(t, token).Scopes
	assert.Contains(t, before, ScopeAgentCreate)
	assert.Contains(t, before, GCPTokenScopeForSA(saID))

	// Replace the parent's edge with the readonly coverage ceiling.
	parentEdges, err := f.store.GetDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, parent.ID)
	require.NoError(t, err)
	for _, e := range parentEdges {
		require.NoError(t, f.store.DeactivateDelegationEdge(ctx, e.ID))
	}
	narrow := readonlyCoverageCeiling()
	f.edge(t, store.DelegationPrincipalUser, f.userID, parent.ID, narrow, provSession)

	token, err = f.srv.GenerateAgentTokenForAgent(ctx, child)
	require.NoError(t, err)
	after := f.tokenClaims(t, token).Scopes
	for _, sc := range after {
		assert.True(t, ceilingAllowsScope(narrow, sc), "scope %s outside the parent's current ceiling", sc)
	}
	assert.NotContains(t, after, ScopeAgentCreate)
	assert.NotContains(t, after, GCPTokenScopeForSA(saID))
}

// A child created through DevAuthMiddleware: the create mint and a refresh
// both yield the dev-auth full scope set.
func TestDevAuthChildMintAndRefresh(t *testing.T) {
	f := newMintFixture(t, "dev-mint")
	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/projects/"+f.projectID+"/agents",
		CreateAgentRequest{Name: "dev-mint-child"})
	require.True(t, rec.Code == http.StatusCreated || rec.Code == http.StatusAccepted, "create: %d %s", rec.Code, rec.Body.String())
	require.True(t, f.client.createCalled)
	full := ScopesForRole(AgentRoleFull)
	assert.ElementsMatch(t, full, f.tokenClaims(t, f.client.lastCreateReq.AgentToken).Scopes, "create mint")

	child, err := f.store.GetAgentBySlug(context.Background(), f.projectID, "dev-mint-child")
	require.NoError(t, err)
	tok := refreshedToken(t, f.refresh(t, child, child.Ancestry))
	assert.ElementsMatch(t, full, f.tokenClaims(t, tok).Scopes, "refresh")
}

// devCreatedChild creates a child through DevAuthMiddleware with no
// dispatcher mint, and returns its stored record.
func devCreatedChild(t *testing.T, f *mintFixture, name string) *store.Agent {
	t.Helper()
	f.srv.SetDispatcher(nil)
	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/projects/"+f.projectID+"/agents", CreateAgentRequest{Name: name})
	require.True(t, rec.Code == http.StatusCreated || rec.Code == http.StatusAccepted, "create: %d %s", rec.Code, rec.Body.String())
	f.srv.SetDispatcher(f.disp)
	child, err := f.store.GetAgentBySlug(context.Background(), f.projectID, name)
	require.NoError(t, err)
	edges := activeEdgesFor(t, f.store, child.ID)
	require.Len(t, edges, 1)
	require.Equal(t, store.SourceCredentialDevLocal, edges[0].SourceCredentialKind)
	child.RuntimeBrokerID = f.brokerID
	child.Phase = string(state.PhaseStopped)
	require.NoError(t, f.store.UpdateAgent(context.Background(), child))
	return child
}

// With dev-local authority disabled, a dev-created child gets no token at
// start or refresh, and the walk denies with the same cause.
func TestDevLocalEdgeDeniedWhenDevAuthDisabled(t *testing.T) {
	f := newMintFixture(t, "dev-off")
	child := devCreatedChild(t, f, "dev-off-child")
	f.srv.authzService.setDevLocalAuthorityEnabled(false)

	// Start mint: 403 ceiling_source_not_allowed, no broker request.
	err := f.disp.DispatchAgentStart(context.Background(), child, "", false)
	rec := httptest.NewRecorder()
	require.True(t, writeAgentTokenIssueError(rec, err), "err %v", err)
	assertCeilingDenied(t, rec)
	assert.ErrorIs(t, err, errSourceNotAllowed)
	assert.False(t, f.client.startCalled)
	assertIssueDeniedAudit(t, f.store, child.ID, mintSiteStart, string(DenyCauseCeilingSourceNotAllowed))

	// Refresh: 403, no token.
	rec = f.refresh(t, child, child.Ancestry)
	assertCeilingDenied(t, rec)
	assert.NotContains(t, rec.Body.String(), `"token"`)

	// The delegation walk for the child denies with the same cause.
	var cause DenyCause
	var allowed bool
	allowed, _, err = f.srv.authzService.walkDelegationChainWithCause(context.Background(),
		Resource{Type: "agent", ParentType: "project", ParentID: f.projectID}, ActionCreate, "agent.create",
		child.ID, true, store.RoleScopeProject, f.projectID, nil, &cause)
	require.NoError(t, err)
	assert.False(t, allowed)
	assert.Equal(t, DenyCauseCeilingSourceNotAllowed, cause)
}

// With dev auth enabled, a dev_local edge whose delegator is not the dev
// user, or whose dev user is suspended, gets no token at mint or refresh; a
// dev user lookup fault is 503.
func TestDevLocalEdgeDeniedWhenDevUserInactive(t *testing.T) {
	ctx := context.Background()

	t.Run("delegator is not the dev user", func(t *testing.T) {
		f := newMintFixture(t, "devu-other")
		a := f.agent(t, "devu-other-agent", AgentRoleFull, state.PhaseStopped)
		f.edge(t, store.DelegationPrincipalUser, f.userID, a.ID, ceilPrincip, provDevLocal)

		err := f.disp.DispatchAgentStart(ctx, a, "", false)
		require.ErrorIs(t, err, errSourceNotAllowed)
		assert.False(t, f.client.startCalled)
		assertIssueDeniedAudit(t, f.store, a.ID, mintSiteStart, string(DenyCauseCeilingSourceNotAllowed))
		assertCeilingDenied(t, f.refresh(t, a, a.Ancestry))
	})

	t.Run("dev user suspended", func(t *testing.T) {
		f := newMintFixture(t, "devu-susp")
		a := f.agent(t, "devu-susp-agent", AgentRoleFull, state.PhaseStopped)
		f.edge(t, store.DelegationPrincipalUser, DevUserID, a.ID, ceilPrincip, provDevLocal)
		_, err := f.srv.GenerateAgentTokenForAgent(ctx, a)
		require.NoError(t, err, "control: active dev user")

		u, err := f.store.GetUser(ctx, DevUserID)
		require.NoError(t, err)
		u.Status = "suspended"
		require.NoError(t, f.store.UpdateUser(ctx, u))

		rec := httptest.NewRecorder()
		err = f.disp.DispatchAgentStart(ctx, a, "", false)
		require.True(t, writeAgentTokenIssueError(rec, err), "err %v", err)
		assertCeilingDenied(t, rec)
		assert.False(t, f.client.startCalled)
		assertIssueDeniedAudit(t, f.store, a.ID, mintSiteStart, string(DenyCauseCeilingSourceNotAllowed))
		assertCeilingDenied(t, f.refresh(t, a, a.Ancestry))
	})

	t.Run("dev user lookup fault", func(t *testing.T) {
		f := newMintFixture(t, "devu-fault")
		a := f.agent(t, "devu-fault-agent", AgentRoleFull, state.PhaseRunning)
		f.edge(t, store.DelegationPrincipalUser, DevUserID, a.ID, ceilPrincip, provDevLocal)
		f.srv.authzService.store = &getUserErrStore{Store: f.store, failID: DevUserID}

		rec := f.refresh(t, a, a.Ancestry)
		require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
		assertIssueDeniedAudit(t, f.store, a.ID, mintSiteRefresh, mintErrorClassLookup)
	})
}
