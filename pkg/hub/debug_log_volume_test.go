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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/eventstreams"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// debugLogRecord is one record written to a debugLogRecorder: its message
// and the whole record rendered as text (message and attributes).
type debugLogRecord struct {
	msg  string
	text string
}

// debugLogRecorder is a slog.Handler that keeps every record at every level.
type debugLogRecorder struct {
	mu      *sync.Mutex
	records *[]debugLogRecord
	attrs   []slog.Attr
	groups  []string
}

func newDebugLogRecorder() *debugLogRecorder {
	return &debugLogRecorder{mu: &sync.Mutex{}, records: &[]debugLogRecord{}}
}

func (h *debugLogRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (h *debugLogRecorder) Handle(_ context.Context, r slog.Record) error {
	var buf bytes.Buffer
	var th slog.Handler = slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	if len(h.attrs) > 0 {
		th = th.WithAttrs(h.attrs)
	}
	for _, g := range h.groups {
		th = th.WithGroup(g)
	}
	if err := th.Handle(context.Background(), r); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	*h.records = append(*h.records, debugLogRecord{msg: r.Message, text: buf.String()})
	return nil
}

func (h *debugLogRecorder) WithAttrs(attrs []slog.Attr) slog.Handler {
	c := *h
	c.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	return &c
}

func (h *debugLogRecorder) WithGroup(name string) slog.Handler {
	c := *h
	c.groups = append(append([]string{}, h.groups...), name)
	return &c
}

func (h *debugLogRecorder) all() []debugLogRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]debugLogRecord{}, *h.records...)
}

// count returns how many records have message msg.
func (h *debugLogRecorder) count(msg string) int {
	n := 0
	for _, rec := range h.all() {
		if rec.msg == msg {
			n++
		}
	}
	return n
}

// newRecordedEventPublisher returns a ChannelEventPublisher that logs to a
// debugLogRecorder.
func newRecordedEventPublisher(t *testing.T) (*ChannelEventPublisher, *debugLogRecorder) {
	t.Helper()
	p := NewChannelEventPublisher()
	t.Cleanup(p.Close)
	rec := newDebugLogRecorder()
	p.log = slog.New(rec)
	return p, rec
}

func TestEventPublishWithoutSubscribersWritesNoDebugLine(t *testing.T) {
	p, rec := newRecordedEventPublisher(t)

	p.PublishRaw("project.p1.broker.status", map[string]string{"k": "v"})
	if n := rec.count("event published"); n != 0 {
		t.Fatalf("publish with no subscriber wrote %d \"event published\" records, want 0", n)
	}

	// Control: the same subject with a subscriber is still logged.
	ch, unsubscribe := p.Subscribe("project.p1.broker.status")
	defer unsubscribe()
	p.PublishRaw("project.p1.broker.status", map[string]string{"k": "v"})
	<-ch
	if n := rec.count("event published"); n != 1 {
		t.Fatalf("publish with a subscriber wrote %d \"event published\" records, want 1", n)
	}
}

func TestAgentStatusEventPublishWritesNoDebugLine(t *testing.T) {
	p, rec := newRecordedEventPublisher(t)
	ch, unsubscribe := p.Subscribe("agent.>", "project.>")
	defer unsubscribe()

	agent := testAgentForDebugLog()
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		p.PublishAgentStatus(ctx, agent)
	}
	p.PublishAgentStatus(eventstreams.WithHeartbeatTrigger(ctx), agent)
	// Each call publishes on agent.<id>.status and project.<id>.agent.status.
	for i := 0; i < 8; i++ {
		<-ch
	}
	if n := rec.count("event published"); n != 0 {
		t.Fatalf("agent status publishes wrote %d \"event published\" records, want 0", n)
	}

	// Control: another agent subject with a subscriber is still logged.
	p.PublishRaw("agent."+agent.ID+".created", map[string]string{"k": "v"})
	<-ch
	if n := rec.count("event published"); n != 1 {
		t.Fatalf("agent created publish wrote %d \"event published\" records, want 1", n)
	}
}

func TestIsAgentStatusSubject(t *testing.T) {
	cases := map[string]bool{
		"agent.a1.status":              true,
		"project.p1.agent.status":      true,
		"agent.a1.created":             false,
		"broker.b1.status":             false,
		"project.p1.broker.status":     false,
		"project.p1.agent.created":     false,
		"agent.a1.status.extra":        false,
		"project.p1.agent.status.more": false,
		"agent.status":                 false,
	}
	for subject, want := range cases {
		if got := isAgentStatusSubject(subject); got != want {
			t.Errorf("isAgentStatusSubject(%q) = %v, want %v", subject, got, want)
		}
	}
}

// debugLogMarkerWindow is the length of the substrings of the marker header
// value that must not appear in any log record.
const debugLogMarkerWindow = 8

// assertNoMarkerInRecords fails if any record contains a
// debugLogMarkerWindow-long substring of marker.
func assertNoMarkerInRecords(t *testing.T, records []debugLogRecord, marker string) {
	t.Helper()
	if len(marker) < debugLogMarkerWindow {
		t.Fatalf("marker shorter than %d bytes", debugLogMarkerWindow)
	}
	for _, rec := range records {
		for i := 0; i+debugLogMarkerWindow <= len(marker); i++ {
			if part := marker[i : i+debugLogMarkerWindow]; strings.Contains(rec.text, part) {
				t.Fatalf("log record %q contains part %q of the marker header value", rec.text, part)
			}
		}
	}
}

func TestUnifiedAuthMiddleware_NoPerRequestAuthDebugLines(t *testing.T) {
	agentTokenSvc, err := NewAgentTokenService(AgentTokenConfig{})
	if err != nil {
		t.Fatalf("failed to create agent token service: %v", err)
	}
	agentToken, err := agentTokenSvc.GenerateAgentToken("agent-dbg-1", "project-dbg-1", []AgentTokenScope{ScopeAgentStatusUpdate}, nil)
	if err != nil {
		t.Fatalf("failed to generate agent token: %v", err)
	}
	const devToken = "scion_dev_q7ZmarkerVx93KpL2wRt8YhN4cJ6dFa0s"

	cases := []struct {
		name   string
		header string
	}{
		{name: "agent token", header: "Bearer " + agentToken},
		{name: "dev token", header: "Bearer " + devToken},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := newDebugLogRecorder()
			cfg := AuthConfig{
				Mode:           "production",
				AgentTokenSvc:  agentTokenSvc,
				DevAuthEnabled: true,
				DevAuthToken:   devToken,
				Debug:          true,
				Logger:         slog.New(rec),
			}
			handler := UnifiedAuthMiddleware(cfg)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if GetIdentityFromContext(r.Context()) == nil {
					t.Error("expected identity in context")
				}
				w.WriteHeader(http.StatusOK)
			}))

			req := httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil)
			req.Header.Set("Authorization", tc.header)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
			}
			for _, msg := range []string{"Auth check", "Agent authenticated"} {
				if n := rec.count(msg); n != 0 {
					t.Errorf("request wrote %d %q records, want 0", n, msg)
				}
			}
			assertNoMarkerInRecords(t, rec.all(), tc.header)
		})
	}
}

// testAgentForDebugLog returns an agent for agent status publishes.
func testAgentForDebugLog() *store.Agent {
	return &store.Agent{ID: "agent-dbg-1", ProjectID: "project-dbg-1", Name: "dbg"}
}
