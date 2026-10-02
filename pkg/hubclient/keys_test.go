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

package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agentkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAgentService_SendKeys_TopLevelRoute proves SendKeys from Client.Agents()
// posts to the top-level /api/v1/agents/{id}/keys route (not /message, and
// not a StructuredMessage body — see .design/agent-keys-contract.md §2.1/2.2).
func TestAgentService_SendKeys_TopLevelRoute(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/agents/agent-1/keys", r.URL.Path)

		var body agentkeys.Request
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, "C-c", body.Keys)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(agentkeys.Response{
			Status:      agentkeys.StatusDispatched,
			OperationID: "op-123",
			AgentID:     "agent-1",
		})
	}))
	defer server.Close()

	client, err := New(server.URL)
	require.NoError(t, err)

	resp, err := client.Agents().SendKeys(context.Background(), "agent-1", "C-c")
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, agentkeys.StatusDispatched, resp.Status)
	assert.Equal(t, "op-123", resp.OperationID)
	assert.Equal(t, "agent-1", resp.AgentID)
}

// TestAgentService_SendKeys_ProjectScopedRoute proves ProjectAgents(id)
// SendKeys posts to the project-scoped route shape with the identical body.
func TestAgentService_SendKeys_ProjectScopedRoute(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/projects/proj-42/agents/agent-1/keys", r.URL.Path)

		var body agentkeys.Request
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, "Enter", body.Keys)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(agentkeys.Response{
			Status:      agentkeys.StatusDispatched,
			OperationID: "op-456",
			AgentID:     "agent-1",
		})
	}))
	defer server.Close()

	client, err := New(server.URL)
	require.NoError(t, err)

	resp, err := client.ProjectAgents("proj-42").SendKeys(context.Background(), "agent-1", "Enter")
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, "op-456", resp.OperationID)
}

// TestAgentService_SendKeys_ErrorPreservesOutcomeAndOperationID proves a
// non-2xx response's outcome code (Code) and operation_id (Details) survive
// into the returned error unchanged — contract §2.4a/§2.5: "errors keep the
// outcome and operation ID whenever one was received."
func TestAgentService_SendKeys_ErrorPreservesOutcomeAndOperationID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict) // 409 agent_not_running
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]interface{}{
				"code":    "agent_not_running",
				"message": "target is not running",
				"details": map[string]interface{}{"operation_id": "op-789"},
			},
		})
	}))
	defer server.Close()

	client, err := New(server.URL)
	require.NoError(t, err)

	resp, err := client.Agents().SendKeys(context.Background(), "agent-1", "C-c")
	require.Error(t, err)
	assert.Nil(t, resp)

	apiErr, ok := err.(*apiclient.APIError)
	require.True(t, ok, "expected *apiclient.APIError, got %T", err)
	assert.Equal(t, "agent_not_running", apiErr.Code)
	assert.Equal(t, http.StatusConflict, apiErr.StatusCode)
	require.NotNil(t, apiErr.Details)
	assert.Equal(t, "op-789", apiErr.Details["operation_id"])
}

// TestAgentService_SendKeys_UnauthorizedCarriesNoOperationID proves a 401
// (OutcomeUnauthorized, decided by shared middleware before any keys handler
// runs — contract §2.5) is preserved as-is, with no operation_id fabricated.
func TestAgentService_SendKeys_UnauthorizedCarriesNoOperationID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]interface{}{
				"code":    "unauthorized",
				"message": "authentication required",
			},
		})
	}))
	defer server.Close()

	client, err := New(server.URL)
	require.NoError(t, err)

	_, err = client.Agents().SendKeys(context.Background(), "agent-1", "C-c")
	require.Error(t, err)
	apiErr, ok := err.(*apiclient.APIError)
	require.True(t, ok)
	assert.Equal(t, "unauthorized", apiErr.Code)
	assert.NotContains(t, apiErr.Details, "operation_id")
}

// TestAgentService_SendKeys_NetworkErrorNotTranslated proves a connection
// failure (no HTTP response received at all) comes back as a plain
// transport error, never reclassified into one of the keys outcome codes —
// the caller cannot know what happened and must not be told otherwise.
func TestAgentService_SendKeys_NetworkErrorNotTranslated(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		require.True(t, ok)
		conn, _, err := hj.Hijack()
		require.NoError(t, err)
		_ = conn.Close()
	}))
	defer server.Close()

	client, err := New(server.URL)
	require.NoError(t, err)

	resp, err := client.Agents().SendKeys(context.Background(), "agent-1", "C-c")
	require.Error(t, err)
	assert.Nil(t, resp)
	_, isAPIError := err.(*apiclient.APIError)
	assert.False(t, isAPIError, "a network-level failure must not be reported as a structured hub APIError")
}

