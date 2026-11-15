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

package spec_test

import (
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/systemone/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCompile_ValidBinaryAndChoiceProbeSpec(t *testing.T) {
	raw := []byte(`{
		"dialect": "system_one",
		"inputs": ["probe.text"],
		"params": {"domain": "billing"},
		"timeout": "1500ms",
		"spec": {
			"version": 1,
			"form": "portable",
			"binding": {"instance": "jev-prod"},
			"questions": [
				{
					"id": "asks_choice",
					"type": "binary",
					"instructions": "Does probe.text ask a {domain} question?",
					"criteria": {
						"true": "Asks about {domain} or invoices.",
						"false": "Does not ask."
					}
				},
				{
					"id": "topic",
					"type": "choice",
					"instructions": "Classify probe.text into a topic.",
					"options": [
						{"id": "billing", "description": "Invoices, payments, refunds"},
						{"id": "engineering-ops", "description": "Bugs, outages, code"},
						{"id": "cannot_tell", "description": "Not enough context", "abstain": true}
					]
				}
			]
		}
	}`)

	compiled, err := spec.Compile(raw, spec.CompileOptions{
		Kind: spec.KindJudgments,
	})
	require.NoError(t, err)
	require.NotNil(t, compiled)
	assert.Equal(t, spec.FormPortable, compiled.Spec.Form)
	assert.Equal(t, spec.KindJudgments, compiled.Kind)
	assert.Equal(t, []string{spec.ProbeFieldText}, compiled.Inputs)
	assert.Equal(t, 1500*time.Millisecond, compiled.Timeout)
	assert.Equal(t, spec.OversizeReject, compiled.OnOversize)
	assert.Equal(t, []string{"asks_choice", "topic"}, compiled.Required)
	assert.True(t, compiled.IsRequired("asks_choice"))
	assert.True(t, compiled.IsRequired("topic"))
	assert.Equal(t, "Does probe.text ask a billing question?", compiled.Spec.Questions[0].Instructions)
	assert.True(t, strings.HasPrefix(compiled.QuestionHashes["asks_choice"], "sha256:"))
	assert.True(t, strings.HasPrefix(compiled.QuestionHashes["topic"], "sha256:"))
	assert.True(t, strings.HasPrefix(compiled.ContentHash, "sha256:"))
}

func TestCompile_InlineProbeSpecDefaultsInput(t *testing.T) {
	raw := []byte(`{
		"version": 1,
		"questions": [
			{
				"id": "is_urgent",
				"type": "binary",
				"instructions": "Is probe.text urgent?"
			}
		],
		"output": {"question": "is_urgent"}
	}`)

	compiled, err := spec.Compile(raw, spec.CompileOptions{
		Kind: spec.KindJudgmentBool,
	})
	require.NoError(t, err)
	assert.Equal(t, []string{spec.ProbeFieldText}, compiled.Inputs)
	assert.Equal(t, spec.FormPortable, compiled.Spec.Form)
}

func TestCompile_RefusesDeferredCapabilitiesWithCategoryCapability(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		kind     spec.Kind
		wantPath string
	}{
		{
			name: "score_question_type",
			raw: `{
				"version": 1,
				"questions": [{
					"id": "urgency",
					"type": "score",
					"instructions": "Rate urgency.",
					"levels": [
						{"id": "low", "description": "Can wait"},
						{"id": "high", "description": "Now"}
					]
				}]
			}`,
			kind:     spec.KindJudgments,
			wantPath: "spec.questions[0].type",
		},
		{
			name: "uncertain_if_present",
			raw: `{
				"version": 1,
				"questions": [{
					"id": "q1",
					"type": "binary",
					"instructions": "Check probe.text."
				}],
				"uncertain_if": [{
					"question": "q1",
					"metric": "p_top",
					"below": 0.8
				}]
			}`,
			kind:     spec.KindJudgments,
			wantPath: "spec.uncertain_if",
		},
		{
			name: "uncertain_if_empty_array",
			raw: `{
				"version": 1,
				"questions": [{
					"id": "q1",
					"type": "binary",
					"instructions": "Check probe.text."
				}],
				"uncertain_if": []
			}`,
			kind:     spec.KindJudgments,
			wantPath: "spec.uncertain_if",
		},
		{
			name: "native_spec_form",
			raw: `{
				"version": 1,
				"form": "native",
				"require": {"implementation": "typesafe"},
				"native_version": 1,
				"questions": [{
					"id": "q1",
					"type": "binary",
					"instructions": "Check probe.text."
				}]
			}`,
			kind:     spec.KindJudgments,
			wantPath: "spec.form",
		},
		{
			name: "dynamic_options_from",
			raw: `{
				"version": 1,
				"questions": [{
					"id": "recipient",
					"type": "choice",
					"instructions": "Who is addressed?",
					"options_from": "$participants",
					"extra_options": [{"id": "none", "description": "Nobody"}]
				}]
			}`,
			kind:     spec.KindJudgments,
			wantPath: "spec.questions[0].options_from",
		},
		{
			name: "judgment_score_kind",
			raw: `{
				"version": 1,
				"questions": [{
					"id": "q1",
					"type": "binary",
					"instructions": "Check probe.text."
				}],
				"output": {"question": "q1"}
			}`,
			kind:     spec.KindJudgmentScore,
			wantPath: "kind",
		},
		{
			name: "truncate_oversize_policy",
			raw: `{
				"dialect": "system_one",
				"inputs": ["probe.text"],
				"on_oversize": "truncate",
				"spec": {
					"version": 1,
					"questions": [{
						"id": "q1",
						"type": "binary",
						"instructions": "Check probe.text."
					}]
				}
			}`,
			kind:     spec.KindJudgments,
			wantPath: "on_oversize",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := spec.Compile([]byte(tc.raw), spec.CompileOptions{Kind: tc.kind})
			require.Error(t, err)
			ce, ok := spec.AsCompileError(err)
			require.True(t, ok, "expected *CompileError, got %T: %v", err, err)
			assert.Equal(t, spec.CategoryCapability, ce.Category, "error: %v", ce)
			assert.Equal(t, tc.wantPath, ce.Path)
		})
	}
}

