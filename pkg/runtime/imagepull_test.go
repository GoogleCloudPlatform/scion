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

package runtime

import (
	"io/fs"
	"sort"
	"testing"

	harnessesEmbed "github.com/GoogleCloudPlatform/scion/harnesses"
)

// TestHarnessImageMap_MatchesBundledHarnesses asserts that harnessImageMap has
// exactly one entry per bundled harness that ships an image (a harnesses/<name>/
// directory with a Dockerfile), and that each maps to scion-<name>:latest.
// HarnessImages silently skips unknown keys, so a harness missing from the map
// would otherwise be dropped from image pulls with no error.
func TestHarnessImageMap_MatchesBundledHarnesses(t *testing.T) {
	entries, err := fs.ReadDir(harnessesEmbed.FS, ".")
	if err != nil {
		t.Fatalf("read embedded harnesses FS: %v", err)
	}
	var want []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := fs.Stat(harnessesEmbed.FS, e.Name()+"/Dockerfile"); err != nil {
			continue
		}
		want = append(want, e.Name())
	}
	if len(want) == 0 {
		t.Fatal("found no harnesses/<name>/Dockerfile in the embedded FS; harness discovery is broken")
	}

	got := make([]string, 0, len(harnessImageMap))
	for k := range harnessImageMap {
		got = append(got, k)
	}
	sort.Strings(want)
	sort.Strings(got)

	if len(got) != len(want) {
		t.Errorf("harnessImageMap keys = %v, want harnesses with a Dockerfile = %v", got, want)
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("harnessImageMap keys = %v, want harnesses with a Dockerfile = %v", got, want)
				break
			}
		}
	}

	for _, name := range want {
		if img, ok := harnessImageMap[name]; ok && img != "scion-"+name+":latest" {
			t.Errorf("harnessImageMap[%q] = %q, want %q", name, img, "scion-"+name+":latest")
		}
	}
}

func TestHarnessImages_Registry(t *testing.T) {
	got := HarnessImages([]string{"muse-code", "unknown", "grok-build"}, "example.com/reg")
	want := []string{"example.com/reg/scion-muse-code:latest", "example.com/reg/scion-grok-build:latest"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("HarnessImages = %v, want %v", got, want)
	}
}
