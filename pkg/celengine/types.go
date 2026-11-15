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

// Package celengine provides a consumer-neutral, bounded, no-I/O CEL expression
// compiler, content-addressed program cache, and evaluator. It is the sole
// importer of cel.dev/cel-go in the repository.
package celengine

import (
	"fmt"
	"time"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
)

type typeKind uint8

const (
	typeInvalid typeKind = iota
	typeBool
	typeString
	typeInt
	typeDouble
	typeTimestamp
	typeDyn
	typeList
	typeMap
)

// Type is an engine-owned CEL type descriptor so consumers never import cel-go.
type Type struct {
	kind typeKind
	elem *Type
	key  *Type
	val  *Type
}

// Bool returns the boolean CEL type descriptor.
func Bool() Type { return Type{kind: typeBool} }

// String returns the string CEL type descriptor.
func String() Type { return Type{kind: typeString} }

// Int returns the 64-bit signed integer CEL type descriptor.
func Int() Type { return Type{kind: typeInt} }

// Double returns the 64-bit floating-point CEL type descriptor.
func Double() Type { return Type{kind: typeDouble} }

// Timestamp returns the timestamp CEL type descriptor.
func Timestamp() Type { return Type{kind: typeTimestamp} }

// Dyn returns the dynamic CEL type descriptor.
func Dyn() Type { return Type{kind: typeDyn} }

// List returns a homogeneous or dynamic list CEL type descriptor.
func List(elem Type) Type {
	e := elem
	return Type{kind: typeList, elem: &e}
}

// Map returns a map CEL type descriptor.
func Map(key, val Type) Type {
	k, v := key, val
	return Type{kind: typeMap, key: &k, val: &v}
}

// Valid reports whether t represents a well-formed Stage 1 CEL type descriptor.
func (t Type) Valid() bool {
	switch t.kind {
	case typeBool, typeString, typeInt, typeDouble, typeTimestamp, typeDyn:
		return true
	case typeList:
		return t.elem != nil && t.elem.Valid()
	case typeMap:
		if t.key == nil || t.val == nil || !t.key.Valid() || !t.val.Valid() {
			return false
		}
		// CEL map keys must be bool, int, uint, string, or dyn.
		switch t.key.kind {
		case typeBool, typeString, typeInt, typeDyn:
			return true
		default:
			return false
		}
	default:
		return false
	}
}

// Equal reports whether t and other describe the exact same type.
func (t Type) Equal(other Type) bool {
	if t.kind != other.kind {
		return false
	}
	switch t.kind {
	case typeList:
		if t.elem == nil || other.elem == nil {
			return t.elem == other.elem
		}
		return t.elem.Equal(*other.elem)
	case typeMap:
		if t.key == nil || other.key == nil || t.val == nil || other.val == nil {
			return t.key == other.key && t.val == other.val
		}
		return t.key.Equal(*other.key) && t.val.Equal(*other.val)
	default:
		return true
	}
}

// String returns the canonical string representation of t.
func (t Type) String() string {
	switch t.kind {
	case typeBool:
		return "bool"
	case typeString:
		return "string"
	case typeInt:
		return "int"
	case typeDouble:
		return "double"
	case typeTimestamp:
		return "timestamp"
	case typeDyn:
		return "dyn"
	case typeList:
		if t.elem == nil {
			return "list(<invalid>)"
		}
		return "list(" + t.elem.String() + ")"
	case typeMap:
		if t.key == nil || t.val == nil {
			return "map(<invalid>)"
		}
		return "map(" + t.key.String() + ", " + t.val.String() + ")"
	default:
		return "<invalid>"
	}
}

func (t Type) toCELType() (*cel.Type, error) {
	if !t.Valid() {
		return nil, fmt.Errorf("celengine: invalid type %s", t.String())
	}
	switch t.kind {
	case typeBool:
		return cel.BoolType, nil
	case typeString:
		return cel.StringType, nil
	case typeInt:
		return cel.IntType, nil
	case typeDouble:
		return cel.DoubleType, nil
	case typeTimestamp:
		return cel.TimestampType, nil
	case typeDyn:
		return cel.DynType, nil
	case typeList:
		elem, err := t.elem.toCELType()
		if err != nil {
			return nil, err
		}
		return cel.ListType(elem), nil
	case typeMap:
		k, err := t.key.toCELType()
		if err != nil {
			return nil, err
		}
		v, err := t.val.toCELType()
		if err != nil {
			return nil, err
		}
		return cel.MapType(k, v), nil
	default:
		return nil, fmt.Errorf("celengine: unsupported type %s", t.String())
	}
}

