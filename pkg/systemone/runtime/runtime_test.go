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
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/systemone/egress"
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

	t.Run("adapter_panic_in_list_models_recovered_as_failed_internal", func(t *testing.T) {
		base := newFakeAdapter(nil)
		ml := &fakeModelListerAdapter{
			fakeAdapter: base,
			modelsFn: func(_ context.Context, _ provider.Transport, _ provider.InstanceView) ([]string, error) {
				panic("LIST_MODELS_PANIC_DO_NOT_ECHO")
			},
		}
		reg := provider.NewRegistry()
		reg.MustRegister(ml)
		rt := runtime.New(runtime.Config{
			Registry:    reg,
			Credentials: runtime.StaticCredential("sk-test"),
		})

		models, err := rt.ListModels(context.Background(), defaultTestInstance())
		require.Error(t, err)
		assert.Nil(t, models)
		assert.NotContains(t, err.Error(), "LIST_MODELS_PANIC_DO_NOT_ECHO")
		pe, ok := provider.AsProviderError(err)
		require.True(t, ok)
		assert.Equal(t, provider.FailInternal, pe.Reason)
	})

	t.Run("sanitizes_and_caps_listed_model_names", func(t *testing.T) {
		raw := []string{
			"  jev-1.14.0  ",
			"",
			"   ",
			"bad\ninjected-log",
			"bad\x1b[31mterminal-escape",
			"bad internal space",
			"語-non-ascii",
			strings.Repeat("m", runtime.MaxProviderIdentifierBytes+1),
			strings.Repeat("k", runtime.MaxProviderIdentifierBytes),
		}
		for i := 0; i < 300; i++ {
			raw = append(raw, fmt.Sprintf("jev-extra-%03d", i))
		}

		base := newFakeAdapter(nil)
		ml := &fakeModelListerAdapter{
			fakeAdapter: base,
			modelsFn: func(_ context.Context, _ provider.Transport, _ provider.InstanceView) ([]string, error) {
				return raw, nil
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
		require.Len(t, models, runtime.MaxListedModels)
		assert.Equal(t, "jev-1.14.0", models[0])
		assert.Equal(t, strings.Repeat("k", runtime.MaxProviderIdentifierBytes), models[1])
		assert.Equal(t, "jev-extra-000", models[2])
		for _, m := range models {
			assert.NotContains(t, m, "\n")
			assert.NotContains(t, m, "\x1b")
			assert.NotContains(t, m, " ")
			assert.LessOrEqual(t, len(m), runtime.MaxProviderIdentifierBytes)
		}
	})
}

type staticPublicResolver struct{}

func (staticPublicResolver) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil
}

type hostRoutingDialer struct {
	targets map[string]string
}

func (d hostRoutingDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	target, ok := d.targets[port]
	if !ok {
		return nil, fmt.Errorf("unexpected port %s", port)
	}
	var nd net.Dialer
	return nd.DialContext(ctx, network, target)
}

