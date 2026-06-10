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

package projectcompat

import "testing"

func TestProjectIDFromLabels(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{"nil", nil, ""},
		{"canonical", map[string]string{LabelProjectID: "p1"}, "p1"},
		{"legacy", map[string]string{LabelGroveID: "p1"}, "p1"},
		{"canonical wins", map[string]string{LabelProjectID: "p1", LabelGroveID: "old"}, "p1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ProjectIDFromLabels(tt.labels); got != tt.want {
				t.Fatalf("ProjectIDFromLabels() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestProjectIDLabels(t *testing.T) {
	labels := ProjectIDLabels("p1", true)
	if labels[LabelProjectID] != "p1" || labels[LabelGroveID] != "p1" {
		t.Fatalf("ProjectIDLabels(includeLegacy=true) = %#v", labels)
	}

	labels = ProjectIDLabels("p1", false)
	if labels[LabelProjectID] != "p1" {
		t.Fatalf("ProjectIDLabels(includeLegacy=false) missing canonical label: %#v", labels)
	}
	if _, ok := labels[LabelGroveID]; ok {
		t.Fatalf("ProjectIDLabels(includeLegacy=false) included legacy label: %#v", labels)
	}
}

func TestProjectNameFromLabels(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{"nil", nil, ""},
		{"canonical", map[string]string{LabelProject: "p1"}, "p1"},
		{"legacy", map[string]string{LabelGrove: "p1"}, "p1"},
		{"canonical wins", map[string]string{LabelProject: "p1", LabelGrove: "old"}, "p1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ProjectNameFromLabels(tt.labels); got != tt.want {
				t.Fatalf("ProjectNameFromLabels() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestProjectPathFromLabels(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{"nil", nil, ""},
		{"canonical", map[string]string{LabelProjectPath: "/projects/p1"}, "/projects/p1"},
		{"legacy", map[string]string{LabelGrovePath: "/groves/p1"}, "/groves/p1"},
		{"canonical wins", map[string]string{LabelProjectPath: "/projects/p1", LabelGrovePath: "/groves/p1"}, "/projects/p1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ProjectPathFromLabels(tt.labels); got != tt.want {
				t.Fatalf("ProjectPathFromLabels() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestProjectNameAndPathLabels(t *testing.T) {
	nameLabels := ProjectNameLabels("p1", true)
	if nameLabels[LabelProject] != "p1" || nameLabels[LabelGrove] != "p1" {
		t.Fatalf("ProjectNameLabels(includeLegacy=true) = %#v", nameLabels)
	}
	if _, ok := ProjectNameLabels("p1", false)[LabelGrove]; ok {
		t.Fatalf("ProjectNameLabels(includeLegacy=false) included legacy label")
	}

	pathLabels := ProjectPathLabels("/projects/p1", true)
	if pathLabels[LabelProjectPath] != "/projects/p1" || pathLabels[LabelGrovePath] != "/projects/p1" {
		t.Fatalf("ProjectPathLabels(includeLegacy=true) = %#v", pathLabels)
	}
	if _, ok := ProjectPathLabels("/projects/p1", false)[LabelGrovePath]; ok {
		t.Fatalf("ProjectPathLabels(includeLegacy=false) included legacy label")
	}
}

func TestCanonicalFieldAliases(t *testing.T) {
	tests := []struct {
		in        string
		canonical string
		legacy    bool
	}{
		{"projectId", "projectId", false},
		{"groveId", "projectId", true},
		{"grove_id", "project_id", true},
		{"hub.grove_id", "hub.project_id", true},
		{"unrelated", "unrelated", false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			canonical, legacy := CanonicalFieldAliases(tt.in)
			if canonical != tt.canonical || legacy != tt.legacy {
				t.Fatalf("CanonicalFieldAliases(%q) = %q, %v; want %q, %v", tt.in, canonical, legacy, tt.canonical, tt.legacy)
			}
		})
	}
}

func TestDeprecatedGroveRoute(t *testing.T) {
	for _, path := range []string{"/api/v1/groves", "/api/v1/groves/p1/agents"} {
		if !DeprecatedGroveRoute(path) {
			t.Fatalf("DeprecatedGroveRoute(%q) = false, want true", path)
		}
	}
	for _, path := range []string{"/api/v1/projects", "/api/v1/groves-old"} {
		if DeprecatedGroveRoute(path) {
			t.Fatalf("DeprecatedGroveRoute(%q) = true, want false", path)
		}
	}
}
