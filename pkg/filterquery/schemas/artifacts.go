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

// Artifacts is the filter query schema for the global artifacts list view.
var Artifacts = filterquery.Schema{
	View:     "artifacts",
	TextHelp: "Matches artifact title or key",
	Keys: []filterquery.Key{
		{
			Name:        "is",
			Help:        "Artifact state or access",
			Kind:        filterquery.KindFlagSet,
			Exec:        filterquery.ExecServer,
			Enum:        []string{"review", "requested", "mine", "shared", "draft"},
			Exclusive:   [][]string{{"mine", "shared"}},
			MultiValued: true,
		},
		{
			Name:        "project",
			Help:        "Home project slug or ID",
			Kind:        filterquery.KindRef,
			Exec:        filterquery.ExecServer,
			MultiValued: false,
		},
	},
}

// ProjectArtifacts is the filter query schema for the project-scoped artifacts list view.
var ProjectArtifacts = filterquery.Schema{
	View:     "artifacts-project",
	TextHelp: "Matches artifact title or key",
	Keys: []filterquery.Key{
		{
			Name:        "is",
			Help:        "Artifact state or shared with this project",
			Kind:        filterquery.KindFlagSet,
			Exec:        filterquery.ExecServer,
			Enum:        []string{"shared", "requested"},
			MultiValued: true,
		},
	},
}
