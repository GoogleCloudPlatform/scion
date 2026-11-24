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
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/systemone/spec"
)

func sampleMessageEnv() spec.ContentEnvironment {
	return spec.ContentEnvironment{
		Name:    "test_message_env",
		Version: 1,
		Fields: []spec.FieldDecl{
			{Name: "message.body", Kind: spec.FieldKindText, MaxBytes: 16384, Trust: spec.TrustUntrusted},
			{Name: "message.type", Kind: spec.FieldKindText, MaxBytes: 64, Trust: spec.TrustUntrusted},
			{Name: "sender.slug", Kind: spec.FieldKindText, MaxBytes: 128, Trust: spec.TrustTrusted},
		},
		ChoiceSets: []spec.ChoiceSetDecl{
			{Name: "unmentioned_agents", MaxItems: 32, MaxLabelBytes: 128},
		},
		MaxTotalBytes: 24 * 1024,
	}
}

func sampleTriggerEnv() spec.ContentEnvironment {
	return spec.ContentEnvironment{
		Name:    "test_trigger_env",
		Version: 1,
		Fields: []spec.FieldDecl{
			{Name: "event.type", Kind: spec.FieldKindText, MaxBytes: 128, Trust: spec.TrustTrusted},
			{Name: "event.source_kind", Kind: spec.FieldKindText, MaxBytes: 32, Trust: spec.TrustTrusted},
			{Name: "event.subject", Kind: spec.FieldKindText, MaxBytes: 256, Trust: spec.TrustUntrusted},
			{Name: "event.attributes", Kind: spec.FieldKindJSON, MaxBytes: 8192, Trust: spec.TrustUntrusted},
			{Name: "event.payload", Kind: spec.FieldKindJSON, MaxBytes: 24576, Trust: spec.TrustUntrusted},
		},
		MaxTotalBytes: 32 * 1024, // 32768 < sum(33184); each individual field <= 32768
	}
}

