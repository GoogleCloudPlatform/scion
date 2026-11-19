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

package jevwire

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/systemone/egress"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/spec"
)

// Adapter implements provider.Adapter, provider.ModelLister,
// provider.EndpointDefaulter, and provider.InstanceValidator for a Jev-wire
// Profile.
type Adapter struct {
	profile Profile
}

var (
	_ provider.Adapter           = (*Adapter)(nil)
	_ provider.ModelLister       = (*Adapter)(nil)
	_ provider.EndpointDefaulter = (*Adapter)(nil)
	_ provider.InstanceValidator = (*Adapter)(nil)
)

// NewAdapter constructs a Jev-wire Adapter for the supplied Profile.
func NewAdapter(p Profile) *Adapter {
	return &Adapter{profile: p}
}

// NewTypeSafe constructs the TypeSafe Jev ("typesafe") adapter profile.
func NewTypeSafe() *Adapter {
	return NewAdapter(TypeSafeProfile())
}

// Name returns the profile's implementation identifier (for example "typesafe").
func (a *Adapter) Name() string {
	return a.profile.Name
}

// Version returns the adapter implementation version recorded in provenance.
func (a *Adapter) Version() string {
	return a.profile.Version
}

// DefaultEndpoint returns the profile's default HTTPS base URL (for example
// "https://api.typesafe.ai" for TypeSafe), or "" if the profile defines none.
func (a *Adapter) DefaultEndpoint() string {
	return a.profile.DefaultEndpoint
}

// ValidateInstance checks profile-specific instance configuration (for example
// requiring a non-empty InstanceView.Model on TypeSafe).
func (a *Adapter) ValidateInstance(inst provider.InstanceView) error {
	trimmed := strings.TrimSpace(inst.Model)
	if a.profile.RequireModel && trimmed == "" {
		return provider.NewProviderError(
			provider.FailInvalidRequest,
			false,
			0,
			0,
			fmt.Sprintf("model is required for adapter %q", a.profile.Name),
		)
	}
	if !utf8.ValidString(trimmed) {
		return provider.NewProviderError(provider.FailInvalidRequest, false, 0, 0, "instance model must be valid UTF-8")
	}
	for _, r := range trimmed {
		if r < 0x20 || r == 0x7f {
			return provider.NewProviderError(provider.FailInvalidRequest, false, 0, 0, "instance model contains invalid control characters")
		}
	}
	return nil
}

// Capabilities returns a defensive copy of the profile's declared capabilities.
func (a *Adapter) Capabilities(_ provider.InstanceView) (provider.Capabilities, error) {
	return cloneCapabilities(a.profile.Capabilities), nil
}

// Validate checks rule against the portable v1 grammar and the profile's capability limits.
func (a *Adapter) Validate(inst provider.InstanceView, rule provider.RuleSpec) error {
	caps, err := a.Capabilities(inst)
	if err != nil {
		return err
	}
	ruleCopy := rule
	return spec.ValidatePortable(&ruleCopy, caps.SpecLimits())
}

// Invoke sends exactly one POST /v1/systemone request over the egress-supplied
// Transport (JSON without compression, never setting danger-zone headers) and
// decodes the response without performing retries.
func (a *Adapter) Invoke(ctx context.Context, t provider.Transport, inst provider.InstanceView, req provider.ModelRequest) (provider.ModelResult, error) {
	if t == nil {
		return provider.ModelResult{}, provider.NewProviderError(provider.FailInternal, false, 0, 0, "nil provider transport")
	}

	targetURL, err := resolveEndpointURL(inst.Endpoint, a.profile.DefaultEndpoint, a.profile.DecisionsPath)
	if err != nil {
		return provider.ModelResult{}, err
	}

	payload, selectedQuestions, err := EncodeRequest(inst.Model, req)
	if err != nil {
		return provider.ModelResult{}, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(payload))
	if err != nil {
		return provider.ModelResult{}, provider.NewProviderError(provider.FailInvalidRequest, false, 0, 0, "failed to construct provider HTTP request")
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	resp, body, err := executeOnce(ctx, t, httpReq)
	var errMeta provider.ModelResult
	var rawReqID string
	if resp != nil {
		if a.profile.RequestIDHeader != "" {
			rawReqID = resp.Header.Get(a.profile.RequestIDHeader)
		}
		errMeta = provider.ModelResult{
			ProviderRequestID: sanitizeRequestID(rawReqID),
			HTTPStatus:        resp.StatusCode,
		}
	}
	if err != nil {
		return errMeta, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errMeta, classifyHTTPError(resp.StatusCode, resp.Header, body, time.Now())
	}

	res, err := DecodeResponse(a.profile, selectedQuestions, resp.StatusCode, rawReqID, body)
	if err != nil {
		return errMeta, err
	}
	return res, nil
}

// ListModels queries GET /v1/models over the egress-supplied Transport and
// returns the provider's model names in declared order (provider-design §7.4).
func (a *Adapter) ListModels(ctx context.Context, t provider.Transport, inst provider.InstanceView) ([]string, error) {
	if a.profile.ModelsPath == "" {
		return nil, provider.NewProviderError(provider.FailUnsupported, false, 0, 0, "profile does not support listing models")
	}
	if t == nil {
		return nil, provider.NewProviderError(provider.FailInternal, false, 0, 0, "nil provider transport")
	}

	targetURL, err := resolveEndpointURL(inst.Endpoint, a.profile.DefaultEndpoint, a.profile.ModelsPath)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, provider.NewProviderError(provider.FailInvalidRequest, false, 0, 0, "failed to construct models HTTP request")
	}
	httpReq.Header.Set("Accept", "application/json")

	resp, body, err := executeOnce(ctx, t, httpReq)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, classifyHTTPError(resp.StatusCode, resp.Header, body, time.Now())
	}

	return DecodeModelsResponse(resp.StatusCode, body)
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

func cloneCapabilities(c provider.Capabilities) provider.Capabilities {
	prims := make(map[string]provider.PrimitiveLimits, len(c.Primitives))
	for k, v := range c.Primitives {
		prims[k] = v
	}
	var native []provider.MetricDecl
	if len(c.NativeMetrics) > 0 {
		native = make([]provider.MetricDecl, len(c.NativeMetrics))
		copy(native, c.NativeMetrics)
	}
	var scopes []string
	if len(c.AuthScheme.Scopes) > 0 {
		scopes = make([]string, len(c.AuthScheme.Scopes))
		copy(scopes, c.AuthScheme.Scopes)
	}
	out := c
	out.Primitives = prims
	out.NativeMetrics = native
	out.AuthScheme.Scopes = scopes
	return out
}
