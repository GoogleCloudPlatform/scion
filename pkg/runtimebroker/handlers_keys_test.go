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

package runtimebroker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/agentkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// postKeys builds and sends a POST /api/v1/agents/{slug}/keys request against
// srv's handler and returns the recorded response.
func postKeys(t *testing.T, srv *Server, slug, projectIDQuery string, body agentkeys.BrokerRequest) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	url := "/api/v1/agents/" + slug + "/keys"
	if projectIDQuery != "" {
		url += "?projectId=" + projectIDQuery
	}
	req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

// postKeysRaw is postKeys for callers that need to control the exact raw
// request body bytes (oversize, unknown fields, trailing data) rather than
// marshaling a well-formed agentkeys.BrokerRequest.
func postKeysRaw(t *testing.T, srv *Server, slug, projectIDQuery string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	url := "/api/v1/agents/" + slug + "/keys"
	if projectIDQuery != "" {
		url += "?projectId=" + projectIDQuery
	}
	req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

func decodeBrokerResult(t *testing.T, w *httptest.ResponseRecorder) agentkeys.BrokerResult {
	t.Helper()
	var result agentkeys.BrokerResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("decoding BrokerResult from body %q: %v", w.Body.String(), err)
	}
	return result
}

// TestSendKeys_HTTP_Success covers the success path end to end through the
// HTTP mux: 200, agentkeys.OutcomeDispatched, the operation ID echoed back,
// and the handler passing the path slug, query projectID and body fields
// through to Manager.SendKeys unchanged.
func TestSendKeys_HTTP_Success(t *testing.T) {
	var gotProjectID, gotSlug, gotExpectedAgentID, gotKeys string
	mgr := &mockManager{
		sendKeysFunc: func(ctx context.Context, projectID, agentSlug, expectedAgentID, keys string) error {
			gotProjectID, gotSlug, gotExpectedAgentID, gotKeys = projectID, agentSlug, expectedAgentID, keys
			return nil
		},
	}
	srv := newTestServerWithManager(t, mgr)

	reqBody := agentkeys.BrokerRequest{
		ProjectID:     "proj-1",
		AgentID:       "agent-abc",
		OperationID:   "op-123",
		ExecuteBefore: time.Now().UTC().Add(10 * time.Second),
		Keys:          "C-c",
	}
	w := postKeys(t, srv, "test-agent", "proj-1", reqBody)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	result := decodeBrokerResult(t, w)
	if result.Outcome != agentkeys.OutcomeDispatched {
		t.Errorf("Outcome = %q, want %q", result.Outcome, agentkeys.OutcomeDispatched)
	}
	if result.OperationID != "op-123" {
		t.Errorf("OperationID = %q, want %q", result.OperationID, "op-123")
	}

	if gotProjectID != "proj-1" || gotSlug != "test-agent" || gotExpectedAgentID != "agent-abc" || gotKeys != "C-c" {
		t.Errorf("SendKeys called with (%q,%q,%q,%q), want (proj-1,test-agent,agent-abc,C-c)",
			gotProjectID, gotSlug, gotExpectedAgentID, gotKeys)
	}
}

// TestSendKeys_HTTP_OutcomeMapping covers that each agentkeys sentinel
// SendKeys can return maps to the exact HTTP status and Outcome the frozen
// contract table assigns it.
func TestSendKeys_HTTP_OutcomeMapping(t *testing.T) {
	cases := []struct {
		name       string
		sendErr    error
		wantStatus int
		wantOut    agentkeys.Outcome
		wantMsg    string // empty means "don't check"
	}{
		{"not_found", agentkeys.ErrTargetNotFound, http.StatusNotFound, agentkeys.OutcomeNotFound, ""},
		{"agent_not_running", agentkeys.ErrAgentNotRunning, http.StatusConflict, agentkeys.OutcomeAgentNotRunning, ""},
		{"terminal_not_ready", agentkeys.ErrTerminalNotReady, http.StatusConflict, agentkeys.OutcomeTerminalNotReady, ""},
		{"unsupported_backend", agent.ErrKeysUnsupported, http.StatusUnprocessableEntity, agentkeys.OutcomeKeysUnsupported, ""},
		// This is the contract §4.3 row "error
		// wrapping agent.ErrKeysNotStarted -> BrokerResult{Outcome:
		// OutcomeKeysUnavailable}, HTTP 503": disabling the handler's
		// ErrKeysNotStarted case leaves every other keys-route test green,
		// so this mapping needs its own row.
		{"keys_not_started", fmt.Errorf("%w: %v", agent.ErrKeysNotStarted, context.DeadlineExceeded), http.StatusServiceUnavailable, agentkeys.OutcomeKeysUnavailable, "admission deadline expired before dispatch"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr := &mockManager{
				sendKeysFunc: func(ctx context.Context, projectID, agentSlug, expectedAgentID, keys string) error {
					return tc.sendErr
				},
			}
			srv := newTestServerWithManager(t, mgr)

			w := postKeys(t, srv, "test-agent", "proj-1", agentkeys.BrokerRequest{
				ProjectID:     "proj-1",
				AgentID:       "agent-abc",
				OperationID:   "op-1",
				ExecuteBefore: time.Now().UTC().Add(10 * time.Second),
				Keys:          "Enter",
			})

			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", w.Code, tc.wantStatus, w.Body.String())
			}
			result := decodeBrokerResult(t, w)
			if result.Outcome != tc.wantOut {
				t.Errorf("Outcome = %q, want %q", result.Outcome, tc.wantOut)
			}
			if result.OperationID != "op-1" {
				t.Errorf("OperationID = %q, want %q", result.OperationID, "op-1")
			}
			if tc.wantMsg != "" && result.Message != tc.wantMsg {
				t.Errorf("Message = %q, want %q", result.Message, tc.wantMsg)
			}
		})
	}
}

