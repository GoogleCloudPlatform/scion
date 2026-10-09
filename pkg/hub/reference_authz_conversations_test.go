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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// disableWriteDenySwitch turns the conversation envelope switch off on srv.
func disableWriteDenySwitch(t *testing.T, srv *Server) {
	t.Helper()
	fakeStore := newFakeHubSettingStore()
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	fakeStore.seed("messaging", json.RawMessage(`{"conversation_envelope_switch":false}`))
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)
	srv.SetOperationalSettings(ops)
	require.False(t, srv.writeDenyEnabled(), "write-deny must be off")
}

// externalRefFixture is the routed inbound fixture plus a conversation that
// a chat thread reference already names in another project.
type externalRefFixture struct {
	routedTestEnv
	otherProject *store.Project
	otherConv    *store.Conversation
	surface      string
	ref          string
}

func newExternalRefFixture(t *testing.T) externalRefFixture {
	t.Helper()
	env := setupRoutedTestEnv(t)
	ctx := context.Background()

	other := &store.Project{
		ID:        tid("proj-extref-other"),
		Slug:      "extref-other",
		Name:      "External Ref Other Project",
		OwnerID:   env.user.ID,
		CreatedBy: env.user.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, env.store.CreateProject(ctx, other))

	const surface, ref = "slack", "C0EXTREF:1700000000.000100"
	otherProjectID := other.ID
	conv, err := env.store.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "group",
		Surface:     surface,
		ExternalRef: ref,
		ParentRef:   "C0EXTREF",
		DisplayName: "other project thread",
		DriftState:  "active",
		ProjectID:   &otherProjectID,
	})
	require.NoError(t, err)
	// Read back so later comparisons use the stored values.
	conv, err = env.store.GetConversation(ctx, conv.ID)
	require.NoError(t, err)

	return externalRefFixture{routedTestEnv: env, otherProject: other, otherConv: conv, surface: surface, ref: ref}
}

