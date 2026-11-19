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

// Package provider defines the credential-free Service Provider Interface (SPI)
// and adapter registry for system-one decision model providers.
//
// Adapters construct and decode wire payloads using the egress-supplied
// Transport (*http.Client); they never receive or store credentials and never
// construct their own HTTP clients (runtime-design §4.3, P1 brief §2, AC-2).
package provider

import (
	"context"
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/systemone/metrics"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/spec"
)

// Transport is the outbound *http.Client constructed by pkg/systemone/egress
// with SSRF protection, redirect refusal, response-size capping, and RoundTripper
// authentication. Adapters must use the supplied Transport and never construct
// their own http.Client.
type Transport = *http.Client

// RuleSpec aliases spec.Spec as the compiled rule contract passed to Adapter.Validate
// and ModelRequest.Rule.
type RuleSpec = spec.Spec

// PrimitiveLimits aliases spec.PrimitiveLimits so adapter capability tables and
// spec.ValidatePortable share one type without conversion.
type PrimitiveLimits = spec.PrimitiveLimits

// Metric aliases metrics.Metric for typed portable and native metrics on Answer.
type Metric = metrics.Metric

// FieldKind and TrustLabel alias the content-environment classifications in spec.
type (
	FieldKind  = spec.FieldKind
	TrustLabel = spec.TrustLabel
)

const (
	FieldKindText = spec.FieldKindText
	FieldKindJSON = spec.FieldKindJSON

	TrustUntrusted = spec.TrustUntrusted
	TrustTrusted   = spec.TrustTrusted

	CompletenessComplete = spec.CompletenessComplete
	CompletenessPartial  = spec.CompletenessPartial
	CompletenessNone     = spec.CompletenessNone

	// TruncationNever means the provider rejects oversize inputs with an error
	// rather than truncating.
	TruncationNever = "never"
	// TruncationReported means the provider may truncate and reports it in Usage.Truncated.
	TruncationReported = "reported"
	// TruncationSilent means the provider truncates silently; such adapters must
	// prove non-truncation before calling or return FailInputTooLarge.
	TruncationSilent = "silent"

	AnswerStatusAnswered  = "answered"
	AnswerStatusRefused   = "refused"
	AnswerStatusAbstained = "abstained"

	// BinaryTrueKey and BinaryFalseKey are the canonical keys in Answer.Distribution
	// for binary questions.
	BinaryTrueKey  = "true"
	BinaryFalseKey = "false"

	// Content-free provenance note constants (provider-design §6.1, §7.4; runtime-design §8.1; PV3 brief §5.3).
	NoteModelAliasReported         = "model_alias_reported"
	NoteModelUnreported            = "model_unreported"
	NoteClamped001To099            = "clamped_0.01_0.99"
	NoteOptionsSortedByProvider    = "options_sorted_by_provider"
	NoteConfidenceFormulaDeviation = "confidence_formula_deviation"
	NoteUsageUnreported            = "usage_unreported"
)

var closedNotes = []string{
	NoteModelAliasReported,
	NoteModelUnreported,
	NoteClamped001To099,
	NoteOptionsSortedByProvider,
	NoteConfidenceFormulaDeviation,
	NoteUsageUnreported,
}

// AllNotes returns a copy of the closed set of content-free provenance note
// constants allowed on Evaluation and AnswerRecord (runtime-design §8.1).
func AllNotes() []string {
	out := make([]string, len(closedNotes))
	copy(out, closedNotes)
	return out
}

// IsValidNote reports whether note belongs to the closed set of content-free
// provenance note constants (runtime-design §8.1).
func IsValidNote(note string) bool {
	switch note {
	case NoteModelAliasReported,
		NoteModelUnreported,
		NoteClamped001To099,
		NoteOptionsSortedByProvider,
		NoteConfidenceFormulaDeviation,
		NoteUsageUnreported:
		return true
	default:
		return false
	}
}

