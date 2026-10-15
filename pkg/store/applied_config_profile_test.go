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

package store

import "testing"

// AppliedConfigProfile decodes as the hub's agent store does: any decode
// error, including a mistyped field other than profile, gives "".
func TestAppliedConfigProfile(t *testing.T) {
	for _, c := range []struct {
		raw     string
		profile string
		decoded bool
	}{
		{``, "", true},
		{`{"profile":"local"}`, "local", true},
		{`{"image":"img:1"}`, "", true},
		{`{"profile":"local"`, "", false},
		{`{"profile":"local","thinkingLevel":"high"}`, "", false},
		{`{"profile":7}`, "", false},
	} {
		p, ok := AppliedConfigProfile(c.raw)
		if p != c.profile || ok != c.decoded {
			t.Errorf("AppliedConfigProfile(%q) = (%q, %v), want (%q, %v)", c.raw, p, ok, c.profile, c.decoded)
		}
	}
}
