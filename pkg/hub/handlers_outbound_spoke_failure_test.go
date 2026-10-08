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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/require"
)

// errSpokeBus is an eventbus.EventBus whose Publish always returns err.
type errSpokeBus struct{ err error }

func (b errSpokeBus) Publish(context.Context, string, *messages.StructuredMessage) error {
	return b.err
}

func (errSpokeBus) Subscribe(string, eventbus.EventHandler) (eventbus.Subscription, error) {
	return nullSub{}, nil
}

func (errSpokeBus) Close() error { return nil }

var errPluginSpokeDown = errors.New("plugin spoke unavailable")

type outboundSpokeFixture struct {
	srv     *Server
	store   store.Store
	project *store.Project
	user    *store.User
	agent   *store.Agent
	proxy   *MessageBrokerProxy
}

// newOutboundSpokeFixture wires the broker delivery path with the given
// inprocess spoke (nil for none) and a non-observer "chatplugin" spoke that
// always fails. The plugin spoke handles the "web" channel through
// ChannelID, so the dm: backfill (Channel "web") targets it.
func newOutboundSpokeFixture(t *testing.T, inproc eventbus.EventBus) *outboundSpokeFixture {
	t.Helper()
	return newOutboundSpokeFixtureWithPlugin(t, inproc, errSpokeBus{err: errPluginSpokeDown})
}

// newOutboundSpokeFixtureWithPlugin is newOutboundSpokeFixture with the
// given bus as the "chatplugin" spoke.
func newOutboundSpokeFixtureWithPlugin(t *testing.T, inproc, plugin eventbus.EventBus) *outboundSpokeFixture {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: api.NewUUID(), Name: "spoke-fail-project", Slug: "spoke-fail-project"}
	require.NoError(t, s.CreateProject(ctx, project))
	user := &store.User{ID: api.NewUUID(), Email: "spoke-fail@example.com", DisplayName: "Spoke Fail"}
	require.NoError(t, s.CreateUser(ctx, user))
	agent := &store.Agent{
		ID:              api.NewUUID(),
		Name:            "spoke-fail-agent",
		Slug:            "spoke-fail-agent",
		ProjectID:       project.ID,
		Phase:           "running",
		RuntimeBrokerID: "test-broker",
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	var spokes []eventbus.NamedEventBus
	if inproc != nil {
		spokes = append(spokes, eventbus.NamedEventBus{Name: eventbus.InProcessBusName, Bus: inproc})
	}
	spokes = append(spokes, eventbus.NamedEventBus{
		Name: "chatplugin", ChannelID: "web", Bus: plugin,
	})
	fanout := eventbus.NewFanOutEventBus(spokes, slog.Default())
	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)
	proxy := NewMessageBrokerProxy(fanout, s, events,
		func() AgentDispatcher { return noopDispatcher{} }, slog.Default())
	srv.SetMessageBrokerProxy(proxy)

	return &outboundSpokeFixture{srv: srv, store: s, project: project, user: user, agent: agent, proxy: proxy}
}

func (f *outboundSpokeFixture) send(t *testing.T, msg string) *httptest.ResponseRecorder {
	t.Helper()
	return f.sendRequest(t, OutboundMessageRequest{Recipient: "user:" + f.user.Email, Msg: msg})
}

func (f *outboundSpokeFixture) sendRequest(t *testing.T, outReq OutboundMessageRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(outReq)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/agents/"+f.agent.ID+"/outbound-message", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(contextWithIdentity(req.Context(), &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: f.agent.ID},
		ProjectID: f.project.ID,
	}}))
	rr := httptest.NewRecorder()
	f.srv.handleAgentOutboundMessage(rr, req, f.agent.ID)
	return rr
}

func (f *outboundSpokeFixture) storedRows(t *testing.T) int {
	t.Helper()
	res, err := f.store.ListMessages(context.Background(), store.MessageFilter{
		ProjectID:   f.project.ID,
		RecipientID: f.user.ID,
	}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	return len(res.Items)
}

// requireStoredRowsStay asserts the stored row count stays at want for
// 200ms. It polls on the test goroutine rather than using require.Never:
// Never returns at its deadline without waiting for an in-flight condition
// goroutine, which can then call storedRows (and require.NoError) after
// cleanup has closed the store, failing the test from a goroutine after it
// completed and panicking the whole test binary.
func (f *outboundSpokeFixture) requireStoredRowsStay(t *testing.T, want int, msg string) {
	t.Helper()
	for deadline := time.Now().Add(200 * time.Millisecond); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		require.Equal(t, want, f.storedRows(t), msg)
	}
	require.Equal(t, want, f.storedRows(t), msg)
}

