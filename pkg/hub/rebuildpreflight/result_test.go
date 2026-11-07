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

package rebuildpreflight

import (
	"reflect"
	"testing"
)

func TestMergeFinal(t *testing.T) {
	cases := []struct {
		name     string
		executor map[string]any
		errText  string
		want     map[string]any
	}{
		{"no result, success", nil, "", nil},
		{"no result, failure", nil, "boom", map[string]any{"error": "boom"}},
		{"result, success", map[string]any{"commit": "abc", "n": 2}, "", map[string]any{"commit": "abc", "n": 2}},
		{"result, failure", map[string]any{"commit": "abc"}, "boom", map[string]any{"commit": "abc", "error": "boom"}},
		{"executor error key loses to the run error", map[string]any{"error": "mine", "commit": "abc"}, "boom", map[string]any{"commit": "abc", "error": "boom"}},
		{"executor error key dropped on success", map[string]any{"error": "mine", "commit": "abc"}, "", map[string]any{"commit": "abc"}},
		{"only an executor error key, success", map[string]any{"error": "mine"}, "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := MergeFinal(tc.executor, tc.errText)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("MergeFinal(%v, %q) = %v, want %v", tc.executor, tc.errText, got, tc.want)
			}
		})
	}
}

// MergeFinal copies: the executor's map is never changed.
func TestMergeFinal_DoesNotModifyTheExecutorResult(t *testing.T) {
	in := map[string]any{"error": "mine", "commit": "abc"}
	_ = MergeFinal(in, "boom")
	if !reflect.DeepEqual(in, map[string]any{"error": "mine", "commit": "abc"}) {
		t.Errorf("executor result changed: %v", in)
	}
}
