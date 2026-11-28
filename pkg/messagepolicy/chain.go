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

package messagepolicy

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/celengine"
	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone"
)

// Consumer verdicts emitted by the message-policy ChainRun StepPolicy (SP1 §1.4).
// These are kept strictly separate from ruleengine.StepClass.
const (
	ChainVerdictAllow      = "allow"
	ChainVerdictSkipPolicy = "skip_policy"
	ChainVerdictReject     = "reject"
	ChainVerdictError      = "error"

	// ConsumerHandling tokens recorded on ChainDecisionProvenance (SP1 §1.4).
	HandlingContinue        = "continue"
	HandlingSkipPolicy      = "skip_policy"
	HandlingRejectViolation = "reject:violation"
	HandlingRejectMapping   = "reject:mapping"
	HandlingOutcomeFailed   = "outcome:on_failed:"
	HandlingOutcomeUncert   = "outcome:on_uncertain:"
)

// ChainStepInput is one authored step passed into the P1 ChainRun adapter.
type ChainStepInput struct {
	ID          string
	Role        string
	Dialect     string
	Expr        string
	Reason      string
	Remediation string
}

// ChainDecidingStep identifies the step that terminated a chain evaluation.
type ChainDecidingStep struct {
	Index int    `json:"index"`
	ID    string `json:"id"`
	Role  string `json:"role"`
}

// ChainDecisionStepTrace is one content-free step entry inside the additive
// message_policy_decisions.chain JSON column (SP1 §1.5).
type ChainDecisionStepTrace struct {
	Index      int    `json:"i"`
	ID         string `json:"id"`
	Role       string `json:"role"`
	Dialect    string `json:"dialect"`
	Class      string `json:"class"`
	Reason     string `json:"reason,omitempty"`
	DurationMs int64  `json:"duration_ms"`
}

// ChainDecisionProvenance is the versioned, content-free JSON document stored
// in message_policy_decisions.chain (SP1 §1.5). It never contains message body
// text, prompt text, raw traces, or judgment content, and keeps engine_class
// strictly separate from consumer_handling and verdict.
type ChainDecisionProvenance struct {
	Version          int                      `json:"v"`
	Source           string                   `json:"source"`
	Through          int                      `json:"through"`
	Deciding         *ChainDecidingStep       `json:"deciding,omitempty"`
	EngineClass      string                   `json:"engine_class"`
	ConsumerHandling string                   `json:"consumer_handling"`
	Verdict          string                   `json:"verdict"`
	Steps            []ChainDecisionStepTrace `json:"steps"`
	Unexecuted       []int                    `json:"unexecuted,omitempty"`
}

// MarshalChainProvenance encodes prov as compact JSON, returning nil when prov
// is nil.
func MarshalChainProvenance(prov *ChainDecisionProvenance) json.RawMessage {
	if prov == nil {
		return nil
	}
	b, err := json.Marshal(prov)
	if err != nil {
		return nil
	}
	return json.RawMessage(b)
}

// MapLegacyToSteps returns the read-time chain projection of a legacy policy
// (SP1 §1.3): [judgment?, violation]. Legacy rows are never rewritten.
func MapLegacyToSteps(violationRule, systemOneRule string) []ChainStepSpec {
	var out []ChainStepSpec
	if strings.TrimSpace(systemOneRule) != "" {
		out = append(out, ChainStepSpec{
			ID:        "judgment",
			Role:      StepRoleJudgment,
			Dialect:   StepDialectSystemOne,
			SystemOne: json.RawMessage(systemOneRule),
		})
	}
	out = append(out, ChainStepSpec{
		ID:      "violation",
		Role:    StepRoleViolation,
		Dialect: StepDialectCEL,
		Expr:    violationRule,
	})
	return out
}

// IsP1ChainSupported reports whether steps can be evaluated by the P1
// CEL-only ChainRun adapter (non-empty, within cap, every step CEL guard or
// violation).
func IsP1ChainSupported(steps []ChainStepInput) bool {
	if len(steps) == 0 || len(steps) > MaxChainSteps {
		return false
	}
	for _, st := range steps {
		if st.Dialect != StepDialectCEL {
			return false
		}
		if st.Role != StepRoleGuard && st.Role != StepRoleViolation {
			return false
		}
	}
	return true
}

