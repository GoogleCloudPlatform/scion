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

// Operation-level tests for ExecuteAgentKeys (task 2.2, execute_agent_keys.go):
// terminal outcomes on both route shapes, rate-limit budget isolation
// (including ordering), content-free audit with positive controls,
// zero-write/zero-event separation from the messaging model, and no-replay.
// Route-shape/ordering tests specific to
// resolution/authorization precedence (T vs P, cross-project, resolution,
// validation-first ordering) live in authorize_agentkeys_route_test.go;
// this file focuses on what happens *after* authorizeAgentKeys allows the
// call: admission (rate limits, phase/runtime checks, the execute-before
// deadline) and dispatch (agentkeys.Dispatcher). Most of these tests use a
// fully-controllable fake dispatcher (fakeAgentKeysDispatcher) so a test can
// pin every ExecuteAgentKeys-side outcome directly; the
// TestExecuteAgentKeys_RealHTTPDispatcher* tests below additionally
// integrate the real, now-merged task 1.2 HTTPAgentDispatcher end to end
// against a mock broker client, proving the Target this file builds
// (including RuntimeBrokerID) is exactly what that dispatcher needs.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agentkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/require"
)

// fakeAgentKeysDispatcher is a fully-controllable agentkeys.Dispatcher used
// for most ExecuteAgentKeys operation tests. It embeds brokerMockDispatcher
// (messagebroker_test.go) so it satisfies every other AgentDispatcher method
// with an inert no-op, and adds DispatchAgentKeys itself so a test can pin
// exactly what ExecuteAgentKeys observes from "the broker" without needing a
// real broker process. See TestExecuteAgentKeys_RealHTTPDispatcher* below
// for the complementary tests that install the real task 1.2
// HTTPAgentDispatcher instead, against a mock broker *client*.
type fakeAgentKeysDispatcher struct {
	*brokerMockDispatcher

	mu      sync.Mutex
	calls   int
	lastReq struct {
		target        agentkeys.Target
		operationID   string
		executeBefore time.Time
		keys          string
	}

	result agentkeys.BrokerResult
	err    error

	// skipResultDefaults disables DispatchAgentKeys' convenience defaulting
	// of an empty result.OperationID/Outcome to the minted ID/
	// OutcomeDispatched, for tests that need to pin exactly what the
	// handler does when a Dispatcher returns an empty operation ID.
	skipResultDefaults bool
}

func newFakeAgentKeysDispatcher() *fakeAgentKeysDispatcher {
	return &fakeAgentKeysDispatcher{brokerMockDispatcher: &brokerMockDispatcher{}}
}

func (d *fakeAgentKeysDispatcher) DispatchAgentKeys(ctx context.Context, target agentkeys.Target, operationID string, executeBefore time.Time, keys string) (agentkeys.BrokerResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	d.lastReq.target = target
	d.lastReq.operationID = operationID
	d.lastReq.executeBefore = executeBefore
	d.lastReq.keys = keys
	if d.err != nil {
		return agentkeys.BrokerResult{}, d.err
	}
	result := d.result
	if !d.skipResultDefaults {
		if result.OperationID == "" {
			result.OperationID = operationID
		}
		if result.Outcome == "" {
			result.Outcome = agentkeys.OutcomeDispatched
		}
	}
	return result, nil
}

func (d *fakeAgentKeysDispatcher) callCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

var (
	_ agentkeys.Dispatcher = (*fakeAgentKeysDispatcher)(nil)
	_ AgentDispatcher      = (*fakeAgentKeysDispatcher)(nil)
)

// agentKeysWriteSpyStore wraps a store.Store and counts calls to every
// store method the messaging model uses to persist a conversation/message
// (AK-33, issue #2196 AC5: "zero-write/event spies prove complete
// separation from the messaging model for all keys outcomes").
// ExecuteAgentKeys must never call any of these, for any outcome, success
// or failure.
type agentKeysWriteSpyStore struct {
	store.Store
	mu                         sync.Mutex
	createMessageCalls         int
	createConversationCalls    int
	createNotificationCalls    int
	createNotificationSubCalls int
}

func (s *agentKeysWriteSpyStore) CreateMessage(ctx context.Context, msg *store.Message) error {
	s.mu.Lock()
	s.createMessageCalls++
	s.mu.Unlock()
	return s.Store.CreateMessage(ctx, msg)
}

func (s *agentKeysWriteSpyStore) CreateConversation(ctx context.Context, conv *store.Conversation) error {
	s.mu.Lock()
	s.createConversationCalls++
	s.mu.Unlock()
	return s.Store.CreateConversation(ctx, conv)
}

func (s *agentKeysWriteSpyStore) CreateNotification(ctx context.Context, notif *store.Notification) error {
	s.mu.Lock()
	s.createNotificationCalls++
	s.mu.Unlock()
	return s.Store.CreateNotification(ctx, notif)
}

func (s *agentKeysWriteSpyStore) CreateNotificationSubscription(ctx context.Context, sub *store.NotificationSubscription) error {
	s.mu.Lock()
	s.createNotificationSubCalls++
	s.mu.Unlock()
	return s.Store.CreateNotificationSubscription(ctx, sub)
}

func (s *agentKeysWriteSpyStore) totalWrites() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.createMessageCalls + s.createConversationCalls + s.createNotificationCalls + s.createNotificationSubCalls
}

// agentKeysEventSpy counts every EventPublisher call so keys-outcome tests
// can prove zero SSE/observer/notification fan-out (AK-33, issue #2196 AC5)
// with a live spy, not merely an absence that could equally mean "the
// publisher was never wired up at all". It embeds noopEventPublisher so the
// type satisfies the full interface, and
// overrides every method itself (rather than relying on any embedded
// no-op) so a call reaching the interface through the Server's own s.events
// field is provably observed here.
type agentKeysEventSpy struct {
	noopEventPublisher
	mu    sync.Mutex
	calls []string
}

func (e *agentKeysEventSpy) record(method string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, method)
}

func (e *agentKeysEventSpy) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.calls)
}

func (e *agentKeysEventSpy) callsSnapshot() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.calls))
	copy(out, e.calls)
	return out
}

func (e *agentKeysEventSpy) PublishAgentStatus(_ context.Context, _ *store.Agent) {
	e.record("PublishAgentStatus")
}
func (e *agentKeysEventSpy) PublishAgentCreated(_ context.Context, _ *store.Agent) {
	e.record("PublishAgentCreated")
}
func (e *agentKeysEventSpy) PublishAgentDeleted(_ context.Context, _, _ string) {
	e.record("PublishAgentDeleted")
}
func (e *agentKeysEventSpy) PublishProjectCreated(_ context.Context, _ *store.Project) {
	e.record("PublishProjectCreated")
}
func (e *agentKeysEventSpy) PublishProjectUpdated(_ context.Context, _ *store.Project) {
	e.record("PublishProjectUpdated")
}
func (e *agentKeysEventSpy) PublishProjectDeleted(_ context.Context, _ string) {
	e.record("PublishProjectDeleted")
}
func (e *agentKeysEventSpy) PublishBrokerConnected(_ context.Context, _, _ string, _ []string) {
	e.record("PublishBrokerConnected")
}
func (e *agentKeysEventSpy) PublishBrokerDisconnected(_ context.Context, _ string, _ []string) {
	e.record("PublishBrokerDisconnected")
}
func (e *agentKeysEventSpy) PublishBrokerStatus(_ context.Context, _, _ string) {
	e.record("PublishBrokerStatus")
}
func (e *agentKeysEventSpy) PublishNotification(_ context.Context, _ *store.Notification) {
	e.record("PublishNotification")
}
func (e *agentKeysEventSpy) PublishChatNotification(_ context.Context, _ *store.Notification, _ ChatMessageContext) {
	e.record("PublishChatNotification")
}
func (e *agentKeysEventSpy) PublishUserMessage(_ context.Context, _ *store.Message, _ []AttachmentRef) {
	e.record("PublishUserMessage")
}
func (e *agentKeysEventSpy) PublishAgentPorts(_ context.Context, _ *store.Agent) {
	e.record("PublishAgentPorts")
}
func (e *agentKeysEventSpy) PublishAllowListChanged(_ context.Context, _, _ string) {
	e.record("PublishAllowListChanged")
}
func (e *agentKeysEventSpy) PublishInviteChanged(_ context.Context, _, _, _ string) {
	e.record("PublishInviteChanged")
}
func (e *agentKeysEventSpy) PublishDispatchDone(_ context.Context, _ string) {
	e.record("PublishDispatchDone")
}
func (e *agentKeysEventSpy) PublishChatTopicEvent(_ context.Context, _ string, _ string, _ WebChatTopic) {
	e.record("PublishChatTopicEvent")
}
func (e *agentKeysEventSpy) PublishChatReadStateEvent(_ context.Context, _, _, _ string) {
	e.record("PublishChatReadStateEvent")
}
func (e *agentKeysEventSpy) PublishChatOwnReadStateEvent(_ context.Context, _, _, _ string) {
	e.record("PublishChatOwnReadStateEvent")
}
func (e *agentKeysEventSpy) PublishChatMessageEdited(_ context.Context, _, _ string, _ ChatMessageEditedEvent) {
	e.record("PublishChatMessageEdited")
}
func (e *agentKeysEventSpy) PublishChatMessageDeleted(_ context.Context, _, _ string, _ ChatMessageDeletedEvent) {
	e.record("PublishChatMessageDeleted")
}
func (e *agentKeysEventSpy) PublishDMPromotedEvent(_ context.Context, _ string, _ WebChatTopic) {
	e.record("PublishDMPromotedEvent")
}
func (e *agentKeysEventSpy) PublishChatScheduledEvent(_ context.Context, _ string, _ ChatScheduledEvent) {
	e.record("PublishChatScheduledEvent")
}
func (e *agentKeysEventSpy) PublishRaw(_ string, _ interface{}) {
	e.record("PublishRaw")
}

var _ EventPublisher = (*agentKeysEventSpy)(nil)

// newExecuteAgentKeysFixture builds an agentKeysRouteFixture (fixture agents
// in two projects, both Phase: running) plus a fake dispatcher wired in via
// SetDispatcher, a message-write spy store, and an event-publish spy, so
// tests can assert zero messaging-model writes and zero published events.
func newExecuteAgentKeysFixture(t *testing.T) (*agentKeysRouteFixture, *fakeAgentKeysDispatcher, *agentKeysWriteSpyStore, *agentKeysEventSpy) {
	t.Helper()
	f := newAgentKeysRouteFixture(t)
	storeSpy := &agentKeysWriteSpyStore{Store: f.store}
	f.srv.store = storeSpy
	d := newFakeAgentKeysDispatcher()
	f.srv.SetDispatcher(d)
	events := &agentKeysEventSpy{}
	f.srv.SetEventPublisher(events)
	return f, d, storeSpy, events
}

// assertNoKeysSideEffects asserts zero messaging-model store writes and
// zero published events -- the complete AK-33/AC5 separation proof, for any
// keys outcome.
func assertNoKeysSideEffects(t *testing.T, storeSpy *agentKeysWriteSpyStore, events *agentKeysEventSpy) {
	t.Helper()
	if got := storeSpy.totalWrites(); got != 0 {
		t.Errorf("expected zero messaging-model writes, got %d (messages=%d conversations=%d notifications=%d subs=%d)",
			got, storeSpy.createMessageCalls, storeSpy.createConversationCalls, storeSpy.createNotificationCalls, storeSpy.createNotificationSubCalls)
	}
	if got := events.count(); got != 0 {
		t.Errorf("expected zero published events, got %d: %v", got, events.callsSnapshot())
	}
}

