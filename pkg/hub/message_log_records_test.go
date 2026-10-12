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

//go:build !no_sqlite && (!hubshard || hubshard_3)

package hub

// ---------------------------------------------------------------------------
// ptone/scion#3852: the message log holds one message record per message.
// Lifecycle lines are tagged log_kind=event and name sender and recipient,
// so the Messages view (which leaves event entries out) shows no
// "unknown -> unknown" rows and no duplicates.
// ---------------------------------------------------------------------------

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newJSONMessageLogger captures the dedicated message log as JSON lines.
func newJSONMessageLogger() (*slog.Logger, *syncBuffer) {
	buf := &syncBuffer{}
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})), buf
}

// messageLogEntries parses every captured message log line.
func messageLogEntries(t *testing.T, buf *syncBuffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &rec), "line: %s", line)
		out = append(out, rec)
	}
	return out
}

// messageRecords returns the entries the Messages view lists: no
// log_kind=event tag and a sender or recipient. It fails the test on an
// untagged entry without sender or recipient (an "unknown" row).
func messageRecords(t *testing.T, entries []map[string]any) []map[string]any {
	t.Helper()
	var recs []map[string]any
	for _, e := range entries {
		if e[logging.AttrLogKind] == logging.LogKindEvent {
			continue
		}
		sender, _ := e[logging.AttrSender].(string)
		recipient, _ := e[logging.AttrRecipient].(string)
		if sender == "" && recipient == "" {
			t.Errorf("untagged message log entry without sender or recipient: %v", e)
			continue
		}
		recs = append(recs, e)
	}
	return recs
}

func findMessageLogEntry(t *testing.T, entries []map[string]any, msg string) map[string]any {
	t.Helper()
	for _, e := range entries {
		if e["msg"] == msg {
			return e
		}
	}
	t.Fatalf("no %q entry in message log: %v", msg, entries)
	return nil
}

// Through handleAgentMessage: "message received for delivery" is the
// record; the agent DM dispatch line is a lifecycle entry with sender and
// recipient.
func TestMessageLog_AgentDM_HandleAgentMessage_OneRecord(t *testing.T) {
	srv, _, _, sender, target, _, _, _ := paritySetup(t)
	logger, buf := newJSONMessageLogger()
	srv.SetMessageLogger(logger)

	rr := sendViaStructured(t, srv, sender, target, "records-structured")
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	entries := messageLogEntries(t, buf)
	recs := messageRecords(t, entries)
	require.Len(t, recs, 1, "exactly one message record: %v", entries)
	assert.Equal(t, "message received for delivery", recs[0]["msg"])
	assert.Equal(t, "records-structured", recs[0]["message_content"])

	dm := findMessageLogEntry(t, entries, "agent DM: message dispatched")
	assert.Equal(t, logging.LogKindEvent, dm[logging.AttrLogKind])
	assert.Equal(t, "agent:"+sender.Slug, dm[logging.AttrSender])
	assert.Equal(t, sender.ID, dm[logging.AttrSenderID])
	assert.Equal(t, "agent:"+target.Slug, dm[logging.AttrRecipient])
	assert.Equal(t, target.ID, dm[logging.AttrRecipientID])
	assert.NotContains(t, dm, "message_content", "lifecycle entry carries no body")
}

// Through the outbound adapter (ExecuteAgentDM without RecordLogged): the
// operation writes the single record; its dispatch line is an event.
func TestMessageLog_AgentDM_Outbound_OneRecord(t *testing.T) {
	srv, _, _, sender, target, convID, _, _ := paritySetup(t)
	logger, buf := newJSONMessageLogger()
	srv.SetMessageLogger(logger)

	rr := sendViaOutbound(t, srv, sender, convID, "records-outbound")
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	recs := messageRecords(t, messageLogEntries(t, buf))
	require.Len(t, recs, 1, "exactly one message record")
	rec := recs[0]
	assert.Equal(t, "message handed to delivery", rec["msg"])
	assert.Equal(t, "agent:"+sender.Slug, rec[logging.AttrSender])
	assert.Equal(t, "agent:"+target.Slug, rec[logging.AttrRecipient])
	assert.Equal(t, target.ID, rec[logging.AttrRecipientID])
	assert.Equal(t, "records-outbound", rec["message_content"])
	assert.Equal(t, convID, rec["conversation_id"])
}

