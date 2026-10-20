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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Agent uploads of attachments stored as artifacts (ptone/scion#4596,
// design section 6.1).

// agentToken mints an agent token for a with the full agent role.
func (f *refFixture) agentToken(t *testing.T, a *store.Agent) string {
	t.Helper()
	token, err := f.srv.GenerateAgentToken(a.ID, a.ProjectID, a.Ancestry, AgentRoleFull, nil)
	require.NoError(t, err)
	return token
}

// agentRequest serves req through the hub's full handler with agent a's
// token.
func (f *refFixture) agentRequest(t *testing.T, a *store.Agent, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	req.Header.Set("Authorization", "Bearer "+f.agentToken(t, a))
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// agentUpload uploads one file as agent a through the full handler and
// returns the answer.
func (f *refFixture) agentUpload(t *testing.T, a *store.Agent, name, content string) *httptest.ResponseRecorder {
	t.Helper()
	return f.agentRequest(t, a, attachmentUploadRequest(t, f.projB.ID, name, content))
}

// directAttachmentRoute serves the upload collection route (routeGuard,
// chatAttachmentRoute and the handler) with ctx as the request context,
// as an internal caller would set it.
func (f *refFixture) directAttachmentRoute(t *testing.T, ctx context.Context, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h := f.srv.guarded(chatAttachmentsPath, f.srv.chatAttachmentRoute(f.srv.handleChatAttachments))
	h(rec, req.WithContext(ctx))
	return rec
}

// TestAgentUpload_CreatesAgentUploadScope: an agent's upload is stored in
// its own upload scope, owned by the agent; the project_id form value
// (here another project) is ignored.
func TestAgentUpload_CreatesAgentUploadScope(t *testing.T) {
	f := newRefFixture(t)
	st := enableAttachmentArtifacts(t, f)
	rec := f.agentUpload(t, f.aa, "report.txt", "agent bytes")
	id := uploadedAttachmentID(t, rec, "report.txt")
	a, err := st.GetAttachment(context.Background(), id)
	require.NoError(t, err)
	require.Equal(t, artifacts.ScopeKindUpload, a.ScopeKind)
	require.Equal(t, artifacts.PrincipalRef(artifacts.PrincipalKindAgent, f.aa.ID), a.ScopeRef)
	require.Equal(t, artifacts.PrincipalKindAgent, a.OwnerKind)
	require.Equal(t, f.aa.ID, a.OwnerRef)
	require.Nil(t, a.MessageProjectID, "an upload has no project until it is bound")
}

// TestAgentUpload_RefusedWhenAgentMaySendFalse (K1): an agent that may not
// send gets 403 and nothing is stored.
func TestAgentUpload_RefusedWhenAgentMaySendFalse(t *testing.T) {
	f := newRefFixture(t)
	st := enableAttachmentArtifacts(t, f)
	held := &store.Agent{ID: uuid.NewString(), ProjectID: f.projA.ID, Name: "held", Slug: "held",
		Phase: "running", OwnerID: f.ua.ID, CreatedBy: f.ua.ID}
	require.NoError(t, f.st.CreateAgent(context.Background(), held))
	holdAgentFor(t, f.st, held.ID, f.projA.ID, f.ua.ID)
	_, ok := f.srv.agentMaySend(storedAgentContext(held))
	require.False(t, ok)
	rec := f.directAttachmentRoute(t, storedAgentContext(held), attachmentUploadRequest(t, "", "x.txt", "x"))
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	requireNoUploads(t, st, artifacts.PrincipalKindAgent, held.ID)
}

// requireNoUploads asserts that principal (kind, ref) has no upload.
func requireNoUploads(t *testing.T, st artifacts.Store, kind, ref string) {
	t.Helper()
	refs, err := st.ListLiveScopeRefs(context.Background(), artifacts.ScopeKindUpload, "", 1000)
	require.NoError(t, err)
	require.NotContains(t, refs, artifacts.PrincipalRef(kind, ref), "an upload was stored")
}

// TestAgentUpload_ExperimentOff_Distinct404: with the experiment off an
// agent upload answers 404 with code attachment_artifacts_unavailable.
func TestAgentUpload_ExperimentOff_Distinct404(t *testing.T) {
	f := newRefFixture(t)
	enableAttachmentArtifacts(t, f)
	setAttachmentExperiments(t, f.srv, false, false)
	rec := f.agentUpload(t, f.aa, "report.txt", "agent bytes")
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), codeAttachmentArtifactsUnavailable)
}

