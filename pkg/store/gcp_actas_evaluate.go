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
	"strings"
)

// Mechanism values produced by evaluateActAs itself, as opposed to those
// produced by a checker implementation.
const (
	// MechanismCheckUnwired marks a denial caused by a nil checker. This is a
	// programming error, never a configuration: switching the check off is done
	// by passing NewDisabledCallerPermissionChecker.
	MechanismCheckUnwired = "check-unwired"

	// MechanismSameAccount marks an allow granted because the caller already
	// acts as the target account.
	MechanismSameAccount = "same-account"

	// MechanismNoCallerIdentity marks a denial because the caller has no GCP
	// identity that any permission could have been granted to.
	MechanismNoCallerIdentity = "no-caller-identity"

	// MechanismNoTarget marks a denial because no target account was supplied.
	MechanismNoTarget = "no-target"

	// MechanismCheckFailed marks a result where the checker was consulted but
	// did not return a verdict — a transport or programming failure.
	//
	// ⚠️ It REPLACES whatever mechanism and reason the checker reported, rather
	// than being recorded alongside them. A failed check that kept its
	// reported reason produces an audit record like "Outcome: indeterminate,
	// Mechanism: iam-getIamPolicy, Reason: caller has roles/..." — which
	// asserts that IAM was consulted and answered. It was not. Preserving a
	// verdict-shaped explanation for a call that produced no verdict is worse
	// than recording nothing, because it reads as evidence.
	MechanismCheckFailed = "check-failed"

	// MechanismUnattributableAllow marks an allow that was DOWNGRADED to a
	// denial because the checker reported no mechanism.
	//
	// An allow nobody can attribute is the exact thing the audit record exists
	// to prevent: "allowed because IAM said so" and "allowed because nobody
	// asked" become indistinguishable, and the difference is only ever needed
	// after the fact, when it can no longer be recovered. A checker that
	// allows without naming the check is malformed in the same way as one that
	// returns (Allowed, err), and is treated the same way.
	//
	// Alertable by design: no conforming checker produces this, so any
	// occurrence is a bug in a checker implementation and not a caller problem.
	MechanismUnattributableAllow = "allow-unattributable"

	// MechanismUnspecified marks a DENIAL from a checker that reported no
	// mechanism. Unlike the allow case this does not change the outcome — a
	// denial is already the safe direction, and downgrading it further would
	// achieve nothing — but it is still named rather than left empty so it can
	// be alerted on as the checker bug it is.
	MechanismUnspecified = "unspecified"
)

