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
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// adtCrossProjectOptIn makes a cross-project send from actor into P2 pass
// the cross-project gates an agent send applies: the hub
// cross_project_messaging_enabled setting is on, actor is in hub message
// mode, and P2 accepts any inbound agent message.
func (f *adtFixture) adtCrossProjectOptIn(t *testing.T, actor *store.Agent) {
	t.Helper()
	enableCrossProjectMessaging(t, f.srv)
	f.mutateAgent(t, actor.ID, func(a *store.Agent) { a.MessageMode = store.MessageModeHub })
	adtSetInbound(t, f.store, f.other.ID, store.CrossProjectInboundAny)
}

// adtSetInbound sets projectID's cross-project inbound policy.
func adtSetInbound(t *testing.T, s store.Store, projectID, policy string) {
	t.Helper()
	ctx := context.Background()
	p, err := s.GetProject(ctx, projectID)
	require.NoError(t, err)
	_, err = s.UpdateProjectMessagingPolicy(ctx, projectID, policy, p.CrossProjectInboundRevision)
	require.NoError(t, err)
}

// adtSetCrossProjectEnabled replaces the server's operational settings with
// ones whose hub cross_project_messaging_enabled setting is on (true) or off
// (false).
func adtSetCrossProjectEnabled(t *testing.T, srv *Server, on bool) {
	t.Helper()
	fake := newFakeHubSettingStore()
	fake.seed("messaging", json.RawMessage(fmt.Sprintf(`{"cross_project_messaging_enabled": %t}`, on)))
	ops := NewOperationalSettings(fake, emptyKoanf(), emptyKoanf())
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)
	srv.SetOperationalSettings(ops)
}

// adtGrantProjectPermissions binds userID in projectID to a custom project
// role holding perms (a user keeps its one built-in membership role) and
// returns the binding ID.
func adtGrantProjectPermissions(t *testing.T, s store.Store, userID, projectID string, perms ...string) string {
	t.Helper()
	ctx := context.Background()
	rd, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        "adt-custom-" + tid(userID+projectID+t.Name()),
		Description: "agent delegation test role",
		ScopeType:   store.RoleScopeProject,
		Permissions: perms,
	})
	require.NoError(t, err)
	rb, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
	return rb.ID
}

// adtMessageWorld readies the fixture for delegated message tests: agent B
// in P2 is running on the fixture's online broker, Alice holds agent.message
// in P2 through a custom role, and dispatches are recorded. A send from A
// (P1) to B (P2) is cross-project, so it passes the cross-project gates only
// because the world opts in explicitly (adtCrossProjectOptIn: the hub
// setting on, A in hub mode, P2 accepting any inbound agent message). It
// returns the recording dispatcher.
func (f *adtFixture) adtMessageWorld(t *testing.T) *recordingDispatcher {
	t.Helper()
	adtGrantProjectPermissions(t, f.store, f.alice.ID, f.other.ID, "agent.message")
	f.adtCrossProjectOptIn(t, f.agentA)
	f.mutateAgent(t, f.agentB.ID, func(a *store.Agent) { a.Phase = "running"; a.RuntimeBrokerID = f.broker.ID })
	d := &recordingDispatcher{}
	f.srv.SetDispatcher(d)
	return d
}

func (f *adtFixture) sendMessage(t *testing.T, token, path string, body map[string]interface{}) *httptest.ResponseRecorder {
	t.Helper()
	return f.do(t, http.MethodPost, path, body, adtBearer(token))
}

func adtText(text string) map[string]interface{} {
	return map[string]interface{}{"message": text}
}

