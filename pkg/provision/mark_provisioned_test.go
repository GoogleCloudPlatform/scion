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

package provision

import (
	"path/filepath"
	"testing"
)

func TestMarkProvisioned(t *testing.T) {
	dir := t.TempDir()
	if SentinelPresent(dir) {
		t.Fatal("fixture: no sentinel yet")
	}
	if err := MarkProvisioned(dir); err != nil {
		t.Fatal(err)
	}
	if !SentinelPresent(dir) {
		t.Fatal("MarkProvisioned should write the sentinel")
	}
	matches, _ := filepath.Glob(filepath.Join(dir, ".scion-provisioned-*"))
	if len(matches) != 0 {
		t.Errorf("temporary sentinel files left behind: %v", matches)
	}
	for _, bad := range []string{"", "relative/dir"} {
		if err := MarkProvisioned(bad); err == nil {
			t.Errorf("MarkProvisioned(%q) should fail", bad)
		}
	}
}
