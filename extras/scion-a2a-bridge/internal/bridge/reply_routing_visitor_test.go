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

package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
)

// Reply routing with reply polling on: a federated user waits on the push
// path, a member of the project polls, and the bridge's own sends take
// replies on the topic of the user they were sent as.

const (
	routingMemberID    = "33333333-4444-4555-8666-777777777777"
	routingMemberToken = "member-credential-routing"
)

// routingMember is a local user caller (a member of the project) with a
// user access credential.
func routingMember() *CallerIdentity {
	return &CallerIdentity{
		UserID: routingMemberID, Email: "member@example.com", Role: "member",
		RawToken: routingMemberToken, TokenType: "uat",
	}
}

// waitForHubCalls waits until cond holds for what the hub double has seen.
func waitForHubCalls(t *testing.T, hub *fedUserHub, what string, cond func(fedUserHubCalls) bool) fedUserHubCalls {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s := hub.snapshot(); cond(s) {
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("hub double: %s did not happen", what)
	return fedUserHubCalls{}
}

// sendsWith counts the message sends that carried fedToken as the
// federation credential ("" for sends without one).
func sendsWith(s fedUserHubCalls, fedToken string) int {
	n := 0
	for _, sent := range s.sent {
		if sent.FedToken == fedToken {
			n++
		}
	}
	return n
}

// convRequestsWith returns the conversation route requests that carried
// fedToken as the federation credential.
func convRequestsWith(s fedUserHubCalls, fedToken string) []fedUserHubConvRequest {
	var out []fedUserHubConvRequest
	for _, r := range s.convRequests {
		if r.FedToken == fedToken {
			out = append(out, r)
		}
	}
	return out
}

// activeTasksOn returns the registered tasks on an agent, oldest first,
// with their cache entries.
func activeTasksOn(b *Bridge, aKey string) ([]string, map[string]activeTaskEntry) {
	b.tasksMu.RLock()
	defer b.tasksMu.RUnlock()
	ids := append([]string(nil), b.agentTasks[aKey]...)
	entries := make(map[string]activeTaskEntry, len(ids))
	for _, id := range ids {
		entries[id] = b.activeTasks[id]
	}
	return ids, entries
}

// onlyOtherTask returns the one registered task on the agent that is not
// in skip.
func onlyOtherTask(t *testing.T, b *Bridge, aKey string, skip ...string) (string, activeTaskEntry) {
	t.Helper()
	ids, entries := activeTasksOn(b, aKey)
	var found []string
	for _, id := range ids {
		skipped := false
		for _, s := range skip {
			if id == s {
				skipped = true
			}
		}
		if !skipped {
			found = append(found, id)
		}
	}
	if len(found) != 1 {
		t.Fatalf("registered tasks on %s: %v (skipping %v), want exactly one", aKey, ids, skip)
	}
	return found[0], entries[found[0]]
}

// eventsCarrying counts the events logged for a task that carry text.
func eventsCarrying(t *testing.T, b *Bridge, taskID, text string) int {
	t.Helper()
	events, err := b.store.ReadTaskEvents(context.Background(), taskID, 0, 1000)
	if err != nil {
		t.Fatalf("ReadTaskEvents(%s): %v", taskID, err)
	}
	n := 0
	for _, ev := range events {
		if strings.Contains(string(ev.Payload), text) {
			n++
		}
	}
	return n
}

// runExecutorAsync runs one executor call for proj1/agent1 as caller and
// returns the final status event's state.
func runExecutorAsync(ctx context.Context, b *Bridge, taskID string, caller *CallerIdentity) <-chan a2a.TaskState {
	done := make(chan a2a.TaskState, 1)
	go func() {
		ctx := WithCallerIdentity(WithRouteInfo(ctx, RouteInfo{ProjectSlug: "proj1", AgentSlug: "agent1"}), caller)
		execCtx := &a2asrv.ExecutorContext{
			TaskID:    a2a.TaskID(taskID),
			ContextID: "ctx-" + taskID,
			Message:   a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("a member's request")),
		}
		var final a2a.TaskState
		for ev, err := range NewScionExecutor(b, b.log).Execute(ctx, execCtx) {
			if err != nil {
				break
			}
			if su, ok := ev.(*a2a.TaskStatusUpdateEvent); ok {
				final = su.Status.State
			}
		}
		done <- final
	}()
	return done
}

