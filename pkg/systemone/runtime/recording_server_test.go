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

package runtime_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/systemone/egress"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/metrics"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider/jevwire"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// wireRecordingAdapter is an offline test provider.Adapter + ModelLister that
// speaks a minimal JSON HTTP wire protocol over the runtime-supplied
// egress.NewClient Transport without importing jevwire or touching credentials.
type wireRecordingAdapter struct {
	caps provider.Capabilities
}

func newWireRecordingAdapter() *wireRecordingAdapter {
	return &wireRecordingAdapter{
		caps: provider.Capabilities{
			Primitives: map[string]provider.PrimitiveLimits{
				spec.PrimitiveBinary: {
					Supported:                true,
					DistributionCompleteness: spec.CompletenessComplete,
				},
				spec.PrimitiveChoice: {
					Supported:                true,
					MinOptions:               2,
					MaxOptions:               64,
					DistributionCompleteness: spec.CompletenessComplete,
				},
			},
			MaxQuestionsPerRequest: 64,
			MaxInputTokens:         32768,
			Truncation:             provider.TruncationNever,
			ReturnsDistribution:    true,
			NativeMetrics: []provider.MetricDecl{
				{
					Name:   "typesafe.confidence",
					Unit:   metrics.UnitFraction,
					Min:    0,
					Max:    1,
					Source: metrics.SourceProviderDoc,
				},
			},
			SideEffectFree: true,
			AuthScheme:     provider.AuthBearer,
		},
	}
}

type wireRequestPayload struct {
	Model     string                `json:"model"`
	RequestID string                `json:"request_id"`
	Input     []provider.InputField `json:"input"`
	Questions []spec.Question       `json:"questions"`
}

type wireResponsePayload struct {
	ResolvedModel string                     `json:"resolved_model"`
	Notes         []string                   `json:"notes,omitempty"`
	Answers       map[string]provider.Answer `json:"answers"`
}

func (a *wireRecordingAdapter) Name() string    { return "wire-test" }
func (a *wireRecordingAdapter) Version() string { return "0.1.0-wire" }
func (a *wireRecordingAdapter) Capabilities(provider.InstanceView) (provider.Capabilities, error) {
	return a.caps, nil
}
func (a *wireRecordingAdapter) Validate(provider.InstanceView, provider.RuleSpec) error {
	return nil
}

func (a *wireRecordingAdapter) Invoke(
	ctx context.Context,
	tr provider.Transport,
	inst provider.InstanceView,
	req provider.ModelRequest,
) (provider.ModelResult, error) {
	payload := wireRequestPayload{
		Model:     inst.Model,
		RequestID: req.RequestID,
		Input:     req.Input,
		Questions: req.Rule.Questions,
	}
	rawBody, err := json.Marshal(payload)
	if err != nil {
		return provider.ModelResult{}, provider.NewProviderError(provider.FailInvalidRequest, false, 0, 0, "marshal wire payload")
	}

	endpoint := strings.TrimRight(inst.Endpoint, "/") + "/v1/evaluate"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(rawBody))
	if err != nil {
		return provider.ModelResult{}, provider.NewProviderError(provider.FailInvalidRequest, false, 0, 0, "build http request")
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Request-ID", req.RequestID)

	resp, err := tr.Do(httpReq)
	if err != nil {
		return provider.ModelResult{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return provider.ModelResult{HTTPStatus: resp.StatusCode}, err
	}

	reqID := resp.Header.Get("X-Request-ID")
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return provider.ModelResult{ProviderRequestID: reqID, HTTPStatus: resp.StatusCode},
			provider.NewProviderError(provider.FailAuth, false, 0, resp.StatusCode, "provider rejected credentials")
	case resp.StatusCode == http.StatusTooManyRequests:
		return provider.ModelResult{ProviderRequestID: reqID, HTTPStatus: resp.StatusCode},
			provider.NewProviderError(provider.FailRateLimited, true, time.Second, resp.StatusCode, "provider rate limited")
	case resp.StatusCode >= 500:
		return provider.ModelResult{ProviderRequestID: reqID, HTTPStatus: resp.StatusCode},
			provider.NewProviderError(provider.FailUnavailable, true, 0, resp.StatusCode, "provider 5xx")
	case resp.StatusCode >= 400:
		return provider.ModelResult{ProviderRequestID: reqID, HTTPStatus: resp.StatusCode},
			provider.NewProviderError(provider.FailInvalidRequest, false, 0, resp.StatusCode, "provider 4xx")
	}

	var decoded wireResponsePayload
	if err := json.Unmarshal(respBytes, &decoded); err != nil {
		return provider.ModelResult{ProviderRequestID: reqID, HTTPStatus: resp.StatusCode},
			provider.NewProviderError(provider.FailMalformedResponse, false, 0, resp.StatusCode, "invalid json body")
	}
	return provider.ModelResult{
		Answers:           decoded.Answers,
		ResolvedModel:     decoded.ResolvedModel,
		ProviderRequestID: reqID,
		HTTPStatus:        resp.StatusCode,
		Notes:             decoded.Notes,
	}, nil
}

