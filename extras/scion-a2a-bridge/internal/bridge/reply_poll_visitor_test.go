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
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// Reply polling for a federated user caller: the executor reads the reply
// from the caller's own conversation with the agent, through the chat route
// of that conversation, with the caller's own credential (federatedUserGet).

// visitorPollTimeout is the call timeout of these tests: long enough for a
// retried read on the poll schedule (first read 1 s after the send, then
// 1.5 s later).
const visitorPollTimeout = 8 * time.Second

func visitorPollConfig(cfg *Config) {
	cfg.Timeouts.SendMessage = visitorPollTimeout
}

// visitorDMRows is the visitor's conversation, newest first: the agent's
// reply, then the anchor (the message the bridge sent, msg-1 in the hub
// double's send answer).
func visitorDMRows(replyText string) []map[string]interface{} {
	sent := time.Now().UTC().Add(-time.Second)
	return []map[string]interface{}{
		{"id": "reply-1", "sender": "agent:agent1", "senderId": fedUserAgentID, "type": messages.TypeInstruction, "msg": replyText, "createdAt": sent.Add(500 * time.Millisecond).Format(time.RFC3339Nano)},
		{"id": "msg-1", "sender": "user:" + fedUserAccountID, "senderId": fedUserAccountID, "type": messages.TypeInstruction, "msg": "Hello from hub A", "createdAt": sent.Format(time.RFC3339Nano)},
	}
}

func visitorDMKey(t *testing.T) string {
	t.Helper()
	key, err := messages.DMConversationKey("agent", fedUserAgentID, "user", fedUserAccountID)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// TestReplyPoll_VisitorReturnsReplyFromOwnConversation: a federated user's
// call returns the agent's reply read from its own conversation. Every read
// (preflight, poll, access re-check) is of that conversation's key with the
// caller's credential; the preflight (one message) comes before the send,
// on a conversation with no messages yet (first contact), and the poll
// reads full pages.
func TestReplyPoll_VisitorReturnsReplyFromOwnConversation(t *testing.T) {
	token := fedUserToken(t)
	hub := newFedUserHub(t, token)
	_, _, ts := newFedUserTestServer(t, hub, visitorPollConfig)

	done := sendAsync(t, ts, "agent1", token)
	sent := waitForSend(t, hub)
	hub.set(func(h *fedUserHub) { h.dmMessages = visitorDMRows(fedUserReplyText) })

	status, body := awaitResult(t, done)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", status, body)
	}
	if !strings.Contains(body, fedUserReplyText) {
		t.Fatalf("response does not carry the agent's reply; body %s", body)
	}
	if sent.DMReadsBefore != 1 {
		t.Errorf("conversation reads before the send = %d, want 1 (the preflight)", sent.DMReadsBefore)
	}

	snap := hub.snapshot()
	wantKey := visitorDMKey(t)
	if len(snap.dmReadKeys) < 3 {
		t.Fatalf("conversation reads = %d, want at least the preflight, a poll and the access re-check", len(snap.dmReadKeys))
	}
	for i, key := range snap.dmReadKeys {
		if key != wantKey || snap.dmReadTokens[i] != token {
			t.Errorf("read %d: key %q with credential %q, want %q with the caller's", i, key, snap.dmReadTokens[i], wantKey)
		}
	}
	if got := snap.dmReadQueries[0].Get("limit"); got != "1" {
		t.Errorf("preflight read limit = %q, want 1", got)
	}
	if got := snap.dmReadQueries[1].Get("limit"); got != fmt.Sprint(replyPollPageSize) {
		t.Errorf("poll read limit = %q, want %d", got, replyPollPageSize)
	}
	if got := snap.dmReadQueries[1].Get("around"); got != "" {
		t.Errorf("poll read carries around=%q, want none", got)
	}
}

