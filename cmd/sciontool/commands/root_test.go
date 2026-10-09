/*
Copyright 2025 The Scion Authors.
*/
package commands

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging/loglevel"

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

// resetLogLevelState clears leaked SCION_* level variables and the shared
// level state, restoring them when the test ends.
func resetLogLevelState(t *testing.T) {
	t.Helper()
	t.Setenv(loglevel.EnvLogLevel, "")
	t.Setenv(loglevel.EnvDebug, "")
	loglevel.SetWarningOutput(io.Discard)
	loglevel.Reset(true)
	t.Cleanup(func() {
		loglevel.Reset(true)
		loglevel.SetWarningOutput(os.Stderr)
		// --log-level debug turns on sciontool's debug switch; do not
		// leak it into later tests.
		log.SetDebug(false)
	})
}

func TestLogLevelFlagPrecedence(t *testing.T) {
	tests := []struct {
		name        string
		logLevel    string
		debugEnv    string
		args        []string
		wantDefault slog.Level
		wantSource  loglevel.Source
	}{
		{name: "nothing set keeps info", args: []string{"version"}, wantDefault: slog.LevelInfo, wantSource: loglevel.SourceDefault},
		{name: "env used when flag absent", logLevel: "warn", args: []string{"version"}, wantDefault: slog.LevelWarn, wantSource: loglevel.SourceEnv},
		{name: "deprecated SCION_DEBUG when flag absent", debugEnv: "1", args: []string{"version"}, wantDefault: slog.LevelDebug, wantSource: loglevel.SourceEnv},
		{name: "flag warn", args: []string{"--log-level", "warn", "version"}, wantDefault: slog.LevelWarn, wantSource: loglevel.SourceFlag},
		{name: "flag error over env debug", logLevel: "debug", args: []string{"--log-level", "error", "version"}, wantDefault: slog.LevelError, wantSource: loglevel.SourceFlag},
		{name: "flag info over SCION_DEBUG", debugEnv: "1", args: []string{"--log-level", "info", "version"}, wantDefault: slog.LevelInfo, wantSource: loglevel.SourceFlag},
		{name: "invalid flag falls back to info", logLevel: "debug", args: []string{"--log-level", "loud", "version"}, wantDefault: slog.LevelInfo, wantSource: loglevel.SourceFlag},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetRootCmdState(t)
			resetLogLevelState(t)
			t.Setenv(loglevel.EnvLogLevel, tt.logLevel)
			t.Setenv(loglevel.EnvDebug, tt.debugEnv)
			buf := new(bytes.Buffer)
			rootCmd.SetOut(buf)
			rootCmd.SetErr(buf)
			rootCmd.SetArgs(tt.args)
			require.NoError(t, rootCmd.Execute())

			spec, src := loglevel.Current()
			assert.Equal(t, tt.wantDefault, spec.Default)
			assert.Equal(t, tt.wantSource, src)
		})
	}
}

func TestLogLevelFlagPerComponent(t *testing.T) {
	resetRootCmdState(t)
	resetLogLevelState(t)
	rootCmd.SetOut(io.Discard)
	rootCmd.SetErr(io.Discard)
	rootCmd.SetArgs([]string{"--log-level", "warn,hooks=debug", "version"})
	require.NoError(t, rootCmd.Execute())

	assert.Equal(t, slog.LevelWarn, loglevel.Effective(""))
	assert.Equal(t, slog.LevelDebug, loglevel.Effective("hooks"))
}

func TestIsHookInvocation(t *testing.T) {
	tests := []struct {
		args []string
		want bool
	}{
		{[]string{"hook"}, true},
		{[]string{"status", "blocked", "waiting"}, true},
		{[]string{"--log-level", "debug", "status", "task_completed"}, true},
		{[]string{"version"}, false},
		{[]string{"init"}, false},
		{nil, false},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, isHookInvocation(tt.args), "args=%v", tt.args)
	}
}
