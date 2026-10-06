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

// Package credentialmeta owns the dependency-neutral validation contract for
// descriptive credential metadata. It never carries credential material or
// grants authority.
package credentialmeta

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	MaxKindBytes       = 64
	MaxIDBytes         = 128
	MaxNameBytes       = 128
	MaxPurposeBytes    = 128
	MaxLabelCount      = 8
	MaxLabelKeyBytes   = 32
	MaxLabelValueBytes = 64
)

// Kind is a canonical server credential kind.
type Kind string

const (
	KindInteractive Kind = "interactive"
	KindUAT         Kind = "uat"
	KindAgentJWT    Kind = "agent_jwt"
	KindFederation  Kind = "federation"
	KindBroker      Kind = "broker"
	KindDev         Kind = "dev"
)

var kinds = []Kind{KindInteractive, KindUAT, KindAgentJWT, KindFederation, KindBroker, KindDev}

// Kinds returns every canonical credential kind.
func Kinds() []Kind {
	return append([]Kind(nil), kinds...)
}

// BoundaryKind is the canonical descriptive credential-boundary kind.
type BoundaryKind string

const (
	BoundaryProject BoundaryKind = "project"
	BoundaryHub     BoundaryKind = "hub"
)

var boundaryKinds = []BoundaryKind{BoundaryProject, BoundaryHub}

// BoundaryKinds returns every canonical credential-boundary kind.
func BoundaryKinds() []BoundaryKind {
	return append([]BoundaryKind(nil), boundaryKinds...)
}

// ValidationError identifies a rejected field and rule without retaining the
// rejected value.
type ValidationError struct {
	Field string
	Rule  string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("invalid credential metadata field %s: %s", e.Field, e.Rule)
}

func invalid(field, rule string) error {
	return &ValidationError{Field: field, Rule: rule}
}

// RefInput is accepted only by NewRef, which returns an immutable validated
// credential reference suitable for serialization at an audit boundary.
type RefInput struct {
	Kind              Kind
	ID                string
	Name              string
	BoundaryKind      BoundaryKind
	BoundaryProjectID string
	Labels            map[string]string
}

// Ref is validated descriptive credential metadata. Its fields are private so
// callers cannot construct or mutate an unvalidated reference.
type Ref struct {
	kind              Kind
	id                string
	name              string
	boundaryKind      BoundaryKind
	boundaryProjectID string
	labels            map[string]string
}

// NewRef validates and defensively copies descriptive credential metadata.
func NewRef(input RefInput) (Ref, error) {
	ref := Ref{
		kind:              input.Kind,
		id:                input.ID,
		name:              input.Name,
		boundaryKind:      input.BoundaryKind,
		boundaryProjectID: input.BoundaryProjectID,
		labels:            cloneLabels(input.Labels),
	}
	if err := ref.Validate(); err != nil {
		return Ref{}, err
	}
	return ref, nil
}

// Validate checks the complete serialized credential reference contract.
func (r Ref) Validate() error {
	if !slices.Contains(kinds, r.kind) {
		return invalid("kind", "must be a canonical credential kind")
	}
	if err := validateString("id", r.id, MaxIDBytes, false); err != nil {
		return err
	}
	if err := validateString("name", r.name, MaxNameBytes, false); err != nil {
		return err
	}
	switch r.boundaryKind {
	case "":
		if r.boundaryProjectID != "" {
			return invalid("boundary_project_id", "requires a boundary kind")
		}
	case BoundaryProject:
		if r.boundaryProjectID == "" {
			return invalid("boundary_project_id", "is required for a project boundary")
		}
	case BoundaryHub:
		if r.boundaryProjectID != "" {
			return invalid("boundary_project_id", "must be absent for a hub boundary")
		}
	default:
		return invalid("boundary_kind", "must be a canonical boundary kind")
	}
	if err := validateString("boundary_project_id", r.boundaryProjectID, MaxIDBytes, false); err != nil {
		return err
	}
	return validateLabels(r.labels)
}

func (r Ref) Kind() Kind                 { return r.kind }
func (r Ref) ID() string                 { return r.id }
func (r Ref) Name() string               { return r.name }
func (r Ref) BoundaryKind() BoundaryKind { return r.boundaryKind }
func (r Ref) BoundaryProjectID() string  { return r.boundaryProjectID }
func (r Ref) Labels() map[string]string  { return cloneLabels(r.labels) }
func (r Ref) IsZero() bool               { return r.kind == "" }

// ValidateIssuance preserves the existing UAT issuance behavior for optional
// name and purpose metadata while sharing the canonical label contract.
func ValidateIssuance(name, purpose string, labels map[string]string) error {
	if err := validateString("name", name, MaxNameBytes, false); err != nil {
		return err
	}
	trimmedPurpose := strings.TrimSpace(purpose)
	if trimmedPurpose != "" {
		if err := validateString("purpose", trimmedPurpose, MaxPurposeBytes, false); err != nil {
			if validationErr, ok := err.(*ValidationError); ok && validationErr.Rule == "must not contain control or formatting characters" {
				return invalid("purpose", "must not contain control or formatting characters, and must be a single line")
			}
			return err
		}
	}
	return validateLabels(labels)
}

