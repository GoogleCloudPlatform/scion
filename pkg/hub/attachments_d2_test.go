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
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Decision D2 (ptone/scion#4596): an agent-to-user message that carries
// attachments stored as artifacts and takes the broker path is stored by
// the sending handler; deliverToUser then runs only its post-persist steps.

// TestStoredMessageID_NotSettableFromWire (K9): StoredMessageID never
// decodes from JSON, so a message from the wire is persisted once.
func TestStoredMessageID_NotSettableFromWire(t *testing.T) {
	id := uuid.NewString()
	var msg messages.StructuredMessage
	require.NoError(t, json.Unmarshal([]byte(`{"msg":"wire","StoredMessageID":"`+id+`","stored_message_id":"`+id+`"}`), &msg))
	require.Empty(t, msg.StoredMessageID, "StoredMessageID is never read from JSON")
	raw, err := json.Marshal(messages.StructuredMessage{StoredMessageID: id})
	require.NoError(t, err)
	require.NotContains(t, string(raw), id, "StoredMessageID is never written to JSON")

	f := newSendFixture(t)
	proxy := NewMessageBrokerProxy(nil, f.st, NewChannelEventPublisher(), func() AgentDispatcher { return f.disp }, slog.Default())
	msg.Sender, msg.SenderID = "agent:"+f.sender.Slug, f.sender.ID
	msg.Recipient, msg.RecipientID = "user:"+f.ua.Email, f.ua.ID
	proxy.deliverToUser(context.Background(), f.projA.ID, "project."+f.projA.ID+".user.message", &msg)
	require.Equal(t, 1, f.countStored(t, "wire"), "a message decoded from the wire is persisted once")
}

// TestDeliverToUser_SkipsCreateForStoredMessage (K13): with StoredMessageID
// set, deliverToUser stores no row and publishes no SSE event, and leaves the
// message, which the bus shares with its other subscribers, unchanged
// (ptone/scion#5504).
func TestDeliverToUser_SkipsCreateForStoredMessage(t *testing.T) {
	f := newSendFixture(t)
	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)
	ch, unsub := events.Subscribe("user." + f.ua.ID + ".message")
	t.Cleanup(unsub)
	proxy := NewMessageBrokerProxy(nil, f.st, events, func() AgentDispatcher { return f.disp }, slog.Default())
	msg := messages.NewInstruction("agent:"+f.sender.Slug, "user:"+f.ua.Email, "already stored")
	msg.SenderID, msg.RecipientID = f.sender.ID, f.ua.ID
	msg.StoredMessageID = uuid.NewString()
	msg.Metadata = map[string]string{attachmentsMetadataKey: "[]"}
	before := *msg
	before.Metadata = maps.Clone(msg.Metadata)
	proxy.deliverToUser(context.Background(), f.projA.ID, "project."+f.projA.ID+".user.message", msg)
	require.Equal(t, 0, f.countStored(t, "already stored"), "no second row")
	select {
	case evt := <-ch:
		t.Fatalf("unexpected SSE event: %s", evt.Data)
	case <-time.After(200 * time.Millisecond):
	}
	require.Equal(t, before, *msg, "the shared message, its metadata included, is unchanged")
}

// TestDeliverToUser_PersistsOnceWithoutStoredID: without StoredMessageID,
// deliverToUser persists the message once, as before.
func TestDeliverToUser_PersistsOnceWithoutStoredID(t *testing.T) {
	f := newSendFixture(t)
	proxy := NewMessageBrokerProxy(nil, f.st, NewChannelEventPublisher(), func() AgentDispatcher { return f.disp }, slog.Default())
	msg := messages.NewInstruction("agent:"+f.sender.Slug, "user:"+f.ua.Email, "store me")
	msg.SenderID, msg.RecipientID = f.sender.ID, f.ua.ID
	proxy.deliverToUser(context.Background(), f.projA.ID, "project."+f.projA.ID+".user.message", msg)
	require.Equal(t, 1, f.countStored(t, "store me"))
}