// freezeKeysRateLimiters replaces the fixture's production, wall-clock-driven
// rate limiters with clock-frozen ones (same rate/burst constants), so a
// burst test's assertions do not depend on how much real wall-clock time the
// surrounding test happens to take -- a token bucket accrues real refill
// during a slow-running test loop (store/JWT overhead per iteration adds up
// across a burst-sized loop), which would otherwise let one or two extra
// requests through past the nominal burst and make the test flaky rather
// than wrong.
func freezeKeysRateLimiters(f *agentKeysRouteFixture) {
	frozen := func() time.Time { return time.Unix(0, 0) }
	f.srv.keysPrincipalLimiter = newKeysRateLimiterWithClock(agentkeys.PrincipalProjectRateLimit, agentkeys.PrincipalProjectBurst, frozen)
	f.srv.keysTargetLimiter = newKeysRateLimiterWithClock(agentkeys.TargetRateLimit, agentkeys.TargetBurst, frozen)
}

// agentKeysTestPrincipalKey derives the principal+project bucket key for an
// agent-credential caller the exact same way production does
// (agentKeysPrincipalBucketKey), instead of a test hardcoding the
// "type:id:project" string format itself. A hardcoded format string would
// still pass every "probe N times, expect allowed" check even against the
// wrong key -- Allow silently creates a fresh, full bucket for any key that
// doesn't yet exist, so a test that only checks "is this probed key full"
// can never distinguish "this really is the untouched production key" from
// "this is some other key that was never touched by anything". Deriving the
// key through the real function instead means these tests actually break if
// the production key format ever changes.
func agentKeysTestPrincipalKey(callerAgentID, targetProjectID string) string {
	ctx := contextWithIdentity(context.Background(), &agentIdentityWrapper{&AgentTokenClaims{Claims: jwt.Claims{Subject: callerAgentID}}})
	return agentKeysPrincipalBucketKey(ctx, targetProjectID)
}

// TestAgentKeysPrincipalBucketKey_DimensionsAreDistinct is a pure unit test
// (no handler, store or rate limiter involved) pinning that
// agentKeysPrincipalBucketKey's doc-promised dimensions actually produce
// distinct keys: the same raw ID against two different target projects, and
// the same raw ID and project under two different identity types (contract
// requires the limit be per principal *and* project; the doc comment
// promises identity-type namespacing so a user and an agent that happen to
// share a raw ID cannot share or drain each other's bucket).
func TestAgentKeysPrincipalBucketKey_DimensionsAreDistinct(t *testing.T) {
	const rawID = "execkeys-bucketkey-shared-raw-id"
	const projectA = "execkeys-bucketkey-project-a"
	const projectB = "execkeys-bucketkey-project-b"

	agentCtx := contextWithIdentity(context.Background(), &agentIdentityWrapper{&AgentTokenClaims{Claims: jwt.Claims{Subject: rawID}}})
	userCtx := contextWithIdentity(context.Background(), NewAuthenticatedUser(rawID, "", "", "", ""))

	agentKeyA := agentKeysPrincipalBucketKey(agentCtx, projectA)
	agentKeyB := agentKeysPrincipalBucketKey(agentCtx, projectB)
	userKeyA := agentKeysPrincipalBucketKey(userCtx, projectA)

	if agentKeyA == agentKeyB {
		t.Errorf("expected the bucket key to differ by target project for the same identity: got %q for both project %q and project %q", agentKeyA, projectA, projectB)
	}
	if agentKeyA == userKeyA {
		t.Errorf("expected the bucket key to differ by identity type for the same raw ID and project: agent key %q == user key %q", agentKeyA, userKeyA)
	}
}

// keysRouteShape parameterizes a test over both public route shapes, so
// post-admission outcomes are exercised on the project-scoped route as well
// as the top-level one.
type keysRouteShape struct {
	name string
	path func(agent *store.Agent) string
}

var keysRouteShapes = []keysRouteShape{
	{
		name: "top-level",
		path: func(agent *store.Agent) string { return "/api/v1/agents/" + agent.ID + "/keys" },
	},
	{
		name: "project-scoped",
		path: func(agent *store.Agent) string {
			return "/api/v1/projects/" + agent.ProjectID + "/agents/" + agent.Slug + "/keys"
		},
	},
}

// TestExecuteAgentKeys_Success drives a full, authorized, admitted request
// through to a successful dispatch on both route shapes (AK-1/AK-2) and
// pins every part of the success contract: 200 status,
// {"status":"dispatched","operation_id":...,"agent_id":...} body (contract
// §2.4), the exact Target/operationID/keys the dispatcher received, and
// zero messaging-model writes/events (AK-33). Also covers a human-session
// success case on each shape.
func TestExecuteAgentKeys_Success(t *testing.T) {
	for _, shape := range keysRouteShapes {
		t.Run(shape.name+"/agent credential", func(t *testing.T) {
			f, d, storeSpy, events := newExecuteAgentKeysFixture(t)
			token := f.agentToken(t, tid("execkeys-success-"+shape.name), f.projectA.ID, ScopeAgentLifecycle)

			rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, shape.path(f.agentInA), validKeysBody, token)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
			}

			var resp agentkeys.Response
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			if resp.Status != agentkeys.StatusDispatched {
				t.Errorf("status field = %q, want %q", resp.Status, agentkeys.StatusDispatched)
			}
			if resp.OperationID == "" {
				t.Error("expected a non-empty operation_id")
			}
			if resp.AgentID != f.agentInA.ID {
				t.Errorf("agent_id = %q, want %q", resp.AgentID, f.agentInA.ID)
			}

			if got := d.callCount(); got != 1 {
				t.Fatalf("dispatcher called %d times, want exactly 1 (no replay)", got)
			}
			if d.lastReq.operationID != resp.OperationID {
				t.Errorf("dispatcher saw operation ID %q, response carried %q", d.lastReq.operationID, resp.OperationID)
			}
			if d.lastReq.target.AgentID != f.agentInA.ID || d.lastReq.target.AgentSlug != f.agentInA.Slug || d.lastReq.target.ProjectID != f.agentInA.ProjectID {
				t.Errorf("dispatcher target = %+v, want agent %s/%s in project %s", d.lastReq.target, f.agentInA.ID, f.agentInA.Slug, f.agentInA.ProjectID)
			}
			// RuntimeBrokerID is what a real Dispatcher (task 1.2's
			// HTTPAgentDispatcher) uses to resolve the broker endpoint before
			// it can dispatch at all; dropping it from Target would make
			// every production keys call fail with keys_unavailable (see
			// TestExecuteAgentKeys_RealHTTPDispatcherIntegration, which pins
			// this against the real dispatcher, not just this fake).
			if d.lastReq.target.RuntimeBrokerID != f.agentInA.RuntimeBrokerID || d.lastReq.target.RuntimeBrokerID == "" {
				t.Errorf("dispatcher target RuntimeBrokerID = %q, want %q (non-empty)", d.lastReq.target.RuntimeBrokerID, f.agentInA.RuntimeBrokerID)
			}
			if d.lastReq.executeBefore.IsZero() || !d.lastReq.executeBefore.After(time.Now()) {
				t.Errorf("expected a future, non-zero execute-before deadline, got %v", d.lastReq.executeBefore)
			}

			assertNoKeysSideEffects(t, storeSpy, events)
		})

		t.Run(shape.name+"/human session", func(t *testing.T) {
			f, d, storeSpy, events := newExecuteAgentKeysFixture(t)
			rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, shape.path(f.agentInA), validKeysBody)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
			}
			if got := d.callCount(); got != 1 {
				t.Errorf("dispatcher called %d times, want exactly 1", got)
			}
			assertNoKeysSideEffects(t, storeSpy, events)
		})
	}
}

// newMockKeysBrokerClient builds a mockKeysBrokerClient (httpdispatcher_keys_test.go,
// task 1.2) wired to satisfy both RuntimeBrokerClient (via the embedded
// mockRuntimeBrokerClient) and agentkeys.BrokerClient (via its own
// ExecuteKeys), for tests that install the real HTTPAgentDispatcher instead
// of fakeAgentKeysDispatcher.
func newMockKeysBrokerClient() *mockKeysBrokerClient {
	return &mockKeysBrokerClient{mockRuntimeBrokerClient: &mockRuntimeBrokerClient{}}
}

// TestExecuteAgentKeys_RealHTTPDispatcherIntegration installs the real,
// now-merged task 1.2 HTTPAgentDispatcher (via SetDispatcher) instead of
// fakeAgentKeysDispatcher, on both route shapes. fakeAgentKeysDispatcher
// accepts any agentkeys.Target and so cannot catch a Target built with the
// wrong -- or a missing -- RuntimeBrokerID; HTTPAgentDispatcher.DispatchAgentKeys
// genuinely resolves that broker ID against the store
// (getBrokerEndpoint(ctx, target.RuntimeBrokerID)) before it can dispatch at
// all, so this is the only test that actually exercises that mapping
// end-to-end rather than trusting a fake that ignores it.
func TestExecuteAgentKeys_RealHTTPDispatcherIntegration(t *testing.T) {
	for _, shape := range keysRouteShapes {
		t.Run(shape.name+"/success", func(t *testing.T) {
			f := newAgentKeysRouteFixture(t)
			client := newMockKeysBrokerClient()
			client.result = agentkeys.BrokerResult{Outcome: agentkeys.OutcomeDispatched}
			f.srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(f.store, client, false, slog.Default()))
			token := f.agentToken(t, tid("execkeys-realdispatch-success-"+shape.name), f.projectA.ID, ScopeAgentLifecycle)

			before := time.Now()
			rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, shape.path(f.agentInA), validKeysBody, token)
			after := time.Now()
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
			}
			var resp agentkeys.Response
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

			if client.calls != 1 {
				t.Fatalf("expected exactly one broker client call, got %d", client.calls)
			}
			if client.lastBrokerID != f.agentInA.RuntimeBrokerID {
				t.Errorf("brokerID = %q, want %q", client.lastBrokerID, f.agentInA.RuntimeBrokerID)
			}
			if client.lastAgentSlug != f.agentInA.Slug {
				t.Errorf("agentSlug = %q, want %q", client.lastAgentSlug, f.agentInA.Slug)
			}
			if client.lastReq.OperationID != resp.OperationID {
				t.Errorf("client operation_id %q != response operation_id %q", client.lastReq.OperationID, resp.OperationID)
			}
			if client.lastReq.Keys != validKeysBody["keys"] {
				t.Errorf("client keys = %q, want %q", client.lastReq.Keys, validKeysBody["keys"])
			}
			// ExecuteBefore = min(request deadline, now+30s): with no
			// deadline on the incoming request context, admitAndDispatchAgentKeys
			// establishes its own now+DefaultAdmissionWindow, so it must land
			// strictly after "before" and at or before
			// after+DefaultAdmissionWindow.
			if !client.lastReq.ExecuteBefore.After(before) || client.lastReq.ExecuteBefore.After(after.Add(agentkeys.DefaultAdmissionWindow)) {
				t.Errorf("execute_before %v not within (%v, %v]", client.lastReq.ExecuteBefore, before, after.Add(agentkeys.DefaultAdmissionWindow))
			}
		})

		t.Run(shape.name+"/keys_outcome_unknown", func(t *testing.T) {
			f := newAgentKeysRouteFixture(t)
			storeSpy := &agentKeysWriteSpyStore{Store: f.store}
			f.srv.store = storeSpy
			events := &agentKeysEventSpy{}
			f.srv.SetEventPublisher(events)
			client := newMockKeysBrokerClient()
			client.err = errors.New("simulated ambiguous transport failure")
			f.srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(f.store, client, false, slog.Default()))
			token := f.agentToken(t, tid("execkeys-realdispatch-unknown-"+shape.name), f.projectA.ID, ScopeAgentLifecycle)

			rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, shape.path(f.agentInA), validKeysBody, token)
			assertKeysDenialOutcome(t, shape.name+"/keys_outcome_unknown", rec, http.StatusBadGateway, "keys_outcome_unknown")
			if client.calls != 1 {
				t.Errorf("expected exactly one broker client call, got %d", client.calls)
			}
			assertNoKeysSideEffects(t, storeSpy, events)
		})

		t.Run(shape.name+"/keys_unavailable_not_dispatched", func(t *testing.T) {
			f := newAgentKeysRouteFixture(t)
			storeSpy := &agentKeysWriteSpyStore{Store: f.store}
			f.srv.store = storeSpy
			events := &agentKeysEventSpy{}
			f.srv.SetEventPublisher(events)
			client := newMockKeysBrokerClient()
			client.err = agentkeys.ErrNotDispatched
			f.srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(f.store, client, false, slog.Default()))
			token := f.agentToken(t, tid("execkeys-realdispatch-unavail-"+shape.name), f.projectA.ID, ScopeAgentLifecycle)

			rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, shape.path(f.agentInA), validKeysBody, token)
			assertKeysDenialOutcome(t, shape.name+"/keys_unavailable", rec, http.StatusServiceUnavailable, "keys_unavailable")
			if client.calls != 1 {
				t.Errorf("expected exactly one broker client call, got %d", client.calls)
			}
			assertNoKeysSideEffects(t, storeSpy, events)
		})
	}
}

