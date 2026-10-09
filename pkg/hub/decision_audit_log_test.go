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
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/auditevent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

const testDecisionRequestID = "req-0123456789"

func decisionCtx() context.Context {
	return logging.ContextWithRequestMeta(context.Background(), &logging.RequestMeta{RequestID: testDecisionRequestID})
}

// inDomainDecisionRecord is a system-scoped project read by a user, with a
// UAT credential carrying a name and labels that must never be emitted.
func inDomainDecisionRecord() *store.DecisionAuditRecord {
	return &store.DecisionAuditRecord{
		Timestamp:                   time.Date(2026, 10, 9, 21, 0, 0, 0, time.UTC),
		PrincipalKind:               "user",
		PrincipalID:                 "user-1",
		CredentialID:                "uat-1",
		CredentialType:              "uat",
		CredentialName:              "SECRET-NAME-CANARY",
		CredentialLabels:            `{"team":"LABEL-CANARY"}`,
		CredentialBoundaryKind:      "project",
		CredentialBoundaryProjectID: "proj-1",
		ResourceType:                "project",
		ResourceID:                  "proj-1",
		Permission:                  "read",
		PermissionID:                "project.read",
		Result:                      "allow",
		Reason:                      "granted by binding",
		MatchedPolicy:               "POLICY-CANARY",
		MatchedGrant:                "GRANT-CANARY",
		PolicyID:                    "POLICYID-CANARY",
		Route:                       "GET /api/v1/projects/{id}",
		Sampled:                     false,
	}
}

func renderEnvelope(t *testing.T, env auditevent.EnvelopeV1) map[string]any {
	t.Helper()
	raw, err := auditevent.Render(env)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	return m
}

func TestMapDecisionEnvelope_InDomainMapping(t *testing.T) {
	rec := inDomainDecisionRecord()
	env, d := mapDecisionEnvelope(decisionCtx(), rec)
	require.Equal(t, decisionAuditEnqueued, d)
	require.NoError(t, auditevent.Validate(env))
	require.Equal(t, "authorization", env.Family)
	require.Equal(t, "decide", env.Action)
	require.Equal(t, auditevent.PhaseDecision, env.Phase)
	require.Equal(t, auditevent.OutcomeAllow, env.Outcome)
	require.Equal(t, auditevent.SeverityInfo, env.Severity)
	require.Equal(t, testDecisionRequestID, env.CorrelationID)
	require.Equal(t, &auditevent.RequestRef{ID: testDecisionRequestID, Route: rec.Route}, env.Request)
	require.Equal(t, &auditevent.IdentityRef{Kind: auditevent.IdentityUser, ID: "user-1"}, env.Principal)
	require.Equal(t, auditevent.ResourceScopeSystem, env.Resource.Scope)
	require.Empty(t, env.Resource.ProjectID)
	require.Equal(t, rec.Timestamp.UTC(), env.OccurredAt)
	require.NotNil(t, env.Credential)
	require.Equal(t, auditevent.CredentialUAT, env.Credential.Kind())
	require.Empty(t, env.Credential.Name())
	require.Empty(t, env.Credential.Labels())
	require.Nil(t, env.Executor)

	out := renderEnvelope(t, env)
	raw, err := json.Marshal(out)
	require.NoError(t, err)
	for _, canary := range []string{"SECRET-NAME-CANARY", "LABEL-CANARY", "POLICY-CANARY", "GRANT-CANARY", "POLICYID-CANARY"} {
		require.NotContains(t, string(raw), canary)
	}
	payload := out["payload"].(map[string]any)
	require.Equal(t, "project.read", payload["permission_id"])
	require.Equal(t, "read", payload["permission"])
	require.Equal(t, "granted by binding", payload["reason"])
	require.Equal(t, "false", payload["sampled"])
	require.NotContains(t, payload, "denied_by")
}

func TestMapDecisionEnvelope_DenyAndPrincipalKinds(t *testing.T) {
	for kind, want := range map[string]auditevent.IdentityKind{
		"user": auditevent.IdentityUser, "agent": auditevent.IdentityAgent, "broker": auditevent.IdentityBroker,
	} {
		rec := inDomainDecisionRecord()
		rec.PrincipalKind = kind
		rec.Result = "deny"
		rec.DeniedBy = "uat_ceiling"
		env, d := mapDecisionEnvelope(decisionCtx(), rec)
		require.Equal(t, decisionAuditEnqueued, d, kind)
		require.Equal(t, want, env.Principal.Kind)
		require.Equal(t, auditevent.OutcomeDeny, env.Outcome)
		require.Equal(t, auditevent.SeverityWarning, env.Severity)
		require.Equal(t, "uat_ceiling", env.Payload.(auditevent.AuthorizationDecisionPayload).DeniedBy)
	}
}

