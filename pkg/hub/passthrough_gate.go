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
	"log/slog"
	"net/http"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Passthrough surface labels. The gate logic for passthrough is the same on
// create and PATCH, but the audit surface must distinguish the two — an SA
// exposed at agent creation and one swapped in via PATCH have different blast
// radii. These surface names appear in actAs audit records.
const (
	// SurfacePassthroughCreate is passthrough mode set during agent creation.
	SurfacePassthroughCreate = "passthrough-create"

	// SurfacePassthroughPatch is passthrough mode set via PATCH on an
	// existing agent.
	SurfacePassthroughPatch = "passthrough-patch"
)

// isValidServiceAccountEmail validates that an email address looks like a
// GCP service account email: <name>@<project>.iam.gserviceaccount.com.
func isValidServiceAccountEmail(email string) bool {
	at := strings.IndexByte(email, '@')
	if at <= 0 {
		return false
	}
	name := email[:at]
	domain := email[at+1:]

	// Name part must be non-empty and may contain lowercase alphanumeric,
	// hyphens, and dots (for domain-wide delegation SAs).
	if len(name) == 0 || len(name) > 100 {
		return false
	}

	suffix := ".iam.gserviceaccount.com"
	if !strings.HasSuffix(domain, suffix) {
		return false
	}

	// Project ID portion must be non-empty.
	projectID := domain[:len(domain)-len(suffix)]
	return len(projectID) > 0
}

// authorizePassthroughIdentity gates passthrough mode for a caller against a
// broker. It enforces TWO INDEPENDENT CHECKS, neither of which alone is
// sufficient:
//
//  1. Broker-owner/admin restriction — the caller must be the broker owner or
//     a hub admin.
//  2. actAs on broker host SA — the caller's GCP principal must have
//     iam.serviceAccounts.actAs on the broker's registered host service
//     account. If the broker has no host SA registered, the request is denied
//     with a configuration error (fail-closed).
//
// Both checks are required and independent. A broker owner who lacks actAs is
// denied; an admin who lacks actAs is denied.
//
// surface names the call site for audit — SurfacePassthroughCreate or
// SurfacePassthroughPatch. It affects labelling only, never the decision.
//
// Returns true if passthrough is allowed. On false, the response has been
// written and the caller must return immediately.
func (s *Server) authorizePassthroughIdentity(
	w http.ResponseWriter,
	r *http.Request,
	broker *store.RuntimeBroker,
	surface string,
) bool {
	ctx := r.Context()

	// -------------------------------------------------------------------
	// Check 1: Broker-owner/admin restriction.
	// -------------------------------------------------------------------
	userIdent := GetUserIdentityFromContext(ctx)
	if userIdent == nil {
		// Agents and brokers are not users and cannot own a broker.
		logAuthzDenial(r, GetIdentityFromContext(ctx),
			Resource{Type: "broker", ID: broker.ID}, ActionDispatch,
			"passthrough is restricted to broker owners and admins")
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"GCP identity passthrough requires broker ownership. "+
				"Only the broker owner can expose the broker's GCP identity to agents.", nil)
		return false
	}

	// Permission-based admin check via Decide replaces the former
	// inline role check. The super-admin bypass and scoped-admin access
	// are preserved through the authorization pipeline.
	isAdmin := false
	decision := s.authzService.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(userIdent),
		Credential: credentialContextForIdentity(userIdent),
		Resource:   Resource{Type: "hub", ID: "hub"},
		Action:     Action("update"),
		Permission: "hub.integrations.update",
	})
	if decision.Allowed {
		isAdmin = true
	}

	if !isAdmin && broker.CreatedBy != userIdent.ID() {
		logAuthzDenial(r, userIdent, brokerResource(broker), ActionDispatch,
			"not broker owner or admin")
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"GCP identity passthrough requires broker ownership. "+
				"Only the broker owner can expose the broker's GCP identity to agents.", nil)
		return false
	}

	// -------------------------------------------------------------------
	// Check 2: Broker host SA must be registered.
	// -------------------------------------------------------------------
	if broker.GCPHostServiceAccountEmail == "" {
		slog.Warn("passthrough denied: broker has no host service account registered",
			"surface", surface, "broker", broker.ID)
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"GCP identity passthrough requires the broker's host service account to be "+
				"registered. Set gcpHostServiceAccountEmail on the broker before using passthrough.", nil)
		return false
	}

	// -------------------------------------------------------------------
	// Check 3: actAs on broker host SA.
	// -------------------------------------------------------------------
	//
	// Synthesize a transient store.GCPServiceAccount target for the broker
	// host SA. This is not persisted — it exists only as the target shape
	// required by the frozen checker interface.
	hostProjectID := broker.GCPHostProjectID
	if hostProjectID == "" {
		// Derive from the email when the operator did not set it explicitly.
		hostProjectID = projectIDFromServiceAccountEmail(broker.GCPHostServiceAccountEmail)
	}

	target := &store.GCPServiceAccount{
		Email:     broker.GCPHostServiceAccountEmail,
		ProjectID: hostProjectID,
		Verified:  true, // broker host SA is trusted by registration
	}

	principal, err := s.callerPrincipal(ctx)
	if err != nil {
		logAuthzDenial(r, GetIdentityFromContext(ctx),
			Resource{Type: "broker", ID: broker.ID}, ActionDispatch,
			"caller principal: "+err.Error())
		writeForbidden(w, "You don't have permission to use passthrough on this broker")
		return false
	}

	result, err := store.EvaluateActAs(ctx, store.ActAsGate{
		Checker: s.saAssignCheckerFor(),
		Caller:  principal,
		Surface: surface,
		Audit:   s.GetAuditLogger(),
	}, target)
	if err != nil {
		slog.Warn("passthrough: caller-permission check failed",
			"surface", surface, "caller", principal.ID,
			"targetSA", target.Email, "error", err.Error())
	}

	if result.Outcome != store.ActAsAllowed {
		logAuthzDenial(r, GetIdentityFromContext(ctx),
			Resource{Type: "broker", ID: broker.ID}, ActionDispatch,
			"actAs "+result.Outcome.String()+" ("+result.Mechanism+"): "+result.Reason)
		slog.Warn("passthrough denied",
			"surface", surface, "callerKind", principal.Kind.String(),
			"caller", principal.ID, "targetSA", target.Email,
			"outcome", result.Outcome.String(), "mechanism", result.Mechanism,
			"reason", result.Reason)

		// Mirror the mechanism-specific messaging from authorizeSAAssignment.
		switch result.Mechanism {
		case store.MechanismNoCallerIdentity:
			writeForbidden(w, "Your identity cannot be granted permission to use passthrough on this broker")
		case store.MechanismCheckUnwired, store.MechanismCheckUnavailable:
			writeForbidden(w, "GCP permission checking is not available on this Hub; "+
				"passthrough is refused until it is configured")
		case store.MechanismCheckFailed:
			writeForbidden(w, "Could not verify your permission to use passthrough "+
				"because the check did not complete; try again")
		default:
			writeForbidden(w, "You don't have permission to use passthrough on this broker ("+
				store.PermissionActAs+" is required on "+target.Email+")")
		}
		return false
	}

	slog.Info("passthrough allowed",
		"surface", surface, "callerKind", principal.Kind.String(),
		"caller", principal.ID, "targetSA", target.Email,
		"mechanism", result.Mechanism)
	return true
}
