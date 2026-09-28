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

package projectkeys

import "testing"

func TestProjectIDFromLabels(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{"nil", nil, ""},
		{"canonical", map[string]string{LabelProjectID: "p1"}, "p1"},
		{"missing", map[string]string{}, ""},
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
	labels := ProjectIDLabels("p1")
	if labels[LabelProjectID] != "p1" {
		t.Fatalf("ProjectIDLabels() = %#v", labels)
	}
	if len(labels) != 1 {
		t.Fatalf("ProjectIDLabels() carries unexpected keys: %#v", labels)
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
		{"missing", map[string]string{}, ""},
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
		{"missing", map[string]string{}, ""},
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
	nameLabels := ProjectNameLabels("p1")
	if nameLabels[LabelProject] != "p1" || len(nameLabels) != 1 {
		t.Fatalf("ProjectNameLabels() = %#v", nameLabels)
	}

	pathLabels := ProjectPathLabels("/projects/p1")
	if pathLabels[LabelProjectPath] != "/projects/p1" || len(pathLabels) != 1 {
		t.Fatalf("ProjectPathLabels() = %#v", pathLabels)
	}
}
