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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/util/urlredact/urlredacttest"
)

// SanitizeText on the table every URL redactor shares: URL user info in
// log text follows the CLI display rule, with RedactedMarker as its mask.
func TestURLRedactorTable_SanitizeText(t *testing.T) {
	urlredacttest.Check(t, urlredacttest.LoggingSanitizeText, func(in string) string {
		return SanitizeText(urlredacttest.Text(in))
	})
}