// BuildChainEnricherMap builds the strictly JSON-shaped `chain` map for step
// from prior passing judgments in j (SEC-S2-L1: maps, slices, strings, numbers,
// bools only; no structs or []byte). P1 uses ruleengine.NoopEnricher{} because
// there are no judgment steps in P1; this helper is retained and tested for
// SEC-S2-L1 conformance ahead of P4+P5.
func BuildChainEnricherMap(steps []ChainStepInput, j ruleengine.JudgmentView) map[string]any {
	chainMap := make(map[string]any)
	if j == nil {
		return chainMap
	}
	for _, idx := range j.Steps() {
		if idx < 0 || idx >= len(steps) {
			continue
		}
		jdg, ok := j.Get(idx)
		if !ok {
			continue
		}
		eval, _ := jdg.Detail.(*systemone.Evaluation)
		extracted := ExtractJudgments(eval)

		flags := make(map[string]any, len(extracted.Flags))
		for k, v := range extracted.Flags {
			flags[k] = v
		}
		labels := make(map[string]any, len(extracted.Labels))
		for k, v := range extracted.Labels {
			labels[k] = v
		}
		scores := make(map[string]any)
		chainMap[steps[idx].ID] = map[string]any{
			"flags":  flags,
			"labels": labels,
			"scores": scores,
		}
	}
	return chainMap
}

// EvaluateChain evaluates a P1 CEL-only ordered rule chain through
// ruleengine.ChainRun using pre-compiled CEL elements (never ChainElementSource
// in P1) and Amendment A1 domains (Inline = request ctx + send CEL tracker).
//
// P1 boundary: every step must be CEL (PerformsIO == false); EvaluateChain
// never constructs I/O elements, never calls EvaluatePending/EvaluateGroup, and
// fails closed if a PendingStep is ever returned.
func (e *Evaluator) EvaluateChain(
	ctx context.Context,
	policyID string,
	revision int64,
	steps []ChainStepInput,
	chainRejectReason string,
	chainRejectRemediation string,
	facts Facts,
	opts ...EvalOption,
) Classification {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return ClassifyError(&ruleengine.FailedError{Reason: celengine.ReasonCanceled, Cause: err})
	}
	if !IsP1ChainSupported(steps) {
		return Classification{
			Class:  ClassFailed,
			Reason: ReasonChainUnsupported,
		}
	}
	cfg := applyEvalOpts(opts)
	if cfg.tracker != nil && cfg.tracker.Exhausted() {
		return ClassifyError(ruleengine.ErrBudgetExhausted)
	}

	// Pre-compile every CEL step via CachedCompile before NewChainRun (brief P1
	// boundary: pre-compiled Compiled elements, NOT ChainElementSource).
	elements := make([]ruleengine.ChainElement, len(steps))
	scope := ScopeForPolicy(policyID, revision)
	envRef := ChainStepEnvironmentRef()
	for i, st := range steps {
		compileStart := time.Now()
		compiled, err := e.engine.CachedCompile(
			ctx,
			ruleengine.DialectCEL,
			scope,
			st.Expr,
			ruleengine.KindFilter,
			envRef,
		)
		if cfg.tracker != nil {
			cfg.tracker.Charge(time.Since(compileStart))
		}
		if err != nil {
			return ClassifyError(err)
		}
		elements[i] = ruleengine.ChainElement{
			Index:    i,
			Dialect:  ruleengine.DialectCEL,
			Kind:     ruleengine.KindFilter,
			Compiled: compiled,
			Role:     st.Role,
		}
	}

	stepPolicy := func(out ruleengine.StepOutcome) ruleengine.Control {
		if out.Index < 0 || out.Index >= len(steps) {
			return ruleengine.Control{Stop: true, Verdict: ChainVerdictError}
		}
		role := steps[out.Index].Role
		switch out.Class {
		case ruleengine.StepClassPass:
			switch role {
			case StepRoleGuard:
				return ruleengine.Control{Stop: false}
			case StepRoleViolation:
				return ruleengine.Control{Stop: true, Verdict: ChainVerdictReject}
			default:
				return ruleengine.Control{Stop: true, Verdict: ChainVerdictError}
			}
		case ruleengine.StepClassFail:
			switch role {
			case StepRoleGuard:
				return ruleengine.Control{Stop: true, Verdict: ChainVerdictSkipPolicy}
			case StepRoleViolation:
				return ruleengine.Control{Stop: false}
			default:
				return ruleengine.Control{Stop: true, Verdict: ChainVerdictError}
			}
		default:
			// Uncertain, failed, budget, canceled: terminal stop.
			return ruleengine.Control{Stop: true, Verdict: ChainVerdictError}
		}
	}

	inlineDom := ruleengine.Domain{
		Ctx: ctx,
		Budget: ruleengine.Budget{
			MaxEvalTime: cfg.maxEvalTime,
			Tracker:     cfg.tracker,
		},
	}
	ioDom := ruleengine.Domain{
		Ctx: ctx,
	}

	act := NewChainBaseActivation(facts)
	run, err := e.engine.NewChainRun(
		ruleengine.ChainDomains{Inline: inlineDom, IO: ioDom},
		ruleengine.Chain{Elements: elements},
		ruleengine.Input{Activation: act.Map()},
		ruleengine.ChainLimits{
			MaxElements:   MaxChainSteps,
			MaxTraceBytes: 16 * 1024,
		},
		stepPolicy,
		ruleengine.NoopEnricher{},
		nil,
	)
	if err != nil {
		return ClassifyError(err)
	}

	for {
		_, pending, done, nextErr := run.Next()
		if nextErr != nil {
			return ClassifyError(nextErr)
		}
		if pending != nil {
			// P1 invariant: CEL-only chains never issue a PendingStep.
			return ClassifyError(errors.New("messagepolicy: unexpected pending step on P1 CEL-only chain"))
		}
		if done {
			break
		}
	}

	res, _ := run.Result()
	return classifyChainResult(res, steps, chainRejectReason, chainRejectRemediation)
}

