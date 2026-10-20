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
// GCP service account email. Accepted formats:
//
//   - Custom IAM SA:       <name>@<project>.iam.gserviceaccount.com
//   - Default Compute SA:  <number>-compute@developer.gserviceaccount.com
//   - App Engine default:  <project-id>@appspot.gserviceaccount.com
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

	switch {
	case strings.HasSuffix(domain, ".iam.gserviceaccount.com"):
		// Custom IAM SA: project ID portion must be non-empty.
		projectID := domain[:len(domain)-len(".iam.gserviceaccount.com")]
		return len(projectID) > 0
	case domain == "developer.gserviceaccount.com":
		// Default Compute Engine SA (e.g. <project-number>-compute@developer.gserviceaccount.com).
		return true
	case domain == "appspot.gserviceaccount.com":
		// App Engine default SA (e.g. <project-id>@appspot.gserviceaccount.com).
		return true
	default:
		return false
	}
}

// authorizePassthroughIdentity gates passthrough mode for a caller against a
// broker: evaluatePassthroughIdentity, with a refusal written to w.
//
// Returns true if passthrough is allowed. On false, the response has been
// written and the caller must return immediately.
func (s *Server) authorizePassthroughIdentity(
	w http.ResponseWriter,
	r *http.Request,
	broker *store.RuntimeBroker,
	surface string,
) bool {
	d := s.evaluatePassthroughIdentity(r.Context(), r, broker, surface)
	if d == nil {
		return true
	}
	d.write(w)
	return false
}

// passthroughDenial is a refusal from evaluatePassthroughIdentity. It carries
// the message the HTTP surface writes (403 forbidden), and satisfies error so
// callers without a response writer can fail with the same message.
type passthroughDenial struct {
	msg string
}

func (d *passthroughDenial) Error() string { return d.msg }

func (d *passthroughDenial) write(w http.ResponseWriter) { writeForbidden(w, d.msg) }

// evaluatePassthroughIdentity is the transport-independent body of the
// passthrough gate, with the caller taken from the identity on ctx. It
// enforces TWO INDEPENDENT CHECKS, neither of which alone is sufficient:
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
// It returns nil when passthrough is allowed. r is used only to name the
// request in denial logs and may be nil for callers with no HTTP request.
func (s *Server) evaluatePassthroughIdentity(
	ctx context.Context,
	r *http.Request,
	broker *store.RuntimeBroker,
	surface string,
) *passthroughDenial {
	// -------------------------------------------------------------------
	// Check 1: Broker-owner/admin restriction.
	// -------------------------------------------------------------------
	userIdent := GetUserIdentityFromContext(ctx)
	if userIdent == nil {
		// Agents and brokers are not users and cannot own a broker.
		logAuthzDenial(r, GetIdentityFromContext(ctx),
			Resource{Type: "broker", ID: broker.ID}, ActionDispatch,
			"passthrough is restricted to broker owners and admins")
		return &passthroughDenial{msg: "GCP identity passthrough requires broker ownership. " +
			"Only the broker owner can expose the broker's GCP identity to agents."}
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

	// The embedded (co-located) broker counts as owned by an unscoped local
	// platform administrator. On single-node deployments the embedded broker
	// is registered without a CreatedBy value, so it has no recorded owner.
	// It is identified by the broker ID the server recorded at startup, as
	// in the hub-default check, not by its labels.
	// The test-admin policy keeps this to human admins: it would expose
	// the co-located host's GCP identity to agents.
	if !isAdmin && s.isEmbeddedBroker(broker.ID) &&
		IsUnscopedLocalPlatformAdmin(userIdent) &&
		!s.testAdminPolicyRefuses(ctx, "passthrough_gate.go:evaluatePassthroughIdentity") {
		isAdmin = true
	}

	if !isAdmin && broker.CreatedBy != userIdent.ID() {
		logAuthzDenial(r, userIdent, brokerResource(broker), ActionDispatch,
			"not broker owner or admin")
		return &passthroughDenial{msg: "GCP identity passthrough requires broker ownership. " +
			"Only the broker owner can expose the broker's GCP identity to agents."}
	}

	// -------------------------------------------------------------------
	// Check 2: Broker host SA must be registered.
	// -------------------------------------------------------------------
	if broker.GCPHostServiceAccountEmail == "" {
		slog.Warn("passthrough denied: broker has no host service account registered",
			"surface", surface, "broker", broker.ID)
		return &passthroughDenial{msg: "GCP identity passthrough requires the broker's host service account to be " +
			"registered. Set gcpHostServiceAccountEmail on the broker before using passthrough."}
	}

	// -------------------------------------------------------------------
	// Check 3: actAs on broker host SA.
	// -------------------------------------------------------------------
	//
	// The transient actAs target for the broker host SA is shared with the
	// host service account setters (hostSATarget), so a claim is checked
	// against the same resource when it is set and when it is used.
	target := hostSATarget(hostSAClaim{
		Email:     broker.GCPHostServiceAccountEmail,
		ProjectID: broker.GCPHostProjectID,
	})

	principal, err := s.callerPrincipal(ctx)
	if err != nil {
		logAuthzDenial(r, GetIdentityFromContext(ctx),
			Resource{Type: "broker", ID: broker.ID}, ActionDispatch,
			"caller principal: "+err.Error())
		return &passthroughDenial{msg: "You don't have permission to use passthrough on this broker"}
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
			return &passthroughDenial{msg: "Your identity cannot be granted permission to use passthrough on this broker"}
		case store.MechanismCheckUnwired, store.MechanismCheckUnavailable:
			return &passthroughDenial{msg: "GCP permission checking is not available on this Hub; " +
				"passthrough is refused until it is configured"}
		case store.MechanismCheckFailed:
			return &passthroughDenial{msg: "Could not verify your permission to use passthrough " +
				"because the check did not complete; try again"}
		default:
			return &passthroughDenial{msg: "You don't have permission to use passthrough on this broker (" +
				store.PermissionActAs + " is required on " + target.Email + ")"}
		}
	}

	slog.Info("passthrough allowed",
		"surface", surface, "callerKind", principal.Kind.String(),
		"caller", principal.ID, "targetSA", target.Email,
		"mechanism", result.Mechanism)
	return nil
}
