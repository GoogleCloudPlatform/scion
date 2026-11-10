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

package messaging

import "testing"

func TestIsHTMLOrSVG(t *testing.T) {
	cases := []struct {
		name string
		head string
		want bool
	}{
		{"page.html", "", true},
		{"PAGE.HTM", "", true},
		{"icon.SVG", "", true},
		{"notes.txt", "<!doctype html><HTML><body>", true},
		{"image.bin", "<?xml version=\"1.0\"?><svg xmlns=", true},
		{"notes.txt", "plain text", false},
		{"report.pdf", "%PDF-1.7", false},
		{"", "", false},
	}
	for _, c := range cases {
		if got := IsHTMLOrSVG(c.name, []byte(c.head)); got != c.want {
			t.Errorf("IsHTMLOrSVG(%q, %q) = %v, want %v", c.name, c.head, got, c.want)
		}
	}
}