// postLegacyInbound posts to /api/v1/broker/inbound for agent alpha.
func (f externalRefFixture) postLegacyInbound(t *testing.T, surface, ref string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(inboundMessageRequest{
		Topic: "scion.project." + f.project.ID + ".agent." + f.agent1.Slug + ".messages",
		Message: &messages.StructuredMessage{
			Version:   messages.Version,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Channel:   "slack",
			Sender:    "user:" + f.user.Email,
			Recipient: "agent:" + f.agent1.Slug,
			Msg:       "hello from the thread",
			Type:      messages.TypeInstruction,
		},
		Surface:     surface,
		ExternalRef: ref,
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/broker/inbound", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity("test-broker")))
	rec := httptest.NewRecorder()
	f.srv.mux.ServeHTTP(rec, req)
	return rec
}

// postRoutedInbound posts to /api/v1/broker/inbound/routed for agent alpha.
func (f externalRefFixture) postRoutedInbound(t *testing.T, surface, ref string) *httptest.ResponseRecorder {
	t.Helper()
	return f.doRoutedRequest(t, routedInboundRequest{
		ProjectID:    f.project.ID,
		DefaultAgent: f.agent1.Slug,
		Surface:      surface,
		ExternalRef:  ref,
		Message: &messages.StructuredMessage{
			Version: messages.Version,
			Channel: "slack",
			Sender:  "user:" + f.user.Email,
			Msg:     "hello from the thread",
			Type:    messages.TypeInstruction,
		},
	})
}

// routedBodyWithoutMessageIDs decodes a routed response and blanks the
// per-request message IDs so two answers can be compared.
func routedBodyWithoutMessageIDs(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "body: %s", rec.Body.String())
	if results, ok := resp.Error.Details["results"].([]interface{}); ok {
		for _, r := range results {
			if m, ok := r.(map[string]interface{}); ok {
				if _, has := m["message_id"]; has {
					m["message_id"] = "<message-id>"
				}
			}
		}
	}
	out, err := json.Marshal(resp)
	require.NoError(t, err)
	return string(out)
}

// requireNothingDelivered asserts that no message reached agent alpha and the
// other project's conversation is unchanged.
func (f externalRefFixture) requireNothingDelivered(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	assert.Empty(t, f.dispatcher.getCalls(), "nothing dispatched")
	msgs, err := f.store.ListMessages(ctx, store.MessageFilter{AgentID: f.agent1.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	assert.Empty(t, msgs.Items, "no message stored")
	got, err := f.store.GetConversation(ctx, f.otherConv.ID)
	require.NoError(t, err)
	require.NotNil(t, got.ProjectID)
	assert.Equal(t, f.otherProject.ID, *got.ProjectID, "conversation keeps its project")
	assert.Nil(t, got.DefaultAgentID, "conversation default agent unchanged")
	assert.True(t, f.otherConv.LastActivityAt.Equal(got.LastActivityAt), "conversation not touched")
}

// TestBrokerInbound_ExternalRefOfOtherProjectNotReused: a chat thread
// reference that already names a conversation of another project is not
// reused. Both inbound endpoints answer exactly as for any other conversation
// resolution failure, and nothing is delivered or stored.
func TestBrokerInbound_ExternalRefOfOtherProjectNotReused(t *testing.T) {
	t.Run("inbound", func(t *testing.T) {
		f := newExternalRefFixture(t)
		unresolved := f.postLegacyInbound(t, f.surface, "thread:bad")
		require.Equal(t, http.StatusConflict, unresolved.Code, "baseline body: %s", unresolved.Body.String())

		rec := f.postLegacyInbound(t, f.surface, f.ref)
		require.Equal(t, http.StatusConflict, rec.Code, "body: %s", rec.Body.String())
		assert.Equal(t, unresolved.Body.String(), rec.Body.String(), "same answer as an unresolved conversation")
		f.requireNothingDelivered(t)
	})

	t.Run("routed", func(t *testing.T) {
		f := newExternalRefFixture(t)
		unresolved := f.postRoutedInbound(t, f.surface, "thread:bad")
		require.Equal(t, http.StatusConflict, unresolved.Code, "baseline body: %s", unresolved.Body.String())

		rec := f.postRoutedInbound(t, f.surface, f.ref)
		require.Equal(t, http.StatusConflict, rec.Code, "body: %s", rec.Body.String())
		assert.Contains(t, rec.Body.String(), `"status":"conversation_not_resolved"`)
		assert.Equal(t, routedBodyWithoutMessageIDs(t, unresolved), routedBodyWithoutMessageIDs(t, rec),
			"same answer as an unresolved conversation")
		f.requireNothingDelivered(t)
	})
}

// TestBrokerInbound_ExternalRefMismatchRefusedWithWriteDenyOff: with the
// conversation envelope switch off, other resolution failures continue
// without a conversation, but a reference of another project is still
// refused with the same answer.
func TestBrokerInbound_ExternalRefMismatchRefusedWithWriteDenyOff(t *testing.T) {
	t.Run("inbound", func(t *testing.T) {
		f := newExternalRefFixture(t)
		unresolved := f.postLegacyInbound(t, f.surface, "thread:bad")
		require.Equal(t, http.StatusConflict, unresolved.Code)

		disableWriteDenySwitch(t, f.srv)
		rec := f.postLegacyInbound(t, f.surface, f.ref)
		require.Equal(t, http.StatusConflict, rec.Code, "body: %s", rec.Body.String())
		assert.Equal(t, unresolved.Body.String(), rec.Body.String())
		f.requireNothingDelivered(t)
	})

	t.Run("routed", func(t *testing.T) {
		f := newExternalRefFixture(t)
		unresolved := f.postRoutedInbound(t, f.surface, "thread:bad")
		require.Equal(t, http.StatusConflict, unresolved.Code)

		disableWriteDenySwitch(t, f.srv)
		rec := f.postRoutedInbound(t, f.surface, f.ref)
		require.Equal(t, http.StatusConflict, rec.Code, "body: %s", rec.Body.String())
		assert.True(t, strings.Contains(rec.Body.String(), `"status":"conversation_not_resolved"`), "body: %s", rec.Body.String())
		assert.Equal(t, routedBodyWithoutMessageIDs(t, unresolved), routedBodyWithoutMessageIDs(t, rec))
		f.requireNothingDelivered(t)
	})
}

// postAgentMessageAs posts req to /api/v1/agents/{target}/message with the
// given request context set up by withCtx.
func postAgentMessageAs(t *testing.T, srv *Server, withCtx func(context.Context) context.Context, target *store.Agent, req MessageRequest) refAnswer {
	t.Helper()
	body, err := json.Marshal(req)
	require.NoError(t, err)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+target.ID+"/message", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(withCtx(r.Context()))
	rr := httptest.NewRecorder()
	srv.handleAgentMessage(rr, r, target.ID)
	return refAnswer{status: rr.Code, body: rr.Body.String()}
}

func externalRefMessage(target *store.Agent, surface, ref, parent string) MessageRequest {
	return MessageRequest{
		StructuredMessage: &messages.StructuredMessage{
			Version: messages.Version, Timestamp: time.Now().UTC().Format(time.RFC3339),
			Recipient: "agent:" + target.Slug, RecipientID: target.ID,
			Msg: "from a thread", Type: messages.TypeInstruction,
		},
		Surface:     surface,
		ExternalRef: ref,
		ParentRef:   parent,
	}
}

const chatIntegrationsOnly = "surface and external_ref are set by chat integrations"

// TestAgentMessage_ExternalRefFromNonIntegrationCallerRefused: on
// /agents/{id}/message only chat integrations (broker-authenticated callers)
// may name surface, external_ref or parent_ref. Users and agents get 400 and
// no conversation is created.
func TestAgentMessage_ExternalRefFromNonIntegrationCallerRefused(t *testing.T) {
	f := newRefFixture(t)
	f.srv.SetDispatcher(&recordingDispatcher{})
	ctx := context.Background()
	alice := NewAuthenticatedUser(f.ua.ID, f.ua.Email, f.ua.DisplayName, f.ua.Role, string(ClientTypeWeb))
	agentA := agentIdentityFor(tid("ref-agent-a-sender"), f.projA.ID)
	asIdentity := func(id Identity) func(context.Context) context.Context {
		return func(c context.Context) context.Context { return contextWithIdentity(c, id) }
	}

	cases := []struct {
		name                 string
		caller               Identity
		surface, ref, parent string
	}{
		{"user surface and external_ref", alice, "slack", "C0USER:1.1", ""},
		{"agent surface and external_ref", agentA, "slack", "C0AGENT:1.1", ""},
		{"user parent_ref alone", alice, "", "", "C0PARENT"},
		{"agent parent_ref alone", agentA, "", "", "C0PARENT"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := postAgentMessageAs(t, f.srv, asIdentity(tc.caller), f.aa, externalRefMessage(f.aa, tc.surface, tc.ref, tc.parent))
			require.Equal(t, http.StatusBadRequest, got.status, got.body)
			assert.Contains(t, got.body, chatIntegrationsOnly)
			if tc.ref != "" {
				_, err := f.st.GetConversationByExternalRef(ctx, tc.surface, tc.ref)
				assert.ErrorIs(t, err, store.ErrNotFound, "no conversation is created")
			}
		})
	}

	t.Run("chat integration acting for a user is accepted", func(t *testing.T) {
		const ref = "C0BROKER:1.1"
		withBroker := func(c context.Context) context.Context {
			return contextWithIdentity(contextWithBrokerIdentity(c, NewBrokerIdentity("test-broker")), alice)
		}
		got := postAgentMessageAs(t, f.srv, withBroker, f.aa, externalRefMessage(f.aa, "slack", ref, "C0BROKER"))
		require.NotContains(t, got.body, chatIntegrationsOnly)
		conv, err := f.st.GetConversationByExternalRef(ctx, "slack", ref)
		require.NoError(t, err, "the chat integration's reference resolves a conversation (answer: %d %s)", got.status, got.body)
		require.NotNil(t, conv.ProjectID)
		assert.Equal(t, f.projA.ID, *conv.ProjectID)
	})
}

// TestAgentMessage_NonIntegrationCallerCannotMoveProjectlessConversation: a
// user naming the reference of a conversation that has no project is refused
// and the conversation is left exactly as it was.
func TestAgentMessage_NonIntegrationCallerCannotMoveProjectlessConversation(t *testing.T) {
	f := newRefFixture(t)
	f.srv.SetDispatcher(&recordingDispatcher{})
	ctx := context.Background()

	const surface, ref = "slack", "C0LEGACY:1.1"
	created, err := f.st.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind: "group", Surface: surface, ExternalRef: ref, DisplayName: "legacy thread", DriftState: "active",
	})
	require.NoError(t, err)
	before, err := f.st.GetConversation(ctx, created.ID)
	require.NoError(t, err)
	require.Nil(t, before.ProjectID)

	alice := NewAuthenticatedUser(f.ua.ID, f.ua.Email, f.ua.DisplayName, f.ua.Role, string(ClientTypeWeb))
	got := postAgentMessageAs(t, f.srv, func(c context.Context) context.Context { return contextWithIdentity(c, alice) },
		f.aa, externalRefMessage(f.aa, surface, ref, "C0LEGACY"))
	require.Equal(t, http.StatusBadRequest, got.status, got.body)
	assert.Contains(t, got.body, chatIntegrationsOnly)

	after, err := f.st.GetConversation(ctx, created.ID)
	require.NoError(t, err)
	assert.Nil(t, after.ProjectID, "the conversation stays without a project")
	assert.Nil(t, after.DefaultAgentID, "default agent unchanged")
	assert.Equal(t, before.ParentRef, after.ParentRef, "parent_ref unchanged")
	assert.True(t, before.LastActivityAt.Equal(after.LastActivityAt), "conversation not touched")
}