// TestReplyPoll_VisitorPreflightRefusalSendsNothing: when the hub refuses
// the federated user's read of its conversation (401, 403 or 404) before
// the send, the call ends with the uniform refusal and nothing is sent.
func TestReplyPoll_VisitorPreflightRefusalSendsNothing(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			token := fedUserToken(t)
			hub := newFedUserHub(t, token)
			hub.set(func(h *fedUserHub) { h.dmReadStatus = status })
			_, _, ts := newFedUserTestServer(t, hub, visitorPollConfig)

			code, body := awaitResult(t, sendAsync(t, ts, "agent1", token))
			if code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body %s", code, body)
			}
			assertVisitorRefusal(t, body)
			if n := len(hub.snapshot().sent); n != 0 {
				t.Errorf("%d sends reached the hub, want 0", n)
			}
		})
	}
}

// TestReplyPoll_VisitorReadRefusalEndsCall: when the hub refuses a poll of
// the federated user's conversation (401, 403 or 404) after the send, the
// call ends at once with the uniform refusal, not at the timeout.
func TestReplyPoll_VisitorReadRefusalEndsCall(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			token := fedUserToken(t)
			hub := newFedUserHub(t, token)
			hub.set(func(h *fedUserHub) {
				h.dmReadSeq = []int{0} // the preflight is served
				h.dmReadStatus = status
				h.dmMessages = visitorDMRows(fedUserReplyText)
			})
			_, _, ts := newFedUserTestServer(t, hub, visitorPollConfig)

			start := time.Now()
			code, body := awaitResult(t, sendAsync(t, ts, "agent1", token))
			if code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body %s", code, body)
			}
			assertVisitorRefusal(t, body)
			if strings.Contains(body, fedUserReplyText) {
				t.Errorf("reply returned although the hub refused the read; body %s", body)
			}
			if elapsed := time.Since(start); elapsed >= visitorPollTimeout {
				t.Errorf("call ended after %v, want before the %v timeout", elapsed, visitorPollTimeout)
			}
			if n := len(hub.snapshot().sent); n != 1 {
				t.Errorf("%d sends reached the hub, want 1", n)
			}
		})
	}
}

// TestReplyPoll_VisitorUnavailableReadIsRetried: a 503 on a poll is not a
// refusal; the next poll returns the reply.
func TestReplyPoll_VisitorUnavailableReadIsRetried(t *testing.T) {
	token := fedUserToken(t)
	hub := newFedUserHub(t, token)
	hub.set(func(h *fedUserHub) {
		h.dmReadSeq = []int{0, http.StatusServiceUnavailable}
		h.dmMessages = visitorDMRows(fedUserReplyText)
	})
	_, _, ts := newFedUserTestServer(t, hub, visitorPollConfig)

	code, body := awaitResult(t, sendAsync(t, ts, "agent1", token))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", code, body)
	}
	if !strings.Contains(body, fedUserReplyText) {
		t.Fatalf("response does not carry the agent's reply after a retried read; body %s", body)
	}
}

// TestReplyPoll_VisitorRevokedBeforeForwardStopsReply: the poll finds the
// reply, then the hub refuses the access re-check made before the reply is
// forwarded: the call ends with the uniform refusal and the reply is not
// forwarded.
func TestReplyPoll_VisitorRevokedBeforeForwardStopsReply(t *testing.T) {
	token := fedUserToken(t)
	hub := newFedUserHub(t, token)
	hub.set(func(h *fedUserHub) {
		h.dmReadSeq = []int{0, 0} // the preflight and the poll are served
		h.dmReadStatus = http.StatusForbidden
		h.dmMessages = visitorDMRows(fedUserReplyText)
	})
	_, _, ts := newFedUserTestServer(t, hub, visitorPollConfig)

	code, body := awaitResult(t, sendAsync(t, ts, "agent1", token))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", code, body)
	}
	if strings.Contains(body, fedUserReplyText) {
		t.Fatalf("reply forwarded after the access re-check was refused; body %s", body)
	}
	assertVisitorRefusal(t, body)
	if n := len(hub.snapshot().dmReadKeys); n != 3 {
		t.Errorf("conversation reads = %d, want 3 (preflight, the poll that found the reply, the re-check)", n)
	}
}