// AuthKind identifies the transport authentication mechanism declared by an
// adapter in Capabilities.AuthScheme (runtime-design §4.3).
type AuthKind string

const (
	AuthKindNone              AuthKind = "none"
	AuthKindBearer            AuthKind = "bearer"
	AuthKindHeader            AuthKind = "header"
	AuthKindGoogleAccessToken AuthKind = "google_access_token"
	AuthKindGoogleIDToken     AuthKind = "google_id_token"
	AuthKindAzureEntra        AuthKind = "azure_entra"
)

// AuthScheme describes the transport authentication scheme an adapter expects
// pkg/systemone/egress to apply. It carries scheme metadata only, never
// credential values.
type AuthScheme struct {
	Kind       AuthKind `json:"kind"`
	HeaderName string   `json:"header_name,omitempty"`
	Scopes     []string `json:"scopes,omitempty"`
	Audience   string   `json:"audience,omitempty"`
}

func (a AuthScheme) String() string {
	if a.Kind == AuthKindHeader && a.HeaderName != "" {
		return "header:" + a.HeaderName
	}
	return string(a.Kind)
}

var (
	// AuthNone declares that an adapter uses no HTTP authentication header.
	AuthNone = AuthScheme{Kind: AuthKindNone}
	// AuthBearer declares that an adapter uses HTTP Authorization: Bearer authentication.
	AuthBearer = AuthScheme{Kind: AuthKindBearer}
)

// MetricDecl describes a namespaced provider-native metric declared by an
// adapter (provider-design §5.1).
type MetricDecl struct {
	Name        string  `json:"name"`
	Unit        string  `json:"unit"`
	Min         float64 `json:"min"`
	Max         float64 `json:"max"`
	HigherMeans string  `json:"higher_means,omitempty"`
	Source      string  `json:"source"`
}

// Capabilities declares the limits and behavioral guarantees of an adapter for
// a given InstanceView (runtime-design §4.3, provider-design §5.1).
type Capabilities struct {
	Primitives             map[string]PrimitiveLimits `json:"primitives"`
	MaxQuestionsPerRequest int                        `json:"max_questions_per_request"`
	MaxInputTokens         int                        `json:"max_input_tokens"`
	Truncation             string                     `json:"truncation"`
	ReturnsDistribution    bool                       `json:"returns_distribution"`
	NativeMetrics          []MetricDecl               `json:"native_metrics,omitempty"`
	Abstains               bool                       `json:"abstains"`
	SideEffectFree         bool                       `json:"side_effect_free"`
	AuthScheme             AuthScheme                 `json:"auth_scheme"`
	ProviderRetention      string                     `json:"provider_retention,omitempty"`
}

// SpecLimits projects Capabilities into the *spec.CapabilityLimits view consumed
// by spec.ValidatePortable.
func (c Capabilities) SpecLimits() *spec.CapabilityLimits {
	return &spec.CapabilityLimits{
		Primitives:             c.Primitives,
		MaxQuestionsPerRequest: c.MaxQuestionsPerRequest,
		MaxInputTokens:         c.MaxInputTokens,
		Truncation:             c.Truncation,
		ReturnsDistribution:    c.ReturnsDistribution,
	}
}

// InstanceView is the credential-free projection of a configured provider
// instance passed to Adapter methods (runtime-design §4.3, AC-2).
type InstanceView struct {
	Name     string            `json:"name"`
	Revision int64             `json:"revision"`
	Model    string            `json:"model"`
	Endpoint string            `json:"endpoint"`
	Location string            `json:"location,omitempty"`
	Options  map[string]string `json:"options,omitempty"`
}

// InputField is a single bounded, consumer-selected content field sent in the
// provider's data channel (runtime-design §4.4, AC-2).
type InputField struct {
	Name      string     `json:"name"`
	Kind      FieldKind  `json:"kind"`
	Value     string     `json:"value"`
	Truncated bool       `json:"truncated,omitempty"`
	Trust     TrustLabel `json:"trust"`
}

