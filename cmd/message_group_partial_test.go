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
	"sync"
	"sync/atomic"
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

// groupOutcome is how the fake hub answers one recipient's send.
type groupOutcome struct {
	status     int    // HTTP status; 200 = delivered
	code       string // Hub error code for an error status (default send_failed)
	noEnvelope bool   // error with a plain-text body, as a proxy would send
	respStatus string // body "status" for a 2xx (default "delivered")
	block      bool   // block until the request is cancelled
}

func ok() groupOutcome                  { return groupOutcome{status: http.StatusOK} }
func hubErr(status int) groupOutcome    { return groupOutcome{status: status} }
func blockUntilCancelled() groupOutcome { return groupOutcome{block: true} }
func ambiguous() groupOutcome {
	return groupOutcome{status: http.StatusAccepted, respStatus: "ambiguous"}
}
func hubCode(s int, c string) groupOutcome { return groupOutcome{status: s, code: c} }

// groupFakeHub is a fake Hub for group sends. Agent recipients are keyed by
// name ("agent-a"), user recipients by wire recipient ("user:alice").
type groupFakeHub struct {
	srv      *httptest.Server
	outcomes map[string]groupOutcome
	// listAgents is returned by the project agent list (mention resolution).
	listAgents []string
	// blocked receives once a blocking recipient's request is in flight.
	blocked chan string
	// onRequest, if set, sees every request before it is answered.
	onRequest func(r *http.Request)

	mu       sync.Mutex
	requests []string // "METHOD path" (outbound sends: "POST outbound <recipient>")
}

func newGroupFakeHub(t *testing.T, outcomes map[string]groupOutcome) *groupFakeHub {
	t.Helper()
	h := &groupFakeHub{outcomes: outcomes, blocked: make(chan string, 8)}
	h.srv = httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *groupFakeHub) log(s string) {
	h.mu.Lock()
	h.requests = append(h.requests, s)
	h.mu.Unlock()
}

func (h *groupFakeHub) seen(s string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.requests {
		if strings.Contains(r, s) {
			return true
		}
	}
	return false
}

func (h *groupFakeHub) serve(w http.ResponseWriter, r *http.Request) {
	if h.onRequest != nil {
		h.onRequest(r)
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/healthz":
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		return
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/auth/me":
		h.log("GET me")
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "u1", "email": "tester@example.com"})
		return
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/agents"):
		h.log("GET agents")
		agents := make([]map[string]string, 0, len(h.listAgents))
		for _, a := range h.listAgents {
			agents = append(agents, map[string]string{"id": a, "name": a, "slug": a})
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"agents": agents})
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	var key string
	body, _ := io.ReadAll(r.Body)
	switch {
	case strings.HasSuffix(r.URL.Path, "/outbound-message"):
		var req struct {
			Recipient string `json:"recipient"`
		}
		_ = json.Unmarshal(body, &req)
		key = req.Recipient
		h.log("POST outbound " + key)
	case strings.HasSuffix(r.URL.Path, "/message"):
		parts := strings.Split(strings.TrimSuffix(r.URL.Path, "/message"), "/")
		key = parts[len(parts)-1]
		h.log("POST message " + key)
	default:
		w.WriteHeader(http.StatusNotFound)
		return
	}
	o, found := h.outcomes[key]
	if !found {
		// Unlisted recipients (for example a --cc mention target) succeed.
		o = ok()
	}
	if o.block {
		// The body has been read, so the server now watches for the client
		// going away and cancels r.Context() when the CLI cancels.
		h.blocked <- key
		select {
		case <-r.Context().Done():
		case <-time.After(20 * time.Second):
		}
		return
	}
	if o.status < 300 {
		st := o.respStatus
		if st == "" {
			st = "delivered"
		}
		w.WriteHeader(o.status)
		_ = json.NewEncoder(w).Encode(map[string]string{"message_id": "m-" + key, "status": st, "agent": key})
		return
	}
	if o.noEnvelope {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(o.status)
		_, _ = io.WriteString(w, "<html>bad gateway</html>")
		return
	}
	code := o.code
	if code == "" {
		code = "send_failed"
	}
	w.WriteHeader(o.status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]string{"code": code, "message": "boom for " + key},
	})
}

