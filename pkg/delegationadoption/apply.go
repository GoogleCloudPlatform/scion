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

package delegationadoption

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Actor is the initiator recorded on an adopted edge and its record. The
// zero value (boot migration) records no initiator: no human initiated the
// write, and none is invented.
type Actor struct {
	PrincipalKind  string
	PrincipalID    string
	CredentialKind string
	CredentialID   string
}

// Result is the outcome of applying one record.
type Result struct {
	Status        store.DelegationAdoptionStatus
	Reason        Reason
	AdoptedEdgeID string
	AfterSummary  string
	// Before is the summary of the row the write replaced.
	Before string
}

// AdoptedEdge returns the recorded edge that replaces orig: the same typed
// delegator, delegate, scope, role and grandfathered flag; provenance V1 with
// source = the delegator and credential kind system_migration; a bounded V1
// ceiling of ids bound to the edge's project. Source credential, event and
// schedule fields stay empty.
func AdoptedEdge(orig *store.DelegationEdge, ids []string, actor Actor) *store.DelegationEdge {
	return &store.DelegationEdge{
		DelegatorType: orig.DelegatorType,
		DelegatorID:   orig.DelegatorID,
		DelegateType:  orig.DelegateType,
		DelegateID:    orig.DelegateID,
		ScopeType:     orig.ScopeType,
		ScopeID:       orig.ScopeID,
		Role:          orig.Role,
		Active:        true,
		Grandfathered: orig.Grandfathered,
		AuthorityProvenance: store.AuthorityProvenance{
			ProvenanceVersion:       store.ProvenanceVersionV1,
			SourcePrincipalKind:     orig.DelegatorType,
			SourcePrincipalID:       orig.DelegatorID,
			SourceCredentialKind:    store.SourceCredentialSystemMigration,
			InitiatorPrincipalKind:  actor.PrincipalKind,
			InitiatorPrincipalID:    actor.PrincipalID,
			InitiatorCredentialKind: actor.CredentialKind,
			InitiatorCredentialID:   actor.CredentialID,
		},
		EffectCeiling: store.EffectCeiling{
			Kind:              store.EffectCeilingBounded,
			Version:           permissions.CeilingVersionV1,
			PermissionIDs:     append([]string{}, ids...),
			BoundaryKind:      string(permissions.BoundaryKindProject),
			BoundaryProjectID: orig.ScopeID,
		},
	}
}

// EdgeSummary is the evidence summary of an edge: version, kind, delegator,
// role, scope, and the ceiling's ID count and hash. It carries no
// credential values.
func EdgeSummary(e *store.DelegationEdge) string {
	if e == nil {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.Join(e.PermissionIDs, "\n")))
	b, _ := json.Marshal(struct {
		EdgeID            string `json:"edge_id"`
		ProvenanceVersion int    `json:"provenance_version"`
		CeilingKind       string `json:"ceiling_kind"`
		CeilingIDCount    int    `json:"ceiling_id_count"`
		CeilingIDsSHA256  string `json:"ceiling_ids_sha256,omitempty"`
		SourceCredential  string `json:"source_credential_kind,omitempty"`
		Delegator         string `json:"delegator"`
		Role              string `json:"role"`
		Scope             string `json:"scope"`
		Active            bool   `json:"active"`
		PolicyVersion     int    `json:"policy_version,omitempty"`
	}{
		EdgeID:            e.ID,
		ProvenanceVersion: e.ProvenanceVersion,
		CeilingKind:       string(e.Kind),
		CeilingIDCount:    len(e.PermissionIDs),
		CeilingIDsSHA256: func() string {
			if e.Kind != store.EffectCeilingBounded {
				return ""
			}
			return hex.EncodeToString(sum[:])
		}(),
		SourceCredential: string(e.SourceCredentialKind),
		Delegator:        e.DelegatorType + ":" + e.DelegatorID,
		Role:             e.Role,
		Scope:            e.ScopeType + ":" + e.ScopeID,
		Active:           e.Active,
		PolicyVersion: func() int {
			if e.SourceCredentialKind == store.SourceCredentialSystemMigration {
				return int(PolicyVersion)
			}
			return 0
		}(),
	})
	return string(b)
}

func skipped(reason Reason) Result {
	return Result{Status: store.DelegationAdoptionSkippedChanged, Reason: reason}
}

