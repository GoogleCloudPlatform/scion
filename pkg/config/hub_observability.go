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

// Settings keys of the hub observability export switches
// (server.hub.observability). Both are Layer-1 settings that apply at the
// next hub start.
const (
	HubMetricsEnabledKey = "server.hub.observability.metrics_enabled"
	HubTracingEnabledKey = "server.hub.observability.tracing_enabled"
)

// HubObservabilityConfig holds the switches for the hub's own metrics and
// trace export (server.hub.observability). A nil switch is unset, and the
// hub keeps the rule that applied before the switch existed: metrics export
// runs when server.hub.gcp_project_id is set, and trace export runs when
// SCION_TRACING_ENABLED is set as well.
type HubObservabilityConfig struct {
	// MetricsEnabled turns the hub metrics exporter on or off.
	MetricsEnabled *bool `json:"metricsEnabled,omitempty" yaml:"metricsEnabled,omitempty" koanf:"metricsEnabled"`
	// TracingEnabled turns the hub trace exporter on or off.
	TracingEnabled *bool `json:"tracingEnabled,omitempty" yaml:"tracingEnabled,omitempty" koanf:"tracingEnabled"`
}

// IsZero reports whether neither switch is set.
func (c HubObservabilityConfig) IsZero() bool {
	return c.MetricsEnabled == nil && c.TracingEnabled == nil
}

// copyBoolPtr returns a copy of p that does not share its storage, or nil.
func copyBoolPtr(p *bool) *bool {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
