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

package schemas

import (
	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/filterquery"
)

func graphPhaseEnum() []string {
	return []string{
		string(state.PhaseCreated),
		string(state.PhaseProvisioning),
		string(state.PhaseCloning),
		string(state.PhaseStarting),
		string(state.PhaseRunning),
		string(state.PhaseSuspended),
		string(state.PhaseStopping),
		string(state.PhaseStopped),
		string(state.PhaseError),
		string(state.PhaseHalted),
	}
}

func graphActivityEnum() []string {
	return []string{
		string(state.ActivityInitializing),
		string(state.ActivityWorking),
		string(state.ActivityThinking),
		string(state.ActivityExecuting),
		string(state.ActivityWaitingForInput),
		string(state.ActivityBlocked),
		string(state.ActivityIdle),
		string(state.ActivityCompleted),
		string(state.ActivityLimitsExceeded),
		string(state.ActivityStalled),
		string(state.ActivityOffline),
		string(state.ActivityCrashed),
		string(state.ActivityHarnessError),
	}
}

func graphModeEnum() []string {
	return []string{
		"hub",
		"project",
		"branch",
		"lineage",
		"none",
		"can_message",
		"cannot_message",
	}
}

func graphKeys(includeProject bool) []filterquery.Key {
	keys := []filterquery.Key{
		{
			Name:        "phase",
			Help:        "Agent lifecycle phase",
			Kind:        filterquery.KindEnum,
			Exec:        filterquery.ExecClient,
			Enum:        graphPhaseEnum(),
			MultiValued: false,
		},
		{
			Name:        "activity",
			Help:        "Current agent activity",
			Kind:        filterquery.KindEnum,
			Exec:        filterquery.ExecClient,
			Enum:        graphActivityEnum(),
			MultiValued: false,
		},
		{
			Name:        "label",
			Help:        "Label key or key=value pair",
			Kind:        filterquery.KindLabel,
			Exec:        filterquery.ExecClient,
			MultiValued: true,
		},
	}
	if includeProject {
		keys = append(keys, filterquery.Key{
			Name:        "project",
			Help:        "Project slug, name, or ID",
			Kind:        filterquery.KindRef,
			Exec:        filterquery.ExecClient,
			MultiValued: false,
		})
	}
	keys = append(keys,
		filterquery.Key{
			Name:        "template",
			Help:        "Agent template name",
			Kind:        filterquery.KindString,
			Exec:        filterquery.ExecClient,
			MultiValued: false,
		},
		filterquery.Key{
			Name:        "harness",
			Help:        "Harness config name",
			Kind:        filterquery.KindString,
			Exec:        filterquery.ExecClient,
			MultiValued: false,
		},
		filterquery.Key{
			Name:        "mode",
			Help:        "Messaging mode or messageability",
			Kind:        filterquery.KindEnum,
			Exec:        filterquery.ExecClient,
			Enum:        graphModeEnum(),
			MultiValued: false,
		},
	)
	return keys
}

// Graph is the filter query schema for the global agent graph view.
// All keys are evaluated on the client over AgentCompactItem fields.
var Graph = filterquery.Schema{
	View:     "graph",
	TextHelp: "Matches agent name or slug",
	Keys:     graphKeys(true),
}

// GraphProject is the filter query schema for a project-scoped agent graph view.
// It omits the project key because the view is already scoped to one project.
var GraphProject = filterquery.Schema{
	View:     "graph_project",
	TextHelp: "Matches agent name or slug",
	Keys:     graphKeys(false),
}
