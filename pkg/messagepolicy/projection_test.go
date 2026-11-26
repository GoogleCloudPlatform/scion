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

package messagepolicy_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/messagepolicy"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/spec"
)

func TestEvaluationProjection_SourceEvalIDRoundTrip(t *testing.T) {
	trueVal := true
	primaryEval := &systemone.Evaluation{
		EvalID: "eval-primary-0001",
		Rule: systemone.RuleRef{
			Consumer:    messagepolicy.ConsumerName,
			RuleID:      "pol-a",
			Revision:    2,
			ContentHash: "sha256:abc",
			Dialect:     systemone.DialectName,
		},
		Requested:       spec.Binding{Instance: "inst-a"},
		ConfiguredModel: "model-1",
		ResolvedModel:   "model-1",
		Class:           systemone.ClassSuccess,
		Answers: map[string]systemone.AnswerRecord{
			"asks_choice": {
				QuestionID: "asks_choice",
				Primitive:  spec.PrimitiveBinary,
				Status:     provider.AnswerStatusAnswered,
				Required:   true,
				Bool:       &trueVal,
			},
		},
		Usage: &provider.Usage{InputTokens: 20, OutputTokens: 2},
		Attempts: []systemone.Attempt{
			{Instance: "inst-a", Outcome: "success", Requests: 1, Duration: 15 * time.Millisecond},
		},
		Elapsed: 18 * time.Millisecond,
	}

	// Primary projection omits source_eval_id and cache_hit.
	primaryRaw := messagepolicy.MarshalEvaluationProjectionWithSource(primaryEval, "")
	require.NotEmpty(t, primaryRaw)
	assert.NotContains(t, string(primaryRaw), `"source_eval_id"`)
	assert.NotContains(t, string(primaryRaw), `"cache_hit"`)

	var primaryProj messagepolicy.EvaluationProjection
	require.NoError(t, json.Unmarshal(primaryRaw, &primaryProj))
	assert.Equal(t, "eval-primary-0001", primaryProj.EvalID)
	assert.Equal(t, "pol-a", primaryProj.Rule.RuleID)
	assert.Equal(t, int64(2), primaryProj.Rule.Revision)
	assert.False(t, primaryProj.CacheHit)
	assert.Empty(t, primaryProj.SourceEvalID)

	// Secondary member projection carries fresh eval_id, own rule ref,
	// cache_hit=true, and source_eval_id pointing to the primary.
	secondaryEval := messagepolicy.CloneEvaluationForSecondary(primaryEval, "pol-b", 7)
	require.NotNil(t, secondaryEval)
	assert.Nil(t, secondaryEval.Usage, "secondary member must not carry usage")
	assert.True(t, secondaryEval.CacheHit)
	assert.NotEqual(t, primaryEval.EvalID, secondaryEval.EvalID)

	secondaryRaw := messagepolicy.MarshalEvaluationProjectionWithSource(secondaryEval, primaryEval.EvalID)
	require.NotEmpty(t, secondaryRaw)
	assert.Contains(t, string(secondaryRaw), `"source_eval_id":"eval-primary-0001"`)
	assert.Contains(t, string(secondaryRaw), `"cache_hit":true`)

	var secondaryProj messagepolicy.EvaluationProjection
	require.NoError(t, json.Unmarshal(secondaryRaw, &secondaryProj))
	assert.Equal(t, secondaryEval.EvalID, secondaryProj.EvalID)
	assert.NotEqual(t, primaryProj.EvalID, secondaryProj.EvalID)
	assert.Equal(t, "pol-b", secondaryProj.Rule.RuleID)
	assert.Equal(t, int64(7), secondaryProj.Rule.Revision)
	assert.True(t, secondaryProj.CacheHit)
	assert.Equal(t, "eval-primary-0001", secondaryProj.SourceEvalID)
	require.Contains(t, secondaryProj.Answers, "asks_choice")
	require.NotNil(t, secondaryProj.Answers["asks_choice"].Bool)
	assert.True(t, *secondaryProj.Answers["asks_choice"].Bool)

	// Failed zero-attempt provenance is preserved for secondary members too.
	zeroAttemptFailed := &systemone.Evaluation{
		EvalID: "eval-failed-zero-1",
		Rule: systemone.RuleRef{
			Consumer: messagepolicy.ConsumerName,
			RuleID:   "pol-a",
			Revision: 1,
			Dialect:  systemone.DialectName,
		},
		Requested: spec.Binding{Instance: "inst-a"},
		Class:     systemone.ClassFailed,
		Reason:    string(provider.FailCapacity),
		Elapsed:   100 * time.Millisecond,
	}
	failedSec := messagepolicy.CloneEvaluationForSecondary(zeroAttemptFailed, "pol-b", 3)
	failedRaw := messagepolicy.MarshalEvaluationProjectionWithSource(failedSec, zeroAttemptFailed.EvalID)
	require.NotEmpty(t, failedRaw)
	var failedProj messagepolicy.EvaluationProjection
	require.NoError(t, json.Unmarshal(failedRaw, &failedProj))
	assert.Equal(t, "failed", failedProj.Class)
	assert.Equal(t, string(provider.FailCapacity), failedProj.Reason)
	assert.Empty(t, failedProj.Attempts)
	assert.Equal(t, "inst-a", failedProj.Instance)
	assert.True(t, failedProj.CacheHit)
	assert.Equal(t, "eval-failed-zero-1", failedProj.SourceEvalID)
	assert.Equal(t, "pol-b", failedProj.Rule.RuleID)
	assert.Equal(t, int64(3), failedProj.Rule.Revision)
}

func TestEvaluationProjection_BackwardReadWithoutSourceEvalID(t *testing.T) {
	// Legacy persisted JSON row from before source_eval_id existed.
	legacyJSON := `{
		"eval_id": "eval-legacy-999",
		"rule": {
			"consumer": "message_policy",
			"rule_id": "pol-legacy",
			"revision": 4,
			"content_hash": "sha256:deadbeef",
			"dialect": "system_one"
		},
		"class": "success",
		"configured_model": "jev-1.14.0",
		"resolved_model": "jev-1.14.0",
		"instance": "ts-main",
		"answers": {
			"asks_choice": {
				"question_id": "asks_choice",
				"primitive": "binary",
				"status": "answered",
				"required": true,
				"bool": false
			}
		},
		"elapsed_ms": 42
	}`

	var proj messagepolicy.EvaluationProjection
	require.NoError(t, json.Unmarshal([]byte(legacyJSON), &proj))
	assert.Equal(t, "eval-legacy-999", proj.EvalID)
	assert.Equal(t, "pol-legacy", proj.Rule.RuleID)
	assert.Equal(t, int64(4), proj.Rule.Revision)
	assert.Equal(t, "success", proj.Class)
	assert.Equal(t, "ts-main", proj.Instance)
	assert.False(t, proj.CacheHit)
	assert.Empty(t, proj.SourceEvalID)
	assert.Equal(t, int64(42), proj.ElapsedMs)
	require.Contains(t, proj.Answers, "asks_choice")
	require.NotNil(t, proj.Answers["asks_choice"].Bool)
	assert.False(t, *proj.Answers["asks_choice"].Bool)
}
