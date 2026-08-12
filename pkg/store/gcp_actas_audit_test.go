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
	"errors"
	"testing"
)

// The audit record is a deliverable of design §7, not decoration. These tests
// assert the two properties that make it worth having: it is emitted on every
// outcome including denials, and it never claims something that did not happen.

type recordingSink struct {
	events []*SAAssignmentEvent
	err    error
}

func (r *recordingSink) RecordSAAssignment(_ context.Context, e *SAAssignmentEvent) error {
	r.events = append(r.events, e)
	return r.err
}

func (r *recordingSink) only(t *testing.T) *SAAssignmentEvent {
	t.Helper()
	if len(r.events) != 1 {
		t.Fatalf("expected exactly 1 audit event, got %d", len(r.events))
	}
	return r.events[0]
}

func auditGate(checker CallerPermissionChecker, caller Principal, sink SAAssignmentAuditSink) ActAsGate {
	return ActAsGate{Checker: checker, Caller: caller, Surface: "test-surface", Audit: sink}
}

// ---------------------------------------------------------------------------
// Emitted on every outcome
// ---------------------------------------------------------------------------

// TestAudit_EmittedOnAllow — an audit trail that records only refusals cannot
// answer "who was given this account".
func TestAudit_EmittedOnAllow(t *testing.T) {
	sink := &recordingSink{}
	_, _ = EvaluateActAs(context.Background(),
		auditGate(NewDisabledCallerPermissionChecker(), evalUser(), sink), evalTarget())

	ev := sink.only(t)
	if ev.Decision == nil || *ev.Decision != ActAsAllowed {
		t.Fatalf("decision: got %v, want allowed", ev.Decision)
	}
	if ev.Type != SAAssignmentDecision {
		t.Errorf("type: got %q, want %q", ev.Type, SAAssignmentDecision)
	}
	if ev.Mechanism != MechanismCheckDisabled {
		t.Errorf("mechanism: got %q, want %q", ev.Mechanism, MechanismCheckDisabled)
	}
	if ev.Permission != PermissionActAs {
		t.Errorf("permission: got %q, want %q", ev.Permission, PermissionActAs)
	}
}

// TestAudit_EmittedOnDeny — the case the brief singles out. Omitting denials
// defeats the point.
func TestAudit_EmittedOnDeny(t *testing.T) {
	sink := &recordingSink{}
	blockMode := Principal{Kind: PrincipalAgent, ID: "agent-1"}

	_, _ = EvaluateActAs(context.Background(),
		auditGate(NewDisabledCallerPermissionChecker(), blockMode, sink), evalTarget())

	ev := sink.only(t)
	if ev.Decision == nil || *ev.Decision != ActAsDenied {
		t.Fatalf("decision: got %v, want denied", ev.Decision)
	}
	if ev.Mechanism != MechanismNoCallerIdentity {
		t.Errorf("mechanism: got %q, want %q", ev.Mechanism, MechanismNoCallerIdentity)
	}
}

// TestAudit_EmittedOnIndeterminate — indeterminate is a third state and must
// not be flattened into allow or deny in the record.
func TestAudit_EmittedOnIndeterminate(t *testing.T) {
	sink := &recordingSink{}
	checker := &malformedChecker{result: ActAsResult{Outcome: ActAsAllowed}, err: errors.New("boom")}

	_, _ = EvaluateActAs(context.Background(),
		auditGate(checker, evalUser(), sink), evalTarget())

	ev := sink.only(t)
	if ev.Decision == nil || *ev.Decision != ActAsIndeterminate {
		t.Fatalf("decision: got %v, want indeterminate", ev.Decision)
	}
	if ev.Mechanism != MechanismCheckFailed {
		t.Errorf("mechanism: got %q, want %q", ev.Mechanism, MechanismCheckFailed)
	}
}

// TestAudit_EmittedWhenCheckerIsUnwired — a wiring bug must leave a trace. This
// is the state most likely to be discovered from logs rather than from a test.
func TestAudit_EmittedWhenCheckerIsUnwired(t *testing.T) {
	sink := &recordingSink{}
	_, _ = EvaluateActAs(context.Background(),
		auditGate(nil, evalUser(), sink), evalTarget())

	ev := sink.only(t)
	if ev.Mechanism != MechanismCheckUnwired {
		t.Errorf("mechanism: got %q, want %q", ev.Mechanism, MechanismCheckUnwired)
	}
	if ev.Decision == nil || *ev.Decision != ActAsDenied {
		t.Errorf("decision: got %v, want denied", ev.Decision)
	}
}

// ---------------------------------------------------------------------------
// The record never claims what did not happen
// ---------------------------------------------------------------------------