// TestExecuteAgentKeys_NeverDispatchedHTTPRouteIsUnavailable is the end-to-end
// regression for ptone/scion#2628: a broker with no control-channel session
// and no live affinity owner is routed over HTTP, and its endpoint never
// accepts the connection. The dispatch fails only with a timeout error, before
// any request byte was written, so the Hub must answer 503 keys_unavailable,
// not 502 keys_outcome_unknown.
func TestExecuteAgentKeys_NeverDispatchedHTTPRouteIsUnavailable(t *testing.T) {
	for _, shape := range keysRouteShapes {
		t.Run(shape.name, func(t *testing.T) {
			f := newAgentKeysRouteFixture(t)
			storeSpy := &agentKeysWriteSpyStore{Store: f.store}
			f.srv.store = storeSpy
			events := &agentKeysEventSpy{}
			f.srv.SetEventPublisher(events)

			// The handler's own admission window is 30s; end the pending
			// dial sooner through the keys client's timeout.
			httpClient, dials := pendingDialKeysClient(t, 300*time.Millisecond)
			hybrid := NewHybridBrokerClient(NewControlChannelManager(DefaultControlChannelConfig(), slog.Default()), httpClient, nil, false)
			hybrid.SetAffinityLookup(func(context.Context, string) (string, bool) { return "", false })
			f.srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(f.store, hybrid, false, slog.Default()))
			token := f.agentToken(t, tid("execkeys-2628-"+shape.name), f.projectA.ID, ScopeAgentLifecycle)

			rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, shape.path(f.agentInA), validKeysBody, token)
			assertKeysDenialOutcome(t, shape.name, rec, http.StatusServiceUnavailable, "keys_unavailable")
			if dials.Load() == 0 {
				t.Fatal("expected the HTTP route to start a dial")
			}
			assertNoKeysSideEffects(t, storeSpy, events)
		})
	}
}

// TestExecuteAgentKeys_RunningPhaseGate pins AK-26 on both route shapes,
// over a table of non-running phases: a target that is not in the running
// phase is refused before any dispatch is attempted -- no wake/start.
func TestExecuteAgentKeys_RunningPhaseGate(t *testing.T) {
	phases := []string{"stopped", "suspended", "provisioning", "starting"}
	for _, shape := range keysRouteShapes {
		for _, phase := range phases {
			t.Run(shape.name+"/"+phase, func(t *testing.T) {
				f, d, storeSpy, events := newExecuteAgentKeysFixture(t)
				token := f.agentToken(t, tid("execkeys-notrunning-"+shape.name+"-"+phase), f.projectA.ID, ScopeAgentLifecycle)

				f.agentInA.Phase = phase
				require.NoError(t, f.store.UpdateAgent(context.Background(), f.agentInA))

				log := installSentinelLogCapture(t)
				rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, shape.path(f.agentInA), validKeysBody, token)
				assertKeysDenialOutcome(t, shape.name+"/"+phase, rec, http.StatusConflict, "agent_not_running")
				assertKeysDenialOutcomeAuditMatches(t, shape.name+"/"+phase, rec, log)

				if got := d.callCount(); got != 0 {
					t.Errorf("dispatcher called %d times, want 0 (no wake/start attempt)", got)
				}
				assertNoKeysSideEffects(t, storeSpy, events)
			})
		}
	}
}

// TestExecuteAgentKeys_ManagedRuntimeUnsupported pins AK-28's
// managed-backend half on both route shapes: a managed-runtime target is
// refused before any dispatch is attempted, never downgraded to a message
// send.
func TestExecuteAgentKeys_ManagedRuntimeUnsupported(t *testing.T) {
	for _, shape := range keysRouteShapes {
		t.Run(shape.name, func(t *testing.T) {
			f, d, storeSpy, events := newExecuteAgentKeysFixture(t)
			token := f.agentToken(t, tid("execkeys-managed-"+shape.name), f.projectA.ID, ScopeAgentLifecycle)

			f.agentInA.Runtime = ManagedRuntimePrefix + "google"
			require.NoError(t, f.store.UpdateAgent(context.Background(), f.agentInA))

			log := installSentinelLogCapture(t)
			rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, shape.path(f.agentInA), validKeysBody, token)
			assertKeysDenialOutcome(t, shape.name, rec, http.StatusUnprocessableEntity, "keys_unsupported")
			assertKeysDenialOutcomeAuditMatches(t, shape.name, rec, log)

			if got := d.callCount(); got != 0 {
				t.Errorf("dispatcher called %d times, want 0", got)
			}
			assertNoKeysSideEffects(t, storeSpy, events)
		})
	}
}

// TestExecuteAgentKeys_DispatchOutcomes is a table of every dispatch-layer
// outcome ExecuteAgentKeys must classify correctly via
// agentkeys.ClassifyDispatchError (contract §4.3), on both route shapes,
// each proven with exactly one dispatcher call (no replay, AK-34) and zero
// messaging-model side effects.
func TestExecuteAgentKeys_DispatchOutcomes(t *testing.T) {
	cases := []struct {
		name        string
		dispatchErr error
		wantStatus  int
		wantCode    string
	}{
		{
			name:        "AK-27/45/46: broker reports target not found (identity-binding mismatch)",
			dispatchErr: &agentkeys.BrokerOutcomeError{Outcome: agentkeys.OutcomeNotFound},
			wantStatus:  http.StatusNotFound, wantCode: "not_found",
		},
		{
			name:        "AK-27: terminal not ready",
			dispatchErr: &agentkeys.BrokerOutcomeError{Outcome: agentkeys.OutcomeTerminalNotReady},
			wantStatus:  http.StatusConflict, wantCode: "terminal_not_ready",
		},
		{
			name:        "AK-28/47: broker reports keys unsupported (old broker or unsupported runtime)",
			dispatchErr: &agentkeys.BrokerOutcomeError{Outcome: agentkeys.OutcomeKeysUnsupported},
			wantStatus:  http.StatusUnprocessableEntity, wantCode: "keys_unsupported",
		},
		{
			name:        "AK-29: not dispatched (no immediate route / expired admission)",
			dispatchErr: agentkeys.ErrNotDispatched,
			wantStatus:  http.StatusServiceUnavailable, wantCode: "keys_unavailable",
		},
		{
			name:        "AK-30: ambiguous/unclassified error never claims delivery",
			dispatchErr: errors.New("keys: simulated ambiguous transport failure"),
			wantStatus:  http.StatusBadGateway, wantCode: "keys_outcome_unknown",
		},
	}

	for _, shape := range keysRouteShapes {
		for _, tc := range cases {
			t.Run(shape.name+"/"+tc.name, func(t *testing.T) {
				f, d, storeSpy, events := newExecuteAgentKeysFixture(t)
				d.err = tc.dispatchErr
				token := f.agentToken(t, tid("execkeys-outcome-"+shape.name+"-"+tc.wantCode), f.projectA.ID, ScopeAgentLifecycle)

				rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, shape.path(f.agentInA), validKeysBody, token)
				assertKeysDenialOutcome(t, shape.name+"/"+tc.name, rec, tc.wantStatus, tc.wantCode)

				if got := d.callCount(); got != 1 {
					t.Errorf("%s: dispatcher called %d times, want exactly 1 (no automatic replay)", tc.name, got)
				}
				if tc.wantCode == "keys_outcome_unknown" {
					env := decodeKeysError(t, rec.Body.Bytes())
					if strings.Contains(strings.ToLower(env.Message), "delivered") || strings.Contains(strings.ToLower(env.Message), "dispatched") {
						t.Errorf("%s: message must not claim delivery: %q", tc.name, env.Message)
					}
				}
				assertNoKeysSideEffects(t, storeSpy, events)
			})
		}
	}
}

// TestExecuteAgentKeys_NoDispatcherConfigured pins AK-29's "no immediate
// route" case, on both route shapes, for a Hub with no dispatcher wired at
// all: 503 keys_unavailable, never a wake or queue.
func TestExecuteAgentKeys_NoDispatcherConfigured(t *testing.T) {
	for _, shape := range keysRouteShapes {
		t.Run(shape.name, func(t *testing.T) {
			f := newAgentKeysRouteFixture(t)
			storeSpy := &agentKeysWriteSpyStore{Store: f.store}
			f.srv.store = storeSpy
			events := &agentKeysEventSpy{}
			f.srv.SetEventPublisher(events)
			token := f.agentToken(t, tid("execkeys-nodispatcher-"+shape.name), f.projectA.ID, ScopeAgentLifecycle)

			log := installSentinelLogCapture(t)
			rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, shape.path(f.agentInA), validKeysBody, token)
			assertKeysDenialOutcome(t, shape.name, rec, http.StatusServiceUnavailable, "keys_unavailable")
			assertKeysDenialOutcomeAuditMatches(t, shape.name, rec, log)
			assertNoKeysSideEffects(t, storeSpy, events)
		})
	}
}

// newExecuteAgentKeysFixtureWithFailingLookups is newExecuteAgentKeysFixture
// plus a lookup-failing layer (agentKeysLookupSpyStore, failLookups: true)
// wrapped around the write-spy store, for exercising
// finishAgentKeysInternalError's generic 5xx path while still counting any
// messaging-model write that might occur.
func newExecuteAgentKeysFixtureWithFailingLookups(t *testing.T) (*agentKeysRouteFixture, *agentKeysWriteSpyStore, *agentKeysEventSpy) {
	t.Helper()
	f := newAgentKeysRouteFixture(t)
	writeSpy := &agentKeysWriteSpyStore{Store: f.store}
	lookupSpy := &agentKeysLookupSpyStore{Store: writeSpy, failLookups: true}
	f.srv.store = lookupSpy
	f.srv.SetDispatcher(newFakeAgentKeysDispatcher())
	events := &agentKeysEventSpy{}
	f.srv.SetEventPublisher(events)
	return f, writeSpy, events
}

