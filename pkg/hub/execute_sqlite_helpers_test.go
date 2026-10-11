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
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agentkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
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

// keysRouteShape parameterizes a test over both public route shapes, so
// post-admission outcomes are exercised on the project-scoped route as well
// as the top-level one.
type keysRouteShape struct {
	name string
	path func(agent *store.Agent) string
}

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
func (e *agentKeysEventSpy) PublishUserNotification(_ context.Context, _ *store.Notification) {
	e.record("PublishUserNotification")
}
func (e *agentKeysEventSpy) PublishUserMessage(_ context.Context, _ *store.Message, _ []AttachmentRef, _ []artifacts.MessageRef) {
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
func (e *agentKeysEventSpy) PublishChatOwnStateChanged(_ context.Context, _, _, _ string, _ *bool) {
	e.record("PublishChatOwnStateChanged")
}
func (e *agentKeysEventSpy) PublishChatMemberMessage(_ context.Context, _ *store.Message, _ []AttachmentRef, _ []artifacts.MessageRef, _ []string) {
	e.record("PublishChatMemberMessage")
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

// logfmtFieldRe extracts key=value pairs from one line of
// slog.NewTextHandler output (the format installSentinelLogCapture
// installs): value is either a double-quoted, possibly-escaped string, or a
// bare run of non-space characters (including the empty string, for a field
// slog rendered as "key=" with no quoting).
var logfmtFieldRe = regexp.MustCompile(`(\S+?)=("(?:[^"\\]|\\.)*"|\S*)`)
