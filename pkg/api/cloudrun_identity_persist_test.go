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
	"reflect"
	"testing"
)

// StartOptions.CloudRunIdentity is per dispatch and never persisted.
//
// StartOptions itself cannot be JSON-encoded as a whole (its hook fields
// are funcs, which encoding/json refuses), so the pin is structural: the
// field is tagged json:"-", and the types written to disk for an agent
// (ScionConfig as scion-agent.json, AgentInfo as its recorded state)
// contain no CloudRunIdentity anywhere in their field graph.
func TestCloudRunIdentity_NeverPersisted(t *testing.T) {
	f, ok := reflect.TypeOf(StartOptions{}).FieldByName("CloudRunIdentity")
	if !ok {
		t.Fatal("StartOptions has no CloudRunIdentity field")
	}
	if got := f.Tag.Get("json"); got != "-" {
		t.Errorf(`StartOptions.CloudRunIdentity json tag = %q, want "-"`, got)
	}
	if got, ok := f.Tag.Lookup("yaml"); ok && got != "-" {
		t.Errorf(`StartOptions.CloudRunIdentity yaml tag = %q, want none or "-"`, got)
	}

	target := reflect.TypeOf(CloudRunIdentity{})
	for _, persisted := range []reflect.Type{reflect.TypeOf(ScionConfig{}), reflect.TypeOf(AgentInfo{})} {
		if path, found := findType(persisted, target, persisted.Name(), map[reflect.Type]bool{}); found {
			t.Errorf("%s reaches CloudRunIdentity at %s; it must never be persisted", persisted.Name(), path)
		}
	}
}

// findType reports whether target is reachable from t through struct
// fields, pointers, slices, arrays and maps, with the field path.
func findType(t, target reflect.Type, path string, seen map[reflect.Type]bool) (string, bool) {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
		t = t.Elem()
	}
	if t == target {
		return path, true
	}
	if seen[t] {
		return "", false
	}
	seen[t] = true
	switch t.Kind() {
	case reflect.Map:
		if p, ok := findType(t.Key(), target, path+"[key]", seen); ok {
			return p, true
		}
		return findType(t.Elem(), target, path+"[]", seen)
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			sf := t.Field(i)
			if p, ok := findType(sf.Type, target, path+"."+sf.Name, seen); ok {
				return p, true
			}
		}
	}
	return "", false
}
