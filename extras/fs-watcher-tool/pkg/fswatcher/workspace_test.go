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

package fswatcher

import "testing"

func TestIsWorkspaceMount(t *testing.T) {
	tests := []struct {
		dest string
		want bool
	}{
		{"/workspace", true},
		{"/repo-root", true},
		{"/repo-root/.scion/agents/my-agent/workspace", true},
		{"/repo-root/.scion/agents/foo/workspace", true},
		{"/home/user/workspace", false},
		{"/home/gemini", false},
		{"/tmp", false},
		{"/repo-root/.scion/agents/foo/home", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.dest, func(t *testing.T) {
			got := isWorkspaceMount(tt.dest)
			if got != tt.want {
				t.Errorf("isWorkspaceMount(%q) = %v, want %v", tt.dest, got, tt.want)
			}
		})
	}
}
