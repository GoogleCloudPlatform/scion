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

package geminidecisions

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/systemone/egress"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/metrics"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/spec"
)

const (
	// AdapterName is the registered System One adapter identifier for Gemini Decision 1.
	AdapterName = "gemini_decisions"

	// AdapterVersion is the adapter version string recorded in provenance.
	AdapterVersion = "0.1.0"

	// DefaultEndpoint is the default HTTPS base URL for the Gemini Developer API
	// when InstanceView.Endpoint is unset (PV3 brief §5.1).
	DefaultEndpoint = "https://generativelanguage.googleapis.com"

	// DecisionsPath is the Gemini Developer API v1beta decisions HTTP route path.
	DecisionsPath = "/v1beta/decisions"

	// HeaderAPIKey is the adapter-declared HTTP header name used by
	// pkg/systemone/egress for Gemini API-key authentication (PV3 brief §§4, 5.1).
	HeaderAPIKey = "x-goog-api-key"

	// NativeMetricConfidence is the namespaced native metric key for Gemini's
	// documented answer confidence field (PV3 brief §5.1, provider-design §6.2).
	NativeMetricConfidence = "gemini.confidence"

	// ProviderRetention documents Gemini Developer API free- vs paid-tier data
	// retention and training policy (PV3 brief §1, §5.1).
	ProviderRetention = "Free tier: inputs used to improve Google products; paid tier: not used (ai.google.dev pricing, 2026-11-19)"

	// MaxQuestionsPerRequest is the Scion single-request question ceiling for
	// gemini_decisions (vendor ceiling 1,000; PV3 brief §5.1).
	MaxQuestionsPerRequest = 64

	// MaxInputTokens is the documented Gemini Decision 1 context limit covering
	// input and all questions (PV3 brief §1, §5.1).
	MaxInputTokens = 262144
)

var confidenceMetricDecl = provider.MetricDecl{
	Name:        NativeMetricConfidence,
	Unit:        metrics.UnitFraction,
	Min:         0,
	Max:         1,
	HigherMeans: "more_probable_top_outcome",
	Source:      metrics.SourceProviderDoc,
}

// Adapter implements provider.Adapter, provider.EndpointDefaulter, and
// provider.InstanceValidator for Gemini Decision 1 (v1beta). ModelLister is
// intentionally not implemented because model listing support for
// gemini-decision-1 is undocumented (PV3 brief §5.1, U-G1).
type Adapter struct{}

var (
	_ provider.Adapter           = (*Adapter)(nil)
	_ provider.EndpointDefaulter = (*Adapter)(nil)
	_ provider.InstanceValidator = (*Adapter)(nil)
)

// New constructs a Gemini Decision 1 provider Adapter.
func New() *Adapter {
	return &Adapter{}
}

// Name returns AdapterName ("gemini_decisions").
func (a *Adapter) Name() string {
	return AdapterName
}

// Version returns AdapterVersion ("0.1.0").
func (a *Adapter) Version() string {
	return AdapterVersion
}

// DefaultEndpoint returns DefaultEndpoint ("https://generativelanguage.googleapis.com").
func (a *Adapter) DefaultEndpoint() string {
	return DefaultEndpoint
}

// ValidateInstance checks Gemini-specific instance configuration (requiring a
// non-empty model identifier free of control characters, valid location, and a
// valid endpoint without userinfo, query parameters, or fragments).
func (a *Adapter) ValidateInstance(inst provider.InstanceView) error {
	if _, err := validateModelIdentifier(inst.Model); err != nil {
		return err
	}
	if !utf8.ValidString(inst.Location) {
		return provider.NewProviderError(provider.FailInvalidRequest, false, 0, 0, "instance location must be valid UTF-8")
	}
	for _, r := range inst.Location {
		if r < 0x20 || r == 0x7f {
			return provider.NewProviderError(provider.FailInvalidRequest, false, 0, 0, "instance location contains invalid control characters")
		}
	}
	if strings.TrimSpace(inst.Endpoint) != "" {
		if _, err := resolveEndpointURL(inst.Endpoint, DefaultEndpoint, DecisionsPath); err != nil {
			return provider.NewProviderError(provider.FailInvalidRequest, false, 0, 0, "invalid instance endpoint")
		}
	}
	return nil
}

// Capabilities returns a static, defensive copy of the Gemini Decision 1
// capability declaration (PV3 brief §5.1).
func (a *Adapter) Capabilities(_ provider.InstanceView) (provider.Capabilities, error) {
	return provider.Capabilities{
		Primitives: map[string]provider.PrimitiveLimits{
			spec.PrimitiveBinary: {
				Supported:                true,
				DistributionCompleteness: provider.CompletenessComplete,
			},
			spec.PrimitiveChoice: {
				Supported:                true,
				MinOptions:               spec.MinChoiceOptions,
				MaxOptions:               spec.MaxChoiceOptions,
				DistributionCompleteness: provider.CompletenessComplete,
			},
		},
		MaxQuestionsPerRequest: MaxQuestionsPerRequest,
		MaxInputTokens:         MaxInputTokens,
		Truncation:             provider.TruncationNever,
		ReturnsDistribution:    true,
		NativeMetrics:          []provider.MetricDecl{confidenceMetricDecl},
		Abstains:               false,
		SideEffectFree:         true,
		AuthScheme: provider.AuthScheme{
			Kind:       provider.AuthKindHeader,
			HeaderName: HeaderAPIKey,
		},
		ProviderRetention: ProviderRetention,
	}, nil
}

