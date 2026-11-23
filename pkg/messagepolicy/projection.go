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
	"encoding/json"
	"maps"
	"slices"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/systemone"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/spec"
)

// RuleRefProjection is the content-free rule metadata persisted on a message
// policy decision record (consumer design §5.8).
type RuleRefProjection struct {
	Consumer    string `json:"consumer,omitempty"`
	RuleID      string `json:"rule_id,omitempty"`
	Revision    int64  `json:"revision,omitempty"`
	ContentHash string `json:"content_hash,omitempty"`
	Dialect     string `json:"dialect,omitempty"`
	Native      bool   `json:"native,omitempty"`
}

// MetricProjection records a content-free uncertainty metric value and unit.
type MetricProjection struct {
	Name  string  `json:"name"`
	Value float64 `json:"value"`
	Unit  string  `json:"unit,omitempty"`
}

// AnswerProjection is the content-free per-question answer persisted in a
// decision record (consumer design §5.8).
type AnswerProjection struct {
	QuestionID    string                      `json:"question_id"`
	Primitive     string                      `json:"primitive"`
	Status        string                      `json:"status"`
	Required      bool                        `json:"required"`
	Abstained     bool                        `json:"abstained,omitempty"`
	Tied          bool                        `json:"tied,omitempty"`
	Bool          *bool                       `json:"bool,omitempty"`
	Choice        *string                     `json:"choice,omitempty"`
	Distribution  map[string]float64          `json:"distribution,omitempty"`
	Metrics       map[string]MetricProjection `json:"metrics,omitempty"`
	NativeMetrics map[string]MetricProjection `json:"native_metrics,omitempty"`
	ReportedModel string                      `json:"reported_model,omitempty"`
	Notes         []string                    `json:"notes,omitempty"`
}

// AttemptProjection is the content-free provider invocation attempt persisted
// in a decision record (consumer design §5.8).
type AttemptProjection struct {
	Instance         string `json:"instance,omitempty"`
	InstanceRevision int64  `json:"instance_revision,omitempty"`
	Adapter          string `json:"adapter,omitempty"`
	AdapterVersion   string `json:"adapter_version,omitempty"`
	ConfiguredModel  string `json:"configured_model,omitempty"`
	ReportedModel    string `json:"reported_model,omitempty"`
	Location         string `json:"location,omitempty"`
	DurationMs       int64  `json:"duration_ms"`
	Requests         int    `json:"requests,omitempty"`
	Retries          int    `json:"retries,omitempty"`
	Outcome          string `json:"outcome,omitempty"`
	Reason           string `json:"reason,omitempty"`
	HTTPStatusClass  string `json:"http_status_class,omitempty"`
}

// InputFieldProjection records only the name, byte length, and truncation flag
// of a selected input field — never its value and never an input hash
// (consumer design §5.8).
type InputFieldProjection struct {
	Name      string `json:"name"`
	Bytes     int    `json:"bytes"`
	Truncated bool   `json:"truncated,omitempty"`
}

// EvaluationProjection is the content-free System One evaluation projection
// persisted on message_policy_decisions.evaluation (consumer design §5.8).
//
// Invariant: carries NO message body text, NO input SHA-256 hash, NO secrets,
// and NO auth/credential metadata.
type EvaluationProjection struct {
	EvalID          string                      `json:"eval_id,omitempty"`
	Rule            RuleRefProjection           `json:"rule"`
	Class           string                      `json:"class"`
	Reason          string                      `json:"reason,omitempty"`
	ConfiguredModel string                      `json:"configured_model,omitempty"`
	ResolvedModel   string                      `json:"resolved_model,omitempty"`
	ModelUnreported bool                        `json:"model_unreported,omitempty"`
	Instance        string                      `json:"instance,omitempty"`
	Answers         map[string]AnswerProjection `json:"answers,omitempty"`
	Attempts        []AttemptProjection         `json:"attempts,omitempty"`
	FallbackUsed    bool                        `json:"fallback_used,omitempty"`
	FallbackCause   string                      `json:"fallback_cause,omitempty"`
	CacheHit        bool                        `json:"cache_hit,omitempty"`
	RevalidationDue bool                        `json:"revalidation_due,omitempty"`
	InputFields     []InputFieldProjection      `json:"input_fields,omitempty"`
	Truncated       bool                        `json:"truncated,omitempty"`
	ElapsedMs       int64                       `json:"elapsed_ms"`
}

