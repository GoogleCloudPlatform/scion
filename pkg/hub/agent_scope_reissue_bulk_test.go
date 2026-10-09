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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func (f *reissueFixture) bulk(t *testing.T, dryRun bool) *ScopeReissueBulkResponse {
	t.Helper()
	resp, err := f.srv.runScopeReissueBulk(context.Background(), f.operator, dryRun)
	require.NoError(t, err)
	return resp
}

func bulkAgent(t *testing.T, resp *ScopeReissueBulkResponse, id string) ScopeReissueBulkAgent {
	t.Helper()
	for _, a := range resp.Agents {
		if a.ID == id {
			return a
		}
	}
	t.Fatalf("agent %s not in bulk result", id)
	return ScopeReissueBulkAgent{}
}

func batchAudits(t *testing.T, s store.Store) []*store.MutationAuditRecord {
	t.Helper()
	recs, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{MutationType: mutationTypeAgentScopesReissueBatch})
	require.NoError(t, err)
	return recs
}

// T10: in one bulk run, P is processed before A, so A is computed against
// P's freshly committed record and gains the scopes in the same run.
func TestScopeReissueBulk_T10_TopDown(t *testing.T) {
	f := newReissueFixture(t, "rsb-t10", store.ProjectRoleOwner)
	resp := f.bulk(t, false)

	assert.Equal(t, 0, bulkAgent(t, resp, f.root.ID).Depth)
	assert.Equal(t, 1, bulkAgent(t, resp, f.parent.ID).Depth)
	assert.Equal(t, 2, bulkAgent(t, resp, f.child.ID).Depth)
	child := bulkAgent(t, resp, f.child.ID)
	assert.Equal(t, "changed", child.Outcome)
	assert.Equal(t, artifactScopeStrings(), reissueSorted(child.Added))
	for _, s := range artifactScopeStrings() {
		assert.Contains(t, scopeStrings(f.grant(t, f.child)), s)
	}
	assert.Equal(t, "noop", bulkAgent(t, resp, f.root.ID).Outcome, "session-rooted root has nothing to change")

	// Each agent has its own row carrying the batch ID.
	recs := reissueAudits(t, f.store, f.child.ID, mutationTypeAgentScopesReissued)
	require.Len(t, recs, 1)
	assert.Equal(t, resp.BatchOpID, decodeReissueSummary(t, recs[0]).BatchOpID)

	// One batch row, counts only.
	batch := batchAudits(t, f.store)
	require.Len(t, batch, 1)
	var summary reissueBatchSummary
	require.NoError(t, json.Unmarshal([]byte(batch[0].AfterSummary), &summary))
	assert.Equal(t, reissueBatchSummary{BatchOpID: resp.BatchOpID, DryRun: false, Total: 3, Succeeded: 2, Noop: 1}, summary)
	assert.NotContains(t, batch[0].AfterSummary, "scope")
	assert.NotContains(t, batch[0].AfterSummary, "artifact")
}

// selectiveFailClient fails the reset-auth push for one agent slug.
type selectiveFailClient struct {
	*mintBrokerClient
	failSlug string
}

func (m *selectiveFailClient) ResetAuthAgent(ctx context.Context, brokerID, endpoint, slug, projectID, token, transport string) error {
	if slug == m.failSlug {
		return errors.New("injected push failure")
	}
	return m.mintBrokerClient.ResetAuthAgent(ctx, brokerID, endpoint, slug, projectID, token, transport)
}