func (h *groupFakeHub) hubCtx(t *testing.T) *HubContext {
	t.Helper()
	client, err := hubclient.New(h.srv.URL)
	require.NoError(t, err)
	return &HubContext{Client: client, Endpoint: h.srv.URL, ProjectID: "project-group-3510"}
}

func agentRecipients(names ...string) []messages.GroupRecipient {
	out := make([]messages.GroupRecipient, len(names))
	for i, n := range names {
		out[i] = messages.GroupRecipient{Kind: messages.RecipientAgent, Name: n}
	}
	return out
}

// groupTestState isolates the package-level flags a group send reads.
func groupTestState(t *testing.T, format string) {
	t.Helper()
	orig := saveMessageTestState()
	oldFormat, oldCC := outputFormat, msgCC
	outputFormat, msgCC = format, nil
	t.Setenv("SCION_AGENT_NAME", "")
	t.Cleanup(func() {
		orig.restore()
		outputFormat, msgCC = oldFormat, oldCC
	})
}

// runGroupJSON sends with --format json through the production entry point
// and decodes the single JSON document written to stdout.
func runGroupJSON(t *testing.T, h *groupFakeHub, recips []messages.GroupRecipient) (groupSendResult, error) {
	t.Helper()
	var sendErr error
	out := captureStdout(t, func() {
		sendErr = sendGroupMessageViaHub(h.hubCtx(t), recips, "hi", false)
	})
	var got groupSendResult
	require.NoError(t, json.Unmarshal([]byte(out), &got), "stdout must be one JSON document; got:\n%s", out)
	return got, sendErr
}