func addUserParticipant(t *testing.T, s store.Store, convID, userID string) {
	t.Helper()
	require.NoError(t, s.EnsureParticipant(context.Background(), &store.ConversationParticipant{
		ConversationID: convID, PrincipalKind: "user", PrincipalID: userID, Role: "member",
	}))
}

// seedAgentDM creates the direct conversation between agent and user, with
// both as participants, and returns its ID.
func seedAgentDM(t *testing.T, s store.Store, agent *store.Agent, user *store.User) string {
	t.Helper()
	key, err := messages.DMConversationKey("agent", agent.ID, "user", user.ID)
	require.NoError(t, err)
	conv, err := s.UpsertConversationByExternalRef(context.Background(), &store.Conversation{
		Kind: "direct", Surface: "native", ExternalRef: key, DriftState: "active",
	})
	require.NoError(t, err)
	addUserParticipant(t, s, conv.ID, user.ID)
	require.NoError(t, s.EnsureParticipant(context.Background(), &store.ConversationParticipant{
		ConversationID: conv.ID, PrincipalKind: "agent", PrincipalID: agent.ID, Role: "member",
	}))
	return conv.ID
}

// TestConversationList_OmitsGroupsOfUnreadableProject: once a user can no
// longer read a project, its group conversations leave their list even
// though their participant rows remain; their direct conversation with the
// project's agent stays listed.
func TestConversationList_OmitsGroupsOfUnreadableProject(t *testing.T) {
	f := newRefFixture(t)

	groupA := seedGroupConversation(t, f.st, f.projA.ID, "list-general")
	addUserParticipant(t, f.st, groupA, f.ua.ID)
	dm := seedAgentDM(t, f.st, f.aa, f.ua)

	before := listConversationIDsAsUser(t, f.srv, f.ua)
	require.Contains(t, before, groupA, "listed while the user reads the project")
	require.Contains(t, before, dm)

	f.revokeProjectAccess(t, f.ua, f.projA)

	after := listConversationIDsAsUser(t, f.srv, f.ua)
	assert.NotContains(t, after, groupA, "the group leaves the list")
	assert.Contains(t, after, dm, "the direct conversation stays listed")
}