// T11: one agent's refusal and another's push failure leave the others
// applied, each with its own row; a re-run touches only the refused agent.
func TestScopeReissueBulk_T11_Isolation(t *testing.T) {
	f := newReissueFixture(t, "rsb-t11", store.ProjectRoleOwner)
	broken := f.agent(t, "rsb-t11-broken", AgentRoleFull, state.PhaseRunning)
	f.edge(t, store.DelegationPrincipalAgent, f.parent.ID, broken.ID, store.EffectCeiling{}, store.AuthorityProvenance{})
	disp := NewHTTPAgentDispatcherWithClient(f.store, &selectiveFailClient{mintBrokerClient: f.client, failSlug: f.child.Slug}, false, nil)
	disp.SetTokenGenerator(f.srv)
	f.srv.SetDispatcher(disp)

	resp := f.bulk(t, false)
	assert.Equal(t, "refused", bulkAgent(t, resp, broken.ID).Outcome)
	assert.Equal(t, string(DenyCauseCeilingUnrecorded), bulkAgent(t, resp, broken.ID).Cause)
	assert.Equal(t, "push_failed", bulkAgent(t, resp, f.child.ID).Outcome)
	assert.Equal(t, "changed", bulkAgent(t, resp, f.parent.ID).Outcome)
	require.Len(t, resp.Refused, 1)
	require.Len(t, resp.PushFailed, 1)
	assertIssueDeniedAudit(t, f.store, broken.ID, mintSiteReissue, string(DenyCauseCeilingUnrecorded))
	require.Len(t, reissueAudits(t, f.store, f.child.ID, mutationTypeAgentScopesReissueDispatch), 1)

	again := f.bulk(t, false)
	assert.Equal(t, "refused", bulkAgent(t, again, broken.ID).Outcome)
	for _, id := range []string{f.root.ID, f.parent.ID, f.child.ID} {
		assert.Equal(t, "noop", bulkAgent(t, again, id).Outcome, "re-run is a no-op for %s", id)
	}
}

// T12: without an explicit dry_run=false the bulk run writes no record and
// no credential change, and its report equals the applied run's diff.
func TestScopeReissueBulk_T12_DryRunDefault(t *testing.T) {
	f := newReissueFixture(t, "rsb-t12", store.ProjectRoleOwner)
	f.run(t, f.parent, false) // so the child's dry-run diff is not order-dependent
	jti := "rsb-t12-jti"
	insertTestAgentCredential(t, f.store, f.child.ID, f.projectID, jti)
	credBefore := getTestAgentCredential(t, f.store, jti)
	edges := f.allEdges(t, f.child)

	body, err := json.Marshal(map[string]bool{"reissue_scopes": true})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/agents/reset-auth-all", bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	admin := f.adminSession(t)
	req = req.WithContext(contextWithCredentialContext(contextWithIdentity(req.Context(), admin), CredentialContext{Kind: CredentialKindInteractive}))
	rec := httptest.NewRecorder()
	f.srv.handleAdminResetAuthAll(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var dry ScopeReissueBulkResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &dry))
	assert.True(t, dry.DryRun, "dry run is the default")
	assert.Equal(t, edges, f.allEdges(t, f.child))
	assertCredentialUnrevoked(t, f.store, jti, credBefore)

	applied := f.bulk(t, false)
	for _, id := range []string{f.root.ID, f.parent.ID, f.child.ID} {
		d, a := bulkAgent(t, &dry, id), bulkAgent(t, applied, id)
		// The dry run came back over JSON, where an empty list is omitted.
		assert.ElementsMatch(t, d.Added, a.Added, id)
		assert.ElementsMatch(t, d.Removed, a.Removed, id)
		assert.Equal(t, d.RoleAfter, a.RoleAfter, id)
	}
}

func (f *reissueFixture) adminSession(t *testing.T) UserIdentity {
	t.Helper()
	id := tid(f.projectID + "-admin")
	if _, err := f.store.GetUser(context.Background(), id); err != nil {
		require.NoError(t, f.store.CreateUser(context.Background(), &store.User{
			ID: id, Email: id + "@test.com", DisplayName: "Admin", Role: "admin", Status: "active",
		}))
	}
	grantSuperAdmin(t, f.store, id)
	return NewAuthenticatedUser(id, id+"@test.com", "Admin", "admin", "")
}

