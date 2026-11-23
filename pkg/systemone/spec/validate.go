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

package spec

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	questionIDRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	optionIDRE   = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
)

// CompileOptions configures Compile.
type CompileOptions struct {
	// Kind is the consumer projection kind (filter, route, judgment_bool,
	// judgment_choice, judgments). Required.
	Kind Kind
	// Environment selects the ContentEnvironment to validate inputs against.
	// When nil, Compile defaults to ProbeEnvironment().
	Environment *ContentEnvironment
	// Inputs overrides or supplies the referenced input fields when compiling an
	// inline Spec that does not wrap an Envelope. When nil on an inline Spec
	// targeting the built-in probe environment, it defaults to ["probe.text"].
	Inputs []string
	// Params overrides or supplies trusted {param} values when compiling an
	// inline Spec without an Envelope.
	Params map[string]string
	// Caps optionally validates the portable spec and declared input sizes
	// against a specific instance's capabilities at compile time.
	Caps *CapabilityLimits
}

// ParseSpec parses a standalone JSON Spec with strict unknown-field and
// duplicate-key rejection.
func ParseSpec(raw []byte) (*Spec, error) {
	if err := checkNoDuplicateJSONKeys(raw); err != nil {
		return nil, err
	}
	if form, ok := peekFormField(raw); ok && form == FormNative {
		return nil, &CompileError{
			Path:     "spec.form",
			Category: CategoryCapability,
			Message:  "native specs are not supported in P1 (deferred to PV5)",
		}
	}
	var s Spec
	if err := decodeStrict(raw, &s); err != nil {
		return nil, wrapJSONError(err, "spec")
	}
	return &s, nil
}

// ParseEnvelope parses a consumer rule Envelope JSON with strict unknown-field
// and duplicate-key rejection.
func ParseEnvelope(raw []byte) (*Envelope, error) {
	if err := checkNoDuplicateJSONKeys(raw); err != nil {
		return nil, err
	}
	var probe struct {
		Spec json.RawMessage `json:"spec"`
	}
	if err := json.Unmarshal(raw, &probe); err == nil && len(probe.Spec) > 0 {
		if form, ok := peekFormField(probe.Spec); ok && form == FormNative {
			return nil, &CompileError{
				Path:     "spec.form",
				Category: CategoryCapability,
				Message:  "native specs are not supported in P1 (deferred to PV5)",
			}
		}
	}
	var env Envelope
	if err := decodeStrict(raw, &env); err != nil {
		return nil, wrapJSONError(err, "")
	}
	return &env, nil
}

