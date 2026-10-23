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

package hubclient

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// The hub's project settings endpoint refuses bucket, runtimes, harnesses
// and profiles on PUT and never returns them, so ProjectSettings has no
// field that would send or read them (ptone/scion#5360).
func TestProjectSettings_NoKeysTheHubRefuses(t *testing.T) {
	refused := map[string]bool{"bucket": true, "runtimes": true, "harnesses": true, "profiles": true}
	typ := reflect.TypeOf(ProjectSettings{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" {
			name = f.Name
		}
		if refused[strings.ToLower(name)] {
			t.Errorf("ProjectSettings.%s carries the %q key, which the hub refuses", f.Name, name)
		}
	}

	// A fully zero value marshals to an object with none of them.
	data, err := json.Marshal(ProjectSettings{})
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(data, &keys); err != nil {
		t.Fatal(err)
	}
	for k := range refused {
		if _, ok := keys[k]; ok {
			t.Errorf("marshalled ProjectSettings has the %q key: %s", k, data)
		}
	}
}
