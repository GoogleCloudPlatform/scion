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
	"slices"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/auditevent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// Broker family audit envelopes (remaining-audit P4 slice 1).
//
// LogAuditLogger.LogBrokerAuthEvent routes the five converted event types
// here. Each former legacy line becomes exactly one envelope, emitted
// unconditionally (never gated on the decision-audit experiment) through
// the shared async audit writer. Writer drops are counted by the existing
// scion.logging.write.failures{writer=audit}; there are no new metrics.
//
// An event that cannot be built (no sink, no operation context, an actor
// missing on an action that requires one, or a validation failure) is not
// emitted; one rate-limited operational warning names only the action, the
// drop reason and, when set, the classified failure_code. This is a
// disclosed non-parity case: in production it occurs only on a bug,
// because RequestLog always mints the request ID the operation adopts.

// Drop reasons carried by the operational warning.
const (
	brokerAuditDropNoSink                 = "no_sink"
	brokerAuditDropMissingCorrelation     = "missing_correlation"
	brokerAuditDropMissingActor           = "missing_actor"
	brokerAuditDropUnrepresentableOutcome = "unrepresentable_outcome"
	brokerAuditDropInvalidEnvelope        = "invalid_envelope"
)

// convertedBrokerAction maps a converted legacy event type to its broker
// family action. auth_success, link, unlink, update_settings and revoke are
// not converted.
func convertedBrokerAction(eventType BrokerAuthEventType) (auditevent.BrokerAction, bool) {
	switch eventType {
	case BrokerAuthEventAuthFailure:
		return auditevent.BrokerActionAuthFailure, true
	case BrokerAuthEventRegister:
		return auditevent.BrokerActionRegister, true
	case BrokerAuthEventJoin:
		return auditevent.BrokerActionJoin, true
	case BrokerAuthEventRotate:
		return auditevent.BrokerActionRotate, true
	case BrokerAuthEventDeregister:
		return auditevent.BrokerActionDeregister, true
	}
	return "", false
}

// wireBrokerAuditEnvelopes gives the server's LogAuditLogger a SlogSink
// over the shared async audit writer's handler (the writer
// initDecisionAuditLog creates unconditionally). It must run after that
// writer exists and before brokers and boundary services are wired. A
// custom AuditLogger (SetAuditLogger) is left unchanged.
func (s *Server) wireBrokerAuditEnvelopes() error {
	logger, ok := s.auditLogger.(*LogAuditLogger)
	if !ok || logger == nil || s.auditWriterHandler == nil {
		return nil
	}
	sink, err := auditevent.NewSlogSink(slog.New(s.auditWriterHandler))
	if err != nil {
		return err
	}
	logger.setBrokerEnvelopeSink(sink)
	return nil
}

// setBrokerEnvelopeSink installs the sink for converted broker events. It
// is called once during server construction, before requests are served.
func (l *LogAuditLogger) setBrokerEnvelopeSink(sink auditevent.Sink) {
	l.brokerEnvelopes = sink
}

// emitBrokerEnvelope builds and emits the envelope for one converted
// event. It returns the sink's dispatch error, if any; callers ignore it,
// as they ignore every LogBrokerAuthEvent result.
func (l *LogAuditLogger) emitBrokerEnvelope(ctx context.Context, action auditevent.BrokerAction, event *BrokerAuthEvent) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if l.brokerEnvelopes == nil {
		l.warnBrokerAuditDrop(ctx, action, brokerAuditDropNoSink, event.FailureCode)
		return nil
	}
	input, reason := brokerEnvelopeInput(ctx, action, event)
	if reason != "" {
		l.warnBrokerAuditDrop(ctx, action, reason, input.FailureCode)
		return nil
	}
	envelope, err := auditevent.BuildBrokerEvent(ctx, input)
	if err != nil {
		reason = brokerAuditDropInvalidEnvelope
		var verr *auditevent.ValidationError
		if errors.As(err, &verr) && verr.Field == "operation_context" {
			reason = brokerAuditDropMissingCorrelation
		}
		l.warnBrokerAuditDrop(ctx, action, reason, input.FailureCode)
		return nil
	}
	return l.brokerEnvelopes.Emit(ctx, envelope)
}

