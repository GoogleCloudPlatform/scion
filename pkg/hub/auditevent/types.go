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

// Package auditevent defines the versioned security-audit event contract.
package auditevent

import (
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/credentialmeta"
)

const (
	// EventName is the stable structured-log event name.
	EventName = "scion.audit"
	// SchemaVersion is the current serialized envelope version.
	SchemaVersion = 1
)

// Phase identifies which part of an operation an event represents.
type Phase string

const (
	PhaseAttempt     Phase = "attempt"
	PhaseDecision    Phase = "decision"
	PhaseObservation Phase = "observation"
	PhaseCommit      Phase = "commit"
	PhaseFailure     Phase = "failure"
	PhaseDelivery    Phase = "delivery"
)

// Outcome is the bounded result associated with a phase. It is empty only
// for an attempt event.
type Outcome string

const (
	OutcomeAllow     Outcome = "allow"
	OutcomeDeny      Outcome = "deny"
	OutcomeSucceeded Outcome = "succeeded"
	OutcomeFailed    Outcome = "failed"
	OutcomeSkipped   Outcome = "skipped"
	OutcomeDeferred  Outcome = "deferred"
)

// Severity is the bounded operator severity of an audit event.
type Severity string

const (
	SeverityInfo    Severity = "info"
	SeverityWarning Severity = "warning"
)

// IdentityKind identifies an actor without adding incidental PII.
type IdentityKind string

const (
	IdentityUser    IdentityKind = "user"
	IdentityAgent   IdentityKind = "agent"
	IdentityBroker  IdentityKind = "broker"
	IdentitySystem  IdentityKind = "system"
	IdentityProject IdentityKind = "project"
)

// IdentityRef identifies one participant in an audited operation.
type IdentityRef struct {
	Kind IdentityKind `json:"kind"`
	ID   string       `json:"id"`
}

// CredentialRef is immutable, validated descriptive credential metadata.
// Construct one with NewCredentialRef; arbitrary public refs cannot enter an
// audit builder.
type CredentialRef = credentialmeta.Ref

type CredentialRefInput = credentialmeta.RefInput
type CredentialKind = credentialmeta.Kind
type CredentialBoundaryKind = credentialmeta.BoundaryKind
type CredentialValidationError = credentialmeta.ValidationError

const (
	CredentialInteractive = credentialmeta.KindInteractive
	CredentialUAT         = credentialmeta.KindUAT
	CredentialAgentJWT    = credentialmeta.KindAgentJWT
	CredentialFederation  = credentialmeta.KindFederation
	CredentialBroker      = credentialmeta.KindBroker
	CredentialDev         = credentialmeta.KindDev

	CredentialBoundaryProject = credentialmeta.BoundaryProject
	CredentialBoundaryHub     = credentialmeta.BoundaryHub
)

func NewCredentialRef(input CredentialRefInput) (CredentialRef, error) {
	return credentialmeta.NewRef(input)
}

// RequestRef records bounded, trusted request metadata.
type RequestRef struct {
	ID      string `json:"id,omitempty"`
	Method  string `json:"method,omitempty"`
	Route   string `json:"route,omitempty"`
	Surface string `json:"surface,omitempty"`
}

// ResourceScope identifies whether an audited resource belongs to the system
// or to one project. It is validation metadata and is not serialized as a
// second resource schema.
type ResourceScope string

const (
	ResourceScopeSystem  ResourceScope = "system"
	ResourceScopeProject ResourceScope = "project"
)

// ResourceRef identifies the object affected by an operation.
type ResourceRef struct {
	Kind      string        `json:"kind"`
	ID        string        `json:"id"`
	Scope     ResourceScope `json:"-"`
	ProjectID string        `json:"project_id,omitempty"`
}

// EnvelopeV1 is the common in-memory audit envelope. Payload construction is
// intentionally package-owned so callers cannot submit arbitrary maps.
type EnvelopeV1 struct {
	SchemaVersion int
	EventID       string
	OccurredAt    time.Time
	Family        string
	Action        string
	Phase         Phase
	Outcome       Outcome
	Severity      Severity
	CorrelationID string
	CausationID   string
	Request       *RequestRef
	Initiator     *IdentityRef
	Principal     *IdentityRef
	Executor      *IdentityRef
	Credential    *CredentialRef
	Resource      *ResourceRef
	Payload       Payload
}

// ImpactCounts is the bounded impact summary for an access-boundary change.
type ImpactCounts struct {
	Agents   uint32 `json:"agents"`
	Users    uint32 `json:"users"`
	Projects uint32 `json:"projects"`
}

// Payload is a sealed typed audit payload. Each family owns its concrete
// payload type; callers cannot provide an arbitrary map at the audit boundary.
type Payload interface {
	auditPayloadLeaves() map[string]any
}

// AccessBoundaryPayload is the exact v1 payload for access-boundary create,
// update, and recovery events.
type AccessBoundaryPayload struct {
	BeforeRevision *int64
	AfterRevision  *int64
	Classification BoundaryClassification
	PreviewID      string
	DraftHash      string
	ImpactCounts   *ImpactCounts
	ChangedFields  []string
}

func (p AccessBoundaryPayload) auditPayloadLeaves() map[string]any {
	leaves := map[string]any{"classification": string(p.Classification)}
	if p.BeforeRevision != nil {
		leaves["before_revision"] = *p.BeforeRevision
	}
	if p.AfterRevision != nil {
		leaves["after_revision"] = *p.AfterRevision
	}
	if p.PreviewID != "" {
		leaves["preview_id"] = p.PreviewID
	}
	if p.DraftHash != "" {
		leaves["draft_hash"] = p.DraftHash
	}
	if p.ImpactCounts != nil {
		leaves["impact_counts"] = *p.ImpactCounts
	}
	if p.ChangedFields != nil {
		leaves["changed_fields"] = append([]string(nil), p.ChangedFields...)
	}
	return leaves
}

// BoundaryClassification describes the authorization direction of a boundary
// mutation.
type BoundaryClassification string

const (
	BoundaryTighten  BoundaryClassification = "tighten"
	BoundaryRelax    BoundaryClassification = "relax"
	BoundaryMixed    BoundaryClassification = "mixed"
	BoundaryNoEffect BoundaryClassification = "no_effect"
)
