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

// This file defines the caller-permission check that gates binding a GCP
// service account to an agent (svc-accnt Goal 1).
//
// Why it lives in package store rather than package hub: this is a predicate
// over GCPServiceAccount's own fields, so it belongs to the type rather than
// to either consumer. Both pkg/hub and pkg/lifecyclehooks gate on it, and the
// import graph runs hub -> lifecyclehooks -> store, so store is the genuine
// common ancestor. Do not "tidy" this into pkg/hub: the moment it moves there,
// the lifecyclehooks execution-identity consumer silently stops being covered,
// which is exactly the failure this placement exists to prevent.

// PermissionActAs is the IAM permission checked on the caller against the
// target service account.
//
// It is deliberately actAs and NOT iam.serviceAccounts.getAccessToken or
// roles/iam.serviceAccountTokenCreator. Two different service accounts and two
// different permissions are in play, and conflating them is the easy mistake:
//
//	                | which SA        | which permission | who holds it
//	----------------+-----------------+------------------+--------------
//	enables a probe | the CALLER's SA | tokenCreator     | the Hub
//	is being tested | the TARGET SA   | actAs            | the CALLER
//
// The Hub's own tokenCreator grant is what makes an impersonated probe
// possible; it is not the permission being checked. Checking tokenCreator on
// the caller would be checking the wrong thing on the wrong principal.
//
// "Assign an SA to an agent" is the same operation as "attach an SA to a VM" —
// Scion's metadata sidecar emulates the GCE metadata server — and attaching is
// gated on actAs. That correspondence is the reason for the choice.
const PermissionActAs = "iam.serviceAccounts.actAs"

// ActAsOutcome is the result of a caller-permission check.
//
// It is three-valued rather than a bool because "the check could not reach an
// answer" is a real and common state: Policy Troubleshooter returns UNKNOWN
// variants, an IAM API call can time out, and a getIamPolicy-based check is
// blind to IAM Deny and Principal Access Boundary policies. With only
// allowed/denied, that third state has nowhere to live and gets flattened into
// one of the other two at the point of least information.
//
// Keeping it explicit means the policy question "what do we do when we cannot
// tell?" is a one-line decision at a single call site instead of a signature
// change across every implementation and test.
type ActAsOutcome int

const (
	// ActAsIndeterminate is the zero value, deliberately. A result struct that
	// was never populated must not read as "allowed". Any code path that
	// forgets to set an outcome therefore fails closed by construction.
	ActAsIndeterminate ActAsOutcome = iota

	// ActAsAllowed means the caller holds PermissionActAs on the target SA.
	ActAsAllowed

	// ActAsDenied means the caller provably does not hold PermissionActAs.
	ActAsDenied
)

// String renders the outcome for logs and audit events.
func (o ActAsOutcome) String() string {
	switch o {
	case ActAsAllowed:
		return "allowed"
	case ActAsDenied:
		return "denied"
	case ActAsIndeterminate:
		return "indeterminate"
	default:
		return "indeterminate"
	}
}

// ActAsResult is the outcome of a CanActAs check together with enough context
// to audit it. Every SA-assignment decision is auditable on both allow and
// deny, so Mechanism and Reason are part of the contract rather than debug
// decoration.
type ActAsResult struct {
	// Outcome is the decision. The zero value is ActAsIndeterminate.
	Outcome ActAsOutcome

	// Mechanism names the check that produced this outcome — for example
	// "impersonated-testIamPermissions" or "policy-troubleshooter". Required
	// for the audit event: "denied" is not actionable without knowing which
	// check denied it.
	Mechanism string

	// Reason is a human-readable explanation, surfaced to the caller on
	// denial. It must not contain credentials or raw IAM policy content.
	Reason string
}

// Allowed reports whether the result permits the assignment. Only
// ActAsAllowed does. Indeterminate is not an allow — callers that want a
// different disposition for indeterminate must test Outcome explicitly, which
// is the point of the three-valued type.
func (r ActAsResult) Allowed() bool {
	return r.Outcome == ActAsAllowed
}

// PrincipalKind distinguishes the caller types that CanActAs must evaluate
// differently. What the Hub is able to check depends on the principal: an
// agent's SA can be impersonated, a human's account cannot.
type PrincipalKind int

