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

func TestRootCommand(t *testing.T) {
	// Test that the root command exists and has expected properties
	assert.Equal(t, "sciontool", rootCmd.Use)
	assert.NotEmpty(t, rootCmd.Short)
	assert.NotEmpty(t, rootCmd.Long)
}

func TestRootCommandHelp(t *testing.T) {
	resetRootCmdState(t)
	// Test that help runs without error
	buf := new(bytes.Buffer)
	rootCmd.SetOut(buf)
	rootCmd.SetErr(buf)
	rootCmd.SetArgs([]string{"--help"})

	err := rootCmd.Execute()
	require.NoError(t, err)

	output := buf.String()
	assert.Contains(t, output, "sciontool")
	assert.Contains(t, output, "init")
	assert.Contains(t, output, "version")
}

func TestLogLevelFlag(t *testing.T) {
	// Verify the log-level flag is registered
	flag := rootCmd.PersistentFlags().Lookup("log-level")
	require.NotNil(t, flag)
	assert.Equal(t, "info", flag.DefValue)
}
