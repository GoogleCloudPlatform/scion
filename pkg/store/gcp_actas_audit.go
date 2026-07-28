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

package store

import (
	"context"
	"log/slog"
	"time"
)

// This file defines the audit record for service-account assignment (svc-accnt
// design §7). It lives in package store for the same reason gcp_actas.go does:
// pkg/hub and pkg/lifecyclehooks both produce these records, and store is the
// common ancestor in the import graph.

// SAAssignmentEventType distinguishes the two kinds of record, which are NOT
// interchangeable and must never be collapsed into one.
type SAAssignmentEventType string

const (
	// SAAssignmentDecision is a record of an authorization decision: a caller
	// asked to bind a service account it supplied, and the actAs gate reached
	// a verdict. These records carry a Decision.
	SAAssignmentDecision SAAssignmentEventType = "sa_assignment_decision"

	// SAAssignmentBinding is a record that a service account was bound to an
	// agent WITHOUT any authorization decision being made, because the account
	// was not caller-supplied and there was therefore no caller privilege to
	// evaluate. These records carry NO Decision.
	//
	// The distinction is load-bearing. See SAAssignmentEvent.Decision.
	SAAssignmentBinding SAAssignmentEventType = "sa_binding"
)

// MechanismProjectDefault names why a binding record has no decision attached.
//
// It deliberately states the REASON there is no gate, not merely the fact.
// "ungated" would read as a gap and invite someone to close it; the account
// here comes from project settings rather than from the caller, so there is no
// caller-elected privilege to authorize and nothing to check. Gating it would
// turn a project-admin decision into a per-caller lottery and break routine
// agent creation (design §8, P4 item F).
const MechanismProjectDefault = "project-default-not-caller-supplied"

// SurfaceUnnamed marks a record whose producer did not name its surface. No
// correct call site emits this; it exists so the failure is alertable instead
// of arriving as an empty string that reads like a formatting artefact.
const SurfaceUnnamed = "surface-unnamed"

// SAAssignmentEvent is one audit record for a service-account assignment.
//
// ⚠️ THIS IS NOT A WIRE TYPE AND CARRIES NO JSON TAGS, DELIBERATELY. Sinks
// translate it field by field. A struct that is half-serialisable invites a
// future sink to json.Marshal it and silently drop whichever fields were tagged
// "-", and the field most likely to be dropped that way is the decision itself.
// Making it obviously not-marshalable forces a sink author to look at every
// field, which is the outcome we want for an audit record.
type SAAssignmentEvent struct {
	// Type selects which kind of record this is. Always set.
	Type SAAssignmentEventType

	// Surface names the code path that produced the record — see the Surface
	// constants in pkg/hub and pkg/lifecyclehooks. Always set.
	Surface string

	// Caller is the principal evaluated, or for binding records the principal
	// that triggered the binding without being evaluated.
	Caller Principal

	// TargetSAID and TargetSAEmail identify the account being assigned. Both
	// are recorded because the ID is stable and the email is what a human
	// reading an IAM policy will recognise.
	TargetSAID    string
	TargetSAEmail string

	// Permission is the IAM permission checked — PermissionActAs. It is EMPTY
	// on binding records, because no permission was checked. An audit record
	// naming a permission it never evaluated asserts a check that did not
	// happen.
	Permission string

	// Mechanism names what produced this record: the check that reached the
	// verdict on a decision record, or MechanismProjectDefault on a binding
	// record. Never empty.
	Mechanism string

	// Decision is the verdict, and is nil ON BINDING RECORDS ONLY.
	//
	// ⚠️ NIL MEANS "NO DECISION WAS MADE", AND IT IS NOT A DENIAL. Do not
	// deref this without a nil check, and do not substitute the zero value:
	// ActAsOutcome's zero value is ActAsIndeterminate, which DENIES. If a
	// binding record were given Indeterminate to fill the slot, then anyone
	// who later wires enforcement to these records — a reasonable thing to do,
	// since this is where the decisions are — would fail the project-default
	// path closed and break routine agent creation. The nil is what stops an
	// audit change from becoming an outage.
	//
	// The honest reading of a binding record is the one used at 2836b685: no
	// verdict was obtained, and this is not a statement about the caller's
	// permissions — here because no caller was consulted, by design.
	Decision *ActAsOutcome

	// Reason is the human-readable explanation from the decision, or the
	// explanation of why no decision was required.
	Reason string

	// CacheHit reports whether the decision was served from a cached result.
	//
	// ⚠️ NIL MEANS NO CACHE WAS CONSULTED, WHICH IS NOT THE SAME AS FALSE.
	// False asserts that a cache was checked and missed — i.e. that a live IAM
	// call was made. Today no caller-permission cache exists at all, so this is
	// nil on every record the current tree produces, and sinks MUST OMIT the
	// attribute entirely rather than serialise it as null or coerce it to
	// false: a lenient consumer renders null as false, which fabricates the
	// cache miss the nil exists to avoid.
	//
	// The field is present now so the eventual real checker can populate it
	// without a schema change.
	CacheHit *bool

	// Timestamp is when the record was produced.
	Timestamp time.Time
}

// SAAssignmentAuditSink receives service-account assignment audit records.
//
// It is a single-method interface in package store so that pkg/lifecyclehooks
// can emit without importing pkg/hub, which would be an import cycle.
type SAAssignmentAuditSink interface {
	RecordSAAssignment(ctx context.Context, event *SAAssignmentEvent) error
}