// projectLookupFaultStore fails GetProject for one project ID.
type projectLookupFaultStore struct {
	store.Store
	failProjectID string
}

func (p *projectLookupFaultStore) GetProject(ctx context.Context, id string) (*store.Project, error) {
	if id == p.failProjectID {
		return nil, errRefStoreFault
	}
	return p.Store.GetProject(ctx, id)
}

// TestConversationList_GroupReadLookupErrorOmitsRow: when the read check for
// one project fails, that project's groups are omitted and the rest of the
// list is still returned.
func TestConversationList_GroupReadLookupErrorOmitsRow(t *testing.T) {
	f := newRefFixture(t)
	ctx := context.Background()

	projC := &store.Project{
		ID: tid("ref-project-c"), Name: "Project C", Slug: "project-c",
		OwnerID: f.ua.ID, CreatedBy: f.ua.ID, Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, f.st.CreateProject(ctx, projC))
	f.srv.seedProjectCreatorMembership(ctx, projC)

	groupA1 := seedGroupConversation(t, f.st, f.projA.ID, "fault-one")
	groupA2 := seedGroupConversation(t, f.st, f.projA.ID, "fault-two")
	groupC := seedGroupConversation(t, f.st, projC.ID, "fault-other")
	for _, id := range []string{groupA1, groupA2, groupC} {
		addUserParticipant(t, f.st, id, f.ua.ID)
	}
	require.Subset(t, listConversationIDsAsUser(t, f.srv, f.ua), []string{groupA1, groupA2, groupC},
		"all listed while the lookups work")

	f.srv.store = &projectLookupFaultStore{Store: f.srv.store, failProjectID: f.projA.ID}
	got := listConversationIDsAsUser(t, f.srv, f.ua)
	assert.NotContains(t, got, groupA1)
	assert.NotContains(t, got, groupA2)
	assert.Contains(t, got, groupC, "groups of other projects are still listed")
}