// requireSentOnce asserts the normal success response (status "sent" and a
// message_id) and that exactly one row is stored, with no duplicate write.
func (f *outboundSpokeFixture) requireSentOnce(t *testing.T, rr *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusOK, rr.Code, "handler response: %s", rr.Body.String())
	var resp struct {
		MessageID string `json:"message_id"`
		Status    string `json:"status"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Equal(t, "sent", resp.Status)
	require.NotEmpty(t, resp.MessageID)

	require.Eventually(t, func() bool { return f.storedRows(t) >= 1 },
		3*time.Second, 20*time.Millisecond, "expected a stored row")
	f.requireStoredRowsStay(t, 1, "expected exactly one stored row")
}

// TestHandleAgentOutboundMessage_PluginSpokeFailureIsDelivered covers
// ptone/scion#2757: when only a non-observer plugin spoke fails, the
// inprocess spoke has already queued the persisting deliverToUser, so the
// handler must report success (a retry would duplicate the stored row).
// The dm: backfill sets Channel "web", so this pins FanOut's
// channel-targeted branch.
func TestHandleAgentOutboundMessage_PluginSpokeFailureIsDelivered(t *testing.T) {
	f := newOutboundSpokeFixture(t, eventbus.NewInProcessEventBus(slog.Default()))
	f.proxy.Start()
	t.Cleanup(f.proxy.Stop)

	rr := f.send(t, "hello despite a failing plugin")
	f.requireSentOnce(t, rr)
}

// TestHandleAgentOutboundMessage_PluginSpokeFailureNoChannelIsDelivered
// pins the same outcome through FanOut's default branch (no channel, every
// spoke published). An explicit group conversation_id skips the dm:
// backfill, so the message carries no channel.
func TestHandleAgentOutboundMessage_PluginSpokeFailureNoChannelIsDelivered(t *testing.T) {
	f := newOutboundSpokeFixture(t, eventbus.NewInProcessEventBus(slog.Default()))
	f.proxy.Start()
	t.Cleanup(f.proxy.Stop)

	projectID := f.project.ID
	conv := &store.Conversation{
		ID:        api.NewUUID(),
		ProjectID: &projectID,
		Kind:      "group",
		Surface:   "native",
	}
	require.NoError(t, f.store.CreateConversation(context.Background(), conv))

	rr := f.sendRequest(t, OutboundMessageRequest{
		Recipient:      "user:" + f.user.Email,
		Msg:            "hello in a group despite a failing plugin",
		ConversationID: conv.ID,
	})
	f.requireSentOnce(t, rr)

	res, err := f.store.ListMessages(context.Background(), store.MessageFilter{
		ProjectID:   f.project.ID,
		RecipientID: f.user.ID,
	}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, res.Items, 1)
	require.Empty(t, res.Items[0].Channel, "expected the channel-less FanOut branch")
}

// TestHandleAgentOutboundMessage_PluginSpokeFailureWithoutSubscriberFails
// pins that the plugin-failure success branch needs the persisting
// user-message subscription: without an inprocess spoke nothing is stored,
// so the failure is still reported as 502.
func TestHandleAgentOutboundMessage_PluginSpokeFailureWithoutSubscriberFails(t *testing.T) {
	f := newOutboundSpokeFixture(t, nil)
	f.proxy.Start()
	t.Cleanup(f.proxy.Stop)

	rr := f.send(t, "hello with no persisting subscriber")
	require.Equal(t, http.StatusBadGateway, rr.Code, "handler response: %s", rr.Body.String())
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Equal(t, ErrCodeDeliveryFailed, resp.Error.Code)
	require.NotContains(t, rr.Body.String(), errPluginSpokeDown.Error(),
		"the spoke's error text stays in the log, not the response")
	require.Equal(t, 0, f.storedRows(t))
}

// countingSpokeBus is a plugin spoke that accepts every publish and counts
// them.
type countingSpokeBus struct{ n atomic.Int32 }

func (b *countingSpokeBus) Publish(context.Context, string, *messages.StructuredMessage) error {
	b.n.Add(1)
	return nil
}

func (*countingSpokeBus) Subscribe(string, eventbus.EventHandler) (eventbus.Subscription, error) {
	return nullSub{}, nil
}

func (*countingSpokeBus) Close() error { return nil }

// With no persisting subscriber (no inprocess spoke) and a plugin spoke
// that accepts the message, the handler stores the row itself, once,
// emits the user message event, and does not publish to the plugin spoke
// a second time.
func TestHandleAgentOutboundMessage_NoPersistingSubscriberStoresRow(t *testing.T) {
	plugin := &countingSpokeBus{}
	f := newOutboundSpokeFixtureWithPlugin(t, nil, plugin)
	ep := NewChannelEventPublisher()
	t.Cleanup(ep.Close)
	f.srv.SetEventPublisher(ep)
	userEvents, unsub := ep.Subscribe("user." + f.user.ID + ".message")
	t.Cleanup(unsub)
	f.proxy.Start()
	t.Cleanup(f.proxy.Stop)

	rr := f.send(t, "hello with only a plugin spoke")
	f.requireSentOnce(t, rr)
	require.Equal(t, int32(1), plugin.n.Load(), "the plugin spoke gets the message once")

	var resp struct {
		MessageID string `json:"message_id"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	select {
	case evt := <-userEvents:
		var got UserMessageEvent
		require.NoError(t, json.Unmarshal(evt.Data, &got))
		require.Equal(t, resp.MessageID, got.ID, "the event names the stored message")
		require.Equal(t, "hello with only a plugin spoke", got.Msg)
	case <-time.After(3 * time.Second):
		t.Fatal("expected a user message event for the stored row")
	}
}

