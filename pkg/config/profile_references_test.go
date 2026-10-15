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

package config

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestCountProfileReferences(t *testing.T) {
	agents := []ProfileRefAgent{
		{SavedProfile: "local", RuntimeBrokerID: "co"},
		{SavedProfile: "local", RuntimeBrokerID: "co"},
		{SavedProfile: "local", RuntimeBrokerID: ""},                      // unplaced: counted
		{SavedProfile: "remote", RuntimeBrokerID: "other"},                // other broker: skipped
		{SavedProfile: "local", RuntimeBrokerID: "co", Deleted: true},     // skipped
		{SavedProfile: "flat", RuntimeBrokerID: "co", OnFlatBroker: true}, // skipped
		{SavedProfile: "", RuntimeBrokerID: "co"},                         // no profile: ""
	}
	projects := []ProfileRefProject{
		{ID: "p1", ActiveProfile: "local", ProvidedByColocated: true},
		{ID: "p2", ActiveProfile: "remote", ProvidedByColocated: false}, // remote-only project: skipped
		{ID: "p3", ActiveProfile: "", ProvidedByColocated: true},
	}
	refs, ok := CountProfileReferences("co", agents, projects)
	if !ok {
		t.Fatal("ok = false with a broker ID")
	}
	if want := map[string]int{"local": 3, "": 1}; !reflect.DeepEqual(refs.Agents, want) {
		t.Errorf("Agents = %v, want %v", refs.Agents, want)
	}
	if want := map[string]int{"local": 1}; !reflect.DeepEqual(refs.Projects, want) {
		t.Errorf("Projects = %v, want %v", refs.Projects, want)
	}

	refs, ok = CountProfileReferences("", agents, projects)
	if ok || refs.Agents != nil || refs.Projects != nil {
		t.Errorf("empty broker ID: refs=%+v ok=%v, want zero and false", refs, ok)
	}
}

func TestProjectDispatchLocationAndBrokerProjectPath(t *testing.T) {
	global := "/home/u/.scion"
	cases := []struct {
		name               string
		localPath, slug    string
		globalProject      bool
		wantPath, wantSlug string
		wantBrokerPath     string
	}{
		{"linked", "/repo/.scion", "proj", false, "/repo/.scion", "", "/repo/.scion"},
		{"hub-native", "", "proj", false, "", "proj", filepath.Join(global, "projects", "proj")},
		{"global", global, "global", true, global, "global", global},
		{"nothing", "", "", false, "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, s := ProjectDispatchLocation(tc.localPath, tc.slug, tc.globalProject)
			if p != tc.wantPath || s != tc.wantSlug {
				t.Fatalf("ProjectDispatchLocation = (%q, %q), want (%q, %q)", p, s, tc.wantPath, tc.wantSlug)
			}
			if got := BrokerProjectPath(global, p, s); got != tc.wantBrokerPath {
				t.Fatalf("BrokerProjectPath = %q, want %q", got, tc.wantBrokerPath)
			}
		})
	}
}