func (t Type) isSupportedResultKind() bool {
	return t.kind == typeBool || t.kind == typeString
}

func (t Type) matchesStaticOutput(out *types.Type) bool {
	if out == nil {
		return false
	}
	if out.Kind() == types.DynKind {
		return true
	}
	switch t.kind {
	case typeBool:
		return out.IsExactType(cel.BoolType)
	case typeString:
		return out.IsExactType(cel.StringType)
	default:
		return false
	}
}

// Variable declares a named top-level variable in a consumer's Environment.
type Variable struct {
	Name string
	Type Type
}

// Extension identifies an optional pure CEL library extension.
type Extension string

const (
	// ExtensionStrings enables cel-go's ext.Strings() library.
	ExtensionStrings Extension = "strings"
)

// MacroSet selects which CEL parser macros are enabled in an Environment.
type MacroSet string

const (
	// MacrosStandard enables standard CEL macros (has, all, exists, exists_one, map, filter).
	MacrosStandard MacroSet = "standard"
	// MacrosNone disables all CEL parser macros.
	MacrosNone MacroSet = "none"
)

// Environment is an immutable consumer declaration of variables, pure
// extensions, and macro set.
type Environment struct {
	Name       string
	Version    int
	Variables  []Variable
	Extensions []Extension
	Macros     MacroSet
}

// Kind identifies an expression slot (e.g. "filter", "subject", "admit", "route")
// and its required result type (Bool() or String() in Stage 1).
type Kind struct {
	Name   string
	Result Type
}

// Default limit values and hard upper bounds enforced by New.
//
// Structural evaluation-time bound (Elapsed <= one loop body + one guarded call,
// capped by Program.Eval's Elapsed > EvalTimeout backstop):
//  1. Bounded single host calls: outputBoundsDecorator enforces pre-call input
//     and output byte limits (MaxOutputBytes), collection cardinality limits
//     (MaxCollectionElements), fail-closed container walk bounds
//     (MaxContainerDepth = 32, 2*MaxCollectionElements, 2*MaxOutputBytes on ==,
//     !=, in, and %s format), linear KMP fallback for pathological substring
//     patterns, cached IANA timezone lookups, and RE2 program-size / match-work
//     bounds (len(input)*progSize <= MaxRegexWork; matches on very large payload
//     strings fails closed with ReasonCostLimit above MaxRegexWork).
//  2. Proportional CostTracker charging up to CostLimit: every non-O(1) host
//     call — including container == / !=, in_list / in_map, string_format (%s
//     container walks and formatted output), counted-repetition matches
//     (progSize factor), size(string), indexOf / lastIndexOf (including empty
//     needle), string/bytes ordering and conversions, and dyn-dispatched
//     multi-overload calls (overloadID == "") — charges visited container
//     elements and scanned/produced bytes to the per-evaluation cost.Tracker
//     cloned on each Program.Eval. DefaultCostLimit (250,000) bounds cumulative
//     work across all calls and loop iterations within an evaluation.
//  3. Per-iteration comprehension interruption + wall-clock backstop:
//     DefaultInterruptFrequency = 1 checks the evaluation deadline on every
//     comprehension iteration (all, exists, exists_one, filter, map), so once
//     EvalTimeout fires, execution overruns by at most the remainder of the
//     current iteration body (bounded by (1) and (2)), and Program.Eval rejects
//     any post-return Elapsed > EvalTimeout with ReasonTimeout.
const (
	DefaultMaxExprBytes                 = 4096
	DefaultRecursionDepth               = 32
	DefaultCostLimit             uint64 = 250_000
	DefaultEvalTimeout                  = 100 * time.Millisecond
	DefaultInterruptFrequency    uint   = 1
	DefaultMaxOutputBytes               = 1 << 20 // 1 MiB per string/bytes value or formatted output
	DefaultMaxCollectionElements        = 65_536
	DefaultMaxRegexProgramSize          = 256
	DefaultMaxRegexWork          int64  = 8 << 20 // 8 MiB-instructions (len(input) * regex program size)

	// MaxContainerDepth is the maximum container nesting depth traversed by
	// fail-closed container equality (==, !=), list membership (in), and %s
	// string formatting before aborting with ReasonCostLimit.
	MaxContainerDepth = 32

	MaxEvalTimeoutCeiling               = time.Second
	MaxCostLimitCeiling          uint64 = 10_000_000
	MaxOutputBytesCeiling               = 4 << 20 // 4 MiB
	MaxCollectionElementsCeiling        = 262_144
	MaxRegexProgramSizeCeiling          = 1024
	MaxRegexWorkCeiling          int64  = 8 << 20
)

