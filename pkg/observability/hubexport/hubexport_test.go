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

package hubexport

import (
	"errors"
	"strings"
	"testing"
)

func boolp(v bool) *bool { return &v }

func TestResolveProject(t *testing.T) {
	metaOK := func() (string, error) { return "meta-project", nil }
	metaErr := func() (string, error) { return "", errors.New("not running on GCP") }
	metaCalled := false
	metaSpy := func() (string, error) { metaCalled = true; return "meta-project", nil }

	tests := []struct {
		name       string
		in         ProjectInputs
		wantID     string
		wantSource ProjectSource
		wantDetail string
	}{
		{"env", ProjectInputs{Configured: "env-project", EnvValue: "env-project", Metadata: metaOK}, "env-project", SourceEnv, ""},
		{"file", ProjectInputs{Configured: "file-project", Metadata: metaOK}, "file-project", SourceFile, ""},
		{"metadata fallback", ProjectInputs{Metadata: metaOK}, "meta-project", SourceMetadata, ""},
		{"metadata error", ProjectInputs{Metadata: metaErr}, "", SourceNone, "not running on GCP"},
		{"metadata empty", ProjectInputs{Metadata: func() (string, error) { return " ", nil }}, "", SourceNone, "gave no project"},
		{"no lookup", ProjectInputs{}, "", SourceNone, "server.hub.gcp_project_id is not set"},
		{"blank configured", ProjectInputs{Configured: "  ", Metadata: metaOK}, "meta-project", SourceMetadata, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveProject(tt.in)
			if got.ID != tt.wantID || got.Source != tt.wantSource {
				t.Errorf("ResolveProject = %+v, want id %q source %q", got, tt.wantID, tt.wantSource)
			}
			if (tt.wantDetail == "" && got.Detail != "") || !strings.Contains(got.Detail, tt.wantDetail) {
				t.Errorf("Detail = %q, want it to contain %q", got.Detail, tt.wantDetail)
			}
		})
	}

	ResolveProject(ProjectInputs{Configured: "file-project", Metadata: metaSpy})
	if metaCalled {
		t.Error("the metadata server must not be asked when the operator set the project")
	}
}

var (
	projEnv  = Project{ID: "env-project", Source: SourceEnv}
	projFile = Project{ID: "file-project", Source: SourceFile}
	projMeta = Project{ID: "meta-project", Source: SourceMetadata}
	projNone = Project{Detail: "server.hub.gcp_project_id is not set and the metadata server gave no project: off GCP"}
)

// Each switch true, false and unset, crossed with the project set by the
// operator, resolved from the metadata server only, or absent.
func TestDecideMetrics(t *testing.T) {
	tests := []struct {
		name        string
		setting     *bool
		project     Project
		wantEnabled bool
		wantBasis   Basis
		wantProject string
		wantReason  string
	}{
		{"true, env project", boolp(true), projEnv, true, BasisSetting, "env-project", "metrics_enabled=true"},
		{"true, file project", boolp(true), projFile, true, BasisSetting, "file-project", "metrics_enabled=true"},
		{"true, metadata project", boolp(true), projMeta, true, BasisSetting, "meta-project", "metrics_enabled=true"},
		{"true, no project", boolp(true), projNone, false, BasisNoProject, "", "no export project resolved"},
		{"false, env project", boolp(false), projEnv, false, BasisSetting, "", "metrics_enabled=false"},
		{"false, file project", boolp(false), projFile, false, BasisSetting, "", "metrics_enabled=false"},
		{"false, metadata project", boolp(false), projMeta, false, BasisSetting, "", "metrics_enabled=false"},
		{"false, no project", boolp(false), projNone, false, BasisSetting, "", "metrics_enabled=false"},
		{"unset, env project", nil, projEnv, true, BasisLegacy, "env-project", "on because server.hub.gcp_project_id is set"},
		{"unset, file project", nil, projFile, true, BasisLegacy, "file-project", "on because server.hub.gcp_project_id is set"},
		{"unset, metadata project only", nil, projMeta, false, BasisLegacy, "", "off because server.hub.gcp_project_id is not set"},
		{"unset, no project", nil, projNone, false, BasisLegacy, "", "off because server.hub.gcp_project_id is not set"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := DecideMetrics(tt.setting, tt.project)
			checkDecision(t, d, Metrics, tt.wantEnabled, tt.wantBasis, tt.wantProject, tt.wantReason)
		})
	}
}

