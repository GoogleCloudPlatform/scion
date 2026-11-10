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
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/auditevent"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// Broker family envelope tests (remaining-audit P4 slice 1, T4 and the R12
// extras) through the real LogAuditLogger and a capture sink. They assert on
// the rendered records that cross the audit boundary. captureAuditLogs
// swaps the process default logger, so these tests are not parallel.

const brokerAuditDroppedMsg = "Broker audit event dropped"

// newBrokerEnvelopeTestLogger returns a LogAuditLogger whose legacy lines
// and drop warnings go to the returned buffer and whose converted events go
// to the returned capture sink.
func newBrokerEnvelopeTestLogger(t *testing.T) (*LogAuditLogger, *auditevent.CaptureSink, *bytes.Buffer) {
	t.Helper()
	buf := captureAuditLogs(t)
	logger := NewLogAuditLogger("[Test]", false)
	sink := auditevent.NewCaptureSink()
	logger.setBrokerEnvelopeSink(sink)
	return logger, sink, buf
}

// brokerTestCtx returns ctx carrying an operation and a request ID that are
// both requestID, as auditOperationMiddleware leaves them in production.
func brokerTestCtx(t *testing.T, ctx context.Context, requestID string) context.Context {
	t.Helper()
	op, err := auditevent.NewOperationContext(requestID)
	require.NoError(t, err)
	ctx = logging.ContextWithRequestMeta(ctx, &logging.RequestMeta{RequestID: requestID})
	return auditevent.ContextWithOperation(ctx, op)
}

func brokerEnvelopeRecords(t *testing.T, sink *auditevent.CaptureSink) ([]map[string]any, []string) {
	t.Helper()
	var out []map[string]any
	var raws []string
	for _, raw := range sink.Records() {
		var rec map[string]any
		require.NoError(t, json.Unmarshal(raw, &rec), "record is not JSON: %s", raw)
		out = append(out, rec)
		raws = append(raws, string(raw))
	}
	return out, raws
}

