package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ptone/scion-agent/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGroveInitNestedDetection(t *testing.T) {
	// Save and restore working directory
	origWd, err := os.Getwd()
	require.NoError(t, err)
	defer os.Chdir(origWd)

	// Save and restore HOME
	origHome := os.Getenv("HOME")
	defer os.Setenv("HOME", origHome)

	t.Run("blocks nested grove inside project", func(t *testing.T) {
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

		// Simulate the nested grove check logic
		grovePath, rootDir, found := config.GetEnclosingGrovePath()
		assert.True(t, found, "should find enclosing grove")

		wd, _ := os.Getwd()
		if filepath.Clean(wd) == filepath.Clean(rootDir) {
			t.Skip("same directory - would block as re-init")
		}

		// Check if this is the global grove
		globalDir, err := config.GetGlobalDir()
		assert.NoError(t, err)

		// Should NOT match global - this is a project grove
		assert.NotEqual(t, filepath.Clean(grovePath), filepath.Clean(globalDir),
			"nested project grove should NOT be treated as global")
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
		grovePath, rootDir, found := config.GetEnclosingGrovePath()
		assert.True(t, found, "should find global grove")

		evalTmpHome, _ := filepath.EvalSymlinks(tmpHome)
		assert.Equal(t, evalTmpHome, rootDir, "rootDir should be home directory")

		// Check if this is the global grove
		globalDir, err := config.GetGlobalDir()
		assert.NoError(t, err)

		// grovePath should equal globalDir
		evalGrovePath, _ := filepath.EvalSymlinks(grovePath)
		evalGlobalDir, _ := filepath.EvalSymlinks(globalDir)
		assert.Equal(t, evalGrovePath, evalGlobalDir,
			"found grove should be the global grove - initialization should proceed")
	})

	t.Run("blocks nested grove inside non-global project", func(t *testing.T) {
		// Create temp HOME without global grove
		tmpHome := t.TempDir()
		os.Setenv("HOME", tmpHome)

		// Create a project with .scion that is NOT the global grove
		projectDir := filepath.Join(tmpHome, "projects", "existing-project")
		require.NoError(t, os.MkdirAll(projectDir, 0755))
		scionDir := filepath.Join(projectDir, ".scion")
		require.NoError(t, os.Mkdir(scionDir, 0755))

		// Try to init from a subdirectory
		subDir := filepath.Join(projectDir, "packages", "sub-package")
		require.NoError(t, os.MkdirAll(subDir, 0755))
		require.NoError(t, os.Chdir(subDir))

		// The enclosing grove check will find the project's .scion
		grovePath, _, found := config.GetEnclosingGrovePath()
		assert.True(t, found, "should find enclosing project grove")

		// Check if this is the global grove
		globalDir, err := config.GetGlobalDir()
		assert.NoError(t, err)

		// grovePath should NOT equal globalDir - this should be blocked
		assert.NotEqual(t, filepath.Clean(grovePath), filepath.Clean(globalDir),
			"found grove is not global - should block nested initialization")
	})
}