// ModelRequest is the credential-free evaluation request passed to Adapter.Invoke.
// It contains no free-form or interface fields so credentials cannot enter model
// input by construction (runtime-design §4.3, AC-2).
type ModelRequest struct {
	Rule       RuleSpec            `json:"rule"`
	Questions  []string            `json:"questions,omitempty"`
	Input      []InputField        `json:"input"`
	ChoiceSets map[string][]string `json:"choice_sets,omitempty"`
	RequestID  string              `json:"request_id,omitempty"`
}

// Answer is the per-question result decoded by an Adapter and enriched with
// Scion portable metrics by the runtime (provider-design §6.1, runtime-design §4.3).
type Answer struct {
	QuestionID    string             `json:"question_id"`
	Primitive     string             `json:"primitive"`
	Status        string             `json:"status"`
	Tied          bool               `json:"tied,omitempty"`
	Bool          *bool              `json:"bool,omitempty"`
	Choice        *string            `json:"choice,omitempty"`
	Level         *string            `json:"level,omitempty"`
	ExpectedIndex *float64           `json:"expected_index,omitempty"`
	Distribution  map[string]float64 `json:"distribution,omitempty"`
	Metrics       map[string]Metric  `json:"metrics,omitempty"`
	NativeMetrics map[string]Metric  `json:"native_metrics,omitempty"`
	ReportedModel string             `json:"reported_model,omitempty"`
	Notes         []string           `json:"notes,omitempty"`
}

// Usage reports token consumption and provider-reported truncation for an
// invocation (runtime-design §4.3).
type Usage struct {
	InputTokens  int   `json:"input_tokens"`
	OutputTokens int   `json:"output_tokens"`
	Truncated    *bool `json:"truncated,omitempty"`
}

// ModelResult is the decoded response returned by Adapter.Invoke (runtime-design
// §4.3, §8.1; provider-design §6.1, §7.4).
type ModelResult struct {
	Answers           map[string]Answer `json:"answers"`
	ResolvedModel     string            `json:"resolved_model"`
	ProviderRequestID string            `json:"provider_request_id,omitempty"`
	HTTPStatus        int               `json:"http_status,omitempty"`
	Usage             Usage             `json:"usage"`
	Notes             []string          `json:"notes,omitempty"`
}

// Adapter is the provider protocol SPI implemented by each system-one wire
// profile (for example "typesafe").
type Adapter interface {
	Name() string
	Version() string
	Capabilities(inst InstanceView) (Capabilities, error)
	Validate(inst InstanceView, rule RuleSpec) error
	Invoke(ctx context.Context, t Transport, inst InstanceView, req ModelRequest) (ModelResult, error)
}

// ModelLister is an optional SPI extension implemented by adapters whose
// protocol supports listing available models (for example TypeSafe GET /v1/models),
// invoked during admin connectivity probes (P1 brief §2, §3; provider-design §7.4).
type ModelLister interface {
	ListModels(ctx context.Context, t Transport, inst InstanceView) ([]string, error)
}

// EndpointDefaulter is an optional SPI-adjacent extension implemented by
// adapters that define a default HTTPS base URL when an instance is created
// without an explicit endpoint (PV3 brief §6, Decision 4). Adapters that do not
// implement EndpointDefaulter (or return an empty string) require an explicit
// endpoint at instance creation time.
type EndpointDefaulter interface {
	DefaultEndpoint() string
}

// InstanceValidator is an optional SPI-adjacent extension implemented by
// adapters that validate provider-specific instance configuration (for example
// requiring a non-empty InstanceView.Model) at instance creation time without
// coupling static Capabilities(InstanceView{}) discovery to instance fields.
type InstanceValidator interface {
	ValidateInstance(inst InstanceView) error
}
