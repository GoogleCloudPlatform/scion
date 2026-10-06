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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agentkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// mockKeysBrokerClient embeds the full mockRuntimeBrokerClient stub so it
// satisfies RuntimeBrokerClient for the HTTPAgentDispatcher.client field, and
// adds ExecuteKeys so it also satisfies agentkeys.BrokerClient — proving
// HTTPAgentDispatcher.DispatchAgentKeys reaches the configured client via
// that type assertion.
type mockKeysBrokerClient struct {
	*mockRuntimeBrokerClient
	calls         int
	lastAgentSlug string
	lastReq       agentkeys.BrokerRequest
	lastBrokerID  string
	lastBrokerEP  string
	result        agentkeys.BrokerResult
	err           error
}

func (m *mockKeysBrokerClient) ExecuteKeys(ctx context.Context, brokerID, brokerEndpoint, agentSlug string, req agentkeys.BrokerRequest) (agentkeys.BrokerResult, error) {
	m.calls++
	m.lastBrokerID = brokerID
	m.lastBrokerEP = brokerEndpoint
	m.lastAgentSlug = agentSlug
	m.lastReq = req
	if m.err != nil {
		return agentkeys.BrokerResult{}, m.err
	}
	return m.result, nil
}

func newKeysDispatcherFixture(t *testing.T) (*HTTPAgentDispatcher, *mockKeysBrokerClient, agentkeys.Target) {
	t.Helper()
	ctx := context.Background()
	memStore := createTestStore(t)

	broker := &store.RuntimeBroker{
		ID:       tid("broker-1"),
		Name:     "test-broker",
		Slug:     "test-broker",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	mockClient := &mockKeysBrokerClient{mockRuntimeBrokerClient: &mockRuntimeBrokerClient{}}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())

	target := agentkeys.Target{
		RuntimeBrokerID: tid("broker-1"),
		AgentID:         tid("agent-1"),
		AgentSlug:       "test-agent",
		ProjectID:       tid("project-1"),
	}
	return dispatcher, mockClient, target
}

// TestHTTPAgentDispatcher_DispatchAgentKeys_Success proves DispatchAgentKeys
// resolves the broker endpoint (the one store lookup it may perform, per
// .design/agent-keys-contract.md §4.4), builds BrokerRequest from Target plus
// its own operationID/executeBefore/keys arguments with nothing left to
// disagree with itself, and hands it to the configured client.
func TestHTTPAgentDispatcher_DispatchAgentKeys_Success(t *testing.T) {
	dispatcher, mockClient, target := newKeysDispatcherFixture(t)
	mockClient.result = agentkeys.BrokerResult{OperationID: "op-1", Outcome: agentkeys.OutcomeDispatched}

	// A non-UTC zone makes the UTC-normalization assertion below
	// non-vacuous: time.Now() alone is frequently already UTC in CI.
	deadline := time.Now().In(time.FixedZone("UTC+1", 3600)).Add(20 * time.Second)
	result, err := dispatcher.DispatchAgentKeys(context.Background(), target, "op-1", deadline, "C-c")
	if err != nil {
		t.Fatalf("DispatchAgentKeys failed: %v", err)
	}
	if result.Outcome != agentkeys.OutcomeDispatched {
		t.Fatalf("outcome = %q, want dispatched", result.Outcome)
	}
	if mockClient.calls != 1 {
		t.Fatalf("expected exactly one ExecuteKeys call, got %d", mockClient.calls)
	}
	if mockClient.lastBrokerID != target.RuntimeBrokerID {
		t.Errorf("brokerID = %s, want %s", mockClient.lastBrokerID, target.RuntimeBrokerID)
	}
	if mockClient.lastBrokerEP != "http://localhost:9800" {
		t.Errorf("brokerEndpoint = %s, want http://localhost:9800", mockClient.lastBrokerEP)
	}
	if mockClient.lastAgentSlug != target.AgentSlug {
		t.Errorf("agentSlug = %s, want %s", mockClient.lastAgentSlug, target.AgentSlug)
	}
	if mockClient.lastReq.AgentID != target.AgentID {
		t.Errorf("req.AgentID = %s, want %s", mockClient.lastReq.AgentID, target.AgentID)
	}
	if mockClient.lastReq.ProjectID != target.ProjectID {
		t.Errorf("req.ProjectID = %s, want %s", mockClient.lastReq.ProjectID, target.ProjectID)
	}
	if mockClient.lastReq.OperationID != "op-1" {
		t.Errorf("req.OperationID = %s, want op-1", mockClient.lastReq.OperationID)
	}
	if mockClient.lastReq.Keys != "C-c" {
		t.Errorf("req.Keys = %q, want C-c", mockClient.lastReq.Keys)
	}
	if !mockClient.lastReq.ExecuteBefore.Equal(deadline) {
		t.Errorf("req.ExecuteBefore = %v, want %v", mockClient.lastReq.ExecuteBefore, deadline)
	}
	if mockClient.lastReq.ExecuteBefore.Location() != time.UTC {
		t.Errorf("req.ExecuteBefore must be normalized to UTC before dispatch, got location %v", mockClient.lastReq.ExecuteBefore.Location())
	}
}

