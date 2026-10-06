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

// SurfaceAgentAssign names the agent service-account assignment surface as a
// family, for the startup warning and for wiring. Surfaces are named
// individually rather than collectively because the same disabled checker
// degrades to different things in different places — see
// NewDisabledCallerPermissionChecker.
const SurfaceAgentAssign = "agent-assign"

// The individual call sites within that family. Audit records name these
// rather than SurfaceAgentAssign, because "an SA was assigned at agent
// creation" and "an SA was swapped onto an existing agent" are different
// events with different blast radii, and a record that cannot tell them apart
// cannot answer the question anyone actually asks of it (design §7).
//
// The gate logic is identical for both; only the label differs.
const (
	// SurfaceAgentCreate is assignment during agent creation.
	SurfaceAgentCreate = "agent-create"

	// SurfaceAgentPatch is assignment onto an already-existing agent.
	SurfaceAgentPatch = "agent-patch"

	// SurfaceAgentReincarnate is assignment onto an existing agent's next
	// generation by `scion reincarnate --service-account`
	// (ptone/scion#3302). Unlike PATCH it applies to a running agent.
	SurfaceAgentReincarnate = "agent-reincarnate"

	// SurfaceProjectDefault is an SA assigned from project settings rather than
	// supplied by the caller. P10 changed the ruling: project-default assignment
	// now runs the full authorization gate (ActionAssign + actAs) against the
	// immediate agent creator. This surface produces DECISION records via
	// authorizeSAAssignment, not the binding records it produced before P10.
	SurfaceProjectDefault = "project-default"

	// SurfaceHubDefault is an SA assigned from the hub-level agent_defaults
	// operational setting, one rung below SurfaceProjectDefault in the GCP
	// identity fallback ladder (explicit request -> project default -> hub
	// default -> unset, the broker applies its runtime default). Same
	// authorization gate as SurfaceProjectDefault.
	SurfaceHubDefault = "hub-default"
)

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
//  1. Mode off — the disabled checker, unconditionally. Skips the GCP call
//     and allows, recording Mechanism "check-disabled". This case dominates:
//     with checking off, a missing generator is not a problem because nothing
//     was going to call it. The disabled checker is constructed here rather
//     than read from s.saAssignChecker so that the mode decision is
//     authoritative: SetSAAssignChecker (called unconditionally during
//     server_foreground startup) overwrites the field with the PT checker,
//     which would otherwise be returned when mode=off, defeating the switch.
//  2. Mode on, no generator — the unavailable checker, which DENIES. An absent
//     capability must never become an implicit pass. This is the exact defect
//     in verifyGCPServiceAccount (handlers_gcp_identity.go, tracked as #29),
//     where the nil guard wraps the probe but not the success assignment, so a
//     hub with no generator marks every account verified having contacted
//     nothing. ⚠️ The #29 fix must update this comment when it lands: once the
//     defect is gone, this stops being a live example and becomes a false
//     statement about the current tree.
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
		return store.NewDisabledCallerPermissionChecker()
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
		return store.NewDisabledCallerPermissionChecker()
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
// grant to ASSIGN one. Reachability for project-scoped accounts comes from
// authz.go's AgentScopes wiring (project:agent:sa_assign) for agent callers,
// and from the gcp_service_account.assign permission curated into the
// project-owner, project-admin and project-member RoleDefinitions in
// seed.go for humans (ptone/scion#2147). effectiveAgentScopes also grants
// project:agent:sa_assign to a verified agent JWT that carries no
// scope_schema claim and holds project:agent:create.
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
// surface names the call site for the audit record — SurfaceAgentCreate,
// SurfaceAgentPatch or SurfaceAgentReincarnate. It affects labelling only, never the decision.
//
// Returns true if the assignment may proceed. On false it has already written
// the response and the caller must return immediately.
func (s *Server) authorizeSAAssignment(w http.ResponseWriter, r *http.Request, sa *store.GCPServiceAccount, surface string) bool {
	denial := s.evaluateSAAssignment(r.Context(), r, sa, surface)
	if denial == nil {
		return true
	}
	denial.write(w)
	return false
}

// saAssignDenialKind selects how an saAssignDenial is rendered as HTTP.
type saAssignDenialKind int

const (
	saAssignDenyForbidden saAssignDenialKind = iota
	saAssignDenyForbiddenStructured
	saAssignDenyUnauthorized
)

// saAssignDenial is a refusal from evaluateSAAssignment. It carries exactly
// what the HTTP surface writes, so authorizeSAAssignment's responses are
// unchanged, and it satisfies error so non-HTTP callers (the scheduler's
// dispatch_agent path) can fail with the same message.
type saAssignDenial struct {
	kind         saAssignDenialKind
	msg          string
	resourceType string
}

func (d *saAssignDenial) Error() string {
	switch {
	case d.kind == saAssignDenyUnauthorized:
		return "service-account assignment denied: no caller identity"
	case d.msg != "":
		return "service-account assignment denied: " + d.msg
	default:
		return "service-account assignment denied: insufficient permissions"
	}
}

