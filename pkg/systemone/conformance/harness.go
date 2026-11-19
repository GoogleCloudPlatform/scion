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

package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/systemone/egress"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/metrics"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// RequiredSyntheticNotice is the mandatory disclaimer required on every offline
// fixture bundle in P1 (P1 brief §3, common-dev-context, provider-design §7.4).
const RequiredSyntheticNotice = "synthetic from docs, not recorded from the live API"

const defaultCanaryBearer = "sk-conformance-canary-secret-never-in-body-or-provenance"

// Provenance documents the documentation source and synthetic status of a
// conformance fixture bundle.
type Provenance struct {
	Notice          string   `json:"notice"`
	Sources         []string `json:"sources"`
	UnverifiedItems []string `json:"unverified_items,omitempty"`
}

// FixtureBundle groups offline conformance cases loaded from a synthetic-from-docs
// JSON fixture file.
type FixtureBundle struct {
	FixtureID      string          `json:"fixture_id"`
	Description    string          `json:"description"`
	Adapter        string          `json:"adapter"`
	Provenance     Provenance      `json:"provenance"`
	InvokeCases    []InvokeCase    `json:"invoke_cases,omitempty"`
	ModelListCases []ModelListCase `json:"model_list_cases,omitempty"`
}

// MockHTTPResponse configures the synthetic response served by the loopback
// httptest TLS server for a single test case.
type MockHTTPResponse struct {
	StatusCode         int               `json:"status_code"`
	Headers            map[string]string `json:"headers,omitempty"`
	RetryAfterInFuture string            `json:"retry_after_in_future,omitempty"`
	RawBody            string            `json:"raw_body,omitempty"`
	OversizeBodyBytes  int               `json:"oversize_body_bytes,omitempty"`
	DropConnection     bool              `json:"drop_connection,omitempty"`
}

// ExpectedError specifies the content-free ProviderError (or passed-through
// egress sentinel when EgressSentinel is set) expected from an InvokeCase or
// ModelListCase.
type ExpectedError struct {
	Reason          provider.FailReason `json:"reason,omitempty"`
	EgressSentinel  string              `json:"egress_sentinel,omitempty"`
	Retryable       bool                `json:"retryable,omitempty"`
	HTTPStatus      int                 `json:"http_status,omitempty"`
	RetryAfterMs    *int64              `json:"retry_after_ms,omitempty"`
	MinRetryAfterMs *int64              `json:"min_retry_after_ms,omitempty"`
	MaxRetryAfterMs *int64              `json:"max_retry_after_ms,omitempty"`
	ErrorBodyCanary string              `json:"error_body_canary,omitempty"`
}

// ExpectedPortableMetrics specifies golden Scion-computed concentration metrics
// expected when metrics.Compute runs on a decoded Answer.Distribution.
type ExpectedPortableMetrics struct {
	PTop        float64 `json:"p_top"`
	Margin      float64 `json:"margin"`
	NormEntropy float64 `json:"norm_entropy"`
}

// ExpectedAnswer specifies the decoded Answer shape expected for one question.
type ExpectedAnswer struct {
	Primitive       string                    `json:"primitive"`
	Status          string                    `json:"status"`
	Tied            bool                      `json:"tied,omitempty"`
	Bool            *bool                     `json:"bool,omitempty"`
	Choice          *string                   `json:"choice,omitempty"`
	Distribution    map[string]float64        `json:"distribution,omitempty"`
	NativeMetrics   map[string]metrics.Metric `json:"native_metrics,omitempty"`
	PortableMetrics *ExpectedPortableMetrics  `json:"portable_metrics,omitempty"`
	Notes           []string                  `json:"notes,omitempty"`
}

// ExpectedResult specifies the decoded ModelResult expected on a 2xx InvokeCase.
type ExpectedResult struct {
	ResolvedModel     string                    `json:"resolved_model"`
	ProviderRequestID string                    `json:"provider_request_id,omitempty"`
	HTTPStatus        int                       `json:"http_status"`
	Usage             provider.Usage            `json:"usage"`
	Notes             []string                  `json:"notes,omitempty"`
	Answers           map[string]ExpectedAnswer `json:"answers"`
}