// TestSendKeys_HTTP_PostExecCtxErrorIsAmbiguousNot503 is the broker-level
// regression test for a Manager.SendKeys failure that
// wraps a context error (e.g. a backend's Exec observing cancellation after
// it had genuinely started) must classify as the generic, non-BrokerResult
// envelope (which the Hub reads as keys_outcome_unknown) — never 503
// keys_unavailable, which would tell a caller it is safe to retry a
// dispatch that may have already reached the terminal.
func TestSendKeys_HTTP_PostExecCtxErrorIsAmbiguousNot503(t *testing.T) {
	mgr := &mockManager{
		sendKeysFunc: func(ctx context.Context, projectID, agentSlug, expectedAgentID, keys string) error {
			return fmt.Errorf("exec stream: %w", context.DeadlineExceeded)
		},
	}
	srv := newTestServerWithManager(t, mgr)

	w := postKeys(t, srv, "test-agent", "proj-1", agentkeys.BrokerRequest{
		ProjectID:     "proj-1",
		AgentID:       "agent-abc",
		OperationID:   "op-1",
		ExecuteBefore: time.Now().UTC().Add(10 * time.Second),
		Keys:          "Enter",
	})

	if w.Code == http.StatusServiceUnavailable {
		t.Fatalf("a post-Exec failure wrapping a context error must not be reported as 503 keys_unavailable; body = %s", w.Body.String())
	}
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (the generic non-BrokerResult envelope); body = %s", w.Code, w.Body.String())
	}
	var result agentkeys.BrokerResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err == nil && result.Outcome != "" {
		t.Errorf("must not decode as a well-formed BrokerResult with a non-empty Outcome, got %+v", result)
	}
}

// TestSendKeys_HTTP_ExpiredDeadlineAtAdmission covers "at broker admission":
// an already-past ExecuteBefore must be rejected before Manager.SendKeys is
// ever called, with 503 keys_unavailable.
func TestSendKeys_HTTP_ExpiredDeadlineAtAdmission(t *testing.T) {
	called := false
	mgr := &mockManager{
		sendKeysFunc: func(ctx context.Context, projectID, agentSlug, expectedAgentID, keys string) error {
			called = true
			return nil
		},
	}
	srv := newTestServerWithManager(t, mgr)

	w := postKeys(t, srv, "test-agent", "proj-1", agentkeys.BrokerRequest{
		ProjectID:     "proj-1",
		AgentID:       "agent-abc",
		OperationID:   "op-1",
		ExecuteBefore: time.Now().UTC().Add(-1 * time.Second),
		Keys:          "Enter",
	})

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", w.Code, w.Body.String())
	}
	result := decodeBrokerResult(t, w)
	if result.Outcome != agentkeys.OutcomeKeysUnavailable {
		t.Errorf("Outcome = %q, want %q", result.Outcome, agentkeys.OutcomeKeysUnavailable)
	}
	if called {
		t.Error("Manager.SendKeys must not be called once the admission deadline has already passed")
	}
}

// TestSendKeys_HTTP_MissingDeadline covers the same admission gate for a
// zero ExecuteBefore (a missing/invalid internal deadline) — fail closed,
// never invent a fallback window at the broker.
func TestSendKeys_HTTP_MissingDeadline(t *testing.T) {
	called := false
	mgr := &mockManager{
		sendKeysFunc: func(ctx context.Context, projectID, agentSlug, expectedAgentID, keys string) error {
			called = true
			return nil
		},
	}
	srv := newTestServerWithManager(t, mgr)

	w := postKeys(t, srv, "test-agent", "proj-1", agentkeys.BrokerRequest{
		ProjectID:   "proj-1",
		AgentID:     "agent-abc",
		OperationID: "op-1",
		Keys:        "Enter",
		// ExecuteBefore intentionally zero.
	})

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", w.Code, w.Body.String())
	}
	if called {
		t.Error("Manager.SendKeys must not be called with a missing execute-before deadline")
	}
}

