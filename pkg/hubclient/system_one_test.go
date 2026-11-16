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

package hubclient

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/metrics"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
	systemoneruntime "github.com/GoogleCloudPlatform/scion/pkg/systemone/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/spec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSystemOne_CreateListGetDelete(t *testing.T) {
	now := time.Date(2026, 11, 15, 20, 0, 0, 0, time.UTC)
	fakeSecret := "test-only-synthetic-key-999"

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/admin/system-one/instances", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer test-admin-token", r.Header.Get("Authorization"))
		switch r.Method {
		case http.MethodPost:
			dec := json.NewDecoder(r.Body)
			dec.DisallowUnknownFields()
			var req CreateSystemOneInstanceRequest
			require.NoError(t, dec.Decode(&req))
			assert.Equal(t, "ts-main", req.Name)
			assert.Equal(t, "typesafe", req.Adapter)
			assert.Equal(t, "jev-1.14.0", req.Model)
			require.NotNil(t, req.Auth)
			assert.Equal(t, "api_key", req.Auth.Kind)
			assert.Equal(t, fakeSecret, req.Auth.APIKey)

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(SystemOneInstance{
				ID:       "inst-123",
				Name:     req.Name,
				Adapter:  req.Adapter,
				Model:    req.Model,
				Endpoint: "https://api.typesafe.ai",
				Auth: store.SystemOneInstanceAuth{
					Kind:       "api_key",
					SecretName: "systemone.instance.inst-123",
				},
				Options:   req.Options,
				Limits:    SystemOneInstanceLimitsDTO{MaxInFlight: 8, RatePerSec: 10, CallTimeout: "5s"},
				Enabled:   true,
				Revision:  1,
				CreatedBy: "admin-1",
				UpdatedBy: "admin-1",
				CreatedAt: now,
				UpdatedAt: now,
			})
		case http.MethodGet:
			q := r.URL.Query()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(ListSystemOneInstancesResponse{
				Instances: []SystemOneInstance{
					{
						ID:       "inst-123",
						Name:     "ts-main",
						Adapter:  q.Get("adapter"),
						Model:    "jev-1.14.0",
						Endpoint: "https://api.typesafe.ai",
						Auth:     store.SystemOneInstanceAuth{Kind: "api_key", SecretName: "systemone.instance.inst-123"},
						Enabled:  q.Get("enabled") != "false",
						Revision: 1,
					},
				},
				NextCursor: q.Get("cursor"),
				TotalCount: 1,
			})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/api/v1/admin/system-one/instances/ts-main", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(SystemOneInstance{
				ID:        "inst-123",
				Name:      "ts-main",
				Adapter:   "typesafe",
				Model:     "jev-1.14.0",
				Endpoint:  "https://api.typesafe.ai",
				Auth:      store.SystemOneInstanceAuth{Kind: "api_key", SecretName: "systemone.instance.inst-123"},
				Enabled:   true,
				Revision:  2,
				CreatedAt: now,
				UpdatedAt: now,
			})
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/api/v1/admin/system-one/instances/missing", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"system_one_instance not found"}}`))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c, err := New(srv.URL, WithBearerToken("test-admin-token"))
	require.NoError(t, err)
	ctx := context.Background()

	created, err := c.SystemOne().CreateInstance(ctx, &CreateSystemOneInstanceRequest{
		Name:    "ts-main",
		Adapter: "typesafe",
		Model:   "jev-1.14.0",
		Auth: &CreateSystemOneInstanceAuthRequest{
			Kind:   "api_key",
			APIKey: fakeSecret,
		},
		Options: map[string]string{"profile": "default"},
	})
	require.NoError(t, err)
	assert.Equal(t, "inst-123", created.ID)
	assert.Equal(t, "systemone.instance.inst-123", created.Auth.SecretName)
	assert.Equal(t, "5s", created.Limits.CallTimeout)

	enabled := false
	listed, err := c.SystemOne().ListInstances(ctx, &ListSystemOneInstancesOptions{
		Adapter: "typesafe",
		Enabled: &enabled,
		Cursor:  "cur-1",
		Limit:   25,
	})
	require.NoError(t, err)
	require.Len(t, listed.Instances, 1)
	assert.Equal(t, "typesafe", listed.Instances[0].Adapter)
	assert.False(t, listed.Instances[0].Enabled)
	assert.Equal(t, "cur-1", listed.NextCursor)
	assert.Equal(t, 1, listed.TotalCount)

	got, err := c.SystemOne().GetInstance(ctx, "ts-main")
	require.NoError(t, err)
	assert.Equal(t, "ts-main", got.Name)
	assert.Equal(t, int64(2), got.Revision)

	require.NoError(t, c.SystemOne().DeleteInstance(ctx, "ts-main"))

	_, err = c.SystemOne().GetInstance(ctx, "missing")
	var apiErr *apiclient.APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusNotFound, apiErr.StatusCode)
	assert.Equal(t, "not_found", apiErr.Code)

	err = c.SystemOne().DeleteInstance(ctx, "missing")
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusNotFound, apiErr.StatusCode)
}

