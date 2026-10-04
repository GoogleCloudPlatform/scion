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
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// recordingMessageManager counts every delivery primitive the broker's
// /message handler could reach.
type recordingMessageManager struct {
	*mockManager
	mu        sync.Mutex
	messages  []string
	interrupt []bool
	keys      int
}

func (m *recordingMessageManager) Message(ctx context.Context, agentID, projectID, message string, interrupt bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.messages = append(m.messages, message)
	m.interrupt = append(m.interrupt, interrupt)
	return nil
}

func (m *recordingMessageManager) SendKeys(ctx context.Context, projectID, agentSlug, expectedAgentID, keys string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.keys++
	return nil
}

func (m *recordingMessageManager) SendKeysLocal(ctx context.Context, projectPath, agentSlug, expectedAgentID, keys string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.keys++
	return nil
}

func (m *recordingMessageManager) calls() (int, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.messages), m.keys
}

func postRawBrokerMessage(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent/message", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	return w
}

// TestSendMessage_RetiredRawRejectedWithoutSideEffects pins the broker
// /message tombstone: a request carrying structured_message.raw (or a
// top-level raw) is refused with 422 raw_input_removed for every value
// shape, before decoding, and reaches no delivery primitive and no message
// log.
func TestSendMessage_RetiredRawRejectedWithoutSideEffects(t *testing.T) {
	bodies := map[string]string{
		"nested true":       `{"structured_message":{"version":1,"sender":"user:a","recipient":"agent:test-agent","msg":"Escape","type":"instruction","raw":true}}`,
		"nested false":      `{"structured_message":{"msg":"hi","type":"instruction","raw":false}}`,
		"nested null":       `{"structured_message":{"msg":"hi","raw":null}}`,
		"nested wrong type": `{"structured_message":{"msg":"hi","raw":"true"}}`,
		"nested malformed":  `{"structured_message":{"msg":"hi","raw":tru}}`,
		"nested case":       `{"Structured_Message":{"msg":"hi","RAW":true}}`,
		"nested duplicate":  `{"structured_message":{"msg":"hi"},"structured_message":{"raw":false}}`,
		"top-level true":    `{"message":"hi","raw":true}`,
		"top-level false":   `{"message":"hi","raw":false}`,
		"top-level null":    `{"message":"hi","raw":null}`,
		"top-level wrong":   `{"message":"hi","raw":1}`,
		"top-level bad":     `{"message":"hi","raw":}`,
		"with interrupt":    `{"interrupt":true,"structured_message":{"msg":"hi","raw":true}}`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			mgr := &recordingMessageManager{mockManager: &mockManager{}}
			srv := newTestServerWithManager(t, mgr)
			messageLogSpy := &spyLogHandler{}
			dedicatedLogSpy := &spyLogHandler{}
			srv.messageLog = slog.New(messageLogSpy)
			srv.dedicatedMessageLog = slog.New(dedicatedLogSpy)

			w := postRawBrokerMessage(t, srv, body)
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422; body: %s", w.Code, w.Body.String())
			}
			var resp ErrorResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode error response: %v", err)
			}
			if resp.Error.Code != messages.RawInputRemovedCode {
				t.Errorf("code = %q, want %q", resp.Error.Code, messages.RawInputRemovedCode)
			}
			if !strings.Contains(resp.Error.Message, "scion keys") {
				t.Errorf("message %q must name the replacement", resp.Error.Message)
			}
			if strings.Contains(w.Body.String(), "Escape") {
				t.Errorf("rejection must not echo message content: %s", w.Body.String())
			}
			if msgs, keys := mgr.calls(); msgs != 0 || keys != 0 {
				t.Errorf("delivery calls = (message %d, keys %d), want zero", msgs, keys)
			}
			if messageLogSpy.records != 0 || dedicatedLogSpy.records != 0 {
				t.Errorf("message logs written on rejection: messageLog=%d dedicated=%d", messageLogSpy.records, dedicatedLogSpy.records)
			}
		})
	}
}

