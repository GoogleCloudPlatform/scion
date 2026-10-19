// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package registry

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestCapabilitiesFeaturesRoundTrip: the features list survives the JSON
// document the registry stores, an absent list stays absent, and
// HasFeature matches exact names only.
func TestCapabilitiesFeaturesRoundTrip(t *testing.T) {
	in := Capabilities{StreamKinds: []string{"pty"}, Features: []string{"stream_renewal", "other.v2"}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Capabilities
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out.Features, in.Features) {
		t.Fatalf("features after round trip = %v, want %v", out.Features, in.Features)
	}
	for name, want := range map[string]bool{"stream_renewal": true, "other.v2": true, "stream_renewal.v2": false, "other": false, "": false} {
		if got := out.HasFeature(name); got != want {
			t.Errorf("HasFeature(%q) = %v, want %v", name, got, want)
		}
	}
	if out.Has("stream_renewal") {
		t.Error("Has matched a feature; features are not stream kinds or RPCs")
	}

	b, err = json.Marshal(Capabilities{StreamKinds: []string{"pty"}})
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["features"]; ok {
		t.Fatalf("document without features = %s, want no features key", b)
	}
}
