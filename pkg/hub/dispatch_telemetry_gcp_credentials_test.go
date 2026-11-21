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

package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestWarnGCPTelemetryBlockedWithoutCredentials(t *testing.T) {
	boolPtr := func(v bool) *bool { return &v }
	gcpHubDefault := &api.TelemetryConfig{
		Enabled: boolPtr(true),
		Cloud: &api.TelemetryCloudConfig{
			Enabled:  boolPtr(true),
			Provider: "gcp",
		},
	}
	otlpHubDefault := &api.TelemetryConfig{
		Enabled: boolPtr(true),
		Cloud: &api.TelemetryCloudConfig{
			Enabled:  boolPtr(true),
			Endpoint: "host.docker.internal:4317",
			Protocol: "grpc",
		},
	}
	blockAgent := &store.Agent{
		ID:   "agent-1",
		Name: "a1",
		AppliedConfig: &store.AgentAppliedConfig{
			GCPIdentity: &store.GCPIdentityConfig{MetadataMode: store.GCPMetadataModeBlock},
		},
	}
	assignAgent := &store.Agent{
		ID:   "agent-2",
		Name: "a2",
		AppliedConfig: &store.AgentAppliedConfig{
			GCPIdentity: &store.GCPIdentityConfig{MetadataMode: store.GCPMetadataModeAssign},
		},
	}
	credsSecret := []ResolvedSecret{{
		Name:   telemetryGCPCredentialsSecretName,
		Type:   store.SecretTypeFile,
		Target: "~/.scion/telemetry-gcp-credentials.json",
		Value:  `{"type":"service_account"}`,
	}}

	cases := []struct {
		name     string
		agent    *store.Agent
		tel      telemetryDispatch
		env      map[string]string
		secrets  []ResolvedSecret
		wantWarn bool
	}{
		{
			name:     "warns when GCP cloud on, block mode, no credentials secret",
			agent:    blockAgent,
			tel:      telemetryDispatch{hubDefault: gcpHubDefault},
			wantWarn: true,
		},
		{
			name:     "no warning when credentials file secret is present",
			agent:    blockAgent,
			tel:      telemetryDispatch{hubDefault: gcpHubDefault},
			secrets:  credsSecret,
			wantWarn: false,
		},
		{
			name:     "no warning when SCION_OTEL_GCP_CREDENTIALS env is set",
			agent:    blockAgent,
			tel:      telemetryDispatch{hubDefault: gcpHubDefault},
			env:      map[string]string{"SCION_OTEL_GCP_CREDENTIALS": "/home/scion/.scion/telemetry-gcp-credentials.json"},
			wantWarn: false,
		},
		{
			name:     "no warning when metadata mode is assign",
			agent:    assignAgent,
			tel:      telemetryDispatch{hubDefault: gcpHubDefault},
			wantWarn: false,
		},
		{
			name:     "no warning when using generic OTLP collector",
			agent:    blockAgent,
			tel:      telemetryDispatch{hubDefault: otlpHubDefault},
			wantWarn: false,
		},
		{
			name:     "no warning when project policy disables telemetry",
			agent:    blockAgent,
			tel:      telemetryDispatch{hubDefault: gcpHubDefault, policy: boolPtr(false)},
			wantWarn: false,
		},
		{
			name:  "no warning when requester disables telemetry",
			agent: blockAgent,
			tel: telemetryDispatch{
				hubDefault: gcpHubDefault,
				requester:  &api.TelemetryConfig{Enabled: boolPtr(false)},
			},
			wantWarn: false,
		},
		{
			name:  "no warning when cloud.enabled is false",
			agent: blockAgent,
			tel: telemetryDispatch{
				hubDefault: &api.TelemetryConfig{
					Enabled: boolPtr(true),
					Cloud:   &api.TelemetryCloudConfig{Enabled: boolPtr(false), Provider: "gcp"},
				},
			},
			wantWarn: false,
		},
	}

	d := &HTTPAgentDispatcher{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, collector := withDispatchWarnings(context.Background())
			d.warnGCPTelemetryBlockedWithoutCredentials(ctx, tc.agent, tc.tel, tc.env, tc.secrets)
			warnings := collector.Warnings()
			if tc.wantWarn {
				if len(warnings) != 1 {
					t.Fatalf("warnings = %v, want 1 warning", warnings)
				}
				for _, want := range []string{"block", telemetryGCPCredentialsSecretName, "--service-account"} {
					if !strings.Contains(warnings[0], want) {
						t.Errorf("warning %q missing %q", warnings[0], want)
					}
				}
			} else if len(warnings) != 0 {
				t.Fatalf("warnings = %v, want none", warnings)
			}
		})
	}
}

