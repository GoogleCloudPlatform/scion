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

	"github.com/GoogleCloudPlatform/scion/pkg/agentkeys"
)

// TestControlChannelBrokerClient_ExecuteKeys_Dispatched proves the
// control-channel transport tunnels the same method/path/query/body shape the
// HTTP transport uses (.design/agent-keys-contract.md §4.1's "there is no
// separate control-channel RPC method name" rule) and that a 200 dispatch
// round-trips.
func TestControlChannelBrokerClient_ExecuteKeys_Dispatched(t *testing.T) {
	tunnel := &mockControlChannelTunnel{connected: true}
	signer := &mockBrokerSigner{}
	client := &ControlChannelBrokerClient{manager: tunnel, signer: signer}

	// A non-UTC zone makes the UTC-normalization assertion below
	// non-vacuous: time.Now() alone would often already be UTC in CI, so a
	// bug that skipped .UTC() before marshaling could pass unnoticed.
	deadline := time.Now().In(time.FixedZone("UTC+1", 3600)).Add(30 * time.Second)
	req := agentkeys.BrokerRequest{
		ProjectID:     "project-1",
		AgentID:       "agent-1",
		OperationID:   "op-1",
		ExecuteBefore: deadline,
		Keys:          "Enter",
	}

	tunnel.body, _ = json.Marshal(agentkeys.BrokerResult{OperationID: "op-1", Outcome: agentkeys.OutcomeDispatched})
	tunnel.status = http.StatusOK

	result, err := client.ExecuteKeys(context.Background(), "broker-1", "unused", "test-agent", req)
	if err != nil {
		t.Fatalf("ExecuteKeys failed: %v", err)
	}
	if result.Outcome != agentkeys.OutcomeDispatched {
		t.Fatalf("expected dispatched, got %q", result.Outcome)
	}
	if !signer.called {
		t.Fatal("expected the signer to be invoked")
	}
	if tunnel.lastRequest == nil {
		t.Fatal("expected a tunneled request to be captured")
	}
	if tunnel.lastRequest.Method != agentkeys.BrokerRouteMethod {
		t.Errorf("method = %s, want %s", tunnel.lastRequest.Method, agentkeys.BrokerRouteMethod)
	}
	if tunnel.lastRequest.Path != "/api/v1/agents/test-agent/keys" {
		t.Errorf("path = %s, want /api/v1/agents/test-agent/keys", tunnel.lastRequest.Path)
	}
	// Parse and assert exactly, not by substring: a substring match would
	// still pass on a duplicated or extra query parameter (e.g.
	// "projectId=project-1&projectId=other"), which matters because the
	// broker's Query().Get takes only the first value.
	gotQuery, err := url.ParseQuery(tunnel.lastRequest.Query)
	if err != nil {
		t.Fatalf("failed to parse tunneled query %q: %v", tunnel.lastRequest.Query, err)
	}
	if len(gotQuery) != 1 {
		t.Fatalf("query = %q, want exactly one parameter", tunnel.lastRequest.Query)
	}
	if got := gotQuery.Get(agentkeys.BrokerProjectIDQueryParam); got != "project-1" {
		t.Errorf("query %s = %q, want %q", agentkeys.BrokerProjectIDQueryParam, got, "project-1")
	}
	var wire agentkeys.BrokerRequest
	if err := json.Unmarshal(tunnel.lastRequest.Body, &wire); err != nil {
		t.Fatalf("failed to decode tunneled body: %v", err)
	}
	if wire.Keys != "Enter" || wire.AgentID != "agent-1" || wire.OperationID != "op-1" {
		t.Errorf("unexpected tunneled body: %+v", wire)
	}
	if wire.ProjectID != "project-1" {
		t.Errorf("body.ProjectID = %q, want %q", wire.ProjectID, "project-1")
	}
	if !wire.ExecuteBefore.Equal(deadline) {
		t.Errorf("body.ExecuteBefore = %v, want %v", wire.ExecuteBefore, deadline)
	}
	// The raw wire value must be UTC-normalized (RFC 3339 "Z" suffix), not
	// carrying the non-UTC zone offset the caller constructed it with — the
	// broker compares this deadline against its own UTC clock.
	var rawFields map[string]json.RawMessage
	if err := json.Unmarshal(tunnel.lastRequest.Body, &rawFields); err != nil {
		t.Fatalf("failed to decode tunneled body as a raw map: %v", err)
	}
	var rawExecuteBefore string
	if err := json.Unmarshal(rawFields["execute_before"], &rawExecuteBefore); err != nil {
		t.Fatalf("failed to decode raw execute_before: %v", err)
	}
	if !strings.HasSuffix(rawExecuteBefore, "Z") {
		t.Errorf("execute_before on the wire = %q, want a UTC (Z-suffixed) timestamp", rawExecuteBefore)
	}
}

