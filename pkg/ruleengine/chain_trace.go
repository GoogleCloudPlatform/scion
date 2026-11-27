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

package ruleengine

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/celengine"
)

// Closed engine-owned reason tokens admitted in StepOutcome.Reason and ChainTrace.
const (
	ReasonBudgetExhausted        = "budget_exhausted"
	ReasonCompileBudgetExhausted = "compile_budget_exhausted"
	ReasonCompileError           = "compile_error"
	ReasonInvalidEnrichment      = "invalid_enrichment"
	ReasonInvalidCompletion      = "invalid_completion"
	ReasonIncompatibleGroup      = "incompatible_group"
	ReasonEvalFailed             = "eval_failed"
	ReasonUncertain              = "uncertain"
	ReasonUninitializedProgram   = "uninitialized_program"
	ReasonInvalidCompiledProgram = "invalid_compiled_program"
)

// ChainDomainName identifies the execution domain of a chain element (A1 §3.10).
type ChainDomainName string

const (
	DomainInline ChainDomainName = "inline"
	DomainIO     ChainDomainName = "io"
)

// TraceFieldKind enumerates the closed set of typed operational metadata kinds
// allowed in Capabilities.TraceFields (shared-rule-chain-design v1 §3.5).
// There is intentionally NO free-string kind and NO "bounded printable" fallback.
type TraceFieldKind string

const (
	TraceFieldEnum     TraceFieldKind = "enum"
	TraceFieldBool     TraceFieldKind = "bool"
	TraceFieldIntRange TraceFieldKind = "int_range"
	TraceFieldID       TraceFieldKind = "id"
	TraceFieldHash     TraceFieldKind = "hash"
)

// TraceField declares a single typed operational metadata field that a Dialect
// may emit in Decision.Provenance and that EncodeChainTrace will validate.
type TraceField struct {
	Name       string
	Kind       TraceFieldKind
	EnumValues []string // required for TraceFieldEnum
	Min        int64    // required for TraceFieldIntRange
	Max        int64    // required for TraceFieldIntRange
	MaxLen     int      // optional upper bound for TraceFieldID (default 128, max 256)
	HashLen    int      // required exact hex length for TraceFieldHash
}

// ChainTraceElement records the content-free, bounded evaluation outcome of a
// single chain element (v1 §3.5, A1 §3.10). Raw Decisions, judgments, and
// errors are never serialized.
type ChainTraceElement struct {
	Index      int             `json:"i"`
	Dialect    string          `json:"dialect"`
	Kind       Kind            `json:"kind,omitempty"`
	Class      StepClass       `json:"class"`
	Reason     string          `json:"reason,omitempty"`
	Verdict    string          `json:"verdict,omitempty"`
	DurationMs int64           `json:"duration_ms"`
	GroupID    string          `json:"group_id,omitempty"`
	Domain     ChainDomainName `json:"domain,omitempty"`
	Provenance map[string]any  `json:"provenance,omitempty"`
}

// ChainTrace records the content-free per-element trace, unexecuted step
// indices, and remaining tracked budget for a ChainRun (v1 §3.5, A1 §3.10).
// It is an audit/display artifact only and carries no authorization, admission,
// or reuse identity.
type ChainTrace struct {
	Elements                []ChainTraceElement `json:"elements"`
	Unexecuted              []int               `json:"unexecuted,omitempty"`
	BudgetRemainingMs       *int64              `json:"budget_remaining_ms,omitempty"`
	InlineBudgetRemainingMs *int64              `json:"inline_budget_remaining_ms,omitempty"`
	IOBudgetRemainingMs     *int64              `json:"io_budget_remaining_ms,omitempty"`
}

