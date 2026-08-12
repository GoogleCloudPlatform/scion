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

import "context"

// Mechanism values recorded on an ActAsResult. Mechanism is mandatory on every
// emitted result: "allowed because IAM said so" and "allowed because nobody
// asked" are the same outcome and completely different facts, and only one of
// them is a security control having been applied. Audit needs to tell them
// apart after the fact, so the distinction lives in the record rather than in
// the reader's memory of how the hub was configured that day.
const (
	// MechanismCheckDisabled marks a result produced without consulting GCP
	// because caller-permission checking is switched off for that surface.
	MechanismCheckDisabled = "check-disabled"

	// MechanismCheckUnavailable marks a denial produced because checking was
	// switched ON but the hub cannot perform it. It is deliberately distinct
	// from a GCP-sourced denial: the caller might well hold the permission,
	// and the record should not claim IAM said otherwise.
	MechanismCheckUnavailable = "check-unavailable"
)

// disabledCallerPermissionChecker allows every caller without contacting GCP.
type disabledCallerPermissionChecker struct{}

// NewDisabledCallerPermissionChecker returns the checker used when
// caller-permission checking is switched off for a surface.
//
// ⚠️ This is the ONLY definition of the disabled state, and it is deliberately
// an explicit object rather than an absence.
//
// A nil CallerPermissionChecker is always a programming error and must deny.
// "Checking is off" is a configuration choice and must allow. Those are
// different facts and they cannot share a representation: if nil meant "off",
// then every future call site that simply forgot to wire a checker would
// silently switch the control off, and the failure would look exactly like the
// intended configuration. Making the disabled state a value that somebody has
// to construct and pass means turning the control off is an act, and leaving
// it unwired is a bug that fails closed.
//
// For the same reason there is no package-level default instance, and no
// constructor that falls back to this when a field is unset. Every surface
// passes its own checker explicitly at its own wiring site. The moment this
// becomes something a new call site picks up by omission, the distinction
// above is defeated and nil-means-skip is back wearing a different hat.
//
// Callers wiring this in must log at WARN naming THEIR OWN SURFACE, not the
// feature. The same object degrades to different things in different places —
// on a surface that also has a policy gate it means "policy-gated only", and
// on a surface where actAs is the only gate it means "ungated" — so a single
// message covering "the IAM check" would mislead about the second.
func NewDisabledCallerPermissionChecker() CallerPermissionChecker {
	return disabledCallerPermissionChecker{}
}

// CanActAs allows unconditionally and records that nothing was checked.
//
// It returns ActAsAllowed and never ActAsIndeterminate. Indeterminate means
// "the check ran and could not reach an answer", which would be false here and
// would also make the disabled state fail closed at every caller that denies
// on indeterminate — turning a configuration toggle into an outage.
func (disabledCallerPermissionChecker) CanActAs(
	_ context.Context, _ Principal, _ *GCPServiceAccount,
) (ActAsResult, error) {
	return ActAsResult{
		Outcome:   ActAsAllowed,
		Mechanism: MechanismCheckDisabled,
		Reason:    "caller-permission checking is disabled for this surface; no GCP check was performed",
	}, nil
}

// unavailableCallerPermissionChecker denies every caller without contacting GCP.
type unavailableCallerPermissionChecker struct{ reason string }

// NewUnavailableCallerPermissionChecker returns a checker that denies
// everything, for use when caller-permission checking is switched ON but the
// hub cannot perform it — most concretely when no GCP token generator is
// configured, so there is no way to run an impersonated probe.
//
// This exists so that misconfiguration is a denial rather than a fall-through.
// The failure it is written against is real and lives in this tree:
// verifyGCPServiceAccount wraps its impersonation probe in a
// `if gcpTokenGenerator != nil` guard that covers only the check and not the
// success assignment, so a hub with no generator marks every service account
// verified having never contacted GCP. An absent capability must not become an
// implicit pass.
//
// It denies rather than returning ActAsIndeterminate so the outcome does not
// depend on each caller's indeterminate policy. The distinction is preserved
// in Mechanism instead.
func NewUnavailableCallerPermissionChecker(reason string) CallerPermissionChecker {
	return unavailableCallerPermissionChecker{reason: reason}
}

// CanActAs denies unconditionally and records why the check could not run.
func (c unavailableCallerPermissionChecker) CanActAs(
	_ context.Context, _ Principal, _ *GCPServiceAccount,
) (ActAsResult, error) {
	reason := c.reason
	if reason == "" {
		reason = "caller-permission checking is enabled but unavailable on this hub"
	}
	return ActAsResult{
		Outcome:   ActAsDenied,
		Mechanism: MechanismCheckUnavailable,
		Reason:    reason,
	}, nil
}
