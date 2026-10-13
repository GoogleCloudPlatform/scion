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
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// TestSendMessage_MessageLogLineIsLifecycleEntry pins that the broker's
// acceptance line in the message log is tagged log_kind=event. A broker
// that shares the hub's message log (co-located) must not add a second
// message record for a message the hub already logged.
func TestSendMessage_MessageLogLineIsLifecycleEntry(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		wantMsg string
	}{
		{"buffered", `{"structured_message":{"version":1,"sender":"user:a","recipient":"agent:test-agent","msg":"hi","type":"instruction"}}`, "message accepted (buffered)"},
		{"interrupt", `{"interrupt":true,"structured_message":{"version":1,"sender":"user:a","recipient":"agent:test-agent","msg":"stop","type":"instruction"}}`, "message delivered (interrupt, unbuffered)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr := &recordingMessageManager{mockManager: &mockManager{}}
			srv := newTestServerWithManager(t, mgr)
			var buf bytes.Buffer
			srv.SetMessageLogger(slog.New(slog.NewJSONHandler(&buf, nil)))

			w := postRawBrokerMessage(t, srv, tc.body)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
			}
			if n, _ := mgr.calls(); n != 1 {
				t.Fatalf("delivered %d messages, want 1", n)
			}

			var found bool
			for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
				var rec map[string]any
				if err := json.Unmarshal([]byte(line), &rec); err != nil {
					t.Fatalf("unmarshal %q: %v", line, err)
				}
				if rec["msg"] != tc.wantMsg {
					continue
				}
				found = true
				if rec[logging.AttrLogKind] != logging.LogKindEvent {
					t.Errorf("log_kind = %v, want %q: %s", rec[logging.AttrLogKind], logging.LogKindEvent, line)
				}
				if rec[logging.AttrSender] != "user:a" || rec[logging.AttrRecipient] != "agent:test-agent" {
					t.Errorf("line must still name sender and recipient: %s", line)
				}
			}
			if !found {
				t.Fatalf("no %q line in message log:\n%s", tc.wantMsg, buf.String())
			}
		})
	}
}
