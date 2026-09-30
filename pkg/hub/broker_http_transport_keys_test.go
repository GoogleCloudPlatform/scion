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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agentkeys"
)

// TestHTTPRuntimeBrokerClient_ExecuteKeys_Dispatched proves the HTTP
// transport hits the frozen route/query/body shape (.design/
// agent-keys-contract.md §4.1) and that a normal 200 dispatch round-trips.
func TestHTTPRuntimeBrokerClient_ExecuteKeys_Dispatched(t *testing.T) {
	var gotMethod, gotPath, gotQuery string
	var gotBody agentkeys.BrokerRequest
	var gotRawExecuteBefore string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		rawBody, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("failed to read request body: %v", err)
		}
		if err := json.Unmarshal(rawBody, &gotBody); err != nil {
			t.Errorf("failed to decode request body: %v", err)
		}
		var rawFields map[string]json.RawMessage
		if err := json.Unmarshal(rawBody, &rawFields); err == nil {
			_ = json.Unmarshal(rawFields["execute_before"], &gotRawExecuteBefore)
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(agentkeys.BrokerResult{
			OperationID: gotBody.OperationID,
			Outcome:     agentkeys.OutcomeDispatched,
		})
	}))
	defer server.Close()

	client := NewHTTPRuntimeBrokerClient()
	// A non-UTC zone makes the UTC-normalization assertion below
	// non-vacuous: time.Now() alone is frequently already UTC in CI (offset
	// zero), which would let a missing/broken .UTC() call pass unnoticed.
	deadline := time.Now().In(time.FixedZone("UTC+1", 3600)).Add(30 * time.Second)
	req := agentkeys.BrokerRequest{
		ProjectID:     tid("project-1"),
		AgentID:       tid("agent-1"),
		OperationID:   "op-123",
		ExecuteBefore: deadline,
		Keys:          "C-c",
	}

	result, err := client.ExecuteKeys(context.Background(), tid("broker-1"), server.URL, "test-agent", req)
	if err != nil {
		t.Fatalf("ExecuteKeys failed: %v", err)
	}
	if result.Outcome != agentkeys.OutcomeDispatched {
		t.Fatalf("expected dispatched outcome, got %q", result.Outcome)
	}

	if gotMethod != agentkeys.BrokerRouteMethod {
		t.Errorf("method = %s, want %s", gotMethod, agentkeys.BrokerRouteMethod)
	}
	if gotPath != "/api/v1/agents/test-agent/keys" {
		t.Errorf("path = %s, want /api/v1/agents/test-agent/keys", gotPath)
	}
	q, _ := url.ParseQuery(gotQuery)
	if got := q.Get(agentkeys.BrokerProjectIDQueryParam); got != req.ProjectID {
		t.Errorf("query %s = %s, want %s", agentkeys.BrokerProjectIDQueryParam, got, req.ProjectID)
	}
	if gotBody.AgentID != req.AgentID {
		t.Errorf("body.AgentID = %s, want %s", gotBody.AgentID, req.AgentID)
	}
	if gotBody.ProjectID != req.ProjectID {
		t.Errorf("body.ProjectID = %s, want %s", gotBody.ProjectID, req.ProjectID)
	}
	if gotBody.Keys != "C-c" {
		t.Errorf("body.Keys = %q, want %q", gotBody.Keys, "C-c")
	}
	if !gotBody.ExecuteBefore.Equal(deadline) {
		t.Errorf("body.ExecuteBefore = %v, want %v", gotBody.ExecuteBefore, deadline)
	}
	if gotBody.ExecuteBefore.Location() != time.UTC {
		t.Errorf("body.ExecuteBefore must be marshaled in UTC, got location %v", gotBody.ExecuteBefore.Location())
	}
	if !strings.HasSuffix(gotRawExecuteBefore, "Z") {
		t.Errorf("execute_before on the wire = %q, want a UTC (Z-suffixed) timestamp", gotRawExecuteBefore)
	}
}

