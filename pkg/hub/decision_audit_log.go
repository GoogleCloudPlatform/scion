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
	"errors"
	"log/slog"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/auditevent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/asyncwrite"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// Decision logging (remaining-audit P1, design §3 as corrected by C1-C3).
//
// Decide → emitDecisionAudit (sampling unchanged) → optional perf-trace
// decorator → decisionAuditLogger.EmitDecisionAudit. The logger records
// only: it never changes a Decision, never blocks on I/O and never calls the
// store. The only activation control is the registered server-layer
// experiment hub.authorization_decision_audit_v2 (default off), evaluated
// per record. In-domain records are mapped to the existing
// authorization/decide envelope and handed to an auditevent.SlogSink over a
// logging.AsyncHandler, which validates, snapshots and enqueues without
// blocking. Every record that is not enqueued is counted by disposition,
// never logged.

// decisionAuditDisposition is the closed set of per-record outcomes
// (design C1.4). Each is counted together with the decision result.
type decisionAuditDisposition uint8

const (
	// decisionAuditEnqueued: valid, in domain, accepted by the async writer.
	decisionAuditEnqueued decisionAuditDisposition = iota
	// decisionAuditNotEnqueued: valid and in domain, but the sink did not
	// accept it (writer full, closed or rejected, or the handler disabled
	// for the level). The writer counts the writer-side reason.
	decisionAuditNotEnqueued
	// decisionAuditDisabled: the experiment is off (the status quo).
	decisionAuditDisabled
	// decisionAuditExcludedResource: resource type outside {project, agent}.
	decisionAuditExcludedResource
	// decisionAuditExcludedScope: the resource carries containment or a
	// scope classification, so it is not system-scoped. Never relabeled.
	decisionAuditExcludedScope
	// decisionAuditExcludedPrincipal: principal kind is not user, agent or
	// broker, or the principal ID is empty. No identity is fabricated.
	decisionAuditExcludedPrincipal
	// decisionAuditExcludedPermission: the permission ID is not a
	// registered permission.
	decisionAuditExcludedPermission
	// decisionAuditExcludedCredential: the decision carries credential
	// attribution that cannot be represented as a canonical credential
	// reference (for example hub_delivery). The whole event is excluded so
	// credential attribution is never silently dropped.
	decisionAuditExcludedCredential
	// decisionAuditExcludedCorrelation: no operation context and no
	// request ID. No ID is minted here.
	decisionAuditExcludedCorrelation
	// decisionAuditInvalid: the envelope failed auditevent validation (for
	// example a reason over 256 bytes), the record was nil or had an
	// unknown result, or mapping panicked. Never truncated; the error text
	// is not logged.
	decisionAuditInvalid

	decisionAuditDispositionCount
)

var decisionAuditDispositionNames = [decisionAuditDispositionCount]string{
	decisionAuditEnqueued:            "enqueued",
	decisionAuditNotEnqueued:         "not_enqueued",
	decisionAuditDisabled:            "disabled",
	decisionAuditExcludedResource:    "excluded_resource",
	decisionAuditExcludedScope:       "excluded_scope",
	decisionAuditExcludedPrincipal:   "excluded_principal",
	decisionAuditExcludedPermission:  "excluded_permission",
	decisionAuditExcludedCredential:  "excluded_credential",
	decisionAuditExcludedCorrelation: "excluded_correlation",
	decisionAuditInvalid:             "invalid",
}

func (d decisionAuditDisposition) String() string {
	if d < decisionAuditDispositionCount {
		return decisionAuditDispositionNames[d]
	}
	return "invalid"
}

// Decision results used as the metric label. A record whose Result is not
// "allow" is counted under "deny" (Decide only produces allow or deny; any
// other value is additionally dispositioned invalid).
const (
	decisionAuditResultAllow = "allow"
	decisionAuditResultDeny  = "deny"
)

// DecisionAuditMetricsRecorder mirrors decision-log disposition counts into
// an external metrics system. It must be cheap, non-blocking and must not
// panic.
type DecisionAuditMetricsRecorder interface {
	RecordDecisionAudit(disposition, result string)
}

