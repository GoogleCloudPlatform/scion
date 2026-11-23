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
	"sync"
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

// ClaimRecovery configures how stale claims are handled by a sweeper (C1).
// Kinds whose resolve operation is idempotent (e.g. secret requests) use
// ClaimRecoveryToPending; kinds whose resolve may have already triggered a
// non-idempotent side effect (e.g. exec approval) use ClaimRecoveryNone so a
// stale claim is never re-armed.
type ClaimRecovery string

const (
	ClaimRecoveryNone      ClaimRecovery = "none"
	ClaimRecoveryToPending ClaimRecovery = "to_pending"
)

// Core kind-neutral audit event types (C3). Kinds may register additional
// event types (such as "fulfilled" or "claim_recovered") via KindSpec.EventTypes.
const (
	EventRequested = "requested"
	EventReplaced  = "replaced"
	EventWithdrawn = "withdrawn"
	EventResolved  = "resolved"
	EventDismissed = "dismissed"
	EventExpired   = "expired"
	EventRefused   = "refused"
)

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
	// Name is the event topic segment and audit kind (e.g. "decision", "secret_request", "exec_action").
	Name string
	// ResolvedStatus is the kind's stored status name for ClassResolved
	// (e.g. "decided", "fulfilled", "approved"). Defaults to "resolved" when empty.
	ResolvedStatus string
	// ClaimedStatus is the kind's stored status name for ClassClaimed
	// (e.g. "fulfilling" for secret requests). Defaults to "claimed" when empty.
	ClaimedStatus string
	// ResolvedConflictCode overrides the 409 conflict code for ClassResolved
	// (e.g. "already_answered" for decisions). Defaults to ConflictAlreadyResolved.
	ResolvedConflictCode string
	// UsesClaim reports whether the kind uses the transient ClassClaimed state.
	UsesClaim bool
	// ClaimRecovery controls whether stale claims may return to pending (C1).
	// Only ClaimRecoveryToPending allows automatic recovery; ClaimRecoveryNone
	// (or zero value) never re-arms a stale claim.
	ClaimRecovery ClaimRecovery
	// ClaimTTL is the staleness threshold used only when ClaimRecovery == ClaimRecoveryToPending.
	ClaimTTL time.Duration
	// Expires is the default TTL of a pending action (0 means never expires).
	Expires time.Duration
	// DismissMinAge is the minimum age before a user may dismiss an action (0 means none).
	DismissMinAge time.Duration
	// RepostTerminalOK reports whether re-asking a key whose latest version is
	// terminal automatically allocates the next version without update=true.
	// Decisions set this to false; secret requests set this to true.
	RepostTerminalOK bool
	// ReplaceResolvedOnUpdate reports whether replacing a resolved prior version
	// transitions its status to "replaced" (true for decisions) rather than
	// preserving its resolved status while linking replaced_by_id (false for
	// secret requests and exec actions).
	ReplaceResolvedOnUpdate bool
	// EventTypes lists kind-registered audit event types in addition to the core
	// set (C3), e.g. "fulfilled", "claim_recovered".
	EventTypes []string
	// ConflictCodes lists kind-registered 4xx conflict/error codes in addition to
	// the core set (C3), e.g. "secret_request_key_pending".
	ConflictCodes []string
	// DetailKeys lists the allow-listed audit MessageActionEvent.Details keys
	// for this kind (C3, §4.11). Audit emission fails closed: any detail key not
	// listed here (and all details when empty) is dropped before logging.
	DetailKeys []string
}

var (
	kindRegistryMu sync.RWMutex
	kindRegistry   = map[string]KindSpec{
		DecisionKindSpec.Name: DecisionKindSpec,
	}
)

// RegisterKind registers spec under spec.Name in the package-level kind
// registry used by audit emission to enforce per-kind event types and detail
// key allow-lists.
func RegisterKind(spec KindSpec) {
	if spec.Name == "" {
		return
	}
	kindRegistryMu.Lock()
	kindRegistry[spec.Name] = spec
	kindRegistryMu.Unlock()
}

