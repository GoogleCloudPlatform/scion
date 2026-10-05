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
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/agentkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestExecuteAgentKeys_NoObserverFanOut covers plan row AK-33: the keys
// route must never trigger the message-broker bus's observer fan-out
// (MessageBrokerProxy.PublishMessage). No existing keys fixture wires a
// MessageBrokerProxy/eventbus at all, so a hypothetical leak through that
// path could not previously be observed. This test installs a real
// eventbus.EventBus and MessageBrokerProxy, subscribes directly to the
// exact topic PublishMessage would publish to for the target agent
// (eventbus.TopicAgentMessages), and asserts the subscriber never fires
// across several keys outcomes -- alongside a before/after store.Message
// row count, since a leak through this path would also mean an
// observer-visible row.
func TestExecuteAgentKeys_NoObserverFanOut(t *testing.T) {
	f, d, storeSpy, events := newExecuteAgentKeysFixture(t)

	bus := eventbus.NewInProcessEventBus(slog.Default())
	t.Cleanup(func() { _ = bus.Close() })
	proxy := NewMessageBrokerProxy(bus, f.store, events, func() AgentDispatcher { return nil }, slog.Default())
	f.srv.SetMessageBrokerProxy(proxy)

	var observerFired atomic.Int32
	topic := eventbus.TopicAgentMessages(f.agentInA.ProjectID, f.agentInA.Slug)
	_, err := bus.Subscribe(topic, func(_ context.Context, _ string, _ *messages.StructuredMessage) {
		observerFired.Add(1)
	})
	if err != nil {
		t.Fatalf("failed to subscribe to the observer topic: %v", err)
	}

	rowsBefore, err := f.store.ListMessages(context.Background(), store.MessageFilter{AgentID: f.agentInA.ID}, store.ListOptions{Limit: 100, SkipTotalCount: true})
	if err != nil {
		t.Fatalf("ListMessages (before): %v", err)
	}

	// Discarding every prior request's response
	// (`_ = doRequestWithAgentToken(...)`) would let a request rejected
	// early for an unrelated reason (bad token, scope, fixture drift)
	// still make "0 observer fires" pass trivially -- the loop would never prove
	// it actually reached the dispatcher for each outcome. Each case
	// asserts the response status/outcome it expects, and the final
	// dispatcher call count is checked against the exact number of requests
	// made.
	outcomes := []struct {
		err        error
		wantStatus int
		wantCode   string
	}{
		{nil, http.StatusOK, ""},
		{agentkeys.ErrNotDispatched, http.StatusServiceUnavailable, "keys_unavailable"},
		{&agentkeys.BrokerOutcomeError{Outcome: agentkeys.OutcomeNotFound}, http.StatusNotFound, "not_found"},
		{errors.New("keys: simulated ambiguous failure"), http.StatusBadGateway, "keys_outcome_unknown"},
	}
	var requestCount int
	for _, tc := range outcomes {
		d.err = tc.err
		token := f.agentToken(t, tid("ak33-outcome"), f.projectA.ID, ScopeAgentLifecycle)
		for _, shape := range keysRouteShapes {
			requestCount++
			rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, shape.path(f.agentInA), validKeysBody, token)
			if rec.Code != tc.wantStatus {
				t.Fatalf("%s/%v: status = %d, want %d; body: %s", shape.name, tc.err, rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantCode != "" {
				env := decodeKeysError(t, rec.Body.Bytes())
				if env.Code != tc.wantCode {
					t.Errorf("%s/%v: code = %q, want %q", shape.name, tc.err, env.Code, tc.wantCode)
				}
			}
		}
	}
	if got := d.callCount(); got != requestCount {
		t.Fatalf("dispatcher called %d times, want %d (one per request) -- this test is not actually exercising the dispatch path it claims to", got, requestCount)
	}

	if got := observerFired.Load(); got != 0 {
		t.Errorf("observer fan-out fired %d time(s); the keys route must never publish to the message bus", got)
	}

	rowsAfter, err := f.store.ListMessages(context.Background(), store.MessageFilter{AgentID: f.agentInA.ID}, store.ListOptions{Limit: 100, SkipTotalCount: true})
	if err != nil {
		t.Fatalf("ListMessages (after): %v", err)
	}
	if len(rowsAfter.Items) != len(rowsBefore.Items) {
		t.Errorf("store.Message row count changed: before=%d after=%d", len(rowsBefore.Items), len(rowsAfter.Items))
	}
	assertNoKeysSideEffects(t, storeSpy, events)
}

// brokerRetrySpyDispatcher wraps an AgentDispatcher and counts
// DispatchAgentMessage calls, so a test can prove the keys route never
// reaches dispatchWithBrokerRetry (which only ever calls
// DispatchAgentMessage, never DispatchAgentKeys).
//
// It also explicitly implements agentkeys.Dispatcher by delegating to keys,
// a second reference to the same underlying fake. This is load-bearing, not
// cosmetic: admitAndDispatchAgentKeys
// resolves its dispatcher via `s.GetDispatcher().(agentkeys.Dispatcher)`, a
// type assertion against the *static* method set. Embedding only the
// AgentDispatcher *interface* (as opposed to the concrete
// *fakeAgentKeysDispatcher*) promotes just the methods AgentDispatcher
// itself declares -- which does not include DispatchAgentKeys at all, since
// that lives on the separate agentkeys.Dispatcher interface. Without this
// method, the type assertion silently failed (ok == false), every request
// was denied OutcomeKeysUnavailable before the fake was ever called, and
// "0 DispatchAgentMessage calls" passed for a reason that had nothing to do
// with keys correctly avoiding the legacy dispatch path.
type brokerRetrySpyDispatcher struct {
	AgentDispatcher
	keys                      agentkeys.Dispatcher
	dispatchAgentMessageCalls atomic.Int32
}

func (d *brokerRetrySpyDispatcher) DispatchAgentMessage(ctx context.Context, agent *store.Agent, message string, interrupt bool, structuredMsg *messages.StructuredMessage) error {
	d.dispatchAgentMessageCalls.Add(1)
	return d.AgentDispatcher.DispatchAgentMessage(ctx, agent, message, interrupt, structuredMsg)
}

func (d *brokerRetrySpyDispatcher) DispatchAgentKeys(ctx context.Context, target agentkeys.Target, operationID string, executeBefore time.Time, keys string) (agentkeys.BrokerResult, error) {
	return d.keys.DispatchAgentKeys(ctx, target, operationID, executeBefore, keys)
}

// TestExecuteAgentKeys_NeverReachesDispatchAgentMessage covers plan row
// AK-34: a structural guarantee that the keys route never reaches
// dispatchWithBrokerRetry (or any other DispatchAgentMessage call) --
// dispatchWithBrokerRetry's only consumer is the legacy message path,
// keys exclusively uses DispatchAgentKeys. Wraps the fixture's dispatcher
// to count DispatchAgentMessage calls across every keys outcome and asserts
// the count stays zero throughout.
func TestExecuteAgentKeys_NeverReachesDispatchAgentMessage(t *testing.T) {
	f := newAgentKeysRouteFixture(t)
	inner := newFakeAgentKeysDispatcher()
	spy := &brokerRetrySpyDispatcher{AgentDispatcher: inner, keys: inner}
	f.srv.SetDispatcher(spy)

	// Each case asserts the response status/outcome
	// it expects, and both the keys-dispatcher call count and the
	// DispatchAgentMessage call count are checked against the exact number
	// of requests made (the former must equal it; the latter must stay
	// zero throughout).
	outcomes := []struct {
		err        error
		wantStatus int
		wantCode   string
	}{
		{nil, http.StatusOK, ""},
		{agentkeys.ErrNotDispatched, http.StatusServiceUnavailable, "keys_unavailable"},
		{&agentkeys.BrokerOutcomeError{Outcome: agentkeys.OutcomeAgentNotRunning}, http.StatusConflict, "agent_not_running"},
		{agent.ErrKeysUnsupported, http.StatusBadGateway, "keys_outcome_unknown"},
		{errors.New("keys: simulated ambiguous failure"), http.StatusBadGateway, "keys_outcome_unknown"},
	}
	var requestCount int
	for _, tc := range outcomes {
		inner.err = tc.err
		token := f.agentToken(t, tid("ak34-outcome"), f.projectA.ID, ScopeAgentLifecycle)
		for _, shape := range keysRouteShapes {
			requestCount++
			rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, shape.path(f.agentInA), validKeysBody, token)
			if rec.Code != tc.wantStatus {
				t.Fatalf("%s/%v: status = %d, want %d; body: %s", shape.name, tc.err, rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantCode != "" {
				env := decodeKeysError(t, rec.Body.Bytes())
				if env.Code != tc.wantCode {
					t.Errorf("%s/%v: code = %q, want %q", shape.name, tc.err, env.Code, tc.wantCode)
				}
			}
		}
	}

	if got := inner.callCount(); got != requestCount {
		t.Fatalf("DispatchAgentKeys called %d times, want %d (one per request) -- this test is not actually exercising the dispatch path it claims to", got, requestCount)
	}
	if got := spy.dispatchAgentMessageCalls.Load(); got != 0 {
		t.Errorf("DispatchAgentMessage called %d time(s) from the keys route; want 0 (keys must never reach dispatchWithBrokerRetry)", got)
	}
}

// TestLogAgentKeysInternalErrorAudit_RouteTagIsParameterized covers the
// contract's invariant (contract C§5: "the route tag must remain consistent
// across admission, outcome, and internal-error audit records emitted for
// the same request"): finishAgentKeysInternalError/logAgentKeysInternalErrorAudit
// take route as a real parameter rather than hardcoding "keys". This drives
// the function directly with every route value and asserts the audit record
// reflects whichever one was passed, so the invariant holds structurally,
// not just for the call sites that happen to exist right now.
func TestLogAgentKeysInternalErrorAudit_RouteTagIsParameterized(t *testing.T) {
	for _, route := range []agentKeysRoute{agentKeysRouteKeys, agentKeysRouteRawRemoved} {
		t.Run(string(route), func(t *testing.T) {
			f := newAgentKeysRouteFixture(t)
			log := installSentinelLogCapture(t)

			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test/keys", nil)
			rec := httptest.NewRecorder()
			f.srv.finishAgentKeysInternalError(rec, req, "op-route-param", agentKeysAuditTarget{ProjectID: f.projectA.ID}, 3, errors.New("simulated"), route)

			outcome := lastOutcomeAuditRecord(t, log)
			if outcome["route"] != string(route) {
				t.Errorf("audit route = %q, want %q", outcome["route"], string(route))
			}
		})
	}
}