func TestSystemOne_ProbeModelsAndSpec(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/admin/system-one/instances/ts-main/probe", func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		raw := new(bytes.Buffer)
		_, err := raw.ReadFrom(r.Body)
		require.NoError(t, err)

		dec := json.NewDecoder(bytes.NewReader(raw.Bytes()))
		dec.DisallowUnknownFields()
		var req ProbeSystemOneInstanceRequest
		require.NoError(t, dec.Decode(&req))

		w.Header().Set("Content-Type", "application/json")
		if req.Models {
			assert.JSONEq(t, `{"models":true}`, raw.String())
			_ = json.NewEncoder(w).Encode(ProbeSystemOneInstanceResponse{
				Class:  systemoneruntime.ClassSuccess,
				Models: []string{"jev-1.14.0", "jev-1.13.0"},
			})
			return
		}

		require.NotNil(t, req.Probe)
		assert.Equal(t, "synthetic text", req.Probe.Text)
		require.NotEmpty(t, req.Spec)

		yes := true
		boolAnswer := true
		_ = json.NewEncoder(w).Encode(ProbeSystemOneInstanceResponse{
			Class: systemoneruntime.ClassSuccess,
			Decision: &ProbeSystemOneDecision{
				Kind: spec.KindFilter,
				Bool: &yes,
			},
			Evaluation: &systemoneruntime.Evaluation{
				EvalID:          "eval-1",
				ConfiguredModel: "jev-1.14.0",
				ResolvedModel:   "jev-1.14-prod-20261001",
				Class:           systemoneruntime.ClassSuccess,
				Notes:           []string{provider.NoteModelAliasReported},
				Answers: map[string]systemoneruntime.AnswerRecord{
					"q1": {
						QuestionID: "q1",
						Primitive:  spec.PrimitiveBinary,
						Status:     "answered",
						Required:   true,
						Bool:       &boolAnswer,
						Distribution: map[string]float64{
							"yes": 0.92,
							"no":  0.08,
						},
						Metrics: map[string]metrics.Metric{
							metrics.NamePTop:        {Value: 0.92, Unit: metrics.UnitProbability, Min: 0, Max: 1, Source: metrics.SourceScion},
							metrics.NameMargin:      {Value: 0.84, Unit: metrics.UnitProbability, Min: 0, Max: 1, Source: metrics.SourceScion},
							metrics.NameNormEntropy: {Value: 0.40, Unit: metrics.UnitFraction, Min: 0, Max: 1, Source: metrics.SourceScion},
						},
						NativeMetrics: map[string]metrics.Metric{
							"typesafe.confidence": {Value: 0.91, Unit: metrics.UnitProbability, Min: 0, Max: 1, Source: metrics.SourceProviderUndocumented},
						},
					},
				},
				Attempts: []systemoneruntime.Attempt{
					{
						Instance:         "ts-main",
						InstanceRevision: 1,
						Adapter:          "typesafe",
						AdapterVersion:   "v1",
						ConfiguredModel:  "jev-1.14.0",
						ReportedModel:    "jev-1.14-prod-20261001",
						Duration:         42 * time.Millisecond,
						Requests:         1,
						Outcome:          systemoneruntime.AttemptSuccess,
						HTTPStatusClass:  "2xx",
					},
				},
			},
		})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c, err := New(srv.URL)
	require.NoError(t, err)
	ctx := context.Background()

	modelsResp, err := c.SystemOne().ProbeInstance(ctx, "ts-main", &ProbeSystemOneInstanceRequest{Models: true})
	require.NoError(t, err)
	assert.Equal(t, systemoneruntime.ClassSuccess, modelsResp.Class)
	assert.Equal(t, []string{"jev-1.14.0", "jev-1.13.0"}, modelsResp.Models)

	specJSON := json.RawMessage(`{"version":1,"questions":[{"id":"q1","primitive":"binary","instructions":"Is it synthetic?"}]}`)
	evalResp, err := c.SystemOne().ProbeInstance(ctx, "ts-main", &ProbeSystemOneInstanceRequest{
		Kind:  string(spec.KindFilter),
		Spec:  specJSON,
		Probe: &ProbeSystemOneInput{Text: "synthetic text"},
	})
	require.NoError(t, err)
	assert.Equal(t, systemoneruntime.ClassSuccess, evalResp.Class)
	require.NotNil(t, evalResp.Decision)
	require.NotNil(t, evalResp.Decision.Bool)
	assert.True(t, *evalResp.Decision.Bool)
	require.NotNil(t, evalResp.Evaluation)
	assert.Equal(t, "jev-1.14.0", evalResp.Evaluation.ConfiguredModel)
	assert.Equal(t, "jev-1.14-prod-20261001", evalResp.Evaluation.ResolvedModel)
	assert.True(t, evalResp.Evaluation.HasModelAliasReported())
	assert.InDelta(t, 0.91, evalResp.Evaluation.Answers["q1"].NativeMetrics["typesafe.confidence"].Value, 1e-6)
}

