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
	"encoding/json"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/systemone/metrics"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeAdapter is an in-memory provider.Adapter (and optional ModelLister) for
// testing runtime execution, classification, and provenance without jevwire.
type fakeAdapter struct {
	name       string
	version    string
	caps       provider.Capabilities
	validateFn func(inst provider.InstanceView, rule provider.RuleSpec) error
	invokeFn   func(ctx context.Context, t provider.Transport, inst provider.InstanceView, req provider.ModelRequest) (provider.ModelResult, error)

	mu       sync.Mutex
	calls    int
	lastInst provider.InstanceView
	lastReq  provider.ModelRequest
}

func newFakeAdapter(invokeFn func(ctx context.Context, t provider.Transport, inst provider.InstanceView, req provider.ModelRequest) (provider.ModelResult, error)) *fakeAdapter {
	return &fakeAdapter{
		name:    "fake-typesafe",
		version: "0.1.0-test",
		caps: provider.Capabilities{
			Primitives: map[string]provider.PrimitiveLimits{
				spec.PrimitiveBinary: {
					Supported:                true,
					DistributionCompleteness: spec.CompletenessComplete,
				},
				spec.PrimitiveChoice: {
					Supported:                true,
					MinOptions:               2,
					MaxOptions:               64,
					DistributionCompleteness: spec.CompletenessComplete,
				},
			},
			MaxQuestionsPerRequest: 64,
			MaxInputTokens:         32768,
			Truncation:             provider.TruncationNever,
			ReturnsDistribution:    true,
			NativeMetrics: []provider.MetricDecl{
				{
					Name:   "typesafe.confidence",
					Unit:   metrics.UnitFraction,
					Min:    0,
					Max:    1,
					Source: metrics.SourceProviderDoc,
				},
			},
			SideEffectFree: true,
			AuthScheme:     provider.AuthBearer,
		},
		invokeFn: invokeFn,
	}
}

func (a *fakeAdapter) Name() string    { return a.name }
func (a *fakeAdapter) Version() string { return a.version }
func (a *fakeAdapter) Capabilities(provider.InstanceView) (provider.Capabilities, error) {
	return a.caps, nil
}
func (a *fakeAdapter) Validate(inst provider.InstanceView, rule provider.RuleSpec) error {
	if a.validateFn != nil {
		return a.validateFn(inst, rule)
	}
	return nil
}
func (a *fakeAdapter) Invoke(ctx context.Context, t provider.Transport, inst provider.InstanceView, req provider.ModelRequest) (provider.ModelResult, error) {
	a.mu.Lock()
	a.calls++
	a.lastInst = inst
	a.lastReq = req
	a.mu.Unlock()
	return a.invokeFn(ctx, t, inst, req)
}

func (a *fakeAdapter) callCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

func (a *fakeAdapter) recordedRequest() (provider.InstanceView, provider.ModelRequest) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastInst, a.lastReq
}

type fakeModelListerAdapter struct {
	*fakeAdapter
	modelsFn func(ctx context.Context, t provider.Transport, inst provider.InstanceView) ([]string, error)
}

func (a *fakeModelListerAdapter) ListModels(ctx context.Context, t provider.Transport, inst provider.InstanceView) ([]string, error) {
	return a.modelsFn(ctx, t, inst)
}

func defaultTestInstance() runtime.Instance {
	return runtime.Instance{
		ID:       "inst-uuid-1",
		Name:     "jev-primary",
		Adapter:  "fake-typesafe",
		Model:    "jev-1.14.0",
		Endpoint: "https://api.typesafe.ai",
		Location: "us",
		Auth: runtime.InstanceAuth{
			Kind:       "api_key",
			SecretName: "systemone.instance.inst-uuid-1",
		},
		Enabled:  true,
		Revision: 3,
	}
}

func compileTestSpec(t *testing.T, raw string, kind spec.Kind) *spec.Compiled {
	t.Helper()
	c, err := spec.Compile([]byte(raw), spec.CompileOptions{Kind: kind})
	require.NoError(t, err)
	return c
}

func boolPtr(v bool) *bool    { return &v }
func strPtr(v string) *string { return &v }

const mixedSpecJSON = `{
  "version": 1,
  "questions": [
    {
      "id": "asks_choice",
      "type": "binary",
      "instructions": "Does probe.text ask for a choice?"
    },
    {
      "id": "topic",
      "type": "choice",
      "instructions": "Which topic is probe.text about?",
      "options": [
        {"id": "billing", "description": "Invoices and payments"},
        {"id": "engineering", "description": "Code and bugs"},
        {"id": "cannot_tell", "description": "Insufficient information", "abstain": true}
      ]
    }
  ]
}`

// ---------------------------------------------------------------------------
// AC-6: Success, Provenance, Portable & Native Metrics, Alias Note
// ---------------------------------------------------------------------------

