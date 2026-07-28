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

// Mechanism values produced by EvaluateActAs itself, as opposed to those
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
)

// EvaluateActAs is the single decision sequence for "may this caller act as
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
// but the outcome is forced to ActAsIndeterminate regardless of what the
// implementation put there: a checker returning (Allowed, err) is malformed,
// and this is the one place that can stop that from becoming an allow.
//
// Mechanism is never empty on return. An audit record saying "denied" with no
// mechanism is not actionable, and the gap would only ever be noticed during an
// incident.
func EvaluateActAs(
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
		result.Outcome = ActAsIndeterminate
	}
	if result.Mechanism == "" {
		result.Mechanism = "unspecified"
	}
	return result, err
}
