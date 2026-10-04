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

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
)

// Bearer gate stage names. A stage names the first gate stage that denied a
// bearer-credential request; bearerStagePassed records that every gate
// stage passed and evaluation continued to the kernel.
const (
	bearerStageBoundaryInvalid = "boundary_invalid"
	bearerStageTargetUnknown   = "target_unknown"
	bearerStageOutsideBoundary = "outside_boundary"
	bearerStageCeiling         = "ceiling"
	bearerStageProjectAccess   = "project_access"
	bearerStagePassed          = "passed"
)

// Deny reasons the bearer gate reports. They are stable strings: callers
// and tests match on them.
const (
	bearerReasonBoundaryInvalid     = "token boundary is invalid"
	bearerReasonTargetUnknown       = "token target scope cannot be resolved"
	bearerReasonOutsideProject      = "token not scoped for this project"
	bearerReasonHubLevelResource    = "token not scoped for hub-level resources"
	bearerReasonProjectAccessDenied = "token holder lacks active access to the target project"
)

// bearerGateTrace records how the bearer gate evaluated one request. decide
// sets it on the Decision it returns for every request that reached the
// gate. It never affects the authorization result.
type bearerGateTrace struct {
	// Stage is the first gate stage that denied, or bearerStagePassed. It
	// is empty when the request never reached the gate.
	Stage string
	// TargetScope is the resolved target scope. It is set once the
	// boundary is valid.
	TargetScope TargetScope
	// AccessSource is the evidence that admitted a project target. It is
	// set only when the project access stage admitted the request.
	AccessSource ProjectAccessSource
}

// bearerGateInputs is the credential-side input to the bearer gate: the
// boundary and ceiling the request is confined to.
type bearerGateInputs struct {
	boundary TokenBoundary
	ceiling  permissions.FrozenPermissionCeiling
	// missing is true when the credential is a typed-nil
	// *ScopedUserIdentity: it carries no boundary or ceiling, and the gate
	// denies it.
	missing bool
}

// bearerGateInputsFor selects the boundary and ceiling the bearer gate
// enforces for a UAT-kind request, and reports whether the gate applies.
//
// A *ScopedUserIdentity principal is confined by its own boundary and
// ceiling, the ones ValidateToken loaded from the stored token row. A
// caller-supplied CredentialContext never replaces them, so supplied
// context cannot widen a token.
//
// Any other principal is a local user whose request carries a supplied
// UAT-kind credential. Such a credential is gated when it names a
// Boundary, and is confined to that Boundary and its Ceiling. The
// principal's own authority still decides the request in the kernel, so a
// supplied boundary can only narrow it. A supplied UAT-kind credential
// without a Boundary is not gated here; the kernel's credential_scope
// restriction (step 7a) still applies its Ceiling.
func bearerGateInputsFor(principal PrincipalContext, credential CredentialContext) (bearerGateInputs, bool) {
	if scoped, ok := principal.Identity.(*ScopedUserIdentity); ok {
		if scoped == nil {
			return bearerGateInputs{missing: true}, true
		}
		return bearerGateInputs{boundary: scoped.Boundary(), ceiling: scoped.Ceiling()}, true
	}
	if credential.Boundary != nil {
		return bearerGateInputs{boundary: *credential.Boundary, ceiling: credential.Ceiling}, true
	}
	return bearerGateInputs{}, false
}