// TestExecutor_FederatedUserWaitsOnPushPathWithPollingOn: with reply
// polling on, a federated user's call makes no request to the
// conversation routes (the hub refuses a visitor there), its task is never
// marked as owned by a reply poller, and the reply pushed on the user's
// own topic is returned.
func TestExecutor_FederatedUserWaitsOnPushPathWithPollingOn(t *testing.T) {
	token := fedUserToken(t)
	hub := newFedUserHub(t, token)
	hub.set(func(h *fedUserHub) { h.visitorConvStatus = http.StatusForbidden })
	b, _, ts := newFedUserTestServer(t, hub)
	if !b.config.ReplyPoll.IsEnabled() {
		t.Fatal("reply polling is off; the case needs it on")
	}

	done := sendAsync(t, ts, "agent1", token)

	// The first request the call makes to the hub's message or
	// conversation routes: a conversation read would come before the send.
	snap := waitForHubCalls(t, hub, "a message send or a conversation request", func(s fedUserHubCalls) bool {
		return len(s.sent) > 0 || len(s.convRequests) > 0
	})
	if len(snap.convRequests) != 0 {
		t.Fatalf("conversation route requests %+v before the send, want none for a federated user", snap.convRequests)
	}
	taskID, entry := onlyOtherTask(t, b, agentKey(fedUserProjectID, "agent1"))
	if entry.pollerLive {
		t.Fatalf("task %s is marked as owned by a reply poller, want the push path for a federated user", taskID)
	}

	deliverReply(t, b)

	status, body := awaitResult(t, done)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", status, body)
	}
	if !strings.Contains(body, fedUserReplyText) {
		t.Fatalf("response does not carry the pushed reply; body %s", body)
	}
	if got := hub.snapshot().convRequests; len(got) != 0 {
		t.Fatalf("conversation route requests %+v, want none for a federated user", got)
	}
}

// TestExecutor_MemberPollerAndFederatedUserPushWaitOnOneAgent: a member's
// call on agent1 has a live reply poller while a federated user waits on
// the push path on the same agent. The member's pushed copy is dropped,
// and the federated user's own pushed reply reaches the federated user.
func TestExecutor_MemberPollerAndFederatedUserPushWaitOnOneAgent(t *testing.T) {
	const memberReplyText = "reply from agent1 to the member"
	token := fedUserToken(t)
	hub := newFedUserHub(t, token)
	hub.set(func(h *fedUserHub) { h.visitorConvStatus = http.StatusForbidden })
	b, _, ts := newFedUserTestServer(t, hub, func(c *Config) { c.Timeouts.SendMessage = 15 * time.Second })
	aKey := agentKey(fedUserProjectID, "agent1")

	// The member's call: its send is anchored, so its reply poller runs.
	memberCtx, endMember := context.WithCancel(context.Background())
	t.Cleanup(endMember)
	const memberTask = "member-task"
	memberDone := runExecutorAsync(memberCtx, b, memberTask, routingMember())
	waitForHubCalls(t, hub, "the member's send", func(s fedUserHubCalls) bool { return sendsWith(s, "") == 1 })
	if _, entries := activeTasksOn(b, aKey); !entries[memberTask].pollerLive {
		t.Fatalf("member task entry %+v, want a live reply poller", entries[memberTask])
	}

	// The federated user's call on the same agent.
	done := sendAsync(t, ts, "agent1", token)
	waitForHubCalls(t, hub, "the federated user's send", func(s fedUserHubCalls) bool { return sendsWith(s, token) == 1 })
	visitorTask, entry := onlyOtherTask(t, b, aKey, memberTask)
	if !entry.userCaller || entry.callerUserID != fedUserAccountID {
		t.Fatalf("federated user's task entry %+v, want a user caller with the hub account ID %s", entry, fedUserAccountID)
	}
	if entry.pollerLive {
		t.Fatalf("federated user's task %s is marked as owned by a reply poller, want the push path", visitorTask)
	}

	// The member's pushed copy: the member's poller owns that reply.
	deliverReplyTo(t, b, routingMemberID, memberReplyText)
	if n := eventsCarrying(t, b, memberTask, memberReplyText); n != 0 {
		t.Fatalf("%d events appended to the member's task for its pushed copy, want 0 while its poller is live", n)
	}
	if n := eventsCarrying(t, b, visitorTask, memberReplyText); n != 0 {
		t.Fatalf("%d events appended to the federated user's task for the member's reply, want 0", n)
	}

	// The federated user's own reply.
	deliverReplyTo(t, b, fedUserAccountID, fedUserReplyText)
	if n := eventsCarrying(t, b, visitorTask, fedUserReplyText); n == 0 {
		t.Fatal("the federated user's pushed reply was not appended to its task")
	}
	status, body := awaitResult(t, done)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", status, body)
	}
	if !strings.Contains(body, fedUserReplyText) || strings.Contains(body, memberReplyText) {
		t.Fatalf("response %s, want the federated user's own reply only", body)
	}
	if got := convRequestsWith(hub.snapshot(), token); len(got) != 0 {
		t.Fatalf("conversation route requests with the federated user's credential %+v, want none", got)
	}

	endMember()
	select {
	case <-memberDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the member's call did not end after its context was cancelled")
	}
}

