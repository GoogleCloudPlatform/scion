/*
Copyright 2025 The Scion Authors.
*/
package commands

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVersionCommand(t *testing.T) {
	// Test that the version command exists and has expected properties
	assert.Equal(t, "version", versionCmd.Use)
	assert.NotEmpty(t, versionCmd.Short)
}

func TestVersionCommandExecution(t *testing.T) {
	resetRootCmdState(t)
	// Capture output
	buf := new(bytes.Buffer)
	rootCmd.SetOut(buf)
	rootCmd.SetErr(buf)
	rootCmd.SetArgs([]string{"version"})

	err := rootCmd.Execute()
	require.NoError(t, err)

	output := buf.String()
	assert.Contains(t, output, "sciontool version")
	assert.Contains(t, output, "Commit:")
	assert.Contains(t, output, "Build Time:")
}

func TestGetVersionString(t *testing.T) {
	// Save original values
	origVersion := Version
	origCommit := Commit
	origBuildTime := BuildTime
	defer func() {
		Version = origVersion
		Commit = origCommit
		BuildTime = origBuildTime
	}()

	tests := []struct {
		name         string
		version      string
		commit       string
		buildTime    string
		wantContains []string
	}{
		{
			name:      "with all values set",
			version:   "1.0.0",
			commit:    "abc1234567890",
			buildTime: "2025-01-01T00:00:00Z",
			wantContains: []string{
				"sciontool version 1.0.0",
				"Commit: abc1234",
				"Build Time: 2025-01-01T00:00:00Z",
			},
		},
		{
			name:      "with empty values (fallback to debug info)",
			version:   "",
			commit:    "",
			buildTime: "",
			wantContains: []string{
				"sciontool version",
				"Commit:",
				"Build Time:",
			},
		},
		{
			name:      "with short commit",
			version:   "2.0.0",
			commit:    "abc",
			buildTime: "now",
			wantContains: []string{
				"Commit: abc",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			Version = tt.version
			Commit = tt.commit
			BuildTime = tt.buildTime

			result := getVersionString()

			for _, want := range tt.wantContains {
				assert.Contains(t, result, want)
			}
		})
	}
}

func TestVersionCommandIsRegistered(t *testing.T) {
	// Verify version command is registered with root
	found := false
	for _, cmd := range rootCmd.Commands() {
		if cmd.Name() == "version" {
			found = true
			break
		}
	}
	assert.True(t, found, "version command should be registered with rootCmd")
}