// evaluateBearerGate is the pre-kernel gate for a bearer credential that is
// confined to a boundary and a permission ceiling. It runs these stages in
// order and returns a deny Decision from the first stage that fails, or nil
// when every stage passes:
//
//  1. The boundary is valid (TokenBoundary.Valid).
//  2. The target resolves to a known scope (ResolveTargetScope with the
//     request's evidence), and the boundary allows it (BoundaryAllows). An
//     unresolvable target denies for every boundary kind. Collection-level
//     evidence must name permissionID; evidence for any other permission
//     denies as an unresolvable target.
//  3. The ceiling allows the exact permission Decide resolved for the
//     request.
//  4. For a project target, the principal currently has access to that
//     project for this permission and target (ProjectTargetAdmission). This
//     stage applies to every boundary kind, hub included: a hub boundary
//     never stands in for current project access.
//
// The gate only narrows. Passing it is necessary, not sufficient: the
// kernel, relationship grants and access constraints still decide the
// request, with the ceiling applied again as a kernel restriction.
//
// trace receives the stage outcome; it may be nil. memo is the
// request-scoped project-admission memo, or nil.
func (a *AuthzService) evaluateBearerGate(ctx context.Context, principal PrincipalContext, in bearerGateInputs, target Resource, evidence TargetScopeEvidence, action Action, permissionID string, memo *ProjectAdmissionCache, trace *bearerGateTrace) *Decision {
	if trace == nil {
		trace = &bearerGateTrace{}
	}

	// A typed-nil *ScopedUserIdentity carries no boundary, ceiling, or
	// scopes to evaluate. It denies with the project access reason rather
	// than dereferencing a nil receiver or treating a missing credential as
	// an unconstrained one.
	if in.missing {
		trace.Stage = bearerStageProjectAccess
		return &Decision{Allowed: false, Reason: bearerReasonProjectAccessDenied}
	}

	// Stage 1: the boundary must be well formed. An empty or unknown kind,
	// a project boundary without a project ID, or a hub boundary carrying
	// one denies.
	if !in.boundary.Valid() {
		trace.Stage = bearerStageBoundaryInvalid
		return &Decision{Allowed: false, Reason: bearerReasonBoundaryInvalid}
	}

	// Stage 2: the boundary must allow the resolved target scope.
	// Collection-level evidence classifies the request only for the exact
	// permission being evaluated. Evidence that names any other permission
	// does not describe this request, so the target scope is unknown.
	if evidence.IsCollectionLevel && evidence.PermissionID != permissionID {
		trace.Stage = bearerStageTargetUnknown
		return &Decision{Allowed: false, Reason: bearerReasonTargetUnknown}
	}
	scope := ResolveTargetScope(target, evidence)
	trace.TargetScope = scope
	if scope.Kind == TargetScopeUnknown || !scope.Valid() {
		trace.Stage = bearerStageTargetUnknown
		return &Decision{Allowed: false, Reason: unresolvedTargetReason(target)}
	}
	if !BoundaryAllows(in.boundary, scope) {
		trace.Stage = bearerStageOutsideBoundary
		if scope.Kind == TargetScopeHub {
			return &Decision{Allowed: false, Reason: bearerReasonHubLevelResource}
		}
		return &Decision{Allowed: false, Reason: bearerReasonOutsideProject}
	}

	// Stage 3: the ceiling must allow the exact permission Decide resolved
	// for this request. This is the same frozen ceiling the kernel
	// restriction and CanDelegate consult.
	if !in.ceiling.Allows(permissionID) {
		trace.Stage = bearerStageCeiling
		return &Decision{Allowed: false, Reason: "token does not have scope: " + target.Type + ":" + string(action)}
	}

	// Stage 4: a project target requires the principal's current access to
	// that project, checked on every request. Retained creation ancestry
	// or ownership never substitutes for it. ProjectTargetAdmission
	// re-resolves the target and rejects a target that does not resolve to
	// this project (ErrProjectMismatch). Every error denies. A store or
	// resolution fault is tagged DenyCauseResolutionError, so
	// Decision.IsIndeterminate reports true; a policy-fact error (inactive
	// user, project mismatch, unsupported principal kind, class mismatch)
	// is a plain deny.
	if scope.Kind == TargetScopeProject {
		admission, err := a.ProjectTargetAdmission(ctx, principal, scope.ProjectID, permissionID, target, memo)
		if err != nil {
			trace.Stage = bearerStageProjectAccess
			d := &Decision{Allowed: false, Reason: bearerReasonProjectAccessDenied}
			if isProjectAccessLookupFault(err) {
				d.DenyCause = DenyCauseResolutionError
			}
			return d
		}
		if !admission.Admitted {
			trace.Stage = bearerStageProjectAccess
			return &Decision{Allowed: false, Reason: bearerReasonProjectAccessDenied}
		}
		trace.AccessSource = admission.Source
	}

	trace.Stage = bearerStagePassed
	return nil
}