// TestPersistingUserSubscriber_InProcessOnly: the persisting user-message
// subscriber gets the publisher's struct in process, so StoredMessageID
// survives the bus hop and no second row is stored.
func TestPersistingUserSubscriber_InProcessOnly(t *testing.T) {
	f := newSendFixture(t)
	proxy := f.withBrokerProxy(t)
	require.True(t, proxy.subscribeProjectUserMessages(f.projA.ID))
	msg := messages.NewInstruction("agent:"+f.sender.Slug, "user:"+f.ua.Email, "in process only")
	msg.SenderID, msg.RecipientID = f.sender.ID, f.ua.ID
	msg.StoredMessageID = uuid.NewString()
	require.NoError(t, proxy.PublishUserMessage(context.Background(), f.projA.ID, f.ua.ID, msg))
	require.Never(t, func() bool { return f.countStored(t, "in process only") != 0 }, 500*time.Millisecond, 25*time.Millisecond,
		"the subscriber saw StoredMessageID and stored nothing")
}

// TestAgentSend_ReservedChannelStoresNothing (K14): on the broker path a
// message naming the reserved in-process channel is refused before
// anything is stored or bound.
func TestAgentSend_ReservedChannelStoresNothing(t *testing.T) {
	f := newSendFixture(t)
	proxy := f.withBrokerProxy(t)
	storeMsg := &store.Message{ID: uuid.NewString(), ProjectID: f.projA.ID, Sender: "agent:" + f.sender.Slug, SenderID: f.sender.ID,
		Recipient: "user:" + f.ua.Email, RecipientID: f.ua.ID, Msg: "reserved", Channel: eventbus.InProcessBusName}
	structured := &messages.StructuredMessage{Sender: storeMsg.Sender, SenderID: storeMsg.SenderID,
		Recipient: storeMsg.Recipient, RecipientID: storeMsg.RecipientID, Msg: storeMsg.Msg, Channel: eventbus.InProcessBusName}
	stored := false
	w := httptest.NewRecorder()
	ok, _ := f.srv.storeOutboundBeforeBroker(context.Background(), w, proxy, f.sender, storeMsg, structured,
		func(context.Context) error { stored = true; return nil })
	require.False(t, ok)
	require.False(t, stored, "nothing is stored or bound")
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "reserved for internal use")
	require.Equal(t, 0, f.countStored(t, "reserved"))
}

// orderRecorder records, in order, watermark writes and SSE publishes.
type orderRecorder struct {
	mu     sync.Mutex
	events []string
}

func (o *orderRecorder) add(e string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, e)
}

func (o *orderRecorder) list() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.events...)
}

type orderedWebChatStore struct {
	WebChatStore
	rec *orderRecorder
}

func (s orderedWebChatStore) TouchDMActivity(ctx context.Context, key, messageID string) error {
	if messageID != "" {
		s.rec.add("watermark:" + messageID)
	}
	return s.WebChatStore.TouchDMActivity(ctx, key, messageID)
}

type orderedEvents struct {
	EventPublisher
	rec *orderRecorder
}

func (e orderedEvents) PublishUserMessage(ctx context.Context, msg *store.Message, a []AttachmentRef, r []artifacts.MessageRef) {
	e.rec.add("sse:" + msg.ID)
	e.EventPublisher.PublishUserMessage(ctx, msg, a, r)
}

