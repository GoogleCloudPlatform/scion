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
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
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

// bulkHTTP calls the reset-auth-all handler as identity with cred.
func bulkHTTP(t *testing.T, srv *Server, ctx context.Context, body any, identity Identity, cred CredentialContext) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/agents/reset-auth-all", bytes.NewReader(data))
	req.ContentLength = int64(len(data))
	req = req.WithContext(contextWithCredentialContext(contextWithIdentity(ctx, identity), cred))
	rec := httptest.NewRecorder()
	srv.handleAdminResetAuthAll(rec, req)
	return rec
}

// T12: without an explicit dry_run=false the bulk run is a dry run. A bulk
// dry run audits exactly as a single-agent --dry-run does for each agent
// (one agent_scopes_reissued row with dry_run=true, carrying the shared
// batch_op_id), plus the batch row with dry_run=true and counts. It writes
// no edge and no credential change, and its report equals the applied
// run's diff
// for a parent and child in one tree, where the child gains the scopes
// only through its parent's re-issue: the dry run computes the child
// against the parent's would-be record. The parent is NOT re-issued
// beforehand.
func TestScopeReissueBulk_T12_DryRunDefault(t *testing.T) {
	f := newReissueFixture(t, "rsb-t12", store.ProjectRoleOwner)
	jti := "rsb-t12-jti"
	insertTestAgentCredential(t, f.store, f.child.ID, f.projectID, jti)
	credBefore := getTestAgentCredential(t, f.store, jti)
	parentEdges, childEdges := f.allEdges(t, f.parent), f.allEdges(t, f.child)

	rec := bulkHTTP(t, f.srv, context.Background(), map[string]bool{"reissue_scopes": true}, f.adminSession(t), CredentialContext{Kind: CredentialKindInteractive})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var dry ScopeReissueBulkResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &dry))
	assert.True(t, dry.DryRun, "dry run is the default")
	assert.True(t, dry.BatchAuditRecorded)
	assert.Equal(t, parentEdges, f.allEdges(t, f.parent), "dry run writes no edge")
	assert.Equal(t, childEdges, f.allEdges(t, f.child), "dry run writes no edge")
	assertCredentialUnrevoked(t, f.store, jti, credBefore)
	for _, id := range []string{f.root.ID, f.parent.ID, f.child.ID} {
		// Per-agent parity with the single-agent dry run.
		recs := reissueAudits(t, f.store, id, mutationTypeAgentScopesReissued)
		require.Len(t, recs, 1, "one dry-run row for %s", id)
		summary := decodeReissueSummary(t, recs[0])
		assert.True(t, summary.DryRun, id)
		assert.Equal(t, dry.BatchOpID, summary.BatchOpID, id)
		assert.Empty(t, summary.EdgeNew, id)
		assert.Equal(t, 0, summary.CredentialsRevoked, id)
		assert.Empty(t, reissueAudits(t, f.store, id, mutationTypeAgentScopesReissueDispatch), id)
		assert.Empty(t, issueDeniedAudits(t, f.store, id))
	}
	childSummary := decodeReissueSummary(t, reissueAudits(t, f.store, f.child.ID, mutationTypeAgentScopesReissued)[0])
	assert.Equal(t, artifactScopeStrings(), reissueSorted(childSummary.ScopesAdded), "the child's dry-run row lists what --apply adds")
	batch := batchAudits(t, f.store)
	require.Len(t, batch, 1)
	assert.Contains(t, batch[0].AfterSummary, `"dry_run":true`)

	// The child's preview already includes the scopes it gains through its
	// parent.
	assert.Equal(t, artifactScopeStrings(), reissueSorted(bulkAgent(t, &dry, f.child.ID).Added), "child previewed against the parent's would-be record")
	assert.Equal(t, "changed", bulkAgent(t, &dry, f.child.ID).Outcome)

	applied := f.bulk(t, false)
	for _, id := range []string{f.root.ID, f.parent.ID, f.child.ID} {
		d, a := bulkAgent(t, &dry, id), bulkAgent(t, applied, id)
		// The dry run came back over JSON, where an empty list is omitted.
		assert.ElementsMatch(t, d.Added, a.Added, id)
		assert.ElementsMatch(t, d.Removed, a.Removed, id)
		assert.Equal(t, d.RoleAfter, a.RoleAfter, id)
		assert.Equal(t, d.Outcome, a.Outcome, id)
	}
}

// A refused parent leaves its child computed against the parent's
// unchanged record, in the dry run as in the applied run.
func TestScopeReissueBulk_DryRunRefusedParent(t *testing.T) {
	f := newReissueFixture(t, "rsb-refp", store.ProjectRoleOwner)
	// The parent's edge becomes unrecorded: its re-issue is refused.
	ctx := context.Background()
	_, err := f.store.DeactivateDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, f.parent.ID, store.Deactivation{
		Cause: store.EdgeDeactivationReincarnateReplaced, OpID: "test-op",
	})
	require.NoError(t, err)
	f.edge(t, store.DelegationPrincipalAgent, f.root.ID, f.parent.ID, store.EffectCeiling{}, store.AuthorityProvenance{})

	dry := f.bulk(t, true)
	applied := f.bulk(t, false)
	assert.Equal(t, "refused", bulkAgent(t, dry, f.parent.ID).Outcome)
	assert.Equal(t, "refused", bulkAgent(t, applied, f.parent.ID).Outcome)
	d, a := bulkAgent(t, dry, f.child.ID), bulkAgent(t, applied, f.child.ID)
	assert.Equal(t, a.Outcome, d.Outcome)
	assert.ElementsMatch(t, a.Added, d.Added)
	assert.ElementsMatch(t, a.Removed, d.Removed)
}