// brokerEnvelopeInput maps a legacy event to typed builder input. It reads
// only fields with a declared leaf: never FailReason, Details or
// BrokerName. A non-empty reason means the event must be dropped.
func brokerEnvelopeInput(ctx context.Context, action auditevent.BrokerAction, event *BrokerAuthEvent) (auditevent.BrokerEventInput, string) {
	input := auditevent.BrokerEventInput{
		Action:    action,
		BrokerID:  event.BrokerID,
		SourceIP:  event.IPAddress,
		RequestID: logging.RequestIDFromContext(ctx),
	}
	switch action {
	case auditevent.BrokerActionAuthFailure:
		input.FailureCode = classifiedOrOther(event.FailureCode)
		// user_agent is carried on auth_failure only, the one legacy event
		// that had it (Q6).
		input.UserAgent = event.UserAgent
	case auditevent.BrokerActionJoin:
		if !event.Success {
			input.Failed = true
			input.FailureCode = classifiedOrOther(event.FailureCode)
		}
	default: // register, rotate, deregister
		if !event.Success {
			// No production site emits a failed register, rotate or
			// deregister; the catalog admits only commit/succeeded.
			return input, brokerAuditDropUnrepresentableOutcome
		}
		principal, ok := brokerEnvelopePrincipal(event)
		if !ok {
			return input, brokerAuditDropMissingActor
		}
		input.Principal = principal
		if action == auditevent.BrokerActionRegister {
			input.Operation = event.RegisterOperation
			input.ProjectID = event.ProjectID
		}
	}
	input.Credential, input.CredentialUnrepresentable = brokerEnvelopeCredential(ctx)
	return input, ""
}

// classifiedOrOther returns code when it is a declared failure code, else
// other: anything unmapped becomes other (S1.4).
func classifiedOrOther(code auditevent.BrokerFailureCode) auditevent.BrokerFailureCode {
	if slices.Contains(auditevent.BrokerFailureCodes(), code) {
		return code
	}
	return auditevent.BrokerFailureOther
}

// brokerEnvelopePrincipal maps the legacy actor to the envelope principal
// (Q8 as amended by R14). Every empty actor ID, embedded register
// included, and every actor type other than user or broker is a bug path:
// the event is dropped with the rate-limited warning. There is no system
// fallback; a user-initiated action is never attributed to the hub.
func brokerEnvelopePrincipal(event *BrokerAuthEvent) (*auditevent.IdentityRef, bool) {
	if event.ActorID == "" {
		return nil, false
	}
	switch event.ActorType {
	case "user":
		return &auditevent.IdentityRef{Kind: auditevent.IdentityUser, ID: event.ActorID}, true
	case "broker":
		return &auditevent.IdentityRef{Kind: auditevent.IdentityBroker, ID: event.ActorID}, true
	}
	return nil, false
}

// brokerEnvelopeCredential builds the credential leaf from the request
// credential on ctx (Q5 as amended by R12): kind, id, boundary_kind and
// boundary_project_id only; the decoration's user-chosen name and labels
// are never included. A credential that does not validate is reported as
// unrepresentable (credential_kind=unrepresentable, no credential leaf).
func brokerEnvelopeCredential(ctx context.Context) (*auditevent.CredentialRef, bool) {
	actor := auditActorFromContext(ctx)
	if actor.CredentialKind == "" {
		return nil, false
	}
	ref, err := auditevent.NewCredentialRef(auditevent.CredentialRefInput{
		Kind:              auditevent.CredentialKind(actor.CredentialKind),
		ID:                actor.CredentialID,
		BoundaryKind:      auditevent.CredentialBoundaryKind(actor.CredentialBoundaryKind),
		BoundaryProjectID: actor.CredentialBoundaryProjectID,
	})
	if err != nil {
		return nil, true
	}
	return &ref, false
}