func TestSystemOne_WritesDoNotRetry(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	c, err := New(srv.URL, WithRetry(3, time.Millisecond))
	require.NoError(t, err)
	ctx := context.Background()

	_, err = c.SystemOne().CreateInstance(ctx, &CreateSystemOneInstanceRequest{Name: "ts", Adapter: "typesafe"})
	require.Error(t, err)
	assert.Equal(t, int32(1), hits.Load())

	hits.Store(0)
	_, err = c.SystemOne().ProbeInstance(ctx, "ts", &ProbeSystemOneInstanceRequest{Models: true})
	require.Error(t, err)
	assert.Equal(t, int32(1), hits.Load())
}

func TestSystemOne_ValidationAndRedaction(t *testing.T) {
	c, err := New("http://127.0.0.1:0")
	require.NoError(t, err)
	ctx := context.Background()

	_, err = c.SystemOne().CreateInstance(ctx, nil)
	require.Error(t, err)
	_, err = c.SystemOne().GetInstance(ctx, "")
	require.Error(t, err)
	err = c.SystemOne().DeleteInstance(ctx, "")
	require.Error(t, err)
	_, err = c.SystemOne().ProbeInstance(ctx, "", &ProbeSystemOneInstanceRequest{Models: true})
	require.Error(t, err)
	_, err = c.SystemOne().ProbeInstance(ctx, "ts", nil)
	require.Error(t, err)

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	sentinelKey := "super-secret-synthetic-api-key"
	sentinelText := "private-probe-input-text"
	sentinelSpec := `{"secret_prompt":"do-not-log"}`

	createReq := CreateSystemOneInstanceRequest{
		Name:    "ts-1",
		Adapter: "typesafe",
		APIKey:  sentinelKey,
		Auth: &CreateSystemOneInstanceAuthRequest{
			Kind:   "api_key",
			APIKey: sentinelKey,
		},
	}
	probeReq := ProbeSystemOneInstanceRequest{
		Kind:      "predicate",
		RuleID:    "r1",
		Spec:      json.RawMessage(sentinelSpec),
		Probe:     &ProbeSystemOneInput{Text: sentinelText},
		ProbeText: &sentinelText,
	}
	logger.Info("requests",
		slog.Any("create", createReq),
		slog.Any("auth", *createReq.Auth),
		slog.Any("probe", probeReq),
		slog.Any("probe_input", *probeReq.Probe),
	)

	logged := buf.String()
	assert.NotContains(t, logged, sentinelKey)
	assert.NotContains(t, logged, sentinelText)
	assert.NotContains(t, logged, "do-not-log")
	assert.Contains(t, logged, `"has_api_key":true`)
	assert.Contains(t, logged, `"text_bytes":`)
}