// Direct ExecuteAgentDM calls: the record is written unless the caller
// already logged it.
func TestMessageLog_ExecuteAgentDM_RecordLogged(t *testing.T) {
	for _, tc := range []struct {
		name         string
		recordLogged bool
		wantRecords  int
	}{
		{"not yet logged", false, 1},
		{"already logged", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, _, sender, target, _, _, _ := paritySetup(t)
			logger, buf := newJSONMessageLogger()
			srv.SetMessageLogger(logger)

			_, dmErr := srv.ExecuteAgentDM(context.Background(), &AgentDMInput{
				SenderAgent: sender,
				SenderIdentity: &agentIdentityWrapper{&AgentTokenClaims{
					Claims:    jwt.Claims{Subject: sender.ID},
					ProjectID: sender.ProjectID,
					Ancestry:  sender.Ancestry,
				}},
				TargetAgent:  target,
				Msg:          "records-direct",
				Type:         "instruction",
				ProjectID:    sender.ProjectID,
				RecordLogged: tc.recordLogged,
			})
			require.Nil(t, dmErr)

			entries := messageLogEntries(t, buf)
			assert.Len(t, messageRecords(t, entries), tc.wantRecords)
			dm := findMessageLogEntry(t, entries, "agent DM: message dispatched")
			assert.Equal(t, "agent:"+sender.Slug, dm[logging.AttrSender])
			assert.Equal(t, "agent:"+target.Slug, dm[logging.AttrRecipient])
		})
	}
}

// The agent reply to a user line names sender and recipient.
func TestMessageLog_OutboundMessageSent_HasSenderAndRecipient(t *testing.T) {
	srv, s, project, agent, user := def138Setup(t)
	ctx := context.Background()

	threadRef := "thread:" + project.ID + ":records-outbound-user"
	conv, err := s.UpsertConversationByExternalRef(ctx, &store.Conversation{
		Kind:        "group",
		Surface:     "discord",
		ExternalRef: threadRef,
		ProjectID:   &project.ID,
		DriftState:  "active",
	})
	require.NoError(t, err)

	logger, buf := newJSONMessageLogger()
	srv.SetMessageLogger(logger)
	// The broker path writes the record from the proxy; capture it too.
	if bp := srv.GetMessageBrokerProxy(); bp != nil {
		bp.messageLog = logger
	}

	rr := postOutboundWithConv(t, srv, project.ID, agent.ID, user.Email, "records reply", conv.ID)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	// The proxy may log from a subscriber goroutine.
	require.Eventually(t, func() bool {
		return len(messageRecords(t, messageLogEntries(t, buf))) >= 1
	}, 5*time.Second, 10*time.Millisecond, "no message record written")
	time.Sleep(50 * time.Millisecond)
	entries := messageLogEntries(t, buf)
	recs := messageRecords(t, entries)
	require.Len(t, recs, 1, "exactly one message record: %v", entries)
	assert.Equal(t, "records reply", recs[0]["message_content"])
	line := findMessageLogEntry(t, entries, "outbound message sent")
	assert.Equal(t, "agent:"+agent.Slug, line[logging.AttrSender])
	assert.NotEmpty(t, line[logging.AttrRecipient])
	assert.NotEmpty(t, line[logging.AttrRecipientID])
}