// decisionAuditCounts holds the always-present in-process counts by
// (disposition, result), at most 10 × 2 series, plus an optional recorder.
type decisionAuditCounts struct {
	counts   [decisionAuditDispositionCount][2]atomic.Uint64
	recorder atomic.Pointer[decisionAuditRecorderBox]
}

type decisionAuditRecorderBox struct{ r DecisionAuditMetricsRecorder }

func (c *decisionAuditCounts) add(d decisionAuditDisposition, allow bool) {
	if c == nil {
		return
	}
	if d >= decisionAuditDispositionCount {
		d = decisionAuditInvalid
	}
	result, idx := decisionAuditResultDeny, 1
	if allow {
		result, idx = decisionAuditResultAllow, 0
	}
	c.counts[d][idx].Add(1)
	if box := c.recorder.Load(); box != nil {
		box.r.RecordDecisionAudit(d.String(), result)
	}
}

// get returns the in-process count for (d, allow).
func (c *decisionAuditCounts) get(d decisionAuditDisposition, allow bool) uint64 {
	idx := 1
	if allow {
		idx = 0
	}
	return c.counts[d][idx].Load()
}

func (c *decisionAuditCounts) setRecorder(r DecisionAuditMetricsRecorder) {
	if r == nil {
		c.recorder.Store(nil)
		return
	}
	c.recorder.Store(&decisionAuditRecorderBox{r: r})
}

// decisionAuditLogger is the production DecisionAuditEmitter.
type decisionAuditLogger struct {
	sink auditevent.Sink
	// handler is the sink's handler, consulted for the record's level so a
	// record the handler would discard is counted not_enqueued rather than
	// enqueued (SlogSink.Emit returns nil for a disabled level). Optional.
	handler slog.Handler
	// enabled reports whether the experiment is on; nil means off.
	enabled func() bool
	counts  *decisionAuditCounts
}

var _ DecisionAuditEmitter = (*decisionAuditLogger)(nil)

// EmitDecisionAudit maps and enqueues one decision record, or counts why it
// was not. It recovers every panic and never blocks.
func (l *decisionAuditLogger) EmitDecisionAudit(ctx context.Context, rec *store.DecisionAuditRecord) {
	allow := rec != nil && rec.Result == decisionAuditResultAllow
	disposition := decisionAuditInvalid
	stage := 0 // 0 mapping, 1 dispatch
	defer func() {
		if recover() != nil {
			if stage == 1 {
				disposition = decisionAuditNotEnqueued
			} else {
				disposition = decisionAuditInvalid
			}
		}
		l.countSafely(disposition, allow)
	}()

	if l.enabled == nil || !l.enabled() {
		disposition = decisionAuditDisabled
		return
	}
	env, d := mapDecisionEnvelope(ctx, rec)
	if d != decisionAuditEnqueued {
		disposition = d
		return
	}
	stage = 1
	level := slog.LevelInfo
	if env.Severity == auditevent.SeverityWarning {
		level = slog.LevelWarn
	}
	if l.handler != nil && !l.handler.Enabled(ctx, level) {
		disposition = decisionAuditNotEnqueued
		return
	}
	if l.sink == nil || l.sink.Emit(ctx, env) != nil {
		disposition = decisionAuditNotEnqueued
		return
	}
	disposition = decisionAuditEnqueued
}

// countSafely records the disposition. The in-process count is always
// taken; a recorder that violates its no-panic contract is contained here
// so it can never propagate into Decide (defence in depth).
func (l *decisionAuditLogger) countSafely(d decisionAuditDisposition, allow bool) {
	defer func() { _ = recover() }()
	l.counts.add(d, allow)
}

