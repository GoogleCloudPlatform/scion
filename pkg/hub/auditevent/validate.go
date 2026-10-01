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
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

// ValidationError identifies a rejected audit-envelope field and rule without
// retaining the rejected value.
type ValidationError struct {
	Field string
	Rule  string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("invalid audit event field %s: %s", e.Field, e.Rule)
}

func invalid(field, rule string) error {
	return &ValidationError{Field: field, Rule: rule}
}

// ValidatePhaseOutcome enforces the common truthful phase/result matrix.
func ValidatePhaseOutcome(phase Phase, outcome Outcome) error {
	valid := false
	switch phase {
	case PhaseAttempt:
		valid = outcome == ""
	case PhaseDecision:
		valid = outcome == OutcomeAllow || outcome == OutcomeDeny
	case PhaseObservation:
		valid = outcome == OutcomeSucceeded || outcome == OutcomeFailed
	case PhaseCommit:
		valid = outcome == OutcomeSucceeded
	case PhaseFailure:
		valid = outcome == OutcomeFailed
	case PhaseDelivery:
		valid = slices.Contains([]Outcome{OutcomeSucceeded, OutcomeFailed, OutcomeSkipped, OutcomeDeferred}, outcome)
	}
	if !valid {
		return invalid("phase_outcome", "must be a declared phase/outcome pair")
	}
	return nil
}

// Validate checks the common envelope and its literal catalog schema before
// an event reaches any sink.
func Validate(event EnvelopeV1) error {
	return newRenderSnapshot(event).validate()
}

// validateSnapshot validates the envelope against the already-materialized
// payload leaves. Render uses this entry point so the exact validated map is
// also the map serialized across the audit boundary.
func validateSnapshot(event EnvelopeV1, payload map[string]any, hasPayload bool) error {
	if event.SchemaVersion != SchemaVersion {
		return invalid("schema_version", fmt.Sprintf("must be %d", SchemaVersion))
	}
	if err := validateUUID("event_id", event.EventID); err != nil {
		return err
	}
	if event.OccurredAt.IsZero() || event.OccurredAt.Location() != time.UTC {
		return invalid("occurred_at", "must be a non-zero UTC timestamp")
	}
	if err := validateBoundedString("family", event.Family, 64); err != nil {
		return err
	}
	if err := validateBoundedString("action", event.Action, 64); err != nil {
		return err
	}
	if err := ValidatePhaseOutcome(event.Phase, event.Outcome); err != nil {
		return err
	}
	wantSeverity := SeverityInfo
	if event.Outcome == OutcomeDeny || event.Outcome == OutcomeFailed {
		wantSeverity = SeverityWarning
	}
	if event.Severity != wantSeverity {
		return invalid("severity", "must match the outcome")
	}
	if err := validateBoundedString("correlation_id", event.CorrelationID, 128); err != nil {
		return err
	}
	if event.CausationID != "" {
		if err := validateUUID("causation_id", event.CausationID); err != nil {
			return err
		}
	}
	if err := validateRequest(event.Request, event.CorrelationID); err != nil {
		return err
	}
	identities := []struct {
		name     string
		identity *IdentityRef
	}{
		{name: "initiator", identity: event.Initiator},
		{name: "principal", identity: event.Principal},
		{name: "executor", identity: event.Executor},
	}
	for _, item := range identities {
		if err := validateIdentity(item.name, item.identity); err != nil {
			return err
		}
	}
	if err := validateCredential(event.Credential); err != nil {
		return err
	}

	entry, ok := catalogEntry(event.Family, event.Action)
	if !ok {
		return invalid("family_action", "must be declared in the catalog")
	}
	pairAllowed := slices.Contains(entry.AllowedPairs, PhaseOutcome{Phase: event.Phase, Outcome: event.Outcome})
	if !pairAllowed {
		return invalid("phase_outcome", "must be allowed by the catalog entry")
	}
	if slices.Contains(entry.RequiredEnvelopeLeaves, "principal") && event.Principal == nil {
		return invalid("principal", "is required by the catalog")
	}
	if event.Resource == nil {
		return invalid("resource", "is required by the catalog")
	}
	if event.Resource.Kind != entry.ResourceKind {
		return invalid("resource.kind", "must match the catalog entry")
	}
	if err := validateBoundedString("resource.id", event.Resource.ID, 128); err != nil {
		return err
	}
	var scopeSchema *ResourceScopeSchema
	for i := range entry.ResourceScopes {
		if entry.ResourceScopes[i].Scope == event.Resource.Scope {
			scopeSchema = &entry.ResourceScopes[i]
			break
		}
	}
	if scopeSchema == nil {
		return invalid("resource.scope", "must be declared by the catalog")
	}
	switch scopeSchema.ProjectID {
	case ResourceProjectIDOmitted:
		if event.Resource.ProjectID != "" {
			return invalid("resource.project_id", "must be omitted for the resource scope")
		}
	case ResourceProjectIDRequired:
		if event.Resource.ProjectID == "" {
			return invalid("resource.project_id", "is required for the resource scope")
		}
	default:
		return invalid("resource.project_id", "has an undeclared catalog rule")
	}
	if err := validateOptionalBoundedString("resource.project_id", event.Resource.ProjectID, 128); err != nil {
		return err
	}
	if !hasPayload {
		return invalid("payload", "is required")
	}
	return validatePayload(entry, payload)
}