func TestCompile_ProbeEnvironmentFieldBounds_AC3(t *testing.T) {
	probeEnv := spec.ProbeEnvironment()
	assert.Equal(t, "probe", probeEnv.Name)
	assert.Equal(t, 1, probeEnv.Version)
	assert.Equal(t, 16384, probeEnv.MaxTotalBytes)
	field, ok := probeEnv.Field("probe.text")
	require.True(t, ok)
	assert.Equal(t, 16384, field.MaxBytes)
	assert.Equal(t, spec.FieldKindText, field.Kind)
	assert.Equal(t, spec.TrustUntrusted, field.Trust)

	// Referencing a field outside the built-in probe environment fails at compile (AC-3).
	badInputs := [][]string{
		{"message.body"},
		{"probe.text", "event.payload"},
		{"probe.other"},
		{""},
		{},
	}
	rawSpec := []byte(`{
		"version": 1,
		"questions": [{
			"id": "q1",
			"type": "binary",
			"instructions": "Evaluate input."
		}]
	}`)

	for _, inputs := range badInputs {
		_, err := spec.Compile(rawSpec, spec.CompileOptions{
			Kind:   spec.KindJudgments,
			Inputs: inputs,
		})
		require.Error(t, err, "inputs=%v should fail", inputs)
		ce, ok := spec.AsCompileError(err)
		require.True(t, ok)
		assert.Equal(t, spec.CategoryInput, ce.Category)
	}

	// Envelope with undeclared field also fails with CategoryInput.
	rawEnv := []byte(`{
		"dialect": "system_one",
		"inputs": ["message.body"],
		"spec": {
			"version": 1,
			"questions": [{
				"id": "q1",
				"type": "binary",
				"instructions": "Evaluate input."
			}]
		}
	}`)
	_, err := spec.Compile(rawEnv, spec.CompileOptions{Kind: spec.KindJudgments})
	require.Error(t, err)
	ce, ok := spec.AsCompileError(err)
	require.True(t, ok)
	assert.Equal(t, spec.CategoryInput, ce.Category)
}