// addParticipantAnswer posts an add-participant request as user.
func (f *refFixture) addParticipantAnswer(t *testing.T, user *store.User, convID, kind, principalID string) refAnswer {
	t.Helper()
	rec := doRequestAsUser(t, f.srv, user, http.MethodPost, "/api/v1/conversations/"+convID+"/participants",
		map[string]string{"principalKind": kind, "principalId": principalID})
	return refAnswer{status: rec.Code, body: rec.Body.String()}
}

// agentOfB creates an agent in project B.
func (f *refFixture) agentOfB(t *testing.T) *store.Agent {
	t.Helper()
	bb := &store.Agent{ID: tid("ref-agent-b"), ProjectID: f.projB.ID, Name: "bb", Slug: "bb",
		Phase: "running", OwnerID: f.ub.ID, CreatedBy: f.ub.ID}
	require.NoError(t, f.st.CreateAgent(context.Background(), bb))
	return bb
}

// TestConversationAddParticipant_NonParticipantMatchesUnknownConversation:
// a caller who is not a participant gets exactly the answer for an unknown
// conversation ID.
func TestConversationAddParticipant_NonParticipantMatchesUnknownConversation(t *testing.T) {
	f := newRefFixture(t)
	groupA := seedGroupConversation(t, f.st, f.projA.ID, "add-non-participant")
	before := participantCount(t, f.st, groupA)

	got := f.addParticipantAnswer(t, f.ub, groupA, "user", f.uc.ID)
	unknown := f.addParticipantAnswer(t, f.ub, tid("add-unknown-conversation"), "user", f.uc.ID)
	require.Equal(t, http.StatusNotFound, unknown.status, unknown.body)
	requireSameAnswer(t, unknown, got)
	assert.Equal(t, before, participantCount(t, f.st, groupA), "no participant row is written")
}

// TestConversationAddParticipant_UnreadableGroupMatchesUnknownConversation:
// a participant who cannot read the group's project gets exactly the answer
// for an unknown conversation ID.
func TestConversationAddParticipant_UnreadableGroupMatchesUnknownConversation(t *testing.T) {
	f := newRefFixture(t)
	groupA := seedGroupConversation(t, f.st, f.projA.ID, "add-unreadable")
	addUserParticipant(t, f.st, groupA, f.ub.ID)
	before := participantCount(t, f.st, groupA)

	got := f.addParticipantAnswer(t, f.ub, groupA, "user", f.uc.ID)
	unknown := f.addParticipantAnswer(t, f.ub, tid("add-unknown-conversation"), "user", f.uc.ID)
	require.Equal(t, http.StatusNotFound, unknown.status, unknown.body)
	requireSameAnswer(t, unknown, got)
	assert.Equal(t, before, participantCount(t, f.st, groupA), "no participant row is written")
}

