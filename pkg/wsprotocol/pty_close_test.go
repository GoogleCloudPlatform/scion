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

package wsprotocol

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

type ptyCloseFixture struct {
	Codes []struct {
		Code        int    `json:"code"`
		Name        string `json:"name"`
		Disposition string `json:"disposition"`
	} `json:"codes"`
}

// TestClassifyPTYClose_Parity checks ClassifyPTYClose against the fixture the
// web client's classifyPtyClose is also tested against.
func TestClassifyPTYClose_Parity(t *testing.T) {
	raw, err := os.ReadFile("testdata/pty_close_codes.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var fixture ptyCloseFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	if len(fixture.Codes) == 0 {
		t.Fatal("fixture has no rows")
	}
	seen := map[int]bool{}
	for _, row := range fixture.Codes {
		if seen[row.Code] {
			t.Errorf("duplicate fixture row for code %d", row.Code)
		}
		seen[row.Code] = true
		if got := ClassifyPTYClose(row.Code).String(); got != row.Disposition {
			t.Errorf("ClassifyPTYClose(%d) [%s] = %s, fixture says %s", row.Code, row.Name, got, row.Disposition)
		}
	}
	// Every named contract constant must have a fixture row.
	for _, code := range []int{
		ClosePTYNormal, ClosePTYGoingAway, ClosePTYAbnormal, ClosePTYInternalError,
		ClosePTYServiceRestart, ClosePTYTryAgainLater, ClosePTYAuthRequired,
		ClosePTYForbidden, ClosePTYAgentNotFound, ClosePTYSessionGone,
		ClosePTYUpstreamUnavailable, ClosePTYUpstreamTimeout,
	} {
		if !seen[code] {
			t.Errorf("contract constant %d has no fixture row", code)
		}
	}
}

func TestMapBrokerStreamCloseCode(t *testing.T) {
	cases := map[int]int{
		0:    1000, // legacy "tmux client ended"
		404:  4404, // legacy lookup failure
		500:  1011, // legacy error
		1:    1011,
		999:  1011,
		1000: 1000,
		1011: 1011,
		1015: 1015,
		1016: 1011,
		3000: 1011,
		3999: 1011,
		4000: 4000,
		4410: 4410,
		4503: 4503,
		4999: 4999,
		5000: 1011,
		-1:   1011,
	}
	for in, want := range cases {
		if got := MapBrokerStreamCloseCode(in); got != want {
			t.Errorf("MapBrokerStreamCloseCode(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestIsSendableCloseCode(t *testing.T) {
	for _, code := range []int{1000, 1001, 1011, 1012, 1013, 1014, 3000, 4000, 4410, 4503, 4999} {
		if !IsSendableCloseCode(code) {
			t.Errorf("IsSendableCloseCode(%d) = false, want true", code)
		}
	}
	for _, code := range []int{0, 999, 1004, 1005, 1006, 1015, 1016, 2999, 5000} {
		if IsSendableCloseCode(code) {
			t.Errorf("IsSendableCloseCode(%d) = true, want false", code)
		}
	}
}

func TestTruncateCloseReason(t *testing.T) {
	if got := TruncateCloseReason("session_ended"); got != "session_ended" {
		t.Errorf("short reason changed: %q", got)
	}
	long := strings.Repeat("a", 200)
	if got := TruncateCloseReason(long); len(got) != MaxCloseReasonBytes {
		t.Errorf("len = %d, want %d", len(got), MaxCloseReasonBytes)
	}
	// A multi-byte rune straddling the limit must not be split.
	multi := strings.Repeat("a", MaxCloseReasonBytes-1) + "é" + "tail"
	got := TruncateCloseReason(multi)
	if len(got) > MaxCloseReasonBytes || !utf8.ValidString(got) {
		t.Errorf("truncation split a rune: len=%d valid=%v", len(got), utf8.ValidString(got))
	}
}

func TestTruncateCloseReason_InvalidUTF8(t *testing.T) {
	cases := map[string]string{
		"short":            "session\xffended",
		"long":             strings.Repeat("a", 100) + "\xc3" + strings.Repeat("b", 100) + "\xed\xa0\x80",
		"straddling limit": strings.Repeat("a", MaxCloseReasonBytes-1) + "\xff\xfeé" + "tail",
	}
	for name, in := range cases {
		got := TruncateCloseReason(in)
		if !utf8.ValidString(got) {
			t.Errorf("%s: result is not valid UTF-8: %q", name, got)
		}
		if len(got) > MaxCloseReasonBytes {
			t.Errorf("%s: len = %d, want <= %d", name, len(got), MaxCloseReasonBytes)
		}
	}
	if got := TruncateCloseReason("session\xffended"); got != "sessionended" {
		t.Errorf("invalid bytes not dropped: %q", got)
	}
}