func TestContentEnvironment_Validate(t *testing.T) {
	require.NoError(t, spec.ProbeEnvironment().Validate())
	require.NoError(t, sampleMessageEnv().Validate())
	require.NoError(t, sampleTriggerEnv().Validate())

	badCases := []struct {
		name     string
		env      spec.ContentEnvironment
		wantCat  spec.ErrorCategory
		wantPath string
	}{
		{
			name:     "empty_name",
			env:      spec.ContentEnvironment{Version: 1, Fields: []spec.FieldDecl{{Name: "a", Kind: spec.FieldKindText, MaxBytes: 10, Trust: spec.TrustTrusted}}, MaxTotalBytes: 10},
			wantCat:  spec.CategoryInput,
			wantPath: "environment.name",
		},
		{
			name:     "uppercase_name",
			env:      spec.ContentEnvironment{Name: "BadEnv", Version: 1, Fields: []spec.FieldDecl{{Name: "a", Kind: spec.FieldKindText, MaxBytes: 10, Trust: spec.TrustTrusted}}, MaxTotalBytes: 10},
			wantCat:  spec.CategoryInput,
			wantPath: "environment.name",
		},
		{
			name:     "zero_version",
			env:      spec.ContentEnvironment{Name: "ok_env", Version: 0, Fields: []spec.FieldDecl{{Name: "a", Kind: spec.FieldKindText, MaxBytes: 10, Trust: spec.TrustTrusted}}, MaxTotalBytes: 10},
			wantCat:  spec.CategoryInput,
			wantPath: "environment.version",
		},
		{
			name:     "non_positive_max_total_bytes",
			env:      spec.ContentEnvironment{Name: "ok_env", Version: 1, Fields: []spec.FieldDecl{{Name: "a", Kind: spec.FieldKindText, MaxBytes: 10, Trust: spec.TrustTrusted}}, MaxTotalBytes: 0},
			wantCat:  spec.CategoryLimit,
			wantPath: "environment.max_total_bytes",
		},
		{
			name:     "no_fields",
			env:      spec.ContentEnvironment{Name: "ok_env", Version: 1, MaxTotalBytes: 100},
			wantCat:  spec.CategoryInput,
			wantPath: "environment.fields",
		},
		{
			name: "duplicate_field_name",
			env: spec.ContentEnvironment{
				Name:    "ok_env",
				Version: 1,
				Fields: []spec.FieldDecl{
					{Name: "msg.body", Kind: spec.FieldKindText, MaxBytes: 100, Trust: spec.TrustUntrusted},
					{Name: "msg.body", Kind: spec.FieldKindText, MaxBytes: 100, Trust: spec.TrustUntrusted},
				},
				MaxTotalBytes: 200,
			},
			wantCat:  spec.CategoryInput,
			wantPath: "environment.fields[1].name",
		},
		{
			name: "invalid_field_kind",
			env: spec.ContentEnvironment{
				Name:          "ok_env",
				Version:       1,
				Fields:        []spec.FieldDecl{{Name: "msg.img", Kind: "image", MaxBytes: 100, Trust: spec.TrustUntrusted}},
				MaxTotalBytes: 200,
			},
			wantCat:  spec.CategoryInput,
			wantPath: "environment.fields[0].kind",
		},
		{
			name: "zero_field_max_bytes",
			env: spec.ContentEnvironment{
				Name:          "ok_env",
				Version:       1,
				Fields:        []spec.FieldDecl{{Name: "msg.body", Kind: spec.FieldKindText, MaxBytes: 0, Trust: spec.TrustUntrusted}},
				MaxTotalBytes: 200,
			},
			wantCat:  spec.CategoryLimit,
			wantPath: "environment.fields[0].max_bytes",
		},
		{
			name: "field_max_bytes_exceeds_max_total_bytes",
			env: spec.ContentEnvironment{
				Name:          "ok_env",
				Version:       1,
				Fields:        []spec.FieldDecl{{Name: "msg.body", Kind: spec.FieldKindText, MaxBytes: 500, Trust: spec.TrustUntrusted}},
				MaxTotalBytes: 200,
			},
			wantCat:  spec.CategoryLimit,
			wantPath: "environment.fields[0].max_bytes",
		},
		{
			name: "invalid_trust_label",
			env: spec.ContentEnvironment{
				Name:          "ok_env",
				Version:       1,
				Fields:        []spec.FieldDecl{{Name: "msg.body", Kind: spec.FieldKindText, MaxBytes: 100, Trust: "semi"}},
				MaxTotalBytes: 200,
			},
			wantCat:  spec.CategoryInput,
			wantPath: "environment.fields[0].trust",
		},
		{
			name: "duplicate_choice_set_name",
			env: spec.ContentEnvironment{
				Name:    "ok_env",
				Version: 1,
				Fields:  []spec.FieldDecl{{Name: "msg.body", Kind: spec.FieldKindText, MaxBytes: 100, Trust: spec.TrustUntrusted}},
				ChoiceSets: []spec.ChoiceSetDecl{
					{Name: "targets", MaxItems: 10, MaxLabelBytes: 32},
					{Name: "targets", MaxItems: 5, MaxLabelBytes: 32},
				},
				MaxTotalBytes: 200,
			},
			wantCat:  spec.CategoryInput,
			wantPath: "environment.choice_sets[1].name",
		},
		{
			name: "choice_set_zero_max_items",
			env: spec.ContentEnvironment{
				Name:          "ok_env",
				Version:       1,
				Fields:        []spec.FieldDecl{{Name: "msg.body", Kind: spec.FieldKindText, MaxBytes: 100, Trust: spec.TrustUntrusted}},
				ChoiceSets:    []spec.ChoiceSetDecl{{Name: "targets", MaxItems: 0, MaxLabelBytes: 32}},
				MaxTotalBytes: 200,
			},
			wantCat:  spec.CategoryLimit,
			wantPath: "environment.choice_sets[0].max_items",
		},
		{
			name: "choice_set_zero_max_label_bytes",
			env: spec.ContentEnvironment{
				Name:          "ok_env",
				Version:       1,
				Fields:        []spec.FieldDecl{{Name: "msg.body", Kind: spec.FieldKindText, MaxBytes: 100, Trust: spec.TrustUntrusted}},
				ChoiceSets:    []spec.ChoiceSetDecl{{Name: "targets", MaxItems: 10, MaxLabelBytes: 0}},
				MaxTotalBytes: 200,
			},
			wantCat:  spec.CategoryLimit,
			wantPath: "environment.choice_sets[0].max_label_bytes",
		},
		{
			name: "choice_set_label_exceeds_max_total_bytes",
			env: spec.ContentEnvironment{
				Name:          "ok_env",
				Version:       1,
				Fields:        []spec.FieldDecl{{Name: "msg.body", Kind: spec.FieldKindText, MaxBytes: 100, Trust: spec.TrustUntrusted}},
				ChoiceSets:    []spec.ChoiceSetDecl{{Name: "targets", MaxItems: 10, MaxLabelBytes: 300}},
				MaxTotalBytes: 200,
			},
			wantCat:  spec.CategoryLimit,
			wantPath: "environment.choice_sets[0].max_label_bytes",
		},
	}

	for _, tc := range badCases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.env.Validate()
			require.Error(t, err)
			ce, ok := spec.AsCompileError(err)
			require.True(t, ok, "expected *CompileError, got %T: %v", err, err)
			assert.Equal(t, tc.wantCat, ce.Category)
			assert.Equal(t, tc.wantPath, ce.Path)
		})
	}
}