// TestConversationAddParticipant_AgentOfOtherProjectMatchesUnknownAgent: an
// agent of another project gets exactly the answer for an unknown agent ID,
// and no participant row is written.
func TestConversationAddParticipant_AgentOfOtherProjectMatchesUnknownAgent(t *testing.T) {
	f := newRefFixture(t)
	bb := f.agentOfB(t)
	groupA := seedGroupConversation(t, f.st, f.projA.ID, "add-other-agent")
	addUserParticipant(t, f.st, groupA, f.ua.ID)
	before := participantCount(t, f.st, groupA)

	got := f.addParticipantAnswer(t, f.ua, groupA, "agent", bb.ID)
	unknown := f.addParticipantAnswer(t, f.ua, groupA, "agent", tid("add-unknown-agent"))
	require.Equal(t, http.StatusNotFound, unknown.status, unknown.body)
	requireSameAnswer(t, unknown, got)
	assert.Equal(t, before, participantCount(t, f.st, groupA), "no participant row is written")

	// The project's own agent is still added.
	ok := f.addParticipantAnswer(t, f.ua, groupA, "agent", f.aa.ID)
	require.Equal(t, http.StatusCreated, ok.status, ok.body)
}

// TestConversationAddParticipant_ReadCheckErrorRefuses: when the group read
// check cannot be completed, the request is refused and nothing is written.
func TestConversationAddParticipant_ReadCheckErrorRefuses(t *testing.T) {
	f := newRefFixture(t)
	groupA := seedGroupConversation(t, f.st, f.projA.ID, "add-read-error")
	addUserParticipant(t, f.st, groupA, f.ua.ID)
	before := participantCount(t, f.st, groupA)

	f.faults.failGetProject = true
	f.fault.Arm()
	got := f.addParticipantAnswer(t, f.ua, groupA, "agent", f.aa.ID)
	assert.GreaterOrEqual(t, got.status, http.StatusInternalServerError, got.body)
	assert.Equal(t, before, participantCount(t, f.st, groupA), "no participant row is written")
}

// resolveTargetAs calls GET /api/v1/messaging/targets/resolve as identity.
func resolveTargetAs(t *testing.T, srv *Server, identity Identity, project, agent string) refAnswer {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/messaging/targets/resolve?project="+project+"&agent="+agent, nil)
	req = req.WithContext(contextWithIdentity(req.Context(), identity))
	rr := httptest.NewRecorder()
	srv.handleMessagingTargetsResolve(rr, req)
	return refAnswer{status: rr.Code, body: rr.Body.String()}
}

// TestMessagingTargetsResolve_ReplyOnlyTargetMatchesMissing: target lookup
// answers only for targets the caller may message. A target that could only
// reply to the caller gets exactly the answer for an unknown target.
func TestMessagingTargetsResolve_ReplyOnlyTargetMatchesMissing(t *testing.T) {
	srv, _, _, _, _, _, agentA, agentB := cpmSetup(t)
	// agentA (hub mode, project A) may message agentB (project mode,
	// project B); agentB may not message agentA.
	callerB := cpmAgentIdentity(agentB.ID, agentB.ProjectID, agentB.Ancestry)
	callerA := cpmAgentIdentity(agentA.ID, agentA.ProjectID, agentA.Ancestry)

	got := resolveTargetAs(t, srv, callerB, "project-a", agentA.Slug)
	unknown := resolveTargetAs(t, srv, callerB, "project-a", "no-such-agent")
	require.Equal(t, http.StatusNotFound, unknown.status, unknown.body)
	requireSameAnswer(t, unknown, got)

	forward := resolveTargetAs(t, srv, callerA, "project-b", agentB.Slug)
	require.Equal(t, http.StatusOK, forward.status, forward.body)
	var resp targetResolveResponse
	require.NoError(t, json.Unmarshal([]byte(forward.body), &resp))
	require.NotNil(t, resp.Messageability)
	assert.True(t, resp.Messageability.CanMessage)
	assert.False(t, resp.Messageability.CanReachViewer, "the reply direction is still reported")
	assert.Contains(t, forward.body, `"canReachViewer"`)
}