func TestValidateProjection_AllConsumerKinds(t *testing.T) {
	baseSpec := func(out *spec.Output) *spec.Spec {
		return &spec.Spec{
			Version: 1,
			Form:    spec.FormPortable,
			Questions: []spec.Question{
				{
					ID:           "b_q",
					Type:         spec.PrimitiveBinary,
					Instructions: "Binary question?",
				},
				{
					ID:           "c_q",
					Type:         spec.PrimitiveChoice,
					Instructions: "Choice question?",
					Options: []spec.Option{
						{ID: "alpha", Description: "Option alpha"},
						{ID: "beta", Description: "Option beta"},
						{ID: "none", Description: "Neither", Abstain: true},
					},
				},
			},
			Output: out,
		}
	}

	// Valid projections:
	assert.NoError(t, spec.ValidateProjection(baseSpec(&spec.Output{Question: "b_q"}), spec.KindFilter))
	assert.NoError(t, spec.ValidateProjection(baseSpec(&spec.Output{Question: "c_q", Match: []string{"alpha"}}), spec.KindFilter))
	assert.NoError(t, spec.ValidateProjection(baseSpec(&spec.Output{Question: "c_q"}), spec.KindRoute))
	assert.NoError(t, spec.ValidateProjection(baseSpec(&spec.Output{Question: "b_q"}), spec.KindJudgmentBool))
	assert.NoError(t, spec.ValidateProjection(baseSpec(&spec.Output{Question: "c_q"}), spec.KindJudgmentChoice))
	assert.NoError(t, spec.ValidateProjection(baseSpec(nil), spec.KindJudgments))

	// Invalid projections:
	invalid := []struct {
		name string
		out  *spec.Output
		kind spec.Kind
		cat  spec.ErrorCategory
	}{
		{name: "filter_missing_output", out: nil, kind: spec.KindFilter, cat: spec.CategorySyntax},
		{name: "filter_choice_without_match", out: &spec.Output{Question: "c_q"}, kind: spec.KindFilter, cat: spec.CategorySyntax},
		{name: "filter_binary_with_match", out: &spec.Output{Question: "b_q", Match: []string{"true"}}, kind: spec.KindFilter, cat: spec.CategorySyntax},
		{name: "filter_choice_unknown_match_option", out: &spec.Output{Question: "c_q", Match: []string{"gamma"}}, kind: spec.KindFilter, cat: spec.CategorySyntax},
		{name: "route_on_binary", out: &spec.Output{Question: "b_q"}, kind: spec.KindRoute, cat: spec.CategorySyntax},
		{name: "route_with_match", out: &spec.Output{Question: "c_q", Match: []string{"alpha"}}, kind: spec.KindRoute, cat: spec.CategorySyntax},
		{name: "judgment_bool_on_choice", out: &spec.Output{Question: "c_q"}, kind: spec.KindJudgmentBool, cat: spec.CategorySyntax},
		{name: "judgment_choice_on_binary", out: &spec.Output{Question: "b_q"}, kind: spec.KindJudgmentChoice, cat: spec.CategorySyntax},
		{name: "judgments_with_output_forbidden", out: &spec.Output{Question: "b_q"}, kind: spec.KindJudgments, cat: spec.CategorySyntax},
		{name: "output_unknown_question", out: &spec.Output{Question: "missing"}, kind: spec.KindFilter, cat: spec.CategorySyntax},
		{name: "judgment_score_refused", out: &spec.Output{Question: "b_q"}, kind: spec.KindJudgmentScore, cat: spec.CategoryCapability},
	}

	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			err := spec.ValidateProjection(baseSpec(tc.out), tc.kind)
			require.Error(t, err)
			ce, ok := spec.AsCompileError(err)
			require.True(t, ok)
			assert.Equal(t, tc.cat, ce.Category)
		})
	}
}

