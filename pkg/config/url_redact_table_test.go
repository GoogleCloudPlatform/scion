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

package config

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/util/urlredact/urlredacttest"
)

// RedactURL and RedactURLsIn on the table every URL redactor shares.
func TestURLRedactorTable_RedactURL(t *testing.T) {
	urlredacttest.Check(t, urlredacttest.ConfigRedactURL, RedactURL)
}

func TestURLRedactorTable_RedactURLsIn(t *testing.T) {
	urlredacttest.Check(t, urlredacttest.ConfigRedactURLsIn, func(in string) string {
		return RedactURLsIn(urlredacttest.Text(in))
	})
}
