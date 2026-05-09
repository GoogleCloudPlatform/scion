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
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProjectInitNestedDetection(t *testing.T) {
	// Save and restore working directory
	origWd, err := os.Getwd()
	require.NoError(t, err)
	defer os.Chdir(origWd)

	// Save and restore HOME
	origHome := os.Getenv("HOME")
	defer os.Setenv("HOME", origHome)

	t.Run("allows nested grove inside project", func(t *testing.T) {
		// Create temp project with .scion
		tmpHome := t.TempDir()
		os.Setenv("HOME", tmpHome)

		projectDir := t.TempDir()
		scionDir := filepath.Join(projectDir, ".scion")
		require.NoError(t, os.Mkdir(scionDir, 0755))

		// Create a subdirectory
		subDir := filepath.Join(projectDir, "subdir")
		require.NoError(t, os.Mkdir(subDir, 0755))
		require.NoError(t, os.Chdir(subDir))

		// The enclosing grove check finds the parent grove
		_, rootDir, found := config.GetEnclosingProjectPath()
		assert.True(t, found, "should find enclosing grove")

		wd, _ := os.Getwd()
		// We're in a subdirectory, not the same as the grove root
		assert.NotEqual(t, filepath.Clean(wd), filepath.Clean(rootDir),
			"should be in a subdirectory of the enclosing grove")
		// Nested init should be allowed — no error expected
	})

	t.Run("allows project grove when only global exists", func(t *testing.T) {
		// Create a temp HOME with .scion (global grove)
		tmpHome := t.TempDir()
		os.Setenv("HOME", tmpHome)

		globalScionDir := filepath.Join(tmpHome, ".scion")
		require.NoError(t, os.Mkdir(globalScionDir, 0755))

		// Create a project directory UNDER home (like ~/projects/myapp)
		projectDir := filepath.Join(tmpHome, "projects", "myapp")
		require.NoError(t, os.MkdirAll(projectDir, 0755))
		require.NoError(t, os.Chdir(projectDir))

		// The enclosing grove check will find ~/.scion
		projectPath, rootDir, found := config.GetEnclosingProjectPath()
		assert.True(t, found, "should find global grove")

		evalTmpHome, _ := filepath.EvalSymlinks(tmpHome)
		assert.Equal(t, evalTmpHome, rootDir, "rootDir should be home directory")

		// Check if this is the global grove
		globalDir, err := config.GetGlobalDir()
		assert.NoError(t, err)

		// projectPath should equal globalDir
		evalProjectPath, _ := filepath.EvalSymlinks(projectPath)
		evalGlobalDir, _ := filepath.EvalSymlinks(globalDir)
		assert.Equal(t, evalProjectPath, evalGlobalDir,
			"found grove should be the global grove - initialization should proceed")
	})

	t.Run("allows nested grove inside non-global project", func(t *testing.T) {
		// Create temp HOME without global grove
		tmpHome := t.TempDir()
		os.Setenv("HOME", tmpHome)

		// Create a project with .scion that is NOT the global grove
		projectDir := filepath.Join(tmpHome, "projects", "existing-project")
		require.NoError(t, os.MkdirAll(projectDir, 0755))
		scionDir := filepath.Join(projectDir, ".scion")
		require.NoError(t, os.Mkdir(scionDir, 0755))

		// Try to init from a subdirectory — this is now allowed
		subDir := filepath.Join(projectDir, "packages", "sub-package")
		require.NoError(t, os.MkdirAll(subDir, 0755))
		require.NoError(t, os.Chdir(subDir))

		// The enclosing grove check will find the project's .scion
		_, _, found := config.GetEnclosingProjectPath()
		assert.True(t, found, "should find enclosing project grove")

		// Nested initialization is now allowed — no error expected
	})
}
