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
	"encoding/json"
	"fmt"
)

type serializedEnvelopeV1 struct {
	SchemaVersion int                      `json:"schema_version"`
	EventID       string                   `json:"event_id"`
	OccurredAt    string                   `json:"occurred_at"`
	Family        string                   `json:"family"`
	Action        string                   `json:"action"`
	Phase         Phase                    `json:"phase"`
	Outcome       Outcome                  `json:"outcome,omitempty"`
	Severity      Severity                 `json:"severity"`
	CorrelationID string                   `json:"correlation_id"`
	CausationID   string                   `json:"causation_id,omitempty"`
	Request       *RequestRef              `json:"request,omitempty"`
	Initiator     *IdentityRef             `json:"initiator,omitempty"`
	Principal     *IdentityRef             `json:"principal,omitempty"`
	Executor      *IdentityRef             `json:"executor,omitempty"`
	Credential    *serializedCredentialRef `json:"credential,omitempty"`
	Resource      *ResourceRef             `json:"resource,omitempty"`
	Payload       map[string]any           `json:"payload"`
}

type serializedCredentialRef struct {
	Kind              CredentialKind         `json:"kind"`
	ID                string                 `json:"id,omitempty"`
	Name              string                 `json:"name,omitempty"`
	BoundaryKind      CredentialBoundaryKind `json:"boundary_kind,omitempty"`
	BoundaryProjectID string                 `json:"boundary_project_id,omitempty"`
	Labels            map[string]string      `json:"labels,omitempty"`
}

// Render validates and serializes an envelope with stable field names and no
// undeclared payload leaves.
func Render(event EnvelopeV1) ([]byte, error) {
	var payload map[string]any
	if event.Payload != nil {
		payload = event.Payload.auditPayloadLeaves()
	}
	if err := validateSnapshot(event, payload); err != nil {
		return nil, fmt.Errorf("validate audit event: %w", err)
	}
	encoded, err := json.Marshal(serializedEnvelopeV1{
		SchemaVersion: event.SchemaVersion,
		EventID:       event.EventID,
		OccurredAt:    event.OccurredAt.Format("2006-01-02T15:04:05.999999999Z07:00"),
		Family:        event.Family,
		Action:        event.Action,
		Phase:         event.Phase,
		Outcome:       event.Outcome,
		Severity:      event.Severity,
		CorrelationID: event.CorrelationID,
		CausationID:   event.CausationID,
		Request:       event.Request,
		Initiator:     event.Initiator,
		Principal:     event.Principal,
		Executor:      event.Executor,
		Credential:    serializeCredential(event.Credential),
		Resource:      event.Resource,
		Payload:       payload,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal audit event: %w", err)
	}
	return encoded, nil
}

func serializeCredential(credential *CredentialRef) *serializedCredentialRef {
	if credential == nil {
		return nil
	}
	return &serializedCredentialRef{
		Kind:              credential.Kind(),
		ID:                credential.ID(),
		Name:              credential.Name(),
		BoundaryKind:      credential.BoundaryKind(),
		BoundaryProjectID: credential.BoundaryProjectID(),
		Labels:            credential.Labels(),
	}
}