// auditRecordsWithMsg returns every buffered log record whose msg matches.
func auditRecordsWithMsg(t *testing.T, buf *bytes.Buffer, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, rec := range auditRecords(t, buf) {
		if rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

// assertBrokerRecordClean checks the T4 negative contract on one rendered
// record: only declared top-level keys, and none of the forbidden strings.
func assertBrokerRecordClean(t *testing.T, rec map[string]any, raw string, forbidden ...string) {
	t.Helper()
	allowed := map[string]bool{
		"schema_version": true, "event_id": true, "occurred_at": true, "family": true, "action": true,
		"phase": true, "outcome": true, "severity": true, "correlation_id": true, "request": true,
		"principal": true, "credential": true, "resource": true, "payload": true,
	}
	for key := range rec {
		assert.True(t, allowed[key], "unlisted top-level key %q in %s", key, raw)
	}
	for _, s := range append([]string{"fail_reason", "failReason", "details", "broker_name", "ip_address", "actor_id", "actor_type", "@", "scion_pat_", "scion_join_"}, forbidden...) {
		assert.NotContains(t, raw, s, "record must not carry %q", s)
	}
}

// T4: every converted helper produces exactly one valid envelope and no
// legacy line; failure text, Details, broker names, emails and token-like
// strings never reach the record.
func TestBrokerEnvelope_T4_EachConvertedActionEmitsOneEnvelope(t *testing.T) {
	type tc struct {
		name         string
		emit         func(ctx context.Context, logger *LogAuditLogger)
		want         map[string]any
		wantResource map[string]any
		wantPrinc    map[string]any
		wantPayload  map[string]any
	}
	secretDetails := map[string]string{
		"join_token":     "scion_join_canary",
		"credential_id":  "scion_pat_canary",
		"actor_email":    "someone@example.com",
		"fail_reason":    "secret-canary",
		"join_token_ttl": "1h0m0s",
	}
	cases := []tc{{
		name: "auth_failure",
		emit: func(ctx context.Context, logger *LogAuditLogger) {
			_ = logger.LogBrokerAuthEvent(ctx, &BrokerAuthEvent{
				EventType:   BrokerAuthEventAuthFailure,
				BrokerID:    "broker-1",
				BrokerName:  "name-canary",
				IPAddress:   "203.0.113.7",
				UserAgent:   "scion-broker/1.0",
				FailReason:  "invalid signature secret-canary",
				FailureCode: auditevent.BrokerFailureInvalidSignature,
				Details:     secretDetails,
			})
		},
		want:         map[string]any{"action": "auth_failure", "phase": "decision", "outcome": "deny", "severity": "warning"},
		wantResource: map[string]any{"kind": "runtime_broker", "id": "broker-1"},
		wantPayload:  map[string]any{"failure_code": "invalid_signature", "source_ip": "203.0.113.7", "user_agent": "scion-broker/1.0"},
	}, {
		name: "register",
		emit: func(ctx context.Context, logger *LogAuditLogger) {
			LogRegistrationEvent(ctx, logger, "broker-2", "name-canary", "user-9", "203.0.113.7:5555",
				auditevent.BrokerRegisterReregister, secretDetails)
		},
		want:         map[string]any{"action": "register", "phase": "commit", "outcome": "succeeded", "severity": "info"},
		wantResource: map[string]any{"kind": "runtime_broker", "id": "broker-2"},
		wantPrinc:    map[string]any{"kind": "user", "id": "user-9"},
		wantPayload:  map[string]any{"operation": "reregister", "source_ip": "203.0.113.7"},
	}, {
		name: "embedded register by a user",
		emit: func(ctx context.Context, logger *LogAuditLogger) {
			LogEmbeddedRegistrationEvent(ctx, logger, "broker-3", "name-canary", "project-1", "user-9", "203.0.113.7",
				auditevent.BrokerRegisterNew, secretDetails)
		},
		want:         map[string]any{"action": "register", "phase": "commit", "outcome": "succeeded", "severity": "info"},
		wantResource: map[string]any{"kind": "runtime_broker", "id": "broker-3", "project_id": "project-1"},
		wantPrinc:    map[string]any{"kind": "user", "id": "user-9"},
		wantPayload:  map[string]any{"operation": "register", "source_ip": "203.0.113.7"},
	}, {
		name: "join failed",
		emit: func(ctx context.Context, logger *LogAuditLogger) {
			LogJoinEvent(ctx, logger, "broker-4", "203.0.113.7", false, "join token has expired secret-canary", auditevent.BrokerFailureTokenExpired)
		},
		want:         map[string]any{"action": "join", "phase": "failure", "outcome": "failed", "severity": "warning"},
		wantResource: map[string]any{"kind": "runtime_broker", "id": "broker-4"},
		wantPayload:  map[string]any{"failure_code": "token_expired", "source_ip": "203.0.113.7"},
	}, {
		name: "join succeeded",
		emit: func(ctx context.Context, logger *LogAuditLogger) {
			LogJoinEvent(ctx, logger, "broker-5", "203.0.113.7", true, "", "")
		},
		want:         map[string]any{"action": "join", "phase": "commit", "outcome": "succeeded", "severity": "info"},
		wantResource: map[string]any{"kind": "runtime_broker", "id": "broker-5"},
		wantPayload:  map[string]any{"source_ip": "203.0.113.7"},
	}, {
		name: "rotate by the broker",
		emit: func(ctx context.Context, logger *LogAuditLogger) {
			LogRotateEvent(ctx, logger, "broker-6", "broker-6", "broker", "203.0.113.7", secretDetails)
		},
		want:         map[string]any{"action": "rotate", "phase": "commit", "outcome": "succeeded", "severity": "info"},
		wantResource: map[string]any{"kind": "runtime_broker", "id": "broker-6"},
		wantPrinc:    map[string]any{"kind": "broker", "id": "broker-6"},
		wantPayload:  map[string]any{"source_ip": "203.0.113.7"},
	}, {
		name: "rotate by a user",
		emit: func(ctx context.Context, logger *LogAuditLogger) {
			LogRotateEvent(ctx, logger, "broker-7", "user-9", "user", "203.0.113.7", nil)
		},
		want:         map[string]any{"action": "rotate", "phase": "commit", "outcome": "succeeded", "severity": "info"},
		wantResource: map[string]any{"kind": "runtime_broker", "id": "broker-7"},
		wantPrinc:    map[string]any{"kind": "user", "id": "user-9"},
		wantPayload:  map[string]any{"source_ip": "203.0.113.7"},
	}, {
		name: "deregister",
		emit: func(ctx context.Context, logger *LogAuditLogger) {
			LogDeregisterEvent(ctx, logger, "broker-8", "name-canary", "user-9", "203.0.113.7")
		},
		want:         map[string]any{"action": "deregister", "phase": "commit", "outcome": "succeeded", "severity": "info"},
		wantResource: map[string]any{"kind": "runtime_broker", "id": "broker-8"},
		wantPrinc:    map[string]any{"kind": "user", "id": "user-9"},
		wantPayload:  map[string]any{"source_ip": "203.0.113.7"},
	}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			logger, sink, buf := newBrokerEnvelopeTestLogger(t)
			c.emit(brokerTestCtx(t, context.Background(), "req-t4"), logger)

			recs, raws := brokerEnvelopeRecords(t, sink)
			require.Len(t, recs, 1, "exactly one envelope per former legacy line")
			rec := recs[0]
			assert.Equal(t, "broker", rec["family"])
			for k, v := range c.want {
				assert.Equal(t, v, rec[k], k)
			}
			assert.Equal(t, "req-t4", rec["correlation_id"])
			assert.Equal(t, map[string]any{"id": "req-t4"}, rec["request"])
			assert.Equal(t, c.wantResource, rec["resource"])
			assert.Equal(t, c.wantPayload, rec["payload"])
			if c.wantPrinc == nil {
				assert.NotContains(t, rec, "principal")
			} else {
				assert.Equal(t, c.wantPrinc, rec["principal"])
			}
			assert.NotContains(t, rec, "credential", "no credential on ctx")
			assertBrokerRecordClean(t, rec, raws[0], "canary", "1h0m0s")

			assert.Empty(t, auditRecordsWithMsg(t, buf, "Broker auth audit event"), "the legacy line is replaced, not dual-emitted")
			assert.Empty(t, auditRecordsWithMsg(t, buf, brokerAuditDroppedMsg))
		})
	}
}

