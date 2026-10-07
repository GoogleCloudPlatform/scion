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
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agenthold"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/delegationedge"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/predicate"
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

// validateEdgeDeactivation checks a deactivation record before it is
// written to delegation edges.
func validateEdgeDeactivation(cause store.EdgeDeactivationCause, opID string) error {
	if !store.ValidEdgeDeactivationCause(cause) {
		return fmt.Errorf("%w: unknown edge deactivation cause %q", store.ErrInvalidInput, cause)
	}
	if opID == "" {
		return fmt.Errorf("%w: edge deactivation requires an operation ID", store.ErrInvalidInput)
	}
	return nil
}

// deactivateEdges deactivates the active edges matching where and records d.
func (s *DelegationEdgeStore) deactivateEdges(ctx context.Context, d store.Deactivation, where ...predicate.DelegationEdge) (int, error) {
	if err := validateEdgeDeactivation(d.Cause, d.OpID); err != nil {
		return 0, err
	}
	now := time.Now()
	at := now
	if d.At != nil && !d.At.IsZero() {
		at = *d.At
	}
	n, err := s.client.DelegationEdge.Update().
		Where(append(where, delegationedge.ActiveEQ(true))...).
		SetActive(false).
		SetDeactivationCause(string(d.Cause)).
		SetDeactivatedAt(at).
		SetDeactivationOpID(d.OpID).
		SetUpdated(now).
		Save(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	return n, nil
}

// DeactivateDelegationEdgesForDelegate deactivates every active edge of the
// delegate and records d on each.
func (s *DelegationEdgeStore) DeactivateDelegationEdgesForDelegate(ctx context.Context, delegateType, delegateID string, d store.Deactivation) (int, error) {
	return s.deactivateEdges(ctx, d,
		delegationedge.DelegateTypeEQ(delegationedge.DelegateType(delegateType)),
		delegationedge.DelegateIDEQ(delegateID),
	)
}

// DeactivateDelegationEdgesForDelegator deactivates every active edge of the
// delegator and records d on each.
func (s *DelegationEdgeStore) DeactivateDelegationEdgesForDelegator(ctx context.Context, delegatorType, delegatorID string, d store.Deactivation) (int, error) {
	return s.deactivateEdges(ctx, d,
		delegationedge.DelegatorTypeEQ(delegationedge.DelegatorType(delegatorType)),
		delegationedge.DelegatorIDEQ(delegatorID),
	)
}

// deactivatedEdgesOf selects the inactive edges of the delegate deactivated
// with cause under opID.
func deactivatedEdgesOf(delegateType, delegateID string, cause store.EdgeDeactivationCause, opID string) []predicate.DelegationEdge {
	return []predicate.DelegationEdge{
		delegationedge.DelegateTypeEQ(delegationedge.DelegateType(delegateType)),
		delegationedge.DelegateIDEQ(delegateID),
		delegationedge.ActiveEQ(false),
		delegationedge.DeactivationCauseEQ(string(cause)),
		delegationedge.DeactivationOpIDEQ(opID),
	}
}

// GetDeactivatedDelegationEdgesForDelegate returns the inactive edges of the
// delegate deactivated with cause under opID, oldest first.
func (s *DelegationEdgeStore) GetDeactivatedDelegationEdgesForDelegate(ctx context.Context, delegateType, delegateID string, cause store.EdgeDeactivationCause, opID string) ([]*store.DelegationEdge, error) {
	if err := validateEdgeDeactivation(cause, opID); err != nil {
		return nil, err
	}
	edges, err := s.client.DelegationEdge.Query().
		Where(deactivatedEdgesOf(delegateType, delegateID, cause, opID)...).
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

// ReactivateDelegationEdgesForDelegate reactivates exactly the inactive edges
// of the delegate deactivated with cause under opID, and clears their
// deactivation record. A conflict with an active edge in the same scope
// surfaces as store.ErrAlreadyExists from the partial unique index.
func (s *DelegationEdgeStore) ReactivateDelegationEdgesForDelegate(ctx context.Context, delegateType, delegateID string, cause store.EdgeDeactivationCause, opID string) (int, error) {
	if err := validateEdgeDeactivation(cause, opID); err != nil {
		return 0, err
	}
	n, err := s.client.DelegationEdge.Update().
		Where(deactivatedEdgesOf(delegateType, delegateID, cause, opID)...).
		SetActive(true).
		SetDeactivationCause("").
		ClearDeactivatedAt().
		SetDeactivationOpID("").
		SetUpdated(time.Now()).
		Save(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	return n, nil
}

// GetDelegationEdge returns one edge by ID, active or not.
func (s *DelegationEdgeStore) GetDelegationEdge(ctx context.Context, edgeID string) (*store.DelegationEdge, error) {
	uid, err := parseGetID(edgeID)
	if err != nil {
		return nil, err
	}
	e, err := s.client.DelegationEdge.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	return entDelegationEdgeToStore(e), nil
}

// ListAllDelegationEdgesForDelegate returns every edge, active or not, where
// the given principal is the delegate, oldest first.
func (s *DelegationEdgeStore) ListAllDelegationEdgesForDelegate(ctx context.Context, delegateType, delegateID string) ([]*store.DelegationEdge, error) {
	edges, err := s.client.DelegationEdge.Query().
		Where(
			delegationedge.DelegateTypeEQ(delegationedge.DelegateType(delegateType)),
			delegationedge.DelegateIDEQ(delegateID),
		).
		Order(ent.Asc(delegationedge.FieldCreated), ent.Asc(delegationedge.FieldID)).
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

// DeactivateDelegationEdgeGuarded deactivates edgeID with cause and opID when
// it is active and satisfies guard. Every guard predicate, including the
// updated time, is part of the UPDATE's WHERE clause, so a concurrent change
// between the caller's read and the write affects zero rows rather than
// deactivating a changed edge. The existence read only distinguishes
// ErrNotFound from an unmet precondition. A guard with both Unrecorded and
// Recorded set, an empty cause or an empty opID returns ErrInvalidInput.
func (s *DelegationEdgeStore) DeactivateDelegationEdgeGuarded(ctx context.Context, edgeID string, guard store.DelegationEdgeDeactivateGuard, cause store.EdgeDeactivationCause, opID string) (bool, error) {
	if guard.Unrecorded && guard.Recorded {
		return false, fmt.Errorf("%w: guard cannot require both unrecorded and recorded provenance", store.ErrInvalidInput)
	}
	if cause == "" || opID == "" {
		return false, fmt.Errorf("%w: guarded deactivation requires a cause and an operation ID", store.ErrInvalidInput)
	}
	uid, err := parseGetID(edgeID)
	if err != nil {
		return false, err
	}
	if _, err := s.client.DelegationEdge.Get(ctx, uid); err != nil {
		return false, mapError(err)
	}
	preds := []predicate.DelegationEdge{
		delegationedge.IDEQ(uid),
		delegationedge.ActiveEQ(true),
	}
	if guard.UpdatedAt != nil {
		preds = append(preds, delegationedge.UpdatedEQ(*guard.UpdatedAt))
	}
	if guard.Unrecorded {
		preds = append(preds,
			delegationedge.ProvenanceVersionEQ(0),
			delegationedge.CeilingKindEQ(string(store.EffectCeilingUnrecorded)),
		)
	}
	if guard.Recorded {
		preds = append(preds,
			delegationedge.ProvenanceVersionEQ(store.ProvenanceVersionV1),
			delegationedge.CeilingKindIn(string(store.EffectCeilingBounded), string(store.EffectCeilingPrincipal)),
		)
	}
	now := time.Now()
	n, err := s.client.DelegationEdge.Update().
		Where(preds...).
		SetActive(false).
		SetDeactivationCause(string(cause)).
		SetDeactivatedAt(now).
		SetDeactivationOpID(opID).
		SetUpdated(now).
		Save(ctx)
	if err != nil {
		return false, mapError(err)
	}
	return n == 1, nil
}

// ReactivateDelegationEdge reactivates edgeID when it is inactive with cause
// expectCause and no other active edge exists for its delegate and scope.
// The partial unique index on active edges backs the second condition
// against a concurrent insert.
func (s *DelegationEdgeStore) ReactivateDelegationEdge(ctx context.Context, edgeID string, expectCause store.EdgeDeactivationCause) error {
	uid, err := parseGetID(edgeID)
	if err != nil {
		return err
	}
	current, err := s.client.DelegationEdge.Get(ctx, uid)
	if err != nil {
		return mapError(err)
	}
	if current.Active || current.DeactivationCause != string(expectCause) {
		return store.ErrRevisionConflict
	}
	others, err := s.client.DelegationEdge.Query().
		Where(
			delegationedge.DelegateTypeEQ(current.DelegateType),
			delegationedge.DelegateIDEQ(current.DelegateID),
			delegationedge.ScopeTypeEQ(current.ScopeType),
			delegationedge.ScopeIDEQ(current.ScopeID),
			delegationedge.ActiveEQ(true),
		).
		Count(ctx)
	if err != nil {
		return mapError(err)
	}
	if others > 0 {
		return store.ErrRevisionConflict
	}
	n, err := s.client.DelegationEdge.Update().
		Where(
			delegationedge.IDEQ(uid),
			delegationedge.ActiveEQ(false),
			delegationedge.DeactivationCauseEQ(string(expectCause)),
		).
		SetActive(true).
		SetDeactivationCause("").
		ClearDeactivatedAt().
		SetDeactivationOpID("").
		SetUpdated(time.Now()).
		Save(ctx)
	if err != nil {
		if mapped := mapError(err); errors.Is(mapped, store.ErrAlreadyExists) {
			return store.ErrRevisionConflict
		}
		return mapError(err)
	}
	if n != 1 {
		return store.ErrRevisionConflict
	}
	return nil
}

// descendantFrontierBatch bounds the IDs sent in one IN list.
const descendantFrontierBatch = 500

// descendantParent is one principal of the walk's current frontier. The ID
// is always parsed, so every comparison uses the canonical form.
type descendantParent struct {
	typ string
	id  uuid.UUID
}

// descendantHit is one agent first reached at the current level.
type descendantHit struct {
	id  uuid.UUID
	ref store.DescendantRef
}

// ListDelegationDescendants walks level by level from the root principal: at
// each level it selects the delegation edges (then, with LegacyLinks, the
// owner_id / created_by / ancestry links) leaving the current frontier, drops
// agents already visited, and makes the rest the next frontier. Every
// reached agent is expanded; only those whose agent row is in the project
// (and, without IncludeSoftDeleted, not soft-deleted) are returned. See
// store.DelegationEdgeStore for the contract.
func (s *DelegationEdgeStore) ListDelegationDescendants(ctx context.Context, q store.DescendantQuery) (store.DescendantResult, error) {
	var res store.DescendantResult
	if q.RootType != store.DelegationPrincipalUser && q.RootType != store.DelegationPrincipalAgent {
		return res, fmt.Errorf("%w: unknown root principal type %q", store.ErrInvalidInput, q.RootType)
	}
	if q.SkipHeldForRoot && q.RootType != store.DelegationPrincipalUser {
		return res, fmt.Errorf("%w: SkipHeldForRoot requires a user root principal", store.ErrInvalidInput)
	}
	rootID, err := parseUUID(q.RootID)
	if err != nil {
		return res, fmt.Errorf("%w: descendant query requires a root principal UUID", store.ErrInvalidInput)
	}
	projectID, err := parseUUID(q.ProjectID)
	if err != nil {
		return res, err
	}
	if q.MaxDepth < 0 || q.MaxNodes < 0 {
		return res, fmt.Errorf("%w: descendant bounds must not be negative", store.ErrInvalidInput)
	}
	// From here on q carries the canonical forms of its IDs.
	q.RootID = rootID.String()
	q.ProjectID = projectID.String()
	maxDepth := q.MaxDepth
	if maxDepth == 0 {
		maxDepth = store.DefaultDescendantMaxDepth
	}
	maxNodes := q.MaxNodes
	if maxNodes == 0 {
		maxNodes = store.DefaultDescendantMaxNodes
	}

	visited := map[uuid.UUID]bool{}
	if q.RootType == store.DelegationPrincipalAgent {
		visited[rootID] = true
	}
	frontier := []descendantParent{{typ: q.RootType, id: rootID}}
	for depth := 1; len(frontier) > 0; depth++ {
		level, err := s.descendantLevel(ctx, q, projectID, frontier, depth, visited)
		if err != nil {
			return res, err
		}
		if len(level) == 0 {
			break
		}
		if depth > maxDepth {
			return res, store.ErrDescendantLimit
		}
		ids := make([]uuid.UUID, len(level))
		for i, h := range level {
			ids[i] = h.id
		}
		returnable, err := s.descendantReturnable(ctx, q, projectID, ids)
		if err != nil {
			return res, err
		}
		var held map[uuid.UUID]bool
		if q.SkipHeldForRoot {
			if held, err = s.heldForRoot(ctx, q.RootID, returnable); err != nil {
				return res, err
			}
		}
		next := make([]descendantParent, 0, len(level))
		for _, h := range level {
			next = append(next, descendantParent{typ: store.DelegationPrincipalAgent, id: h.id})
			if !returnable[h.id] || held[h.id] {
				continue
			}
			if len(res.Agents) >= maxNodes {
				return res, store.ErrDescendantLimit
			}
			res.Agents = append(res.Agents, h.ref)
		}
		frontier = next
	}
	return res, nil
}

// descendantEdgeFollowed selects the edges the walk follows: active edges,
// and edges deactivated because their delegate was soft- or hard-deleted, so
// the walk continues below deleted agents.
func descendantEdgeFollowed() predicate.DelegationEdge {
	return delegationedge.Or(
		delegationedge.ActiveEQ(true),
		delegationedge.And(
			delegationedge.ActiveEQ(false),
			delegationedge.DeactivationCauseIn(
				string(store.EdgeDeactivationAgentSoftDelete),
				string(store.EdgeDeactivationAgentHardDelete),
			),
		),
	)
}

// descendantLevel returns the agents first reached at depth from frontier,
// in order (edge links, then owner, created_by and ancestry links), and
// marks them visited. q's IDs are canonical.
func (s *DelegationEdgeStore) descendantLevel(ctx context.Context, q store.DescendantQuery, projectID uuid.UUID, frontier []descendantParent, depth int, visited map[uuid.UUID]bool) ([]descendantHit, error) {
	var level []descendantHit
	add := func(id uuid.UUID, ref store.DescendantRef) {
		if visited[id] {
			return
		}
		visited[id] = true
		ref.AgentID = id.String()
		level = append(level, descendantHit{id: id, ref: ref})
	}
	via := func(parentID uuid.UUID) string {
		if depth == 1 {
			return ""
		}
		return parentID.String()
	}
	// Every frontier entry has the same type: the root at depth 1, agents
	// below it.
	parentType := frontier[0].typ
	parentUUIDs := make([]uuid.UUID, len(frontier))
	parentIDs := make([]string, len(frontier))
	for i, p := range frontier {
		parentUUIDs[i] = p.id
		parentIDs[i] = p.id.String()
	}

	// delegator_id and scope_id are text columns matched against the
	// canonical form (lower-case, hyphenated) of the parent and project IDs.
	// Stored IDs are matched in canonical text form only, so writers must
	// store canonical IDs (the create paths store uuid.UUID.String()).
	for start := 0; start < len(parentIDs); start += descendantFrontierBatch {
		chunk := parentIDs[start:min(start+descendantFrontierBatch, len(parentIDs))]
		edges, err := s.client.DelegationEdge.Query().
			Where(
				delegationedge.DelegatorTypeEQ(delegationedge.DelegatorType(parentType)),
				delegationedge.DelegatorIDIn(chunk...),
				delegationedge.DelegateTypeEQ(delegationedge.DelegateTypeAgent),
				delegationedge.ScopeTypeEQ(delegationedge.ScopeTypeProject),
				delegationedge.ScopeIDEQ(q.ProjectID),
				descendantEdgeFollowed(),
			).
			Order(ent.Asc(delegationedge.FieldCreated), ent.Asc(delegationedge.FieldID)).
			All(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		for _, e := range edges {
			// Stored rows: an edge whose delegate or delegator ID is not a
			// UUID cannot name an agent row, so it is skipped.
			delegate, err := uuid.Parse(e.DelegateID)
			if err != nil {
				continue
			}
			delegator, err := uuid.Parse(e.DelegatorID)
			if err != nil {
				continue
			}
			add(delegate, store.DescendantRef{
				ViaID:                 via(delegator),
				Depth:                 depth,
				Link:                  store.DescendantLinkEdge,
				EdgeActive:            e.Active,
				EdgeDeactivationCause: store.EdgeDeactivationCause(e.DeactivationCause),
			})
		}
	}
	if !q.LegacyLinks {
		return level, nil
	}

	// Legacy links expand through soft-deleted agents too; the returned set
	// is filtered by the caller.
	agentScope := []predicate.Agent{agent.ProjectIDEQ(projectID)}
	for start := 0; start < len(parentUUIDs); start += descendantFrontierBatch {
		chunk := parentUUIDs[start:min(start+descendantFrontierBatch, len(parentUUIDs))]
		owned, err := s.descendantAgents(ctx, agentScope, agent.OwnerIDIn(chunk...))
		if err != nil {
			return nil, err
		}
		for _, a := range owned {
			if a.OwnerID == nil {
				continue
			}
			add(a.ID, store.DescendantRef{ViaID: via(*a.OwnerID), Depth: depth, Link: store.DescendantLinkOwner})
		}
	}
	for start := 0; start < len(parentUUIDs); start += descendantFrontierBatch {
		chunk := parentUUIDs[start:min(start+descendantFrontierBatch, len(parentUUIDs))]
		created, err := s.descendantAgents(ctx, agentScope, agent.OwnerIDIsNil(), agent.CreatedByIn(chunk...))
		if err != nil {
			return nil, err
		}
		for _, a := range created {
			if a.CreatedBy == nil {
				continue
			}
			add(a.ID, store.DescendantRef{ViaID: via(*a.CreatedBy), Depth: depth, Link: store.DescendantLinkCreatedBy})
		}
	}
	if depth == 1 {
		// Ancestry is a fallback for agents without an owner: an agent
		// with an owner is reached through it, so an agent whose owner
		// changed is not reached from an earlier root in its ancestry.
		// Ancestry entries are matched against the canonical root ID in
		// text form only, so writers must store canonical IDs.
		seeded, err := s.descendantAgents(ctx, agentScope, agent.OwnerIDIsNil(), ancestryContains(q.RootID))
		if err != nil {
			return nil, err
		}
		for _, a := range seeded {
			add(a.ID, store.DescendantRef{Depth: depth, Link: store.DescendantLinkAncestry})
		}
	}
	return level, nil
}

// descendantAgents returns the agents matching scope and preds with the columns the
// legacy links need, in creation order.
func (s *DelegationEdgeStore) descendantAgents(ctx context.Context, scope []predicate.Agent, preds ...predicate.Agent) ([]*ent.Agent, error) {
	rows, err := s.client.Agent.Query().
		Where(scope...).
		Where(preds...).
		Order(ent.Asc(agent.FieldCreated), ent.Asc(agent.FieldID)).
		Select(agent.FieldID, agent.FieldOwnerID, agent.FieldCreatedBy).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return rows, nil
}

// descendantReturnable returns which of ids the walk may return: those
// whose agent row exists in the project and, unless q.IncludeSoftDeleted, is
// not soft-deleted. IDs without a row (hard-deleted or purged agents) are
// never returned.
func (s *DelegationEdgeStore) descendantReturnable(ctx context.Context, q store.DescendantQuery, projectID uuid.UUID, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	out := make(map[uuid.UUID]bool, len(ids))
	for start := 0; start < len(ids); start += descendantFrontierBatch {
		chunk := ids[start:min(start+descendantFrontierBatch, len(ids))]
		rows, err := s.client.Agent.Query().
			Where(agent.IDIn(chunk...), agent.ProjectIDEQ(projectID)).
			Select(agent.FieldID, agent.FieldDeletedAt).
			All(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		for _, a := range rows {
			if q.IncludeSoftDeleted || a.DeletedAt == nil {
				out[a.ID] = true
			}
		}
	}
	return out, nil
}

// heldForRoot returns which of the candidate agents have an active hold
// whose root principal ID is rootID (canonical). Holds are keyed by
// (agent_id, root_principal_id), so the lookup uses the same key.
func (s *DelegationEdgeStore) heldForRoot(ctx context.Context, rootID string, candidates map[uuid.UUID]bool) (map[uuid.UUID]bool, error) {
	ids := make([]uuid.UUID, 0, len(candidates))
	for id := range candidates {
		ids = append(ids, id)
	}
	held := map[uuid.UUID]bool{}
	for start := 0; start < len(ids); start += descendantFrontierBatch {
		chunk := ids[start:min(start+descendantFrontierBatch, len(ids))]
		holds, err := s.client.AgentHold.Query().
			Where(
				agenthold.AgentIDIn(chunk...),
				agenthold.RootPrincipalIDEQ(rootID),
				agenthold.ClearedAtIsNil(),
			).
			Select(agenthold.FieldAgentID).
			All(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		for _, h := range holds {
			held[h.AgentID] = true
		}
	}
	return held, nil
}
