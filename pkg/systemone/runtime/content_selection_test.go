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

package runtime_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/systemone/egress"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider/jevwire"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/spec"
)

func testConsumerEnv() spec.ContentEnvironment {
	return spec.ContentEnvironment{
		Name:    "test_eval_env",
		Version: 1,
		Fields: []spec.FieldDecl{
			{Name: "doc.title", Kind: spec.FieldKindText, MaxBytes: 16, Trust: spec.TrustTrusted},
			{Name: "doc.body", Kind: spec.FieldKindText, MaxBytes: 16, Trust: spec.TrustUntrusted},
			{Name: "doc.meta", Kind: spec.FieldKindJSON, MaxBytes: 24, Trust: spec.TrustUntrusted},
		},
		ChoiceSets: []spec.ChoiceSetDecl{
			{Name: "candidates", MaxItems: 5, MaxLabelBytes: 12},
		},
		MaxTotalBytes: 24,
	}
}

func TestRuntime_OnlyReferencedFieldsReachRecordingProvider(t *testing.T) {
	env := spec.ContentEnvironment{
		Name:    "test_recording_env",
		Version: 1,
		Fields: []spec.FieldDecl{
			{Name: "doc.title", Kind: spec.FieldKindText, MaxBytes: 256, Trust: spec.TrustTrusted},
			{Name: "doc.body", Kind: spec.FieldKindText, MaxBytes: 4096, Trust: spec.TrustUntrusted},
			{Name: "doc.meta", Kind: spec.FieldKindJSON, MaxBytes: 1024, Trust: spec.TrustUntrusted},
		},
		MaxTotalBytes: 4096,
	}

	rawEnvelope := []byte(`{
		"dialect": "system_one",
		"inputs": ["doc.title"],
		"spec": {
			"version": 1,
			"questions": [{"id": "q1", "type": "binary", "instructions": "Is urgent?"}],
			"output": {"question": "q1"}
		}
	}`)
	compiled, err := spec.Compile(rawEnvelope, spec.CompileOptions{
		Kind:        spec.KindFilter,
		Environment: &env,
	})
	require.NoError(t, err)

	var recordedBody string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		recordedBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model": "jev-1.14.0",
			"answers": {"q1": {"type": "noul", "noul": 0.92}},
			"usage": {"input_tokens": 10, "output_tokens": 1}
		}`))
	}))
	defer srv.Close()

	inst := runtime.Instance{
		ID:       "inst-rec-fields",
		Name:     "ts-rec",
		Adapter:  jevwire.AdapterNameTypeSafe,
		Model:    "jev-1.14.0",
		Endpoint: srv.URL,
		Auth:     runtime.InstanceAuth{Kind: "api_key", SecretName: "systemone.instance.inst-rec-fields"},
		Enabled:  true,
		Revision: 1,
	}
	rt := runtime.New(
		runtime.Config{
			Instances:   runtime.StaticInstances(inst),
			Credentials: runtime.StaticCredential("sk-recording-secret-token"),
		},
		runtime.WithEgressOptionsForTesting(
			egress.WithLoopbackServerForTesting(srv.URL, srv.Client()),
		),
	)
	t.Cleanup(rt.CloseIdleConnections)

	dec, err := rt.Evaluate(context.Background(), compiled, runtime.Input{
		Fields: []provider.InputField{
			{Name: "doc.title", Kind: spec.FieldKindText, Value: "SELECTED_TITLE_MARKER", Trust: spec.TrustTrusted},
			{Name: "doc.body", Kind: spec.FieldKindText, Value: "UNREFERENCED_BODY_SHOULD_NOT_APPEAR", Trust: spec.TrustUntrusted},
			{Name: "doc.meta", Kind: spec.FieldKindJSON, Value: `{"unreferenced":"META_SECRET_MARKER"}`, Trust: spec.TrustUntrusted},
			{Name: "undeclared.secret", Kind: spec.FieldKindText, Value: "UNDECLARED_SECRET_SHOULD_NOT_APPEAR", Trust: spec.TrustTrusted},
		},
	}, runtime.EvalOptions{})
	require.NoError(t, err)
	assert.True(t, dec.Bool)

	assert.Contains(t, recordedBody, "SELECTED_TITLE_MARKER")
	assert.NotContains(t, recordedBody, "UNREFERENCED_BODY_SHOULD_NOT_APPEAR")
	assert.NotContains(t, recordedBody, "META_SECRET_MARKER")
	assert.NotContains(t, recordedBody, "UNDECLARED_SECRET_SHOULD_NOT_APPEAR")
	assert.NotContains(t, recordedBody, "sk-recording-secret-token")

	require.Len(t, dec.Eval.Input.Fields, 1)
	assert.Equal(t, "doc.title", dec.Eval.Input.Fields[0].Name)
	assert.False(t, dec.Eval.Input.Fields[0].Truncated)
}

func TestRuntime_OversizeRejectAndTruncateUTF8(t *testing.T) {
	env := testConsumerEnv()

	compileRule := func(inputs, oversize string) *spec.Compiled {
		t.Helper()
		raw := []byte(`{
			"dialect": "system_one",
			"inputs": ` + inputs + `,
			"on_oversize": "` + oversize + `",
			"spec": {
				"version": 1,
				"questions": [{"id": "q1", "type": "binary", "instructions": "Check."}],
				"output": {"question": "q1"}
			}
		}`)
		c, err := spec.Compile(raw, spec.CompileOptions{Kind: spec.KindFilter, Environment: &env})
		require.NoError(t, err)
		return c
	}

	t.Run("reject_per_field_and_total_makes_zero_provider_calls", func(t *testing.T) {
		adapter := newFakeAdapter(func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
			t.Fatal("provider must not be called on oversize reject")
			return provider.ModelResult{}, nil
		})
		reg := provider.NewRegistry()
		reg.MustRegister(adapter)
		rt := runtime.New(runtime.Config{
			Registry:    reg,
			Instances:   runtime.StaticInstances(defaultTestInstance()),
			Credentials: runtime.StaticCredential("sk-test"),
		})

		c := compileRule(`["doc.title", "doc.body"]`, spec.OversizeReject)

		// Per-field oversize: doc.body is 17 bytes (> MaxBytes 16).
		_, err := rt.Evaluate(context.Background(), c, runtime.Input{
			Fields: []provider.InputField{
				{Name: "doc.title", Value: "ok"},
				{Name: "doc.body", Value: strings.Repeat("x", 17)},
			},
		}, runtime.EvalOptions{})
		require.Error(t, err)
		eval, ok := runtime.EvaluationFrom(err)
		require.True(t, ok)
		assert.Equal(t, string(provider.FailInputTooLarge), eval.Reason)
		assert.Equal(t, 0, adapter.callCount())

		// Total oversize: each field is 14 bytes (<= 16), sum 28 > MaxTotalBytes 24.
		_, err = rt.Evaluate(context.Background(), c, runtime.Input{
			Fields: []provider.InputField{
				{Name: "doc.title", Value: strings.Repeat("a", 14)},
				{Name: "doc.body", Value: strings.Repeat("b", 14)},
			},
		}, runtime.EvalOptions{})
		require.Error(t, err)
		eval, ok = runtime.EvaluationFrom(err)
		require.True(t, ok)
		assert.Equal(t, string(provider.FailInputTooLarge), eval.Reason)
		assert.Equal(t, 0, adapter.callCount())
	})

	t.Run("truncate_per_field_respects_utf8_boundary_and_records_provenance", func(t *testing.T) {
		var seenReq provider.ModelRequest
		adapter := newFakeAdapter(func(_ context.Context, _ provider.Transport, _ provider.InstanceView, req provider.ModelRequest) (provider.ModelResult, error) {
			seenReq = req
			dist, val, _, _ := provider.BinaryDistribution(0.9)
			return provider.ModelResult{
				ResolvedModel: "jev-1.14.0",
				Answers: map[string]provider.Answer{
					"q1": {QuestionID: "q1", Primitive: spec.PrimitiveBinary, Status: provider.AnswerStatusAnswered, Bool: val, Distribution: dist},
				},
			}, nil
		})
		reg := provider.NewRegistry()
		reg.MustRegister(adapter)
		rt := runtime.New(runtime.Config{
			Registry:    reg,
			Instances:   runtime.StaticInstances(defaultTestInstance()),
			Credentials: runtime.StaticCredential("sk-test"),
		})

		c := compileRule(`["doc.body"]`, spec.OversizeTruncate)
		// "abcdefghijklm" (13 bytes) + "🚀" (4 bytes) = 17 bytes; MaxBytes = 16 cuts before 🚀 -> 13 bytes.
		val := "abcdefghijklm🚀"
		require.Equal(t, 17, len(val))

		dec, err := rt.Evaluate(context.Background(), c, runtime.Input{
			Fields: []provider.InputField{
				{Name: "doc.body", Value: val, Trust: spec.TrustTrusted}, // spoofed Trust must be overwritten by env declaration
			},
		}, runtime.EvalOptions{})
		require.NoError(t, err)
		assert.True(t, dec.Bool)
		assert.Equal(t, 1, adapter.callCount())

		require.Len(t, seenReq.Input, 1)
		assert.Equal(t, "abcdefghijklm", seenReq.Input[0].Value)
		assert.True(t, utf8.ValidString(seenReq.Input[0].Value))
		assert.True(t, seenReq.Input[0].Truncated)
		assert.Equal(t, spec.TrustUntrusted, seenReq.Input[0].Trust, "Trust must come from ContentEnvironment declaration")

		require.Len(t, dec.Eval.Input.Fields, 1)
		assert.Equal(t, "doc.body", dec.Eval.Input.Fields[0].Name)
		assert.Equal(t, 13, dec.Eval.Input.Fields[0].Bytes)
		assert.True(t, dec.Eval.Input.Fields[0].Truncated)
	})

	t.Run("truncate_total_bytes_caps_across_fields_on_utf8_boundary", func(t *testing.T) {
		var seenReq provider.ModelRequest
		adapter := newFakeAdapter(func(_ context.Context, _ provider.Transport, _ provider.InstanceView, req provider.ModelRequest) (provider.ModelResult, error) {
			seenReq = req
			dist, val, _, _ := provider.BinaryDistribution(0.9)
			return provider.ModelResult{
				ResolvedModel: "jev-1.14.0",
				Answers: map[string]provider.Answer{
					"q1": {QuestionID: "q1", Primitive: spec.PrimitiveBinary, Status: provider.AnswerStatusAnswered, Bool: val, Distribution: dist},
				},
			}, nil
		})
		reg := provider.NewRegistry()
		reg.MustRegister(adapter)
		rt := runtime.New(runtime.Config{
			Registry:    reg,
			Instances:   runtime.StaticInstances(defaultTestInstance()),
			Credentials: runtime.StaticCredential("sk-test"),
		})

		c := compileRule(`["doc.title", "doc.body"]`, spec.OversizeTruncate)
		// doc.title = 15 bytes (<= 16), doc.body = "語語語語" (12 bytes <= 16).
		// Total = 27 > MaxTotalBytes (24). Remaining for doc.body = 24 - 15 = 9 -> 3 runes ("語語語" = 9 bytes).
		dec, err := rt.Evaluate(context.Background(), c, runtime.Input{
			Fields: []provider.InputField{
				{Name: "doc.title", Value: strings.Repeat("t", 15)},
				{Name: "doc.body", Value: "語語語語"},
			},
		}, runtime.EvalOptions{})
		require.NoError(t, err)
		require.Len(t, seenReq.Input, 2)
		assert.Equal(t, strings.Repeat("t", 15), seenReq.Input[0].Value)
		assert.False(t, seenReq.Input[0].Truncated)
		assert.Equal(t, "語語語", seenReq.Input[1].Value)
		assert.True(t, seenReq.Input[1].Truncated)
		assert.True(t, utf8.ValidString(seenReq.Input[1].Value))

		require.Len(t, dec.Eval.Input.Fields, 2)
		assert.False(t, dec.Eval.Input.Fields[0].Truncated)
		assert.True(t, dec.Eval.Input.Fields[1].Truncated)
		assert.Equal(t, 9, dec.Eval.Input.Fields[1].Bytes)
	})

	t.Run("json_field_rejects_invalid_json", func(t *testing.T) {
		jsonEnv := spec.ContentEnvironment{
			Name:    "test_json_env",
			Version: 1,
			Fields: []spec.FieldDecl{
				{Name: "event.payload", Kind: spec.FieldKindJSON, MaxBytes: 128, Trust: spec.TrustUntrusted},
			},
			MaxTotalBytes: 128,
		}
		raw := []byte(`{
			"dialect": "system_one",
			"inputs": ["event.payload"],
			"spec": {
				"version": 1,
				"questions": [{"id": "q1", "type": "binary", "instructions": "Check."}],
				"output": {"question": "q1"}
			}
		}`)
		c, err := spec.Compile(raw, spec.CompileOptions{Kind: spec.KindFilter, Environment: &jsonEnv})
		require.NoError(t, err)

		adapter := newFakeAdapter(func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
			t.Fatal("provider must not be called for invalid JSON field")
			return provider.ModelResult{}, nil
		})
		reg := provider.NewRegistry()
		reg.MustRegister(adapter)
		rt := runtime.New(runtime.Config{
			Registry:    reg,
			Instances:   runtime.StaticInstances(defaultTestInstance()),
			Credentials: runtime.StaticCredential("sk-test"),
		})

		_, err = rt.Evaluate(context.Background(), c, runtime.Input{
			Fields: []provider.InputField{{Name: "event.payload", Value: "{not-json"}},
		}, runtime.EvalOptions{})
		require.Error(t, err)
		eval, ok := runtime.EvaluationFrom(err)
		require.True(t, ok)
		assert.Equal(t, string(provider.FailInvalidRequest), eval.Reason)
		assert.Equal(t, 0, adapter.callCount())
	})
}

func TestRuntime_DynamicChoiceSetsValidationAndForwarding(t *testing.T) {
	env := testConsumerEnv() // candidates: MaxItems 5, MaxLabelBytes 12; MaxTotalBytes 24
	raw := []byte(`{
		"dialect": "system_one",
		"inputs": ["doc.title"],
		"spec": {
			"version": 1,
			"questions": [{"id": "q1", "type": "binary", "instructions": "Check."}],
			"output": {"question": "q1"}
		}
	}`)
	compiled, err := spec.Compile(raw, spec.CompileOptions{Kind: spec.KindFilter, Environment: &env})
	require.NoError(t, err)

	var seenChoiceSets map[string][]string
	adapter := newFakeAdapter(func(_ context.Context, _ provider.Transport, _ provider.InstanceView, req provider.ModelRequest) (provider.ModelResult, error) {
		seenChoiceSets = req.ChoiceSets
		dist, val, _, _ := provider.BinaryDistribution(0.85)
		return provider.ModelResult{
			ResolvedModel: "jev-1.14.0",
			Answers: map[string]provider.Answer{
				"q1": {QuestionID: "q1", Primitive: spec.PrimitiveBinary, Status: provider.AnswerStatusAnswered, Bool: val, Distribution: dist},
			},
		}, nil
	})
	adapter.caps.Primitives[spec.PrimitiveChoice] = provider.PrimitiveLimits{
		Supported:                true,
		MinOptions:               2,
		MaxOptions:               3, // stricter than env MaxItems=5 to test instance limit -> FailUnsupported
		DistributionCompleteness: spec.CompletenessComplete,
	}
	reg := provider.NewRegistry()
	reg.MustRegister(adapter)
	rt := runtime.New(runtime.Config{
		Registry:    reg,
		Instances:   runtime.StaticInstances(defaultTestInstance()),
		Credentials: runtime.StaticCredential("sk-test"),
	})

	// 1. Valid choice set passes and is forwarded to ModelRequest.ChoiceSets.
	dec, err := rt.Evaluate(context.Background(), compiled, runtime.Input{
		Fields:     []provider.InputField{{Name: "doc.title", Value: "hi"}},
		ChoiceSets: map[string][]string{"candidates": {"alice", "bob"}},
	}, runtime.EvalOptions{})
	require.NoError(t, err)
	assert.True(t, dec.Bool)
	assert.Equal(t, map[string][]string{"candidates": {"alice", "bob"}}, seenChoiceSets)
	assert.Equal(t, 1, adapter.callCount())

	// 2. Undeclared choice set -> FailInvalidRequest (0 new provider calls).
	_, err = rt.Evaluate(context.Background(), compiled, runtime.Input{
		Fields:     []provider.InputField{{Name: "doc.title", Value: "hi"}},
		ChoiceSets: map[string][]string{"unknown_set": {"alice"}},
	}, runtime.EvalOptions{})
	require.Error(t, err)
	eval, _ := runtime.EvaluationFrom(err)
	assert.Equal(t, string(provider.FailInvalidRequest), eval.Reason)
	assert.Equal(t, 1, adapter.callCount())

	// 3. Exceeds env MaxItems (6 > 5) -> FailInputTooLarge.
	_, err = rt.Evaluate(context.Background(), compiled, runtime.Input{
		Fields:     []provider.InputField{{Name: "doc.title", Value: "hi"}},
		ChoiceSets: map[string][]string{"candidates": {"a", "b", "c", "d", "e", "f"}},
	}, runtime.EvalOptions{})
	require.Error(t, err)
	eval, _ = runtime.EvaluationFrom(err)
	assert.Equal(t, string(provider.FailInputTooLarge), eval.Reason)
	assert.Equal(t, 1, adapter.callCount())

	// 4. Exceeds env MaxLabelBytes (13 > 12) -> FailInputTooLarge.
	_, err = rt.Evaluate(context.Background(), compiled, runtime.Input{
		Fields:     []provider.InputField{{Name: "doc.title", Value: "hi"}},
		ChoiceSets: map[string][]string{"candidates": {"abcdefghijklm"}},
	}, runtime.EvalOptions{})
	require.Error(t, err)
	eval, _ = runtime.EvaluationFrom(err)
	assert.Equal(t, string(provider.FailInputTooLarge), eval.Reason)
	assert.Equal(t, 1, adapter.callCount())

	// 5. Labels count toward MaxTotalBytes: doc.title(16) + "alice"(5) + "bob"(3) + "x"(1) = 25 > 24 -> FailInputTooLarge.
	_, err = rt.Evaluate(context.Background(), compiled, runtime.Input{
		Fields:     []provider.InputField{{Name: "doc.title", Value: strings.Repeat("t", 16)}},
		ChoiceSets: map[string][]string{"candidates": {"alice", "bob", "x"}},
	}, runtime.EvalOptions{})
	require.Error(t, err)
	eval, _ = runtime.EvaluationFrom(err)
	assert.Equal(t, string(provider.FailInputTooLarge), eval.Reason)
	assert.Equal(t, 1, adapter.callCount())

	// 6. Within env MaxItems (4 <= 5) and MaxTotalBytes, but exceeds instance choice MaxOptions (4 > 3) -> FailUnsupported.
	_, err = rt.Evaluate(context.Background(), compiled, runtime.Input{
		Fields:     []provider.InputField{{Name: "doc.title", Value: "hi"}},
		ChoiceSets: map[string][]string{"candidates": {"a", "b", "c", "d"}},
	}, runtime.EvalOptions{})
	require.Error(t, err)
	eval, _ = runtime.EvaluationFrom(err)
	assert.Equal(t, string(provider.FailUnsupported), eval.Reason)
	assert.Equal(t, 1, adapter.callCount())
}
