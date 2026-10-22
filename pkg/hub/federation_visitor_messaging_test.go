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

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Project visitors messaging agents (ptone/scion#4603): sub-route
// admission, sender resolution, the uniform refusal, the visitor DM and its
// read routes, and per-request authorization.

// expectedVisitorSubRoutes is the complete set of (pattern, sub-path,
// method) triples on which a subtree route admits a visitor
// (visitorAdmittedSubRoutes). Adding an entry there fails
// TestVisitorSubRoutes_AdmittedSetIsPinned until it is added here.
var expectedVisitorSubRoutes = []struct{ pattern, subPath, method string }{
	{"/api/v1/projects/", "{projectId}/agents/{agentId}/message", http.MethodPost},
	{"/api/v1/agents/", "{agentId}/message", http.MethodPost},
	{"/api/v1/chat/conversations/", "{dmKey}/messages", http.MethodGet},
	{"/api/v1/chat/conversations/", "{dmKey}/read", http.MethodGet},
	{"/api/v1/chat/conversations/", "{dmKey}/read", http.MethodPost},
	{"/api/v1/projects/", "{projectId}/agents", http.MethodGet},
	{"/api/v1/projects/", "{projectId}/agents/{agentId}", http.MethodGet},
}

func TestVisitorSubRoutes_AdmittedSetIsPinned(t *testing.T) {
	type triple = struct{ pattern, subPath, method string }
	var got []triple
	for _, sub := range visitorAdmittedSubRoutes {
		got = append(got, triple{sub.Pattern, sub.SubPath, sub.Method})
		meta, ok := routeMetadataTable[sub.Pattern]
		require.True(t, ok, "%s is not a registered route", sub.Pattern)
		assert.True(t, routePatternIsSubtree(sub.Pattern), "%s is not a subtree pattern", sub.Pattern)
		assert.Contains(t, meta.VisitorMethods, sub.Method, "%s must list %s in VisitorMethods", sub.Pattern, sub.Method)
	}
	assert.ElementsMatch(t, expectedVisitorSubRoutes, got)

	// Every method a subtree route lists has an admitted sub-route.
	for pattern, meta := range routeMetadataTable {
		if !routePatternIsSubtree(pattern) {
			continue
		}
		for _, m := range meta.VisitorMethods {
			assert.True(t, visitorSubRouteListed(pattern, m), "%s %s has no admitted sub-route", m, pattern)
		}
	}
}

// A subtree route listing a method with no admitted sub-route is refused
// at startup.
func TestValidateVisitorRoutes_SubtreeMethodNeedsSubRoute(t *testing.T) {
	m := RouteMetadata{Pattern: "/api/v1/test/visitor/", RouteID: "test.visitor",
		Classification: RouteAuthenticated, VisitorMethods: []string{http.MethodGet}}
	err := validateVisitorRoutes(map[string]RouteMetadata{m.Pattern: m})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `method "GET" on a subtree pattern has no admitted sub-route`)

	m = routeMetadataTable["/api/v1/agents/"]
	m.VisitorMethods = []string{http.MethodPost, http.MethodDelete}
	err = validateVisitorRoutes(map[string]RouteMetadata{m.Pattern: m})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `method "DELETE" on a subtree pattern has no admitted sub-route`)
	assert.NotContains(t, err.Error(), `method "POST"`)
}

// matchVisitorSubRoute admits the exact sub-paths and methods only.
func TestVisitorSubRoutes_MatchExactShapes(t *testing.T) {
	pid, aid, uid := tid("match-project"), tid("match-agent"), tid("match-user")
	dm := "dm:agent:" + aid + ":user:" + uid
	for _, tc := range []struct {
		pattern, method, path string
		want                  bool
	}{
		{"/api/v1/projects/", http.MethodPost, "/api/v1/projects/" + pid + "/agents/" + aid + "/message", true},
		{"/api/v1/projects/", http.MethodPost, "/api/v1/projects/" + pid + "/agents/my-agent/message", true},
		{"/api/v1/agents/", http.MethodPost, "/api/v1/agents/" + aid + "/message", true},
		{"/api/v1/chat/conversations/", http.MethodGet, "/api/v1/chat/conversations/" + dm + "/messages", true},
		{"/api/v1/chat/conversations/", http.MethodGet, "/api/v1/chat/conversations/" + dm + "/read", true},
		{"/api/v1/chat/conversations/", http.MethodPost, "/api/v1/chat/conversations/" + dm + "/read", true},

		// Wrong method on an admitted shape.
		{"/api/v1/projects/", http.MethodGet, "/api/v1/projects/" + pid + "/agents/" + aid + "/message", false},
		{"/api/v1/agents/", http.MethodPut, "/api/v1/agents/" + aid + "/message", false},
		{"/api/v1/chat/conversations/", http.MethodPost, "/api/v1/chat/conversations/" + dm + "/messages", false},
		{"/api/v1/chat/conversations/", http.MethodDelete, "/api/v1/chat/conversations/" + dm + "/read", false},
		// Other sub-routes of the same patterns.
		{"/api/v1/projects/", http.MethodPost, "/api/v1/projects/" + pid + "/agents", false},
		{"/api/v1/projects/", http.MethodPost, "/api/v1/projects/" + pid + "/agents/" + aid + "/start", false},
		{"/api/v1/projects/", http.MethodPost, "/api/v1/projects/" + pid + "/agents/" + aid + "/message/extra", false},
		{"/api/v1/projects/", http.MethodPost, "/api/v1/projects/" + pid + "/visitors", false},
		{"/api/v1/projects/", http.MethodPost, "/api/v1/projects/" + pid + "/members", false},
		{"/api/v1/agents/", http.MethodPost, "/api/v1/agents/" + aid + "/outbound-message", false},
		{"/api/v1/agents/", http.MethodPost, "/api/v1/agents/" + aid + "/messages", false},
		{"/api/v1/agents/", http.MethodPost, "/api/v1/agents/stop-all", false},
		{"/api/v1/chat/conversations/", http.MethodGet, "/api/v1/chat/conversations/" + dm + "/read/", false},
		{"/api/v1/chat/conversations/", http.MethodPost, "/api/v1/chat/conversations/" + dm + "/typing", false},
		{"/api/v1/chat/conversations/", http.MethodGet, "/api/v1/chat/conversations/" + dm + "/scheduled", false},
		{"/api/v1/chat/conversations/", http.MethodGet, "/api/v1/chat/conversations/" + tid("topic") + "/messages", false},
		// An admitted shape under another pattern.
		{"/api/v1/agents/", http.MethodPost, "/api/v1/projects/" + pid + "/agents/" + aid + "/message", false},
		{"/api/v1/projects/", http.MethodPost, "/api/v1/agents/" + aid + "/message", false},
	} {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		_, ok := matchVisitorSubRoute(tc.pattern, r)
		assert.Equal(t, tc.want, ok, "%s %s under %s", tc.method, tc.path, tc.pattern)
	}
}