// ProjectEvaluation converts a *systemone.Evaluation into its content-free
// decision-record projection (consumer design §5.8). Returns nil when eval is nil.
func ProjectEvaluation(eval *systemone.Evaluation) *EvaluationProjection {
	if eval == nil {
		return nil
	}
	elapsedMs := eval.Elapsed.Milliseconds()
	if elapsedMs < 0 {
		elapsedMs = 0
	}

	proj := &EvaluationProjection{
		EvalID: eval.EvalID,
		Rule: RuleRefProjection{
			Consumer:    eval.Rule.Consumer,
			RuleID:      eval.Rule.RuleID,
			Revision:    eval.Rule.Revision,
			ContentHash: eval.Rule.ContentHash,
			Dialect:     eval.Rule.Dialect,
			Native:      eval.Rule.Native,
		},
		Class:           string(eval.Class),
		Reason:          eval.Reason,
		ConfiguredModel: eval.ConfiguredModel,
		ResolvedModel:   eval.ResolvedModel,
		ModelUnreported: eval.HasModelUnreported(),
		FallbackUsed:    eval.FallbackUsed,
		FallbackCause:   eval.FallbackCause,
		CacheHit:        eval.CacheHit,
		RevalidationDue: eval.RevalidationDue,
		ElapsedMs:       elapsedMs,
	}

	if len(eval.Answers) > 0 {
		proj.Answers = make(map[string]AnswerProjection, len(eval.Answers))
		for id, ans := range eval.Answers {
			ap := AnswerProjection{
				QuestionID:    ans.QuestionID,
				Primitive:     ans.Primitive,
				Status:        ans.Status,
				Required:      ans.Required,
				Abstained:     ans.Abstained,
				Tied:          ans.Tied,
				ReportedModel: ans.ReportedModel,
			}
			if ans.Bool != nil {
				v := *ans.Bool
				ap.Bool = &v
			}
			if ans.Choice != nil {
				v := *ans.Choice
				ap.Choice = &v
			}
			if len(ans.Distribution) > 0 {
				ap.Distribution = make(map[string]float64, len(ans.Distribution))
				maps.Copy(ap.Distribution, ans.Distribution)
			}
			if len(ans.Metrics) > 0 {
				ap.Metrics = make(map[string]MetricProjection, len(ans.Metrics))
				for k, m := range ans.Metrics {
					ap.Metrics[k] = MetricProjection{Name: k, Value: m.Value, Unit: m.Unit}
				}
			}
			if len(ans.NativeMetrics) > 0 {
				ap.NativeMetrics = make(map[string]MetricProjection, len(ans.NativeMetrics))
				for k, m := range ans.NativeMetrics {
					ap.NativeMetrics[k] = MetricProjection{Name: k, Value: m.Value, Unit: m.Unit}
				}
			}
			if len(ans.Notes) > 0 {
				ap.Notes = slices.Clone(ans.Notes)
			}
			proj.Answers[id] = ap
		}
	}

	if len(eval.Attempts) > 0 {
		proj.Attempts = make([]AttemptProjection, 0, len(eval.Attempts))
		for _, att := range eval.Attempts {
			durMs := att.Duration.Milliseconds()
			if durMs < 0 {
				durMs = 0
			}
			proj.Attempts = append(proj.Attempts, AttemptProjection{
				Instance:         att.Instance,
				InstanceRevision: att.InstanceRevision,
				Adapter:          att.Adapter,
				AdapterVersion:   att.AdapterVersion,
				ConfiguredModel:  att.ConfiguredModel,
				ReportedModel:    att.ReportedModel,
				Location:         att.Location,
				DurationMs:       durMs,
				Requests:         att.Requests,
				Retries:          att.Retries,
				Outcome:          att.Outcome,
				Reason:           att.Reason,
				HTTPStatusClass:  att.HTTPStatusClass,
			})
			if att.Instance != "" {
				proj.Instance = att.Instance
			}
		}
	}
	if proj.Instance == "" && eval.Requested.Instance != "" {
		proj.Instance = eval.Requested.Instance
	}

	if len(eval.Input.Fields) > 0 {
		proj.InputFields = make([]InputFieldProjection, 0, len(eval.Input.Fields))
		for _, f := range eval.Input.Fields {
			proj.InputFields = append(proj.InputFields, InputFieldProjection{
				Name:      f.Name,
				Bytes:     f.Bytes,
				Truncated: f.Truncated,
			})
			if f.Truncated {
				proj.Truncated = true
			}
		}
	}

	return proj
}