// Agents whose delegation record cannot be read for ordering are reported.
func TestScopeReissueBulk_DepthUnresolvedReported(t *testing.T) {
	f := newReissueFixture(t, "rsb-depth", store.ProjectRoleOwner)
	f.faults.dupEdgeAgentID = f.child.ID
	f.faults.arm()
	resp := f.bulk(t, true)
	assert.Equal(t, []string{f.child.ID}, resp.DepthUnresolved)
	assert.Equal(t, "refused", bulkAgent(t, resp, f.child.ID).Outcome)
}

// When the batch row cannot be written the response says so; the
// per-agent rows stand.
func TestScopeReissueBulk_BatchAuditFailureReported(t *testing.T) {
	f := newReissueFixture(t, "rsb-batchaudit", store.ProjectRoleOwner)
	f.faults.batchAuditFail = true
	f.faults.arm()
	resp := f.bulk(t, false)
	assert.False(t, resp.BatchAuditRecorded)
	assert.Empty(t, batchAudits(t, f.store))
	require.Len(t, reissueAudits(t, f.store, f.child.ID, mutationTypeAgentScopesReissued), 1)
}

// A client that goes away mid-run does not cancel the remaining agents or
// lose the batch row.
func TestScopeReissueBulk_ClientCancelDoesNotStopRun(t *testing.T) {
	f := newReissueFixture(t, "rsb-cancel", store.ProjectRoleOwner)
	for i := 0; i < 3; i++ {
		f.childAgent(t, "rsb-cancel-extra-"+itoa(i), f.parent, AgentRoleFull)
	}
	reqCtx, cancelReq := context.WithCancel(context.Background())
	defer cancelReq()
	var calls atomic.Int32
	prev := reissueBulkAgentHook
	reissueBulkAgentHook = func(string) {
		if calls.Add(1) == 1 {
			cancelReq() // the client disconnects as the first agent starts
		}
	}
	t.Cleanup(func() { reissueBulkAgentHook = prev })

	rec := bulkHTTP(t, f.srv, reqCtx, map[string]bool{"reissue_scopes": true, "dry_run": false}, f.adminSession(t), CredentialContext{Kind: CredentialKindInteractive})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp ScopeReissueBulkResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, int32(6), calls.Load(), "every agent had its turn")
	assert.Empty(t, resp.Refused, "no agent failed because the client went away: %+v", resp.Refused)
	assert.Len(t, resp.Succeeded, 5, "parent, child and three extras changed")
	assert.True(t, resp.BatchAuditRecorded)
	require.Len(t, batchAudits(t, f.store), 1, "the batch row is written")
	assert.Contains(t, scopeStrings(f.grant(t, f.child)), string(ScopeProjectArtifactWrite))
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
	hubAdminID := tid("rsb-op-hub-admin")
	require.NoError(t, f.store.CreateUser(context.Background(), &store.User{
		ID: hubAdminID, Email: "hub-admin@test.com", DisplayName: "Hub Admin", Role: "member", Status: "active",
	}))
	grantSystemRole(t, f.store, hubAdminID, store.SystemRoleHubAdmin)
	require.False(t, f.srv.authzService.IsSystemAdmin(context.Background(), hubAdminID))
	hubAdmin := NewAuthenticatedUser(hubAdminID, "hub-admin@test.com", "Hub Admin", "member", "")
	adminUser := f.adminSession(t)
	uat := NewScopedUserIdentityWithCeiling(adminUser, f.projectID, []string{"project:agent:manage"}, "uat-rsb-op",
		permissions.FrozenPermissionCeiling{Version: permissions.CeilingVersionV1, PermissionIDs: allRegistryIDs()})
	claims := &AgentTokenClaims{ProjectID: f.projectID, Scopes: ScopesForRole(AgentRoleFull)}
	claims.Subject = f.child.ID
	for name, tc := range map[string]struct {
		identity Identity
		cred     CredentialContext
	}{
		"member session":                        {member, CredentialContext{Kind: CredentialKindInteractive}},
		"agent token":                           {&agentIdentityWrapper{claims}, CredentialContext{Kind: CredentialKindAgentJWT}},
		"hub-admin session without super-admin": {hubAdmin, CredentialContext{Kind: CredentialKindInteractive}},
		"super-admin user access token":         {uat, credentialContextForIdentity(uat)},
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
	for _, id := range []string{f.root.ID, f.parent.ID, f.child.ID} {
		assert.Empty(t, reissueAudits(t, f.store, id, mutationTypeAgentScopesReissued), "no per-agent row for %s", id)
		assert.Empty(t, reissueAudits(t, f.store, id, mutationTypeAgentScopesReissueDispatch), "no dispatch row for %s", id)
		assert.Empty(t, issueDeniedAudits(t, f.store, id), "no denial row for %s", id)
	}
}
