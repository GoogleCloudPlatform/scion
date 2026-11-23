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

// Package filterquery implements the shared key:value filter query dialect,
// AST, schema validation, canonicalization, and in-memory evaluator.
// It has no dependencies on pkg/hub, pkg/store, or cel-go.
package filterquery

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Query limits enforced by Parse and Schema.Validate.
const (
	MaxInputBytes   = 512
	MaxTerms        = 32
	MaxValuesPerKey = 20
	MaxTextRunes    = 200
)

// Error codes returned by Parse and Schema.Validate.
const (
	ErrCodeSyntax         = "syntax"
	ErrCodeValue          = "value"
	ErrCodeLimit          = "limit"
	ErrCodeUnsupportedKey = "unsupported_key"
)

// Mode distinguishes pure-text fallback mode from structured key:value mode.
type Mode int

const (
	// ModeText indicates no known key term was present; Text holds the trimmed input verbatim.
	ModeText Mode = iota
	// ModeStructured indicates at least one known key term was present.
	ModeStructured
)

// String returns the wire representation of Mode.
func (m Mode) String() string {
	switch m {
	case ModeText:
		return "text"
	case ModeStructured:
		return "structured"
	default:
		return fmt.Sprintf("mode(%d)", int(m))
	}
}

// MarshalJSON encodes Mode as "text" or "structured".
func (m Mode) MarshalJSON() ([]byte, error) {
	return json.Marshal(m.String())
}

// Query is the parsed AST of a filter query.
type Query struct {
	Mode        Mode     `json:"mode"`
	Text        string   `json:"text,omitempty"`
	Terms       []Term   `json:"terms,omitempty"`
	ExcludeText []string `json:"excludeText,omitempty"`
}

// IsEmpty reports whether the query imposes no filter constraints.
func (q Query) IsEmpty() bool {
	return q.Text == "" && len(q.Terms) == 0 && len(q.ExcludeText) == 0
}

// Term is one key:value[,value...] clause, optionally negated.
type Term struct {
	Key    string   `json:"key"`
	Values []string `json:"values"`
	Negate bool     `json:"negate"`
	Offset int      `json:"offset"`
	Length int      `json:"length"`
}

// Exec classifies where a schema key is evaluated.
type Exec int

const (
	// ExecServer means the key is translated to a store predicate on the hub
	// and can also be evaluated locally in the browser over loaded rows.
	ExecServer Exec = iota
	// ExecClient means the key is computable only in the browser and is
	// rejected if sent to the hub.
	ExecClient
)

// String returns the wire representation of Exec.
func (e Exec) String() string {
	switch e {
	case ExecServer:
		return "server"
	case ExecClient:
		return "client"
	default:
		return fmt.Sprintf("exec(%d)", int(e))
	}
}

// MarshalJSON encodes Exec as "server" or "client".
func (e Exec) MarshalJSON() ([]byte, error) {
	return json.Marshal(e.String())
}

// KeyKind classifies the value syntax and validation rules for a schema key.
type KeyKind int

const (
	KindEnum KeyKind = iota
	KindString
	KindLabel
	KindFlagSet
	KindRef
)

// String returns the wire representation of KeyKind.
func (k KeyKind) String() string {
	switch k {
	case KindEnum:
		return "enum"
	case KindString:
		return "string"
	case KindLabel:
		return "label"
	case KindFlagSet:
		return "flag_set"
	case KindRef:
		return "ref"
	default:
		return fmt.Sprintf("kind(%d)", int(k))
	}
}

// MarshalJSON encodes KeyKind as a string.
func (k KeyKind) MarshalJSON() ([]byte, error) {
	return json.Marshal(k.String())
}

// Key defines one filterable key in a view schema.
type Key struct {
	Name        string     `json:"name"`
	Help        string     `json:"help"`
	Kind        KeyKind    `json:"kind"`
	Exec        Exec       `json:"exec"`
	Enum        []string   `json:"enum,omitempty"`
	Exclusive   [][]string `json:"exclusive,omitempty"`
	MultiValued bool       `json:"multiValued"`
}

// Schema declares the filterable keys and text-search help for one view.
type Schema struct {
	View     string `json:"view"`
	Keys     []Key  `json:"keys"`
	TextHelp string `json:"textHelp"`
}

// HasKey reports whether key (case-insensitive) is declared in the schema.
func (s Schema) HasKey(key string) bool {
	_, ok := s.LookupKey(key)
	return ok
}

// LookupKey returns the Key definition matching name (case-insensitive).
func (s Schema) LookupKey(name string) (Key, bool) {
	lower := strings.ToLower(name)
	for _, k := range s.Keys {
		if strings.ToLower(k.Name) == lower {
			return k, true
		}
	}
	return Key{}, false
}

// Row supplies field values and searchable text for in-memory evaluation.
type Row interface {
	Values(key string) []string
	Text() []string
}

// Error is a structured parse or validation error carrying the offending span.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Offset  int    `json:"offset"`
	Length  int    `json:"length"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s at %d:%d: %s", e.Code, e.Offset, e.Length, e.Message)
}