// TestReplyPoll_VisitorSameCallerGuard: a second concurrent call by the same
// federated user to the same agent is refused at once and sends nothing;
// the first call still returns its reply.
func TestReplyPoll_VisitorSameCallerGuard(t *testing.T) {
	token := fedUserToken(t)
	hub := newFedUserHub(t, token)
	_, _, ts := newFedUserTestServer(t, hub, visitorPollConfig)

	first := sendAsync(t, ts, "agent1", token)
	waitForSend(t, hub)

	code, body := awaitResult(t, sendAsync(t, ts, "agent1", token))
	if code != http.StatusOK {
		t.Fatalf("second call: status = %d, want 200; body %s", code, body)
	}
	if !strings.Contains(body, replyMsgBusy) {
		t.Fatalf("second call: response does not carry the busy refusal; body %s", body)
	}
	if n := len(hub.snapshot().sent); n != 1 {
		t.Errorf("%d sends reached the hub, want 1 (the first call's)", n)
	}

	hub.set(func(h *fedUserHub) { h.dmMessages = visitorDMRows(fedUserReplyText) })
	code, body = awaitResult(t, first)
	if code != http.StatusOK || !strings.Contains(body, fedUserReplyText) {
		t.Fatalf("first call: status %d, body %s; want the reply", code, body)
	}
}

// visitorDMFiller returns n messages of the caller's own, newest first, all
// newer than the anchor of visitorDMRows: rows the reply selection skips.
func visitorDMFiller(n int) []map[string]interface{} {
	at := time.Now().UTC()
	rows := make([]map[string]interface{}, 0, n)
	for i := 0; i < n; i++ {
		rows = append(rows, map[string]interface{}{"id": fmt.Sprintf("filler-%d", i), "sender": "user:" + fedUserAccountID, "senderId": fedUserAccountID, "type": messages.TypeInstruction, "msg": "later message", "createdAt": at.Add(-time.Duration(i) * time.Millisecond).Format(time.RFC3339Nano)})
	}
	return rows
}

// visitorPollQueries returns the queries of the poll reads (full pages),
// leaving out the preflight and the access re-check.
func visitorPollQueries(snap fedUserHubCalls) []url.Values {
	var polls []url.Values
	for _, q := range snap.dmReadQueries {
		if q.Get("limit") == fmt.Sprint(replyPollPageSize) {
			polls = append(polls, q)
		}
	}
	return polls
}

// TestReplyPoll_VisitorFollowsPageCursor: the anchor is on the second page
// of the federated user's conversation and the reply on the first. The poll
// sends the first page's cursor back for the second page and returns the
// reply.
func TestReplyPoll_VisitorFollowsPageCursor(t *testing.T) {
	token := fedUserToken(t)
	hub := newFedUserHub(t, token)
	rows := visitorDMRows(fedUserReplyText)
	// reply-1, 49 later messages (page 1), then the anchor (page 2).
	paged := append(append([]map[string]interface{}{rows[0]}, visitorDMFiller(replyPollPageSize-1)...), rows[1])
	hub.set(func(h *fedUserHub) { h.dmMessages = paged })
	_, _, ts := newFedUserTestServer(t, hub, visitorPollConfig)

	code, body := awaitResult(t, sendAsync(t, ts, "agent1", token))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", code, body)
	}
	if !strings.Contains(body, fedUserReplyText) {
		t.Fatalf("response does not carry the reply on the first page; body %s", body)
	}
	polls := visitorPollQueries(hub.snapshot())
	if len(polls) != 2 {
		t.Fatalf("poll reads = %d (%v), want 2 (both pages)", len(polls), polls)
	}
	if got := polls[0].Get("cursor"); got != "" {
		t.Errorf("first page read carries cursor %q, want none", got)
	}
	if got, want := polls[1].Get("cursor"), fedUserDMCursorPrefix+fmt.Sprint(replyPollPageSize); got != want {
		t.Errorf("second page read carries cursor %q, want the first page's %q", got, want)
	}
}

