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

package util

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/util/logging/loglevel"
)

// resetDebugState clears explicit debug state, the SCION_* level variables
// (which the container may export) and the shared level state, restoring
// all of it when the test ends.
func resetDebugState(t *testing.T) {
	t.Helper()
	reset := func() {
		debugMu.Lock()
		debugEnabled = false
		debugInitialized = false
		debugMu.Unlock()
		loglevel.Reset(true)
	}
	t.Setenv(loglevel.EnvLogLevel, "")
	t.Setenv(loglevel.EnvDebug, "")
	loglevel.SetWarningOutput(io.Discard)
	reset()
	t.Cleanup(func() {
		reset()
		loglevel.SetWarningOutput(os.Stderr)
	})
}

func TestDebugEnabled(t *testing.T) {
	tests := []struct {
		name     string
		logLevel string
		debugEnv string
		explicit bool
		want     bool
	}{
		{name: "nothing set", want: false},
		{name: "SCION_LOG_LEVEL=debug", logLevel: "debug", want: true},
		{name: "SCION_LOG_LEVEL=DEBUG case-insensitive", logLevel: "DEBUG", want: true},
		{name: "SCION_LOG_LEVEL=info", logLevel: "info", want: false},
		{name: "SCION_LOG_LEVEL=warn", logLevel: "warn", want: false},
		{name: "component-only debug leaves default off", logLevel: "info,hubsync=debug", want: false},
		{name: "deprecated SCION_DEBUG alias", debugEnv: "1", want: true},
		{name: "SCION_LOG_LEVEL wins over SCION_DEBUG", logLevel: "info", debugEnv: "1", want: false},
		{name: "EnableDebug overrides env", logLevel: "error", explicit: true, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetDebugState(t)
			t.Setenv(loglevel.EnvLogLevel, tt.logLevel)
			t.Setenv(loglevel.EnvDebug, tt.debugEnv)
			if tt.explicit {
				EnableDebug()
			}
			if got := DebugEnabled(); got != tt.want {
				t.Errorf("DebugEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDebugfTaggedComponentLevel(t *testing.T) {
	resetDebugState(t)
	t.Setenv(loglevel.EnvLogLevel, "info,hubsync=debug")

	if !debugEnabledFor("hubsync") {
		t.Error("hubsync tag should be enabled by hubsync=debug")
	}
	if debugEnabledFor("other") {
		t.Error("other tag should follow the info default")
	}

	out := captureStderr(t, func() {
		DebugfTagged("hubsync", "synced %d", 3)
		DebugfTagged("other", "hidden")
		Debugf("hidden too")
	})
	if out != "[hubsync] synced 3\n" {
		t.Errorf("stderr = %q, want only the hubsync line", out)
	}
}

func TestDebugEnvDeprecationWarningOnce(t *testing.T) {
	resetDebugState(t)
	t.Setenv(loglevel.EnvDebug, "1")
	var warn bytes.Buffer
	loglevel.SetWarningOutput(&warn)

	for i := 0; i < 3; i++ {
		if !DebugEnabled() {
			t.Fatal("SCION_DEBUG should still enable debug output")
		}
	}
	if got := strings.Count(warn.String(), "SCION_DEBUG is deprecated"); got != 1 {
		t.Errorf("deprecation warning printed %d times, want 1: %q", got, warn.String())
	}
	if !strings.Contains(warn.String(), "SCION_LOG_LEVEL") {
		t.Errorf("deprecation warning should name SCION_LOG_LEVEL: %q", warn.String())
	}
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	oldStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	fn()
	_ = w.Close()
	os.Stderr = oldStderr
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	return buf.String()
}

func TestDebugf(t *testing.T) {
	resetDebugState(t)

	// Capture stderr
	oldStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	// Test: No output when debug disabled
	Debugf("test message %d", 42)

	_ = w.Close()
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	os.Stderr = oldStderr

	if buf.String() != "" {
		t.Errorf("Debugf should not output when debug is disabled, got: %s", buf.String())
	}

	// Test: Output when debug enabled
	r, w, _ = os.Pipe()
	os.Stderr = w

	EnableDebug()
	Debugf("test message %d", 42)

	_ = w.Close()
	buf.Reset()
	_, _ = io.Copy(&buf, r)
	os.Stderr = oldStderr

	output := buf.String()
	if !strings.HasSuffix(output, " [DEBUG] test message 42\n") {
		t.Errorf("Debugf output = %q, want suffix %q", output, " [DEBUG] test message 42\n")
	}
	// Verify timestamp prefix format (HH:MM:SS.mmm)
	if len(output) < 13 || output[2] != ':' || output[5] != ':' || output[8] != '.' {
		t.Errorf("Debugf output missing timestamp prefix, got: %q", output)
	}

	// Cleanup
	debugMu.Lock()
	debugEnabled = false
	debugInitialized = false
	debugMu.Unlock()
}

func TestDebugfTagged(t *testing.T) {
	resetDebugState(t)

	// Capture stderr
	oldStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	EnableDebug()
	DebugfTagged("mytag", "test %s", "value")

	_ = w.Close()
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	os.Stderr = oldStderr

	expected := "[mytag] test value\n"
	if buf.String() != expected {
		t.Errorf("DebugfTagged output = %q, want %q", buf.String(), expected)
	}

	// Cleanup
	debugMu.Lock()
	debugEnabled = false
	debugInitialized = false
	debugMu.Unlock()
}
