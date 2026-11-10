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
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Posts to an agent by free-text thread_id (no conversation_id). The sender
// is decided before any other answer that depends on the recipient and
// before any row is written: a user must be able to read the recipient's
// project, an agent goes through its message mode, and any other sender is
// refused. A refusal answers exactly as an unknown recipient agent does.

// threadRequest is a message to target naming threadID, on the web channel
// (a thread_id needs a channel).
func threadRequest(target *store.Agent, threadID string) MessageRequest {
	return MessageRequest{StructuredMessage: &messages.StructuredMessage{
		Version:     messages.Version,
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
		Recipient:   "agent:" + target.Slug,
		RecipientID: target.ID,
		Msg:         "to a thread",
		Type:        messages.TypeInstruction,
		ThreadID:    threadID,
		Channel:     "web",
	}}
}

// postThreadRequestAs posts req to target through the agent message
// handler, as sender.
func postThreadRequestAs(t *testing.T, srv *Server, sender Identity, target *store.Agent, req MessageRequest) refAnswer {
	t.Helper()
	return postAgentMessageAs(t, srv,
		func(c context.Context) context.Context { return contextWithIdentity(c, sender) },
		target, req)
}

// postThreadMessageAs posts a message to target naming threadID, as sender.
func postThreadMessageAs(t *testing.T, srv *Server, sender Identity, target *store.Agent, threadID string) refAnswer {
	t.Helper()
	return postThreadRequestAs(t, srv, sender, target, threadRequest(target, threadID))
}

// unknownAgentLike returns an agent record that is not stored, with the
// same slug and project as a, for the unknown-recipient answer.
func unknownAgentLike(a *store.Agent) *store.Agent {
	return &store.Agent{ID: tid("thread-sender-unknown-agent"), Slug: a.Slug, Name: a.Name, ProjectID: a.ProjectID}
}

// threadConversationAbsent asserts that no conversation exists for threadID
// in projectID.
func threadConversationAbsent(t *testing.T, s store.Store, projectID, threadID string) {
	t.Helper()
	extRef, err := messaging.ThreadConversationExternalRef(projectID, threadID)
	require.NoError(t, err)
	_, lookupErr := s.GetConversationByExternalRef(context.Background(), "native", extRef)
	assert.True(t, errors.Is(lookupErr, store.ErrNotFound), "no thread conversation is created (err=%v)", lookupErr)
}

// threadConversationPresent asserts that the conversation for threadID in
// projectID exists.
func threadConversationPresent(t *testing.T, s store.Store, projectID, threadID string) {
	t.Helper()
	extRef, err := messaging.ThreadConversationExternalRef(projectID, threadID)
	require.NoError(t, err)
	_, lookupErr := s.GetConversationByExternalRef(context.Background(), "native", extRef)
	assert.NoError(t, lookupErr, "the thread conversation exists")
}

// agentMessageCount counts the stored messages involving agentID.
func agentMessageCount(t *testing.T, s store.Store, agentID string) int {
	t.Helper()
	res, err := s.ListMessages(context.Background(), store.MessageFilter{AgentID: agentID}, store.ListOptions{Limit: 200})
	require.NoError(t, err)
	return len(res.Items)
}

func userIdentityOf(u *store.User) UserIdentity {
	return NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, string(ClientTypeWeb))
}

func TestAgentMessageThread_UserWithoutProjectReadMatchesUnknownAgent(t *testing.T) {
	f := acceptanceSetup(t)
	// ownerA has no role in project B.
	userA := userIdentityOf(f.ownerA)
	require.False(t, f.srv.canReadProject(context.Background(), userA, f.projectB), "precondition: the sender cannot read project B")
	threadID := "thread-sender-no-read"
	before := agentMessageCount(t, f.store, f.hubAgentB.ID)

	unknown := postThreadMessageAs(t, f.srv, userA, unknownAgentLike(f.hubAgentB), threadID)
	require.Equal(t, http.StatusNotFound, unknown.status, unknown.body)

	got := postThreadMessageAs(t, f.srv, userA, f.hubAgentB, threadID)
	requireSameAnswer(t, unknown, got)
	threadConversationAbsent(t, f.store, f.projectB, threadID)
	assert.Equal(t, before, agentMessageCount(t, f.store, f.hubAgentB.ID), "no message row is written")
}