// TestControlChannelBrokerClient_ExecuteKeys_NotConnected proves the pre-send
// connection check fails closed to agentkeys.ErrNotDispatched without ever
// attempting a tunnel round-trip (proven via the call counter) — this is a
// provable, before-any-send failure, unlike a mid-flight disconnect.
func TestControlChannelBrokerClient_ExecuteKeys_NotConnected(t *testing.T) {
	tunnel := &mockControlChannelTunnel{connected: false}
	client := &ControlChannelBrokerClient{manager: tunnel}

	_, err := client.ExecuteKeys(context.Background(), "broker-1", "unused", "test-agent", agentkeys.BrokerRequest{ExecuteBefore: time.Now().Add(time.Minute)})
	if !errors.Is(err, agentkeys.ErrNotDispatched) {
		t.Fatalf("expected agentkeys.ErrNotDispatched, got %v", err)
	}
	if got := agentkeys.ClassifyDispatchError(err); got != agentkeys.OutcomeKeysUnavailable {
		t.Fatalf("ClassifyDispatchError = %q, want %q", got, agentkeys.OutcomeKeysUnavailable)
	}
	if tunnel.calls != 0 {
		t.Fatalf("expected zero tunnel attempts when not connected, got %d", tunnel.calls)
	}
}

// TestControlChannelBrokerClient_ExecuteKeys_TooLargeForTunnel proves the
// pre-send body-size check also fails closed to ErrNotDispatched without
// attempting a tunnel round-trip: unlike MessageAgent, keys has no HTTP
// fallback to retry through when a payload cannot be tunneled.
func TestControlChannelBrokerClient_ExecuteKeys_TooLargeForTunnel(t *testing.T) {
	tunnel := &mockControlChannelTunnel{connected: true}
	client := &ControlChannelBrokerClient{manager: tunnel}

	huge := strings.Repeat("a", maxControlChannelBodySize+1)
	_, err := client.ExecuteKeys(context.Background(), "broker-1", "unused", "test-agent", agentkeys.BrokerRequest{Keys: huge, ExecuteBefore: time.Now().Add(time.Minute)})
	if !errors.Is(err, agentkeys.ErrNotDispatched) {
		t.Fatalf("expected agentkeys.ErrNotDispatched, got %v", err)
	}
	if tunnel.calls != 0 {
		t.Fatalf("expected zero tunnel attempts for an oversized payload, got %d", tunnel.calls)
	}
}

// TestControlChannelBrokerClient_ExecuteKeys_MidFlightFailure simulates a
// broker reconnect / response loss after the connection check already passed
// (IsConnected returned true, but the tunnel round-trip itself then fails).
// This must NOT be reported as ErrNotDispatched — the request may have
// reached the broker — and must be single-attempt (exactly one call).
func TestControlChannelBrokerClient_ExecuteKeys_MidFlightFailure(t *testing.T) {
	tunnel := &mockControlChannelTunnel{connected: true, err: fmt.Errorf("tunnel closed: broker reconnecting")}
	client := &ControlChannelBrokerClient{manager: tunnel}

	_, err := client.ExecuteKeys(context.Background(), "broker-1", "unused", "test-agent", agentkeys.BrokerRequest{ExecuteBefore: time.Now().Add(time.Minute)})
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
		t.Fatalf("expected exactly one tunnel attempt (no reconnect/retry), got %d", tunnel.calls)
	}
}