var engineClosedReasons = map[string]bool{
	celengine.ReasonMissingInput:           true,
	celengine.ReasonCostLimit:              true,
	celengine.ReasonTimeout:                true,
	celengine.ReasonCanceled:               true,
	celengine.ReasonRuntime:                true,
	celengine.ReasonType:                   true,
	celengine.CategorySize:                 true,
	celengine.CategorySyntax:               true,
	celengine.CategoryUnknownVariable:      true,
	celengine.CategoryUnknownFunction:      true,
	celengine.CategoryResultType:           true,
	celengine.CategoryRegexLiteralRequired: true,
	ReasonEmptyConsumer:                    true,
	ReasonConsumerNotRegistered:            true,
	ReasonDialectNotAllowed:                true,
	ReasonIONotAllowed:                     true,
	ReasonUnknownDialect:                   true,
	ReasonBudgetExhausted:                  true,
	ReasonCompileBudgetExhausted:           true,
	ReasonCompileError:                     true,
	ReasonInvalidEnrichment:                true,
	ReasonInvalidCompletion:                true,
	ReasonIncompatibleGroup:                true,
	ReasonEvalFailed:                       true,
	ReasonUncertain:                        true,
	ReasonUninitializedProgram:             true,
	ReasonInvalidCompiledProgram:           true,
}

func isAdmittedReason(reason string, caps Capabilities) bool {
	if reason == "" {
		return true
	}
	if engineClosedReasons[reason] {
		return true
	}
	return slices.Contains(caps.TraceReasons, reason)
}

func sanitizeStepReason(reason string, class StepClass, caps Capabilities) string {
	if reason == "" {
		return ""
	}
	if isAdmittedReason(reason, caps) {
		return reason
	}
	if class == StepClassUncertain {
		return ReasonUncertain
	}
	return ReasonEvalFailed
}

// ValidateTraceFieldValue checks val against a declared TraceField specification.
// It rejects any undeclared kind, wrong Go type, out-of-range integer, non-enum
// string, control characters, whitespace, or malformed ID/hash string.
func ValidateTraceFieldValue(field TraceField, val any) (any, error) {
	if field.Name == "" {
		return nil, errors.New("ruleengine: trace field has empty name")
	}
	switch field.Kind {
	case TraceFieldEnum:
		s, ok := val.(string)
		if !ok {
			return nil, fmt.Errorf("ruleengine: trace field %q expects string enum, got %T", field.Name, val)
		}
		if len(field.EnumValues) == 0 {
			return nil, fmt.Errorf("ruleengine: trace field %q declares empty enum set", field.Name)
		}
		if !slices.Contains(field.EnumValues, s) {
			return nil, fmt.Errorf("ruleengine: trace field %q value is not in declared enum", field.Name)
		}
		return s, nil

	case TraceFieldBool:
		b, ok := val.(bool)
		if !ok {
			return nil, fmt.Errorf("ruleengine: trace field %q expects bool, got %T", field.Name, val)
		}
		return b, nil

	case TraceFieldIntRange:
		if field.Min > field.Max {
			return nil, fmt.Errorf("ruleengine: trace field %q has invalid range [%d, %d]", field.Name, field.Min, field.Max)
		}
		n, ok := toInt64Strict(val)
		if !ok {
			return nil, fmt.Errorf("ruleengine: trace field %q expects integer, got %T", field.Name, val)
		}
		if n < field.Min || n > field.Max {
			return nil, fmt.Errorf("ruleengine: trace field %q value %d outside [%d, %d]", field.Name, n, field.Min, field.Max)
		}
		return n, nil

	case TraceFieldID:
		s, ok := val.(string)
		if !ok {
			return nil, fmt.Errorf("ruleengine: trace field %q expects id string, got %T", field.Name, val)
		}
		maxLen := field.MaxLen
		if maxLen <= 0 {
			maxLen = 128
		}
		if maxLen > 256 {
			maxLen = 256
		}
		if len(s) == 0 || len(s) > maxLen {
			return nil, fmt.Errorf("ruleengine: trace field %q id length %d outside [1, %d]", field.Name, len(s), maxLen)
		}
		if !isValidTraceID(s) {
			return nil, fmt.Errorf("ruleengine: trace field %q contains invalid id characters", field.Name)
		}
		return s, nil

	case TraceFieldHash:
		s, ok := val.(string)
		if !ok {
			return nil, fmt.Errorf("ruleengine: trace field %q expects hash string, got %T", field.Name, val)
		}
		if field.HashLen <= 0 || field.HashLen > 128 {
			return nil, fmt.Errorf("ruleengine: trace field %q declares invalid hash length %d", field.Name, field.HashLen)
		}
		if len(s) != field.HashLen {
			return nil, fmt.Errorf("ruleengine: trace field %q hash length %d != %d", field.Name, len(s), field.HashLen)
		}
		if !isLowerHex(s) {
			return nil, fmt.Errorf("ruleengine: trace field %q is not lowercase hex", field.Name)
		}
		return s, nil

	default:
		return nil, fmt.Errorf("ruleengine: trace field %q uses unsupported kind %q", field.Name, field.Kind)
	}
}