// TestExecuteAgentKeys_NoMessagingSideEffectsOnAnyOutcome closes the
// remaining gap in AC5's "zero-write/event spies prove complete separation
// from the messaging model for all keys outcomes" requirement: the other
// outcome tests (Success, RunningPhaseGate, ManagedRuntimeUnsupported,
// DispatchOutcomes, NoDispatcherConfigured) only assert
// assertNoKeysSideEffects on outcomes reached *after* authorizeAgentKeys
// allows the call. This table covers every outcome decided before or during
// admission that those tests do not: keys_denied, cross-project, not_found,
// the rate limit, both validation failures, and the resolution-failure
// internal 5xx -- on both route shapes. A mutation that published an event
// or wrote a message on any one of these paths must fail exactly one of
// these subtests (this is not hypothetical: a mutation publishing on every
// denial/not_found path passed the rest of this suite untouched before this
// test existed).
func TestExecuteAgentKeys_NoMessagingSideEffectsOnAnyOutcome(t *testing.T) {
	for _, shape := range keysRouteShapes {
		t.Run(shape.name+"/keys_denied", func(t *testing.T) {
			f, _, storeSpy, events := newExecuteAgentKeysFixture(t)
			token := f.agentToken(t, tid("execkeys-sideeffects-denied-"+shape.name), f.projectA.ID) // no lifecycle scope
			log := installSentinelLogCapture(t)
			rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, shape.path(f.agentInA), validKeysBody, token)
			assertKeysDenialOutcome(t, shape.name+"/keys_denied", rec, http.StatusForbidden, "keys_denied")
			assertKeysDenialOutcomeAuditMatches(t, shape.name+"/keys_denied", rec, log)
			assertNoKeysSideEffects(t, storeSpy, events)
		})

		t.Run(shape.name+"/cross_project_keys_unsupported", func(t *testing.T) {
			f, _, storeSpy, events := newExecuteAgentKeysFixture(t)
			token := f.agentToken(t, tid("execkeys-sideeffects-crossproj-"+shape.name), f.projectA.ID, ScopeAgentLifecycle)
			log := installSentinelLogCapture(t)
			rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, shape.path(f.agentInB), validKeysBody, token)
			assertKeysDenialOutcome(t, shape.name+"/cross_project", rec, http.StatusUnprocessableEntity, "cross_project_keys_unsupported")
			assertKeysDenialOutcomeAuditMatches(t, shape.name+"/cross_project_keys_unsupported", rec, log)
			assertNoKeysSideEffects(t, storeSpy, events)
		})

		t.Run(shape.name+"/not_found", func(t *testing.T) {
			f, _, storeSpy, events := newExecuteAgentKeysFixture(t)
			token := f.agentToken(t, tid("execkeys-sideeffects-notfound-"+shape.name), f.projectA.ID, ScopeAgentLifecycle)
			path := "/api/v1/agents/" + tid("execkeys-sideeffects-nonexistent") + "/keys"
			if shape.name == "project-scoped" {
				path = "/api/v1/projects/" + f.projectA.ID + "/agents/does-not-exist/keys"
			}
			log := installSentinelLogCapture(t)
			rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, path, validKeysBody, token)
			assertKeysDenialOutcome(t, shape.name+"/not_found", rec, http.StatusNotFound, "not_found")
			assertKeysDenialOutcomeAuditMatches(t, shape.name+"/not_found", rec, log)
			assertNoKeysSideEffects(t, storeSpy, events)
		})

		t.Run(shape.name+"/keys_rate_limited", func(t *testing.T) {
			f, _, storeSpy, events := newExecuteAgentKeysFixture(t)
			freezeKeysRateLimiters(f)
			token := f.agentToken(t, tid("execkeys-sideeffects-ratelimit-"+shape.name), f.projectA.ID, ScopeAgentLifecycle)
			log := installSentinelLogCapture(t)
			var last *httptest.ResponseRecorder
			for i := 0; i < agentkeys.PrincipalProjectBurst+1; i++ {
				last = doRequestWithAgentToken(t, f.srv, http.MethodPost, shape.path(f.agentInA), validKeysBody, token)
			}
			assertKeysDenialOutcome(t, shape.name+"/keys_rate_limited", last, http.StatusTooManyRequests, "keys_rate_limited")
			assertKeysDenialOutcomeAuditMatches(t, shape.name+"/keys_rate_limited", last, log)
			assertNoKeysSideEffects(t, storeSpy, events)
		})

		t.Run(shape.name+"/invalid_request", func(t *testing.T) {
			f, _, storeSpy, events := newExecuteAgentKeysFixture(t)
			token := f.agentToken(t, tid("execkeys-sideeffects-invalid-"+shape.name), f.projectA.ID, ScopeAgentLifecycle)
			rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, shape.path(f.agentInA), map[string]string{}, token)
			assertKeysValidationFailure(t, rec, http.StatusBadRequest, "invalid_request")
			assertNoKeysSideEffects(t, storeSpy, events)
		})

		t.Run(shape.name+"/payload_too_large", func(t *testing.T) {
			f, _, storeSpy, events := newExecuteAgentKeysFixture(t)
			token := f.agentToken(t, tid("execkeys-sideeffects-oversized-"+shape.name), f.projectA.ID, ScopeAgentLifecycle)
			rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, shape.path(f.agentInA),
				map[string]string{"keys": strings.Repeat("a", agentkeys.MaxBytes+1)}, token)
			assertKeysValidationFailure(t, rec, http.StatusRequestEntityTooLarge, "payload_too_large")
			assertNoKeysSideEffects(t, storeSpy, events)
		})

		// The transport-level read (io.ReadAll(http.MaxBytesReader(...)) in
		// beginAgentKeysRequest) is a separate branch from ValidateBody's own
		// 413/400: it writes its own response and audit record before
		// ValidateBody ever runs, so it needs its own side-effect proof
		// rather than relying on the ValidateBody-path subtests above to
		// stand in for it.
		t.Run(shape.name+"/payload_too_large_transport", func(t *testing.T) {
			f, _, storeSpy, events := newExecuteAgentKeysFixture(t)
			token := f.agentToken(t, tid("execkeys-sideeffects-oversized-transport-"+shape.name), f.projectA.ID, ScopeAgentLifecycle)
			rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, shape.path(f.agentInA),
				map[string]string{"keys": strings.Repeat("a", agentkeys.MaxHTTPBodyBytes+100)}, token)
			assertKeysValidationFailure(t, rec, http.StatusRequestEntityTooLarge, "payload_too_large")
			assertNoKeysSideEffects(t, storeSpy, events)
		})

		t.Run(shape.name+"/invalid_request_read_error", func(t *testing.T) {
			f, _, storeSpy, events := newExecuteAgentKeysFixture(t)
			token := f.agentToken(t, tid("execkeys-sideeffects-readerr-"+shape.name), f.projectA.ID, ScopeAgentLifecycle)
			req := httptest.NewRequest(http.MethodPost, shape.path(f.agentInA), nil)
			req.Body = io.NopCloser(&erroringReader{err: errors.New("simulated read failure")})
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Scion-Agent-Token", token)
			rec := httptest.NewRecorder()
			f.srv.Handler().ServeHTTP(rec, req)
			assertKeysValidationFailure(t, rec, http.StatusBadRequest, "invalid_request")
			assertNoKeysSideEffects(t, storeSpy, events)
		})

		t.Run(shape.name+"/internal_error", func(t *testing.T) {
			f, storeSpy, events := newExecuteAgentKeysFixtureWithFailingLookups(t)
			callerID := tid("execkeys-sideeffects-internalerr-" + shape.name)
			token := f.agentToken(t, callerID, f.projectA.ID, ScopeAgentLifecycle)
			claims, err := f.srv.GetAgentTokenService().ValidateAgentToken(token)
			require.NoError(t, err)
			log := installSentinelLogCapture(t)

			rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, shape.path(f.agentInA), validKeysBody, token)
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("%s/internal_error: status = %d, want 500: %s", shape.name, rec.Code, rec.Body.String())
			}
			env := decodeKeysError(t, rec.Body.Bytes())
			if env.Code != "internal_error" {
				t.Errorf("%s/internal_error: code = %q, want internal_error", shape.name, env.Code)
			}
			opID, _ := env.Details["operation_id"].(string)
			if opID == "" {
				t.Errorf("%s/internal_error: expected a non-empty operation_id, got details=%v", shape.name, env.Details)
			}

			outcome := lastOutcomeAuditRecord(t, log)
			if outcome["decision"] != "" {
				t.Errorf("%s/internal_error: audit decision = %q, want empty (not an agentkeys.Outcome value)", shape.name, outcome["decision"])
			}
			if outcome["error_class"] != "internal_error" {
				t.Errorf("%s/internal_error: audit error_class = %q, want internal_error", shape.name, outcome["error_class"])
			}
			if outcome["operation_id"] != opID {
				t.Errorf("%s/internal_error: audit operation_id %q != response operation_id %q", shape.name, outcome["operation_id"], opID)
			}
			// logAgentKeysInternalErrorAudit shares agentKeysAuditActor with
			// logAgentKeysAudit, but nothing had asserted it on this
			// record. The target is whatever was known when the lookup
			// failed: the top-level route never learns even the project
			// (GetAgent's failure carries no target at all), while the
			// project-scoped route already knows the URL {project} before
			// resolveProjectAgent ever runs.
			wantTargetProjectID := ""
			if shape.name == "project-scoped" {
				wantTargetProjectID = f.projectA.ID
			}
			assertAuditIdentityFields(t, "outcome", outcome, auditIdentityExpectation{
				actorType:       "agent",
				actorID:         callerID,
				sourceProjectID: f.projectA.ID,
				targetAgentID:   "",
				targetProjectID: wantTargetProjectID,
				credentialKind:  "agent_jwt",
				credentialID:    claims.ID,
			})
			assertNoKeysSideEffects(t, storeSpy, events)
		})
	}
}

// TestExecuteAgentKeys_EventSpyPositiveControl proves agentKeysEventSpy is
// actually wired into the fixture's Server and intercepts calls made
// through the s.events interface field -- the same path every real handler
// uses -- rather than only counting calls made directly against the spy
// value.
func TestExecuteAgentKeys_EventSpyPositiveControl(t *testing.T) {
	f, _, _, events := newExecuteAgentKeysFixture(t)
	if got := events.count(); got != 0 {
		t.Fatalf("expected zero events before the control publish, got %d", got)
	}
	f.srv.events.PublishAgentStatus(context.Background(), f.agentInA)
	if got := events.count(); got != 1 {
		t.Fatalf("expected the spy to record the control publish exactly once, got %d: %v", got, events.callsSnapshot())
	}
}

// TestExecuteAgentKeys_StoreSpyPositiveControl is
// TestExecuteAgentKeys_EventSpyPositiveControl's counterpart for
// agentKeysWriteSpyStore: it proves the store spy is actually wired into
// the fixture's Server and intercepts a call made through the s.store
// interface field -- the same path every real handler uses -- rather than
// trusting an absence of writes that could equally mean "never wired up".
func TestExecuteAgentKeys_StoreSpyPositiveControl(t *testing.T) {
	f, _, storeSpy, _ := newExecuteAgentKeysFixture(t)
	if got := storeSpy.totalWrites(); got != 0 {
		t.Fatalf("expected zero writes before the control message, got %d", got)
	}

	msg := &store.Message{
		ID:        tid("execkeys-storespy-control-msg"),
		ProjectID: f.projectA.ID,
		AgentID:   f.agentInA.ID,
		Sender:    "user:" + f.owner.Email,
		SenderID:  f.owner.ID,
		Recipient: "agent:" + f.agentInA.Slug,
		Msg:       "control message",
		Type:      messages.TypeChat,
		Channel:   "web",
		ThreadID:  "execkeys-storespy-control-thread",
		CreatedAt: time.Now().UTC(),
	}
	require.NoError(t, f.srv.store.CreateMessage(context.Background(), msg))
	if got := storeSpy.totalWrites(); got != 1 {
		t.Fatalf("expected the spy to record the control CreateMessage exactly once, got %d", got)
	}
}

