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

package config

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

func TestConvertV1TelemetryToAPI_Nil(t *testing.T) {
	result := ConvertV1TelemetryToAPI(nil)
	if result != nil {
		t.Errorf("expected nil, got %+v", result)
	}
}

func TestConvertV1TelemetryToAPI_Full(t *testing.T) {
	enabled := true
	cloudEnabled := true
	insecure := false
	tlsEnabled := true
	caFile := "/etc/ssl/certs/custom-root.pem"
	certFile := "/etc/ssl/certs/client.pem"
	keyFile := "/etc/ssl/private/client-key.pem"
	hubEnabled := true
	localEnabled := true
	console := false
	filterEnabled := true
	respectDebug := true

	v1 := &V1TelemetryConfig{
		Enabled: &enabled,
		Cloud: &V1TelemetryCloudConfig{
			Enabled:  &cloudEnabled,
			Endpoint: "otel.example.com:4317",
			Protocol: "grpc",
			Headers:  map[string]string{"Authorization": "Bearer tok"},
			TLS: &V1TelemetryTLSConfig{
				Enabled:            &tlsEnabled,
				InsecureSkipVerify: &insecure,
				CAFile:             caFile,
				CertFile:           certFile,
				KeyFile:            keyFile,
			},
			Batch: &V1TelemetryBatchConfig{
				MaxSize: 512,
				Timeout: "5s",
			},
			Provider: "gcp",
		},
		Hub: &V1TelemetryHubConfig{
			Enabled:        &hubEnabled,
			ReportInterval: "30s",
		},
		Local: &V1TelemetryLocalConfig{
			Enabled: &localEnabled,
			File:    "/tmp/telemetry.log",
			Console: &console,
		},
		Filter: &V1TelemetryFilterConfig{
			Enabled:          &filterEnabled,
			RespectDebugMode: &respectDebug,
			Events: &V1TelemetryEventsConfig{
				Include: []string{"agent.tool.call"},
				Exclude: []string{"agent.user.prompt"},
			},
			Attributes: &V1TelemetryAttributesConfig{
				Redact: []string{"prompt"},
				Hash:   []string{"session_id"},
			},
		},
		Resource: map[string]string{"service.name": "scion-agent"},
	}

	result := ConvertV1TelemetryToAPI(v1)
	if result == nil {
		t.Fatal("expected non-nil result")
	}

	// Top-level
	if result.Enabled == nil || *result.Enabled != true {
		t.Errorf("Enabled = %v, want true", result.Enabled)
	}

	// Cloud
	if result.Cloud == nil {
		t.Fatal("Cloud is nil")
	}
	if result.Cloud.Endpoint != "otel.example.com:4317" {
		t.Errorf("Cloud.Endpoint = %q, want %q", result.Cloud.Endpoint, "otel.example.com:4317")
	}
	if result.Cloud.Protocol != "grpc" {
		t.Errorf("Cloud.Protocol = %q, want %q", result.Cloud.Protocol, "grpc")
	}
	if result.Cloud.Headers["Authorization"] != "Bearer tok" {
		t.Errorf("Cloud.Headers[Authorization] = %q", result.Cloud.Headers["Authorization"])
	}
	if result.Cloud.TLS == nil {
		t.Fatal("Cloud.TLS is nil")
	}
	if result.Cloud.TLS.InsecureSkipVerify == nil || *result.Cloud.TLS.InsecureSkipVerify != false {
		t.Errorf("Cloud.TLS.InsecureSkipVerify = %v, want false", result.Cloud.TLS.InsecureSkipVerify)
	}
	if result.Cloud.TLS.CAFile != caFile {
		t.Errorf("Cloud.TLS.CAFile = %q, want %q", result.Cloud.TLS.CAFile, caFile)
	}
	if result.Cloud.TLS.CertFile != certFile {
		t.Errorf("Cloud.TLS.CertFile = %q, want %q", result.Cloud.TLS.CertFile, certFile)
	}
	if result.Cloud.TLS.KeyFile != keyFile {
		t.Errorf("Cloud.TLS.KeyFile = %q, want %q", result.Cloud.TLS.KeyFile, keyFile)
	}
	if result.Cloud.Batch == nil {
		t.Fatal("Cloud.Batch is nil")
	}
	if result.Cloud.Batch.MaxSize != 512 {
		t.Errorf("Cloud.Batch.MaxSize = %d, want 512", result.Cloud.Batch.MaxSize)
	}
	if result.Cloud.Batch.Timeout != "5s" {
		t.Errorf("Cloud.Batch.Timeout = %q, want %q", result.Cloud.Batch.Timeout, "5s")
	}
	if result.Cloud.Provider != "gcp" {
		t.Errorf("Cloud.Provider = %q, want %q", result.Cloud.Provider, "gcp")
	}

	// Hub
	if result.Hub == nil {
		t.Fatal("Hub is nil")
	}
	if result.Hub.Enabled == nil || *result.Hub.Enabled != true {
		t.Errorf("Hub.Enabled = %v, want true", result.Hub.Enabled)
	}
	if result.Hub.ReportInterval != "30s" {
		t.Errorf("Hub.ReportInterval = %q, want %q", result.Hub.ReportInterval, "30s")
	}

	// Local
	if result.Local == nil {
		t.Fatal("Local is nil")
	}
	if result.Local.File != "/tmp/telemetry.log" {
		t.Errorf("Local.File = %q", result.Local.File)
	}
	if result.Local.Console == nil || *result.Local.Console != false {
		t.Errorf("Local.Console = %v, want false", result.Local.Console)
	}

	// Filter
	if result.Filter == nil {
		t.Fatal("Filter is nil")
	}
	if result.Filter.Enabled == nil || *result.Filter.Enabled != true {
		t.Errorf("Filter.Enabled = %v, want true", result.Filter.Enabled)
	}
	if result.Filter.Events == nil {
		t.Fatal("Filter.Events is nil")
	}
	if len(result.Filter.Events.Include) != 1 || result.Filter.Events.Include[0] != "agent.tool.call" {
		t.Errorf("Filter.Events.Include = %v", result.Filter.Events.Include)
	}
	if result.Filter.Attributes == nil {
		t.Fatal("Filter.Attributes is nil")
	}
	if len(result.Filter.Attributes.Redact) != 1 || result.Filter.Attributes.Redact[0] != "prompt" {
		t.Errorf("Filter.Attributes.Redact = %v", result.Filter.Attributes.Redact)
	}

	// Resource
	if result.Resource["service.name"] != "scion-agent" {
		t.Errorf("Resource = %v", result.Resource)
	}
}

