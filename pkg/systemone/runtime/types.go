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

// Package runtime implements bounded single-attempt execution, content-bounded
// field selection, deadline composition, outcome classification, and
// content-free provenance recording for System One model evaluations
// (runtime-design §§4.4, 6, 7.1, 7.7, 8.1; P1 brief §2).
package runtime

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/systemone/metrics"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/spec"
)

// DialectName is the canonical ruleengine dialect identifier for System One.
const DialectName = "system_one"

// Default execution bounds (runtime-design §§4.2, 7.1; P1 brief §2).
const (
	// DefaultMaxEvalTime is the default dialect MaxEvalTime ceiling (3 s).
	DefaultMaxEvalTime = 3 * time.Second
	// MaxAllowedEvalTime is the hard upper bound on MaxEvalTime and CallTimeout (30 s).
	MaxAllowedEvalTime = 30 * time.Second
	// DefaultRuleTimeout is the authoring default rule timeout when omitted (2 s).
	DefaultRuleTimeout = 2 * time.Second
	// DefaultCallTimeout is the default per-instance CallTimeout ceiling when unset (5 s).
	DefaultCallTimeout = 5 * time.Second
	// BytesPerTokenPrecheck is the loose bytes-per-token upper-bound guard used
	// for the pre-call MaxInputTokens check on non-silent adapters
	// (TruncationNever / TruncationReported). Multibyte UTF-8 text (CJK, emoji)
	// can run well over 4 bytes per token, and non-silent providers enforce token
	// limits authoritatively (e.g. TypeSafe 400 max_tokens_exceeded ->
	// input_too_large), so this guard rejects only totalInputBytes >
	// effectiveMaxTokens * 16 (runtime-design §4.3, §4.4; AC-3).
	BytesPerTokenPrecheck = 16
	// SilentBytesPerTokenPrecheck is the strict bytes-per-token lower bound (1
	// byte per token, since every token is >= 1 byte) used to prove non-truncation
	// before calling a TruncationSilent adapter (runtime-design §4.3, §6.3; AC-3).
	SilentBytesPerTokenPrecheck = 1
	// MaxProviderIdentifierBytes bounds provider-reported identifier strings
	// (ResolvedModel, ReportedModel, ProviderRequestID) before they enter
	// content-free Evaluation provenance or logs (runtime-design §8.1).
	MaxProviderIdentifierBytes = 128
	// MaxListedModels bounds the number of model identifiers returned by
	// Runtime.ListModels.
	MaxListedModels = 256
)

// Class is the top-level evaluation outcome class (runtime-design §6.1).
type Class string

const (
	ClassSuccess   Class = "success"
	ClassUncertain Class = "uncertain"
	ClassFailed    Class = "failed"
)

// Attempt outcome constants recorded in Attempt.Outcome.
const (
	AttemptSuccess   = "success"
	AttemptUncertain = "uncertain"
	AttemptFailed    = "failed"
)

// InstanceLimits holds the runtime execution bounds configured on an Instance
// (runtime-design §4.2).
type InstanceLimits struct {
	MaxInFlight    int           `json:"max_in_flight,omitempty"`
	RatePerSec     float64       `json:"rate_per_sec,omitempty"`
	MaxInputTokens int           `json:"max_input_tokens,omitempty"`
	CallTimeout    time.Duration `json:"call_timeout,omitempty"`
}

// InstanceAuth holds credential reference metadata (never secret values) for a
// configured provider instance (runtime-design §4.2, P1 brief §2).
type InstanceAuth struct {
	Kind       string   `json:"kind"`
	SecretName string   `json:"secret_name,omitempty"`
	HeaderName string   `json:"header_name,omitempty"`
	Scopes     []string `json:"scopes,omitempty"`
	Audience   string   `json:"audience,omitempty"`
}