func TestAgentMessageThread_RefusalReasonLogged(t *testing.T) {
	logs := captureSlog(t)
	f := acceptanceSetup(t)
	userA := userIdentityOf(f.ownerA)

	logs.Reset()
	got := postThreadMessageAs(t, f.srv, userA, f.hubAgentB, "thread-sender-logged")
	require.Equal(t, http.StatusNotFound, got.status, "the post is refused as not found: %s", got.body)
	logged := logs.String()
	assert.Contains(t, logged, "authorization denied", "the refusal is logged as an authorization denial")
	assert.Contains(t, logged, "thread post: sender cannot read the project")
	assert.Contains(t, logged, f.ownerA.ID, "the log names the sender")
	assert.Contains(t, logged, f.projectB, "the log names the project")
	for _, word := range []string{"denied", "permission", "project", "thread"} {
		assert.NotContains(t, got.body, word, "the response carries no reason")
	}
}

func TestAgentMessageThread_UserWithProjectReadPosts(t *testing.T) {
	f := acceptanceSetup(t)
	userB := userIdentityOf(f.ownerB)
	threadID := "thread-sender-reader"
	before := agentMessageCount(t, f.store, f.hubAgentB.ID)

	got := postThreadMessageAs(t, f.srv, userB, f.hubAgentB, threadID)
	require.Contains(t, []int{http.StatusOK, http.StatusAccepted}, got.status, got.body)
	threadConversationPresent(t, f.store, f.projectB, threadID)
	assert.Greater(t, agentMessageCount(t, f.store, f.hubAgentB.ID), before, "the message is stored")
}

func TestAgentMessageThread_AgentSenderUsesMessageMode(t *testing.T) {
	f := acceptanceSetup(t)

	// A hub-mode agent of project A posts into a thread of project B: its
	// message mode allows it, and no project read rule applies to it.
	hubA := accAgentIdentity(f.hubAgentA.ID, f.projectA, f.hubAgentA.Ancestry)
	allowedThread := "thread-sender-hub-agent"
	got := postThreadMessageAs(t, f.srv, hubA, f.hubAgentB, allowedThread)
	require.Contains(t, []int{http.StatusOK, http.StatusAccepted}, got.status, "the hub-mode agent's thread post is accepted: %s", got.body)
	threadConversationPresent(t, f.store, f.projectB, allowedThread)

	// A none-mode agent is refused by its message mode, with no row.
	noneA := accAgentIdentity(f.noneAgentA.ID, f.projectA, f.noneAgentA.Ancestry)
	refusedThread := "thread-sender-none-agent"
	before := agentMessageCount(t, f.store, f.projectAgentA.ID)
	refused := postThreadMessageAs(t, f.srv, noneA, f.projectAgentA, refusedThread)
	require.Equal(t, http.StatusForbidden, refused.status, refused.body)
	assert.Contains(t, refused.body, ErrCodeMessageDenied)
	threadConversationAbsent(t, f.store, f.projectA, refusedThread)
	assert.Equal(t, before, agentMessageCount(t, f.store, f.projectAgentA.ID), "no message row is written")
}

func TestAgentMessageThread_OtherSenderKindRefused(t *testing.T) {
	logs := captureSlog(t)
	f := acceptanceSetup(t)
	broker := NewBrokerIdentity(tid("thread-sender-broker"))
	threadID := "thread-sender-broker"
	before := agentMessageCount(t, f.store, f.hubAgentB.ID)

	unknown := postThreadMessageAs(t, f.srv, broker, unknownAgentLike(f.hubAgentB), threadID)
	require.Equal(t, http.StatusNotFound, unknown.status, unknown.body)

	logs.Reset()
	got := postThreadMessageAs(t, f.srv, broker, f.hubAgentB, threadID)
	requireSameAnswer(t, unknown, got)
	assert.Contains(t, logs.String(), "thread post: unsupported sender kind")
	threadConversationAbsent(t, f.store, f.projectB, threadID)
	assert.Equal(t, before, agentMessageCount(t, f.store, f.hubAgentB.ID), "no message row is written")
}

// agentInStateB stores a hub-mode agent of project B in phase, on brokerID.
func agentInStateB(t *testing.T, f acceptanceFixture, name, phase, brokerID string) *store.Agent {
	t.Helper()
	a := &store.Agent{
		ID: tid("thread-state-" + name), Slug: "thread-state-" + name, Name: "thread-state-" + name,
		ProjectID: f.projectB, MessageMode: store.MessageModeHub, Ancestry: []string{f.ownerB.ID},
		Phase: phase, RuntimeBrokerID: brokerID, Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, f.store.CreateAgent(context.Background(), a))
	return a
}