// mapDecisionEnvelope maps an in-domain decision record to the
// authorization/decide envelope. It returns decisionAuditEnqueued (meaning
// "mappable") with the validated envelope, or the exclusion/invalid
// disposition with a zero envelope. It performs no I/O.
func mapDecisionEnvelope(ctx context.Context, rec *store.DecisionAuditRecord) (auditevent.EnvelopeV1, decisionAuditDisposition) {
	var zero auditevent.EnvelopeV1
	if rec == nil {
		return zero, decisionAuditInvalid
	}
	outcome, severity := auditevent.OutcomeAllow, auditevent.SeverityInfo
	switch rec.Result {
	case decisionAuditResultAllow:
	case decisionAuditResultDeny:
		outcome, severity = auditevent.OutcomeDeny, auditevent.SeverityWarning
	default:
		return zero, decisionAuditInvalid
	}

	if rec.ResourceType != "project" && rec.ResourceType != "agent" {
		return zero, decisionAuditExcludedResource
	}
	if rec.ResourceParentType != "" || rec.ResourceParentID != "" || rec.ResourceAncestryLen != 0 ||
		rec.ResourceScopeKind != "" || rec.ResourceScopeUserIDSet {
		return zero, decisionAuditExcludedScope
	}

	principalKind, ok := decisionAuditIdentityKind(rec.PrincipalKind)
	if !ok || rec.PrincipalID == "" {
		return zero, decisionAuditExcludedPrincipal
	}

	if !isKnownPermission(rec.PermissionID) {
		return zero, decisionAuditExcludedPermission
	}

	requestID := logging.RequestIDFromContext(ctx)
	correlationID := requestID
	if op, ok := auditevent.OperationFromContext(ctx); ok {
		correlationID = op.CorrelationID
	}
	if correlationID == "" {
		return zero, decisionAuditExcludedCorrelation
	}

	var credential *auditevent.CredentialRef
	if rec.CredentialType != "" || rec.CredentialID != "" {
		ref, err := auditevent.NewCredentialRef(auditevent.CredentialRefInput{
			Kind:              auditevent.CredentialKind(rec.CredentialType),
			ID:                rec.CredentialID,
			BoundaryKind:      auditevent.CredentialBoundaryKind(rec.CredentialBoundaryKind),
			BoundaryProjectID: rec.CredentialBoundaryProjectID,
		})
		if err != nil {
			return zero, decisionAuditExcludedCredential
		}
		credential = &ref
	}

	env := auditevent.EnvelopeV1{
		SchemaVersion: auditevent.SchemaVersion,
		EventID:       uuid.NewString(),
		OccurredAt:    rec.Timestamp.UTC(),
		Family:        "authorization",
		Action:        "decide",
		Phase:         auditevent.PhaseDecision,
		Outcome:       outcome,
		Severity:      severity,
		CorrelationID: correlationID,
		Principal:     &auditevent.IdentityRef{Kind: principalKind, ID: rec.PrincipalID},
		Credential:    credential,
		Resource:      &auditevent.ResourceRef{Kind: rec.ResourceType, ID: rec.ResourceID, Scope: auditevent.ResourceScopeSystem},
		Payload: auditevent.AuthorizationDecisionPayload{
			PermissionID: rec.PermissionID,
			Permission:   rec.Permission,
			Reason:       rec.Reason,
			DeniedBy:     rec.DeniedBy,
			Sampled:      strconv.FormatBool(rec.Sampled),
		},
	}
	request := &auditevent.RequestRef{Route: rec.Route}
	if requestID == correlationID {
		request.ID = requestID
	}
	if request.ID != "" || request.Route != "" {
		env.Request = request
	}
	if kind, ok := decisionAuditExecutorKind(rec.ExecutorKind); ok && rec.ExecutorID != "" {
		env.Executor = &auditevent.IdentityRef{Kind: kind, ID: rec.ExecutorID}
	}
	if err := auditevent.Validate(env); err != nil {
		return zero, decisionAuditInvalid
	}
	return env, decisionAuditEnqueued
}

// decisionAuditIdentityKind maps a decision principal kind to the envelope
// identity kind. Federated, dev and unknown kinds do not map.
func decisionAuditIdentityKind(kind string) (auditevent.IdentityKind, bool) {
	switch PrincipalKind(kind) {
	case PrincipalKindUser:
		return auditevent.IdentityUser, true
	case PrincipalKindAgent:
		return auditevent.IdentityAgent, true
	case PrincipalKindBroker:
		return auditevent.IdentityBroker, true
	default:
		return "", false
	}
}

// decisionAuditExecutorKind maps an executor kind only when it is literally
// a declared envelope identity kind. Subsystem executor kinds (scheduler,
// broker_dispatch, ...) do not map and the optional executor leaf is
// omitted.
func decisionAuditExecutorKind(kind string) (auditevent.IdentityKind, bool) {
	switch k := auditevent.IdentityKind(kind); k {
	case auditevent.IdentityUser, auditevent.IdentityAgent, auditevent.IdentityBroker,
		auditevent.IdentitySystem, auditevent.IdentityProject:
		return k, true
	default:
		return "", false
	}
}

