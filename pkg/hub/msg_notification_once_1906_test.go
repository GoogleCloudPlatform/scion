//go:build !no_sqlite

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

package hub

// Tests for ptone/scion#1906: with a broker proxy configured, a user
// notification is persisted (and announced over SSE) exactly once. These use
// a real, started MessageBrokerProxy on an in-process bus, so the broker's
// deliverToUser subscriber actually runs.

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startRealBrokerProxy wires a started MessageBrokerProxy on an in-process
// bus into env.nd and returns the bus wrapper recording every publish.
func (env *notificationTestEnv) startRealBrokerProxy(t *testing.T) *capturingBus {
	t.Helper()
	inner := eventbus.NewInProcessEventBus(slog.Default())
	bus := &capturingBus{EventBus: inner}
	env.startBrokerProxyOn(t, bus, env.pub, func() { _ = inner.Close() })
	return bus
}

// startBrokerProxyOn starts a MessageBrokerProxy on bus, publishing its SSE
// and receiving lifecycle events on events, and wires it into env.nd.
// closeBus must drain and close the bus; it becomes env.quiesceBroker.
func (env *notificationTestEnv) startBrokerProxyOn(t *testing.T, bus eventbus.EventBus, events *ChannelEventPublisher, closeBus func()) *MessageBrokerProxy {
	t.Helper()
	t.Cleanup(closeBus)
	proxy := NewMessageBrokerProxy(bus, env.store, events, func() AgentDispatcher { return env.dispatcher }, slog.Default())
	proxy.Start()
	t.Cleanup(proxy.Stop)
	env.nd.SetBrokerProxy(proxy)
	env.quiesceBroker = closeBus
	return proxy
}

// useUserSubscription replaces the default agent subscription with a user
// subscription for subscriberID on the given trigger.
func (env *notificationTestEnv) useUserSubscription(t *testing.T, subscriberID, trigger string) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, env.store.DeleteNotificationSubscription(ctx, env.sub.ID))
	require.NoError(t, env.store.CreateNotificationSubscription(ctx, &store.NotificationSubscription{
		ID:                api.NewUUID(),
		AgentID:           env.watched.ID,
		SubscriberType:    store.SubscriberTypeUser,
		SubscriberID:      subscriberID,
		ProjectID:         env.project.ID,
		TriggerActivities: []string{trigger},
		CreatedAt:         time.Now().Add(-time.Minute),
		CreatedBy:         "test",
	}))
}

// userMessageSSECounter counts user.message SSE events for one user.
// ChannelEventPublisher sends synchronously (non-blocking) into the
// subscriber channel, so once the writers have quiesced every event is
// already buffered and count can drain it without waiting.
type userMessageSSECounter struct {
	ch <-chan Event
	n  int
}

func countUserMessageSSE(t *testing.T, events *ChannelEventPublisher, userID string) *userMessageSSECounter {
	t.Helper()
	ch, unsub := events.Subscribe("user." + userID + ".message")
	t.Cleanup(unsub)
	return &userMessageSSECounter{ch: ch}
}

func (env *notificationTestEnv) countUserMessageSSE(t *testing.T, userID string) *userMessageSSECounter {
	t.Helper()
	return countUserMessageSSE(t, env.pub, userID)
}

func (c *userMessageSSECounter) count() int {
	for {
		select {
		case <-c.ch:
			c.n++
		default:
			return c.n
		}
	}
}

func (env *notificationTestEnv) listUserRows(t *testing.T, userID string) []store.Message {
	t.Helper()
	res, err := env.store.ListMessages(context.Background(), store.MessageFilter{
		RecipientID: userID,
		ProjectID:   env.project.ID,
	}, store.ListOptions{})
	require.NoError(t, err)
	return res.Items
}