// TestAgentSend_BrokerPathStampsWatermark (K18): under D2 the handler
// stamps the DM watermark with the stored id, and before the SSE event.
func TestAgentSend_BrokerPathStampsWatermark(t *testing.T) {
	f := newSendFixture(t)
	proxy := f.withBrokerProxy(t)
	rec := &orderRecorder{}
	proxy.webChatStore = orderedWebChatStore{WebChatStore: f.wcs, rec: rec}
	f.srv.events = orderedEvents{EventPublisher: f.srv.events, rec: rec}
	id := f.upload(t, f.sender, "x.txt", "x")
	resp := f.outbound(t, f.sender, OutboundMessageRequest{Recipient: "user:" + f.ua.Email, Msg: "watermark", AttachmentIDs: []string{id}})
	require.Equal(t, http.StatusOK, resp.Code, resp.Body.String())
	msgID := messageIDOf(t, resp)
	got := rec.list()
	wm, sse := -1, -1
	for i, e := range got {
		if e == "watermark:"+msgID && wm < 0 {
			wm = i
		}
		if e == "sse:"+msgID && sse < 0 {
			sse = i
		}
	}
	require.GreaterOrEqual(t, wm, 0, "the DM watermark carries the stored id: %v", got)
	require.GreaterOrEqual(t, sse, 0, "the SSE event was published: %v", got)
	require.Less(t, wm, sse, "the watermark is stamped before the SSE event: %v", got)

	// The web channel spoke's later empty-id touch leaves the stored id as
	// the DM's last message.
	// The spoke publish completes before the handler answers, so the first
	// time the stored id is the last message it stays so: Eventually only
	// absorbs the asynchronous store write, it cannot pass on a transient.
	key := dmKeyFor(t, "agent", f.sender.ID, "user", f.ua.ID)
	require.Eventually(t, func() bool {
		dms, err := f.wcs.ListDMs(context.Background(), f.ua.ID)
		if err != nil {
			return false
		}
		for _, dm := range dms {
			if dm.ConversationKey == key {
				return dm.LastMessageID == msgID
			}
		}
		return false
	}, 2*time.Second, 25*time.Millisecond, "last_message_id is the stored id")
}

// TestAgentSend_BrokerPathStampsCrossChannelWatermark (K18): a message on
// a non-DM thread stamps the cross-channel DM watermark with the stored
// id, before the SSE event.
func TestAgentSend_BrokerPathStampsCrossChannelWatermark(t *testing.T) {
	f := newSendFixture(t)
	proxy := f.withBrokerProxy(t)
	rec := &orderRecorder{}
	proxy.webChatStore = orderedWebChatStore{WebChatStore: f.wcs, rec: rec}
	f.srv.events = orderedEvents{EventPublisher: f.srv.events, rec: rec}
	id := f.upload(t, f.sender, "x.txt", "x")
	resp := f.outbound(t, f.sender, OutboundMessageRequest{Recipient: "user:" + f.ua.Email, Msg: "in a thread",
		Channel: "web", ThreadID: f.topicA, AttachmentIDs: []string{id}})
	require.Equal(t, http.StatusOK, resp.Code, resp.Body.String())
	msgID := messageIDOf(t, resp)
	got := rec.list()
	wm, sse := -1, -1
	for i, e := range got {
		if e == "watermark:"+msgID && wm < 0 {
			wm = i
		}
		if e == "sse:"+msgID && sse < 0 {
			sse = i
		}
	}
	require.GreaterOrEqual(t, wm, 0, "the cross-channel DM watermark carries the stored id: %v", got)
	require.GreaterOrEqual(t, sse, 0, "the SSE event was published: %v", got)
	require.Less(t, wm, sse, "the watermark is stamped before the SSE event: %v", got)
}

// failingConvStore fails conversation upserts.
type failingConvStore struct{ store.Store }

func (failingConvStore) UpsertConversationByExternalRef(context.Context, *store.Conversation) (*store.Conversation, error) {
	return nil, errors.New("conversation store unavailable")
}