// T13: with more agents than one page, every agent is processed; a page
// that cannot be read fails the batch and changes nothing.
func TestScopeReissueBulk_T13_NoTruncation(t *testing.T) {
	f := newReissueFixture(t, "rsb-t13", store.ProjectRoleOwner)
	for i := 0; i < 4; i++ {
		f.childAgent(t, "rsb-t13-extra-"+itoa(i), f.parent, AgentRoleFull)
	}
	prev := reissueBulkPageSize
	reissueBulkPageSize = 2
	t.Cleanup(func() { reissueBulkPageSize = prev })

	prevFault := reissueBulkListFault
	reissueBulkListFault = func(page int) error {
		if page == 2 {
			return errors.New("injected page read fault")
		}
		return nil
	}
	edges := f.allEdges(t, f.child)
	_, err := f.srv.runScopeReissueBulk(context.Background(), f.operator, false)
	require.ErrorIs(t, err, errReissueEnumeration)
	assert.Equal(t, edges, f.allEdges(t, f.child), "a failed enumeration changes nothing")
	assert.Empty(t, batchAudits(t, f.store), "never reported as a completed batch")
	reissueBulkListFault = prevFault

	resp := f.bulk(t, false)
	assert.Equal(t, 7, resp.Total, "root, parent, child and four extras")
	assert.Len(t, resp.Agents, 7)
}

// T14: an agent whose only change is a dropped scope is applied as a
// removal, never skipped as a no-op.
func TestScopeReissueBulk_T14_RemovalIsNotNoop(t *testing.T) {
	f := newUserReissueFixture(t, "rsb-t14")
	a := f.agent(t, "rsb-t14-agent", AgentRoleFull, state.PhaseRunning)
	f.edge(t, store.DelegationPrincipalUser, f.userID, a.ID, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, sessionProv(f.userID))
	prev := reissueLiveCheckFault
	reissueLiveCheckFault = func(perm string) error {
		if perm == "artifact.create" {
			return errors.New("injected permission lookup fault")
		}
		return nil
	}
	t.Cleanup(func() { reissueLiveCheckFault = prev })

	resp := f.bulk(t, false)
	got := bulkAgent(t, resp, a.ID)
	assert.Equal(t, "changed", got.Outcome)
	assert.Empty(t, got.Added)
	assert.Equal(t, []string{string(ScopeProjectArtifactWrite)}, got.Removed)
	assert.NotContains(t, scopeStrings(f.grant(t, a)), string(ScopeProjectArtifactWrite))
}

// Only a hub super-admin session may run the bulk re-issue.
func TestScopeReissueBulk_OperatorRefusals(t *testing.T) {
	f := newReissueFixture(t, "rsb-op", store.ProjectRoleOwner)
	body, err := json.Marshal(map[string]bool{"reissue_scopes": true, "dry_run": false})
	require.NoError(t, err)
	member := NewAuthenticatedUser(f.userID, "owner@test.com", "Owner", "member", "")
	claims := &AgentTokenClaims{ProjectID: f.projectID, Scopes: ScopesForRole(AgentRoleFull)}
	claims.Subject = f.child.ID
	for name, tc := range map[string]struct {
		identity Identity
		cred     CredentialContext
	}{
		"member session": {member, CredentialContext{Kind: CredentialKindInteractive}},
		"agent token":    {&agentIdentityWrapper{claims}, CredentialContext{Kind: CredentialKindAgentJWT}},
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/agents/reset-auth-all", bytes.NewReader(body))
			req.ContentLength = int64(len(body))
			req = req.WithContext(contextWithCredentialContext(contextWithIdentity(req.Context(), tc.identity), tc.cred))
			rec := httptest.NewRecorder()
			f.srv.handleAdminResetAuthAll(rec, req)
			assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		})
	}
	assert.Len(t, f.allEdges(t, f.child), 1)
	assert.Empty(t, batchAudits(t, f.store))
}
