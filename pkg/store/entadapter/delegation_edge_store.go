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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/delegationedge"
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
	return &store.DelegationEdge{
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
	}
}

// CreateDelegationEdge records a new delegation edge.
func (s *DelegationEdgeStore) CreateDelegationEdge(ctx context.Context, edge *store.DelegationEdge) error {
	builder := s.client.DelegationEdge.Create().
		SetDelegatorType(delegationedge.DelegatorType(edge.DelegatorType)).
		SetDelegatorID(edge.DelegatorID).
		SetDelegateType(delegationedge.DelegateType(edge.DelegateType)).
		SetDelegateID(edge.DelegateID).
		SetScopeType(delegationedge.ScopeType(edge.ScopeType)).
		SetScopeID(edge.ScopeID).
		SetRole(edge.Role).
		SetActive(edge.Active).
		SetGrandfathered(edge.Grandfathered)

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
