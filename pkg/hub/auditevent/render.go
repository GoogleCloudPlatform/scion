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

// renderSnapshot owns every reference that validation or JSON encoding reads.
// Payload implementations hand their leaves to this snapshot; callers must not
// mutate a returned map while auditPayloadLeaves is running.
type renderSnapshot struct {
	event      EnvelopeV1
	payload    map[string]any
	hasPayload bool
	serialized serializedEnvelopeV1
}

// Render validates and serializes an envelope with stable field names and no
// undeclared payload leaves.
func Render(event EnvelopeV1) ([]byte, error) {
	return newRenderSnapshot(event).render()
}

func newRenderSnapshot(event EnvelopeV1) renderSnapshot {
	var payload map[string]any
	hasPayload := event.Payload != nil
	if hasPayload {
		payload = clonePayloadLeaves(event.Payload.auditPayloadLeaves())
	}

	snapshotEvent := event
	snapshotEvent.Request = cloneSnapshotRequest(event.Request)
	snapshotEvent.Initiator = cloneIdentity(event.Initiator)
	snapshotEvent.Principal = cloneIdentity(event.Principal)
	snapshotEvent.Executor = cloneIdentity(event.Executor)
	snapshotEvent.Credential = cloneCredential(event.Credential)
	snapshotEvent.Resource = cloneResource(event.Resource)
	snapshotEvent.Payload = nil

	return renderSnapshot{
		event:      snapshotEvent,
		payload:    payload,
		hasPayload: hasPayload,
		serialized: serializedEnvelopeV1{
			SchemaVersion: snapshotEvent.SchemaVersion,
			EventID:       snapshotEvent.EventID,
			OccurredAt:    snapshotEvent.OccurredAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
			Family:        snapshotEvent.Family,
			Action:        snapshotEvent.Action,
			Phase:         snapshotEvent.Phase,
			Outcome:       snapshotEvent.Outcome,
			Severity:      snapshotEvent.Severity,
			CorrelationID: snapshotEvent.CorrelationID,
			CausationID:   snapshotEvent.CausationID,
			Request:       snapshotEvent.Request,
			Initiator:     snapshotEvent.Initiator,
			Principal:     snapshotEvent.Principal,
			Executor:      snapshotEvent.Executor,
			Credential:    serializeCredential(snapshotEvent.Credential),
			Resource:      snapshotEvent.Resource,
			Payload:       payload,
		},
	}
}

func cloneSnapshotRequest(request *RequestRef) *RequestRef {
	cloned := cloneRequest(request)
	if cloned != nil && *cloned == (RequestRef{}) {
		return nil
	}
	return cloned
}

func (snapshot renderSnapshot) render() ([]byte, error) {
	if err := snapshot.validate(); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(snapshot.serialized)
	if err != nil {
		return nil, fmt.Errorf("marshal audit event: %w", err)
	}
	return encoded, nil
}

func (snapshot renderSnapshot) validate() error {
	return validateSnapshot(snapshot.event, snapshot.payload, snapshot.hasPayload)
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

func clonePayloadLeaves(leaves map[string]any) map[string]any {
	if leaves == nil {
		return nil
	}
	clone := make(map[string]any, len(leaves))
	for key, value := range leaves {
		clone[key] = clonePayloadValue(value)
	}
	return clone
}

func clonePayloadValue(value any) any {
	switch value := value.(type) {
	case []string:
		return append([]string(nil), value...)
	case []any:
		clone := make([]any, len(value))
		for i, item := range value {
			clone[i] = clonePayloadValue(item)
		}
		return clone
	case map[string]string:
		clone := make(map[string]string, len(value))
		for key, item := range value {
			clone[key] = item
		}
		return clone
	case map[string]any:
		return clonePayloadLeaves(value)
	case *ImpactCounts:
		return cloneImpactCounts(value)
	case *int64:
		return cloneInt64(value)
	default:
		return value
	}
}
