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
	"sort"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// inboxFixture is a live server with two projects A and B, an agent in
// each, a super-admin owner of both (who can mint every selector), a
// plain owner of A only, and a member of A only.
type inboxFixture struct {
	srv    *Server
	s      store.Store
	admin  string // super-admin, owner of A and B
	owner  string // owner of A only, no system role
	member string // project-member of A only
	projA  string
	projB  string
	agentA *store.Agent
	agentB *store.Agent
}

func newInboxFixture(t *testing.T) *inboxFixture {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()
	f := &inboxFixture{srv: srv, s: s, projA: uuid.NewString(), projB: uuid.NewString()}
	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: f.projA, Name: "Inbox A", Slug: "inbox-a-" + f.projA[:8]}))
	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: f.projB, Name: "Inbox B", Slug: "inbox-b-" + f.projB[:8]}))

	f.admin = uuid.NewString()
	createTestUserWithRole(t, s, f.admin, f.admin+"@test.com", "admin", store.SystemRoleSuperAdmin)
	ensureHubMembership(ctx, s, f.admin)
	createTestUserWithProjectRole(t, s, f.admin, f.admin+"@test.com", f.projA, store.ProjectRoleOwner)
	createTestUserWithProjectRole(t, s, f.admin, f.admin+"@test.com", f.projB, store.ProjectRoleOwner)

	f.owner = uuid.NewString()
	createTestUserWithProjectRole(t, s, f.owner, f.owner+"@test.com", f.projA, store.ProjectRoleOwner)
	ensureHubMembership(ctx, s, f.owner)

	f.member = uuid.NewString()
	createTestUserWithProjectRole(t, s, f.member, f.member+"@test.com", f.projA, store.ProjectRoleMember)
	ensureHubMembership(ctx, s, f.member)

	f.agentA = &store.Agent{ID: uuid.NewString(), Slug: "inbox-agent-a", Name: "Inbox Agent A", ProjectID: f.projA, Phase: string(state.PhaseRunning)}
	f.agentB = &store.Agent{ID: uuid.NewString(), Slug: "inbox-agent-b", Name: "Inbox Agent B", ProjectID: f.projB, Phase: string(state.PhaseRunning)}
	require.NoError(t, s.CreateAgent(ctx, f.agentA))
	require.NoError(t, s.CreateAgent(ctx, f.agentB))
	return f
}

// plainUser creates an active hub member with no project or system role.
func (f *inboxFixture) plainUser(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	id := uuid.NewString()
	require.NoError(t, f.s.CreateUser(ctx, &store.User{ID: id, Email: id + "@test.com", DisplayName: "Plain", Role: store.UserRoleMember, Status: "active"}))
	ensureHubMembership(ctx, f.s, id)
	return id
}

func (f *inboxFixture) mint(t *testing.T, userID string, boundary TokenBoundary, scopes ...string) string {
	t.Helper()
	return mintHubConfigToken(t, f.srv, userID, boundary, scopes...)
}