// InvokeCase defines a single offline Adapter.Invoke conformance test case.
type InvokeCase struct {
	Name                 string                `json:"name"`
	Note                 string                `json:"note,omitempty"`
	Model                string                `json:"model"`
	InstanceOptions      map[string]string     `json:"instance_options,omitempty"`
	Request              provider.ModelRequest `json:"request"`
	ExpectedPath         string                `json:"expected_path,omitempty"`
	ExpectedRequestJSON  json.RawMessage       `json:"expected_request_json,omitempty"`
	ExactRequestKeyOrder bool                  `json:"exact_request_key_order,omitempty"`
	MockResponse         MockHTTPResponse      `json:"mock_response"`
	ExpectResult         *ExpectedResult       `json:"expect_result,omitempty"`
	ExpectError          *ExpectedError        `json:"expect_error,omitempty"`
}

// ModelListCase defines a single offline ModelLister.ListModels conformance test case.
type ModelListCase struct {
	Name            string            `json:"name"`
	Note            string            `json:"note,omitempty"`
	Model           string            `json:"model,omitempty"`
	InstanceOptions map[string]string `json:"instance_options,omitempty"`
	ExpectedPath    string            `json:"expected_path,omitempty"`
	MockResponse    MockHTTPResponse  `json:"mock_response"`
	ExpectModels    []string          `json:"expect_models,omitempty"`
	ExpectError     *ExpectedError    `json:"expect_error,omitempty"`
}

// RecordedRequest captures the outbound HTTP request received by the loopback
// recording server.
type RecordedRequest struct {
	Method  string
	Path    string
	Query   string
	Headers http.Header
	Body    []byte
}

// SuiteConfig configures wire-family invariants checked across every case in a
// conformance run.
type SuiteConfig struct {
	// Auth configures the transport credential placement applied by
	// pkg/systemone/egress and verified on every recorded outbound request.
	// Zero value defaults to egress.BearerPlacement (Authorization: Bearer <canary>).
	Auth egress.Placement
	// ForbiddenHeaders lists HTTP header names that the adapter must never set
	// on any outbound request (for example TypeSafe danger-zone headers).
	ForbiddenHeaders []string
	// ForbiddenHeaderSubstrings lists case-insensitive header-name substrings
	// that must never appear on any outbound request.
	ForbiddenHeaderSubstrings []string
	// DataChannelInspector optionally verifies that only the referenced input
	// field names and requested question IDs appear in the wire request JSON.
	DataChannelInspector func(t testing.TB, body []byte, expectedInputNames []string, expectedQuestionIDs []string)
}

func effectiveSuiteAuth(p egress.Placement) egress.Placement {
	if p.Kind == "" && p.HeaderName == "" {
		return egress.BearerPlacement
	}
	return p
}

func newSuiteTransport(srv *httptest.Server, cfg SuiteConfig) provider.Transport {
	return egress.NewClient(
		egress.StaticBearer(defaultCanaryBearer),
		egress.WithCredentialPlacement(effectiveSuiteAuth(cfg.Auth)),
		egress.WithLoopbackServerForTesting(srv.URL, srv.Client()),
	)
}

// LoadBundle reads and parses a synthetic-from-docs FixtureBundle JSON file and
// enforces that it carries RequiredSyntheticNotice and at least one source citation.
func LoadBundle(t testing.TB, path string) FixtureBundle {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err, "reading conformance fixture %s", path)

	var bundle FixtureBundle
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	require.NoError(t, dec.Decode(&bundle), "decoding conformance fixture %s", path)

	ValidateProvenance(t, path, bundle.Provenance)
	require.NotEmpty(t, bundle.FixtureID, "fixture %s must set fixture_id", path)
	require.NotEmpty(t, bundle.Adapter, "fixture %s must set adapter", path)
	return bundle
}

// ValidateProvenance asserts that p explicitly states RequiredSyntheticNotice
// and cites at least one primary documentation source.
func ValidateProvenance(t testing.TB, label string, p Provenance) {
	t.Helper()
	assert.Contains(t, p.Notice, RequiredSyntheticNotice,
		"fixture %s must be labelled %q", label, RequiredSyntheticNotice)
	require.NotEmpty(t, p.Sources, "fixture %s must cite its documentation source page(s)", label)
	for i, src := range p.Sources {
		assert.NotEmpty(t, strings.TrimSpace(src), "fixture %s has empty source[%d]", label, i)
	}
}