func TestEnvironmentRegistry_ValidationAndConcurrency(t *testing.T) {
	reg := spec.NewEnvironmentRegistry()

	// Built-in probe environment is pre-registered and cannot be overwritten.
	probe, ok := reg.Get(spec.ProbeEnvironmentName)
	require.True(t, ok)
	assert.Equal(t, spec.ProbeEnvironment(), probe)
	require.Error(t, reg.Register(spec.ProbeEnvironment()), "duplicate probe registration must fail")

	// Register custom environment without ChoiceSets and verify defensive copies.
	msgEnv := sampleMessageEnv()
	msgEnv.ChoiceSets = nil
	require.NoError(t, reg.Register(msgEnv))
	msgEnv.Fields[0].MaxBytes = 1 // mutate caller copy

	got, ok := reg.Get("test_message_env")
	require.True(t, ok)
	assert.Equal(t, 16384, got.Fields[0].MaxBytes, "registry must store a defensive copy")

	got.Fields[0].MaxBytes = 2
	gotAgain, ok := reg.Get("test_message_env")
	require.True(t, ok)
	assert.Equal(t, 16384, gotAgain.Fields[0].MaxBytes, "Get must return a defensive copy")

	// Duplicate registration fails.
	dupEnv := sampleMessageEnv()
	dupEnv.ChoiceSets = nil
	require.Error(t, reg.Register(dupEnv))

	// Environments declaring ChoiceSets are refused fail-closed in both
	// Register and RegisterForConsumer (probe remains unaffected).
	csEnv := sampleMessageEnv()
	csEnv.Name = "env_with_choicesets"
	err := reg.Register(csEnv)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dynamic choice sets are not yet supported")

	err = reg.RegisterForConsumer("test_consumer", csEnv)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dynamic choice sets are not yet supported")

	// Concurrent Register, Get, and Names must be race-free.
	var wg sync.WaitGroup
	for i := range 24 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			name := fmt.Sprintf("conc_env_%d", idx)
			env := spec.ContentEnvironment{
				Name:    name,
				Version: 1,
				Fields: []spec.FieldDecl{
					{Name: "f.text", Kind: spec.FieldKindText, MaxBytes: 256, Trust: spec.TrustUntrusted},
				},
				MaxTotalBytes: 256,
			}
			assert.NoError(t, reg.Register(env))
			loaded, found := reg.Get(name)
			assert.True(t, found)
			assert.Equal(t, name, loaded.Name)
			_ = reg.Names()
		}(i)
	}
	wg.Wait()

	names := reg.Names()
	assert.Contains(t, names, spec.ProbeEnvironmentName)
	assert.Contains(t, names, "test_message_env")
	assert.Len(t, names, 26)
}