// Validate checks inst and rule against the portable v1 grammar and Gemini
// Decision 1 capability bounds.
func (a *Adapter) Validate(inst provider.InstanceView, rule provider.RuleSpec) error {
	caps, err := a.Capabilities(inst)
	if err != nil {
		return err
	}
	ruleCopy := rule
	return spec.ValidatePortable(&ruleCopy, caps.SpecLimits())
}

// Invoke sends exactly one POST /v1beta/decisions request over the
// egress-supplied Transport (uncompressed JSON, Content-Type: application/json
// only, never setting Authorization or query parameters) and decodes the
// response without performing retries (PV3 brief §§5.2–5.4).
func (a *Adapter) Invoke(ctx context.Context, t provider.Transport, inst provider.InstanceView, req provider.ModelRequest) (provider.ModelResult, error) {
	errMeta := provider.ModelResult{
		Notes: []string{provider.NoteModelUnreported},
	}
	if _, err := validateModelIdentifier(inst.Model); err != nil {
		return errMeta, err
	}
	if t == nil {
		return errMeta, provider.NewProviderError(provider.FailInternal, false, 0, 0, "nil provider transport")
	}

	targetURL, err := resolveEndpointURL(inst.Endpoint, DefaultEndpoint, DecisionsPath)
	if err != nil {
		return errMeta, err
	}

	payload, selectedQuestions, err := EncodeRequest(inst.Model, req)
	if err != nil {
		return errMeta, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(payload))
	if err != nil {
		return errMeta, provider.NewProviderError(provider.FailInvalidRequest, false, 0, 0, "failed to construct provider HTTP request")
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, body, err := executeOnce(ctx, t, httpReq)
	if resp != nil {
		errMeta.HTTPStatus = resp.StatusCode
	}
	if err != nil {
		return errMeta, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errMeta, classifyHTTPError(resp.StatusCode, resp.Header, body, time.Now())
	}

	res, err := DecodeResponse(selectedQuestions, resp.StatusCode, body)
	if err != nil {
		return errMeta, err
	}
	return res, nil
}

func executeOnce(ctx context.Context, t provider.Transport, httpReq *http.Request) (*http.Response, []byte, error) {
	resp, err := t.Do(httpReq)
	if err != nil {
		return nil, nil, classifyTransportError(ctx, err)
	}
	defer func() {
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
	}()

	if resp.ContentLength > egress.MaxResponseBytes {
		return resp, nil, egress.ErrResponseTooLarge
	}

	var body []byte
	if resp.Body != nil {
		var readErr error
		body, readErr = io.ReadAll(io.LimitReader(resp.Body, egress.MaxResponseBytes+1))
		if readErr != nil {
			classified := classifyTransportError(ctx, readErr)
			if pe, ok := provider.AsProviderError(classified); ok {
				pe.HTTPStatus = resp.StatusCode
			}
			return resp, nil, classified
		}
		if int64(len(body)) > egress.MaxResponseBytes {
			return resp, nil, egress.ErrResponseTooLarge
		}
	}
	return resp, body, nil
}

func resolveEndpointURL(configuredEndpoint, defaultEndpoint, routePath string) (string, error) {
	base := strings.TrimSpace(configuredEndpoint)
	if base == "" {
		base = strings.TrimSpace(defaultEndpoint)
	}
	if base == "" {
		return "", egress.ErrInvalidEndpoint
	}

	u, err := url.Parse(base)
	if err != nil || u.Opaque != "" || u.User != nil || u.Host == "" || u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" {
		return "", egress.ErrInvalidEndpoint
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && scheme != "http" {
		return "", egress.ErrSchemeNotAllowed
	}

	prefix := strings.TrimRight(u.Path, "/")
	rel := "/" + strings.TrimLeft(routePath, "/")
	u.Path = prefix + rel
	u.RawPath = ""
	return u.String(), nil
}

func validateModelIdentifier(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || !utf8.ValidString(trimmed) {
		return "", provider.NewProviderError(provider.FailInvalidRequest, false, 0, 0, "instance model is required for gemini_decisions")
	}
	for _, r := range trimmed {
		if r < 0x20 || r == 0x7f {
			return "", provider.NewProviderError(provider.FailInvalidRequest, false, 0, 0, "instance model contains invalid control characters")
		}
	}
	return trimmed, nil
}

func init() {
	provider.MustRegister(New())
}

// Register registers the Gemini Decision 1 adapter into r.
func Register(r *provider.Registry) error {
	return r.Register(New())
}
