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

package cmd

import (
	"errors"
	"testing"

	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/v2"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/observability/hubexport"
)

// TestDecideHubObservabilityExport_InputSelection pins the startup wiring
// of ptone/scion#4914: the switches come from the operational settings
// snapshot when the service exists and from the loaded config otherwise,
// and the project source depends on SCION_SERVER_HUB_GCPPROJECTID.
func TestDecideHubObservabilityExport_InputSelection(t *testing.T) {
	on, off := true, false

	// opsWith builds an operational settings service whose bootstrap
	// (file and env) layer holds the given Layer-1 keys and no DB rows.
	opsWith := func(t *testing.T, keys map[string]any) *hub.OperationalSettings {
		t.Helper()
		k := koanf.New(".")
		if err := k.Load(confmap.Provider(keys, "."), nil); err != nil {
			t.Fatalf("load bootstrap koanf: %v", err)
		}
		return hub.NewOperationalSettings(nil, k, koanf.New("."))
	}
	cfgWith := func(project string, metrics, tracing *bool) *config.GlobalConfig {
		cfg := &config.GlobalConfig{}
		cfg.Hub.GCPProjectID = project
		cfg.Hub.Observability.MetricsEnabled = metrics
		cfg.Hub.Observability.TracingEnabled = tracing
		return cfg
	}
	env := func(vals map[string]string) func(string) string {
		return func(k string) string { return vals[k] }
	}
	metaOK := func() (string, error) { return "meta-project", nil }
	metaErr := func() (string, error) { return "", errors.New("not on GCP") }

	tests := []struct {
		name        string
		cfg         *config.GlobalConfig
		ops         func(t *testing.T) *hub.OperationalSettings
		in          hubObservabilityInputs
		wantMetrics bool
		wantTracing bool
		wantBasis   hubexport.Basis // metrics basis
		wantProject string
		wantSource  hubexport.ProjectSource
	}{
		{
			name:        "no ops: switches from cfg",
			cfg:         cfgWith("", &on, &off),
			in:          hubObservabilityInputs{getenv: env(nil), metadata: metaOK, tracingEnv: true},
			wantMetrics: true, wantTracing: false, wantBasis: hubexport.BasisSetting,
			wantProject: "meta-project", wantSource: hubexport.SourceMetadata,
		},
		{
			name: "ops: snapshot overrides cfg",
			cfg:  cfgWith("file-project", &on, &on),
			ops: func(t *testing.T) *hub.OperationalSettings {
				return opsWith(t, map[string]any{
					config.HubMetricsEnabledKey: false,
					config.HubTracingEnabledKey: false,
				})
			},
			in:          hubObservabilityInputs{getenv: env(nil), metadata: metaOK, tracingEnv: true},
			wantMetrics: false, wantTracing: false, wantBasis: hubexport.BasisSetting,
			wantProject: "file-project", wantSource: hubexport.SourceFile,
		},
		{
			name: "ops without the keys: unset, cfg switches ignored, legacy rule with project from cfg",
			cfg:  cfgWith("file-project", &off, &off),
			ops: func(t *testing.T) *hub.OperationalSettings {
				return opsWith(t, map[string]any{})
			},
			in:          hubObservabilityInputs{getenv: env(nil), metadata: metaOK, tracingEnv: true},
			wantMetrics: true, wantTracing: true, wantBasis: hubexport.BasisLegacy,
			wantProject: "file-project", wantSource: hubexport.SourceFile,
		},
		{
			name: "env source when SCION_SERVER_HUB_GCPPROJECTID is set",
			cfg:  cfgWith("env-project", nil, nil),
			in: hubObservabilityInputs{
				getenv:   env(map[string]string{hubexport.ProjectIDEnv: "env-project"}),
				metadata: metaOK,
			},
			wantMetrics: true, wantTracing: false, wantBasis: hubexport.BasisLegacy,
			wantProject: "env-project", wantSource: hubexport.SourceEnv,
		},
		{
			name:        "no configured project: metadata lookup, unset switch stays off",
			cfg:         cfgWith("", nil, nil),
			in:          hubObservabilityInputs{getenv: env(nil), metadata: metaOK, tracingEnv: true},
			wantMetrics: false, wantTracing: false, wantBasis: hubexport.BasisLegacy,
			wantProject: "meta-project", wantSource: hubexport.SourceMetadata,
		},
		{
			name:        "no project at all: true switch warns",
			cfg:         cfgWith("", &on, nil),
			in:          hubObservabilityInputs{getenv: env(nil), metadata: metaErr},
			wantMetrics: false, wantTracing: false, wantBasis: hubexport.BasisNoProject,
			wantProject: "", wantSource: hubexport.SourceNone,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ops *hub.OperationalSettings
			if tt.ops != nil {
				ops = tt.ops(t)
			}
			st := decideHubObservabilityExport(tt.cfg, ops, tt.in)
			if st.Metrics.Enabled != tt.wantMetrics || st.Metrics.Basis != tt.wantBasis {
				t.Errorf("metrics = %+v, want enabled %v basis %q", st.Metrics, tt.wantMetrics, tt.wantBasis)
			}
			if st.Tracing.Enabled != tt.wantTracing {
				t.Errorf("tracing = %+v, want enabled %v", st.Tracing, tt.wantTracing)
			}
			if st.Project.ID != tt.wantProject || st.Project.Source != tt.wantSource {
				t.Errorf("project = %+v, want id %q source %q", st.Project, tt.wantProject, tt.wantSource)
			}
		})
	}
}