// TestSendKeys_HTTP_AmbiguousFailureIsGenericEnvelope covers "any execution
// error with uncertain terminal effect is represented as unknown, not
// definitely undelivered": an unclassified SendKeys failure must not be
// reported as a well-formed BrokerResult at all (which would tempt a
// Hub-side adapter to trust an outcome the broker never decided) — it must
// be the ordinary, non-BrokerResult error envelope, so the adapter's
// documented "any other malformed/disagreeing response" rule classifies it
// as keys_outcome_unknown.
func TestSendKeys_HTTP_AmbiguousFailureIsGenericEnvelope(t *testing.T) {
	mgr := &mockManager{
		sendKeysFunc: func(ctx context.Context, projectID, agentSlug, expectedAgentID, keys string) error {
			return errors.New("tmux: some ambiguous failure")
		},
	}
	srv := newTestServerWithManager(t, mgr)

	w := postKeys(t, srv, "test-agent", "proj-1", agentkeys.BrokerRequest{
		ProjectID:     "proj-1",
		AgentID:       "agent-abc",
		OperationID:   "op-1",
		ExecuteBefore: time.Now().UTC().Add(10 * time.Second),
		Keys:          "Enter",
	})

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body = %s", w.Code, w.Body.String())
	}
	var result agentkeys.BrokerResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err == nil && result.Outcome != "" {
		t.Errorf("ambiguous failure must not decode as a well-formed BrokerResult with a non-empty Outcome, got %+v", result)
	}
}

// TestSendKeys_HTTP_MethodNotAllowed covers that the route only accepts
// POST, matching api.RuntimeBrokerAgentActionMethod's registration for
// AgentActionKeys.
func TestSendKeys_HTTP_MethodNotAllowed(t *testing.T) {
	srv := newTestServerWithManager(t, &mockManager{})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/test-agent/keys", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405; body = %s", w.Code, w.Body.String())
	}
}

// spanText concatenates every string an OTel span exposes that could carry
// free-form content: its name, every attribute key/value (on the span and on
// each event), every event name, and the status description. Used to search
// for a distinctive secret across the whole span rather than guessing which
// field a leak would land in.
func spanText(s sdktrace.ReadOnlySpan) string {
	var b strings.Builder
	b.WriteString(s.Name())
	for _, kv := range s.Attributes() {
		b.WriteString(string(kv.Key))
		b.WriteString(kv.Value.String())
	}
	for _, ev := range s.Events() {
		b.WriteString(ev.Name)
		for _, kv := range ev.Attributes {
			b.WriteString(string(kv.Key))
			b.WriteString(kv.Value.String())
		}
	}
	b.WriteString(s.Status().Description)
	return b.String()
}