// RunBundle executes all InvokeCases and ModelListCases in bundle against adapter
// over an in-process loopback TLS server through pkg/systemone/egress.
func RunBundle(t *testing.T, adapter provider.Adapter, bundle FixtureBundle, cfg SuiteConfig) {
	t.Helper()
	ValidateProvenance(t, bundle.FixtureID, bundle.Provenance)
	require.Equal(t, adapter.Name(), bundle.Adapter, "bundle adapter mismatch")

	for _, tc := range bundle.InvokeCases {
		tc := tc
		t.Run(bundle.FixtureID+"/"+tc.Name, func(t *testing.T) {
			runInvokeCase(t, adapter, tc, cfg)
		})
	}

	for _, mc := range bundle.ModelListCases {
		mc := mc
		t.Run(bundle.FixtureID+"/"+mc.Name, func(t *testing.T) {
			runModelListCase(t, adapter, mc, cfg)
		})
	}
}

func runInvokeCase(t testing.TB, adapter provider.Adapter, tc InvokeCase, cfg SuiteConfig) {
	t.Helper()
	require.True(t, (tc.ExpectResult != nil) != (tc.ExpectError != nil),
		"case %s must set exactly one of expect_result or expect_error", tc.Name)

	instBase := provider.InstanceView{
		Name:     "conformance-" + adapter.Name(),
		Revision: 1,
		Model:    tc.Model,
		Options:  tc.InstanceOptions,
	}
	if tc.Model != "" {
		require.NoError(t, adapter.Validate(instBase, tc.Request.Rule),
			"case %s portable rule must pass adapter.Validate", tc.Name)
	}

	srv, recorded := startMockServer(t, tc.MockResponse)
	defer srv.Close()

	inst := instBase
	inst.Endpoint = srv.URL

	transport := newSuiteTransport(srv, cfg)

	res, err := adapter.Invoke(context.Background(), transport, inst, tc.Request)
	reqs := recorded()

	if tc.Model == "" && tc.ExpectError != nil && len(reqs) == 0 {
		// Pre-flight validation failure (e.g. missing required model) before HTTP call.
		assertExpectedError(t, err, *tc.ExpectError)
		return
	}

	require.Len(t, reqs, 1, "P1 adapter must issue exactly 1 HTTP request (no retries)")
	rec := reqs[0]
	assert.Equal(t, http.MethodPost, rec.Method)
	if tc.ExpectedPath != "" {
		assert.Equal(t, tc.ExpectedPath, rec.Path)
	}
	assertCommonRequestGuards(t, rec, http.MethodPost, cfg)

	expectedInputs := make([]string, len(tc.Request.Input))
	for i, f := range tc.Request.Input {
		expectedInputs[i] = f.Name
	}
	expectedQIDs := expectedQuestionIDs(tc.Request)
	if cfg.DataChannelInspector != nil {
		cfg.DataChannelInspector(t, rec.Body, expectedInputs, expectedQIDs)
	}

	if len(tc.ExpectedRequestJSON) > 0 {
		var compactWant bytes.Buffer
		require.NoError(t, json.Compact(&compactWant, tc.ExpectedRequestJSON))
		if tc.ExactRequestKeyOrder {
			assert.Equal(t, compactWant.String(), string(rec.Body),
				"exact wire JSON key order mismatch in case %s", tc.Name)
		} else {
			assert.JSONEq(t, compactWant.String(), string(rec.Body))
		}
	}

	if tc.ExpectError != nil {
		assertExpectedError(t, err, *tc.ExpectError)
		return
	}

	require.NoError(t, err)
	assertExpectedResult(t, tc.Request, res, *tc.ExpectResult)
}

func runModelListCase(t testing.TB, adapter provider.Adapter, mc ModelListCase, cfg SuiteConfig) {
	t.Helper()
	lister, ok := adapter.(provider.ModelLister)
	require.True(t, ok, "adapter %q does not implement provider.ModelLister", adapter.Name())
	require.True(t, (mc.ExpectError != nil) != (mc.ExpectModels != nil),
		"model list case %s must set expect_models or expect_error", mc.Name)

	srv, recorded := startMockServer(t, mc.MockResponse)
	defer srv.Close()

	inst := provider.InstanceView{
		Name:     "conformance-" + adapter.Name(),
		Revision: 1,
		Model:    mc.Model,
		Endpoint: srv.URL,
		Options:  mc.InstanceOptions,
	}
	transport := newSuiteTransport(srv, cfg)

	models, err := lister.ListModels(context.Background(), transport, inst)
	reqs := recorded()
	require.Len(t, reqs, 1, "ModelLister.ListModels must issue exactly 1 HTTP request")

	rec := reqs[0]
	assert.Equal(t, http.MethodGet, rec.Method)
	if mc.ExpectedPath != "" {
		assert.Equal(t, mc.ExpectedPath, rec.Path)
	}
	assertCommonRequestGuards(t, rec, http.MethodGet, cfg)

	if mc.ExpectError != nil {
		assertExpectedError(t, err, *mc.ExpectError)
		return
	}

	require.NoError(t, err)
	assert.Equal(t, mc.ExpectModels, models)
}

