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
)

func TestDebugEnabled(t *testing.T) {
	// Reset state for testing
	debugMu.Lock()
	debugEnabled = false
	debugMu.Unlock()

	// Clean environment
	_ = os.Unsetenv("SCION_DEBUG")

	// Test 1: No debug when not set
	if DebugEnabled() {
		t.Error("DebugEnabled should return false when not enabled")
	}

	// Test 2: Debug via environment variable
	_ = os.Setenv("SCION_DEBUG", "1")
	if !DebugEnabled() {
		t.Error("DebugEnabled should return true when SCION_DEBUG is set")
	}
	_ = os.Unsetenv("SCION_DEBUG")

	// Test 3: Debug via EnableDebug()
	debugMu.Lock()
	debugEnabled = false
	debugMu.Unlock()

	EnableDebug()
	if !DebugEnabled() {
		t.Error("DebugEnabled should return true after EnableDebug()")
	}

	// Test 4: EnableDebug() overrides environment
	_ = os.Unsetenv("SCION_DEBUG")
	if !DebugEnabled() {
		t.Error("DebugEnabled should remain true after EnableDebug() even without env var")
	}

	// Cleanup
	debugMu.Lock()
	debugEnabled = false
	debugMu.Unlock()
}

func TestDebugf(t *testing.T) {
	// Reset state
	debugMu.Lock()
	debugEnabled = false
	debugMu.Unlock()

	// Capture stderr
	oldStderr := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	// Test: No output when debug disabled
	_ = os.Unsetenv("SCION_DEBUG")
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
	debugMu.Unlock()
}

func TestDebugfTagged(t *testing.T) {
	// Reset state
	debugMu.Lock()
	debugEnabled = false
	debugMu.Unlock()

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
	debugMu.Unlock()
}

// resetDebugState clears explicit debug and the agent policy for a test.
func resetDebugState(t *testing.T) {
	t.Helper()
	reset := func() {
		SetExplicitDebug(false)
		SetAgentDebugPolicy(false)
	}
	reset()
	t.Cleanup(reset)
}

func TestDebugEnabled_AgentPolicy(t *testing.T) {
	tests := []struct {
		name        string
		agentPolicy bool
		explicit    bool
		scionDebug  string
		logLevel    string
		want        bool
	}{
		{name: "outside agent, SCION_DEBUG set", scionDebug: "1", want: true},
		{name: "outside agent, nothing set", want: false},
		{name: "agent, inherited SCION_DEBUG ignored", agentPolicy: true, scionDebug: "1", want: false},
		{name: "agent, SCION_LOG_LEVEL=debug", agentPolicy: true, logLevel: "debug", want: true},
		{name: "agent, SCION_LOG_LEVEL=info", agentPolicy: true, scionDebug: "1", logLevel: "info", want: false},
		{name: "agent, explicit debug", agentPolicy: true, explicit: true, scionDebug: "1", want: true},
		{name: "agent, nothing set", agentPolicy: true, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetDebugState(t)
			t.Setenv("SCION_DEBUG", tt.scionDebug)
			t.Setenv("SCION_LOG_LEVEL", tt.logLevel)
			SetAgentDebugPolicy(tt.agentPolicy)
			SetExplicitDebug(tt.explicit)
			if got := DebugEnabled(); got != tt.want {
				t.Errorf("DebugEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSetExplicitDebug_FalseFallsBackToEnv(t *testing.T) {
	resetDebugState(t)
	t.Setenv("SCION_DEBUG", "")
	EnableDebug()
	SetExplicitDebug(false)
	if DebugEnabled() {
		t.Error("DebugEnabled() = true after SetExplicitDebug(false) with no env set")
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

func TestDebugfTagged_AgentPolicyQuietWithInheritedSCIONDebug(t *testing.T) {
	resetDebugState(t)
	t.Setenv("SCION_DEBUG", "1")
	t.Setenv("SCION_LOG_LEVEL", "")
	SetAgentDebugPolicy(true)

	if out := captureStderr(t, func() {
		DebugfTagged("hubsync", "probe %d", 1)
		Debugf("probe %d", 2)
	}); out != "" {
		t.Errorf("expected no debug output in agent mode with inherited SCION_DEBUG, got %q", out)
	}

	t.Setenv("SCION_LOG_LEVEL", "debug")
	if out := captureStderr(t, func() {
		DebugfTagged("hubsync", "probe %d", 3)
	}); out != "[hubsync] probe 3\n" {
		t.Errorf("DebugfTagged with SCION_LOG_LEVEL=debug = %q, want %q", out, "[hubsync] probe 3\n")
	}
}