// TestAgentMessageThread_RefusalMatchesUnknownAgentInEveryState: for every
// recipient state that has its own answer (each phase answered with 409, and
// a broker that is not reachable, answered with 503), a sender who cannot
// read the project gets exactly the unknown-agent answer instead, while a
// reader still gets the state's own answer.
func TestAgentMessageThread_RefusalMatchesUnknownAgentInEveryState(t *testing.T) {
	f := acceptanceSetup(t)
	ctx := context.Background()
	userA := userIdentityOf(f.ownerA)
	userB := userIdentityOf(f.ownerB)

	offline := &store.RuntimeBroker{ID: tid("thread-state-offline-broker"), Name: "Offline Broker",
		Slug: "thread-state-offline-broker", Status: store.BrokerStatusOffline}
	require.NoError(t, f.store.CreateRuntimeBroker(ctx, offline))

	rows := []struct {
		name, phase, broker string
		readerStatus        int
	}{
		{"stopped", "stopped", f.hubAgentB.RuntimeBrokerID, http.StatusConflict},
		{"suspended", "suspended", f.hubAgentB.RuntimeBrokerID, http.StatusConflict},
		{"halted", "halted", f.hubAgentB.RuntimeBrokerID, http.StatusConflict},
		{"error", "error", f.hubAgentB.RuntimeBrokerID, http.StatusConflict},
		{"starting", "starting", f.hubAgentB.RuntimeBrokerID, http.StatusConflict},
		{"broker-unreachable", "running", offline.ID, http.StatusServiceUnavailable},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			target := agentInStateB(t, f, row.name, row.phase, row.broker)
			threadID := "thread-state-" + row.name

			reader := postThreadMessageAs(t, f.srv, userB, target, threadID)
			require.Equal(t, row.readerStatus, reader.status, "a reader gets the state's own answer: %s", reader.body)

			unknown := postThreadMessageAs(t, f.srv, userA, unknownAgentLike(target), threadID)
			require.Equal(t, http.StatusNotFound, unknown.status, unknown.body)
			requireSameAnswer(t, unknown, postThreadMessageAs(t, f.srv, userA, target, threadID))
			threadConversationAbsent(t, f.store, f.projectB, threadID)
		})
	}
}

// grantMessageAndLifecycle binds userID, in projectID, to a role that holds
// agent.message and agent.lifecycle and nothing else (no project read).
func grantMessageAndLifecycle(t *testing.T, s store.Store, userID, projectID string) {
	t.Helper()
	ctx := context.Background()
	rd, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        "test-thread-message-lifecycle",
		Description: "Test role: agent.message and agent.lifecycle only",
		ScopeType:   store.RoleScopeProject,
		Permissions: []string{"agent.message", "agent.lifecycle"},
	})
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
}

// startCallsFor counts the recorded resume (start) dispatches of agentID.
func startCallsFor(f acceptanceFixture, agentID string) int {
	n := 0
	for _, c := range f.dispatcher.getStartCalls() {
		if c.AgentID == agentID {
			n++
		}
	}
	return n
}