// Q5 (as amended by R12): the credential leaf carries kind, id,
// boundary_kind and boundary_project_id only; the decoration's token name
// and labels never appear.
func TestBrokerEnvelope_CredentialFromContextOmitsNameAndLabels(t *testing.T) {
	logger, sink, _ := newBrokerEnvelopeTestLogger(t)
	ctx := brokerTestCtx(t, ctxWithTestDecoration("brokerauth-token", "ci-token-name-canary"), "req-cred")

	LogRotateEvent(ctx, logger, "broker-1", "user-9", "user", "203.0.113.7", nil)

	recs, raws := brokerEnvelopeRecords(t, sink)
	require.Len(t, recs, 1)
	cred, ok := recs[0]["credential"].(map[string]any)
	require.True(t, ok, "credential leaf expected: %s", raws[0])
	assert.Equal(t, "uat", cred["kind"])
	assert.Equal(t, "brokerauth-token", cred["id"])
	assert.Equal(t, "project", cred["boundary_kind"])
	assert.NotEmpty(t, cred["boundary_project_id"])
	assert.NotContains(t, cred, "name")
	assert.NotContains(t, cred, "labels")
	assert.NotContains(t, raws[0], "ci-token-name-canary")
	assert.NotContains(t, recs[0]["payload"], "credential_kind", "credential_kind is set only when unrepresentable")
}

// Q5: a request credential that does not validate (here the internal
// hub_delivery kind, which is not canonical) is recorded as
// credential_kind=unrepresentable with no credential leaf, never silently.
func TestBrokerEnvelope_UnrepresentableCredential(t *testing.T) {
	logger, sink, _ := newBrokerEnvelopeTestLogger(t)
	ctx := contextWithCredentialContext(context.Background(), CredentialContext{Kind: CredentialKindHubDelivery, ID: "delivery-1"})
	ctx = brokerTestCtx(t, ctx, "req-unrep")

	LogDeregisterEvent(ctx, logger, "broker-1", "name", "user-9", "203.0.113.7")

	recs, _ := brokerEnvelopeRecords(t, sink)
	require.Len(t, recs, 1)
	assert.NotContains(t, recs[0], "credential")
	assert.Equal(t, "unrepresentable", recs[0]["payload"].(map[string]any)["credential_kind"])
}