func classifyChainResult(
	res ruleengine.ChainResult,
	steps []ChainStepInput,
	chainRejectReason string,
	chainRejectRemediation string,
) Classification {
	stepTraces := make([]ChainDecisionStepTrace, 0, len(res.Steps))
	for _, s := range res.Steps {
		var id, role string
		if s.Index >= 0 && s.Index < len(steps) {
			id = steps[s.Index].ID
			role = steps[s.Index].Role
		}
		durMs := s.Elapsed.Milliseconds()
		if durMs < 0 {
			durMs = 0
		}
		stepTraces = append(stepTraces, ChainDecisionStepTrace{
			Index:      s.Index,
			ID:         id,
			Role:       role,
			Dialect:    s.Dialect,
			Class:      string(s.Class),
			Reason:     s.Reason,
			DurationMs: durMs,
		})
	}

	prov := &ChainDecisionProvenance{
		Version:    ChainVersion,
		Source:     ChainSourceAuthored,
		Through:    res.Through,
		Steps:      stepTraces,
		Unexecuted: res.Unexecuted,
	}

	if res.Stopped {
		dec := res.Deciding
		var decStep ChainStepInput
		if dec.Index >= 0 && dec.Index < len(steps) {
			decStep = steps[dec.Index]
			prov.Deciding = &ChainDecidingStep{
				Index: dec.Index,
				ID:    decStep.ID,
				Role:  decStep.Role,
			}
		}
		prov.EngineClass = string(dec.Class)

		switch res.Verdict {
		case ChainVerdictReject:
			prov.ConsumerHandling = HandlingRejectViolation
			prov.Verdict = ChainVerdictReject
			reason := decStep.Reason
			if strings.TrimSpace(reason) == "" {
				reason = chainRejectReason
			}
			remediation := decStep.Remediation
			if strings.TrimSpace(remediation) == "" {
				remediation = chainRejectRemediation
			}
			return Classification{
				Class:               ClassViolation,
				ChainProvenance:     prov,
				DecidingReason:      reason,
				DecidingRemediation: remediation,
			}

		case ChainVerdictSkipPolicy:
			prov.ConsumerHandling = HandlingSkipPolicy
			prov.Verdict = ChainVerdictSkipPolicy
			return Classification{
				Class:           ClassPass,
				ChainProvenance: prov,
			}

		default:
			cls := mapEngineStepError(dec)
			if cls.Class == ClassUncertain {
				prov.ConsumerHandling = HandlingOutcomeUncert + cls.Reason
			} else {
				prov.ConsumerHandling = HandlingOutcomeFailed + cls.Reason
			}
			prov.Verdict = ChainVerdictError
			cls.ChainProvenance = prov
			return cls
		}
	}

	// Reached end of chain without stopping: allow (this policy only).
	if len(res.Steps) > 0 {
		prov.EngineClass = string(res.Steps[len(res.Steps)-1].Class)
	} else {
		prov.EngineClass = string(ruleengine.StepClassPass)
	}
	prov.ConsumerHandling = HandlingContinue
	prov.Verdict = ChainVerdictAllow
	return Classification{
		Class:           ClassPass,
		ChainProvenance: prov,
	}
}

func mapEngineStepError(dec ruleengine.StepOutcome) Classification {
	switch dec.Class {
	case ruleengine.StepClassBudget:
		return Classification{
			Class:  ClassFailed,
			Reason: ReasonBudgetExhausted,
		}
	case ruleengine.StepClassCanceled:
		return Classification{
			Class:  ClassFailed,
			Reason: celengine.ReasonCanceled,
		}
	case ruleengine.StepClassUncertain:
		reason := dec.Reason
		if reason == "" {
			reason = "uncertain"
		}
		return Classification{
			Class:  ClassUncertain,
			Reason: reason,
		}
	default:
		if dec.Err != nil {
			return ClassifyError(dec.Err)
		}
		reason := dec.Reason
		if reason == "" {
			reason = celengine.ReasonRuntime
		}
		return Classification{
			Class:  ClassFailed,
			Reason: reason,
		}
	}
}
