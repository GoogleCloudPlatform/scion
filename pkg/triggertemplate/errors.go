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

// Package triggertemplate implements the Triggers Stage 2 placeholder grammar,
// prefix-based trust classification, per-slot validation registry, renderer,
// and untrusted-data framing trailer (stage2-design §5.2, S1 §5.8).
package triggertemplate

import (
	"errors"
	"fmt"
)

// ErrorKind classifies render-time failures for fire refusal reporting.
type ErrorKind string

const (
	// KindValueMissing is returned when a required ${path} placeholder has no value.
	KindValueMissing ErrorKind = "template value missing"
	// KindValueRejected is returned when an expanded value or rendered slot
	// violates a slot bound, character-set rule, or scalar requirement.
	KindValueRejected ErrorKind = "template value rejected"
)

var (
	// ErrValueMissing matches RenderError values with Kind == KindValueMissing via errors.Is.
	ErrValueMissing = errors.New(string(KindValueMissing))
	// ErrValueRejected matches RenderError values with Kind == KindValueRejected via errors.Is.
	ErrValueRejected = errors.New(string(KindValueRejected))
	// ErrUnknownSlot is wrapped by ParseError when Parse is called with an unregistered slot name.
	ErrUnknownSlot = errors.New("unknown template slot")
	// ErrSlotExists is returned by Register when a slot name is already registered.
	ErrSlotExists = errors.New("template slot already registered")
)

// ParseError represents an authoring-time syntax, phase, or placeholder-cap
// error in a template string. Hub handlers map this to 400 invalid_template
// {slot, offset, message}.
type ParseError struct {
	Slot    string `json:"slot"`
	Offset  int    `json:"offset"`
	Message string `json:"message"`
	cause   error
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("invalid template for slot %q at byte offset %d: %s", e.Slot, e.Offset, e.Message)
}

func (e *ParseError) Unwrap() error { return e.cause }

// UntrustedVarError indicates that a template references an untrusted variable
// path not covered by the rule's allowed untrusted-vars list. Hub handlers map
// this to 400 untrusted_var_not_allowed {slot, path}.
type UntrustedVarError struct {
	Slot string `json:"slot"`
	Path string `json:"path"`
}

func (e *UntrustedVarError) Error() string {
	return fmt.Sprintf("untrusted template variable %q is not allowed in slot %q", e.Path, e.Slot)
}

// RenderError represents a fire-time missing or rejected value. By contract it
// carries Slot and Path (when attributable to a placeholder) and never embeds
// any variable value or rendered text.
type RenderError struct {
	Kind   ErrorKind `json:"kind"`
	Slot   string    `json:"slot"`
	Path   string    `json:"path,omitempty"`
	Reason string    `json:"reason,omitempty"`
}

func (e *RenderError) Error() string {
	base := string(e.Kind)
	if e.Path != "" {
		base += fmt.Sprintf(" (slot %q, path %q)", e.Slot, e.Path)
	} else if e.Slot != "" {
		base += fmt.Sprintf(" (slot %q)", e.Slot)
	}
	if e.Reason != "" {
		base += ": " + e.Reason
	}
	return base
}

// Is allows errors.Is(err, ErrValueMissing) and errors.Is(err, ErrValueRejected).
func (e *RenderError) Is(target error) bool {
	switch target {
	case ErrValueMissing:
		return e.Kind == KindValueMissing
	case ErrValueRejected:
		return e.Kind == KindValueRejected
	default:
		return false
	}
}

// PayloadValueError is a content-free error returned by JSON payload helpers
// when a payload cannot be decoded or a looked-up path resolves to a non-scalar
// JSON value (null, object, or array).
type PayloadValueError struct {
	Reason string
}

func (e *PayloadValueError) Error() string {
	if e.Reason == "" {
		return "invalid payload value"
	}
	return e.Reason
}
