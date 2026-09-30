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

package telemetrycontract

import "testing"

func TestValidTokenType(t *testing.T) {
	for _, v := range TokenTypes {
		if !ValidTokenType(v) {
			t.Errorf("ValidTokenType(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"", "bogus", "Input", "cache-read"} {
		if ValidTokenType(v) {
			t.Errorf("ValidTokenType(%q) = true, want false", v)
		}
	}
}

func TestSummableTokenTypesExcludesReasoning(t *testing.T) {
	for _, v := range SummableTokenTypes {
		if v == TokenTypeReasoning {
			t.Fatal("SummableTokenTypes must not include TokenTypeReasoning: it already overlaps TokenTypeOutput")
		}
		if !ValidTokenType(v) {
			t.Errorf("SummableTokenTypes contains invalid token type %q", v)
		}
	}
	if len(SummableTokenTypes) != 4 {
		t.Errorf("len(SummableTokenTypes) = %d, want 4", len(SummableTokenTypes))
	}
}