// Compile parses either a full Envelope JSON (containing top-level "spec") or
// an inline Spec JSON, validates field references against the built-in probe
// environment (AC-3), substitutes trusted {param} values, checks the portable v1
// grammar and capability bounds, verifies the consumer Kind projection, and
// computes QuestionHash and ContentHash.
func Compile(raw []byte, opts CompileOptions) (*Compiled, error) {
	if err := checkNoDuplicateJSONKeys(raw); err != nil {
		return nil, err
	}

	isEnv, err := hasTopLevelSpecKey(raw)
	if err != nil {
		return nil, wrapJSONError(err, "")
	}

	var (
		sp         Spec
		inputs     []string
		params     map[string]string
		onOversize string
		timeout    time.Duration
	)

	contentEnv := ProbeEnvironment()
	if opts.Environment != nil {
		contentEnv = opts.Environment.Clone()
	}

	if isEnv {
		env, err := ParseEnvelope(raw)
		if err != nil {
			return nil, err
		}
		if env.Dialect != "" && env.Dialect != "system_one" {
			return nil, &CompileError{
				Path:     "dialect",
				Category: CategorySyntax,
				Message:  fmt.Sprintf("unsupported dialect %q (expected \"system_one\")", env.Dialect),
			}
		}
		sp = env.Spec
		inputs = env.Inputs
		if opts.Inputs != nil {
			inputs = opts.Inputs
		}
		params = env.Params
		if opts.Params != nil {
			params = opts.Params
		}
		onOversize = env.OnOversize
		if env.Timeout != "" {
			d, err := time.ParseDuration(env.Timeout)
			if err != nil || d <= 0 {
				return nil, &CompileError{
					Path:     "timeout",
					Category: CategorySyntax,
					Message:  fmt.Sprintf("invalid positive duration %q", env.Timeout),
				}
			}
			timeout = d
		}
	} else {
		parsed, err := ParseSpec(raw)
		if err != nil {
			return nil, err
		}
		sp = *parsed
		if opts.Inputs != nil {
			inputs = opts.Inputs
		} else if contentEnv.Name == ProbeEnvironmentName {
			inputs = []string{ProbeFieldText}
		}
		params = opts.Params
	}

	switch onOversize {
	case "", OversizeReject:
		onOversize = OversizeReject
	case OversizeTruncate:
		onOversize = OversizeTruncate
	default:
		return nil, &CompileError{
			Path:     "on_oversize",
			Category: CategorySyntax,
			Message:  fmt.Sprintf("unknown on_oversize policy %q", onOversize),
		}
	}

	// Refuse judgment_score early with CategoryCapability before field/grammar checks.
	if opts.Kind == KindJudgmentScore {
		return nil, &CompileError{
			Path:     "kind",
			Category: CategoryCapability,
			Message:  "judgment_score is not supported in P1 (score primitive deferred to PV9)",
		}
	}

	if err := ValidateInputs(inputs, contentEnv); err != nil {
		return nil, err
	}
	if err := ValidateInputTokenBudget(inputs, contentEnv, opts.Caps); err != nil {
		return nil, err
	}

	nfcParams, err := normalizeParams(params)
	if err != nil {
		return nil, err
	}

	// Check raw pre-substitution instruction bounds and capability refusals first,
	// then compute hashes from pre-substitution questions + params, then apply
	// substituted text and re-validate portable limits and projection.
	if err := ValidatePortable(&sp, opts.Caps); err != nil {
		return nil, err
	}

	qHashes := make(map[string]string, len(sp.Questions))
	hctx := HashContext{
		Params:     params,
		Inputs:     inputs,
		EnvName:    contentEnv.Name,
		EnvVersion: contentEnv.Version,
	}
	for i := range sp.Questions {
		q := &sp.Questions[i]
		qPath := fmt.Sprintf("spec.questions[%d]", i)

		h, err := QuestionHash(*q, hctx)
		if err != nil {
			return nil, err
		}
		qHashes[q.ID] = h

		subInstr, err := substituteParams(q.Instructions, nfcParams, qPath+".instructions")
		if err != nil {
			return nil, err
		}
		q.Instructions = subInstr

		if q.Criteria != nil {
			subTrue, err := substituteParams(q.Criteria.True, nfcParams, qPath+".criteria.true")
			if err != nil {
				return nil, err
			}
			subFalse, err := substituteParams(q.Criteria.False, nfcParams, qPath+".criteria.false")
			if err != nil {
				return nil, err
			}
			q.Criteria.True = subTrue
			q.Criteria.False = subFalse
		}
	}

	// Re-validate after parameter substitution so expanded byte lengths still
	// respect MaxInstructionsBytes and MaxCriteriaBytes.
	if err := ValidatePortable(&sp, opts.Caps); err != nil {
		return nil, err
	}
	if err := ValidateProjection(&sp, opts.Kind); err != nil {
		return nil, err
	}

	reqList := sp.EffectiveRequired()
	reqSet := make(map[string]struct{}, len(reqList))
	for _, id := range reqList {
		reqSet[id] = struct{}{}
	}

	contentHash, err := computeCompiledHash(&sp, opts.Kind, inputs, contentEnv, onOversize, qHashes)
	if err != nil {
		return nil, err
	}

	inCopy := append([]string(nil), inputs...)
	return &Compiled{
		Spec:           sp,
		Kind:           opts.Kind,
		Inputs:         inCopy,
		Environment:    contentEnv,
		OnOversize:     onOversize,
		Timeout:        timeout,
		Required:       reqList,
		QuestionHashes: qHashes,
		ContentHash:    contentHash,
		requiredSet:    reqSet,
	}, nil
}

