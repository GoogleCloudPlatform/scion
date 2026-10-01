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
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

var labelKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)

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
	if event.SchemaVersion != SchemaVersion {
		return fmt.Errorf("schema_version must be %d", SchemaVersion)
	}
	if _, err := uuid.Parse(event.EventID); err != nil {
		return fmt.Errorf("event_id must be a UUID: %w", err)
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
		if _, err := uuid.Parse(event.CausationID); err != nil {
			return fmt.Errorf("causation_id must be a UUID: %w", err)
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
	return validatePayload(entry, event.Payload.auditPayloadLeaves())
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
	if err := validateBoundedString("credential.kind", credential.Kind, 64); err != nil {
		return err
	}
	fields := []struct {
		name  string
		value string
		limit int
	}{
		{"credential.id", credential.ID, 128},
		{"credential.name", credential.Name, 128},
		{"credential.boundary_kind", credential.BoundaryKind, 64},
		{"credential.boundary_project_id", credential.BoundaryProjectID, 128},
	}
	for _, field := range fields {
		if err := validateOptionalBoundedString(field.name, field.value, field.limit); err != nil {
			return err
		}
	}
	if len(credential.Labels) > 16 {
		return fmt.Errorf("credential.labels must contain at most 16 entries")
	}
	for key, value := range credential.Labels {
		if !labelKeyPattern.MatchString(key) {
			return fmt.Errorf("credential label key %q is invalid", key)
		}
		if err := validateOptionalBoundedString("credential.labels."+key, value, 128); err != nil {
			return err
		}
	}
	return nil
}

func validatePayload(entry CatalogEntry, payload map[string]any) error {
	allowed := make(map[string]struct{}, len(entry.RequiredPayloadLeaves)+len(entry.OptionalPayloadLeaves))
	for _, leaf := range entry.RequiredPayloadLeaves {
		allowed[leaf] = struct{}{}
		if _, ok := payload[leaf]; !ok {
			return fmt.Errorf("required payload leaf %q is missing", leaf)
		}
	}
	for _, leaf := range entry.OptionalPayloadLeaves {
		allowed[leaf] = struct{}{}
	}
	for leaf := range payload {
		if _, ok := allowed[leaf]; !ok {
			return fmt.Errorf("undeclared payload leaf %q", leaf)
		}
	}

	classification, ok := payload["classification"].(string)
	if !ok || !slices.Contains([]string{"tighten", "relax", "mixed", "no_effect"}, classification) {
		return fmt.Errorf("payload.classification is invalid")
	}
	for _, leaf := range []string{"before_revision", "after_revision"} {
		if value, ok := payload[leaf]; ok {
			if _, ok := value.(int64); !ok {
				return fmt.Errorf("payload.%s must be int64", leaf)
			}
		}
	}
	for _, leaf := range []string{"preview_id"} {
		if value, ok := payload[leaf]; ok {
			text, ok := value.(string)
			if !ok {
				return fmt.Errorf("payload.%s must be a string", leaf)
			}
			if err := validateBoundedString("payload."+leaf, text, 128); err != nil {
				return err
			}
		}
	}
	if value, ok := payload["draft_hash"]; ok {
		digest, ok := value.(string)
		if !ok || len(digest) != 64 {
			return fmt.Errorf("payload.draft_hash must be a 64-character hexadecimal digest")
		}
		if _, err := hex.DecodeString(digest); err != nil {
			return fmt.Errorf("payload.draft_hash must be a 64-character hexadecimal digest")
		}
	}
	if value, ok := payload["impact_counts"]; ok {
		if _, ok := value.(ImpactCounts); !ok {
			return fmt.Errorf("payload.impact_counts has an invalid type")
		}
	}
	if value, ok := payload["changed_fields"]; ok {
		fields, ok := value.([]string)
		if !ok {
			return fmt.Errorf("payload.changed_fields must be a string array")
		}
		if len(fields) > 32 {
			return fmt.Errorf("payload.changed_fields must contain at most 32 items")
		}
		for i, field := range fields {
			if err := validateBoundedString(fmt.Sprintf("payload.changed_fields[%d]", i), field, 256); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateBoundedString(name, value string, maxBytes int) error {
	if value == "" {
		return fmt.Errorf("%s is required", name)
	}
	return validateOptionalBoundedString(name, value, maxBytes)
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