func TestConvertV1TelemetryToAPI_Partial(t *testing.T) {
	enabled := false
	v1 := &V1TelemetryConfig{
		Enabled: &enabled,
		// Cloud, Hub, Local, Filter all nil
	}

	result := ConvertV1TelemetryToAPI(v1)
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.Enabled == nil || *result.Enabled != false {
		t.Errorf("Enabled = %v, want false", result.Enabled)
	}
	if result.Cloud != nil {
		t.Errorf("Cloud should be nil, got %+v", result.Cloud)
	}
	if result.Hub != nil {
		t.Errorf("Hub should be nil, got %+v", result.Hub)
	}
	if result.Local != nil {
		t.Errorf("Local should be nil, got %+v", result.Local)
	}
	if result.Filter != nil {
		t.Errorf("Filter should be nil, got %+v", result.Filter)
	}
}

func TestTelemetryConfigToEnv_Nil(t *testing.T) {
	result := TelemetryConfigToEnv(nil)
	if result != nil {
		t.Errorf("expected nil, got %v", result)
	}
}

func TestTelemetryConfigToEnv_Full(t *testing.T) {
	enabled := true
	cloudEnabled := true
	insecure := false
	caFile := "/etc/ssl/certs/custom-root.pem"
	hubEnabled := true
	localEnabled := true
	console := true
	filterEnabled := true
	respectDebug := false

	cfg := &api.TelemetryConfig{
		Enabled: &enabled,
		Cloud: &api.TelemetryCloudConfig{
			Enabled:  &cloudEnabled,
			Endpoint: "otel.example.com:4317",
			Protocol: "grpc",
			Headers:  map[string]string{"X-Key": "val"},
			TLS: &api.TelemetryTLS{
				InsecureSkipVerify: &insecure,
				CAFile:             caFile,
			},
			Batch: &api.TelemetryBatch{
				MaxSize: 256,
				Timeout: "10s",
			},
			Provider: "gcp",
		},
		Hub: &api.TelemetryHubConfig{
			Enabled:        &hubEnabled,
			ReportInterval: "60s",
		},
		Local: &api.TelemetryLocalConfig{
			Enabled: &localEnabled,
			File:    "/var/log/telemetry.jsonl",
			Console: &console,
		},
		Filter: &api.TelemetryFilterConfig{
			Enabled:          &filterEnabled,
			RespectDebugMode: &respectDebug,
			Events: &api.TelemetryEventsConfig{
				Include: []string{"agent.tool.call", "agent.turn"},
				Exclude: []string{"agent.user.prompt"},
			},
			Attributes: &api.TelemetryAttributesConfig{
				Redact: []string{"prompt", "user.email"},
				Hash:   []string{"session_id"},
			},
		},
	}

	env := TelemetryConfigToEnv(cfg)
	if env == nil {
		t.Fatal("expected non-nil env map")
	}

	expected := map[string]string{
		"SCION_TELEMETRY_ENABLED":                   "true",
		"SCION_TELEMETRY_CLOUD_ENABLED":             "true",
		"SCION_OTEL_ENDPOINT":                       "otel.example.com:4317",
		"SCION_OTEL_PROTOCOL":                       "grpc",
		"SCION_OTEL_INSECURE":                       "false",
		"SCION_OTEL_CA_FILE":                        "/etc/ssl/certs/custom-root.pem",
		"SCION_TELEMETRY_CLOUD_BATCH_MAX_SIZE":      "256",
		"SCION_TELEMETRY_CLOUD_BATCH_TIMEOUT":       "10s",
		"SCION_TELEMETRY_CLOUD_PROVIDER":            "gcp",
		"SCION_TELEMETRY_HUB_ENABLED":               "true",
		"SCION_TELEMETRY_HUB_REPORT_INTERVAL":       "60s",
		"SCION_TELEMETRY_LOCAL_ENABLED":             "true",
		"SCION_TELEMETRY_DEBUG":                     "true",
		"SCION_TELEMETRY_LOCAL_FILE":                "/var/log/telemetry.jsonl",
		"SCION_TELEMETRY_LOCAL_CONSOLE":             "true",
		"SCION_TELEMETRY_FILTER_ENABLED":            "true",
		"SCION_TELEMETRY_FILTER_RESPECT_DEBUG_MODE": "false",
		"SCION_TELEMETRY_FILTER_INCLUDE":            "agent.tool.call,agent.turn",
		"SCION_TELEMETRY_FILTER_EXCLUDE":            "agent.user.prompt",
		"SCION_TELEMETRY_REDACT":                    "prompt,user.email",
		"SCION_TELEMETRY_HASH":                      "session_id",
	}

	// Check headers JSON separately since map ordering is non-deterministic
	headersJSON := env["SCION_OTEL_HEADERS"]
	if headersJSON == "" {
		t.Error("SCION_OTEL_HEADERS not set")
	} else {
		var headers map[string]string
		if err := json.Unmarshal([]byte(headersJSON), &headers); err != nil {
			t.Errorf("SCION_OTEL_HEADERS is not valid JSON: %v", err)
		} else if headers["X-Key"] != "val" {
			t.Errorf("SCION_OTEL_HEADERS[X-Key] = %q, want %q", headers["X-Key"], "val")
		}
	}
	delete(env, "SCION_OTEL_HEADERS")

	for k, want := range expected {
		got, ok := env[k]
		if !ok {
			t.Errorf("missing env var %s", k)
			continue
		}
		if got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
		delete(env, k)
	}

	// Ensure no unexpected env vars
	if len(env) > 0 {
		keys := make([]string, 0, len(env))
		for k := range env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		t.Errorf("unexpected env vars: %s", strings.Join(keys, ", "))
	}
}

