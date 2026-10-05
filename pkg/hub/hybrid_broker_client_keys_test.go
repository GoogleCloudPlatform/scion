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
	"fmt"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agentkeys"
)

// fakeKeysHTTPClient embeds the full mockRuntimeBrokerClient stub (defined in
// httpdispatcher_test.go) so it satisfies RuntimeBrokerClient for free, and
// adds ExecuteKeys so it also satisfies agentkeys.BrokerClient — the same
// "optional interface, type-asserted at the call site" pattern
// HybridBrokerClient already uses for brokerImageClient.
type fakeKeysHTTPClient struct {
	*mockRuntimeBrokerClient
	calls  int
	result agentkeys.BrokerResult
	err    error
}

func (f *fakeKeysHTTPClient) ExecuteKeys(ctx context.Context, brokerID, brokerEndpoint, agentSlug string, req agentkeys.BrokerRequest) (agentkeys.BrokerResult, error) {
	f.calls++
	if f.err != nil {
		return agentkeys.BrokerResult{}, f.err
	}
	return f.result, nil
}

// TestHybridBrokerClient_ExecuteKeys_RouteLocal proves a locally connected
// broker is actually dispatched via the control channel tunnel, never HTTP —
// by calling ExecuteKeys itself (not just checking route()'s decision), with
// a fake tunnel wired directly behind c.controlChannel so the call has
// somewhere real to go.
func TestHybridBrokerClient_ExecuteKeys_RouteLocal(t *testing.T) {
	tunnel := &mockControlChannelTunnel{connected: true, status: http.StatusOK}
	tunnel.body = mustMarshalBrokerResult(t, agentkeys.BrokerResult{OperationID: "op-1", Outcome: agentkeys.OutcomeDispatched})

	httpClient := &fakeKeysHTTPClient{mockRuntimeBrokerClient: &mockRuntimeBrokerClient{}}
	c := &HybridBrokerClient{
		controlChannel: &ControlChannelBrokerClient{manager: tunnel},
		httpClient:     httpClient,
	}

	result, err := c.ExecuteKeys(context.Background(), "broker-local", "", "agent-1", agentkeys.BrokerRequest{OperationID: "op-1", ExecuteBefore: time.Now().Add(time.Minute)})
	if err != nil {
		t.Fatalf("ExecuteKeys failed: %v", err)
	}
	if result.Outcome != agentkeys.OutcomeDispatched {
		t.Fatalf("outcome = %q, want dispatched", result.Outcome)
	}
	if tunnel.calls != 1 {
		t.Fatalf("expected exactly one tunnel call, got %d", tunnel.calls)
	}
	if tunnel.lastRequest == nil || tunnel.lastRequest.Path != "/api/v1/agents/agent-1/keys" {
		t.Fatalf("expected the tunneled request to hit the keys route, got %+v", tunnel.lastRequest)
	}
	if httpClient.calls != 0 {
		t.Fatalf("HTTP client must not be called when routeLocal, got %d calls", httpClient.calls)
	}
}

// TestHybridBrokerClient_ExecuteKeys_RouteLocalMidFlightFailure proves a
// tunnel failure after the connection check passed (e.g. a broker reconnect)
// classifies as uncertain — never a false success, never ErrNotDispatched —
// and is single-attempt with no fallback to HTTP after the possible send.
func TestHybridBrokerClient_ExecuteKeys_RouteLocalMidFlightFailure(t *testing.T) {
	tunnel := &mockControlChannelTunnel{connected: true, err: fmt.Errorf("tunnel closed: broker reconnecting")}
	httpClient := &fakeKeysHTTPClient{mockRuntimeBrokerClient: &mockRuntimeBrokerClient{}}
	c := &HybridBrokerClient{
		controlChannel: &ControlChannelBrokerClient{manager: tunnel},
		httpClient:     httpClient,
	}

	_, err := c.ExecuteKeys(context.Background(), "broker-local", "", "agent-1", agentkeys.BrokerRequest{ExecuteBefore: time.Now().Add(time.Minute)})
	if err == nil {
		t.Fatal("expected an error")
	}
	if errors.Is(err, agentkeys.ErrNotDispatched) {
		t.Fatalf("a mid-flight tunnel failure must not be reported as ErrNotDispatched, got %v", err)
	}
	if got := agentkeys.ClassifyDispatchError(err); got != agentkeys.OutcomeKeysOutcomeUnknown {
		t.Fatalf("ClassifyDispatchError = %q, want %q", got, agentkeys.OutcomeKeysOutcomeUnknown)
	}
	if tunnel.calls != 1 {
		t.Fatalf("expected exactly one tunnel attempt (no retry), got %d", tunnel.calls)
	}
	if httpClient.calls != 0 {
		t.Fatalf("must not fall back to HTTP after an uncertain send, got %d HTTP calls", httpClient.calls)
	}
}