// Instance represents a configured provider instance supplied to Runtime by the
// Hub store adapter or tests (runtime-design §4.2). It never holds secret values.
type Instance struct {
	ID       string            `json:"id,omitempty"`
	Name     string            `json:"name"`
	Adapter  string            `json:"adapter"`
	Model    string            `json:"model"`
	Endpoint string            `json:"endpoint"`
	Location string            `json:"location,omitempty"`
	Auth     InstanceAuth      `json:"auth"`
	Options  map[string]string `json:"options,omitempty"`
	Limits   InstanceLimits    `json:"limits,omitempty"`
	Enabled  bool              `json:"enabled"`
	Revision int64             `json:"revision"`
}

// View returns a defensive, credential-free provider.InstanceView projection of
// i suitable for passing to Adapter methods (AC-2).
func (i Instance) View() provider.InstanceView {
	var opts map[string]string
	if len(i.Options) > 0 {
		opts = make(map[string]string, len(i.Options))
		for k, v := range i.Options {
			opts[k] = v
		}
	}
	return provider.InstanceView{
		Name:     i.Name,
		Revision: i.Revision,
		Model:    i.Model,
		Endpoint: i.Endpoint,
		Location: i.Location,
		Options:  opts,
	}
}

// Input holds the consumer-supplied evaluation inputs (runtime-design §4.4).
// Runtime selects only fields declared in the compiled rule's Environment and
// referenced by Compiled.Inputs before building ModelRequest (AC-2, AC-3).
type Input struct {
	Fields     []provider.InputField `json:"fields,omitempty"`
	ChoiceSets map[string][]string   `json:"choice_sets,omitempty"`
	Params     map[string]string     `json:"params,omitempty"`
}

// ProbeInput constructs an Input for the built-in P1 synthetic probe
// environment containing the single untrusted text field "probe.text".
func ProbeInput(text string) Input {
	return Input{
		Fields: []provider.InputField{
			{
				Name:  spec.ProbeFieldText,
				Kind:  spec.FieldKindText,
				Value: text,
				Trust: spec.TrustUntrusted,
			},
		},
	}
}

// RuleRef identifies the evaluated rule in content-free provenance
// (runtime-design §8.1).
type RuleRef struct {
	Consumer     string `json:"consumer,omitempty"`
	RuleID       string `json:"rule_id,omitempty"`
	Revision     int64  `json:"revision,omitempty"`
	ElementIndex *int   `json:"element_index,omitempty"`
	ContentHash  string `json:"content_hash"`
	Dialect      string `json:"dialect"`
	Native       bool   `json:"native,omitempty"`
}

// AnswerRecord is the content-free per-question result recorded in
// Evaluation.Answers (runtime-design §8.1, provider-design §6.1).
type AnswerRecord struct {
	QuestionID    string                    `json:"question_id"`
	Primitive     string                    `json:"primitive"`
	Status        string                    `json:"status"`
	Required      bool                      `json:"required"`
	Abstained     bool                      `json:"abstained,omitempty"`
	Tied          bool                      `json:"tied,omitempty"`
	Bool          *bool                     `json:"bool,omitempty"`
	Choice        *string                   `json:"choice,omitempty"`
	Level         *string                   `json:"level,omitempty"`
	ExpectedIndex *float64                  `json:"expected_index,omitempty"`
	Distribution  map[string]float64        `json:"distribution,omitempty"`
	Metrics       map[string]metrics.Metric `json:"metrics,omitempty"`
	NativeMetrics map[string]metrics.Metric `json:"native_metrics,omitempty"`
	ReportedModel string                    `json:"reported_model,omitempty"`
	Notes         []string                  `json:"notes,omitempty"`
}

// ThresholdCheck records an uncertainty threshold evaluation (runtime-design §8.1;
// populated starting in P10/PV6).
type ThresholdCheck struct {
	Question  string  `json:"question"`
	Metric    string  `json:"metric"`
	Unit      string  `json:"unit"`
	Direction string  `json:"direction,omitempty"`
	Threshold float64 `json:"threshold"`
	Observed  float64 `json:"observed"`
	Crossed   bool    `json:"crossed"`
}

