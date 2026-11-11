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
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Audit surface labels for writes of a broker's host service account
// (gcpHostServiceAccountEmail / gcpHostProjectId).
const (
	// SurfaceBrokerHostSARegister is a host service account set or changed
	// by broker registration or re-registration.
	SurfaceBrokerHostSARegister = "broker-host-sa-register"

	// SurfaceBrokerHostSAUpdate is a host service account set or changed by
	// PATCH /api/v1/runtime-brokers/{id}.
	SurfaceBrokerHostSAUpdate = "broker-host-sa-update"
)

// MechanismHostSAClaimNonUser is the audit mechanism for a host service
// account change refused because the caller is not a user principal.
const MechanismHostSAClaimNonUser = "non-user-caller"

// ErrHostSAClaimNotAuthorized is returned by a write site that would store a
// changed host service account without a matching allowed verdict.
var ErrHostSAClaimNotAuthorized = errors.New("host service account claim not authorized")

// ErrHostSAClaimStale is returned when a broker's stored host service account
// differs from the value the permission decision was made against, so a write
// would store a pair over a value nobody was checked for. Handlers map it to
// 409; the request can be retried.
var ErrHostSAClaimStale = errors.New("broker host service account changed after the permission decision")

// hostSAClaim is a broker's host service account as the pair the actAs check
// is evaluated against: the email and the GCP project ID. A change to either
// half is a change to the claim. Email is lowercased by hostSAClaimOf.
type hostSAClaim struct {
	Email     string
	ProjectID string
}

// hostSAClaimOf returns the claim stored on b. A nil broker has the empty
// claim (a row that does not exist yet).
func hostSAClaimOf(b *store.RuntimeBroker) hostSAClaim {
	if b == nil {
		return hostSAClaim{}
	}
	return hostSAClaim{
		Email:     strings.ToLower(b.GCPHostServiceAccountEmail),
		ProjectID: b.GCPHostProjectID,
	}
}

// hostSAClaimVerdict is the actAs decision for one requested claim, made by
// the handler that holds the request identity and enforced by the write site
// (hostSAClaimWriteAllowed). The zero value authorizes nothing.
type hostSAClaimVerdict struct {
	// Claim is the claim the decision was made for.
	Claim hostSAClaim
	// Prior is the stored claim the decision was made against.
	Prior hostSAClaim
	// Allowed is true when the claim may be written.
	Allowed bool
	// Checked is true when an actAs evaluation produced the decision. An
	// unchecked allowed verdict (an unchanged or empty request) authorizes
	// a write only while the stored claim is still Prior.
	Checked bool
	// Result carries the actAs outcome and mechanism for the 403 message.
	// It is zero when no actAs evaluation ran.
	Result store.ActAsResult
}

// hostSATarget builds the transient EvaluateActAs target for a claim. When the
// project ID is unset it is derived from the email, which works for custom IAM
// service accounts only; default Compute Engine and App Engine accounts need
// the project ID set explicitly. The passthrough gate uses the same target, so
// setting a claim and using it are checked against the same resource.
//
// The target is never persisted. Verified is set for parity with the stored
// record shape; evaluateActAs does not read it.
func hostSATarget(c hostSAClaim) *store.GCPServiceAccount {
	projectID := c.ProjectID
	if projectID == "" {
		projectID = projectIDFromServiceAccountEmail(c.Email)
	}
	return &store.GCPServiceAccount{
		Email:     c.Email,
		ProjectID: projectID,
		Verified:  true,
	}
}

// evaluateHostSAClaim decides whether the caller on ctx may set a broker's host
// service account claim from prior to requested.
//
// An empty requested email, or requested equal to prior, needs no decision: it
// returns an allowed verdict without calling the checker, so re-sending the
// stored value (broker restarts, repeated registration) makes no GCP call, and
// clearing the claim only removes capability.
//
// Otherwise the caller must be a user principal and pass store.EvaluateActAs
// on the requested claim, which also writes the audit record under surface.
// Invariant: only a user principal can change the host service account
// claim. Broker callers fail callerPrincipal; agent callers resolve to an
// agent principal and are refused here explicitly, before any actAs
// evaluation (an agent acting as the requested account would otherwise pass
// the same-account rule). Both refusals write a denied audit record.
//
// The checker follows gcpIamCheckMode (saAssignCheckerFor). In off mode the
// disabled checker allows and the audit record names mechanism check-disabled.
// Following the mode here grants nothing the mode does not already grant: the
// passthrough gate runs the same mode-following actAs check at use time, and
// translatePassthroughForSandbox refuses translation to a hub-scoped
// assignment at create unless the mode is enforce, where this check is not
// disabled (the start and token-mint preconditions in
// admissibleGCPServiceAccount refuse such an assignment again). Relaxing that
// refusal requires revisiting this mode-following. With enforce and no token
// generator the unavailable checker denies.
func (s *Server) evaluateHostSAClaim(ctx context.Context, prior, requested hostSAClaim, surface string) hostSAClaimVerdict {
	if requested.Email == "" || requested == prior {
		return hostSAClaimVerdict{Claim: requested, Prior: prior, Allowed: true}
	}

	principal, err := s.callerPrincipal(ctx)
	if err != nil {
		// The error can carry store detail; it goes to the log only, and the
		// audit record gets a fixed reason.
		slog.WarnContext(ctx, "broker host service account: caller principal not resolved",
			"surface", surface, "targetSA", requested.Email, "error", err.Error())
		v := s.denyHostSAClaimNonUser(ctx, store.Principal{}, requested, surface,
			"caller identity could not be resolved to a principal")
		v.Prior = prior
		return v
	}
	if principal.Kind != store.PrincipalUser {
		v := s.denyHostSAClaimNonUser(ctx, principal, requested, surface,
			"caller is a "+principal.Kind.String()+" principal; only a user may set it")
		v.Prior = prior
		return v
	}

	target := hostSATarget(requested)
	result, err := store.EvaluateActAs(ctx, store.ActAsGate{
		// Off mode allows here (check-disabled) because translation is
		// refused at create unless the mode is enforce. See the doc comment.
		Checker: s.saAssignCheckerFor(),
		Caller:  principal,
		Surface: surface,
		Audit:   s.GetAuditLogger(),
	}, target)
	if err != nil {
		slog.WarnContext(ctx, "broker host service account: caller-permission check failed",
			"surface", surface, "caller", principal.ID,
			"targetSA", target.Email, "error", err.Error())
	}

	allowed := result.Outcome == store.ActAsAllowed
	if allowed {
		slog.InfoContext(ctx, "broker host service account change allowed",
			"surface", surface, "caller", principal.ID,
			"targetSA", target.Email, "mechanism", result.Mechanism)
	} else {
		slog.WarnContext(ctx, "broker host service account change denied",
			"surface", surface, "callerKind", principal.Kind.String(),
			"caller", principal.ID, "targetSA", target.Email,
			"outcome", result.Outcome.String(), "mechanism", result.Mechanism,
			"reason", result.Reason)
	}
	return hostSAClaimVerdict{Claim: requested, Prior: prior, Allowed: allowed, Checked: true, Result: result}
}