func TestRuntime_TransportCacheKeyIsolatesEndpointAndAuth(t *testing.T) {
	var mu sync.Mutex
	var authHeadersA, authHeadersB []string

	srvA := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		authHeadersA = append(authHeadersA, r.Header.Get("Authorization"))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srvA.Close()

	srvB := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		authHeadersB = append(authHeadersB, r.Header.Get("Authorization"))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srvB.Close()

	_, portA, err := net.SplitHostPort(srvA.Listener.Addr().String())
	require.NoError(t, err)
	_, portB, err := net.SplitHostPort(srvB.Listener.Addr().String())
	require.NoError(t, err)

	base := newFakeAdapter(nil)
	ml := &fakeModelListerAdapter{
		fakeAdapter: base,
		modelsFn: func(ctx context.Context, tr provider.Transport, inst provider.InstanceView) ([]string, error) {
			req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, inst.Endpoint+"/v1/models", nil)
			if reqErr != nil {
				return nil, reqErr
			}
			resp, doErr := tr.Do(req)
			if doErr != nil {
				return nil, doErr
			}
			_ = resp.Body.Close()
			return []string{"jev-1.14.0"}, nil
		},
	}
	reg := provider.NewRegistry()
	reg.MustRegister(ml)

	rt := runtime.New(
		runtime.Config{
			Registry: reg,
			Credentials: func(_ context.Context, inst runtime.Instance) (string, error) {
				return fmt.Sprintf("key[%s|%s|%s|%s|%s]",
					inst.Auth.SecretName,
					inst.Endpoint,
					inst.Auth.HeaderName,
					inst.Auth.Audience,
					strings.Join(inst.Auth.Scopes, ","),
				), nil
			},
		},
		runtime.WithEgressOptionsForTesting(
			egress.WithDialerForTesting(staticPublicResolver{}, hostRoutingDialer{
				targets: map[string]string{
					portA: srvA.Listener.Addr().String(),
					portB: srvB.Listener.Addr().String(),
				},
			}),
			egress.WithTLSConfigForTesting(&tls.Config{InsecureSkipVerify: true}), //nolint:gosec // offline test TLS server
		),
	)
	defer rt.CloseIdleConnections()

	endpointA := "https://provider-a.example.test:" + portA
	endpointB := "https://provider-b.example.test:" + portB

	// Case 1: Identical (ID, Name, Revision, Endpoint), different SecretName.
	instSecretA := runtime.Instance{
		ID:       "same-id",
		Name:     "same-name",
		Revision: 1,
		Adapter:  "fake-typesafe",
		Endpoint: endpointA,
		Enabled:  true,
		Auth:     runtime.InstanceAuth{Kind: "api_key", SecretName: "secret-A"},
	}
	instSecretB := instSecretA
	instSecretB.Auth.SecretName = "secret-B"

	_, err = rt.ListModels(context.Background(), instSecretA)
	require.NoError(t, err)
	_, err = rt.ListModels(context.Background(), instSecretB)
	require.NoError(t, err)

	// Case 2: Identical (ID, Name, Revision, Auth), different Endpoint.
	instEndB := instSecretA
	instEndB.Endpoint = endpointB
	_, err = rt.ListModels(context.Background(), instEndB)
	require.NoError(t, err)

	// Case 3: Identical (ID, Name, Revision, Endpoint, SecretName), different Audience / Scopes.
	instAud := instSecretA
	instAud.Auth.Audience = "aud-2"
	instAud.Auth.Scopes = []string{"scope-x"}
	_, err = rt.ListModels(context.Background(), instAud)
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{
		fmt.Sprintf("Bearer key[secret-A|%s|||]", endpointA),
		fmt.Sprintf("Bearer key[secret-B|%s|||]", endpointA),
		fmt.Sprintf("Bearer key[secret-A|%s||aud-2|scope-x]", endpointA),
	}, authHeadersA, "instances with identical (id, name, revision) must never share bearer credentials")
	require.Equal(t, []string{
		fmt.Sprintf("Bearer key[secret-A|%s|||]", endpointB),
	}, authHeadersB, "different endpoint must never reuse credential bound to previous endpoint")
}