// ---------------------------------------------------------------------------
// Binding obligation: AgentService.SendKeys must NEVER be replayed — not by
// WithRetry(>0), connection loss, 5xx, or redirects. Each test below counts
// server hits with the client configured in the way that would otherwise
// trigger a second attempt.
// ---------------------------------------------------------------------------

// TestSendKeys_NoReplay_WithRetryConfigured proves a client constructed with
// WithRetry(>0) still sends the keys request exactly once when the server
// returns a 5xx — the status Transport.Do itself would retry.
func TestSendKeys_NoReplay_WithRetryConfigured(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusBadGateway) // keys_outcome_unknown-shaped 502
	}))
	defer server.Close()

	client, err := New(server.URL, WithRetry(5, time.Millisecond))
	require.NoError(t, err)

	_, err = client.Agents().SendKeys(context.Background(), "agent-1", "C-c")
	require.Error(t, err)
	assert.EqualValues(t, 1, atomic.LoadInt32(&hits),
		"SendKeys must bypass WithRetry entirely: exactly one server hit expected")
}

// TestSendKeys_NoReplay_ConnectionLoss proves a connection dropped mid-request
// is not retried even with WithRetry configured.
// countingErrorRoundTripper is an http.RoundTripper that always fails,
// simulating a connection-level error, and counts invocations. Counting
// hits on a real httptest.Server (via a hijack-and-close handler) does not
// reliably distinguish "retried" from "not retried" — Transport.Do's retry
// loop reuses the same *http.Request, so a second attempt can resend an
// already-partially-consumed body that a real server observes differently
// depending on exact timing, letting a mutated (retrying) implementation
// pass undetected. Counting RoundTrip calls directly has no such gap.
type countingErrorRoundTripper struct {
	calls int32
	err   error
}

func (rt *countingErrorRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	atomic.AddInt32(&rt.calls, 1)
	return nil, rt.err
}

func TestSendKeys_NoReplay_ConnectionLoss(t *testing.T) {
	rt := &countingErrorRoundTripper{err: errors.New("simulated connection refused")}
	client, err := New("http://127.0.0.1:0",
		WithHTTPClient(&http.Client{Transport: rt}),
		WithRetry(5, time.Millisecond))
	require.NoError(t, err)

	_, err = client.Agents().SendKeys(context.Background(), "agent-1", "C-c")
	require.Error(t, err)
	assert.EqualValues(t, 1, atomic.LoadInt32(&rt.calls),
		"connection loss must not be replayed even with WithRetry configured: expected exactly 1 RoundTrip call")
}

// TestSendKeys_ConnectionLoss_WouldBeRetried_IfRoutedThroughPost is
// TestSendKeys_NoReplay_ConnectionLoss's mutation control: the same
// RoundTripper/assertion machinery, but going through the client's plain
// (retrying) post path, proves the counting approach actually distinguishes
// retried from not-retried — it must observe more than one call.
func TestSendKeys_ConnectionLoss_WouldBeRetried_IfRoutedThroughPost(t *testing.T) {
	rt := &countingErrorRoundTripper{err: errors.New("simulated connection refused")}
	c, err := New("http://127.0.0.1:0",
		WithHTTPClient(&http.Client{Transport: rt}),
		WithRetry(3, time.Millisecond))
	require.NoError(t, err)
	impl := c.(*client)

	_, err = impl.post(context.Background(), "/api/v1/agents/agent-1/message", map[string]string{"message": "hi"}, nil)
	require.Error(t, err)
	assert.EqualValues(t, 4, atomic.LoadInt32(&rt.calls),
		"the retrying post path should make 4 RoundTrip calls (1 original + 3 retries), proving this counting approach would have caught a SendKeys-through-post mutation")
}

// TestSendKeys_NoReplay_5xx proves a 5xx response is returned as-is on the
// first and only attempt (not retried internally), with the exact outcome
// code preserved.
func TestSendKeys_NoReplay_5xx(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]interface{}{
				"code":    "keys_outcome_unknown",
				"message": "dispatch outcome unknown",
				"details": map[string]interface{}{"operation_id": "op-502"},
			},
		})
	}))
	defer server.Close()

	client, err := New(server.URL, WithRetry(5, time.Millisecond))
	require.NoError(t, err)

	_, err = client.Agents().SendKeys(context.Background(), "agent-1", "C-c")
	require.Error(t, err)
	apiErr, ok := err.(*apiclient.APIError)
	require.True(t, ok)
	assert.Equal(t, "keys_outcome_unknown", apiErr.Code)
	assert.Equal(t, "op-502", apiErr.Details["operation_id"])
	assert.EqualValues(t, 1, atomic.LoadInt32(&hits), "a 5xx must not be retried for keys")
}

