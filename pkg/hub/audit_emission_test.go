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

// ---------------------------------------------------------------------------
// Item C: LogBrokerAuthEvent
// ---------------------------------------------------------------------------

// TestBrokerAuthEvent_FailureIsEmittedAtWarn is the regression test for the
// gap left by 500efd1a. A rejected broker credential left no trace anywhere
// while this method was a no-op; that is the single event here most worth
// having, and it must never be suppressed.
func TestBrokerAuthEvent_FailureIsEmittedAtWarn(t *testing.T) {
	buf := captureAuditLogs(t)
	logger := NewLogAuditLogger("[Test]", false)

	err := logger.LogBrokerAuthEvent(context.Background(), &BrokerAuthEvent{
		EventType:  BrokerAuthEventAuthFailure,
		BrokerID:   "broker-1",
		IPAddress:  "203.0.113.7",
		Success:    false,
		FailReason: "signature mismatch",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rec := auditRecordWithMsg(t, buf, "Broker auth audit event")
	if rec == nil {
		t.Fatal("broker auth failure produced no audit record")
	}
	if rec["level"] != "WARN" {
		t.Errorf("level: got %v, want WARN", rec["level"])
	}
	if rec["event_type"] != string(BrokerAuthEventAuthFailure) {
		t.Errorf("event_type: got %v", rec["event_type"])
	}
	if rec["fail_reason"] != "signature mismatch" {
		t.Errorf("fail_reason: got %v", rec["fail_reason"])
	}
	if rec["broker_id"] != "broker-1" {
		t.Errorf("broker_id: got %v", rec["broker_id"])
	}
	if rec["ip_address"] != "203.0.113.7" {
		t.Errorf("ip_address: got %v", rec["ip_address"])
	}
}

// TestBrokerAuthEvent_SuccessIsDebugLevel pins the concession to 500efd1a.
// auth_success is emitted per-request by AuditableBrokerAuthMiddleware, so it
// must not reappear at INFO — that was the actual defect being fixed.
func TestBrokerAuthEvent_SuccessIsDebugLevel(t *testing.T) {
	buf := captureAuditLogs(t)
	logger := NewLogAuditLogger("[Test]", false)

	_ = logger.LogBrokerAuthEvent(context.Background(), &BrokerAuthEvent{
		EventType: BrokerAuthEventAuthSuccess,
		BrokerID:  "broker-1",
		Success:   true,
	})

	rec := auditRecordWithMsg(t, buf, "Broker auth audit event")
	if rec == nil {
		t.Fatal("expected auth_success to be emitted at debug, got no record")
	}
	if rec["level"] != "DEBUG" {
		t.Errorf("level: got %v, want DEBUG — per-request event must not be INFO", rec["level"])
	}
}

// TestBrokerAuthEvent_AdminEventsAreInfoLevel: the low-volume administrative
// events are the ones 500efd1a silenced as collateral. Each changes who can
// talk to the Hub and belongs in normal operation, not behind a debug flag.
func TestBrokerAuthEvent_AdminEventsAreInfoLevel(t *testing.T) {
	for _, et := range []BrokerAuthEventType{
		BrokerAuthEventRegister,
		BrokerAuthEventDeregister,
		BrokerAuthEventJoin,
		BrokerAuthEventRotate,
		BrokerAuthEventRevoke,
		BrokerAuthEventLink,
		BrokerAuthEventUnlink,
	} {
		t.Run(string(et), func(t *testing.T) {
			buf := captureAuditLogs(t)
			logger := NewLogAuditLogger("[Test]", false)

			_ = logger.LogBrokerAuthEvent(context.Background(), &BrokerAuthEvent{
				EventType: et,
				BrokerID:  "broker-1",
				Success:   true,
			})

			rec := auditRecordWithMsg(t, buf, "Broker auth audit event")
			if rec == nil {
				t.Fatalf("%s produced no audit record", et)
			}
			if rec["level"] != "INFO" {
				t.Errorf("level: got %v, want INFO", rec["level"])
			}
		})
	}
}

// TestBrokerAuthEvent_LinkCarriesProjectID covers the debug-gating defect the
// no-op was hiding. LogLinkEvent puts projectId in Details, and a link record
// without it says "broker B was linked" without saying to what.
func TestBrokerAuthEvent_LinkCarriesProjectID(t *testing.T) {
	// debug=false deliberately: the field must survive with debug OFF.
	buf := captureAuditLogs(t)
	logger := NewLogAuditLogger("[Test]", false)

	LogLinkEvent(context.Background(), logger,
		"broker-1", "broker-name", "project-42", "user-9", "203.0.113.7")

	rec := auditRecordWithMsg(t, buf, "Broker auth audit event")
	if rec == nil {
		t.Fatal("link event produced no audit record")
	}
	if rec["projectId"] != "project-42" {
		t.Errorf("projectId: got %v, want %q (Details must not be debug-gated)", rec["projectId"], "project-42")
	}
	if rec["broker_name"] != "broker-name" {
		t.Errorf("broker_name: got %v", rec["broker_name"])
	}
}

// TestBrokerAuthEvent_ActorIsEmittedAsAPair: an actor id without its type is
// ambiguous between a user and a broker acting on its own behalf.
func TestBrokerAuthEvent_ActorIsEmittedAsAPair(t *testing.T) {
	buf := captureAuditLogs(t)
	logger := NewLogAuditLogger("[Test]", false)

	LogRegistrationEvent(context.Background(), logger,
		"broker-1", "broker-name", "user-9", "203.0.113.7")

	rec := auditRecordWithMsg(t, buf, "Broker auth audit event")
	if rec == nil {
		t.Fatal("registration produced no audit record")
	}
	if rec["actor_id"] != "user-9" {
		t.Errorf("actor_id: got %v", rec["actor_id"])
	}
	if rec["actor_type"] != "user" {
		t.Errorf("actor_type: got %v", rec["actor_type"])
	}
}

// TestBrokerAuthEvent_NilEventDoesNotPanic — the helpers guard a nil logger but
// nothing guarantees a non-nil event.
func TestBrokerAuthEvent_NilEventDoesNotPanic(t *testing.T) {
	logger := NewLogAuditLogger("[Test]", false)
	if err := logger.LogBrokerAuthEvent(context.Background(), nil); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestBrokerAuthEvent_FailedAdminActionIsWarn: the level switch keys on success
// first, so a failed rotate is a warning even though rotate is administrative.
func TestBrokerAuthEvent_FailedAdminActionIsWarn(t *testing.T) {
	buf := captureAuditLogs(t)
	logger := NewLogAuditLogger("[Test]", false)

	_ = logger.LogBrokerAuthEvent(context.Background(), &BrokerAuthEvent{
		EventType:  BrokerAuthEventRotate,
		BrokerID:   "broker-1",
		Success:    false,
		FailReason: "store unavailable",
	})

	rec := auditRecordWithMsg(t, buf, "Broker auth audit event")
	if rec == nil {
		t.Fatal("failed rotate produced no audit record")
	}
	if rec["level"] != "WARN" {
		t.Errorf("level: got %v, want WARN", rec["level"])
	}
}

// ---------------------------------------------------------------------------
// Item A: SA assignment records (design §7)
// ---------------------------------------------------------------------------

const saAuditMsg = "SA assignment audit event"

// saDecisionEvent builds a decision record with everything populated, so each
// test can vary one thing.
func saDecisionEvent(outcome store.ActAsOutcome) *store.SAAssignmentEvent {
	return &store.SAAssignmentEvent{
		Type:          store.SAAssignmentDecision,
		Surface:       "agent-create",
		Caller:        store.Principal{Kind: store.PrincipalUser, ID: "u-1", Email: "dev@example.com"},
		TargetSAID:    "sa-1",
		TargetSAEmail: "worker@p.iam.gserviceaccount.com",
		Permission:    store.PermissionActAs,
		Mechanism:     store.MechanismCheckDisabled,
		Decision:      &outcome,
		Reason:        "checking is switched off",
	}
}

// TestSAAssignment_AllowIsEmitted. An audit trail that records only refusals
// cannot answer "who was given this account", which is the question asked after
// an incident.
func TestSAAssignment_AllowIsEmitted(t *testing.T) {
	buf := captureAuditLogs(t)
	_ = NewLogAuditLogger("[Test]", false).
		RecordSAAssignment(context.Background(), saDecisionEvent(store.ActAsAllowed))

	rec := auditRecordWithMsg(t, buf, saAuditMsg)
	if rec == nil {
		t.Fatal("no SA assignment record was emitted on the allow path")
	}
	for field, want := range map[string]any{
		"level":                "INFO",
		"event_type":           "sa_assignment_decision",
		"surface":              "agent-create",
		"caller_kind":          "user",
		"caller_id":            "u-1",
		"caller_gcp_principal": "user:dev@example.com",
		"target_sa_id":         "sa-1",
		"target_sa_email":      "worker@p.iam.gserviceaccount.com",
		"permission":           store.PermissionActAs,
		"mechanism":            store.MechanismCheckDisabled,
		"decision":             "allowed",
	} {
		if rec[field] != want {
			t.Errorf("%s: got %v, want %v", field, rec[field], want)
		}
	}
}

// TestSAAssignment_MechanismSurvivesTheAllowPath is the reason the allow record
// is worth emitting at all while the gate is inert. "Allowed because IAM said
// so" and "allowed because nobody asked" are the same outcome and completely
// different facts, and the mechanism is the only place that difference is
// recorded. Without it, the current toggle-off records would be
// indistinguishable from enforced approvals.
func TestSAAssignment_MechanismSurvivesTheAllowPath(t *testing.T) {
	buf := captureAuditLogs(t)
	_ = NewLogAuditLogger("[Test]", false).
		RecordSAAssignment(context.Background(), saDecisionEvent(store.ActAsAllowed))

	rec := auditRecordWithMsg(t, buf, saAuditMsg)
	if rec == nil {
		t.Fatal("no record emitted")
	}
	if rec["mechanism"] != store.MechanismCheckDisabled {
		t.Errorf("mechanism: got %v, want %q", rec["mechanism"], store.MechanismCheckDisabled)
	}
}

// TestSAAssignment_DenialIsWarn — a denial is the record an operator goes
// looking for, and it must not be filed at the same level as routine traffic.
func TestSAAssignment_DenialIsWarn(t *testing.T) {
	for _, outcome := range []store.ActAsOutcome{store.ActAsDenied, store.ActAsIndeterminate} {
		t.Run(outcome.String(), func(t *testing.T) {
			buf := captureAuditLogs(t)
			_ = NewLogAuditLogger("[Test]", false).
				RecordSAAssignment(context.Background(), saDecisionEvent(outcome))

			rec := auditRecordWithMsg(t, buf, saAuditMsg)
			if rec == nil {
				t.Fatal("no record emitted on the deny path")
			}
			if rec["level"] != "WARN" {
				t.Errorf("level: got %v, want WARN", rec["level"])
			}
			if rec["decision"] != outcome.String() {
				t.Errorf("decision: got %v, want %q", rec["decision"], outcome.String())
			}
		})
	}
}

// TestSAAssignment_CacheHitIsAbsentFromTheWireWhenNil is the ruled behaviour,
// and it is asserted against the RAW BYTES rather than the parsed map because
// the two failures being excluded look different on the wire and identical
// after parsing: `"cache_hit":null` and `"cache_hit":false` both arrive at a
// lenient consumer as false, which fabricates a cache miss — i.e. asserts a
// live IAM call that never happened. The key must simply not be there.
func TestSAAssignment_CacheHitIsAbsentFromTheWireWhenNil(t *testing.T) {
	buf := captureAuditLogs(t)
	event := saDecisionEvent(store.ActAsAllowed)
	if event.CacheHit != nil {
		t.Fatal("fixture is wrong: CacheHit must start nil")
	}
	_ = NewLogAuditLogger("[Test]", false).RecordSAAssignment(context.Background(), event)

	if strings.Contains(buf.String(), "cache_hit") {
		t.Errorf("cache_hit appears on the wire when no cache was consulted: %s", buf.String())
	}
	rec := auditRecordWithMsg(t, buf, saAuditMsg)
	if rec == nil {
		t.Fatal("no record emitted")
	}
	if _, present := rec["cache_hit"]; present {
		t.Error("cache_hit key is present; nil must be omitted, not serialised")
	}
}

// TestSAAssignment_CacheHitIsEmittedWhenConsulted is the other half, and
// without it the previous test would also pass on an implementation that never
// emitted the field at all. False in particular must survive: a genuine cache
// MISS is a real observation and is exactly what nil is not.
func TestSAAssignment_CacheHitIsEmittedWhenConsulted(t *testing.T) {
	for _, hit := range []bool{true, false} {
		t.Run(map[bool]string{true: "hit", false: "miss"}[hit], func(t *testing.T) {
			buf := captureAuditLogs(t)
			event := saDecisionEvent(store.ActAsAllowed)
			event.CacheHit = &hit
			_ = NewLogAuditLogger("[Test]", false).RecordSAAssignment(context.Background(), event)

			rec := auditRecordWithMsg(t, buf, saAuditMsg)
			if rec == nil {
				t.Fatal("no record emitted")
			}
			got, present := rec["cache_hit"]
			if !present {
				t.Fatal("cache_hit missing although a cache was consulted")
			}
			if got != hit {
				t.Errorf("cache_hit: got %v, want %v", got, hit)
			}
		})
	}
}

// TestSAAssignment_BindingRecordCarriesNoVerdict. A project-default binding
// decided nothing, so the record must not appear to contain a verdict. See
// store.SAAssignmentEvent.Decision for why every available ActAsOutcome value
// would be a lie here — and why the one that would be picked by default,
// Indeterminate, denies.
func TestSAAssignment_BindingRecordCarriesNoVerdict(t *testing.T) {
	buf := captureAuditLogs(t)
	_ = NewLogAuditLogger("[Test]", false).RecordSAAssignment(context.Background(),
		&store.SAAssignmentEvent{
			Type:          store.SAAssignmentBinding,
			Surface:       "project-default",
			Caller:        store.Principal{Kind: store.PrincipalUser, ID: "u-1", Email: "dev@example.com"},
			TargetSAID:    "sa-1",
			TargetSAEmail: "worker@p.iam.gserviceaccount.com",
			Mechanism:     store.MechanismProjectDefault,
			Reason:        "the project's default service account",
		})

	body := buf.String()
	if strings.Contains(body, `"decision"`) {
		t.Errorf("a binding record carries a decision: %s", body)
	}
	if strings.Contains(body, `"permission"`) {
		t.Errorf("a binding record names a permission that was never checked: %s", body)
	}

	rec := auditRecordWithMsg(t, buf, saAuditMsg)
	if rec == nil {
		t.Fatal("no binding record emitted; a binding that produces no record is " +
			"indistinguishable from no binding")
	}
	if rec["level"] != "INFO" {
		t.Errorf("level: got %v, want INFO — nothing was refused", rec["level"])
	}
	if rec["event_type"] != "sa_binding" {
		t.Errorf("event_type: got %v, want sa_binding", rec["event_type"])
	}
	// The mechanism has to say WHY there is no gate. "ungated" would read as a
	// gap and invite someone to close it; this says there is nothing to close.
	if rec["mechanism"] != store.MechanismProjectDefault {
		t.Errorf("mechanism: got %v, want %q", rec["mechanism"], store.MechanismProjectDefault)
	}
}

// TestSAAssignment_CallerWithoutGCPIdentityOmitsThePrincipal — a block-mode
// agent has no IAM principal, and emitting an empty one would suggest the
// lookup failed rather than that there is nothing to look up.
func TestSAAssignment_CallerWithoutGCPIdentityOmitsThePrincipal(t *testing.T) {
	buf := captureAuditLogs(t)
	event := saDecisionEvent(store.ActAsDenied)
	event.Caller = store.Principal{Kind: store.PrincipalAgent, ID: "agent-1"}
	event.Mechanism = store.MechanismNoCallerIdentity
	_ = NewLogAuditLogger("[Test]", false).RecordSAAssignment(context.Background(), event)

	rec := auditRecordWithMsg(t, buf, saAuditMsg)
	if rec == nil {
		t.Fatal("no record emitted")
	}
	if _, present := rec["caller_gcp_principal"]; present {
		t.Errorf("caller_gcp_principal present for a caller that has none: %v", rec["caller_gcp_principal"])
	}
	// The caller is still identified. "Some agent was refused" is not a record.
	if rec["caller_id"] != "agent-1" || rec["caller_kind"] != "agent" {
		t.Errorf("caller not identified: kind=%v id=%v", rec["caller_kind"], rec["caller_id"])
	}
}

// TestSAAssignment_NilEventDoesNotPanic — these emits sit on the agent-creation
// path. An audit change must never be able to take it down.
func TestSAAssignment_NilEventDoesNotPanic(t *testing.T) {
	if err := NewLogAuditLogger("[Test]", false).RecordSAAssignment(context.Background(), nil); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	var nilLogger *LogAuditLogger
	if err := nilLogger.RecordSAAssignment(context.Background(), saDecisionEvent(store.ActAsAllowed)); err != nil {
		t.Errorf("unexpected error from nil receiver: %v", err)
	}
}
