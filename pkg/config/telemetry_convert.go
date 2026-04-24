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
	"strconv"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// ConvertV1TelemetryToAPI converts settings-level telemetry config (V1TelemetryConfig)
// to the agent-level telemetry config (api.TelemetryConfig). The two types are
// structurally identical but live in different packages.
func ConvertV1TelemetryToAPI(v1 *V1TelemetryConfig) *api.TelemetryConfig {
	if v1 == nil {
		return nil
	}

	result := &api.TelemetryConfig{
		Enabled:  v1.Enabled,
		Resource: v1.Resource,
	}

	if v1.Cloud != nil {
		result.Cloud = &api.TelemetryCloudConfig{
			Enabled:  v1.Cloud.Enabled,
			Endpoint: v1.Cloud.Endpoint,
			Protocol: v1.Cloud.Protocol,
			Headers:  v1.Cloud.Headers,
			Provider: v1.Cloud.Provider,
		}
		if v1.Cloud.TLS != nil {
			result.Cloud.TLS = &api.TelemetryTLS{
				Enabled:            v1.Cloud.TLS.Enabled,
				InsecureSkipVerify: v1.Cloud.TLS.InsecureSkipVerify,
				CAFile:             v1.Cloud.TLS.CAFile,
				CertFile:           v1.Cloud.TLS.CertFile,
				KeyFile:            v1.Cloud.TLS.KeyFile,
			}
		}
		if v1.Cloud.Batch != nil {
			result.Cloud.Batch = &api.TelemetryBatch{
				MaxSize: v1.Cloud.Batch.MaxSize,
				Timeout: v1.Cloud.Batch.Timeout,
			}
		}
	}

	if v1.Hub != nil {
		result.Hub = &api.TelemetryHubConfig{
			Enabled:        v1.Hub.Enabled,
			ReportInterval: v1.Hub.ReportInterval,
		}
	}

	if v1.Local != nil {
		result.Local = &api.TelemetryLocalConfig{
			Enabled: v1.Local.Enabled,
			File:    v1.Local.File,
			Console: v1.Local.Console,
		}
	}

	if v1.Filter != nil {
		result.Filter = &api.TelemetryFilterConfig{
			Enabled:          v1.Filter.Enabled,
			RespectDebugMode: v1.Filter.RespectDebugMode,
		}
		if v1.Filter.Events != nil {
			result.Filter.Events = &api.TelemetryEventsConfig{
				Include: v1.Filter.Events.Include,
				Exclude: v1.Filter.Events.Exclude,
			}
		}
		if v1.Filter.Attributes != nil {
			result.Filter.Attributes = &api.TelemetryAttributesConfig{
				Redact: v1.Filter.Attributes.Redact,
				Hash:   v1.Filter.Attributes.Hash,
			}
		}
		if v1.Filter.Sampling != nil {
			result.Filter.Sampling = &api.TelemetrySamplingConfig{
				Default: v1.Filter.Sampling.Default,
				Rates:   v1.Filter.Sampling.Rates,
			}
		}
	}

	return result
}