// TestAgentUpload_DelegatedCredentialRefused: a delegated agent credential
// is refused on the upload route.
func TestAgentUpload_DelegatedCredentialRefused(t *testing.T) {
	f := newRefFixture(t)
	st := enableAttachmentArtifacts(t, f)
	id := adtTestIdentity(f.aa.ID, f.projA.ID)
	ctx := contextWithCredentialContext(contextWithIdentity(context.Background(), id), credentialContextForIdentity(id))
	rec := f.directAttachmentRoute(t, ctx, attachmentUploadRequest(t, "", "x.txt", "x"))
	require.GreaterOrEqual(t, rec.Code, 400, rec.Body.String())
	require.NotEqual(t, http.StatusCreated, rec.Code)
	_, ok := f.srv.agentMaySend(ctx)
	require.False(t, ok)
	requireNoUploads(t, st, artifacts.PrincipalKindAgent, f.aa.ID)
}

// TestAgentUpload_NotInStandingRefused (K10): with a valid token, an agent
// whose upward chain is held and an agent whose root user is no longer
// admitted get 403; a held agent (whose own token the hub refuses at
// authentication) is refused by agentMaySend too.
func TestAgentUpload_NotInStandingRefused(t *testing.T) {
	t.Run("upward chain held", func(t *testing.T) {
		f := newRefFixture(t)
		enableAttachmentArtifacts(t, f)
		child := &store.Agent{ID: uuid.NewString(), ProjectID: f.projA.ID, Name: "child", Slug: "child",
			Phase: "running", OwnerID: f.ua.ID, CreatedBy: f.aa.ID, Ancestry: []string{f.ua.ID, f.aa.ID}}
		require.NoError(t, f.st.CreateAgent(context.Background(), child))
		delegate(t, f.st, store.DelegationPrincipalUser, f.ua.ID, f.aa)
		delegate(t, f.st, store.DelegationPrincipalAgent, f.aa.ID, child)
		require.Equal(t, http.StatusCreated, f.agentUpload(t, child, "ok.txt", "before").Code, "precondition: the child uploads")
		holdAgentFor(t, f.st, f.aa.ID, f.projA.ID, f.ua.ID)
		requireStandingReason(t, f.srv.agentStanding(context.Background(), child.ID), standingReasonChainHeld)
		rec := f.agentUpload(t, child, "after.txt", "after")
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	})
	t.Run("root user not admitted", func(t *testing.T) {
		f := newRefFixture(t)
		enableAttachmentArtifacts(t, f)
		require.Equal(t, http.StatusCreated, f.agentUpload(t, f.aa, "ok.txt", "before").Code, "precondition: aa uploads")
		f.revokeProjectAccess(t, f.ua, f.projA)
		rec := f.agentUpload(t, f.aa, "after.txt", "after")
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	})
	t.Run("held agent", func(t *testing.T) {
		f := newRefFixture(t)
		enableAttachmentArtifacts(t, f)
		holdAgentFor(t, f.st, f.aa.ID, f.projA.ID, f.ua.ID)
		rec := f.directAttachmentRoute(t, storedAgentContext(f.aa), attachmentUploadRequest(t, "", "x.txt", "x"))
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	})
}

// TestAgentUpload_ReadableOnlyByUploader: the agent's own user and another
// user get the unknown-id answer for the agent's unbound upload.
func TestAgentUpload_ReadableOnlyByUploader(t *testing.T) {
	f := newRefFixture(t)
	enableAttachmentArtifacts(t, f)
	id := uploadedAttachmentID(t, f.agentUpload(t, f.aa, "report.txt", "agent bytes"), "report.txt")
	missing := f.download(t, f.ua, uuid.NewString())
	require.Equal(t, http.StatusNotFound, missing.status)
	requireSameAnswer(t, missing, f.download(t, f.ua, id))
	requireSameAnswer(t, missing, f.download(t, f.ub, id))
}

// TestAgentUpload_NoLegacyStoreNeeded: with no AttachmentStore configured,
// an agent upload still succeeds.
func TestAgentUpload_NoLegacyStoreNeeded(t *testing.T) {
	f := newRefFixture(t)
	enableAttachmentArtifacts(t, f)
	f.srv.SetAttachmentStore(nil)
	uploadedAttachmentID(t, f.agentUpload(t, f.aa, "report.txt", "agent bytes"), "report.txt")
}

