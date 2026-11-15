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

package runtime

import (
	"math"
	"slices"

	"github.com/GoogleCloudPlatform/scion/pkg/systemone/metrics"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/spec"
)

func classifyAndProject(
	compiled *spec.Compiled,
	caps provider.Capabilities,
	res provider.ModelResult,
	eval *Evaluation,
) (Decision, Class, string) {
	zero := Decision{Kind: compiled.Kind}

	// Provider-reported input truncation with on_oversize: reject -> input_too_large (AC-3).
	if res.Usage.Truncated != nil && *res.Usage.Truncated && compiled.OnOversize != spec.OversizeTruncate {
		return zero, ClassFailed, string(provider.FailInputTooLarge)
	}

	declaredQuestions := make(map[string]struct{}, len(compiled.Spec.Questions))
	for _, q := range compiled.Spec.Questions {
		declaredQuestions[q.ID] = struct{}{}
	}
	for qID := range res.Answers {
		if _, ok := declaredQuestions[qID]; !ok {
			return zero, ClassFailed, string(provider.FailMalformedResponse)
		}
	}

	answers := make(map[string]AnswerRecord, len(compiled.Spec.Questions))
	refusedRequired := false
	uncertainReason := ""

	for _, q := range compiled.Spec.Questions {
		req := compiled.IsRequired(q.ID)
		ans, present := res.Answers[q.ID]
		if !present {
			if req {
				return zero, ClassFailed, string(provider.FailMalformedResponse)
			}
			continue
		}

		rec, failReason := validateAndBuildAnswerRecord(q, req, caps, ans)
		if failReason != "" {
			return zero, ClassFailed, string(failReason)
		}
		answers[q.ID] = rec

		if !req {
			continue
		}
		if rec.Status == provider.AnswerStatusRefused {
			refusedRequired = true
			continue
		}
		if rec.Abstained && uncertainReason == "" {
			uncertainReason = provider.UncertainAbstained
		} else if rec.Tied && uncertainReason == "" {
			uncertainReason = provider.UncertainTie
		}
	}

	eval.Answers = answers

	if refusedRequired {
		return zero, ClassFailed, string(provider.FailProviderRefused)
	}
	if uncertainReason != "" {
		return zero, ClassUncertain, uncertainReason
	}

	return projectDecision(compiled, answers)
}

func validateAndBuildAnswerRecord(
	q spec.Question,
	required bool,
	caps provider.Capabilities,
	ans provider.Answer,
) (AnswerRecord, provider.FailReason) {
	if ans.QuestionID != "" && ans.QuestionID != q.ID {
		return AnswerRecord{}, provider.FailMalformedResponse
	}
	if ans.Primitive != "" && ans.Primitive != q.Type {
		return AnswerRecord{}, provider.FailMalformedResponse
	}

	nativeCopy, ok := cloneAndValidateNativeMetrics(ans.NativeMetrics)
	if !ok {
		return AnswerRecord{}, provider.FailMalformedResponse
	}

	rec := AnswerRecord{
		QuestionID:    q.ID,
		Primitive:     q.Type,
		Status:        ans.Status,
		Required:      required,
		NativeMetrics: nativeCopy,
		ReportedModel: ans.ReportedModel,
		Notes:         copyStrings(ans.Notes),
	}

	lim := caps.Primitives[q.Type]
	expectCompleteDist := lim.DistributionCompleteness == spec.CompletenessComplete ||
		(lim.DistributionCompleteness == "" && caps.ReturnsDistribution)

	switch ans.Status {
	case provider.AnswerStatusRefused:
		if ans.Bool != nil || ans.Choice != nil || ans.Level != nil || ans.ExpectedIndex != nil {
			return AnswerRecord{}, provider.FailMalformedResponse
		}
		if len(ans.Distribution) > 0 {
			distCopy, portable, errReason := validateQuestionDistribution(q, ans.Distribution)
			if errReason != "" {
				return AnswerRecord{}, errReason
			}
			rec.Distribution = distCopy
			rec.Metrics = portable
		}
		return rec, ""

	case provider.AnswerStatusAbstained:
		if ans.Bool != nil || ans.Choice != nil || ans.Level != nil || ans.ExpectedIndex != nil {
			return AnswerRecord{}, provider.FailMalformedResponse
		}
		rec.Abstained = true
		if len(ans.Distribution) > 0 {
			distCopy, portable, errReason := validateQuestionDistribution(q, ans.Distribution)
			if errReason != "" {
				return AnswerRecord{}, errReason
			}
			rec.Distribution = distCopy
			rec.Metrics = portable
		}
		return rec, ""

	case provider.AnswerStatusAnswered:
		switch q.Type {
		case spec.PrimitiveBinary:
			return populateBinaryAnswer(rec, expectCompleteDist, ans)
		case spec.PrimitiveChoice:
			return populateChoiceAnswer(q, rec, expectCompleteDist, ans)
		default:
			return AnswerRecord{}, provider.FailUnsupported
		}

	default:
		return AnswerRecord{}, provider.FailMalformedResponse
	}
}