func TestRuntime_ForgetInstanceEvictsAllKeysForID(t *testing.T) {
	var mu sync.Mutex
	var seenTransports []provider.Transport

	base := newFakeAdapter(nil)
	ml := &fakeModelListerAdapter{
		fakeAdapter: base,
		modelsFn: func(_ context.Context, tr provider.Transport, _ provider.InstanceView) ([]string, error) {
			mu.Lock()
			seenTransports = append(seenTransports, tr)
			mu.Unlock()
			return []string{"jev-1.14.0"}, nil
		},
	}
	reg := provider.NewRegistry()
	reg.MustRegister(ml)

	rt := runtime.New(runtime.Config{
		Registry: reg,
		Credentials: func(_ context.Context, inst runtime.Instance) (string, error) {
			return "tok-" + inst.Auth.SecretName, nil
		},
	})
	defer rt.CloseIdleConnections()

	lastTransport := func() provider.Transport {
		mu.Lock()
		defer mu.Unlock()
		return seenTransports[len(seenTransports)-1]
	}

	targetA := runtime.Instance{
		ID:       "inst-target",
		Name:     "target",
		Revision: 1,
		Adapter:  "fake-typesafe",
		Endpoint: "https://provider-a.example.test",
		Enabled:  true,
		Auth:     runtime.InstanceAuth{Kind: "api_key", SecretName: "secret-a"},
	}
	// Same ID + Revision, different Endpoint and SecretName (distinct transportKey).
	targetB := targetA
	targetB.Endpoint = "https://provider-b.example.test"
	targetB.Auth.SecretName = "secret-b"

	other := runtime.Instance{
		ID:       "inst-other",
		Name:     "other",
		Revision: 1,
		Adapter:  "fake-typesafe",
		Endpoint: "https://provider-other.example.test",
		Enabled:  true,
		Auth:     runtime.InstanceAuth{Kind: "api_key", SecretName: "secret-other"},
	}

	_, err := rt.ListModels(context.Background(), targetA)
	require.NoError(t, err)
	trTargetA1 := lastTransport()

	_, err = rt.ListModels(context.Background(), targetA)
	require.NoError(t, err)
	trTargetA2 := lastTransport()
	require.Same(t, trTargetA1, trTargetA2, "transport for targetA should be cached before ForgetInstance")

	_, err = rt.ListModels(context.Background(), targetB)
	require.NoError(t, err)
	trTargetB1 := lastTransport()
	require.NotSame(t, trTargetA1, trTargetB1)

	_, err = rt.ListModels(context.Background(), other)
	require.NoError(t, err)
	trOther1 := lastTransport()

	// Blank ID is a no-op.
	rt.ForgetInstance("   ")
	_, err = rt.ListModels(context.Background(), targetA)
	require.NoError(t, err)
	require.Same(t, trTargetA1, lastTransport(), "blank ForgetInstance must not evict cached entries")

	// Evict all keys belonging to inst-target regardless of endpoint/auth fields.
	rt.ForgetInstance("inst-target")

	_, err = rt.ListModels(context.Background(), targetA)
	require.NoError(t, err)
	require.NotSame(t, trTargetA1, lastTransport(), "targetA transport must be evicted after ForgetInstance")

	_, err = rt.ListModels(context.Background(), targetB)
	require.NoError(t, err)
	require.NotSame(t, trTargetB1, lastTransport(), "targetB transport with same ID must also be evicted")

	_, err = rt.ListModels(context.Background(), other)
	require.NoError(t, err)
	require.Same(t, trOther1, lastTransport(), "unrelated instance transport must remain cached")
}

type wrappedAdapterError struct {
	pe    *provider.ProviderError
	cause error
}

func (e wrappedAdapterError) Error() string { return e.pe.Error() }
func (e wrappedAdapterError) Unwrap() []error {
	return []error{e.pe, e.cause}
}

func TestRuntime_WrappedEgressErrorOverridesProviderErrorReason(t *testing.T) {
	binSpec := `{
	  "version": 1,
	  "questions": [{"id": "q1", "type": "binary", "instructions": "Check."}],
	  "output": {"question": "q1"}
	}`
	compiled := compileTestSpec(t, binSpec, spec.KindFilter)

	cases := []struct {
		name     string
		sentinel error
		want     provider.FailReason
	}{
		{name: "wrapped_redirect_blocked", sentinel: egress.ErrRedirectBlocked, want: provider.FailEgressRefused},
		{name: "wrapped_blocked_destination", sentinel: egress.ErrBlockedDestination, want: provider.FailEgressRefused},
		{name: "wrapped_scheme_not_allowed", sentinel: egress.ErrSchemeNotAllowed, want: provider.FailEgressRefused},
		{name: "wrapped_invalid_endpoint", sentinel: egress.ErrInvalidEndpoint, want: provider.FailEgressRefused},
		{name: "wrapped_response_too_large", sentinel: egress.ErrResponseTooLarge, want: provider.FailMalformedResponse},
		{name: "wrapped_credential_unavailable", sentinel: egress.ErrCredentialUnavailable, want: provider.FailUnavailable},
		{name: "wrapped_auth", sentinel: egress.ErrAuth, want: provider.FailAuth},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			adapter := newFakeAdapter(func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
				// Deliberately wrap inside a ProviderError carrying a conflicting Reason
				// to verify runtime's errors.Is egress table wins before pe.Reason is read.
				return provider.ModelResult{}, wrappedAdapterError{
					pe:    provider.NewProviderError(provider.FailInternal, false, 0, 502, "adapter wrapper"),
					cause: tc.sentinel,
				}
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
			assert.Equal(t, string(tc.want), eval.Reason)
			require.Len(t, eval.Attempts, 1)
			assert.Equal(t, string(tc.want), eval.Attempts[0].Reason)
			assert.Equal(t, "5xx", eval.Attempts[0].HTTPStatusClass)
		})
	}
}