// call sends a request with a bearer credential (a token key).
func (f *inboxFixture) call(t *testing.T, key, method, path string, body interface{}) *httptest.ResponseRecorder {
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
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// session sends a request with an interactive session of userID.
func (f *inboxFixture) session(t *testing.T, userID, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	user, err := f.s.GetUser(context.Background(), userID)
	require.NoError(t, err)
	return doRequestAsUser(t, f.srv, user, method, path, body)
}

func (f *inboxFixture) message(t *testing.T, recipient, projectID string) string {
	t.Helper()
	id := uuid.NewString()
	require.NoError(t, f.s.CreateMessage(context.Background(), &store.Message{
		ID: id, ProjectID: projectID, Sender: "agent:x", Recipient: "user:" + recipient, RecipientID: recipient,
		Msg: "hello " + id, Type: "instruction", CreatedAt: time.Now().UTC(),
	}))
	return id
}

func (f *inboxFixture) group(t *testing.T, projectID string, participants ...string) *store.Conversation {
	t.Helper()
	at := time.Now().UTC()
	conv := &store.Conversation{ID: uuid.NewString(), Kind: "group", Surface: "native", DisplayName: "g-" + uuid.NewString()[:8], DriftState: "active", LastActivityAt: at, CreatedAt: at}
	if projectID != "" {
		conv.ProjectID = &projectID
	}
	require.NoError(t, f.s.CreateConversation(context.Background(), conv))
	for _, p := range participants {
		addConvParticipant(t, f.s, conv.ID, "user", p)
	}
	return conv
}

func (f *inboxFixture) direct(t *testing.T, userID, peerKind, peerID string) *store.Conversation {
	t.Helper()
	extRef, err := messages.DMConversationKey("user", userID, peerKind, peerID)
	require.NoError(t, err)
	at := time.Now().UTC()
	conv := &store.Conversation{ID: uuid.NewString(), Kind: "direct", Surface: "native", ExternalRef: extRef, DriftState: "active", LastActivityAt: at, CreatedAt: at}
	require.NoError(t, f.s.CreateConversation(context.Background(), conv))
	addConvParticipant(t, f.s, conv.ID, "user", userID)
	addConvParticipant(t, f.s, conv.ID, peerKind, peerID)
	return conv
}

func listMessageIDs(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var result store.ListResult[store.Message]
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
	ids := []string{}
	for _, m := range result.Items {
		ids = append(ids, m.ID)
	}
	sort.Strings(ids)
	return ids
}

func sorted(ids ...string) []string {
	out := append([]string{}, ids...)
	sort.Strings(out)
	return out
}

// TestInboxToken_ProjectBoundaryFiltersMessages requires a token to carry
// inbox:read to list or read the holder's messages, and holds a project
// token to messages of its boundary project. A hub token and a session see
// every message.
func TestInboxToken_ProjectBoundaryFiltersMessages(t *testing.T) {
	f := newInboxFixture(t)
	mA := f.message(t, f.admin, f.projA)
	mB := f.message(t, f.admin, f.projB)

	projTok := f.mint(t, f.admin, projectBoundary(f.projA), "inbox:read")
	assert.Equal(t, []string{mA}, listMessageIDs(t, f.call(t, projTok, http.MethodGet, "/api/v1/messages", nil)))
	assert.Empty(t, listMessageIDs(t, f.call(t, projTok, http.MethodGet, "/api/v1/messages?project="+f.projB, nil)),
		"naming another project lists nothing")
	assert.Equal(t, http.StatusOK, f.call(t, projTok, http.MethodGet, "/api/v1/messages/"+mA, nil).Code)
	assert.Equal(t, http.StatusForbidden, f.call(t, projTok, http.MethodGet, "/api/v1/messages/"+mB, nil).Code)

	hubTok := f.mint(t, f.admin, hubBoundary(), "inbox:read")
	assert.Equal(t, sorted(mA, mB), listMessageIDs(t, f.call(t, hubTok, http.MethodGet, "/api/v1/messages", nil)))
	assert.Equal(t, http.StatusOK, f.call(t, hubTok, http.MethodGet, "/api/v1/messages/"+mB, nil).Code)

	unrelated := f.mint(t, f.admin, hubBoundary(), "project:read")
	assert.Equal(t, http.StatusForbidden, f.call(t, unrelated, http.MethodGet, "/api/v1/messages", nil).Code)
	assert.Equal(t, http.StatusForbidden, f.call(t, unrelated, http.MethodGet, "/api/v1/messages/"+mA, nil).Code)
	readOnly := f.mint(t, f.admin, hubBoundary(), "inbox:read")
	assert.Equal(t, http.StatusForbidden, f.call(t, readOnly, http.MethodPost, "/api/v1/messages/"+mA+"/read", nil).Code,
		"inbox:read does not mark read")

	assert.Equal(t, sorted(mA, mB), listMessageIDs(t, f.session(t, f.admin, http.MethodGet, "/api/v1/messages", nil)))
}

// TestInboxToken_MarkAllReadTouchesOnlyVisibleRows requires mark-all-read
// by a project token to mark only messages of its boundary project, and
// single mark-read to refuse a message outside the boundary.
func TestInboxToken_MarkAllReadTouchesOnlyVisibleRows(t *testing.T) {
	f := newInboxFixture(t)
	ctx := context.Background()
	mA1 := f.message(t, f.admin, f.projA)
	mA2 := f.message(t, f.admin, f.projA)
	mB := f.message(t, f.admin, f.projB)

	projTok := f.mint(t, f.admin, projectBoundary(f.projA), "inbox:write")
	assert.Equal(t, http.StatusForbidden, f.call(t, projTok, http.MethodPost, "/api/v1/messages/"+mB+"/read", nil).Code)
	rec := f.call(t, projTok, http.MethodPost, "/api/v1/messages/read-all", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	for _, id := range []string{mA1, mA2} {
		msg, err := f.s.GetMessage(ctx, id)
		require.NoError(t, err)
		assert.True(t, msg.Read, "boundary-project message %s is marked read", id)
	}
	msg, err := f.s.GetMessage(ctx, mB)
	require.NoError(t, err)
	assert.False(t, msg.Read, "a message outside the boundary stays unread")

	unrelated := f.mint(t, f.admin, hubBoundary(), "inbox:read")
	assert.Equal(t, http.StatusForbidden, f.call(t, unrelated, http.MethodPost, "/api/v1/messages/read-all", nil).Code)

	hubTok := f.mint(t, f.admin, hubBoundary(), "inbox:write")
	require.Equal(t, http.StatusOK, f.call(t, hubTok, http.MethodPost, "/api/v1/messages/read-all", nil).Code)
	msg, err = f.s.GetMessage(ctx, mB)
	require.NoError(t, err)
	assert.True(t, msg.Read, "a hub token marks every message read")
}

// TestInboxToken_ProjectMembershipRecheckedOnEveryRequest requires a
// project token's holder to be a current member of the boundary project on
// every request to a self-scoped route: once the membership is removed, the
// token is refused on the list, a single record and a write, though the
// static boundary still matches.
func TestInboxToken_ProjectMembershipRecheckedOnEveryRequest(t *testing.T) {
	f := newInboxFixture(t)
	ctx := context.Background()
	m := f.message(t, f.owner, f.projA)
	tok := f.mint(t, f.owner, projectBoundary(f.projA), "inbox:read", "inbox:write")

	assert.Equal(t, []string{m}, listMessageIDs(t, f.call(t, tok, http.MethodGet, "/api/v1/messages", nil)))
	assert.Equal(t, http.StatusOK, f.call(t, tok, http.MethodGet, "/api/v1/messages/"+m, nil).Code)

	bindings, err := f.s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, f.owner)
	require.NoError(t, err)
	removed := 0
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == f.projA {
			require.NoError(t, f.s.DeleteRoleBinding(ctx, b.ID))
			removed++
		}
	}
	require.Equal(t, 1, removed)

	assert.Equal(t, http.StatusForbidden, f.call(t, tok, http.MethodGet, "/api/v1/messages", nil).Code)
	assert.Equal(t, http.StatusForbidden, f.call(t, tok, http.MethodGet, "/api/v1/messages/"+m, nil).Code)
	assert.Equal(t, http.StatusForbidden, f.call(t, tok, http.MethodPost, "/api/v1/messages/"+m+"/read", nil).Code)
	assert.Equal(t, http.StatusForbidden, f.call(t, tok, http.MethodGet, "/api/v1/notifications", nil).Code)

	// The row check carries the same rule.
	scoped, err := f.srv.uatService.ValidateToken(ctx, tok)
	require.NoError(t, err)
	check := f.srv.newSelfScopeCheck(ctx, scoped, permInboxRead)
	ok, reason := check.decide(f.projA)
	assert.False(t, ok)
	assert.Equal(t, selfScopeReasonProjectAccess, reason)
}