// TestHTTPRuntimeBrokerClient_ExecuteKeys_OldBrokerIsUnsupported pins the one
// specified 404 exception in .design/agent-keys-contract.md §4.3: an old
// broker's generic unrecognized-action handler answers 404 with no top-level
// "outcome" field, which must classify as keys_unsupported, not unknown.
func TestHTTPRuntimeBrokerClient_ExecuteKeys_OldBrokerIsUnsupported(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"Action not found"}}`))
	}))
	defer server.Close()

	client := NewHTTPRuntimeBrokerClient()
	_, err := client.ExecuteKeys(context.Background(), tid("broker-1"), server.URL, "test-agent", agentkeys.BrokerRequest{ExecuteBefore: time.Now().Add(time.Minute)})
	if err == nil {
		t.Fatal("expected an error")
	}
	var boe *agentkeys.BrokerOutcomeError
	if !errors.As(err, &boe) {
		t.Fatalf("expected a *BrokerOutcomeError, got %v", err)
	}
	if boe.Outcome != agentkeys.OutcomeKeysUnsupported {
		t.Fatalf("outcome = %q, want %q", boe.Outcome, agentkeys.OutcomeKeysUnsupported)
	}
	if got := agentkeys.ClassifyDispatchError(err); got != agentkeys.OutcomeKeysUnsupported {
		t.Fatalf("ClassifyDispatchError = %q, want %q", got, agentkeys.OutcomeKeysUnsupported)
	}
}

// TestHTTPRuntimeBrokerClient_ExecuteKeys_BrokerDecision pins the broker's own
// well-formed allow-listed decisions (agent not running, terminal not ready)
// round-tripping through ExecuteKeys.
func TestHTTPRuntimeBrokerClient_ExecuteKeys_BrokerDecision(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		outcome agentkeys.Outcome
	}{
		{"agent not running", http.StatusConflict, agentkeys.OutcomeAgentNotRunning},
		{"terminal not ready", http.StatusConflict, agentkeys.OutcomeTerminalNotReady},
		{"keys unsupported (managed backend)", http.StatusUnprocessableEntity, agentkeys.OutcomeKeysUnsupported},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(agentkeys.BrokerResult{OperationID: "op-1", Outcome: tc.outcome})
			}))
			defer server.Close()

			client := NewHTTPRuntimeBrokerClient()
			_, err := client.ExecuteKeys(context.Background(), tid("broker-1"), server.URL, "test-agent", agentkeys.BrokerRequest{ExecuteBefore: time.Now().Add(time.Minute)})
			var boe *agentkeys.BrokerOutcomeError
			if !errors.As(err, &boe) {
				t.Fatalf("expected a *BrokerOutcomeError, got %v", err)
			}
			if boe.Outcome != tc.outcome {
				t.Fatalf("outcome = %q, want %q", boe.Outcome, tc.outcome)
			}
		})
	}
}

// TestHTTPRuntimeBrokerClient_ExecuteKeys_ResponseBodyIsBounded proves a
// misbehaving or compromised broker cannot make ExecuteKeys read an
// unbounded response body: agentkeys.BrokerResult is a small, fixed-shape
// JSON value, so a response many times larger than maxKeysResponseBodyBytes
// must still be read promptly and truncated, and — since the truncated body
// no longer parses as a well-formed BrokerResult — must classify as an
// honest "outcome unknown" rather than a false success or a crash.
func TestHTTPRuntimeBrokerClient_ExecuteKeys_ResponseBodyIsBounded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		// A well-formed-looking success body, padded with far more bytes
		// than the response ceiling allows, via a long "message" value.
		_, _ = w.Write([]byte(`{"operation_id":"op-1","outcome":"dispatched","message":"`))
		padding := make([]byte, 10*maxKeysResponseBodyBytes)
		for i := range padding {
			padding[i] = 'a'
		}
		_, _ = w.Write(padding)
		_, _ = w.Write([]byte(`"}`))
	}))
	defer server.Close()

	client := NewHTTPRuntimeBrokerClient()
	done := make(chan struct{})
	var err error
	go func() {
		defer close(done)
		_, err = client.ExecuteKeys(context.Background(), tid("broker-1"), server.URL, "test-agent", agentkeys.BrokerRequest{ExecuteBefore: time.Now().Add(time.Minute)})
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("ExecuteKeys did not return promptly reading an oversized response body")
	}

	if err == nil {
		t.Fatal("expected an error: the truncated body cannot parse as a well-formed BrokerResult")
	}
	if got := agentkeys.ClassifyDispatchError(err); got != agentkeys.OutcomeKeysOutcomeUnknown {
		t.Fatalf("ClassifyDispatchError = %q, want %q (err=%v)", got, agentkeys.OutcomeKeysOutcomeUnknown, err)
	}
}