// settledUserRows waits for the first inbox row for userID, then quiesces
// both writers before counting, so a late duplicate cannot be missed:
// nd.Stop waits for the in-flight storeAndDispatch (the notifier's own
// write and its publish), and closing the broker bus drains every queued
// deliverToUser. No settle sleep is involved.
func (env *notificationTestEnv) settledUserRows(t *testing.T, userID string) []store.Message {
	t.Helper()
	require.Eventually(t, func() bool { return len(env.listUserRows(t, userID)) > 0 }, 5*time.Second, 10*time.Millisecond,
		"the notification must be persisted to the user's inbox")
	env.nd.Stop()
	if env.quiesceBroker != nil {
		env.quiesceBroker()
	}
	return env.listUserRows(t, userID)
}

func TestNotificationDispatcher_RealBrokerPersistsUserNotificationOnce(t *testing.T) {
	env := setupNotificationTest(t)
	userID := api.NewUUID()
	require.NoError(t, env.store.CreateUser(context.Background(), &store.User{ID: userID, Email: "once@example.com", DisplayName: "Once"}))
	env.useUserSubscription(t, userID, "COMPLETED")
	bus := env.startRealBrokerProxy(t)
	sse := env.countUserMessageSSE(t, userID)

	env.nd.Start()
	defer env.nd.Stop()
	env.publishStatus("completed")

	rows := env.settledUserRows(t, userID)
	require.Len(t, rows, 1, "exactly one inbox row per notification")
	assert.Equal(t, "agent:watched-agent", rows[0].Sender)
	assert.Equal(t, messages.TypeStateChange, rows[0].Type)
	assert.NotEmpty(t, rows[0].ConversationID, "the broker path resolves the DM conversation")
	assert.Equal(t, 1, sse.count(), "exactly one user.message SSE publish per notification")
	require.Len(t, bus.published(), 1, "external plugins still receive the notification")
}

func TestNotificationDispatcher_RealBrokerKeepsWaitingForInputBody(t *testing.T) {
	env := setupNotificationTest(t)
	ctx := context.Background()
	require.NoError(t, env.store.UpdateAgentStatus(ctx, env.watched.ID, store.AgentStatusUpdate{
		Activity: "waiting_for_input",
		Message:  "What branch should I target?",
	}))
	userID := api.NewUUID()
	require.NoError(t, env.store.CreateUser(ctx, &store.User{ID: userID, Email: "wfi@example.com", DisplayName: "WFI"}))
	env.useUserSubscription(t, userID, "WAITING_FOR_INPUT")
	bus := env.startRealBrokerProxy(t)

	env.nd.Start()
	defer env.nd.Stop()
	env.publishStatus("waiting_for_input")

	rows := env.settledUserRows(t, userID)
	require.Len(t, rows, 1)
	assert.Equal(t, "What branch should I target?", rows[0].Msg,
		"the broker path must persist the agent's raw question, as the inbox path does")
	assert.Equal(t, messages.TypeInputNeeded, rows[0].Type)
	published := bus.published()
	require.Len(t, published, 1)
	assert.Equal(t, rows[0].Msg, published[0].Msg)
}

// A federated (non-UUID) subscriber under G2 write-deny: deliverToUser drops
// it, so the notifier keeps the G2-exempt inbox write — still exactly once.
func TestNotificationDispatcher_RealBrokerFederatedSubscriberWriteDenyOnce(t *testing.T) {
	env := setupNotificationTest(t)
	const userID = "fed-user@example.org"
	env.useUserSubscription(t, userID, "COMPLETED")
	bus := env.startRealBrokerProxy(t)
	writeDeny := func() bool { return true }
	env.nd.writeDenyEnabled = writeDeny
	env.nd.brokerProxy.writeDenyEnabled = writeDeny
	sse := env.countUserMessageSSE(t, userID)

	env.nd.Start()
	defer env.nd.Stop()
	env.publishStatus("completed")

	rows := env.settledUserRows(t, userID)
	require.Len(t, rows, 1, "exactly one inbox row for a federated subscriber")
	assert.Equal(t, 1, sse.count())
	require.Len(t, bus.published(), 1, "external plugins still receive the notification")
}

