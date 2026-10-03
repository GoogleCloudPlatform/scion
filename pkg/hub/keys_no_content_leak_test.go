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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agentkeys"
)

// keysContentSentinel is a distinctive value used as the "keys" field across
// every test in this file. Per .design/agent-keys-contract.md §5 ("Never
// logged in a keys path, including debug logs and errors: the input
// itself... request JSON") and AK-32 ("captured logs contain no key content
// when searched for a distinctive test secret"), no error message or log
// line produced by any BrokerClient adapter may ever contain it.
const keysContentSentinel = "SENTINEL-KEYS-7f3a"

// installSentinelLogCapture points the slog default logger at an in-memory
// buffer, at Debug level so brokerHTTPTransport's debug-only logging is
// captured too, and restores the previous default logger on test cleanup.
func installSentinelLogCapture(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// assertNoSentinelLeak fails the test if err's message or the captured log
// buffer contains keysContentSentinel. It also asserts wantOutcome, so a
// sub-test that stopped reaching its intended error path (e.g. the redirect
// case began succeeding, or the signer case failed earlier at the endpoint
// check) fails here instead of silently passing on an empty error/log — a
// "no leak" assertion alone cannot distinguish "reached the path cleanly"
// from "never got there."
func assertNoSentinelLeak(t *testing.T, err error, log *bytes.Buffer, wantOutcome agentkeys.Outcome) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error (a nil error would make this leak check vacuous)")
	}
	if got := agentkeys.ClassifyDispatchError(err); got != wantOutcome {
		t.Fatalf("ClassifyDispatchError = %q, want %q (err=%v) — this sub-test may not have reached its intended path", got, wantOutcome, err)
	}
	if strings.Contains(err.Error(), keysContentSentinel) {
		t.Errorf("error message leaked key content: %v", err)
	}
	if strings.Contains(log.String(), keysContentSentinel) {
		t.Errorf("captured log leaked key content:\n%s", log.String())
	}
}

// assertReachedBrokerDo fails the test if the captured log does not contain
// the "Outgoing keys request to broker" debug line brokerHTTPTransport.
// ExecuteKeys emits right before keysClient.Do. Sub-tests that expect to
// reach that call use this to prove the log capture is actually live and
// scoped correctly, rather than trusting an empty buffer as proof of "no
// leak" when it could equally mean "captured nothing at all."
func assertReachedBrokerDo(t *testing.T, log *bytes.Buffer) {
	t.Helper()
	if !strings.Contains(log.String(), "Outgoing keys request to broker") {
		t.Fatalf("expected the captured log to contain the pre-send debug line, proving the log capture is live; got:\n%s", log.String())
	}
}

// assertNoLogOutput fails the test if the captured log buffer is non-empty.
// Used on ControlChannelBrokerClient.ExecuteKeys, which emits no log lines
// today: this turns "the log capture found nothing" from an unstated
// assumption into an enforced property, so a future log line added to this
// path is caught here (and, if it ever echoed key content, by
// assertNoSentinelLeak) rather than silently changing what "no leak" means
// for this transport.
func assertNoLogOutput(t *testing.T, log *bytes.Buffer) {
	t.Helper()
	if log.Len() != 0 {
		t.Fatalf("expected no log output from the control-channel adapter, got:\n%s", log.String())
	}
}

func sentinelKeysRequest() agentkeys.BrokerRequest {
	return agentkeys.BrokerRequest{
		OperationID:   "op-1",
		ProjectID:     "project-1",
		AgentID:       "agent-1",
		Keys:          keysContentSentinel,
		ExecuteBefore: time.Now().Add(time.Minute),
	}
}

