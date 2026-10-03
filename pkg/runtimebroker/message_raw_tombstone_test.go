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