// If the publish cannot reach the broker's persisting subscriber at all, the
// notifier persists directly instead of losing the inbox row.
func TestNotificationDispatcher_BrokerPublishFailureFallsBackToInbox(t *testing.T) {
	env := setupNotificationTest(t)
	userID := api.NewUUID()
	env.useUserSubscription(t, userID, "COMPLETED")
	bus := env.startRealBrokerProxy(t)
	require.NoError(t, bus.Close()) // every publish now fails
	sse := env.countUserMessageSSE(t, userID)

	env.nd.Start()
	defer env.nd.Stop()
	env.publishStatus("completed")

	rows := env.settledUserRows(t, userID)
	require.Len(t, rows, 1)
	assert.Equal(t, 1, sse.count())
}

// failingSpoke is a non-observer plugin spoke whose publish always fails.
type failingSpoke struct{}

func (failingSpoke) Publish(context.Context, string, *messages.StructuredMessage) error {
	return errors.New("plugin RPC: connection refused")
}
func (failingSpoke) Subscribe(string, eventbus.EventHandler) (eventbus.Subscription, error) {
	return noopSub{}, nil
}
func (failingSpoke) Close() error { return nil }

type noopSub struct{}

func (noopSub) Unsubscribe() error { return nil }

// Review finding 1: a failing non-observer plugin spoke makes FanOut return
// an error even though inproc already queued the message for deliverToUser.
// That must not trigger the inbox fallback (which would write a second row).
func TestNotificationDispatcher_FailingPluginSpokeStillPersistsOnce(t *testing.T) {
	env := setupNotificationTest(t)
	userID := api.NewUUID()
	env.useUserSubscription(t, userID, "COMPLETED")
	inproc := eventbus.NewInProcessEventBus(slog.Default())
	fan := eventbus.NewFanOutEventBus([]eventbus.NamedEventBus{
		{Name: eventbus.InProcessBusName, Bus: inproc},
		{Name: "telegram", Bus: failingSpoke{}},
	}, slog.Default())
	env.startBrokerProxyOn(t, fan, env.pub, func() { _ = inproc.Close() })
	sse := env.countUserMessageSSE(t, userID)

	env.nd.Start()
	defer env.nd.Stop()
	env.publishStatus("completed")

	rows := env.settledUserRows(t, userID)
	require.Len(t, rows, 1, "a plugin spoke outage must not duplicate the inbox row")
	assert.Equal(t, 1, sse.count())
}

// Review finding 3: the agent becomes running after the proxy started and
// the proxy never sees a lifecycle event (it listens on its own publisher),
// so only the notifier's ensure-subscription call can create the persisting
// subscriber. Without it the publish reaches nobody and no row is written.
func TestNotificationDispatcher_EnsuresSubscriptionWithoutLifecycleEvent(t *testing.T) {
	env := setupNotificationTest(t)
	ctx := context.Background()
	userID := api.NewUUID()
	env.useUserSubscription(t, userID, "COMPLETED")

	// No running agent at proxy start, so bootstrap subscribes nothing.
	for _, a := range []*store.Agent{env.watched, env.subscriber} {
		a.Phase = string(state.PhaseStopped)
		require.NoError(t, env.store.UpdateAgent(ctx, a))
	}
	proxyEvents := NewChannelEventPublisher()
	t.Cleanup(proxyEvents.Close)
	inner := eventbus.NewInProcessEventBus(slog.Default())
	proxy := env.startBrokerProxyOn(t, inner, proxyEvents, func() { _ = inner.Close() })
	proxy.mu.Lock()
	subscribed := proxy.subscribedTopics[eventbus.TopicAllUserMessages(env.project.ID)]
	proxy.mu.Unlock()
	require.False(t, subscribed, "precondition: no user-message subscription before the notification")

	// The agent starts running; only the store changes.
	env.watched.Phase = string(state.PhaseRunning)
	require.NoError(t, env.store.UpdateAgent(ctx, env.watched))
	sse := countUserMessageSSE(t, proxyEvents, userID)

	env.nd.Start()
	defer env.nd.Stop()
	env.publishStatus("completed")

	rows := env.settledUserRows(t, userID)
	require.Len(t, rows, 1)
	assert.Equal(t, 1, sse.count(),
		"the row was persisted by the broker subscriber the notifier ensured (its SSE goes to the proxy's publisher)")
}

