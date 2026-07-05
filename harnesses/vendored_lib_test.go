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

package harnesses

import (
	"io/fs"
	"strings"
	"testing"
)

const generatedHeader = "# GENERATED FILE — DO NOT EDIT. Source: harnesses/scion_harness.py\n"

func TestVendoredLibMatchesCanonical(t *testing.T) {
	canonical := CanonicalHarnessLib
	if len(canonical) == 0 {
		t.Fatal("canonical scion_harness.py is empty")
	}

	want := generatedHeader + string(canonical)

	entries, err := fs.ReadDir(FS, ".")
	if err != nil {
		t.Fatalf("read harnesses FS root: %v", err)
	}

	found := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		vendoredPath := name + "/scion_harness.py"

		data, err := fs.ReadFile(FS, vendoredPath)
		if err != nil {
			// Not every subdirectory is required to have a vendored copy;
			// only those that have provision.py (real bundles).
			if _, provErr := fs.ReadFile(FS, name+"/provision.py"); provErr != nil {
				continue
			}
			t.Errorf("%s: missing vendored scion_harness.py (run: go generate ./harnesses/)", name)
			continue
		}
		found++

		if !strings.HasPrefix(string(data), generatedHeader) {
			t.Errorf("%s/scion_harness.py: missing GENERATED header (run: go generate ./harnesses/)", name)
			continue
		}

		if string(data) != want {
			t.Errorf("%s/scion_harness.py: content does not match canonical harnesses/scion_harness.py (run: go generate ./harnesses/)", name)
		}
	}

	if found == 0 {
		t.Fatal("no vendored scion_harness.py copies found — expected at least one bundle")
	}
}
