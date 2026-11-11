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

package api

import "testing"

// TestIsBrokerRuntimeCause: only the BrokerRuntimeCause values are in the
// set the hub relays; a well-formed but unknown code is not.
func TestIsBrokerRuntimeCause(t *testing.T) {
	for code, want := range map[string]bool{
		BrokerRuntimeCauseForbidden:    true,
		BrokerRuntimeCauseUnauthorized: true,
		"runtime_other":                false,
		"file_not_found":               false,
		"":                             false,
	} {
		if got := IsBrokerRuntimeCause(code); got != want {
			t.Errorf("IsBrokerRuntimeCause(%q) = %v, want %v", code, got, want)
		}
	}
}