// Review finding 6: after Stop the proxy registers nothing and reports "not
// subscribed", so the notifier falls back to the inbox write.
func TestNotificationDispatcher_StoppedProxyFallsBackToInbox(t *testing.T) {
	env := setupNotificationTest(t)
	userID := api.NewUUID()
	env.useUserSubscription(t, userID, "COMPLETED")
	inner := eventbus.NewInProcessEventBus(slog.Default())
	bus := &subscribeCountingBus{EventBus: inner}
	proxy := env.startBrokerProxyOn(t, bus, env.pub, func() { _ = inner.Close() })
	proxy.Stop()
	before := bus.subscribes.Load()

	sse := env.countUserMessageSSE(t, userID)
	env.nd.Start()
	defer env.nd.Stop()
	env.publishStatus("completed")

	rows := env.settledUserRows(t, userID)
	require.Len(t, rows, 1)
	assert.Equal(t, 1, sse.count())
	assert.Equal(t, before, bus.subscribes.Load(), "a stopped proxy must not register subscriptions")
	assert.False(t, proxy.subscribeProjectUserMessages(env.project.ID))
}

// subscribeCountingBus counts Subscribe calls; when gate is non-nil each
// Subscribe signals entered and blocks until gate is closed, and returned is
// set once the inner Subscribe has returned. failFirst makes the first
// Subscribe fail.
type subscribeCountingBus struct {
	eventbus.EventBus
	subscribes atomic.Int32
	entered    chan struct{}
	gate       chan struct{}
	returned   atomic.Bool
	failFirst  bool
}

func (b *subscribeCountingBus) Subscribe(pattern string, h eventbus.EventHandler) (eventbus.Subscription, error) {
	n := b.subscribes.Add(1)
	if b.gate != nil {
		b.entered <- struct{}{}
		<-b.gate
	}
	if b.failFirst && n == 1 {
		return nil, errors.New("subscribe failed")
	}
	sub, err := b.EventBus.Subscribe(pattern, h)
	b.returned.Store(true)
	return sub, err
}

func newProxyOn(t *testing.T, bus eventbus.EventBus) *MessageBrokerProxy {
	t.Helper()
	s := newBrokerTestStore(t)
	events := NewChannelEventPublisher()
	t.Cleanup(events.Close)
	return NewMessageBrokerProxy(bus, s, events, func() AgentDispatcher { return nil }, slog.Default())
}

// Review finding 2: a concurrent caller must not be told "subscribed" before
// the winning caller's Subscribe has returned, or its publish can reach no
// subscriber.
func TestSubscribeProjectUserMessages_ConcurrentCallerWaitsForSubscribe(t *testing.T) {
	inner := eventbus.NewInProcessEventBus(slog.Default())
	t.Cleanup(func() { _ = inner.Close() })
	bus := &subscribeCountingBus{EventBus: inner, entered: make(chan struct{}, 1), gate: make(chan struct{})}
	p := newProxyOn(t, bus)
	const projectID = "11111111-1111-1111-1111-111111111111"

	aDone := make(chan bool, 1)
	go func() { aDone <- p.subscribeProjectUserMessages(projectID) }()
	<-bus.entered // A is inside Subscribe

	type result struct{ ok, subscribeReturned bool }
	bDone := make(chan result, 1)
	go func() {
		ok := p.subscribeProjectUserMessages(projectID)
		bDone <- result{ok, bus.returned.Load()}
	}()
	// Give B the chance to return early (the old bug); a correct B blocks
	// until A's Subscribe returns, so this bound only delays the release.
	var early *result
	select {
	case r := <-bDone:
		early = &r
	case <-time.After(200 * time.Millisecond):
	}
	close(bus.gate)
	require.True(t, <-aDone)
	r := result{}
	if early != nil {
		r = *early
	} else {
		r = <-bDone
	}
	assert.True(t, r.ok)
	assert.True(t, r.subscribeReturned, "a caller told \"subscribed\" must find Subscribe already returned")
	assert.Equal(t, int32(1), bus.subscribes.Load(), "the topic is subscribed once")
}

