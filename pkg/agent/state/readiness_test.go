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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
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

func TestInitRunSpec_RoundTrip(t *testing.T) {
	for _, spec := range []api.ReadySpec{
		DefaultReadySpec(ReadyStrategyHook),
		DefaultReadySpec(ReadyStrategyDelay),
		{Strategy: ReadyStrategyHook, Settle: 0, Timeout: 10 * time.Minute},
		{Strategy: ReadyStrategyHook, Settle: 2500 * time.Millisecond, Timeout: 30 * time.Second},
		{Strategy: ReadyStrategyDelay, Settle: 60 * time.Second},
	} {
		content := FormatInitRunSpec("0123abcd", spec)
		if len(content) > 256 {
			t.Errorf("%+v: content is %d bytes, over the 256-byte probe cap", spec, len(content))
		}
		got, ok := ParseInitRunSpec(content)
		if !ok {
			t.Fatalf("%+v: ParseInitRunSpec(%q) rejected", spec, content)
		}
		want := InitRunSpec{Nonce: "0123abcd", Mode: ReadyModeForStrategy(spec.Strategy), HasSpec: true, Spec: spec}
		if got != want {
			t.Errorf("round trip %q: got %+v, want %+v", content, got, want)
		}
	}
}

// TestFormatInitRunSpec_Line1IsLegacyFormat pins line 1 to the format hosts
// that predate the ready spec read.
func TestFormatInitRunSpec_Line1IsLegacyFormat(t *testing.T) {
	for strategy, mode := range map[string]string{ReadyStrategyHook: ReadyModeHook, ReadyStrategyDelay: ReadyModeSettle} {
		content := FormatInitRunSpec("beef", DefaultReadySpec(strategy))
		line1, rest, _ := strings.Cut(content, "\n")
		if line1+"\n" != FormatInitRun("beef", mode) {
			t.Errorf("%s: line 1 = %q, want %q", strategy, line1, FormatInitRun("beef", mode))
		}
		if strings.Count(rest, "\n") != 1 || !strings.HasSuffix(rest, "\n") {
			t.Errorf("%s: want exactly one more newline-terminated line, got %q", strategy, rest)
		}
	}
	if got := FormatInitRunSpec("beef", DefaultReadySpec(ReadyStrategyHook)); got != "nonce=beef mode=hook\nstrategy=hook settle_ms=1500 deadline_ms=90000\n" {
		t.Errorf("hook default = %q", got)
	}
	if got := FormatInitRunSpec("beef", DefaultReadySpec(ReadyStrategyDelay)); got != "nonce=beef mode=settle\nstrategy=delay settle_ms=8000 deadline_ms=0\n" {
		t.Errorf("delay default = %q", got)
	}
}

func TestParseInitRun_IgnoresLine2(t *testing.T) {
	for _, in := range []string{
		"nonce=abcd mode=hook\nstrategy=hook settle_ms=1500 deadline_ms=90000\n",
		"nonce=abcd mode=hook\ngarbage that is not key=value pairs\n",
		"nonce=abcd mode=hook\nnonce=abcd mode=hook\n",
		"nonce=abcd mode=hook\nstrategy=hook\nthird line\n",
	} {
		nonce, mode, ok := ParseInitRun(in)
		if !ok || nonce != "abcd" || mode != ReadyModeHook {
			t.Errorf("ParseInitRun(%q) = %q %q %v, want abcd hook true", in, nonce, mode, ok)
		}
	}
	// A bad line 1 is still rejected whatever line 2 says.
	for _, in := range []string{
		"\nstrategy=hook settle_ms=1500 deadline_ms=90000\n",
		"nonce=abcd mode=other\nstrategy=hook settle_ms=1500 deadline_ms=90000\n",
		"nonce=abcd mode=hook\r\nstrategy=hook\n",
	} {
		if _, _, ok := ParseInitRun(in); ok {
			t.Errorf("ParseInitRun(%q) accepted", in)
		}
		if _, ok := ParseInitRunSpec(in); ok {
			t.Errorf("ParseInitRunSpec(%q) accepted", in)
		}
	}
}

func TestParseInitRunSpec_NoLine2(t *testing.T) {
	for mode, strategy := range map[string]string{ReadyModeHook: ReadyStrategyHook, ReadyModeSettle: ReadyStrategyDelay} {
		for _, in := range []string{FormatInitRun("abcd", mode), strings.TrimSuffix(FormatInitRun("abcd", mode), "\n"), FormatInitRun("abcd", mode) + "\n", FormatInitRun("abcd", mode) + "   \n"} {
			got, ok := ParseInitRunSpec(in)
			want := InitRunSpec{Nonce: "abcd", Mode: mode, Spec: DefaultReadySpec(strategy)}
			if !ok || got != want {
				t.Errorf("ParseInitRunSpec(%q) = %+v %v, want %+v", in, got, ok, want)
			}
		}
	}
}