func TestRuntime_EvaluateSuccessProvenanceAndMetrics(t *testing.T) {
	const probeContent = "SENSITIVE_SYNTHETIC_PROBE_TEXT_DO_NOT_RECORD_IN_EVAL"

	adapter := newFakeAdapter(func(_ context.Context, _ provider.Transport, inst provider.InstanceView, req provider.ModelRequest) (provider.ModelResult, error) {
		distBin, valBin, tiedBin, err := provider.BinaryDistribution(0.85)
		if err != nil || tiedBin {
			t.Fatalf("unexpected binary dist error: %v", err)
		}
		return provider.ModelResult{
			ResolvedModel:     "jev-1.14.0",
			ProviderRequestID: "req-ts-98765",
			HTTPStatus:        200,
			Notes:             []string{provider.NoteModelAliasReported},
			Usage: provider.Usage{
				InputTokens:  42,
				OutputTokens: 2,
			},
			Answers: map[string]provider.Answer{
				"asks_choice": {
					QuestionID:   "asks_choice",
					Primitive:    spec.PrimitiveBinary,
					Status:       provider.AnswerStatusAnswered,
					Bool:         valBin,
					Distribution: distBin,
				},
				"topic": {
					QuestionID: "topic",
					Primitive:  spec.PrimitiveChoice,
					Status:     provider.AnswerStatusAnswered,
					Choice:     strPtr("engineering"),
					Distribution: map[string]float64{
						"billing":     0.15,
						"engineering": 0.80,
						"cannot_tell": 0.05,
					},
					NativeMetrics: map[string]provider.Metric{
						"typesafe.confidence": {
							Value:  0.70,
							Unit:   metrics.UnitFraction,
							Min:    0,
							Max:    1,
							Source: metrics.SourceProviderDoc,
						},
					},
				},
			},
		}, nil
	})

	reg := provider.NewRegistry()
	reg.MustRegister(adapter)

	inst := defaultTestInstance()
	inst.Model = "jev-latest"

	rt := runtime.New(runtime.Config{
		Registry:    reg,
		Instances:   runtime.StaticInstances(inst),
		Credentials: runtime.StaticCredential("sk-never-touch-eval"),
	})

	compiled := compileTestSpec(t, mixedSpecJSON, spec.KindJudgments)
	// Pass an extra unreferenced field alongside probe.text to verify stripping.
	in := runtime.Input{
		Fields: []provider.InputField{
			{Name: spec.ProbeFieldText, Kind: spec.FieldKindText, Value: probeContent, Trust: spec.TrustTrusted /* must be coerced to env's untrusted */},
			{Name: "unreferenced.extra", Kind: spec.FieldKindText, Value: "UNREFERENCED_PAYLOAD", Trust: spec.TrustUntrusted},
		},
	}

	dec, err := rt.Evaluate(context.Background(), compiled, in, runtime.EvalOptions{
		Consumer:  "probe",
		RuleID:    "rule-1",
		Revision:  7,
		ProjectID: "proj-1",
	})
	require.NoError(t, err)
	assert.Equal(t, spec.KindJudgments, dec.Kind)
	require.NotNil(t, dec.Eval)
	assert.Equal(t, 1, adapter.callCount(), "must make exactly one attempt")

	// Verify only referenced probe.text reached ModelRequest, with env's Untrusted label.
	gotView, gotReq := adapter.recordedRequest()
	assert.Equal(t, "jev-primary", gotView.Name)
	assert.Equal(t, "jev-latest", gotView.Model)
	require.Len(t, gotReq.Input, 1)
	assert.Equal(t, spec.ProbeFieldText, gotReq.Input[0].Name)
	assert.Equal(t, probeContent, gotReq.Input[0].Value)
	assert.Equal(t, spec.TrustUntrusted, gotReq.Input[0].Trust)
	assert.Equal(t, []string{"asks_choice", "topic"}, gotReq.Questions)

	eval := dec.Eval
	assert.NotEmpty(t, eval.EvalID)
	assert.Equal(t, runtime.ClassSuccess, eval.Class)
	assert.Empty(t, eval.Reason)
	assert.Equal(t, "jev-latest", eval.ConfiguredModel)
	assert.Equal(t, "jev-1.14.0", eval.ResolvedModel)
	assert.True(t, eval.HasModelAliasReported())
	assert.Empty(t, eval.EffectiveResolvedModel(), "alias note makes EffectiveResolvedModel empty")
	assert.Equal(t, compiled.ContentHash, eval.Rule.ContentHash)
	assert.Equal(t, "system_one", eval.Rule.Dialect)

	// Attempt provenance
	require.Len(t, eval.Attempts, 1)
	att := eval.Attempts[0]
	assert.Equal(t, "jev-primary", att.Instance)
	assert.Equal(t, int64(3), att.InstanceRevision)
	assert.Equal(t, "fake-typesafe", att.Adapter)
	assert.Equal(t, "0.1.0-test", att.AdapterVersion)
	assert.Equal(t, "jev-latest", att.ConfiguredModel)
	assert.Equal(t, "jev-1.14.0", att.ReportedModel)
	assert.Equal(t, "req-ts-98765", att.ProviderRequestID)
	assert.Equal(t, 1, att.Requests)
	assert.Equal(t, 0, att.Retries)
	assert.Equal(t, runtime.AttemptSuccess, att.Outcome)
	assert.Equal(t, "2xx", att.HTTPStatusClass)

	// Per-question answers, portable metrics, and native metrics
	require.Len(t, eval.Answers, 2)
	binRec := eval.Answers["asks_choice"]
	require.NotNil(t, binRec.Bool)
	assert.True(t, *binRec.Bool)
	assert.InDelta(t, 0.85, binRec.Metrics[metrics.NamePTop].Value, 1e-9)
	assert.InDelta(t, 0.70, binRec.Metrics[metrics.NameMargin].Value, 1e-9)
	assert.Equal(t, metrics.SourceScion, binRec.Metrics[metrics.NameNormEntropy].Source)

	choiceRec := eval.Answers["topic"]
	require.NotNil(t, choiceRec.Choice)
	assert.Equal(t, "engineering", *choiceRec.Choice)
	assert.False(t, choiceRec.Abstained)
	assert.InDelta(t, 0.80, choiceRec.Metrics[metrics.NamePTop].Value, 1e-9)
	assert.InDelta(t, 0.65, choiceRec.Metrics[metrics.NameMargin].Value, 1e-9)
	assert.InDelta(t, 0.70, choiceRec.NativeMetrics["typesafe.confidence"].Value, 1e-9)
	assert.Equal(t, metrics.SourceProviderDoc, choiceRec.NativeMetrics["typesafe.confidence"].Source)

	// InputFingerprint is content-free: probe text and secret never appear in serialized Evaluation.
	require.Len(t, eval.Input.Fields, 1)
	assert.Equal(t, spec.ProbeFieldText, eval.Input.Fields[0].Name)
	assert.Equal(t, len(probeContent), eval.Input.Fields[0].Bytes)
	assert.True(t, strings.HasPrefix(eval.Input.SHA256, "sha256:"))

	rawEvalJSON, err := json.Marshal(eval)
	require.NoError(t, err)
	assert.NotContains(t, string(rawEvalJSON), probeContent)
	assert.NotContains(t, string(rawEvalJSON), "UNREFERENCED_PAYLOAD")
	assert.NotContains(t, string(rawEvalJSON), "sk-never-touch-eval")
}