// RunCapabilitiesCheck verifies adapter metadata, SideEffectFree registration
// gate, namespaced native metric declarations, and portable spec compatibility
// (provider-design §5.2, §12 AC-10).
func RunCapabilitiesCheck(t testing.TB, adapter provider.Adapter, inst provider.InstanceView) {
	t.Helper()
	assert.NotEmpty(t, adapter.Name())
	assert.NotEmpty(t, adapter.Version())

	caps, err := adapter.Capabilities(inst)
	require.NoError(t, err)
	assert.True(t, caps.SideEffectFree, "all system-one adapters must declare SideEffectFree: true")
	assert.Greater(t, caps.MaxQuestionsPerRequest, 0)
	assert.Greater(t, caps.MaxInputTokens, 0)

	for _, nm := range caps.NativeMetrics {
		assert.False(t, metrics.IsPortableMetric(nm.Name),
			"native metric %q must never shadow a portable metric", nm.Name)
		assert.True(t, hasValidAdapterMetricNamespace(adapter.Name(), nm.Name),
			"native metric %q must be namespaced under adapter %q", nm.Name, adapter.Name())
		assert.NotEmpty(t, nm.Unit)
		assert.NotEmpty(t, nm.Source)
	}

	sample := &spec.Spec{
		Version: 1,
		Questions: []spec.Question{
			{ID: "b", Type: spec.PrimitiveBinary, Instructions: "Check binary."},
			{
				ID:           "c",
				Type:         spec.PrimitiveChoice,
				Instructions: "Check choice.",
				Options: []spec.Option{
					{ID: "opt_a", Description: "First option"},
					{ID: "opt_b", Description: "Second option"},
				},
			},
		},
	}
	require.NoError(t, spec.ValidatePortable(sample, caps.SpecLimits()))
}

// RunUntrustedInputIsolationCheck feeds a unique sentinel string in an untrusted
// InputField and verifies that it appears solely in the protocol's data channel
// and never in instructions, criteria, headers, or URL (provider-design §12 AC-6,
// P1 brief §7 AC-2).
func RunUntrustedInputIsolationCheck(
	t testing.TB,
	adapter provider.Adapter,
	inst provider.InstanceView,
	rule spec.Spec,
	mockOK MockHTTPResponse,
	cfg SuiteConfig,
	extractNonDataText func(t testing.TB, rawReq []byte) string,
) {
	t.Helper()
	const sentinel = "UNTRUSTED_INPUT_SENTINEL_IGNORE_PREVIOUS_INSTRUCTIONS_9911"

	srv, recorded := startMockServer(t, mockOK)
	defer srv.Close()

	callInst := inst
	callInst.Endpoint = srv.URL

	transport := newSuiteTransport(srv, cfg)

	req := provider.ModelRequest{
		Rule: rule,
		Input: []provider.InputField{
			{
				Name:  spec.ProbeFieldText,
				Kind:  provider.FieldKindText,
				Value: sentinel,
				Trust: provider.TrustUntrusted,
			},
		},
	}

	_, err := adapter.Invoke(context.Background(), transport, callInst, req)
	require.NoError(t, err)

	reqs := recorded()
	require.Len(t, reqs, 1)
	rec := reqs[0]
	assertCommonRequestGuards(t, rec, http.MethodPost, cfg)
	assert.NotContains(t, rec.Path, sentinel)
	assert.NotContains(t, rec.Query, sentinel)
	for k, vals := range rec.Headers {
		for _, v := range vals {
			assert.NotContains(t, v, sentinel, "untrusted sentinel leaked into header %q", k)
		}
	}

	nonData := extractNonDataText(t, rec.Body)
	assert.NotContains(t, nonData, sentinel,
		"untrusted input sentinel must never appear in instructions, criteria, or options")
	assert.Contains(t, string(rec.Body), sentinel,
		"untrusted input sentinel must be present in the wire data channel")
}