// TestAudit_CacheHitIsNilWhenNoCacheWasConsulted is the ruled behaviour for Q1.
// No caller-permission cache exists, and false would assert a live IAM call
// that missed a cache. Nil is the only honest value.
func TestAudit_CacheHitIsNilWhenNoCacheWasConsulted(t *testing.T) {
	sink := &recordingSink{}
	_, _ = EvaluateActAs(context.Background(),
		auditGate(NewDisabledCallerPermissionChecker(), evalUser(), sink), evalTarget())

	if got := sink.only(t).CacheHit; got != nil {
		t.Errorf("CacheHit: got %v, want nil — false would assert a cache miss that never happened", *got)
	}
}

// TestAudit_TargetIsRecorded — "denied" without naming the account is not
// actionable.
func TestAudit_TargetIsRecorded(t *testing.T) {
	sink := &recordingSink{}
	target := evalTarget()

	_, _ = EvaluateActAs(context.Background(),
		auditGate(NewDisabledCallerPermissionChecker(), evalUser(), sink), target)

	ev := sink.only(t)
	if ev.TargetSAID != target.ID {
		t.Errorf("TargetSAID: got %q, want %q", ev.TargetSAID, target.ID)
	}
	if ev.TargetSAEmail != target.Email {
		t.Errorf("TargetSAEmail: got %q, want %q", ev.TargetSAEmail, target.Email)
	}
}

// TestAudit_MechanismIsNeverEmpty — the mechanism is what tells you WHY, and
// evaluateActAs already guarantees it. Pinned here because the audit record is
// the consumer that depends on it.
func TestAudit_MechanismIsNeverEmpty(t *testing.T) {
	cases := []struct {
		name    string
		checker CallerPermissionChecker
		caller  Principal
	}{
		{"disabled", NewDisabledCallerPermissionChecker(), evalUser()},
		{"unwired", nil, evalUser()},
		{"no identity", NewDisabledCallerPermissionChecker(), Principal{Kind: PrincipalAgent}},
		{"unattributable allow", &malformedChecker{result: ActAsResult{Outcome: ActAsAllowed}}, evalUser()},
		{"unspecified denial", &malformedChecker{result: ActAsResult{Outcome: ActAsDenied}}, evalUser()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := &recordingSink{}
			_, _ = EvaluateActAs(context.Background(),
				auditGate(tc.checker, tc.caller, sink), evalTarget())
			if got := sink.only(t).Mechanism; got == "" {
				t.Error("mechanism is empty; an unattributable record is an audit gap")
			}
		})
	}
}

// TestAudit_SurfaceIsRecorded — the surface is how a reader tells an agent
// creation from an identity swap on a live agent.
func TestAudit_SurfaceIsRecorded(t *testing.T) {
	sink := &recordingSink{}
	gate := ActAsGate{
		Checker: NewDisabledCallerPermissionChecker(),
		Caller:  evalUser(),
		Surface: "agent-patch",
		Audit:   sink,
	}
	_, _ = EvaluateActAs(context.Background(), gate, evalTarget())

	if got := sink.only(t).Surface; got != "agent-patch" {
		t.Errorf("surface: got %q, want %q", got, "agent-patch")
	}
}

// TestAudit_UnnamedSurfaceIsNamedNotBlank — a caller bug, but the record must
// still be attributable and alertable rather than carrying an empty string
// that reads like a formatting artefact.
func TestAudit_UnnamedSurfaceIsNamedNotBlank(t *testing.T) {
	sink := &recordingSink{}
	gate := ActAsGate{Checker: NewDisabledCallerPermissionChecker(), Caller: evalUser(), Audit: sink}

	_, _ = EvaluateActAs(context.Background(), gate, evalTarget())

	if got := sink.only(t).Surface; got != SurfaceUnnamed {
		t.Errorf("surface: got %q, want %q", got, SurfaceUnnamed)
	}
}

// ---------------------------------------------------------------------------
// The decision is not affected by the audit layer
// ---------------------------------------------------------------------------

// TestAudit_NilSinkDoesNotChangeTheDecision is the asymmetry with a nil
// checker, and it is deliberate: a missing checker means we do not know the
// answer, a missing sink means we cannot file it. Failing closed on the latter
// would turn a logging misconfiguration into an authorization outage.
func TestAudit_NilSinkDoesNotChangeTheDecision(t *testing.T) {
	got, err := EvaluateActAs(context.Background(),
		ActAsGate{Checker: NewDisabledCallerPermissionChecker(), Caller: evalUser(), Surface: "s"},
		evalTarget())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.Allowed() {
		t.Errorf("outcome: got %v, want allowed — a nil sink must not deny", got.Outcome)
	}
}

// TestAudit_SinkErrorDoesNotChangeTheDecision — same reasoning as the nil sink.
func TestAudit_SinkErrorDoesNotChangeTheDecision(t *testing.T) {
	sink := &recordingSink{err: errors.New("sink is down")}

	got, err := EvaluateActAs(context.Background(),
		auditGate(NewDisabledCallerPermissionChecker(), evalUser(), sink), evalTarget())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.Allowed() {
		t.Errorf("outcome: got %v, want allowed — a failing sink must not deny", got.Outcome)
	}
}

