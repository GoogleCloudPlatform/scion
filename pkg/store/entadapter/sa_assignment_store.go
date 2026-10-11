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

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agentserviceaccountassignment"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/predicate"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// SAAssignmentStore implements store.AgentServiceAccountAssignmentStore using
// Ent ORM.
type SAAssignmentStore struct {
	client *ent.Client
}

// NewSAAssignmentStore creates a new Ent-backed SAAssignmentStore.
func NewSAAssignmentStore(client *ent.Client) *SAAssignmentStore {
	return &SAAssignmentStore{client: client}
}

// entSAAssignmentToStore converts an Ent row to the store model.
func entSAAssignmentToStore(e *ent.AgentServiceAccountAssignment) store.AgentServiceAccountAssignment {
	a := store.AgentServiceAccountAssignment{
		ID:               e.ID.String(),
		AgentID:          e.AgentID,
		ProjectID:        e.ProjectID,
		ServiceAccountID: e.ServiceAccountID,
		Origin:           store.SAAssignmentOrigin(e.Origin),
		Active:           e.Active,
		CreatedAt:        e.Created,
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
		a.SourceScheduleID = *e.SourceScheduleID
	}
	// As for edges: only a bounded ceiling carries permission IDs, and a
	// bounded ceiling with a NULL or malformed list allows nothing.
	if a.Kind == store.EffectCeilingBounded {
		ids := unmarshalCeilingPermissionIDs(e.CeilingPermissionIds)
		if ids == nil {
			ids = []string{}
		}
		a.PermissionIDs = ids
	}
	return a
}

// validateAssignmentDeactivation checks a deactivation record before it is
// written to assignments.
func validateAssignmentDeactivation(cause store.EdgeDeactivationCause, opID string) error {
	if !store.ValidAssignmentDeactivationCause(cause) {
		return fmt.Errorf("%w: unknown assignment deactivation cause %q", store.ErrInvalidInput, cause)
	}
	if opID == "" {
		return fmt.Errorf("%w: assignment deactivation requires an operation ID", store.ErrInvalidInput)
	}
	return nil
}