// TestSendMessage_PlainNormalInterruptUnaffected is the positive control
// for the tombstone: plain, normal and interrupt messages (and a message
// whose text merely mentions raw) are still delivered.
func TestSendMessage_PlainNormalInterruptUnaffected(t *testing.T) {
	cases := []struct {
		name          string
		body          string
		wantText      string
		wantInterrupt bool
	}{
		{"legacy text", `{"message":"hello"}`, "hello", false},
		{"plain", `{"structured_message":{"version":1,"sender":"user:a","recipient":"agent:test-agent","msg":"plain text","type":"instruction","plain":true}}`, "plain text", false},
		{"interrupt", `{"interrupt":true,"structured_message":{"msg":"stop","type":"instruction","plain":true}}`, "stop", true},
		{"raw only as text", `{"structured_message":{"msg":"raw","type":"raw","plain":true,"metadata":{"raw":"x"}}}`, "raw", false},
		{"delivery text", `{"delivery_text":"rendered","structured_message":{"msg":"x","type":"instruction"}}`, "rendered", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr := &recordingMessageManager{mockManager: &mockManager{}}
			srv := newTestServerWithManager(t, mgr)
			w := postRawBrokerMessage(t, srv, tc.body)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
			}
			mgr.mu.Lock()
			defer mgr.mu.Unlock()
			if len(mgr.messages) != 1 || mgr.messages[0] != tc.wantText || mgr.interrupt[0] != tc.wantInterrupt {
				t.Fatalf("delivered %q interrupt=%v, want [%q] interrupt=%v", mgr.messages, mgr.interrupt, tc.wantText, tc.wantInterrupt)
			}
			if mgr.keys != 0 {
				t.Fatalf("message path must never call SendKeys, got %d", mgr.keys)
			}
		})
	}
}

// TestSendMessage_HubShapedNearLimitDelivered pins that this Hub-only route
// applies no byte cap of its own. The Hub accepts message bodies up to 2 MiB
// on its public ingress and then forwards a rebuilt request in which the text
// appears three times (structured_message.msg, the nested rendered
// delivery_text, and the top-level delivery_text), so the forwarded body is
// well over 2 MiB. It must be delivered, not refused with 413.
func TestSendMessage_HubShapedNearLimitDelivered(t *testing.T) {
	mgr := &recordingMessageManager{mockManager: &mockManager{}}
	srv := newTestServerWithManager(t, mgr)

	// Just under the Hub's 2 MiB public ingress limit.
	text := strings.Repeat("x", 2<<20-4096)
	delivery := "---BEGIN SCION MESSAGE---\n" + text + "\n---END SCION MESSAGE---"
	reqBody := map[string]interface{}{
		"interrupt":  false,
		"project_id": "proj-1",
		"message_id": "msg-1",
		"structured_message": map[string]interface{}{
			"version":       1,
			"sender":        "user:alice",
			"recipient":     "agent:test-agent",
			"msg":           text,
			"type":          "instruction",
			"delivery_text": delivery,
		},
		"delivery_text": delivery,
	}
	raw, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(raw) <= 3*(2<<20)-3*4096 {
		t.Fatalf("forwarded body is %d bytes; the test needs a body well over 2 MiB", len(raw))
	}

	w := postRawBrokerMessage(t, srv, string(raw))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a Hub-forwarded near-limit message must be delivered); body: %.300s", w.Code, w.Body.String())
	}
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	if len(mgr.messages) != 1 {
		t.Fatalf("mgr.Message calls = %d, want 1", len(mgr.messages))
	}
	if mgr.messages[0] != delivery {
		t.Errorf("delivered %d bytes, want the top-level delivery_text (%d bytes)", len(mgr.messages[0]), len(delivery))
	}
	if mgr.keys != 0 {
		t.Fatalf("message path must never call SendKeys, got %d", mgr.keys)
	}
}
