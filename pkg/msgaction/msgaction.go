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

// Package msgaction defines the kind-agnostic message-action lifecycle core
// shared by decisions, secret requests, and future interactive message actions.
// It is pure and has no hub or store imports.
package msgaction

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

// Status classes shared across message-action kinds.
type Class string

const (
	ClassPending   Class = "pending"
	ClassClaimed   Class = "claimed"
	ClassResolved  Class = "resolved"
	ClassReplaced  Class = "replaced"
	ClassWithdrawn Class = "withdrawn"
	ClassDismissed Class = "dismissed"
	ClassExpired   Class = "expired"
)

// Terminal reports whether c is a terminal lifecycle class (no longer pending
// or claimed).
func (c Class) Terminal() bool {
	switch c {
	case ClassResolved, ClassReplaced, ClassWithdrawn, ClassDismissed, ClassExpired:
		return true
	default:
		return false
	}
}

// Answerable reports whether a row in class c can accept a fresh response.
func (c Class) Answerable() bool {
	return c == ClassPending
}

// Conflict codes returned on HTTP 409 for non-answerable states.
const (
	ConflictReplaced        = "replaced"
	ConflictWithdrawn       = "withdrawn"
	ConflictDismissed       = "dismissed"
	ConflictExpired         = "expired"
	ConflictAlreadyResolved = "already_resolved"
	ConflictSubmitting      = "submitting"
)

// Shared limits for message-action requests.
const (
	MaxReasonRunes       = 500
	MaxIdempotencyKeyLen = 128
	MaxRequestBodyBytes  = 64 * 1024
)

// Sentinel errors returned by pure lifecycle helpers.
var (
	// ErrKeyPending is returned by PlanRepost when the key has a non-terminal
	// (pending or claimed) version and update is false.
	ErrKeyPending = errors.New("key has a pending version")
	// ErrKeyExists is returned by PlanRepost when the key has a terminal prior
	// version, update is false, and KindSpec.RepostTerminalOK is false.
	ErrKeyExists = errors.New("key already exists")
	// ErrReasonTooLong is returned by NormalizeReason when the trimmed reason
	// exceeds MaxReasonRunes.
	ErrReasonTooLong = errors.New("reason exceeds maximum length")
)

// KindSpec configures the kind-specific parameters of a message-action kind.
type KindSpec struct {
	// Name is the event topic segment and audit kind (e.g. "decision", "secret_request").
	Name string
	// ResolvedStatus is the kind's stored status name for ClassResolved
	// (e.g. "decided", "fulfilled"). Defaults to "resolved" when empty.
	ResolvedStatus string
	// ResolvedConflictCode overrides the 409 conflict code for ClassResolved
	// (e.g. "already_answered" for decisions). Defaults to ConflictAlreadyResolved.
	ResolvedConflictCode string
	// UsesClaim reports whether the kind uses the transient ClassClaimed state.
	UsesClaim bool
	// Expires is the default TTL of a pending action (0 means never expires).
	Expires time.Duration
	// DismissMinAge is the minimum age before a user may dismiss an action (0 means none).
	DismissMinAge time.Duration
	// RepostTerminalOK reports whether re-asking a key whose latest version is
	// terminal automatically allocates the next version without update=true.
	// Decisions set this to false; secret requests set this to true.
	RepostTerminalOK bool
}

// DecisionKindSpec is the KindSpec for decision prompts.
var DecisionKindSpec = KindSpec{
	Name:                 "decision",
	ResolvedStatus:       "decided",
	ResolvedConflictCode: "already_answered",
	UsesClaim:            false,
	Expires:              0,
	DismissMinAge:        5 * time.Minute,
	RepostTerminalOK:     false,
}

