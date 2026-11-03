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
	"os"
	"path/filepath"
	"testing"
)

// fsSnapshot records every path under roots, for the fail-closed
// "nothing created on disk" check on a refused start.
func fsSnapshot(t *testing.T, roots ...string) map[string]bool {
	t.Helper()
	m := map[string]bool{}
	for _, r := range roots {
		_ = filepath.Walk(r, func(p string, _ os.FileInfo, err error) error {
			if err == nil {
				m[p] = true
			}
			return nil
		})
	}
	return m
}

// assertNothingCreated fails if any path under roots is new since before.
func assertNothingCreated(t *testing.T, before map[string]bool, roots ...string) {
	t.Helper()
	for p := range fsSnapshot(t, roots...) {
		if !before[p] {
			t.Errorf("refused start created %s on disk", p)
		}
	}
}
