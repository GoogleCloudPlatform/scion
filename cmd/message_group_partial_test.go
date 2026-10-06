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

package cmd

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for ptone/scion#3510: a group[] send reports every recipient's
// outcome, exits exitCodeGroupPartial when only some recipients received the
// message, and reports the delivered set even when the send is interrupted.

// groupFakeHub serves per-agent message POSTs. outcomes maps an agent name
// to an HTTP status (200 = delivered). An agent mapped to -1 blocks until
// its request is cancelled, and signals blocked once it is in flight.
func groupFakeHub(t *testing.T, outcomes map[string]int, blocked chan<- struct{}) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/healthz" {
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
			return
		}
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/message") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		parts := strings.Split(strings.TrimSuffix(r.URL.Path, "/message"), "/")
		name := parts[len(parts)-1]
		code, ok := outcomes[name]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch code {
		case -1:
			// Drain the body so the server watches for the client going
			// away and cancels r.Context() when the CLI cancels.
			_, _ = io.Copy(io.Discard, r.Body)
			if blocked != nil {
				blocked <- struct{}{}
			}
			select {
			case <-r.Context().Done():
			case <-time.After(20 * time.Second):
			}
			return
		case http.StatusOK:
			_ = json.NewEncoder(w).Encode(map[string]string{
				"message_id": "m-" + name, "status": "delivered", "agent": name,
			})
		default:
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"error": map[string]string{"code": "send_failed", "message": "boom for " + name},
			})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func groupHubCtx(t *testing.T, srv *httptest.Server) *HubContext {
	t.Helper()
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	return &HubContext{Client: client, Endpoint: srv.URL, ProjectID: "project-group-3510"}
}

func agentRecipients(names ...string) []messages.GroupRecipient {
	out := make([]messages.GroupRecipient, len(names))
	for i, n := range names {
		out[i] = messages.GroupRecipient{Kind: messages.RecipientAgent, Name: n}
	}
	return out
}

func TestSendGroupMessage3510_PartialHumanOutputAndExitCode(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	srv := groupFakeHub(t, map[string]int{
		"agent-a": http.StatusOK,
		"agent-b": http.StatusInternalServerError,
		"agent-c": http.StatusOK,
	}, nil)
	hubCtx := groupHubCtx(t, srv)

	var sendErr error
	out := captureStdout(t, func() {
		sendErr = sendGroupMessageViaHub(hubCtx, agentRecipients("agent-a", "agent-b", "agent-c"), "hi", false)
	})

	require.Error(t, sendErr)
	assert.Equal(t, exitCodeGroupPartial, exitCodeFor(sendErr), "partial success must use the partial exit code")
	assert.Contains(t, sendErr.Error(), "group delivery partially failed: 2 delivered, 0 deferred, 1 failed (of 3 total)")
	assert.Contains(t, sendErr.Error(), "retry only group[agent:agent-b]")

	assert.Contains(t, out, "Group delivery incomplete: 2 delivered, 0 deferred, 1 failed (of 3 total).")
	assert.Contains(t, out, "Delivered (2): agent:agent-a, agent:agent-c\n", "summary must list the delivered set in input order; got:\n%s", out)
	assert.Contains(t, out, "Failed (1):\n  agent:agent-b: ")
	assert.Contains(t, out, "boom for agent-b", "the failure reason must be shown")
	assert.Contains(t, out, `send to "group[agent:agent-b]"`)
	assert.NotContains(t, out, "Group delivery complete", "an incomplete send must not be reported complete")
}