// TestReplyPoll_VisitorTooManyPages: more than replyPollMaxPages full pages
// of the federated user's conversation are newer than the anchor. The call
// ends at once with the too-many message after replyPollMaxPages reads,
// each with the previous page's cursor.
func TestReplyPoll_VisitorTooManyPages(t *testing.T) {
	token := fedUserToken(t)
	hub := newFedUserHub(t, token)
	rows := visitorDMRows(fedUserReplyText)
	paged := append(visitorDMFiller(replyPollMaxPages*replyPollPageSize+1), rows[1])
	hub.set(func(h *fedUserHub) { h.dmMessages = paged })
	_, _, ts := newFedUserTestServer(t, hub, visitorPollConfig)

	start := time.Now()
	code, body := awaitResult(t, sendAsync(t, ts, "agent1", token))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", code, body)
	}
	if !strings.Contains(body, replyMsgTooMany) {
		t.Fatalf("response does not carry the too-many message; body %s", body)
	}
	if elapsed := time.Since(start); elapsed >= visitorPollTimeout {
		t.Errorf("call ended after %v, want before the %v timeout", elapsed, visitorPollTimeout)
	}
	polls := visitorPollQueries(hub.snapshot())
	if len(polls) != replyPollMaxPages {
		t.Fatalf("poll reads = %d, want %d", len(polls), replyPollMaxPages)
	}
	for i, q := range polls {
		want := ""
		if i > 0 {
			want = fedUserDMCursorPrefix + fmt.Sprint(i*replyPollPageSize)
		}
		if got := q.Get("cursor"); got != want {
			t.Errorf("page read %d carries cursor %q, want %q", i, got, want)
		}
	}
}

// TestReplyPoll_VisitorPreflightUnavailableProceeds: a 503 on the preflight
// read is not a refusal: the message is sent once and the poll returns the
// reply.
func TestReplyPoll_VisitorPreflightUnavailableProceeds(t *testing.T) {
	token := fedUserToken(t)
	hub := newFedUserHub(t, token)
	hub.set(func(h *fedUserHub) {
		h.dmReadSeq = []int{http.StatusServiceUnavailable}
		h.dmMessages = visitorDMRows(fedUserReplyText)
	})
	_, _, ts := newFedUserTestServer(t, hub, visitorPollConfig)

	code, body := awaitResult(t, sendAsync(t, ts, "agent1", token))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", code, body)
	}
	if !strings.Contains(body, fedUserReplyText) {
		t.Fatalf("response does not carry the reply; body %s", body)
	}
	snap := hub.snapshot()
	if len(snap.sent) != 1 {
		t.Fatalf("%d sends reached the hub, want 1", len(snap.sent))
	}
	if snap.sent[0].DMReadsBefore != 1 {
		t.Errorf("conversation reads before the send = %d, want 1 (the preflight)", snap.sent[0].DMReadsBefore)
	}
}

// TestReplyPoll_VisitorQuestionEndsInputRequired: the agent's question (an
// input-needed message with a thread ID) in the federated user's
// conversation ends the call input-required with the question.
func TestReplyPoll_VisitorQuestionEndsInputRequired(t *testing.T) {
	const question = "which file do you mean?"
	token := fedUserToken(t)
	hub := newFedUserHub(t, token)
	rows := visitorDMRows(fedUserReplyText)
	rows[0]["type"] = messages.TypeInputNeeded
	rows[0]["threadId"] = "thread-1"
	rows[0]["msg"] = question
	hub.set(func(h *fedUserHub) { h.dmMessages = rows })
	_, _, ts := newFedUserTestServer(t, hub, visitorPollConfig)

	code, body := awaitResult(t, sendAsync(t, ts, "agent1", token))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", code, body)
	}
	if !strings.Contains(body, question) {
		t.Fatalf("response does not carry the question; body %s", body)
	}
	if !strings.Contains(body, "input-required") && !strings.Contains(body, "INPUT_REQUIRED") {
		t.Errorf("task did not end input-required; body %s", body)
	}
}

