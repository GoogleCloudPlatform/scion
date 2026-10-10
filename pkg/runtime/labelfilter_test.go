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

package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLabelsMatchFilter(t *testing.T) {
	tmp := t.TempDir()
	projectDir := filepath.Join(tmp, "proj")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(tmp, "link")
	if err := os.Symlink(projectDir, link); err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(tmp, "proj-other")
	if err := os.MkdirAll(sibling, 0755); err != nil {
		t.Fatal(err)
	}

	labels := map[string]string{
		"scion.agent":        "true",
		"scion.name":         "alpha",
		"scion.project":      "proj",
		"scion.project_id":   "hub-1",
		"scion.project_path": link,
		"scion.template":     "tmpl",
	}
	tests := []struct {
		name   string
		filter map[string]string
		want   bool
	}{
		{name: "nil filter", filter: nil, want: true},
		{name: "empty filter", filter: map[string]string{}, want: true},
		{name: "scion.agent", filter: map[string]string{"scion.agent": "true"}, want: true},
		{name: "scion.agent mismatch", filter: map[string]string{"scion.agent": "false"}, want: false},
		{name: "scion.name", filter: map[string]string{"scion.name": "alpha"}, want: true},
		{name: "scion.name mismatch", filter: map[string]string{"scion.name": "beta"}, want: false},
		{name: "scion.name is exact", filter: map[string]string{"scion.name": "Alpha"}, want: false},
		{name: "scion.project", filter: map[string]string{"scion.project": "proj"}, want: true},
		{name: "scion.project mismatch", filter: map[string]string{"scion.project": "other"}, want: false},
		{name: "scion.project_id", filter: map[string]string{"scion.project_id": "hub-1"}, want: true},
		{name: "scion.project_id mismatch", filter: map[string]string{"scion.project_id": "hub-2"}, want: false},
		{name: "scion.project_path resolves symlinks", filter: map[string]string{"scion.project_path": projectDir}, want: true},
		{name: "scion.project_path sibling", filter: map[string]string{"scion.project_path": sibling}, want: false},
		{name: "status is not a label", filter: map[string]string{"status": "running"}, want: false},
		{name: "absent key never matches a value", filter: map[string]string{"agent_id": "x"}, want: false},
		{name: "all keys must match", filter: map[string]string{"scion.name": "alpha", "scion.project_id": "hub-2"}, want: false},
		{name: "several matching keys", filter: map[string]string{"scion.name": "alpha", "scion.project": "proj", "scion.agent": "true"}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := LabelsMatchFilter(labels, tt.filter); got != tt.want {
				t.Errorf("LabelsMatchFilter(labels, %v) = %v, want %v", tt.filter, got, tt.want)
			}
		})
	}
	if LabelsMatchFilter(nil, map[string]string{"scion.name": "alpha"}) {
		t.Error("LabelsMatchFilter(nil labels, non-empty filter) = true, want false")
	}
}