func TestTelemetryConfigToEnv_OnlyNonZero(t *testing.T) {
	// An empty config with no fields set should return nil
	cfg := &api.TelemetryConfig{}
	env := TelemetryConfigToEnv(cfg)
	if env != nil {
		t.Errorf("expected nil for empty config, got %v", env)
	}

	// A config with only Enabled set should return only that var
	enabled := true
	cfg = &api.TelemetryConfig{Enabled: &enabled}
	env = TelemetryConfigToEnv(cfg)
	if len(env) != 1 {
		t.Errorf("expected 1 env var, got %d: %v", len(env), env)
	}
	if env["SCION_TELEMETRY_ENABLED"] != "true" {
		t.Errorf("SCION_TELEMETRY_ENABLED = %q, want %q", env["SCION_TELEMETRY_ENABLED"], "true")
	}

	// Cloud with empty endpoint/protocol should not emit those vars
	cfg = &api.TelemetryConfig{
		Cloud: &api.TelemetryCloudConfig{
			// All zero values
		},
	}
	env = TelemetryConfigToEnv(cfg)
	if env != nil {
		t.Errorf("expected nil for cloud with all zero values, got %v", env)
	}
}

func TestTelemetryConfigToEnv_ProviderNotEmittedWhenEmpty(t *testing.T) {
	cloudEnabled := true
	cfg := &api.TelemetryConfig{
		Cloud: &api.TelemetryCloudConfig{
			Enabled:  &cloudEnabled,
			Endpoint: "otel.example.com:4317",
			Provider: "", // empty provider
		},
	}

	env := TelemetryConfigToEnv(cfg)
	if _, ok := env["SCION_TELEMETRY_CLOUD_PROVIDER"]; ok {
		t.Error("SCION_TELEMETRY_CLOUD_PROVIDER should not be set when Provider is empty")
	}
}