// bearerGateRun carries the request-scoped inputs and the outcome of one
// bearer gate evaluation through decide's body. A nil *bearerGateRun is
// valid: it supplies no memo and records nothing.
type bearerGateRun struct {
	memo  *ProjectAdmissionCache
	trace bearerGateTrace
}

func (r *bearerGateRun) memoOrNil() *ProjectAdmissionCache {
	if r == nil {
		return nil
	}
	return r.memo
}

func (r *bearerGateRun) traceOrNil() *bearerGateTrace {
	if r == nil {
		return nil
	}
	return &r.trace
}

// Bearer evaluation stages reported in BearerEvaluation.Stage. The first
// five name the bearer gate stage that denied. BearerStageAuthority means
// the gate passed and the principal's live authority (role bindings,
// groups, relationship grants, access constraints and the ceiling as a
// kernel restriction) denied. An allowed evaluation has an empty Stage.
//
// BearerStageError covers every deny that is not attributed to a gate
// stage or to live authority:
//   - inputs rejected before evaluation (an unsupported principal, an empty
//     or unknown permission ID);
//   - a policy deny decide applies before the gate, such as the delivery
//     credential gate for a deliver permission;
//   - a store or resolution fault at any stage.
//
// BearerStageError therefore does not by itself mean the evaluation was
// indeterminate or is worth retrying. Decision.IsIndeterminate reports
// whether a fault, rather than a policy fact, denied the request.
const (
	BearerStageBoundaryInvalid = bearerStageBoundaryInvalid
	BearerStageTargetUnknown   = bearerStageTargetUnknown
	BearerStageOutsideBoundary = bearerStageOutsideBoundary
	BearerStageCeiling         = bearerStageCeiling
	BearerStageProjectAccess   = bearerStageProjectAccess
	BearerStageAuthority       = "authority"
	BearerStageError           = "error"
)

// BearerOptions carries the inputs to EvaluateBearerCeiling other than the
// principal, boundary, ceiling, permission and target.
type BearerOptions struct {
	// Evidence is the server-constructed collection-level classification
	// of the target. The zero value classifies the target from the
	// Resource alone. Collection-level evidence must name the evaluated
	// permissionID; evidence naming another permission denies with
	// BearerStageTargetUnknown.
	Evidence TargetScopeEvidence
	// Explain requests decision provenance, as AuthzRequest.Explain does.
	Explain bool
	// Memo is a request-scoped project-admission memo, or nil. It must
	// never be shared across requests. nil uses no memo.
	Memo *ProjectAdmissionCache
}

// BearerEvaluation is the result of EvaluateBearerCeiling.
type BearerEvaluation struct {
	// Decision is the full decision, including kernel and relationship
	// provenance when requested. It is not audited.
	Decision Decision
	// Stage names the stage that denied; empty when allowed. See the
	// BearerStage constants. Use Decision.IsIndeterminate, not Stage, to
	// tell a fault from a policy deny.
	Stage string
	// TargetScope is the resolved target scope, set once the boundary was
	// found valid.
	TargetScope TargetScope
	// AccessSource is the evidence that admitted a project target, set
	// when the project access stage admitted the request.
	AccessSource ProjectAccessSource
}

// Reason EvaluateBearerCeiling reports for inputs it rejects before
// evaluation.
const (
	bearerReasonUnsupportedPrincipal = "bearer evaluation requires a local user principal"
	bearerReasonPermissionRequired   = "bearer evaluation requires a known canonical permission ID"
)

