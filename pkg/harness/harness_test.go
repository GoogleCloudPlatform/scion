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
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/stretchr/testify/assert"
)

func TestNew_EmbedFSHarnesses(t *testing.T) {
	h := New("claude")
	assert.Equal(t, "claude", h.Name())

	h = New("gemini-cli")
	assert.Equal(t, "gemini-cli", h.Name())
}

func TestNew_UnknownFallsToGeneric(t *testing.T) {
	h := New("unknown-harness")
	assert.Equal(t, "generic", h.Name())
}

func TestEmbedOnlyHarnesses_ReturnsEmpty(t *testing.T) {
	all := EmbedOnlyHarnesses()
	assert.Empty(t, all)
}

func TestAllHarnessNames_IncludesAll(t *testing.T) {
	names := AllHarnessNames()
	assert.Contains(t, names, "claude")
	assert.Contains(t, names, "gemini-cli")
	assert.Contains(t, names, "codex")
	assert.Contains(t, names, "opencode")
	assert.Contains(t, names, "antigravity")
	assert.Contains(t, names, "copilot")
	assert.Contains(t, names, "hermes")
}

// TestAllHarnessNames_MatchesDisk asserts that AllHarnessNames() (derived from
// the hand-maintained //go:embed directive in harnesses/embed.go) matches the
// real harnesses/ directory on disk. If a new harness directory is added under
// harnesses/ but not listed in the embed directive, this test fails and names
// the missing entry — making the embed line self-policing.
func TestAllHarnessNames_MatchesDisk(t *testing.T) {
	root, err := util.RepoRoot()
	if err != nil {
		t.Skipf("skipping: could not locate repo root: %v", err)
	}

	harnessesDir := filepath.Join(root, "harnesses")

	entries, err := os.ReadDir(harnessesDir)
	if err != nil {
		t.Fatalf("failed to read harnesses/ directory: %v", err)
	}

	// Collect directories that contain a config.yaml — these are real harness
	// bundles. Directories without config.yaml (e.g. gen/) are not harnesses.
	var diskNames []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		configPath := filepath.Join(harnessesDir, e.Name(), "config.yaml")
		if _, err := os.Stat(configPath); err == nil {
			diskNames = append(diskNames, e.Name())
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unexpected error checking %s: %v", configPath, err)
		}
	}
	sort.Strings(diskNames)

	embedNames := AllHarnessNames()

	// Check for harnesses on disk but missing from the embed.
	for _, name := range diskNames {
		assert.Contains(t, embedNames, name,
			"harness %q exists on disk (harnesses/%s/config.yaml) but is missing from "+
				"AllHarnessNames(); add it to the //go:embed directive in harnesses/embed.go", name, name)
	}

	// Check for harnesses in the embed but missing from disk.
	for _, name := range embedNames {
		assert.Contains(t, diskNames, name,
			"harness %q is in AllHarnessNames() (via embed) but has no directory with "+
				"config.yaml under harnesses/", name)
	}
}
