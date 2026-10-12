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

package state

import (
	"strings"
	"testing"
)

func TestInitRunRoundTrip(t *testing.T) {
	for _, mode := range []string{ReadyModeHook, ReadyModeSettle} {
		nonce, gotMode, ok := ParseInitRun(FormatInitRun("0123abcd", mode))
		if !ok || nonce != "0123abcd" || gotMode != mode {
			t.Fatalf("round trip %s: got %q %q %v", mode, nonce, gotMode, ok)
		}
	}
	if n, ok := ParseReadyMarker(FormatReadyMarker("beef")); !ok || n != "beef" {
		t.Fatalf("marker round trip: %q %v", n, ok)
	}
}

func TestParseInitRunRejectsMalformed(t *testing.T) {
	for _, in := range []string{
		"",
		"\n",
		"nonce=abcd\n",
		"mode=hook\n",
		"nonce=abcd mode=other\n",
		"nonce=ABCD mode=hook\n",
		"nonce=xyz mode=hook\n",
		"nonce= mode=hook\n",
		"nonce=abcd mode=hook extra=1\n",
		"nonce=abcd nonce=abcd\n",
		"nonce=abcd mode=hook\nnonce=abcd mode=hook\n",
		"nonce=abcd  mode=hook\n",
		"nonce=abcd mode=hook\r\n",
		"nonce=" + strings.Repeat("a", maxNonceLen+1) + " mode=hook\n",
	} {
		if _, _, ok := ParseInitRun(in); ok {
			t.Errorf("ParseInitRun(%q) accepted", in)
		}
	}
	for _, in := range []string{"", "nonce=\n", "nonce=abcd mode=hook\n", "abcd\n", "nonce=zz\n"} {
		if _, ok := ParseReadyMarker(in); ok {
			t.Errorf("ParseReadyMarker(%q) accepted", in)
		}
	}
}