// Classify maps a kind-specific stored status string to its lifecycle Class.
func (s KindSpec) Classify(status string) (Class, bool) {
	resolved := s.ResolvedStatus
	if resolved == "" {
		resolved = string(ClassResolved)
	}
	switch status {
	case string(ClassPending):
		return ClassPending, true
	case string(ClassClaimed):
		if s.UsesClaim {
			return ClassClaimed, true
		}
		return "", false
	case resolved:
		return ClassResolved, true
	case string(ClassReplaced):
		return ClassReplaced, true
	case string(ClassWithdrawn):
		return ClassWithdrawn, true
	case string(ClassDismissed):
		return ClassDismissed, true
	case string(ClassExpired):
		if s.Expires > 0 {
			return ClassExpired, true
		}
		return "", false
	default:
		return "", false
	}
}

// Terminal reports whether status is terminal under s.
func (s KindSpec) Terminal(status string) bool {
	cls, ok := s.Classify(status)
	return ok && cls.Terminal()
}

// Answerable reports whether status is answerable under s.
func (s KindSpec) Answerable(status string) bool {
	cls, ok := s.Classify(status)
	return ok && cls.Answerable()
}

// ConflictCode maps a stored status string to its HTTP 409 conflict code,
// or "" when status is pending or unrecognized.
func ConflictCode(spec KindSpec, status string) string {
	cls, ok := spec.Classify(status)
	if !ok {
		return ""
	}
	switch cls {
	case ClassReplaced:
		return ConflictReplaced
	case ClassWithdrawn:
		return ConflictWithdrawn
	case ClassDismissed:
		return ConflictDismissed
	case ClassExpired:
		return ConflictExpired
	case ClassClaimed:
		return ConflictSubmitting
	case ClassResolved:
		if spec.ResolvedConflictCode != "" {
			return spec.ResolvedConflictCode
		}
		return ConflictAlreadyResolved
	default:
		return ""
	}
}

// Head describes the latest stored version of one (asker_agent_id, key).
type Head struct {
	RowID      string
	Key        string
	Version    int
	Status     string
	ReplacedBy string
}

// RepostPlan describes how a new prompt version for a key should be inserted
// and how its predecessor (if any) should be updated.
type RepostPlan struct {
	NextVersion   int
	DisplayKey    string
	ReplacesRowID string
	// MarkReplaced is true when the predecessor row should transition its status
	// to "replaced" (alongside setting replaced_at and replaced_by_id). When
	// false and ReplacesRowID is non-empty, the predecessor keeps its existing
	// terminal status (e.g. withdrawn, dismissed, expired) while recording
	// replaced_at and replaced_by_id.
	MarkReplaced bool
}

// PlanRepost computes the version and predecessor transition for one key.
// head is nil when the agent has no prior versions for the key.
func PlanRepost(spec KindSpec, head *Head, update bool) (RepostPlan, error) {
	if head == nil {
		return RepostPlan{NextVersion: 1}, nil
	}
	cls, ok := spec.Classify(head.Status)
	if !ok {
		return RepostPlan{}, fmt.Errorf("unknown status %q for kind %q", head.Status, spec.Name)
	}
	if !update {
		if !cls.Terminal() {
			return RepostPlan{}, ErrKeyPending
		}
		if !spec.RepostTerminalOK {
			return RepostPlan{}, ErrKeyExists
		}
	}
	plan := RepostPlan{
		NextVersion: head.Version + 1,
		DisplayKey:  head.Key,
	}
	if head.RowID != "" && head.ReplacedBy == "" && cls != ClassReplaced {
		plan.ReplacesRowID = head.RowID
		switch cls {
		case ClassPending, ClassClaimed:
			plan.MarkReplaced = true
		case ClassResolved:
			// Kinds that require --update to replace a resolved version (like
			// decisions) transition the prior row's status to replaced; kinds
			// with RepostTerminalOK keep the terminal status intact.
			plan.MarkReplaced = !spec.RepostTerminalOK
		default:
			plan.MarkReplaced = false
		}
	}
	return plan, nil
}

