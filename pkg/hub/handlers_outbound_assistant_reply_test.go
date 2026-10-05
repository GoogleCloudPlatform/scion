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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The assistant-reply type was the harness's automatic end-of-turn
// transcript mirror. It is retired, but agents on an older sciontool still
// send it every turn. Once a user has web-DMed an agent (recording "web"
// reply affinity), each such send must be dropped: no web DM row, no
// webchat_dm registration, no affinity write, no channel or SSE delivery.
// Deliberate agent messages under the same affinity must still land in the
// web DM.

// recordingSpoke is a channel spoke that records every published message.
type recordingSpoke struct {
	mu   sync.Mutex
	msgs []messages.StructuredMessage
}

func (r *recordingSpoke) Publish(_ context.Context, _ string, msg *messages.StructuredMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = append(r.msgs, *msg)
	return nil
}

func (r *recordingSpoke) Subscribe(string, eventbus.EventHandler) (eventbus.Subscription, error) {
	return nullSub{}, nil
}

func (r *recordingSpoke) Close() error { return nil }

func (r *recordingSpoke) has(body string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range r.msgs {
		if m.Msg == body {
			return true
		}
	}
	return false
}

// webAffinityCountingStore counts RecordChannel(..., "web", ...) calls.
type webAffinityCountingStore struct {
	WebChatStore
	webRecords atomic.Int32
}

func (c *webAffinityCountingStore) RecordChannel(ctx context.Context, userID, projectID, agentID, channel string, at time.Time) error {
	if channel == "web" {
		c.webRecords.Add(1)
	}
	return c.WebChatStore.RecordChannel(ctx, userID, projectID, agentID, channel, at)
}

type assistantReplyFixture struct {
	srv      *Server
	s        store.Store
	wcs      *webAffinityCountingStore
	project  *store.Project
	agent    *store.Agent
	user     *store.User
	dmKey    string
	telegram *recordingSpoke
	teams    *recordingSpoke
	events   <-chan Event
}

func newAssistantReplyFixture(t *testing.T, withBroker bool) *assistantReplyFixture {
	t.Helper()
	srv, s, project, agent, user := def138Setup(t)
	ctx := context.Background()

	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	// Each pooled connection to ":memory:" is a separate, empty database;
	// pin the pool to one so every query sees the webchat tables.
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	inner := NewWebChatStore(db, "sqlite3")
	require.NoError(t, inner.Init())
	wcs := &webAffinityCountingStore{WebChatStore: inner}
	srv.SetWebChatStore(wcs)

	f := &assistantReplyFixture{
		srv: srv, s: s, wcs: wcs, project: project, agent: agent, user: user,
		telegram: &recordingSpoke{}, teams: &recordingSpoke{},
	}
	f.dmKey, err = messages.DMConversationKey("agent", agent.ID, "user", user.ID)
	require.NoError(t, err)

	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)
	ch, unsub := events.Subscribe(
		"user."+user.ID+".message",
		"agent."+agent.ID+".message",
		"user."+user.ID+".chat.dm",
	)
	t.Cleanup(unsub)
	f.events = ch

	if withBroker {
		fanout := eventbus.NewFanOutEventBus([]eventbus.NamedEventBus{
			{Name: eventbus.InProcessBusName, Bus: eventbus.NewInProcessEventBus(slog.Default())},
			{Name: "web", Bus: NewWebChannelBus(slog.Default(), wcs), Observer: true},
			{Name: "telegram", Bus: f.telegram},
			{Name: "teams", Bus: f.teams},
		}, slog.Default())
		proxy := NewMessageBrokerProxy(fanout, s, events,
			func() AgentDispatcher { return nil }, slog.Default())
		proxy.Start()
		t.Cleanup(proxy.Stop)
		srv.SetMessageBrokerProxy(proxy)
		srv.mu.RLock()
		proxy.webChatStore = srv.webChatStore
		proxy.chatNotifier = srv.chatNotifier
		srv.mu.RUnlock()
		proxy.subscribeProjectUserMessages(project.ID)
	} else {
		srv.events = events
	}

	// The user has web-DMed the agent: "web" reply affinity is recorded.
	require.NoError(t, inner.RecordChannel(ctx, user.ID, project.ID, agent.ID, "web", time.Now()))
	return f
}

func (f *assistantReplyFixture) send(t *testing.T, msgType, body string) map[string]interface{} {
	t.Helper()
	raw, _ := json.Marshal(OutboundMessageRequest{
		RecipientID: f.user.ID,
		Msg:         body,
		Type:        msgType,
	})
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/agents/"+f.agent.ID+"/outbound-message", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: f.agent.ID},
		ProjectID: f.project.ID,
	}}))
	rr := httptest.NewRecorder()
	f.srv.handleAgentOutboundMessage(rr, req, f.agent.ID)
	require.Equal(t, http.StatusOK, rr.Code, "body: %s", rr.Body.String())
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	return resp
}

func (f *assistantReplyFixture) waitStored(t *testing.T, body string) store.Message {
	t.Helper()
	var stored store.Message
	require.Eventually(t, func() bool {
		res, err := f.s.ListMessages(context.Background(), store.MessageFilter{
			AgentID: f.agent.ID,
		}, store.ListOptions{Limit: 50})
		if err != nil {
			return false
		}
		for _, m := range res.Items {
			if m.Msg == body {
				stored = m
				return true
			}
		}
		return false
	}, 5*time.Second, 20*time.Millisecond, "message %q not persisted", body)
	return stored
}