// TestSendKeys_HTTP_NoLeakOfDistinctiveSecret is the log-capture test: it
// sends a distinctive secret as the keys payload and asserts it appears
// nowhere in the captured debug/info/warn log output, the HTTP response
// body, or any recorded OTel span, across every outcome path (success, each
// sentinel, keys_unsupported, and an ambiguous failure whose error text
// might otherwise be logged). A positive control (asserting the capture
// mechanism actually saw content at all) guards against the test passing
// vacuously if some other change silently stopped logging or tracing
// anything.
func TestSendKeys_HTTP_NoLeakOfDistinctiveSecret(t *testing.T) {
	const secret = "AK-SENTINEL-do-not-leak-8f3c1e9b"

	var buf bytes.Buffer
	origWriter := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(origWriter)
		log.SetFlags(origFlags)
	})
	origLevel := slog.SetLogLoggerLevel(slog.LevelDebug)
	t.Cleanup(func() { slog.SetLogLoggerLevel(origLevel) })

	// Record every span broker.keys.inject produces during this test. The
	// package's "tracer" var (otel.Tracer("scion-broker")) resolves against
	// whatever TracerProvider is current *at span-start time*, not the one
	// current when the var was created, so swapping the global provider here
	// takes effect for spans started after this point.
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	origTP := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(origTP) })

	// assertNoLeak applies the same log/body/span/positive-control checks
	// used by every case below.
	assertNoLeak := func(name string, w *httptest.ResponseRecorder) {
		t.Helper()
		logOutput := buf.String()
		if strings.Contains(logOutput, secret) {
			t.Errorf("%s: captured log output contains the distinctive secret\nLog: %s", name, logOutput)
		}
		// Positive control: the audit line must actually have been written
		// and captured, so an accidental logger swap elsewhere in the
		// package can't make this test pass by writing nothing at all.
		if !strings.Contains(logOutput, "keys dispatch") {
			t.Errorf("%s: expected the audit log line ('keys dispatch...') to be captured, got: %s", name, logOutput)
		}

		if strings.Contains(w.Body.String(), secret) {
			t.Errorf("%s: HTTP response body contains the distinctive secret\nBody: %s", name, w.Body.String())
		}

		ended := recorder.Ended()
		if len(ended) == 0 {
			t.Errorf("%s: expected at least one recorded span", name)
		}
		for _, span := range ended {
			if strings.Contains(spanText(span), secret) {
				t.Errorf("%s: span %q contains the distinctive secret", name, span.Name())
			}
		}
	}

	outcomes := []error{
		nil,
		agentkeys.ErrTargetNotFound,
		agentkeys.ErrAgentNotRunning,
		agentkeys.ErrTerminalNotReady,
		agent.ErrKeysUnsupported,
		fmt.Errorf("%w: %v", agent.ErrKeysNotStarted, context.DeadlineExceeded), // proven not started, wrapping a context error
		errors.New("tmux: failed on " + secret),                                 // a buggy manager that leaked the secret into its error
	}

	for i, sendErr := range outcomes {
		mgr := &mockManager{
			sendKeysFunc: func(ctx context.Context, projectID, agentSlug, expectedAgentID, keys string) error {
				return sendErr
			},
		}
		srv := newTestServerWithManager(t, mgr)

		buf.Reset()
		recorder.Reset()
		w := postKeys(t, srv, "test-agent", "proj-1", agentkeys.BrokerRequest{
			ProjectID:     "proj-1",
			AgentID:       "agent-abc",
			OperationID:   "op-1",
			ExecuteBefore: time.Now().UTC().Add(10 * time.Second),
			Keys:          secret,
		})

		assertNoLeak(fmt.Sprintf("outcome case %d", i), w)
	}

	// The ValidateKeys rejection path (and its
	// audit line) is not exercised by the loop above, since every case
	// there reaches Manager.SendKeys — a NUL byte makes the keys value
	// itself invalid, rejected before SendKeys is ever called.
	t.Run("validation_rejection", func(t *testing.T) {
		srv := newTestServerWithManager(t, &mockManager{})

		buf.Reset()
		recorder.Reset()
		w := postKeys(t, srv, "test-agent", "proj-1", agentkeys.BrokerRequest{
			ProjectID:     "proj-1",
			AgentID:       "agent-abc",
			OperationID:   "op-1",
			ExecuteBefore: time.Now().UTC().Add(10 * time.Second),
			Keys:          secret + "\x00",
		})

		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", w.Code, w.Body.String())
		}
		assertNoLeak("validation_rejection", w)
	})
}

// spyLogHandler is a minimal slog.Handler that only counts Handle calls, so
// tests can prove a specific *slog.Logger was never written to — swapping it
// in for Server.messageLog/dedicatedMessageLog directly, rather than relying
// on global log capture, isolates the assertion to that one logger.
type spyLogHandler struct {
	records int
}

func (h *spyLogHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *spyLogHandler) Handle(context.Context, slog.Record) error {
	h.records++
	return nil
}
func (h *spyLogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *spyLogHandler) WithGroup(string) slog.Handler      { return h }