func TestMapDecisionEnvelope_AgentResourceWithoutContainment(t *testing.T) {
	rec := inDomainDecisionRecord()
	rec.ResourceType = "agent"
	rec.ResourceID = "agent-1"
	rec.PermissionID = "agent.read"
	_, d := mapDecisionEnvelope(decisionCtx(), rec)
	require.Equal(t, decisionAuditEnqueued, d)
}

func TestMapDecisionEnvelope_OperationContextPrecedesRequestID(t *testing.T) {
	op, err := auditevent.NewOperationContext("op-correlation-1")
	require.NoError(t, err)
	ctx := auditevent.ContextWithOperation(decisionCtx(), op)
	env, d := mapDecisionEnvelope(ctx, inDomainDecisionRecord())
	require.Equal(t, decisionAuditEnqueued, d)
	require.Equal(t, "op-correlation-1", env.CorrelationID)
	require.Equal(t, &auditevent.RequestRef{Route: "GET /api/v1/projects/{id}"}, env.Request, "request id differs from correlation, so it is omitted")

	opOnly := auditevent.ContextWithOperation(context.Background(), op)
	env, d = mapDecisionEnvelope(opOnly, inDomainDecisionRecord())
	require.Equal(t, decisionAuditEnqueued, d)
	require.Equal(t, "op-correlation-1", env.CorrelationID)
}

func TestMapDecisionEnvelope_CredentialRule(t *testing.T) {
	// No derived credential: legitimately absent, event emitted.
	rec := inDomainDecisionRecord()
	rec.CredentialType, rec.CredentialID = "", ""
	rec.CredentialBoundaryKind, rec.CredentialBoundaryProjectID = "", ""
	env, d := mapDecisionEnvelope(decisionCtx(), rec)
	require.Equal(t, decisionAuditEnqueued, d)
	require.Nil(t, env.Credential)

	// Present but unrepresentable attribution excludes the whole event.
	for name, mutate := range map[string]func(*store.DecisionAuditRecord){
		"hub_delivery kind":       func(r *store.DecisionAuditRecord) { r.CredentialType = string(CredentialKindHubDelivery) },
		"id without kind":         func(r *store.DecisionAuditRecord) { r.CredentialType = "" },
		"bad boundary kind":       func(r *store.DecisionAuditRecord) { r.CredentialBoundaryKind = "galaxy" },
		"project boundary no id":  func(r *store.DecisionAuditRecord) { r.CredentialBoundaryProjectID = "" },
		"oversized credential id": func(r *store.DecisionAuditRecord) { r.CredentialID = strings.Repeat("c", 129) },
	} {
		rec := inDomainDecisionRecord()
		mutate(rec)
		_, d := mapDecisionEnvelope(decisionCtx(), rec)
		require.Equal(t, decisionAuditExcludedCredential, d, name)
	}
}

func TestMapDecisionEnvelope_ExecutorOnlyWhenKindMaps(t *testing.T) {
	rec := inDomainDecisionRecord()
	rec.ExecutorKind, rec.ExecutorID = "scheduler", "scheduled_event:1"
	env, d := mapDecisionEnvelope(decisionCtx(), rec)
	require.Equal(t, decisionAuditEnqueued, d)
	require.Nil(t, env.Executor)

	rec.ExecutorKind, rec.ExecutorID = "system", "job-1"
	env, d = mapDecisionEnvelope(decisionCtx(), rec)
	require.Equal(t, decisionAuditEnqueued, d)
	require.Equal(t, &auditevent.IdentityRef{Kind: auditevent.IdentitySystem, ID: "job-1"}, env.Executor)
}