func validateRequest(request *RequestRef, correlationID string) error {
	if request == nil {
		return nil
	}
	if request.ID != "" && request.ID != correlationID {
		return invalid("request.id", "must equal correlation_id")
	}
	if err := validateOptionalBoundedString("request.id", request.ID, 128); err != nil {
		return err
	}
	if request.Method != "" && !slices.Contains([]string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"}, request.Method) {
		return invalid("request.method", "must be a standard HTTP method")
	}
	if err := validateOptionalBoundedString("request.route", request.Route, 256); err != nil {
		return err
	}
	return validateOptionalBoundedString("request.surface", request.Surface, 64)
}

func validateIdentity(name string, identity *IdentityRef) error {
	if identity == nil {
		return nil
	}
	if !slices.Contains([]IdentityKind{IdentityUser, IdentityAgent, IdentityBroker, IdentitySystem, IdentityProject}, identity.Kind) {
		return invalid(name+".kind", "must be a declared identity kind")
	}
	return validateBoundedString(name+".id", identity.ID, 128)
}

func validateCredential(credential *CredentialRef) error {
	if credential == nil {
		return nil
	}
	return credential.Validate()
}

func validatePayload(entry CatalogEntry, payload map[string]any) error {
	allowed := make(map[string]struct{}, len(entry.RequiredPayloadLeaves)+len(entry.OptionalPayloadLeaves))
	for _, schema := range entry.RequiredPayloadLeaves {
		allowed[schema.Name] = struct{}{}
		value, ok := payload[schema.Name]
		if !ok {
			return invalid("payload."+schema.Name, "is required by the catalog")
		}
		if err := validatePayloadLeaf(schema, value); err != nil {
			return err
		}
	}
	for _, schema := range entry.OptionalPayloadLeaves {
		allowed[schema.Name] = struct{}{}
		if value, ok := payload[schema.Name]; ok {
			if err := validatePayloadLeaf(schema, value); err != nil {
				return err
			}
		}
	}
	for leaf := range payload {
		if _, ok := allowed[leaf]; !ok {
			return invalid("payload", "contains an undeclared leaf")
		}
	}
	return nil
}

func validatePayloadLeaf(schema PayloadLeafSchema, value any) error {
	name := "payload." + schema.Name
	switch schema.Type {
	case PayloadString:
		text, ok := value.(string)
		if !ok {
			return invalid(name, "must be a string")
		}
		if err := validateBoundedString(name, text, schema.MaxBytes); err != nil {
			return err
		}
		if len(schema.AllowedValues) > 0 && !slices.Contains(schema.AllowedValues, text) {
			return invalid(name, "must be an allowed value")
		}
	case PayloadInt64:
		if _, ok := value.(int64); !ok {
			return invalid(name, "must be int64")
		}
	case PayloadHexString:
		digest, ok := value.(string)
		if !ok || len(digest) != schema.ExactLength {
			return invalid(name, fmt.Sprintf("must be a %d-character hexadecimal digest", schema.ExactLength))
		}
		if _, err := hex.DecodeString(digest); err != nil {
			return invalid(name, fmt.Sprintf("must be a %d-character hexadecimal digest", schema.ExactLength))
		}
	case PayloadImpactCounts:
		if _, ok := value.(ImpactCounts); !ok {
			return invalid(name, "must be impact counts")
		}
	case PayloadStringArray:
		items, ok := value.([]string)
		if !ok {
			return invalid(name, "must be a string array")
		}
		if len(items) > schema.MaxItems {
			return invalid(name, fmt.Sprintf("must contain at most %d items", schema.MaxItems))
		}
		for i, item := range items {
			if err := validateBoundedString(fmt.Sprintf("%s[%d]", name, i), item, schema.ItemMaxBytes); err != nil {
				return err
			}
		}
	default:
		return invalid(name, "has an undeclared catalog type")
	}
	return nil
}

func validateBoundedString(name, value string, maxBytes int) error {
	if value == "" {
		return invalid(name, "is required")
	}
	return validateOptionalBoundedString(name, value, maxBytes)
}

func validateUUID(name, value string) error {
	parsed, err := uuid.Parse(value)
	if err != nil || parsed == uuid.Nil || parsed.String() != value {
		return invalid(name, "must be a canonical UUID")
	}
	return nil
}

func validateOptionalBoundedString(name, value string, maxBytes int) error {
	if value == "" {
		return nil
	}
	if !utf8.ValidString(value) || len(value) > maxBytes {
		return invalid(name, fmt.Sprintf("must be valid UTF-8 of at most %d bytes", maxBytes))
	}
	if strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return invalid(name, "must not contain control characters")
	}
	return nil
}