// TestSendKeys_HTTP_NeverWritesMessageLog covers
// C§1: "removes the message logger from the keys path entirely": unlike
// sendMessage, the keys handler must never write to the broker.messages
// subsystem logger (messageLog) or its dedicated-log override
// (dedicatedMessageLog), for every outcome shape — success, every sentinel,
// keys_unsupported, and an ambiguous failure.
func TestSendKeys_HTTP_NeverWritesMessageLog(t *testing.T) {
	outcomes := []error{
		nil,
		agentkeys.ErrTargetNotFound,
		agentkeys.ErrAgentNotRunning,
		agentkeys.ErrTerminalNotReady,
		agent.ErrKeysUnsupported,
		fmt.Errorf("%w: %v", agent.ErrKeysNotStarted, context.DeadlineExceeded),
		errors.New("tmux: some ambiguous failure"),
	}

	for i, sendErr := range outcomes {
		t.Run(fmt.Sprintf("outcome_%d", i), func(t *testing.T) {
			mgr := &mockManager{
				sendKeysFunc: func(ctx context.Context, projectID, agentSlug, expectedAgentID, keys string) error {
					return sendErr
				},
			}
			srv := newTestServerWithManager(t, mgr)

			messageLogSpy := &spyLogHandler{}
			srv.messageLog = slog.New(messageLogSpy)
			dedicatedLogSpy := &spyLogHandler{}
			srv.dedicatedMessageLog = slog.New(dedicatedLogSpy)

			postKeys(t, srv, "test-agent", "proj-1", agentkeys.BrokerRequest{
				ProjectID:     "proj-1",
				AgentID:       "agent-abc",
				OperationID:   "op-1",
				ExecuteBefore: time.Now().UTC().Add(10 * time.Second),
				Keys:          "Enter",
			})

			if messageLogSpy.records != 0 {
				t.Errorf("messageLog received %d record(s); the keys path must never write to it", messageLogSpy.records)
			}
			if dedicatedLogSpy.records != 0 {
				t.Errorf("dedicatedMessageLog received %d record(s); the keys path must never write to it", dedicatedLogSpy.records)
			}
		})
	}

	// Also cover the pre-admission validation-rejection path (never reaches
	// Manager.SendKeys at all), and the oversize-body 413 path.
	t.Run("validation_rejection", func(t *testing.T) {
		srv := newTestServerWithManager(t, &mockManager{})
		messageLogSpy := &spyLogHandler{}
		srv.messageLog = slog.New(messageLogSpy)
		dedicatedLogSpy := &spyLogHandler{}
		srv.dedicatedMessageLog = slog.New(dedicatedLogSpy)

		postKeys(t, srv, "test-agent", "proj-1", agentkeys.BrokerRequest{
			ProjectID:     "proj-1",
			AgentID:       "agent-abc",
			OperationID:   "op-1",
			ExecuteBefore: time.Now().UTC().Add(10 * time.Second),
			Keys:          "abc\x00def",
		})

		if messageLogSpy.records != 0 {
			t.Errorf("messageLog received %d record(s) on a validation rejection; must never write to it", messageLogSpy.records)
		}
		if dedicatedLogSpy.records != 0 {
			t.Errorf("dedicatedMessageLog received %d record(s) on a validation rejection; must never write to it", dedicatedLogSpy.records)
		}
	})

	// Positive control. None of the
	// zero-record assertions above prove the spy logger is actually capable
	// of capturing anything -- a broken swap (e.g. the spy installed on the
	// wrong field) would silently report zero records regardless. Sending
	// an ordinary message through the same server's /message route (which
	// does write to messageLog, unlike keys) confirms the spy is live.
	t.Run("positive_control_message_path_writes", func(t *testing.T) {
		srv := newTestServerWithManager(t, &mockManager{})
		messageLogSpy := &spyLogHandler{}
		srv.messageLog = slog.New(messageLogSpy)

		body, err := json.Marshal(MessageRequest{Message: "hello"})
		if err != nil {
			t.Fatalf("marshal message request: %v", err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent/message", bytes.NewReader(body))
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
		}
		if messageLogSpy.records == 0 {
			t.Fatal("positive control failed: expected the ordinary message path to write to messageLog, got zero records -- the spy swap itself may be broken")
		}
	})
}

// TestSendKeys_HTTP_UnsupportedBackend covers the case where a manager
// whose SendKeys reports the backend does not support keys delivery must
// produce 422 keys_unsupported, with the response message stating only that
// fact.
func TestSendKeys_HTTP_UnsupportedBackend(t *testing.T) {
	mgr := &mockManager{
		sendKeysFunc: func(ctx context.Context, projectID, agentSlug, expectedAgentID, keys string) error {
			return agent.ErrKeysUnsupported
		},
	}
	srv := newTestServerWithManager(t, mgr)

	w := postKeys(t, srv, "test-agent", "proj-1", agentkeys.BrokerRequest{
		ProjectID:     "proj-1",
		AgentID:       "agent-abc",
		OperationID:   "op-1",
		ExecuteBefore: time.Now().UTC().Add(10 * time.Second),
		Keys:          "Enter",
	})

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body = %s", w.Code, w.Body.String())
	}
	result := decodeBrokerResult(t, w)
	if result.Outcome != agentkeys.OutcomeKeysUnsupported {
		t.Errorf("Outcome = %q, want %q", result.Outcome, agentkeys.OutcomeKeysUnsupported)
	}
	if result.Message != "this backend does not support keys delivery" {
		t.Errorf("Message = %q, want the fixed, mechanism-free string", result.Message)
	}
}

// TestSendKeys_HTTP_BodyTooLarge covers the case where a request body
// larger than agentkeys.MaxHTTPBodyBytes must be rejected by the
// transport-level read, at 413, before Manager.SendKeys is ever called.
func TestSendKeys_HTTP_BodyTooLarge(t *testing.T) {
	called := false
	mgr := &mockManager{
		sendKeysFunc: func(ctx context.Context, projectID, agentSlug, expectedAgentID, keys string) error {
			called = true
			return nil
		},
	}
	srv := newTestServerWithManager(t, mgr)

	// A "keys" value comfortably within agentkeys.MaxBytes, padded with an
	// oversized unknown field so the overall body exceeds MaxHTTPBodyBytes.
	// The oversize check must fire from the bounded read itself, before JSON
	// decoding ever inspects field names.
	pad := strings.Repeat("x", agentkeys.MaxHTTPBodyBytes+1024)
	body := fmt.Sprintf(`{"project_id":"proj-1","agent_id":"agent-abc","operation_id":"op-1","execute_before":"2099-01-01T00:00:00Z","keys":"C-c","padding":"%s"}`, pad)

	w := postKeysRaw(t, srv, "test-agent", "proj-1", []byte(body))

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body = %s", w.Code, w.Body.String())
	}
	if called {
		t.Error("Manager.SendKeys must not be called for an oversize request body")
	}
}