// TestExecuteKeys_NoContentLeak_HTTP drives every HTTP-transport error path
// with a distinctive keys value, with debug logging enabled, and asserts
// neither the returned error nor the debug log contains it.
func TestExecuteKeys_NoContentLeak_HTTP(t *testing.T) {
	t.Run("signer failure", func(t *testing.T) {
		log := installSentinelLogCapture(t)
		client := &HTTPRuntimeBrokerClient{transport: &brokerHTTPTransport{
			client:     &http.Client{},
			keysClient: &http.Client{},
			signer:     failingSigner{},
			debug:      true,
		}}
		_, err := client.ExecuteKeys(context.Background(), "broker-1", "http://example.invalid", "test-agent", sentinelKeysRequest())
		assertNoSentinelLeak(t, err, log, agentkeys.OutcomeKeysUnavailable)
		// A signing failure is proven pre-send: the request never reaches
		// keysClient.Do, so the pre-send debug line must NOT appear either.
		if strings.Contains(log.String(), "Outgoing keys request to broker") {
			t.Fatal("a signer failure must be reported before the pre-send debug line, not after it")
		}
	})

	t.Run("5xx", func(t *testing.T) {
		log := installSentinelLogCapture(t)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":{"code":"internal","message":"boom"}}`))
		}))
		defer server.Close()
		client := &HTTPRuntimeBrokerClient{transport: newBrokerHTTPTransport(true, nil)}
		_, err := client.ExecuteKeys(context.Background(), tid("broker-1"), server.URL, "test-agent", sentinelKeysRequest())
		assertNoSentinelLeak(t, err, log, agentkeys.OutcomeKeysOutcomeUnknown)
		assertReachedBrokerDo(t, log)
	})

	t.Run("response loss", func(t *testing.T) {
		log := installSentinelLogCapture(t)
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("failed to listen: %v", err)
		}
		defer func() { _ = ln.Close() }()
		done := make(chan struct{})
		go func() {
			defer close(done)
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			_ = conn.Close()
		}()

		client := &HTTPRuntimeBrokerClient{transport: newBrokerHTTPTransport(true, nil)}
		_, dispatchErr := client.ExecuteKeys(context.Background(), tid("broker-1"), "http://"+ln.Addr().String(), "test-agent", sentinelKeysRequest())
		<-done
		assertNoSentinelLeak(t, dispatchErr, log, agentkeys.OutcomeKeysOutcomeUnknown)
		assertReachedBrokerDo(t, log)
	})

	t.Run("redirect", func(t *testing.T) {
		log := installSentinelLogCapture(t)
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(agentkeys.BrokerResult{OperationID: "op-1", Outcome: agentkeys.OutcomeDispatched})
		}))
		defer target.Close()
		redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+r.URL.Path+"?"+r.URL.RawQuery, http.StatusTemporaryRedirect)
		}))
		defer redirector.Close()

		client := &HTTPRuntimeBrokerClient{transport: newBrokerHTTPTransport(true, nil)}
		_, err := client.ExecuteKeys(context.Background(), tid("broker-1"), redirector.URL, "test-agent", sentinelKeysRequest())
		assertNoSentinelLeak(t, err, log, agentkeys.OutcomeKeysOutcomeUnknown)
		assertReachedBrokerDo(t, log)
	})

	t.Run("old broker 404", func(t *testing.T) {
		log := installSentinelLogCapture(t)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"Action not found"}}`))
		}))
		defer server.Close()

		client := &HTTPRuntimeBrokerClient{transport: newBrokerHTTPTransport(true, nil)}
		_, err := client.ExecuteKeys(context.Background(), tid("broker-1"), server.URL, "test-agent", sentinelKeysRequest())
		assertNoSentinelLeak(t, err, log, agentkeys.OutcomeKeysUnsupported)
		assertReachedBrokerDo(t, log)
	})

	t.Run("mismatched operation-ID echo", func(t *testing.T) {
		log := installSentinelLogCapture(t)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(agentkeys.BrokerResult{OperationID: "different-op", Outcome: agentkeys.OutcomeDispatched})
		}))
		defer server.Close()

		client := &HTTPRuntimeBrokerClient{transport: newBrokerHTTPTransport(true, nil)}
		_, err := client.ExecuteKeys(context.Background(), tid("broker-1"), server.URL, "test-agent", sentinelKeysRequest())
		assertNoSentinelLeak(t, err, log, agentkeys.OutcomeKeysOutcomeUnknown)
		assertReachedBrokerDo(t, log)
	})
}

// TestExecuteKeys_NoContentLeak_ControlChannel drives every control-channel
// error path with a distinctive keys value and asserts the returned error
// does not contain it. ControlChannelBrokerClient.ExecuteKeys itself emits no
// log lines (verified by reading it, and enforced here via assertNoLogOutput
// on every sub-test below), and this test replaces
// ControlChannelManager/BrokerConnection with mockControlChannelTunnel, so no
// real control-channel transport logging runs either way. The log capture is
// still installed and checked so a future log line added to this path — the
// regression this test exists to catch — cannot leak key content without
// being caught immediately.
func TestExecuteKeys_NoContentLeak_ControlChannel(t *testing.T) {
	t.Run("signer failure", func(t *testing.T) {
		log := installSentinelLogCapture(t)
		tunnel := &mockControlChannelTunnel{connected: true}
		client := &ControlChannelBrokerClient{manager: tunnel, signer: failingControlChannelSigner{}}
		_, err := client.ExecuteKeys(context.Background(), "broker-1", "unused", "test-agent", sentinelKeysRequest())
		assertNoSentinelLeak(t, err, log, agentkeys.OutcomeKeysUnavailable)
		assertNoLogOutput(t, log)
		if tunnel.calls != 0 {
			t.Fatalf("expected zero tunnel calls for a signer failure, got %d", tunnel.calls)
		}
	})

	t.Run("mid-flight failure", func(t *testing.T) {
		log := installSentinelLogCapture(t)
		tunnel := &mockControlChannelTunnel{connected: true, err: fmt.Errorf("tunnel closed: broker reconnecting")}
		client := &ControlChannelBrokerClient{manager: tunnel}
		_, err := client.ExecuteKeys(context.Background(), "broker-1", "unused", "test-agent", sentinelKeysRequest())
		assertNoSentinelLeak(t, err, log, agentkeys.OutcomeKeysOutcomeUnknown)
		assertNoLogOutput(t, log)
		if tunnel.calls != 1 {
			t.Fatalf("expected exactly one tunnel call, got %d", tunnel.calls)
		}
	})

	t.Run("5xx", func(t *testing.T) {
		log := installSentinelLogCapture(t)
		tunnel := &mockControlChannelTunnel{
			connected: true,
			status:    http.StatusBadGateway,
			body:      []byte(`{"error":{"code":"internal","message":"boom"}}`),
		}
		client := &ControlChannelBrokerClient{manager: tunnel}
		_, err := client.ExecuteKeys(context.Background(), "broker-1", "unused", "test-agent", sentinelKeysRequest())
		assertNoSentinelLeak(t, err, log, agentkeys.OutcomeKeysOutcomeUnknown)
		assertNoLogOutput(t, log)
		if tunnel.calls != 1 {
			t.Fatalf("expected exactly one tunnel call, got %d", tunnel.calls)
		}
	})

	t.Run("old broker 404", func(t *testing.T) {
		log := installSentinelLogCapture(t)
		tunnel := &mockControlChannelTunnel{
			connected: true,
			status:    http.StatusNotFound,
			body:      []byte(`{"error":{"code":"not_found","message":"Action not found"}}`),
		}
		client := &ControlChannelBrokerClient{manager: tunnel}
		_, err := client.ExecuteKeys(context.Background(), "broker-1", "unused", "test-agent", sentinelKeysRequest())
		assertNoSentinelLeak(t, err, log, agentkeys.OutcomeKeysUnsupported)
		assertNoLogOutput(t, log)
		if tunnel.calls != 1 {
			t.Fatalf("expected exactly one tunnel call, got %d", tunnel.calls)
		}
	})

	t.Run("mismatched operation-ID echo", func(t *testing.T) {
		log := installSentinelLogCapture(t)
		body, _ := json.Marshal(agentkeys.BrokerResult{OperationID: "different-op", Outcome: agentkeys.OutcomeDispatched})
		tunnel := &mockControlChannelTunnel{connected: true, status: http.StatusOK, body: body}
		client := &ControlChannelBrokerClient{manager: tunnel}
		_, err := client.ExecuteKeys(context.Background(), "broker-1", "unused", "test-agent", sentinelKeysRequest())
		assertNoSentinelLeak(t, err, log, agentkeys.OutcomeKeysOutcomeUnknown)
		assertNoLogOutput(t, log)
		if tunnel.calls != 1 {
			t.Fatalf("expected exactly one tunnel call, got %d", tunnel.calls)
		}
	})

	t.Run("oversized tunnel body", func(t *testing.T) {
		log := installSentinelLogCapture(t)
		tunnel := &mockControlChannelTunnel{connected: true}
		client := &ControlChannelBrokerClient{manager: tunnel}
		req := sentinelKeysRequest()
		req.Keys = keysContentSentinel + strings.Repeat("a", maxControlChannelBodySize)
		_, err := client.ExecuteKeys(context.Background(), "broker-1", "unused", "test-agent", req)
		assertNoSentinelLeak(t, err, log, agentkeys.OutcomeKeysUnavailable)
		assertNoLogOutput(t, log)
		if tunnel.calls != 0 {
			t.Fatalf("expected zero tunnel calls for an oversized payload, got %d", tunnel.calls)
		}
	})
}
