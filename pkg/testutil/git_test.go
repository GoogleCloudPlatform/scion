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

package testutil

import "testing"

func TestGitVersionAtLeast(t *testing.T) {
	tests := []struct {
		version string
		want    bool
	}{
		{"2.56.0", true},
		{"2.35.2", true},
		{"2.35.1", false},
		{"2.34.9", false},
		{"2.39.3 (Apple Git-145)", true},
		{"3.0", true},
		{"", false},
	}
	for _, tt := range tests {
		if got := GitVersionAtLeast(tt.version, 2, 35, 2); got != tt.want {
			t.Errorf("GitVersionAtLeast(%q) = %v, want %v", tt.version, got, tt.want)
		}
	}
}
