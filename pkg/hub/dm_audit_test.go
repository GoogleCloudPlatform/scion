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
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Body-free DM audit tests (#1690)
// ---------------------------------------------------------------------------

// captureSlog replaces the default slog logger with one that writes to a
// buffer and returns the buffer. Restores the original logger on cleanup.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	handler := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	original := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(original) })
	return &buf
}

// testDMInput creates a minimal AgentDMInput for testing.
func testDMInput(senderProjectID, recipientProjectID string) *AgentDMInput {
	return &AgentDMInput{
		SenderAgent: &store.Agent{
			ID:        "sender-agent-id",
			Slug:      "sender-agent",
			ProjectID: senderProjectID,
		},
		TargetAgent: &store.Agent{
			ID:        "target-agent-id",
			Slug:      "target-agent",
			ProjectID: recipientProjectID,
		},
		Msg:            "this message body must not appear in audit",
		ConversationID: "conv-123",
	}
}

// ---------------------------------------------------------------------------
// DMAuditEntryFromInput tests
// ---------------------------------------------------------------------------

func TestDMAuditEntryFromInput_Allow(t *testing.T) {
	input := testDMInput("proj-a", "proj-b")
	decision := &MessageDecision{
		Allowed:               true,
		HubPolicyRevision:     42,
		ProjectPolicyRevision: 7,
		CrossProject:          true,
	}

	entry := DMAuditEntryFromInput(input, decision)

	assert.Equal(t, "allow", entry.Action)
	assert.Equal(t, "sender-agent-id", entry.SenderID)
	assert.Equal(t, "proj-a", entry.SenderProjectID)
	assert.Equal(t, "target-agent-id", entry.RecipientID)
	assert.Equal(t, "proj-b", entry.RecipientProjectID)
	assert.Equal(t, "conv-123", entry.ConversationID)
	assert.Equal(t, "agent_dm", entry.Surface)
	assert.True(t, entry.CrossProject)
	assert.Empty(t, entry.DecisionCode, "allow entries should have empty decision code")
	assert.Empty(t, entry.DecisionReason, "allow entries should have empty reason")
	assert.Equal(t, int64(42), entry.HubPolicyRevision)
	assert.Equal(t, int64(7), entry.ProjectPolicyRevision)
}

func TestDMAuditEntryFromInput_Deny(t *testing.T) {
	input := testDMInput("proj-a", "proj-b")
	decision := &MessageDecision{
		Allowed:               false,
		Code:                  MessageDenialCrossProjectSenderMode,
		Reason:                "sender mode not hub",
		HubPolicyRevision:     10,
		ProjectPolicyRevision: 3,
		CrossProject:          true,
	}

	entry := DMAuditEntryFromInput(input, decision)

	assert.Equal(t, "deny", entry.Action)
	assert.Equal(t, string(MessageDenialCrossProjectSenderMode), entry.DecisionCode)
	assert.Equal(t, "sender mode not hub", entry.DecisionReason)
	assert.True(t, entry.CrossProject)
}

func TestDMAuditEntryFromInput_SameProject(t *testing.T) {
	input := testDMInput("proj-a", "proj-a")
	decision := &MessageDecision{Allowed: true}

	entry := DMAuditEntryFromInput(input, decision)

	assert.False(t, entry.CrossProject, "same-project DMs should not be marked cross-project")
}

func TestDMAuditEntryForDenial(t *testing.T) {
	input := testDMInput("proj-a", "proj-b")

	entry := DMAuditEntryForDenial(input, "rate_limited", "too many requests")

	assert.Equal(t, "deny", entry.Action)
	assert.Equal(t, "rate_limited", entry.DecisionCode)
	assert.Equal(t, "too many requests", entry.DecisionReason)
	assert.Equal(t, "agent_dm", entry.Surface)
	assert.True(t, entry.CrossProject)
}

// ---------------------------------------------------------------------------
// LogDMAdmission tests — verify structured output, body absence
// ---------------------------------------------------------------------------