// ---------------------------------------------------------------------------
// AC-3: Built-in probe environment 16 KiB limit & conservative bytes pre-check
// ---------------------------------------------------------------------------

func TestRuntime_AC3_ProbeOversizeAndBytesPrecheck(t *testing.T) {
	binaryFilterJSON := `{
	  "version": 1,
	  "questions": [{"id": "q1", "type": "binary", "instructions": "Check text."}],
	  "output": {"question": "q1"}
	}`
	compiled := compileTestSpec(t, binaryFilterJSON, spec.KindFilter)

	t.Run("exact_16KiB_succeeds", func(t *testing.T) {
		adapter := newFakeAdapter(func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
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

		text16KiB := strings.Repeat("a", spec.ProbeMaxBytes)
		dec, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput(text16KiB), runtime.EvalOptions{})
		require.NoError(t, err)
		assert.True(t, dec.Bool)
		assert.Equal(t, 1, adapter.callCount())
	})

	t.Run("over_16KiB_returns_input_too_large_without_calling_adapter", func(t *testing.T) {
		adapter := newFakeAdapter(func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
			t.Fatal("adapter must not be called when input exceeds 16 KiB")
			return provider.ModelResult{}, nil
		})
		reg := provider.NewRegistry()
		reg.MustRegister(adapter)
		rt := runtime.New(runtime.Config{
			Registry:    reg,
			Instances:   runtime.StaticInstances(defaultTestInstance()),
			Credentials: runtime.StaticCredential("sk-test"),
		})

		oversize := strings.Repeat("x", spec.ProbeMaxBytes+1)
		_, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput(oversize), runtime.EvalOptions{})
		require.Error(t, err)
		assert.Equal(t, 0, adapter.callCount())

		eval, ok := runtime.EvaluationFrom(err)
		require.True(t, ok)
		assert.Equal(t, runtime.ClassFailed, eval.Class)
		assert.Equal(t, string(provider.FailInputTooLarge), eval.Reason)
		require.Len(t, eval.Input.Fields, 1)
		assert.Equal(t, spec.ProbeMaxBytes+1, eval.Input.Fields[0].Bytes)
	})

	t.Run("over_conservative_bytes_precheck_returns_input_too_large", func(t *testing.T) {
		adapter := newFakeAdapter(func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
			t.Fatal("adapter must not be called when input exceeds MaxInputTokens bytes pre-check")
			return provider.ModelResult{}, nil
		})
		adapter.caps.MaxInputTokens = 100 // 100 tokens * 16 bytes/token = 1600 bytes loose pre-check ceiling
		reg := provider.NewRegistry()
		reg.MustRegister(adapter)
		rt := runtime.New(runtime.Config{
			Registry:    reg,
			Instances:   runtime.StaticInstances(defaultTestInstance()),
			Credentials: runtime.StaticCredential("sk-test"),
		})

		overPrecheck := strings.Repeat("b", 100*runtime.BytesPerTokenPrecheck+1)
		_, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput(overPrecheck), runtime.EvalOptions{})
		require.Error(t, err)
		assert.Equal(t, 0, adapter.callCount())

		eval, ok := runtime.EvaluationFrom(err)
		require.True(t, ok)
		assert.Equal(t, runtime.ClassFailed, eval.Class)
		assert.Equal(t, string(provider.FailInputTooLarge), eval.Reason)
	})

	t.Run("multibyte_cjk_under_16_bytes_per_token_is_not_falsely_rejected_on_non_silent_adapter", func(t *testing.T) {
		adapter := newFakeAdapter(func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
			dist, val, _, _ := provider.BinaryDistribution(0.9)
			return provider.ModelResult{
				ResolvedModel: "jev-1.14.0",
				Answers: map[string]provider.Answer{
					"q1": {QuestionID: "q1", Primitive: spec.PrimitiveBinary, Status: provider.AnswerStatusAnswered, Bool: val, Distribution: dist},
				},
			}, nil
		})
		adapter.caps.MaxInputTokens = 100 // 600 bytes (e.g. 200 3-byte CJK runes) fits well under 100 * 16 = 1600
		reg := provider.NewRegistry()
		reg.MustRegister(adapter)
		rt := runtime.New(runtime.Config{
			Registry:    reg,
			Instances:   runtime.StaticInstances(defaultTestInstance()),
			Credentials: runtime.StaticCredential("sk-test"),
		})

		cjkText := strings.Repeat("語", 200) // 600 UTF-8 bytes
		dec, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput(cjkText), runtime.EvalOptions{})
		require.NoError(t, err)
		assert.True(t, dec.Bool)
		assert.Equal(t, 1, adapter.callCount())
	})

	t.Run("silent_truncation_adapter_enforces_strict_1_byte_per_token", func(t *testing.T) {
		adapter := newFakeAdapter(func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
			t.Fatal("silent-truncation adapter must reject unprovable non-truncation")
			return provider.ModelResult{}, nil
		})
		adapter.caps.Truncation = provider.TruncationSilent
		adapter.caps.MaxInputTokens = 200
		reg := provider.NewRegistry()
		reg.MustRegister(adapter)
		rt := runtime.New(runtime.Config{
			Registry:    reg,
			Instances:   runtime.StaticInstances(defaultTestInstance()),
			Credentials: runtime.StaticCredential("sk-test"),
		})

		_, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput(strings.Repeat("c", 201)), runtime.EvalOptions{})
		require.Error(t, err)
		eval, ok := runtime.EvaluationFrom(err)
		require.True(t, ok)
		assert.Equal(t, string(provider.FailInputTooLarge), eval.Reason)
	})

	t.Run("provider_usage_truncated_true_returns_input_too_large", func(t *testing.T) {
		adapter := newFakeAdapter(func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
			dist, val, _, _ := provider.BinaryDistribution(0.8)
			tr := true
			return provider.ModelResult{
				ResolvedModel: "jev-1.14.0",
				Usage:         provider.Usage{InputTokens: 512, Truncated: &tr},
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

		_, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("short"), runtime.EvalOptions{})
		require.Error(t, err)
		eval, ok := runtime.EvaluationFrom(err)
		require.True(t, ok)
		assert.Equal(t, runtime.ClassFailed, eval.Class)
		assert.Equal(t, string(provider.FailInputTooLarge), eval.Reason)
	})
}