// TestAgentMessageThread_RefusalDoesNotWake: a refused post that asks to
// wake a suspended agent gets the unknown-agent answer and attempts no
// resume, even from a sender who may message and resume the agent but
// cannot read its project. A reader's post with wake does resume it.
func TestAgentMessageThread_RefusalDoesNotWake(t *testing.T) {
	f := acceptanceSetup(t)
	ctx := context.Background()
	prevReady := wakeReadyTimeout
	wakeReadyTimeout = 2 * time.Second
	t.Cleanup(func() { wakeReadyTimeout = prevReady })

	grantMessageAndLifecycle(t, f.store, f.ownerA.ID, f.projectB)
	userA := userIdentityOf(f.ownerA)
	target := agentInStateB(t, f, "wake", "suspended", f.hubAgentB.RuntimeBrokerID)
	require.False(t, f.srv.canReadProject(ctx, userA, f.projectB), "precondition: the sender cannot read project B")
	require.True(t, f.srv.agentLifecycleAllowed(ctx, userA, target), "precondition: the sender may resume the agent")
	allowed, reason, _ := f.srv.authorizeAgentMessage(ctx, userA, target, false)
	require.True(t, allowed, "precondition: the sender may message the agent: %s", reason)

	wakeRequest := func(a *store.Agent, threadID string) MessageRequest {
		req := threadRequest(a, threadID)
		req.Wake = true
		return req
	}
	threadID := "thread-state-wake"
	f.dispatcher.reset()
	unknown := postThreadRequestAs(t, f.srv, userA, unknownAgentLike(target), wakeRequest(unknownAgentLike(target), threadID))
	require.Equal(t, http.StatusNotFound, unknown.status, unknown.body)
	requireSameAnswer(t, unknown, postThreadRequestAs(t, f.srv, userA, target, wakeRequest(target, threadID)))
	assert.Equal(t, 0, startCallsFor(f, target.ID), "no resume is attempted")
	got, err := f.store.GetAgent(ctx, target.ID)
	require.NoError(t, err)
	assert.Equal(t, "suspended", got.Phase, "the agent stays suspended")
	threadConversationAbsent(t, f.store, f.projectB, threadID)

	// Reader control: the project owner's post with wake resumes the agent.
	readerTarget := agentInStateB(t, f, "wake-reader", "suspended", f.hubAgentB.RuntimeBrokerID)
	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = f.store.UpdateAgentStatus(context.Background(), readerTarget.ID, store.AgentStatusUpdate{
			Phase: "running", Activity: "idle",
		})
	}()
	reader := postThreadRequestAs(t, f.srv, userIdentityOf(f.ownerB), readerTarget, wakeRequest(readerTarget, "thread-state-wake-reader"))
	assert.Contains(t, []int{http.StatusOK, http.StatusAccepted}, reader.status, "a reader's post with wake is accepted: %s", reader.body)
	assert.Equal(t, 1, startCallsFor(f, readerTarget.ID), "a reader's post with wake resumes the agent")
}

// TestAgentMessageThread_ChatIntegrationThreadPostWritesNoConversation: a
// chat integration (broker) post that names a thread_id is refused before
// its chat reference is resolved, so no conversation row is written for the
// reference.
func TestAgentMessageThread_ChatIntegrationThreadPostWritesNoConversation(t *testing.T) {
	f := acceptanceSetup(t)
	broker := NewBrokerIdentity(tid("thread-sender-chat-broker"))
	const surface = "gchat"
	const externalRef = "spaces/thread-sender/threads/t1"
	threadID := "thread-sender-chat"
	request := func(target *store.Agent) MessageRequest {
		req := threadRequest(target, threadID)
		req.Surface = surface
		req.ExternalRef = externalRef
		return req
	}

	unknown := postThreadRequestAs(t, f.srv, broker, unknownAgentLike(f.hubAgentB), request(unknownAgentLike(f.hubAgentB)))
	require.Equal(t, http.StatusNotFound, unknown.status, unknown.body)
	requireSameAnswer(t, unknown, postThreadRequestAs(t, f.srv, broker, f.hubAgentB, request(f.hubAgentB)))
	_, lookupErr := f.store.GetConversationByExternalRef(context.Background(), surface, externalRef)
	assert.True(t, errors.Is(lookupErr, store.ErrNotFound), "no chat integration conversation is created (err=%v)", lookupErr)
	threadConversationAbsent(t, f.store, f.projectB, threadID)
}

// TestAgentMessageThread_RouteRefusalMatchesUnknownAgent: through the
// route (authentication, then authorizeAgentMessage), a user who may message
// the agent but cannot read its project gets exactly the route's answer for
// an unknown agent ID, and no row is written.
func TestAgentMessageThread_RouteRefusalMatchesUnknownAgent(t *testing.T) {
	f := newRefFixture(t)
	ctx := context.Background()
	target := &store.Agent{ID: tid("thread-route-agent"), ProjectID: f.projA.ID, Name: "thread-route", Slug: "thread-route",
		MessageMode: store.MessageModeProject, Phase: "running", OwnerID: f.ua.ID, CreatedBy: f.ua.ID}
	require.NoError(t, f.st.CreateAgent(ctx, target))
	msgAuthzGrantAgentMessage(t, f.st, f.uc.ID, f.projA.ID)
	carol := userIdentityOf(f.uc)
	require.False(t, f.srv.canReadProject(ctx, carol, f.projA.ID), "precondition: carol cannot read project A")
	allowed, reason, _ := f.srv.authorizeAgentMessage(ctx, carol, target, false)
	require.True(t, allowed, "precondition: carol may message the agent: %s", reason)
	threadID := "thread-route"

	post := func(id string) refAnswer {
		rec := doRequestAsUser(t, f.srv, f.uc, http.MethodPost, "/api/v1/agents/"+id+"/message", threadRequest(target, threadID))
		return refAnswer{status: rec.Code, body: rec.Body.String()}
	}
	unknown := post(tid("thread-route-unknown-agent"))
	require.Equal(t, http.StatusNotFound, unknown.status, unknown.body)
	requireSameAnswer(t, unknown, post(target.ID))
	threadConversationAbsent(t, f.st, f.projA.ID, threadID)
	assert.Equal(t, 0, agentMessageCount(t, f.st, target.ID), "no message row is written")
}