// TestInboxToken_RefusedCredentialKinds requires the inbox routes to refuse
// every credential kind other than a session, a dev login, an agent and a
// user access token: a broker acting on behalf of a user, a federated
// credential and an unknown or empty kind are refused on a read route and
// a write route.
func TestInboxToken_RefusedCredentialKinds(t *testing.T) {
	f := newInboxFixture(t)
	m := f.message(t, f.admin, f.projA)
	user := NewAuthenticatedUser(f.admin, f.admin+"@test.com", "Admin", "admin", string(ClientTypeWeb))

	type route struct {
		method, path string
		handler      func(http.ResponseWriter, *http.Request)
	}
	routes := []route{
		{http.MethodGet, "/api/v1/messages", f.srv.handleMessages},
		{http.MethodPost, "/api/v1/messages/" + m + "/read", f.srv.handleMessageRoutes},
		{http.MethodGet, "/api/v1/notifications", f.srv.handleNotifications},
		{http.MethodPost, "/api/v1/notifications/ack-all", f.srv.handleNotificationRoutes},
	}
	run := func(r route, cred *CredentialContext) int {
		req := httptest.NewRequest(r.method, r.path, nil)
		ctx := contextWithIdentity(req.Context(), user)
		if cred != nil {
			ctx = contextWithCredentialContext(ctx, *cred)
		}
		rec := httptest.NewRecorder()
		r.handler(rec, req.WithContext(ctx))
		return rec.Code
	}
	for _, r := range routes {
		for _, kind := range []CredentialKind{CredentialKindBroker, CredentialKindFederation, CredentialKind("unknown-kind")} {
			assert.Equal(t, http.StatusForbidden, run(r, &CredentialContext{Kind: kind}), "%s %s with %q", r.method, r.path, kind)
		}
		assert.Equal(t, http.StatusOK, run(r, &CredentialContext{Kind: CredentialKindInteractive}), "%s %s with a session", r.method, r.path)
		assert.Equal(t, http.StatusOK, run(r, nil), "%s %s with an identity and no credential record", r.method, r.path)
	}
}

// TestMessagingStaticMetadata_AnyTokenReads records that the messaging
// capability and channel listings carry no records, so any token reads
// them whatever its selectors or boundary.
func TestMessagingStaticMetadata_AnyTokenReads(t *testing.T) {
	f := newInboxFixture(t)
	for _, key := range []string{
		f.mint(t, f.admin, hubBoundary(), "project:read"),
		f.mint(t, f.admin, projectBoundary(f.projA), "agent:read"),
	} {
		for _, path := range []string{"/api/v1/messaging/capabilities", "/api/v1/message-channels"} {
			rec := f.call(t, key, http.MethodGet, path, nil)
			assert.Equal(t, http.StatusOK, rec.Code, "%s: %s", path, rec.Body.String())
		}
	}
}

func listConversationIDs(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp conversationListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	ids := []string{}
	for _, c := range resp.Conversations {
		ids = append(ids, c.ID)
	}
	sort.Strings(ids)
	return ids
}

// TestConversationListToken_FilteredToBoundary requires a token to carry
// inbox:read to list conversations, lists for a project token only
// conversations of its boundary project (a direct conversation with an
// agent of that project, with agent:read on it), and leaves project-less
// and human direct conversations to hub tokens.
func TestConversationListToken_FilteredToBoundary(t *testing.T) {
	f := newInboxFixture(t)
	other := f.plainUser(t)
	gA := f.group(t, f.projA, f.admin)
	gB := f.group(t, f.projB, f.admin)
	gNone := f.group(t, "", f.admin)
	dA := f.direct(t, f.admin, "agent", f.agentA.ID)
	dB := f.direct(t, f.admin, "agent", f.agentB.ID)
	dHuman := f.direct(t, f.admin, "user", other)

	projTok := f.mint(t, f.admin, projectBoundary(f.projA), "inbox:read", "agent:read")
	assert.Equal(t, sorted(gA.ID, dA.ID), listConversationIDs(t, f.call(t, projTok, http.MethodGet, "/api/v1/conversations", nil)))

	noAgentRead := f.mint(t, f.admin, projectBoundary(f.projA), "inbox:read")
	assert.Equal(t, []string{gA.ID}, listConversationIDs(t, f.call(t, noAgentRead, http.MethodGet, "/api/v1/conversations", nil)),
		"a direct conversation with an agent needs agent:read on it")

	hubTok := f.mint(t, f.admin, hubBoundary(), "inbox:read", "agent:read")
	assert.Equal(t, sorted(gA.ID, gB.ID, gNone.ID, dA.ID, dB.ID, dHuman.ID),
		listConversationIDs(t, f.call(t, hubTok, http.MethodGet, "/api/v1/conversations", nil)))

	unrelated := f.mint(t, f.admin, hubBoundary(), "project:read")
	assert.Equal(t, http.StatusForbidden, f.call(t, unrelated, http.MethodGet, "/api/v1/conversations", nil).Code)
}