// Concurrent callers (run with -race): exactly one Subscribe, every caller
// told "subscribed".
func TestSubscribeProjectUserMessages_ConcurrentCallersSubscribeOnce(t *testing.T) {
	inner := eventbus.NewInProcessEventBus(slog.Default())
	t.Cleanup(func() { _ = inner.Close() })
	bus := &subscribeCountingBus{EventBus: inner}
	p := newProxyOn(t, bus)
	const projectID = "22222222-2222-2222-2222-222222222222"

	var wg sync.WaitGroup
	var notOK atomic.Int32
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !p.subscribeProjectUserMessages(projectID) {
				notOK.Add(1)
			}
		}()
	}
	wg.Wait()
	assert.Zero(t, notOK.Load())
	assert.Equal(t, int32(1), bus.subscribes.Load())
}

// A failed Subscribe leaves the topic unmarked (reported as not subscribed),
// so the next call retries instead of publishing to nobody forever.
func TestSubscribeProjectUserMessages_FailureIsRetried(t *testing.T) {
	inner := eventbus.NewInProcessEventBus(slog.Default())
	t.Cleanup(func() { _ = inner.Close() })
	bus := &subscribeCountingBus{EventBus: inner, failFirst: true}
	p := newProxyOn(t, bus)
	const projectID = "33333333-3333-3333-3333-333333333333"

	assert.False(t, p.subscribeProjectUserMessages(projectID))
	assert.True(t, p.subscribeProjectUserMessages(projectID))
	assert.True(t, p.subscribeProjectUserMessages(projectID))
	assert.Equal(t, int32(2), bus.subscribes.Load())
}

// A FanOut bus without an inprocess spoke never runs handlers, so nothing
// would persist: report not subscribed.
func TestSubscribeProjectUserMessages_NoInProcessSpoke(t *testing.T) {
	fan := eventbus.NewFanOutEventBus([]eventbus.NamedEventBus{{Name: "telegram", Bus: failingSpoke{}}}, slog.Default())
	p := newProxyOn(t, fan)
	assert.False(t, p.subscribeProjectUserMessages("44444444-4444-4444-4444-444444444444"))
}

func TestCanPersistUserDM(t *testing.T) {
	const id = "0f8fad5b-d9cb-469f-a165-70867728950e"
	assert.True(t, canPersistUserDM(id, true))
	assert.True(t, canPersistUserDM("fed@example.org", false))
	assert.False(t, canPersistUserDM("fed@example.org", true))
	assert.False(t, canPersistUserDM(strings.ToUpper(id), true),
		"DM keys only accept canonical UUIDs")
}

// topicGatedBus blocks Subscribe for one topic until gate is closed; other
// topics subscribe straight through.
type topicGatedBus struct {
	eventbus.EventBus
	topic   string
	entered chan struct{}
	gate    chan struct{}
}

func (b *topicGatedBus) Subscribe(pattern string, h eventbus.EventHandler) (eventbus.Subscription, error) {
	if pattern == b.topic {
		b.entered <- struct{}{}
		<-b.gate
	}
	return b.EventBus.Subscribe(pattern, h)
}

// returnsWithin runs f and fails the test if it has not returned within d
// (a failure bound, not a sleep: a correct f returns immediately).
func returnsWithin(t *testing.T, d time.Duration, f func() bool) bool {
	t.Helper()
	done := make(chan bool, 1)
	go func() { done <- f() }()
	select {
	case ok := <-done:
		return ok
	case <-time.After(d):
		t.Fatalf("call blocked for %s (want an immediate return)", d)
		return false
	}
}