// ValidatePortable validates portable v1 grammar, P1 capability gates (refusing
// score, uncertain_if, native form, and dynamic options_from with CategoryCapability),
// internal reference integrity, and optional instance CapabilityLimits.
func ValidatePortable(s *Spec, caps *CapabilityLimits) error {
	if s == nil {
		return &CompileError{
			Path:     "spec",
			Category: CategorySyntax,
			Message:  "spec must not be nil",
		}
	}

	switch s.Form {
	case "", FormPortable:
		s.Form = FormPortable
	case FormNative:
		return &CompileError{
			Path:     "spec.form",
			Category: CategoryCapability,
			Message:  "native specs are not supported in P1 (deferred to PV5)",
		}
	default:
		return &CompileError{
			Path:     "spec.form",
			Category: CategorySyntax,
			Message:  fmt.Sprintf("unknown spec form %q (expected %q)", s.Form, FormPortable),
		}
	}

	if s.Version != CurrentVersion {
		return &CompileError{
			Path:     "spec.version",
			Category: CategorySyntax,
			Message:  fmt.Sprintf("unsupported spec version %d (expected %d)", s.Version, CurrentVersion),
		}
	}

	if s.Binding.Instance != "" && s.Binding.Pool != "" {
		return &CompileError{
			Path:     "spec.binding",
			Category: CategoryBinding,
			Message:  "binding may specify instance or pool, not both",
		}
	}

	if s.UncertainIf != nil {
		return &CompileError{
			Path:     "spec.uncertain_if",
			Category: CategoryCapability,
			Message:  "uncertain_if thresholds are not supported in P1 (deferred to PV6)",
		}
	}

	if len(s.Questions) < 1 || len(s.Questions) > MaxQuestionsPerSpec {
		return &CompileError{
			Path:     "spec.questions",
			Category: CategoryLimit,
			Message:  fmt.Sprintf("questions count must be 1..%d, got %d", MaxQuestionsPerSpec, len(s.Questions)),
		}
	}

	qByID := make(map[string]*Question, len(s.Questions))
	for i := range s.Questions {
		q := &s.Questions[i]
		qPath := fmt.Sprintf("spec.questions[%d]", i)
		if err := validateQuestion(q, qPath, qByID, caps); err != nil {
			return err
		}
		qByID[q.ID] = q
	}

	if s.Required != nil {
		if len(s.Required) == 0 {
			return &CompileError{
				Path:     "spec.required",
				Category: CategorySyntax,
				Message:  "required must name at least one question when specified",
			}
		}
		seenReq := make(map[string]struct{}, len(s.Required))
		for i, id := range s.Required {
			rPath := fmt.Sprintf("spec.required[%d]", i)
			if _, ok := qByID[id]; !ok {
				return &CompileError{
					Path:     rPath,
					Category: CategorySyntax,
					Message:  fmt.Sprintf("required references unknown question %q", id),
				}
			}
			if _, dup := seenReq[id]; dup {
				return &CompileError{
					Path:     rPath,
					Category: CategorySyntax,
					Message:  fmt.Sprintf("duplicate question %q in required", id),
				}
			}
			seenReq[id] = struct{}{}
		}
		if s.Output != nil && s.Output.Question != "" {
			if _, ok := seenReq[s.Output.Question]; !ok {
				return &CompileError{
					Path:     "spec.output.question",
					Category: CategorySyntax,
					Message:  fmt.Sprintf("output question %q must be marked required", s.Output.Question),
				}
			}
		}
	}

	if s.Output != nil {
		if err := validateOutputRef(s.Output, qByID); err != nil {
			return err
		}
	}

	return nil
}