func toInt64Strict(v any) (int64, bool) {
	switch x := v.(type) {
	case int:
		return int64(x), true
	case int8:
		return int64(x), true
	case int16:
		return int64(x), true
	case int32:
		return int64(x), true
	case int64:
		return x, true
	case uint:
		if uint64(x) > uint64(1<<63-1) {
			return 0, false
		}
		return int64(x), true
	case uint8:
		return int64(x), true
	case uint16:
		return int64(x), true
	case uint32:
		return int64(x), true
	case uint64:
		if x > uint64(1<<63-1) {
			return 0, false
		}
		return int64(x), true
	default:
		return 0, false
	}
}

// isValidTraceID matches ^[A-Za-z0-9][A-Za-z0-9._:-]*$ (UUIDs, instance IDs,
// operational handles; rejects control chars, whitespace, quotes, slashes, etc.).
func isValidTraceID(s string) bool {
	if len(s) == 0 {
		return false
	}
	for i := 0; i < len(s); i++ {
		b := s[i]
		isAlnum := (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
		if i == 0 {
			if !isAlnum {
				return false
			}
			continue
		}
		if !isAlnum && b != '.' && b != '_' && b != ':' && b != '-' {
			return false
		}
	}
	return true
}

func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		b := s[i]
		if (b < '0' || b > '9') && (b < 'a' || b > 'f') {
			return false
		}
	}
	return true
}

// projectProvenance validates every key/value in raw against caps.TraceFields
// and returns a fresh map of validated typed values. Any unknown key or
// non-conforming value returns an error.
func projectProvenance(raw map[string]any, caps Capabilities) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	byName := make(map[string]TraceField, len(caps.TraceFields))
	for _, f := range caps.TraceFields {
		if f.Name != "" {
			byName[f.Name] = f
		}
	}
	out := make(map[string]any, len(raw))
	for k, v := range raw {
		field, ok := byName[k]
		if !ok {
			return nil, fmt.Errorf("ruleengine: undeclared trace provenance field %q", k)
		}
		validVal, err := ValidateTraceFieldValue(field, v)
		if err != nil {
			return nil, err
		}
		out[k] = validVal
	}
	return out, nil
}

// filterValidProvenance extracts only declared, valid operational provenance
// fields when recording runtime step outcomes so hostile or unprojected dialect
// fields never enter ChainTrace.
func filterValidProvenance(raw map[string]any, caps Capabilities) map[string]any {
	if len(raw) == 0 || len(caps.TraceFields) == 0 {
		return nil
	}
	byName := make(map[string]TraceField, len(caps.TraceFields))
	for _, f := range caps.TraceFields {
		if f.Name != "" {
			byName[f.Name] = f
		}
	}
	var out map[string]any
	for k, v := range raw {
		field, ok := byName[k]
		if !ok {
			continue
		}
		validVal, err := ValidateTraceFieldValue(field, v)
		if err != nil {
			continue
		}
		if out == nil {
			out = make(map[string]any)
		}
		out[k] = validVal
	}
	return out
}