const (
	// PrincipalUnknown is the zero value, deliberately, so an unpopulated
	// Principal cannot be mistaken for an authorized one. Broker and
	// unidentified callers are denied.
	PrincipalUnknown PrincipalKind = iota

	// PrincipalAgent is an agent caller acting as its own service account.
	PrincipalAgent

	// PrincipalUser is a human caller acting as their own account.
	PrincipalUser
)

// String renders the principal kind for logs and audit events.
func (k PrincipalKind) String() string {
	switch k {
	case PrincipalAgent:
		return "agent"
	case PrincipalUser:
		return "user"
	case PrincipalUnknown:
		return "unknown"
	default:
		return "unknown"
	}
}

// Principal is the caller whose permission is evaluated.
//
// Exactly one principal is checked: the immediate caller. Ancestry is
// deliberately not consulted. Checking the originating human would be weaker,
// not stronger — an agent started by an admin but holding a low-privilege SA
// could otherwise pass on the admin's authority to a child it creates.
type Principal struct {
	// Kind selects which evaluation strategy applies. The zero value,
	// PrincipalUnknown, is denied.
	Kind PrincipalKind

	// ID is the Scion-side identifier (agent ID or user ID). Used for audit
	// and as part of the decision cache key; it is never sent to GCP.
	ID string

	// ServiceAccountEmail is the GCP service account the caller acts as.
	// Set only when Kind is PrincipalAgent.
	//
	// It is empty for a block-mode agent, which has no GCP identity at all.
	// That is a meaningful state, not a missing field: such an agent has no
	// principal to evaluate and nothing that could have been delegated to it,
	// so it cannot assign any SA. See HasGCPIdentity.
	ServiceAccountEmail string

	// Email is the caller's Google account address, without a "user:" prefix.
	// Set only when Kind is PrincipalUser.
	Email string
}

// HasGCPIdentity reports whether this principal has a GCP identity that a
// permission check could be evaluated against.
//
// A false result is a denial, not an error. The two cases it covers are a
// block-mode agent (no GCP identity by configuration) and an unknown or broker
// caller (no identity at all). Neither has anything that could have been
// granted actAs on the target, so both fail closed.
func (p Principal) HasGCPIdentity() bool {
	switch p.Kind {
	case PrincipalAgent:
		return p.ServiceAccountEmail != ""
	case PrincipalUser:
		return p.Email != ""
	case PrincipalUnknown:
		return false
	default:
		return false
	}
}

// GCPPrincipalID returns the IAM principal identifier for this caller, in the
// "type:id" form IAM uses. It returns "" when the principal has no GCP
// identity.
func (p Principal) GCPPrincipalID() string {
	switch p.Kind {
	case PrincipalAgent:
		if p.ServiceAccountEmail == "" {
			return ""
		}
		return "serviceAccount:" + p.ServiceAccountEmail
	case PrincipalUser:
		if p.Email == "" {
			return ""
		}
		return "user:" + p.Email
	default:
		return ""
	}
}

// CallerPermissionChecker answers whether a caller may act as a service
// account in GCP. It is the IAM half of the SA-assignment gate; the Hub's own
// policy layer is a separate and independently required check, and neither
// subsumes the other.
//
// Implementations must observe three rules.
//
// First, the checker is pure: it answers "may this principal act as this SA",
// not "do we care about the answer". The gcpIamCheckMode toggle is evaluated
// by the caller, before the checker is consulted. Keeping the toggle out keeps
// the checker testable and stops the disabled state from being reimplemented
// in each of the assignment surfaces.
//
// Second, error means a programming or transport failure only. It must never
// carry a denial and never carry "unknown". An IAM API timeout returns
// (ActAsResult{Outcome: ActAsIndeterminate}, err) — the outcome is the
// security-relevant answer and the error is diagnostic. Overloading error with
// denial is how a caller that forgets to check err fails open.
//
// Third, when a check cannot reach an answer, return ActAsIndeterminate rather
// than guessing. Choosing what indeterminate means is the caller's decision,
// made once, at a single site.
type CallerPermissionChecker interface {
	CanActAs(ctx context.Context, caller Principal, targetSA *GCPServiceAccount) (ActAsResult, error)
}