func (d *saAssignDenial) write(w http.ResponseWriter) {
	switch d.kind {
	case saAssignDenyUnauthorized:
		Unauthorized(w)
	case saAssignDenyForbiddenStructured:
		writeForbiddenStructured(w, d.msg, d.resourceType, ActionAssign)
	default:
		writeForbidden(w, d.msg)
	}
}

// saAssignGenericForbiddenMsg is the response for every SA-assign denial that
// has no more specific diagnosis. That spans both layers: Layer 1 (Hub
// policy) uses it for ordinary policy denials, a ceiling store fault
// (DenyCauseCeilingError), and the no-authz-service guard; Layer 2 (GCP
// actAs) uses it when the caller principal cannot be resolved. It must stay
// byte-identical: it predates DenyCause and callers may already match on it.
const saAssignGenericForbiddenMsg = "You don't have permission to assign this GCP service account"

// saAssignForbiddenMessage maps a Decision.DenyCause to the 403 body Layer 1
// of evaluateSAAssignment returns. Pulled out as its own function so a table
// test can drive every DenyCause value, including one no constant names,
// without going through the full evaluateSAAssignment call chain.
//
// The orphaned and lacks-permission messages name "a principal in its
// delegation chain" rather than "the principal that created it": cause is
// set (and propagated) at
// every depth of walkDelegationChain's recursion (authz_delegation_ceiling.go),
// so the failing link can be the agent's own creator or any creator further
// up the chain. Saying "the principal that created it" would be false
// whenever the failure is a grandparent or higher — see the DenyCause doc
// comment on authz.go, which already says "directly or transitively".
//
// DenyCauseCeilingUnrecorded names the usual origin of the cause, an agent
// created without recorded provenance, and the remedy that clears it. The
// unrecorded hop can be this agent's own edge or any edge further up the
// chain. A user's create writes the new agent's edge with recorded provenance
// (commitAgentCreate), and because a user is the root of a chain, a
// user-created agent's chain contains only that one recorded edge. So having
// a user recreate this agent directly always clears the cause, whether the
// unrecorded link was this agent or an ancestor; the message does not need to
// identify which hop failed. Recreating the agent from an agent whose chain
// includes the unrecorded hop (for example the same parent) keeps that hop,
// and reincarnating an agent keeps its existing edge, so neither clears the
// cause. The same cause also covers a hop whose provenance version this
// binary does not interpret (hopEffectCeilingDeny); the remedy is the same
// for both.
//
// DenyCauseCeilingError and any unrecognised cause (including "", the zero
// value) fall through to the generic message: a store fault is
// transient/internal, not a fact about the caller worth surfacing, and an
// unknown cause is safer treated as no diagnosis than guessed at.
func saAssignForbiddenMessage(cause DenyCause) string {
	switch cause {
	case DenyCauseCeilingOrphaned:
		return "This agent cannot assign service accounts: a principal in its delegation chain " +
			"(the user or agent that created it, or one of their creators) does not exist. " +
			"Ask an admin to recreate the agent under a current user."
	case DenyCauseCeilingDelegatorLacksPermission:
		return "This agent cannot assign service accounts: a principal in its delegation chain " +
			"(the user or agent that created it, or one of their creators) does not hold permission " +
			"to assign this service account."
	case DenyCauseCeilingUnrecorded:
		return "This agent cannot assign service accounts: its delegation chain includes an agent " +
			"created without recorded provenance (this agent or one of the agents that created it). " +
			"Have an authorized user recreate this agent directly (not from another agent)."
	default:
		return saAssignGenericForbiddenMsg
	}
}