func TestDecideTracing(t *testing.T) {
	tests := []struct {
		name        string
		setting     *bool
		env         bool
		project     Project
		wantEnabled bool
		wantBasis   Basis
		wantProject string
		wantReason  string
	}{
		// A set switch decides on its own; the env variable is ignored.
		{"true, env project", boolp(true), false, projEnv, true, BasisSetting, "env-project", "tracing_enabled=true"},
		{"true, file project", boolp(true), false, projFile, true, BasisSetting, "file-project", "tracing_enabled=true"},
		{"true, metadata project", boolp(true), false, projMeta, true, BasisSetting, "meta-project", "tracing_enabled=true"},
		{"true, no project", boolp(true), true, projNone, false, BasisNoProject, "", "no export project resolved"},
		{"false, env project, env on", boolp(false), true, projEnv, false, BasisSetting, "", "tracing_enabled=false"},
		{"false, file project", boolp(false), false, projFile, false, BasisSetting, "", "tracing_enabled=false"},
		{"false, metadata project", boolp(false), true, projMeta, false, BasisSetting, "", "tracing_enabled=false"},
		{"false, no project", boolp(false), true, projNone, false, BasisSetting, "", "tracing_enabled=false"},
		// Unset: the earlier rule (env variable and operator project).
		{"unset, env on, env project", nil, true, projEnv, true, BasisLegacy, "env-project", "SCION_TRACING_ENABLED and server.hub.gcp_project_id are set"},
		{"unset, env on, file project", nil, true, projFile, true, BasisLegacy, "file-project", "SCION_TRACING_ENABLED and server.hub.gcp_project_id are set"},
		{"unset, env on, metadata project only", nil, true, projMeta, false, BasisLegacy, "", "SCION_TRACING_ENABLED is set but server.hub.gcp_project_id is not"},
		{"unset, env on, no project", nil, true, projNone, false, BasisLegacy, "", "SCION_TRACING_ENABLED is set but server.hub.gcp_project_id is not"},
		{"unset, env off, env project", nil, false, projEnv, false, BasisLegacy, "", "off because SCION_TRACING_ENABLED is not set"},
		{"unset, env off, file project", nil, false, projFile, false, BasisLegacy, "", "off because SCION_TRACING_ENABLED is not set"},
		{"unset, env off, metadata project", nil, false, projMeta, false, BasisLegacy, "", "off because SCION_TRACING_ENABLED is not set"},
		{"unset, env off, no project", nil, false, projNone, false, BasisLegacy, "", "off because SCION_TRACING_ENABLED is not set"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := DecideTracing(tt.setting, tt.env, tt.project)
			checkDecision(t, d, Tracing, tt.wantEnabled, tt.wantBasis, tt.wantProject, tt.wantReason)
		})
	}
}