func TestParseInitRunSpec_UnknownKeysIgnored_BadValuesDefault(t *testing.T) {
	hook := DefaultReadySpec(ReadyStrategyHook)
	delay := DefaultReadySpec(ReadyStrategyDelay)
	tests := []struct {
		name, in string
		want     api.ReadySpec
	}{
		{"unknown keys and bare tokens ignored", "nonce=ab mode=hook\nfuture=1 strategy=hook bare settle_ms=2000 events=a,b deadline_ms=30000\n",
			api.ReadySpec{Strategy: ReadyStrategyHook, Settle: 2 * time.Second, Timeout: 30 * time.Second}},
		{"first repeated key wins", "nonce=ab mode=hook\nstrategy=hook settle_ms=2000 settle_ms=3000\n",
			api.ReadySpec{Strategy: ReadyStrategyHook, Settle: 2 * time.Second, Timeout: hook.Timeout}},
		{"missing values default", "nonce=ab mode=hook\nstrategy=hook\n", hook},
		{"non-numeric settle", "nonce=ab mode=hook\nstrategy=hook settle_ms=1.5s deadline_ms=30000\n",
			api.ReadySpec{Strategy: ReadyStrategyHook, Settle: hook.Settle, Timeout: 30 * time.Second}},
		{"negative settle", "nonce=ab mode=hook\nstrategy=hook settle_ms=-1\n", hook},
		{"signed settle", "nonce=ab mode=hook\nstrategy=hook settle_ms=+1000\n", hook},
		{"settle over max", "nonce=ab mode=hook\nstrategy=hook settle_ms=60001\n", hook},
		{"settle at max", "nonce=ab mode=hook\nstrategy=hook settle_ms=60000\n",
			api.ReadySpec{Strategy: ReadyStrategyHook, Settle: 60 * time.Second, Timeout: hook.Timeout}},
		{"huge settle", "nonce=ab mode=hook\nstrategy=hook settle_ms=99999999999999999999\n", hook},
		{"deadline under min", "nonce=ab mode=hook\nstrategy=hook deadline_ms=9999\n", hook},
		{"deadline over max", "nonce=ab mode=hook\nstrategy=hook deadline_ms=600001\n", hook},
		{"empty value", "nonce=ab mode=hook\nstrategy=hook settle_ms= deadline_ms=\n", hook},
		{"unknown strategy follows mode", "nonce=ab mode=hook\nstrategy=pane settle_ms=500 deadline_ms=30000\n",
			api.ReadySpec{Strategy: ReadyStrategyHook, Settle: 500 * time.Millisecond, Timeout: 30 * time.Second}},
		{"missing strategy follows mode", "nonce=ab mode=settle\nsettle_ms=4000\n",
			api.ReadySpec{Strategy: ReadyStrategyDelay, Settle: 4 * time.Second}},
		{"strategy disagreeing with mode follows mode", "nonce=ab mode=settle\nstrategy=hook settle_ms=1500 deadline_ms=90000\n",
			api.ReadySpec{Strategy: ReadyStrategyDelay, Settle: 1500 * time.Millisecond}},
		{"delay has no deadline", "nonce=ab mode=settle\nstrategy=delay settle_ms=8000 deadline_ms=30000\n", delay},
		{"CRLF line 2", "nonce=ab mode=hook\nstrategy=hook settle_ms=2000\r\n",
			api.ReadySpec{Strategy: ReadyStrategyHook, Settle: 2 * time.Second, Timeout: hook.Timeout}},
		{"garbage line 2", "nonce=ab mode=settle\n\x00\x01 ===\n", delay},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseInitRunSpec(tt.in)
			if !ok {
				t.Fatalf("ParseInitRunSpec(%q) rejected", tt.in)
			}
			if !got.HasSpec || got.Nonce != "ab" || got.Spec != tt.want {
				t.Errorf("ParseInitRunSpec(%q) = %+v, want HasSpec and spec %+v", tt.in, got, tt.want)
			}
			if err := CheckReadySpec(got.Spec); err != nil {
				t.Errorf("parsed spec %+v fails CheckReadySpec: %v", got.Spec, err)
			}
		})
	}
}