// evaluateSAAssignment is the transport-independent body of
// authorizeSAAssignment: every check, log line and audit record, with the
// caller taken from the identity on ctx. It returns nil when the assignment
// may proceed. r is used only to name the request path in denial logs and may
// be nil for callers with no HTTP request (the scheduler); logAuthzDenial
// accepts a nil request, and TestEvaluateSAAssignment_NilRequest* pin that.
func (s *Server) evaluateSAAssignment(ctx context.Context, r *http.Request, sa *store.GCPServiceAccount, surface string) *saAssignDenial {
	if sa == nil {
		// Caller bug rather than a policy outcome; deny rather than panic.
		slog.Error("service-account assignment denied: nil service account",
			"surface", surface)
		return &saAssignDenial{kind: saAssignDenyForbidden}
	}

	// Precondition: hub-scoped SA assignment requires gcpIamCheckMode=enforce.
	//
	// This is D4: assignment-time coupling, not registration-time. Registration
	// checks are insufficient because the mode can be switched off later.
	// gcpIamCheckMode=off remains a transitional escape hatch for project-scoped
	// assignment only; hub-scoped SAs carry hub-wide blast radius and must not
	// become assignable when the GCP permission check is disabled.
	if sa.Scope == store.ScopeHub {
		s.mu.RLock()
		mode := s.saAssignCheckMode
		s.mu.RUnlock()
		if mode != SAAssignCheckEnforce {
			slog.Warn("hub-scoped SA assignment denied: gcpIamCheckMode is not enforce",
				"surface", surface, "targetSA", sa.Email, "mode", mode)
			return &saAssignDenial{kind: saAssignDenyForbidden,
				msg: "Hub-scoped service account assignment requires gcpIamCheckMode=enforce"}
		}
	}

	identity := GetIdentityFromContext(ctx)
	resource := gcpServiceAccountResource(sa)

	// Layer 1: Hub policy. Same decision and responses as authorizeMsg.
	if identity == nil {
		return &saAssignDenial{kind: saAssignDenyUnauthorized}
	}
	if s.authzService == nil {
		logAuthzDenial(r, identity, resource, ActionAssign, "no authz service")
		return &saAssignDenial{kind: saAssignDenyForbiddenStructured,
			msg: saAssignGenericForbiddenMsg, resourceType: resource.Type}
	}
	if decision := s.authzService.CheckAccess(ctx, identity, resource, ActionAssign); !decision.Allowed {
		logAuthzDenial(r, identity, resource, ActionAssign, decision.Reason)
		return &saAssignDenial{kind: saAssignDenyForbiddenStructured,
			msg: saAssignForbiddenMessage(decision.DenyCause), resourceType: resource.Type}
	}

	// Layer 2: GCP actAs.
	principal, err := s.callerPrincipal(ctx)
	if err != nil {
		logAuthzDenial(r, identity, resource, ActionAssign, "caller principal: "+err.Error())
		return &saAssignDenial{kind: saAssignDenyForbidden,
			msg: saAssignGenericForbiddenMsg}
	}

	// The decision sequence — same-account propagation, no-GCP-identity denial,
	// unwired-checker denial, then the checker itself — lives in
	// store.EvaluateActAs and is shared with the lifecycle-hook
	// execution-identity surface. This surface owns how the result is REPORTED
	// and logged, not how it is reached. Do not reintroduce any of those steps
	// here; two copies of this ordering is the thing EvaluateActAs exists to
	// prevent.
	//
	// EvaluateActAs also emits the §7 audit record, on allow and deny alike.
	// That is why it takes the sink: the record is not this surface's to
	// remember, and a surface that forgot it would be indistinguishable from
	// one where nothing happened.
	result, err := store.EvaluateActAs(ctx, store.ActAsGate{
		Checker: s.saAssignCheckerFor(),
		Caller:  principal,
		Surface: surface,
		Audit:   s.GetAuditLogger(),
	}, sa)
	if err != nil {
		// Per the interface contract an error is a transport or programming
		// failure and carries no verdict; EvaluateActAs has already forced the
		// outcome to Indeterminate, which denies below. Logged apart from the
		// outcome so a transport failure is not read as an IAM denial.
		slog.Warn("service-account assignment: caller-permission check failed",
			"surface", surface, "caller", principal.ID,
			"targetSA", sa.Email, "error", err.Error())
	}

	if result.Outcome != store.ActAsAllowed {
		// Indeterminate denies. What indeterminate means is the caller's choice
		// to make once, at a single site, and this is that site.
		logAuthzDenial(r, identity, resource, ActionAssign,
			"actAs "+result.Outcome.String()+" ("+result.Mechanism+"): "+result.Reason)
		slog.Warn("service-account assignment denied",
			"surface", surface, "callerKind", principal.Kind.String(),
			"caller", principal.ID, "targetSA", sa.Email,
			"outcome", result.Outcome.String(), "mechanism", result.Mechanism,
			"reason", result.Reason)

		// The 403 body varies by mechanism because the remedies are different
		// and none of them is guessable from "denied": a missing actAs grant is
		// fixed in IAM, a caller with no GCP identity is fixed by giving the
		// agent one, and an unwired checker is fixed by an operator and not by
		// the caller at all. Mechanism is not secret — it names which check
		// ran, never what any policy contains.
		var msg string
		switch result.Mechanism {
		case store.MechanismNoCallerIdentity:
			msg = "Your identity cannot be granted permission to use this GCP service account"
		case store.MechanismCheckUnwired, store.MechanismCheckUnavailable:
			msg = "GCP permission checking is not available on this Hub; " +
				"service-account assignment is refused until it is configured"
		case store.MechanismCheckFailed:
			// Transient and not the caller's fault. Deliberately does not tell
			// them to request a grant they may already hold.
			msg = "Could not verify your permission to use this GCP service " +
				"account because the check did not complete; try again"
		case store.MechanismUnattributableAllow:
			// A checker bug, not a caller problem. Says nothing actionable to
			// the caller because there is nothing they can do about it.
			msg = "Could not verify your permission to use this GCP service account"
		default:
			msg = "You don't have permission to use this GCP service account (" +
				store.PermissionActAs + " is required on " + sa.Email + ")"
		}
		return &saAssignDenial{kind: saAssignDenyForbidden, msg: msg}
	}

	// Mechanism is recorded on the allow path too, and this is the point of it:
	// "allowed because IAM said so" and "allowed because nobody asked" are the
	// same outcome and different facts. Only one of them is a control having
	// been applied, and the audit record is where that difference survives.
	slog.Info("service-account assignment allowed",
		"surface", surface, "callerKind", principal.Kind.String(),
		"caller", principal.ID, "targetSA", sa.Email,
		"mechanism", result.Mechanism)
	return nil
}
