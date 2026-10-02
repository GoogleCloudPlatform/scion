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

package entadapter

import (
	"context"
	"fmt"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/delegationedge"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// DelegationEdgeStore implements store.DelegationEdgeStore using Ent ORM.
type DelegationEdgeStore struct {
	client *ent.Client
}

// NewDelegationEdgeStore creates a new Ent-backed DelegationEdgeStore.
func NewDelegationEdgeStore(client *ent.Client) *DelegationEdgeStore {
	return &DelegationEdgeStore{client: client}
}

// entDelegationEdgeToStore converts an Ent DelegationEdge entity to a store model.
func entDelegationEdgeToStore(e *ent.DelegationEdge) *store.DelegationEdge {
	edge := &store.DelegationEdge{
		ID:            e.ID.String(),
		DelegatorType: string(e.DelegatorType),
		DelegatorID:   e.DelegatorID,
		DelegateType:  string(e.DelegateType),
		DelegateID:    e.DelegateID,
		ScopeType:     string(e.ScopeType),
		ScopeID:       e.ScopeID,
		Role:          e.Role,
		Active:        e.Active,
		Grandfathered: e.Grandfathered,
		CreatedAt:     e.Created,
		UpdatedAt:     e.Updated,
		AuthorityProvenance: store.AuthorityProvenance{
			ProvenanceVersion:           e.ProvenanceVersion,
			SourcePrincipalKind:         e.SourcePrincipalKind,
			SourcePrincipalID:           e.SourcePrincipalID,
			SourceCredentialKind:        store.SourceCredentialKind(e.SourceCredentialKind),
			SourceCredentialID:          e.SourceCredentialID,
			SourceEventID:               e.SourceEventID,
			SourceAuthorizationRevision: e.SourceAuthorizationRevision,
			InitiatorPrincipalKind:      e.InitiatorPrincipalKind,
			InitiatorPrincipalID:        e.InitiatorPrincipalID,
			InitiatorCredentialKind:     e.InitiatorCredentialKind,
			InitiatorCredentialID:       e.InitiatorCredentialID,
		},
		EffectCeiling: store.EffectCeiling{
			Kind:              store.EffectCeilingKind(e.CeilingKind),
			Version:           permissions.CeilingVersion(e.CeilingVersion),
			BoundaryKind:      e.CeilingBoundaryKind,
			BoundaryProjectID: e.CeilingBoundaryProjectID,
			SourceExpiresAt:   e.CeilingSourceExpiresAt,
		},
		Deactivation: store.Deactivation{
			Cause: store.EdgeDeactivationCause(e.DeactivationCause),
			At:    e.DeactivatedAt,
			OpID:  e.DeactivationOpID,
		},
	}
	if e.SourceScheduleID != nil {
		edge.SourceScheduleID = *e.SourceScheduleID
	}
	// Only a bounded ceiling carries permission IDs. A stored list on any
	// other kind is ignored so that it cannot be read as an allow-list. A
	// bounded ceiling with a NULL or malformed list reads back as an empty
	// list, which allows nothing.
	if edge.Kind == store.EffectCeilingBounded {
		ids := unmarshalCeilingPermissionIDs(e.CeilingPermissionIds)
		if ids == nil {
			ids = []string{}
		}
		edge.PermissionIDs = ids
	}
	return edge
}

// validateEdgeCeiling rejects an effect ceiling the edge store cannot
// persist faithfully: an unknown kind, or permission IDs on a kind other
// than bounded.
func validateEdgeCeiling(c store.EffectCeiling) error {
	switch c.Kind {
	case store.EffectCeilingBounded:
		return nil
	case store.EffectCeilingPrincipal, store.EffectCeilingUnrecorded:
		if c.PermissionIDs != nil {
			return fmt.Errorf("%w: effect ceiling kind %q cannot carry permission IDs", store.ErrInvalidInput, c.Kind)
		}
		return nil
	default:
		return fmt.Errorf("%w: unknown effect ceiling kind %q", store.ErrInvalidInput, c.Kind)
	}
}

// CreateDelegationEdge records a new delegation edge.
func (s *DelegationEdgeStore) CreateDelegationEdge(ctx context.Context, edge *store.DelegationEdge) error {
	if err := validateEdgeCeiling(edge.EffectCeiling); err != nil {
		return err
	}
	builder := s.client.DelegationEdge.Create().
		SetDelegatorType(delegationedge.DelegatorType(edge.DelegatorType)).
		SetDelegatorID(edge.DelegatorID).
		SetDelegateType(delegationedge.DelegateType(edge.DelegateType)).
		SetDelegateID(edge.DelegateID).
		SetScopeType(delegationedge.ScopeType(edge.ScopeType)).
		SetScopeID(edge.ScopeID).
		SetRole(edge.Role).
		SetActive(edge.Active).
		SetGrandfathered(edge.Grandfathered).
		SetProvenanceVersion(edge.ProvenanceVersion).
		SetSourcePrincipalKind(edge.SourcePrincipalKind).
		SetSourcePrincipalID(edge.SourcePrincipalID).
		SetSourceCredentialKind(string(edge.SourceCredentialKind)).
		SetSourceCredentialID(edge.SourceCredentialID).
		SetSourceEventID(edge.SourceEventID).
		SetSourceAuthorizationRevision(edge.SourceAuthorizationRevision).
		SetInitiatorPrincipalKind(edge.InitiatorPrincipalKind).
		SetInitiatorPrincipalID(edge.InitiatorPrincipalID).
		SetInitiatorCredentialKind(edge.InitiatorCredentialKind).
		SetInitiatorCredentialID(edge.InitiatorCredentialID).
		SetCeilingKind(string(edge.Kind)).
		SetCeilingVersion(int32(edge.Version)).
		SetCeilingBoundaryKind(edge.BoundaryKind).
		SetCeilingBoundaryProjectID(edge.BoundaryProjectID).
		SetNillableCeilingSourceExpiresAt(edge.SourceExpiresAt).
		SetDeactivationCause(string(edge.Cause)).
		SetNillableDeactivatedAt(edge.At).
		SetDeactivationOpID(edge.OpID)

	if edge.SourceScheduleID != "" {
		builder.SetSourceScheduleID(edge.SourceScheduleID)
	}
	if edge.Kind == store.EffectCeilingBounded {
		ids := edge.PermissionIDs
		if ids == nil {
			ids = []string{}
		}
		builder.SetNillableCeilingPermissionIds(marshalCeilingPermissionIDs(ids))
	}

	if edge.ID != "" {
		uid, err := parseUUID(edge.ID)
		if err != nil {
			return err
		}
		builder.SetID(uid)
	}

	created, err := builder.Save(ctx)
	if err != nil {
		return mapError(err)
	}
	edge.ID = created.ID.String()
	edge.CreatedAt = created.Created
	edge.UpdatedAt = created.Updated
	return nil
}

// GetDelegationEdgesForDelegate returns active delegation edges where
// the given principal is the delegate (receiving authority).
// Results are ordered by creation time (oldest first) for deterministic
// evaluation — authorization must not depend on database row ordering.
func (s *DelegationEdgeStore) GetDelegationEdgesForDelegate(ctx context.Context, delegateType, delegateID string) ([]*store.DelegationEdge, error) {
	edges, err := s.client.DelegationEdge.Query().
		Where(
			delegationedge.DelegateTypeEQ(delegationedge.DelegateType(delegateType)),
			delegationedge.DelegateIDEQ(delegateID),
			delegationedge.ActiveEQ(true),
		).
		Order(ent.Asc(delegationedge.FieldCreated)).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	result := make([]*store.DelegationEdge, len(edges))
	for i, e := range edges {
		result[i] = entDelegationEdgeToStore(e)
	}
	return result, nil
}

// GetDelegationEdgesForDelegator returns active delegation edges where
// the given principal is the delegator (granting authority).
func (s *DelegationEdgeStore) GetDelegationEdgesForDelegator(ctx context.Context, delegatorType, delegatorID string) ([]*store.DelegationEdge, error) {
	edges, err := s.client.DelegationEdge.Query().
		Where(
			delegationedge.DelegatorTypeEQ(delegationedge.DelegatorType(delegatorType)),
			delegationedge.DelegatorIDEQ(delegatorID),
			delegationedge.ActiveEQ(true),
		).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	result := make([]*store.DelegationEdge, len(edges))
	for i, e := range edges {
		result[i] = entDelegationEdgeToStore(e)
	}
	return result, nil
}

// DeactivateDelegationEdge marks an edge as inactive.
func (s *DelegationEdgeStore) DeactivateDelegationEdge(ctx context.Context, edgeID string) error {
	uid, err := parseGetID(edgeID)
	if err != nil {
		return err
	}
	now := time.Now()
	_, err = s.client.DelegationEdge.UpdateOneID(uid).
		SetActive(false).
		SetUpdated(now).
		Save(ctx)
	if err != nil {
		return mapError(err)
	}
	return nil
}
