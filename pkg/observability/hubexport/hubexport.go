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

// Package hubexport decides, once at hub startup, whether the hub's own
// metrics and trace exporters run and which GCP project they export to.
//
// Each exporter has a switch (server.hub.observability.metrics_enabled and
// server.hub.observability.tracing_enabled):
//
//   - true: the exporter runs, exporting to the export project. When no
//     export project resolves, the hub logs a warning and starts without
//     the exporter; startup never fails for this.
//   - false: the exporter does not run.
//   - unset: the rule that applied before the switches existed. Metrics
//     export runs when server.hub.gcp_project_id is set; trace export runs
//     when SCION_TRACING_ENABLED is set as well.
//
// The export project is server.hub.gcp_project_id when the operator set it
// (env or settings file), else the project of the GCP metadata server.
//
// The package has no dependency on the hub server, so the rules are tested
// on their own.
package hubexport

import (
	"fmt"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// ProjectIDEnv is the env variable that sets server.hub.gcp_project_id.
const ProjectIDEnv = "SCION_SERVER_HUB_GCPPROJECTID"

// TracingEnv is the env variable that turns trace export on when the
// tracing switch is unset.
const TracingEnv = "SCION_TRACING_ENABLED"

// projectIDKey is the settings key of the operator-set project ID.
const projectIDKey = "server.hub.gcp_project_id"

// ProjectSource says where the export project came from.
type ProjectSource string

const (
	// SourceNone means no export project resolved.
	SourceNone ProjectSource = ""
	// SourceEnv is server.hub.gcp_project_id set by its env variable.
	SourceEnv ProjectSource = "env"
	// SourceFile is server.hub.gcp_project_id set in the settings file.
	SourceFile ProjectSource = "file"
	// SourceMetadata is the project of the GCP metadata server.
	SourceMetadata ProjectSource = "metadata"
)

// Project is the resolved export project.
type Project struct {
	ID     string        `json:"id,omitempty"`
	Source ProjectSource `json:"source,omitempty"`
	// Detail explains why no project resolved; empty when one did.
	Detail string `json:"detail,omitempty"`
}

// operatorSet reports whether the operator set the project (env or file),
// which is what the rule for an unset switch looks at.
func (p Project) operatorSet() bool {
	return p.ID != "" && (p.Source == SourceEnv || p.Source == SourceFile)
}

func (p Project) String() string {
	if p.ID == "" {
		return "none"
	}
	return fmt.Sprintf("%s (source: %s)", p.ID, p.Source)
}

// ProjectInputs are the inputs of ResolveProject.
type ProjectInputs struct {
	// Configured is server.hub.gcp_project_id as loaded (env over file).
	Configured string
	// EnvValue is the value of ProjectIDEnv; non-empty means the
	// configured value came from the environment.
	EnvValue string
	// Metadata returns the project of the GCP metadata server, or an
	// error when there is none (for example off GCP). Nil means no lookup.
	Metadata func() (string, error)
}

// ResolveProject returns the export project: the operator-set project ID
// when present, else the metadata-server project.
func ResolveProject(in ProjectInputs) Project {
	if id := strings.TrimSpace(in.Configured); id != "" {
		if strings.TrimSpace(in.EnvValue) != "" {
			return Project{ID: id, Source: SourceEnv}
		}
		return Project{ID: id, Source: SourceFile}
	}
	if in.Metadata == nil {
		return Project{Detail: projectIDKey + " is not set"}
	}
	id, err := in.Metadata()
	if err != nil {
		return Project{Detail: fmt.Sprintf("%s is not set and the metadata server gave no project: %v", projectIDKey, err)}
	}
	if id = strings.TrimSpace(id); id == "" {
		return Project{Detail: projectIDKey + " is not set and the metadata server gave no project"}
	}
	return Project{ID: id, Source: SourceMetadata}
}

// Exporter names an exporter.
type Exporter string

const (
	Metrics Exporter = "metrics"
	Tracing Exporter = "tracing"
)

// settingKey returns the settings key of the exporter's switch.
func (e Exporter) settingKey() string {
	if e == Tracing {
		return config.HubTracingEnabledKey
	}
	return config.HubMetricsEnabledKey
}

// Basis says which rule decided an exporter's state.
type Basis string

const (
	// BasisSetting: the switch is set.
	BasisSetting Basis = "setting"
	// BasisLegacy: the switch is unset, and the earlier rule applies.
	BasisLegacy Basis = "legacy"
	// BasisNoProject: the switch is true but no export project resolved.
	BasisNoProject Basis = "no_project"
	// BasisExporterError: the exporter was to run but could not be created.
	BasisExporterError Basis = "exporter_error"
)

// Decision is the startup state of one exporter.
type Decision struct {
	Exporter Exporter `json:"exporter"`
	// Setting is the switch value; nil when unset.
	Setting *bool `json:"setting"`
	Enabled bool  `json:"enabled"`
	Basis   Basis `json:"basis"`
	// Reason is a one-line, human-readable explanation of the state.
	Reason string `json:"reason"`
	// Project is the export project; set only when Enabled.
	Project Project `json:"project"`
}

// Warn reports whether the decision is a warning: the switch asks for the
// exporter, but it does not run.
func (d Decision) Warn() bool {
	return d.Basis == BasisNoProject || d.Basis == BasisExporterError
}

// LogLine is the startup log line for the decision.
func (d Decision) LogLine() string {
	name := "Hub metrics export"
	if d.Exporter == Tracing {
		name = "Hub trace export"
	}
	prefix := ""
	if d.Warn() {
		prefix = "WARNING: "
	}
	if d.Enabled {
		return fmt.Sprintf("%s%s: on (project: %s; %s)", prefix, name, d.Project, d.Reason)
	}
	return fmt.Sprintf("%s%s: off (%s)", prefix, name, d.Reason)
}

// Failed returns the decision for an exporter that was to run but could not
// be created.
func (d Decision) Failed(err error) Decision {
	d.Enabled = false
	d.Project = Project{}
	d.Basis = BasisExporterError
	d.Reason = fmt.Sprintf("exporter could not be created: %v", err)
	return d
}

// DecideMetrics decides the metrics exporter's state from its switch and
// the export project.
func DecideMetrics(setting *bool, project Project) Decision {
	d := Decision{Exporter: Metrics, Setting: copyBool(setting)}
	if setting != nil {
		return decideFromSetting(d, *setting, project)
	}
	d.Basis = BasisLegacy
	key := Metrics.settingKey()
	if project.operatorSet() {
		d.Enabled = true
		d.Project = project
		d.Reason = fmt.Sprintf("%s unset; on because %s is set", key, projectIDKey)
		return d
	}
	d.Reason = fmt.Sprintf("%s unset; off because %s is not set", key, projectIDKey)
	return d
}

// DecideTracing decides the trace exporter's state from its switch, the
// value of TracingEnv (used only when the switch is unset) and the export
// project.
func DecideTracing(setting *bool, tracingEnv bool, project Project) Decision {
	d := Decision{Exporter: Tracing, Setting: copyBool(setting)}
	if setting != nil {
		return decideFromSetting(d, *setting, project)
	}
	d.Basis = BasisLegacy
	key := Tracing.settingKey()
	switch {
	case !tracingEnv:
		d.Reason = fmt.Sprintf("%s unset; off because %s is not set", key, TracingEnv)
	case !project.operatorSet():
		d.Reason = fmt.Sprintf("%s unset; off because %s is set but %s is not", key, TracingEnv, projectIDKey)
	default:
		d.Enabled = true
		d.Project = project
		d.Reason = fmt.Sprintf("%s unset; on because %s and %s are set", key, TracingEnv, projectIDKey)
	}
	return d
}

func decideFromSetting(d Decision, on bool, project Project) Decision {
	key := d.Exporter.settingKey()
	if !on {
		d.Basis = BasisSetting
		d.Reason = key + "=false"
		return d
	}
	if project.ID == "" {
		d.Basis = BasisNoProject
		detail := project.Detail
		if detail == "" {
			detail = "no export project"
		}
		d.Reason = fmt.Sprintf("%s=true but no export project resolved (%s); starting without the exporter. "+
			"Set %s or run on GCP", key, detail, projectIDKey)
		return d
	}
	d.Basis = BasisSetting
	d.Enabled = true
	d.Project = project
	d.Reason = key + "=true"
	return d
}

// Status is the startup state of both exporters and the export project.
// The hub reports it read-only to admins.
type Status struct {
	// Project is the export project resolved at startup, whether or not an
	// exporter uses it.
	Project Project  `json:"project"`
	Metrics Decision `json:"metrics"`
	Tracing Decision `json:"tracing"`
}

func copyBool(p *bool) *bool {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
