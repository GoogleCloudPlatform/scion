// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package hub

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/auditevent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

func withAccessBoundaryAuditOperation(ctx context.Context) context.Context {
	if _, ok := auditevent.OperationFromContext(ctx); ok {
		return ctx
	}
	correlationID := logging.RequestIDFromContext(ctx)
	if correlationID == "" {
		correlationID = correlationIDFromContext(ctx)
	}
	operation := auditevent.StartOperation()
	if correlationID != "" {
		// Validation remains in the builder inside the transaction. Keeping a
		// malformed trusted ID here makes the whole create roll back.
		operation.CorrelationID = correlationID
	}
	return auditevent.ContextWithOperation(ctx, operation)
}

func (gs *GovernanceService) createAccessConstraintWithAudit(
	ctx context.Context,
	req CommitRequest,
	classification string,
) (*CommitResult, auditevent.EnvelopeV1, error) {
	var result *CommitResult
	var event auditevent.EnvelopeV1
	err := gs.store.WithTx(ctx, func(tx store.Store) error {
		created, err := tx.CreateAccessConstraint(ctx, req.Draft)
		if err != nil {
			return err
		}
		candidate, err := buildAccessConstraintCreateEvent(ctx, req, created, classification)
		if err != nil {
			return fmt.Errorf("build audit event: %w", err)
		}
		history, err := accessConstraintHistoryFromEvent(candidate)
		if err != nil {
			return fmt.Errorf("map audit history: %w", err)
		}
		if err := tx.AppendConstraintHistoryTx(ctx, history); err != nil {
			return fmt.Errorf("append audit history: %w", err)
		}
		result = &CommitResult{
			Constraint:     created,
			Operation:      "create",
			Classification: classification,
			AuditID:        candidate.EventID,
		}
		event = candidate
		return nil
	})
	if err != nil {
		return nil, auditevent.EnvelopeV1{}, err
	}
	return result, event, nil
}

func buildAccessConstraintCreateEvent(
	ctx context.Context,
	req CommitRequest,
	created *store.AccessConstraint,
	classification string,
) (auditevent.EnvelopeV1, error) {
	scope, projectID, err := accessConstraintAuditScope(created)
	if err != nil {
		return auditevent.EnvelopeV1{}, err
	}
	principal, err := accessConstraintAuditIdentity(req.Actor.Kind, req.Actor.ID)
	if err != nil {
		return auditevent.EnvelopeV1{}, err
	}
	credential, err := accessConstraintAuditCredential(ctx)
	if err != nil {
		return auditevent.EnvelopeV1{}, err
	}
	var executor *auditevent.IdentityRef
	if current, ok := ExecutorContextFromContext(ctx); ok && !current.IsZero() {
		identity := auditevent.IdentityRef{Kind: auditevent.IdentitySystem, ID: current.ID}
		executor = &identity
	}
	afterRevision := created.Revision
	return auditevent.BuildAccessBoundaryCreate(ctx, auditevent.AccessBoundaryCreateInput{
		Request:        req.AuditRequest,
		Principal:      principal,
		Executor:       executor,
		Credential:     credential,
		ConstraintID:   created.ID,
		Scope:          scope,
		ProjectID:      projectID,
		AfterRevision:  &afterRevision,
		Classification: auditevent.BoundaryClassification(classification),
		PreviewID:      req.PreviewID,
		DraftHash:      req.DraftHash,
	})
}

func accessConstraintAuditScope(constraint *store.AccessConstraint) (auditevent.ResourceScope, string, error) {
	switch constraint.ScopeType {
	case store.RoleScopeSystem:
		if constraint.ScopeID != "" {
			return "", "", &GovernanceError{
				Code:    ErrCodeInvalidRequest,
				Message: "system-scoped constraint must not include a project ID",
			}
		}
		return auditevent.ResourceScopeSystem, "", nil
	case store.RoleScopeProject:
		if constraint.ScopeID == "" {
			return "", "", &GovernanceError{
				Code:    ErrCodeInvalidRequest,
				Message: "project-scoped constraint requires a project ID",
			}
		}
		return auditevent.ResourceScopeProject, constraint.ScopeID, nil
	default:
		return "", "", &GovernanceError{
			Code:    ErrCodeInvalidRequest,
			Message: "constraint scope must be system or project",
		}
	}
}

func accessConstraintAuditIdentity(kind PrincipalKind, id string) (auditevent.IdentityRef, error) {
	var auditKind auditevent.IdentityKind
	switch kind {
	case PrincipalKindUser, PrincipalKindDev, PrincipalKindFederatedUser:
		auditKind = auditevent.IdentityUser
	case PrincipalKindAgent, PrincipalKindFederatedAgent:
		auditKind = auditevent.IdentityAgent
	case PrincipalKindBroker:
		auditKind = auditevent.IdentityBroker
	case PrincipalKindFederatedService:
		auditKind = auditevent.IdentitySystem
	default:
		return auditevent.IdentityRef{}, fmt.Errorf("unsupported principal kind %q", kind)
	}
	return auditevent.IdentityRef{Kind: auditKind, ID: id}, nil
}

func accessConstraintAuditCredential(ctx context.Context) (*auditevent.CredentialRef, error) {
	credential := GetCredentialContextFromContext(ctx)
	if credential.Kind == "" {
		return nil, nil
	}
	input := auditevent.CredentialRefInput{Kind: auditevent.CredentialKind(credential.Kind), ID: credential.ID}
	if decoration, ok := CredentialDecorationFromContext(ctx); ok {
		input.Name = decoration.TokenName
		input.BoundaryKind = auditevent.CredentialBoundaryKind(decoration.Boundary.Kind)
		input.BoundaryProjectID = decoration.Boundary.ProjectID
		input.Labels = decoration.Labels
	}
	ref, err := auditevent.NewCredentialRef(input)
	if err != nil {
		return nil, err
	}
	return &ref, nil
}

func accessConstraintHistoryFromEvent(event auditevent.EnvelopeV1) (*store.AccessConstraintHistory, error) {
	if event.Resource == nil || event.Principal == nil {
		return nil, fmt.Errorf("access-boundary event requires resource and principal")
	}
	payload, ok := event.Payload.(auditevent.AccessBoundaryPayload)
	if !ok {
		return nil, fmt.Errorf("access-boundary event has payload type %T", event.Payload)
	}
	history := &store.AccessConstraintHistory{
		EventID:          event.EventID,
		ConstraintID:     event.Resource.ID,
		OccurredAt:       event.OccurredAt,
		Operation:        event.Action,
		ActorKind:        string(event.Principal.Kind),
		ActorID:          event.Principal.ID,
		CorrelationID:    event.CorrelationID,
		BatchOperationID: event.CausationID,
		BeforeRevision:   payload.BeforeRevision,
		AfterRevision:    payload.AfterRevision,
		Classification:   string(payload.Classification),
		PreviewID:        payload.PreviewID,
		DraftHash:        payload.DraftHash,
	}
	if payload.ImpactCounts != nil {
		encoded, err := json.Marshal(payload.ImpactCounts)
		if err != nil {
			return nil, err
		}
		history.ImpactCountsJSON = string(encoded)
	}
	if payload.ChangedFields != nil {
		encoded, err := json.Marshal(payload.ChangedFields)
		if err != nil {
			return nil, err
		}
		history.ChangedFieldsJSON = string(encoded)
	}
	return history, nil
}