// TestHTTPRuntimeBrokerClient_ExecuteKeys_UnknownOutcomes proves malformed or
// disagreeing responses classify as outcome_unknown rather than being
// guessed at — never a false "definitely didn't happen" or "delivered" — and,
// per ptone/scion#2194 AC3, that exactly one application dispatch attempt is
// made even for a 5xx: a hit counter on the handler proves nothing in this
// adapter retries on a server error.
func TestHTTPRuntimeBrokerClient_ExecuteKeys_UnknownOutcomes(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"2xx with wrong outcome", http.StatusOK, `{"operation_id":"op-1","outcome":"agent_not_running"}`},
		{"status/outcome mismatch", http.StatusNotFound, `{"operation_id":"op-1","outcome":"agent_not_running"}`},
		{"generic 5xx (502)", http.StatusBadGateway, `{"error":{"code":"internal","message":"boom"}}`},
		// 503 with a non-BrokerResult body is the status a retrying HTTP
		// client is most likely to retry (it is the conventional
		// "temporarily unavailable, try again" code) — the case most worth
		// proving isn't silently retried here.
		{"generic 5xx (503, non-BrokerResult body)", http.StatusServiceUnavailable, `{"error":{"code":"unavailable","message":"try again later"}}`},
		{"unparseable 200", http.StatusOK, "not json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var hits int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&hits, 1)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			client := NewHTTPRuntimeBrokerClient()
			_, err := client.ExecuteKeys(context.Background(), tid("broker-1"), server.URL, "test-agent", agentkeys.BrokerRequest{ExecuteBefore: time.Now().Add(time.Minute)})
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := agentkeys.ClassifyDispatchError(err); got != agentkeys.OutcomeKeysOutcomeUnknown {
				t.Fatalf("ClassifyDispatchError = %q, want %q (err=%v)", got, agentkeys.OutcomeKeysOutcomeUnknown, err)
			}
			if got := atomic.LoadInt32(&hits); got != 1 {
				t.Fatalf("expected exactly one dispatch attempt, got %d", got)
			}
		})
	}
}

// TestHTTPRuntimeBrokerClient_ExecuteKeys_ConnectionRefused proves a dial
// failure — provably no bytes ever reached the broker — classifies as
// agentkeys.ErrNotDispatched / OutcomeKeysUnavailable. (This test asserts
// classification only; it does not count dial attempts. A repeated dial to a
// refused port is not an application-level dispatch attempt in the sense
// ptone/scion#2194's AC3 cares about — see the hit-counter tests above and in
// controlchannel_client_keys_test.go for that proof at the application
// layer.)
func TestHTTPRuntimeBrokerClient_ExecuteKeys_ConnectionRefused(t *testing.T) {
	// Bind and immediately close a listener to obtain an address nothing is
	// listening on, guaranteeing a dial failure rather than a flaky "might
	// still be free" port guess.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to allocate a test port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("failed to close listener: %v", err)
	}

	client := NewHTTPRuntimeBrokerClient()
	_, err = client.ExecuteKeys(context.Background(), tid("broker-1"), "http://"+addr, "test-agent", agentkeys.BrokerRequest{ExecuteBefore: time.Now().Add(time.Minute)})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, agentkeys.ErrNotDispatched) {
		t.Fatalf("expected agentkeys.ErrNotDispatched, got %v", err)
	}
	if got := agentkeys.ClassifyDispatchError(err); got != agentkeys.OutcomeKeysUnavailable {
		t.Fatalf("ClassifyDispatchError = %q, want %q", got, agentkeys.OutcomeKeysUnavailable)
	}
}

