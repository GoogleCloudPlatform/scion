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

// Package spec implements parsing, portable v1 grammar validation, consumer
// projection checks, built-in probe environment validation, and canonical
// QuestionHash computation for the system_one rule dialect.
//
// This package is pure and performs no I/O (provider-design §7.1).
package spec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	// CurrentVersion is the only portable spec version accepted in v1.
	CurrentVersion = 1

	// FormPortable is the default portable rule form.
	FormPortable = "portable"
	// FormNative is the native provider-specific rule form (deferred to PV5).
	FormNative = "native"

	// PrimitiveBinary is the boolean yes/no portable question primitive.
	PrimitiveBinary = "binary"
	// PrimitiveChoice is the closed-world option selection portable primitive.
	PrimitiveChoice = "choice"
	// PrimitiveScore is the ordered-level portable primitive (deferred to PV9).
	PrimitiveScore = "score"

	// OversizeReject rejects inputs exceeding environment bounds with input_too_large.
	OversizeReject = "reject"
	// OversizeTruncate truncates oversize inputs on a UTF-8 boundary (deferred to P2).
	OversizeTruncate = "truncate"

	// CompletenessComplete means the adapter returns a probability for every declared option.
	CompletenessComplete = "complete"
	// CompletenessPartial means the adapter may omit options from the distribution.
	CompletenessPartial = "partial"
	// CompletenessNone means the adapter returns only the selected value without probabilities.
	CompletenessNone = "none"

	// Portable v1 grammar limits (provider-design §4.3).
	MaxQuestionsPerSpec  = 8
	MaxInstructionsBytes = 2048
	MaxCriteriaBytes     = 512
	MinChoiceOptions     = 2
	MaxChoiceOptions     = 64
	MaxOptionDescBytes   = 512
)

// Kind identifies the consumer projection shape expected from a compiled rule
// (provider-design §4.5).
type Kind string

const (
	KindFilter         Kind = "filter"
	KindRoute          Kind = "route"
	KindJudgmentBool   Kind = "judgment_bool"
	KindJudgmentChoice Kind = "judgment_choice"
	KindJudgmentScore  Kind = "judgment_score"
	KindJudgments      Kind = "judgments"
)

// ErrorCategory classifies structured compile-time failures (provider-design §12 AC-1,
// runtime-design §4.5).
type ErrorCategory string

const (
	CategorySyntax     ErrorCategory = "syntax"
	CategoryLimit      ErrorCategory = "limit"
	CategoryBinding    ErrorCategory = "binding"
	CategoryCapability ErrorCategory = "capability"
	CategoryInput      ErrorCategory = "input"
	CategoryThreshold  ErrorCategory = "threshold"
)

// CompileError is a structured authoring/compile error carrying the JSON path,
// error category, and human-readable message.
type CompileError struct {
	Path     string        `json:"path"`
	Category ErrorCategory `json:"category"`
	Message  string        `json:"message"`
}

func (e *CompileError) Error() string {
	if e == nil {
		return ""
	}
	if e.Path == "" {
		return fmt.Sprintf("system_one compile (%s): %s", e.Category, e.Message)
	}
	return fmt.Sprintf("system_one compile (%s) at %s: %s", e.Category, e.Path, e.Message)
}

// AsCompileError extracts a *CompileError from err via errors.As.
func AsCompileError(err error) (*CompileError, bool) {
	var ce *CompileError
	if errors.As(err, &ce) {
		return ce, true
	}
	return nil, false
}

// Envelope is the outer rule envelope stored by consumers alongside the
// dialect spec (provider-design §4.1).
type Envelope struct {
	Dialect    string            `json:"dialect,omitempty"`
	Spec       Spec              `json:"spec"`
	Inputs     []string          `json:"inputs,omitempty"`
	Params     map[string]string `json:"params,omitempty"`
	OnOversize string            `json:"on_oversize,omitempty"`
	Timeout    string            `json:"timeout,omitempty"`
}