// TestExecuteAgentKeys_RateLimiting exercises both independent token
// buckets (contract "Concrete defaults": 5 req/s burst 10 per
// principal+project, 10 req/s burst 20 per target) and proves keys rate
// limiting is separate from the aggregate DM message allowance
// (chatSendLimiter) in the "keys exhausted, DM unaffected" direction; see
// TestExecuteAgentKeys_DMBudgetDoesNotBlockKeys for the reverse direction.
func TestExecuteAgentKeys_RateLimiting(t *testing.T) {
	f, d, _, _ := newExecuteAgentKeysFixture(t)
	freezeKeysRateLimiters(f)
	callerID := tid("execkeys-ratelimit-caller")
	token := f.agentToken(t, callerID, f.projectA.ID, ScopeAgentLifecycle)

	var last *httptest.ResponseRecorder
	successes := 0
	// PrincipalProjectBurst is 10; the 11th rapid call from the same
	// principal+project must be refused, regardless of how many the target
	// bucket (burst 20) would otherwise allow.
	for i := 0; i < agentkeys.PrincipalProjectBurst+1; i++ {
		last = doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agentInA.ID+"/keys", validKeysBody, token)
		if last.Code == http.StatusOK {
			successes++
		}
	}
	if last.Code != http.StatusTooManyRequests {
		t.Fatalf("expected the request beyond the principal+project burst to be rate limited, got %d: %s", last.Code, last.Body.String())
	}
	env := decodeKeysError(t, last.Body.Bytes())
	if env.Code != "keys_rate_limited" {
		t.Errorf("code = %q, want keys_rate_limited", env.Code)
	}
	// The frozen clock never advances, so the bucket's raw wait is 1/5s
	// (200ms); waitLocked's one-second floor is what the header must show,
	// not merely a non-empty value.
	if got := last.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After header = %q, want %q (the 1s floor, via ceil)", got, "1")
	}
	if successes != agentkeys.PrincipalProjectBurst {
		t.Errorf("got %d successes, want exactly the burst size %d", successes, agentkeys.PrincipalProjectBurst)
	}
	if got := d.callCount(); got != successes {
		t.Errorf("dispatcher called %d times, want %d (rate-limited calls must never reach dispatch)", got, successes)
	}

	// Budget isolation (AC3): exhausting the keys principal+project bucket
	// must not touch the DM aggregate allowance.
	if f.srv.chatSendLimiter == nil {
		t.Fatal("expected chatSendLimiter to still be configured independently of the keys limiters")
	}
	decision := f.srv.chatSendLimiter.Allow(callerID, chatSenderAgent)
	if !decision.Allowed {
		t.Error("exhausting the keys principal+project bucket must not consume the DM aggregate allowance")
	}
}

// TestExecuteAgentKeys_DMBudgetDoesNotBlockKeys pins the reverse direction
// of budget isolation: draining the DM aggregate allowance for a sender
// must not affect that same sender's keys budget -- keys must not be
// charged against, or blocked by, the DM allowance.
func TestExecuteAgentKeys_DMBudgetDoesNotBlockKeys(t *testing.T) {
	f, d, _, _ := newExecuteAgentKeysFixture(t)
	callerID := tid("execkeys-dm-isolation-caller")
	token := f.agentToken(t, callerID, f.projectA.ID, ScopeAgentLifecycle)

	for i := 0; i < chatSendAgentRatePerMinute+1; i++ {
		f.srv.chatSendLimiter.Allow(callerID, chatSenderAgent)
	}
	if decision := f.srv.chatSendLimiter.Allow(callerID, chatSenderAgent); decision.Allowed {
		t.Fatal("setup: expected the DM aggregate allowance to be exhausted for this sender")
	}

	rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agentInA.ID+"/keys", validKeysBody, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected keys to succeed despite the caller's DM allowance being exhausted, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := d.callCount(); got != 1 {
		t.Errorf("dispatcher called %d times, want 1", got)
	}
}

// TestExecuteAgentKeys_BucketsSharedAcrossRouteShapes pins AC3's "bridge and
// dedicated routes use the same keys limits" for the two dedicated route
// shapes themselves: the principal+project bucket is keyed by
// (identity, target project), never by which route resolved the request, so
// spending the whole burst via the top-level route must also refuse the
// very next call from the same caller against the same target via the
// project-scoped route.
func TestExecuteAgentKeys_BucketsSharedAcrossRouteShapes(t *testing.T) {
	f, _, _, _ := newExecuteAgentKeysFixture(t)
	freezeKeysRateLimiters(f)
	callerID := tid("execkeys-shared-bucket-caller")
	token := f.agentToken(t, callerID, f.projectA.ID, ScopeAgentLifecycle)

	for i := 0; i < agentkeys.PrincipalProjectBurst; i++ {
		rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agentInA.ID+"/keys", validKeysBody, token)
		if rec.Code != http.StatusOK {
			t.Fatalf("setup: expected top-level call %d to succeed, got %d: %s", i, rec.Code, rec.Body.String())
		}
	}

	rec := doRequestWithAgentToken(t, f.srv, http.MethodPost,
		"/api/v1/projects/"+f.projectA.ID+"/agents/"+f.agentInA.Slug+"/keys", validKeysBody, token)
	assertKeysDenialOutcome(t, "project-scoped shares the top-level route's principal bucket", rec, http.StatusTooManyRequests, "keys_rate_limited")
}

// TestExecuteAgentKeys_TargetRateLimit pins the per-target bucket
// independently of the per-principal bucket: many distinct callers hitting
// the same target exhaust the target's own budget (burst 20) even though no
// single caller comes close to its own principal+project limit.
func TestExecuteAgentKeys_TargetRateLimit(t *testing.T) {
	f, d, _, _ := newExecuteAgentKeysFixture(t)
	freezeKeysRateLimiters(f)

	var last *httptest.ResponseRecorder
	successes := 0
	for i := 0; i < agentkeys.TargetBurst+1; i++ {
		token := f.agentToken(t, tid(fmt.Sprintf("execkeys-target-rl-caller-%d", i)), f.projectA.ID, ScopeAgentLifecycle)
		last = doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agentInA.ID+"/keys", validKeysBody, token)
		if last.Code == http.StatusOK {
			successes++
		}
	}
	if last.Code != http.StatusTooManyRequests {
		t.Fatalf("expected the request beyond the target burst to be rate limited, got %d: %s", last.Code, last.Body.String())
	}
	// The frozen clock never advances, so the target bucket's raw wait is
	// 1/10s (100ms); waitLocked's one-second floor is what the header must
	// show -- a 429 must always carry Retry-After (contract §2.4a/§2.5),
	// including on this, the target-bucket branch of allowBoth (as opposed
	// to TestExecuteAgentKeys_RateLimiting, which only pins it for the
	// principal-bucket branch).
	if got := last.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After header = %q, want %q (the 1s floor, via ceil)", got, "1")
	}
	if successes != agentkeys.TargetBurst {
		t.Errorf("got %d successes, want exactly the target burst size %d", successes, agentkeys.TargetBurst)
	}
	if got := d.callCount(); got != successes {
		t.Errorf("dispatcher called %d times, want %d", got, successes)
	}
}

// TestExecuteAgentKeys_TargetSaturationDoesNotChargePrincipal pins that
// allowBoth must not consume a principal+project token when the target
// bucket refuses. Without this, one busy target would drain every caller's
// own principal+project budget for requests that could never have
// proceeded anyway.
func TestExecuteAgentKeys_TargetSaturationDoesNotChargePrincipal(t *testing.T) {
	f, _, _, _ := newExecuteAgentKeysFixture(t)
	freezeKeysRateLimiters(f)

	// Saturate the target bucket (burst 20) with other callers first.
	for i := 0; i < agentkeys.TargetBurst; i++ {
		otherToken := f.agentToken(t, tid(fmt.Sprintf("execkeys-saturate-%d", i)), f.projectA.ID, ScopeAgentLifecycle)
		rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agentInA.ID+"/keys", validKeysBody, otherToken)
		if rec.Code != http.StatusOK {
			t.Fatalf("setup: expected saturating call %d to succeed, got %d: %s", i, rec.Code, rec.Body.String())
		}
	}

	xAgentID := tid("execkeys-saturate-caller-x")
	xToken := f.agentToken(t, xAgentID, f.projectA.ID, ScopeAgentLifecycle)
	rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agentInA.ID+"/keys", validKeysBody, xToken)
	assertKeysDenialOutcome(t, "X refused by saturated target", rec, http.StatusTooManyRequests, "keys_rate_limited")

	// X's principal+project bucket must still hold its full burst: it was
	// never charged for the request the target bucket alone refused.
	principalKey := agentKeysTestPrincipalKey(xAgentID, f.projectA.ID)
	for i := 0; i < agentkeys.PrincipalProjectBurst; i++ {
		if allowed, _ := f.srv.keysPrincipalLimiter.Allow(principalKey); !allowed {
			t.Fatalf("X's principal+project bucket should still hold its full burst after being refused by a saturated target; failed at call %d", i)
		}
	}
}

// TestExecuteAgentKeys_PrincipalBucketKeyIncludesProject pins the project
// dimension of the principal+project bucket (contract: the limit is per
// principal *and* project), at the handler level, with a single human
// caller who has access to two different projects -- the case the key
// format exists to handle, since a human has no single fixed "home"
// project. Uses f.owner, who owns both fixture agents (one per project).
func TestExecuteAgentKeys_PrincipalBucketKeyIncludesProject(t *testing.T) {
	f, d, _, _ := newExecuteAgentKeysFixture(t)
	freezeKeysRateLimiters(f)

	for i := 0; i < agentkeys.PrincipalProjectBurst; i++ {
		rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, "/api/v1/agents/"+f.agentInA.ID+"/keys", validKeysBody)
		if rec.Code != http.StatusOK {
			t.Fatalf("setup: call %d against project A's agent should succeed, got %d: %s", i, rec.Code, rec.Body.String())
		}
	}

	// Project A's bucket is now exhausted for this caller.
	recA := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, "/api/v1/agents/"+f.agentInA.ID+"/keys", validKeysBody)
	assertKeysDenialOutcome(t, "project A after exhausting its own bucket", recA, http.StatusTooManyRequests, "keys_rate_limited")

	// The *same* caller against a *different* project's agent (B) is still
	// admitted: if the bucket key did not include the project, this call
	// would also be refused by the already-exhausted bucket above.
	recB := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, "/api/v1/agents/"+f.agentInB.ID+"/keys", validKeysBody)
	if recB.Code != http.StatusOK {
		t.Fatalf("expected the same caller's call against a different project's agent to still be admitted, got %d: %s", recB.Code, recB.Body.String())
	}

	// Project A is still exhausted: the admitted call in B did not refill
	// or share A's bucket.
	recA2 := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, "/api/v1/agents/"+f.agentInA.ID+"/keys", validKeysBody)
	assertKeysDenialOutcome(t, "project A still exhausted after B's unrelated admission", recA2, http.StatusTooManyRequests, "keys_rate_limited")

	if got, want := d.callCount(), agentkeys.PrincipalProjectBurst+1; got != want {
		t.Errorf("dispatcher called %d times, want %d (the burst admitted in A, plus the one admitted call in B)", got, want)
	}
}

