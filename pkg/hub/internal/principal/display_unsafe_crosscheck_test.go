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

package principal

import (
	"testing"
	"unicode"

	"github.com/GoogleCloudPlatform/scion/pkg/util/plaintext"
)

// TestIsDisplayUnsafeRune_MatchesPlaintext pins that the hub's
// display-unsafe set and plaintext.IsUnsafe (the strip behind notification
// excerpts, ptone/scion#6853) are the same set on every code point, so the
// two cannot drift apart.
func TestIsDisplayUnsafeRune_MatchesPlaintext(t *testing.T) {
	unsafe := 0
	for r := rune(0); r <= unicode.MaxRune; r++ {
		a, b := IsDisplayUnsafeRune(r), plaintext.IsUnsafe(r)
		if a != b {
			t.Fatalf("%U: IsDisplayUnsafeRune %v, plaintext.IsUnsafe %v", r, a, b)
		}
		if a {
			unsafe++
		}
	}
	if unsafe < 65+150 {
		t.Fatalf("only %d unsafe code points; the sets matched too little", unsafe)
	}
}
