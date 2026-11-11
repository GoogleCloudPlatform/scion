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

package opsettings

import (
	"reflect"
	"testing"
)

// withStringOnlyIntervals leaves a schema node with an unexpected shape
// unchanged instead of failing on it.
func TestWithStringOnlyIntervals_UnexpectedShapes(t *testing.T) {
	cases := map[string]map[string]interface{}{
		"no properties":        {"type": "object"},
		"properties not a map": {"properties": "oops"},
		"nil properties":       {"properties": nil},
		"prop not a map":       {"properties": map[string]interface{}{"refresh_interval": "oops"}},
		"nil prop":             {"properties": map[string]interface{}{"refresh_interval": nil}},
		"anyOf not a list":     {"properties": map[string]interface{}{"refresh_interval": map[string]interface{}{"anyOf": "oops"}}},
		"anyOf without string": {"properties": map[string]interface{}{"debounce_interval": map[string]interface{}{"anyOf": []interface{}{"oops", map[string]interface{}{"type": "integer"}}}}},
	}
	for name, node := range cases {
		t.Run(name, func(t *testing.T) {
			got := withStringOnlyIntervals(node)
			if !reflect.DeepEqual(got, node) {
				t.Errorf("withStringOnlyIntervals(%v) = %v, want the node unchanged", node, got)
			}
		})
	}
}

// The expected shape keeps only the string branch, with the description.
func TestWithStringOnlyIntervals_CollapsesToString(t *testing.T) {
	node := map[string]interface{}{"properties": map[string]interface{}{
		"refresh_interval": map[string]interface{}{
			"description": "how often",
			"anyOf":       []interface{}{map[string]interface{}{"type": "integer"}, map[string]interface{}{"type": "string", "pattern": "^x$"}},
		},
	}}
	got := withStringOnlyIntervals(node)
	want := map[string]interface{}{"type": "string", "pattern": "^x$", "description": "how often"}
	if p := got["properties"].(map[string]interface{})["refresh_interval"]; !reflect.DeepEqual(p, want) {
		t.Errorf("refresh_interval = %v, want %v", p, want)
	}
}
