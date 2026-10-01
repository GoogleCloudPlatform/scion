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

package auditevent

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// AccessBoundaryCreateInput contains the approved identity, resource, and
// payload leaves for a committed access-boundary creation.
type AccessBoundaryCreateInput struct {
	Request        *RequestRef
	Initiator      *IdentityRef
	Principal      IdentityRef
	Executor       *IdentityRef
	Credential     *CredentialRef
	CausationID    string
	ConstraintID   string
	Scope          ResourceScope
	ProjectID      string
	BeforeRevision *int64
	AfterRevision  *int64
	Classification BoundaryClassification
	PreviewID      string
	DraftHash      string
	ImpactCounts   *ImpactCounts
	ChangedFields  []string
}

// BuildAccessBoundaryCreate builds the single commit/succeeded event shared by
// boundary history and structured logging. A caller may build it while writing
// transactional history, but must dispatch it to a log sink only after the
// shared boundary transaction commits successfully.
func BuildAccessBoundaryCreate(ctx context.Context, input AccessBoundaryCreateInput) (EnvelopeV1, error) {
	operation, err := requireOperation(ctx)
	if err != nil {
		return EnvelopeV1{}, err
	}
	return buildAccessBoundaryCreate(operation, input, uuid.NewString(), time.Now().UTC())
}

func buildAccessBoundaryCreate(operation AuditOperationContext, input AccessBoundaryCreateInput, eventID string, occurredAt time.Time) (EnvelopeV1, error) {
	event := EnvelopeV1{
		SchemaVersion: SchemaVersion,
		EventID:       eventID,
		OccurredAt:    occurredAt,
		Family:        "access_boundary",
		Action:        "create",
		Phase:         PhaseCommit,
		Outcome:       OutcomeSucceeded,
		Severity:      SeverityInfo,
		CorrelationID: operation.CorrelationID,
		CausationID:   input.CausationID,
		Request:       cloneRequest(input.Request),
		Initiator:     cloneIdentity(input.Initiator),
		Principal:     &input.Principal,
		Executor:      cloneIdentity(input.Executor),
		Credential:    cloneCredential(input.Credential),
		Resource: &ResourceRef{
			Kind:      "access_constraint",
			ID:        input.ConstraintID,
			Scope:     input.Scope,
			ProjectID: input.ProjectID,
		},
		Payload: AccessBoundaryPayload{
			BeforeRevision: cloneInt64(input.BeforeRevision),
			AfterRevision:  cloneInt64(input.AfterRevision),
			Classification: input.Classification,
			PreviewID:      input.PreviewID,
			DraftHash:      input.DraftHash,
			ImpactCounts:   cloneImpactCounts(input.ImpactCounts),
			ChangedFields:  append([]string(nil), input.ChangedFields...),
		},
	}
	if err := Validate(event); err != nil {
		return EnvelopeV1{}, err
	}
	return event, nil
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneImpactCounts(counts *ImpactCounts) *ImpactCounts {
	if counts == nil {
		return nil
	}
	copy := *counts
	return &copy
}

func cloneRequest(request *RequestRef) *RequestRef {
	if request == nil {
		return nil
	}
	copy := *request
	return &copy
}

func cloneIdentity(identity *IdentityRef) *IdentityRef {
	if identity == nil {
		return nil
	}
	copy := *identity
	return &copy
}

func cloneResource(resource *ResourceRef) *ResourceRef {
	if resource == nil {
		return nil
	}
	copy := *resource
	return &copy
}

func cloneCredential(credential *CredentialRef) *CredentialRef {
	if credential == nil {
		return nil
	}
	copy := *credential
	return &copy
}