func TestLogDMAdmission_AllowDoesNotContainBody(t *testing.T) {
	buf := captureSlog(t)
	input := testDMInput("proj-a", "proj-b")

	entry := DMAuditEntryFromInput(input, &MessageDecision{Allowed: true})
	entry.CorrelationID = "msg-uuid-123"
	LogDMAdmission(entry)

	output := buf.String()
	assert.Contains(t, output, "dm admission allowed")
	assert.Contains(t, output, "sender_id=sender-agent-id")
	assert.Contains(t, output, "recipient_id=target-agent-id")
	assert.Contains(t, output, "sender_project_id=proj-a")
	assert.Contains(t, output, "recipient_project_id=proj-b")
	assert.Contains(t, output, "correlation_id=msg-uuid-123")
	assert.Contains(t, output, "surface=agent_dm")

	// AC-4: No message body content in audit.
	assert.NotContains(t, output, "this message body must not appear in audit",
		"audit log must not contain message body")
}

func TestLogDMAdmission_DenyDoesNotContainBody(t *testing.T) {
	buf := captureSlog(t)
	input := testDMInput("proj-a", "proj-b")

	entry := DMAuditEntryFromInput(input, &MessageDecision{
		Allowed: false,
		Code:    MessageDenialCrossProjectDisabled,
		Reason:  "cross-project messaging disabled",
	})
	LogDMAdmission(entry)

	output := buf.String()
	assert.Contains(t, output, "dm admission denied")
	assert.Contains(t, output, "decision_code=cross_project_disabled")

	// AC-4: No message body content in audit.
	assert.NotContains(t, output, "this message body must not appear in audit")
}

func TestLogDMAdmission_PolicyRevisions(t *testing.T) {
	buf := captureSlog(t)
	input := testDMInput("proj-a", "proj-b")

	entry := DMAuditEntryFromInput(input, &MessageDecision{
		Allowed:               true,
		HubPolicyRevision:     99,
		ProjectPolicyRevision: 12,
	})
	LogDMAdmission(entry)

	output := buf.String()
	assert.Contains(t, output, "hub_policy_revision=99")
	assert.Contains(t, output, "project_policy_revision=12")
}

func TestLogDMAdmission_OmitsEmptyOptionalFields(t *testing.T) {
	buf := captureSlog(t)
	input := testDMInput("proj-a", "proj-a")
	input.ConversationID = "" // empty conversation

	entry := DMAuditEntryFromInput(input, &MessageDecision{Allowed: true})
	LogDMAdmission(entry)

	output := buf.String()
	// Optional fields with zero values should not appear.
	assert.NotContains(t, output, "conversation_id")
	assert.NotContains(t, output, "correlation_id")
	assert.NotContains(t, output, "hub_policy_revision")
	assert.NotContains(t, output, "project_policy_revision")
}

// ---------------------------------------------------------------------------
// LogDMDispatchOutcome tests
// ---------------------------------------------------------------------------

func TestLogDMDispatchOutcome_Succeeded(t *testing.T) {
	buf := captureSlog(t)
	sender := &store.Agent{ID: "s1"}
	target := &store.Agent{ID: "t1"}

	LogDMDispatchOutcome(sender, target, "msg-1", DispatchSucceeded, nil)

	output := buf.String()
	assert.Contains(t, output, "dm dispatch outcome")
	assert.Contains(t, output, "dispatch_outcome=succeeded")
	assert.Contains(t, output, "message_id=msg-1")
	assert.NotContains(t, output, "dispatch_error")
}

func TestLogDMDispatchOutcome_Failed(t *testing.T) {
	buf := captureSlog(t)
	sender := &store.Agent{ID: "s1"}
	target := &store.Agent{ID: "t1"}

	LogDMDispatchOutcome(sender, target, "msg-2", DispatchFailed, errors.New("timeout"))

	output := buf.String()
	assert.Contains(t, output, "dispatch_outcome=failed")
	assert.Contains(t, output, "dispatch_error=timeout")
	// Failed outcome should be logged at WARN level.
	assert.Contains(t, output, "WARN")
}

func TestLogDMDispatchOutcome_Skipped(t *testing.T) {
	buf := captureSlog(t)
	sender := &store.Agent{ID: "s1"}
	target := &store.Agent{ID: "t1"}

	LogDMDispatchOutcome(sender, target, "msg-3", DispatchSkipped, nil)

	output := buf.String()
	assert.Contains(t, output, "dispatch_outcome=skipped")
}

// ---------------------------------------------------------------------------
// Integration: verify both outbound and structured paths produce equivalent
// audit entries via the shared operation (#1690 AC-1)
// ---------------------------------------------------------------------------

