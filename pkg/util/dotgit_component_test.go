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

package util

import "testing"

func TestIsDotGitComponent(t *testing.T) {
	cases := []struct {
		comp string
		want bool
	}{
		{".git", true},
		{".GIT", true},
		{".Git", true},
		{".git.", true},
		{".git ", true},
		{".git. .", true},
		{"", false},
		{"README.md", false},
		{".gitignore", false},
		{".github", false},
		{"git", false},
		{"x.git", false},
		{".git/config", false},
	}
	for _, c := range cases {
		if got := IsDotGitComponent(c.comp); got != c.want {
			t.Errorf("IsDotGitComponent(%q) = %v, want %v", c.comp, got, c.want)
		}
	}
}