// TestHTTPRuntimeBrokerClient_ExecuteKeys_ResponseLoss simulates a broker
// that accepts the connection, reads the request, then closes the connection
// without writing any response — "sent, response lost". This must NOT
// classify as ErrNotDispatched (the request was provably transmitted), and
// exactly one connection must be accepted (no reconnect/retry).
func TestHTTPRuntimeBrokerClient_ExecuteKeys_ResponseLoss(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	var acceptCount int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			atomic.AddInt32(&acceptCount, 1)
			// Read (part of) the request, then close without responding.
			reader := bufio.NewReader(conn)
			_, _ = reader.ReadString('\n')
			_ = conn.Close()
		}
	}()

	client := NewHTTPRuntimeBrokerClient()
	_, err = client.ExecuteKeys(context.Background(), tid("broker-1"), "http://"+ln.Addr().String(), "test-agent", agentkeys.BrokerRequest{ExecuteBefore: time.Now().Add(time.Minute)})
	if err == nil {
		t.Fatal("expected an error (response lost)")
	}
	if errors.Is(err, agentkeys.ErrNotDispatched) {
		t.Fatalf("response loss after the request was sent must not be reported as ErrNotDispatched, got %v", err)
	}
	if got := agentkeys.ClassifyDispatchError(err); got != agentkeys.OutcomeKeysOutcomeUnknown {
		t.Fatalf("ClassifyDispatchError = %q, want %q", got, agentkeys.OutcomeKeysOutcomeUnknown)
	}

	_ = ln.Close()
	<-done

	if got := atomic.LoadInt32(&acceptCount); got != 1 {
		t.Fatalf("expected exactly one connection attempt (no retry), got %d", got)
	}
}

// TestHTTPRuntimeBrokerClient_ExecuteKeys_NoRedirectReplay proves a keys
// dispatch never follows an HTTP redirect: a 3xx response is treated as the
// final (malformed) response, not a cue to resend the request body to the
// Location URL. Both endpoints are spied on so a regression that starts
// following redirects is caught even if it would have "succeeded". 307 and
// 308 are the status codes AK-34's replay concern is actually about — unlike
// 302/303, they preserve the method and body on a followed redirect, so a
// client that did follow them would transparently resend the keys POST to a
// new URL; a followed 302 would instead become a bodyless GET, which is a
// different (still wrong, but less on-point) failure mode. Both are guarded
// the same way: CheckRedirect refuses to follow, and GetBody == nil would
// prevent the resend even if something did follow.
func TestHTTPRuntimeBrokerClient_ExecuteKeys_NoRedirectReplay(t *testing.T) {
	cases := []struct {
		name   string
		status int
	}{
		{"302 Found", http.StatusFound},
		{"307 Temporary Redirect (preserves method/body)", http.StatusTemporaryRedirect},
		{"308 Permanent Redirect (preserves method/body)", http.StatusPermanentRedirect},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var targetCalls int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&targetCalls, 1)
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(agentkeys.BrokerResult{OperationID: "op-1", Outcome: agentkeys.OutcomeDispatched})
			}))
			defer target.Close()

			var redirectCalls int32
			redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&redirectCalls, 1)
				http.Redirect(w, r, target.URL+r.URL.Path+"?"+r.URL.RawQuery, tc.status)
			}))
			defer redirector.Close()

			client := NewHTTPRuntimeBrokerClient()
			_, err := client.ExecuteKeys(context.Background(), tid("broker-1"), redirector.URL, "test-agent", agentkeys.BrokerRequest{ExecuteBefore: time.Now().Add(time.Minute)})
			if err == nil {
				t.Fatal("expected an error: a bare 3xx is not a valid BrokerResult")
			}
			if got := agentkeys.ClassifyDispatchError(err); got != agentkeys.OutcomeKeysOutcomeUnknown {
				t.Fatalf("ClassifyDispatchError = %q, want %q", got, agentkeys.OutcomeKeysOutcomeUnknown)
			}
			if got := atomic.LoadInt32(&redirectCalls); got != 1 {
				t.Fatalf("expected exactly one call to the redirector, got %d", got)
			}
			if got := atomic.LoadInt32(&targetCalls); got != 0 {
				t.Fatalf("expected the redirect target to never be called (no replay), got %d calls", got)
			}
		})
	}
}