// NormalizeReason trims whitespace and enforces MaxReasonRunes.
func NormalizeReason(s string) (string, error) {
	trimmed := strings.TrimSpace(s)
	if utf8.RuneCountInString(trimmed) > MaxReasonRunes {
		return "", fmt.Errorf("%w: reason is at most %d characters", ErrReasonTooLong, MaxReasonRunes)
	}
	return trimmed, nil
}

// FieldError describes a request parsing or validation failure.
type FieldError struct {
	Field       string
	Message     string
	InvalidJSON bool
}

func (e *FieldError) Error() string { return e.Message }

// WithdrawRequest is the parsed body of an agent withdraw request.
type WithdrawRequest struct {
	Keys     []string `json:"keys"`
	WholeSet bool     `json:"wholeSet,omitempty"`
	Reason   string   `json:"reason,omitempty"`
}

// WithdrawParseOptions configures ParseWithdrawRequest.
type WithdrawParseOptions struct {
	MaxKeys      int
	NormalizeKey func(string) string
	KeyLabel     string // used in duplicate error message; defaults to "key"
}

// ParseWithdrawRequest decodes and validates a withdraw JSON body.
func ParseWithdrawRequest(r io.Reader, opts WithdrawParseOptions) (*WithdrawRequest, *FieldError) {
	body, ferr := readBoundedBody(r, MaxRequestBodyBytes, false)
	if ferr != nil {
		return nil, ferr
	}
	var req WithdrawRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, &FieldError{Message: "invalid JSON body: " + err.Error(), InvalidJSON: true}
	}
	if opts.MaxKeys <= 0 {
		opts.MaxKeys = 10
	}
	if len(req.Keys) < 1 || len(req.Keys) > opts.MaxKeys {
		return nil, &FieldError{
			Field:   "keys",
			Message: fmt.Sprintf("keys must contain 1 to %d items", opts.MaxKeys),
		}
	}
	normFn := opts.NormalizeKey
	if normFn == nil {
		normFn = strings.ToLower
	}
	keyLabel := opts.KeyLabel
	if keyLabel == "" {
		keyLabel = "key"
	}
	seen := make(map[string]bool, len(req.Keys))
	for i := range req.Keys {
		req.Keys[i] = strings.TrimSpace(req.Keys[i])
		if req.Keys[i] == "" {
			return nil, &FieldError{
				Field:   "keys",
				Message: "each key must be non-empty",
			}
		}
		norm := normFn(req.Keys[i])
		if seen[norm] {
			return nil, &FieldError{
				Field:   "keys",
				Message: fmt.Sprintf("duplicate %s %q in request", keyLabel, req.Keys[i]),
			}
		}
		seen[norm] = true
	}
	reason, err := NormalizeReason(req.Reason)
	if err != nil {
		return nil, &FieldError{
			Field:   "reason",
			Message: fmt.Sprintf("reason is at most %d characters", MaxReasonRunes),
		}
	}
	req.Reason = reason
	return &req, nil
}

// DismissRequest is the parsed body of a user dismiss request.
type DismissRequest struct {
	RowIDs         []string `json:"rowIds,omitempty"`
	Reason         string   `json:"reason,omitempty"`
	Surface        string   `json:"surface,omitempty"`
	IdempotencyKey string   `json:"idempotencyKey,omitempty"`
}

// DismissParseOptions configures ParseDismissRequest.
type DismissParseOptions struct {
	// RowIDsField names the JSON field for row IDs (defaults to "rowIds";
	// decisions use "decisionIds").
	RowIDsField string
	// MaxRowIDs caps len(RowIDs) when > 0.
	MaxRowIDs int
	// NormalizeSurface validates and defaults the optional surface field when non-nil.
	NormalizeSurface func(raw string) (string, bool)
	// SurfaceError formats the validation message when NormalizeSurface fails.
	SurfaceError func(raw string) string
}