func TestSendGroupMessage3510_PartialHumanOutputAndExitCode(t *testing.T) {
	groupTestState(t, "")
	h := newGroupFakeHub(t, map[string]groupOutcome{
		"agent-a": ok(),
		"agent-b": hubErr(http.StatusInternalServerError),
		"agent-c": ok(),
	})

	var sendErr error
	out := captureStdout(t, func() {
		sendErr = sendGroupMessageViaHub(h.hubCtx(t), agentRecipients("agent-a", "agent-b", "agent-c"), "hi", false)
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
	assert.NotContains(t, out, "Interrupted", "nothing was interrupted")
}

func TestSendGroupMessage3510_PartialJSONOutput(t *testing.T) {
	groupTestState(t, "json")
	h := newGroupFakeHub(t, map[string]groupOutcome{
		"agent-a":  ok(),
		"agent-b":  hubErr(http.StatusForbidden),
		"agent-gw": hubErr(http.StatusGatewayTimeout),
	})

	got, sendErr := runGroupJSON(t, h, agentRecipients("agent-a", "agent-b", "agent-gw"))

	require.Error(t, sendErr)
	assert.Equal(t, exitCodeGroupPartial, exitCodeFor(sendErr))
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

// The Hub answers 202 {"status":"ambiguous"} when it may or may not have
// dispatched the message. That is unknown, never delivered.
func TestSendGroupMessage3510_HubAmbiguousIsUnknown(t *testing.T) {
	groupTestState(t, "json")
	h := newGroupFakeHub(t, map[string]groupOutcome{
		"agent-a":   ok(),
		"agent-amb": ambiguous(),
	})

	got, sendErr := runGroupJSON(t, h, agentRecipients("agent-a", "agent-amb"))

	require.Error(t, sendErr, "an ambiguous recipient must not be reported as a complete send")
	assert.Equal(t, exitCodeGroupPartial, exitCodeFor(sendErr))
	assert.Equal(t, 1, got.Delivered)
	assert.Equal(t, 1, got.Unknown)
	require.Len(t, got.Results, 2)
	assert.Equal(t, "unknown", got.Results[1].Status)
	assert.Contains(t, got.Results[1].Error, "ambiguous (message m-agent-amb)")
	assert.Empty(t, got.RetryRecipient, "an ambiguous recipient may have the message and must not be retried")
}

// 502 has two meanings: the Hub's own delivery_failed is a definite failure
// (retryable), while a 502 runtime_error, or a 502/504 with no Hub error
// envelope (a proxy), says nothing about delivery.
func TestSendGroupMessage3510_GatewayErrorClassification(t *testing.T) {
	groupTestState(t, "json")
	h := newGroupFakeHub(t, map[string]groupOutcome{
		"agent-ok":       ok(),
		"agent-dfail":    hubCode(http.StatusBadGateway, "delivery_failed"),
		"agent-rt":       hubCode(http.StatusBadGateway, "runtime_error"),
		"agent-proxy502": {status: http.StatusBadGateway, noEnvelope: true},
		"agent-proxy504": {status: http.StatusGatewayTimeout, noEnvelope: true},
		"agent-btimeout": hubCode(http.StatusGatewayTimeout, "broker_timeout"),
	})
	names := []string{"agent-ok", "agent-dfail", "agent-rt", "agent-proxy502", "agent-proxy504", "agent-btimeout"}

	got, sendErr := runGroupJSON(t, h, agentRecipients(names...))

	require.Error(t, sendErr)
	assert.Equal(t, exitCodeGroupPartial, exitCodeFor(sendErr))
	want := []string{"delivered", "failed", "unknown", "unknown", "unknown", "unknown"}
	require.Len(t, got.Results, len(names))
	for i, w := range want {
		assert.Equal(t, w, got.Results[i].Status, "status for %s", names[i])
	}
	assert.Equal(t, "group[agent:agent-dfail]", got.RetryRecipient, "the Hub's definite delivery_failed must be retryable")
}

// When no recipient definitely failed but some may have the message, the
// whole send is not safe to retry, so it must exit 3, not 1.
func TestSendGroupMessage3510_AllUnknownExitsPartial(t *testing.T) {
	groupTestState(t, "json")
	h := newGroupFakeHub(t, map[string]groupOutcome{
		"agent-a": hubErr(http.StatusGatewayTimeout),
		"agent-b": ambiguous(),
	})

	got, sendErr := runGroupJSON(t, h, agentRecipients("agent-a", "agent-b"))

	require.Error(t, sendErr)
	assert.Equal(t, exitCodeGroupPartial, exitCodeFor(sendErr), "an all-unknown send must not exit 1 (safe to retry everything)")
	assert.Equal(t, got.Total, got.Unknown)
	assert.Empty(t, got.RetryRecipient)
	assert.Contains(t, sendErr.Error(), "do not resend to the whole group")
}

func TestSendGroupMessage3510_TotalFailureExitsOne(t *testing.T) {
	groupTestState(t, "")
	h := newGroupFakeHub(t, map[string]groupOutcome{
		"agent-a": hubErr(http.StatusInternalServerError),
		"agent-b": hubErr(http.StatusNotFound),
	})

	var sendErr error
	out := captureStdout(t, func() {
		sendErr = sendGroupMessageViaHub(h.hubCtx(t), agentRecipients("agent-a", "agent-b"), "hi", false)
	})

	require.Error(t, sendErr)
	assert.Equal(t, 1, exitCodeFor(sendErr), "a send that reached nobody exits 1")
	assert.Contains(t, sendErr.Error(), "group delivery failed: 0 delivered, 0 deferred, 2 failed (of 2 total)")
	assert.Contains(t, out, "Failed (2):")
	assert.NotContains(t, out, "Delivered (")
}

func TestSendGroupMessage3510_FullSuccessUnchanged(t *testing.T) {
	groupTestState(t, "")
	h := newGroupFakeHub(t, map[string]groupOutcome{"agent-a": ok(), "agent-b": ok()})

	var sendErr error
	out := captureStdout(t, func() {
		sendErr = sendGroupMessageViaHub(h.hubCtx(t), agentRecipients("agent-a", "agent-b"), "hi", false)
	})

	require.NoError(t, sendErr)
	assert.Contains(t, out, "Group delivery complete: 2/2 delivered.")
	assert.NotContains(t, out, "Failed")
}

// User recipients go through the outbound endpoint; their errors and the
// Hub's ambiguous answer are classified the same way as agent recipients.
func TestSendGroupMessage3510_UserRecipients(t *testing.T) {
	groupTestState(t, "json")
	t.Setenv("SCION_AGENT_NAME", "sender-agent")
	h := newGroupFakeHub(t, map[string]groupOutcome{
		"agent-a":    ok(),
		"user:alice": hubErr(http.StatusForbidden),
		"user:bob":   hubErr(http.StatusGatewayTimeout),
		"user:carol": ambiguous(),
	})
	recips := append(agentRecipients("agent-a"),
		messages.GroupRecipient{Kind: messages.RecipientUser, Name: "alice"},
		messages.GroupRecipient{Kind: messages.RecipientUser, Name: "bob"},
		messages.GroupRecipient{Kind: messages.RecipientUser, Name: "carol"},
	)

	got, sendErr := runGroupJSON(t, h, recips)

	require.Error(t, sendErr)
	assert.Equal(t, exitCodeGroupPartial, exitCodeFor(sendErr))
	require.Len(t, got.Results, 4)
	assert.Equal(t, "delivered", got.Results[0].Status)
	assert.Equal(t, groupRecipientResult{Recipient: "user:alice", Status: "failed", Error: got.Results[1].Error}, got.Results[1])
	assert.Contains(t, got.Results[1].Error, "boom for user:alice")
	assert.Equal(t, "unknown", got.Results[2].Status)
	assert.Equal(t, "unknown", got.Results[3].Status)
	assert.Equal(t, "group[user:alice]", got.RetryRecipient)
	assert.True(t, h.seen("POST outbound user:alice"), "user recipients must use the outbound endpoint")
}

// testFanOut is a groupSendHooks.fanOutContext that the test can cancel to
// simulate an interrupt, and that tracks whether the handler is installed.
type testFanOut struct {
	active atomic.Bool
	cancel context.CancelFunc
	ready  chan struct{}
}

func newTestFanOut() *testFanOut { return &testFanOut{ready: make(chan struct{})} }

func (f *testFanOut) hook(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	f.cancel = cancel
	f.active.Store(true)
	close(f.ready)
	return ctx, func() {
		f.active.Store(false)
		cancel()
	}
}

// An interrupt mid-send must not hide the recipients already delivered: the
// in-flight send is cancelled and reported unknown, the delivered set is
// still printed, the exit code is the partial one, and the --cc fan-out is
// skipped.
func TestSendGroupMessage3510_InterruptStillReportsDelivered(t *testing.T) {
	groupTestState(t, "")
	msgCC = []string{"agent-cc"}
	h := newGroupFakeHub(t, map[string]groupOutcome{"agent-fast": ok(), "agent-slow": blockUntilCancelled()})
	h.listAgents = []string{"agent-fast", "agent-slow", "agent-cc"}

	fan := newTestFanOut()
	fastRecorded := make(chan struct{})
	hooks := groupSendHooks{
		fanOutContext: fan.hook,
		onRecord: func(r groupRecipientResult) {
			if r.Recipient == "agent:agent-fast" {
				close(fastRecorded)
			}
		},
	}
	go func() {
		// Interrupt only once the fast result is recorded and the slow
		// request is in flight at the hub.
		<-fastRecorded
		<-h.blocked
		fan.cancel()
	}()

	start := time.Now()
	var sendErr error
	var out, errOut string
	out, errOut = captureStdoutStderr(t, func() {
		sendErr = sendGroupMessageViaHubCtx(h.hubCtx(t), agentRecipients("agent-fast", "agent-slow"), "hi", false, hooks)
	})

	assert.Less(t, time.Since(start), 10*time.Second, "an interrupt must cancel in-flight sends promptly")
	require.Error(t, sendErr)
	assert.Equal(t, exitCodeGroupPartial, exitCodeFor(sendErr))
	assert.Contains(t, sendErr.Error(), "group delivery interrupted")
	assert.Contains(t, out, "Interrupted: in-flight sends were cancelled.")
	assert.Contains(t, out, "Delivered (1): agent:agent-fast")
	assert.Contains(t, out, "Unknown (may have been delivered) (1):\n  agent:agent-slow: no response from Hub")
	assert.False(t, h.seen("GET agents"), "the mention fan-out must not run after an interrupt")
	assert.False(t, h.seen("POST message agent-cc"), "--cc recipients must not be messaged after an interrupt")
	assert.Contains(t, errOut, "@mention and --cc notifications were not sent")
}

// An interrupt before a request starts means it was never sent: it is a
// definite failure, so it is retryable, not unknown.
func TestSendGroupMessage3510_InterruptBeforeSendIsFailed(t *testing.T) {
	groupTestState(t, "json")
	h := newGroupFakeHub(t, map[string]groupOutcome{"agent-a": ok(), "agent-b": ok()})
	hooks := groupSendHooks{fanOutContext: func(parent context.Context) (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithCancel(parent)
		cancel() // the signal arrived before any send started
		return ctx, cancel
	}}

	var sendErr error
	out := captureStdout(t, func() {
		sendErr = sendGroupMessageViaHubCtx(h.hubCtx(t), agentRecipients("agent-a", "agent-b"), "hi", false, hooks)
	})

	var got groupSendResult
	require.NoError(t, json.Unmarshal([]byte(out), &got), "stdout must be one JSON document; got:\n%s", out)
	require.Error(t, sendErr)
	assert.Equal(t, 1, exitCodeFor(sendErr), "nothing was sent, so the whole send is safe to retry")
	assert.Equal(t, 2, got.Failed)
	assert.Contains(t, got.Results[0].Error, "not sent")
	assert.Equal(t, "group[agent:agent-a,agent:agent-b]", got.RetryRecipient)
	assert.False(t, h.seen("POST message"), "no request may be sent after the interrupt")
}

// The signal handler covers only the fan-out: the sender lookup before it
// and the mention fan-out after it run with default signal behaviour, so an
// interrupt there exits the process at once. A signal that arrives after
// every send has finished does not mark the send interrupted.
func TestSendGroupMessage3510_SignalHandlerScopedToFanOut(t *testing.T) {
	groupTestState(t, "")
	msgCC = []string{"agent-cc"}
	h := newGroupFakeHub(t, map[string]groupOutcome{"agent-a": ok(), "agent-b": ok()})
	h.listAgents = []string{"agent-a", "agent-b", "agent-cc"}

	fan := newTestFanOut()
	var mu sync.Mutex
	activeAt := map[string]bool{}
	h.onRequest = func(r *http.Request) {
		key := r.Method + " " + r.URL.Path
		switch {
		case strings.HasSuffix(r.URL.Path, "/auth/me"):
			key = "me"
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/agents"):
			key = "list"
		case strings.HasSuffix(r.URL.Path, "/agent-cc/message"):
			key = "cc"
		case strings.HasSuffix(r.URL.Path, "/message"):
			key = "group"
		}
		mu.Lock()
		activeAt[key] = fan.active.Load()
		mu.Unlock()
	}

	var sendErr error
	out := captureStdout(t, func() {
		sendErr = sendGroupMessageViaHubCtx(h.hubCtx(t), agentRecipients("agent-a", "agent-b"), "hi", false,
			groupSendHooks{fanOutContext: fan.hook})
	})
	require.NoError(t, sendErr)
	assert.Contains(t, out, "Group delivery complete: 2/2 delivered.")

	mu.Lock()
	defer mu.Unlock()
	require.Contains(t, activeAt, "me", "the sender lookup must reach the hub")
	require.Contains(t, activeAt, "group")
	require.Contains(t, activeAt, "list", "the --cc fan-out must run")
	require.Contains(t, activeAt, "cc")
	assert.False(t, activeAt["me"], "the signal handler must not be installed before the fan-out")
	assert.True(t, activeAt["group"], "the group sends must run under the signal handler")
	assert.False(t, activeAt["list"], "the signal handler must be removed before the mention fan-out")
	assert.False(t, activeAt["cc"], "the signal handler must be removed before the mention fan-out")
}

// A signal that lands after every send has finished cut nothing short, so the
// send is not reported interrupted, though the mention fan-out is skipped.
func TestSendGroupMessage3510_LateSignalNotReportedInterrupted(t *testing.T) {
	groupTestState(t, "")
	msgCC = []string{"agent-cc"}
	h := newGroupFakeHub(t, map[string]groupOutcome{"agent-a": ok(), "agent-b": ok()})
	h.listAgents = []string{"agent-a", "agent-b", "agent-cc"}

	fan := newTestFanOut()
	var recorded atomic.Int32
	hooks := groupSendHooks{
		fanOutContext: fan.hook,
		onRecord: func(groupRecipientResult) {
			if recorded.Add(1) == 2 {
				fan.cancel() // the signal arrives right after the last result
			}
		},
	}

	var sendErr error
	out, _ := captureStdoutStderr(t, func() {
		sendErr = sendGroupMessageViaHubCtx(h.hubCtx(t), agentRecipients("agent-a", "agent-b"), "hi", false, hooks)
	})

	require.NoError(t, sendErr)
	assert.Contains(t, out, "Group delivery complete: 2/2 delivered.")
	assert.NotContains(t, out, "Interrupted", "nothing was cut short")
	assert.False(t, h.seen("POST message agent-cc"), "the mention fan-out is skipped once a signal was received")
}