func (a *wireRecordingAdapter) ListModels(
	ctx context.Context,
	tr provider.Transport,
	inst provider.InstanceView,
) ([]string, error) {
	endpoint := strings.TrimRight(inst.Endpoint, "/") + "/v1/models"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, provider.NewProviderError(provider.FailInvalidRequest, false, 0, 0, "build list models request")
	}
	resp, err := tr.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, provider.NewProviderError(provider.FailAuth, false, 0, resp.StatusCode, "unauthorized")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, provider.NewProviderError(provider.FailUnavailable, true, 0, resp.StatusCode, "models endpoint error")
	}
	var out struct {
		Models []string `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, provider.NewProviderError(provider.FailMalformedResponse, false, 0, resp.StatusCode, "invalid models json")
	}
	return out.Models, nil
}

// ---------------------------------------------------------------------------
// AC-2: End-to-End Credential & Content Separation via Recording HTTP Server
// ---------------------------------------------------------------------------

func TestRuntime_AC2_EndToEndRecordingServer(t *testing.T) {
	const (
		secretKey        = "sk-ac2-synthetic-bearer-9988776655"
		referencedText   = "Please route invoice #42 to billing or engineering."
		unreferencedText = "UNREFERENCED_EXTRA_FIELD_MUST_NOT_CROSS_WIRE"
	)

	type recordedExchange struct {
		method  string
		url     string
		headers http.Header
		body    []byte
	}
	var (
		mu       sync.Mutex
		recorded []recordedExchange
	)

	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		recorded = append(recorded, recordedExchange{
			method:  r.Method,
			url:     r.URL.String(),
			headers: r.Header.Clone(),
			body:    body,
		})
		mu.Unlock()

		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":["jev-1.14.0","jev-latest"]}`))
			return

		case "/v1/evaluate":
			distBin, valBin, _, _ := provider.BinaryDistribution(0.92)
			resp := wireResponsePayload{
				ResolvedModel: "jev-1.14.0",
				Answers: map[string]provider.Answer{
					"asks_choice": {
						QuestionID:   "asks_choice",
						Primitive:    spec.PrimitiveBinary,
						Status:       provider.AnswerStatusAnswered,
						Bool:         valBin,
						Distribution: distBin,
					},
					"topic": {
						QuestionID: "topic",
						Primitive:  spec.PrimitiveChoice,
						Status:     provider.AnswerStatusAnswered,
						Choice:     strPtr("billing"),
						Distribution: map[string]float64{
							"billing":     0.88,
							"engineering": 0.09,
							"cannot_tell": 0.03,
						},
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Request-ID", "prov-req-ac2-1")
			_ = json.NewEncoder(w).Encode(resp)
			return

		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	reg := provider.NewRegistry()
	reg.MustRegister(newWireRecordingAdapter())

	inst := runtime.Instance{
		ID:       "inst-ac2",
		Name:     "typesafe-ac2",
		Adapter:  "wire-test",
		Model:    "jev-1.14.0",
		Endpoint: ts.URL,
		Auth:     runtime.InstanceAuth{Kind: "api_key", SecretName: "systemone.instance.inst-ac2"},
		Enabled:  true,
		Revision: 1,
	}

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	rt := runtime.New(
		runtime.Config{
			Registry:    reg,
			Instances:   runtime.StaticInstances(inst),
			Credentials: runtime.StaticCredential(secretKey),
			Logger:      logger,
		},
		runtime.WithEgressOptionsForTesting(egress.WithLoopbackServerForTesting(ts.URL, ts.Client())),
	)
	defer rt.CloseIdleConnections()

	compiled := compileTestSpec(t, mixedSpecJSON, spec.KindJudgments)
	in := runtime.Input{
		Fields: []provider.InputField{
			{Name: spec.ProbeFieldText, Kind: spec.FieldKindText, Value: referencedText, Trust: spec.TrustUntrusted},
			{Name: "extra.unreferenced", Kind: spec.FieldKindText, Value: unreferencedText, Trust: spec.TrustUntrusted},
		},
	}

	dec, err := rt.Evaluate(context.Background(), compiled, in, runtime.EvalOptions{
		Consumer: "probe",
		RuleID:   "ac2-rule",
		Revision: 1,
	})
	require.NoError(t, err)
	require.NotNil(t, dec.Eval)
	assert.Equal(t, "jev-1.14.0", dec.Eval.EffectiveResolvedModel())
	assert.Equal(t, "prov-req-ac2-1", dec.Eval.Attempts[0].ProviderRequestID)

	// Also exercise ListModels through the same authenticated egress seam.
	models, err := rt.ListModels(context.Background(), inst)
	require.NoError(t, err)
	assert.Equal(t, []string{"jev-1.14.0", "jev-latest"}, models)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, recorded, 2)

	// Inspect /v1/evaluate wire exchange for AC-2 invariants.
	evalEx := recorded[0]
	assert.Equal(t, http.MethodPost, evalEx.method)
	assert.Equal(t, "/v1/evaluate", evalEx.url)
	assert.NotContains(t, evalEx.url, secretKey)
	assert.Equal(t, "Bearer "+secretKey, evalEx.headers.Get("Authorization"))
	for hdr, vals := range evalEx.headers {
		if strings.EqualFold(hdr, "Authorization") {
			continue
		}
		for _, v := range vals {
			assert.NotContains(t, v, secretKey, "secret key leaked into header %s", hdr)
		}
	}

	// Secret key and unreferenced field must NOT appear anywhere in the HTTP request body.
	bodyStr := string(evalEx.body)
	assert.NotContains(t, bodyStr, secretKey, "secret key must never enter wire body")
	assert.NotContains(t, bodyStr, unreferencedText, "unreferenced input field must not enter wire body")
	assert.Contains(t, bodyStr, referencedText)

	var decodedWire wireRequestPayload
	require.NoError(t, json.Unmarshal(evalEx.body, &decodedWire))
	require.Len(t, decodedWire.Input, 1, "only the single rule-referenced probe.text field may be sent")
	assert.Equal(t, spec.ProbeFieldText, decodedWire.Input[0].Name)
	assert.Equal(t, referencedText, decodedWire.Input[0].Value)
	require.Len(t, decodedWire.Questions, 2)
	assert.Equal(t, "asks_choice", decodedWire.Questions[0].ID)
	assert.Equal(t, "topic", decodedWire.Questions[1].ID)

	// Inspect /v1/models wire exchange: bearer token present ONLY in Authorization.
	modelsEx := recorded[1]
	assert.Equal(t, http.MethodGet, modelsEx.method)
	assert.Equal(t, "Bearer "+secretKey, modelsEx.headers.Get("Authorization"))

	// Provenance Evaluation JSON and debug log must not contain secret key, input values, or Input.SHA256 in logs.
	evalJSON, err := json.Marshal(dec.Eval)
	require.NoError(t, err)
	assert.NotContains(t, string(evalJSON), secretKey)
	assert.NotContains(t, string(evalJSON), referencedText)
	assert.NotContains(t, string(evalJSON), unreferencedText)

	logs := logBuf.String()
	assert.NotContains(t, logs, secretKey)
	assert.NotContains(t, logs, referencedText)
	assert.NotContains(t, logs, unreferencedText)
	assert.NotContains(t, logs, dec.Eval.Input.SHA256, "Input.SHA256 must not be logged on debug lines")
}

// ---------------------------------------------------------------------------
// AC-4: Egress Boundaries Mapped Cleanly by Runtime (1 MiB cap, 3xx, SSRF, Auth)
// ---------------------------------------------------------------------------

type fakeResolverFunc func(ctx context.Context, host string) ([]net.IPAddr, error)

func (f fakeResolverFunc) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return f(ctx, host)
}