// group[] members: every member gets one record, written before the
// outcome, so a member whose dispatch is refused (stopped agent) keeps its
// record; user members get one too. The summary line is a lifecycle entry
// with sender and recipient.
func TestMessageLog_GroupMessage_RecordPerMember(t *testing.T) {
	srv, s := testServer(t)
	projectID, _ := setupGroupTest(t, s, "3852-grp", map[string]string{
		"rec-grp-a":       string(state.PhaseRunning),
		"rec-grp-b":       string(state.PhaseRunning),
		"rec-grp-stopped": string(state.PhaseStopped),
	})
	member := &store.User{
		ID:      tid("rec-grp-user"),
		Email:   "rec-grp-user@test.example",
		Role:    store.UserRoleMember,
		Status:  "active",
		Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(context.Background(), member))
	srv.SetDispatcher(&brokerMockDispatcher{})
	logger, buf := newJSONMessageLogger()
	srv.SetMessageLogger(logger)

	rec := doRequest(t, srv, http.MethodPost,
		"/api/v1/projects/"+projectID+"/agents/rec-grp-a/message",
		MessageRequest{StructuredMessage: &messages.StructuredMessage{
			Version:   messages.Version,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Sender:    "user:3852",
			SenderID:  DevUserID,
			Recipient: "group[agent:rec-grp-a,agent:rec-grp-b,agent:rec-grp-stopped,user:" + member.ID + "]",
			Msg:       "group records",
			Type:      messages.TypeInstruction,
		}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	entries := messageLogEntries(t, buf)
	summary := findMessageLogEntry(t, entries, "set message dispatched")
	assert.Equal(t, logging.LogKindEvent, summary[logging.AttrLogKind])
	assert.NotEmpty(t, summary[logging.AttrSender])
	assert.NotEmpty(t, summary[logging.AttrRecipient])

	recs := messageRecords(t, entries)
	var recipients []string
	for _, r := range recs {
		assert.Equal(t, "set message handed to member", r["msg"])
		assert.Equal(t, summary[logging.AttrSender], r[logging.AttrSender])
		recipients = append(recipients, r[logging.AttrRecipient].(string))
	}
	require.Len(t, recipients, 4, "one record per member: %v", entries)
	assert.Contains(t, recipients, "agent:rec-grp-a")
	assert.Contains(t, recipients, "agent:rec-grp-b")
	assert.Contains(t, recipients, "agent:rec-grp-stopped")
}

// recordsForRecipient returns the message records addressed to recipient.
func recordsForRecipient(t *testing.T, entries []map[string]any, recipient string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, r := range messageRecords(t, entries) {
		if r[logging.AttrRecipient] == recipient {
			out = append(out, r)
		}
	}
	return out
}

// Web chat v2: the primary agent and an @mentioned agent each get exactly
// one record, written by the dispatch choke point.
func TestMessageLog_ChatV2_PrimaryAndMention_OneRecordEach(t *testing.T) {
	srv, s, wcs, proj, db := setupSendTest(t)
	srv.SetDispatcher(&brokerMockDispatcher{})
	logger, buf := newJSONMessageLogger()
	srv.SetMessageLogger(logger)

	for _, slug := range []string{"rec-chat-primary", "rec-chat-second"} {
		require.NoError(t, s.CreateAgent(t.Context(), &store.Agent{
			ID: tid(slug), ProjectID: proj.ID, Name: slug, Slug: slug,
			Phase: "running", OwnerID: DevUserID, CreatedBy: DevUserID,
		}))
	}
	topicID := tid("rec-chat-topic")
	require.NoError(t, wcs.CreateTopic(t.Context(), WebChatTopic{
		ID: topicID, ProjectID: proj.ID, Name: "rec-chat-topic", CreatedBy: "dev",
		CreatedAt: time.Now().UTC(), DefaultAgent: "rec-chat-primary",
	}))
	setTopicConversationID(t, db, s, topicID, proj.ID)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/chat/conversations/"+topicID+"/messages",
		map[string]interface{}{"content": "records check @rec-chat-second"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	entries := messageLogEntries(t, buf)
	assert.Len(t, recordsForRecipient(t, entries, "agent:rec-chat-primary"), 1, "primary: %v", entries)
	assert.Len(t, recordsForRecipient(t, entries, "agent:rec-chat-second"), 1, "mention: %v", entries)
}

// Scheduled messages get exactly one record.
func TestMessageLog_Scheduler_OneRecord(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	projectID := tid("rec-sched-project")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "rec-sched-project", Slug: "rec-sched-project",
	}))
	brokerID := tid("rec-sched-broker")
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: brokerID, Name: "rec-sched-broker", Slug: "rec-sched-broker",
		Status: store.BrokerStatusOnline,
	}))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: projectID, BrokerID: brokerID, BrokerName: "rec-sched-broker",
		Status: store.BrokerStatusOnline,
	}))
	agentID := tid("rec-sched-agent")
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: agentID, Name: "rec-sched-agent", Slug: "rec-sched-agent",
		ProjectID: projectID, RuntimeBrokerID: brokerID, Phase: "running",
	}))
	srv.SetDispatcher(&recordingDispatcher{})
	logger, buf := newJSONMessageLogger()
	srv.SetMessageLogger(logger)

	payload, _ := json.Marshal(MessageEventPayload{AgentName: "rec-sched-agent", Message: "scheduled records"})
	evt := withAgentRevision(t, srv, store.ScheduledEvent{
		ID: api.NewUUID(), ProjectID: projectID, EventType: "message",
		Payload: string(payload), Status: store.ScheduledEventPending, CreatedBy: agentID,
	}, agentID)
	require.NoError(t, srv.messageEventHandler()(ctx, evt))

	recs := messageRecords(t, messageLogEntries(t, buf))
	require.Len(t, recs, 1)
	assert.Equal(t, "agent:rec-sched-agent", recs[0][logging.AttrRecipient])
	assert.Equal(t, "scheduled records", recs[0]["message_content"])
}