// ---------------------------------------------------------------------------
// AC-6: Abstain, Tie, Optional vs Required, Route & Filter Projections
// ---------------------------------------------------------------------------

func TestRuntime_AC6_UncertainAbstainAndTieAndOptionalQuestions(t *testing.T) {
	t.Run("portable_abstain_option_yields_uncertain_abstained", func(t *testing.T) {
		choiceSpec := `{
		  "version": 1,
		  "questions": [{
		    "id": "route_q",
		    "type": "choice",
		    "instructions": "Pick target.",
		    "options": [
		      {"id": "alpha", "description": "Alpha team"},
		      {"id": "beta", "description": "Beta team"},
		      {"id": "unsure", "description": "Cannot tell", "abstain": true}
		    ]
		  }],
		  "output": {"question": "route_q"}
		}`
		compiled := compileTestSpec(t, choiceSpec, spec.KindRoute)

		adapter := newFakeAdapter(func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
			return provider.ModelResult{
				ResolvedModel: "jev-1.14.0",
				Answers: map[string]provider.Answer{
					"route_q": {
						QuestionID: "route_q",
						Primitive:  spec.PrimitiveChoice,
						Status:     provider.AnswerStatusAnswered,
						Choice:     strPtr("unsure"),
						Distribution: map[string]float64{
							"alpha":  0.10,
							"beta":   0.20,
							"unsure": 0.70,
						},
					},
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

		_, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("ambiguous"), runtime.EvalOptions{})
		require.Error(t, err)
		eval, ok := runtime.EvaluationFrom(err)
		require.True(t, ok)
		assert.Equal(t, runtime.ClassUncertain, eval.Class)
		assert.Equal(t, provider.UncertainAbstained, eval.Reason)
		assert.True(t, eval.Answers["route_q"].Abstained)
		require.Len(t, eval.Attempts, 1)
		assert.Equal(t, runtime.AttemptUncertain, eval.Attempts[0].Outcome)
	})

	t.Run("adapter_native_abstained_status_yields_uncertain_abstained", func(t *testing.T) {
		binSpec := `{
		  "version": 1,
		  "questions": [{"id": "q1", "type": "binary", "instructions": "Check."}],
		  "output": {"question": "q1"}
		}`
		compiled := compileTestSpec(t, binSpec, spec.KindJudgmentBool)

		adapter := newFakeAdapter(func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
			return provider.ModelResult{
				ResolvedModel: "jev-1.14.0",
				Answers: map[string]provider.Answer{
					"q1": {
						QuestionID: "q1",
						Primitive:  spec.PrimitiveBinary,
						Status:     provider.AnswerStatusAbstained,
					},
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

		_, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("text"), runtime.EvalOptions{})
		require.Error(t, err)
		eval, ok := runtime.EvaluationFrom(err)
		require.True(t, ok)
		assert.Equal(t, runtime.ClassUncertain, eval.Class)
		assert.Equal(t, provider.UncertainAbstained, eval.Reason)
		assert.True(t, eval.Answers["q1"].Abstained)
	})

	t.Run("choice_exact_top_tie_yields_uncertain_tie", func(t *testing.T) {
		choiceSpec := `{
		  "version": 1,
		  "questions": [{
		    "id": "route_q",
		    "type": "choice",
		    "instructions": "Pick target.",
		    "options": [
		      {"id": "alpha", "description": "Alpha team"},
		      {"id": "beta", "description": "Beta team"}
		    ]
		  }],
		  "output": {"question": "route_q"}
		}`
		compiled := compileTestSpec(t, choiceSpec, spec.KindRoute)

		adapter := newFakeAdapter(func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
			return provider.ModelResult{
				ResolvedModel: "jev-1.14.0",
				Answers: map[string]provider.Answer{
					"route_q": {
						QuestionID: "route_q",
						Primitive:  spec.PrimitiveChoice,
						Status:     provider.AnswerStatusAnswered,
						Tied:       true,
						Choice:     nil,
						Distribution: map[string]float64{
							"alpha": 0.50,
							"beta":  0.50,
						},
					},
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

		_, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("50-50"), runtime.EvalOptions{})
		require.Error(t, err)
		eval, ok := runtime.EvaluationFrom(err)
		require.True(t, ok)
		assert.Equal(t, runtime.ClassUncertain, eval.Class)
		assert.Equal(t, provider.UncertainTie, eval.Reason)
		assert.True(t, eval.Answers["route_q"].Tied)
	})

	t.Run("binary_05_tie_yields_uncertain_tie", func(t *testing.T) {
		binSpec := `{
		  "version": 1,
		  "questions": [{"id": "q1", "type": "binary", "instructions": "Check."}],
		  "output": {"question": "q1"}
		}`
		compiled := compileTestSpec(t, binSpec, spec.KindFilter)

		adapter := newFakeAdapter(func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
			dist, val, tied, _ := provider.BinaryDistribution(0.5)
			return provider.ModelResult{
				ResolvedModel: "jev-1.14.0",
				Answers: map[string]provider.Answer{
					"q1": {QuestionID: "q1", Primitive: spec.PrimitiveBinary, Status: provider.AnswerStatusAnswered, Bool: val, Tied: tied, Distribution: dist},
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

		_, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("coinflip"), runtime.EvalOptions{})
		require.Error(t, err)
		eval, ok := runtime.EvaluationFrom(err)
		require.True(t, ok)
		assert.Equal(t, runtime.ClassUncertain, eval.Class)
		assert.Equal(t, provider.UncertainTie, eval.Reason)
	})

	t.Run("optional_question_uncertain_or_refused_does_not_alter_required_success", func(t *testing.T) {
		optSpec := `{
		  "version": 1,
		  "questions": [
		    {"id": "req_q", "type": "binary", "instructions": "Required question."},
		    {"id": "opt_q", "type": "binary", "instructions": "Optional question."}
		  ],
		  "required": ["req_q"],
		  "output": {"question": "req_q"}
		}`
		compiled := compileTestSpec(t, optSpec, spec.KindFilter)

		adapter := newFakeAdapter(func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
			dist, val, _, _ := provider.BinaryDistribution(0.95)
			return provider.ModelResult{
				ResolvedModel: "jev-1.14.0",
				Answers: map[string]provider.Answer{
					"req_q": {QuestionID: "req_q", Primitive: spec.PrimitiveBinary, Status: provider.AnswerStatusAnswered, Bool: val, Distribution: dist},
					"opt_q": {QuestionID: "opt_q", Primitive: spec.PrimitiveBinary, Status: provider.AnswerStatusRefused},
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

		dec, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("ok"), runtime.EvalOptions{})
		require.NoError(t, err)
		assert.True(t, dec.Bool)
		assert.Equal(t, runtime.ClassSuccess, dec.Eval.Class)
		assert.Equal(t, provider.AnswerStatusRefused, dec.Eval.Answers["opt_q"].Status)
		assert.False(t, dec.Eval.Answers["opt_q"].Required)
	})
}

func TestRuntime_AC6_ProjectionsAndMalformedResponses(t *testing.T) {
	routeSpec := `{
	  "version": 1,
	  "questions": [{
	    "id": "dest",
	    "type": "choice",
	    "instructions": "Select destination.",
	    "options": [
	      {"id": "billing", "description": "Billing queue"},
	      {"id": "support", "description": "Support queue"}
	    ]
	  }],
	  "output": {"question": "dest"}
	}`

	t.Run("route_valid_candidate_succeeds", func(t *testing.T) {
		compiled := compileTestSpec(t, routeSpec, spec.KindRoute)
		adapter := newFakeAdapter(func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
			return provider.ModelResult{
				ResolvedModel: "jev-1.14.0",
				Answers: map[string]provider.Answer{
					"dest": {
						QuestionID:   "dest",
						Primitive:    spec.PrimitiveChoice,
						Status:       provider.AnswerStatusAnswered,
						Choice:       strPtr("support"),
						Distribution: map[string]float64{"billing": 0.1, "support": 0.9},
					},
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

		dec, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("help"), runtime.EvalOptions{})
		require.NoError(t, err)
		assert.Equal(t, "support", dec.String)
	})

	t.Run("filter_choice_match_projection", func(t *testing.T) {
		filterChoiceSpec := `{
		  "version": 1,
		  "questions": [{
		    "id": "dest",
		    "type": "choice",
		    "instructions": "Select destination.",
		    "options": [
		      {"id": "billing", "description": "Billing queue"},
		      {"id": "support", "description": "Support queue"}
		    ]
		  }],
		  "output": {"question": "dest", "match": ["billing"]}
		}`
		compiled := compileTestSpec(t, filterChoiceSpec, spec.KindFilter)
		adapter := newFakeAdapter(func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
			return provider.ModelResult{
				ResolvedModel: "jev-1.14.0",
				Answers: map[string]provider.Answer{
					"dest": {
						QuestionID:   "dest",
						Primitive:    spec.PrimitiveChoice,
						Status:       provider.AnswerStatusAnswered,
						Choice:       strPtr("billing"),
						Distribution: map[string]float64{"billing": 0.85, "support": 0.15},
					},
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

		dec, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("invoice"), runtime.EvalOptions{})
		require.NoError(t, err)
		assert.True(t, dec.Bool)
	})

	malformedCases := []struct {
		name   string
		noDist bool
		result provider.ModelResult
	}{
		{
			name:   "route_answer_outside_candidate_set",
			noDist: true,
			result: provider.ModelResult{
				ResolvedModel: "jev-1.14.0",
				Answers: map[string]provider.Answer{
					"dest": {
						QuestionID: "dest",
						Primitive:  spec.PrimitiveChoice,
						Status:     provider.AnswerStatusAnswered,
						Choice:     strPtr("evil_injected_target"),
					},
				},
			},
		},
		{
			name: "incomplete_distribution_missing_key",
			result: provider.ModelResult{
				ResolvedModel: "jev-1.14.0",
				Answers: map[string]provider.Answer{
					"dest": {
						QuestionID:   "dest",
						Primitive:    spec.PrimitiveChoice,
						Status:       provider.AnswerStatusAnswered,
						Choice:       strPtr("billing"),
						Distribution: map[string]float64{"billing": 1.0},
					},
				},
			},
		},
		{
			name: "non_normalized_distribution_sum_09",
			result: provider.ModelResult{
				ResolvedModel: "jev-1.14.0",
				Answers: map[string]provider.Answer{
					"dest": {
						QuestionID:   "dest",
						Primitive:    spec.PrimitiveChoice,
						Status:       provider.AnswerStatusAnswered,
						Choice:       strPtr("billing"),
						Distribution: map[string]float64{"billing": 0.7, "support": 0.2},
					},
				},
			},
		},
		{
			name: "nan_probability",
			result: provider.ModelResult{
				ResolvedModel: "jev-1.14.0",
				Answers: map[string]provider.Answer{
					"dest": {
						QuestionID:   "dest",
						Primitive:    spec.PrimitiveChoice,
						Status:       provider.AnswerStatusAnswered,
						Choice:       strPtr("billing"),
						Distribution: map[string]float64{"billing": math.NaN(), "support": 0.2},
					},
				},
			},
		},
		{
			name: "choice_not_argmax",
			result: provider.ModelResult{
				ResolvedModel: "jev-1.14.0",
				Answers: map[string]provider.Answer{
					"dest": {
						QuestionID:   "dest",
						Primitive:    spec.PrimitiveChoice,
						Status:       provider.AnswerStatusAnswered,
						Choice:       strPtr("billing"),
						Distribution: map[string]float64{"billing": 0.2, "support": 0.8},
					},
				},
			},
		},
		{
			name: "missing_required_answer",
			result: provider.ModelResult{
				ResolvedModel: "jev-1.14.0",
				Answers:       map[string]provider.Answer{},
			},
		},
		{
			name: "extra_unknown_question_key",
			result: provider.ModelResult{
				ResolvedModel: "jev-1.14.0",
				Answers: map[string]provider.Answer{
					"dest": {
						QuestionID:   "dest",
						Primitive:    spec.PrimitiveChoice,
						Status:       provider.AnswerStatusAnswered,
						Choice:       strPtr("billing"),
						Distribution: map[string]float64{"billing": 0.8, "support": 0.2},
					},
					"unexpected_q": {
						QuestionID: "unexpected_q",
						Primitive:  spec.PrimitiveChoice,
						Status:     provider.AnswerStatusAnswered,
					},
				},
			},
		},
		{
			name: "wrong_primitive_type",
			result: provider.ModelResult{
				ResolvedModel: "jev-1.14.0",
				Answers: map[string]provider.Answer{
					"dest": {
						QuestionID: "dest",
						Primitive:  spec.PrimitiveBinary,
						Status:     provider.AnswerStatusAnswered,
						Bool:       boolPtr(true),
					},
				},
			},
		},
	}

	for _, tc := range malformedCases {
		t.Run(tc.name, func(t *testing.T) {
			compiled := compileTestSpec(t, routeSpec, spec.KindRoute)
			adapter := newFakeAdapter(func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
				return tc.result, nil
			})
			if tc.noDist {
				adapter.caps.ReturnsDistribution = false
				lim := adapter.caps.Primitives[spec.PrimitiveChoice]
				lim.DistributionCompleteness = spec.CompletenessNone
				adapter.caps.Primitives[spec.PrimitiveChoice] = lim
			}
			reg := provider.NewRegistry()
			reg.MustRegister(adapter)
			rt := runtime.New(runtime.Config{
				Registry:    reg,
				Instances:   runtime.StaticInstances(defaultTestInstance()),
				Credentials: runtime.StaticCredential("sk-test"),
			})

			_, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("test"), runtime.EvalOptions{})
			require.Error(t, err)
			eval, ok := runtime.EvaluationFrom(err)
			require.True(t, ok, "Evaluation must be present on every non-cancel path")
			assert.Equal(t, runtime.ClassFailed, eval.Class)
			assert.Equal(t, string(provider.FailMalformedResponse), eval.Reason)
		})
	}
}

// ---------------------------------------------------------------------------
// AC-6: Full closed 17-reason FailReason classification + panic recovery +
// Evaluation present on every non-cancel path
// ---------------------------------------------------------------------------

func TestRuntime_AC6_AllClosedFailReasonsAndPanicAndRefusal(t *testing.T) {
	binSpec := `{
	  "version": 1,
	  "questions": [{"id": "q1", "type": "binary", "instructions": "Check."}],
	  "output": {"question": "q1"}
	}`
	compiled := compileTestSpec(t, binSpec, spec.KindFilter)

	// Verify every member of the closed 17-value FailReason vocabulary surfaces
	// with Evaluation present on every non-cancel path.
	for _, reason := range provider.AllFailReasons() {
		t.Run("provider_reason_"+string(reason), func(t *testing.T) {
			adapter := newFakeAdapter(func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
				return provider.ModelResult{}, provider.NewProviderError(reason, reason.RetryableLater(), 2*time.Second, 503, "synthetic failure")
			})
			reg := provider.NewRegistry()
			reg.MustRegister(adapter)
			rt := runtime.New(runtime.Config{
				Registry:    reg,
				Instances:   runtime.StaticInstances(defaultTestInstance()),
				Credentials: runtime.StaticCredential("sk-test"),
			})

			_, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("probe"), runtime.EvalOptions{})
			require.Error(t, err)
			assert.Equal(t, 1, adapter.callCount(), "P1 must not retry even when Retryable is true")

			eval, ok := runtime.EvaluationFrom(err)
			require.True(t, ok, "Evaluation must be present for reason %s", reason)
			assert.Equal(t, runtime.ClassFailed, eval.Class)
			assert.Equal(t, string(reason), eval.Reason)
			require.Len(t, eval.Attempts, 1)
			assert.Equal(t, runtime.AttemptFailed, eval.Attempts[0].Outcome)
			assert.Equal(t, string(reason), eval.Attempts[0].Reason)
		})
	}

	t.Run("required_question_refused_yields_provider_refused", func(t *testing.T) {
		adapter := newFakeAdapter(func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
			return provider.ModelResult{
				ResolvedModel: "jev-1.14.0",
				Answers: map[string]provider.Answer{
					"q1": {QuestionID: "q1", Primitive: spec.PrimitiveBinary, Status: provider.AnswerStatusRefused},
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

		_, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("probe"), runtime.EvalOptions{})
		require.Error(t, err)
		eval, ok := runtime.EvaluationFrom(err)
		require.True(t, ok)
		assert.Equal(t, runtime.ClassFailed, eval.Class)
		assert.Equal(t, string(provider.FailProviderRefused), eval.Reason)
	})

	t.Run("disabled_or_missing_instance_yields_no_eligible_instance", func(t *testing.T) {
		inst := defaultTestInstance()
		inst.Enabled = false
		reg := provider.NewRegistry()
		reg.MustRegister(newFakeAdapter(nil))
		rt := runtime.New(runtime.Config{
			Registry:    reg,
			Instances:   runtime.StaticInstances(inst),
			Credentials: runtime.StaticCredential("sk-test"),
		})

		_, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("probe"), runtime.EvalOptions{})
		require.Error(t, err)
		eval, ok := runtime.EvaluationFrom(err)
		require.True(t, ok)
		assert.Equal(t, runtime.ClassFailed, eval.Class)
		assert.Equal(t, string(provider.FailNoEligibleInstance), eval.Reason)
	})

	t.Run("adapter_panic_recovered_as_failed_internal_without_leaking_panic_payload", func(t *testing.T) {
		adapter := newFakeAdapter(func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
			panic("PANIC_PAYLOAD_DO_NOT_ECHO")
		})
		reg := provider.NewRegistry()
		reg.MustRegister(adapter)
		rt := runtime.New(runtime.Config{
			Registry:    reg,
			Instances:   runtime.StaticInstances(defaultTestInstance()),
			Credentials: runtime.StaticCredential("sk-test"),
		})

		_, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("probe"), runtime.EvalOptions{})
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "PANIC_PAYLOAD_DO_NOT_ECHO")

		eval, ok := runtime.EvaluationFrom(err)
		require.True(t, ok)
		assert.Equal(t, runtime.ClassFailed, eval.Class)
		assert.Equal(t, string(provider.FailInternal), eval.Reason)
	})
}

func TestRuntime_ListModels(t *testing.T) {
	t.Run("supported_adapter_returns_models", func(t *testing.T) {
		base := newFakeAdapter(nil)
		ml := &fakeModelListerAdapter{
			fakeAdapter: base,
			modelsFn: func(_ context.Context, tr provider.Transport, inst provider.InstanceView) ([]string, error) {
				require.NotNil(t, tr)
				assert.Equal(t, "jev-primary", inst.Name)
				return []string{"jev-1.14.0", "jev-latest"}, nil
			},
		}
		reg := provider.NewRegistry()
		reg.MustRegister(ml)
		rt := runtime.New(runtime.Config{
			Registry:    reg,
			Credentials: runtime.StaticCredential("sk-test"),
		})

		models, err := rt.ListModels(context.Background(), defaultTestInstance())
		require.NoError(t, err)
		assert.Equal(t, []string{"jev-1.14.0", "jev-latest"}, models)
	})

	t.Run("adapter_without_model_lister_returns_unsupported", func(t *testing.T) {
		reg := provider.NewRegistry()
		reg.MustRegister(newFakeAdapter(nil))
		rt := runtime.New(runtime.Config{
			Registry:    reg,
			Credentials: runtime.StaticCredential("sk-test"),
		})

		_, err := rt.ListModels(context.Background(), defaultTestInstance())
		require.Error(t, err)
		pe, ok := provider.AsProviderError(err)
		require.True(t, ok)
		assert.Equal(t, provider.FailUnsupported, pe.Reason)
	})
}
