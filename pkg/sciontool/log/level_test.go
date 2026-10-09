/*
Copyright 2026 The Scion Authors.
*/

package log

import (
	"bytes"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/util/logging/loglevel"
)

func readLog(t *testing.T) string {
	t.Helper()
	mu.Lock()
	p := logPath
	mu.Unlock()
	b, err := os.ReadFile(p)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

func TestLevelFromEnvAndFlag(t *testing.T) {
	tests := []struct {
		name      string
		logLevel  string
		debugEnv  string
		flag      string // empty: flag not given
		wantDebug bool
		wantInfo  bool
		wantWarn  bool
		wantError bool
	}{
		{name: "defaults", wantInfo: true, wantWarn: true, wantError: true},
		{name: "env debug", logLevel: "debug", wantDebug: true, wantInfo: true, wantWarn: true, wantError: true},
		{name: "env warn", logLevel: "warn", wantWarn: true, wantError: true},
		{name: "env error", logLevel: "ERROR", wantError: true},
		{name: "invalid env falls back to info", logLevel: "loud", wantInfo: true, wantWarn: true, wantError: true},
		{name: "deprecated SCION_DEBUG", debugEnv: "1", wantDebug: true, wantInfo: true, wantWarn: true, wantError: true},
		{name: "SCION_LOG_LEVEL wins over SCION_DEBUG", logLevel: "warn", debugEnv: "1", wantWarn: true, wantError: true},
		{name: "flag warn", flag: "warn", wantWarn: true, wantError: true},
		{name: "flag error", flag: "error", wantError: true},
		{name: "flag wins over env", logLevel: "error", flag: "debug", wantDebug: true, wantInfo: true, wantWarn: true, wantError: true},
		{name: "flag info wins over SCION_DEBUG", debugEnv: "1", flag: "info", wantInfo: true, wantWarn: true, wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetUninitializedForTest(t)
			t.Setenv(loglevel.EnvLogLevel, tt.logLevel)
			t.Setenv(loglevel.EnvDebug, tt.debugEnv)
			Init()
			if tt.flag != "" {
				if err := ApplyLogLevel(tt.flag); err != nil {
					t.Fatalf("ApplyLogLevel(%q): %v", tt.flag, err)
				}
			}
			Debug("d-line")
			Info("i-line")
			Warn("w-line")
			Error("e-line")
			out := readLog(t)
			for line, want := range map[string]bool{
				"d-line": tt.wantDebug, "i-line": tt.wantInfo,
				"w-line": tt.wantWarn, "e-line": tt.wantError,
			} {
				if got := strings.Contains(out, line); got != want {
					t.Errorf("%s written = %v, want %v\nlog:\n%s", line, got, want, out)
				}
			}
		})
	}
}

func TestApplyLogLevelInvalidFallsBackToInfo(t *testing.T) {
	resetUninitializedForTest(t)
	t.Setenv(loglevel.EnvLogLevel, "debug")
	Init()
	if err := ApplyLogLevel("verbose"); err == nil {
		t.Fatal("expected an error for an invalid level")
	}
	if debug.Load() {
		t.Error("invalid flag should fall back to info, not keep env debug")
	}
}

func TestComponentLevels(t *testing.T) {
	resetUninitializedForTest(t)
	t.Setenv(loglevel.EnvLogLevel, "warn,hooks=debug")
	Init()

	TaggedInfo("hooks", "tagged-hooks")
	TaggedInfo("other", "tagged-other")
	slog.Default().With("subsystem", "hooks").Debug("slog-hooks-debug")
	slog.Default().Debug("slog-default-debug")
	slog.Info("slog-inline", "subsystem", "hooks")
	slog.Info("slog-plain-info")

	out := readLog(t)
	for line, want := range map[string]bool{
		"tagged-hooks":       true,
		"tagged-other":       false,
		"slog-hooks-debug":   true,
		"slog-default-debug": false,
		"slog-inline":        true,
		"slog-plain-info":    false,
	} {
		if got := strings.Contains(out, line); got != want {
			t.Errorf("%s written = %v, want %v\nlog:\n%s", line, got, want, out)
		}
	}
}

func TestDeprecationWarningOncePerProcess(t *testing.T) {
	resetUninitializedForTest(t)
	quiet.Store(false)
	t.Setenv(loglevel.EnvDebug, "1")
	var warn bytes.Buffer
	loglevel.SetWarningOutput(&warn)

	Init()
	Init()
	Debug("first")
	Debug("second")

	if got := strings.Count(warn.String(), "SCION_DEBUG is deprecated"); got != 1 {
		t.Errorf("warning printed %d times, want 1: %q", got, warn.String())
	}
	if strings.Contains(readLog(t), "deprecated") {
		t.Error("deprecation warning must not be written to agent.log")
	}
}

func TestQuietSilencesDeprecationWarning(t *testing.T) {
	resetUninitializedForTest(t)
	t.Setenv(loglevel.EnvDebug, "1")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStderr := os.Stderr
	os.Stderr = w
	// Point the warning output at the captured stderr first, so the test
	// proves SetQuiet is what silences it.
	loglevel.SetWarningOutput(os.Stderr)
	SetQuiet(true)
	Init()
	Debug("quiet-debug")
	_ = w.Close()
	os.Stderr = oldStderr
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	if strings.Contains(buf.String(), "deprecated") {
		t.Errorf("quiet mode leaked the deprecation warning to stderr: %q", buf.String())
	}
	if !strings.Contains(readLog(t), "quiet-debug") {
		t.Error("SCION_DEBUG should still enable debug lines in agent.log")
	}
}