func startMockServer(t testing.TB, mock MockHTTPResponse) (*httptest.Server, func() []RecordedRequest) {
	t.Helper()
	var (
		mu   sync.Mutex
		reqs []RecordedRequest
	)

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()

		mu.Lock()
		reqs = append(reqs, RecordedRequest{
			Method:  r.Method,
			Path:    r.URL.Path,
			Query:   r.URL.RawQuery,
			Headers: r.Header.Clone(),
			Body:    body,
		})
		mu.Unlock()

		if mock.DropConnection {
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					_ = conn.Close()
					return
				}
			}
		}

		for k, v := range mock.Headers {
			w.Header().Set(k, v)
		}
		if mock.RetryAfterInFuture != "" {
			d, err := time.ParseDuration(mock.RetryAfterInFuture)
			if err == nil {
				w.Header().Set("Retry-After", time.Now().UTC().Add(d).Format(http.TimeFormat))
			}
		}
		if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", "application/json")
		}
		status := mock.StatusCode
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		if mock.OversizeBodyBytes > 0 {
			_, _ = w.Write(bytes.Repeat([]byte("a"), mock.OversizeBodyBytes))
			return
		}
		_, _ = io.WriteString(w, mock.RawBody)
	}))

	return srv, func() []RecordedRequest {
		mu.Lock()
		defer mu.Unlock()
		cp := make([]RecordedRequest, len(reqs))
		copy(cp, reqs)
		return cp
	}
}

func hasValidAdapterMetricNamespace(adapterName, metricName string) bool {
	ns, leaf, ok := strings.Cut(metricName, ".")
	if !ok || ns == "" || leaf == "" {
		return false
	}
	familyPrefix, _, _ := strings.Cut(adapterName, "_")
	return ns == adapterName || (familyPrefix != "" && ns == familyPrefix)
}

func assertSecretOnlyInHeader(t testing.TB, hdrs http.Header, allowedHeader, secret string) {
	t.Helper()
	for name, vals := range hdrs {
		if allowedHeader != "" && strings.EqualFold(name, allowedHeader) {
			continue
		}
		for _, v := range vals {
			assert.NotContains(t, v, secret,
				"credential leaked into unexpected header %q", name)
		}
	}
}

func assertCommonRequestGuards(t testing.TB, rec RecordedRequest, method string, cfg SuiteConfig) {
	t.Helper()
	auth := effectiveSuiteAuth(cfg.Auth)
	switch auth.Kind {
	case provider.AuthKindBearer:
		assert.Equal(t, "Bearer "+defaultCanaryBearer, rec.Headers.Get("Authorization"),
			"key must appear in Authorization header via egress RoundTripper")
		assertSecretOnlyInHeader(t, rec.Headers, "Authorization", defaultCanaryBearer)
	case provider.AuthKindHeader:
		require.NotEmpty(t, auth.HeaderName, "SuiteConfig.Auth header placement requires HeaderName")
		assert.Empty(t, rec.Headers.Get("Authorization"),
			"Authorization header must never be sent with header credential placement")
		assert.Equal(t, defaultCanaryBearer, rec.Headers.Get(auth.HeaderName),
			"key must appear in %s header via egress RoundTripper", auth.HeaderName)
		assertSecretOnlyInHeader(t, rec.Headers, auth.HeaderName, defaultCanaryBearer)
	case provider.AuthKindNone:
		assert.Empty(t, rec.Headers.Get("Authorization"),
			"Authorization header must not be sent with none credential placement")
		assertSecretOnlyInHeader(t, rec.Headers, "", defaultCanaryBearer)
	default:
		t.Fatalf("unsupported SuiteConfig.Auth kind %q", auth.Kind)
	}
	assert.Empty(t, rec.Query, "outbound request must not carry a URL query string")
	assert.NotContains(t, rec.Path, defaultCanaryBearer,
		"credential must never appear in outbound request path (AC-2)")
	assert.NotContains(t, string(rec.Body), defaultCanaryBearer,
		"credential must never appear in outbound request body (AC-2)")
	assert.Empty(t, rec.Headers.Get("Content-Encoding"),
		"P1 requests must be uncompressed JSON")
	assert.False(t, bytes.HasPrefix(rec.Body, []byte{0xEF, 0xBB, 0xBF}),
		"request payload must be UTF-8 without BOM")

	if method == http.MethodPost {
		assert.Equal(t, "application/json", rec.Headers.Get("Content-Type"))
	}

	for _, forbidden := range cfg.ForbiddenHeaders {
		assert.Empty(t, rec.Headers.Get(forbidden),
			"forbidden header %q must never be sent", forbidden)
		for actual := range rec.Headers {
			assert.False(t, strings.EqualFold(actual, forbidden),
				"forbidden header %q was present on request", actual)
		}
	}
	for _, sub := range cfg.ForbiddenHeaderSubstrings {
		lowerSub := strings.ToLower(sub)
		for actual := range rec.Headers {
			assert.False(t, strings.Contains(strings.ToLower(actual), lowerSub),
				"header %q matched forbidden substring %q", actual, sub)
		}
	}
}