// TestSendStreamingMessage_LocalUserTaskTakesAdminTopicReply: the
// streaming path sends as the bridge admin, so a local user's streaming
// task takes the reply on the admin topic and not on the user's own topic.
func TestSendStreamingMessage_LocalUserTaskTakesAdminTopicReply(t *testing.T) {
	const (
		ownTopicText   = "reply on the member's own topic"
		adminTopicText = "reply on the bridge admin's topic"
	)
	hub := newFedUserHub(t, fedUserToken(t))
	b, _, _ := newFedUserTestServer(t, hub)

	ctx := WithCallerIdentity(context.Background(), routingMember())
	taskID, _, cleanup, err := b.SendStreamingMessage(ctx, "proj1", "agent1", "", []Part{{Text: "a streamed request"}})
	if err != nil {
		t.Fatalf("SendStreamingMessage: %v", err)
	}
	t.Cleanup(cleanup)

	_, entries := activeTasksOn(b, agentKey(fedUserProjectID, "agent1"))
	if entry := entries[taskID]; entry.userCaller || entry.callerUserID != "" {
		t.Fatalf("streaming task entry %+v, want the admin class (the send is made as the bridge admin)", entry)
	}

	push := func(userID, text string) {
		t.Helper()
		msg := &messages.StructuredMessage{
			Version: 1, Timestamp: time.Now().UTC().Format(time.RFC3339),
			Sender: "agent:agent1", Recipient: "user:" + userID, Msg: text, Type: messages.TypeInstruction,
		}
		if err := b.HandleBrokerMessage(context.Background(), projectkeys.UserTopic(fedUserProjectID, userID), msg); err != nil {
			t.Fatalf("HandleBrokerMessage: %v", err)
		}
	}
	push(routingMemberID, ownTopicText)
	if n := eventsCarrying(t, b, taskID, ownTopicText); n != 0 {
		t.Fatalf("%d events appended for a reply on the member's own topic, want 0", n)
	}
	push(b.adminTopicUserID(), adminTopicText)
	if n := eventsCarrying(t, b, taskID, adminTopicText); n == 0 {
		t.Fatal("the reply on the bridge admin's topic was not appended to the streaming task")
	}
}

// TestSendMessage_LocalUserBlockingCallTakesOwnTopicReply: a local user's
// blocking Bridge.SendMessage call is sent with the user's credential, so
// its task takes the reply pushed on that user's own topic and returns it.
func TestSendMessage_LocalUserBlockingCallTakesOwnTopicReply(t *testing.T) {
	const replyText = "reply from agent1 on the member's topic"
	hub := newFedUserHub(t, fedUserToken(t))
	b, _, _ := newFedUserTestServer(t, hub)

	ctx, cancel := context.WithCancel(WithCallerIdentity(context.Background(), routingMember()))
	t.Cleanup(cancel)
	type result struct {
		res *TaskResult
		err error
	}
	done := make(chan result, 1)
	go func() {
		res, err := b.SendMessage(ctx, "proj1", "agent1", "", "", []Part{{Text: "a blocking request"}}, true)
		done <- result{res, err}
	}()

	snap := waitForHubCalls(t, hub, "the member's send", func(s fedUserHubCalls) bool { return len(s.sent) == 1 })
	if snap.sent[0].FedToken != "" {
		t.Fatalf("send carried a federation credential %q, want the member's own credential only", snap.sent[0].FedToken)
	}
	_, entry := onlyOtherTask(t, b, agentKey(fedUserProjectID, "agent1"))
	if !entry.userCaller || entry.callerUserID != routingMemberID {
		t.Fatalf("task entry %+v, want a user caller with the member's ID %s", entry, routingMemberID)
	}

	deliverReplyTo(t, b, routingMemberID, replyText)

	var r result
	select {
	case r = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("SendMessage did not return")
	}
	if r.err != nil {
		t.Fatalf("SendMessage: %v", r.err)
	}
	out, err := json.Marshal(r.res)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	if !strings.Contains(string(out), replyText) {
		t.Fatalf("result %s does not carry the reply pushed on the member's topic", out)
	}
}