// TestDirectConversationToken_PeerAgentMustBeInsideBoundary requires a
// token reading a direct conversation to carry inbox:read for the peer
// agent's project and agent:read on the peer agent, and a direct
// conversation between two users to need a hub boundary.
func TestDirectConversationToken_PeerAgentMustBeInsideBoundary(t *testing.T) {
	f := newInboxFixture(t)
	other := f.plainUser(t)
	dA := f.direct(t, f.admin, "agent", f.agentA.ID)
	dB := f.direct(t, f.admin, "agent", f.agentB.ID)
	dHuman := f.direct(t, f.admin, "user", other)

	paths := func(id string) []string {
		return []string{"/api/v1/conversations/" + id, "/api/v1/conversations/" + id + "/messages"}
	}
	projTok := f.mint(t, f.admin, projectBoundary(f.projA), "inbox:read", "agent:read")
	for _, p := range paths(dA.ID) {
		assert.Equal(t, http.StatusOK, f.call(t, projTok, http.MethodGet, p, nil).Code, p)
	}
	for _, id := range []string{dB.ID, dHuman.ID} {
		for _, p := range paths(id) {
			assert.Equal(t, http.StatusForbidden, f.call(t, projTok, http.MethodGet, p, nil).Code, p)
		}
	}
	noAgentRead := f.mint(t, f.admin, projectBoundary(f.projA), "inbox:read")
	assert.Equal(t, http.StatusForbidden, f.call(t, noAgentRead, http.MethodGet, "/api/v1/conversations/"+dA.ID, nil).Code)
	noInbox := f.mint(t, f.admin, projectBoundary(f.projA), "agent:read")
	assert.Equal(t, http.StatusForbidden, f.call(t, noInbox, http.MethodGet, "/api/v1/conversations/"+dA.ID, nil).Code)

	hubTok := f.mint(t, f.admin, hubBoundary(), "inbox:read", "agent:read")
	for _, id := range []string{dA.ID, dB.ID, dHuman.ID} {
		for _, p := range paths(id) {
			assert.Equal(t, http.StatusOK, f.call(t, hubTok, http.MethodGet, p, nil).Code, p)
		}
	}
	assert.Equal(t, http.StatusOK, f.session(t, f.admin, http.MethodGet, "/api/v1/conversations/"+dHuman.ID, nil).Code)
}

// TestGroupConversationToken_ProjectlessGroupRequiresHubBoundary requires
// a token reading a project group to pass project:read on its project, and
// a token reading a group with no project to carry inbox:read on a hub
// boundary.
func TestGroupConversationToken_ProjectlessGroupRequiresHubBoundary(t *testing.T) {
	f := newInboxFixture(t)
	gA := f.group(t, f.projA, f.admin)
	gNone := f.group(t, "", f.admin)

	readA := f.mint(t, f.admin, projectBoundary(f.projA), "project:read")
	assert.Equal(t, http.StatusOK, f.call(t, readA, http.MethodGet, "/api/v1/conversations/"+gA.ID, nil).Code)
	inboxOnly := f.mint(t, f.admin, projectBoundary(f.projA), "inbox:read")
	assert.Equal(t, http.StatusForbidden, f.call(t, inboxOnly, http.MethodGet, "/api/v1/conversations/"+gA.ID, nil).Code)
	projBoth := f.mint(t, f.admin, projectBoundary(f.projA), "project:read", "inbox:read")
	assert.Equal(t, http.StatusForbidden, f.call(t, projBoth, http.MethodGet, "/api/v1/conversations/"+gNone.ID, nil).Code)
	hubTok := f.mint(t, f.admin, hubBoundary(), "inbox:read")
	assert.Equal(t, http.StatusOK, f.call(t, hubTok, http.MethodGet, "/api/v1/conversations/"+gNone.ID, nil).Code)
	hubNoInbox := f.mint(t, f.admin, hubBoundary(), "project:read")
	assert.Equal(t, http.StatusForbidden, f.call(t, hubNoInbox, http.MethodGet, "/api/v1/conversations/"+gNone.ID, nil).Code)
}

// wireInboxWebChat gives the server a web chat store, which group
// conversation create needs.
func wireInboxWebChat(t *testing.T, f *inboxFixture) {
	t.Helper()
	dbProvider, ok := f.s.(interface{ DB() *sql.DB })
	require.True(t, ok)
	wcs := NewWebChatStore(dbProvider.DB(), "sqlite3")
	require.NoError(t, wcs.Init())
	f.srv.SetWebChatStore(wcs)
}

