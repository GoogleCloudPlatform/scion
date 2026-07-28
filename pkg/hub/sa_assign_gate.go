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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// SurfaceAgentAssign names the agent service-account assignment surface in
// audit records and in the startup warning. Surfaces are named individually
// rather than collectively because the same disabled checker degrades to
// different things in different places — see NewDisabledCallerPermissionChecker.
const SurfaceAgentAssign = "agent-assign"

// saAssignCheckMode values. The mode gates the GCP layer only; the Hub policy
// layer is not switchable.
const (
	// SAAssignCheckOff skips the CanActAs call entirely. Assignment is then
	// gated by Hub policy alone.
	SAAssignCheckOff = "off"

	// SAAssignCheckEnforce runs the CanActAs call and denies on anything other
	// than a positive allow.
	SAAssignCheckEnforce = "enforce"
)

var (
	errNoCallerIdentity      = errors.New("no caller identity on request context")
	errUnsupportedCallerKind = errors.New("caller kind may not assign service accounts")
)

// callerPrincipal builds the store.Principal for the caller of the current
// request.
//
// The IMMEDIATE caller, never the ancestry. An agent started by an admin but
// holding a low-privilege service account must not be able to pass on the
// admin's authority to a child it creates; consulting the originating human
// would be weaker, not stronger. store.Principal's doc comment says the same
// thing from the other side.
//
// A returned error means "this caller cannot be established" and must be
// treated as a denial by every caller of this function. It never means allow.
// The zero store.Principal is PrincipalUnknown, which HasGCPIdentity reports
// false for, so even a caller that ignored the error would fail closed — but
// do not rely on that, check the error.
func (s *Server) callerPrincipal(ctx context.Context) (store.Principal, error) {
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		return store.Principal{}, errNoCallerIdentity
	}

	// A switch with a terminating default, not a chain of ifs: an unhandled
	// caller kind falling through the guard is precisely the #591 bug. Broker
	// callers land in default and are denied.
	switch identity.Type() {
	case "agent":
		agentIdent, ok := identity.(AgentIdentity)
		if !ok {
			return store.Principal{}, errUnsupportedCallerKind
		}
		agentRecord, err := s.store.GetAgent(ctx, agentIdent.ID())
		if err != nil {
			// Deliberately NOT the `if err == nil` shape used for attribution
			// in createAgentInProject. A caller we cannot resolve is a caller
			// we cannot authorize.
			return store.Principal{}, err
		}
		p := store.Principal{Kind: store.PrincipalAgent, ID: agentIdent.ID()}
		// ServiceAccountEmail is populated only in assign mode, so the mode has
		// to be checked before the email is trusted. A passthrough-mode agent
		// borrows the broker's identity rather than holding one of its own, so
		// it is treated like block mode here: no GCP identity of its own,
		// therefore nothing that could have been delegated to it.
		if cfg := agentRecord.AppliedConfig; cfg != nil && cfg.GCPIdentity != nil &&
			cfg.GCPIdentity.MetadataMode == store.GCPMetadataModeAssign {
			p.ServiceAccountEmail = cfg.GCPIdentity.ServiceAccountEmail
		}
		return p, nil

	// "dev" accompanies "user" here for the same reason it does in
	// authorizeAgentCreate, checkAccess and buildPrincipals: a dev-auth caller
	// IS a user identity, and omitting it would deny every assignment on a
	// dev-auth hub rather than gate it.
	case "user", "dev":
		userIdent, ok := identity.(UserIdentity)
		if !ok {
			return store.Principal{}, errUnsupportedCallerKind
		}
		return store.Principal{
			Kind:  store.PrincipalUser,
			ID:    userIdent.ID(),
			Email: userIdent.Email(),
		}, nil

	default:
		return store.Principal{}, errUnsupportedCallerKind
	}
}