func TestValidatePortable_GrammarAndCapabilityLimits(t *testing.T) {
	manyOpts := make([]spec.Option, 65)
	for i := range manyOpts {
		manyOpts[i] = spec.Option{ID: "opt_" + strings.Repeat("a", 1) + string(rune('a'+(i%26))) + string(rune('0'+(i/26))), Description: "desc"}
	}

	manyQuestions := make([]spec.Question, 9)
	for i := range manyQuestions {
		manyQuestions[i] = spec.Question{
			ID:           "q_" + string(rune('a'+i)),
			Type:         spec.PrimitiveBinary,
			Instructions: "ok",
		}
	}

	tests := []struct {
		name    string
		spec    spec.Spec
		caps    *spec.CapabilityLimits
		wantCat spec.ErrorCategory
	}{
		{
			name:    "wrong_version",
			spec:    spec.Spec{Version: 2, Questions: []spec.Question{{ID: "q1", Type: "binary", Instructions: "ok"}}},
			wantCat: spec.CategorySyntax,
		},
		{
			name:    "zero_questions",
			spec:    spec.Spec{Version: 1},
			wantCat: spec.CategoryLimit,
		},
		{
			name:    "too_many_questions",
			spec:    spec.Spec{Version: 1, Questions: manyQuestions},
			wantCat: spec.CategoryLimit,
		},
		{
			name: "invalid_question_id_uppercase",
			spec: spec.Spec{Version: 1, Questions: []spec.Question{
				{ID: "BadID", Type: "binary", Instructions: "ok"},
			}},
			wantCat: spec.CategorySyntax,
		},
		{
			name: "duplicate_question_id",
			spec: spec.Spec{Version: 1, Questions: []spec.Question{
				{ID: "dup", Type: "binary", Instructions: "ok"},
				{ID: "dup", Type: "binary", Instructions: "ok"},
			}},
			wantCat: spec.CategorySyntax,
		},
		{
			name: "empty_instructions",
			spec: spec.Spec{Version: 1, Questions: []spec.Question{
				{ID: "q1", Type: "binary", Instructions: ""},
			}},
			wantCat: spec.CategorySyntax,
		},
		{
			name: "instructions_over_2048_bytes",
			spec: spec.Spec{Version: 1, Questions: []spec.Question{
				{ID: "q1", Type: "binary", Instructions: strings.Repeat("x", 2049)},
			}},
			wantCat: spec.CategoryLimit,
		},
		{
			name: "binary_criteria_over_512_bytes",
			spec: spec.Spec{Version: 1, Questions: []spec.Question{
				{
					ID:           "q1",
					Type:         "binary",
					Instructions: "ok",
					Criteria:     &spec.BinaryCriteria{True: strings.Repeat("t", 513), False: "no"},
				},
			}},
			wantCat: spec.CategoryLimit,
		},
		{
			name: "choice_fewer_than_2_options",
			spec: spec.Spec{Version: 1, Questions: []spec.Question{
				{
					ID:           "q1",
					Type:         "choice",
					Instructions: "ok",
					Options:      []spec.Option{{ID: "only", Description: "one"}},
				},
			}},
			wantCat: spec.CategoryLimit,
		},
		{
			name: "choice_more_than_64_options",
			spec: spec.Spec{Version: 1, Questions: []spec.Question{
				{
					ID:           "q1",
					Type:         "choice",
					Instructions: "ok",
					Options:      manyOpts,
				},
			}},
			wantCat: spec.CategoryLimit,
		},
		{
			name: "choice_missing_option_description",
			spec: spec.Spec{Version: 1, Questions: []spec.Question{
				{
					ID:           "q1",
					Type:         "choice",
					Instructions: "ok",
					Options: []spec.Option{
						{ID: "a", Description: "ok"},
						{ID: "b", Description: ""},
					},
				},
			}},
			wantCat: spec.CategorySyntax,
		},
		{
			name: "choice_multiple_abstain_options",
			spec: spec.Spec{Version: 1, Questions: []spec.Question{
				{
					ID:           "q1",
					Type:         "choice",
					Instructions: "ok",
					Options: []spec.Option{
						{ID: "a", Description: "first", Abstain: true},
						{ID: "b", Description: "second", Abstain: true},
					},
				},
			}},
			wantCat: spec.CategorySyntax,
		},
		{
			name: "binding_both_instance_and_pool",
			spec: spec.Spec{
				Version:   1,
				Binding:   spec.Binding{Instance: "inst-1", Pool: "pool-1"},
				Questions: []spec.Question{{ID: "q1", Type: "binary", Instructions: "ok"}},
			},
			wantCat: spec.CategoryBinding,
		},
		{
			name: "instance_caps_choice_unsupported",
			spec: spec.Spec{Version: 1, Questions: []spec.Question{
				{
					ID:           "q1",
					Type:         "choice",
					Instructions: "ok",
					Options: []spec.Option{
						{ID: "a", Description: "one"},
						{ID: "b", Description: "two"},
					},
				},
			}},
			caps: &spec.CapabilityLimits{
				Primitives: map[string]spec.PrimitiveLimits{
					spec.PrimitiveBinary: {Supported: true},
				},
			},
			wantCat: spec.CategoryCapability,
		},
		{
			name: "instance_caps_max_options_exceeded",
			spec: spec.Spec{Version: 1, Questions: []spec.Question{
				{
					ID:           "q1",
					Type:         "choice",
					Instructions: "ok",
					Options: []spec.Option{
						{ID: "a", Description: "one"},
						{ID: "b", Description: "two"},
						{ID: "c", Description: "three"},
					},
				},
			}},
			caps: &spec.CapabilityLimits{
				Primitives: map[string]spec.PrimitiveLimits{
					spec.PrimitiveChoice: {Supported: true, MinOptions: 2, MaxOptions: 2},
				},
			},
			wantCat: spec.CategoryLimit,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.spec
			err := spec.ValidatePortable(&s, tc.caps)
			require.Error(t, err)
			ce, ok := spec.AsCompileError(err)
			require.True(t, ok, "expected *CompileError, got %v", err)
			assert.Equal(t, tc.wantCat, ce.Category, "error: %v", ce)
		})
	}
}

