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

package authzop

import (
	"errors"
	"fmt"
)

// BearerKind classifies how an operation treats a user access token (a
// bearer credential carrying a selector ceiling and a boundary).
type BearerKind string

const (
	// BearerAdmit admits a token that carries the selector of the
	// operation's BasePermission, on one of the declared boundary kinds,
	// for a target supplied by the declared resolver. Live authority,
	// access constraints and field-level checks still apply.
	BearerAdmit BearerKind = "admit"

	// BearerAdmitSelf admits any token on its holder's own records, with
	// no selector. SelfFilter is the boundary filter applied to results.
	BearerAdmitSelf BearerKind = "admit_self"

	// BearerSessionOnly refuses every token. Reason names the policy.
	BearerSessionOnly BearerKind = "session_only"

	// BearerNonUser marks an operation that is not a user surface: agent
	// JWT, broker HMAC, signed URL, public, auth flow, workstation,
	// scheduler or webhook.
	BearerNonUser BearerKind = "non_user"

	// BearerOutOfScope marks an operation whose token admission is owned
	// by another track. Owner names it.
	BearerOutOfScope BearerKind = "out_of_scope"
)

var validBearerKinds = map[BearerKind]bool{
	BearerAdmit:       true,
	BearerAdmitSelf:   true,
	BearerSessionOnly: true,
	BearerNonUser:     true,
	BearerOutOfScope:  true,
}

// AdmitsToken reports whether the kind admits a user access token.
func (k BearerKind) AdmitsToken() bool {
	return k == BearerAdmit || k == BearerAdmitSelf
}

// SessionOnlyReason is the closed set of policy reasons for refusing every
// token on an operation.
type SessionOnlyReason string

const (
	// ReasonCredentialManagement covers token list, create, get, revoke
	// and delete. A session revokes a token; a token does not manage
	// tokens.
	ReasonCredentialManagement SessionOnlyReason = "CREDENTIAL_MANAGEMENT"

	// ReasonSessionRecovery covers session revocation and hub-wide auth
	// reset.
	ReasonSessionRecovery SessionOnlyReason = "SESSION_RECOVERY"

	// ReasonIdentityBinding covers invitation redemption and external
	// account linking. A token does not bind a new identity to its holder.
	ReasonIdentityBinding SessionOnlyReason = "IDENTITY_BINDING"

	// ReasonHostOperations covers maintenance mode, maintenance
	// operations, migrations and restart.
	ReasonHostOperations SessionOnlyReason = "HOST_OPERATIONS"

	// ReasonInteractiveState covers interactive per-user state such as
	// the terminal workspace and the holder's own profile.
	ReasonInteractiveState SessionOnlyReason = "INTERACTIVE_STATE"

	// ReasonIrreversibleCascade covers operations whose effect cascades
	// irreversibly across other users, such as project delete.
	ReasonIrreversibleCascade SessionOnlyReason = "IRREVERSIBLE_CASCADE"

	// ReasonGovernancePending covers operations that create or change
	// durable authority and open to tokens only with their governance
	// batch.
	ReasonGovernancePending SessionOnlyReason = "GOV_PENDING"
)

var validSessionOnlyReasons = map[SessionOnlyReason]bool{
	ReasonCredentialManagement: true,
	ReasonSessionRecovery:      true,
	ReasonIdentityBinding:      true,
	ReasonHostOperations:       true,
	ReasonInteractiveState:     true,
	ReasonIrreversibleCascade:  true,
	ReasonGovernancePending:    true,
}

// BearerBoundary is a token boundary kind an admitted operation accepts.
// Values match the credential boundary kinds.
type BearerBoundary string

const (
	BearerBoundaryProject BearerBoundary = "project"
	BearerBoundaryHub     BearerBoundary = "hub"
)

var validBearerBoundaries = map[BearerBoundary]bool{
	BearerBoundaryProject: true,
	BearerBoundaryHub:     true,
}

// BearerTarget names the resolver that supplies the target an admitted
// token is checked against.
type BearerTarget string

const (
	BearerTargetProjectPath       BearerTarget = "project_path"
	BearerTargetProjectBody       BearerTarget = "project_body"
	BearerTargetAgentRecord       BearerTarget = "agent_record"
	BearerTargetCatalogRecord     BearerTarget = "catalog_record"
	BearerTargetHubInstance       BearerTarget = "hub_instance"
	BearerTargetHubCollection     BearerTarget = "hub_collection"
	BearerTargetProjectCollection BearerTarget = "project_collection"
)

var validBearerTargets = map[BearerTarget]bool{
	BearerTargetProjectPath:       true,
	BearerTargetProjectBody:       true,
	BearerTargetAgentRecord:       true,
	BearerTargetCatalogRecord:     true,
	BearerTargetHubInstance:       true,
	BearerTargetHubCollection:     true,
	BearerTargetProjectCollection: true,
}

// BearerSelfFilter is the result filter an admit_self operation applies.
type BearerSelfFilter string

const (
	// BearerSelfFilterBoundaryProject keeps only rows of a project token's
	// boundary project.
	BearerSelfFilterBoundaryProject BearerSelfFilter = "boundary_project"

	// BearerSelfFilterNone applies no boundary filter.
	BearerSelfFilterNone BearerSelfFilter = "none"
)