// Q8 as amended by R14: an embedded register with an empty actor is a bug
// path like every other empty actor: no envelope, one rate-limited
// warning, and never a {system, "hub"} principal.
func TestBrokerEnvelope_Q8_EmbeddedRegisterEmptyActorDrops(t *testing.T) {
	logger, sink, buf := newBrokerEnvelopeTestLogger(t)
	ctx := brokerTestCtx(t, context.Background(), "req-embedded")
	for i := 0; i < 2; i++ {
		LogEmbeddedRegistrationEvent(ctx, logger,
			"broker-1", "name", "project-1", "", "203.0.113.7", auditevent.BrokerRegisterNew, nil)
	}

	assert.Empty(t, sink.Records(), "no envelope, and no system principal fallback")
	warnings := auditRecordsWithMsg(t, buf, brokerAuditDroppedMsg)
	require.Len(t, warnings, 1, "one rate-limited warning")
	assertDropWarning(t, warnings[0], "register", brokerAuditDropMissingActor, "")
	assert.Empty(t, auditRecordsWithMsg(t, buf, "Broker auth audit event"), "no legacy fallback line")
}

// Q8 (R14): every empty actor is a bug path: no event, one rate-limited
// warning carrying only the action and the reason, and no system fallback.
func TestBrokerEnvelope_Q8_EmptyActorDropsWithOneWarning(t *testing.T) {
	cases := []struct {
		name   string
		action string
		emit   func(ctx context.Context, logger *LogAuditLogger)
	}{
		{"register", "register", func(ctx context.Context, logger *LogAuditLogger) {
			LogRegistrationEvent(ctx, logger, "broker-1", "name", "", "203.0.113.7", auditevent.BrokerRegisterNew, nil)
		}},
		{"rotate by system", "rotate", func(ctx context.Context, logger *LogAuditLogger) {
			LogRotateEvent(ctx, logger, "broker-1", "", "system", "203.0.113.7", nil)
		}},
		{"rotate with unknown actor type", "rotate", func(ctx context.Context, logger *LogAuditLogger) {
			LogRotateEvent(ctx, logger, "broker-1", "actor-1", "system", "203.0.113.7", nil)
		}},
		{"deregister", "deregister", func(ctx context.Context, logger *LogAuditLogger) {
			LogDeregisterEvent(ctx, logger, "broker-1", "name", "", "203.0.113.7")
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			logger, sink, buf := newBrokerEnvelopeTestLogger(t)
			ctx := brokerTestCtx(t, context.Background(), "req-empty-actor")
			c.emit(ctx, logger)
			c.emit(ctx, logger) // a second drop within the interval is not warned again

			recs, _ := brokerEnvelopeRecords(t, sink)
			assert.Empty(t, recs)
			warnings := auditRecordsWithMsg(t, buf, brokerAuditDroppedMsg)
			require.Len(t, warnings, 1, "one rate-limited warning")
			assertDropWarning(t, warnings[0], c.action, brokerAuditDropMissingActor, "")
			assert.Empty(t, auditRecordsWithMsg(t, buf, "Broker auth audit event"), "no legacy fallback line")
		})
	}
}

// assertDropWarning checks that a drop warning carries exactly the action,
// the reason and, when given, the failure code (plus slog's own keys and
// the subsystem), and nothing from the payload.
func assertDropWarning(t *testing.T, rec map[string]any, action, reason, failureCode string) {
	t.Helper()
	assert.Equal(t, "WARN", rec["level"])
	assert.Equal(t, action, rec["action"])
	assert.Equal(t, reason, rec["reason"])
	want := map[string]bool{"time": true, "level": true, "msg": true, logging.AttrSubsystem: true, "action": true, "reason": true}
	if failureCode != "" {
		assert.Equal(t, failureCode, rec["failure_code"])
		want["failure_code"] = true
	} else {
		assert.NotContains(t, rec, "failure_code")
	}
	for key := range rec {
		assert.True(t, want[key], "unexpected warning key %q", key)
	}
}