// EvaluateBearerCeiling answers: may user, presenting a bearer credential
// confined to boundary and ceiling, perform permissionID on target at
// evaluation time? It computes
//
//	boundary valid
//	∧ BoundaryAllows(boundary, ResolveTargetScope(target, opts.Evidence))
//	∧ ceiling allows permissionID
//	∧ (project target ⇒ ProjectTargetAdmission(user, project, permissionID, target))
//	∧ user's live authority on target (role bindings, groups, windows,
//	  relationship grants, access constraints, with the ceiling applied as a
//	  kernel restriction)
//
// by running decide's audit-free body on a UAT-kind request built from
// exactly these arguments. A real UAT request and this call therefore pass
// through the same gate and the same kernel code.
//
// Inputs:
//   - user must be a local user principal carrying an interactive-session
//     identity (an *AuthenticatedUser). Any other principal, including a
//     UAT-backed identity, a dev, agent, broker or federated identity, or a
//     nil identity, denies with BearerStageError.
//   - permissionID is the exact canonical permission evaluated. It is never
//     derived from target or action; the action is taken from the
//     permission's registry row. An empty or unknown ID denies with
//     BearerStageError.
//   - target is the actual target of the request, never an invented one.
//
// Account status: for a project target, the project access stage
// (ProjectTargetAdmission) denies a user whose account is not active. For a
// hub target nothing in this evaluation checks account status, so the
// caller must establish that user is an active account before calling, as
// ValidateToken does for a user access token by rejecting a suspended
// user's token.
//
// The evaluation reads only its arguments: it never reads an identity,
// credential, or scopes from ctx. Every store or evaluation error denies.
// It emits no decision audit; the caller decorates and audits its own
// outer decision exactly once.
func (a *AuthzService) EvaluateBearerCeiling(
	ctx context.Context,
	user PrincipalContext,
	boundary TokenBoundary,
	ceiling permissions.FrozenPermissionCeiling,
	permissionID string,
	target Resource,
	opts BearerOptions,
) BearerEvaluation {
	if isNilIdentity(user.Identity) ||
		principalContextForIdentity(user.Identity).Kind != PrincipalKindUser ||
		credentialContextForIdentity(user.Identity).Kind != CredentialKindInteractive {
		return BearerEvaluation{
			Decision: Decision{Allowed: false, Reason: bearerReasonUnsupportedPrincipal},
			Stage:    BearerStageError,
		}
	}
	action, ok := registryActionFor(permissionID)
	if !ok {
		return BearerEvaluation{
			Decision: Decision{Allowed: false, Reason: bearerReasonPermissionRequired},
			Stage:    BearerStageError,
		}
	}

	b := boundary
	run := &bearerGateRun{memo: opts.Memo}
	decision := a.decide(ctx, AuthzRequest{
		Principal: user,
		Credential: CredentialContext{
			Kind:     CredentialKindUAT,
			Boundary: &b,
			Ceiling:  ceiling,
		},
		Resource:       target,
		Action:         action,
		Permission:     permissionID,
		Explain:        opts.Explain,
		TargetEvidence: opts.Evidence,
		bearerRun:      run,
	})

	result := BearerEvaluation{
		Decision:     decision,
		TargetScope:  run.trace.TargetScope,
		AccessSource: run.trace.AccessSource,
	}
	switch {
	case decision.Allowed:
		result.Stage = ""
	case decision.IsIndeterminate(), run.trace.Stage == "":
		result.Stage = BearerStageError
	case run.trace.Stage == bearerStagePassed:
		result.Stage = BearerStageAuthority
	default:
		result.Stage = run.trace.Stage
	}
	return result
}

// registryActionFor returns the action of permissionID's registry row.
// ok is false for an empty or unregistered ID.
func registryActionFor(permissionID string) (Action, bool) {
	if permissionID == "" {
		return "", false
	}
	for _, p := range permissions.Registry {
		if p.ID == permissionID {
			return Action(p.Action), true
		}
	}
	return "", false
}

// unresolvedTargetReason names why a target whose scope cannot be resolved
// is denied. A typed resource with no project association is hub-level; a
// target that names a project without identifying it is outside the token's
// project; anything else is unresolvable.
func unresolvedTargetReason(target Resource) string {
	switch {
	case target.Type == "project" || target.ParentType == "project":
		return bearerReasonOutsideProject
	case target.Type != "":
		return bearerReasonHubLevelResource
	default:
		return bearerReasonTargetUnknown
	}
}