func TestRuntime_SanitizesProviderIdentifiersAndFiltersNotes(t *testing.T) {
	binSpec := `{
	  "version": 1,
	  "questions": [{"id": "q1", "type": "binary", "instructions": "Check."}],
	  "output": {"question": "q1"}
	}`
	compiled := compileTestSpec(t, binSpec, spec.KindFilter)

	t.Run("oversize_64KiB_and_newline_identifiers_and_arbitrary_notes_dropped", func(t *testing.T) {
		oversizeModel := strings.Repeat("M", 64<<10) + "\nlevel=ERROR msg=injected"
		newlineAnsModel := "jev-ans\r\nspoofed_field=1"
		oversizeReqID := strings.Repeat("R", 200) + "\ninjected"
		arbitraryTopNote := "arbitrary_echoed_user_prompt_secret"
		arbitraryAnsNote := "arbitrary_echoed_answer_note"

		adapter := newFakeAdapter(func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
			dist, val, _, _ := provider.BinaryDistribution(0.9)
			return provider.ModelResult{
				ResolvedModel:     oversizeModel,
				ProviderRequestID: oversizeReqID,
				HTTPStatus:        200,
				Notes:             []string{arbitraryTopNote, provider.NoteClamped001To099},
				Answers: map[string]provider.Answer{
					"q1": {
						QuestionID:    "q1",
						Primitive:     spec.PrimitiveBinary,
						Status:        provider.AnswerStatusAnswered,
						Bool:          val,
						Distribution:  dist,
						ReportedModel: newlineAnsModel,
						Notes:         []string{arbitraryAnsNote, provider.NoteConfidenceFormulaDeviation},
					},
				},
			}, nil
		})

		var logBuf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		reg := provider.NewRegistry()
		reg.MustRegister(adapter)
		rt := runtime.New(runtime.Config{
			Registry:    reg,
			Instances:   runtime.StaticInstances(defaultTestInstance()),
			Credentials: runtime.StaticCredential("sk-test"),
			Logger:      logger,
		})

		dec, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("hello"), runtime.EvalOptions{})
		require.NoError(t, err)
		assert.True(t, dec.Bool)
		require.NotNil(t, dec.Eval)

		// Violating ResolvedModel/ReportedModel/ProviderRequestID are dropped and NoteModelUnreported is recorded.
		assert.Empty(t, dec.Eval.ResolvedModel)
		assert.Empty(t, dec.Eval.EffectiveResolvedModel())
		require.Len(t, dec.Eval.Attempts, 1)
		assert.Empty(t, dec.Eval.Attempts[0].ReportedModel)
		assert.Empty(t, dec.Eval.Attempts[0].ProviderRequestID)
		assert.Equal(t, []string{provider.NoteClamped001To099, provider.NoteModelUnreported}, dec.Eval.Notes)

		rec := dec.Eval.Answers["q1"]
		assert.Empty(t, rec.ReportedModel)
		assert.Equal(t, []string{provider.NoteConfidenceFormulaDeviation, provider.NoteModelUnreported}, rec.Notes)

		evalJSON, err := json.Marshal(dec.Eval)
		require.NoError(t, err)
		jsonStr := string(evalJSON)
		logs := logBuf.String()
		require.NotEmpty(t, logs)

		for _, forbidden := range []string{
			"MMMMMMMM",
			"msg=injected",
			"jev-ans",
			"spoofed_field",
			"RRRRRRRR",
			arbitraryTopNote,
			arbitraryAnsNote,
		} {
			assert.NotContains(t, jsonStr, forbidden, "Evaluation JSON leaked unsanitized value %q", forbidden)
			assert.NotContains(t, logs, forbidden, "debug logs leaked unsanitized value %q", forbidden)
		}
	})

	t.Run("trimmed_valid_identifier_preserved_and_error_path_sanitized", func(t *testing.T) {
		var logBuf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		okAdapter := newFakeAdapter(func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
			dist, val, _, _ := provider.BinaryDistribution(0.8)
			return provider.ModelResult{
				ResolvedModel:     "  jev-1.14.0  ",
				ProviderRequestID: "  req_valid_123  ",
				Answers: map[string]provider.Answer{
					"q1": {QuestionID: "q1", Primitive: spec.PrimitiveBinary, Status: provider.AnswerStatusAnswered, Bool: val, Distribution: dist, ReportedModel: " jev-1.14.0 "},
				},
			}, nil
		})
		reg := provider.NewRegistry()
		reg.MustRegister(okAdapter)
		rt := runtime.New(runtime.Config{
			Registry:    reg,
			Instances:   runtime.StaticInstances(defaultTestInstance()),
			Credentials: runtime.StaticCredential("sk-test"),
			Logger:      logger,
		})

		dec, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("ok"), runtime.EvalOptions{})
		require.NoError(t, err)
		assert.Equal(t, "jev-1.14.0", dec.Eval.ResolvedModel)
		assert.Equal(t, "jev-1.14.0", dec.Eval.EffectiveResolvedModel())
		assert.Equal(t, "jev-1.14.0", dec.Eval.Attempts[0].ReportedModel)
		assert.Equal(t, "req_valid_123", dec.Eval.Attempts[0].ProviderRequestID)
		assert.Equal(t, "jev-1.14.0", dec.Eval.Answers["q1"].ReportedModel)
		assert.Empty(t, dec.Eval.Notes)

		// Error path with hostile ResolvedModel + note must also scrub both.
		errAdapter := newFakeAdapter(func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
			return provider.ModelResult{
				ResolvedModel: "bad-err-model\ninjected",
				Notes:         []string{"arbitrary_err_secret"},
				HTTPStatus:    503,
			}, provider.NewProviderError(provider.FailUnavailable, true, 0, 503, "upstream unavailable")
		})
		regErr := provider.NewRegistry()
		regErr.MustRegister(errAdapter)
		rtErr := runtime.New(runtime.Config{
			Registry:    regErr,
			Instances:   runtime.StaticInstances(defaultTestInstance()),
			Credentials: runtime.StaticCredential("sk-test"),
			Logger:      logger,
		})

		_, err = rtErr.Evaluate(context.Background(), compiled, runtime.ProbeInput("err"), runtime.EvalOptions{})
		require.Error(t, err)
		evalErr, ok := runtime.EvaluationFrom(err)
		require.True(t, ok)
		assert.Empty(t, evalErr.ResolvedModel)
		assert.Empty(t, evalErr.Attempts[0].ReportedModel)
		assert.Equal(t, []string{provider.NoteModelUnreported}, evalErr.Notes)

		errJSON, mErr := json.Marshal(evalErr)
		require.NoError(t, mErr)
		assert.NotContains(t, string(errJSON), "bad-err-model")
		assert.NotContains(t, string(errJSON), "arbitrary_err_secret")
		assert.NotContains(t, logBuf.String(), "bad-err-model")
		assert.NotContains(t, logBuf.String(), "arbitrary_err_secret")
	})
}