// TestSendKeys_HTTP_MalformedBodyRejected covers the rest of the malformed-
// body requirement: readKeysRequest must reject unknown fields and trailing
// content that the bare readJSON it replaced would have accepted.
func TestSendKeys_HTTP_MalformedBodyRejected(t *testing.T) {
	validExecuteBefore := `"2099-01-01T00:00:00Z"`
	cases := []struct {
		name string
		body string
	}{
		{
			"unknown_field",
			`{"project_id":"proj-1","agent_id":"agent-abc","operation_id":"op-1","execute_before":` + validExecuteBefore + `,"keys":"C-c","unexpected_field":true}`,
		},
		{
			"trailing_data",
			`{"project_id":"proj-1","agent_id":"agent-abc","operation_id":"op-1","execute_before":` + validExecuteBefore + `,"keys":"C-c"}{"trailing":"object"}`,
		},
		{
			"trailing_garbage",
			`{"project_id":"proj-1","agent_id":"agent-abc","operation_id":"op-1","execute_before":` + validExecuteBefore + `,"keys":"C-c"} not json`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			mgr := &mockManager{
				sendKeysFunc: func(ctx context.Context, projectID, agentSlug, expectedAgentID, keys string) error {
					called = true
					return nil
				},
			}
			srv := newTestServerWithManager(t, mgr)

			w := postKeysRaw(t, srv, "test-agent", "proj-1", []byte(tc.body))

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", w.Code, w.Body.String())
			}
			if called {
				t.Error("Manager.SendKeys must not be called for a malformed request body")
			}
		})
	}
}

// TestSendKeys_HTTP_InvalidKeysShape covers that the AC
// "empty/NUL/oversize/invalid shapes never execute" applies at this
// execution point, not only at the Hub. Each case must be rejected by
// agentkeys.ValidateKeys before Manager.SendKeys is ever called, at the
// status the contract's Outcome table assigns the validation failure.
//
// No invalid-UTF-8 case: postKeys round-trips agentkeys.BrokerRequest
// through encoding/json.Marshal/Decode, and Go's encoding/json silently
// replaces invalid UTF-8 byte sequences with U+FFFD on decode (verified:
// json.Unmarshal(`{"keys":"abc\xff\xfe"}`, ...) succeeds with a valid-UTF-8
// result) — so req.Keys can never actually be invalid UTF-8 by the time
// ValidateKeys sees it via this decode path, regardless of the wire bytes.
// ValidateKeys's utf8.ValidString check is still correct defensive code for
// any other caller with a differently-sourced string; there is just no way
// to exercise it through this handler's ordinary JSON decode.
func TestSendKeys_HTTP_InvalidKeysShape(t *testing.T) {
	cases := []struct {
		name       string
		keys       string
		wantStatus int
	}{
		{"empty", "", http.StatusBadRequest},
		{"nul_byte", "abc\x00def", http.StatusBadRequest},
		{"oversize", strings.Repeat("a", agentkeys.MaxBytes+1), http.StatusRequestEntityTooLarge},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			mgr := &mockManager{
				sendKeysFunc: func(ctx context.Context, projectID, agentSlug, expectedAgentID, keys string) error {
					called = true
					return nil
				},
			}
			srv := newTestServerWithManager(t, mgr)

			w := postKeys(t, srv, "test-agent", "proj-1", agentkeys.BrokerRequest{
				ProjectID:     "proj-1",
				AgentID:       "agent-abc",
				OperationID:   "op-1",
				ExecuteBefore: time.Now().UTC().Add(10 * time.Second),
				Keys:          tc.keys,
			})

			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", w.Code, tc.wantStatus, w.Body.String())
			}
			var result agentkeys.BrokerResult
			if err := json.Unmarshal(w.Body.Bytes(), &result); err == nil && result.Outcome != "" {
				t.Errorf("invalid keys shape must not decode as a well-formed BrokerResult, got %+v", result)
			}
			if called {
				t.Errorf("Manager.SendKeys must not be called for invalid keys shape %q", tc.name)
			}
		})
	}
}