// Attempt records a single provider invocation attempt in content-free
// provenance (runtime-design §8.1, P1 brief §3).
type Attempt struct {
	Instance          string        `json:"instance"`
	InstanceRevision  int64         `json:"instance_revision"`
	Adapter           string        `json:"adapter"`
	AdapterVersion    string        `json:"adapter_version"`
	ConfiguredModel   string        `json:"configured_model,omitempty"`
	ReportedModel     string        `json:"reported_model,omitempty"`
	ProviderRequestID string        `json:"provider_request_id,omitempty"`
	Location          string        `json:"location,omitempty"`
	Started           time.Time     `json:"started"`
	Duration          time.Duration `json:"duration"`
	Requests          int           `json:"requests"`
	Retries           int           `json:"retries"`
	Outcome           string        `json:"outcome"`
	Reason            string        `json:"reason,omitempty"`
	HTTPStatusClass   string        `json:"http_status_class,omitempty"`
}

// FieldFingerprint records content-free size and truncation metadata for one
// selected input field (runtime-design §8.1). Field values are never stored.
type FieldFingerprint struct {
	Name      string `json:"name"`
	Bytes     int    `json:"bytes"`
	Truncated bool   `json:"truncated,omitempty"`
}

// InputFingerprint records content-free metadata for the selected input fields
// of an evaluation (runtime-design §8.1).
type InputFingerprint struct {
	Fields []FieldFingerprint `json:"fields,omitempty"`
	SHA256 string             `json:"sha256,omitempty"`
}

// SecretRefProvenance records content-free named-secret reference metadata for
// an evaluation (PV3 brief Addendum F §§F2, F7). Secret values are never stored here.
type SecretRefProvenance struct {
	Name      string `json:"name"`
	Scope     string `json:"scope,omitempty"`
	ScopeUsed string `json:"scope_used,omitempty"`
}

// AuthProvenance records content-free, non-secret authentication metadata for
// an evaluation (PV3 brief Addendum B §§B4, B6, B10; Addendum F §§F2, F7). Token
// and secret values are never stored here.
type AuthProvenance struct {
	AuthKind            string               `json:"auth_kind,omitempty"`
	SecretRef           *SecretRefProvenance `json:"secret_ref,omitempty"`
	SecretVersion       int                  `json:"secret_version,omitempty"`
	Scopes              []string             `json:"scopes,omitempty"`
	QuotaProject        string               `json:"quota_project,omitempty"`
	CredentialType      string               `json:"credential_type,omitempty"`
	ServiceAccountEmail string               `json:"service_account_email,omitempty"`
	ReasonHint          string               `json:"reason_hint,omitempty"`
}

// Evaluation is the content-free provenance record returned on every evaluation
// path (in Decision.Eval on Success and inside EvaluationError on Uncertain and
// Failed; runtime-design §8.1, P1 brief §1, §5).
type Evaluation struct {
	EvalID          string                  `json:"eval_id"`
	Rule            RuleRef                 `json:"rule"`
	ProjectID       string                  `json:"project_id,omitempty"`
	Requested       spec.Binding            `json:"requested"`
	ConfiguredModel string                  `json:"configured_model,omitempty"`
	ResolvedModel   string                  `json:"resolved_model,omitempty"`
	Auth            *AuthProvenance         `json:"auth,omitempty"`
	Class           Class                   `json:"class"`
	Reason          string                  `json:"reason,omitempty"`
	Answers         map[string]AnswerRecord `json:"answers,omitempty"`
	Uncertainty     []ThresholdCheck        `json:"uncertainty,omitempty"`
	Usage           *provider.Usage         `json:"usage,omitempty"`
	Notes           []string                `json:"notes,omitempty"`
	Attempts        []Attempt               `json:"attempts,omitempty"`
	FallbackUsed    bool                    `json:"fallback_used,omitempty"`
	FallbackCause   string                  `json:"fallback_cause,omitempty"`
	Input           InputFingerprint        `json:"input"`
	CacheHit        bool                    `json:"cache_hit,omitempty"`
	RevalidationDue bool                    `json:"revalidation_due,omitempty"`
	Elapsed         time.Duration           `json:"elapsed"`
}