// TestReplyPoll_VisitorIgnoresPushedReply: with reply polling on, a reply
// pushed on the federated user's topic does not end the call; the reply read
// from its conversation does.
func TestReplyPoll_VisitorIgnoresPushedReply(t *testing.T) {
	const polledText = "the reply in the conversation"
	token := fedUserToken(t)
	hub := newFedUserHub(t, token)
	b, _, ts := newFedUserTestServer(t, hub, visitorPollConfig)

	done := sendAsync(t, ts, "agent1", token)
	waitForSend(t, hub)
	deliverReply(t, b)
	hub.set(func(h *fedUserHub) { h.dmMessages = visitorDMRows(polledText) })

	code, body := awaitResult(t, done)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", code, body)
	}
	if !strings.Contains(body, polledText) {
		t.Fatalf("response does not carry the reply read from the conversation; body %s", body)
	}
	if strings.Contains(body, fedUserReplyText) {
		t.Errorf("response carries the pushed reply; body %s", body)
	}
}

// TestVisitorDMReader_KeyNotDerivedIsRefusal: a federated user whose
// account ID does not give a conversation key (not a canonical UUID) gets
// a refusal from the reader and from the access re-check, before any hub
// request (the Bridge here has no hub).
func TestVisitorDMReader_KeyNotDerivedIsRefusal(t *testing.T) {
	caller := &CallerIdentity{TokenType: "federation", Subject: fedUserSubject, IssuerURL: fedUserIssuer, UserID: "account-1"}
	r, err := newVisitorDMReader(&Bridge{}, caller, fedUserAgentID)
	if r != nil || !errors.Is(err, errFederatedUserRefused) {
		t.Errorf("newVisitorDMReader = %v, %v; want no reader and a refusal", r, err)
	}
	if err := (&Bridge{}).recheckFederatedUserAccess(context.Background(), caller, fedUserAgentID); !errors.Is(err, errFederatedUserRefused) || !isHubRefusal(err) {
		t.Errorf("recheckFederatedUserAccess = %v, want a refusal", err)
	}
}

// TestReplyWaitKey_FederatedUser: the same-caller key of a federated user is
// the conversation's reader, its hub account ID, and the agent. A local
// caller with the same account ID reads the same conversation and shares
// the key; another account, another agent or the admin class does not.
func TestReplyWaitKey_FederatedUser(t *testing.T) {
	visitor := &CallerIdentity{TokenType: "federation", Subject: fedUserSubject, IssuerURL: fedUserIssuer, UserID: fedUserAccountID}
	key := replyWaitKey(visitor, "admin@test", fedUserAgentID)
	if got := replyWaitKey(&CallerIdentity{TokenType: "uat", UserID: fedUserAccountID}, "admin@test", fedUserAgentID); got != key {
		t.Errorf("local caller with the same account ID: key %q, want the visitor's %q", got, key)
	}
	other := &CallerIdentity{TokenType: "federation", Subject: "other-subject", IssuerURL: fedUserIssuer, UserID: "44444444-5555-4666-8777-888888888888"}
	for name, got := range map[string]string{
		"another account": replyWaitKey(other, "admin@test", fedUserAgentID),
		"another agent":   replyWaitKey(visitor, "admin@test", "bbbbbbbb-cccc-4ddd-8eee-ffffffffffff"),
		"admin class":     replyWaitKey(nil, "admin@test", fedUserAgentID),
	} {
		if got == key {
			t.Errorf("%s: key %q equals the visitor's", name, got)
		}
	}
}