// UnregisterKind removes name from the package-level kind registry (intended
// for test cleanup).
func UnregisterKind(name string) {
	kindRegistryMu.Lock()
	delete(kindRegistry, name)
	kindRegistryMu.Unlock()
}

// LookupKind returns the registered KindSpec for name, or false when name is
// not registered.
func LookupKind(name string) (KindSpec, bool) {
	kindRegistryMu.RLock()
	spec, ok := kindRegistry[name]
	kindRegistryMu.RUnlock()
	return spec, ok
}

// DecisionKindSpec is the KindSpec for decision prompts.
var DecisionKindSpec = KindSpec{
	Name:                    "decision",
	ResolvedStatus:          "decided",
	ResolvedConflictCode:    "already_answered",
	UsesClaim:               false,
	ClaimRecovery:           ClaimRecoveryNone,
	Expires:                 0,
	DismissMinAge:           5 * time.Minute,
	RepostTerminalOK:        false,
	ReplaceResolvedOnUpdate: true,
}

// Anchor optionally links a message-action row to a chat message and block.
// Identity is (asker_agent_id, key_norm, version), not the message, so a row
// needs no anchor (C4).
type Anchor struct {
	MessageID  string
	BlockIndex int
}

// IsZero reports whether a has no message anchor.
func (a Anchor) IsZero() bool {
	return a.MessageID == ""
}