func validateQuestion(q *Question, qPath string, seen map[string]*Question, caps *CapabilityLimits) error {
	if !questionIDRE.MatchString(q.ID) {
		return &CompileError{
			Path:     qPath + ".id",
			Category: CategorySyntax,
			Message:  fmt.Sprintf("invalid question id %q (must match ^[a-z][a-z0-9_]{0,63}$)", q.ID),
		}
	}
	if _, dup := seen[q.ID]; dup {
		return &CompileError{
			Path:     qPath + ".id",
			Category: CategorySyntax,
			Message:  fmt.Sprintf("duplicate question id %q", q.ID),
		}
	}

	switch q.Type {
	case PrimitiveBinary, PrimitiveChoice:
	case PrimitiveScore:
		return &CompileError{
			Path:     qPath + ".type",
			Category: CategoryCapability,
			Message:  "score primitive is not supported in P1 (deferred to PV9)",
		}
	default:
		return &CompileError{
			Path:     qPath + ".type",
			Category: CategorySyntax,
			Message:  fmt.Sprintf("unknown question type %q", q.Type),
		}
	}

	if len(q.Levels) > 0 {
		return &CompileError{
			Path:     qPath + ".levels",
			Category: CategoryCapability,
			Message:  "score levels are not supported in P1 (deferred to PV9)",
		}
	}
	if q.OptionsFrom != "" {
		return &CompileError{
			Path:     qPath + ".options_from",
			Category: CategoryCapability,
			Message:  "dynamic choice sets (options_from) are not supported in P1 (deferred to PV8)",
		}
	}
	if len(q.ExtraOptions) > 0 {
		return &CompileError{
			Path:     qPath + ".extra_options",
			Category: CategoryCapability,
			Message:  "dynamic choice extra_options are not supported in P1 (deferred to PV8)",
		}
	}

	if len(q.Instructions) == 0 {
		return &CompileError{
			Path:     qPath + ".instructions",
			Category: CategorySyntax,
			Message:  "instructions must not be empty",
		}
	}
	if !utf8.ValidString(q.Instructions) {
		return &CompileError{
			Path:     qPath + ".instructions",
			Category: CategorySyntax,
			Message:  "instructions must be valid UTF-8",
		}
	}
	if len(q.Instructions) > MaxInstructionsBytes {
		return &CompileError{
			Path:     qPath + ".instructions",
			Category: CategoryLimit,
			Message:  fmt.Sprintf("instructions length %d exceeds %d bytes", len(q.Instructions), MaxInstructionsBytes),
		}
	}

	switch q.Type {
	case PrimitiveBinary:
		if len(q.Options) > 0 {
			return &CompileError{
				Path:     qPath + ".options",
				Category: CategorySyntax,
				Message:  "binary question must not declare options",
			}
		}
		if q.Criteria != nil {
			if err := validateBinaryCriteria(q.Criteria, qPath+".criteria"); err != nil {
				return err
			}
		}
	case PrimitiveChoice:
		if q.Criteria != nil {
			return &CompileError{
				Path:     qPath + ".criteria",
				Category: CategorySyntax,
				Message:  "choice question must declare options, not binary criteria",
			}
		}
		if err := validateChoiceOptions(q.Options, qPath+".options"); err != nil {
			return err
		}
	}

	if caps != nil {
		lim, ok := caps.Primitives[q.Type]
		if !ok || !lim.Supported {
			return &CompileError{
				Path:     qPath + ".type",
				Category: CategoryCapability,
				Message:  fmt.Sprintf("primitive %q is not supported by instance capabilities", q.Type),
			}
		}
		if q.Type == PrimitiveChoice {
			if lim.MinOptions > 0 && len(q.Options) < lim.MinOptions {
				return &CompileError{
					Path:     qPath + ".options",
					Category: CategoryLimit,
					Message:  fmt.Sprintf("choice option count %d is below instance minimum %d", len(q.Options), lim.MinOptions),
				}
			}
			if lim.MaxOptions > 0 && len(q.Options) > lim.MaxOptions {
				return &CompileError{
					Path:     qPath + ".options",
					Category: CategoryLimit,
					Message:  fmt.Sprintf("choice option count %d exceeds instance maximum %d", len(q.Options), lim.MaxOptions),
				}
			}
		}
	}

	return nil
}

func validateBinaryCriteria(c *BinaryCriteria, path string) error {
	if c.True == "" || c.False == "" {
		return &CompileError{
			Path:     path,
			Category: CategorySyntax,
			Message:  "binary criteria must specify non-empty true and false descriptions when present",
		}
	}
	if !utf8.ValidString(c.True) || !utf8.ValidString(c.False) {
		return &CompileError{
			Path:     path,
			Category: CategorySyntax,
			Message:  "binary criteria descriptions must be valid UTF-8",
		}
	}
	if len(c.True) > MaxCriteriaBytes {
		return &CompileError{
			Path:     path + ".true",
			Category: CategoryLimit,
			Message:  fmt.Sprintf("criteria.true length %d exceeds %d bytes", len(c.True), MaxCriteriaBytes),
		}
	}
	if len(c.False) > MaxCriteriaBytes {
		return &CompileError{
			Path:     path + ".false",
			Category: CategoryLimit,
			Message:  fmt.Sprintf("criteria.false length %d exceeds %d bytes", len(c.False), MaxCriteriaBytes),
		}
	}
	return nil
}