// Agent notifications get exactly one record.
func TestMessageLog_Notification_OneRecord(t *testing.T) {
	env := setupNotificationTest(t)
	logger, buf := newJSONMessageLogger()
	env.nd.messageLog = logger
	env.nd.Start()
	defer env.nd.Stop()

	env.publishStatus("completed")
	require.Eventually(t, func() bool {
		return len(env.dispatcher.getCalls()) == 1
	}, 2*time.Second, 50*time.Millisecond)

	recs := recordsForRecipient(t, messageLogEntries(t, buf), "agent:"+env.subscriber.Slug)
	require.Len(t, recs, 1)
}

// Mention copies from processMentions get exactly one record each.
func TestMessageLog_ProcessMentions_OneRecord(t *testing.T) {
	srv, s := testServer(t)
	_, agents := setupGroupTest(t, s, "3852-mention", map[string]string{
		"rec-mention-primary": string(state.PhaseRunning),
		"rec-mention-target":  string(state.PhaseRunning),
	})
	srv.SetDispatcher(&brokerMockDispatcher{})
	logger, buf := newJSONMessageLogger()
	srv.SetMessageLogger(logger)

	primary := agents["rec-mention-primary"]
	orig := &messages.StructuredMessage{
		Sender: "user:tester", SenderID: tid("user-tester"),
		Recipient: "agent:" + primary.Slug, Msg: "hello @rec-mention-target",
		Type: messages.TypeInstruction, Version: messages.Version,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
	ctx := contextWithIdentity(context.Background(),
		NewAuthenticatedUser(tid("user-tester"), "tester@example.com", "Tester", "admin", "web"))
	results := srv.processMentions(ctx, []string{"rec-mention-target"}, primary, orig, "", "")
	require.Len(t, results, 1)
	require.Equal(t, "delivered", results[0].Status, "%+v", results[0])

	recs := recordsForRecipient(t, messageLogEntries(t, buf), "agent:rec-mention-target")
	require.Len(t, recs, 1)
}

// Routed inbound: the primary and an @mentioned agent each get exactly
// one record (written before dispatch); the delivered lines are events.
func TestMessageLog_RoutedInbound_PrimaryAndMention_OneRecordEach(t *testing.T) {
	env := setupRoutedTestEnv(t)
	logger, buf := newJSONMessageLogger()
	env.srv.SetMessageLogger(logger)

	rec := env.doRoutedRequest(t, routedInboundRequest{
		ProjectID:    env.project.ID,
		DefaultAgent: "alpha",
		Message: &messages.StructuredMessage{
			Version: messages.Version,
			Channel: "slack",
			Sender:  "user:" + env.user.Email,
			Msg:     "records check @beta",
			Type:    messages.TypeInstruction,
		},
	})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	entries := messageLogEntries(t, buf)
	assert.Len(t, recordsForRecipient(t, entries, "agent:alpha"), 1, "primary: %v", entries)
	assert.Len(t, recordsForRecipient(t, entries, "agent:beta"), 1, "mention: %v", entries)
	assert.Len(t, messageRecords(t, entries), 2, "no other records: %v", entries)
	delivered := findMessageLogEntry(t, entries, "routed inbound message delivered")
	assert.Equal(t, logging.LogKindEvent, delivered[logging.AttrLogKind])
}

// Outbound to a user through the broker proxy (recordLogged true): the
// proxy's delivery line is the one record and the hub line is an event.
func TestMessageLog_OutboundToUser_BrokerPath_OneRecord(t *testing.T) {
	srv, _, project, agent, user := def141BrokerSetup(t)
	logger, buf := newJSONMessageLogger()
	srv.SetMessageLogger(logger)
	bp := srv.GetMessageBrokerProxy()
	require.NotNil(t, bp)
	bp.messageLog = logger

	rr := postOutboundNoConv(t, srv, project.ID, agent.ID, user.Email, "records broker reply")
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())

	// deliverToUser runs on a subscriber goroutine.
	require.Eventually(t, func() bool {
		return len(messageRecords(t, messageLogEntries(t, buf))) >= 1
	}, 5*time.Second, 10*time.Millisecond, "no message record written")
	time.Sleep(50 * time.Millisecond)
	entries := messageLogEntries(t, buf)
	recs := messageRecords(t, entries)
	require.Len(t, recs, 1, "exactly one message record: %v", entries)
	assert.Equal(t, "user message delivered via broker", recs[0]["msg"])
	assert.Equal(t, "records broker reply", recs[0]["message_content"])
	sent := findMessageLogEntry(t, entries, "outbound message sent")
	assert.Equal(t, logging.LogKindEvent, sent[logging.AttrLogKind])
}