func checkDecision(t *testing.T, d Decision, exp Exporter, enabled bool, basis Basis, project, reason string) {
	t.Helper()
	if d.Exporter != exp {
		t.Errorf("Exporter = %q, want %q", d.Exporter, exp)
	}
	if d.Enabled != enabled || d.Basis != basis {
		t.Errorf("Enabled=%v Basis=%q, want Enabled=%v Basis=%q (reason %q)", d.Enabled, d.Basis, enabled, basis, d.Reason)
	}
	if d.Project.ID != project {
		t.Errorf("Project = %+v, want id %q", d.Project, project)
	}
	if !strings.Contains(d.Reason, reason) {
		t.Errorf("Reason = %q, want it to contain %q", d.Reason, reason)
	}
	if got, want := d.Warn(), basis == BasisNoProject; got != want {
		t.Errorf("Warn() = %v, want %v", got, want)
	}
	line := d.LogLine()
	if enabled && !strings.Contains(line, ": on (project: "+project) {
		t.Errorf("LogLine = %q, want an on line naming project %q", line, project)
	}
	if !enabled && !strings.Contains(line, ": off (") {
		t.Errorf("LogLine = %q, want an off line", line)
	}
	if strings.HasPrefix(line, "WARNING: ") != d.Warn() {
		t.Errorf("LogLine = %q, WARNING prefix must match Warn() = %v", line, d.Warn())
	}
}

func TestDecision_SettingIsCopied(t *testing.T) {
	v := true
	d := DecideMetrics(&v, projFile)
	v = false
	if d.Setting == nil || !*d.Setting {
		t.Errorf("Setting = %v, want a copy holding true", d.Setting)
	}
	if DecideTracing(nil, true, projFile).Setting != nil {
		t.Error("an unset switch must be reported as nil")
	}
}

// TestDecision_BlankProjectIDUnsetSwitch records the intended trim: a
// whitespace-only project ID (for example SCION_SERVER_HUB_GCPPROJECTID=" ")
// counts as not set, so with the switches unset neither exporter runs, even
// though a metadata-server project resolves.
func TestDecision_BlankProjectIDUnsetSwitch(t *testing.T) {
	project := ResolveProject(ProjectInputs{
		Configured: " ",
		EnvValue:   " ",
		Metadata:   func() (string, error) { return "meta-project", nil },
	})
	if project.Source != SourceMetadata || project.operatorSet() {
		t.Fatalf("ResolveProject = %+v, want the metadata project, not operator-set", project)
	}
	checkDecision(t, DecideMetrics(nil, project), Metrics, false, BasisLegacy, "",
		"off because server.hub.gcp_project_id is not set")
	checkDecision(t, DecideTracing(nil, true, project), Tracing, false, BasisLegacy, "",
		"SCION_TRACING_ENABLED is set but server.hub.gcp_project_id is not")
}

func TestDecision_LogLines(t *testing.T) {
	cases := map[string]Decision{
		"Hub metrics export: on (project: file-project (source: file); server.hub.observability.metrics_enabled=true)":               DecideMetrics(boolp(true), projFile),
		"Hub trace export: off (server.hub.observability.tracing_enabled=false)":                                                     DecideTracing(boolp(false), true, projEnv),
		"Hub metrics export: off (server.hub.observability.metrics_enabled unset; off because server.hub.gcp_project_id is not set)": DecideMetrics(nil, projMeta),
	}
	for want, d := range cases {
		if got := d.LogLine(); got != want {
			t.Errorf("LogLine =\n  %q\nwant\n  %q", got, want)
		}
	}
	warn := DecideTracing(boolp(true), false, projNone).LogLine()
	for _, sub := range []string{"WARNING: Hub trace export: off (", "server.hub.observability.tracing_enabled=true", "no export project resolved", "starting without the exporter", "Set SCION_SERVER_HUB_GCPPROJECTID or run on Google Cloud"} {
		if !strings.Contains(warn, sub) {
			t.Errorf("warning line %q does not contain %q", warn, sub)
		}
	}
}

func TestDecision_Failed(t *testing.T) {
	d := DecideMetrics(boolp(true), projMeta).Failed(errors.New("permission denied"))
	if d.Enabled || d.Basis != BasisExporterError || !d.Warn() || d.Project.ID != "" {
		t.Errorf("Failed decision = %+v, want off, exporter_error, warning", d)
	}
	if !strings.Contains(d.LogLine(), "WARNING: Hub metrics export: off (exporter could not be created: permission denied)") {
		t.Errorf("LogLine = %q", d.LogLine())
	}
}