// Missing correlation (Q10, disclosed non-parity): no operation context
// means no event and one rate-limited warning with the action, the reason
// and the failure code only.
func TestBrokerEnvelope_MissingCorrelationDropsWithOneWarning(t *testing.T) {
	logger, sink, buf := newBrokerEnvelopeTestLogger(t)
	event := func() *BrokerAuthEvent {
		return &BrokerAuthEvent{
			EventType:   BrokerAuthEventAuthFailure,
			BrokerID:    "broker-1",
			IPAddress:   "203.0.113.7",
			UserAgent:   "ua-canary",
			FailReason:  "invalid signature",
			FailureCode: auditevent.BrokerFailureInvalidSignature,
		}
	}
	_ = logger.LogBrokerAuthEvent(context.Background(), event())
	_ = logger.LogBrokerAuthEvent(context.Background(), event())

	recs, _ := brokerEnvelopeRecords(t, sink)
	assert.Empty(t, recs)
	warnings := auditRecordsWithMsg(t, buf, brokerAuditDroppedMsg)
	require.Len(t, warnings, 1)
	assertDropWarning(t, warnings[0], "auth_failure", brokerAuditDropMissingCorrelation, "invalid_signature")
	assert.NotContains(t, buf.String(), "ua-canary")
	assert.NotContains(t, buf.String(), "203.0.113.7")
	assert.Empty(t, auditRecordsWithMsg(t, buf, "Broker auth audit event"), "no legacy fallback line")
}

// Without a sink (a LogAuditLogger the server did not wire), a converted
// event is dropped with the warning rather than falling back to legacy.
func TestBrokerEnvelope_NoSinkDropsWithOneWarning(t *testing.T) {
	buf := captureAuditLogs(t)
	logger := NewLogAuditLogger("[Test]", false)
	LogJoinEvent(brokerTestCtx(t, context.Background(), "req-nosink"), logger, "broker-1", "203.0.113.7", true, "", "")

	warnings := auditRecordsWithMsg(t, buf, brokerAuditDroppedMsg)
	require.Len(t, warnings, 1)
	assertDropWarning(t, warnings[0], "join", brokerAuditDropNoSink, "")
	assert.Empty(t, auditRecordsWithMsg(t, buf, "Broker auth audit event"))
}

// A failed register, rotate or deregister has no production emit site and
// no catalog pair; it is dropped with the warning.
func TestBrokerEnvelope_FailedAdministrativeActionDrops(t *testing.T) {
	logger, sink, buf := newBrokerEnvelopeTestLogger(t)
	_ = logger.LogBrokerAuthEvent(brokerTestCtx(t, context.Background(), "req-failed-rotate"), &BrokerAuthEvent{
		EventType: BrokerAuthEventRotate, BrokerID: "broker-1", Success: false, FailReason: "store unavailable",
		ActorID: "user-9", ActorType: "user",
	})
	recs, _ := brokerEnvelopeRecords(t, sink)
	assert.Empty(t, recs)
	warnings := auditRecordsWithMsg(t, buf, brokerAuditDroppedMsg)
	require.Len(t, warnings, 1)
	assertDropWarning(t, warnings[0], "rotate", brokerAuditDropUnrepresentableOutcome, "")
}

// An unclassified auth failure is recorded as other, never dropped.
func TestBrokerEnvelope_UnmappedFailureCodeIsOther(t *testing.T) {
	logger, sink, _ := newBrokerEnvelopeTestLogger(t)
	ctx := brokerTestCtx(t, context.Background(), "req-other")
	_ = logger.LogBrokerAuthEvent(ctx, &BrokerAuthEvent{EventType: BrokerAuthEventAuthFailure, BrokerID: "broker-1"})
	_ = logger.LogBrokerAuthEvent(ctx, &BrokerAuthEvent{EventType: BrokerAuthEventAuthFailure, BrokerID: "broker-1", FailureCode: "not-a-code"})
	LogJoinEvent(ctx, logger, "broker-1", "", false, "boom", "")

	recs, _ := brokerEnvelopeRecords(t, sink)
	require.Len(t, recs, 3)
	for _, rec := range recs {
		assert.Equal(t, "other", rec["payload"].(map[string]any)["failure_code"])
	}
}