// visitorDMAgentID accepts only the canonical DM key of an agent and the
// visitor.
func TestVisitorDMAgentID(t *testing.T) {
	aid, uid, other := tid("dmkey-agent"), tid("dmkey-user"), tid("dmkey-other")
	key, err := messages.DMConversationKey("agent", aid, "user", uid)
	require.NoError(t, err)
	got, ok := visitorDMAgentID(key, uid)
	require.True(t, ok)
	assert.Equal(t, aid, got)

	for name, k := range map[string]string{
		"another user's DM":    "dm:agent:" + aid + ":user:" + other,
		"user to user":         "dm:user:" + other + ":user:" + uid,
		"agent to agent":       "dm:agent:" + aid + ":agent:" + other,
		"upper-case agent ID":  "dm:agent:" + strings.ToUpper(aid) + ":user:" + uid,
		"not a DM key":         "dm:agent:" + aid,
		"trailing segment":     key + ":x",
		"agent ID not an UUID": "dm:agent:my-agent:user:" + uid,
	} {
		_, ok := visitorDMAgentID(k, uid)
		assert.False(t, ok, name)
	}
}

// serveGuardedPathAs is serveGuardedAs with a request path.
func serveGuardedPathAs(srv *Server, meta RouteMetadata, method, path string, identity Identity) (*httptest.ResponseRecorder, bool) {
	called := false
	guarded := srv.routeGuard(meta, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(method, path, nil)
	ctx := contextWithIdentity(req.Context(), identity)
	ctx = contextWithCredentialContext(ctx, credentialContextForIdentity(identity))
	rec := httptest.NewRecorder()
	guarded(rec, req.WithContext(ctx))
	return rec, called
}

// The sub-route check needs the experiment: with it off, a visitor
// identity (which the middleware no longer constructs) is refused even on
// an admitted sub-path.
func TestRouteGuard_VisitorSubRouteRefusedWhileExperimentOff(t *testing.T) {
	srv, _ := testServer(t)
	visitorSetExperiment(t, srv, false)
	visitor := NewVisitorIdentity(tid("off-visitor"), visitorTestUserIssuer, "sub", "v@corp.com", "V")
	rec, called := serveGuardedPathAs(srv, routeMetadataTable["/api/v1/agents/"], http.MethodPost,
		"/api/v1/agents/"+tid("off-agent")+"/message", visitor)
	assert.False(t, called)
	assertVisitorRefusal(t, rec, "experiment off")
}

// visitorMessagingFixture is a visitor grant fixture with two running
// agents in the project (one in message mode none), an agent in another
// project, a recording dispatcher, the conversation envelope switch on and
// a web chat store.
type visitorMessagingFixture struct {
	*visitorGrantFixture
	dispatcher *recordingDispatcher
	agent      *store.Agent
	sealed     *store.Agent
	foreign    *store.Agent
}

func newVisitorMessagingFixture(t *testing.T) *visitorMessagingFixture {
	t.Helper()
	f := newVisitorGrantFixture(t)
	ctx := context.Background()

	brokerID := tid(t.Name() + "-broker")
	require.NoError(t, f.st.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: brokerID, Name: "visitor-broker", Slug: "visitor-broker", Status: store.BrokerStatusOnline,
	}))
	otherProject := tid(t.Name() + "-other-project")
	require.NoError(t, f.st.CreateProject(ctx, &store.Project{ID: otherProject, Name: "visitor-other", Slug: "visitor-other"}))
	newAgent := func(name, projectID, mode string) *store.Agent {
		a := &store.Agent{
			ID: tid(t.Name() + "-" + name), Name: name, Slug: name, ProjectID: projectID,
			RuntimeBrokerID: brokerID, Phase: "running", MessageMode: mode,
		}
		require.NoError(t, f.st.CreateAgent(ctx, a))
		return a
	}
	fx := &visitorMessagingFixture{
		visitorGrantFixture: f,
		dispatcher:          &recordingDispatcher{},
		agent:               newAgent("helper", f.projectID, store.MessageModeProject),
		sealed:              newAgent("sealed", f.projectID, store.MessageModeNone),
	}
	fx.foreign = newAgent("foreign", otherProject, store.MessageModeProject)
	f.srv.SetDispatcher(fx.dispatcher)
	enableReadSwitch(t, f.srv)
	wcs := NewWebChatStore(openTestMemorySQLite(t, "sqlite"), "sqlite")
	require.NoError(t, wcs.Init())
	f.srv.SetWebChatStore(wcs)
	return fx
}

// visitor grants email on the fixture's first issuer and returns the
// visitor's account ID and a verified issuer token for subject.
func (f *visitorMessagingFixture) visitor(t *testing.T, subject, email string) (string, string) {
	t.Helper()
	v := f.grantOK(t, grantTestIssuer, email)
	return v.ID, f.token(t, grantTestIssuer, subject, email, boolPtr(true))
}