// TestSendKeys_HTTP_UnscopedOrMismatchedTarget covers
// #2193's "reject unscoped target fallback" and the requirement that the
// query projectId and body project_id agree. Every case must be rejected
// with no execution.
func TestSendKeys_HTTP_UnscopedOrMismatchedTarget(t *testing.T) {
	cases := []struct {
		name           string
		projectIDQuery string
		req            agentkeys.BrokerRequest
	}{
		{
			"missing_query_project",
			"",
			agentkeys.BrokerRequest{ProjectID: "proj-1", AgentID: "agent-abc", OperationID: "op-1", ExecuteBefore: time.Now().UTC().Add(10 * time.Second), Keys: "C-c"},
		},
		{
			"missing_body_project",
			"proj-1",
			agentkeys.BrokerRequest{ProjectID: "", AgentID: "agent-abc", OperationID: "op-1", ExecuteBefore: time.Now().UTC().Add(10 * time.Second), Keys: "C-c"},
		},
		{
			"missing_agent_id",
			"proj-1",
			agentkeys.BrokerRequest{ProjectID: "proj-1", AgentID: "", OperationID: "op-1", ExecuteBefore: time.Now().UTC().Add(10 * time.Second), Keys: "C-c"},
		},
		{
			"query_body_project_mismatch",
			"proj-1",
			agentkeys.BrokerRequest{ProjectID: "proj-2", AgentID: "agent-abc", OperationID: "op-1", ExecuteBefore: time.Now().UTC().Add(10 * time.Second), Keys: "C-c"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			mgr := &mockManager{
				sendKeysFunc: func(ctx context.Context, projectID, agentSlug, expectedAgentID, keys string) error {
					called = true
					return nil
				},
			}
			srv := newTestServerWithManager(t, mgr)

			w := postKeys(t, srv, "test-agent", tc.projectIDQuery, tc.req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", w.Code, w.Body.String())
			}
			if called {
				t.Errorf("Manager.SendKeys must not be called for %s", tc.name)
			}
		})
	}
}

// TestSendKeys_HTTP_ValidationAndScopeRejectionsAreAudited covers
// ptone/scion#2184's execution-order requirement: content-free audit on
// denial/validation paths too, wherever actor/target
// can already be established — not only on post-admission outcomes. Both
// the key-shape validation rejection and the scope (unscoped/mismatched
// project) rejection must emit an audit line through logKeysOutcome.
func TestSendKeys_HTTP_ValidationAndScopeRejectionsAreAudited(t *testing.T) {
	var buf bytes.Buffer
	origWriter := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(origWriter)
		log.SetFlags(origFlags)
	})

	cases := []struct {
		name           string
		projectIDQuery string
		req            agentkeys.BrokerRequest
	}{
		{
			"invalid_keys_shape",
			"proj-1",
			agentkeys.BrokerRequest{ProjectID: "proj-1", AgentID: "agent-abc", OperationID: "op-audit-1", ExecuteBefore: time.Now().UTC().Add(10 * time.Second), Keys: ""},
		},
		{
			"scope_rejection",
			"proj-1",
			agentkeys.BrokerRequest{ProjectID: "proj-2", AgentID: "agent-abc", OperationID: "op-audit-2", ExecuteBefore: time.Now().UTC().Add(10 * time.Second), Keys: "Enter"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServerWithManager(t, &mockManager{})

			buf.Reset()
			w := postKeys(t, srv, "test-agent", tc.projectIDQuery, tc.req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", w.Code, w.Body.String())
			}
			logOutput := buf.String()
			if !strings.Contains(logOutput, "keys dispatch") {
				t.Errorf("expected a content-free audit line for %s, got log: %s", tc.name, logOutput)
			}
			if !strings.Contains(logOutput, tc.req.OperationID) {
				t.Errorf("expected the audit line to carry operation_id %q, got log: %s", tc.req.OperationID, logOutput)
			}
		})
	}
}