type fakeDialerFunc func(ctx context.Context, network, addr string) (net.Conn, error)

func (f fakeDialerFunc) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return f(ctx, network, addr)
}

func TestRuntime_AC4_EgressRecordingServerBoundaries(t *testing.T) {
	const secretKey = "sk-ac4-secret-do-not-echo"

	binSpec := `{
	  "version": 1,
	  "questions": [{"id": "q1", "type": "binary", "instructions": "Check."}],
	  "output": {"question": "q1"}
	}`
	compiled := compileTestSpec(t, binSpec, spec.KindFilter)

	t.Run("response_over_1MiB_maps_to_malformed_response", func(t *testing.T) {
		ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(bytes.Repeat([]byte("a"), int(egress.MaxResponseBytes)+64))
		}))
		defer ts.Close()

		reg := provider.NewRegistry()
		reg.MustRegister(newWireRecordingAdapter())
		inst := runtime.Instance{
			Name: "jev-ac4", Adapter: "wire-test", Model: "jev-1.14.0", Endpoint: ts.URL, Enabled: true, Revision: 1,
		}
		rt := runtime.New(
			runtime.Config{
				Registry:    reg,
				Instances:   runtime.StaticInstances(inst),
				Credentials: runtime.StaticCredential(secretKey),
			},
			runtime.WithEgressOptionsForTesting(egress.WithLoopbackServerForTesting(ts.URL, ts.Client())),
		)
		defer rt.CloseIdleConnections()

		_, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("hi"), runtime.EvalOptions{})
		require.Error(t, err)
		assert.NotContains(t, err.Error(), secretKey)

		eval, ok := runtime.EvaluationFrom(err)
		require.True(t, ok)
		assert.Equal(t, runtime.ClassFailed, eval.Class)
		assert.Equal(t, string(provider.FailMalformedResponse), eval.Reason)
	})

	t.Run("redirect_302_refused_and_mapped_to_unavailable", func(t *testing.T) {
		var targetHit atomic.Int32
		redirectTarget := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			targetHit.Add(1)
			w.WriteHeader(http.StatusOK)
		}))
		defer redirectTarget.Close()

		ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, redirectTarget.URL+"/steal", http.StatusFound)
		}))
		defer ts.Close()

		reg := provider.NewRegistry()
		reg.MustRegister(newWireRecordingAdapter())
		inst := runtime.Instance{
			Name: "jev-ac4", Adapter: "wire-test", Model: "jev-1.14.0", Endpoint: ts.URL, Enabled: true, Revision: 1,
		}
		rt := runtime.New(
			runtime.Config{
				Registry:    reg,
				Instances:   runtime.StaticInstances(inst),
				Credentials: runtime.StaticCredential(secretKey),
			},
			runtime.WithEgressOptionsForTesting(egress.WithLoopbackServerForTesting(ts.URL, ts.Client())),
		)
		defer rt.CloseIdleConnections()

		_, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("hi"), runtime.EvalOptions{})
		require.Error(t, err)
		assert.Zero(t, targetHit.Load(), "redirect target must never be contacted")
		assert.NotContains(t, err.Error(), secretKey)
		assert.NotContains(t, err.Error(), "/steal")

		eval, ok := runtime.EvaluationFrom(err)
		require.True(t, ok)
		assert.Equal(t, runtime.ClassFailed, eval.Class)
		assert.Equal(t, string(provider.FailUnavailable), eval.Reason)
	})

	t.Run("blocked_private_ip_ssrf_maps_to_unavailable_without_dialing", func(t *testing.T) {
		var dialed atomic.Int32
		resolver := fakeResolverFunc(func(_ context.Context, _ string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("10.0.0.42")}}, nil
		})
		dialer := fakeDialerFunc(func(_ context.Context, _, _ string) (net.Conn, error) {
			dialed.Add(1)
			return nil, errors.New("unexpected dial")
		})

		reg := provider.NewRegistry()
		reg.MustRegister(newWireRecordingAdapter())
		inst := runtime.Instance{
			Name: "jev-ssrf", Adapter: "wire-test", Model: "jev-1.14.0", Endpoint: "https://internal.example.invalid", Enabled: true, Revision: 1,
		}
		rt := runtime.New(
			runtime.Config{
				Registry:    reg,
				Instances:   runtime.StaticInstances(inst),
				Credentials: runtime.StaticCredential(secretKey),
			},
			runtime.WithEgressOptionsForTesting(egress.WithDialerForTesting(resolver, dialer)),
		)
		defer rt.CloseIdleConnections()

		_, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("hi"), runtime.EvalOptions{})
		require.Error(t, err)
		assert.Zero(t, dialed.Load(), "blocked private IP must be rejected before TCP dial")
		assert.NotContains(t, err.Error(), secretKey)

		eval, ok := runtime.EvaluationFrom(err)
		require.True(t, ok)
		assert.Equal(t, runtime.ClassFailed, eval.Class)
		assert.Equal(t, string(provider.FailUnavailable), eval.Reason)
	})

	t.Run("non_https_endpoint_maps_to_invalid_request", func(t *testing.T) {
		reg := provider.NewRegistry()
		reg.MustRegister(newWireRecordingAdapter())
		inst := runtime.Instance{
			Name: "jev-http", Adapter: "wire-test", Model: "jev-1.14.0", Endpoint: "http://plaintext.example.invalid", Enabled: true, Revision: 1,
		}
		rt := runtime.New(runtime.Config{
			Registry:    reg,
			Instances:   runtime.StaticInstances(inst),
			Credentials: runtime.StaticCredential(secretKey),
		})
		defer rt.CloseIdleConnections()

		_, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("hi"), runtime.EvalOptions{})
		require.Error(t, err)
		eval, ok := runtime.EvaluationFrom(err)
		require.True(t, ok)
		assert.Equal(t, runtime.ClassFailed, eval.Class)
		assert.Equal(t, string(provider.FailInvalidRequest), eval.Reason)
	})

	t.Run("credential_source_error_maps_to_auth_without_echoing_secret", func(t *testing.T) {
		ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			t.Fatal("server must not be contacted when credential resolution fails")
		}))
		defer ts.Close()

		reg := provider.NewRegistry()
		reg.MustRegister(newWireRecordingAdapter())
		inst := runtime.Instance{
			Name: "jev-auth", Adapter: "wire-test", Model: "jev-1.14.0", Endpoint: ts.URL, Enabled: true, Revision: 1,
		}
		rt := runtime.New(
			runtime.Config{
				Registry:  reg,
				Instances: runtime.StaticInstances(inst),
				Credentials: func(context.Context, runtime.Instance) (string, error) {
					return "", errors.New("vault error decrypting " + secretKey)
				},
			},
			runtime.WithEgressOptionsForTesting(egress.WithLoopbackServerForTesting(ts.URL, ts.Client())),
		)
		defer rt.CloseIdleConnections()

		_, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("hi"), runtime.EvalOptions{})
		require.Error(t, err)
		assert.NotContains(t, err.Error(), secretKey)

		eval, ok := runtime.EvaluationFrom(err)
		require.True(t, ok)
		assert.Equal(t, runtime.ClassFailed, eval.Class)
		assert.Equal(t, string(provider.FailAuth), eval.Reason)
	})

	t.Run("http_status_401_429_503_single_attempt_classification", func(t *testing.T) {
		cases := []struct {
			status    int
			want      provider.FailReason
			wantClass string
		}{
			{status: http.StatusUnauthorized, want: provider.FailAuth, wantClass: "4xx"},
			{status: http.StatusTooManyRequests, want: provider.FailRateLimited, wantClass: "4xx"},
			{status: http.StatusServiceUnavailable, want: provider.FailUnavailable, wantClass: "5xx"},
		}
		for _, tc := range cases {
			var hits atomic.Int32
			ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				hits.Add(1)
				w.Header().Set("X-Request-ID", "req-err-status")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"error":"SENSITIVE_PROVIDER_ERROR_BODY"}`))
			}))

			reg := provider.NewRegistry()
			reg.MustRegister(newWireRecordingAdapter())
			inst := runtime.Instance{
				Name: "jev-status", Adapter: "wire-test", Model: "jev-1.14.0", Endpoint: ts.URL, Enabled: true, Revision: 1,
			}
			rt := runtime.New(
				runtime.Config{
					Registry:    reg,
					Instances:   runtime.StaticInstances(inst),
					Credentials: runtime.StaticCredential(secretKey),
				},
				runtime.WithEgressOptionsForTesting(egress.WithLoopbackServerForTesting(ts.URL, ts.Client())),
			)

			_, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput("hi"), runtime.EvalOptions{})
			rt.CloseIdleConnections()
			ts.Close()

			require.Error(t, err)
			assert.Equal(t, int32(1), hits.Load(), "must make exactly 1 attempt without retry")
			assert.NotContains(t, err.Error(), "SENSITIVE_PROVIDER_ERROR_BODY")
			eval, ok := runtime.EvaluationFrom(err)
			require.True(t, ok)
			assert.Equal(t, string(tc.want), eval.Reason)
			assert.Equal(t, tc.wantClass, eval.Attempts[0].HTTPStatusClass)
			assert.Equal(t, "req-err-status", eval.Attempts[0].ProviderRequestID)
		}
	})
}

// ---------------------------------------------------------------------------
// AC-5: Deadlines & Cancellation Against Hanging Recording Server
// ---------------------------------------------------------------------------

func TestRuntime_AC5_DeadlinesAndCancellation(t *testing.T) {
	const (
		targetDeadline = 60 * time.Millisecond
		maxSlack       = 50 * time.Millisecond
	)

	var (
		notifyMu  sync.Mutex
		onArrival chan struct{}
	)
	setArrivalSignal := func(ch chan struct{}) {
		notifyMu.Lock()
		onArrival = ch
		notifyMu.Unlock()
	}

	// Hanging loopback server that drains the request body and blocks until
	// either the request context disconnects or the test closes serverDone.
	serverDone := make(chan struct{})
	hangingServer := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		notifyMu.Lock()
		ch := onArrival
		notifyMu.Unlock()
		if ch != nil {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
		select {
		case <-r.Context().Done():
		case <-serverDone:
		}
	}))
	defer func() {
		close(serverDone)
		hangingServer.CloseClientConnections()
		hangingServer.Close()
	}()

	reg := provider.NewRegistry()
	reg.MustRegister(newWireRecordingAdapter())

	baseInst := func() runtime.Instance {
		return runtime.Instance{
			ID:       "inst-ac5",
			Name:     "jev-ac5",
			Adapter:  "wire-test",
			Model:    "jev-1.14.0",
			Endpoint: hangingServer.URL,
			Enabled:  true,
			Revision: 1,
		}
	}

	defaultSpecRaw := `{
	  "version": 1,
	  "questions": [{"id": "q1", "type": "binary", "instructions": "Check."}],
	  "output": {"question": "q1"}
	}`
	ruleTimeoutSpecRaw := `{
	  "dialect": "system_one",
	  "inputs": ["probe.text"],
	  "timeout": "60ms",
	  "spec": {
	    "version": 1,
	    "questions": [{"id": "q1", "type": "binary", "instructions": "Check."}],
	    "output": {"question": "q1"}
	  }
	}`

	compiledDefault := compileTestSpec(t, defaultSpecRaw, spec.KindFilter)
	compiledRule60ms := compileTestSpec(t, ruleTimeoutSpecRaw, spec.KindFilter)

	runtimeDeadlineCases := []struct {
		name        string
		compiled    *spec.Compiled
		cfgMaxEval  time.Duration
		callTimeout time.Duration
		opts        runtime.EvalOptions
	}{
		{
			name:     "rule_timeout_60ms",
			compiled: compiledRule60ms,
		},
		{
			name:       "config_max_eval_time_60ms",
			compiled:   compiledDefault,
			cfgMaxEval: targetDeadline,
		},
		{
			name:     "eval_options_max_eval_time_60ms",
			compiled: compiledDefault,
			opts:     runtime.EvalOptions{MaxEvalTime: targetDeadline},
		},
		{
			name:        "instance_call_timeout_60ms",
			compiled:    compiledDefault,
			callTimeout: targetDeadline,
		},
		{
			name:     "consumer_budget_60ms",
			compiled: compiledDefault,
			opts:     runtime.EvalOptions{Budget: targetDeadline},
		},
	}

	for _, tc := range runtimeDeadlineCases {
		t.Run(tc.name, func(t *testing.T) {
			inst := baseInst()
			inst.Limits.CallTimeout = tc.callTimeout

			rt := runtime.New(
				runtime.Config{
					Registry:    reg,
					Instances:   runtime.StaticInstances(inst),
					Credentials: runtime.StaticCredential("sk-ac5"),
					MaxEvalTime: tc.cfgMaxEval,
				},
				runtime.WithEgressOptionsForTesting(
					egress.WithLoopbackServerForTesting(hangingServer.URL, hangingServer.Client()),
				),
			)
			defer rt.CloseIdleConnections()

			start := time.Now()
			dec, err := rt.Evaluate(context.Background(), tc.compiled, runtime.ProbeInput("hang"), tc.opts)
			elapsed := time.Since(start)

			require.Error(t, err)
			assert.LessOrEqual(t, elapsed, targetDeadline+maxSlack,
				"runtime deadline source %s must terminate within deadline (%v) + %v; got %v",
				tc.name, targetDeadline, maxSlack, elapsed)

			eval, ok := runtime.EvaluationFrom(err)
			require.True(t, ok, "timeout must carry Evaluation in EvaluationError")
			assert.Same(t, eval, dec.Eval)
			assert.Equal(t, runtime.ClassFailed, eval.Class)
			assert.Equal(t, string(provider.FailTimeout), eval.Reason)
			require.Len(t, eval.Attempts, 1)
			assert.Equal(t, runtime.AttemptFailed, eval.Attempts[0].Outcome)
			assert.Equal(t, string(provider.FailTimeout), eval.Attempts[0].Reason)
		})
	}

	t.Run("caller_context_cancel_in_flight_ends_within_50ms_with_canceled", func(t *testing.T) {
		arrived := make(chan struct{}, 1)
		setArrivalSignal(arrived)
		defer setArrivalSignal(nil)

		inst := baseInst()
		rt := runtime.New(
			runtime.Config{
				Registry:    reg,
				Instances:   runtime.StaticInstances(inst),
				Credentials: runtime.StaticCredential("sk-ac5"),
			},
			runtime.WithEgressOptionsForTesting(
				egress.WithLoopbackServerForTesting(hangingServer.URL, hangingServer.Client()),
			),
		)
		defer rt.CloseIdleConnections()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		type evalOutcome struct {
			err       error
			completed time.Time
		}
		done := make(chan evalOutcome, 1)
		go func() {
			_, err := rt.Evaluate(ctx, compiledDefault, runtime.ProbeInput("hang"), runtime.EvalOptions{})
			done <- evalOutcome{err: err, completed: time.Now()}
		}()

		select {
		case <-arrived:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for hanging server to receive request")
		}

		canceledAt := time.Now()
		cancel()

		out := <-done
		cancelLatency := out.completed.Sub(canceledAt)
		require.Error(t, out.err)
		assert.LessOrEqual(t, cancelLatency, maxSlack,
			"caller cancel must abandon in-flight call within %v; got %v", maxSlack, cancelLatency)

		eval, ok := runtime.EvaluationFrom(out.err)
		require.True(t, ok)
		assert.Equal(t, runtime.ClassFailed, eval.Class)
		assert.Equal(t, string(provider.FailCanceled), eval.Reason)
		require.Len(t, eval.Attempts, 1)
		assert.Equal(t, string(provider.FailCanceled), eval.Attempts[0].Reason)
	})

	t.Run("caller_shorter_context_deadline_yields_canceled", func(t *testing.T) {
		inst := baseInst()
		rt := runtime.New(
			runtime.Config{
				Registry:    reg,
				Instances:   runtime.StaticInstances(inst),
				Credentials: runtime.StaticCredential("sk-ac5"),
			},
			runtime.WithEgressOptionsForTesting(
				egress.WithLoopbackServerForTesting(hangingServer.URL, hangingServer.Client()),
			),
		)
		defer rt.CloseIdleConnections()

		ctx, cancel := context.WithTimeout(context.Background(), targetDeadline)
		defer cancel()

		start := time.Now()
		_, err := rt.Evaluate(ctx, compiledDefault, runtime.ProbeInput("hang"), runtime.EvalOptions{})
		elapsed := time.Since(start)

		require.Error(t, err)
		assert.LessOrEqual(t, elapsed, targetDeadline+maxSlack)

		eval, ok := runtime.EvaluationFrom(err)
		require.True(t, ok)
		assert.Equal(t, runtime.ClassFailed, eval.Class)
		assert.Equal(t, string(provider.FailCanceled), eval.Reason)
	})

	t.Run("pre_canceled_caller_context_returns_canceled_without_network_call", func(t *testing.T) {
		arrived := make(chan struct{}, 1)
		setArrivalSignal(arrived)
		defer setArrivalSignal(nil)

		inst := baseInst()
		rt := runtime.New(
			runtime.Config{
				Registry:    reg,
				Instances:   runtime.StaticInstances(inst),
				Credentials: runtime.StaticCredential("sk-ac5"),
			},
			runtime.WithEgressOptionsForTesting(
				egress.WithLoopbackServerForTesting(hangingServer.URL, hangingServer.Client()),
			),
		)
		defer rt.CloseIdleConnections()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		start := time.Now()
		_, err := rt.Evaluate(ctx, compiledDefault, runtime.ProbeInput("hang"), runtime.EvalOptions{})
		elapsed := time.Since(start)

		require.Error(t, err)
		assert.LessOrEqual(t, elapsed, maxSlack)
		select {
		case <-arrived:
			t.Fatal("pre-canceled call must not hit the server")
		default:
		}

		eval, ok := runtime.EvaluationFrom(err)
		require.True(t, ok)
		assert.Equal(t, runtime.ClassFailed, eval.Class)
		assert.Equal(t, string(provider.FailCanceled), eval.Reason)
		assert.Empty(t, eval.Attempts)
	})
}

// ---------------------------------------------------------------------------
// Optional End-to-End Integration: Runtime -> Egress -> jevwire ("typesafe")
// ---------------------------------------------------------------------------

func TestRuntime_EndToEndWithTypeSafeJevwireAdapter(t *testing.T) {
	// Synthetic from docs (not recorded from the live API): TypeSafe OpenAPI 0.2.0
	// POST /v1/systemone and GET /v1/models schemas.
	const (
		secretKey = "sk-typesafe-e2e-synthetic-778899"
		probeText = "Please route this invoice discrepancy to billing."
	)

	var (
		mu              sync.Mutex
		capturedHeaders http.Header
		capturedBody    []byte
	)

	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/systemone":
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			capturedHeaders = r.Header.Clone()
			capturedBody = body
			mu.Unlock()

			w.Header().Set("X-TypeSafe-Request-Id", "req_synth_e2e_01")
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{
				"model": "jev-latest",
				"answers": {
					"asks_choice": {"type": "noul", "noul": 0.90},
					"topic": {
						"type": "choice",
						"choice": "billing",
						"confidence": 0.775,
						"probabilities": {"billing": 0.85, "engineering": 0.10, "cannot_tell": 0.05}
					}
				},
				"usage": {"input_tokens": 38, "output_tokens": 4}
			}`)

		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"models":[{"name":"jev-1.14.0","description":"Jev 1.14","release_date":"2026-01-15"},{"name":"jev-latest","description":"Latest Jev alias","release_date":"2026-01-15"}]}`)

		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	inst := runtime.Instance{
		ID:       "inst-typesafe-e2e",
		Name:     "typesafe-prod",
		Adapter:  jevwire.AdapterNameTypeSafe,
		Model:    "jev-latest",
		Endpoint: ts.URL,
		Auth:     runtime.InstanceAuth{Kind: "api_key", SecretName: "systemone.instance.inst-typesafe-e2e"},
		Enabled:  true,
		Revision: 2,
	}

	// Uses DefaultRegistry() where jevwire registers "typesafe" via init().
	rt := runtime.New(
		runtime.Config{
			Instances:   runtime.StaticInstances(inst),
			Credentials: runtime.StaticCredential(secretKey),
		},
		runtime.WithEgressOptionsForTesting(egress.WithLoopbackServerForTesting(ts.URL, ts.Client())),
	)
	defer rt.CloseIdleConnections()

	compiled := compileTestSpec(t, mixedSpecJSON, spec.KindJudgments)
	dec, err := rt.Evaluate(context.Background(), compiled, runtime.ProbeInput(probeText), runtime.EvalOptions{
		Consumer: "probe",
		RuleID:   "e2e-typesafe",
		Revision: 1,
	})
	require.NoError(t, err)
	require.NotNil(t, dec.Eval)
	assert.Equal(t, "jev-latest", dec.Eval.ConfiguredModel)
	assert.Equal(t, "jev-latest", dec.Eval.ResolvedModel)
	assert.True(t, dec.Eval.HasModelAliasReported(), "echoed jev-latest alias must carry model_alias_reported note")
	assert.Empty(t, dec.Eval.EffectiveResolvedModel())
	assert.Equal(t, "req_synth_e2e_01", dec.Eval.Attempts[0].ProviderRequestID)
	assert.Equal(t, "billing", *dec.Eval.Answers["topic"].Choice)
	assert.InDelta(t, 0.85, dec.Eval.Answers["topic"].Metrics[metrics.NamePTop].Value, 1e-9)
	assert.InDelta(t, 0.775, dec.Eval.Answers["topic"].NativeMetrics["typesafe.confidence"].Value, 1e-9)

	models, err := rt.ListModels(context.Background(), inst)
	require.NoError(t, err)
	assert.Equal(t, []string{"jev-1.14.0", "jev-latest"}, models)

	mu.Lock()
	assert.Equal(t, "Bearer "+secretKey, capturedHeaders.Get("Authorization"))
	assert.NotContains(t, string(capturedBody), secretKey)
	assert.Contains(t, string(capturedBody), probeText)
	mu.Unlock()

	t.Run("binary_tie_boundary_shared_provider_IsTie_across_jevwire_and_runtime", func(t *testing.T) {
		binSpecRaw := `{
		  "version": 1,
		  "questions": [{"id": "q1", "type": "binary", "instructions": "Check."}],
		  "output": {"question": "q1"}
		}`
		binCompiled := compileTestSpec(t, binSpecRaw, spec.KindFilter)

		var noulVal atomic.Value
		tieSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			p := noulVal.Load().(float64)
			w.Header().Set("X-TypeSafe-Request-Id", "req_synth_tie")
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{
				"model": "jev-1.14.0",
				"answers": {"q1": {"type": "noul", "noul": %.17g}},
				"usage": {"input_tokens": 12, "output_tokens": 1}
			}`, p)
		}))
		defer tieSrv.Close()

		tieInst := runtime.Instance{
			ID:       "inst-typesafe-tie",
			Name:     "typesafe-tie",
			Adapter:  jevwire.AdapterNameTypeSafe,
			Model:    "jev-1.14.0",
			Endpoint: tieSrv.URL,
			Enabled:  true,
			Revision: 1,
		}
		tieRT := runtime.New(
			runtime.Config{
				Instances:   runtime.StaticInstances(tieInst),
				Credentials: runtime.StaticCredential(secretKey),
			},
			runtime.WithEgressOptionsForTesting(egress.WithLoopbackServerForTesting(tieSrv.URL, tieSrv.Client())),
		)
		defer tieRT.CloseIdleConnections()

		// 0.5 + 4e-10 (|2p - 1| = 8e-10 <= 1e-9) -> Uncertain("tie"), never malformed_response.
		noulVal.Store(0.5 + 4e-10)
		_, err := tieRT.Evaluate(context.Background(), binCompiled, runtime.ProbeInput("borderline"), runtime.EvalOptions{})
		require.Error(t, err)
		evalTie, ok := runtime.EvaluationFrom(err)
		require.True(t, ok)
		assert.Equal(t, runtime.ClassUncertain, evalTie.Class)
		assert.Equal(t, provider.UncertainTie, evalTie.Reason)
		assert.True(t, evalTie.Answers["q1"].Tied)
		assert.Nil(t, evalTie.Answers["q1"].Bool)

		// Reviewer reproduction value 0.5000000003 (|2p - 1| = 6e-10 <= 1e-9) -> Uncertain("tie").
		noulVal.Store(0.5000000003)
		_, err = tieRT.Evaluate(context.Background(), binCompiled, runtime.ProbeInput("borderline-2"), runtime.EvalOptions{})
		require.Error(t, err)
		evalTie2, ok := runtime.EvaluationFrom(err)
		require.True(t, ok)
		assert.Equal(t, runtime.ClassUncertain, evalTie2.Class)
		assert.Equal(t, provider.UncertainTie, evalTie2.Reason)

		// 0.5 + 2e-9 (|2p - 1| = 4e-9 > 1e-9) -> Success(true).
		noulVal.Store(0.5 + 2e-9)
		decOK, err := tieRT.Evaluate(context.Background(), binCompiled, runtime.ProbeInput("clear"), runtime.EvalOptions{})
		require.NoError(t, err)
		assert.True(t, decOK.Bool)
		assert.Equal(t, runtime.ClassSuccess, decOK.Eval.Class)
	})

	t.Run("http_429_preserves_provider_request_id_and_status_class", func(t *testing.T) {
		errSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-TypeSafe-Request-Id", "req_synth_429_99")
			w.Header().Set("Retry-After", "3")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"detail":"rate limit exceeded"}`)
		}))
		defer errSrv.Close()

		errInst := runtime.Instance{
			ID:       "inst-typesafe-429",
			Name:     "typesafe-429",
			Adapter:  jevwire.AdapterNameTypeSafe,
			Model:    "jev-1.14.0",
			Endpoint: errSrv.URL,
			Enabled:  true,
			Revision: 1,
		}
		errRT := runtime.New(
			runtime.Config{
				Instances:   runtime.StaticInstances(errInst),
				Credentials: runtime.StaticCredential(secretKey),
			},
			runtime.WithEgressOptionsForTesting(egress.WithLoopbackServerForTesting(errSrv.URL, errSrv.Client())),
		)
		defer errRT.CloseIdleConnections()

		_, err := errRT.Evaluate(context.Background(), compiled, runtime.ProbeInput(probeText), runtime.EvalOptions{})
		require.Error(t, err)
		eval429, ok := runtime.EvaluationFrom(err)
		require.True(t, ok)
		assert.Equal(t, runtime.ClassFailed, eval429.Class)
		assert.Equal(t, string(provider.FailRateLimited), eval429.Reason)
		require.Len(t, eval429.Attempts, 1)
		assert.Equal(t, "req_synth_429_99", eval429.Attempts[0].ProviderRequestID)
		assert.Equal(t, "4xx", eval429.Attempts[0].HTTPStatusClass)
	})
}