func TestCompile_ContentEnvironmentAndTokenBudgetMatrix(t *testing.T) {
	msgEnv := sampleMessageEnv()
	trigEnv := sampleTriggerEnv()

	envelopeRule := func(inputsJSON, oversize string) []byte {
		ov := ""
		if oversize != "" {
			ov = fmt.Sprintf(`"on_oversize": %q,`, oversize)
		}
		return []byte(fmt.Sprintf(`{
			"dialect": "system_one",
			"inputs": %s,
			%s
			"spec": {
				"version": 1,
				"questions": [{
					"id": "q1",
					"type": "binary",
					"instructions": "Evaluate input."
				}],
				"output": {"question": "q1"}
			}
		}`, inputsJSON, ov))
	}

	t.Run("custom_env_accepts_declared_fields_and_on_oversize_truncate", func(t *testing.T) {
		cReject, err := spec.Compile(envelopeRule(`["message.body", "sender.slug"]`, ""), spec.CompileOptions{
			Kind:        spec.KindFilter,
			Environment: &msgEnv,
		})
		require.NoError(t, err)
		assert.Equal(t, spec.OversizeReject, cReject.OnOversize)
		assert.Equal(t, []string{"message.body", "sender.slug"}, cReject.Inputs)
		assert.Equal(t, "test_message_env", cReject.Environment.Name)

		cTrunc, err := spec.Compile(envelopeRule(`["message.body", "sender.slug"]`, spec.OversizeTruncate), spec.CompileOptions{
			Kind:        spec.KindFilter,
			Environment: &msgEnv,
		})
		require.NoError(t, err)
		assert.Equal(t, spec.OversizeTruncate, cTrunc.OnOversize)
		assert.NotEqual(t, cReject.ContentHash, cTrunc.ContentHash, "on_oversize must affect ContentHash")
	})

	t.Run("custom_env_rejects_undeclared_and_empty_inputs", func(t *testing.T) {
		for _, badInputs := range []string{`["probe.text"]`, `["message.body", "secret.key"]`, `[]`} {
			_, err := spec.Compile(envelopeRule(badInputs, ""), spec.CompileOptions{
				Kind:        spec.KindFilter,
				Environment: &msgEnv,
			})
			require.Error(t, err, "inputs=%s must be rejected", badInputs)
			ce, ok := spec.AsCompileError(err)
			require.True(t, ok)
			assert.Equal(t, spec.CategoryInput, ce.Category)
		}

		// Inline spec on non-probe environment without explicit Inputs fails closed (no implicit probe.text).
		inline := []byte(`{
			"version": 1,
			"questions": [{"id": "q1", "type": "binary", "instructions": "Check."}],
			"output": {"question": "q1"}
		}`)
		_, err := spec.Compile(inline, spec.CompileOptions{
			Kind:        spec.KindFilter,
			Environment: &msgEnv,
		})
		require.Error(t, err)
		ce, ok := spec.AsCompileError(err)
		require.True(t, ok)
		assert.Equal(t, spec.CategoryInput, ce.Category)
	})

	t.Run("max_input_tokens_precheck_reported_vs_silent", func(t *testing.T) {
		// message.body MaxBytes = 16384.
		baseCaps := func(maxTokens int, trunc string) *spec.CapabilityLimits {
			return &spec.CapabilityLimits{
				Primitives: map[string]spec.PrimitiveLimits{
					spec.PrimitiveBinary: {Supported: true, DistributionCompleteness: spec.CompletenessComplete},
				},
				MaxQuestionsPerRequest: 8,
				MaxInputTokens:         maxTokens,
				Truncation:             trunc,
				ReturnsDistribution:    true,
			}
		}

		// Reported: 1024 tokens * 16 = 16384 -> fits; 1023 tokens * 16 = 16368 < 16384 -> rejected.
		_, err := spec.Compile(envelopeRule(`["message.body"]`, ""), spec.CompileOptions{
			Kind:        spec.KindFilter,
			Environment: &msgEnv,
			Caps:        baseCaps(1024, "reported"),
		})
		require.NoError(t, err)

		_, err = spec.Compile(envelopeRule(`["message.body"]`, ""), spec.CompileOptions{
			Kind:        spec.KindFilter,
			Environment: &msgEnv,
			Caps:        baseCaps(1023, "reported"),
		})
		require.Error(t, err)
		ce, ok := spec.AsCompileError(err)
		require.True(t, ok)
		assert.Equal(t, spec.CategoryLimit, ce.Category)
		assert.Equal(t, "inputs", ce.Path)

		// Silent: 16384 tokens * 1 = 16384 -> fits; 16383 -> rejected; 0 -> rejected.
		_, err = spec.Compile(envelopeRule(`["message.body"]`, ""), spec.CompileOptions{
			Kind:        spec.KindFilter,
			Environment: &msgEnv,
			Caps:        baseCaps(16384, spec.TruncationSilent),
		})
		require.NoError(t, err)

		for _, tok := range []int{16383, 0} {
			_, err = spec.Compile(envelopeRule(`["message.body"]`, ""), spec.CompileOptions{
				Kind:        spec.KindFilter,
				Environment: &msgEnv,
				Caps:        baseCaps(tok, spec.TruncationSilent),
			})
			require.Error(t, err, "silent tokens=%d must fail", tok)
			ce, ok := spec.AsCompileError(err)
			require.True(t, ok)
			assert.Equal(t, spec.CategoryLimit, ce.Category)
		}

		// Trigger env: sum of all fields is 33184, capped by MaxTotalBytes = 32768 (2048 * 16).
		assert.Equal(t, 32768, spec.MaxDeclaredInputBytes(
			[]string{"event.type", "event.source_kind", "event.subject", "event.attributes", "event.payload"},
			trigEnv,
		))
		_, err = spec.Compile(
			envelopeRule(`["event.type", "event.source_kind", "event.subject", "event.attributes", "event.payload"]`, ""),
			spec.CompileOptions{
				Kind:        spec.KindFilter,
				Environment: &trigEnv,
				Caps:        baseCaps(2048, "never"),
			},
		)
		require.NoError(t, err, "32768 <= 2048*16 must pass because MaxTotalBytes caps sum")
	})

	t.Run("choice_option_limits_against_instance_caps", func(t *testing.T) {
		choiceRule := []byte(`{
			"dialect": "system_one",
			"inputs": ["message.body"],
			"spec": {
				"version": 1,
				"questions": [{
					"id": "route_q",
					"type": "choice",
					"instructions": "Pick one.",
					"options": [
						{"id": "a", "description": "A"},
						{"id": "b", "description": "B"},
						{"id": "c", "description": "C"},
						{"id": "d", "description": "D"}
					]
				}],
				"output": {"question": "route_q"}
			}
		}`)
		capsTight := &spec.CapabilityLimits{
			Primitives: map[string]spec.PrimitiveLimits{
				spec.PrimitiveChoice: {Supported: true, MinOptions: 2, MaxOptions: 3, DistributionCompleteness: spec.CompletenessComplete},
			},
			MaxInputTokens: 8192,
			Truncation:     "reported",
		}
		_, err := spec.Compile(choiceRule, spec.CompileOptions{
			Kind:        spec.KindRoute,
			Environment: &msgEnv,
			Caps:        capsTight,
		})
		require.Error(t, err)
		ce, ok := spec.AsCompileError(err)
		require.True(t, ok)
		assert.Equal(t, spec.CategoryLimit, ce.Category)
		assert.Equal(t, "spec.questions[0].options", ce.Path)
	})

	t.Run("truncate_refused_for_json_fields", func(t *testing.T) {
		jsonEnv := spec.ContentEnvironment{
			Name:    "json_trunc_test",
			Version: 1,
			Fields: []spec.FieldDecl{
				{Name: "event.type", Kind: spec.FieldKindText, MaxBytes: 128, Trust: spec.TrustTrusted},
				{Name: "event.payload", Kind: spec.FieldKindJSON, MaxBytes: 1024, Trust: spec.TrustUntrusted},
			},
			MaxTotalBytes: 2048,
		}
		// Text-only field with truncate succeeds.
		_, err := spec.Compile(
			envelopeRule(`["event.type"]`, "truncate"),
			spec.CompileOptions{Kind: spec.KindFilter, Environment: &jsonEnv},
		)
		require.NoError(t, err)

		// Referencing a JSON field with truncate is refused at compile (O4).
		_, err = spec.Compile(
			envelopeRule(`["event.type", "event.payload"]`, "truncate"),
			spec.CompileOptions{Kind: spec.KindFilter, Environment: &jsonEnv},
		)
		require.Error(t, err)
		ce, ok := spec.AsCompileError(err)
		require.True(t, ok)
		assert.Equal(t, spec.CategoryInput, ce.Category)
		assert.Equal(t, "on_oversize", ce.Path)
	})
}