// TestAudit_DecisionMatchesTheReturnedResult — the record and the enforcement
// path must not be able to disagree; that would be worse than no record.
func TestAudit_DecisionMatchesTheReturnedResult(t *testing.T) {
	cases := []struct {
		name    string
		checker CallerPermissionChecker
		caller  Principal
	}{
		{"allow", NewDisabledCallerPermissionChecker(), evalUser()},
		{"deny", NewUnavailableCallerPermissionChecker("nope"), evalUser()},
		{"unwired", nil, evalUser()},
		{"no identity", NewDisabledCallerPermissionChecker(), Principal{Kind: PrincipalUser}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := &recordingSink{}
			result, _ := EvaluateActAs(context.Background(),
				auditGate(tc.checker, tc.caller, sink), evalTarget())

			ev := sink.only(t)
			if ev.Decision == nil {
				t.Fatal("decision record has no decision")
			}
			if *ev.Decision != result.Outcome {
				t.Errorf("record says %v, caller was told %v", *ev.Decision, result.Outcome)
			}
			if ev.Mechanism != result.Mechanism {
				t.Errorf("record mechanism %q, result mechanism %q", ev.Mechanism, result.Mechanism)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Binding records (project-default)
// ---------------------------------------------------------------------------

// TestBindingRecord_CarriesNoDecision is the trap sa-arch ruled on. Nothing was
// decided at a project-default binding, so there is no honest ActAsOutcome:
// Allowed is false, Denied is false, and Indeterminate DENIES — which means a
// later enforcement path driven from these records would break routine agent
// creation. Nil is the encoding that cannot be misread as a verdict.
func TestBindingRecord_CarriesNoDecision(t *testing.T) {
	sink := &recordingSink{}
	RecordSABinding(context.Background(), sink, "project-default", evalUser(), evalTarget(), "because")

	ev := sink.only(t)
	if ev.Decision != nil {
		t.Errorf("Decision: got %v, want nil — a binding record must carry no verdict", *ev.Decision)
	}
	if ev.Type != SAAssignmentBinding {
		t.Errorf("type: got %q, want %q", ev.Type, SAAssignmentBinding)
	}
}

// TestBindingRecord_NamesNoPermission — no permission was checked, and naming
// one would assert a check that did not happen.
func TestBindingRecord_NamesNoPermission(t *testing.T) {
	sink := &recordingSink{}
	RecordSABinding(context.Background(), sink, "project-default", evalUser(), evalTarget(), "because")

	if got := sink.only(t).Permission; got != "" {
		t.Errorf("Permission: got %q, want empty — nothing was checked", got)
	}
}

// TestBindingRecord_MechanismExplainsWhyThereIsNoGate — "ungated" would read as
// a gap and invite someone to close it. The mechanism has to say that the
// account is not caller-supplied, so there is nothing to check.
func TestBindingRecord_MechanismExplainsWhyThereIsNoGate(t *testing.T) {
	sink := &recordingSink{}
	RecordSABinding(context.Background(), sink, "project-default", evalUser(), evalTarget(), "because")

	ev := sink.only(t)
	if ev.Mechanism != MechanismProjectDefault {
		t.Errorf("mechanism: got %q, want %q", ev.Mechanism, MechanismProjectDefault)
	}
	if ev.Reason == "" {
		t.Error("a binding record with no reason cannot explain itself")
	}
}

// TestBindingRecord_IdentifiesTheAccount — the whole reason to emit here is so
// item D's compliance report can find these bindings.
func TestBindingRecord_IdentifiesTheAccount(t *testing.T) {
	sink := &recordingSink{}
	target := evalTarget()
	RecordSABinding(context.Background(), sink, "project-default", evalUser(), target, "because")

	ev := sink.only(t)
	if ev.TargetSAID != target.ID || ev.TargetSAEmail != target.Email {
		t.Errorf("target not recorded: id=%q email=%q", ev.TargetSAID, ev.TargetSAEmail)
	}
	if ev.Surface != "project-default" {
		t.Errorf("surface: got %q", ev.Surface)
	}
}

// TestBindingRecord_NilSinkDoesNotPanic — the emit sits on the agent-creation
// path, so it must never be able to take it down.
func TestBindingRecord_NilSinkDoesNotPanic(t *testing.T) {
	RecordSABinding(context.Background(), nil, "project-default", evalUser(), evalTarget(), "because")
}

// TestBindingRecord_NilTargetDoesNotPanic — defensive; a nil target would mean
// a caller bug upstream, and losing agent creation over it is worse.
func TestBindingRecord_NilTargetDoesNotPanic(t *testing.T) {
	sink := &recordingSink{}
	RecordSABinding(context.Background(), sink, "project-default", evalUser(), nil, "because")
	if got := sink.only(t).TargetSAID; got != "" {
		t.Errorf("TargetSAID: got %q, want empty", got)
	}
}
