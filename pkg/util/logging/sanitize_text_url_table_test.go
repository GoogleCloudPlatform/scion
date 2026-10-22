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

package logging

import (
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/util/urlredact/urlredacttest"
)

// SanitizeText on the table every URL redactor shares: URL user info in
// log text follows the CLI display rule, with RedactedMarker as its mask.
func TestURLRedactorTable_SanitizeText(t *testing.T) {
	urlredacttest.Check(t, urlredacttest.LoggingSanitizeText, func(in string) string {
		return SanitizeText(urlredacttest.Text(in))
	})
}

// SanitizeText takes time linear in its input for URL-heavy text: 1 MiB of
// dense scheme starts with no white space, ending in an '@', finishes well
// within a bound that a quadratic scan cannot meet, and no part of the text
// before the '@' is left as a URL with user info.
func TestSanitizeText_URLScanLinearTime(t *testing.T) {
	in := strings.Repeat("a://", 1<<18) + "@h.example"
	start := time.Now()
	got := SanitizeText(in)
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("SanitizeText on %d bytes took %v", len(in), d)
	}
	if want := "a://" + RedactedMarker; got != want {
		t.Errorf("SanitizeText on %d bytes = %q, want %q", len(in), got, want)
	}
}
