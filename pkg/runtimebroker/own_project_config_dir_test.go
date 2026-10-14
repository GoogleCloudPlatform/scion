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

func TestIsOwnProjectConfigDir(t *testing.T) {
	_, home := newHubFilesTestServer(t)
	repo, dotScion := newHubFilesCheckout(t)
	globalDir := filepath.Join(home, config.GlobalDir)
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	markerParent := t.TempDir()
	markerFile := filepath.Join(markerParent, config.DotScion)
	if err := os.WriteFile(markerFile, []byte("project-id: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		path string
		want bool
	}{
		{"linked checkout .scion dir", dotScion, true},
		{"linked checkout .scion dir with trailing slash", dotScion + string(filepath.Separator), true},
		{"repo root", repo, false},
		{"global dir", globalDir, false},
		{"marker file", markerFile, false},
		{"missing .scion", filepath.Join(t.TempDir(), config.DotScion), false},
	} {
		if got := isOwnProjectConfigDir(tc.path); got != tc.want {
			t.Errorf("%s: isOwnProjectConfigDir(%q) = %v, want %v", tc.name, tc.path, got, tc.want)
		}
	}
}