func validateChoiceOptions(opts []Option, path string) error {
	if len(opts) < MinChoiceOptions || len(opts) > MaxChoiceOptions {
		return &CompileError{
			Path:     path,
			Category: CategoryLimit,
			Message:  fmt.Sprintf("choice options count must be %d..%d, got %d", MinChoiceOptions, MaxChoiceOptions, len(opts)),
		}
	}
	seen := make(map[string]struct{}, len(opts))
	abstainCount := 0
	for i, opt := range opts {
		oPath := fmt.Sprintf("%s[%d]", path, i)
		if !optionIDRE.MatchString(opt.ID) {
			return &CompileError{
				Path:     oPath + ".id",
				Category: CategorySyntax,
				Message:  fmt.Sprintf("invalid option id %q (must match ^[a-z0-9][a-z0-9_-]{0,63}$)", opt.ID),
			}
		}
		if _, dup := seen[opt.ID]; dup {
			return &CompileError{
				Path:     oPath + ".id",
				Category: CategorySyntax,
				Message:  fmt.Sprintf("duplicate option id %q", opt.ID),
			}
		}
		seen[opt.ID] = struct{}{}
		if len(opt.Description) == 0 {
			return &CompileError{
				Path:     oPath + ".description",
				Category: CategorySyntax,
				Message:  "option description is required",
			}
		}
		if !utf8.ValidString(opt.Description) {
			return &CompileError{
				Path:     oPath + ".description",
				Category: CategorySyntax,
				Message:  "option description must be valid UTF-8",
			}
		}
		if len(opt.Description) > MaxOptionDescBytes {
			return &CompileError{
				Path:     oPath + ".description",
				Category: CategoryLimit,
				Message:  fmt.Sprintf("option description length %d exceeds %d bytes", len(opt.Description), MaxOptionDescBytes),
			}
		}
		if opt.Abstain {
			abstainCount++
			if abstainCount > 1 {
				return &CompileError{
					Path:     oPath + ".abstain",
					Category: CategorySyntax,
					Message:  "at most one option per choice question may set abstain: true",
				}
			}
		}
	}
	return nil
}

func validateOutputRef(out *Output, qByID map[string]*Question) error {
	if out.Question == "" {
		return &CompileError{
			Path:     "spec.output.question",
			Category: CategorySyntax,
			Message:  "output.question must not be empty",
		}
	}
	q, ok := qByID[out.Question]
	if !ok {
		return &CompileError{
			Path:     "spec.output.question",
			Category: CategorySyntax,
			Message:  fmt.Sprintf("output references unknown question %q", out.Question),
		}
	}
	if len(out.Match) > 0 {
		if q.Type != PrimitiveChoice {
			return &CompileError{
				Path:     "spec.output.match",
				Category: CategorySyntax,
				Message:  "output.match is only valid when output.question is a choice question",
			}
		}
		optSet := make(map[string]struct{}, len(q.Options))
		for _, opt := range q.Options {
			optSet[opt.ID] = struct{}{}
		}
		seenMatch := make(map[string]struct{}, len(out.Match))
		for i, m := range out.Match {
			mPath := fmt.Sprintf("spec.output.match[%d]", i)
			if _, ok := optSet[m]; !ok {
				return &CompileError{
					Path:     mPath,
					Category: CategorySyntax,
					Message:  fmt.Sprintf("output.match option %q is not declared on question %q", m, q.ID),
				}
			}
			if _, dup := seenMatch[m]; dup {
				return &CompileError{
					Path:     mPath,
					Category: CategorySyntax,
					Message:  fmt.Sprintf("duplicate option %q in output.match", m),
				}
			}
			seenMatch[m] = struct{}{}
		}
	}
	return nil
}

