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
	"encoding/json"
	"reflect"
	"testing"

	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/v2"
)

func overlayTestBootstrap(t *testing.T) *koanf.Koanf {
	t.Helper()
	k := koanf.New(".")
	if err := k.Load(confmap.Provider(map[string]interface{}{
		"runtimes.docker.type":           "docker",
		"profiles.local.runtime":         "docker",
		"harness_configs.claude.harness": "claude",
	}, "."), nil); err != nil {
		t.Fatal(err)
	}
	return k
}

// A stored document wins (an empty or null one is an empty map); with no
// stored document the bootstrap koanf is the fallback.
func TestOverlayMapSections_StoredOrBootstrap(t *testing.T) {
	k := overlayTestBootstrap(t)

	got, errs := OverlayMapSections(map[string]json.RawMessage{
		"runtimes": json.RawMessage(`{"k8s":{"type":"kubernetes"}}`),
		"profiles": json.RawMessage(`{}`),
	}, k)
	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	if !reflect.DeepEqual(got.Runtimes, map[string]V1RuntimeConfig{"k8s": {Type: "kubernetes"}}) {
		t.Errorf("runtimes = %v, want the stored k8s entry", got.Runtimes)
	}
	if got.Profiles == nil || len(got.Profiles) != 0 {
		t.Errorf("profiles = %#v, want a non-nil empty map (stored {})", got.Profiles)
	}
	if _, ok := got.HarnessConfigs["claude"]; !ok || len(got.HarnessConfigs) != 1 {
		t.Errorf("harness_configs = %v, want the bootstrap claude entry", got.HarnessConfigs)
	}

	got, _ = OverlayMapSections(map[string]json.RawMessage{"profiles": json.RawMessage(`null`)}, k)
	if got.Profiles == nil || len(got.Profiles) != 0 {
		t.Errorf("null stored profiles = %#v, want a non-nil empty map", got.Profiles)
	}
	if !reflect.DeepEqual(got.Runtimes, map[string]V1RuntimeConfig{"docker": {Type: "docker"}}) {
		t.Errorf("runtimes with no stored doc = %v, want the bootstrap docker entry", got.Runtimes)
	}

	got, _ = OverlayMapSections(nil, nil)
	if got.Runtimes != nil || got.Profiles != nil || got.HarnessConfigs != nil {
		t.Errorf("no stored docs and no bootstrap = %+v, want all nil", got)
	}
}

// A stored document that does not decode leaves that section nil and is
// reported; the other sections are unaffected.
func TestOverlayMapSections_DecodeError(t *testing.T) {
	got, errs := OverlayMapSections(map[string]json.RawMessage{
		"profiles": json.RawMessage(`["not", "a", "map"]`),
	}, overlayTestBootstrap(t))
	if len(errs) != 1 || errs[0].Section != "profiles" {
		t.Fatalf("errors = %v, want one profiles decode error", errs)
	}
	if got.Profiles != nil {
		t.Errorf("profiles = %v, want nil after a decode error", got.Profiles)
	}
	if len(got.Runtimes) != 1 {
		t.Errorf("runtimes = %v, want the bootstrap entry", got.Runtimes)
	}
}
