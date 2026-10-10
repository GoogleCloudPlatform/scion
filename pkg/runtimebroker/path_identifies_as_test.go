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

package runtimebroker

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// pathIdentifiesAs only matches project IDs that have the project ID format.
func TestPathIdentifiesAs_RequiresWellFormedProjectID(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	configs := filepath.Join(home, config.GlobalDir, config.ProjectConfigsDir)
	valid := config.ProjectMarker{ProjectID: "22222222-bbbb-bbbb-bbbb-222222222222", ProjectSlug: "proj"}
	validDir := filepath.Join(configs, valid.DirName(), config.DotScion)
	placeholderDir := filepath.Join(configs, (config.ProjectMarker{ProjectID: "a/b", ProjectSlug: "proj"}).DirName(), config.DotScion)
	for _, d := range []string{validDir, placeholderDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	if !pathIdentifiesAs(validDir, valid.ProjectID) {
		t.Errorf("pathIdentifiesAs(%q, %q) = false, want true", validDir, valid.ProjectID)
	}
	for _, id := range []string{"a/b", "..", "../other", "x y"} {
		if pathIdentifiesAs(placeholderDir, id) {
			t.Errorf("pathIdentifiesAs(%q, %q) = true, want false", placeholderDir, id)
		}
	}
}