// TestExecuteAgentKeys_RefusedRequestsDoNotChargeBudgets pins that a request
// refused before admission -- keys_denied or cross_project_keys_unsupported,
// decided by authorizeAgentKeys/authorizeAgentKeysCrossProject, both before
// admitAndDispatchAgentKeys's one allowBoth call -- never consumes either
// keys budget. Otherwise refused requests would reduce the budget available
// to admitted ones. The existing saturation tests cover the opposite
// direction (a target refusal does not charge the principal bucket); this
// test covers denial outcomes that are decided even earlier, before either
// bucket is ever examined.
func TestExecuteAgentKeys_RefusedRequestsDoNotChargeBudgets(t *testing.T) {
	t.Run("keys_denied", func(t *testing.T) {
		for _, shape := range keysRouteShapes {
			t.Run(shape.name, func(t *testing.T) {
				f, _, _, _ := newExecuteAgentKeysFixture(t)
				freezeKeysRateLimiters(f)
				deniedCallerID := tid("execkeys-refused-nocharge-denied-" + shape.name)
				deniedToken := f.agentToken(t, deniedCallerID, f.projectA.ID) // no lifecycle scope -> keys_denied

				for i := 0; i < agentkeys.TargetBurst+1; i++ {
					rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, shape.path(f.agentInA), validKeysBody, deniedToken)
					assertKeysDenialOutcome(t, fmt.Sprintf("%s: denied call %d", shape.name, i), rec, http.StatusForbidden, "keys_denied")
				}

				// An authorized caller must still succeed: none of the refused
				// calls above may have consumed a token from the target's
				// per-target bucket.
				okToken := f.agentToken(t, tid("execkeys-refused-nocharge-ok-"+shape.name), f.projectA.ID, ScopeAgentLifecycle)
				rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, shape.path(f.agentInA), validKeysBody, okToken)
				if rec.Code != http.StatusOK {
					t.Fatalf("%s: expected an authorized caller to still succeed after %d refused calls, got %d: %s", shape.name, agentkeys.TargetBurst+1, rec.Code, rec.Body.String())
				}

				// The target bucket must hold exactly TargetBurst-1 tokens now:
				// the TargetBurst+1 refusals above charged nothing at all, and
				// only the one authorized dispatch just above actually consumed
				// a single token.
				for i := 0; i < agentkeys.TargetBurst-1; i++ {
					if allowed, _ := f.srv.keysTargetLimiter.Allow(f.agentInA.ID); !allowed {
						t.Fatalf("%s: target bucket should still hold %d tokens after only one real dispatch; failed at probe %d", shape.name, agentkeys.TargetBurst-1, i)
					}
				}
				if allowed, _ := f.srv.keysTargetLimiter.Allow(f.agentInA.ID); allowed {
					t.Errorf("%s: target bucket should be exactly exhausted after draining TargetBurst-1 remaining tokens", shape.name)
				}

				// The refused caller's own principal+project bucket must also
				// still hold its full burst: none of its refused calls ever
				// reached admitAndDispatchAgentKeys's allowBoth call.
				principalKey := agentKeysTestPrincipalKey(deniedCallerID, f.projectA.ID)
				for i := 0; i < agentkeys.PrincipalProjectBurst; i++ {
					if allowed, _ := f.srv.keysPrincipalLimiter.Allow(principalKey); !allowed {
						t.Fatalf("%s: denied caller's principal+project bucket should still hold its full burst; failed at probe %d", shape.name, i)
					}
				}
			})
		}
	})

	t.Run("cross_project_keys_unsupported", func(t *testing.T) {
		for _, shape := range keysRouteShapes {
			t.Run(shape.name, func(t *testing.T) {
				f, _, _, _ := newExecuteAgentKeysFixture(t)
				freezeKeysRateLimiters(f)
				callerID := tid("execkeys-refused-nocharge-crossproj-" + shape.name)
				token := f.agentToken(t, callerID, f.projectA.ID, ScopeAgentLifecycle)

				// On the top-level shape this denial happens only after GetAgent
				// has already resolved agentInB, so the target is known at the
				// point of refusal and could be charged by mistake; on the
				// project-scoped shape it is refused before any lookup at all.
				path := shape.path(f.agentInB)

				for i := 0; i < agentkeys.TargetBurst+1; i++ {
					rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, path, validKeysBody, token)
					assertKeysDenialOutcome(t, fmt.Sprintf("%s: cross-project call %d", shape.name, i), rec, http.StatusUnprocessableEntity, "cross_project_keys_unsupported")
				}

				// agentInB's target bucket must still hold its full, untouched
				// burst: a cross-project refusal must never charge it, even
				// when (as on the top-level shape) the target was already
				// resolved and known at the point of refusal.
				for i := 0; i < agentkeys.TargetBurst; i++ {
					if allowed, _ := f.srv.keysTargetLimiter.Allow(f.agentInB.ID); !allowed {
						t.Fatalf("%s: agentInB's target bucket should still hold its full burst after being refused as cross-project; failed at probe %d", shape.name, i)
					}
				}

				// The caller's own principal+project bucket (keyed by the
				// target's project, B, per agentKeysPrincipalBucketKey's
				// doc comment) must also still hold its full, untouched
				// burst: this half of the test previously checked only the
				// target bucket, so a mutation charging only the principal
				// bucket on a cross-project refusal would have passed here
				// on both shapes.
				principalKey := agentKeysTestPrincipalKey(callerID, f.projectB.ID)
				for i := 0; i < agentkeys.PrincipalProjectBurst; i++ {
					if allowed, _ := f.srv.keysPrincipalLimiter.Allow(principalKey); !allowed {
						t.Fatalf("%s: caller's principal+project bucket should still hold its full burst after being refused as cross-project; failed at probe %d", shape.name, i)
					}
				}
			})
		}
	})

	// ptone/scion#2460: a same-project agent caller denied by the new attach-
	// relationship check (authorizeAgentTargetAction) is refused before
	// admission exactly like keys_denied for any other reason, so it must
	// not charge either budget either.
	t.Run("attach_relationship_denied", func(t *testing.T) {
		for _, shape := range keysRouteShapes {
			t.Run(shape.name, func(t *testing.T) {
				f, _, _, _ := newExecuteAgentKeysFixture(t)
				freezeKeysRateLimiters(f)
				markEdgeBackfillComplete(t, f.store)
				deniedCallerID := tid("execkeys-refused-nocharge-attach-" + shape.name)
				deniedToken := f.agentToken(t, deniedCallerID, f.projectA.ID, ScopeAgentLifecycle) // lifecycle scope, same project, but no delegation edge

				for i := 0; i < agentkeys.TargetBurst+1; i++ {
					rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, shape.path(f.agentInA), validKeysBody, deniedToken)
					assertKeysDenialOutcome(t, fmt.Sprintf("%s: attach-denied call %d", shape.name, i), rec, http.StatusForbidden, "keys_denied")
				}

				okToken := f.agentToken(t, tid("execkeys-refused-nocharge-attach-ok-"+shape.name), f.projectA.ID, ScopeAgentLifecycle)
				addProjectEdge(t, f.store, store.DelegationPrincipalUser, f.owner.ID, tid("execkeys-refused-nocharge-attach-ok-"+shape.name), f.projectA.ID)
				rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, shape.path(f.agentInA), validKeysBody, okToken)
				if rec.Code != http.StatusOK {
					t.Fatalf("%s: expected an authorized caller to still succeed after %d attach-denied calls, got %d: %s", shape.name, agentkeys.TargetBurst+1, rec.Code, rec.Body.String())
				}

				for i := 0; i < agentkeys.TargetBurst-1; i++ {
					if allowed, _ := f.srv.keysTargetLimiter.Allow(f.agentInA.ID); !allowed {
						t.Fatalf("%s: target bucket should still hold %d tokens after only one real dispatch; failed at probe %d", shape.name, agentkeys.TargetBurst-1, i)
					}
				}
				if allowed, _ := f.srv.keysTargetLimiter.Allow(f.agentInA.ID); allowed {
					t.Errorf("%s: target bucket should be exactly exhausted after draining TargetBurst-1 remaining tokens", shape.name)
				}

				principalKey := agentKeysTestPrincipalKey(deniedCallerID, f.projectA.ID)
				for i := 0; i < agentkeys.PrincipalProjectBurst; i++ {
					if allowed, _ := f.srv.keysPrincipalLimiter.Allow(principalKey); !allowed {
						t.Fatalf("%s: attach-denied caller's principal+project bucket should still hold its full burst; failed at probe %d", shape.name, i)
					}
				}
			})
		}
	})
}

// TestExecuteAgentKeysAllowBoth_SameLimiterInstance pins allowBoth's a == b
// handling: it must return promptly (never deadlock, since reserve/reserve
// on the same *sync.Mutex would) and must still uphold "neither bucket is
// charged unless both allow" even when both arguments are literally the
// same limiter -- production never does this (the principal+project and
// target limiters are always distinct instances), but the a == b branch is
// real code with a real contract, not just a deadlock guard.
func TestExecuteAgentKeysAllowBoth_SameLimiterInstance(t *testing.T) {
	t.Run("distinct keys, fresh burst -> allowed, does not deadlock", func(t *testing.T) {
		l := newKeysRateLimiter(5, 2)
		done := make(chan struct{})
		var allowed bool
		go func() {
			allowed, _ = allowBoth(l, "k1", l, "k2")
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("allowBoth(l, \"k1\", l, \"k2\") deadlocked when a == b")
		}
		if !allowed {
			t.Error("expected allowBoth to allow: both keys have a fresh burst")
		}
	})

	t.Run("same key twice needs two tokens, both-or-neither still holds", func(t *testing.T) {
		l := newKeysRateLimiter(5, 1) // burst 1: one token, but this call asks for two reservations against it.
		allowed, _ := allowBoth(l, "k", l, "k")
		if allowed {
			t.Fatal("expected refusal: only 1 token available but two reservations were requested against the same key")
		}
		// Neither reservation attempt may have consumed the one token that
		// was never actually available to spend twice.
		if ok, _ := l.Allow("k"); !ok {
			t.Error("expected the untouched token to still be available after the refused allowBoth")
		}
	})
}

// TestExecuteAgentKeys_ExecuteBeforeHonorsShorterRequestDeadline pins the
// other half of the execute-before deadline (contract §4.2, issue #2196
// AC1): min(request deadline, now+DefaultAdmissionWindow). Every other test
// sends a request whose context carries no deadline, so only the
// "no deadline -> now+30s" branch ever runs; this test is the only one that
// drives a request whose context already has its own, shorter deadline, and
// pins that admitAndDispatchAgentKeys actually uses it rather than silently
// defaulting to the 30s window regardless.
func TestExecuteAgentKeys_ExecuteBeforeHonorsShorterRequestDeadline(t *testing.T) {
	f, d, _, _ := newExecuteAgentKeysFixture(t)
	token := f.agentToken(t, tid("execkeys-deadline-caller"), f.projectA.ID, ScopeAgentLifecycle)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wantDeadline, _ := ctx.Deadline()

	body, err := json.Marshal(validKeysBody)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+f.agentInA.ID+"/keys", bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Scion-Agent-Token", token)
	rec := httptest.NewRecorder()

	f.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	// agentkeys.CapExecuteBefore returns the request deadline unchanged
	// whenever it is earlier than now+DefaultAdmissionWindow, which a 5s
	// deadline always is -- so the dispatched executeBefore must be exactly
	// the request's own deadline, not ~30s away from the request start.
	if got := d.lastReq.executeBefore; !got.Equal(wantDeadline) {
		t.Errorf("executeBefore = %v, want exactly the request's own deadline %v (the shorter of the two, per min(request deadline, now+30s))", got, wantDeadline)
	}
}