// mustMarshalBrokerResult is a small test helper shared by the keys hybrid
// tests below.
func mustMarshalBrokerResult(t *testing.T, r agentkeys.BrokerResult) []byte {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("failed to marshal BrokerResult fixture: %v", err)
	}
	return b
}

// TestHybridBrokerClient_ExecuteKeys_RouteHTTP proves a broker with no local
// socket and a direct endpoint is dispatched via the HTTP keys client.
func TestHybridBrokerClient_ExecuteKeys_RouteHTTP(t *testing.T) {
	mgr := NewControlChannelManager(DefaultControlChannelConfig(), slog.Default())
	httpClient := &fakeKeysHTTPClient{
		mockRuntimeBrokerClient: &mockRuntimeBrokerClient{},
		result:                  agentkeys.BrokerResult{OperationID: "op-1", Outcome: agentkeys.OutcomeDispatched},
	}
	c := NewHybridBrokerClient(mgr, httpClient, nil, false)
	c.SetAffinityLookup(func(context.Context, string) (string, bool) { return "", false })

	result, err := c.ExecuteKeys(context.Background(), "broker-remote", "http://endpoint", "agent-1", agentkeys.BrokerRequest{ExecuteBefore: time.Now().Add(time.Minute)})
	if err != nil {
		t.Fatalf("ExecuteKeys failed: %v", err)
	}
	if result.Outcome != agentkeys.OutcomeDispatched {
		t.Fatalf("outcome = %q, want dispatched", result.Outcome)
	}
	if httpClient.calls != 1 {
		t.Fatalf("expected exactly one HTTP call, got %d", httpClient.calls)
	}
}

// TestHybridBrokerClient_ExecuteKeys_RouteForwardAndUndeliverable prove that,
// unlike MessageAgent (which defers to a durable queue via
// ErrMessageDeferred, consumed by dispatchWithBrokerRetry), keys has no
// durable queue: both routeForward (another node believed to own the broker)
// and routeUndeliverable (no owner, no endpoint) must return
// agentkeys.ErrNotDispatched directly — never ErrMessageDeferred, which would
// imply a retry/durable-delivery path that does not exist for keys (contract
// §4.3 "concrete trap named and closed") — and must never call the HTTP
// client.
//
// HybridBrokerClient's ExecuteKeys path has no reference to any message
// queue or store at all (only controlChannel and httpClient), so "zero
// message-queue writes" holds structurally here, not just by assertion: there
// is nothing in this call graph capable of writing to one. The explicit
// !errors.Is(err, ErrMessageDeferred) check below is still asserted directly,
// so a future change that routed keys through dispatchWithBrokerRetry (which
// is the only producer of a message-queue write reachable from that
// sentinel) would be caught here before it could reintroduce the trap.
func TestHybridBrokerClient_ExecuteKeys_RouteForwardAndUndeliverable(t *testing.T) {
	mgr := NewControlChannelManager(DefaultControlChannelConfig(), slog.Default())
	httpClient := &fakeKeysHTTPClient{mockRuntimeBrokerClient: &mockRuntimeBrokerClient{}}
	c := NewHybridBrokerClient(mgr, httpClient, nil, false)

	t.Run("routeForward", func(t *testing.T) {
		httpClient.calls = 0
		c.SetAffinityLookup(func(context.Context, string) (string, bool) { return "hubA", true })
		// A non-empty direct endpoint is the case that matters: route()
		// still picks routeForward (a live affinity owner wins over a direct
		// endpoint — see broker_routing.go's route()), and keys must not
		// fall back to that endpoint over HTTP just because one exists.
		_, err := c.ExecuteKeys(context.Background(), "broker-remote", "http://endpoint", "agent-1", agentkeys.BrokerRequest{ExecuteBefore: time.Now().Add(time.Minute)})
		if !errors.Is(err, agentkeys.ErrNotDispatched) {
			t.Fatalf("expected agentkeys.ErrNotDispatched, got %v", err)
		}
		if errors.Is(err, ErrMessageDeferred) {
			t.Fatalf("keys must never report ErrMessageDeferred (no durable queue exists for keys), got %v", err)
		}
		if got := agentkeys.ClassifyDispatchError(err); got != agentkeys.OutcomeKeysUnavailable {
			t.Fatalf("ClassifyDispatchError = %q, want %q", got, agentkeys.OutcomeKeysUnavailable)
		}
		if httpClient.calls != 0 {
			t.Fatalf("HTTP client must not be called for routeForward even though a direct endpoint exists, got %d calls", httpClient.calls)
		}
	})

	t.Run("routeUndeliverable", func(t *testing.T) {
		httpClient.calls = 0
		c.SetAffinityLookup(func(context.Context, string) (string, bool) { return "", false })
		_, err := c.ExecuteKeys(context.Background(), "broker-remote", "", "agent-1", agentkeys.BrokerRequest{ExecuteBefore: time.Now().Add(time.Minute)})
		if !errors.Is(err, agentkeys.ErrNotDispatched) {
			t.Fatalf("expected agentkeys.ErrNotDispatched, got %v", err)
		}
		if errors.Is(err, ErrMessageDeferred) {
			t.Fatalf("keys must never report ErrMessageDeferred (no durable queue exists for keys), got %v", err)
		}
		if httpClient.calls != 0 {
			t.Fatalf("HTTP client must not be called for routeUndeliverable, got %d calls", httpClient.calls)
		}
	})
}