func TestTelemetryConfigToEnv_BoolFormat(t *testing.T) {
	trueVal := true
	falseVal := false

	cfg := &api.TelemetryConfig{
		Enabled: &trueVal,
		Cloud: &api.TelemetryCloudConfig{
			Enabled: &falseVal,
		},
	}

	env := TelemetryConfigToEnv(cfg)
	if env["SCION_TELEMETRY_ENABLED"] != "true" {
		t.Errorf("true bool should emit %q, got %q", "true", env["SCION_TELEMETRY_ENABLED"])
	}
	if env["SCION_TELEMETRY_CLOUD_ENABLED"] != "false" {
		t.Errorf("false bool should emit %q, got %q", "false", env["SCION_TELEMETRY_CLOUD_ENABLED"])
	}
}

func TestTelemetryConfigToEnv_CSVFormat(t *testing.T) {
	cfg := &api.TelemetryConfig{
		Filter: &api.TelemetryFilterConfig{
			Events: &api.TelemetryEventsConfig{
				Include: []string{"a", "b", "c"},
			},
			Attributes: &api.TelemetryAttributesConfig{
				Redact: []string{"x"},
				Hash:   []string{"y", "z"},
			},
		},
	}

	env := TelemetryConfigToEnv(cfg)
	if env["SCION_TELEMETRY_FILTER_INCLUDE"] != "a,b,c" {
		t.Errorf("CSV include = %q, want %q", env["SCION_TELEMETRY_FILTER_INCLUDE"], "a,b,c")
	}
	if env["SCION_TELEMETRY_REDACT"] != "x" {
		t.Errorf("CSV redact = %q, want %q", env["SCION_TELEMETRY_REDACT"], "x")
	}
	if env["SCION_TELEMETRY_HASH"] != "y,z" {
		t.Errorf("CSV hash = %q, want %q", env["SCION_TELEMETRY_HASH"], "y,z")
	}
}

func TestTelemetryConfigToEnv_HeadersJSON(t *testing.T) {
	cfg := &api.TelemetryConfig{
		Cloud: &api.TelemetryCloudConfig{
			Headers: map[string]string{
				"Authorization": "Bearer secret",
				"X-Custom":      "value",
			},
		},
	}

	env := TelemetryConfigToEnv(cfg)
	raw := env["SCION_OTEL_HEADERS"]
	if raw == "" {
		t.Fatal("SCION_OTEL_HEADERS not set")
	}

	var parsed map[string]string
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("failed to parse headers JSON: %v", err)
	}

	if parsed["Authorization"] != "Bearer secret" {
		t.Errorf("Authorization = %q", parsed["Authorization"])
	}
	if parsed["X-Custom"] != "value" {
		t.Errorf("X-Custom = %q", parsed["X-Custom"])
	}
}
