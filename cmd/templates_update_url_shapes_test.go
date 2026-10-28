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

package cmd

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/util/urlredact/urlredacttest"
)

// No part of the credential of any shape shows in a printed template
// source, and an ordinary or token-bearing source keeps its host and path.
func TestDisplaySourceURL_Shapes(t *testing.T) {
	for _, s := range urlredacttest.Shapes {
		if got := displaySourceURL(s.URL); urlredacttest.SecretFragment(got, s.Secret) != "" {
			t.Errorf("%s: displaySourceURL(%q) = %q shows part of the secret", s.Name, s.URL, got)
		}
	}
	for _, k := range urlredacttest.KeptURLs {
		if got := displaySourceURL(k.URL); got != k.Want {
			t.Errorf("%s: displaySourceURL(%q) = %q, want %q", k.Name, k.URL, got, k.Want)
		}
	}
}