// saAssignCheckerFor resolves the caller-permission checker for one request.
//
// Resolved per request rather than once at construction because
// gcpTokenGenerator is late-wired: SetGCPTokenGenerator runs after NewServer,
// so the generator is always nil at construction time and a decision made
// there would be permanently wrong.
//
// The three outcomes, in the order the brief requires:
//
//  1. Mode off — the configured checker, which is the disabled one. Skips the
//     GCP call and allows, recording Mechanism "check-disabled". This case
//     dominates: with checking off, a missing generator is not a problem
//     because nothing was going to call it.
//  2. Mode on, no generator — the unavailable checker, which DENIES. An absent
//     capability must never become an implicit pass. This is the exact defect
//     in verifyGCPServiceAccount (handlers_gcp_identity.go:471), where the nil
//     guard wraps the probe but not the success assignment, so a hub with no
//     generator marks every account verified having contacted nothing.
//  3. Mode on, generator present — the configured checker.
//
// Substituting the unavailable checker in case 2 is a deliberate downgrade on
// a missing capability, not a fallback for an unset field. There is no path
// here that invents a permissive checker; the field is always explicitly
// installed at the wiring site.
func (s *Server) saAssignCheckerFor() store.CallerPermissionChecker {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.saAssignCheckMode == SAAssignCheckOff {
		return s.saAssignChecker
	}
	if s.gcpTokenGenerator == nil {
		return store.NewUnavailableCallerPermissionChecker(
			"caller-permission checking is enforced but this Hub has no GCP token generator configured")
	}
	return s.saAssignChecker
}

// hookIdentityCheckerFor resolves the caller-permission checker for one
// lifecycle-hook validation. Same three cases and same reasoning as
// saAssignCheckerFor — read that first — against this surface's own mode and
// checker fields.
//
// Kept as a separate function rather than a shared one parameterised by
// surface: the two are one short function each, and the cost of the duplication
// is far lower than the cost of a future change to "the checker resolver"
// silently altering a surface whose degraded state is worse. The decision that
// must not be duplicated is the actAs ordering, and that lives in
// store.EvaluateActAs.
func (s *Server) hookIdentityCheckerFor() store.CallerPermissionChecker {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.hookIdentityCheckMode == SAAssignCheckOff {
		return s.hookIdentityChecker
	}
	if s.gcpTokenGenerator == nil {
		return store.NewUnavailableCallerPermissionChecker(
			"caller-permission checking is enforced but this Hub has no GCP token generator configured")
	}
	return s.hookIdentityChecker
}