// assertVisitorRefusal checks that a call ended failed with the uniform
// refusal and with none of the local callers' read messages.
func assertVisitorRefusal(t *testing.T, body string) {
	t.Helper()
	if !strings.Contains(body, federatedUserRefusalText) {
		t.Errorf("response does not carry the uniform refusal; body %s", body)
	}
	if !strings.Contains(body, "FAILED") && !strings.Contains(body, "failed") {
		t.Errorf("task did not end failed; body %s", body)
	}
	for _, local := range []string{replyMsgUnauthorized, replyMsgForbidden, replyMsgNotListed} {
		if strings.Contains(body, local) {
			t.Errorf("response carries a local caller's message %q; body %s", local, body)
		}
	}
}

// visitorCallResult decodes the final task state and status message text
// from a JSON-RPC SendMessage response body.
func visitorCallResult(t *testing.T, body string) (state, text string) {
	t.Helper()
	type statusJSON struct {
		State   string `json:"state"`
		Message *struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"message"`
	}
	var resp struct {
		Result struct {
			Task *struct {
				Status statusJSON `json:"status"`
			} `json:"task"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decoding the response: %v; body %s", err, body)
	}
	if resp.Result.Task == nil {
		t.Fatalf("response carries no task; body %s", body)
	}
	st := resp.Result.Task.Status
	var parts []string
	if st.Message != nil {
		for _, p := range st.Message.Parts {
			parts = append(parts, p.Text)
		}
	}
	return st.State, strings.Join(parts, "\n")
}

// TestExecutor_FederatedUserPolledWaitIgnoresAppendedPushEvent: while a
// federated user's call waits with a live reply poller on its own
// conversation, an agent message appended to the task without the poller's
// mark (as another bridge process that saw the pushed copy would append it)
// does not end the call; the call ends with the reply read from the
// federated user's conversation.
func TestExecutor_FederatedUserPolledWaitIgnoresAppendedPushEvent(t *testing.T) {
	const (
		appendedText = "an appended copy"
		polledText   = "the reply in the federated user's conversation"
	)
	token := fedUserToken(t)
	hub := newFedUserHub(t, token)
	b, _, ts := newFedUserTestServer(t, hub, visitorPollConfig)
	if !b.config.ReplyPoll.IsEnabled() {
		t.Fatal("reply polling is off; the case needs it on")
	}

	done := sendAsync(t, ts, "agent1", token)
	waitForSend(t, hub)
	taskID, _ := onlyOtherTask(t, b, agentKey(fedUserProjectID, "agent1"))
	if !b.taskPollerLive(taskID) {
		t.Fatal("task is not marked as having a live poller after the send")
	}

	// Append the agent's message directly, past the push path's drop.
	b.processAndAppendEvent(context.Background(), taskID, "agent1", &messages.StructuredMessage{
		Version: 1, Sender: "agent:agent1", Recipient: "user:" + fedUserAccountID,
		Type: messages.TypeInstruction, Msg: appendedText,
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
	}, "")

	// The wait reads the log every 100ms; give it several reads.
	select {
	case r := <-done:
		t.Fatalf("call ended on the appended event: %v %v, want it still waiting", r[0], r[1])
	case <-time.After(700 * time.Millisecond):
	}

	hub.set(func(h *fedUserHub) { h.dmMessages = visitorDMRows(polledText) })
	start := time.Now()
	code, body := awaitResult(t, done)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", code, body)
	}
	finalState, text := visitorCallResult(t, body)
	if !strings.Contains(strings.ToLower(finalState), "completed") || text != polledText {
		t.Fatalf("call ended %q %q, want completed with the reply read from the conversation; body %s", finalState, text, body)
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("polled reply took %v to end the call, want it delivered promptly", elapsed)
	}

	// The appended event is in the task's log ahead of the poller's reply,
	// so the wait read it and skipped it.
	events, err := b.store.ReadTaskEvents(context.Background(), taskID, 0, 100)
	if err != nil {
		t.Fatalf("ReadTaskEvents: %v", err)
	}
	appendedAt, polledAt := -1, -1
	for i, ev := range events {
		if !isResponseEvent(ev) {
			continue
		}
		if strings.HasPrefix(ev.DedupKey, replyPollDedupPrefix) {
			if polledAt < 0 {
				polledAt = i
			}
		} else if appendedAt < 0 {
			appendedAt = i
		}
	}
	if appendedAt < 0 || polledAt < 0 || appendedAt > polledAt {
		t.Fatalf("task log response events: appended at %d, polled at %d; want the appended event before the polled reply", appendedAt, polledAt)
	}
}

