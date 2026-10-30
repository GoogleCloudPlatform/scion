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

package harness

import (
	"io/fs"
	"testing"

	harnessesEmbed "github.com/GoogleCloudPlatform/scion/harnesses"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

func TestVendor(t *testing.T) {
	tests := []struct {
		harness string
		want    string
	}{
		{"claude", VendorAnthropic},
		{"gemini-cli", VendorGoogle},
		{"antigravity", VendorGoogle},
		{"codex", VendorOpenAI},
		{"grok-build", VendorXAI},
		{"muse-code", VendorMeta},
		{"opencode", "opencode"},
		{"copilot", "copilot"},
		{"hermes", "hermes"},
		{"Claude", VendorAnthropic},
		{" codex ", VendorOpenAI},
		{"generic", ""},
		{"my-custom-harness", ""},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.harness, func(t *testing.T) {
			if got := Vendor(tt.harness); got != tt.want {
				t.Errorf("Vendor(%q) = %q, want %q", tt.harness, got, tt.want)
			}
		})
	}
}

// TestVendor_CoversEveryBuiltInHarness fails when a harness is added under
// harnesses/ without a vendor entry, or an entry names no built-in harness.
func TestVendor_CoversEveryBuiltInHarness(t *testing.T) {
	entries, err := fs.ReadDir(harnessesEmbed.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	builtIn := map[string]bool{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		data, err := fs.ReadFile(harnessesEmbed.FS, e.Name()+"/config.yaml")
		if err != nil {
			continue // not a harness directory (e.g. gen/)
		}
		entry, err := config.ParseHarnessConfigYAML(data)
		if err != nil {
			t.Fatalf("parse %s/config.yaml: %v", e.Name(), err)
		}
		builtIn[entry.Harness] = true
		if Vendor(entry.Harness) == "" {
			t.Errorf("built-in harness %q has no vendor in harnessVendors", entry.Harness)
		}
	}
	if len(builtIn) == 0 {
		t.Fatal("no built-in harnesses found")
	}
	for name := range harnessVendors {
		if !builtIn[name] {
			t.Errorf("harnessVendors entry %q names no built-in harness", name)
		}
	}
}
