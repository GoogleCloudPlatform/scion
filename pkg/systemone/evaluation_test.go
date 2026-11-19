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

package systemone_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/systemone"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// engineWrappedError simulates ruleengine.FailedError / UncertainError wrapping
// the dialect's *systemone.EvaluationError via Unwrap() error (P1 brief §5).
type engineWrappedError struct {
	Reason string
	Cause  error
}

func (e *engineWrappedError) Error() string {
	return "ruleengine failed: " + e.Reason
}

func (e *engineWrappedError) Unwrap() error {
	return e.Cause
}

func TestEvaluationFrom_UnwrapsThroughEngineAndFmtWrapping(t *testing.T) {
	eval := &systemone.Evaluation{
		EvalID:          "eval-123",
		Class:           systemone.ClassFailed,
		Reason:          string(provider.FailTimeout),
		ConfiguredModel: "jev-1.14.0",
		ResolvedModel:   "jev-1.14.0",
	}

	t.Run("nil_error", func(t *testing.T) {
		got, ok := systemone.EvaluationFrom(nil)
		assert.False(t, ok)
		assert.Nil(t, got)
	})

	t.Run("unrelated_error", func(t *testing.T) {
		got, ok := systemone.EvaluationFrom(errors.New("plain error"))
		assert.False(t, ok)
		assert.Nil(t, got)
	})

	t.Run("nil_eval_inside_error", func(t *testing.T) {
		got, ok := systemone.EvaluationFrom(&systemone.EvaluationError{Eval: nil})
		assert.False(t, ok)
		assert.Nil(t, got)
	})

	t.Run("direct_pointer_error", func(t *testing.T) {
		err := &systemone.EvaluationError{Eval: eval}
		assert.Equal(t, "system_one failed: timeout", err.Error())

		got, ok := systemone.EvaluationFrom(err)
		require.True(t, ok)
		assert.Same(t, eval, got)

		// Verify runtime.EvaluationFrom and systemone.EvaluationFrom share one type.
		gotRT, okRT := runtime.EvaluationFrom(err)
		require.True(t, okRT)
		assert.Same(t, eval, gotRT)
	})

	t.Run("value_evaluation_error", func(t *testing.T) {
		var err error = systemone.EvaluationError{Eval: eval}
		got, ok := systemone.EvaluationFrom(err)
		require.True(t, ok)
		assert.Same(t, eval, got)
	})

	t.Run("wrapped_in_engine_error_and_fmt_errorf", func(t *testing.T) {
		dialectErr := &systemone.EvaluationError{Eval: eval}
		engineErr := &engineWrappedError{Reason: "timeout", Cause: dialectErr}
		outerErr := fmt.Errorf("evaluate rule: %w", engineErr)

		got, ok := systemone.EvaluationFrom(outerErr)
		require.True(t, ok)
		assert.Same(t, eval, got)
	})
}

func TestEvaluation_EffectiveResolvedModelHonorsAliasNote(t *testing.T) {
	t.Run("exact_version_without_alias_note", func(t *testing.T) {
		eval := &systemone.Evaluation{
			ConfiguredModel: "jev-latest",
			ResolvedModel:   "jev-1.14.0",
		}
		assert.False(t, eval.HasModelAliasReported())
		assert.Equal(t, "jev-1.14.0", eval.EffectiveResolvedModel())
	})

	t.Run("alias_echoed_with_model_alias_reported_note", func(t *testing.T) {
		eval := &systemone.Evaluation{
			ConfiguredModel: "jev-latest",
			ResolvedModel:   "jev-latest",
			Notes:           []string{provider.NoteModelAliasReported},
		}
		assert.True(t, eval.HasModelAliasReported())
		assert.Equal(t, "jev-latest", eval.ResolvedModel, "verbatim ResolvedModel is preserved")
		assert.Empty(t, eval.EffectiveResolvedModel(), "alias-reported model counts as unresolved")
	})

	t.Run("model_unreported_note", func(t *testing.T) {
		eval := &systemone.Evaluation{
			ConfiguredModel: "gemini-decision-1",
			ResolvedModel:   "gemini-decision-1",
			Notes:           []string{provider.NoteModelUnreported},
		}
		assert.True(t, eval.HasModelUnreported())
		assert.False(t, eval.HasUsageUnreported())
		assert.Empty(t, eval.EffectiveResolvedModel(), "unreported fallback model counts as unresolved")
	})

	t.Run("usage_unreported_note", func(t *testing.T) {
		eval := &systemone.Evaluation{
			ConfiguredModel: "gemini-decision-1",
			Notes:           []string{provider.NoteModelUnreported, provider.NoteUsageUnreported},
		}
		assert.True(t, eval.HasModelUnreported())
		assert.True(t, eval.HasUsageUnreported())
		assert.Nil(t, eval.Usage)
	})
}

func TestRetryableLater_MatchesRuntimeDesignSection63(t *testing.T) {
	wantTrue := map[provider.FailReason]bool{
		provider.FailUnavailable: true,
		provider.FailRateLimited: true,
		provider.FailCapacity:    true,
		provider.FailCircuitOpen: true,
		provider.FailTimeout:     true,
	}
	for _, r := range provider.AllFailReasons() {
		assert.Equal(t, wantTrue[r], systemone.RetryableLater(r), "reason=%s", r)
	}
}