// TestAgentDelegatedMessage_SendsAsTheAgent: with a hub grant for
// agent:message, agent A messages agent B in P2 through the by-ID route and
// the project route (by slug and by ID). Each message is stored and
// dispatched with the agent as sender: Sender agent:<A's slug>, SenderID
// A's ID, never Alice.
func TestAgentDelegatedMessage_SendsAsTheAgent(t *testing.T) {
	f := newADTFixture(t, "adt-msg")
	ctx := context.Background()
	dispatcher := f.adtMessageWorld(t)
	grant := f.adtIssueGrant(t, map[string]string{"kind": "hub"}, "agent:message")
	cred := f.delegated(t, grant.ID)

	for _, path := range []string{
		agentMessagePath(f.agentB.ID),
		"/api/v1/projects/" + f.other.ID + "/agents/" + f.agentB.Slug + "/message",
		"/api/v1/projects/" + f.other.ID + "/agents/" + f.agentB.ID + "/message",
	} {
		rec := f.sendMessage(t, cred.Token, path, adtText("hello from A"))
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", path, rec.Body.String())
		assert.NotContains(t, rec.Body.String(), delegatedCredentialPrefix)
	}

	calls := dispatcher.getCalls()
	require.Len(t, calls, 3)
	for _, c := range calls {
		require.NotNil(t, c.StructuredMessage)
		assert.Equal(t, f.agentB.ID, c.Agent.ID)
		assert.Equal(t, "agent:"+f.agentA.Slug, c.StructuredMessage.Sender)
		assert.Equal(t, f.agentA.ID, c.StructuredMessage.SenderID)
	}
	stored, err := f.store.ListMessages(ctx, store.MessageFilter{RecipientID: f.agentB.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, stored.Items, 3)
	for _, m := range stored.Items {
		assert.Equal(t, "agent:"+f.agentA.Slug, m.Sender, "the stored sender is the agent")
		assert.Equal(t, f.agentA.ID, m.SenderID)
		assert.NotEqual(t, f.alice.ID, m.SenderID, "the issuer is never the sender")
		if assert.NotNil(t, m.SenderProjectID, "an agent-sender row carries the sender's project") {
			assert.Equal(t, f.agentA.ProjectID, *m.SenderProjectID)
		}
		if assert.NotNil(t, m.RecipientProjectID) {
			assert.Equal(t, f.agentB.ProjectID, *m.RecipientProjectID)
		}
		// §14.5: the issuer is recorded as the authorizing user, with the
		// grant, and both are part of the message's API shape.
		assert.Equal(t, f.alice.ID, m.AuthorizingUserID, "the issuer authorized the send")
		assert.Equal(t, grant.ID, m.DelegationGrantID)
		body, err := json.Marshal(m)
		require.NoError(t, err)
		var shape map[string]interface{}
		require.NoError(t, json.Unmarshal(body, &shape))
		assert.Equal(t, f.alice.ID, shape["authorizingUserId"])
		assert.Equal(t, grant.ID, shape["delegationGrantId"])
	}
}

// TestAgentDelegatedMessage_AttributionOnlyOnDelegatedSends: a message the
// agent sends with its own agent token, and one a user sends, carry no
// authorizing user and no grant; the fields are left out of the API shape.
func TestAgentDelegatedMessage_AttributionOnlyOnDelegatedSends(t *testing.T) {
	f := newADTFixture(t, "adt-msg-attr")
	ctx := context.Background()
	f.adtMessageWorld(t)
	f.mutateAgent(t, f.agentC.ID, func(a *store.Agent) { a.Phase = "running"; a.RuntimeBrokerID = f.broker.ID })

	rec := f.do(t, http.MethodPost, agentMessagePath(f.agentB.ID), adtText("own token"),
		map[string]string{"X-Scion-Agent-Token": f.agentJWT(t, f.agentA)})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = f.sendMessage(t, f.session(t, f.carol), agentMessagePath(f.agentC.ID), adtText("from a user"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	for _, recipient := range []string{f.agentB.ID, f.agentC.ID} {
		stored, err := f.store.ListMessages(ctx, store.MessageFilter{RecipientID: recipient}, store.ListOptions{Limit: 10})
		require.NoError(t, err)
		require.Len(t, stored.Items, 1, recipient)
		m := stored.Items[0]
		assert.Empty(t, m.AuthorizingUserID)
		assert.Empty(t, m.DelegationGrantID)
		body, err := json.Marshal(m)
		require.NoError(t, err)
		assert.NotContains(t, string(body), "authorizingUserId")
		assert.NotContains(t, string(body), "delegationGrantId")
	}
}

// TestAgentDelegatedMessageAndAttach_OneRecordPerCheckAndNoReason: each
// delegated message send and each attach handshake is recorded once, with
// the grant, even with allow sampling at 0, and a denied one answers
// without the decision's reason.
func TestAgentDelegatedMessageAndAttach_OneRecordPerCheckAndNoReason(t *testing.T) {
	f := newADTFixture(t, "adt-ma-records")
	f.adtMessageWorld(t)
	e, _ := f.adtAttachTarget(t, "adt-ma-records")
	decisions := f.captureDecisions(t)
	f.srv.authzService.DecisionAuditSampleRate = 0
	t.Cleanup(func() { f.srv.authzService.DecisionAuditSampleRate = 1.0 })
	grant := f.adtIssueGrant(t, map[string]string{"kind": "hub"}, "agent:message", "agent:attach")
	cred := f.delegated(t, grant.ID)

	for name, request := range map[string]func() *httptest.ResponseRecorder{
		"message": func() *httptest.ResponseRecorder {
			return f.sendMessage(t, cred.Token, agentMessagePath(f.agentB.ID), adtText("x"))
		},
		"attach": func() *httptest.ResponseRecorder {
			return f.do(t, http.MethodGet, adtPTYPath(e.ID), nil, adtBearer(cred.Token))
		},
	} {
		t.Run(name, func(t *testing.T) {
			decisions.reset()
			rec := request()
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			recs := decisions.delegated()
			require.Len(t, recs, 1, "one record for the delegated check")
			assert.Equal(t, "allow", recs[0].Result)
			assert.Equal(t, "agent_delegation:"+grant.ID, recs[0].MatchedGrant)
			assert.Equal(t, f.agentA.ID, recs[0].PrincipalID, "the actor is the agent")
			assert.NotContains(t, rec.Body.String(), "agent delegation")
		})
	}

	// Denied: the target is outside the issuer's authority (agent C).
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"message": f.sendMessage(t, cred.Token, agentMessagePath(f.agentC.ID), adtText("x")),
		"attach":  f.do(t, http.MethodGet, adtPTYPath(f.agentC.ID), nil, adtBearer(cred.Token)),
	} {
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", name, rec.Body.String())
		assert.NotContains(t, rec.Body.String(), "agent delegation", name)
		assert.NotContains(t, rec.Body.String(), agentDelegationCodeIssuerAuthority, name)
	}
}

// TestAgentDelegatedMessage_ChargedToTheAgentSendBudget: a delegated send
// is an agent send, so it shares the actor agent's send budget with the
// agent's own sends. With a budget of one send a minute, a delegated send
// after the agent's own send (and after another delegated send) answers
// 429 with Retry-After, and nothing more is dispatched or stored.
func TestAgentDelegatedMessage_ChargedToTheAgentSendBudget(t *testing.T) {
	ctx := context.Background()
	for _, first := range []string{"the agent's own send", "a delegated send"} {
		t.Run("after "+first, func(t *testing.T) {
			f := newADTFixture(t, "adt-msg-budget")
			dispatcher := f.adtMessageWorld(t)
			cred := f.delegated(t, f.adtIssueGrant(t, map[string]string{"kind": "hub"}, "agent:message").ID)
			f.srv.chatSendLimiter = newChatSendLimiterWithRates(map[chatSenderClass]float64{
				chatSenderHuman: 60,
				chatSenderAgent: 1,
			}, time.Now)
			if first == "the agent's own send" {
				f.limiterSpend(t)
			} else {
				require.Equal(t, http.StatusOK, f.sendMessage(t, cred.Token, agentMessagePath(f.agentB.ID), adtText("first")).Code)
			}
			calls := len(dispatcher.getCalls())
			stored, err := f.store.ListMessages(ctx, store.MessageFilter{RecipientID: f.agentB.ID}, store.ListOptions{Limit: 10})
			require.NoError(t, err)

			rec := f.sendMessage(t, cred.Token, agentMessagePath(f.agentB.ID), adtText("second"))
			adtAssertAPIError(t, rec, http.StatusTooManyRequests, ErrCodeRateLimited)
			assert.NotEmpty(t, rec.Header().Get("Retry-After"))
			assert.Len(t, dispatcher.getCalls(), calls, "the limited send was not dispatched")
			after, err := f.store.ListMessages(ctx, store.MessageFilter{RecipientID: f.agentB.ID}, store.ListOptions{Limit: 10})
			require.NoError(t, err)
			assert.Len(t, after.Items, len(stored.Items), "the limited send was not stored")
		})
	}
}

// limiterSpend spends agent A's send budget the way an agent's own send
// does (ExecuteAgentDM charges SenderAgent.ID as an agent sender).
func (f *adtFixture) limiterSpend(t *testing.T) {
	t.Helper()
	require.True(t, f.srv.chatSendLimiter.Allow(f.agentA.ID, chatSenderAgent).Allowed, "the budget starts with one send")
}

// TestSenderAgentProject_CoversDelegatedSenders: the sender project used
// for the stored SenderProjectID and for the cross-project observer
// sanitizing is the agent caller's project, or for a delegated credential
// the actor agent's project (from the request's loaded agent row, else the
// identity). A human caller has none.
func TestSenderAgentProject_CoversDelegatedSenders(t *testing.T) {
	f := newADTFixture(t, "adt-sender-project")
	cred := f.delegated(t, f.adtIssueGrant(t, map[string]string{"kind": "hub"}, "agent:read").ID)
	st := adtLoadedState(t, f, cred.Token)
	withState := contextWithDelegatedState(contextWithIdentity(context.Background(), st.identity), st)

	got, ok := senderAgentProject(withState)
	assert.True(t, ok)
	assert.Equal(t, f.agentA.ProjectID, got, "the actor agent's project from the loaded row")
	assert.NotEqual(t, f.agentB.ProjectID, got, "a send to B in P2 is cross-project, so its observer copy is sanitized")

	bare := adtTestIdentity("agent-x", "project-x")
	got, ok = senderAgentProject(contextWithIdentity(context.Background(), bare))
	assert.True(t, ok)
	assert.Equal(t, "project-x", got, "without loaded state, the identity's agent project")

	got, ok = senderAgentProject(contextWithIdentity(context.Background(), newAgentIdentityFromStore(f.agentC)))
	assert.True(t, ok)
	assert.Equal(t, f.agentC.ProjectID, got)

	got, ok = senderAgentProject(contextWithIdentity(context.Background(), authUser(f.alice)))
	assert.False(t, ok)
	assert.Empty(t, got)
	assert.Empty(t, senderAgentProjectID(context.Background()))
}

// TestAgentDelegatedMessage_ObserverCopyFollowsTheActorProject: through the
// real handler, a delegated send publishes the agent-to-agent observer copy
// to the target's project. A send from agent A (P1) to agent B (P2) is
// cross-project, so its single observer copy carries no body. The control,
// A to agent C in P1, keeps the body.
func TestAgentDelegatedMessage_ObserverCopyFollowsTheActorProject(t *testing.T) {
	f := newADTFixture(t, "adt-msg-observer")
	f.adtMessageWorld(t)
	adtGrantProjectPermissions(t, f.store, f.alice.ID, f.proj.ID, "agent.message")
	f.mutateAgent(t, f.agentC.ID, func(a *store.Agent) { a.Phase = "running"; a.RuntimeBrokerID = f.broker.ID })
	spy := &spyBrokerBus{}
	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)
	f.srv.SetMessageBrokerProxy(NewMessageBrokerProxy(spy, f.store, events,
		func() AgentDispatcher { return f.srv.GetDispatcher() }, slog.Default()))
	cred := f.delegated(t, f.adtIssueGrant(t, map[string]string{"kind": "hub"}, "agent:message").ID)

	observers := func(recipientSlug string) []*messages.StructuredMessage {
		var out []*messages.StructuredMessage
		for _, evt := range spy.getEvents() {
			if evt.msg != nil && evt.msg.ObserverOnly && evt.msg.Recipient == "agent:"+recipientSlug {
				out = append(out, evt.msg)
			}
		}
		return out
	}

	rec := f.sendMessage(t, cred.Token, agentMessagePath(f.agentB.ID), adtText("cross-project body"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	cross := observers(f.agentB.Slug)
	require.Len(t, cross, 1, "one observer copy for the cross-project send")
	assert.Equal(t, "agent:"+f.agentA.Slug, cross[0].Sender)
	assert.Empty(t, cross[0].Msg, "a cross-project observer copy carries no body")

	rec = f.sendMessage(t, cred.Token, agentMessagePath(f.agentC.ID), adtText("same-project body"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	same := observers(f.agentC.Slug)
	require.Len(t, same, 1, "one observer copy for the same-project send")
	assert.Equal(t, "same-project body", same[0].Msg, "a same-project observer copy keeps the body")
}

// TestAgentDelegatedMessage_DeliveryChecksAsForAnAgentSend: a delegated
// send gets the delivery checks the actor agent's own send gets
// (ExecuteAgentDM), before anything is written. A held target (still
// running) is refused with 409 agent_not_running like a suspended one, a
// stopped target with the agent send's phase refusal, and a message that
// cannot be stored with 500 and no dispatch; each refusal is the same as
// for agent A's own JWT send, and nothing is dispatched.
func TestAgentDelegatedMessage_DeliveryChecksAsForAnAgentSend(t *testing.T) {
	ctx := context.Background()
	f := newADTFixture(t, "adt-msg-held")
	dispatcher := f.adtMessageWorld(t)
	cred := f.delegated(t, f.adtIssueGrant(t, map[string]string{"kind": "hub"}, "agent:message").ID)
	agentToken := f.agentJWT(t, f.agentA)
	path := agentMessagePath(f.agentB.ID)
	require.Equal(t, http.StatusOK, f.sendMessage(t, cred.Token, path, adtText("before")).Code)

	sameRefusal := func(t *testing.T) {
		t.Helper()
		calls := len(dispatcher.getCalls())
		before, err := f.store.ListMessages(ctx, store.MessageFilter{RecipientID: f.agentB.ID}, store.ListOptions{Limit: 50})
		require.NoError(t, err)
		delegated := adtAssertAPIError(t, f.sendMessage(t, cred.Token, path, adtText("x")), http.StatusConflict, ErrCodeAgentNotRunning)
		own := adtAssertAPIError(t, f.do(t, http.MethodPost, path, adtText("x"), map[string]string{"X-Scion-Agent-Token": agentToken}),
			http.StatusConflict, ErrCodeAgentNotRunning)
		assert.Equal(t, own.Message, delegated.Message, "the same refusal as the agent's own send")
		assert.Equal(t, own.Details, delegated.Details)
		assert.Len(t, dispatcher.getCalls(), calls, "nothing refused was dispatched")
		after, err := f.store.ListMessages(ctx, store.MessageFilter{RecipientID: f.agentB.ID}, store.ListOptions{Limit: 50})
		require.NoError(t, err)
		assert.Len(t, after.Items, len(before.Items), "nothing refused was stored")
	}

	t.Run("stopped target", func(t *testing.T) {
		f.mutateAgent(t, f.agentB.ID, func(a *store.Agent) { a.Phase = "stopped" })
		defer func() { _ = f.mutateAgent(t, f.agentB.ID, func(a *store.Agent) { a.Phase = "running" }) }()
		sameRefusal(t)
	})
	t.Run("message that cannot be stored is not dispatched", func(t *testing.T) {
		f.faults.inject(t, adtFaults{createMessage: errors.New("message write failed")})
		defer f.faults.clear()
		calls := len(dispatcher.getCalls())
		delegated := adtAssertAPIError(t, f.sendMessage(t, cred.Token, path, adtText("x")), http.StatusInternalServerError, ErrCodeInternalError)
		own := adtAssertAPIError(t, f.do(t, http.MethodPost, path, adtText("x"), map[string]string{"X-Scion-Agent-Token": agentToken}),
			http.StatusInternalServerError, ErrCodeInternalError)
		assert.Equal(t, own.Message, delegated.Message, "the same refusal as the agent's own send")
		assert.Len(t, dispatcher.getCalls(), calls, "nothing unstored was dispatched")
	})
	t.Run("held target", func(t *testing.T) {
		_, err := f.store.CreateAgentHolds(ctx, []*store.AgentHold{{
			AgentID: f.agentB.ID, ProjectID: f.agentB.ProjectID, Cause: store.AgentHoldCauseOwnerAccessEnded,
			RootPrincipalType: store.AgentHoldRootUser, RootPrincipalID: f.bob.ID,
			Trigger: store.MembershipLossTriggerMemberRemove, ActorKind: "system", ActorID: "hub", CorrelationID: "test",
		}})
		require.NoError(t, err)
		held, err := f.store.HasActiveAgentHold(ctx, f.agentB.ID)
		require.NoError(t, err)
		require.True(t, held, "the target is held while still running")
		sameRefusal(t)
	})
}

// TestAgentDelegatedMessage_SuperAdminIssuerPiercesNoMode: a super-admin's
// hub grant lets its agent message an agent in another project, but the
// delegated sender pierces no message mode, although the super-admin
// itself would.
func TestAgentDelegatedMessage_SuperAdminIssuerPiercesNoMode(t *testing.T) {
	f := newADTFixture(t, "adt-msg-sa")
	dispatcher := f.adtMessageWorld(t)
	admin := hubAdminUser(t, f.bypassAgentsFixture)
	adminAgent, _ := f.createdAgent(t, f.create(t, authUser(admin), CreateAgentRequest{Name: "adt-msg-sa-s"}), "adt-msg-sa-s")
	f.adtCrossProjectOptIn(t, adminAgent) // the admin's agent sends across projects as an agent
	rec := f.issue(t, f.session(t, admin), adminAgent.ID, map[string]interface{}{
		"boundary": map[string]string{"kind": "hub"}, "permissions": []string{"agent:message"}, "name": "sa",
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var grant AgentDelegationGrantResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &grant))
	cred := f.credentialFor(t, adminAgent, grant.ID)

	rec = f.sendMessage(t, cred.Token, agentMessagePath(f.agentB.ID), adtText("from the admin's agent"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	calls := dispatcher.getCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, "agent:"+adminAgent.Slug, calls[0].StructuredMessage.Sender)

	for _, mode := range []string{store.MessageModeNone, store.MessageModeLineage, store.MessageModeBranch} {
		t.Run(mode, func(t *testing.T) {
			f.mutateAgent(t, f.agentB.ID, func(a *store.Agent) { a.MessageMode = mode })
			defer func() {
				_ = f.mutateAgent(t, f.agentB.ID, func(a *store.Agent) { a.MessageMode = store.MessageModeProject })
			}()
			rec := f.sendMessage(t, cred.Token, agentMessagePath(f.agentB.ID), adtText("blocked"))
			adtAssertAPIError(t, rec, http.StatusForbidden, ErrCodeMessageDenied)
			assert.Equal(t, http.StatusOK, requestAsIdentity(t, f.srv, authUser(admin), http.MethodPost,
				agentMessagePath(f.agentB.ID), adtText("the admin pierces it")).Code, "the issuer itself pierces %s", mode)
		})
	}
	assert.Len(t, dispatcher.getCalls(), 4, "only the allowed sends were dispatched")
}

// TestAgentDelegatedMessage_Denials covers the in-scope §17.2 flows for the
// message route: a grant without agent:message, a project-bounded grant on
// another project, the issuer losing agent.message on the target, a
// revoked or expired credential, the experiment off, and the response
// convention (no decision reason in the response).
func TestAgentDelegatedMessage_Denials(t *testing.T) {
	f := newADTFixture(t, "adt-msg-deny")
	ctx := context.Background()
	dispatcher := f.adtMessageWorld(t)
	decisions := f.captureDecisions(t)
	path := agentMessagePath(f.agentB.ID)

	t.Run("permission outside the ceiling", func(t *testing.T) {
		cred := f.delegated(t, f.hubGrant(t).ID) // agent:read only
		decisions.reset()
		rec := f.sendMessage(t, cred.Token, path, adtText("x"))
		adtAssertAPIError(t, rec, http.StatusForbidden, ErrCodeMessageDenied)
		assert.NotContains(t, rec.Body.String(), "agent delegation")
		decisions.assertDelegatedDeny(t, agentDelegationCodeOutsideCeiling)
	})
	t.Run("project-bounded grant on another project", func(t *testing.T) {
		adtGrantProjectPermissions(t, f.store, f.alice.ID, f.proj.ID, "agent.message")
		cred := f.delegated(t, f.adtIssueGrant(t, map[string]string{"kind": "project", "projectId": f.proj.ID}, "agent:message").ID)
		decisions.reset()
		adtAssertAPIError(t, f.sendMessage(t, cred.Token, path, adtText("x")), http.StatusForbidden, ErrCodeMessageDenied)
		decisions.assertDelegatedDeny(t, agentDelegationCodeOutsideBoundary)
		rec := f.sendMessage(t, cred.Token, "/api/v1/projects/"+f.other.ID+"/agents/"+f.agentB.Slug+"/message", adtText("x"))
		adtAssertAPIError(t, rec, http.StatusForbidden, ErrCodeForbidden)
		assert.NotContains(t, rec.Body.String(), "agent delegation")
	})
	t.Run("issuer loses agent.message on the target", func(t *testing.T) {
		g := newADTFixture(t, "adt-msg-lose")
		gd := g.adtMessageWorld(t)
		gdec := g.captureDecisions(t)
		cred := g.delegated(t, g.adtIssueGrant(t, map[string]string{"kind": "hub"}, "agent:message").ID)
		require.Equal(t, http.StatusOK, g.sendMessage(t, cred.Token, agentMessagePath(g.agentB.ID), adtText("x")).Code)
		bindings, err := g.store.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, g.alice.ID)
		require.NoError(t, err)
		for _, b := range bindings {
			if b.ScopeID == g.other.ID && b.ID != g.aliceP2Binding {
				require.NoError(t, g.store.DeleteRoleBinding(ctx, b.ID))
			}
		}
		gdec.reset()
		adtAssertAPIError(t, g.sendMessage(t, cred.Token, agentMessagePath(g.agentB.ID), adtText("x")), http.StatusForbidden, ErrCodeMessageDenied)
		recs := gdec.delegated()
		require.NotEmpty(t, recs)
		assert.Equal(t, "deny", recs[len(recs)-1].Result)
		assert.Equal(t, string(DeniedByAgentDelegation), recs[len(recs)-1].DeniedBy)
		assert.Len(t, gd.getCalls(), 1, "the refused send was not dispatched")
	})
	t.Run("revoked credential is refused at the next use", func(t *testing.T) {
		cred := f.delegated(t, f.adtIssueGrant(t, map[string]string{"kind": "hub"}, "agent:message").ID)
		require.Equal(t, http.StatusOK, f.sendMessage(t, cred.Token, path, adtText("x")).Code)
		row, err := f.store.GetAgentDelegatedCredentialByKeyHash(ctx, hashDelegatedCredential(cred.Token))
		require.NoError(t, err)
		_, err = f.store.RevokeAgentDelegatedCredential(ctx, row.ID, "test", time.Now())
		require.NoError(t, err)
		assert.Equal(t, http.StatusUnauthorized, f.sendMessage(t, cred.Token, path, adtText("x")).Code)
	})
	t.Run("experiment off refuses, back on works again", func(t *testing.T) {
		cred := f.delegated(t, f.adtIssueGrant(t, map[string]string{"kind": "hub"}, "agent:message").ID)
		adtSetExperiment(t, f.srv, false)
		assert.Equal(t, http.StatusUnauthorized, f.sendMessage(t, cred.Token, path, adtText("x")).Code)
		adtSetExperiment(t, f.srv, true)
		assert.Equal(t, http.StatusOK, f.sendMessage(t, cred.Token, path, adtText("x")).Code)
	})
	t.Run("missing agent answers the handler's own 404", func(t *testing.T) {
		cred := f.delegated(t, f.adtIssueGrant(t, map[string]string{"kind": "hub"}, "agent:message").ID)
		rec := f.sendMessage(t, cred.Token, agentMessagePath(tid("adt-msg-deny-missing")), adtText("x"))
		assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	})
	t.Run("nothing refused was dispatched", func(t *testing.T) {
		// Two allowed sends above (revocation and experiment cases).
		assert.Len(t, dispatcher.getCalls(), 2)
	})
}

// TestAgentDelegatedMessage_DirectMessageOnly: a delegated send is a direct
// text message to the route's own agent. Thread, conversation, mention,
// group, notification, interrupt and wake fields are refused before any
// lookup, write or dispatch.
func TestAgentDelegatedMessage_DirectMessageOnly(t *testing.T) {
	f := newADTFixture(t, "adt-msg-shape")
	dispatcher := f.adtMessageWorld(t)
	cred := f.delegated(t, f.adtIssueGrant(t, map[string]string{"kind": "hub"}, "agent:message").ID)
	path := agentMessagePath(f.agentB.ID)

	for name, body := range map[string]map[string]interface{}{
		"mentions":        {"message": "x", "mentions": []string{f.agentC.Slug}},
		"notify":          {"message": "x", "notify": true},
		"interrupt":       {"message": "x", "interrupt": true},
		"wake":            {"message": "x", "wake": true},
		"thread":          {"structured_message": map[string]interface{}{"msg": "x", "thread_id": "t-1"}},
		"conversation":    {"structured_message": map[string]interface{}{"msg": "x", "conversation_id": tid("adt-msg-conv")}},
		"group recipient": {"structured_message": map[string]interface{}{"msg": "x", "recipient": "group[" + f.agentC.Slug + "]"}},
		"mention type":    {"structured_message": map[string]interface{}{"msg": "x", "type": "mention"}},
	} {
		t.Run(name, func(t *testing.T) {
			rec := f.sendMessage(t, cred.Token, path, body)
			assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		})
	}
	assert.Empty(t, dispatcher.getCalls(), "no refused request is dispatched")

	rec := f.sendMessage(t, cred.Token, path, map[string]interface{}{
		"structured_message": map[string]interface{}{"msg": "x", "sender": "user:" + f.alice.ID, "sender_id": f.alice.ID},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	calls := dispatcher.getCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, "agent:"+f.agentA.Slug, calls[0].StructuredMessage.Sender, "a client-supplied sender is replaced")
	assert.Equal(t, f.agentA.ID, calls[0].StructuredMessage.SenderID)
}

// TestAgentDelegatedMessage_OnlyTheRouteTarget: the delegated messaging
// decision allows only the agent routeGuard admitted. Any other target (a
// fan-out, mention or group member) is refused without a decision, and the
// project route's slug names the agent only inside its own project.
func TestAgentDelegatedMessage_OnlyTheRouteTarget(t *testing.T) {
	f := newADTFixture(t, "adt-msg-target")
	f.adtMessageWorld(t)
	decisions := f.captureDecisions(t)
	cred := f.delegated(t, f.adtIssueGrant(t, map[string]string{"kind": "hub"}, "agent:message").ID)
	st := adtLoadedState(t, f, cred.Token)
	entry, ok := agentDelegationAdmissionFor(http.MethodPost, delegatedRouteID(AgentRouteActionMessage))
	require.True(t, ok)
	b, err := f.store.GetAgent(context.Background(), f.agentB.ID)
	require.NoError(t, err)
	ctx := withStandingMemo(contextWithDelegatedTarget(
		contextWithDelegatedAdmission(contextWithDelegatedState(context.Background(), st), entry),
		delegatedRouteTarget{Type: "agent", ID: b.ID}))

	decisions.reset()
	allowed, _, _ := f.srv.authorizeAgentMessage(ctx, st.identity, f.agentC, false)
	assert.False(t, allowed, "another agent is not the route target")
	assert.Empty(t, decisions.delegated(), "no decision runs for another target")
	allowed, reason, _ := f.srv.authorizeAgentMessage(ctx, st.identity, b, false)
	assert.True(t, allowed, reason)
	assert.Len(t, decisions.delegated(), 1, "one decision for the route target")

	// Message modes on the route target, evaluated as for the actor agent's
	// own send (A is in hub mode): in A's own project, project and hub
	// allow, and an empty mode, lineage and none deny; across projects a
	// lineage target fails the cross-project target-mode gate. The issuer
	// pierces nothing.
	adtGrantProjectPermissions(t, f.store, f.alice.ID, f.proj.ID, "agent.message")
	for _, tc := range []struct {
		name    string
		project string
		mode    string
		allow   bool
		code    MessageDenialCode
	}{
		{"same project, hub", f.agentA.ProjectID, store.MessageModeHub, true, ""},
		{"same project, project", f.agentA.ProjectID, store.MessageModeProject, true, ""},
		{"same project, empty", f.agentA.ProjectID, "", false, ""},
		{"same project, lineage", f.agentA.ProjectID, store.MessageModeLineage, false, ""},
		{"same project, none", f.agentA.ProjectID, store.MessageModeNone, false, ""},
		{"other project, lineage", b.ProjectID, store.MessageModeLineage, false, MessageDenialCrossProjectTargetMode},
		{"other project, hub", b.ProjectID, store.MessageModeHub, true, ""},
	} {
		moded := *b
		moded.ProjectID = tc.project
		moded.MessageMode = tc.mode
		allowed, reason, d := f.srv.authorizeAgentMessage(ctx, st.identity, &moded, false)
		assert.Equal(t, tc.allow, allowed, "%s: %s", tc.name, reason)
		require.NotNil(t, d, tc.name)
		assert.Equal(t, tc.code, d.Code, tc.name)
	}

	// The agent read entry does not admit a send.
	readEntry, ok := agentDelegationAdmissionFor(http.MethodGet, delegatedRouteID(AgentRouteRoot))
	require.True(t, ok)
	rctx := withStandingMemo(contextWithDelegatedTarget(
		contextWithDelegatedAdmission(contextWithDelegatedState(context.Background(), st), readEntry),
		delegatedRouteTarget{Type: "agent", ID: b.ID}))
	allowed, _, _ = f.srv.authorizeAgentMessage(rctx, st.identity, b, false)
	assert.False(t, allowed)

	// Route target matching on the project form.
	other := *b
	other.ProjectID = f.proj.ID
	for _, tc := range []struct {
		name   string
		target delegatedRouteTarget
		agent  *store.Agent
		want   bool
	}{
		{"by-ID route, the ID", delegatedRouteTarget{Type: "agent", ID: b.ID}, b, true},
		{"by-ID route, the slug", delegatedRouteTarget{Type: "agent", ID: b.Slug}, b, false},
		{"project route, the slug", delegatedRouteTarget{Type: "agent", ID: b.Slug, ProjectID: b.ProjectID}, b, true},
		{"project route, the ID", delegatedRouteTarget{Type: "agent", ID: b.ID, ProjectID: b.ProjectID}, b, true},
		{"project route, another project", delegatedRouteTarget{Type: "agent", ID: b.Slug, ProjectID: f.proj.ID}, b, false},
		{"project route, agent moved", delegatedRouteTarget{Type: "agent", ID: b.ID, ProjectID: b.ProjectID}, &other, false},
		{"project target type", delegatedRouteTarget{Type: "project", ID: b.ID}, b, false},
		{"nil agent", delegatedRouteTarget{Type: "agent", ID: b.ID}, nil, false},
	} {
		assert.Equal(t, tc.want, tc.target.namesAgent(tc.agent), tc.name)
	}
}

// TestAgentDelegatedMessage_OrdinaryAgentTokenGainsNothing: with P2
// back at the default inbound policy (none), agent A's own JWT gets the
// same refusal messaging B in P2 before and after Alice issues and A
// exchanges a grant for agent:message, and the delegated credential gets
// that same refusal: the grant adds nothing to the agent's own send. With
// both headers the valid agent token wins. The delegated credential does
// send to agent C in A's own project.
func TestAgentDelegatedMessage_OrdinaryAgentTokenGainsNothing(t *testing.T) {
	f := newADTFixture(t, "adt-msg-golden")
	f.adtMessageWorld(t)
	adtSetInbound(t, f.store, f.other.ID, store.CrossProjectInboundNone)
	adtGrantProjectPermissions(t, f.store, f.alice.ID, f.proj.ID, "agent.message")
	f.mutateAgent(t, f.agentC.ID, func(a *store.Agent) { a.Phase = "running"; a.RuntimeBrokerID = f.broker.ID })
	agentToken := f.agentJWT(t, f.agentA)
	path := agentMessagePath(f.agentB.ID)
	send := func(headers map[string]string) *httptest.ResponseRecorder {
		return f.do(t, http.MethodPost, path, adtText("x"), headers)
	}
	asAgent := map[string]string{"X-Scion-Agent-Token": agentToken}
	before := send(asAgent) // the agent JWT cannot message B: P2 accepts no inbound agent messages

	want := adtAssertAPIError(t, before, http.StatusForbidden, ErrCodeMessageDenied)
	assert.Equal(t, string(MessageDenialCrossProjectInboundNone), want.Details["reason"])
	sameRefusal := func(rec *httptest.ResponseRecorder, msg string) {
		got := adtAssertAPIError(t, rec, http.StatusForbidden, ErrCodeMessageDenied)
		assert.Equal(t, want.Details, got.Details, msg)
	}

	cred := f.delegated(t, f.adtIssueGrant(t, map[string]string{"kind": "hub"}, "agent:message").ID)
	sameRefusal(send(asAgent), "the agent JWT gains nothing from the grant")
	sameRefusal(send(adtBearer(cred.Token)), "the delegated credential gets the agent's own refusal")
	sameRefusal(send(map[string]string{"X-Scion-Agent-Token": agentToken, "Authorization": "Bearer " + cred.Token}), "with both headers the agent token wins")
	assert.Equal(t, http.StatusOK, f.sendMessage(t, cred.Token, agentMessagePath(f.agentC.ID), adtText("x")).Code)
}

// TestAgentDelegatedMessage_CrossProjectGatesAsForAnAgentSend: a delegated
// send to an agent in another project passes every cross-project gate the
// actor agent's own send must pass, and is refused with the same code and
// shape when one fails: a destination project at the default inbound
// policy (none), the hub cross-project setting off, and a sender that is
// not in hub mode. A destination that opted in is reached.
func TestAgentDelegatedMessage_CrossProjectGatesAsForAnAgentSend(t *testing.T) {
	f := newADTFixture(t, "adt-msg-xproj")
	dispatcher := f.adtMessageWorld(t)
	cred := f.delegated(t, f.adtIssueGrant(t, map[string]string{"kind": "hub"}, "agent:message").ID)
	agentToken := f.agentJWT(t, f.agentA)
	path := agentMessagePath(f.agentB.ID)

	// refused asserts a delegated refusal with denial code code, identical
	// in status, error code and details to the agent's own send.
	refused := func(t *testing.T, code MessageDenialCode) {
		t.Helper()
		calls := len(dispatcher.getCalls())
		delegated := adtAssertAPIError(t, f.sendMessage(t, cred.Token, path, adtText("x")), http.StatusForbidden, ErrCodeMessageDenied)
		assert.Equal(t, string(code), delegated.Details["reason"])
		own := adtAssertAPIError(t, f.do(t, http.MethodPost, path, adtText("x"), map[string]string{"X-Scion-Agent-Token": agentToken}),
			http.StatusForbidden, ErrCodeMessageDenied)
		assert.Equal(t, own.Details, delegated.Details, "the same refusal as the agent's own send")
		assert.Len(t, dispatcher.getCalls(), calls, "nothing refused was dispatched")
	}

	t.Run("destination that opted in is allowed", func(t *testing.T) {
		require.Equal(t, http.StatusOK, f.sendMessage(t, cred.Token, path, adtText("x")).Code)
	})
	t.Run("default-none destination is refused", func(t *testing.T) {
		adtSetInbound(t, f.store, f.other.ID, store.CrossProjectInboundNone)
		defer adtSetInbound(t, f.store, f.other.ID, store.CrossProjectInboundAny)
		refused(t, MessageDenialCrossProjectInboundNone)
	})
	t.Run("hub cross-project setting off is refused", func(t *testing.T) {
		adtSetCrossProjectEnabled(t, f.srv, false)
		defer adtSetCrossProjectEnabled(t, f.srv, true)
		refused(t, MessageDenialCrossProjectDisabled)
	})
	t.Run("sender not in hub mode is refused", func(t *testing.T) {
		f.mutateAgent(t, f.agentA.ID, func(a *store.Agent) { a.MessageMode = store.MessageModeProject })
		defer func() {
			_ = f.mutateAgent(t, f.agentA.ID, func(a *store.Agent) { a.MessageMode = store.MessageModeHub })
		}()
		refused(t, MessageDenialCrossProjectSenderMode)
	})
	t.Run("opted-in destination is reached again", func(t *testing.T) {
		require.Equal(t, http.StatusOK, f.sendMessage(t, cred.Token, path, adtText("x")).Code)
	})
}

// adtAttachTarget creates agent E in P1, owned by Carol, with Alice in its
// ancestry (Alice is E's ancestor), running on the fixture broker, which is
// connected to this hub through a loopback control channel. It returns E
// and the broker side of that channel.
func (f *adtFixture) adtAttachTarget(t *testing.T, name string) (*store.Agent, *websocket.Conn) {
	t.Helper()
	e := &store.Agent{
		ID: tid(name + "-agent-e"), Name: name + "-e", Slug: name + "-e", ProjectID: f.proj.ID,
		OwnerID: f.carol.ID, Ancestry: []string{f.alice.ID, f.carol.ID}, Phase: "running",
	}
	require.NoError(t, f.store.CreateAgent(context.Background(), e))
	broker := connectLoopbackFixtureBroker(t, f.bypassAgentsFixture, e)
	return e, broker
}

func adtPTYPath(agentID string) string { return "/api/v1/agents/" + agentID + "/pty" }

// dialDelegatedPTY opens a PTY WebSocket to agentID on hub with a delegated
// bearer and returns the connection (nil on a failed handshake) and the
// handshake status.
func dialDelegatedPTY(t *testing.T, hub *httptest.Server, token, agentID string) (*websocket.Conn, int) {
	t.Helper()
	header := http.Header{}
	header.Set("Authorization", "Bearer "+token)
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	conn, resp, err := dialer.Dial("ws"+strings.TrimPrefix(hub.URL, "http")+adtPTYPath(agentID), header)
	require.NotNil(t, resp, "dial error: %v", err)
	if resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		require.ErrorIs(t, err, websocket.ErrBadHandshake)
		return nil, resp.StatusCode
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, resp.StatusCode
}

// TestAgentDelegatedAttach_IssuerAncestorAttaches: Alice's project grant
// for agent:attach lets agent A attach to agent E, whose ancestor Alice is,
// although A has no relationship to E of its own. The preflight answers
// 200 and the WebSocket handshake opens the broker terminal stream. One
// decision is recorded for each handshake.
func TestAgentDelegatedAttach_IssuerAncestorAttaches(t *testing.T) {
	f := newADTFixture(t, "adt-att")
	decisions := f.captureDecisions(t)
	e, broker := f.adtAttachTarget(t, "adt-att")
	grant := f.adtIssueGrant(t, map[string]string{"kind": "project", "projectId": f.proj.ID}, "agent:attach")
	cred := f.delegated(t, grant.ID)

	decisions.reset()
	rec := f.do(t, http.MethodGet, adtPTYPath(e.ID), nil, adtBearer(cred.Token))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, decisions.delegated(), 1)
	assert.Equal(t, "allow", decisions.delegated()[0].Result)
	assert.Equal(t, "agent_delegation:"+grant.ID, decisions.delegated()[0].MatchedGrant)

	hub := httptest.NewServer(f.srv.Handler())
	t.Cleanup(hub.Close)
	conn, status := dialDelegatedPTY(t, hub, cred.Token, e.ID)
	require.Equal(t, http.StatusSwitchingProtocols, status)
	require.NotNil(t, conn)
	require.NoError(t, broker.SetReadDeadline(time.Now().Add(5*time.Second)))
	var open wsprotocol.StreamOpenMessage
	require.NoError(t, broker.ReadJSON(&open))
	assert.Equal(t, wsprotocol.TypeStreamOpen, open.Type)
	assert.Equal(t, e.Slug, open.Slug)
}

// TestAgentDelegatedAttach_SessionOutlivesRevocationAndExpiry documents
// .design/agent-delegation.md §16.6: an attach is authorized at the
// handshake only. An established session keeps carrying input after its
// credential is revoked and after the credential's lifetime has passed,
// while every new handshake (a reconnect) is refused.
func TestAgentDelegatedAttach_SessionOutlivesRevocationAndExpiry(t *testing.T) {
	f := newADTFixture(t, "adt-att-live")
	ctx := context.Background()
	e, broker := f.adtAttachTarget(t, "adt-att-live")
	hub := httptest.NewServer(f.srv.Handler())
	t.Cleanup(hub.Close)

	// next reads broker frames until one of type typ, skipping the
	// stream_close of a session an earlier subtest ended.
	next := func(t *testing.T, typ string) []byte {
		t.Helper()
		for {
			require.NoError(t, broker.SetReadDeadline(time.Now().Add(5*time.Second)))
			_, raw, err := broker.ReadMessage()
			require.NoError(t, err)
			var head struct {
				Type string `json:"type"`
			}
			require.NoError(t, json.Unmarshal(raw, &head))
			if head.Type == typ {
				return raw
			}
			require.Equal(t, wsprotocol.TypeStreamClose, head.Type, "unexpected broker frame %s", raw)
		}
	}
	open := func(t *testing.T, token string) (*websocket.Conn, wsprotocol.StreamOpenMessage) {
		t.Helper()
		conn, status := dialDelegatedPTY(t, hub, token, e.ID)
		require.Equal(t, http.StatusSwitchingProtocols, status)
		var msg wsprotocol.StreamOpenMessage
		require.NoError(t, json.Unmarshal(next(t, wsprotocol.TypeStreamOpen), &msg))
		return conn, msg
	}
	carries := func(t *testing.T, conn *websocket.Conn, streamID string, input string) {
		t.Helper()
		require.NoError(t, conn.WriteJSON(wsprotocol.NewPTYDataMessage([]byte(input))))
		var frame wsprotocol.StreamFrame
		require.NoError(t, json.Unmarshal(next(t, wsprotocol.TypeStream), &frame))
		assert.Equal(t, streamID, frame.StreamID)
		assert.Equal(t, []byte(input), frame.Data)
	}

	t.Run("revocation", func(t *testing.T) {
		cred := f.delegated(t, f.adtIssueGrant(t, map[string]string{"kind": "project", "projectId": f.proj.ID}, "agent:attach").ID)
		conn, msg := open(t, cred.Token)
		row, err := f.store.GetAgentDelegatedCredentialByKeyHash(ctx, hashDelegatedCredential(cred.Token))
		require.NoError(t, err)
		_, err = f.store.RevokeAgentDelegatedCredential(ctx, row.ID, "test", time.Now())
		require.NoError(t, err)

		carries(t, conn, msg.StreamID, "after revocation")
		_, status := dialDelegatedPTY(t, hub, cred.Token, e.ID)
		assert.Equal(t, http.StatusUnauthorized, status, "a reconnect after revocation is refused")
		require.NoError(t, conn.Close())
	})
	t.Run("expiry", func(t *testing.T) {
		cred := f.delegated(t, f.adtIssueGrant(t, map[string]string{"kind": "project", "projectId": f.proj.ID}, "agent:attach").ID)
		conn, msg := open(t, cred.Token)
		f.faults.inject(t, adtFaults{expireCredential: true})

		carries(t, conn, msg.StreamID, "after expiry")
		_, status := dialDelegatedPTY(t, hub, cred.Token, e.ID)
		assert.Equal(t, http.StatusUnauthorized, status, "a reconnect after expiry is refused")
		require.NoError(t, conn.Close())
	})
}

// TestAgentDelegatedAttach_Denials covers the in-scope §17.2 flows for the
// PTY route: a grant without agent:attach, a target on which the issuer has
// no attach authority, a project-bounded grant on another project, the
// lifecycle operations that share agent.attach, and the response
// convention.
func TestAgentDelegatedAttach_Denials(t *testing.T) {
	f := newADTFixture(t, "adt-att-deny")
	decisions := f.captureDecisions(t)
	e, _ := f.adtAttachTarget(t, "adt-att-deny")

	t.Run("permission outside the ceiling", func(t *testing.T) {
		cred := f.delegated(t, f.hubGrant(t).ID) // agent:read only
		decisions.reset()
		rec := f.do(t, http.MethodGet, adtPTYPath(e.ID), nil, adtBearer(cred.Token))
		adtAssertAPIError(t, rec, http.StatusForbidden, ErrCodeForbidden)
		assert.NotContains(t, rec.Body.String(), "agent delegation")
		decisions.assertDelegatedDeny(t, agentDelegationCodeOutsideCeiling)
	})
	attach := f.delegated(t, f.adtIssueGrant(t, map[string]string{"kind": "hub"}, "agent:attach").ID)
	t.Run("issuer has no attach authority on the target", func(t *testing.T) {
		decisions.reset()
		adtAssertAPIError(t, f.do(t, http.MethodGet, adtPTYPath(f.agentC.ID), nil, adtBearer(attach.Token)), http.StatusForbidden, ErrCodeForbidden)
		recs := decisions.delegated()
		require.NotEmpty(t, recs)
		assert.Equal(t, "deny", recs[len(recs)-1].Result)
		assert.Equal(t, string(DeniedByAgentDelegation), recs[len(recs)-1].DeniedBy)
	})
	t.Run("project-bounded grant on another project", func(t *testing.T) {
		cred := f.delegated(t, f.adtIssueGrant(t, map[string]string{"kind": "project", "projectId": f.proj.ID}, "agent:attach").ID)
		decisions.reset()
		adtAssertAPIError(t, f.do(t, http.MethodGet, adtPTYPath(f.agentB.ID), nil, adtBearer(cred.Token)), http.StatusForbidden, ErrCodeForbidden)
		decisions.assertDelegatedDeny(t, agentDelegationCodeOutsideBoundary)
	})
	t.Run("operations that share agent.attach are not admitted", func(t *testing.T) {
		for _, suffix := range []string{"/exec", "/env", "/reset-auth", "/keys", "/stop", "/start", "/restart"} {
			rec := f.do(t, http.MethodPost, "/api/v1/agents/"+e.ID+suffix, map[string]interface{}{}, adtBearer(attach.Token))
			adtAssertAPIError(t, rec, http.StatusForbidden, errCodeCredentialNotAdmitted)
		}
	})
	t.Run("missing agent answers the handler's own 404", func(t *testing.T) {
		rec := f.do(t, http.MethodGet, adtPTYPath(tid("adt-att-deny-missing")), nil, adtBearer(attach.Token))
		assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	})
	t.Run("an allowed preflight", func(t *testing.T) {
		assert.Equal(t, http.StatusOK, f.do(t, http.MethodGet, adtPTYPath(e.ID), nil, adtBearer(attach.Token)).Code)
	})
}

// TestAgentDelegatedAttach_OrdinaryAgentTokenGainsNothing: agent A's own
// JWT gets the same PTY answer on agent B in P2 before and after Alice
// issues and A exchanges a grant for agent:attach, and with both headers
// the valid agent token wins. The delegated credential attaches to E.
func TestAgentDelegatedAttach_OrdinaryAgentTokenGainsNothing(t *testing.T) {
	f := newADTFixture(t, "adt-att-golden")
	e, _ := f.adtAttachTarget(t, "adt-att-golden")
	agentToken := f.agentJWT(t, f.agentA)
	preflight := func(agentID string, headers map[string]string) int {
		return f.do(t, http.MethodGet, adtPTYPath(agentID), nil, headers).Code
	}
	asAgent := map[string]string{"X-Scion-Agent-Token": agentToken}
	before := preflight(f.agentB.ID, asAgent)
	require.NotEqual(t, http.StatusOK, before, "the agent JWT cannot attach to B")

	cred := f.delegated(t, f.adtIssueGrant(t, map[string]string{"kind": "project", "projectId": f.proj.ID}, "agent:attach").ID)
	assert.Equal(t, before, preflight(f.agentB.ID, asAgent), "the agent JWT gains nothing from the grant")
	assert.Equal(t, http.StatusOK, preflight(e.ID, adtBearer(cred.Token)))
	assert.Equal(t, before, preflight(f.agentB.ID, map[string]string{"X-Scion-Agent-Token": agentToken, "Authorization": "Bearer " + cred.Token}))
}

// TestAgentDelegatedAttach_ConduitPathRefusedBeforeUpgrade: an attach that
// would take the agent's conduit path is refused for a delegated caller,
// before any upgrade, with 503 runtime_attach_unsupported and its own
// reason, which clients treat as final. A user still takes that path, and
// a decision for the broker path is unchanged. Minting a conduit grant for
// a delegated identity is refused too (the backstop).
func TestAgentDelegatedAttach_ConduitPathRefusedBeforeUpgrade(t *testing.T) {
	f := newPTYConduitFixture(t)
	ctx := context.Background()
	delegated := &DelegatedAgentIdentity{agentID: tid("conduit-delegated-agent")}
	f.startPTYAgent(t, f.public.URL)

	f.setBrokerRow(t, attachCaps(false), "")
	require.Equal(t, ptyPathAgent, f.srv.resolvePTYPath(ctx, f.owner, f.launched).Path, "a user takes the agent path")
	d := f.srv.resolvePTYPath(ctx, delegated, f.launched)
	assert.Equal(t, ptyPathNone, d.Path)
	assert.Equal(t, http.StatusServiceUnavailable, d.Status)
	assert.Equal(t, wsprotocol.ErrCodeRuntimeAttachUnsupported, d.Code)
	assert.Equal(t, ptyReasonDelegatedBrokerOnly, d.Reason)

	f.setBrokerRow(t, attachCaps(true), "")
	assert.Equal(t, f.srv.resolvePTYPath(ctx, f.owner, f.launched), f.srv.resolvePTYPath(ctx, delegated, f.launched),
		"the broker path decision is the same for a delegated caller")

	_, _, err := f.srv.mintConduitGrant(ctx, conduitGrantRequest{
		Identity: delegated,
		Agent:    f.launched,
		Stream:   grant.StreamHeader{Kind: grant.StreamKindPTY, Params: ptyStreamParams(80, 24)},
		Target:   grant.Target{Kind: grant.TargetKindAgent, ID: f.launched.ID},
		Via:      grant.ViaPTY,
	})
	assert.ErrorIs(t, err, errConduitForbidden)
}

// adtConduitDelegatedToken has the conduit fixture's owner issue a project
// grant for agent:attach to its agent f.agent, and returns the delegated
// credential f.agent exchanges for it.
func adtConduitDelegatedToken(t *testing.T, f *ptyConduitFixture) string {
	t.Helper()
	ctx := context.Background()
	adtSetExperiment(t, f.srv, true)
	if f.agent.RunID == "" {
		_, err := f.store.SetAgentRunID(ctx, f.agent.ID, uuid.NewString(), nil)
		require.NoError(t, err)
	}
	actor, err := f.store.GetAgent(ctx, f.agent.ID)
	require.NoError(t, err)
	session, _, _, err := f.srv.userTokenService.GenerateTokenPair(f.owner.ID(), "owner@conduit.test", "Owner", store.UserRoleMember, ClientTypeWeb)
	require.NoError(t, err)
	do := func(path, token string, body map[string]interface{}) *httptest.ResponseRecorder {
		buf, err := json.Marshal(body)
		require.NoError(t, err)
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(buf))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		f.srv.Handler().ServeHTTP(rec, req)
		return rec
	}
	rec := do("/api/v1/agents/"+actor.ID+"/delegations", session, map[string]interface{}{
		"boundary":    map[string]string{"kind": "project", "projectId": actor.ProjectID},
		"permissions": []string{"agent:attach"}, "name": "conduit",
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var grant AgentDelegationGrantResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &grant))
	agentToken, err := f.srv.issueAgentTokenForTest(ctx, actor)
	require.NoError(t, err)
	rec = do("/api/v1/agents/"+actor.ID+"/delegations/"+grant.ID+"/exchange", agentToken,
		map[string]interface{}{"audience": f.srv.agentDelegationAudience()})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out ExchangeAgentDelegationResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out.Token
}

// TestAgentDelegatedAttach_ConduitPathRefusedThroughTheHandler: through the
// real middleware, route guard and handler, a delegated attach whose path is
// the agent's conduit path is refused before any upgrade. The preflight
// answers 503 runtime_attach_unsupported with reason
// delegated_credential_broker_path_only, the WebSocket handshake fails with
// the same 503, and no terminal is spawned on the agent. The fixture owner
// itself still attaches over that path.
func TestAgentDelegatedAttach_ConduitPathRefusedThroughTheHandler(t *testing.T) {
	f := newPTYConduitFixture(t)
	f.setBrokerRow(t, attachCaps(false), "")
	f.startPTYAgent(t, f.public.URL)
	status, path, _ := f.preflight(t)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, string(ptyPathAgent), path, "the owner takes the agent path")
	token := adtConduitDelegatedToken(t, f)

	req, err := http.NewRequest(http.MethodGet, f.ptyURL(), nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	var er ErrorResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&er))
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.Equal(t, wsprotocol.ErrCodeRuntimeAttachUnsupported, er.Error.Code)
	assert.Equal(t, ptyReasonDelegatedBrokerOnly, er.Error.Details["reason"])

	c, wsResp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(f.ptyURL(), "http"), http.Header{"Authorization": {"Bearer " + token}})
	if c != nil {
		_ = c.Close()
	}
	require.ErrorIs(t, err, websocket.ErrBadHandshake, "no WebSocket is upgraded")
	require.NotNil(t, wsResp)
	_ = wsResp.Body.Close()
	assert.Equal(t, http.StatusServiceUnavailable, wsResp.StatusCode)
	assert.Empty(t, f.spawned, "no terminal was spawned on the agent")
}