// TestReplyPoll_VisitorNullPageIsRetried: a read of the federated user's
// conversation that the hub answers 200 with a JSON null body carries no
// page. The poll reports it as a failed read (pollError), retried, not as
// an empty page ("anchor not seen"), and the next poll finds the reply. The
// preflight's access read decodes nothing, so a null answer passes it.
func TestReplyPoll_VisitorNullPageIsRetried(t *testing.T) {
	token := fedUserToken(t)
	hub := newFedUserHub(t, token)
	b, _, _ := newFedUserTestServer(t, hub)
	caller := &CallerIdentity{TokenType: "federation", Subject: fedUserSubject, IssuerURL: fedUserIssuer, UserID: fedUserAccountID, RawToken: token}
	r, err := newVisitorDMReader(b, caller, fedUserAgentID)
	if err != nil {
		t.Fatalf("newVisitorDMReader: %v", err)
	}
	ctx := context.Background()

	hub.set(func(h *fedUserHub) { h.dmNullReads = 1 })
	if refusal := visitorReplyPreflight(ctx, r); refusal != "" {
		t.Fatalf("preflight on a null answer = %q, want no refusal", refusal)
	}

	hub.set(func(h *fedUserHub) {
		h.dmMessages = visitorDMRows(fedUserReplyText)
		h.dmNullReads = 1
	})
	p := newVisitorReplyPoller(b, r, "visitor-null-task", "agent1", fedUserAgentID, "msg-1", nil)
	if outcome, msg := p.pollOnce(ctx); outcome != pollError || msg != nil {
		t.Fatalf("poll on a null answer = (%q, %v), want (%q, nil)", outcome, msg, pollError)
	}
	outcome, msg := p.pollOnce(ctx)
	if outcome != pollFound || msg == nil || msg.ID != "reply-1" {
		t.Fatalf("next poll = (%q, %v), want the reply", outcome, msg)
	}
	if n := len(hub.snapshot().dmReadKeys); n != 3 {
		t.Errorf("conversation reads = %d, want 3 (preflight, null poll, poll)", n)
	}
}

// TestReplyPoll_VisitorAgentKeyNotDerivedSendsNothing: with reply polling
// on, when the federated user's conversation key cannot be derived because
// the hub names the agent by a non-canonical ID, the call ends with the
// uniform refusal before anything is sent or read; it never falls back to
// the push path.
func TestReplyPoll_VisitorAgentKeyNotDerivedSendsNothing(t *testing.T) {
	token := fedUserToken(t)
	hub := newFedUserHub(t, token)
	hub.set(func(h *fedUserHub) {
		h.adminAgents = []map[string]interface{}{
			{"id": strings.ToUpper(fedUserAgentID), "name": "agent1", "slug": "agent1", "projectId": fedUserProjectID, "status": "running"},
		}
	})
	_, _, ts := newFedUserTestServer(t, hub, visitorPollConfig)

	code, body := awaitResult(t, sendAsync(t, ts, "agent1", token))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", code, body)
	}
	assertVisitorRefusal(t, body)
	snap := hub.snapshot()
	if n := len(snap.sent); n != 0 {
		t.Errorf("%d sends reached the hub, want 0", n)
	}
	if n := len(snap.dmReadKeys); n != 0 {
		t.Errorf("%d conversation reads reached the hub, want 0", n)
	}
}