// TestAgentByID_Refused (K16): an agent credential on the by-id route is
// refused by the route wrapper, for artifact and legacy ids alike.
func TestAgentByID_Refused(t *testing.T) {
	f := newRefFixture(t)
	enableAttachmentArtifacts(t, f)
	artifactID := uploadedAttachmentID(t, f.agentUpload(t, f.aa, "own.txt", "agent bytes"), "own.txt")
	legacy := f.attach(t, f.projA.ID, f.aa.ID, "legacy.txt")
	for _, id := range []string{artifactID, legacy} {
		rec := f.agentRequest(t, f.aa, httptest.NewRequest(http.MethodGet, "/api/v1/chat/attachments/"+id, nil))
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		require.NotContains(t, rec.Body.String(), "agent bytes")

		// The wrapper itself refuses: the handler behind it is never
		// reached for an agent on the by-id route.
		reached := false
		h := f.srv.chatAttachmentRoute(func(http.ResponseWriter, *http.Request) { reached = true })
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/chat/attachments/"+id, nil)
		h(w, req.WithContext(storedAgentContext(f.aa)))
		require.False(t, reached, "chatAttachmentRoute hands an agent on the by-id route to its handler")
		require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	}
}

// probe asks the capability probe through the full handler.
func probeAnswer(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got attachmentUploadProbe
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	return got.Uploads
}

// TestAttachmentProbe_AnswersPerCredential: sessions and agents that may
// send get "artifact" while the experiment is on; with it off, "legacy".
// The probe uploads nothing.
func TestAttachmentProbe_AnswersPerCredential(t *testing.T) {
	f := newRefFixture(t)
	st := enableAttachmentArtifacts(t, f)
	session := doRequestAsUser(t, f.srv, f.ua, http.MethodGet, chatAttachmentsPath, nil)
	require.Equal(t, "artifact", probeAnswer(t, session))
	require.Equal(t, "artifact", probeAnswer(t, f.agentRequest(t, f.aa, httptest.NewRequest(http.MethodGet, chatAttachmentsPath, nil))))
	requireNoUploads(t, st, artifacts.PrincipalKindAgent, f.aa.ID)

	setAttachmentExperiments(t, f.srv, false, false)
	require.Equal(t, "legacy", probeAnswer(t, doRequestAsUser(t, f.srv, f.ua, http.MethodGet, chatAttachmentsPath, nil)))
	require.Equal(t, "legacy", probeAnswer(t, f.agentRequest(t, f.aa, httptest.NewRequest(http.MethodGet, chatAttachmentsPath, nil))))
}

// delegate records an active delegation edge from (delegatorType,
// delegatorID) to agent a, as the hub records it when it creates a.
func delegate(t *testing.T, s store.Store, delegatorType, delegatorID string, a *store.Agent) {
	t.Helper()
	require.NoError(t, s.CreateDelegationEdge(context.Background(), &store.DelegationEdge{
		DelegatorType: delegatorType, DelegatorID: delegatorID,
		DelegateType: store.DelegationPrincipalAgent, DelegateID: a.ID,
		ScopeType: store.RoleScopeProject, ScopeID: a.ProjectID,
		Role: string(AgentRoleBaseline), Active: true,
		AuthorityProvenance: store.AuthorityProvenance{
			ProvenanceVersion:   store.ProvenanceVersionV1,
			SourcePrincipalKind: delegatorType, SourcePrincipalID: delegatorID,
			SourceCredentialKind: store.SourceCredentialSession,
		},
		EffectCeiling: store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
	}))
}

// TestAttachmentProbe_AnyTokenReads: the probe describes only the caller's
// own upload capability, so any user access token reads it, whatever its
// selectors or boundary. A token uploads to the artifact service
// (ptone/scion#4596, section 9), so it gets "artifact"; the upload and
// the send keep their token checks.
func TestAttachmentProbe_AnyTokenReads(t *testing.T) {
	f := newRefFixture(t)
	enableAttachmentArtifacts(t, f)
	for _, token := range []*ScopedUserIdentity{
		selfToken(t, f.ua.ID, hubBoundary(), "project:read"),
		selfToken(t, f.ua.ID, projectBoundary(f.projB.ID), "agent:read"),
	} {
		req := httptest.NewRequest(http.MethodGet, chatAttachmentsPath, nil)
		rec := httptest.NewRecorder()
		f.srv.chatAttachmentRoute(f.srv.handleChatAttachments)(rec, req.WithContext(contextWithIdentity(req.Context(), token)))
		require.Equal(t, "artifact", probeAnswer(t, rec))
	}
}
