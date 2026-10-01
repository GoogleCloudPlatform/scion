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
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	credentialMaxNameBytes       = 128
	credentialMaxLabelCount      = 8
	credentialMaxLabelKeyBytes   = 32
	credentialMaxLabelValueBytes = 64
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

// CredentialValidationError reports a credential metadata rule violation
// without retaining or echoing the rejected value.
type CredentialValidationError struct {
	field string
	rule  string
}

func (e *CredentialValidationError) Error() string {
	return fmt.Sprintf("invalid credential metadata field %s: %s", e.field, e.rule)
}

func newCredentialValidationError(field, rule string) error {
	return &CredentialValidationError{field: field, rule: rule}
}

func validateCredentialString(field, value string, maxBytes int, required, rejectSecret bool) error {
	if value == "" {
		if required {
			return newCredentialValidationError(field, "is required")
		}
		return nil
	}
	if !utf8.ValidString(value) {
		return newCredentialValidationError(field, "must be valid UTF-8")
	}
	if len(value) > maxBytes {
		return newCredentialValidationError(field, fmt.Sprintf("must be at most %d bytes", maxBytes))
	}
	if hasAuditUnsafeRune(value) {
		return newCredentialValidationError(field, "must not contain control or formatting characters")
	}
	if rejectSecret && credentialMetadataLooksSecret(value) {
		return newCredentialValidationError(field, "must not resemble a bearer token or credential value")
	}
	return nil
}

func validCredentialLabelKey(value string) bool {
	if len(value) == 0 || len(value) > credentialMaxLabelKeyBytes {
		return false
	}
	for i, r := range value {
		if i == 0 {
			if r >= 'a' && r <= 'z' {
				continue
			}
			return false
		}
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '.', r == '-':
		default:
			return false
		}
	}
	return true
}

func validCredentialLabelValue(value string) bool {
	for _, r := range value {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case strings.ContainsRune(" _.:/@+=,-", r):
		default:
			return false
		}
	}
	return true
}

var reservedCredentialLabelKeys = map[string]struct{}{
	"agent": {}, "agent_id": {}, "actor": {}, "principal": {},
	"principal_id": {}, "principal_kind": {}, "user": {}, "user_id": {},
	"email": {}, "on_behalf_of": {}, "delegate": {}, "delegator": {},
	"delegation": {}, "ancestry": {}, "creator": {}, "created_by": {},
	"owner": {}, "project_id": {}, "broker_id": {}, "credential": {},
	"credential_id": {}, "token": {}, "token_id": {}, "role": {},
	"scope": {}, "scopes": {}, "permission": {}, "permissions": {},
	"verified": {}, "system": {}, "executor": {}, "initiator": {},
	"actor_binding": {}, "actor_agent_id": {}, "authorizing_user_id": {},
	"source_grant_id": {}, "delegation_edge_id": {}, "parent_grant_id": {},
	"exchange_agent_credential_id": {}, "actor_kind": {},
}

var reservedCredentialLabelPrefixes = []string{"scion.", "hub.", "x-"}

func reservedCredentialLabelKey(value string) bool {
	lower := strings.ToLower(value)
	for _, prefix := range reservedCredentialLabelPrefixes {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	normalized := strings.ReplaceAll(lower, "-", "_")
	if _, ok := reservedCredentialLabelKeys[normalized]; ok {
		return true
	}
	for reserved := range reservedCredentialLabelKeys {
		if strings.HasPrefix(normalized, reserved+".") {
			return true
		}
	}
	return false
}

func credentialMetadataLooksSecret(value string) bool {
	lower := strings.ToLower(value)
	return strings.Contains(lower, "scion_pat_") || strings.Contains(lower, "bearer ")
}

func hasAuditUnsafeRune(value string) bool {
	for _, r := range value {
		if unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
			return true
		}
	}
	return false
}

func validateCredential(credential *CredentialRef) error {
	if credential == nil {
		return nil
	}
	if err := validateCredentialString("kind", credential.Kind, 64, true, false); err != nil {
		return err
	}
	fields := []struct {
		name         string
		value        string
		limit        int
		rejectSecret bool
	}{
		{"id", credential.ID, 128, false},
		{"name", credential.Name, credentialMaxNameBytes, true},
		{"boundary_kind", credential.BoundaryKind, 64, false},
		{"boundary_project_id", credential.BoundaryProjectID, 128, false},
	}
	for _, field := range fields {
		if err := validateCredentialString(field.name, field.value, field.limit, false, field.rejectSecret); err != nil {
			return err
		}
	}
	if len(credential.Labels) > credentialMaxLabelCount {
		return newCredentialValidationError("labels", fmt.Sprintf("must contain at most %d entries", credentialMaxLabelCount))
	}
	keys := make([]string, 0, len(credential.Labels))
	for key := range credential.Labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := credential.Labels[key]
		if !utf8.ValidString(key) || !utf8.ValidString(value) {
			return newCredentialValidationError("labels", "label key and value must be valid UTF-8")
		}
		if !validCredentialLabelKey(key) {
			return newCredentialValidationError("labels", fmt.Sprintf("label key must match ^[a-z][a-z0-9_.-]{0,%d}$", credentialMaxLabelKeyBytes-1))
		}
		if reservedCredentialLabelKey(key) {
			return newCredentialValidationError("labels", "label key is reserved")
		}
		if len(value) > credentialMaxLabelValueBytes {
			return newCredentialValidationError("labels", fmt.Sprintf("label value must be at most %d bytes", credentialMaxLabelValueBytes))
		}
		if value != strings.TrimSpace(value) {
			return newCredentialValidationError("labels", "label value must not have leading or trailing whitespace")
		}
		if !validCredentialLabelValue(value) {
			return newCredentialValidationError("labels", "label value contains a disallowed character")
		}
		if credentialMetadataLooksSecret(key) || credentialMetadataLooksSecret(value) {
			return newCredentialValidationError("labels", "must not resemble a bearer token or credential value")
		}
	}
	return nil
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