type fakeMetricsHintSecretBackend struct {
	secret.SecretBackend
	metas map[string]*secret.SecretMeta
}

func (f *fakeMetricsHintSecretBackend) GetMeta(_ context.Context, name, scope, scopeID string) (*secret.SecretMeta, error) {
	key := scope + ":" + scopeID + ":" + name
	if m, ok := f.metas[key]; ok {
		return m, nil
	}
	return nil, store.ErrNotFound
}

func TestMetricsDashboard_TelemetryHintWhenGCPBlockedWithoutCredentials(t *testing.T) {
	boolPtr := func(v bool) *bool { return &v }
	gcpTel := &api.TelemetryConfig{
		Enabled: boolPtr(true),
		Cloud: &api.TelemetryCloudConfig{
			Enabled:  boolPtr(true),
			Provider: "gcp",
		},
	}
	otlpTel := &api.TelemetryConfig{
		Enabled: boolPtr(true),
		Cloud: &api.TelemetryCloudConfig{
			Enabled:  boolPtr(true),
			Endpoint: "host.docker.internal:4317",
			Protocol: "grpc",
		},
	}

	newServer := func(tel *api.TelemetryConfig, defaultMode string, metas map[string]*secret.SecretMeta) *Server {
		srv := &Server{
			hubID: "hub-test",
			config: ServerConfig{
				TelemetryConfig: tel,
				AgentDefaults: opsettings.AgentDefaultsSettings{
					DefaultGCPIdentityMode: defaultMode,
				},
			},
			secretBackend: &fakeMetricsHintSecretBackend{metas: metas},
			metricsDashboard: &MetricsDashboardService{
				client:    &fakeMetricsClient{},
				projectID: "test-gcp-project",
				cache:     make(map[string]*cacheEntry),
			},
		}
		return srv
	}

	t.Run("shows hint when GCP cloud on, default block mode, secret missing", func(t *testing.T) {
		srv := newServer(gcpTel, "", nil)
		rec := httptest.NewRecorder()
		srv.serveMetricsDashboard(rec, httptest.NewRequest(http.MethodGet, "/api/v1/metrics/?view=summary&period=7", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		var got DashboardSummary
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.TelemetryHint == "" {
			t.Fatal("expected non-empty telemetryHint, got empty")
		}
		for _, want := range []string{"block", telemetryGCPCredentialsSecretName, "--service-account"} {
			if !strings.Contains(got.TelemetryHint, want) {
				t.Errorf("telemetryHint %q missing %q", got.TelemetryHint, want)
			}
		}
	})

	t.Run("hides hint when deliverable hub secret is configured", func(t *testing.T) {
		metas := map[string]*secret.SecretMeta{
			"hub:hub-test:" + telemetryGCPCredentialsSecretName: {
				Name:       telemetryGCPCredentialsSecretName,
				Scope:      store.ScopeHub,
				ScopeID:    "hub-test",
				SecretType: store.SecretTypeFile,
				Target:     "~/.scion/telemetry-gcp-credentials.json",
			},
		}
		srv := newServer(gcpTel, store.GCPMetadataModeBlock, metas)
		rec := httptest.NewRecorder()
		srv.serveMetricsDashboard(rec, httptest.NewRequest(http.MethodGet, "/api/v1/metrics/?view=summary&period=7", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		var got DashboardSummary
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.TelemetryHint != "" {
			t.Fatalf("expected empty telemetryHint when secret is configured, got %q", got.TelemetryHint)
		}
	})

	t.Run("hides hint when default metadata mode is assign", func(t *testing.T) {
		srv := newServer(gcpTel, store.GCPMetadataModeAssign, nil)
		rec := httptest.NewRecorder()
		srv.serveMetricsDashboard(rec, httptest.NewRequest(http.MethodGet, "/api/v1/metrics/?view=summary&period=7", nil))
		var got DashboardSummary
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.TelemetryHint != "" {
			t.Fatalf("expected empty telemetryHint in assign mode, got %q", got.TelemetryHint)
		}
	})

	t.Run("hides hint when using generic OTLP collector", func(t *testing.T) {
		srv := newServer(otlpTel, store.GCPMetadataModeBlock, nil)
		rec := httptest.NewRecorder()
		srv.serveMetricsDashboard(rec, httptest.NewRequest(http.MethodGet, "/api/v1/metrics/?view=summary&period=7", nil))
		var got DashboardSummary
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.TelemetryHint != "" {
			t.Fatalf("expected empty telemetryHint for OTLP collector, got %q", got.TelemetryHint)
		}
	})
}