// TestAgentMessageThread_KeyKindsAreDirectOrGroup pins the conversation key
// kinds decideKeyedPostSender has rules for: every kind
// messaging.DeriveConversationKey returns is direct or group. A new kind
// fails here and needs a rule there (until then it is refused).
func TestAgentMessageThread_KeyKindsAreDirectOrGroup(t *testing.T) {
	projectID := tid("thread-kind-project")
	userID := tid("thread-kind-user")
	agentID := tid("thread-kind-agent")
	otherAgentID := tid("thread-kind-other-agent")
	dmKey, err := messages.DMConversationKey("user", userID, "agent", agentID)
	require.NoError(t, err)
	cases := []struct {
		name string
		in   messaging.KeyInputs
		want string
	}{
		{"dm thread_id", messaging.KeyInputs{ThreadID: dmKey, ProjectID: projectID, SenderKind: "user", SenderID: userID, RecipientKind: "agent", RecipientID: agentID}, "direct"},
		{"free-text thread_id", messaging.KeyInputs{ThreadID: "plan", ProjectID: projectID, SenderKind: "user", SenderID: userID, RecipientKind: "agent", RecipientID: agentID}, "group"},
		{"free-text thread_id from an agent", messaging.KeyInputs{ThreadID: "plan", ProjectID: projectID, SenderKind: "agent", SenderID: otherAgentID, RecipientKind: "agent", RecipientID: agentID}, "group"},
		{"user to agent", messaging.KeyInputs{ProjectID: projectID, SenderKind: "user", SenderID: userID, RecipientKind: "agent", RecipientID: agentID}, "direct"},
		{"agent to agent", messaging.KeyInputs{ProjectID: projectID, SenderKind: "agent", SenderID: otherAgentID, RecipientKind: "agent", RecipientID: agentID}, "direct"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, kind, _, err := messaging.DeriveConversationKey(c.in)
			require.NoError(t, err)
			assert.Contains(t, []string{"direct", "group"}, kind, "a key kind with no rule in decideKeyedPostSender")
			assert.Equal(t, c.want, kind)
		})
	}
}

// TestAgentMessageThread_VisitorRefusalIsTheVisitorAnswer: a refused post
// from a federation visitor gets the one visitor refusal, which is also its
// answer for an unknown agent. Through the route, a visitor's thread post
// never reaches the sender decision: thread_id is not a visitor field, so it
// is refused first, by name, and nothing is dispatched. The refusal helper
// is therefore checked directly for a visitor.
func TestAgentMessageThread_VisitorRefusalIsTheVisitorAnswer(t *testing.T) {
	vf := newVisitorMessagingFixture(t)
	_, token := vf.visitor(t, "sub-thread", "thread-visitor@corp.example")
	for _, path := range []string{agentMessagePath(vf.agent.ID), vf.projectMessagePath(vf.agent.Slug)} {
		rec := vf.do(t, token, http.MethodPost, path, map[string]interface{}{
			"structured_message": &messages.StructuredMessage{Msg: "hi", Type: messages.TypeInstruction, ThreadID: "thread-visitor"},
		})
		require.Equal(t, http.StatusBadRequest, rec.Code, "%s: %s", path, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "structured_message.thread_id is not available to visitors", path)
	}
	assert.Empty(t, vf.dispatcher.getCalls(), "no visitor thread post is dispatched")

	f := acceptanceSetup(t)
	visitor := NewVisitorIdentity(tid("thread-visitor"), "https://issuer.example", "sub", "v@example.com", "V")
	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+f.hubAgentB.ID+"/message", nil)
	r = r.WithContext(contextWithIdentity(r.Context(), visitor))
	got := httptest.NewRecorder()
	f.srv.refuseKeyedPostAsNotFound(got, r, f.projectB, "thread post: test")
	want := httptest.NewRecorder()
	writeVisitorRefusal(want)
	requireSameAnswer(t, refAnswer{status: want.Code, body: want.Body.String()}, refAnswer{status: got.Code, body: got.Body.String()})
}