func TestParseSpec_RejectsStructuredInstructionsAndDuplicateKeys(t *testing.T) {
	structured := []byte(`{
		"version": 1,
		"questions": [
			{
				"id": "q1",
				"type": "binary",
				"instructions": "plain string ok"
			},
			{
				"id": "q2",
				"type": "binary",
				"instructions": {"task": "not allowed in portable"}
			}
		]
	}`)
	_, err := spec.ParseSpec(structured)
	require.Error(t, err)
	ce, ok := spec.AsCompileError(err)
	require.True(t, ok)
	assert.Equal(t, spec.CategorySyntax, ce.Category)
	assert.Equal(t, "spec.questions[1].instructions", ce.Path)

	envelopeStructured := []byte(`{
		"dialect": "system_one",
		"spec": {
			"version": 1,
			"questions": [{
				"id": "q1",
				"type": "binary",
				"instructions": ["not", "plain", "string"]
			}]
		}
	}`)
	_, err = spec.Compile(envelopeStructured, spec.CompileOptions{Kind: spec.KindJudgments})
	require.Error(t, err)
	ce, ok = spec.AsCompileError(err)
	require.True(t, ok)
	assert.Equal(t, spec.CategorySyntax, ce.Category)
	assert.Equal(t, "spec.questions[0].instructions", ce.Path)

	dupKey := []byte(`{
		"version": 1,
		"version": 1,
		"questions": [{
			"id": "q1",
			"type": "binary",
			"instructions": "ok"
		}]
	}`)
	_, err = spec.ParseSpec(dupKey)
	require.Error(t, err)
	ce, ok = spec.AsCompileError(err)
	require.True(t, ok)
	assert.Equal(t, spec.CategorySyntax, ce.Category)
	assert.Equal(t, "version", ce.Path)

	nestedDupKey := []byte(`{
		"version": 1,
		"questions": [{
			"id": "q1",
			"id": "q1",
			"type": "binary",
			"instructions": "ok"
		}]
	}`)
	_, err = spec.ParseSpec(nestedDupKey)
	require.Error(t, err)
	ce, ok = spec.AsCompileError(err)
	require.True(t, ok)
	assert.Equal(t, spec.CategorySyntax, ce.Category)
	assert.Equal(t, "questions[0].id", ce.Path)
}

