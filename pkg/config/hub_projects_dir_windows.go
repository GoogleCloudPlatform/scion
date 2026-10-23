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
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/util/pathcanon"
)

// isUnderHubProjectsDir reports whether p lies below the global
// directory's projects/ directory, where hub-managed workspaces live.
//
// Containment is pathcanon.Within on both sides, so case, 8.3 short names,
// junctions and symlinks are resolved before the comparison. Every
// ambiguity resolves to "below" (its own files are then not loaded as a
// project layer): an unknown global directory, and any Within error. The
// boolean Within returns with an error is never used.
func isUnderHubProjectsDir(p string) bool {
	globalDir, err := GetGlobalDir()
	if err != nil {
		return true
	}
	return projectsDirContains(filepath.Join(globalDir, "projects"), p)
}

// projectsDirContains reports whether p lies strictly below projects; any
// resolution error is "below" (see isUnderHubProjectsDir).
func projectsDirContains(projects, p string) bool {
	under, err := pathcanon.Within(projects, p)
	if err != nil {
		return true
	}
	if !under {
		return false
	}
	// Within is true for projects itself, which is not below it.
	same, err := pathcanon.Within(p, projects)
	if err != nil {
		return true
	}
	return !same
}