// TestAgentSend_WriteDenyConversationFailureStoresNothing: with write-deny
// on, a conversation resolution failure is answered before anything is
// stored or bound.
func TestAgentSend_WriteDenyConversationFailureStoresNothing(t *testing.T) {
	f := newSendFixture(t)
	proxy := NewMessageBrokerProxy(nil, failingConvStore{f.st}, NewChannelEventPublisher(), func() AgentDispatcher { return f.disp }, slog.Default())
	proxy.writeDenyEnabled = func() bool { return true }
	storeMsg := &store.Message{ID: uuid.NewString(), ProjectID: f.projA.ID, Sender: "agent:" + f.sender.Slug, SenderID: f.sender.ID,
		Recipient: "user:" + f.ua.Email, RecipientID: f.ua.ID, Msg: "no conversation"}
	structured := &messages.StructuredMessage{Sender: storeMsg.Sender, SenderID: storeMsg.SenderID,
		Recipient: storeMsg.Recipient, RecipientID: storeMsg.RecipientID, Msg: storeMsg.Msg}
	stored := false
	w := httptest.NewRecorder()
	ok, _ := f.srv.storeOutboundBeforeBroker(context.Background(), w, proxy, f.sender, storeMsg, structured,
		func(context.Context) error { stored = true; return nil })
	require.False(t, ok)
	require.False(t, stored, "nothing is stored")
	require.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
}

// TestAgentSend_BusFailureAfterStoreAnswersSuccess: a broker publish
// failure after the handler stored the message answers success, and the
// row exists once.
func TestAgentSend_BusFailureAfterStoreAnswersSuccess(t *testing.T) {
	f := newSendFixture(t)
	// A registered spoke whose publish fails.
	inproc := eventbus.NewInProcessEventBus(slog.Default())
	fanout := eventbus.NewFanOutEventBus([]eventbus.NamedEventBus{
		{Name: eventbus.InProcessBusName, Bus: inproc},
		{Name: "failing", Bus: &failingSpokeBus{}},
	}, slog.Default())
	proxy := NewMessageBrokerProxy(fanout, f.st, NewChannelEventPublisher(), func() AgentDispatcher { return f.disp }, slog.Default())
	proxy.Start()
	t.Cleanup(proxy.Stop)
	f.srv.SetMessageBrokerProxy(proxy)
	id := f.upload(t, f.sender, "x.txt", "x")
	rec := f.outbound(t, f.sender, OutboundMessageRequest{Recipient: "user:" + f.ua.Email, Msg: "no spoke",
		Channel: "failing", AttachmentIDs: []string{id}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	f.requireBound(t, id, messageIDOf(t, rec))
	require.Equal(t, 1, f.countStored(t, "no spoke"))
}

// TestAgentSend_NotPersistingBranchSkippedUnderP: without a persisting
// subscriber, the handler under D2 stores the row once and does not run
// the not-persisting branch's second store.
func TestAgentSend_NotPersistingBranchSkippedUnderP(t *testing.T) {
	f := newSendFixture(t)
	// A bus without the in-process spoke: no persisting subscriber.
	fanout := eventbus.NewFanOutEventBus([]eventbus.NamedEventBus{{Name: "spy", Bus: &spyBrokerBus{}}}, slog.Default())
	proxy := NewMessageBrokerProxy(fanout, f.st, NewChannelEventPublisher(), func() AgentDispatcher { return f.disp }, slog.Default())
	proxy.Start()
	t.Cleanup(proxy.Stop)
	f.srv.SetMessageBrokerProxy(proxy)
	require.False(t, proxy.subscribeProjectUserMessages(f.projA.ID))
	id := f.upload(t, f.sender, "x.txt", "x")
	rec := f.outbound(t, f.sender, OutboundMessageRequest{Recipient: "user:" + f.ua.Email, Msg: "not persisting", AttachmentIDs: []string{id}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	// CreateMessage calls are counted by the fixture's fault store,
	// installed before any setup ran.
	require.Equal(t, 1, f.faults.messageCreates("not persisting"), "one CreateMessage call")
}

// failingSpokeBus is a channel spoke whose publish fails.
type failingSpokeBus struct{ spyBrokerBus }

func (*failingSpokeBus) Publish(context.Context, string, *messages.StructuredMessage) error {
	return errors.New("spoke delivery failed")
}