// Classify maps a kind-specific stored status string to its lifecycle Class.
func (s KindSpec) Classify(status string) (Class, bool) {
	resolved := s.ResolvedStatus
	if resolved == "" {
		resolved = string(ClassResolved)
	}
	claimed := s.ClaimedStatus
	if claimed == "" {
		claimed = string(ClassClaimed)
	}
	switch status {
	case string(ClassPending):
		return ClassPending, true
	case claimed:
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

// ShouldRecoverClaim reports whether a row claimed at claimedAt should be
// returned to pending at now (C1). It is false whenever UsesClaim is false,
// ClaimRecovery != ClaimRecoveryToPending (including ClaimRecoveryNone),
// ClaimTTL <= 0, or claimedAt is zero.
func (s KindSpec) ShouldRecoverClaim(claimedAt, now time.Time) bool {
	if !s.UsesClaim || s.ClaimRecovery != ClaimRecoveryToPending || s.ClaimTTL <= 0 || claimedAt.IsZero() {
		return false
	}
	return !now.Before(claimedAt.Add(s.ClaimTTL))
}

// AcceptsEventType reports whether evType is valid for s: either one of the
// core audit event types, s.ResolvedStatus, or a kind-registered entry in
// s.EventTypes (C3).
func (s KindSpec) AcceptsEventType(evType string) bool {
	if evType == "" {
		return false
	}
	switch evType {
	case EventRequested, EventReplaced, EventWithdrawn, EventResolved, EventDismissed, EventExpired, EventRefused:
		return true
	}
	if s.ResolvedStatus != "" && evType == s.ResolvedStatus {
		return true
	}
	for _, extra := range s.EventTypes {
		if extra != "" && evType == extra {
			return true
		}
	}
	return false
}

// AcceptsDetailKey reports whether key is allow-listed in s.DetailKeys (C3).
// Fails closed: returns false when key is empty or s.DetailKeys is empty.
func (s KindSpec) AcceptsDetailKey(key string) bool {
	if key == "" {
		return false
	}
	for _, allowed := range s.DetailKeys {
		if allowed != "" && key == allowed {
			return true
		}
	}
	return false
}

// AcceptsConflictCode reports whether code is valid for s: either one of the
// core conflict codes, s.ResolvedConflictCode, or a kind-registered entry in
// s.ConflictCodes (C3).
func (s KindSpec) AcceptsConflictCode(code string) bool {
	if code == "" {
		return false
	}
	switch code {
	case ConflictReplaced, ConflictWithdrawn, ConflictDismissed, ConflictExpired, ConflictAlreadyResolved, ConflictSubmitting:
		return true
	}
	if s.ResolvedConflictCode != "" && code == s.ResolvedConflictCode {
		return true
	}
	for _, extra := range s.ConflictCodes {
		if extra != "" && code == extra {
			return true
		}
	}
	return false
}

// ConflictCode maps a stored status string (or a kind-registered conflict code)
// to its HTTP 409 conflict code, or "" when status is pending or unrecognized.
func ConflictCode(spec KindSpec, status string) string {
	cls, ok := spec.Classify(status)
	if ok {
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
	for _, extra := range spec.ConflictCodes {
		if extra != "" && status == extra {
			return status
		}
	}
	return ""
}

// Head describes the latest stored version of one (asker_agent_id, key).
// ResolvedAt is populated when the latest version resolved, letting a kind
// surface the prior outcome on repost.
type Head struct {
	RowID      string
	Key        string
	Version    int
	Status     string
	ReplacedBy string
	ResolvedAt *time.Time
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
	// terminal status (e.g. withdrawn, dismissed, expired, or resolved when
	// ReplaceResolvedOnUpdate is false) while recording replaced_at and
	// replaced_by_id.
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
			plan.MarkReplaced = spec.ReplaceResolvedOnUpdate
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

// WithdrawParseOptions configures ParseWithdrawRequest and ValidateWithdrawRequest.
type WithdrawParseOptions struct {
	MaxKeys      int
	NormalizeKey func(string) string
	KeyLabel     string // used in duplicate error message; defaults to "key"
}

// ValidateWithdrawRequest normalizes and validates an already-decoded
// WithdrawRequest in place. Callers that decode into their own request struct
// (e.g. via http.MaxBytesReader + json.NewDecoder) pass the decoded fields
// here so wire decoding semantics remain intact.
func ValidateWithdrawRequest(req *WithdrawRequest, opts WithdrawParseOptions) *FieldError {
	if req == nil {
		return &FieldError{Field: "keys", Message: "keys must contain at least 1 item"}
	}
	if opts.MaxKeys <= 0 {
		opts.MaxKeys = 10
	}
	if len(req.Keys) < 1 || len(req.Keys) > opts.MaxKeys {
		return &FieldError{
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
			return &FieldError{
				Field:   "keys",
				Message: "each key must be non-empty",
			}
		}
		norm := normFn(req.Keys[i])
		if seen[norm] {
			return &FieldError{
				Field:   "keys",
				Message: fmt.Sprintf("duplicate %s %q in request", keyLabel, req.Keys[i]),
			}
		}
		seen[norm] = true
	}
	reason, err := NormalizeReason(req.Reason)
	if err != nil {
		return &FieldError{
			Field:   "reason",
			Message: fmt.Sprintf("reason is at most %d characters", MaxReasonRunes),
		}
	}
	req.Reason = reason
	return nil
}

// ParseWithdrawRequest decodes a WithdrawRequest using json.NewDecoder
// (first-value struct decoding, case-insensitive field matching) and validates
// it via ValidateWithdrawRequest.
//
// Note: ParseWithdrawRequest currently has no production callers. Its decode
// rules differ from the hub's readJSON + http.MaxBytesReader path (it does not
// wrap r with a body-size limit). Decision handlers decode into
// WithdrawDecisionsRequest via readJSON and call ValidateWithdrawRequest so
// existing wire decoding and oversize error texts remain unchanged.
func ParseWithdrawRequest(r io.Reader, opts WithdrawParseOptions) (*WithdrawRequest, *FieldError) {
	if r == nil {
		return nil, &FieldError{Message: "invalid JSON body: EOF", InvalidJSON: true}
	}
	var req WithdrawRequest
	if err := json.NewDecoder(r).Decode(&req); err != nil {
		return nil, &FieldError{Message: "invalid JSON body: " + err.Error(), InvalidJSON: true}
	}
	if ferr := ValidateWithdrawRequest(&req, opts); ferr != nil {
		return nil, ferr
	}
	return &req, nil
}

// DismissRequest is the parsed body of a user dismiss request.
type DismissRequest struct {
	RowIDs         []string `json:"rowIds,omitempty"`
	Reason         string   `json:"reason,omitempty"`
	Surface        string   `json:"surface,omitempty"`
	IdempotencyKey string   `json:"idempotencyKey,omitempty"`
}

// DismissParseOptions configures ParseDismissRequest and ValidateDismissRequest.
type DismissParseOptions struct {
	// RowIDsField names the JSON field for row IDs in error details (defaults
	// to "rowIds"; decisions pass "decisionIds").
	RowIDsField string
	// MaxRowIDs caps len(RowIDs) when > 0.
	MaxRowIDs int
	// NormalizeSurface validates and defaults the optional surface field when non-nil.
	NormalizeSurface func(raw string) (string, bool)
	// SurfaceError formats the validation message when NormalizeSurface fails.
	SurfaceError func(raw string) string
}

// ValidateDismissRequest normalizes and validates an already-decoded
// DismissRequest in place. Callers with kind-specific JSON field names (such
// as decisions' "decisionIds") decode into their own struct and pass the
// fields here so Go's case-insensitive struct decoding and type-error messages
// are preserved verbatim.
func ValidateDismissRequest(req *DismissRequest, opts DismissParseOptions) *FieldError {
	if req == nil {
		return nil
	}
	rowIDsField := opts.RowIDsField
	if rowIDsField == "" {
		rowIDsField = "rowIds"
	}
	if len(req.IdempotencyKey) > MaxIdempotencyKeyLen {
		return &FieldError{
			Field:   "idempotencyKey",
			Message: fmt.Sprintf("idempotencyKey is at most %d bytes", MaxIdempotencyKeyLen),
		}
	}
	if opts.MaxRowIDs > 0 && len(req.RowIDs) > opts.MaxRowIDs {
		return &FieldError{
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
			return &FieldError{Field: "surface", Message: msg}
		}
		req.Surface = surface
	}
	reason, err := NormalizeReason(req.Reason)
	if err != nil {
		return &FieldError{
			Field:   "reason",
			Message: fmt.Sprintf("reason is at most %d characters", MaxReasonRunes),
		}
	}
	req.Reason = reason
	return nil
}

// ParseDismissRequest decodes an optional-body DismissRequest using standard
// Go struct decoding (case-insensitive field names) and validates it via
// ValidateDismissRequest.
//
// Note: ParseDismissRequest currently has no production callers. Its decode
// rules differ from the hub's readOptionalDismissJSON path (it has no
// ContentLength == 0 short-circuit and enforces MaxRequestBodyBytes directly
// via io.LimitReader + json.Unmarshal). Decision handlers decode into
// DismissDecisionPromptRequest via readOptionalDismissJSON and call
// ValidateDismissRequest so existing wire decoding and Go struct field type
// error texts remain unchanged.
func ParseDismissRequest(r io.Reader, opts DismissParseOptions) (*DismissRequest, *FieldError) {
	var req DismissRequest
	if r != nil {
		body, err := io.ReadAll(io.LimitReader(r, MaxRequestBodyBytes+1))
		if err != nil {
			return nil, &FieldError{Message: "Invalid request body: " + err.Error(), InvalidJSON: true}
		}
		if int64(len(body)) > MaxRequestBodyBytes {
			return nil, &FieldError{
				Message:     fmt.Sprintf("Invalid request body: request body exceeds %d bytes", MaxRequestBodyBytes),
				InvalidJSON: true,
			}
		}
		if len(bytes.TrimSpace(body)) > 0 {
			if err := json.Unmarshal(body, &req); err != nil {
				return nil, &FieldError{Message: "Invalid request body: " + err.Error(), InvalidJSON: true}
			}
		}
	}
	if ferr := ValidateDismissRequest(&req, opts); ferr != nil {
		return nil, ferr
	}
	return &req, nil
}