// ApplyAdopt adopts the hop rec names, inside tx, against current state:
//  1. re-plan the delegate's closure in tx; the hop must be adoptable,
//     name rec.OriginalEdgeID and, when rec.BeforeFingerprint is set, match
//     it;
//  2. an agent delegator's own hop must already be recorded (top-down);
//  3. deactivate the original row only if it is active, unrecorded
//     and unchanged (cause provenance_adopted, op ID rec.ID);
//  4. insert the adopted row (the partial unique index on active edges
//     rejects a second active row).
//
// A changed state returns a skipped_changed Result with nothing written. A
// hop whose active edge is an already-adopted edge returns a recognized
// Result. A
// write error is returned; the caller rolls the transaction back. ApplyAdopt
// does not write rec; the caller does, in the same transaction.
func ApplyAdopt(ctx context.Context, tx store.Store, rec *store.DelegationAdoption, actor Actor) (Result, error) {
	plan, err := Build(ctx, tx, Scope{AgentIDs: []string{rec.DelegateID}})
	if err != nil {
		return Result{}, err
	}
	hop := plan.Hop(rec.DelegateID)
	if hop == nil {
		return skipped(ReasonDelegateNotLive), nil
	}
	switch hop.Outcome {
	case OutcomeAdopt:
	case OutcomeRecognized, OutcomeRecognizedAbovePolicy:
		if hop.OriginalEdgeID != "" && hop.OriginalEdgeID == rec.OriginalEdgeID {
			return Result{Status: store.DelegationAdoptionStatus(hop.Outcome), Reason: ReasonConcurrentAdoption, AdoptedEdgeID: hop.Edge.ID}, nil
		}
		return skipped(ReasonConcurrentAdoption), nil
	case OutcomeExcluded:
		return skipped(hop.Reason), nil
	default:
		return skipped(ReasonNotAdoptable), nil
	}
	if hop.Edge.ID != rec.OriginalEdgeID {
		return skipped(ReasonEdgeChanged), nil
	}
	if rec.BeforeFingerprint != "" && hop.Fingerprint != rec.BeforeFingerprint {
		return skipped(ReasonFingerprintChanged), nil
	}
	if hop.Edge.DelegatorType == store.DelegationPrincipalAgent {
		parent := plan.Hop(hop.Edge.DelegatorID)
		if parent == nil || parent.Unrecorded() {
			return skipped(ReasonAncestorNotAdopted), nil
		}
	}
	return writeAdoption(ctx, tx, hop, rec.ID, actor)
}

// ApplyPlannedAdopt writes hop, which the caller planned inside tx, without
// re-planning. Admin commits use it after checking the plan fingerprint in
// the same transaction.
func ApplyPlannedAdopt(ctx context.Context, tx store.Store, hop *Hop, opID string, actor Actor) (Result, error) {
	if hop == nil || hop.Outcome != OutcomeAdopt || hop.Edge == nil {
		return skipped(ReasonNotAdoptable), nil
	}
	return writeAdoption(ctx, tx, hop, opID, actor)
}

func writeAdoption(ctx context.Context, tx store.Store, hop *Hop, opID string, actor Actor) (Result, error) {
	updated := hop.Edge.UpdatedAt
	ok, err := tx.DeactivateDelegationEdgeGuarded(ctx, hop.Edge.ID,
		store.DelegationEdgeDeactivateGuard{Unrecorded: true, UpdatedAt: &updated},
		store.EdgeDeactivationProvenanceAdopted, opID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return skipped(ReasonEdgeChanged), nil
		}
		return Result{}, fmt.Errorf("deactivate edge %s: %w", hop.Edge.ID, err)
	}
	if !ok {
		return skipped(ReasonEdgeChanged), nil
	}
	adopted := AdoptedEdge(hop.Edge, hop.CeilingIDs, actor)
	if err := tx.CreateDelegationEdge(ctx, adopted); err != nil {
		return Result{}, fmt.Errorf("insert adopted edge for agent %s: %w", hop.DelegateID, err)
	}
	return Result{
		Status:        store.DelegationAdoptionAdopted,
		AdoptedEdgeID: adopted.ID,
		AfterSummary:  EdgeSummary(adopted),
		Before:        EdgeSummary(hop.Edge),
	}, nil
}
