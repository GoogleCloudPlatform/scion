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
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// webRegistryFixturePath is read by the web admin settings payload tests
// (web/src/components/pages/__fixtures__/settings-registry.ts, used by
// admin-server-config.test.ts and admin-federation-payload.test.ts;
// ptone/scion#3924), which check every key an admin settings form sends
// against it.
const webRegistryFixturePath = "testdata/web_registry_fixture.json"

var updateWebRegistryFixture = flag.Bool("update-web-registry-fixture", false,
	"rewrite "+webRegistryFixturePath+" from Registry and layer0Prefixes")

// webRegistryFixture is the JSON layout of webRegistryFixturePath.
type webRegistryFixture struct {
	Comment string `json:"_comment"`
	// Layer1Sections maps each Registry section name to its KoanfPaths
	// (empty for a section with no settings.yaml representation).
	Layer1Sections map[string][]string `json:"layer1_sections"`
	// Layer0Prefixes is layer0Prefixes.
	Layer0Prefixes []string `json:"layer0_prefixes"`
	// FileKeys are top-level settings.yaml keys that are neither Layer-1
	// nor Layer-0 (unclassified). A workstation hub writes them to
	// settings.yaml. This list is kept by hand; the test only checks that
	// each entry is a VersionedSettings koanf key and is unclassified.
	FileKeys []string `json:"file_keys"`
}

const webRegistryFixtureComment = "Generated from pkg/config/opsettings/registry.go (Registry) and koanf.go " +
	"(layer0Prefixes); file_keys is kept by hand. Regenerate with: " +
	"go test ./pkg/config/opsettings -run TestWebRegistryFixture -update-web-registry-fixture"

// TestWebRegistryFixture keeps the web test fixture in step with the
// registry, so the web payload test fails when a form sends a key the
// server no longer knows, or misses one it added.
func TestWebRegistryFixture(t *testing.T) {
	raw, err := os.ReadFile(webRegistryFixturePath)
	if err != nil && !*updateWebRegistryFixture {
		t.Fatalf("read %s: %v", webRegistryFixturePath, err)
	}
	var current webRegistryFixture
	if err == nil {
		if err := json.Unmarshal(raw, &current); err != nil {
			t.Fatalf("parse %s: %v", webRegistryFixturePath, err)
		}
	}

	want := webRegistryFixture{
		Comment:        webRegistryFixtureComment,
		Layer1Sections: map[string][]string{},
		Layer0Prefixes: append([]string{}, layer0Prefixes...),
		FileKeys:       current.FileKeys,
	}
	for _, s := range Registry {
		want.Layer1Sections[s.Name] = append([]string{}, s.KoanfPaths...)
	}
	if want.FileKeys == nil {
		want.FileKeys = []string{}
	}

	settingsKeys := map[string]bool{}
	vt := reflect.TypeOf(config.VersionedSettings{})
	for i := 0; i < vt.NumField(); i++ {
		if name, _, _ := strings.Cut(vt.Field(i).Tag.Get("koanf"), ","); name != "" {
			settingsKeys[name] = true
		}
	}
	for _, k := range want.FileKeys {
		if !settingsKeys[k] {
			t.Errorf("file_keys entry %q is not a top-level VersionedSettings koanf key", k)
		}
		if IsLayer1Key(k) || isLayer0Key(k) {
			t.Errorf("file_keys entry %q is classified (Layer-1 or Layer-0); remove it", k)
		}
	}

	if *updateWebRegistryFixture {
		out, err := json.MarshalIndent(want, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, '\n')
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(webRegistryFixturePath, out, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}

	wantJSON, _ := json.Marshal(want)
	gotJSON, _ := json.Marshal(current)
	if !bytes.Equal(wantJSON, gotJSON) {
		t.Errorf("%s is out of date with Registry / layer0Prefixes.\nRegenerate with: "+
			"go test ./pkg/config/opsettings -run TestWebRegistryFixture -update-web-registry-fixture\n"+
			"want: %s\ngot:  %s", webRegistryFixturePath, wantJSON, gotJSON)
	}
}