// Spec is the system_one rule specification (provider-design §4.2).
// It contains only concrete value types (no interface/free-form fields) so
// ModelRequest satisfies credential-separation reflection invariants (AC-2).
type Spec struct {
	Version     int             `json:"version"`
	Form        string          `json:"form,omitempty"`
	Binding     Binding         `json:"binding,omitempty"`
	Questions   []Question      `json:"questions"`
	Required    []string        `json:"required,omitempty"`
	Output      *Output         `json:"output,omitempty"`
	UncertainIf []UncertainRule `json:"uncertain_if,omitempty"`
}

// UnmarshalJSON decodes a Spec with strict unknown-field rejection while
// preserving the question index on structured-instructions CompileErrors.
func (s *Spec) UnmarshalJSON(data []byte) error {
	var raw struct {
		Version     int               `json:"version"`
		Form        string            `json:"form,omitempty"`
		Binding     Binding           `json:"binding,omitempty"`
		Questions   []json.RawMessage `json:"questions"`
		Required    []string          `json:"required,omitempty"`
		Output      *Output           `json:"output,omitempty"`
		UncertainIf []UncertainRule   `json:"uncertain_if,omitempty"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return err
	}
	var questions []Question
	if raw.Questions != nil {
		questions = make([]Question, len(raw.Questions))
		for i, qRaw := range raw.Questions {
			instrPath := fmt.Sprintf("spec.questions[%d].instructions", i)
			if err := questions[i].unmarshalAt(qRaw, instrPath); err != nil {
				return err
			}
		}
	}
	s.Version = raw.Version
	s.Form = raw.Form
	s.Binding = raw.Binding
	s.Questions = questions
	s.Required = raw.Required
	s.Output = raw.Output
	s.UncertainIf = raw.UncertainIf
	return nil
}

// QuestionByID looks up a question in s.Questions by ID.
func (s *Spec) QuestionByID(id string) (*Question, bool) {
	if s == nil {
		return nil, false
	}
	for i := range s.Questions {
		if s.Questions[i].ID == id {
			return &s.Questions[i], true
		}
	}
	return nil, false
}

// EffectiveRequired returns the required question IDs in declared order. When
// s.Required is nil (omitted), all questions are required by default (runtime-design §6.1).
func (s *Spec) EffectiveRequired() []string {
	if s == nil {
		return nil
	}
	if s.Required == nil {
		out := make([]string, len(s.Questions))
		for i, q := range s.Questions {
			out[i] = q.ID
		}
		return out
	}
	reqSet := make(map[string]struct{}, len(s.Required))
	for _, id := range s.Required {
		reqSet[id] = struct{}{}
	}
	out := make([]string, 0, len(s.Required))
	for _, q := range s.Questions {
		if _, ok := reqSet[q.ID]; ok {
			out = append(out, q.ID)
		}
	}
	return out
}

// Binding pins a portable rule to a specific configured instance or pool.
type Binding struct {
	Instance string `json:"instance,omitempty"`
	Pool     string `json:"pool,omitempty"`
}

// Question is a single typed decision question inside a Spec.
type Question struct {
	ID           string          `json:"id"`
	Type         string          `json:"type"`
	Instructions string          `json:"instructions"`
	Criteria     *BinaryCriteria `json:"criteria,omitempty"`
	Options      []Option        `json:"options,omitempty"`
	OptionsFrom  string          `json:"options_from,omitempty"`
	ExtraOptions []Option        `json:"extra_options,omitempty"`
	Levels       []Level         `json:"levels,omitempty"`
}

// UnmarshalJSON decodes a Question while producing a structured syntax error if
// instructions is supplied as structured JSON rather than a plain string.
func (q *Question) UnmarshalJSON(data []byte) error {
	return q.unmarshalAt(data, "spec.questions.instructions")
}

func (q *Question) unmarshalAt(data []byte, instrPath string) error {
	var raw struct {
		ID           string          `json:"id"`
		Type         string          `json:"type"`
		Instructions json.RawMessage `json:"instructions"`
		Criteria     *BinaryCriteria `json:"criteria,omitempty"`
		Options      []Option        `json:"options,omitempty"`
		OptionsFrom  string          `json:"options_from,omitempty"`
		ExtraOptions []Option        `json:"extra_options,omitempty"`
		Levels       []Level         `json:"levels,omitempty"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return err
	}
	var instr string
	if len(raw.Instructions) > 0 && !bytes.Equal(bytes.TrimSpace(raw.Instructions), []byte("null")) {
		trimmed := bytes.TrimSpace(raw.Instructions)
		if len(trimmed) > 0 && trimmed[0] != '"' {
			return &CompileError{
				Path:     instrPath,
				Category: CategorySyntax,
				Message:  "portable instructions must be a plain text string, not structured JSON",
			}
		}
		if err := json.Unmarshal(raw.Instructions, &instr); err != nil {
			return err
		}
	}
	q.ID = raw.ID
	q.Type = raw.Type
	q.Instructions = instr
	q.Criteria = raw.Criteria
	q.Options = raw.Options
	q.OptionsFrom = raw.OptionsFrom
	q.ExtraOptions = raw.ExtraOptions
	q.Levels = raw.Levels
	return nil
}