func TestSendGroupMessage3510_PartialJSONOutput(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()
	oldFormat := outputFormat
	outputFormat = "json"
	defer func() { outputFormat = oldFormat }()

	srv := groupFakeHub(t, map[string]int{
		"agent-a":  http.StatusOK,
		"agent-b":  http.StatusForbidden,
		"agent-gw": http.StatusGatewayTimeout,
	}, nil)
	hubCtx := groupHubCtx(t, srv)

	var sendErr error
	out := captureStdout(t, func() {
		sendErr = sendGroupMessageViaHub(hubCtx, agentRecipients("agent-a", "agent-b", "agent-gw"), "hi", false)
	})

	require.Error(t, sendErr)
	assert.Equal(t, exitCodeGroupPartial, exitCodeFor(sendErr))

	var got groupSendResult
	require.NoError(t, json.Unmarshal([]byte(out), &got), "stdout must be one JSON document; got:\n%s", out)
	assert.NotEmpty(t, got.GroupID)
	assert.Equal(t, 3, got.Total)
	assert.Equal(t, 1, got.Delivered)
	assert.Equal(t, 0, got.Deferred)
	assert.Equal(t, 1, got.Failed)
	assert.Equal(t, 1, got.Unknown)
	require.Len(t, got.Results, 3)
	assert.Equal(t, groupRecipientResult{Recipient: "agent:agent-a", Status: "delivered"}, got.Results[0])
	assert.Equal(t, "failed", got.Results[1].Status)
	assert.Contains(t, got.Results[1].Error, "boom for agent-b")
	assert.Equal(t, "unknown", got.Results[2].Status, "a gateway timeout does not prove the message was not delivered")
	assert.Contains(t, got.Results[2].Error, "may have been delivered")
	assert.Equal(t, "group[agent:agent-b]", got.RetryRecipient, "retry must name only definite failures")
}

func TestSendGroupMessage3510_TotalFailureExitsOne(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	srv := groupFakeHub(t, map[string]int{
		"agent-a": http.StatusInternalServerError,
		"agent-b": http.StatusNotFound,
	}, nil)
	hubCtx := groupHubCtx(t, srv)

	var sendErr error
	out := captureStdout(t, func() {
		sendErr = sendGroupMessageViaHub(hubCtx, agentRecipients("agent-a", "agent-b"), "hi", false)
	})

	require.Error(t, sendErr)
	assert.Equal(t, 1, exitCodeFor(sendErr), "a send that reached nobody exits 1")
	assert.Contains(t, sendErr.Error(), "group delivery failed: 0 delivered, 0 deferred, 2 failed (of 2 total)")
	assert.Contains(t, out, "Failed (2):")
	assert.NotContains(t, out, "Delivered (")
}

func TestSendGroupMessage3510_FullSuccessUnchanged(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	srv := groupFakeHub(t, map[string]int{"agent-a": http.StatusOK, "agent-b": http.StatusOK}, nil)
	hubCtx := groupHubCtx(t, srv)

	var sendErr error
	out := captureStdout(t, func() {
		sendErr = sendGroupMessageViaHub(hubCtx, agentRecipients("agent-a", "agent-b"), "hi", false)
	})

	require.NoError(t, sendErr)
	assert.Contains(t, out, "Group delivery complete: 2/2 delivered.")
	assert.NotContains(t, out, "Failed")
}

// An interrupt mid-send must not hide the recipients already delivered: the
// in-flight send is cancelled and reported unknown, the delivered set is
// still printed, and the exit code is the partial one.
func TestSendGroupMessage3510_InterruptStillReportsDelivered(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()

	blocked := make(chan struct{}, 1)
	srv := groupFakeHub(t, map[string]int{"agent-fast": http.StatusOK, "agent-slow": -1}, blocked)
	hubCtx := groupHubCtx(t, srv)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-blocked:
			// Let the fast recipient finish, then interrupt.
			time.Sleep(200 * time.Millisecond)
			cancel()
		case <-time.After(10 * time.Second):
		}
	}()

	start := time.Now()
	var sendErr error
	out := captureStdout(t, func() {
		sendErr = sendGroupMessageViaHubCtx(ctx, hubCtx, agentRecipients("agent-fast", "agent-slow"), "hi", false)
	})

	assert.Less(t, time.Since(start), 10*time.Second, "an interrupt must cancel in-flight sends promptly")
	require.Error(t, sendErr)
	assert.Equal(t, exitCodeGroupPartial, exitCodeFor(sendErr))
	assert.Contains(t, sendErr.Error(), "group delivery interrupted")
	assert.Contains(t, out, "Interrupted: in-flight sends were cancelled.")
	assert.Contains(t, out, "Delivered (1): agent:agent-fast")
	assert.Contains(t, out, "Unknown (may have been delivered) (1):\n  agent:agent-slow: no response from Hub")
}