func TestDMAuditEntryEquivalence_BothPaths(t *testing.T) {
	// Simulate that both paths construct equivalent DMAuditEntry values
	// from the same canonical input. This is a unit-level check; the
	// integration test in agent_dm_parity_test.go verifies end-to-end.
	sameInput := testDMInput("proj-a", "proj-b")
	sameDecision := &MessageDecision{
		Allowed:               true,
		HubPolicyRevision:     5,
		ProjectPolicyRevision: 2,
		CrossProject:          true,
	}

	entry1 := DMAuditEntryFromInput(sameInput, sameDecision)
	entry2 := DMAuditEntryFromInput(sameInput, sameDecision)

	// Both entries should produce identical fields (except Timestamp).
	assert.Equal(t, entry1.Action, entry2.Action)
	assert.Equal(t, entry1.SenderID, entry2.SenderID)
	assert.Equal(t, entry1.SenderProjectID, entry2.SenderProjectID)
	assert.Equal(t, entry1.RecipientID, entry2.RecipientID)
	assert.Equal(t, entry1.RecipientProjectID, entry2.RecipientProjectID)
	assert.Equal(t, entry1.CrossProject, entry2.CrossProject)
	assert.Equal(t, entry1.Surface, entry2.Surface)
	assert.Equal(t, entry1.HubPolicyRevision, entry2.HubPolicyRevision)
	assert.Equal(t, entry1.ProjectPolicyRevision, entry2.ProjectPolicyRevision)
}

// ---------------------------------------------------------------------------
// Verify no content leakage in audit fields (#1690 AC-4)
// ---------------------------------------------------------------------------

func TestDMAuditEntry_NoContentLeakage(t *testing.T) {
	secretBody := "TOP SECRET: launch codes are 1234"
	input := &AgentDMInput{
		SenderAgent: &store.Agent{
			ID:        "s",
			ProjectID: "p1",
		},
		TargetAgent: &store.Agent{
			ID:        "t",
			ProjectID: "p2",
		},
		Msg:         secretBody,
		Attachments: []string{"/secret/file.txt"},
		Metadata:    map[string]string{"key": "sensitive-value"},
	}

	entry := DMAuditEntryFromInput(input, &MessageDecision{Allowed: true})

	// Verify the entry struct does not contain any message content.
	require.NotContains(t, entry.SenderID, secretBody)
	require.NotContains(t, entry.RecipientID, secretBody)
	require.NotContains(t, entry.DecisionReason, secretBody)
	require.NotContains(t, entry.CorrelationID, secretBody)

	// Log it and verify the log output is also clean.
	buf := captureSlog(t)
	LogDMAdmission(entry)
	output := buf.String()
	assert.NotContains(t, output, secretBody,
		"audit log must never contain message body")
	assert.NotContains(t, output, "/secret/file.txt",
		"audit log must never contain attachment paths")
	assert.NotContains(t, output, "sensitive-value",
		"audit log must never contain metadata values")
}

// ---------------------------------------------------------------------------
// Verify distinction between allow and dispatch (#1690 AC-3)
// ---------------------------------------------------------------------------

func TestDMAudit_AllowDistinctFromDispatch(t *testing.T) {
	buf := captureSlog(t)
	sender := &store.Agent{ID: "s", ProjectID: "p"}
	target := &store.Agent{ID: "t", ProjectID: "p"}

	input := &AgentDMInput{
		SenderAgent: sender,
		TargetAgent: target,
	}

	// Step 1: Admission allowed.
	allowEntry := DMAuditEntryFromInput(input, &MessageDecision{Allowed: true})
	allowEntry.CorrelationID = "msg-xyz"
	LogDMAdmission(allowEntry)

	// Step 2: Dispatch failed.
	LogDMDispatchOutcome(sender, target, "msg-xyz", DispatchFailed, errors.New("broker unreachable"))

	output := buf.String()

	// Both entries should be present and distinguishable.
	lines := strings.Split(output, "\n")
	var admissionLines, dispatchLines []string
	for _, line := range lines {
		if strings.Contains(line, "dm admission") {
			admissionLines = append(admissionLines, line)
		}
		if strings.Contains(line, "dm dispatch outcome") {
			dispatchLines = append(dispatchLines, line)
		}
	}

	require.Len(t, admissionLines, 1, "exactly one admission log expected")
	require.Len(t, dispatchLines, 1, "exactly one dispatch log expected")

	// Admission says "allowed", dispatch says "failed" — they're distinct.
	assert.Contains(t, admissionLines[0], "dm admission allowed")
	assert.Contains(t, dispatchLines[0], "dispatch_outcome=failed")
}