// TestSendKeys_HTTP_ExecuteBeforeCappedAtAdmissionWindow covers
// #2193's "Cap admission at 30 seconds/request deadline". A
// far-future ExecuteBefore must not reach Manager.SendKeys unmodified — the
// ctx deadline SendKeys observes must be capped at
// agentkeys.DefaultAdmissionWindow from admission, not the Hub-supplied
// value.
func TestSendKeys_HTTP_ExecuteBeforeCappedAtAdmissionWindow(t *testing.T) {
	var gotDeadline time.Time
	var hasDeadline bool
	mgr := &mockManager{
		sendKeysFunc: func(ctx context.Context, projectID, agentSlug, expectedAgentID, keys string) error {
			gotDeadline, hasDeadline = ctx.Deadline()
			return nil
		},
	}
	srv := newTestServerWithManager(t, mgr)

	before := time.Now().UTC()
	w := postKeys(t, srv, "test-agent", "proj-1", agentkeys.BrokerRequest{
		ProjectID:     "proj-1",
		AgentID:       "agent-abc",
		OperationID:   "op-1",
		ExecuteBefore: before.Add(time.Hour), // far beyond the 30s admission window
		Keys:          "C-c",
	})
	after := time.Now().UTC()

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if !hasDeadline {
		t.Fatal("expected SendKeys to receive a ctx with a deadline")
	}
	maxAllowed := after.Add(agentkeys.DefaultAdmissionWindow)
	if gotDeadline.After(maxAllowed) {
		t.Errorf("ctx deadline %v exceeds admission window cap (now %v + %v = %v)", gotDeadline, after, agentkeys.DefaultAdmissionWindow, maxAllowed)
	}
	if !gotDeadline.Before(before.Add(time.Hour)) {
		t.Errorf("ctx deadline %v was not capped below the Hub-supplied execute_before (%v)", gotDeadline, before.Add(time.Hour))
	}
}

// TestSendKeys_ViaControlChannelDispatch covers: the keys
// handler reached through the actual control-channel dispatch path
// (ControlChannelClient.dispatchRequest, with X-Scion-Hub-Connection set —
// the header the Hub's tunneled requests carry so the server can route to
// the correct hydrator), end to end in-process, for three outcome shapes:
// success, a broker-decided outcome (agent not running), and an
// already-expired admission deadline. This is deliberately not a saturated-
// semaphore scenario (that is covered in controlchannel_keys_test.go) —
// here the dispatch path itself (envelope in, response envelope out) is what
// is under test.
func TestSendKeys_ViaControlChannelDispatch(t *testing.T) {
	cases := []struct {
		name          string
		sendErr       error
		executeBefore time.Time
		wantStatus    int
		wantOutcome   agentkeys.Outcome
	}{
		{
			name:          "success",
			sendErr:       nil,
			executeBefore: time.Now().UTC().Add(10 * time.Second),
			wantStatus:    http.StatusOK,
			wantOutcome:   agentkeys.OutcomeDispatched,
		},
		{
			name:          "broker_decided_outcome",
			sendErr:       agentkeys.ErrAgentNotRunning,
			executeBefore: time.Now().UTC().Add(10 * time.Second),
			wantStatus:    http.StatusConflict,
			wantOutcome:   agentkeys.OutcomeAgentNotRunning,
		},
		{
			name:          "expiry",
			sendErr:       nil,
			executeBefore: time.Now().UTC().Add(-1 * time.Second),
			wantStatus:    http.StatusServiceUnavailable,
			wantOutcome:   agentkeys.OutcomeKeysUnavailable,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			mgr := &mockManager{
				sendKeysFunc: func(ctx context.Context, projectID, agentSlug, expectedAgentID, keys string) error {
					called = true
					return tc.sendErr
				},
			}
			srv := newTestServerWithManager(t, mgr)

			brokerConn, hubConn, cleanup := newWSPair(t)
			defer cleanup()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			client := &ControlChannelClient{
				config:         ControlChannelConfig{},
				conn:           brokerConn,
				handlers:       srv.Handler(),
				connectionName: "hub-conn-1",
				log:            slog.Default(),
				streams:        make(map[string]*StreamHandler),
				dispatchSem:    make(chan struct{}, defaultMaxConcurrentDispatches),
				cancels:        make(map[string]*requestCancel),
				ctx:            ctx,
				cancel:         cancel,
			}

			req := keysRequestEnvelope(t, "keys-req-"+tc.name, "test-agent", "proj-1", agentkeys.BrokerRequest{
				ProjectID:     "proj-1",
				AgentID:       "agent-abc",
				OperationID:   "op-1",
				ExecuteBefore: tc.executeBefore,
				Keys:          "C-c",
			})

			client.wg.Add(1)
			go client.dispatchRequest(brokerConn, req)
			client.wg.Wait()

			var resp wsprotocol.ResponseEnvelope
			if err := hubConn.ReadJSON(&resp); err != nil {
				t.Fatalf("reading response envelope: %v", err)
			}

			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", resp.StatusCode, tc.wantStatus, resp.Body)
			}
			var result agentkeys.BrokerResult
			if err := json.Unmarshal(resp.Body, &result); err != nil {
				t.Fatalf("decoding BrokerResult: %v; body = %s", err, resp.Body)
			}
			if result.Outcome != tc.wantOutcome {
				t.Errorf("Outcome = %q, want %q", result.Outcome, tc.wantOutcome)
			}

			if tc.name == "expiry" && called {
				t.Error("Manager.SendKeys must not be called once the admission deadline has already passed")
			}
			if tc.name != "expiry" && !called {
				t.Error("expected Manager.SendKeys to be called")
			}
		})
	}
}