func assertExpectedError(t testing.TB, err error, want ExpectedError) {
	t.Helper()
	require.Error(t, err)
	assert.NotContains(t, err.Error(), defaultCanaryBearer,
		"credential must never appear in errors")

	if want.EgressSentinel != "" {
		if want.ErrorBodyCanary != "" {
			assert.NotContains(t, err.Error(), want.ErrorBodyCanary,
				"canary text must never appear in passed-through egress error")
		}
		switch want.EgressSentinel {
		case "response_too_large":
			assert.True(t, egress.IsResponseTooLarge(err),
				"expected egress response_too_large sentinel pass-through, got %T: %v", err, err)
		case "redirect_blocked":
			assert.True(t, egress.IsRedirectBlocked(err),
				"expected egress redirect_blocked sentinel pass-through, got %T: %v", err, err)
		case "blocked_destination":
			assert.True(t, egress.IsBlockedDestination(err),
				"expected egress blocked_destination sentinel pass-through, got %T: %v", err, err)
		case "scheme_not_allowed":
			assert.True(t, egress.IsSchemeNotAllowed(err),
				"expected egress scheme_not_allowed sentinel pass-through, got %T: %v", err, err)
		case "invalid_endpoint":
			assert.True(t, egress.IsInvalidEndpoint(err),
				"expected egress invalid_endpoint sentinel pass-through, got %T: %v", err, err)
		case "invalid_placement":
			assert.True(t, egress.IsInvalidPlacement(err),
				"expected egress invalid_placement sentinel pass-through, got %T: %v", err, err)
		case "auth":
			assert.True(t, egress.IsAuthError(err),
				"expected egress auth sentinel pass-through, got %T: %v", err, err)
		default:
			t.Fatalf("unknown EgressSentinel %q in conformance fixture", want.EgressSentinel)
		}
		return
	}

	pe, ok := provider.AsProviderError(err)
	require.True(t, ok, "adapter error must unwrap as *provider.ProviderError, got %T: %v", err, err)
	assert.True(t, pe.Reason.Valid(), "ProviderError.Reason %q must belong to closed FailReason set", pe.Reason)
	assert.Equal(t, want.Reason, pe.Reason)
	assert.Equal(t, want.Retryable, pe.Retryable)
	if want.HTTPStatus > 0 {
		assert.Equal(t, want.HTTPStatus, pe.HTTPStatus)
	}
	if want.RetryAfterMs != nil {
		assert.Equal(t, time.Duration(*want.RetryAfterMs)*time.Millisecond, pe.RetryAfter)
	}
	if want.MinRetryAfterMs != nil {
		assert.GreaterOrEqual(t, pe.RetryAfter, time.Duration(*want.MinRetryAfterMs)*time.Millisecond)
	}
	if want.MaxRetryAfterMs != nil {
		assert.LessOrEqual(t, pe.RetryAfter, time.Duration(*want.MaxRetryAfterMs)*time.Millisecond)
	}
	if want.ErrorBodyCanary != "" {
		assert.NotContains(t, pe.Message, want.ErrorBodyCanary,
			"error body text must never be copied into ProviderError.Message")
		assert.NotContains(t, err.Error(), want.ErrorBodyCanary,
			"error body text must never appear in ProviderError.Error()")
	}
}