// ParseDismissRequest decodes and validates an optional-body dismiss JSON request.
func ParseDismissRequest(r io.Reader, opts DismissParseOptions) (*DismissRequest, *FieldError) {
	rowIDsField := opts.RowIDsField
	if rowIDsField == "" {
		rowIDsField = "rowIds"
	}
	body, ferr := readBoundedBody(r, MaxRequestBodyBytes, true)
	if ferr != nil {
		return nil, ferr
	}
	var req DismissRequest
	if len(body) > 0 {
		if rowIDsField == "rowIds" {
			if err := json.Unmarshal(body, &req); err != nil {
				return nil, &FieldError{Message: "Invalid request body: " + err.Error(), InvalidJSON: true}
			}
		} else {
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(body, &raw); err != nil {
				return nil, &FieldError{Message: "Invalid request body: " + err.Error(), InvalidJSON: true}
			}
			if v, ok := raw[rowIDsField]; ok {
				if err := json.Unmarshal(v, &req.RowIDs); err != nil {
					return nil, &FieldError{Message: "Invalid request body: " + err.Error(), InvalidJSON: true}
				}
			}
			if v, ok := raw["reason"]; ok {
				if err := json.Unmarshal(v, &req.Reason); err != nil {
					return nil, &FieldError{Message: "Invalid request body: " + err.Error(), InvalidJSON: true}
				}
			}
			if v, ok := raw["surface"]; ok {
				if err := json.Unmarshal(v, &req.Surface); err != nil {
					return nil, &FieldError{Message: "Invalid request body: " + err.Error(), InvalidJSON: true}
				}
			}
			if v, ok := raw["idempotencyKey"]; ok {
				if err := json.Unmarshal(v, &req.IdempotencyKey); err != nil {
					return nil, &FieldError{Message: "Invalid request body: " + err.Error(), InvalidJSON: true}
				}
			}
		}
	}
	if len(req.IdempotencyKey) > MaxIdempotencyKeyLen {
		return nil, &FieldError{
			Field:   "idempotencyKey",
			Message: fmt.Sprintf("idempotencyKey is at most %d bytes", MaxIdempotencyKeyLen),
		}
	}
	if opts.MaxRowIDs > 0 && len(req.RowIDs) > opts.MaxRowIDs {
		return nil, &FieldError{
			Field:   rowIDsField,
			Message: fmt.Sprintf("%s accepts at most %d ids", rowIDsField, opts.MaxRowIDs),
		}
	}
	if opts.NormalizeSurface != nil {
		surface, ok := opts.NormalizeSurface(req.Surface)
		if !ok {
			msg := fmt.Sprintf("surface %q is not supported", req.Surface)
			if opts.SurfaceError != nil {
				msg = opts.SurfaceError(req.Surface)
			}
			return nil, &FieldError{Field: "surface", Message: msg}
		}
		req.Surface = surface
	}
	reason, err := NormalizeReason(req.Reason)
	if err != nil {
		return nil, &FieldError{
			Field:   "reason",
			Message: fmt.Sprintf("reason is at most %d characters", MaxReasonRunes),
		}
	}
	req.Reason = reason
	return &req, nil
}

func readBoundedBody(r io.Reader, limit int64, allowEmpty bool) ([]byte, *FieldError) {
	if r == nil {
		if allowEmpty {
			return nil, nil
		}
		return nil, &FieldError{Message: "invalid JSON body: empty body", InvalidJSON: true}
	}
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, &FieldError{Message: "Invalid request body: " + err.Error(), InvalidJSON: true}
	}
	if int64(len(body)) > limit {
		return nil, &FieldError{
			Message:     fmt.Sprintf("Invalid request body: request body exceeds %d bytes", limit),
			InvalidJSON: true,
		}
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		if allowEmpty {
			return nil, nil
		}
		return nil, &FieldError{Message: "invalid JSON body: EOF", InvalidJSON: true}
	}
	return trimmed, nil
}