// MarshalEvaluationProjection serializes ProjectEvaluation(eval) as JSON for
// persistence on store.MessagePolicyDecision.Evaluation. Returns nil when eval
// is nil.
func MarshalEvaluationProjection(eval *systemone.Evaluation) json.RawMessage {
	proj := ProjectEvaluation(eval)
	if proj == nil {
		return nil
	}
	raw, err := json.Marshal(proj)
	if err != nil {
		return nil
	}
	return json.RawMessage(raw)
}

// ExtractJudgments builds typed judgment maps (Flags and Labels only; Scores
// stays empty until M2b) from a successful *systemone.Evaluation. Optional
// questions that were abstained, refused, or omitted are excluded so reading
// them in CEL yields missing_input -> ClassFailed (consumer design §5.4).
func ExtractJudgments(eval *systemone.Evaluation) Judgments {
	if eval == nil || len(eval.Answers) == 0 {
		return Judgments{}
	}
	var (
		flags  map[string]bool
		labels map[string]string
	)
	for id, ans := range eval.Answers {
		if ans.Status != provider.AnswerStatusAnswered || ans.Abstained {
			continue
		}
		switch ans.Primitive {
		case spec.PrimitiveBinary:
			if ans.Bool != nil {
				if flags == nil {
					flags = make(map[string]bool)
				}
				flags[id] = *ans.Bool
			}
		case spec.PrimitiveChoice:
			if ans.Choice != nil {
				if labels == nil {
					labels = make(map[string]string)
				}
				labels[id] = *ans.Choice
			}
		}
	}
	return Judgments{
		Flags:  flags,
		Labels: labels,
	}
}

// ExtractSystemOneRefs returns the sorted, deduplicated instance and pool names
// referenced by a System One rule's binding (consumer design §5.3). Returns nil
// when rawRule is empty or declares no explicit binding reference.
func ExtractSystemOneRefs(rawRule string) []string {
	trimmed := strings.TrimSpace(rawRule)
	if trimmed == "" {
		return nil
	}
	var env spec.Envelope
	if err := json.Unmarshal([]byte(trimmed), &env); err == nil && (env.Spec.Version != 0 || len(env.Spec.Questions) > 0 || env.Spec.Binding.Instance != "" || env.Spec.Binding.Pool != "") {
		return refsFromBinding(env.Spec.Binding)
	}
	var sp spec.Spec
	if err := json.Unmarshal([]byte(trimmed), &sp); err == nil {
		return refsFromBinding(sp.Binding)
	}
	return nil
}

func refsFromBinding(b spec.Binding) []string {
	var refs []string
	if inst := strings.TrimSpace(b.Instance); inst != "" {
		refs = append(refs, inst)
	}
	if pool := strings.TrimSpace(b.Pool); pool != "" && !slices.Contains(refs, pool) {
		refs = append(refs, pool)
	}
	if len(refs) == 0 {
		return nil
	}
	slices.Sort(refs)
	return refs
}