// TestControlChannelBrokerClient_ExecuteKeys_OldBrokerUnsupported pins the
// same 404-without-outcome classification the HTTP transport uses, now over
// the control-channel path.
func TestControlChannelBrokerClient_ExecuteKeys_OldBrokerUnsupported(t *testing.T) {
	tunnel := &mockControlChannelTunnel{
		connected: true,
		status:    http.StatusNotFound,
		body:      []byte(`{"error":{"code":"not_found","message":"Action not found"}}`),
	}
	client := &ControlChannelBrokerClient{manager: tunnel}

	_, err := client.ExecuteKeys(context.Background(), "broker-1", "unused", "test-agent", agentkeys.BrokerRequest{ExecuteBefore: time.Now().Add(time.Minute)})
	var boe *agentkeys.BrokerOutcomeError
	if !errors.As(err, &boe) {
		t.Fatalf("expected a *BrokerOutcomeError, got %v", err)
	}
	if boe.Outcome != agentkeys.OutcomeKeysUnsupported {
		t.Fatalf("outcome = %q, want %q", boe.Outcome, agentkeys.OutcomeKeysUnsupported)
	}
	if tunnel.calls != 1 {
		t.Fatalf("expected exactly one tunnel attempt, got %d", tunnel.calls)
	}
}

// TestControlChannelBrokerClient_ExecuteKeys_ServerError proves a 5xx-style
// response over the control channel (a non-BrokerResult error envelope, the
// same shape the runtime broker's generic error handler would produce)
// classifies as outcome_unknown and is single-attempt — per ptone/scion#2194
// AC3, the same proof TestHTTPRuntimeBrokerClient_ExecuteKeys_UnknownOutcomes
// gives for the HTTP transport.
func TestControlChannelBrokerClient_ExecuteKeys_ServerError(t *testing.T) {
	cases := []struct {
		name   string
		status int
	}{
		{"502", http.StatusBadGateway},
		{"503", http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tunnel := &mockControlChannelTunnel{
				connected: true,
				status:    tc.status,
				body:      []byte(`{"error":{"code":"internal","message":"boom"}}`),
			}
			client := &ControlChannelBrokerClient{manager: tunnel}

			_, err := client.ExecuteKeys(context.Background(), "broker-1", "unused", "test-agent", agentkeys.BrokerRequest{ExecuteBefore: time.Now().Add(time.Minute)})
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := agentkeys.ClassifyDispatchError(err); got != agentkeys.OutcomeKeysOutcomeUnknown {
				t.Fatalf("ClassifyDispatchError = %q, want %q (err=%v)", got, agentkeys.OutcomeKeysOutcomeUnknown, err)
			}
			if tunnel.calls != 1 {
				t.Fatalf("expected exactly one tunnel attempt, got %d", tunnel.calls)
			}
		})
	}
}

// failingControlChannelSigner always fails, for testing that a signing
// failure — proven to occur before the tunnel is ever used — classifies as
// agentkeys.ErrNotDispatched rather than an uncertain outcome.
type failingControlChannelSigner struct{}

func (failingControlChannelSigner) Sign(context.Context, *http.Request, string) error {
	return errors.New("boom: no broker secret")
}

// TestControlChannelBrokerClient_ExecuteKeys_SignerFailureIsNotDispatched
// proves a signing failure is reported as agentkeys.ErrNotDispatched, and
// that the tunnel is never used when signing fails.
func TestControlChannelBrokerClient_ExecuteKeys_SignerFailureIsNotDispatched(t *testing.T) {
	tunnel := &mockControlChannelTunnel{connected: true}
	client := &ControlChannelBrokerClient{manager: tunnel, signer: failingControlChannelSigner{}}

	_, err := client.ExecuteKeys(context.Background(), "broker-1", "unused", "test-agent", agentkeys.BrokerRequest{ExecuteBefore: time.Now().Add(time.Minute)})
	if !errors.Is(err, agentkeys.ErrNotDispatched) {
		t.Fatalf("expected agentkeys.ErrNotDispatched, got %v", err)
	}
	if tunnel.calls != 0 {
		t.Fatalf("expected zero tunnel attempts when signing fails, got %d", tunnel.calls)
	}
}