// P1-5: the negative domain table. Each row yields its disposition.
func TestMapDecisionEnvelope_NegativeDomain(t *testing.T) {
	cases := []struct {
		name   string
		ctx    context.Context
		mutate func(*store.DecisionAuditRecord)
		want   decisionAuditDisposition
	}{
		{"resource type outside domain", nil, func(r *store.DecisionAuditRecord) { r.ResourceType = "template" }, decisionAuditExcludedResource},
		{"empty resource type", nil, func(r *store.DecisionAuditRecord) { r.ResourceType = "" }, decisionAuditExcludedResource},
		{"parent type", nil, func(r *store.DecisionAuditRecord) { r.ResourceParentType = "project" }, decisionAuditExcludedScope},
		{"parent id", nil, func(r *store.DecisionAuditRecord) { r.ResourceParentID = "proj-1" }, decisionAuditExcludedScope},
		{"ancestry", nil, func(r *store.DecisionAuditRecord) { r.ResourceAncestryLen = 1 }, decisionAuditExcludedScope},
		{"scope kind", nil, func(r *store.DecisionAuditRecord) { r.ResourceScopeKind = "user" }, decisionAuditExcludedScope},
		{"scope user", nil, func(r *store.DecisionAuditRecord) { r.ResourceScopeUserIDSet = true }, decisionAuditExcludedScope},
		{"federated user", nil, func(r *store.DecisionAuditRecord) { r.PrincipalKind = "federated_user" }, decisionAuditExcludedPrincipal},
		{"federated agent", nil, func(r *store.DecisionAuditRecord) { r.PrincipalKind = "federated_agent" }, decisionAuditExcludedPrincipal},
		{"federated service", nil, func(r *store.DecisionAuditRecord) { r.PrincipalKind = "federated_service" }, decisionAuditExcludedPrincipal},
		{"dev", nil, func(r *store.DecisionAuditRecord) { r.PrincipalKind = "dev" }, decisionAuditExcludedPrincipal},
		{"empty principal kind", nil, func(r *store.DecisionAuditRecord) { r.PrincipalKind = "" }, decisionAuditExcludedPrincipal},
		{"empty principal id", nil, func(r *store.DecisionAuditRecord) { r.PrincipalID = "" }, decisionAuditExcludedPrincipal},
		{"empty permission", nil, func(r *store.DecisionAuditRecord) { r.PermissionID = "" }, decisionAuditExcludedPermission},
		{"unregistered permission", nil, func(r *store.DecisionAuditRecord) { r.PermissionID = "project.teleport" }, decisionAuditExcludedPermission},
		{"no correlation", context.Background(), func(*store.DecisionAuditRecord) {}, decisionAuditExcludedCorrelation},
		{"reason over 256", nil, func(r *store.DecisionAuditRecord) { r.Reason = strings.Repeat("r", 257) }, decisionAuditInvalid},
		{"zero timestamp", nil, func(r *store.DecisionAuditRecord) { r.Timestamp = time.Time{} }, decisionAuditInvalid},
		{"unknown result", nil, func(r *store.DecisionAuditRecord) { r.Result = "maybe" }, decisionAuditInvalid},
		{"oversized resource id", nil, func(r *store.DecisionAuditRecord) { r.ResourceID = strings.Repeat("p", 129) }, decisionAuditInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := tc.ctx
			if ctx == nil {
				ctx = decisionCtx()
			}
			rec := inDomainDecisionRecord()
			tc.mutate(rec)
			env, d := mapDecisionEnvelope(ctx, rec)
			require.Equal(t, tc.want, d)
			require.Zero(t, env.EventID)

			// Through the logger: counted, nothing captured.
			sink := auditevent.NewCaptureSink()
			counts := &decisionAuditCounts{}
			l := &decisionAuditLogger{sink: sink, enabled: func() bool { return true }, counts: counts}
			l.EmitDecisionAudit(ctx, rec)
			require.Empty(t, sink.Records())
			require.Equal(t, uint64(1), counts.get(tc.want, rec.Result == "allow"))
		})
	}
	_, d := mapDecisionEnvelope(decisionCtx(), nil)
	require.Equal(t, decisionAuditInvalid, d)
}

type failingAuditSink struct{ calls int }

func (s *failingAuditSink) Emit(context.Context, auditevent.EnvelopeV1) error {
	s.calls++
	return errors.New("queue full")
}

type panickingAuditSink struct{}

func (panickingAuditSink) Emit(context.Context, auditevent.EnvelopeV1) error { panic("sink panic") }

type levelHandler struct {
	slog.Handler
	min slog.Level
}

func (h levelHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.min }

