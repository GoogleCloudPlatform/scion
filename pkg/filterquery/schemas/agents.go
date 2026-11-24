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

// Agents is the Phase 1 filter query schema for the global agents list view.
var Agents = filterquery.Schema{
	View:     "agents",
	TextHelp: "Matches agent name or slug",
	Keys: []filterquery.Key{
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
			Name:        "label",
			Help:        "Label key or key=value pair",
			Kind:        filterquery.KindLabel,
			Exec:        filterquery.ExecServer,
			MultiValued: true,
		},
	},
}

// All returns every registered view schema in deterministic order.
func All() []filterquery.Schema {
	return []filterquery.Schema{
		Agents,
		Artifacts,
		ProjectArtifacts,
		Files,
		Graph,
	}
}