// BinaryCriteria supplies optional true/false rubric descriptions for a binary question.
type BinaryCriteria struct {
	True  string `json:"true"`
	False string `json:"false"`
}

// Option is a closed-choice candidate option. At most one option per choice
// question may set Abstain to true (provider-design §4.3).
type Option struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Abstain     bool   `json:"abstain,omitempty"`
}

// Level is an ordered score level (parsed so score specs fail cleanly with
// CategoryCapability in P1; implemented in PV9).
type Level struct {
	ID          string `json:"id"`
	Description string `json:"description"`
}

// Output designates the question projected to a single-value consumer Kind.
type Output struct {
	Question string   `json:"question"`
	Match    []string `json:"match,omitempty"`
}

// UncertainRule represents a threshold entry (parsed so thresholded specs fail
// cleanly with CategoryCapability in P1; implemented in PV6).
type UncertainRule struct {
	Question     string            `json:"question"`
	Metric       string            `json:"metric"`
	Below        *float64          `json:"below,omitempty"`
	Above        *float64          `json:"above,omitempty"`
	ValidatedFor []ValidatedTarget `json:"validated_for,omitempty"`
}

// ValidatedTarget records calibration provenance for a threshold (PV6).
type ValidatedTarget struct {
	Adapter      string         `json:"adapter"`
	Model        ValidatedModel `json:"model"`
	Instance     string         `json:"instance,omitempty"`
	QuestionHash string         `json:"question_hash"`
	ReplayReport string         `json:"replay_report"`
	OptionCount  []int          `json:"option_count,omitempty"`
}

// ValidatedModel pins the exact or family model identifier in ValidatedTarget.
type ValidatedModel struct {
	Exact  string `json:"exact,omitempty"`
	Family string `json:"family,omitempty"`
}

// PrimitiveLimits declares per-primitive capability limits of an adapter
// instance (provider-design §5.1). Aliased by pkg/systemone/provider.
type PrimitiveLimits struct {
	Supported                bool   `json:"supported"`
	MinOptions               int    `json:"min_options,omitempty"`
	MaxOptions               int    `json:"max_options,omitempty"`
	MinLevels                int    `json:"min_levels,omitempty"`
	MaxLevels                int    `json:"max_levels,omitempty"`
	AccuracyAdvisoryOptions  int    `json:"accuracy_advisory_options,omitempty"`
	DistributionCompleteness string `json:"distribution_completeness,omitempty"`
}

// CapabilityLimits is the adapter-independent view of instance capabilities
// checked by ValidatePortable (provider-design §5.3).
type CapabilityLimits struct {
	Primitives             map[string]PrimitiveLimits
	MaxQuestionsPerRequest int
	MaxInputTokens         int
	Truncation             string
	ReturnsDistribution    bool
}

// Compiled is a validated, parameter-substituted portable specification ready
// for runtime evaluation.
type Compiled struct {
	Spec           Spec
	Kind           Kind
	Inputs         []string
	Environment    ContentEnvironment
	OnOversize     string
	Timeout        time.Duration
	Required       []string
	QuestionHashes map[string]string
	ContentHash    string
	requiredSet    map[string]struct{}
}

// IsRequired reports whether questionID is required for outcome classification.
func (c *Compiled) IsRequired(questionID string) bool {
	if c == nil {
		return false
	}
	_, ok := c.requiredSet[questionID]
	return ok
}
