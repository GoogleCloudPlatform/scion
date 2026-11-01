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

package transfertest

// WorkspaceObjectCase is one stored workspace object name and the decision
// transfer.ExcludeWorkspaceObject must make for it: Excluded is "" for a name
// that is materialized, else the exclusion reason ("directory",
// "invalid_path" or "identity").
type WorkspaceObjectCase struct {
	Name     string
	Excluded string
}

// WorkspaceObjectCases is the shared table of object names, including near
// misses, that the hub's listing filter, the Runtime Broker's re-check and
// transfer's own test all feed through their side, so the sides are tested
// against one table and cannot drift apart.
func WorkspaceObjectCases() []WorkspaceObjectCase {
	return []WorkspaceObjectCase{
		{"README.md", ""},
		{"src/main.go", ""},
		{"a/.scion", ""},
		{"a/.scion/settings.yaml", ""},
		{".scionrc", ""},
		{".scion-old/x", ""},
		{".github/workflows/ci.yml", ""},
		{".scion", "identity"},
		{".scion/x", "identity"},
		{".scion/project-id", "identity"},
		{".SCION", "identity"},
		{".Scion/x", "identity"},
		{".SCION/settings.yaml", "identity"},
		{"a/.SCION", ""},
		{"./.scion", "invalid_path"},
		{"./a", "invalid_path"},
		{"../x", "invalid_path"},
		{"a/../../x", "invalid_path"},
		{"/etc/passwd", "invalid_path"},
		{"a//b", "invalid_path"},
		{`a\b`, "invalid_path"},
		{"", "invalid_path"},
		{".", "invalid_path"},
		{"dir/", "directory"},
		{".scion/", "directory"},
		{"/", "directory"},
	}
}