// Limits bounds expression size, parser recursion, runtime cost, wall-clock time,
// per-call output / collection sizes, and RE2 program size / match work. Zero
// values for MaxOutputBytes, MaxCollectionElements, MaxRegexProgramSize, and
// MaxRegexWork default to their Default* constants in New.
type Limits struct {
	MaxExprBytes          int
	RecursionDepth        int
	CostLimit             uint64
	EvalTimeout           time.Duration
	InterruptFrequency    uint
	MaxOutputBytes        int
	MaxCollectionElements int
	MaxRegexProgramSize   int
	MaxRegexWork          int64
}

// DefaultLimits returns the Stage 1 standard engine limits.
func DefaultLimits() Limits {
	return Limits{
		MaxExprBytes:          DefaultMaxExprBytes,
		RecursionDepth:        DefaultRecursionDepth,
		CostLimit:             DefaultCostLimit,
		EvalTimeout:           DefaultEvalTimeout,
		InterruptFrequency:    DefaultInterruptFrequency,
		MaxOutputBytes:        DefaultMaxOutputBytes,
		MaxCollectionElements: DefaultMaxCollectionElements,
		MaxRegexProgramSize:   DefaultMaxRegexProgramSize,
		MaxRegexWork:          DefaultMaxRegexWork,
	}
}

func (l Limits) normalize() Limits {
	if l.MaxOutputBytes == 0 {
		l.MaxOutputBytes = DefaultMaxOutputBytes
	}
	if l.MaxCollectionElements == 0 {
		l.MaxCollectionElements = DefaultMaxCollectionElements
	}
	if l.MaxRegexProgramSize == 0 {
		l.MaxRegexProgramSize = DefaultMaxRegexProgramSize
	}
	if l.MaxRegexWork == 0 {
		l.MaxRegexWork = DefaultMaxRegexWork
	}
	return l
}

func (l Limits) validateCeiling() error {
	if l.MaxExprBytes <= 0 {
		return fmt.Errorf("celengine: MaxExprBytes must be > 0 (got %d)", l.MaxExprBytes)
	}
	if l.RecursionDepth <= 0 {
		return fmt.Errorf("celengine: RecursionDepth must be > 0 (got %d)", l.RecursionDepth)
	}
	if l.CostLimit == 0 || l.CostLimit > MaxCostLimitCeiling {
		return fmt.Errorf("celengine: CostLimit must be in 1..%d (got %d)", MaxCostLimitCeiling, l.CostLimit)
	}
	if l.EvalTimeout <= 0 || l.EvalTimeout > MaxEvalTimeoutCeiling {
		return fmt.Errorf("celengine: EvalTimeout must be in (0, %s] (got %s)", MaxEvalTimeoutCeiling, l.EvalTimeout)
	}
	if l.InterruptFrequency == 0 {
		return fmt.Errorf("celengine: InterruptFrequency must be > 0")
	}
	if l.MaxOutputBytes < 0 || l.MaxOutputBytes > MaxOutputBytesCeiling {
		return fmt.Errorf("celengine: MaxOutputBytes must be 0 (default) or 1..%d (got %d)", MaxOutputBytesCeiling, l.MaxOutputBytes)
	}
	if l.MaxCollectionElements < 0 || l.MaxCollectionElements > MaxCollectionElementsCeiling {
		return fmt.Errorf("celengine: MaxCollectionElements must be 0 (default) or 1..%d (got %d)", MaxCollectionElementsCeiling, l.MaxCollectionElements)
	}
	if l.MaxRegexProgramSize < 0 || l.MaxRegexProgramSize > MaxRegexProgramSizeCeiling {
		return fmt.Errorf("celengine: MaxRegexProgramSize must be 0 (default) or 1..%d (got %d)", MaxRegexProgramSizeCeiling, l.MaxRegexProgramSize)
	}
	if l.MaxRegexWork < 0 || l.MaxRegexWork > MaxRegexWorkCeiling {
		return fmt.Errorf("celengine: MaxRegexWork must be 0 (default) or 1..%d (got %d)", MaxRegexWorkCeiling, l.MaxRegexWork)
	}
	return nil
}