// MetricDecisionAuditRecords is the decision-log disposition counter.
const MetricDecisionAuditRecords = "scion.hub.authz.decision_audit.records"

// OTelDecisionAuditMetrics counts decision-log dispositions as
// scion.hub.authz.decision_audit.records{disposition, result}. There are no
// principal, resource, route or project labels.
type OTelDecisionAuditMetrics struct {
	records metric.Int64Counter
}

// NewOTelDecisionAuditMetrics creates the counter on mp.
func NewOTelDecisionAuditMetrics(mp metric.MeterProvider) (*OTelDecisionAuditMetrics, error) {
	if mp == nil {
		return nil, errors.New("otel decision audit metrics: nil MeterProvider")
	}
	c, err := mp.Meter(instrumentationScope).Int64Counter(MetricDecisionAuditRecords,
		metric.WithUnit("{record}"),
		metric.WithDescription("Authorization decision records seen by the decision log, by disposition "+
			"(enqueued, not_enqueued, disabled, excluded_*, invalid) and result (allow, deny). "+
			"Counted even while decision logging is disabled."))
	if err != nil {
		return nil, err
	}
	return &OTelDecisionAuditMetrics{records: c}, nil
}

// RecordDecisionAudit implements DecisionAuditMetricsRecorder.
func (m *OTelDecisionAuditMetrics) RecordDecisionAudit(disposition, result string) {
	m.records.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("disposition", disposition),
		attribute.String("result", result),
	))
}

// auditWriterName is the bounded writer label of the audit writer.
const auditWriterName = "audit"

// auditLogWriterHealthKey is the non-critical /healthz key for the audit
// writer. It is outside criticalHealthChecks: a degraded value makes the
// composite status degraded (serving), never unhealthy, and readiness is
// independent of it.
const auditLogWriterHealthKey = "audit_log_writer"

// initDecisionAuditLog builds the audit writer and the decision logger.
// The inner handler is the process default handler captured now, as the
// boundary services do (slog.SetDefault has already run in production).
func (s *Server) initDecisionAuditLog() error {
	w, err := logging.NewAsyncWriter(asyncwrite.Config{Name: auditWriterName})
	if err != nil {
		return err
	}
	h := logging.NewAsyncHandler(slog.Default().Handler(), w)
	sink, err := auditevent.NewSlogSink(slog.New(h))
	if err != nil {
		_ = w.Close(context.Background())
		return err
	}
	s.auditWriter = w
	s.decisionAuditLogger = &decisionAuditLogger{
		sink:    sink,
		handler: h,
		enabled: func() bool { return s.experimentEnabled(experiments.AuthorizationDecisionAuditV2) },
		counts:  &decisionAuditCounts{},
	}
	return nil
}

// checkAuditWriterHealth reports the audit writer as a fixed string:
// "healthy", "degraded: recent write failures" (a failure within the last
// 5 minutes), "degraded: writer stalled" (a write in flight past its 2s
// budget) or "degraded: writer closed".
func (s *Server) checkAuditWriterHealth(checks map[string]string) {
	if s.auditWriter == nil {
		return
	}
	h := s.auditWriter.Health(time.Now())
	if h.Healthy {
		checks[auditLogWriterHealthKey] = HealthStatusHealthy
		return
	}
	checks[auditLogWriterHealthKey] = HealthStatusDegraded + ": " + h.Reason.String()
}

// SetAuditWriterMetrics attaches the OTel logging write metrics to the
// audit writer (failures, records, late returns, queue depth, stalled).
func (s *Server) SetAuditWriterMetrics(m *logging.WriteMetrics) {
	if m == nil || s.auditWriter == nil {
		return
	}
	s.auditWriter.SetRecorder(m)
	m.Observe(s.auditWriter)
}

// SetDecisionAuditMetrics attaches the OTel decision-log disposition
// counter. In-process counts exist regardless.
func (s *Server) SetDecisionAuditMetrics(m *OTelDecisionAuditMetrics) {
	if m == nil || s.decisionAuditLogger == nil {
		return
	}
	s.decisionAuditLogger.counts.setRecorder(m)
}