// TestAuthenticatedBrokerClient_ExecuteKeys_Signs proves the authenticated
// wrapper signs the outgoing keys request the same way it signs every other
// broker call, reusing the mockBrokerSigner spy defined in
// controlchannel_client_test.go.
func TestAuthenticatedBrokerClient_ExecuteKeys_Signs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(agentkeys.BrokerResult{OperationID: "op-1", Outcome: agentkeys.OutcomeDispatched})
	}))
	defer server.Close()

	signer := &mockBrokerSigner{}
	client := &AuthenticatedBrokerClient{transport: newBrokerHTTPTransport(false, signer)}
	_, err := client.ExecuteKeys(context.Background(), "broker-1", server.URL, "test-agent", agentkeys.BrokerRequest{OperationID: "op-1", ExecuteBefore: time.Now().Add(time.Minute)})
	if err != nil {
		t.Fatalf("ExecuteKeys failed: %v", err)
	}
	if !signer.called {
		t.Fatal("expected the signer to be invoked for a keys dispatch")
	}
}

// spyRoundTripper records whether the outgoing request had a non-nil GetBody
// and returns a canned successful keys response.
type spyRoundTripper struct {
	calls         int
	sawGetBodyNil bool
}

func (s *spyRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	s.calls++
	s.sawGetBodyNil = req.GetBody == nil
	body, _ := json.Marshal(agentkeys.BrokerResult{OperationID: "op-1", Outcome: agentkeys.OutcomeDispatched})
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Body:       io.NopCloser(bytes.NewReader(body)),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

// TestBrokerHTTPTransport_ExecuteKeys_ClearsGetBody is a regression test for
// a Go HTTP/2 transparent-resend hazard: http.NewRequestWithContext
// populates Request.GetBody for a []byte-backed reader, and net/http's HTTP/2
// transport resends a request with a non-nil GetBody on certain stream
// errors (golang/go#47635) even though the peer may already have started
// handling it — a replay this adapter's single-attempt contract must not
// allow. ExecuteKeys must clear GetBody before sending.
func TestBrokerHTTPTransport_ExecuteKeys_ClearsGetBody(t *testing.T) {
	spy := &spyRoundTripper{}
	transport := &brokerHTTPTransport{
		client:     &http.Client{Transport: spy},
		keysClient: &http.Client{Transport: spy},
	}

	_, err := transport.ExecuteKeys(context.Background(), "broker-1", "http://example.invalid", "test-agent", agentkeys.BrokerRequest{OperationID: "op-1", ExecuteBefore: time.Now().Add(time.Minute)})
	if err != nil {
		t.Fatalf("ExecuteKeys failed: %v", err)
	}
	if spy.calls != 1 {
		t.Fatalf("expected exactly one RoundTrip call, got %d", spy.calls)
	}
	if !spy.sawGetBodyNil {
		t.Fatal("expected the outgoing request's GetBody to be nil, to prevent an HTTP/2 transparent resend after a possible send (golang/go#47635)")
	}
}

// getBodyRepopulatingSigner simulates a future signer that rebuilds the
// request body (e.g. via http.NewRequest) and, in doing so, repopulates
// GetBody. Unlike mockBrokerSigner — which only sets headers and never
// touches GetBody, so it cannot tell whether the clear runs before or after
// Sign — this signer makes the ordering observable: if ExecuteKeys ever
// clears GetBody *before* calling Sign instead of after, this signer's
// repopulation would survive to the outgoing request and the regression test
// below would (correctly) fail.
type getBodyRepopulatingSigner struct {
	called bool
}

func (s *getBodyRepopulatingSigner) Sign(_ context.Context, req *http.Request, _ string) error {
	s.called = true
	req.GetBody = func() (io.ReadCloser, error) { return http.NoBody, nil }
	return nil
}

// TestBrokerHTTPTransport_ExecuteKeys_ClearsGetBodyOnSignedPath is the signed
// counterpart to TestBrokerHTTPTransport_ExecuteKeys_ClearsGetBody: it proves
// GetBody is still nil on the request that is actually sent even when the
// signer itself repopulates it, which only holds because ExecuteKeys clears
// GetBody *after* Sign runs, not before. A signer that only sets headers
// (like mockBrokerSigner) cannot distinguish the two orderings — this test
// uses getBodyRepopulatingSigner specifically so a future accidental
// reordering (clearing GetBody before Sign) is caught here, not left for a
// future signer implementation to rediscover in production.
func TestBrokerHTTPTransport_ExecuteKeys_ClearsGetBodyOnSignedPath(t *testing.T) {
	spy := &spyRoundTripper{}
	signer := &getBodyRepopulatingSigner{}
	transport := &brokerHTTPTransport{
		client:     &http.Client{Transport: spy},
		keysClient: &http.Client{Transport: spy},
		signer:     signer,
	}

	_, err := transport.ExecuteKeys(context.Background(), "broker-1", "http://example.invalid", "test-agent", agentkeys.BrokerRequest{OperationID: "op-1", ExecuteBefore: time.Now().Add(time.Minute)})
	if err != nil {
		t.Fatalf("ExecuteKeys failed: %v", err)
	}
	if !signer.called {
		t.Fatal("expected the signer to be invoked")
	}
	if spy.calls != 1 {
		t.Fatalf("expected exactly one RoundTrip call, got %d", spy.calls)
	}
	if !spy.sawGetBodyNil {
		t.Fatal("expected the outgoing request's GetBody to be nil even though the signer repopulated it — GetBody must be cleared after Sign runs, not before")
	}
}

// failingSigner always fails, for testing that a signing failure — proven to
// occur before anything is sent — classifies as agentkeys.ErrNotDispatched.
type failingSigner struct{}

func (failingSigner) Sign(context.Context, *http.Request, string) error {
	return errors.New("boom: no broker secret")
}

// TestBrokerHTTPTransport_ExecuteKeys_SignerFailureIsNotDispatched proves a
// signing failure (e.g. a missing/expired broker secret) is reported as
// agentkeys.ErrNotDispatched, and that the transport never attempts to send
// when signing fails.
func TestBrokerHTTPTransport_ExecuteKeys_SignerFailureIsNotDispatched(t *testing.T) {
	spy := &spyRoundTripper{}
	transport := &brokerHTTPTransport{
		client:     &http.Client{Transport: spy},
		keysClient: &http.Client{Transport: spy},
		signer:     failingSigner{},
	}

	_, err := transport.ExecuteKeys(context.Background(), "broker-1", "http://example.invalid", "test-agent", agentkeys.BrokerRequest{ExecuteBefore: time.Now().Add(time.Minute)})
	if !errors.Is(err, agentkeys.ErrNotDispatched) {
		t.Fatalf("expected agentkeys.ErrNotDispatched, got %v", err)
	}
	if spy.calls != 0 {
		t.Fatalf("expected zero HTTP calls when signing fails, got %d", spy.calls)
	}
}