// TestExecuteAgentKeys_SuccessEchoesMintedOperationID pins that the success
// response must carry the operation ID this handler minted, never whatever
// a Dispatcher implementation happens to return -- even a buggy or future
// one that returns a different or empty ID.
func TestExecuteAgentKeys_SuccessEchoesMintedOperationID(t *testing.T) {
	t.Run("dispatcher echoes correctly", func(t *testing.T) {
		f, d, _, _ := newExecuteAgentKeysFixture(t)
		token := f.agentToken(t, tid("execkeys-echo-ok-caller"), f.projectA.ID, ScopeAgentLifecycle)
		rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agentInA.ID+"/keys", validKeysBody, token)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		var resp agentkeys.Response
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		if resp.OperationID != d.lastReq.operationID {
			t.Errorf("response operation_id %q != minted operation_id %q", resp.OperationID, d.lastReq.operationID)
		}
	})

	t.Run("dispatcher returns an empty ID -> success with the minted ID", func(t *testing.T) {
		// The chosen behavior: an empty result.OperationID is not treated as
		// a mismatch (it asserts nothing, rather than asserting something
		// wrong) -- the response still carries the handler's own minted ID,
		// and the call counts as an ordinary single-dispatch success.
		f, _, _, _ := newExecuteAgentKeysFixture(t)
		d := newFakeAgentKeysDispatcher()
		d.skipResultDefaults = true
		d.result = agentkeys.BrokerResult{OperationID: "", Outcome: agentkeys.OutcomeDispatched}
		f.srv.SetDispatcher(d)
		token := f.agentToken(t, tid("execkeys-echo-empty-caller"), f.projectA.ID, ScopeAgentLifecycle)

		rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agentInA.ID+"/keys", validKeysBody, token)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		var resp agentkeys.Response
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		if resp.OperationID != d.lastReq.operationID || resp.OperationID == "" {
			t.Errorf("response operation_id %q != minted operation_id %q (or empty)", resp.OperationID, d.lastReq.operationID)
		}
		if got := d.callCount(); got != 1 {
			t.Errorf("dispatcher called %d times, want exactly 1", got)
		}
	})

	t.Run("dispatcher returns a mismatched ID -> keys_outcome_unknown, not a false success", func(t *testing.T) {
		f, _, _, _ := newExecuteAgentKeysFixture(t)
		d := newFakeAgentKeysDispatcher()
		d.result = agentkeys.BrokerResult{OperationID: "some-other-id", Outcome: agentkeys.OutcomeDispatched}
		f.srv.SetDispatcher(d)
		token := f.agentToken(t, tid("execkeys-echo-mismatch-caller"), f.projectA.ID, ScopeAgentLifecycle)
		log := installSentinelLogCapture(t)

		rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agentInA.ID+"/keys", validKeysBody, token)
		assertKeysDenialOutcome(t, "mismatched operation id", rec, http.StatusBadGateway, "keys_outcome_unknown")

		if got := d.callCount(); got != 1 {
			t.Errorf("dispatcher called %d times, want exactly 1 (no replay)", got)
		}
		outcome := lastOutcomeAuditRecord(t, log)
		if outcome["decision"] != "keys_outcome_unknown" {
			t.Errorf("audit decision = %q, want keys_outcome_unknown", outcome["decision"])
		}
	})

	t.Run("dispatcher returns a nil error with a non-dispatched Outcome -> keys_outcome_unknown, not a false success", func(t *testing.T) {
		// pkg/agentkeys's BrokerResult doc: "Success is HTTP 200 with
		// Outcome == OutcomeDispatched; there is no other success shape."
		// Every current real transport enforces this before ever returning
		// a nil error (decodeBrokerKeysResponse), but this handler must not
		// rely on every current and future Dispatcher implementation
		// getting that right -- the same defense-in-depth reasoning as the
		// operation-ID mismatch case above.
		f, _, _, _ := newExecuteAgentKeysFixture(t)
		d := newFakeAgentKeysDispatcher()
		d.skipResultDefaults = true
		d.result = agentkeys.BrokerResult{OperationID: "", Outcome: ""}
		f.srv.SetDispatcher(d)
		token := f.agentToken(t, tid("execkeys-echo-badoutcome-caller"), f.projectA.ID, ScopeAgentLifecycle)
		log := installSentinelLogCapture(t)

		rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agentInA.ID+"/keys", validKeysBody, token)
		assertKeysDenialOutcome(t, "empty outcome", rec, http.StatusBadGateway, "keys_outcome_unknown")

		if got := d.callCount(); got != 1 {
			t.Errorf("dispatcher called %d times, want exactly 1 (no replay)", got)
		}
		outcome := lastOutcomeAuditRecord(t, log)
		if outcome["decision"] != "keys_outcome_unknown" {
			t.Errorf("audit decision = %q, want keys_outcome_unknown", outcome["decision"])
		}
	})
}

// erroringReader is an io.Reader that always fails with a fixed, non-size
// related error -- used to simulate a client disconnect or truncated body,
// as distinct from http.MaxBytesReader's own *http.MaxBytesError.
type erroringReader struct{ err error }

func (r *erroringReader) Read(_ []byte) (int, error) { return 0, r.err }