// ValidateProjection checks that s.Output matches the consumer Kind
// (provider-design §4.5).
func ValidateProjection(s *Spec, kind Kind) error {
	if s == nil {
		return &CompileError{
			Path:     "spec",
			Category: CategorySyntax,
			Message:  "spec must not be nil",
		}
	}
	if kind == KindJudgmentScore {
		return &CompileError{
			Path:     "kind",
			Category: CategoryCapability,
			Message:  "judgment_score is not supported in P1 (score primitive deferred to PV9)",
		}
	}

	qByID := make(map[string]*Question, len(s.Questions))
	for i := range s.Questions {
		qByID[s.Questions[i].ID] = &s.Questions[i]
	}

	if kind == KindJudgments {
		if s.Output != nil {
			return &CompileError{
				Path:     "spec.output",
				Category: CategorySyntax,
				Message:  "judgments specs must not declare an output projection",
			}
		}
		return nil
	}

	switch kind {
	case KindFilter, KindRoute, KindJudgmentBool, KindJudgmentChoice:
	default:
		return &CompileError{
			Path:     "kind",
			Category: CategorySyntax,
			Message:  fmt.Sprintf("unknown consumer projection kind %q", kind),
		}
	}

	if s.Output == nil {
		return &CompileError{
			Path:     "spec.output",
			Category: CategorySyntax,
			Message:  fmt.Sprintf("consumer kind %q requires spec.output", kind),
		}
	}
	if err := validateOutputRef(s.Output, qByID); err != nil {
		return err
	}
	q := qByID[s.Output.Question]

	switch kind {
	case KindFilter:
		switch q.Type {
		case PrimitiveBinary:
			if len(s.Output.Match) > 0 {
				return &CompileError{
					Path:     "spec.output.match",
					Category: CategorySyntax,
					Message:  "filter projection on a binary question must not set output.match",
				}
			}
		case PrimitiveChoice:
			if len(s.Output.Match) == 0 {
				return &CompileError{
					Path:     "spec.output.match",
					Category: CategorySyntax,
					Message:  "filter projection on a choice question requires non-empty output.match",
				}
			}
		default:
			return &CompileError{
				Path:     "spec.output.question",
				Category: CategorySyntax,
				Message:  fmt.Sprintf("filter cannot project primitive %q", q.Type),
			}
		}
	case KindRoute:
		if q.Type != PrimitiveChoice {
			return &CompileError{
				Path:     "spec.output.question",
				Category: CategorySyntax,
				Message:  fmt.Sprintf("route projection requires a choice question, got %q", q.Type),
			}
		}
		if len(s.Output.Match) > 0 {
			return &CompileError{
				Path:     "spec.output.match",
				Category: CategorySyntax,
				Message:  "route projection must not set output.match",
			}
		}
	case KindJudgmentBool:
		if q.Type != PrimitiveBinary {
			return &CompileError{
				Path:     "spec.output.question",
				Category: CategorySyntax,
				Message:  fmt.Sprintf("judgment_bool projection requires a binary question, got %q", q.Type),
			}
		}
		if len(s.Output.Match) > 0 {
			return &CompileError{
				Path:     "spec.output.match",
				Category: CategorySyntax,
				Message:  "judgment_bool projection must not set output.match",
			}
		}
	case KindJudgmentChoice:
		if q.Type != PrimitiveChoice {
			return &CompileError{
				Path:     "spec.output.question",
				Category: CategorySyntax,
				Message:  fmt.Sprintf("judgment_choice projection requires a choice question, got %q", q.Type),
			}
		}
		if len(s.Output.Match) > 0 {
			return &CompileError{
				Path:     "spec.output.match",
				Category: CategorySyntax,
				Message:  "judgment_choice projection must not set output.match",
			}
		}
	}

	return nil
}

func computeCompiledHash(s *Spec, kind Kind, inputs []string, env ContentEnvironment, onOversize string, qHashes map[string]string) (string, error) {
	payload := struct {
		Binding        Binding              `json:"binding"`
		Environment    canonicalEnvironment `json:"environment"`
		Form           string               `json:"form"`
		Inputs         []string             `json:"inputs"`
		Kind           Kind                 `json:"kind"`
		OnOversize     string               `json:"on_oversize"`
		Output         *Output              `json:"output,omitempty"`
		QuestionHashes map[string]string    `json:"question_hashes"`
		QuestionOrder  []string             `json:"question_order"`
		Required       []string             `json:"required"`
		Version        int                  `json:"version"`
	}{
		Binding: s.Binding,
		Environment: canonicalEnvironment{
			Name:    env.Name,
			Version: env.Version,
		},
		Form:           s.Form,
		Inputs:         canonicalizeStringSet(inputs),
		Kind:           kind,
		OnOversize:     onOversize,
		Output:         s.Output,
		QuestionHashes: qHashes,
		QuestionOrder:  make([]string, len(s.Questions)),
		Required:       s.EffectiveRequired(),
		Version:        s.Version,
	}
	for i, q := range s.Questions {
		payload.QuestionOrder[i] = q.ID
	}
	encoded, err := marshalCanonicalJSON(payload)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func decodeStrict(raw []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("unexpected trailing JSON content")
		}
		return err
	}
	return nil
}

