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
	"log/slog"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// captureAuditLogs redirects the default slog logger into a buffer for the
// duration of the test. LogAuditLogger writes through the package-level
// slog.LogAttrs, so this is the seam that sees real output.
func captureAuditLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// auditRecords parses every JSON log line in the buffer.
func auditRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

// auditRecordWithMsg returns the first record whose "msg" matches, or nil.
func auditRecordWithMsg(t *testing.T, buf *bytes.Buffer, msg string) map[string]any {
	t.Helper()
	for _, rec := range auditRecords(t, buf) {
		if rec["msg"] == msg {
			return rec
		}
	}
	return nil
}

// mockAuditLogger captures audit events for testing.
type mockAuditLogger struct {
	brokerEvents []*BrokerAuthEvent
	gcpEvents    []*GCPTokenEvent
	saEvents     []*store.SAAssignmentEvent
}

func (m *mockAuditLogger) RecordSAAssignment(_ context.Context, event *store.SAAssignmentEvent) error {
	m.saEvents = append(m.saEvents, event)
	return nil
}

func (m *mockAuditLogger) LogBrokerAuthEvent(_ context.Context, event *BrokerAuthEvent) error {
	m.brokerEvents = append(m.brokerEvents, event)
	return nil
}

func (m *mockAuditLogger) LogGCPTokenEvent(_ context.Context, event *GCPTokenEvent) error {
	m.gcpEvents = append(m.gcpEvents, event)
	return nil
}

func (m *mockAuditLogger) LogInviteAuditEvent(_ context.Context, _ *InviteAuditEvent) error {
	return nil
}

func (m *mockAuditLogger) LogLifecycleHookEvent(_ context.Context, _ *LifecycleHookEvent) error {
	return nil
}

func (m *mockAuditLogger) LogLifecycleHookExecutionEvent(_ context.Context, _ *LifecycleHookExecutionEvent) error {
	return nil
}

func (m *mockAuditLogger) LogAgentSecretReadEvent(_ context.Context, _ *AgentSecretReadEvent) error {
	return nil
}

func (m *mockAuditLogger) LogGCSLinkFetchEvent(_ context.Context, _ *GCSLinkFetchEvent) error {
	return nil
}