func TestRuntime_BinarySubUnitSumArgmaxRule(t *testing.T) {
	binSpec := `{
	  "version": 1,
	  "questions": [{"id": "q1", "type": "binary", "instructions": "Check."}],
	  "output": {"question": "q1"}
	}`
	compiled := compileTestSpec(t, binSpec, spec.KindFilter)

	cases := []struct {
		name      string
		pTrue     float64
		pFalse    float64
		givenBool *bool
		wantFail  bool
		wantBool  bool
	}{
		{
			name:      "sub_1_sum_within_tolerance_nil_bool_derives_true_by_argmax",
			pTrue:     0.4998,
			pFalse:    0.4994,
			givenBool: nil,
			wantBool:  true,
		},
		{
			name:      "sub_1_sum_within_tolerance_matching_true_bool_succeeds",
			pTrue:     0.4998,
			pFalse:    0.4994,
			givenBool: boolPtr(true),
			wantBool:  true,
		},
		{
			name:      "sub_1_sum_within_tolerance_contradictory_false_bool_rejected",
			pTrue:     0.4998,
			pFalse:    0.4994,
			givenBool: boolPtr(false),
			wantFail:  true,
		},
		{
			name:      "sub_1_sum_within_tolerance_false_argmax_derives_false",
			pTrue:     0.4994,
			pFalse:    0.4998,
			givenBool: nil,
			wantBool:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			adapter := newFakeAdapter(func(_ context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
				return provider.ModelResult{
					ResolvedModel: "jev-1.14.0",
					Answers: map[string]provider.Answer{
						"q1": {
							QuestionID: "q1",
							Primitive:  spec.PrimitiveBinary,
							Status:     provider.AnswerStatusAnswered,
							Bool:       tc.givenBool,
							Distribution: map[string]float64{
								provider.BinaryTrueKey:  tc.pTrue,
								provider.BinaryFalseKey: tc.pFalse,
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

			dec, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("check"), runtime.EvalOptions{})
			if tc.wantFail {
				require.Error(t, err)
				eval, ok := runtime.EvaluationFrom(err)
				require.True(t, ok)
				assert.Equal(t, runtime.ClassFailed, eval.Class)
				assert.Equal(t, string(provider.FailMalformedResponse), eval.Reason)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantBool, dec.Bool)
		})
	}
}

func TestRuntime_DeadlineCauseWinsOverSubsequentCallerCancel(t *testing.T) {
	binSpec := `{
	  "version": 1,
	  "questions": [{"id": "q1", "type": "binary", "instructions": "Check."}],
	  "output": {"question": "q1"}
	}`
	compiled := compileTestSpec(t, binSpec, spec.KindFilter)

	callerCtx, callerCancel := context.WithCancel(context.Background())
	defer callerCancel()

	adapter := newFakeAdapter(func(attemptCtx context.Context, _ provider.Transport, _ provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
		<-attemptCtx.Done()
		// Cancel the parent caller context AFTER the runtime's own deadline has
		// already expired on attemptCtx, before returning to Evaluate.
		callerCancel()
		return provider.ModelResult{}, attemptCtx.Err()
	})

	reg := provider.NewRegistry()
	reg.MustRegister(adapter)
	rt := runtime.New(runtime.Config{
		Registry:    reg,
		Instances:   runtime.StaticInstances(defaultTestInstance()),
		Credentials: runtime.StaticCredential("sk-test"),
		MaxEvalTime: 35 * time.Millisecond,
	})

	_, err := rt.Evaluate(callerCtx, compiled, runtime.ProbeInput("race"), runtime.EvalOptions{})
	require.Error(t, err)
	eval, ok := runtime.EvaluationFrom(err)
	require.True(t, ok)
	assert.Equal(t, runtime.ClassFailed, eval.Class)
	assert.Equal(t, string(provider.FailTimeout), eval.Reason, "runtime deadline cause must win over a subsequent caller cancel")
	require.Len(t, eval.Attempts, 1)
	assert.Equal(t, string(provider.FailTimeout), eval.Attempts[0].Reason)
}