func validateString(field, value string, maxBytes int, required bool) error {
	if value == "" {
		if required {
			return invalid(field, "is required")
		}
		return nil
	}
	if !utf8.ValidString(value) {
		return invalid(field, "must be valid UTF-8")
	}
	if len(value) > maxBytes {
		return invalid(field, fmt.Sprintf("must be at most %d bytes", maxBytes))
	}
	if hasUnsafeRune(value) {
		return invalid(field, "must not contain control or formatting characters")
	}
	if LooksSecret(value) {
		return invalid(field, "must not resemble a bearer token or credential value")
	}
	return nil
}

func validateLabels(labels map[string]string) error {
	if len(labels) > MaxLabelCount {
		return invalid("labels", fmt.Sprintf("at most %d labels are allowed", MaxLabelCount))
	}
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := labels[key]
		if !utf8.ValidString(key) || !utf8.ValidString(value) {
			return invalid("labels", "label key and value must be valid UTF-8")
		}
		if !ValidLabelKey(key) {
			return invalid("labels", fmt.Sprintf("label key must match ^[a-z][a-z0-9_.-]{0,%d}$", MaxLabelKeyBytes-1))
		}
		if reservedLabelKey(key) {
			return invalid("labels", "label key is reserved")
		}
		if len(value) > MaxLabelValueBytes {
			return invalid("labels", fmt.Sprintf("label value must be at most %d bytes", MaxLabelValueBytes))
		}
		if value != strings.TrimSpace(value) {
			return invalid("labels", "label value must not have leading or trailing whitespace")
		}
		if !validLabelValue(value) {
			return invalid("labels", "label value contains a disallowed character")
		}
		if LooksSecret(key) || LooksSecret(value) {
			return invalid("labels", "must not resemble a bearer token or credential value")
		}
	}
	return nil
}

// ValidLabelKey reports whether value matches ^[a-z][a-z0-9_.-]{0,31}$.
func ValidLabelKey(value string) bool {
	if len(value) == 0 || len(value) > MaxLabelKeyBytes {
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

func validLabelValue(value string) bool {
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

var reservedLabelKeys = map[string]struct{}{
	"agent": {}, "agent_id": {}, "actor": {}, "principal": {},
	"principal_id": {}, "principal_kind": {}, "user": {}, "user_id": {},
	"email": {}, "on_behalf_of": {}, "delegate": {}, "delegator": {},
	"delegation": {}, "ancestry": {}, "creator": {}, "created_by": {},
	"owner": {}, "project_id": {}, "broker_id": {}, "credential": {},
	"credential_id": {}, "token": {}, "token_id": {}, "role": {},
	"scope": {}, "scopes": {}, "permission": {}, "permissions": {},
	"verified": {}, "system": {}, "executor": {}, "initiator": {},
	"actor_binding": {},
}

var reservedActorLabelKeys = []string{
	"actor_agent_id",
	"authorizing_user_id",
	"source_grant_id",
	"delegation_edge_id",
	"parent_grant_id",
	"exchange_agent_credential_id",
	"actor_kind",
}

func init() {
	for _, key := range reservedActorLabelKeys {
		reservedLabelKeys[key] = struct{}{}
	}
}

// ReservedActorLabelKeys returns the canonical verified-actor names reserved
// from issuer-supplied labels.
func ReservedActorLabelKeys() []string {
	return append([]string(nil), reservedActorLabelKeys...)
}

func reservedLabelKey(value string) bool {
	lower := strings.ToLower(value)
	for _, prefix := range []string{"scion.", "hub.", "x-"} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	normalized := strings.ReplaceAll(lower, "-", "_")
	if _, ok := reservedLabelKeys[normalized]; ok {
		return true
	}
	for reserved := range reservedLabelKeys {
		if strings.HasPrefix(normalized, reserved+".") {
			return true
		}
	}
	return false
}

// LooksSecret reports whether value resembles credential material forbidden
// from descriptive metadata.
func LooksSecret(value string) bool {
	lower := strings.ToLower(value)
	return strings.Contains(lower, "scion_pat_") || strings.Contains(lower, "bearer ")
}

func hasUnsafeRune(value string) bool {
	for _, r := range value {
		if unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
			return true
		}
	}
	return false
}

func cloneLabels(labels map[string]string) map[string]string {
	if labels == nil {
		return nil
	}
	copy := make(map[string]string, len(labels))
	for key, value := range labels {
		copy[key] = value
	}
	return copy
}