// Notification sweep retry: exactly one record.
func TestMessageLog_NotificationRetry_OneRecord(t *testing.T) {
	env := setupNotificationTest(t)
	logger, buf := newJSONMessageLogger()
	env.nd.messageLog = logger
	ctx := context.Background()

	require.NoError(t, env.store.CreateNotification(ctx, &store.Notification{
		ID:             api.NewUUID(),
		SubscriptionID: env.sub.ID,
		AgentID:        env.watched.ID,
		ProjectID:      env.project.ID,
		SubscriberType: store.SubscriberTypeAgent,
		SubscriberID:   env.subscriber.Slug,
		Status:         "COMPLETED",
		Message:        "watched-agent has reached a state of COMPLETED",
		Dispatched:     false,
		CreatedAt:      time.Now().Add(-2 * time.Minute),
	}))
	notifs, err := env.store.GetUndispatchedAgentNotifications(ctx, "")
	require.NoError(t, err)
	require.Len(t, notifs, 1)

	env.nd.RetryDispatch(ctx, &notifs[0])
	require.Len(t, env.dispatcher.getCalls(), 1)

	recs := recordsForRecipient(t, messageLogEntries(t, buf), "agent:"+env.subscriber.Slug)
	require.Len(t, recs, 1)
}

// Artifact review notices: exactly one record.
func TestMessageLog_ArtifactReview_OneRecord(t *testing.T) {
	srv, s := testServer(t)
	_, agents := setupGroupTest(t, s, "3852-review", map[string]string{
		"rec-review-owner": string(state.PhaseRunning),
	})
	srv.SetDispatcher(&brokerMockDispatcher{})
	logger, buf := newJSONMessageLogger()
	srv.SetMessageLogger(logger)

	owner := agents["rec-review-owner"]
	require.NoError(t, srv.deliverArtifactReview(context.Background(), artifacts.ReviewNotice{
		ArtifactID:   "rec-review-artifact",
		Seq:          2,
		OwnerKind:    artifacts.PrincipalKindAgent,
		OwnerRef:     owner.ID,
		ReviewerKind: artifacts.PrincipalKindUser,
		ReviewerRef:  DevUserID,
	}))

	recs := recordsForRecipient(t, messageLogEntries(t, buf), "agent:rec-review-owner")
	require.Len(t, recs, 1)
}

// DELIVERY_FAILED and DELIVERY_DEFERRED notices: one record each.
func TestMessageLog_DeliveryNotices_OneRecordEach(t *testing.T) {
	s := newBrokerTestStore(t)
	projectID := setupBrokerTestProject(t, s)
	sender := setupBrokerTestAgent(t, s, projectID, "rec-notice-sender", "running")

	events := NewChannelEventPublisher()
	defer events.Close()
	b := eventbus.NewInProcessEventBus(slog.Default())
	defer func() { _ = b.Close() }()
	dispatcher := &brokerMockDispatcher{}
	proxy := NewMessageBrokerProxy(b, s, events, func() AgentDispatcher { return dispatcher }, slog.Default())
	logger, buf := newJSONMessageLogger()
	proxy.messageLog = logger

	msg := messages.NewInstruction("agent:rec-notice-sender", "agent:rec-notice-target", "hello")
	msg.SenderID = sender.ID
	ctx := context.Background()
	proxy.publishDeliveryFailed(ctx, projectID, "rec-notice-target", msg, errors.New("broker refused"))
	proxy.publishDeliveryDeferred(ctx, "rec-notice-target", msg)

	recs := recordsForRecipient(t, messageLogEntries(t, buf), "agent:rec-notice-sender")
	require.Len(t, recs, 2, "one record per notice")
	assert.Contains(t, recs[0]["message_content"], "delivery failed")
	assert.Contains(t, recs[1]["message_content"], "reincarnating")
}