// TestHybridBrokerClient_ExecuteKeys_HTTPClientMissingSupport proves a
// non-keys-aware httpClient (implementing only RuntimeBrokerClient) fails as
// agentkeys.ErrNotDispatched — a wiring defect proven before any request
// could be built, consistent with HTTPAgentDispatcher's handling of the same
// defect — rather than panicking or reporting a false success.
func TestHybridBrokerClient_ExecuteKeys_HTTPClientMissingSupport(t *testing.T) {
	mgr := NewControlChannelManager(DefaultControlChannelConfig(), slog.Default())
	c := NewHybridBrokerClient(mgr, &mockRuntimeBrokerClient{}, nil, false)
	c.SetAffinityLookup(func(context.Context, string) (string, bool) { return "", false })

	_, err := c.ExecuteKeys(context.Background(), "broker-remote", "http://endpoint", "agent-1", agentkeys.BrokerRequest{ExecuteBefore: time.Now().Add(time.Minute)})
	if !errors.Is(err, agentkeys.ErrNotDispatched) {
		t.Fatalf("expected agentkeys.ErrNotDispatched (a wiring defect proven before any request could be built), got %v", err)
	}
}

// droppingTunnel reports the control-channel session as connected on the
// first IsConnected call (the one route() makes) and disconnected after that,
// modelling a session that drops between the routing decision and the
// control-channel client's own pre-send check.
type droppingTunnel struct {
	*mockControlChannelTunnel
	checks int
}

func (d *droppingTunnel) IsConnected(string) bool {
	d.checks++
	return d.checks == 1
}