// visitorDMRowsFor is the conversation of the visitor with hub account
// accountID, newest first: the agent's reply, then the anchor.
func visitorDMRowsFor(accountID, replyText string) []map[string]interface{} {
	rows := visitorDMRows(replyText)
	rows[1]["sender"] = "user:" + accountID
	rows[1]["senderId"] = accountID
	return rows
}

// TestExecutor_TwoFederatedUserPollersOnOneAgent: two visitor accounts
// call agent1 at the same time with reply polling on. Each call's poll
// reads only its own conversation, with its own credential, and returns
// only the reply in that conversation.
func TestExecutor_TwoFederatedUserPollersOnOneAgent(t *testing.T) {
	const (
		firstReplyText  = "the reply in the first visitor's conversation"
		secondReplyText = "the reply in the second visitor's conversation"
	)
	token1, token2 := fedUserToken(t), fedUser2Token(t)
	key1 := visitorDMKey(t)
	key2, err := messages.DMConversationKey("agent", fedUserAgentID, "user", fedUser2AccountID)
	if err != nil {
		t.Fatal(err)
	}
	hub := newFedUserHub(t, token1)
	hub.set(func(h *fedUserHub) {
		h.otherUsers = map[string]string{token2: fedUser2AccountID}
		h.dmMessagesByKey = map[string][]map[string]interface{}{}
	})
	b, _, ts := newFedUserTestServer(t, hub, visitorPollConfig)
	aKey := agentKey(fedUserProjectID, "agent1")

	done1 := sendAsync(t, ts, "agent1", token1)
	waitForHubCalls(t, hub, "the first visitor's send", func(s fedUserHubCalls) bool { return sendsWith(s, token1) == 1 })
	done2 := sendAsync(t, ts, "agent1", token2)
	waitForHubCalls(t, hub, "the second visitor's send", func(s fedUserHubCalls) bool { return sendsWith(s, token2) == 1 })

	_, entries := activeTasksOn(b, aKey)
	pollers := map[string]bool{}
	for _, e := range entries {
		if e.userCaller && e.pollerLive {
			pollers[e.callerUserID] = true
		}
	}
	if !pollers[fedUserAccountID] || !pollers[fedUser2AccountID] {
		t.Fatalf("task entries on agent1 %+v, want a live reply poller for each visitor account", entries)
	}

	hub.set(func(h *fedUserHub) {
		h.dmMessagesByKey[key1] = visitorDMRowsFor(fedUserAccountID, firstReplyText)
		h.dmMessagesByKey[key2] = visitorDMRowsFor(fedUser2AccountID, secondReplyText)
	})
	for _, c := range []struct {
		name       string
		done       <-chan [2]interface{}
		own, other string
	}{
		{"first visitor", done1, firstReplyText, secondReplyText},
		{"second visitor", done2, secondReplyText, firstReplyText},
	} {
		status, body := awaitResult(t, c.done)
		if status != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200; body %s", c.name, status, body)
		}
		if !strings.Contains(body, c.own) || strings.Contains(body, c.other) {
			t.Fatalf("%s: response %s, want only the reply in its own conversation", c.name, body)
		}
	}

	snap := hub.snapshot()
	wantKey := map[string]string{token1: key1, token2: key2}
	reads := map[string]int{}
	for i, key := range snap.dmReadKeys {
		tok := snap.dmReadTokens[i]
		want, ok := wantKey[tok]
		if !ok || key != want {
			t.Errorf("read %d: key %q with a credential of neither visitor or of the other visitor, want each visitor's own key with its own credential", i, key)
			continue
		}
		reads[tok]++
	}
	if reads[token1] < 3 || reads[token2] < 3 {
		t.Errorf("conversation reads per visitor = %d and %d, want at least the preflight, a poll and the access re-check for each", reads[token1], reads[token2])
	}
}