// TestHTTPAgentDispatcher_DispatchAgentKeys_PropagatesClientOutcome proves a
// non-nil error from the underlying client (however it is classified) passes
// straight through, unmodified and unretried.
func TestHTTPAgentDispatcher_DispatchAgentKeys_PropagatesClientOutcome(t *testing.T) {
	dispatcher, mockClient, target := newKeysDispatcherFixture(t)
	mockClient.err = &agentkeys.BrokerOutcomeError{Outcome: agentkeys.OutcomeAgentNotRunning}

	_, err := dispatcher.DispatchAgentKeys(context.Background(), target, "op-1", time.Now().Add(time.Minute), "C-c")
	var boe *agentkeys.BrokerOutcomeError
	if !errors.As(err, &boe) || boe.Outcome != agentkeys.OutcomeAgentNotRunning {
		t.Fatalf("expected AgentNotRunning BrokerOutcomeError to propagate, got %v", err)
	}
	if mockClient.calls != 1 {
		t.Fatalf("expected exactly one ExecuteKeys call (no retry), got %d", mockClient.calls)
	}
}

// TestHTTPAgentDispatcher_DispatchAgentKeys_UnknownBroker proves a broker
// lookup failure (e.g. the broker record was deleted) is reported as
// agentkeys.ErrNotDispatched — a Hub-side failure proven before any request
// could have reached a broker — without ever calling the client.
func TestHTTPAgentDispatcher_DispatchAgentKeys_UnknownBroker(t *testing.T) {
	dispatcher, mockClient, target := newKeysDispatcherFixture(t)
	target.RuntimeBrokerID = tid("no-such-broker")

	_, err := dispatcher.DispatchAgentKeys(context.Background(), target, "op-1", time.Now().Add(time.Minute), "C-c")
	if !errors.Is(err, agentkeys.ErrNotDispatched) {
		t.Fatalf("expected agentkeys.ErrNotDispatched, got %v", err)
	}
	if mockClient.calls != 0 {
		t.Fatalf("expected zero ExecuteKeys calls when the broker cannot be resolved, got %d", mockClient.calls)
	}
}

// TestHTTPAgentDispatcher_DispatchAgentKeys_ClientWithoutKeysSupport proves a
// RuntimeBrokerClient that does not implement agentkeys.BrokerClient (a
// wiring defect) fails as ErrNotDispatched rather than panicking.
func TestHTTPAgentDispatcher_DispatchAgentKeys_ClientWithoutKeysSupport(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)
	broker := &store.RuntimeBroker{ID: tid("broker-1"), Name: "b", Slug: "b", Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline}
	if err := memStore.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, &mockRuntimeBrokerClient{}, false, slog.Default())

	target := agentkeys.Target{RuntimeBrokerID: tid("broker-1"), AgentID: tid("agent-1"), AgentSlug: "a", ProjectID: tid("project-1")}
	_, err := dispatcher.DispatchAgentKeys(ctx, target, "op-1", time.Now().Add(time.Minute), "C-c")
	if !errors.Is(err, agentkeys.ErrNotDispatched) {
		t.Fatalf("expected agentkeys.ErrNotDispatched, got %v", err)
	}
}

// TestHTTPAgentDispatcher_DispatchAgentKeys_FailsClosedOnInvalidInput proves a
// zero or already-past executeBefore, an empty operationID, or an empty
// Target identity field is rejected as agentkeys.ErrNotDispatched before any
// client call. BrokerRequest.ExecuteBefore's doc requires failing closed on a
// missing/invalid deadline, and there is no reason to spend a network round
// trip on a request the broker is contractually required to reject anyway.
// An empty operationID gets the same treatment because
// decodeBrokerKeysResponse's success-path echo check ("OperationID ==
// expectedOperationID") would otherwise be vacuous for it: an empty echo
// would satisfy an empty expectation. An empty AgentSlug/AgentID/ProjectID
// gets the same treatment because these are caller bugs (task 2.2 always
// passes them from an already-resolved *store.Agent), and an empty AgentSlug
// in particular would build a self-redirecting broker path this adapter's
// redirect-refusing client would then report as an uncertain "may have run"
// outcome for a request that never reached a handler.
func TestHTTPAgentDispatcher_DispatchAgentKeys_FailsClosedOnInvalidInput(t *testing.T) {
	dispatcher, mockClient, target := newKeysDispatcherFixture(t)

	cases := []struct {
		name         string
		operationID  string
		deadline     time.Time
		mutateTarget func(agentkeys.Target) agentkeys.Target
	}{
		{"zero deadline", "op-1", time.Time{}, nil},
		{"already past", "op-1", time.Now().Add(-time.Second), nil},
		{"empty operation ID", "", time.Now().Add(time.Minute), nil},
		{"empty agent slug", "op-1", time.Now().Add(time.Minute), func(tg agentkeys.Target) agentkeys.Target {
			tg.AgentSlug = ""
			return tg
		}},
		{"empty agent ID", "op-1", time.Now().Add(time.Minute), func(tg agentkeys.Target) agentkeys.Target {
			tg.AgentID = ""
			return tg
		}},
		{"empty project ID", "op-1", time.Now().Add(time.Minute), func(tg agentkeys.Target) agentkeys.Target {
			tg.ProjectID = ""
			return tg
		}},
		{"empty runtime broker ID", "op-1", time.Now().Add(time.Minute), func(tg agentkeys.Target) agentkeys.Target {
			tg.RuntimeBrokerID = ""
			return tg
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mockClient.calls = 0
			tg := target
			if tc.mutateTarget != nil {
				tg = tc.mutateTarget(tg)
			}
			_, err := dispatcher.DispatchAgentKeys(context.Background(), tg, tc.operationID, tc.deadline, "C-c")
			if !errors.Is(err, agentkeys.ErrNotDispatched) {
				t.Fatalf("expected agentkeys.ErrNotDispatched, got %v", err)
			}
			if mockClient.calls != 0 {
				t.Fatalf("expected zero ExecuteKeys calls for a %s, got %d", tc.name, mockClient.calls)
			}
		})
	}
}