func TestDefaultReadySpec(t *testing.T) {
	if got, want := DefaultReadySpec(ReadyStrategyHook), (api.ReadySpec{Strategy: "hook", Settle: 1500 * time.Millisecond, Timeout: 90 * time.Second}); got != want {
		t.Errorf("hook = %+v, want %+v", got, want)
	}
	for _, s := range []string{ReadyStrategyDelay, "", "pane", "HOOK"} {
		if got, want := DefaultReadySpec(s), (api.ReadySpec{Strategy: "delay", Settle: 8 * time.Second}); got != want {
			t.Errorf("DefaultReadySpec(%q) = %+v, want %+v", s, got, want)
		}
	}
	for _, s := range []string{ReadyStrategyHook, ReadyStrategyDelay} {
		if err := CheckReadySpec(DefaultReadySpec(s)); err != nil {
			t.Errorf("default %s spec fails CheckReadySpec: %v", s, err)
		}
	}
}

func TestCheckReadySpec(t *testing.T) {
	bad := []api.ReadySpec{
		{},
		{Strategy: "pane", Settle: time.Second, Timeout: 30 * time.Second},
		{Strategy: ReadyStrategyDelay, Settle: time.Second, Timeout: 30 * time.Second},
		{Strategy: ReadyStrategyDelay, Settle: -time.Millisecond},
		{Strategy: ReadyStrategyDelay, Settle: 61 * time.Second},
		{Strategy: ReadyStrategyHook, Settle: time.Second},
		{Strategy: ReadyStrategyHook, Settle: time.Second, Timeout: 9 * time.Second},
		{Strategy: ReadyStrategyHook, Settle: time.Second, Timeout: 11 * time.Minute},
	}
	for _, spec := range bad {
		if err := CheckReadySpec(spec); err == nil {
			t.Errorf("CheckReadySpec(%+v) accepted", spec)
		}
	}
	good := []api.ReadySpec{
		{Strategy: ReadyStrategyDelay},
		{Strategy: ReadyStrategyDelay, Settle: 60 * time.Second},
		{Strategy: ReadyStrategyHook, Timeout: 10 * time.Second},
		{Strategy: ReadyStrategyHook, Settle: 60 * time.Second, Timeout: 10 * time.Minute},
	}
	for _, spec := range good {
		if err := CheckReadySpec(spec); err != nil {
			t.Errorf("CheckReadySpec(%+v) = %v", spec, err)
		}
	}
}

// TestReadySpecEnv_Golden pins the SCION_HARNESS_READY wire format, which a
// broker and an image of different versions must agree on.
func TestReadySpecEnv_Golden(t *testing.T) {
	for spec, want := range map[api.ReadySpec]string{
		DefaultReadySpec(ReadyStrategyHook):                                 `{"strategy":"hook","settle_ms":1500,"timeout_ms":90000}`,
		DefaultReadySpec(ReadyStrategyDelay):                                `{"strategy":"delay","settle_ms":8000,"timeout_ms":0}`,
		{Strategy: ReadyStrategyHook, Settle: 0, Timeout: 10 * time.Minute}: `{"strategy":"hook","settle_ms":0,"timeout_ms":600000}`,
	} {
		if got := FormatReadySpecEnv(spec); got != want {
			t.Errorf("FormatReadySpecEnv(%+v) = %s, want %s", spec, got, want)
		}
		got, err := ParseReadySpecEnv(want)
		if err != nil || got != spec {
			t.Errorf("ParseReadySpecEnv(%s) = %+v, %v; want %+v", want, got, err, spec)
		}
	}
}

func TestParseReadySpecEnv_Rejects(t *testing.T) {
	for _, raw := range []string{
		"",
		"null",
		"{}",
		"[]",
		`{"Strategy":"hook","Settle":1500000000,"Timeout":90000000000}`,
		`{"strategy":"hook","settle_ms":1500}`,
		`{"strategy":"delay","settle_ms":8000,"timeout_ms":30000}`,
		`{"strategy":"pane","settle_ms":500,"timeout_ms":30000}`,
		`{"strategy":"delay","settle_ms":-1}`,
		`{"strategy":"delay","settle_ms":60001}`,
		`{"strategy":"hook","settle_ms":0,"timeout_ms":9999}`,
		`{"strategy":"hook","settle_ms":0,"timeout_ms":9223372036854775807}`,
		`{"strategy":"delay","settle_ms":9223372036854775807}`,
		`{"strategy":"delay","settle_ms":"8000"}`,
	} {
		if spec, err := ParseReadySpecEnv(raw); err == nil {
			t.Errorf("ParseReadySpecEnv(%s) accepted: %+v", raw, spec)
		}
	}
	// Unknown fields are ignored.
	got, err := ParseReadySpecEnv(`{"strategy":"delay","settle_ms":3000,"timeout_ms":0,"pane_regex":"x"}`)
	if err != nil || got != (api.ReadySpec{Strategy: ReadyStrategyDelay, Settle: 3 * time.Second}) {
		t.Errorf("unknown field: got %+v, %v", got, err)
	}
}