// Q10 characterization: on conduit re-entry the outer operation is kept
// and the current request ID differs, so the envelope's correlation is the
// outer operation and request.id is omitted (validate.go:185 forbids
// request.id != correlation_id).
func TestBrokerEnvelope_Q10_ReentryKeepsOuterCorrelationAndOmitsRequestID(t *testing.T) {
	logger, sink, _ := newBrokerEnvelopeTestLogger(t)
	outer, err := auditevent.NewOperationContext("req-outer")
	require.NoError(t, err)
	ctx := auditevent.ContextWithOperation(context.Background(), outer)
	ctx = logging.ContextWithRequestMeta(ctx, &logging.RequestMeta{RequestID: "req-inner"})

	_ = logger.LogBrokerAuthEvent(ctx, &BrokerAuthEvent{
		EventType: BrokerAuthEventAuthFailure, BrokerID: "broker-1", FailureCode: auditevent.BrokerFailureMissingCredentials,
	})

	recs, raws := brokerEnvelopeRecords(t, sink)
	require.Len(t, recs, 1)
	assert.Equal(t, "req-outer", recs[0]["correlation_id"])
	assert.NotContains(t, recs[0], "request")
	assert.NotContains(t, raws[0], "req-inner")
}

// auth_success, link, unlink, update_settings and revoke stay legacy.
func TestBrokerEnvelope_UnconvertedTypesStayLegacy(t *testing.T) {
	for _, et := range []BrokerAuthEventType{
		BrokerAuthEventAuthSuccess, BrokerAuthEventLink, BrokerAuthEventUnlink,
		BrokerAuthEventUpdateSettings, BrokerAuthEventRevoke,
	} {
		t.Run(string(et), func(t *testing.T) {
			logger, sink, buf := newBrokerEnvelopeTestLogger(t)
			_ = logger.LogBrokerAuthEvent(brokerTestCtx(t, context.Background(), "req-legacy"), &BrokerAuthEvent{
				EventType: et, BrokerID: "broker-1", Success: true, ActorID: "user-9", ActorType: "user",
			})
			assert.Empty(t, sink.Records())
			require.Len(t, auditRecordsWithMsg(t, buf, "Broker auth audit event"), 1)
		})
	}
}

func TestBrokerJoinFailureCode(t *testing.T) {
	t.Parallel()

	cases := []struct {
		err  error
		want auditevent.BrokerFailureCode
	}{
		{ErrJoinTokenInvalid, auditevent.BrokerFailureTokenInvalid},
		{ErrJoinTokenBrokerMismatch, auditevent.BrokerFailureTokenInvalid},
		{ErrJoinTokenExpired, auditevent.BrokerFailureTokenExpired},
		{fmt.Errorf("wrapped: %w", ErrJoinTokenExpired), auditevent.BrokerFailureTokenExpired},
		{ErrTestAdminRemoteBrokerRefused, auditevent.BrokerFailureRefused},
		{fmt.Errorf("join: %w", ErrTestAdminRemoteBrokerRefused), auditevent.BrokerFailureRefused},
		{&RuntimeTargetRefusal{Status: http.StatusConflict, Code: "conflict", Message: "refused"}, auditevent.BrokerFailureRefused},
		{errors.New("failed to store broker secret: db down"), auditevent.BrokerFailureInternal},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, brokerJoinFailureCode(c.err), "%v", c.err)
	}
}

func TestAuditDropWarner_RateLimitsPerKey(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 22, 4, 0, 0, 0, time.UTC)
	w := newAuditDropWarner()
	w.now = func() time.Time { return now }

	assert.True(t, w.allow("register/missing_actor"))
	assert.False(t, w.allow("register/missing_actor"))
	assert.True(t, w.allow("register/missing_correlation"), "keys are independent")
	assert.True(t, w.allow("rotate/missing_actor"), "keys are independent")

	now = now.Add(brokerAuditDropWarnInterval - time.Nanosecond)
	assert.False(t, w.allow("register/missing_actor"))
	now = now.Add(time.Nanosecond)
	assert.True(t, w.allow("register/missing_actor"), "allowed again after the interval")
	assert.False(t, w.allow("register/missing_actor"))
	assert.Len(t, w.last, 3, "one entry per key")
}