var validBearerSelfFilters = map[BearerSelfFilter]bool{
	BearerSelfFilterBoundaryProject: true,
	BearerSelfFilterNone:            true,
}

// BearerOwner names the track that owns token admission for an
// out_of_scope operation.
type BearerOwner string

const (
	BearerOwnerBrokerRegistration BearerOwner = "broker-registration"
	BearerOwnerUserProvisioning   BearerOwner = "user-provisioning"
)

var validBearerOwners = map[BearerOwner]bool{
	BearerOwnerBrokerRegistration: true,
	BearerOwnerUserProvisioning:   true,
}

// BearerDisposition records how an operation treats a user access token.
// The selector is not stored: it is derived from the operation's
// BasePermission through the permission registry.
type BearerDisposition struct {
	Kind BearerKind

	// Boundaries lists the token boundary kinds an admit operation
	// accepts. Required for admit, empty otherwise.
	Boundaries []BearerBoundary

	// Target names the target resolver. Required for admit, empty
	// otherwise.
	Target BearerTarget

	// SelfFilter is required for admit_self, empty otherwise.
	SelfFilter BearerSelfFilter

	// Reason is required for session_only, empty otherwise.
	Reason SessionOnlyReason

	// Owner is required for out_of_scope, empty otherwise.
	Owner BearerOwner

	// Pin names the test that pins this disposition when a generic
	// real-token check cannot exercise the operation. May be empty.
	Pin string
}

// IsZero reports whether no disposition has been recorded.
func (d BearerDisposition) IsZero() bool {
	return d.Kind == "" && len(d.Boundaries) == 0 && d.Target == "" &&
		d.SelfFilter == "" && d.Reason == "" && d.Owner == "" && d.Pin == ""
}

// Validate checks the disposition's structure: a known kind, the fields
// that kind requires, and no fields that belong to another kind.
func (d BearerDisposition) Validate() []error {
	var errs []error
	if d.Kind == "" {
		return []error{errors.New("bearer disposition: kind is required")}
	}
	if !validBearerKinds[d.Kind] {
		return []error{fmt.Errorf("bearer disposition: unknown kind %q", d.Kind)}
	}

	if d.Kind == BearerAdmit {
		if len(d.Boundaries) == 0 {
			errs = append(errs, errors.New("bearer disposition: admit requires at least one boundary"))
		}
		seen := make(map[BearerBoundary]bool)
		for _, b := range d.Boundaries {
			if !validBearerBoundaries[b] {
				errs = append(errs, fmt.Errorf("bearer disposition: unknown boundary %q", b))
			}
			if seen[b] {
				errs = append(errs, fmt.Errorf("bearer disposition: duplicate boundary %q", b))
			}
			seen[b] = true
		}
		if !validBearerTargets[d.Target] {
			errs = append(errs, fmt.Errorf("bearer disposition: admit requires a known target, got %q", d.Target))
		}
	} else {
		if len(d.Boundaries) > 0 {
			errs = append(errs, fmt.Errorf("bearer disposition: boundaries apply only to admit, not %s", d.Kind))
		}
		if d.Target != "" {
			errs = append(errs, fmt.Errorf("bearer disposition: target applies only to admit, not %s", d.Kind))
		}
	}

	if d.Kind == BearerAdmitSelf {
		if !validBearerSelfFilters[d.SelfFilter] {
			errs = append(errs, fmt.Errorf("bearer disposition: admit_self requires a known self filter, got %q", d.SelfFilter))
		}
	} else if d.SelfFilter != "" {
		errs = append(errs, fmt.Errorf("bearer disposition: self filter applies only to admit_self, not %s", d.Kind))
	}

	if d.Kind == BearerSessionOnly {
		if !validSessionOnlyReasons[d.Reason] {
			errs = append(errs, fmt.Errorf("bearer disposition: session_only requires a known reason, got %q", d.Reason))
		}
	} else if d.Reason != "" {
		errs = append(errs, fmt.Errorf("bearer disposition: reason applies only to session_only, not %s", d.Kind))
	}

	if d.Kind == BearerOutOfScope {
		if !validBearerOwners[d.Owner] {
			errs = append(errs, fmt.Errorf("bearer disposition: out_of_scope requires a known owner, got %q", d.Owner))
		}
	} else if d.Owner != "" {
		errs = append(errs, fmt.Errorf("bearer disposition: owner applies only to out_of_scope, not %s", d.Kind))
	}
	return errs
}

// Convenience constructors keep catalog entries short.

// AdmitOn returns an admit disposition for the given target and boundaries.
func AdmitOn(target BearerTarget, boundaries ...BearerBoundary) BearerDisposition {
	return BearerDisposition{Kind: BearerAdmit, Target: target, Boundaries: boundaries}
}

// SessionOnly returns a session_only disposition with the given reason.
func SessionOnly(reason SessionOnlyReason) BearerDisposition {
	return BearerDisposition{Kind: BearerSessionOnly, Reason: reason}
}

// NonUser returns a non_user disposition.
func NonUser() BearerDisposition {
	return BearerDisposition{Kind: BearerNonUser}
}