// TestHybridBrokerClient_ExecuteKeys_NeverDispatchedIsUnavailableOnEveryRoute
// pins ptone/scion#2628 across every route HybridBrokerClient can pick: when
// the Hub can prove the keys request never reached the broker, the outcome is
// keys_unavailable (503), never keys_outcome_unknown (502). The two HTTP-route
// cases use the real HTTP keys transport with a dial that never completes, so
// the failure surfaces only as the request context's deadline error.
func TestHybridBrokerClient_ExecuteKeys_NeverDispatchedIsUnavailableOnEveryRoute(t *testing.T) {
	const brokerID = "broker-2628"
	req := agentkeys.BrokerRequest{OperationID: "op-1", ExecuteBefore: time.Now().Add(time.Minute)}

	cases := []struct {
		name      string
		build     func(t *testing.T) (*HybridBrokerClient, func() int)
		endpoint  string
		wantRoute routeDecision
	}{
		{
			name:      "routeHTTP/stateless local broker",
			endpoint:  "http://broker.invalid:9800",
			wantRoute: routeHTTP,
			build: func(t *testing.T) (*HybridBrokerClient, func() int) {
				httpClient, dials := pendingDialKeysClient(t, 0)
				c := NewHybridBrokerClient(NewControlChannelManager(DefaultControlChannelConfig(), slog.Default()), httpClient, nil, false)
				c.SetStatelessLocalBrokers([]string{brokerID})
				return c, func() int { return int(dials.Load()) }
			},
		},
		{
			name:      "routeHTTP/no control-channel session, no live affinity owner",
			endpoint:  "http://broker.invalid:9800",
			wantRoute: routeHTTP,
			build: func(t *testing.T) (*HybridBrokerClient, func() int) {
				httpClient, dials := pendingDialKeysClient(t, 0)
				c := NewHybridBrokerClient(NewControlChannelManager(DefaultControlChannelConfig(), slog.Default()), httpClient, nil, false)
				c.SetAffinityLookup(func(context.Context, string) (string, bool) { return "hub-gone", false })
				return c, func() int { return int(dials.Load()) }
			},
		},
		{
			name:      "routeLocal/session drops before send",
			wantRoute: routeLocal,
			build: func(t *testing.T) (*HybridBrokerClient, func() int) {
				tunnel := &droppingTunnel{mockControlChannelTunnel: &mockControlChannelTunnel{}}
				c := &HybridBrokerClient{
					controlChannel: &ControlChannelBrokerClient{manager: tunnel},
					httpClient:     &fakeKeysHTTPClient{mockRuntimeBrokerClient: &mockRuntimeBrokerClient{}},
				}
				return c, func() int { return tunnel.calls }
			},
		},
		{
			name:      "routeForward",
			endpoint:  "http://broker.invalid:9800",
			wantRoute: routeForward,
			build: func(t *testing.T) (*HybridBrokerClient, func() int) {
				httpClient := &fakeKeysHTTPClient{mockRuntimeBrokerClient: &mockRuntimeBrokerClient{}}
				c := NewHybridBrokerClient(NewControlChannelManager(DefaultControlChannelConfig(), slog.Default()), httpClient, nil, false)
				c.SetAffinityLookup(func(context.Context, string) (string, bool) { return "hub-a", true })
				return c, func() int { return httpClient.calls }
			},
		},
		{
			name:      "routeUndeliverable",
			wantRoute: routeUndeliverable,
			build: func(t *testing.T) (*HybridBrokerClient, func() int) {
				httpClient := &fakeKeysHTTPClient{mockRuntimeBrokerClient: &mockRuntimeBrokerClient{}}
				c := NewHybridBrokerClient(NewControlChannelManager(DefaultControlChannelConfig(), slog.Default()), httpClient, nil, false)
				return c, func() int { return httpClient.calls }
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, attempts := tc.build(t)
			tunnel, dropping := c.controlChannel.manager.(*droppingTunnel)
			if !dropping {
				// route() has no side effects for these cases, so the
				// decision can be asserted directly before the real call.
				if got := c.route(context.Background(), brokerID, tc.endpoint); got != tc.wantRoute {
					t.Fatalf("route = %v, want %v", got, tc.wantRoute)
				}
			}

			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			_, err := c.ExecuteKeys(ctx, brokerID, tc.endpoint, "agent-1", req)
			if !errors.Is(err, agentkeys.ErrNotDispatched) {
				t.Fatalf("expected agentkeys.ErrNotDispatched, got %v", err)
			}
			if got := agentkeys.ClassifyDispatchError(err); got != agentkeys.OutcomeKeysUnavailable {
				t.Fatalf("ClassifyDispatchError = %q, want %q", got, agentkeys.OutcomeKeysUnavailable)
			}
			if dropping && tunnel.checks != 2 {
				// One check by route() (connected -> routeLocal), one by
				// the control-channel client's pre-send guard
				// (disconnected).
				t.Fatalf("expected 2 connection checks, got %d", tunnel.checks)
			}
			switch tc.wantRoute {
			case routeHTTP:
				if attempts() == 0 {
					t.Fatal("expected the HTTP route to start a dial")
				}
			default:
				if got := attempts(); got != 0 {
					t.Fatalf("expected no send attempt, got %d", got)
				}
			}
		})
	}
}