// TestSendKeys_NoReplay_RedirectNotFollowed proves a 3xx response from the
// keys route is returned unchanged rather than being followed and re-sent —
// the server must see exactly one hit even though it issued a redirect to a
// second, otherwise-reachable path on itself.
func TestSendKeys_NoReplay_RedirectNotFollowed(t *testing.T) {
	var hits int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/agents/agent-1/keys", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Redirect(w, r, "/api/v1/agents/agent-1/keys-moved", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/api/v1/agents/agent-1/keys-moved", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(agentkeys.Response{Status: agentkeys.StatusDispatched})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client, err := New(server.URL)
	require.NoError(t, err)

	_, err = client.Agents().SendKeys(context.Background(), "agent-1", "C-c")
	// A 307 with no parseable agentkeys.Response body does not decode as a
	// success: an error is expected, but the point under test is the hit
	// count, not this particular error's shape.
	require.Error(t, err)
	assert.EqualValues(t, 1, atomic.LoadInt32(&hits),
		"a redirect must never be followed/re-sent for keys: exactly one hit on the origin path expected")
}

// ---------------------------------------------------------------------------
// SendKeys must accept ONLY HTTP 200 with
// Status == agentkeys.StatusDispatched as success. Everything else below
// 400 — a 204, a 3xx carrying a body that happens to decode, a 2xx whose
// Status is something else — is a typed "unknown outcome" error, never a
// panic and never a false "dispatched".
// ---------------------------------------------------------------------------

// TestSendKeys_204NoContent_IsUnknownNotPanic proves a 204 (no body) does
// not crash SendKeys (a prior version dereferenced a nil *Response) and is
// reported as an unknown-outcome error, not success.
func TestSendKeys_204NoContent_IsUnknownNotPanic(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client, err := New(server.URL)
	require.NoError(t, err)

	var resp *agentkeys.Response
	require.NotPanics(t, func() {
		resp, err = client.Agents().SendKeys(context.Background(), "agent-1", "C-c")
	})
	require.Error(t, err)
	assert.Nil(t, resp)
	apiErr, ok := err.(*apiclient.APIError)
	require.True(t, ok, "expected *apiclient.APIError, got %T", err)
	assert.Equal(t, string(agentkeys.OutcomeKeysOutcomeUnknown), apiErr.Code)
}

// TestSendKeys_RedirectWithDecodableBody_IsUnknownNotSuccess proves a 3xx
// response whose body happens to decode as a well-formed agentkeys.Response
// (e.g. a misconfigured proxy relaying a stale 200 body under a 3xx status)
// is never reported as success: only HTTP 200 counts.
func TestSendKeys_RedirectWithDecodableBody_IsUnknownNotSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTemporaryRedirect)
		_ = json.NewEncoder(w).Encode(agentkeys.Response{
			Status:      agentkeys.StatusDispatched,
			OperationID: "op-redirect",
			AgentID:     "agent-1",
		})
	}))
	defer server.Close()

	client, err := New(server.URL)
	require.NoError(t, err)

	resp, err := client.Agents().SendKeys(context.Background(), "agent-1", "C-c")
	require.Error(t, err, "a 3xx must never be reported as success even with a decodable dispatched-shaped body")
	assert.Nil(t, resp)
	apiErr, ok := err.(*apiclient.APIError)
	require.True(t, ok, "expected *apiclient.APIError, got %T", err)
	assert.Equal(t, string(agentkeys.OutcomeKeysOutcomeUnknown), apiErr.Code)
}

// TestSendKeys_200WithWrongStatus_IsUnknownNotSuccess proves a 2xx response
// whose decoded Status is not "dispatched" is not treated as success.
func TestSendKeys_200WithWrongStatus_IsUnknownNotSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "queued"})
	}))
	defer server.Close()

	client, err := New(server.URL)
	require.NoError(t, err)

	resp, err := client.Agents().SendKeys(context.Background(), "agent-1", "C-c")
	require.Error(t, err)
	assert.Nil(t, resp)
	apiErr, ok := err.(*apiclient.APIError)
	require.True(t, ok, "expected *apiclient.APIError, got %T", err)
	assert.Equal(t, string(agentkeys.OutcomeKeysOutcomeUnknown), apiErr.Code)
}