// waitSubjects drains events until want are all seen (or timeout), then
// keeps draining briefly so a same-publish subject is not missed.
func (f *assistantReplyFixture) waitSubjects(t *testing.T, want ...string) map[string]bool {
	t.Helper()
	seen := map[string]bool{}
	deadline := time.After(5 * time.Second)
	allSeen := func() bool {
		for _, w := range want {
			if !seen[w] {
				return false
			}
		}
		return true
	}
	for !allSeen() {
		select {
		case evt := <-f.events:
			seen[evt.Subject] = true
		case <-deadline:
			t.Fatalf("timed out waiting for %v; saw %v", want, seen)
		}
	}
	grace := time.After(200 * time.Millisecond)
	for {
		select {
		case evt := <-f.events:
			seen[evt.Subject] = true
		case <-grace:
			return seen
		}
	}
}

func (f *assistantReplyFixture) webDMRows(t *testing.T, convID string) int {
	t.Helper()
	ctx := context.Background()
	n := 0
	byThread, err := f.s.ListMessages(ctx, store.MessageFilter{Channel: "web", ThreadID: f.dmKey}, store.ListOptions{Limit: 50})
	require.NoError(t, err)
	n += len(byThread.Items)
	if convID != "" {
		byConv, err := f.s.ListMessages(ctx, store.MessageFilter{Channel: "web", ConversationID: convID}, store.ListOptions{Limit: 50})
		require.NoError(t, err)
		n += len(byConv.Items)
	}
	return n
}

func TestOutboundAssistantReply_WebAffinity_Dropped(t *testing.T) {
	for _, withBroker := range []bool{true, false} {
		name := "direct"
		if withBroker {
			name = "broker"
		}
		t.Run(name, func(t *testing.T) {
			f := newAssistantReplyFixture(t, withBroker)
			const body = "end-of-turn mirror text"

			resp := f.send(t, messages.TypeAssistantReply, body)
			assert.Equal(t, "dropped", resp["status"])
			assert.NotContains(t, resp, "message_id")

			// A deliberate message afterwards proves the pipeline is live, so
			// the absence checks below are not just a race with delivery.
			const marker = "deliberate marker"
			f.send(t, messages.TypeInputNeeded, marker)
			f.waitStored(t, marker)
			f.waitSubjects(t, "user."+f.user.ID+".chat.dm")

			res, err := f.s.ListMessages(context.Background(), store.MessageFilter{AgentID: f.agent.ID}, store.ListOptions{Limit: 50})
			require.NoError(t, err)
			for _, m := range res.Items {
				assert.NotEqual(t, body, m.Msg, "assistant-reply must not be persisted")
				assert.NotEqual(t, messages.TypeAssistantReply, m.Type)
			}
			assert.False(t, f.telegram.has(body), "assistant-reply must not reach a channel spoke")
			assert.False(t, f.teams.has(body), "assistant-reply must not reach a channel spoke")

			// Affinity is untouched by the dropped send: only the deliberate
			// marker re-recorded it (broker path), and it is still "web".
			lastCh, err := f.wcs.GetLastChannel(context.Background(), f.user.ID, f.project.ID, f.agent.ID)
			require.NoError(t, err)
			assert.Equal(t, "web", lastCh)
		})
	}
}

// TestOutboundAssistantReply_DroppedBeforeAnySideEffect checks the drop in
// isolation: with no deliberate message sent, there are no DM registry rows
// and no web affinity writes at all.
func TestOutboundAssistantReply_DroppedBeforeAnySideEffect(t *testing.T) {
	f := newAssistantReplyFixture(t, true)
	f.send(t, messages.TypeAssistantReply, "end-of-turn mirror text")

	dms, err := f.wcs.ListDMs(context.Background(), f.user.ID)
	require.NoError(t, err)
	assert.Empty(t, dms, "assistant-reply must not register webchat_dm rows")
	assert.Zero(t, f.wcs.webRecords.Load(), "assistant-reply must not re-record web affinity")
	select {
	case evt := <-f.events:
		t.Fatalf("assistant-reply must not publish events, got %s", evt.Subject)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestOutboundDeliberate_WebAffinity_LandsInWebDM(t *testing.T) {
	f := newAssistantReplyFixture(t, true)
	const body = "deliberate agent message"

	f.send(t, messages.TypeInputNeeded, body)
	stored := f.waitStored(t, body)

	assert.Equal(t, "web", stored.Channel, "deliberate message must follow web affinity")
	assert.Equal(t, f.dmKey, stored.ThreadID)
	assert.NotZero(t, f.webDMRows(t, stored.ConversationID), "deliberate message must land in the web DM")

	dms, err := f.wcs.ListDMs(context.Background(), f.user.ID)
	require.NoError(t, err)
	assert.NotEmpty(t, dms, "deliberate message must register the DM")
	assert.NotZero(t, f.wcs.webRecords.Load(), "web outbound re-records web affinity")

	seen := f.waitSubjects(t, "user."+f.user.ID+".chat.dm")
	assert.True(t, seen["user."+f.user.ID+".chat.dm"])
	// Channel-targeted to web: external spokes are not published to.
	assert.False(t, f.telegram.has(body))
}

func TestOutboundDeliberate_DirectPath_StillBackfillsWebDM(t *testing.T) {
	f := newAssistantReplyFixture(t, false)
	const body = "deliberate message, no broker"

	f.send(t, messages.TypeInputNeeded, body)
	stored := f.waitStored(t, body)

	assert.Equal(t, "web", stored.Channel)
	assert.Equal(t, f.dmKey, stored.ThreadID)
	dms, err := f.wcs.ListDMs(context.Background(), f.user.ID)
	require.NoError(t, err)
	assert.NotEmpty(t, dms)
	seen := f.waitSubjects(t, "user."+f.user.ID+".chat.dm")
	assert.True(t, seen["user."+f.user.ID+".chat.dm"])
}