// authorizeSAAssignment gates assigning a GCP service account to an agent.
//
// TWO INDEPENDENT LAYERS, BOTH REQUIRED, NEITHER SUBSUMING THE OTHER:
//
//  1. The Hub policy layer, via authorizeMsg on ActionAssign. This answers
//     "may this caller assign service accounts here" in Scion's own model.
//  2. The GCP layer, via CanActAs. This answers "may this caller act as THIS
//     service account" in Google's model, and it is the check that makes
//     assignment something other than a Scion-local opinion.
//
// The policy layer runs first so a caller with no business on this surface is
// refused without a GCP round-trip, and so each denial reason stays in the
// layer that owns it.
//
// ⚠️ ActionAssign, not ActionRead. A grant to READ a service account is not a
// grant to ASSIGN one; the two were conflated here until svc-accnt Step 2.
// Reachability is preserved by the assign baselines that landed first for this
// purpose: authz.go step 3b for agent callers, the per-project
// member-assign-service-accounts policy in seed.go for humans.
//
// ⚠️ WHAT THE CONVERSION CHANGES DEPENDS ON THE CALLER KIND. Hub scope removes
// confinement for humans and adds it for agents, so no single sentence about
// "the gate" on a hub-scoped account is true of both:
//
//   - HUMAN caller, HUB-scoped (parentless) account: behaviour CHANGES. Under
//     ActionRead the seeded hub-members read-all policy matched it, because
//     matchesResource's scope switch has no "hub" arm and falls through to
//     true. Nothing grants hub-wide assign, so under ActionAssign a plain hub
//     member is now DENIED and only hub admins and the account's creator pass.
//     That looks like a regression and is the ruled fail-closed answer
//     (design-draft §8.2). Do NOT "fix" it with a hub-scoped assign policy —
//     hub-scoped policies match every resource, so that would grant every hub
//     member every service account on the hub. It is also the one behaviour
//     change worth a release note.
//   - AGENT caller, HUB-scoped account: behaviour is UNCHANGED, already denied
//     before and after. Agent principals come only from the agent's own groups,
//     which never include hub-members, so the read-all policy was never fetched
//     for them; and both the read baseline and the assign baseline require
//     pid != "", which a parentless resource fails.
//   - Either caller, PROJECT-scoped account in its own project: UNCHANGED,
//     allowed, via the two baselines above. A denial here is a real bug and
//     should be reported rather than explained away by §8.2 — that ruling is
//     about hub-scoped accounts and plain hub members, and nothing else.
//
// Returns true if the assignment may proceed. On false it has already written
// the response and the caller must return immediately.
func (s *Server) authorizeSAAssignment(w http.ResponseWriter, r *http.Request, sa *store.GCPServiceAccount) bool {
	if sa == nil {
		// Caller bug rather than a policy outcome; deny rather than panic.
		slog.Error("service-account assignment denied: nil service account",
			"surface", SurfaceAgentAssign)
		writeForbidden(w, "")
		return false
	}

	// Layer 1: Hub policy.
	if !s.authorizeMsg(w, r, gcpServiceAccountResource(sa), ActionAssign,
		"You don't have permission to assign this GCP service account") {
		return false
	}

	// Layer 2: GCP actAs.
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	resource := gcpServiceAccountResource(sa)

	principal, err := s.callerPrincipal(ctx)
	if err != nil {
		logAuthzDenial(r, identity, resource, ActionAssign, "caller principal: "+err.Error())
		writeForbidden(w, "You don't have permission to assign this GCP service account")
		return false
	}

	// The decision sequence — same-account propagation, no-GCP-identity denial,
	// unwired-checker denial, then the checker itself — lives in
	// store.EvaluateActAs and is shared with the lifecycle-hook
	// execution-identity surface. This surface owns how the result is REPORTED
	// and logged, not how it is reached. Do not reintroduce any of those steps
	// here; two copies of this ordering is the thing EvaluateActAs exists to
	// prevent.
	result, err := store.EvaluateActAs(ctx, s.saAssignCheckerFor(), principal, sa)
	if err != nil {
		// Per the interface contract an error is a transport or programming
		// failure and carries no verdict; EvaluateActAs has already forced the
		// outcome to Indeterminate, which denies below. Logged apart from the
		// outcome so a transport failure is not read as an IAM denial.
		slog.Warn("service-account assignment: caller-permission check failed",
			"surface", SurfaceAgentAssign, "caller", principal.ID,
			"targetSA", sa.Email, "error", err.Error())
	}

	if result.Outcome != store.ActAsAllowed {
		// Indeterminate denies. What indeterminate means is the caller's choice
		// to make once, at a single site, and this is that site.
		logAuthzDenial(r, identity, resource, ActionAssign,
			"actAs "+result.Outcome.String()+" ("+result.Mechanism+"): "+result.Reason)
		slog.Warn("service-account assignment denied",
			"surface", SurfaceAgentAssign, "callerKind", principal.Kind.String(),
			"caller", principal.ID, "targetSA", sa.Email,
			"outcome", result.Outcome.String(), "mechanism", result.Mechanism,
			"reason", result.Reason)

		// The 403 body varies by mechanism because the remedies are different
		// and none of them is guessable from "denied": a missing actAs grant is
		// fixed in IAM, a caller with no GCP identity is fixed by giving the
		// agent one, and an unwired checker is fixed by an operator and not by
		// the caller at all. Mechanism is not secret — it names which check
		// ran, never what any policy contains.
		switch result.Mechanism {
		case store.MechanismNoCallerIdentity:
			writeForbidden(w, "Your identity cannot be granted permission to use this GCP service account")
		case store.MechanismCheckUnwired, store.MechanismCheckUnavailable:
			writeForbidden(w, "GCP permission checking is not available on this Hub; "+
				"service-account assignment is refused until it is configured")
		default:
			writeForbidden(w, "You don't have permission to use this GCP service account ("+
				store.PermissionActAs+" is required on "+sa.Email+")")
		}
		return false
	}

	// Mechanism is recorded on the allow path too, and this is the point of it:
	// "allowed because IAM said so" and "allowed because nobody asked" are the
	// same outcome and different facts. Only one of them is a control having
	// been applied, and the audit record is where that difference survives.
	slog.Info("service-account assignment allowed",
		"surface", SurfaceAgentAssign, "callerKind", principal.Kind.String(),
		"caller", principal.ID, "targetSA", sa.Email,
		"mechanism", result.Mechanism)
	return true
}