// evaluateActAs is the single decision sequence for "may this caller act as
// this service account", shared by every surface that gates on it.
//
// ⚠️ IT EXISTS TO BE THE ONLY COPY. Two surfaces gate on this today — agent
// service-account assignment in pkg/hub and lifecycle-hook execution identity
// in pkg/lifecyclehooks — and the ordering below is security-relevant at every
// step. Reimplemented per surface, the copies drift, and the drift is invisible
// because each one looks locally reasonable. Surfaces are expected to differ in
// how they REPORT the result (HTTP 403 versus a FieldError) and in what they
// log; they must not differ in how they REACH it.
//
// ⚠️ IT IS UNEXPORTED ON PURPOSE. Surfaces call the exported EvaluateActAs in
// gcp_actas_audit.go, which wraps this and emits the audit record. Keeping the
// pure sequence package-private means a new surface cannot reach a decision
// without also filing a record for it — the audit trail is inherited by
// construction rather than by the author remembering (design §7). This function
// stays pure so the decision ordering remains directly testable without a sink.
//
// ⚠️ THE GUARANTEE STOPS AT THE PACKAGE BOUNDARY, AND THAT LIMIT IS THE WHOLE
// OF IT. Unexporting stops pkg/hub and pkg/lifecyclehooks from reaching a
// verdict unaudited; it does nothing about pkg/store. Inside this package this
// function is directly callable and the compiler will not object, so the
// property "every decision produces a record" holds here only because no
// non-test caller in pkg/store calls it — an absence, not an enforcement, and
// an absence that a single future in-package call site would end silently. The
// tests in gcp_actas_evaluate_test.go are already such callers, benignly and on
// purpose, because the point of keeping this pure is that the ordering stays
// testable without a sink.
//
// SO: CALLERS INSIDE pkg/store MUST GO THROUGH EvaluateActAs TOO. This function
// is package-private for the audit guarantee, not for convenience, and calling
// it directly from non-test code in this package would drop the record with
// nothing to catch it. If a legitimate in-package caller ever appears, the
// honest fix is to give it the sink, not to widen this door.
//
// The sequence, in order, with the reason each step is where it is:
//
//  1. Nil target denies. Nothing to check.
//  2. Nil checker denies, Mechanism "check-unwired". A nil checker is a wiring
//     bug and must be loud. It is placed BEFORE the same-account fast path on
//     purpose: a fast path that succeeds while the gate is unwired would mask
//     the bug for exactly the callers most likely to exercise it.
//  3. Same-account allows without contacting GCP. A caller configuring the
//     account it already acts as grants nothing it does not already have, so
//     there is no privilege to escalate. Both emails must be non-empty, or an
//     empty-equals-empty match would turn every caller with no GCP identity
//     into a permitted user of a malformed account.
//  4. A caller with no GCP identity denies. Block-mode and passthrough-mode
//     agents, and unknown or broker callers, have nothing that could have been
//     granted actAs. Nothing to check means no, and it means no without a
//     round-trip.
//  5. Otherwise the checker decides.
//
// An error from the checker is diagnostic and carries no verdict, per the
// CallerPermissionChecker contract. It is returned to the caller for logging,
// but the whole result is replaced: outcome forced to ActAsIndeterminate,
// mechanism to MechanismCheckFailed, reason to one that says no verdict was
// obtained. A checker returning (Allowed, err) is malformed and this is the one
// place that can stop it becoming an allow — and a preserved reason from a call
// that never answered is worse than no reason at all, because it reads as
// evidence that IAM was consulted.
//
// THE RESULT IS ALWAYS ATTRIBUTABLE. Mechanism is never empty on return, and an
// ALLOW that arrives without one is downgraded to indeterminate rather than
// stamped and honoured — see MechanismUnattributableAllow. An audit record that
// cannot say which check produced a permit is the failure the mechanism field
// exists to prevent, and it is only ever missed during an incident.
func evaluateActAs(
	ctx context.Context,
	checker CallerPermissionChecker,
	caller Principal,
	targetSA *GCPServiceAccount,
) (ActAsResult, error) {
	if targetSA == nil {
		return ActAsResult{
			Outcome:   ActAsDenied,
			Mechanism: MechanismNoTarget,
			Reason:    "no target service account was supplied",
		}, nil
	}

	if checker == nil {
		return ActAsResult{
			Outcome:   ActAsDenied,
			Mechanism: MechanismCheckUnwired,
			Reason: "no caller-permission checker is configured for this surface; " +
				"this is a wiring error, not a setting",
		}, nil
	}

	if caller.Kind == PrincipalAgent &&
		caller.ServiceAccountEmail != "" && targetSA.Email != "" &&
		strings.EqualFold(caller.ServiceAccountEmail, targetSA.Email) {
		return ActAsResult{
			Outcome:   ActAsAllowed,
			Mechanism: MechanismSameAccount,
			Reason:    "caller already acts as this service account",
		}, nil
	}

	if !caller.HasGCPIdentity() {
		return ActAsResult{
			Outcome:   ActAsDenied,
			Mechanism: MechanismNoCallerIdentity,
			Reason:    "caller has no GCP identity of its own",
		}, nil
	}

	result, err := checker.CanActAs(ctx, caller, targetSA)
	if err != nil {
		// Force the outcome rather than trusting it. See the doc comment: the
		// contract says error carries no verdict, and this is where a
		// non-conforming implementation is prevented from turning a transport
		// failure into an allow.
		//
		// The mechanism and reason are REPLACED, not kept. Whatever the checker
		// reported describes a verdict it did not reach; leaving it in place
		// produces an audit record that asserts IAM was consulted and answered.
		// The attempted mechanism is preserved inside the reason because it is
		// useful for triage and is a fixed identifier chosen by our own
		// implementations.
		attempted := result.Mechanism
		if attempted == "" {
			attempted = "caller-permission"
		}
		return ActAsResult{
			Outcome:   ActAsIndeterminate,
			Mechanism: MechanismCheckFailed,
			Reason: "the " + attempted + " check did not complete, so no verdict was " +
				"obtained; this is not a statement about the caller's permissions",
		}, err
		// ⚠️ The underlying error is deliberately NOT folded into Reason.
		// Reason is surfaced to the caller on denial, and an error from a
		// remote IAM call is not a string this package can promise is free of
		// policy content. Both consumers already log err separately with the
		// full text, which is where it belongs.
	}

	if result.Mechanism == "" {
		// An unattributable ALLOW is not an allow. See
		// MechanismUnattributableAllow: a permit nobody can account for defeats
		// the reason the mechanism field is mandatory, and a checker that emits
		// one is malformed. Fails closed.
		if result.Outcome == ActAsAllowed {
			return ActAsResult{
				Outcome:   ActAsIndeterminate,
				Mechanism: MechanismUnattributableAllow,
				Reason: "the checker allowed this caller without naming the check that " +
					"produced the decision; an unattributable allow is not honoured",
			}, nil
		}
		// A denial with no mechanism stays a denial — already the safe
		// direction — but is named so it is alertable rather than blank.
		result.Mechanism = MechanismUnspecified
	}
	return result, err
}