// TelemetryConfigToEnv converts a resolved api.TelemetryConfig into a map of
// environment variables suitable for injection into an agent container. Only
// non-nil/non-zero fields are emitted. The env var names match the constants
// in pkg/sciontool/telemetry/config.go and the design doc §10.3.
func TelemetryConfigToEnv(cfg *api.TelemetryConfig) map[string]string {
	if cfg == nil {
		return nil
	}

	env := make(map[string]string)

	if cfg.Enabled != nil {
		env["SCION_TELEMETRY_ENABLED"] = strconv.FormatBool(*cfg.Enabled)
	}

	// Cloud config
	if cfg.Cloud != nil {
		if cfg.Cloud.Enabled != nil {
			env["SCION_TELEMETRY_CLOUD_ENABLED"] = strconv.FormatBool(*cfg.Cloud.Enabled)
		}
		if cfg.Cloud.Endpoint != "" {
			env["SCION_OTEL_ENDPOINT"] = cfg.Cloud.Endpoint
		}
		if cfg.Cloud.Protocol != "" {
			env["SCION_OTEL_PROTOCOL"] = cfg.Cloud.Protocol
		}
		if cfg.Cloud.Headers != nil {
			if data, err := json.Marshal(cfg.Cloud.Headers); err == nil {
				env["SCION_OTEL_HEADERS"] = string(data)
			}
		}
		if cfg.Cloud.TLS != nil && cfg.Cloud.TLS.InsecureSkipVerify != nil {
			env["SCION_OTEL_INSECURE"] = strconv.FormatBool(*cfg.Cloud.TLS.InsecureSkipVerify)
		}
		if cfg.Cloud.TLS != nil && cfg.Cloud.TLS.CAFile != "" {
			env["SCION_OTEL_CA_FILE"] = cfg.Cloud.TLS.CAFile
		}
		if cfg.Cloud.Batch != nil {
			if cfg.Cloud.Batch.MaxSize > 0 {
				env["SCION_TELEMETRY_CLOUD_BATCH_MAX_SIZE"] = strconv.Itoa(cfg.Cloud.Batch.MaxSize)
			}
			if cfg.Cloud.Batch.Timeout != "" {
				env["SCION_TELEMETRY_CLOUD_BATCH_TIMEOUT"] = cfg.Cloud.Batch.Timeout
			}
		}
		if cfg.Cloud.Provider != "" {
			env["SCION_TELEMETRY_CLOUD_PROVIDER"] = cfg.Cloud.Provider
		}
	}

	// Hub config
	if cfg.Hub != nil {
		if cfg.Hub.Enabled != nil {
			env["SCION_TELEMETRY_HUB_ENABLED"] = strconv.FormatBool(*cfg.Hub.Enabled)
		}
		if cfg.Hub.ReportInterval != "" {
			env["SCION_TELEMETRY_HUB_REPORT_INTERVAL"] = cfg.Hub.ReportInterval
		}
	}

	// Local config
	if cfg.Local != nil {
		if cfg.Local.Enabled != nil {
			env["SCION_TELEMETRY_LOCAL_ENABLED"] = strconv.FormatBool(*cfg.Local.Enabled)
			env["SCION_TELEMETRY_DEBUG"] = strconv.FormatBool(*cfg.Local.Enabled)
		}
		if cfg.Local.File != "" {
			env["SCION_TELEMETRY_LOCAL_FILE"] = cfg.Local.File
		}
		if cfg.Local.Console != nil {
			env["SCION_TELEMETRY_LOCAL_CONSOLE"] = strconv.FormatBool(*cfg.Local.Console)
		}
	}

	// Filter config
	if cfg.Filter != nil {
		if cfg.Filter.Enabled != nil {
			env["SCION_TELEMETRY_FILTER_ENABLED"] = strconv.FormatBool(*cfg.Filter.Enabled)
		}
		if cfg.Filter.RespectDebugMode != nil {
			env["SCION_TELEMETRY_FILTER_RESPECT_DEBUG_MODE"] = strconv.FormatBool(*cfg.Filter.RespectDebugMode)
		}
		if cfg.Filter.Events != nil {
			if len(cfg.Filter.Events.Include) > 0 {
				env["SCION_TELEMETRY_FILTER_INCLUDE"] = strings.Join(cfg.Filter.Events.Include, ",")
			}
			if len(cfg.Filter.Events.Exclude) > 0 {
				env["SCION_TELEMETRY_FILTER_EXCLUDE"] = strings.Join(cfg.Filter.Events.Exclude, ",")
			}
		}
		if cfg.Filter.Attributes != nil {
			if len(cfg.Filter.Attributes.Redact) > 0 {
				env["SCION_TELEMETRY_REDACT"] = strings.Join(cfg.Filter.Attributes.Redact, ",")
			}
			if len(cfg.Filter.Attributes.Hash) > 0 {
				env["SCION_TELEMETRY_HASH"] = strings.Join(cfg.Filter.Attributes.Hash, ",")
			}
		}
	}

	if len(env) == 0 {
		return nil
	}

	// Validate no empty values slipped through.
	for k, v := range env {
		if v == "" {
			delete(env, k)
		}
	}

	if len(env) == 0 {
		return nil
	}

	return env
}
