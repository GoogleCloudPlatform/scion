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
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/messagepolicy"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/spec"
)

func TestEvaluateJudgmentAndApplyJudgment_MatchesEvaluatePolicy(t *testing.T) {
	ctx := context.Background()
	rule := `{
		"dialect": "system_one",
		"inputs": ["message.body"],
		"spec": {
			"version": 1,
			"binding": {"instance": "mp-inst"},
			"questions": [{
				"id": "asks_choice",
				"type": "binary",
				"instructions": "Does the message ask the recipient to choose?"
			}]
		}
	}`

	ev, _ := newTestEvaluatorWithSystemOne(t, nil, func(_ context.Context, _ provider.Transport, iv provider.InstanceView, _ provider.ModelRequest) (provider.ModelResult, error) {
		v := true
		return provider.ModelResult{
			ResolvedModel: iv.Model,
			HTTPStatus:    200,
			Answers: map[string]provider.Answer{
				"asks_choice": {
					QuestionID:   "asks_choice",
					Primitive:    spec.PrimitiveBinary,
					Status:       provider.AnswerStatusAnswered,
					Bool:         &v,
					Distribution: map[string]float64{"true": 0.9, "false": 0.1},
				},
			},
			Usage: provider.Usage{InputTokens: 15, OutputTokens: 2},
		}, nil
	})

	facts := messagepolicy.Facts{
		Surface:   messagepolicy.SurfaceAgentToUser,
		Sender:    messagepolicy.SenderFacts{Kind: "agent", ID: "ag-1", Slug: "agent-a", ProjectID: "proj-1"},
		Recipient: messagepolicy.RecipientFacts{Kind: "user", ID: "u-1", Slug: "alice"},
		Message:   messagepolicy.MessageFacts{Type: "message", Body: "Option A or B?"},
	}

	compiled, err := ev.CompileSystemOne(ctx, rule)
	require.NoError(t, err)

	req := systemone.Request{
		Input:     messagepolicy.BuildSystemOneInput(facts),
		ProjectID: "proj-1",
		RuleID:    "pol-1",
		Revision:  3,
	}

	eval, soCls := ev.EvaluateJudgment(ctx, compiled, req, messagepolicy.WithMaxEvalTime(time.Second))
	require.Equal(t, messagepolicy.ClassPass, soCls.Class)
	require.NotNil(t, eval)

	// Two policies applying different CEL violation_rules to the same judgment.
	budget := messagepolicy.NewSendBudget()
	clsViol := ev.ApplyJudgment(ctx, "pol-1", 3, `judgment_flags.asks_choice`, facts, eval, soCls, messagepolicy.WithBudget(budget))
	assert.Equal(t, messagepolicy.ClassViolation, clsViol.Class)
	assert.Equal(t, eval, clsViol.Evaluation)
	assert.True(t, clsViol.Judgments.Flags["asks_choice"])

	secEval := messagepolicy.CloneEvaluationForSecondary(eval, "pol-2", 9)
	clsPass := ev.ApplyJudgment(ctx, "pol-2", 9, `!judgment_flags.asks_choice`, facts, secEval, soCls, messagepolicy.WithBudget(budget))
	assert.Equal(t, messagepolicy.ClassPass, clsPass.Class)
	assert.Equal(t, secEval, clsPass.Evaluation)
	assert.Equal(t, "pol-2", clsPass.Evaluation.Rule.RuleID)
	assert.True(t, clsPass.Evaluation.CacheHit)
	assert.Nil(t, clsPass.Evaluation.Usage)

	// Compare against EvaluatePolicy on the primary policy.
	direct := ev.EvaluatePolicy(
		ctx, "pol-1", 3, `judgment_flags.asks_choice`, rule, "proj-1", facts,
		[]messagepolicy.EvalOption{messagepolicy.WithBudget(messagepolicy.NewSendBudget())},
		[]messagepolicy.EvalOption{messagepolicy.WithMaxEvalTime(time.Second)},
	)
	assert.Equal(t, direct.Class, clsViol.Class)
	assert.Equal(t, direct.Reason, clsViol.Reason)
	assert.Equal(t, direct.Judgments, clsViol.Judgments)

	// Failed System One result short-circuits before CEL and attaches the member's eval.
	failedPrimary := &systemone.Evaluation{
		EvalID: "eval-fail-1",
		Class:  systemone.ClassFailed,
		Reason: string(provider.FailCapacity),
	}
	failedSec := messagepolicy.CloneEvaluationForSecondary(failedPrimary, "pol-2", 9)
	failedCls := messagepolicy.Classification{
		Class:      messagepolicy.ClassFailed,
		Reason:     string(provider.FailCapacity),
		Evaluation: failedPrimary,
	}
	appliedFailed := ev.ApplyJudgment(ctx, "pol-2", 9, `judgment_labels.missing_key == "boom"`, facts, failedSec, failedCls, messagepolicy.WithBudget(budget))
	assert.Equal(t, messagepolicy.ClassFailed, appliedFailed.Class)
	assert.Equal(t, string(provider.FailCapacity), appliedFailed.Reason)
	assert.Equal(t, failedSec, appliedFailed.Evaluation)
}