func populateBinaryAnswer(
	rec AnswerRecord,
	expectCompleteDist bool,
	ans provider.Answer,
) (AnswerRecord, provider.FailReason) {
	if ans.Choice != nil || ans.Level != nil || ans.ExpectedIndex != nil {
		return AnswerRecord{}, provider.FailMalformedResponse
	}

	if len(ans.Distribution) > 0 || expectCompleteDist {
		declared := []string{provider.BinaryTrueKey, provider.BinaryFalseKey}
		if err := provider.ValidateDistribution(ans.Distribution, declared); err != nil {
			return AnswerRecord{}, provider.FailMalformedResponse
		}
		portable, err := metrics.Compute(ans.Distribution)
		if err != nil {
			return AnswerRecord{}, provider.FailMalformedResponse
		}
		pTrue := ans.Distribution[provider.BinaryTrueKey]
		pFalse := ans.Distribution[provider.BinaryFalseKey]
		isTie := math.Abs(pTrue-pFalse) <= provider.TieTolerance
		if isTie {
			if ans.Bool != nil {
				return AnswerRecord{}, provider.FailMalformedResponse
			}
			rec.Tied = true
		} else {
			if ans.Tied {
				return AnswerRecord{}, provider.FailMalformedResponse
			}
			want := pTrue > pFalse
			if ans.Bool != nil && *ans.Bool != want {
				return AnswerRecord{}, provider.FailMalformedResponse
			}
			rec.Bool = &want
		}
		rec.Distribution = copyDistribution(ans.Distribution)
		rec.Metrics = portable
		return rec, ""
	}

	if ans.Tied {
		if ans.Bool != nil {
			return AnswerRecord{}, provider.FailMalformedResponse
		}
		rec.Tied = true
		return rec, ""
	}
	if ans.Bool == nil {
		return AnswerRecord{}, provider.FailMalformedResponse
	}
	b := *ans.Bool
	rec.Bool = &b
	return rec, ""
}

func populateChoiceAnswer(
	q spec.Question,
	rec AnswerRecord,
	expectCompleteDist bool,
	ans provider.Answer,
) (AnswerRecord, provider.FailReason) {
	if ans.Bool != nil || ans.Level != nil || ans.ExpectedIndex != nil {
		return AnswerRecord{}, provider.FailMalformedResponse
	}
	if len(q.Options) < 2 {
		return AnswerRecord{}, provider.FailMalformedResponse
	}

	declared := make([]string, len(q.Options))
	validOpts := make(map[string]spec.Option, len(q.Options))
	for i, opt := range q.Options {
		declared[i] = opt.ID
		validOpts[opt.ID] = opt
	}

	// Enforce closed candidate set before and independent of distribution checks
	// (runtime-design §5, Stage 1 §7.0a item 3, AC-6).
	if ans.Choice != nil {
		if _, ok := validOpts[*ans.Choice]; !ok {
			return AnswerRecord{}, provider.FailMalformedResponse
		}
	}

	if len(ans.Distribution) > 0 || expectCompleteDist {
		if err := provider.ValidateDistribution(ans.Distribution, declared); err != nil {
			return AnswerRecord{}, provider.FailMalformedResponse
		}
		portable, err := metrics.Compute(ans.Distribution)
		if err != nil {
			return AnswerRecord{}, provider.FailMalformedResponse
		}
		winner, isTie := provider.ArgmaxChoice(ans.Distribution)
		if isTie {
			if ans.Choice != nil {
				return AnswerRecord{}, provider.FailMalformedResponse
			}
			rec.Tied = true
		} else {
			if ans.Tied {
				return AnswerRecord{}, provider.FailMalformedResponse
			}
			if ans.Choice != nil && *ans.Choice != winner {
				return AnswerRecord{}, provider.FailMalformedResponse
			}
			c := winner
			rec.Choice = &c
			if validOpts[winner].Abstain {
				rec.Abstained = true
			}
		}
		rec.Distribution = copyDistribution(ans.Distribution)
		rec.Metrics = portable
		return rec, ""
	}

	if ans.Tied {
		if ans.Choice != nil {
			return AnswerRecord{}, provider.FailMalformedResponse
		}
		rec.Tied = true
		return rec, ""
	}
	if ans.Choice == nil {
		return AnswerRecord{}, provider.FailMalformedResponse
	}
	c := *ans.Choice
	rec.Choice = &c
	if validOpts[c].Abstain {
		rec.Abstained = true
	}
	return rec, ""
}