// clampEval lowers ceiling fields where override supplies a positive smaller value;
// it never raises any ceiling.
func (l Limits) clampEval(overrides ...Limits) Limits {
	eff := l
	for _, o := range overrides {
		if o.CostLimit > 0 && o.CostLimit < eff.CostLimit {
			eff.CostLimit = o.CostLimit
		}
		if o.EvalTimeout > 0 && o.EvalTimeout < eff.EvalTimeout {
			eff.EvalTimeout = o.EvalTimeout
		}
		if o.InterruptFrequency > 0 && o.InterruptFrequency < eff.InterruptFrequency {
			eff.InterruptFrequency = o.InterruptFrequency
		}
		if o.MaxOutputBytes > 0 && o.MaxOutputBytes < eff.MaxOutputBytes {
			eff.MaxOutputBytes = o.MaxOutputBytes
		}
		if o.MaxCollectionElements > 0 && o.MaxCollectionElements < eff.MaxCollectionElements {
			eff.MaxCollectionElements = o.MaxCollectionElements
		}
		if o.MaxRegexProgramSize > 0 && o.MaxRegexProgramSize < eff.MaxRegexProgramSize {
			eff.MaxRegexProgramSize = o.MaxRegexProgramSize
		}
		if o.MaxRegexWork > 0 && o.MaxRegexWork < eff.MaxRegexWork {
			eff.MaxRegexWork = o.MaxRegexWork
		}
	}
	return eff
}

// Option customizes a single compilation or Cached lookup without raising engine ceilings.
type Option func(*compileOptions)

type compileOptions struct {
	maxExprBytes int
}

// WithMaxExprBytes lowers MaxExprBytes for a specific expression kind (for
// example subject_rule <= 1024 bytes when the engine ceiling is 4096 bytes).
// Values <= 0 or above the engine ceiling are clamped to the engine ceiling.
func WithMaxExprBytes(n int) Option {
	return func(o *compileOptions) {
		if n > 0 && (o.maxExprBytes == 0 || n < o.maxExprBytes) {
			o.maxExprBytes = n
		}
	}
}

// CompileError categories returned at authoring/compile time.
const (
	CategorySize            = "size"
	CategorySyntax          = "syntax"
	CategoryUnknownVariable = "unknown_variable"
	CategoryUnknownFunction = "unknown_function"
	CategoryType            = "type"
	CategoryResultType      = "result_type"
)

// CompileError describes a structured parse, type-check, or result-type error.
type CompileError struct {
	Line     int
	Column   int
	Message  string
	Kind     string
	Category string
}

func (e *CompileError) Error() string {
	return fmt.Sprintf("cel compile error (%s, kind=%s) at %d:%d: %s", e.Category, e.Kind, e.Line, e.Column, e.Message)
}

// EvalError reasons returned when program evaluation fails.
const (
	ReasonMissingInput = "missing_input"
	ReasonCostLimit    = "cost_limit"
	ReasonTimeout      = "timeout"
	ReasonCanceled     = "canceled"
	ReasonRuntime      = "runtime"
	ReasonType         = "type"
)

// EvalError is a content-free evaluation failure classification. It never
// embeds variable values, map keys, or expression text.
type EvalError struct {
	Reason string
}

func (e *EvalError) Error() string {
	return "cel evaluation error: " + e.Reason
}

// EvalStats reports runtime cost and wall-clock duration for a single Eval call.
type EvalStats struct {
	Cost    uint64
	Elapsed time.Duration
}

// Result exposes checked accessors for the expression's declared Kind.
// Unknown, missing, and error values never produce a Result.
type Result interface {
	Kind() Kind
	Bool() (bool, error)
	String() (string, error)
}

type boolResult struct {
	kind Kind
	val  bool
}

func (r boolResult) Kind() Kind          { return r.kind }
func (r boolResult) Bool() (bool, error) { return r.val, nil }
func (r boolResult) String() (string, error) {
	return "", fmt.Errorf("celengine: result for kind %q has type bool, not string", r.kind.Name)
}

type stringResult struct {
	kind Kind
	val  string
}

func (r stringResult) Kind() Kind { return r.kind }
func (r stringResult) Bool() (bool, error) {
	return false, fmt.Errorf("celengine: result for kind %q has type string, not bool", r.kind.Name)
}
func (r stringResult) String() (string, error) { return r.val, nil }