// EncodeChainTrace validates every element's reason and provenance against the
// engine closed reason set and dialectCaps, then serializes trace to bounded
// content-free JSON (<= maxBytes, clamped by MaxChainTraceBytesCeiling).
//
// Undeclared reasons and undeclared or invalid provenance fields are rejected
// with a non-nil error. Deterministic truncation drops provenance earliest-first,
// then optional metadata earliest-first while keeping {i, dialect, class}.
func EncodeChainTrace(trace ChainTrace, dialectCaps map[string]Capabilities, maxBytes int) (string, error) {
	if maxBytes <= 0 || maxBytes > MaxChainTraceBytesCeiling {
		maxBytes = MaxChainTraceBytesCeiling
	}

	work := ChainTrace{
		Elements:                make([]ChainTraceElement, len(trace.Elements)),
		Unexecuted:              append([]int(nil), trace.Unexecuted...),
		BudgetRemainingMs:       cloneInt64Ptr(trace.BudgetRemainingMs),
		InlineBudgetRemainingMs: cloneInt64Ptr(trace.InlineBudgetRemainingMs),
		IOBudgetRemainingMs:     cloneInt64Ptr(trace.IOBudgetRemainingMs),
	}

	for i, el := range trace.Elements {
		if el.Domain != "" && el.Domain != DomainInline && el.Domain != DomainIO {
			return "", fmt.Errorf("ruleengine: element %d has invalid domain %q", el.Index, el.Domain)
		}
		caps := dialectCaps[strings.TrimSpace(el.Dialect)]
		if !isAdmittedReason(el.Reason, caps) {
			return "", fmt.Errorf("ruleengine: element %d has undeclared reason %q for dialect %q", el.Index, el.Reason, el.Dialect)
		}
		if len(el.Provenance) > 0 {
			proj, err := projectProvenance(el.Provenance, caps)
			if err != nil {
				return "", fmt.Errorf("ruleengine: element %d provenance rejected: %w", el.Index, err)
			}
			el.Provenance = proj
		} else {
			el.Provenance = nil
		}
		work.Elements[i] = el
	}

	if raw, err := json.Marshal(work); err == nil && len(raw) <= maxBytes {
		return string(raw), nil
	}

	// Deterministic truncation step 1: drop provenance earliest-first.
	for i := range work.Elements {
		if work.Elements[i].Provenance != nil {
			work.Elements[i].Provenance = nil
			if raw, err := json.Marshal(work); err == nil && len(raw) <= maxBytes {
				return string(raw), nil
			}
		}
	}

	// Deterministic truncation step 2: drop optional fields earliest-first while
	// preserving {i, dialect, class} for every element.
	for i := range work.Elements {
		work.Elements[i].GroupID = ""
		work.Elements[i].Verdict = ""
		work.Elements[i].Reason = ""
		work.Elements[i].Domain = ""
		work.Elements[i].Kind = ""
		if raw, err := json.Marshal(work); err == nil && len(raw) <= maxBytes {
			return string(raw), nil
		}
	}

	// Deterministic truncation step 3: drop earliest elements if maxBytes is
	// smaller than even minimal metadata for all elements.
	for len(work.Elements) > 1 {
		work.Elements = work.Elements[1:]
		if raw, err := json.Marshal(work); err == nil && len(raw) <= maxBytes {
			return string(raw), nil
		}
	}

	raw, err := json.Marshal(work)
	if err != nil {
		return "", err
	}
	if len(raw) <= maxBytes {
		return string(raw), nil
	}
	return `{"elements":[]}`, nil
}

func cloneInt64Ptr(p *int64) *int64 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func cloneProvenanceMap(m map[string]any) map[string]any {
	if len(m) == 0 {
		return nil
	}
	cp := make(map[string]any, len(m))
	maps.Copy(cp, m)
	return cp
}