// TestHandleAgentOutboundMessage_InProcessFailureStillFails pins that a
// failure of the hub's own inprocess spoke is still reported as 502: the
// message was not stored, so the sender must see the failure.
func TestHandleAgentOutboundMessage_InProcessFailureStillFails(t *testing.T) {
	f := newOutboundSpokeFixture(t, errSpokeBus{err: eventbus.ErrEventBusClosed})

	rr := f.send(t, "hello into a closed bus")
	require.Equal(t, http.StatusBadGateway, rr.Code, "handler response: %s", rr.Body.String())
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Equal(t, ErrCodeDeliveryFailed, resp.Error.Code)
	require.Equal(t, 0, f.storedRows(t))
}

// TestHandleAgentOutboundMessage_InProcessBufferFullWithPluginFailure pins
// that a full inprocess subscriber buffer still maps to 503 when a plugin
// spoke fails in the same publish (ptone/scion#2311 behaviour unchanged).
func TestHandleAgentOutboundMessage_InProcessBufferFullWithPluginFailure(t *testing.T) {
	f := newOutboundSpokeFixture(t, alwaysDropUserBus{})

	rr := f.send(t, "hello into a full buffer")
	require.Equal(t, http.StatusServiceUnavailable, rr.Code, "handler response: %s", rr.Body.String())
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Equal(t, ErrCodeUnavailable, resp.Error.Code)
}

// TestHandleAgentOutboundMessage_PluginSpokeFailureSubscribesOnDemand pins
// that the handler creates the persisting subscription itself. Without
// proxy.Start there is no bootstrap subscription, so the stored row comes
// only from the handler's on-demand subscribe.
func TestHandleAgentOutboundMessage_PluginSpokeFailureSubscribesOnDemand(t *testing.T) {
	f := newOutboundSpokeFixture(t, eventbus.NewInProcessEventBus(slog.Default()))
	t.Cleanup(f.proxy.Stop) // no Start: no bootstrap subscription

	f.requireSentOnce(t, f.send(t, "on demand"))
}

// requireDeliveryFailedNoRow asserts a 502 delivery failure with nothing
// stored.
func (f *outboundSpokeFixture) requireDeliveryFailedNoRow(t *testing.T, rr *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusBadGateway, rr.Code, "handler response: %s", rr.Body.String())
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Equal(t, ErrCodeDeliveryFailed, resp.Error.Code)
	f.requireStoredRowsStay(t, 0, "expected no stored row")
}