// msgb-rev-3 round 2, finding 1: a slow (e.g. plugin-backed) first Subscribe
// for project A must not stall projects that are already subscribed, nor
// first subscriptions for other projects.
func TestSubscribeProjectUserMessages_SlowSubscribeDoesNotBlockOtherProjects(t *testing.T) {
	const (
		projectA = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
		projectB = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
		projectC = "cccccccc-cccc-cccc-cccc-cccccccccccc"
	)
	inner := eventbus.NewInProcessEventBus(slog.Default())
	t.Cleanup(func() { _ = inner.Close() })
	bus := &topicGatedBus{
		EventBus: inner,
		topic:    eventbus.TopicAllUserMessages(projectA),
		entered:  make(chan struct{}, 1),
		gate:     make(chan struct{}),
	}
	p := newProxyOn(t, bus)
	var gateOnce sync.Once
	releaseA := func() { gateOnce.Do(func() { close(bus.gate) }) }
	t.Cleanup(releaseA) // never leave A stuck if an assertion fails

	require.True(t, p.subscribeProjectUserMessages(projectB), "B pre-subscribed")

	aDone := make(chan bool, 1)
	go func() { aDone <- p.subscribeProjectUserMessages(projectA) }()
	<-bus.entered // A is stuck inside Subscribe

	assert.True(t, returnsWithin(t, 5*time.Second, func() bool { return p.subscribeProjectUserMessages(projectB) }),
		"an already-subscribed project returns at once")
	assert.True(t, returnsWithin(t, 5*time.Second, func() bool { return p.subscribeProjectUserMessages(projectC) }),
		"a first subscription for another project proceeds")

	releaseA()
	assert.True(t, <-aDone)
}

// The already-subscribed check runs before the topic lock is taken, so a
// subscribed topic answers even while its lock is held.
func TestSubscribeProjectUserMessages_FastPathSkipsTopicLock(t *testing.T) {
	const projectID = "dddddddd-dddd-dddd-dddd-dddddddddddd"
	inner := eventbus.NewInProcessEventBus(slog.Default())
	t.Cleanup(func() { _ = inner.Close() })
	p := newProxyOn(t, inner)
	require.True(t, p.subscribeProjectUserMessages(projectID))

	p.mu.Lock()
	topicMu := p.userSubLocks[eventbus.TopicAllUserMessages(projectID)]
	p.mu.Unlock()
	require.NotNil(t, topicMu)
	topicMu.Lock()
	defer topicMu.Unlock()

	assert.True(t, returnsWithin(t, 5*time.Second, func() bool { return p.subscribeProjectUserMessages(projectID) }))
}

// msgb-rev-3 round 2, finding 2: under write-deny the canonical-UUID check
// only drops DMs whose principal kinds are determined (where DM key
// derivation would fail anyway). A DM with an undetermined kind keeps its
// previous behaviour: persisted without a conversation.
func TestDeliverToUser_NonCanonicalRecipientDropsOnlyWithDeterminedKinds(t *testing.T) {
	const canonical = "0f8fad5b-d9cb-469f-a165-70867728950e"
	nonCanonical := strings.ToUpper(canonical)
	require.NotEqual(t, canonical, nonCanonical)

	cases := []struct {
		name, sender string
		wantRows     int
	}{
		{"determined kinds: dropped", "agent:nc-sender", 0},
		{"undetermined sender kind: persisted", "plugin:nc-sender", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newBrokerTestStore(t)
			projectID := setupBrokerTestProject(t, s)
			p := newNoticeTestProxy(t, s, &brokerMockDispatcher{})
			p.writeDenyEnabled = func() bool { return true }

			msg := messages.NewInstruction(tc.sender, "user:"+nonCanonical, "hello")
			msg.SenderID = "a1b2c3d4-0000-4000-8000-000000000001"
			msg.RecipientID = nonCanonical
			p.deliverToUser(context.Background(), projectID, eventbus.TopicAllUserMessages(projectID), msg)

			res, err := s.ListMessages(context.Background(), store.MessageFilter{RecipientID: nonCanonical}, store.ListOptions{})
			require.NoError(t, err)
			require.Len(t, res.Items, tc.wantRows)
			if tc.wantRows == 1 {
				assert.Empty(t, res.Items[0].ConversationID, "undetermined kinds skip DM conversation resolution")
			}
		})
	}
}
