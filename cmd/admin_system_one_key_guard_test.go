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

package cmd

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSystemOneLiveBootstrapKeyVarOnlyInLiveTest enforces §8 L4: the bootstrap
// live-probe environment variable name (constructed here via concatenation so
// this guard file itself does not match) may appear ONLY in the manual
// systemone_live test file and nowhere in product code, scripts, docs, or other
// tests.
func TestSystemOneLiveBootstrapKeyVarOnlyInLiveTest(t *testing.T) {
	needle := []byte("JEV" + "KEY")
	const allowedRelPath = "cmd/admin_system_one_live_test.go"
	const maxScanFileBytes = 4 * 1024 * 1024

	repoRoot := findScionRepoRoot(t)

	skipDirs := map[string]bool{
		".git":           true,
		".scion":         true,
		".scion-volumes": true,
		"node_modules":   true,
		"vendor":         true,
		"build":          true,
		"dist":           true,
		".astro":         true,
	}

	var matches []string
	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() > maxScanFileBytes {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, needle) {
			rel, err := filepath.Rel(repoRoot, path)
			if err != nil {
				return err
			}
			matches = append(matches, filepath.ToSlash(rel))
		}
		return nil
	})
	require.NoError(t, err)

	sort.Strings(matches)
	assert.Equal(t, []string{allowedRelPath}, matches,
		"the bootstrap live-probe env var literal must appear only in %s", allowedRelPath)
}

func findScionRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		gomod := filepath.Join(dir, "go.mod")
		if data, err := os.ReadFile(gomod); err == nil {
			if strings.Contains(string(data), "module github.com/GoogleCloudPlatform/scion\n") ||
				strings.Contains(string(data), "module github.com/GoogleCloudPlatform/scion\r\n") {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not locate scion repository root above %s", dir)
		}
		dir = parent
	}
}