// TestConversationCreateToken_RequiresInboxWriteAndProjectRead requires a
// token creating a group conversation, or setting its default agent, to
// carry inbox:write for the conversation's project and pass project:read
// on it.
func TestConversationCreateToken_RequiresInboxWriteAndProjectRead(t *testing.T) {
	f := newInboxFixture(t)
	wireInboxWebChat(t, f)
	body := map[string]string{"displayName": "inbox-create", "projectId": f.projA}

	for _, scopes := range [][]string{{"project:read"}, {"inbox:write"}} {
		key := f.mint(t, f.admin, projectBoundary(f.projA), scopes...)
		assert.Equal(t, http.StatusForbidden, f.call(t, key, http.MethodPost, "/api/v1/conversations", body).Code, "%v", scopes)
	}
	otherProject := f.mint(t, f.admin, projectBoundary(f.projB), "project:read", "inbox:write")
	assert.Equal(t, http.StatusForbidden, f.call(t, otherProject, http.MethodPost, "/api/v1/conversations", body).Code)
	both := f.mint(t, f.admin, projectBoundary(f.projA), "project:read", "inbox:write")
	rec := f.call(t, both, http.MethodPost, "/api/v1/conversations", body)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	gA := f.group(t, f.projA, f.admin)
	setBody := map[string]string{"agentId": f.agentA.ID}
	readOnly := f.mint(t, f.admin, projectBoundary(f.projA), "project:read")
	assert.Equal(t, http.StatusForbidden, f.call(t, readOnly, http.MethodPut, "/api/v1/conversations/"+gA.ID+"/default-agent", setBody).Code)
	rec = f.call(t, both, http.MethodPut, "/api/v1/conversations/"+gA.ID+"/default-agent", setBody)
	assert.NotEqual(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

// TestConversationLeaveToken_RequiresInboxWrite requires a token leaving a
// conversation to carry inbox:write for the conversation's project.
func TestConversationLeaveToken_RequiresInboxWrite(t *testing.T) {
	f := newInboxFixture(t)
	gA := f.group(t, f.projA, f.admin)
	gB := f.group(t, f.projB, f.admin)
	readTok := f.mint(t, f.admin, projectBoundary(f.projA), "inbox:read")
	assert.Equal(t, http.StatusForbidden, f.call(t, readTok, http.MethodPost, "/api/v1/conversations/"+gA.ID+"/leave", nil).Code)
	writeTok := f.mint(t, f.admin, projectBoundary(f.projA), "inbox:write")
	assert.Equal(t, http.StatusForbidden, f.call(t, writeTok, http.MethodPost, "/api/v1/conversations/"+gB.ID+"/leave", nil).Code)
	assert.Equal(t, http.StatusNoContent, f.call(t, writeTok, http.MethodPost, "/api/v1/conversations/"+gA.ID+"/leave", nil).Code)
}

// TestConversationAddParticipant_RequiresProjectReadAndMemberPrincipals
// requires every caller adding a participant to a project group to pass
// project:read on the conversation's project, an added agent to belong to
// that project and an added user to be a current member of it; a token
// also needs inbox:write for it. A group with no project keeps the
// participant rule for a session and needs a hub boundary for a token.
func TestConversationAddParticipant_RequiresProjectReadAndMemberPrincipals(t *testing.T) {
	f := newInboxFixture(t)
	outsider := f.plainUser(t)

	add := func(kind, id string) map[string]string {
		return map[string]string{"principalKind": kind, "principalId": id}
	}

	// Project group: a session participant.
	gA := f.group(t, f.projA, f.admin, outsider)
	path := "/api/v1/conversations/" + gA.ID + "/participants"
	assert.Equal(t, http.StatusForbidden, f.session(t, outsider, http.MethodPost, path, add("user", f.member)).Code,
		"a participant without project:read on the conversation's project is refused")
	assert.Equal(t, http.StatusBadRequest, f.session(t, f.admin, http.MethodPost, path, add("user", outsider)).Code,
		"a user who is not a member of the conversation's project cannot be added")
	assert.Equal(t, http.StatusBadRequest, f.session(t, f.admin, http.MethodPost, path, add("agent", f.agentB.ID)).Code,
		"an agent of another project cannot be added")
	assert.Equal(t, http.StatusCreated, f.session(t, f.admin, http.MethodPost, path, add("user", f.member)).Code)
	assert.Equal(t, http.StatusCreated, f.session(t, f.admin, http.MethodPost, path, add("agent", f.agentA.ID)).Code)

	// Project group: tokens.
	gA2 := f.group(t, f.projA, f.admin)
	path2 := "/api/v1/conversations/" + gA2.ID + "/participants"
	readOnly := f.mint(t, f.admin, projectBoundary(f.projA), "project:read")
	assert.Equal(t, http.StatusForbidden, f.call(t, readOnly, http.MethodPost, path2, add("user", f.owner)).Code)
	writeOnly := f.mint(t, f.admin, projectBoundary(f.projA), "inbox:write")
	assert.Equal(t, http.StatusForbidden, f.call(t, writeOnly, http.MethodPost, path2, add("user", f.owner)).Code)
	both := f.mint(t, f.admin, projectBoundary(f.projA), "project:read", "inbox:write")
	assert.Equal(t, http.StatusCreated, f.call(t, both, http.MethodPost, path2, add("user", f.owner)).Code)

	// Group with no project.
	gNone := f.group(t, "", f.admin)
	pathNone := "/api/v1/conversations/" + gNone.ID + "/participants"
	projTok := f.mint(t, f.admin, projectBoundary(f.projA), "project:read", "inbox:write")
	assert.Equal(t, http.StatusForbidden, f.call(t, projTok, http.MethodPost, pathNone, add("user", outsider)).Code,
		"a group with no project needs a hub boundary")
	hubTok := f.mint(t, f.admin, hubBoundary(), "inbox:write")
	assert.Equal(t, http.StatusCreated, f.call(t, hubTok, http.MethodPost, pathNone, add("user", outsider)).Code)
	assert.Equal(t, http.StatusCreated, f.session(t, f.admin, http.MethodPost, pathNone, add("user", f.member)).Code,
		"a session participant keeps the participant rule")
	assert.Equal(t, http.StatusForbidden, f.session(t, f.owner, http.MethodPost, pathNone, add("user", f.member)).Code,
		"a session that is not a participant is refused")
}

func resolveResponse(t *testing.T, rec *httptest.ResponseRecorder) conversationResolveResponse {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp conversationResolveResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp
}

// TestConversationResolve_GroupReferenceRequiresProjectRead requires a
// conv: or #thread reference to a group to resolve only for a caller with
// project:read on the group's project, for sessions and tokens alike.
func TestConversationResolve_GroupReferenceRequiresProjectRead(t *testing.T) {
	f := newInboxFixture(t)
	outsider := f.plainUser(t)
	gB := f.group(t, f.projB, f.admin, outsider)

	convRef := "/api/v1/conversations/resolve?reference=conv:" + gB.ID
	threadRef := "/api/v1/conversations/resolve?reference=%23" + gB.DisplayName
	assert.Equal(t, http.StatusNotFound, f.session(t, outsider, http.MethodGet, convRef, nil).Code,
		"a participant without project:read does not resolve the group")
	assert.False(t, resolveResponse(t, f.session(t, outsider, http.MethodGet, threadRef, nil)).Exists)
	assert.Equal(t, http.StatusOK, f.session(t, f.admin, http.MethodGet, convRef, nil).Code)
	assert.True(t, resolveResponse(t, f.session(t, f.admin, http.MethodGet, threadRef, nil)).Exists)

	inboxOnly := f.mint(t, f.admin, hubBoundary(), "inbox:read")
	assert.Equal(t, http.StatusNotFound, f.call(t, inboxOnly, http.MethodGet, convRef, nil).Code)
	assert.False(t, resolveResponse(t, f.call(t, inboxOnly, http.MethodGet, threadRef, nil)).Exists)
	otherBoundary := f.mint(t, f.admin, projectBoundary(f.projA), "inbox:read", "project:read")
	assert.Equal(t, http.StatusNotFound, f.call(t, otherBoundary, http.MethodGet, convRef, nil).Code)
	both := f.mint(t, f.admin, hubBoundary(), "inbox:read", "project:read")
	assert.Equal(t, http.StatusOK, f.call(t, both, http.MethodGet, convRef, nil).Code)
	assert.True(t, resolveResponse(t, f.call(t, both, http.MethodGet, threadRef, nil)).Exists)
	noInbox := f.mint(t, f.admin, hubBoundary(), "project:read")
	assert.Equal(t, http.StatusForbidden, f.call(t, noInbox, http.MethodGet, convRef, nil).Code)
}

// TestConversationResolve_AgentReferenceRequiresAgentRead requires an
// @agent reference to resolve only for a user caller with agent:read on
// the agent; a token also needs inbox:read for the agent's project. A
// denial answers exists:false with no peer agent, as for no agent.
func TestConversationResolve_AgentReferenceRequiresAgentRead(t *testing.T) {
	f := newInboxFixture(t)
	outsider := f.plainUser(t)
	ref := "/api/v1/conversations/resolve?reference=@" + f.agentB.Slug + "&project_id=" + f.projB

	denied := resolveResponse(t, f.session(t, outsider, http.MethodGet, ref, nil))
	assert.False(t, denied.Exists)
	assert.Nil(t, denied.PeerAgent, "a caller without agent:read learns nothing about the agent")
	allowed := resolveResponse(t, f.session(t, f.admin, http.MethodGet, ref, nil))
	require.NotNil(t, allowed.PeerAgent)
	assert.Equal(t, f.agentB.ID, allowed.PeerAgent.ID)

	inboxOnly := f.mint(t, f.admin, hubBoundary(), "inbox:read")
	assert.Nil(t, resolveResponse(t, f.call(t, inboxOnly, http.MethodGet, ref, nil)).PeerAgent)
	otherBoundary := f.mint(t, f.admin, projectBoundary(f.projA), "inbox:read", "agent:read")
	assert.Nil(t, resolveResponse(t, f.call(t, otherBoundary, http.MethodGet, ref, nil)).PeerAgent)
	both := f.mint(t, f.admin, hubBoundary(), "inbox:read", "agent:read")
	assert.NotNil(t, resolveResponse(t, f.call(t, both, http.MethodGet, ref, nil)).PeerAgent)
}

// TestMessagingTargetsResolve_TokenNeedsAgentMessage requires a token
// resolving a messaging target to pass the agent message authorization,
// which needs agent:message.
func TestMessagingTargetsResolve_TokenNeedsAgentMessage(t *testing.T) {
	f := newInboxFixture(t)
	enableCPM(t, f.srv, f.s)
	path := "/api/v1/messaging/targets/resolve?project=" + f.projA + "&agent=" + f.agentA.Slug

	unrelated := f.mint(t, f.admin, hubBoundary(), "agent:read")
	assert.Equal(t, http.StatusNotFound, f.call(t, unrelated, http.MethodGet, path, nil).Code,
		"a token without agent:message cannot reach the target")
	otherBoundary := f.mint(t, f.admin, projectBoundary(f.projB), "agent:message")
	assert.Equal(t, http.StatusNotFound, f.call(t, otherBoundary, http.MethodGet, path, nil).Code)
	rec := f.call(t, f.mint(t, f.admin, hubBoundary(), "agent:message"), http.MethodGet, path, nil)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// notifyFixture adds a subscription and one notification per project for
// subscriberID.
func (f *inboxFixture) notification(t *testing.T, subscriberType, subscriberID string, agent *store.Agent) string {
	t.Helper()
	ctx := context.Background()
	sub := &store.NotificationSubscription{
		ID: uuid.NewString(), Scope: store.SubscriptionScopeAgent, AgentID: agent.ID,
		SubscriberType: subscriberType, SubscriberID: subscriberID, ProjectID: agent.ProjectID,
		TriggerActivities: []string{"COMPLETED"}, CreatedBy: subscriberID,
	}
	require.NoError(t, f.s.CreateNotificationSubscription(ctx, sub))
	id := uuid.NewString()
	require.NoError(t, f.s.CreateNotification(ctx, &store.Notification{
		ID: id, SubscriptionID: sub.ID, AgentID: agent.ID, ProjectID: agent.ProjectID,
		SubscriberType: subscriberType, SubscriberID: subscriberID, Status: "COMPLETED", Message: "done",
	}))
	return id
}

func notificationIDs(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var rows []store.Notification
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	ids := []string{}
	for _, n := range rows {
		ids = append(ids, n.ID)
	}
	sort.Strings(ids)
	return ids
}

func subscriptionProjects(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var rows []store.NotificationSubscription
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	out := []string{}
	for _, r := range rows {
		out = append(out, r.ProjectID)
	}
	sort.Strings(out)
	return out
}

// TestNotificationToken_RowsFilteredToBoundary requires a token to carry
// inbox:read to list notifications and subscriptions and inbox:write to
// acknowledge or change them, and holds a project token to rows of its
// boundary project: ack-all and bulk delete touch only those rows.
func TestNotificationToken_RowsFilteredToBoundary(t *testing.T) {
	f := newInboxFixture(t)
	ctx := context.Background()
	nA := f.notification(t, store.SubscriberTypeUser, f.admin, f.agentA)
	nB := f.notification(t, store.SubscriberTypeUser, f.admin, f.agentB)

	projRead := f.mint(t, f.admin, projectBoundary(f.projA), "inbox:read")
	assert.Equal(t, []string{nA}, notificationIDs(t, f.call(t, projRead, http.MethodGet, "/api/v1/notifications", nil)))
	assert.Equal(t, []string{f.projA}, subscriptionProjects(t, f.call(t, projRead, http.MethodGet, "/api/v1/notifications/subscriptions", nil)))
	assert.Equal(t, http.StatusForbidden, f.call(t, projRead, http.MethodPost, "/api/v1/notifications/"+nA+"/ack", nil).Code,
		"inbox:read does not acknowledge")
	hubRead := f.mint(t, f.admin, hubBoundary(), "inbox:read")
	assert.Equal(t, sorted(nA, nB), notificationIDs(t, f.call(t, hubRead, http.MethodGet, "/api/v1/notifications", nil)))
	unrelated := f.mint(t, f.admin, hubBoundary(), "project:read")
	assert.Equal(t, http.StatusForbidden, f.call(t, unrelated, http.MethodGet, "/api/v1/notifications", nil).Code)
	assert.Equal(t, http.StatusForbidden, f.call(t, unrelated, http.MethodGet, "/api/v1/notifications/subscriptions", nil).Code)

	projWrite := f.mint(t, f.admin, projectBoundary(f.projA), "inbox:write")
	assert.Equal(t, http.StatusForbidden, f.call(t, projWrite, http.MethodPost, "/api/v1/notifications/"+nB+"/ack", nil).Code)
	require.Equal(t, http.StatusOK, f.call(t, projWrite, http.MethodPost, "/api/v1/notifications/ack-all", nil).Code)
	left, err := f.s.GetNotifications(ctx, store.SubscriberTypeUser, f.admin, true)
	require.NoError(t, err)
	require.Len(t, left, 1, "ack-all by a project token leaves the other project's row")
	assert.Equal(t, nB, left[0].ID)

	subs, err := f.s.GetSubscriptionsForSubscriber(ctx, store.SubscriberTypeUser, f.admin)
	require.NoError(t, err)
	ids := []string{}
	var subB string
	for _, sub := range subs {
		ids = append(ids, sub.ID)
		if sub.ProjectID == f.projB {
			subB = sub.ID
		}
	}
	assert.Equal(t, http.StatusForbidden, f.call(t, projWrite, http.MethodPatch, "/api/v1/notifications/subscriptions/"+subB,
		map[string]interface{}{"triggerActivities": []string{"FAILED"}}).Code)
	assert.Equal(t, http.StatusForbidden, f.call(t, projWrite, http.MethodDelete, "/api/v1/notifications/subscriptions/"+subB, nil).Code)
	rec := f.call(t, projWrite, http.MethodPost, "/api/v1/notifications/subscriptions/bulk-delete", map[string]interface{}{"ids": ids})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"deleted":1}`, rec.Body.String())
	remaining, err := f.s.GetSubscriptionsForSubscriber(ctx, store.SubscriberTypeUser, f.admin)
	require.NoError(t, err)
	require.Len(t, remaining, 1)
	assert.Equal(t, subB, remaining[0].ID)
}

// TestNotificationsByAgent_OtherSubscriberRowsRequireAgentRead requires
// the rows addressed to an agent subscriber, returned with ?agentId=, to
// go only to a caller with agent:read on that agent, sessions and tokens
// alike. The caller's own rows still return.
func TestNotificationsByAgent_OtherSubscriberRowsRequireAgentRead(t *testing.T) {
	f := newInboxFixture(t)
	outsider := f.plainUser(t)
	agentRow := f.notification(t, store.SubscriberTypeAgent, f.agentB.ID, f.agentB)
	own := f.notification(t, store.SubscriberTypeUser, outsider, f.agentB)

	decode := func(rec *httptest.ResponseRecorder) agentNotificationsResponse {
		t.Helper()
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp agentNotificationsResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		return resp
	}
	path := "/api/v1/notifications?agentId=" + f.agentB.ID
	resp := decode(f.session(t, outsider, http.MethodGet, path, nil))
	assert.Empty(t, resp.AgentNotifications, "a caller without agent:read gets no agent subscriber rows")
	require.Len(t, resp.UserNotifications, 1)
	assert.Equal(t, own, resp.UserNotifications[0].ID)

	resp = decode(f.session(t, f.admin, http.MethodGet, path, nil))
	require.Len(t, resp.AgentNotifications, 1)
	assert.Equal(t, agentRow, resp.AgentNotifications[0].ID)

	inboxOnly := f.mint(t, f.admin, hubBoundary(), "inbox:read")
	assert.Empty(t, decode(f.call(t, inboxOnly, http.MethodGet, path, nil)).AgentNotifications)
	both := f.mint(t, f.admin, hubBoundary(), "inbox:read", "agent:read")
	assert.Len(t, decode(f.call(t, both, http.MethodGet, path, nil)).AgentNotifications, 1)
	otherBoundary := f.mint(t, f.admin, projectBoundary(f.projA), "inbox:read", "agent:read")
	assert.Empty(t, decode(f.call(t, otherBoundary, http.MethodGet, path, nil)).AgentNotifications)
}

// TestNotificationSubscription_RequiresProjectAndAgentRead requires every
// user caller creating a subscription, single or bulk, to pass project:read
// on its project and agent:read on a watched agent; a token also needs
// inbox:write for the project. A project or agent that does not exist is
// refused the same way.
func TestNotificationSubscription_RequiresProjectAndAgentRead(t *testing.T) {
	f := newInboxFixture(t)
	outsider := f.plainUser(t)
	sub := func(projectID, agentID string) map[string]interface{} {
		body := map[string]interface{}{"projectId": projectID, "triggerActivities": []string{"COMPLETED"}, "scope": store.SubscriptionScopeProject}
		if agentID != "" {
			body["scope"] = store.SubscriptionScopeAgent
			body["agentId"] = agentID
		}
		return body
	}
	path := "/api/v1/notifications/subscriptions"

	assert.Equal(t, http.StatusForbidden, f.session(t, outsider, http.MethodPost, path, sub(f.projB, "")).Code)
	assert.Equal(t, http.StatusForbidden, f.session(t, outsider, http.MethodPost, path, sub(f.projB, f.agentB.ID)).Code)
	assert.Equal(t, http.StatusForbidden, f.session(t, outsider, http.MethodPost, path, sub(uuid.NewString(), "")).Code)
	assert.Equal(t, http.StatusForbidden, f.session(t, f.admin, http.MethodPost, path, sub(f.projA, uuid.NewString())).Code)
	assert.Equal(t, http.StatusForbidden, f.session(t, outsider, http.MethodPost, path+"/bulk",
		[]map[string]interface{}{sub(f.projB, f.agentB.ID)}).Code, "bulk create applies the same checks")
	assert.Equal(t, http.StatusCreated, f.session(t, f.admin, http.MethodPost, path, sub(f.projB, f.agentB.ID)).Code)

	writeOnly := f.mint(t, f.admin, hubBoundary(), "inbox:write")
	assert.Equal(t, http.StatusForbidden, f.call(t, writeOnly, http.MethodPost, path, sub(f.projA, "")).Code,
		"a token needs project:read on the project")
	readOnly := f.mint(t, f.admin, hubBoundary(), "project:read", "agent:read")
	assert.Equal(t, http.StatusForbidden, f.call(t, readOnly, http.MethodPost, path, sub(f.projA, "")).Code,
		"a token needs inbox:write")
	noAgentRead := f.mint(t, f.admin, hubBoundary(), "inbox:write", "project:read")
	assert.Equal(t, http.StatusForbidden, f.call(t, noAgentRead, http.MethodPost, path, sub(f.projA, f.agentA.ID)).Code)
	otherBoundary := f.mint(t, f.admin, projectBoundary(f.projA), "inbox:write", "project:read")
	assert.Equal(t, http.StatusForbidden, f.call(t, otherBoundary, http.MethodPost, path, sub(f.projB, "")).Code)
	all := f.mint(t, f.admin, projectBoundary(f.projA), "inbox:write", "project:read", "agent:read")
	assert.Equal(t, http.StatusCreated, f.call(t, all, http.MethodPost, path, sub(f.projA, f.agentA.ID)).Code)
}

// TestNotificationTemplates_ListedOnlyForReadableProjects requires the
// template list to return only templates of projects the caller may read
// (templates with no project return to every user caller), template create
// under a project to need project:read on it, and a token to carry the
// inbox selectors and act only inside its boundary.
func TestNotificationTemplates_ListedOnlyForReadableProjects(t *testing.T) {
	f := newInboxFixture(t)
	ctx := context.Background()
	outsider := f.plainUser(t)
	tmpl := func(name, projectID string) string {
		id := uuid.NewString()
		require.NoError(t, f.s.CreateSubscriptionTemplate(ctx, &store.SubscriptionTemplate{
			ID: id, Name: name, Scope: store.SubscriptionScopeProject, TriggerActivities: []string{"COMPLETED"}, ProjectID: projectID, CreatedBy: f.admin,
		}))
		return id
	}
	global := tmpl("global", "")
	inB := tmpl("in-b", f.projB)

	names := func(rec *httptest.ResponseRecorder) []string {
		t.Helper()
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var rows []store.SubscriptionTemplate
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
		out := []string{}
		for _, r := range rows {
			out = append(out, r.ID)
		}
		sort.Strings(out)
		return out
	}
	listB := "/api/v1/notifications/templates?projectId=" + f.projB
	assert.Equal(t, []string{global}, names(f.session(t, outsider, http.MethodGet, listB, nil)))
	assert.Equal(t, sorted(global, inB), names(f.session(t, f.admin, http.MethodGet, listB, nil)))

	projTok := f.mint(t, f.admin, projectBoundary(f.projA), "inbox:read", "project:read")
	assert.Empty(t, names(f.call(t, projTok, http.MethodGet, listB, nil)), "a project token sees neither project-less nor other-project templates")
	hubInboxOnly := f.mint(t, f.admin, hubBoundary(), "inbox:read")
	assert.Equal(t, []string{global}, names(f.call(t, hubInboxOnly, http.MethodGet, listB, nil)),
		"a token needs project:read to see a project's templates")
	hubTok := f.mint(t, f.admin, hubBoundary(), "inbox:read", "project:read")
	assert.Equal(t, sorted(global, inB), names(f.call(t, hubTok, http.MethodGet, listB, nil)))
	unrelated := f.mint(t, f.admin, hubBoundary(), "project:read")
	assert.Equal(t, http.StatusForbidden, f.call(t, unrelated, http.MethodGet, listB, nil).Code)

	create := func(projectID string) map[string]interface{} {
		return map[string]interface{}{"name": "n-" + uuid.NewString()[:8], "triggerActivities": []string{"COMPLETED"}, "projectId": projectID}
	}
	path := "/api/v1/notifications/templates"
	assert.Equal(t, http.StatusForbidden, f.session(t, outsider, http.MethodPost, path, create(f.projB)).Code)
	assert.Equal(t, http.StatusCreated, f.session(t, outsider, http.MethodPost, path, create("")).Code)
	assert.Equal(t, http.StatusCreated, f.session(t, f.admin, http.MethodPost, path, create(f.projB)).Code)
	projWrite := f.mint(t, f.admin, projectBoundary(f.projA), "inbox:write", "project:read")
	assert.Equal(t, http.StatusForbidden, f.call(t, projWrite, http.MethodPost, path, create(f.projB)).Code)
	assert.Equal(t, http.StatusForbidden, f.call(t, projWrite, http.MethodPost, path, create("")).Code)
	assert.Equal(t, http.StatusCreated, f.call(t, projWrite, http.MethodPost, path, create(f.projA)).Code)

	assert.Equal(t, http.StatusForbidden, f.call(t, projWrite, http.MethodDelete, path+"/"+inB, nil).Code,
		"a project token cannot delete another project's template")
	hubWrite := f.mint(t, f.admin, hubBoundary(), "inbox:write")
	assert.Equal(t, http.StatusNoContent, f.call(t, hubWrite, http.MethodDelete, path+"/"+inB, nil).Code)
}