func TestDecisionAuditLogger_Dispositions(t *testing.T) {
	ctx := decisionCtx()

	t.Run("disabled", func(t *testing.T) {
		sink := auditevent.NewCaptureSink()
		counts := &decisionAuditCounts{}
		for _, enabled := range []func() bool{nil, func() bool { return false }} {
			(&decisionAuditLogger{sink: sink, enabled: enabled, counts: counts}).EmitDecisionAudit(ctx, inDomainDecisionRecord())
		}
		require.Empty(t, sink.Records())
		require.Equal(t, uint64(2), counts.get(decisionAuditDisabled, true))
	})
	t.Run("enqueued", func(t *testing.T) {
		sink := auditevent.NewCaptureSink()
		counts := &decisionAuditCounts{}
		(&decisionAuditLogger{sink: sink, enabled: func() bool { return true }, counts: counts}).EmitDecisionAudit(ctx, inDomainDecisionRecord())
		require.Len(t, sink.Records(), 1)
		require.Equal(t, uint64(1), counts.get(decisionAuditEnqueued, true))
	})
	t.Run("sink failure is not_enqueued", func(t *testing.T) {
		sink := &failingAuditSink{}
		counts := &decisionAuditCounts{}
		rec := inDomainDecisionRecord()
		rec.Result = "deny"
		(&decisionAuditLogger{sink: sink, enabled: func() bool { return true }, counts: counts}).EmitDecisionAudit(ctx, rec)
		require.Equal(t, 1, sink.calls)
		require.Equal(t, uint64(1), counts.get(decisionAuditNotEnqueued, false))
	})
	t.Run("handler disabled for level is not_enqueued", func(t *testing.T) {
		sink := auditevent.NewCaptureSink()
		counts := &decisionAuditCounts{}
		h := levelHandler{Handler: slog.DiscardHandler, min: slog.LevelWarn}
		l := &decisionAuditLogger{sink: sink, handler: h, enabled: func() bool { return true }, counts: counts}
		l.EmitDecisionAudit(ctx, inDomainDecisionRecord()) // allow → info, below warn
		require.Empty(t, sink.Records())
		require.Equal(t, uint64(1), counts.get(decisionAuditNotEnqueued, true))
	})
	t.Run("sink panic is recovered as not_enqueued", func(t *testing.T) {
		counts := &decisionAuditCounts{}
		l := &decisionAuditLogger{sink: panickingAuditSink{}, enabled: func() bool { return true }, counts: counts}
		require.NotPanics(t, func() { l.EmitDecisionAudit(ctx, inDomainDecisionRecord()) })
		require.Equal(t, uint64(1), counts.get(decisionAuditNotEnqueued, true))
	})
	t.Run("enabled panic is recovered as invalid", func(t *testing.T) {
		counts := &decisionAuditCounts{}
		l := &decisionAuditLogger{sink: auditevent.NewCaptureSink(), enabled: func() bool { panic("snapshot") }, counts: counts}
		require.NotPanics(t, func() { l.EmitDecisionAudit(ctx, inDomainDecisionRecord()) })
		require.Equal(t, uint64(1), counts.get(decisionAuditInvalid, true))
	})
	t.Run("nil record", func(t *testing.T) {
		counts := &decisionAuditCounts{}
		l := &decisionAuditLogger{sink: auditevent.NewCaptureSink(), enabled: func() bool { return true }, counts: counts}
		require.NotPanics(t, func() { l.EmitDecisionAudit(ctx, nil) })
		require.Equal(t, uint64(1), counts.get(decisionAuditInvalid, false))
	})
}

type recordingDecisionMetrics struct{ got []string }

func (r *recordingDecisionMetrics) RecordDecisionAudit(disposition, result string) {
	r.got = append(r.got, disposition+"/"+result)
}

func TestDecisionAuditCounts_RecorderAndClosedLabelSet(t *testing.T) {
	counts := &decisionAuditCounts{}
	rec := &recordingDecisionMetrics{}
	counts.setRecorder(rec)
	counts.add(decisionAuditExcludedScope, false)
	counts.add(decisionAuditDisposition(200), true) // out of range → invalid
	require.Equal(t, []string{"excluded_scope/deny", "invalid/allow"}, rec.got)

	seen := map[string]bool{}
	for d := decisionAuditDisposition(0); d < decisionAuditDispositionCount; d++ {
		name := d.String()
		require.NotEmpty(t, name)
		require.False(t, seen[name], "duplicate disposition %s", name)
		seen[name] = true
	}
	require.Len(t, seen, 10)
}
