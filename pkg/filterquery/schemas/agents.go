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

// Package schemas declares the per-view filter query schemas.
package schemas

import (
	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/filterquery"
)

var agentCommonKeys = []filterquery.Key{
	{
		Name: "phase",
		Help: "Agent lifecycle phase",
		Kind: filterquery.KindEnum,
		Exec: filterquery.ExecServer,
		Enum: []string{
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
		},
		MultiValued: false,
	},
	{
		Name: "activity",
		Help: "Agent runtime activity",
		Kind: filterquery.KindEnum,
		Exec: filterquery.ExecServer,
		Enum: []string{
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
		},
		MultiValued: false,
	},
	{
		Name:        "label",
		Help:        "Label key or key=value pair",
		Kind:        filterquery.KindLabel,
		Exec:        filterquery.ExecServer,
		MultiValued: true,
	},
	{
		Name:        "harness",
		Help:        "Harness configuration name",
		Kind:        filterquery.KindString,
		Exec:        filterquery.ExecServer,
		MultiValued: false,
	},
	{
		Name:        "broker",
		Help:        "Runtime broker name or ID",
		Kind:        filterquery.KindRef,
		Exec:        filterquery.ExecServer,
		MultiValued: false,
	},
	{
		Name:        "template",
		Help:        "Agent template name",
		Kind:        filterquery.KindString,
		Exec:        filterquery.ExecServer,
		MultiValued: false,
	},
	{
		Name:        "owner",
		Help:        "Owner ID, email, or 'me'",
		Kind:        filterquery.KindRef,
		Exec:        filterquery.ExecServer,
		MultiValued: false,
	},
	{
		Name: "mode",
		Help: "Messaging mode or reachability (client-side)",
		Kind: filterquery.KindEnum,
		Exec: filterquery.ExecClient,
		Enum: []string{
			"hub",
			"project",
			"tree",
			"direct",
			"owner",
			"none",
			"can_message",
			"cannot_message",
		},
		MultiValued: false,
	},
}

// Agents is the filter query schema for the global agents list view.
var Agents = filterquery.Schema{
	View:     "agents",
	TextHelp: "Matches agent name or slug",
	Keys: []filterquery.Key{
		agentCommonKeys[0], // phase
		agentCommonKeys[1], // activity
		agentCommonKeys[2], // label
		{
			Name:        "project",
			Help:        "Project slug or ID",
			Kind:        filterquery.KindRef,
			Exec:        filterquery.ExecServer,
			MultiValued: false,
		},
		agentCommonKeys[3], // harness
		agentCommonKeys[4], // broker
		agentCommonKeys[5], // template
		agentCommonKeys[6], // owner
		{
			Name:        "is",
			Help:        "Scope filter (mine or shared)",
			Kind:        filterquery.KindFlagSet,
			Exec:        filterquery.ExecServer,
			Enum:        []string{"mine", "shared"},
			Exclusive:   [][]string{{"mine", "shared"}},
			MultiValued: false,
		},
		agentCommonKeys[7], // mode
	},
}

// ProjectAgents is the filter query schema for the project-scoped agents list view.
// It exposes the same keys as Agents except project and is:, and its mode enum
// omits reachability values (can_message / cannot_message) because the project
// agents endpoint does not emit per-row _messageability.
var ProjectAgents = filterquery.Schema{
	View:     "project_agents",
	TextHelp: "Matches agent name or slug",
	Keys: []filterquery.Key{
		agentCommonKeys[0], // phase
		agentCommonKeys[1], // activity
		agentCommonKeys[2], // label
		agentCommonKeys[3], // harness
		agentCommonKeys[4], // broker
		agentCommonKeys[5], // template
		agentCommonKeys[6], // owner
		{
			Name: "mode",
			Help: "Messaging mode (client-side)",
			Kind: filterquery.KindEnum,
			Exec: filterquery.ExecClient,
			Enum: []string{
				"hub",
				"project",
				"tree",
				"direct",
				"owner",
				"none",
			},
			MultiValued: false,
		},
	},
}

// All returns every registered view schema in deterministic order.
func All() []filterquery.Schema {
	return []filterquery.Schema{
		Agents,
		ProjectAgents,
		Artifacts,
		ProjectArtifacts,
		Decisions,
		Files,
		Graph,
	}
}