// ActAsGate bundles everything a surface needs to run the actAs gate and have
// the decision audited.
//
// It is a struct rather than four more parameters because every field is
// required at every call site, and because a future field — a cache handle, a
// request id — should not churn two packages and thirty tests to add.
type ActAsGate struct {
	// Checker is the caller-permission checker. Nil is a wiring bug and DENIES;
	// switching the check off is done with NewDisabledCallerPermissionChecker.
	Checker CallerPermissionChecker

	// Caller is the principal to evaluate.
	Caller Principal

	// Surface names the calling code path for the audit record.
	Surface string

	// Audit receives the record. Nil is tolerated but warns — see
	// EvaluateActAs.
	Audit SAAssignmentAuditSink
}

// EvaluateActAs runs the shared actAs decision sequence AND emits the audit
// record for it. It is the only exported entry point, deliberately: a surface
// in another package cannot reach the decision without also producing the
// record, because the pure sequence is unexported. Future surfaces inherit the
// audit trail by construction rather than by remembering to add it (design §7).
//
// ⚠️ THAT IS A GUARANTEE ABOUT OTHER PACKAGES ONLY. Within pkg/store,
// evaluateActAs is directly callable and the property holds by the absence of
// in-package callers rather than by enforcement. Known good today, with the
// precondition named at evaluateActAs — read it before adding a caller here.
//
// The decision logic itself is evaluateActAs, which is unchanged and remains
// pure and directly testable. This function adds recording, and only recording.
//
// The record is emitted on ALLOW, DENY and INDETERMINATE alike. Omitting
// denials is the classic way an audit trail comes to show only the traffic that
// succeeded.
//
// A NIL SINK WARNS AND PROCEEDS; it does not deny. This is deliberately
// asymmetric with a nil Checker, which denies, and the asymmetry is the point:
// a missing checker means we do not know whether the caller is permitted, so
// the only safe answer is no. A missing sink means we DO know the answer and
// merely cannot file it. Failing closed there would convert a logging
// misconfiguration into an authorization outage, which is a strictly worse
// trade than a loud warning.
func EvaluateActAs(
	ctx context.Context,
	gate ActAsGate,
	targetSA *GCPServiceAccount,
) (ActAsResult, error) {
	result, err := evaluateActAs(ctx, gate.Checker, gate.Caller, targetSA)

	decision := result.Outcome
	event := &SAAssignmentEvent{
		Type:       SAAssignmentDecision,
		Surface:    gate.Surface,
		Caller:     gate.Caller,
		Permission: PermissionActAs,
		Mechanism:  result.Mechanism,
		Decision:   &decision,
		Reason:     result.Reason,
		// CacheHit stays nil: no caller-permission cache exists yet. See the
		// field comment — nil is not false.
		Timestamp: time.Now(),
	}
	if targetSA != nil {
		event.TargetSAID = targetSA.ID
		event.TargetSAEmail = targetSA.Email
	}

	emitSAAssignmentEvent(ctx, gate.Audit, event)

	return result, err
}

// RecordSABinding emits a binding record: a service account was bound to an
// agent with no authorization decision, because the account was not
// caller-supplied.
//
// This exists as its own function, rather than as a flag on EvaluateActAs,
// because there is no decision to make at such a site and no checker to
// consult. Routing it through the decision path would require inventing an
// ActAsOutcome for it, and every available value would be a lie — see
// SAAssignmentEvent.Decision.
func RecordSABinding(
	ctx context.Context,
	sink SAAssignmentAuditSink,
	surface string,
	caller Principal,
	targetSA *GCPServiceAccount,
	reason string,
) {
	event := &SAAssignmentEvent{
		Type:    SAAssignmentBinding,
		Surface: surface,
		Caller:  caller,
		// Permission and Decision are deliberately left unset. Nothing was
		// checked and nothing was decided.
		Mechanism: MechanismProjectDefault,
		Reason:    reason,
		Timestamp: time.Now(),
	}
	if targetSA != nil {
		event.TargetSAID = targetSA.ID
		event.TargetSAEmail = targetSA.Email
	}

	emitSAAssignmentEvent(ctx, sink, event)
}

// emitSAAssignmentEvent delivers a record, warning loudly if there is nowhere
// to deliver it or if delivery fails. An audit record that could not be filed
// is itself worth a log line: silence here would mean the absence of records
// looked identical to the absence of events, which is the failure mode the
// whole of §7 exists to close.
func emitSAAssignmentEvent(ctx context.Context, sink SAAssignmentAuditSink, event *SAAssignmentEvent) {
	// A record that cannot say which surface produced it is the same failure as
	// an allow that cannot say which check produced it: the information is only
	// ever wanted after the fact, when it can no longer be recovered. Named
	// rather than left blank so it is alertable, and not fatal — refusing the
	// decision over a missing label would be an audit change causing an outage.
	if event.Surface == "" {
		slog.WarnContext(ctx, "SA assignment audit record has no surface; this is a caller bug",
			"mechanism", event.Mechanism, "target_sa_id", event.TargetSAID)
		event.Surface = SurfaceUnnamed
	}

	if sink == nil {
		slog.WarnContext(ctx,
			"SA assignment audit sink is not wired; decision proceeded but was NOT recorded",
			"surface", event.Surface,
			"mechanism", event.Mechanism,
			"target_sa_id", event.TargetSAID)
		return
	}
	if err := sink.RecordSAAssignment(ctx, event); err != nil {
		slog.WarnContext(ctx, "failed to record SA assignment audit event",
			"surface", event.Surface,
			"mechanism", event.Mechanism,
			"target_sa_id", event.TargetSAID,
			"error", err)
	}
}
