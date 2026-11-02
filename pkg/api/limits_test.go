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

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestCountLimits_JSONRoundTrip: an absent key and null decode to nil
// (inherit), 0 to an explicit "no limit", and 0 is written back rather than
// omitted (ptone/scion#5605).
func TestCountLimits_JSONRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want *int
	}{
		{in: `{}`, want: nil},
		{in: `{"max_turns": null}`, want: nil},
		{in: `{"max_turns": 0}`, want: IntPtr(0)},
		{in: `{"max_turns": 7}`, want: IntPtr(7)},
	} {
		var c ScionConfig
		if err := json.Unmarshal([]byte(tc.in), &c); err != nil {
			t.Fatalf("%s: %v", tc.in, err)
		}
		if !IntPtrEqual(c.MaxTurns, tc.want) {
			t.Errorf("%s: MaxTurns = %s, want %s", tc.in, FormatCountLimit(c.MaxTurns), FormatCountLimit(tc.want))
		}
	}

	out, err := json.Marshal(ScionConfig{MaxTurns: IntPtr(0), MaxModelCalls: IntPtr(3)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"max_turns":0`) || !strings.Contains(string(out), `"max_model_calls":3`) {
		t.Errorf("marshal = %s, want max_turns 0 and max_model_calls 3 written", out)
	}
	out, err = json.Marshal(ScionConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "max_turns") || strings.Contains(string(out), "max_model_calls") {
		t.Errorf("marshal of unset limits = %s, want both omitted", out)
	}
}

func TestCountLimitHelpers(t *testing.T) {
	if n, ok := CountLimit(nil); ok || n != 0 {
		t.Errorf("CountLimit(nil) = %d, %v", n, ok)
	}
	if n, ok := CountLimit(IntPtr(0)); ok || n != 0 {
		t.Errorf("CountLimit(0) = %d, %v", n, ok)
	}
	if n, ok := CountLimit(IntPtr(4)); !ok || n != 4 {
		t.Errorf("CountLimit(4) = %d, %v", n, ok)
	}
	if !IsUnlimitedCount(IntPtr(0)) || IsUnlimitedCount(nil) || IsUnlimitedCount(IntPtr(1)) {
		t.Error("IsUnlimitedCount: want true only for 0")
	}
	if err := ValidateCountLimit("max_turns", IntPtr(-1)); err == nil {
		t.Error("ValidateCountLimit(-1): want an error")
	}
	for _, p := range []*int{nil, IntPtr(0), IntPtr(9)} {
		if err := ValidateCountLimit("max_turns", p); err != nil {
			t.Errorf("ValidateCountLimit(%s): %v", FormatCountLimit(p), err)
		}
	}
	src := IntPtr(2)
	cp := CloneIntPtr(src)
	*cp = 3
	if *src != 2 || CloneIntPtr(nil) != nil {
		t.Error("CloneIntPtr must copy the value and keep nil")
	}
	if !IntPtrEqual(nil, nil) || IntPtrEqual(nil, IntPtr(0)) || !IntPtrEqual(IntPtr(5), IntPtr(5)) {
		t.Error("IntPtrEqual")
	}
	if FormatCountLimit(nil) != "unset" || FormatCountLimit(IntPtr(0)) != "0" {
		t.Error("FormatCountLimit")
	}
	var nilCfg *ScionConfig
	if _, ok := nilCfg.TurnLimit(); ok {
		t.Error("nil config has no turn limit")
	}
	if n, ok := (&ScionConfig{MaxModelCalls: IntPtr(8)}).ModelCallLimit(); !ok || n != 8 {
		t.Errorf("ModelCallLimit = %d, %v", n, ok)
	}
}