// deactivateActive deactivates the agent's active rows and records d.
func (s *SAAssignmentStore) deactivateActive(ctx context.Context, agentID string, d store.Deactivation) (int, error) {
	if err := validateAssignmentDeactivation(d.Cause, d.OpID); err != nil {
		return 0, err
	}
	now := time.Now()
	at := now
	if d.At != nil && !d.At.IsZero() {
		at = *d.At
	}
	n, err := s.client.AgentServiceAccountAssignment.Update().
		Where(
			agentserviceaccountassignment.AgentIDEQ(canonicalPrincipalID(agentID)),
			agentserviceaccountassignment.ActiveEQ(true),
		).
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

// ReplaceAgentServiceAccountAssignment deactivates any active row of
// a.AgentID with cause sa_replaced under a fresh operation ID, then inserts a
// as the active row. Run it in a transaction so both writes commit together.
func (s *SAAssignmentStore) ReplaceAgentServiceAccountAssignment(ctx context.Context, a *store.AgentServiceAccountAssignment) error {
	if a == nil || a.AgentID == "" || a.ServiceAccountID == "" {
		return fmt.Errorf("%w: assignment requires an agent and a service account", store.ErrInvalidInput)
	}
	if err := validateEdgeCeiling(a.EffectCeiling); err != nil {
		return err
	}
	a.AgentID = canonicalPrincipalID(a.AgentID)
	a.ProjectID = canonicalPrincipalID(a.ProjectID)
	now := time.Now()
	if _, err := s.deactivateActive(ctx, a.AgentID, store.Deactivation{
		Cause: store.EdgeDeactivationSAReplaced,
		At:    &now,
		OpID:  uuid.New().String(),
	}); err != nil {
		return err
	}

	builder := s.client.AgentServiceAccountAssignment.Create().
		SetAgentID(a.AgentID).
		SetProjectID(a.ProjectID).
		SetServiceAccountID(a.ServiceAccountID).
		SetOrigin(string(a.Origin)).
		SetActive(true).
		SetProvenanceVersion(a.ProvenanceVersion).
		SetSourcePrincipalKind(a.SourcePrincipalKind).
		SetSourcePrincipalID(a.SourcePrincipalID).
		SetSourceCredentialKind(string(a.SourceCredentialKind)).
		SetSourceCredentialID(a.SourceCredentialID).
		SetSourceEventID(a.SourceEventID).
		SetSourceAuthorizationRevision(a.SourceAuthorizationRevision).
		SetInitiatorPrincipalKind(a.InitiatorPrincipalKind).
		SetInitiatorPrincipalID(a.InitiatorPrincipalID).
		SetInitiatorCredentialKind(a.InitiatorCredentialKind).
		SetInitiatorCredentialID(a.InitiatorCredentialID).
		SetCeilingKind(string(a.Kind)).
		SetCeilingVersion(int32(a.Version)).
		SetCeilingBoundaryKind(a.BoundaryKind).
		SetCeilingBoundaryProjectID(a.BoundaryProjectID).
		SetNillableCeilingSourceExpiresAt(a.SourceExpiresAt)
	if a.SourceScheduleID != "" {
		builder.SetSourceScheduleID(a.SourceScheduleID)
	}
	if a.Kind == store.EffectCeilingBounded {
		ids := a.PermissionIDs
		if ids == nil {
			ids = []string{}
		}
		builder.SetNillableCeilingPermissionIds(marshalCeilingPermissionIDs(ids))
	}
	if a.ID != "" {
		uid, err := parseUUID(a.ID)
		if err != nil {
			return err
		}
		builder.SetID(uid)
	}
	created, err := builder.Save(ctx)
	if err != nil {
		return mapError(err)
	}
	a.ID = created.ID.String()
	a.Active = true
	a.CreatedAt = created.Created
	a.Deactivation = store.Deactivation{}
	return nil
}

// GetActiveAgentServiceAccountAssignments returns the agent's active rows,
// oldest first.
func (s *SAAssignmentStore) GetActiveAgentServiceAccountAssignments(ctx context.Context, agentID string) ([]store.AgentServiceAccountAssignment, error) {
	rows, err := s.client.AgentServiceAccountAssignment.Query().
		Where(
			agentserviceaccountassignment.AgentIDEQ(canonicalPrincipalID(agentID)),
			agentserviceaccountassignment.ActiveEQ(true),
		).
		Order(ent.Asc(agentserviceaccountassignment.FieldCreated), ent.Asc(agentserviceaccountassignment.FieldID)).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	out := make([]store.AgentServiceAccountAssignment, len(rows))
	for i, r := range rows {
		out[i] = entSAAssignmentToStore(r)
	}
	return out, nil
}

// DeactivateAgentServiceAccountAssignments deactivates every active row of
// the agent and records d on each.
func (s *SAAssignmentStore) DeactivateAgentServiceAccountAssignments(ctx context.Context, agentID string, d store.Deactivation) (int, error) {
	return s.deactivateActive(ctx, agentID, d)
}

// deactivatedAssignmentsOf selects the agent's inactive rows deactivated with
// cause under opID.
func deactivatedAssignmentsOf(agentID string, cause store.EdgeDeactivationCause, opID string) []predicate.AgentServiceAccountAssignment {
	return []predicate.AgentServiceAccountAssignment{
		agentserviceaccountassignment.AgentIDEQ(canonicalPrincipalID(agentID)),
		agentserviceaccountassignment.ActiveEQ(false),
		agentserviceaccountassignment.DeactivationCauseEQ(string(cause)),
		agentserviceaccountassignment.DeactivationOpIDEQ(opID),
	}
}

// ReactivateAgentServiceAccountAssignments reactivates exactly the agent's
// rows deactivated with cause under opID and clears their deactivation
// record. A conflict with an active row surfaces as store.ErrAlreadyExists
// from the partial unique index.
func (s *SAAssignmentStore) ReactivateAgentServiceAccountAssignments(ctx context.Context, agentID string, cause store.EdgeDeactivationCause, opID string) (int, error) {
	if err := validateAssignmentDeactivation(cause, opID); err != nil {
		return 0, err
	}
	n, err := s.client.AgentServiceAccountAssignment.Update().
		Where(deactivatedAssignmentsOf(agentID, cause, opID)...).
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

var _ store.AgentServiceAccountAssignmentStore = (*SAAssignmentStore)(nil)