// TestHandleAgentOutboundMessage_PluginSpokeFailureAfterStopFails pins that
// a stopped proxy (no persisting subscription) still reports the plugin
// failure as 502.
func TestHandleAgentOutboundMessage_PluginSpokeFailureAfterStopFails(t *testing.T) {
	f := newOutboundSpokeFixture(t, eventbus.NewInProcessEventBus(slog.Default()))
	f.proxy.Start()
	f.proxy.Stop()

	f.requireDeliveryFailedNoRow(t, f.send(t, "hello after stop"))
}

// failSubscribeBus is an InProcessEventBus whose Subscribe always fails.
type failSubscribeBus struct{ *eventbus.InProcessEventBus }

func (failSubscribeBus) Subscribe(string, eventbus.EventHandler) (eventbus.Subscription, error) {
	return nil, errors.New("subscribe unavailable")
}

// TestHandleAgentOutboundMessage_PluginSpokeFailureSubscribeErrorFails pins
// that a failed persisting Subscribe still reports the plugin failure as
// 502.
func TestHandleAgentOutboundMessage_PluginSpokeFailureSubscribeErrorFails(t *testing.T) {
	f := newOutboundSpokeFixture(t,
		failSubscribeBus{eventbus.NewInProcessEventBus(slog.Default())})
	t.Cleanup(f.proxy.Stop)

	f.requireDeliveryFailedNoRow(t, f.send(t, "hello with a failed subscribe"))
}

// Channel validation matches the key FanOutEventBus routes on: a spoke
// registered as Name "chat-app" with ChannelID "gchat" accepts "gchat"
// and rejects "chat-app", which Publish could not deliver to.
func TestValidateChannelRegistered_UsesRoutingKey(t *testing.T) {
	srv, s := testServer(t)
	fanout := eventbus.NewFanOutEventBus([]eventbus.NamedEventBus{
		{Name: eventbus.InProcessBusName, Bus: eventbus.NewInProcessEventBus(slog.Default())},
		{Name: "chat-app", ChannelID: "gchat", Bus: nullSpokeEventBus{}},
	}, slog.Default())
	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)
	srv.SetMessageBrokerProxy(NewMessageBrokerProxy(fanout, s, events,
		func() AgentDispatcher { return noopDispatcher{} }, slog.Default()))

	rr := httptest.NewRecorder()
	require.True(t, srv.validateChannelRegistered(rr, "gchat"), "routing key must pass: %s", rr.Body.String())

	rr = httptest.NewRecorder()
	require.False(t, srv.validateChannelRegistered(rr, "chat-app"))
	require.Equal(t, http.StatusBadRequest, rr.Code, "body: %s", rr.Body.String())
	require.Contains(t, rr.Body.String(), "available channels: gchat")
}

// A spoke whose ChannelID is the inprocess bus name passes channel
// validation, but FanOutEventBus.Publish refuses that channel with
// ErrReservedChannel. The handler reports it as a 400 validation error
// and stores no row.
func TestHandleAgentOutboundMessage_ReservedChannelIsBadRequest(t *testing.T) {
	f := newOutboundSpokeFixture(t, eventbus.NewInProcessEventBus(slog.Default()))
	fanout := eventbus.NewFanOutEventBus([]eventbus.NamedEventBus{
		{Name: eventbus.InProcessBusName, Bus: eventbus.NewInProcessEventBus(slog.Default())},
		{Name: "chatplugin", ChannelID: eventbus.InProcessBusName, Bus: nullSpokeEventBus{}},
	}, slog.Default())
	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)
	f.proxy = NewMessageBrokerProxy(fanout, f.store, events,
		func() AgentDispatcher { return noopDispatcher{} }, slog.Default())
	f.srv.SetMessageBrokerProxy(f.proxy)
	f.proxy.Start()
	t.Cleanup(f.proxy.Stop)

	rr := f.sendRequest(t, OutboundMessageRequest{
		Recipient: "user:" + f.user.Email,
		Msg:       "hello on a reserved channel",
		Channel:   eventbus.InProcessBusName,
	})
	require.Equal(t, http.StatusBadRequest, rr.Code, "handler response: %s", rr.Body.String())
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.Equal(t, ErrCodeValidationError, resp.Error.Code)
	require.Contains(t, resp.Error.Message, "reserved for internal use")
	f.requireStoredRowsStay(t, 0, "a refused send stores no row")
}