// brokerAuthFailureCode classifies a ValidateBrokerSignature error (and
// the broker admission refusal it can return) without matching text.
func brokerAuthFailureCode(err error) auditevent.BrokerFailureCode {
	switch {
	case errors.Is(err, errBrokerAuthMissingBrokerID), errors.Is(err, errBrokerAuthMissingTimestamp),
		errors.Is(err, errBrokerAuthMissingSignature), errors.Is(err, errBrokerAuthMissingNonce):
		return auditevent.BrokerFailureMissingCredentials
	case errors.Is(err, errBrokerAuthMalformedTimestamp), errors.Is(err, apiclient.ErrUnsizedSignedBody):
		return auditevent.BrokerFailureMalformed
	case errors.Is(err, errBrokerAuthBodyTooLarge):
		// D298: a body over apiclient.MaxSignedRequestBodyBytes (answered
		// 413 payload_too_large) is recorded as malformed, so the closed
		// failure_code enum of audit ruling R12 is not reopened.
		return auditevent.BrokerFailureMalformed
	case errors.Is(err, errBrokerAuthClockSkew):
		return auditevent.BrokerFailureClockSkew
	case errors.Is(err, errBrokerAuthReplay):
		return auditevent.BrokerFailureReplay
	case (errors.Is(err, errBrokerAuthSecretLookup) || errors.Is(err, errBrokerAuthSecretsLookup)) && errors.Is(err, store.ErrInvalidInput):
		// The store rejects a claimed broker ID that cannot be a broker key
		// (not a UUID) with ErrInvalidInput, wrapped under the lookup
		// sentinel. That is a caller-supplied unknown broker, not a hub
		// fault: classifying it internal would let any unauthenticated
		// caller fill the internal bucket at will. Error() text and the
		// order of the checks are unchanged; only the audit code differs.
		return auditevent.BrokerFailureUnknownBroker
	case errors.Is(err, errBrokerAuthNonceCheck), errors.Is(err, errBrokerAuthSecretLookup), errors.Is(err, errBrokerAuthSecretsLookup):
		return auditevent.BrokerFailureInternal
	case errors.Is(err, errBrokerAuthUnknownBroker), errors.Is(err, errBrokerAuthNoValidSecrets):
		return auditevent.BrokerFailureUnknownBroker
	case errors.Is(err, errBrokerAuthSecretInactive):
		return auditevent.BrokerFailureRevokedOrDisabled
	case errors.Is(err, errBrokerAuthSecretExpired):
		return auditevent.BrokerFailureExpired
	case errors.Is(err, errBrokerAuthInvalidSignature):
		return auditevent.BrokerFailureInvalidSignature
	case errors.Is(err, ErrTestAdminRemoteBrokerRefused):
		return auditevent.BrokerFailureRefused
	}
	return auditevent.BrokerFailureOther
}

// brokerJoinFailureCode classifies a CompleteBrokerJoin error in the order
// handleBrokerJoin answers it: a runtime target refusal or the test admin
// refusal is refused; an invalid or mismatched token is token_invalid; an
// expired token is token_expired; everything else is the 5xx default,
// internal.
func brokerJoinFailureCode(err error) auditevent.BrokerFailureCode {
	var refusal *RuntimeTargetRefusal
	switch {
	case errors.As(err, &refusal), errors.Is(err, ErrTestAdminRemoteBrokerRefused):
		return auditevent.BrokerFailureRefused
	case errors.Is(err, ErrJoinTokenInvalid), errors.Is(err, ErrJoinTokenBrokerMismatch):
		return auditevent.BrokerFailureTokenInvalid
	case errors.Is(err, ErrJoinTokenExpired):
		return auditevent.BrokerFailureTokenExpired
	}
	return auditevent.BrokerFailureInternal
}

// brokerAuditDropWarnInterval is the minimum gap between two warnings for
// the same action and drop reason.
const brokerAuditDropWarnInterval = time.Minute

// auditDropWarner rate-limits the operational warning for a dropped
// converted event to one per (action, reason) per interval. The key space
// is finite (five actions, five reasons).
type auditDropWarner struct {
	mu       sync.Mutex
	last     map[string]time.Time
	interval time.Duration
	now      func() time.Time
}

func newAuditDropWarner() *auditDropWarner {
	return &auditDropWarner{last: map[string]time.Time{}, interval: brokerAuditDropWarnInterval, now: time.Now}
}

// defaultBrokerAuditDropWarner serves a LogAuditLogger built as a literal
// (tests), which has no warner of its own.
var defaultBrokerAuditDropWarner = newAuditDropWarner()

// allow reports whether a warning for key may be logged now, and records
// it when so.
func (w *auditDropWarner) allow(key string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	if last, ok := w.last[key]; ok && now.Sub(last) < w.interval {
		return false
	}
	w.last[key] = now
	return true
}

// warnBrokerAuditDrop logs the rate-limited warning for a dropped
// converted event. It carries the action, the drop reason and a declared
// failure_code only, never any payload value.
func (l *LogAuditLogger) warnBrokerAuditDrop(ctx context.Context, action auditevent.BrokerAction, reason string, code auditevent.BrokerFailureCode) {
	warner := l.brokerDropWarn
	if warner == nil {
		warner = defaultBrokerAuditDropWarner
	}
	if !warner.allow(string(action) + "/" + reason) {
		return
	}
	attrs := []slog.Attr{
		slog.String("action", string(action)),
		slog.String("reason", reason),
	}
	if slices.Contains(auditevent.BrokerFailureCodes(), code) {
		attrs = append(attrs, slog.String("failure_code", string(code)))
	}
	l.logger().LogAttrs(ctx, slog.LevelWarn, "Broker audit event dropped", attrs...)
}