// denyHostSAClaimNonUser returns the denied verdict for a caller that is not a
// user principal and writes its audit record. The record has the shape
// store.EvaluateActAs writes, with MechanismHostSAClaimNonUser.
func (s *Server) denyHostSAClaimNonUser(ctx context.Context, caller store.Principal, requested hostSAClaim, surface, reason string) hostSAClaimVerdict {
	result := store.ActAsResult{
		Outcome:   store.ActAsDenied,
		Mechanism: MechanismHostSAClaimNonUser,
		Reason:    reason,
	}
	slog.WarnContext(ctx, "broker host service account change denied: caller is not a user",
		"surface", surface, "callerKind", caller.Kind.String(), "caller", caller.ID,
		"targetSA", requested.Email, "reason", reason)

	decision := result.Outcome
	event := &store.SAAssignmentEvent{
		Type:          store.SAAssignmentDecision,
		Surface:       surface,
		Caller:        caller,
		TargetSAEmail: requested.Email,
		Permission:    store.PermissionActAs,
		Mechanism:     result.Mechanism,
		Decision:      &decision,
		Reason:        reason,
		Timestamp:     time.Now(),
	}
	if sink := s.GetAuditLogger(); sink != nil {
		if err := sink.RecordSAAssignment(ctx, event); err != nil {
			slog.WarnContext(ctx, "failed to record SA assignment audit event",
				"surface", surface, "mechanism", result.Mechanism, "error", err)
		}
	} else {
		slog.WarnContext(ctx, "SA assignment audit sink is not wired; decision was NOT recorded",
			"surface", surface, "mechanism", result.Mechanism)
	}
	return hostSAClaimVerdict{Claim: requested, Result: result}
}

// hostSAClaimWriteAllowed is the write-site guard for a broker's host service
// account. A write that leaves the claim unchanged, or that clears the email,
// is always allowed. Any other write needs an allowed verdict made for exactly
// the claim being stored; a verdict for a different claim does not count. An
// unchecked verdict (no actAs evaluation ran) counts only while the stored
// claim is still the one it was made against, so it never authorizes a
// change back to its value after the row changed.
func hostSAClaimWriteAllowed(prior, next hostSAClaim, v hostSAClaimVerdict) error {
	if next == prior || next.Email == "" {
		return nil
	}
	if v.Allowed && v.Claim == next && (v.Checked || prior == v.Prior) {
		return nil
	}
	return ErrHostSAClaimNotAuthorized
}

// writeHostSAClaimDenied writes the 403 for a refused host service account
// change, with the mechanism-specific wording the passthrough gate uses.
func writeHostSAClaimDenied(w http.ResponseWriter, v hostSAClaimVerdict) {
	switch v.Result.Mechanism {
	case store.MechanismNoCallerIdentity, MechanismHostSAClaimNonUser:
		writeForbidden(w, "Your identity cannot be granted permission to set "+
			"gcpHostServiceAccountEmail on this broker")
	case store.MechanismCheckUnwired, store.MechanismCheckUnavailable:
		writeForbidden(w, "GCP permission checking is not available on this Hub; "+
			"setting gcpHostServiceAccountEmail is refused until it is configured")
	case store.MechanismCheckFailed:
		writeForbidden(w, "Could not verify your permission to set "+
			"gcpHostServiceAccountEmail because the check did not complete; try again")
	default:
		writeForbidden(w, "You don't have permission to set gcpHostServiceAccountEmail "+
			"on this broker ("+store.PermissionActAs+" is required on "+v.Claim.Email+")")
	}
}