// do sends one request with the issuer token through the server handler.
func (f *visitorMessagingFixture) do(t *testing.T, token, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set(FederationTokenHeader, token)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func (f *visitorMessagingFixture) projectMessagePath(agentRef string) string {
	return "/api/v1/projects/" + f.projectID + "/agents/" + agentRef + "/message"
}

func agentMessagePath(agentID string) string {
	return "/api/v1/agents/" + agentID + "/message"
}

func chatDMPath(key, action string) string {
	return chatConversationsPrefix + key + "/" + action
}

func visitorTextBody(text string) map[string]interface{} {
	return map[string]interface{}{"message": text}
}

// assertSameResponse asserts got is exactly want: status, headers and
// body.
func assertSameResponse(t *testing.T, want, got *httptest.ResponseRecorder, label string) {
	t.Helper()
	assert.Equal(t, want.Code, got.Code, "%s: status", label)
	assert.Equal(t, want.Header(), got.Header(), "%s: headers", label)
	assert.Equal(t, want.Body.String(), got.Body.String(), "%s: body", label)
}

// A visitor with a grant sends to an agent in the project, by the project
// form (slug or ID) and the by-ID form, as a user sender keyed by its local
// account: the sender address is the visitor's account ID, the message is
// persisted in the DM keyed by that ID, and the envelope metadata shows the
// visitor marker, the issuer-verified email, the issuer and the display
// label, never the synthetic address.
func TestVisitorMessaging_SendAdmittedWithGrant(t *testing.T) {
	f := newVisitorMessagingFixture(t)
	ctx := context.Background()
	visitorID, token := f.visitor(t, "sub-alice", "alice@corp.example")

	for _, path := range []string{
		f.projectMessagePath(f.agent.Slug),
		f.projectMessagePath(f.agent.ID),
		agentMessagePath(f.agent.ID),
	} {
		rec := f.do(t, token, http.MethodPost, path, visitorTextBody("hello from a visitor"))
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", path, rec.Body.String())
	}

	calls := f.dispatcher.getCalls()
	require.Len(t, calls, 3)
	for _, c := range calls {
		sm := c.StructuredMessage
		require.NotNil(t, sm)
		assert.Equal(t, f.agent.ID, c.Agent.ID)
		assert.Equal(t, "user:"+visitorID, sm.Sender)
		assert.Equal(t, visitorID, sm.SenderID)
		assert.Equal(t, "true", sm.Metadata[messages.MetadataSenderVisitor])
		assert.Equal(t, "alice@corp.example", sm.Metadata[messages.MetadataSenderEmail])
		assert.Equal(t, "idp-grant.example.com", sm.Metadata[messages.MetadataSenderIssuer])
		assert.Equal(t, "alice@corp.example (visitor via idp-grant.example.com)", sm.Metadata[messages.MetadataSenderDisplay])
		require.NotEmpty(t, sm.DeliveryText, "the envelope switch is on")
		assert.Contains(t, sm.DeliveryText, `"from": "user:`+visitorID+`"`)
		assert.Contains(t, sm.DeliveryText, `"sender_visitor": "true"`)
		assert.Contains(t, sm.DeliveryText, `"sender_email": "alice@corp.example"`)
		assert.Contains(t, sm.DeliveryText, `"sender_issuer": "idp-grant.example.com"`)
		assert.Contains(t, sm.DeliveryText, `"sender_display": "alice@corp.example (visitor via idp-grant.example.com)"`)
		assert.NotContains(t, sm.DeliveryText, ".invalid", "never the synthetic address")
	}
	// The legacy renderer forwards the same keys.
	legacy := messages.FormatForDelivery(calls[0].StructuredMessage)
	assert.Contains(t, legacy, `"sender_visitor": "true"`)
	assert.Contains(t, legacy, `"sender_email": "alice@corp.example"`)
	assert.Contains(t, legacy, `"sender_issuer": "idp-grant.example.com"`)
	assert.Contains(t, legacy, `"sender_display": "alice@corp.example (visitor via idp-grant.example.com)"`)

	dmKey, err := messages.DMConversationKey("agent", f.agent.ID, "user", visitorID)
	require.NoError(t, err)
	conv, err := f.st.GetConversationByExternalRef(ctx, "native", dmKey)
	require.NoError(t, err, "the DM is keyed by the visitor's account ID")
	msgs, err := f.st.ListMessages(ctx, store.MessageFilter{ConversationID: conv.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, msgs.Items, 3)
	for _, m := range msgs.Items {
		assert.Equal(t, "user:"+visitorID, m.Sender)
		assert.Equal(t, visitorID, m.SenderID)
	}
}

// Unknown agent, unknown project, an agent of another project, the agent's
// message mode none, no current grant (not started, expired, revoked),
// another sub-route of the same pattern, and the issuer no longer
// accepting visitors all give one response, byte for byte.
func TestVisitorMessaging_UniformRefusal(t *testing.T) {
	f := newVisitorMessagingFixture(t)
	ctx := context.Background()
	_, alice := f.visitor(t, "sub-alice", "alice@corp.example")

	future := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	rec := f.grant(t, f.owner, map[string]interface{}{"issuer": grantTestIssuer, "email": "carol@corp.example", "notBefore": future})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	carol := f.token(t, grantTestIssuer, "sub-carol", "carol@corp.example", boolPtr(true))

	daveGrant := f.grantOK(t, grantTestIssuer, "dave@corp.example")
	dave := f.token(t, grantTestIssuer, "sub-dave", "dave@corp.example", boolPtr(true))
	past := time.Now().Add(-time.Minute)
	_, err := f.st.UpdateVisitorRoleBindingWindow(ctx, daveGrant.BindingID, nil, &past)
	require.NoError(t, err)

	erinGrant := f.grantOK(t, grantTestIssuer, "erin@corp.example")
	erin := f.token(t, grantTestIssuer, "sub-erin", "erin@corp.example", boolPtr(true))
	rec = doRequestAsUser(t, f.srv, f.owner, http.MethodDelete, f.visitorsPath()+"/"+erinGrant.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	// The grant works for the admitted case.
	rec = f.do(t, alice, http.MethodPost, f.projectMessagePath(f.agent.Slug), visitorTextBody("ok"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	type refusal struct {
		label, token, method, path string
	}
	cases := []refusal{
		{"unknown agent (project form)", alice, http.MethodPost, f.projectMessagePath("no-such-agent")},
		{"unknown agent (by ID)", alice, http.MethodPost, agentMessagePath(tid("no-such-agent"))},
		{"unknown project", alice, http.MethodPost, "/api/v1/projects/" + tid("no-such-project") + "/agents/" + f.agent.Slug + "/message"},
		{"agent of another project (project form)", alice, http.MethodPost, f.projectMessagePath(f.foreign.ID)},
		{"agent of another project (by ID)", alice, http.MethodPost, agentMessagePath(f.foreign.ID)},
		{"message mode none (project form)", alice, http.MethodPost, f.projectMessagePath(f.sealed.Slug)},
		{"message mode none (by ID)", alice, http.MethodPost, agentMessagePath(f.sealed.ID)},
		{"grant not started", carol, http.MethodPost, f.projectMessagePath(f.agent.Slug)},
		{"grant expired", dave, http.MethodPost, f.projectMessagePath(f.agent.Slug)},
		{"grant revoked", erin, http.MethodPost, agentMessagePath(f.agent.ID)},
		{"another sub-route", alice, http.MethodPost, "/api/v1/projects/" + f.projectID + "/agents/" + f.agent.Slug + "/start"},
		{"agent read (not open to visitors here)", alice, http.MethodGet, "/api/v1/agents/" + f.agent.ID},
	}
	var first *httptest.ResponseRecorder
	for _, c := range cases {
		rec := f.do(t, c.token, c.method, c.path, visitorTextBody("refused"))
		assertVisitorRefusal(t, rec, c.label)
		if first == nil {
			first = rec
			continue
		}
		assertSameResponse(t, first, rec, c.label)
	}

	f.setIssuerAcceptsVisitors(t, grantTestIssuer, false)
	rec = f.do(t, alice, http.MethodPost, f.projectMessagePath(f.agent.Slug), visitorTextBody("issuer off"))
	assertSameResponse(t, first, rec, "issuer does not accept visitors")
	f.setIssuerAcceptsVisitors(t, grantTestIssuer, true)
	rec = f.do(t, alice, http.MethodPost, f.projectMessagePath(f.agent.Slug), visitorTextBody("issuer on again"))
	assert.Equal(t, http.StatusOK, rec.Code, "accepting the issuer again restores access: %s", rec.Body.String())

	assert.Len(t, f.dispatcher.getCalls(), 2, "no refused request is dispatched")
}

// Revoking a grant refuses the visitor's next send and next DM read:
// nothing caches the grant.
func TestVisitorMessaging_RevocationTakesEffectOnNextRequest(t *testing.T) {
	f := newVisitorMessagingFixture(t)
	visitorID, token := f.visitor(t, "sub-gus", "gus@corp.example")
	dmKey, err := messages.DMConversationKey("agent", f.agent.ID, "user", visitorID)
	require.NoError(t, err)

	rec := f.do(t, token, http.MethodPost, f.projectMessagePath(f.agent.Slug), visitorTextBody("before"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = f.do(t, token, http.MethodGet, chatDMPath(dmKey, "messages"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = doRequestAsUser(t, f.srv, f.owner, http.MethodDelete, f.visitorsPath()+"/"+visitorID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	rec = f.do(t, token, http.MethodPost, f.projectMessagePath(f.agent.Slug), visitorTextBody("after"))
	assertVisitorRefusal(t, rec, "send after revocation")
	rec = f.do(t, token, http.MethodGet, chatDMPath(dmKey, "messages"), nil)
	assertVisitorRefusal(t, rec, "DM read after revocation")
	assert.Len(t, f.dispatcher.getCalls(), 1)
}

// The visitor reads its own DM history with the agent and sets and reads
// its read marker; another visitor's DM, a DM of a local user, and every
// other conversation, chat and inbox route are refused.
func TestVisitorMessaging_OwnDMReadOnly(t *testing.T) {
	f := newVisitorMessagingFixture(t)
	aliceID, alice := f.visitor(t, "sub-alice", "alice@corp.example")
	bobID, bob := f.visitor(t, "sub-bob", "bob@corp.example")
	aliceDM, err := messages.DMConversationKey("agent", f.agent.ID, "user", aliceID)
	require.NoError(t, err)
	bobDM, err := messages.DMConversationKey("agent", f.agent.ID, "user", bobID)
	require.NoError(t, err)
	ownerDM, err := messages.DMConversationKey("agent", f.agent.ID, "user", f.owner.ID)
	require.NoError(t, err)

	rec := f.do(t, alice, http.MethodPost, f.projectMessagePath(f.agent.Slug), visitorTextBody("alice to agent"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var sent MessageDeliveryResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &sent))
	require.NotEmpty(t, sent.MessageID)

	rec = f.do(t, alice, http.MethodGet, chatDMPath(aliceDM, "messages"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var history chatHistoryResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &history))
	require.Len(t, history.Messages, 1, "the visitor's own message, sent through the hub API")
	assert.Equal(t, "alice to agent", history.Messages[0].Msg)

	rec = f.do(t, alice, http.MethodPost, chatDMPath(aliceDM, "read"), map[string]string{"messageId": sent.MessageID})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = f.do(t, alice, http.MethodGet, chatDMPath(aliceDM, "read"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var readState chatReadStateResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &readState))
	assert.Equal(t, sent.MessageID, readState.LastReadMessageID)

	type refusal struct{ label, token, method, path string }
	for _, c := range []refusal{
		{"another visitor's DM history", bob, http.MethodGet, chatDMPath(aliceDM, "messages")},
		{"another visitor's read marker", bob, http.MethodPost, chatDMPath(aliceDM, "read")},
		{"a local user's DM history", alice, http.MethodGet, chatDMPath(ownerDM, "messages")},
		{"a DM with an agent of another project", alice, http.MethodGet,
			chatDMPath("dm:agent:"+f.foreign.ID+":user:"+aliceID, "messages")},
		{"a DM with a sealed agent", alice, http.MethodGet,
			chatDMPath("dm:agent:"+f.sealed.ID+":user:"+aliceID, "messages")},
		{"a non-canonical DM key", alice, http.MethodGet,
			chatDMPath("dm:agent:"+strings.ToUpper(f.agent.ID)+":user:"+aliceID, "messages")},
		{"chat send", alice, http.MethodPost, chatDMPath(aliceDM, "messages")},
		{"chat typing", alice, http.MethodPost, chatDMPath(aliceDM, "typing")},
		{"chat mute", alice, http.MethodPut, chatDMPath(aliceDM, "mute")},
		{"chat unread", alice, http.MethodPost, chatDMPath(aliceDM, "unread")},
		{"chat DM list", alice, http.MethodGet, "/api/v1/chat/dms"},
		{"chat unread count", alice, http.MethodGet, "/api/v1/chat/unread-count"},
		{"conversation list", alice, http.MethodGet, "/api/v1/conversations"},
		{"conversation by ID", alice, http.MethodGet, "/api/v1/conversations/" + tid("conv") + "/messages"},
		{"inbox", alice, http.MethodGet, "/api/v1/messages"},
		{"inbox read-all", alice, http.MethodPost, "/api/v1/messages/read-all"},
		{"agent messages", alice, http.MethodGet, "/api/v1/agents/" + f.agent.ID + "/messages"},
	} {
		assertVisitorRefusal(t, f.do(t, c.token, c.method, c.path, nil), c.label)
	}

	// Bob reads his own (empty) DM.
	rec = f.do(t, bob, http.MethodGet, chatDMPath(bobDM, "messages"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// A visitor sends a direct text message only: every request field outside
// the allow-list is refused with a validation error naming it in
// details.field, before anything is written (a thread ID too, its own DM key
// included), and a client timestamp is replaced by the hub's.
func TestVisitorMessaging_RequestFieldsLimited(t *testing.T) {
	f := newVisitorMessagingFixture(t)
	visitorID, token := f.visitor(t, "sub-hal", "hal@corp.example")
	ownDM, err := messages.DMConversationKey("agent", f.agent.ID, "user", visitorID)
	require.NoError(t, err)
	structured := func(mut func(sm *messages.StructuredMessage)) map[string]interface{} {
		sm := &messages.StructuredMessage{Msg: "hi", Type: messages.TypeInstruction}
		mut(sm)
		return map[string]interface{}{"structured_message": sm}
	}
	assertRefused := func(field string, body interface{}) {
		t.Helper()
		rec := f.do(t, token, http.MethodPost, f.projectMessagePath(f.agent.Slug), body)
		require.Equal(t, http.StatusBadRequest, rec.Code, "%s: %s", field, rec.Body.String())
		var resp ErrorResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, field+" is not available to visitors", resp.Error.Message, field)
		assert.Equal(t, field, resp.Error.Details["field"], field)
	}
	for field, body := range map[string]interface{}{
		"plain":                            map[string]interface{}{"message": "hi", "plain": true},
		"interrupt":                        map[string]interface{}{"message": "hi", "interrupt": true},
		"wake":                             map[string]interface{}{"message": "hi", "wake": true},
		"notify":                           map[string]interface{}{"message": "hi", "notify": true},
		"mentions":                         map[string]interface{}{"message": "hi", "mentions": []string{"sealed"}},
		"surface":                          map[string]interface{}{"message": "hi", "surface": "slack"},
		"external_ref":                     map[string]interface{}{"message": "hi", "external_ref": "ext-1"},
		"parent_ref":                       map[string]interface{}{"message": "hi", "parent_ref": "parent-1"},
		"structured_message.urgent":        structured(func(sm *messages.StructuredMessage) { sm.Urgent = true }),
		"structured_message.status":        structured(func(sm *messages.StructuredMessage) { sm.Status = "done" }),
		"structured_message.recipient_id":  structured(func(sm *messages.StructuredMessage) { sm.RecipientID = tid("some-recipient") }),
		"structured_message.recipients":    structured(func(sm *messages.StructuredMessage) { sm.Recipients = "agent:helper,agent:sealed" }),
		"structured_message.broadcasted":   structured(func(sm *messages.StructuredMessage) { sm.Broadcasted = true }),
		"structured_message.observer_only": structured(func(sm *messages.StructuredMessage) { sm.ObserverOnly = true }),
		"structured_message.plain":         structured(func(sm *messages.StructuredMessage) { sm.Plain = true }),
		"structured_message.type":          structured(func(sm *messages.StructuredMessage) { sm.Type = messages.TypeSystem }),
		"structured_message.attachments":   structured(func(sm *messages.StructuredMessage) { sm.Attachments = []string{"gs://bucket/file"} }),
		"structured_message.metadata":      structured(func(sm *messages.StructuredMessage) { sm.Metadata = map[string]string{"k": "v"} }),
		"structured_message.recipient":     structured(func(sm *messages.StructuredMessage) { sm.Recipient = "group[agent:helper,agent:sealed]" }),
		"structured_message.channel":       structured(func(sm *messages.StructuredMessage) { sm.Channel = "web" }),
		"structured_message.delivery_text": structured(func(sm *messages.StructuredMessage) { sm.DeliveryText = "spoofed" }),
		"structured_message.thread_id":     structured(func(sm *messages.StructuredMessage) { sm.ThreadID = ownDM }),
		"structured_message.conversation_id": structured(func(sm *messages.StructuredMessage) {
			sm.ConversationID = tid("some-conversation")
		}),
	} {
		assertRefused(field, body)
	}
	// Any other thread ID is refused the same way.
	assertRefused("structured_message.thread_id",
		structured(func(sm *messages.StructuredMessage) { sm.ThreadID = "my-thread" }))
	assert.Empty(t, f.dispatcher.getCalls(), "no refused request is dispatched")

	const clientTimestamp = "2001-02-03T04:05:06Z"
	rec := f.do(t, token, http.MethodPost, f.projectMessagePath(f.agent.Slug),
		structured(func(sm *messages.StructuredMessage) { sm.Timestamp = clientTimestamp }))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	calls := f.dispatcher.getCalls()
	require.Len(t, calls, 1)
	assert.NotEqual(t, clientTimestamp, calls[0].StructuredMessage.Timestamp, "the hub sets the timestamp")
}

// Only the hub sets the visitor sender metadata: a local user's message
// carrying the keys is delivered without them.
func TestVisitorMessaging_SenderMetadataNotClientSettable(t *testing.T) {
	f := newVisitorMessagingFixture(t)
	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, f.projectMessagePath(f.agent.Slug), map[string]interface{}{
		"structured_message": &messages.StructuredMessage{
			Msg: "from the owner", Type: messages.TypeInstruction,
			Metadata: map[string]string{
				messages.MetadataSenderVisitor: "true",
				messages.MetadataSenderEmail:   "spoofed@corp.example",
				messages.MetadataSenderIssuer:  "spoofed.example",
				messages.MetadataSenderDisplay: "spoofed@corp.example (visitor via spoofed.example)",
				"other":                        "kept",
			},
		},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	calls := f.dispatcher.getCalls()
	require.Len(t, calls, 1)
	md := calls[0].StructuredMessage.Metadata
	for _, k := range []string{messages.MetadataSenderVisitor, messages.MetadataSenderEmail,
		messages.MetadataSenderIssuer, messages.MetadataSenderDisplay} {
		assert.NotContains(t, md, k)
	}
	assert.Equal(t, "kept", md["other"])
	assert.NotContains(t, calls[0].StructuredMessage.DeliveryText, "spoofed")
}

// visitorDMMessages returns the messages of the DM between agentID and the
// user userID.
func visitorDMMessages(t *testing.T, st store.Store, agentID, userID string) []store.Message {
	t.Helper()
	ctx := context.Background()
	dmKey, err := messages.DMConversationKey("agent", agentID, "user", userID)
	require.NoError(t, err)
	conv, err := st.GetConversationByExternalRef(ctx, "native", dmKey)
	require.NoError(t, err)
	msgs, err := st.ListMessages(ctx, store.MessageFilter{ConversationID: conv.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	return msgs.Items
}

// The agent's reply to the address its envelope showed as the sender lands
// in the visitor's DM, even when a local account has the visitor's email:
// that account receives nothing.
func TestVisitorMessaging_AgentReplyLandsInVisitorDM(t *testing.T) {
	f := newVisitorMessagingFixture(t)
	ctx := context.Background()
	visitorID, token := f.visitor(t, "sub-ida", "ida@corp.example")
	local := &store.User{
		ID: tid(t.Name() + "-local"), Email: "ida@corp.example", DisplayName: "Local Ida", Role: "member", Status: "active",
	}
	require.NoError(t, f.st.CreateUser(ctx, local))
	ensureHubMembership(ctx, f.st, local.ID)

	rec := f.do(t, token, http.MethodPost, f.projectMessagePath(f.agent.Slug), visitorTextBody("question"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	calls := f.dispatcher.getCalls()
	require.Len(t, calls, 1)
	shown := calls[0].StructuredMessage.Sender
	require.Contains(t, calls[0].StructuredMessage.DeliveryText, `"from": "`+shown+`"`)

	rec = postOutboundTo(t, f.srv, f.projectID, f.agent.ID, shown, "answer")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var reply *store.Message
	msgs := visitorDMMessages(t, f.st, f.agent.ID, visitorID)
	for i := range msgs {
		if msgs[i].Msg == "answer" {
			reply = &msgs[i]
		}
	}
	require.NotNil(t, reply, "the reply is in the visitor's DM")
	assert.Equal(t, visitorID, reply.RecipientID)
	assert.Equal(t, "user:"+visitorID, reply.Recipient, "the visitor's address, never an email")
	assertNoMessagesFor(t, f.st, local.ID)

	// The visitor reads both messages from its DM.
	dmKey, err := messages.DMConversationKey("agent", f.agent.ID, "user", visitorID)
	require.NoError(t, err)
	rec = f.do(t, token, http.MethodGet, chatDMPath(dmKey, "messages"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var history chatHistoryResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &history))
	assert.Len(t, history.Messages, 2)
}

// The visitor's email never addresses the visitor: with no local account of
// that email the agent's send is refused as an unknown addressee, and
// nothing reaches the visitor's DM.
func TestVisitorMessaging_EmailAddressNeverReachesVisitor(t *testing.T) {
	f := newVisitorMessagingFixture(t)
	visitorID, token := f.visitor(t, "sub-kim", "kim@corp.example")
	rec := f.do(t, token, http.MethodPost, f.projectMessagePath(f.agent.Slug), visitorTextBody("question"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = postOutboundTo(t, f.srv, f.projectID, f.agent.ID, "user:kim@corp.example", "answer")
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, ErrCodeAddrUnknown, resp.Error.Code)
	assert.Len(t, visitorDMMessages(t, f.st, f.agent.ID, visitorID), 1, "only the visitor's own message")
}

// An agent sends to a visitor only into a DM the visitor started with that
// agent. Before the visitor writes, and from another agent after it wrote,
// a send by account ID or by recipient_id is answered exactly like a send
// to an unknown user, and nothing is written.
func TestVisitorMessaging_UnsolicitedAgentSendRefused(t *testing.T) {
	f := newVisitorMessagingFixture(t)
	visitorID, token := f.visitor(t, "sub-lee", "lee@corp.example")
	unknownID := tid(t.Name() + "-nobody")
	other := f.sealed // another agent of the project

	// sameAsUnknown asserts that got is the answer to the same send to
	// unknownID, with the address in the message swapped.
	sameAsUnknown := func(label string, got, unknown *httptest.ResponseRecorder) {
		t.Helper()
		assert.Equal(t, unknown.Code, got.Code, "%s: status", label)
		assert.Equal(t, unknown.Header(), got.Header(), "%s: headers", label)
		assert.Equal(t, strings.ReplaceAll(unknown.Body.String(), unknownID, visitorID), got.Body.String(), "%s: body", label)
	}
	check := func(label string, agent *store.Agent) {
		t.Helper()
		byAddress := postOutboundTo(t, f.srv, f.projectID, agent.ID, "user:"+visitorID, "unsolicited")
		unknownAddress := postOutboundTo(t, f.srv, f.projectID, agent.ID, "user:"+unknownID, "unsolicited")
		require.Equal(t, http.StatusBadRequest, unknownAddress.Code, unknownAddress.Body.String())
		sameAsUnknown(label+" by address", byAddress, unknownAddress)

		byRecipientID := postOutboundRequest(t, f.srv, f.projectID, agent.ID, OutboundMessageRequest{RecipientID: visitorID, Msg: "unsolicited"})
		unknownRecipientID := postOutboundRequest(t, f.srv, f.projectID, agent.ID, OutboundMessageRequest{RecipientID: unknownID, Msg: "unsolicited"})
		require.Equal(t, http.StatusBadRequest, unknownRecipientID.Code, unknownRecipientID.Body.String())
		sameAsUnknown(label+" by recipient_id", byRecipientID, unknownRecipientID)
	}

	check("before the visitor wrote", f.agent)
	assertNoMessagesFor(t, f.st, visitorID)

	rec := f.do(t, token, http.MethodPost, f.projectMessagePath(f.agent.Slug), visitorTextBody("question"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	check("another agent", other)
	assertNoMessagesFor(t, f.st, visitorID)

	// The agent the visitor wrote to may now answer in that DM.
	rec = postOutboundTo(t, f.srv, f.projectID, f.agent.ID, "user:"+visitorID, "answer")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// An unlinked federated user (a verified issuer token with no linked
// account) messaging an agent gets the same response with the experiment
// on as with it off, and never the visitor refusal.
func TestVisitorMessaging_UnlinkedFederatedUserUnchanged(t *testing.T) {
	f := newVisitorMessagingFixture(t)
	token := f.token(t, grantTestOtherIssuer, "sub-nobody", "nobody@elsewhere.example", boolPtr(true))
	path := f.projectMessagePath(f.agent.Slug)

	on := f.do(t, token, http.MethodPost, path, visitorTextBody("unlinked"))
	visitorSetExperiment(t, f.srv, false)
	off := f.do(t, token, http.MethodPost, path, visitorTextBody("unlinked"))
	assertSameResponse(t, off, on, "unlinked federated user, experiment on vs off")
	assert.NotContains(t, on.Body.String(), visitorRouteRefusalMessage)
	assert.Empty(t, f.dispatcher.getCalls())
}

// With the experiment off a granted visitor's issuer token is an ordinary
// federated user: it is not admitted to send.
func TestVisitorMessaging_ExperimentOffNoVisitorSend(t *testing.T) {
	f := newVisitorMessagingFixture(t)
	_, token := f.visitor(t, "sub-jo", "jo@corp.example")
	rec := f.do(t, token, http.MethodPost, f.projectMessagePath(f.agent.Slug), visitorTextBody("first contact"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	visitorSetExperiment(t, f.srv, false)
	rec = f.do(t, token, http.MethodPost, f.projectMessagePath(f.agent.Slug), visitorTextBody("experiment off"))
	assert.NotEqual(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Len(t, f.dispatcher.getCalls(), 1)
}

// serveChatRouteAs calls s.chatRoute directly, without routeGuard, with a
// handler that records whether it ran.
func serveChatRouteAs(srv *Server, method, path string, identity Identity, cred CredentialContext) (*httptest.ResponseRecorder, bool) {
	called := false
	handler := srv.chatRoute(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(method, path, nil)
	ctx := contextWithIdentity(req.Context(), identity)
	ctx = contextWithCredentialContext(ctx, cred)
	rec := httptest.NewRecorder()
	handler(rec, req.WithContext(ctx))
	return rec, called
}

// chatRoute refuses a visitor on its own, without routeGuard, on every
// chat request that is not a sub-route open to visitors, with the uniform
// refusal and before the handler runs; the visitor's own DM history and
// read marker reach the handler.
func TestChatRoute_VisitorRefusedOutsideAdmittedSubRoutes(t *testing.T) {
	srv, _ := testServer(t)
	visitorID := tid("chat-route-visitor")
	visitor := NewVisitorIdentity(visitorID, visitorTestUserIssuer, "sub", "v@corp.com", "V")
	cred := credentialContextForIdentity(visitor)
	dmKey, err := messages.DMConversationKey("agent", tid("chat-route-agent"), "user", visitorID)
	require.NoError(t, err)

	for _, c := range []struct{ label, method, path string }{
		{"chat DM list", http.MethodGet, "/api/v1/chat/dms"},
		{"chat typing", http.MethodPost, chatDMPath(dmKey, "typing")},
		{"chat send", http.MethodPost, chatDMPath(dmKey, "messages")},
		{"chat unread count", http.MethodGet, "/api/v1/chat/unread-count"},
	} {
		rec, called := serveChatRouteAs(srv, c.method, c.path, visitor, cred)
		assert.False(t, called, "%s: the handler must not run", c.label)
		assertVisitorRefusal(t, rec, c.label)
	}

	for _, c := range []struct{ label, method, path string }{
		{"own DM history", http.MethodGet, chatDMPath(dmKey, "messages")},
		{"own read marker", http.MethodGet, chatDMPath(dmKey, "read")},
		{"set own read marker", http.MethodPost, chatDMPath(dmKey, "read")},
	} {
		rec, called := serveChatRouteAs(srv, c.method, c.path, visitor, cred)
		assert.True(t, called, "%s: the handler runs", c.label)
		assert.Equal(t, http.StatusOK, rec.Code, "%s", c.label)
	}
}

// A project member's chat requests through chatRoute are unchanged by the
// visitor check: every request above reaches the handler.
func TestChatRoute_MemberRequestsUnchanged(t *testing.T) {
	srv, _ := testServer(t)
	memberID := tid("chat-route-member")
	member := NewAuthenticatedUser(memberID, memberID+"@test.com", "Member", "member", string(ClientTypeWeb))
	cred := CredentialContext{Kind: CredentialKindInteractive}
	dmKey, err := messages.DMConversationKey("agent", tid("chat-route-agent"), "user", memberID)
	require.NoError(t, err)

	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/chat/dms"},
		{http.MethodPost, chatDMPath(dmKey, "typing")},
		{http.MethodPost, chatDMPath(dmKey, "messages")},
		{http.MethodGet, "/api/v1/chat/unread-count"},
		{http.MethodGet, chatDMPath(dmKey, "messages")},
		{http.MethodGet, chatDMPath(dmKey, "read")},
		{http.MethodPost, chatDMPath(dmKey, "read")},
	} {
		rec, called := serveChatRouteAs(srv, c.method, c.path, member, cred)
		assert.True(t, called, "%s %s: the handler runs", c.method, c.path)
		assert.Equal(t, http.StatusOK, rec.Code, "%s %s", c.method, c.path)
	}
}

// ruleVisitor adds a domain rule on the fixture's project and returns the
// rule, the account ID of a visitor the rule binds, and that visitor's
// issuer token.
func (f *visitorMessagingFixture) ruleVisitor(t *testing.T, subject, email string) (ProjectVisitorRuleView, string, string) {
	t.Helper()
	rule := f.addRuleOK(t, grantTestOtherIssuer, "any.example")
	token := f.token(t, grantTestOtherIssuer, subject, email, boolPtr(true))
	v := f.visitorThrough(t, token)
	require.NotNil(t, v, "the rule makes a visitor")
	require.Len(t, f.ruleBindings(t, f.projectID, rule.ID), 1)
	return rule, v.ID(), token
}

// A visitor created by a domain rule sends to an agent and reads its own
// DM like a visitor granted by email; removing the rule refuses both on the
// next request.
func TestVisitorMessaging_RuleVisitorAdmittedUntilRuleRemoved(t *testing.T) {
	f := newVisitorMessagingFixture(t)
	rule, visitorID, token := f.ruleVisitor(t, "sub-rua", "rua@any.example")
	dmKey, err := messages.DMConversationKey("agent", f.agent.ID, "user", visitorID)
	require.NoError(t, err)

	rec := f.do(t, token, http.MethodPost, f.projectMessagePath(f.agent.Slug), visitorTextBody("from a rule visitor"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = f.do(t, token, http.MethodGet, chatDMPath(dmKey, "messages"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = doRequestAsUser(t, f.srv, f.owner, http.MethodDelete, f.rulesPath()+"/"+rule.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	rec = f.do(t, token, http.MethodPost, f.projectMessagePath(f.agent.Slug), visitorTextBody("after rule removal"))
	assertVisitorRefusal(t, rec, "send after rule removal")
	rec = f.do(t, token, http.MethodGet, chatDMPath(dmKey, "messages"), nil)
	assertVisitorRefusal(t, rec, "DM read after rule removal")
	assert.Len(t, f.dispatcher.getCalls(), 1, "no refused request is dispatched")
}

// Revoking one rule visitor refuses its next send and next DM read, and
// the rule does not bind it again on those requests.
func TestVisitorMessaging_RuleVisitorRevokedStaysRefused(t *testing.T) {
	f := newVisitorMessagingFixture(t)
	rule, visitorID, token := f.ruleVisitor(t, "sub-rho", "rho@any.example")
	dmKey, err := messages.DMConversationKey("agent", f.agent.ID, "user", visitorID)
	require.NoError(t, err)

	rec := f.do(t, token, http.MethodPost, agentMessagePath(f.agent.ID), visitorTextBody("before"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = f.do(t, token, http.MethodGet, chatDMPath(dmKey, "messages"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = doRequestAsUser(t, f.srv, f.owner, http.MethodDelete, f.visitorsPath()+"/"+visitorID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	rec = f.do(t, token, http.MethodPost, agentMessagePath(f.agent.ID), visitorTextBody("after"))
	assertVisitorRefusal(t, rec, "send after revocation")
	rec = f.do(t, token, http.MethodGet, chatDMPath(dmKey, "messages"), nil)
	assertVisitorRefusal(t, rec, "DM read after revocation")
	assert.Empty(t, f.ruleBindings(t, f.projectID, rule.ID), "the rule does not bind the revoked visitor again")
	assert.Len(t, f.dispatcher.getCalls(), 1, "no refused request is dispatched")
}
