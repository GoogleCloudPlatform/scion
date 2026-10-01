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
		return fmt.Errorf("invalid audit phase/outcome pair %q/%q", phase, outcome)
	}
	return nil
}

// Validate checks the common envelope and its literal catalog schema before
// an event reaches any sink.
func Validate(event EnvelopeV1) error {
	var payload map[string]any
	if event.Payload != nil {
		payload = event.Payload.auditPayloadLeaves()
	}
	return validateSnapshot(event, payload)
}

// validateSnapshot validates the envelope against the already-materialized
// payload leaves. Render uses this entry point so the exact validated map is
// also the map serialized across the audit boundary.
func validateSnapshot(event EnvelopeV1, payload map[string]any) error {
	if event.SchemaVersion != SchemaVersion {
		return fmt.Errorf("schema_version must be %d", SchemaVersion)
	}
	if err := validateUUID("event_id", event.EventID); err != nil {
		return err
	}
	if event.OccurredAt.IsZero() || event.OccurredAt.Location() != time.UTC {
		return fmt.Errorf("occurred_at must be a non-zero UTC timestamp")
	}
	for name, value := range map[string]string{"family": event.Family, "action": event.Action} {
		if err := validateBoundedString(name, value, 64); err != nil {
			return err
		}
	}
	if err := ValidatePhaseOutcome(event.Phase, event.Outcome); err != nil {
		return err
	}
	wantSeverity := SeverityInfo
	if event.Outcome == OutcomeDeny || event.Outcome == OutcomeFailed {
		wantSeverity = SeverityWarning
	}
	if event.Severity != wantSeverity {
		return fmt.Errorf("severity must be %q for outcome %q", wantSeverity, event.Outcome)
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
	for name, identity := range map[string]*IdentityRef{
		"initiator": event.Initiator,
		"principal": event.Principal,
		"executor":  event.Executor,
	} {
		if err := validateIdentity(name, identity); err != nil {
			return err
		}
	}
	if err := validateCredential(event.Credential); err != nil {
		return err
	}

	entry, ok := catalogEntry(event.Family, event.Action)
	if !ok {
		return fmt.Errorf("audit action %q/%q is not declared in the catalog", event.Family, event.Action)
	}
	pairAllowed := slices.Contains(entry.AllowedPairs, PhaseOutcome{Phase: event.Phase, Outcome: event.Outcome})
	if !pairAllowed {
		return fmt.Errorf("phase/outcome %q/%q is not allowed by the catalog for %s/%s", event.Phase, event.Outcome, event.Family, event.Action)
	}
	if slices.Contains(entry.RequiredEnvelopeLeaves, "principal") && event.Principal == nil {
		return fmt.Errorf("principal is required by the catalog")
	}
	if event.Resource == nil {
		return fmt.Errorf("resource is required by the catalog")
	}
	if event.Resource.Kind != entry.ResourceKind {
		return fmt.Errorf("resource kind must be %q for %s/%s", entry.ResourceKind, event.Family, event.Action)
	}
	if err := validateBoundedString("resource.id", event.Resource.ID, 128); err != nil {
		return err
	}
	if slices.Contains(entry.RequiredEnvelopeLeaves, "resource.project_id") && event.Resource.ProjectID == "" {
		return fmt.Errorf("resource.project_id is required by the catalog")
	}
	if err := validateOptionalBoundedString("resource.project_id", event.Resource.ProjectID, 128); err != nil {
		return err
	}
	if event.Payload == nil {
		return fmt.Errorf("payload is required")
	}
	return validatePayload(entry, payload)
}

func validateRequest(request *RequestRef, correlationID string) error {
	if request == nil {
		return nil
	}
	if request.ID != "" && request.ID != correlationID {
		return fmt.Errorf("request.id must equal correlation_id")
	}
	if err := validateOptionalBoundedString("request.id", request.ID, 128); err != nil {
		return err
	}
	if request.Method != "" && !slices.Contains([]string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"}, request.Method) {
		return fmt.Errorf("request.method %q is not a standard HTTP method", request.Method)
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
		return fmt.Errorf("%s.kind %q is invalid", name, identity.Kind)
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
			return fmt.Errorf("required payload leaf %q is missing", schema.Name)
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
			return fmt.Errorf("undeclared payload leaf %q", leaf)
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
			return fmt.Errorf("%s must be a string", name)
		}
		if err := validateBoundedString(name, text, schema.MaxBytes); err != nil {
			return err
		}
		if len(schema.AllowedValues) > 0 && !slices.Contains(schema.AllowedValues, text) {
			return fmt.Errorf("%s is not an allowed value", name)
		}
	case PayloadInt64:
		if _, ok := value.(int64); !ok {
			return fmt.Errorf("%s must be int64", name)
		}
	case PayloadHexString:
		digest, ok := value.(string)
		if !ok || len(digest) != schema.ExactLength {
			return fmt.Errorf("%s must be a %d-character hexadecimal digest", name, schema.ExactLength)
		}
		if _, err := hex.DecodeString(digest); err != nil {
			return fmt.Errorf("%s must be a %d-character hexadecimal digest", name, schema.ExactLength)
		}
	case PayloadImpactCounts:
		if _, ok := value.(ImpactCounts); !ok {
			return fmt.Errorf("%s has an invalid type", name)
		}
	case PayloadStringArray:
		items, ok := value.([]string)
		if !ok {
			return fmt.Errorf("%s must be a string array", name)
		}
		if len(items) > schema.MaxItems {
			return fmt.Errorf("%s must contain at most %d items", name, schema.MaxItems)
		}
		for i, item := range items {
			if err := validateBoundedString(fmt.Sprintf("%s[%d]", name, i), item, schema.ItemMaxBytes); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("%s has undeclared catalog type %q", name, schema.Type)
	}
	return nil
}

func validateBoundedString(name, value string, maxBytes int) error {
	if value == "" {
		return fmt.Errorf("%s is required", name)
	}
	return validateOptionalBoundedString(name, value, maxBytes)
}

func validateUUID(name, value string) error {
	parsed, err := uuid.Parse(value)
	if err != nil || parsed == uuid.Nil || parsed.String() != value {
		return fmt.Errorf("%s must be a canonical UUID", name)
	}
	return nil
}

func validateOptionalBoundedString(name, value string, maxBytes int) error {
	if value == "" {
		return nil
	}
	if !utf8.ValidString(value) || len(value) > maxBytes {
		return fmt.Errorf("%s must be valid UTF-8 of at most %d bytes", name, maxBytes)
	}
	if strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return fmt.Errorf("%s must not contain control characters", name)
	}
	return nil
}