// postBroadcast sends a project broadcast as the dev user.
func postBroadcast(t *testing.T, srv *Server, projectID, body string) {
	t.Helper()
	rec := doRequest(t, srv, http.MethodPost, fmt.Sprintf("/api/v1/projects/%s/broadcast", projectID), map[string]interface{}{
		"structured_message": &messages.StructuredMessage{
			Sender: "user:test", Msg: body, Type: messages.TypeInstruction,
		},
	})
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
}

// Direct broadcast: one record per recipient; the summary is an event.
func TestMessageLog_BroadcastDirect_RecordPerRecipient(t *testing.T) {
	srv, s := testServer(t)
	projectID, _ := setupGroupTest(t, s, "3852-bcast", map[string]string{
		"rec-bcast-a": string(state.PhaseRunning),
		"rec-bcast-b": string(state.PhaseRunning),
	})
	srv.SetDispatcher(&brokerMockDispatcher{})
	logger, buf := newJSONMessageLogger()
	srv.SetMessageLogger(logger)

	postBroadcast(t, srv, projectID, "records broadcast")

	entries := messageLogEntries(t, buf)
	assert.Len(t, recordsForRecipient(t, entries, "agent:rec-bcast-a"), 1, "%v", entries)
	assert.Len(t, recordsForRecipient(t, entries, "agent:rec-bcast-b"), 1, "%v", entries)
	assert.Len(t, messageRecords(t, entries), 2, "no summary record: %v", entries)
	summary := findMessageLogEntry(t, entries, "broadcast message published")
	assert.Equal(t, logging.LogKindEvent, summary[logging.AttrLogKind])
}

// A broadcast that reaches no recipient still appears once: the summary
// is its record.
func TestMessageLog_Broadcast_NoRecipients_SummaryIsRecord(t *testing.T) {
	srv, s := testServer(t)
	projectID, _ := setupGroupTest(t, s, "3852-bcast-none", map[string]string{
		"rec-bcast-stopped": string(state.PhaseStopped),
	})
	srv.SetDispatcher(&brokerMockDispatcher{})
	logger, buf := newJSONMessageLogger()
	srv.SetMessageLogger(logger)

	postBroadcast(t, srv, projectID, "records broadcast none")

	recs := messageRecords(t, messageLogEntries(t, buf))
	require.Len(t, recs, 1)
	assert.Equal(t, "broadcast message published", recs[0]["msg"])
}

// Broadcast through the broker proxy: one record per recipient from the
// proxy's delivery; the summary is an event.
func TestMessageLog_BroadcastProxy_RecordPerRecipient(t *testing.T) {
	srv, s := testServer(t)
	projectID, agents := setupGroupTest(t, s, "3852-bcast-proxy", map[string]string{
		"rec-bproxy-a": string(state.PhaseRunning),
		"rec-bproxy-b": string(state.PhaseRunning),
	})
	dispatcher := &brokerMockDispatcher{}
	srv.SetDispatcher(dispatcher)
	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)
	bus := eventbus.NewInProcessEventBus(slog.Default())
	t.Cleanup(func() { _ = bus.Close() })
	proxy := NewMessageBrokerProxy(bus, s, events, func() AgentDispatcher { return dispatcher }, slog.Default())
	logger, buf := newJSONMessageLogger()
	proxy.messageLog = logger
	proxy.Start()
	t.Cleanup(proxy.Stop)
	srv.SetMessageBrokerProxy(proxy)
	srv.SetMessageLogger(logger)
	for slug := range agents {
		proxy.subscribeAgent(projectID, slug)
	}

	postBroadcast(t, srv, projectID, "records broadcast proxy")

	require.Eventually(t, func() bool {
		entries := messageLogEntries(t, buf)
		return len(recordsForRecipient(t, entries, "agent:rec-bproxy-a")) >= 1 &&
			len(recordsForRecipient(t, entries, "agent:rec-bproxy-b")) >= 1
	}, 5*time.Second, 10*time.Millisecond, "per-recipient records not written")
	time.Sleep(50 * time.Millisecond)
	entries := messageLogEntries(t, buf)
	assert.Len(t, recordsForRecipient(t, entries, "agent:rec-bproxy-a"), 1, "%v", entries)
	assert.Len(t, recordsForRecipient(t, entries, "agent:rec-bproxy-b"), 1, "%v", entries)
	assert.Len(t, messageRecords(t, entries), 2, "no summary record: %v", entries)
	summary := findMessageLogEntry(t, entries, "broadcast message published")
	assert.Equal(t, logging.LogKindEvent, summary[logging.AttrLogKind])
}