func validateQuestionDistribution(
	q spec.Question,
	dist map[string]float64,
) (map[string]float64, map[string]metrics.Metric, provider.FailReason) {
	var declared []string
	switch q.Type {
	case spec.PrimitiveBinary:
		declared = []string{provider.BinaryTrueKey, provider.BinaryFalseKey}
	case spec.PrimitiveChoice:
		declared = make([]string, len(q.Options))
		for i, opt := range q.Options {
			declared[i] = opt.ID
		}
	default:
		return nil, nil, provider.FailUnsupported
	}
	if err := provider.ValidateDistribution(dist, declared); err != nil {
		return nil, nil, provider.FailMalformedResponse
	}
	portable, err := metrics.Compute(dist)
	if err != nil {
		return nil, nil, provider.FailMalformedResponse
	}
	return copyDistribution(dist), portable, ""
}

func projectDecision(compiled *spec.Compiled, answers map[string]AnswerRecord) (Decision, Class, string) {
	zero := Decision{Kind: compiled.Kind}

	if compiled.Kind == spec.KindJudgments {
		return Decision{Kind: spec.KindJudgments}, ClassSuccess, ""
	}

	if compiled.Spec.Output == nil || compiled.Spec.Output.Question == "" {
		return zero, ClassFailed, string(provider.FailInvalidRequest)
	}
	outQID := compiled.Spec.Output.Question
	outQ, ok := compiled.Spec.QuestionByID(outQID)
	if !ok {
		return zero, ClassFailed, string(provider.FailInvalidRequest)
	}
	rec, ok := answers[outQID]
	if !ok {
		return zero, ClassFailed, string(provider.FailMalformedResponse)
	}
	if rec.Status == provider.AnswerStatusRefused {
		return zero, ClassFailed, string(provider.FailProviderRefused)
	}
	if rec.Abstained {
		return zero, ClassUncertain, provider.UncertainAbstained
	}
	if rec.Tied {
		return zero, ClassUncertain, provider.UncertainTie
	}

	switch compiled.Kind {
	case spec.KindFilter:
		switch outQ.Type {
		case spec.PrimitiveBinary:
			if rec.Bool == nil {
				return zero, ClassFailed, string(provider.FailMalformedResponse)
			}
			return Decision{Kind: spec.KindFilter, Bool: *rec.Bool}, ClassSuccess, ""
		case spec.PrimitiveChoice:
			if rec.Choice == nil || len(compiled.Spec.Output.Match) == 0 {
				return zero, ClassFailed, string(provider.FailMalformedResponse)
			}
			if !hasDeclaredOption(outQ.Options, *rec.Choice) {
				return zero, ClassFailed, string(provider.FailMalformedResponse)
			}
			return Decision{
				Kind: spec.KindFilter,
				Bool: slices.Contains(compiled.Spec.Output.Match, *rec.Choice),
			}, ClassSuccess, ""
		default:
			return zero, ClassFailed, string(provider.FailUnsupported)
		}

	case spec.KindJudgmentBool:
		if outQ.Type != spec.PrimitiveBinary || rec.Bool == nil {
			return zero, ClassFailed, string(provider.FailMalformedResponse)
		}
		return Decision{Kind: spec.KindJudgmentBool, Bool: *rec.Bool}, ClassSuccess, ""

	case spec.KindRoute, spec.KindJudgmentChoice:
		if outQ.Type != spec.PrimitiveChoice || rec.Choice == nil {
			return zero, ClassFailed, string(provider.FailMalformedResponse)
		}
		if !hasDeclaredOption(outQ.Options, *rec.Choice) {
			return zero, ClassFailed, string(provider.FailMalformedResponse)
		}
		return Decision{Kind: compiled.Kind, String: *rec.Choice}, ClassSuccess, ""

	default:
		return zero, ClassFailed, string(provider.FailUnsupported)
	}
}

func hasDeclaredOption(opts []spec.Option, id string) bool {
	for _, opt := range opts {
		if opt.ID == id {
			return true
		}
	}
	return false
}

func copyDistribution(dist map[string]float64) map[string]float64 {
	if len(dist) == 0 {
		return nil
	}
	cp := make(map[string]float64, len(dist))
	for k, v := range dist {
		cp[k] = v
	}
	return cp
}

func cloneAndValidateNativeMetrics(in map[string]metrics.Metric) (map[string]metrics.Metric, bool) {
	if len(in) == 0 {
		return nil, true
	}
	cp := make(map[string]metrics.Metric, len(in))
	for k, m := range in {
		if k == "" || metrics.IsPortableMetric(k) {
			return nil, false
		}
		if math.IsNaN(m.Value) || math.IsInf(m.Value, 0) || math.IsNaN(m.Min) || math.IsInf(m.Min, 0) || math.IsNaN(m.Max) || math.IsInf(m.Max, 0) {
			return nil, false
		}
		cp[k] = m
	}
	return cp, true
}

func copyStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	return append([]string(nil), in...)
}
