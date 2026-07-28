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
)

// These tests assert on EMITTED OUTPUT, not on struct fields.
//
// That distinction is the whole point of the file. TestLogGCPTokenGeneration_Success
// in audit_gcp_test.go already asserted `event.ServiceAccountID == "sa-789"` and
// passed for as long as the defect existed, because the field was faithfully set
// on the struct and then dropped on the floor by the logger. A test that reads
// back the value it just wrote to a struct proves the struct works; it says
// nothing about whether anyone can ever read the value in a log.
//
// The only audit assertion that means anything is one made against the bytes
// that leave the process.

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

// ---------------------------------------------------------------------------
// Item B: GCPTokenEvent.ServiceAccountID
// ---------------------------------------------------------------------------

// TestGCPTokenEvent_ServiceAccountIDIsEmitted is the regression test for the
// defect: the field was set on the struct and never written as an attribute.
func TestGCPTokenEvent_ServiceAccountIDIsEmitted(t *testing.T) {
	buf := captureAuditLogs(t)
	logger := NewLogAuditLogger("[Test]", false)

	err := logger.LogGCPTokenEvent(context.Background(), &GCPTokenEvent{
		EventType:           GCPTokenEventAccessToken,
		AgentID:             "agent-1",
		ProjectID:           "project-1",
		ServiceAccountEmail: "sa@proj.iam.gserviceaccount.com",
		ServiceAccountID:    "sa-789",
		Success:             true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rec := auditRecordWithMsg(t, buf, "GCP token audit event")
	if rec == nil {
		t.Fatal("no GCP token audit record was emitted")
	}
	if got := rec["sa_id"]; got != "sa-789" {
		t.Errorf("sa_id: got %v, want %q", got, "sa-789")
	}
}

// TestGCPTokenEvent_ServiceAccountIDSurvivesTheHelper covers the path callers
// actually use. The struct-level test and the logger-level test can both pass
// while the helper drops the argument in between.
func TestGCPTokenEvent_ServiceAccountIDSurvivesTheHelper(t *testing.T) {
	buf := captureAuditLogs(t)

	LogGCPTokenGeneration(context.Background(), NewLogAuditLogger("[Test]", false),
		GCPTokenEventMintSA, "agent-1", "project-1",
		"sa@proj.iam.gserviceaccount.com", "sa-from-helper", true, "")

	rec := auditRecordWithMsg(t, buf, "GCP token audit event")
	if rec == nil {
		t.Fatal("no GCP token audit record was emitted")
	}
	if got := rec["sa_id"]; got != "sa-from-helper" {
		t.Errorf("sa_id: got %v, want %q", got, "sa-from-helper")
	}
}

// TestGCPTokenEvent_SchemaKeysAreStable pins every attribute the record is
// expected to carry. A key silently disappearing is the exact failure this
// whole item is about, and it is invisible to any test that only checks the
// keys it happens to care about.
func TestGCPTokenEvent_SchemaKeysAreStable(t *testing.T) {
	buf := captureAuditLogs(t)
	logger := NewLogAuditLogger("[Test]", false)

	_ = logger.LogGCPTokenEvent(context.Background(), &GCPTokenEvent{
		EventType:           GCPTokenEventIdentityToken,
		AgentID:             "agent-1",
		ProjectID:           "project-1",
		ServiceAccountEmail: "sa@proj.iam.gserviceaccount.com",
		ServiceAccountID:    "sa-789",
		Success:             false,
		FailReason:          "permission denied",
	})

	rec := auditRecordWithMsg(t, buf, "GCP token audit event")
	if rec == nil {
		t.Fatal("no GCP token audit record was emitted")
	}
	for _, key := range []string{
		"event_type", "success", "agent_id", "project_id",
		"sa_email", "sa_id", "fail_reason",
	} {
		if _, ok := rec[key]; !ok {
			t.Errorf("attribute %q missing from emitted record", key)
		}
	}
	// A failed token event must be visible at WARN, not buried at INFO.
	if rec["level"] != "WARN" {
		t.Errorf("level: got %v, want WARN for an unsuccessful event", rec["level"])
	}
}

// TestGCPTokenEvent_EmptyServiceAccountIDStillEmitsKey documents the choice to
// emit unconditionally. A key that appears only when populated makes downstream
// queries depend on whether the field happened to be set.
func TestGCPTokenEvent_EmptyServiceAccountIDStillEmitsKey(t *testing.T) {
	buf := captureAuditLogs(t)
	logger := NewLogAuditLogger("[Test]", false)

	_ = logger.LogGCPTokenEvent(context.Background(), &GCPTokenEvent{
		EventType: GCPTokenEventAccessToken,
		AgentID:   "agent-1",
		Success:   true,
	})

	rec := auditRecordWithMsg(t, buf, "GCP token audit event")
	if rec == nil {
		t.Fatal("no GCP token audit record was emitted")
	}
	if _, ok := rec["sa_id"]; !ok {
		t.Error("sa_id key should be present even when the value is empty")
	}
}