func TestCompile_RejectsDeepNestingWithBoundedMemory(t *testing.T) {
	// Depth 32 passes the nesting-depth guard (and fails only on unknown field "a").
	depth32 := []byte(strings.Repeat(`{"a":`, 32) + `1` + strings.Repeat(`}`, 32))
	_, err := spec.ParseSpec(depth32)
	require.Error(t, err)
	ce, ok := spec.AsCompileError(err)
	require.True(t, ok)
	assert.Equal(t, spec.CategorySyntax, ce.Category)
	assert.NotContains(t, ce.Message, "nesting depth")

	// Depth 33 (> 32) is rejected immediately by the nesting-depth guard.
	depth33 := []byte(strings.Repeat(`{"a":`, 33) + `1` + strings.Repeat(`}`, 33))
	_, err = spec.ParseSpec(depth33)
	require.Error(t, err)
	ce, ok = spec.AsCompileError(err)
	require.True(t, ok)
	assert.Equal(t, spec.CategorySyntax, ce.Category)
	assert.Contains(t, ce.Message, "nesting depth")

	// Regression test for R1: depth >= 10,000 (20,000 nested brackets, ~40 KB)
	// previously allocated ~620 MB via quadratic path strings. It must fail fast
	// with CategorySyntax and negligible memory allocation across all entry points.
	const deep = 20_000
	deepArrays := []byte(`{"version":1,"questions":` + strings.Repeat("[", deep) + strings.Repeat("]", deep) + `}`)
	deepObjects := []byte(strings.Repeat(`{"a":`, 12_000) + `1` + strings.Repeat(`}`, 12_000))

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()

	for _, entry := range []struct {
		name string
		call func([]byte) error
	}{
		{
			name: "Compile_deep_arrays",
			call: func(b []byte) error {
				_, err := spec.Compile(b, spec.CompileOptions{Kind: spec.KindJudgments})
				return err
			},
		},
		{
			name: "ParseSpec_deep_arrays",
			call: func(b []byte) error {
				_, err := spec.ParseSpec(b)
				return err
			},
		},
		{
			name: "ParseEnvelope_deep_objects",
			call: func(b []byte) error {
				_, err := spec.ParseEnvelope(b)
				return err
			},
		},
	} {
		payload := deepArrays
		if strings.Contains(entry.name, "objects") {
			payload = deepObjects
		}
		err := entry.call(payload)
		require.Error(t, err, entry.name)
		ce, ok := spec.AsCompileError(err)
		require.True(t, ok, "%s: expected *CompileError, got %v", entry.name, err)
		assert.Equal(t, spec.CategorySyntax, ce.Category, entry.name)
		assert.Contains(t, ce.Message, "nesting depth", entry.name)
	}

	elapsed := time.Since(start)
	runtime.ReadMemStats(&after)
	totalAllocDelta := after.TotalAlloc - before.TotalAlloc

	assert.Less(t, elapsed, 200*time.Millisecond, "deep nesting rejection should be sub-millisecond, took %v", elapsed)
	assert.Less(t, totalAllocDelta, uint64(1<<20), "deep nesting rejection allocated %d bytes (expected < 1 MiB)", totalAllocDelta)
}