// TestExecuteAgentKeys_BodyReadErrorClassification pins that only a body
// that actually exceeded MaxHTTPBodyBytes is payload_too_large; any other
// read failure (client disconnect, truncated body) must be reported as 400
// invalid_request instead, never misclassified as oversized.
func TestExecuteAgentKeys_BodyReadErrorClassification(t *testing.T) {
	f := newAgentKeysRouteFixture(t)
	token := f.agentToken(t, tid("execkeys-bodyerr-caller"), f.projectA.ID, ScopeAgentLifecycle)

	t.Run("oversized body -> 413 payload_too_large", func(t *testing.T) {
		oversized := `{"keys":"` + strings.Repeat("a", agentkeys.MaxHTTPBodyBytes) + `"}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+f.agentInA.ID+"/keys", strings.NewReader(oversized))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Scion-Agent-Token", token)
		rec := httptest.NewRecorder()
		f.srv.Handler().ServeHTTP(rec, req)
		assertKeysValidationFailure(t, rec, http.StatusRequestEntityTooLarge, "payload_too_large")
	})

	t.Run("non-size read failure -> 400 invalid_request, not 413", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+f.agentInA.ID+"/keys", nil)
		req.Body = io.NopCloser(&erroringReader{err: errors.New("simulated read failure")})
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Scion-Agent-Token", token)
		rec := httptest.NewRecorder()
		f.srv.Handler().ServeHTTP(rec, req)
		assertKeysValidationFailure(t, rec, http.StatusBadRequest, "invalid_request")
	})
}

// assertKeysValidationFailure asserts rec is a pre-operation-ID validation
// failure (400/413): the expected status/code, and no operation_id key in
// details at all.
func assertKeysValidationFailure(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("status = %d, want %d: %s", rec.Code, wantStatus, rec.Body.String())
	}
	env := decodeKeysError(t, rec.Body.Bytes())
	if env.Code != wantCode {
		t.Errorf("code = %q, want %q", env.Code, wantCode)
	}
	if _, present := env.Details["operation_id"]; present {
		t.Errorf("a validation failure must not carry an operation_id: %v", env.Details)
	}
}

// TestExecuteAgentKeys_AdmissionAndOutcomeAuditEventsAreDistinguishable pins
// that the pre-dispatch admission record and the terminal outcome record
// must be told apart by an explicit "event" field, not by relying on an
// empty "decision" string as a coincidental signal.
func TestExecuteAgentKeys_AdmissionAndOutcomeAuditEventsAreDistinguishable(t *testing.T) {
	f, _, _, _ := newExecuteAgentKeysFixture(t)
	token := f.agentToken(t, tid("execkeys-eventfield-caller"), f.projectA.ID, ScopeAgentLifecycle)
	log := installSentinelLogCapture(t)

	rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agentInA.ID+"/keys", validKeysBody, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	records := findAuditRecords(log)
	var admission, outcome map[string]string
	for _, r := range records {
		switch r["event"] {
		case "admission":
			admission = r
		case "outcome":
			outcome = r
		}
	}
	if admission == nil {
		t.Fatalf("expected an admission-event audit record; got records: %v", records)
	}
	if outcome == nil {
		t.Fatalf("expected an outcome-event audit record; got records: %v", records)
	}
	if admission["decision"] != "" {
		t.Errorf("admission record decision = %q, want empty (no decision exists yet)", admission["decision"])
	}
	if outcome["decision"] != "dispatched" {
		t.Errorf("outcome record decision = %q, want dispatched", outcome["decision"])
	}
}

// TestExecuteAgentKeys_AuditIsContentFree drives requests whose "keys"
// value is a distinctive sentinel and asserts, for every outcome kind (a
// successful dispatch, a denial, a validation failure, and an unclassified
// dispatch error), that: (a) the sentinel never appears in the captured
// log, (b) the sentinel never appears in the HTTP response body, and (c)
// the captured "agent keys audit" outcome record actually exists and
// carries the expected decision/operation_id -- a positive control proving
// the capture itself is live, not merely silent. Reuses
// keysContentSentinel/installSentinelLogCapture from
// keys_no_content_leak_test.go (task 1.2).
func TestExecuteAgentKeys_AuditIsContentFree(t *testing.T) {
	t.Run("successful dispatch", func(t *testing.T) {
		f, _, _, _ := newExecuteAgentKeysFixture(t)
		callerID := tid("execkeys-audit-success-caller")
		token := f.agentToken(t, callerID, f.projectA.ID, ScopeAgentLifecycle)
		claims, err := f.srv.GetAgentTokenService().ValidateAgentToken(token)
		require.NoError(t, err)
		log := installSentinelLogCapture(t)

		rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agentInA.ID+"/keys",
			map[string]string{"keys": keysContentSentinel}, token)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		var resp agentkeys.Response
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

		outcome := lastOutcomeAuditRecord(t, log)
		if outcome["decision"] != "dispatched" {
			t.Errorf("decision = %q, want dispatched", outcome["decision"])
		}
		if outcome["operation_id"] != resp.OperationID {
			t.Errorf("audit operation_id %q != response operation_id %q", outcome["operation_id"], resp.OperationID)
		}
		if outcome["input_bytes"] != strconv.Itoa(len(keysContentSentinel)) {
			t.Errorf("input_bytes = %q, want %d", outcome["input_bytes"], len(keysContentSentinel))
		}
		// Contract §5's audit field list requires the actor, target and
		// credential to actually be recorded, not just a decision/operation
		// ID: an audit record that named nobody would still pass every
		// check above. Assert both the admission record (written just
		// before dispatch) and the outcome record (the terminal one) carry
		// the real values, not placeholders.
		admission := firstAdmissionAuditRecord(t, log)
		wantIdentity := auditIdentityExpectation{
			actorType:       "agent",
			actorID:         callerID,
			sourceProjectID: f.projectA.ID,
			targetAgentID:   f.agentInA.ID,
			targetProjectID: f.projectA.ID,
			credentialKind:  "agent_jwt",
			credentialID:    claims.ID,
		}
		assertAuditIdentityFields(t, "admission", admission, wantIdentity)
		assertAuditIdentityFields(t, "outcome", outcome, wantIdentity)
		assertNoSentinelInLogOrBody(t, log, rec)
	})

	t.Run("denied", func(t *testing.T) {
		f, _, _, _ := newExecuteAgentKeysFixture(t)
		callerID := tid("execkeys-audit-denied-caller")
		token := f.agentToken(t, callerID, f.projectA.ID) // no lifecycle scope
		claims, err := f.srv.GetAgentTokenService().ValidateAgentToken(token)
		require.NoError(t, err)
		log := installSentinelLogCapture(t)

		rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agentInA.ID+"/keys",
			map[string]string{"keys": keysContentSentinel}, token)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
		}
		env := decodeKeysError(t, rec.Body.Bytes())
		opID, _ := env.Details["operation_id"].(string)
		if opID == "" {
			t.Fatal("expected a non-empty operation_id on a denial")
		}

		outcome := lastOutcomeAuditRecord(t, log)
		if outcome["decision"] != "keys_denied" {
			t.Errorf("decision = %q, want keys_denied", outcome["decision"])
		}
		if outcome["operation_id"] != opID {
			t.Errorf("audit operation_id %q != response operation_id %q", outcome["operation_id"], opID)
		}
		if outcome["input_bytes"] != strconv.Itoa(len(keysContentSentinel)) {
			t.Errorf("input_bytes = %q, want %d", outcome["input_bytes"], len(keysContentSentinel))
		}
		assertAuditIdentityFields(t, "outcome", outcome, auditIdentityExpectation{
			actorType:       "agent",
			actorID:         callerID,
			sourceProjectID: f.projectA.ID,
			targetAgentID:   f.agentInA.ID,
			targetProjectID: f.projectA.ID,
			credentialKind:  "agent_jwt",
			credentialID:    claims.ID,
		})
		assertNoSentinelInLogOrBody(t, log, rec)
	})

	t.Run("project-scoped cross-project denial records the project but no agent", func(t *testing.T) {
		// The pre-lookup cross-project refusal (authorizeAgentKeysCrossProject,
		// contract §3.1 invariant 4 / AK-21c) runs before any agent-target
		// lookup, so its audit record can only ever know the URL {project},
		// never a resolved agent ID -- this pins that it records exactly
		// that (a real target_project_id, an empty target_agent_id), not a
		// zero-value record that happens to look content-free by accident.
		f, _, _, _ := newExecuteAgentKeysFixture(t)
		callerID := tid("execkeys-audit-crossproj-caller")
		token := f.agentToken(t, callerID, f.projectA.ID, ScopeAgentLifecycle)
		claims, err := f.srv.GetAgentTokenService().ValidateAgentToken(token)
		require.NoError(t, err)
		log := installSentinelLogCapture(t)

		rec := doRequestWithAgentToken(t, f.srv, http.MethodPost,
			"/api/v1/projects/"+f.projectB.ID+"/agents/"+f.agentInB.Slug+"/keys",
			map[string]string{"keys": keysContentSentinel}, token)
		assertKeysDenialOutcome(t, "project-scoped cross-project", rec, http.StatusUnprocessableEntity, "cross_project_keys_unsupported")

		outcome := lastOutcomeAuditRecord(t, log)
		assertAuditIdentityFields(t, "outcome", outcome, auditIdentityExpectation{
			actorType:       "agent",
			actorID:         callerID,
			sourceProjectID: f.projectA.ID,
			targetAgentID:   "",
			targetProjectID: f.projectB.ID,
			credentialKind:  "agent_jwt",
			credentialID:    claims.ID,
		})
		assertNoSentinelInLogOrBody(t, log, rec)
	})

	t.Run("validation failure", func(t *testing.T) {
		f, _, _, _ := newExecuteAgentKeysFixture(t)
		callerID := tid("execkeys-audit-invalid-caller")
		token := f.agentToken(t, callerID, f.projectA.ID, ScopeAgentLifecycle)
		claims, err := f.srv.GetAgentTokenService().ValidateAgentToken(token)
		require.NoError(t, err)
		log := installSentinelLogCapture(t)

		// An unknown field alongside a sentinel-bearing keys value: rejected
		// by ValidateBody (400) before any operation ID exists, but the
		// content-free audit call in beginAgentKeysRequest still must not
		// leak it.
		body := map[string]string{"keys": keysContentSentinel, "unexpected": "field"}
		bodyBytes, err := json.Marshal(body)
		require.NoError(t, err)

		rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agentInA.ID+"/keys", body, token)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
		}

		outcome := lastOutcomeAuditRecord(t, log)
		if outcome["decision"] != "invalid_request" {
			t.Errorf("decision = %q, want invalid_request", outcome["decision"])
		}
		if outcome["operation_id"] != "" {
			t.Errorf("audit operation_id = %q, want empty (no operation exists yet)", outcome["operation_id"])
		}
		// logAgentKeysValidationAudit reports the raw pre-decode body length
		// as "body_bytes", a distinct field from the post-validation
		// "input_bytes" (the decoded "keys" value's length) every other
		// audit record uses -- this record must use the former, not the
		// latter.
		if outcome["body_bytes"] != strconv.Itoa(len(bodyBytes)) {
			t.Errorf("body_bytes = %q, want %d", outcome["body_bytes"], len(bodyBytes))
		}
		if _, present := outcome["input_bytes"]; present {
			t.Errorf("a validation-failure audit record must not carry input_bytes, got %v", outcome)
		}
		// logAgentKeysValidationAudit shares agentKeysAuditActor with
		// logAgentKeysAudit, but nothing had asserted it on this record:
		// the actor/credential are already known at this point (validation
		// runs after authentication), and no target exists yet (resolution
		// has not run).
		assertAuditIdentityFields(t, "outcome", outcome, auditIdentityExpectation{
			actorType:       "agent",
			actorID:         callerID,
			sourceProjectID: f.projectA.ID,
			targetAgentID:   "",
			targetProjectID: "",
			credentialKind:  "agent_jwt",
			credentialID:    claims.ID,
		})
		assertNoSentinelInLogOrBody(t, log, rec)
	})

	t.Run("keys_outcome_unknown never leaks dispatcher error text", func(t *testing.T) {
		f, d, _, _ := newExecuteAgentKeysFixture(t)
		d.err = fmt.Errorf("simulated ambiguous transport failure containing %s", keysContentSentinel)
		token := f.agentToken(t, tid("execkeys-audit-unknown-caller"), f.projectA.ID, ScopeAgentLifecycle)
		log := installSentinelLogCapture(t)

		rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agentInA.ID+"/keys",
			map[string]string{"keys": keysContentSentinel}, token)
		assertKeysDenialOutcome(t, "unknown", rec, http.StatusBadGateway, "keys_outcome_unknown")

		outcome := lastOutcomeAuditRecord(t, log)
		if outcome["decision"] != "keys_outcome_unknown" {
			t.Errorf("decision = %q, want keys_outcome_unknown", outcome["decision"])
		}
		assertNoSentinelInLogOrBody(t, log, rec)
	})
}

func assertNoSentinelInLogOrBody(t *testing.T, log *bytes.Buffer, rec *httptest.ResponseRecorder) {
	t.Helper()
	if strings.Contains(log.String(), keysContentSentinel) {
		t.Errorf("captured log leaked key content:\n%s", log.String())
	}
	if strings.Contains(rec.Body.String(), keysContentSentinel) {
		t.Errorf("HTTP response body leaked key content: %s", rec.Body.String())
	}
}

// logfmtFieldRe extracts key=value pairs from one line of
// slog.NewTextHandler output (the format installSentinelLogCapture
// installs): value is either a double-quoted, possibly-escaped string, or a
// bare run of non-space characters (including the empty string, for a field
// slog rendered as "key=" with no quoting).
var logfmtFieldRe = regexp.MustCompile(`(\S+?)=("(?:[^"\\]|\\.)*"|\S*)`)

func parseLogfmtFields(line string) map[string]string {
	fields := map[string]string{}
	for _, m := range logfmtFieldRe.FindAllStringSubmatch(line, -1) {
		key, val := m[1], m[2]
		if strings.HasPrefix(val, `"`) {
			if unquoted, err := strconv.Unquote(val); err == nil {
				val = unquoted
			}
		}
		fields[key] = val
	}
	return fields
}

// findAuditRecords returns the parsed fields of every "agent keys audit"
// line in the captured log, in order.
func findAuditRecords(log *bytes.Buffer) []map[string]string {
	var records []map[string]string
	for _, line := range strings.Split(log.String(), "\n") {
		if strings.Contains(line, `msg="agent keys audit"`) {
			records = append(records, parseLogfmtFields(line))
		}
	}
	return records
}

// lastOutcomeAuditRecord returns the last "agent keys audit" record whose
// event field is "outcome" -- the terminal decision for the request, as
// opposed to the one non-terminal "admission" record a successful dispatch
// also logs. Fails the test if none is found, since a missing record is a
// broken capture, not a passing "no leak" result.
func lastOutcomeAuditRecord(t *testing.T, log *bytes.Buffer) map[string]string {
	t.Helper()
	records := findAuditRecords(log)
	for i := len(records) - 1; i >= 0; i-- {
		if records[i]["event"] == "outcome" {
			return records[i]
		}
	}
	t.Fatalf("no 'outcome' agent-keys audit record found in captured log (capture may not be live):\n%s", log.String())
	return nil
}

// firstAdmissionAuditRecord returns the first "agent keys audit" record
// whose event field is "admission" -- the one non-terminal record a
// successful dispatch logs just before calling the dispatcher, as opposed to
// the terminal "outcome" record lastOutcomeAuditRecord returns. Fails the
// test if none is found.
func firstAdmissionAuditRecord(t *testing.T, log *bytes.Buffer) map[string]string {
	t.Helper()
	for _, r := range findAuditRecords(log) {
		if r["event"] == "admission" {
			return r
		}
	}
	t.Fatalf("no 'admission' agent-keys audit record found in captured log (capture may not be live):\n%s", log.String())
	return nil
}

// auditIdentityExpectation is the set of actor/target/credential fields
// assertAuditIdentityFields checks on one audit record. An empty
// targetAgentID is a legitimate expectation (the pre-lookup cross-project
// denial never resolves one), so callers must set every field explicitly
// rather than relying on a zero value meaning "don't care".
type auditIdentityExpectation struct {
	actorType       string
	actorID         string
	sourceProjectID string
	targetAgentID   string
	targetProjectID string
	credentialKind  string
	credentialID    string
}

// assertAuditIdentityFields asserts that record carries exactly the actor,
// target and credential values contract §5's audit field list requires --
// not merely that some non-empty record exists. label identifies which
// audit record (e.g. "admission" or "outcome") is being checked, for
// failure messages.
func assertAuditIdentityFields(t *testing.T, label string, record map[string]string, want auditIdentityExpectation) {
	t.Helper()
	if record == nil {
		t.Fatalf("%s: no audit record to check", label)
	}
	checks := []struct {
		field, got, want string
	}{
		{"actor_type", record["actor_type"], want.actorType},
		{"actor_id", record["actor_id"], want.actorID},
		{"source_project_id", record["source_project_id"], want.sourceProjectID},
		{"target_agent_id", record["target_agent_id"], want.targetAgentID},
		{"target_project_id", record["target_project_id"], want.targetProjectID},
		{"credential_kind", record["credential_kind"], want.credentialKind},
		{"credential_id", record["credential_id"], want.credentialID},
		{"route", record["route"], "keys"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s record: %s = %q, want %q (full record: %v)", label, c.field, c.got, c.want, record)
		}
	}
}

// TestExecuteAgentKeys_OperationIDUniquePerRequest pins that a fresh
// operation ID is minted per request, never reused as an idempotency key
// (contract §2.4: "operation_id... is not an idempotency key").
func TestExecuteAgentKeys_OperationIDUniquePerRequest(t *testing.T) {
	f, _, _, _ := newExecuteAgentKeysFixture(t)
	token := f.agentToken(t, tid("execkeys-opid-caller"), f.projectA.ID, ScopeAgentLifecycle)

	first := doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agentInA.ID+"/keys", validKeysBody, token)
	second := doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agentInA.ID+"/keys", validKeysBody, token)

	var r1, r2 agentkeys.Response
	require.NoError(t, json.Unmarshal(first.Body.Bytes(), &r1))
	require.NoError(t, json.Unmarshal(second.Body.Bytes(), &r2))
	if r1.OperationID == "" || r2.OperationID == "" {
		t.Fatal("expected non-empty operation IDs on both requests")
	}
	if r1.OperationID == r2.OperationID {
		t.Errorf("expected distinct operation IDs per request, got the same value twice: %q", r1.OperationID)
	}
}

// TestExecuteAgentKeys_HumanCrossProjectWithAttach_Dispatched covers plan
// row AK-22: unlike an agent caller (which TestAuthorizeAgentKeysCrossProject
// pins is blanket-denied cross-project before any attach check), a human
// caller with a genuine attach relationship on a target in a *different*
// project than any other agent they own must still be dispatched, at the
// full HTTP level, on both route shapes. f.owner owns both f.agentInA
// (project A) and f.agentInB (project B); targeting f.agentInB exercises
// exactly this "no blanket cross-project ban for humans" property end to
// end, not just at authorizeAgentKeysCrossProject's own unit level.
func TestExecuteAgentKeys_HumanCrossProjectWithAttach_Dispatched(t *testing.T) {
	for _, shape := range keysRouteShapes {
		t.Run(shape.name, func(t *testing.T) {
			f, d, storeSpy, events := newExecuteAgentKeysFixture(t)

			rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, shape.path(f.agentInB), validKeysBody)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: status = %d, want 200; body: %s", shape.name, rec.Code, rec.Body.String())
			}
			if got := d.callCount(); got != 1 {
				t.Errorf("%s: dispatcher called %d times, want 1", shape.name, got)
			}
			assertNoKeysSideEffects(t, storeSpy, events)
		})
	}
}
