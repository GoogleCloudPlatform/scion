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
	// set only when the project-access stage admitted the request.
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
//     unresolvable target denies for every boundary kind.
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
// trace receives the stage outcome; it may be nil. memo is the optional
// request-scoped project-admission memo.
func (a *AuthzService) evaluateBearerGate(ctx context.Context, principal PrincipalContext, in bearerGateInputs, target Resource, evidence TargetScopeEvidence, action Action, permissionID string, memo *ProjectAdmissionCache, trace *bearerGateTrace) *Decision {
	if trace == nil {
		trace = &bearerGateTrace{}
	}

	// A typed-nil *ScopedUserIdentity carries no boundary, ceiling, or
	// scopes to evaluate. It denies with the project-access reason rather
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
	scope := ResolveTargetScope(target, evidence)
	trace.TargetScope = scope
	if scope.Kind == TargetScopeUnknown || !scope.Valid() {
		trace.Stage = bearerStageTargetUnknown
		return &Decision{Allowed: false, Reason: bearerReasonTargetUnknown}
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