func assertExpectedResult(t testing.TB, req provider.ModelRequest, got provider.ModelResult, want ExpectedResult) {
	t.Helper()
	const eps = 1e-12

	assert.Equal(t, want.ResolvedModel, got.ResolvedModel)
	assert.Equal(t, want.ProviderRequestID, got.ProviderRequestID)
	assert.Equal(t, want.HTTPStatus, got.HTTPStatus)
	assert.Equal(t, want.Usage, got.Usage)
	assert.ElementsMatch(t, want.Notes, got.Notes)

	require.Len(t, got.Answers, len(want.Answers))
	qByID := make(map[string]spec.Question, len(req.Rule.Questions))
	for _, q := range req.Rule.Questions {
		qByID[q.ID] = q
	}

	for qid, wantAns := range want.Answers {
		gotAns, ok := got.Answers[qid]
		require.True(t, ok, "missing decoded answer for question %q", qid)
		assert.Equal(t, qid, gotAns.QuestionID)
		assert.Equal(t, wantAns.Primitive, gotAns.Primitive)
		assert.Equal(t, wantAns.Status, gotAns.Status)
		assert.Equal(t, wantAns.Tied, gotAns.Tied)
		assert.Equal(t, wantAns.Bool, gotAns.Bool)
		assert.Equal(t, wantAns.Choice, gotAns.Choice)
		assert.Empty(t, gotAns.Metrics, "adapters must not compute Scion portable metrics directly")
		assert.ElementsMatch(t, wantAns.Notes, gotAns.Notes)

		// Verify exact closed-world distribution keys match declared IDs without repair/renormalization.
		qSpec, hasQ := qByID[qid]
		require.True(t, hasQ)
		declared := declaredKeysForQuestion(qSpec)
		gotKeys := sortedMapKeys(gotAns.Distribution)
		wantKeys := append([]string(nil), declared...)
		sort.Strings(wantKeys)
		assert.Equal(t, wantKeys, gotKeys,
			"decoded distribution keys for %q must equal declared option IDs exactly (AC-3)", qid)

		require.Len(t, gotAns.Distribution, len(wantAns.Distribution))
		for k, wantProb := range wantAns.Distribution {
			gotProb, exists := gotAns.Distribution[k]
			require.True(t, exists, "missing distribution key %q on question %q", k, qid)
			assert.InDelta(t, wantProb, gotProb, eps, "distribution[%q] mismatch on %q", k, qid)
		}

		// Verify Scion portable metrics compute cleanly on every decoded distribution (AC-PV1, §12 AC-4).
		computed, err := metrics.Compute(gotAns.Distribution)
		require.NoError(t, err, "metrics.Compute failed on decoded distribution for %q", qid)
		if wantAns.PortableMetrics != nil {
			assert.InDelta(t, wantAns.PortableMetrics.PTop, computed[metrics.NamePTop].Value, eps)
			assert.InDelta(t, wantAns.PortableMetrics.Margin, computed[metrics.NameMargin].Value, eps)
			assert.InDelta(t, wantAns.PortableMetrics.NormEntropy, computed[metrics.NameNormEntropy].Value, eps)
		}

		// Verify native metrics remain namespaced with Source set.
		require.Len(t, gotAns.NativeMetrics, len(wantAns.NativeMetrics))
		for mName, wantM := range wantAns.NativeMetrics {
			assert.False(t, metrics.IsPortableMetric(mName),
				"provider confidence/metric %q must never be mapped onto a portable metric name", mName)
			gotM, exists := gotAns.NativeMetrics[mName]
			require.True(t, exists, "missing expected native metric %q", mName)
			assert.InDelta(t, wantM.Value, gotM.Value, eps)
			assert.Equal(t, wantM.Unit, gotM.Unit)
			assert.Equal(t, wantM.Min, gotM.Min)
			assert.Equal(t, wantM.Max, gotM.Max)
			assert.Equal(t, wantM.Source, gotM.Source)
		}
	}
}

func expectedQuestionIDs(req provider.ModelRequest) []string {
	if len(req.Questions) > 0 {
		out := make([]string, len(req.Questions))
		copy(out, req.Questions)
		return out
	}
	out := make([]string, len(req.Rule.Questions))
	for i, q := range req.Rule.Questions {
		out[i] = q.ID
	}
	return out
}

func declaredKeysForQuestion(q spec.Question) []string {
	switch q.Type {
	case spec.PrimitiveBinary:
		return []string{provider.BinaryTrueKey, provider.BinaryFalseKey}
	case spec.PrimitiveChoice:
		out := make([]string, len(q.Options))
		for i, opt := range q.Options {
			out[i] = opt.ID
		}
		return out
	default:
		return nil
	}
}

func sortedMapKeys(m map[string]float64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
