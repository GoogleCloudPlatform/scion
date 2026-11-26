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

import "github.com/GoogleCloudPlatform/scion/pkg/filterquery"

// Decisions is the filter query schema for the project Decisions page
// (covers both Pending and Made tabs; Made-only keys are ignored on Pending).
var Decisions = filterquery.Schema{
	View:     "decisions",
	TextHelp: "Matches question, answer, conversation label, or agent name",
	Keys: []filterquery.Key{
		{
			Name:        "agent",
			Help:        "Asking agent name or ID",
			Kind:        filterquery.KindRef,
			Exec:        filterquery.ExecServer,
			MultiValued: false,
		},
		{
			Name:        "conversation",
			Help:        "Conversation label or chat key",
			Kind:        filterquery.KindRef,
			Exec:        filterquery.ExecServer,
			MultiValued: false,
		},
		{
			Name:        "by",
			Help:        "me, email, ID, or display name · Made tab only",
			Kind:        filterquery.KindRef,
			Exec:        filterquery.ExecServer,
			MultiValued: false,
		},
		{
			Name: "provenance",
			Help: "How it was answered · Made tab only",
			Kind: filterquery.KindEnum,
			Exec: filterquery.ExecServer,
			Enum: []string{
				"direct",
				"interpreted",
				"confirmed",
				"overridden",
				"dismissed",
			},
			MultiValued: false,
		},
	},
}

// ProfileDecisions is the filter query schema for the profile Decisions page
// (covers both Pending and Made tabs; adds project: to Decisions).
var ProfileDecisions = filterquery.Schema{
	View:     "profile-decisions",
	TextHelp: "Matches question, answer, conversation label, or agent name",
	Keys: append(
		append([]filterquery.Key(nil), Decisions.Keys...),
		filterquery.Key{
			Name:        "project",
			Help:        "Project slug or ID (your projects)",
			Kind:        filterquery.KindRef,
			Exec:        filterquery.ExecServer,
			MultiValued: false,
		},
	),
}