// emptyProjectAgentStore answers GetAgent for agentID with an empty
// ProjectID while its switch is armed, the record the ent store never
// returns but other hub code defends against.
type emptyProjectAgentStore struct {
	store.Store
	fault   *storeFaultSwitch
	agentID string
}

func (e *emptyProjectAgentStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	a, err := e.Store.GetAgent(ctx, id)
	if err == nil && a != nil && id == e.agentID && e.fault.Active() {
		cp := *a
		cp.ProjectID = ""
		return &cp, nil
	}
	return a, err
}

// TestAgentMessageThread_RecipientWithoutProjectRefusesEverySender: a
// thread post to an agent record with no project (its thread key cannot be
// derived) is refused for a user and for an agent with the unknown-agent
// answer, and writes no conversation or message row.
func TestAgentMessageThread_RecipientWithoutProjectRefusesEverySender(t *testing.T) {
	logs := captureSlog(t)
	targetID := tid("thread-empty-project-agent")
	srv, s, _, fault := testServerWithStoreFault(t, func(inner store.Store, fault *storeFaultSwitch) *emptyProjectAgentStore {
		return &emptyProjectAgentStore{Store: inner, fault: fault, agentID: targetID}
	})
	ctx := context.Background()
	project := &store.Project{ID: tid("thread-empty-project-project"), Name: "Empty Project Case", Slug: "thread-empty-project",
		Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, project))
	user := &store.User{ID: tid("thread-empty-project-user"), Email: "empty-project@test.com", DisplayName: "EP",
		Role: store.UserRoleMember, Status: "active", Created: time.Now()}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)
	target := &store.Agent{ID: targetID, Slug: "thread-empty-project-agent", Name: "thread-empty-project-agent",
		ProjectID: project.ID, MessageMode: store.MessageModeHub, Phase: "running", Created: time.Now(), Updated: time.Now()}
	sender := &store.Agent{ID: tid("thread-empty-project-sender"), Slug: "thread-empty-project-sender", Name: "thread-empty-project-sender",
		ProjectID: project.ID, MessageMode: store.MessageModeHub, Phase: "running", Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateAgent(ctx, target))
	require.NoError(t, s.CreateAgent(ctx, sender))
	fault.Arm()
	loaded, err := srv.store.GetAgent(ctx, target.ID)
	require.NoError(t, err)
	require.Empty(t, loaded.ProjectID, "precondition: the recipient record has no project")

	senders := []struct {
		name     string
		identity Identity
	}{
		{"user", userIdentityOf(user)},
		{"agent", accAgentIdentity(sender.ID, project.ID, nil)},
		{"broker", NewBrokerIdentity(tid("thread-empty-project-broker"))},
	}
	for _, c := range senders {
		t.Run(c.name, func(t *testing.T) {
			threadID := "thread-empty-project-" + c.name
			unknown := postThreadMessageAs(t, srv, c.identity, unknownAgentLike(target), threadID)
			require.Equal(t, http.StatusNotFound, unknown.status, unknown.body)
			logs.Reset()
			requireSameAnswer(t, unknown, postThreadMessageAs(t, srv, c.identity, target, threadID))
			logged := logs.String()
			assert.Contains(t, logged, "authorization denied", "the refusal is logged as an authorization denial")
			assert.Contains(t, logged, "thread post: conversation key not derived")
			assert.Contains(t, logged, c.identity.ID(), "the log names the sender")
			convs, err := s.ListConversations(ctx, store.ConversationFilter{ProjectID: project.ID}, store.ListOptions{Limit: 50})
			require.NoError(t, err)
			assert.Empty(t, convs.Items, "no conversation row is written")
			assert.Equal(t, 0, agentMessageCount(t, s, target.ID), "no message row is written")
		})
	}
}