func wrapJSONError(err error, defaultPath string) error {
	if ce, ok := AsCompileError(err); ok {
		return ce
	}
	return &CompileError{
		Path:     defaultPath,
		Category: CategorySyntax,
		Message:  err.Error(),
	}
}

func peekFormField(raw []byte) (string, bool) {
	var partial struct {
		Form string `json:"form"`
	}
	if err := json.Unmarshal(raw, &partial); err != nil || partial.Form == "" {
		return "", false
	}
	return partial.Form, true
}

func hasTopLevelSpecKey(raw []byte) (bool, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return false, err
	}
	if obj == nil {
		return false, errors.New("JSON document must be an object")
	}
	_, hasSpec := obj["spec"]
	return hasSpec, nil
}

// maxJSONNestingDepth bounds container nesting in ParseSpec, ParseEnvelope, and
// Compile so untrusted rule/probe bodies cannot cause deep recursion or large
// allocations before schema validation runs (portable specs and envelopes are
// at most ~6 containers deep).
const maxJSONNestingDepth = 32

type jsonPathStep struct {
	key   string
	index int // >= 0 for array element, -1 for object key
}

func formatJSONPath(steps []jsonPathStep) string {
	if len(steps) == 0 {
		return ""
	}
	var b strings.Builder
	for i, step := range steps {
		if step.index >= 0 {
			b.WriteByte('[')
			b.WriteString(strconv.Itoa(step.index))
			b.WriteByte(']')
			continue
		}
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(step.key)
	}
	return b.String()
}

// checkNoDuplicateJSONKeys walks the JSON token stream with bounded nesting
// depth (maxJSONNestingDepth) and rejects any object containing duplicate keys
// at the same nesting level. Path strings are formatted lazily only on error.
func checkNoDuplicateJSONKeys(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	var pathBuf [maxJSONNestingDepth]jsonPathStep
	if err := walkTokensNoDup(dec, 0, pathBuf[:0]); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		msg := "unexpected trailing JSON content"
		if err != nil {
			msg = err.Error()
		}
		return &CompileError{
			Path:     "",
			Category: CategorySyntax,
			Message:  msg,
		}
	}
	return nil
}

func walkTokensNoDup(dec *json.Decoder, depth int, steps []jsonPathStep) error {
	tok, err := dec.Token()
	if err != nil {
		return &CompileError{
			Path:     formatJSONPath(steps),
			Category: CategorySyntax,
			Message:  err.Error(),
		}
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	nextDepth := depth + 1
	if nextDepth > maxJSONNestingDepth {
		return &CompileError{
			Path:     formatJSONPath(steps),
			Category: CategorySyntax,
			Message:  fmt.Sprintf("JSON nesting depth exceeds maximum (%d)", maxJSONNestingDepth),
		}
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for dec.More() {
			kTok, err := dec.Token()
			if err != nil {
				return &CompileError{
					Path:     formatJSONPath(steps),
					Category: CategorySyntax,
					Message:  err.Error(),
				}
			}
			key, ok := kTok.(string)
			if !ok {
				return &CompileError{
					Path:     formatJSONPath(steps),
					Category: CategorySyntax,
					Message:  "expected object key string",
				}
			}
			childSteps := append(steps, jsonPathStep{key: key, index: -1})
			if _, dup := seen[key]; dup {
				return &CompileError{
					Path:     formatJSONPath(childSteps),
					Category: CategorySyntax,
					Message:  fmt.Sprintf("duplicate JSON object key %q", key),
				}
			}
			seen[key] = struct{}{}
			if err := walkTokensNoDup(dec, nextDepth, childSteps); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil { // closing '}'
			return &CompileError{
				Path:     formatJSONPath(steps),
				Category: CategorySyntax,
				Message:  err.Error(),
			}
		}
	case '[':
		idx := 0
		for dec.More() {
			childSteps := append(steps, jsonPathStep{index: idx})
			if err := walkTokensNoDup(dec, nextDepth, childSteps); err != nil {
				return err
			}
			idx++
		}
		if _, err := dec.Token(); err != nil { // closing ']'
			return &CompileError{
				Path:     formatJSONPath(steps),
				Category: CategorySyntax,
				Message:  err.Error(),
			}
		}
	}
	return nil
}