// HasModelAliasReported reports whether the adapter flagged ResolvedModel with
// NoteModelAliasReported (for example when TypeSafe echoes "jev-latest").
func (e *Evaluation) HasModelAliasReported() bool {
	if e == nil {
		return false
	}
	return slices.Contains(e.Notes, provider.NoteModelAliasReported)
}

// HasModelUnreported reports whether Notes contains NoteModelUnreported
// (indicating that the provider did not report a serving model identifier).
func (e *Evaluation) HasModelUnreported() bool {
	if e == nil {
		return false
	}
	return slices.Contains(e.Notes, provider.NoteModelUnreported)
}

// HasUsageUnreported reports whether Notes contains NoteUsageUnreported
// (indicating that provider token usage was absent rather than measured zero).
func (e *Evaluation) HasUsageUnreported() bool {
	if e == nil {
		return false
	}
	return slices.Contains(e.Notes, provider.NoteUsageUnreported)
}

// EffectiveResolvedModel returns ResolvedModel when it identifies a concrete
// provider-reported model, or "" when Notes marks it as an echoed alias
// (NoteModelAliasReported) or unreported fallback (NoteModelUnreported) per
// runtime-design §8.1 and provider-design §7.4.
func (e *Evaluation) EffectiveResolvedModel() string {
	if e == nil {
		return ""
	}
	if slices.Contains(e.Notes, provider.NoteModelAliasReported) ||
		slices.Contains(e.Notes, provider.NoteModelUnreported) {
		return ""
	}
	return e.ResolvedModel
}

// Decision is the typed result returned by Runtime.Evaluate. On ClassSuccess,
// Bool or String holds the consumer Kind projection (for KindJudgments, all
// answers live in Eval.Answers). On ClassUncertain or ClassFailed, Evaluate
// returns an *EvaluationError carrying Eval alongside Decision.Eval.
type Decision struct {
	Kind   spec.Kind   `json:"kind"`
	Bool   bool        `json:"bool,omitempty"`
	String string      `json:"string,omitempty"`
	Eval   *Evaluation `json:"eval"`
}

// EvaluationError carries the content-free Evaluation provenance record on
// Uncertain and Failed evaluations so callers (and ruleengine error wrapping in
// P1b/P1-reg) can extract it with EvaluationFrom via errors.As (runtime-design
// §5, P1 brief §5).
type EvaluationError struct {
	Eval *Evaluation
}

// Error returns a concise, content-free summary of the Uncertain or Failed
// evaluation outcome.
func (e EvaluationError) Error() string {
	if e.Eval == nil {
		return "system_one evaluation error"
	}
	if e.Eval.Reason != "" {
		return fmt.Sprintf("system_one %s: %s", e.Eval.Class, e.Eval.Reason)
	}
	return fmt.Sprintf("system_one %s", e.Eval.Class)
}

// Class returns the evaluation Class carried by e, or "" if Eval is nil.
func (e EvaluationError) Class() Class {
	if e.Eval == nil {
		return ""
	}
	return e.Eval.Class
}

// Reason returns the content-free Uncertain or Failed reason carried by e, or "".
func (e EvaluationError) Reason() string {
	if e.Eval == nil {
		return ""
	}
	return e.Eval.Reason
}

// EvaluationFrom extracts the *Evaluation provenance record from err using
// errors.As, traversing any outer error wrapping (such as ruleengine's
// UncertainError/FailedError Cause chain).
func EvaluationFrom(err error) (*Evaluation, bool) {
	if err == nil {
		return nil, false
	}
	var ptr *EvaluationError
	if errors.As(err, &ptr) && ptr != nil && ptr.Eval != nil {
		return ptr.Eval, true
	}
	var val EvaluationError
	if errors.As(err, &val) && val.Eval != nil {
		return val.Eval, true
	}
	return nil, false
}
