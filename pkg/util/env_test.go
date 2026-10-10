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

func TestExpandEnv(t *testing.T) {
	t.Setenv("TEST_VAR", "test_value")

	tests := []struct {
		input    string
		expected string
		warn     bool
	}{
		{"Hello ${TEST_VAR}", "Hello test_value", false},
		{"Hello $TEST_VAR", "Hello test_value", false},
		{"Hello ${MISSING_VAR}", "Hello ", true},
		{"No vars here", "No vars here", false},
	}

	for _, tt := range tests {
		// Capture stderr
		r, w, _ := os.Pipe()
		oldStderr := os.Stderr
		os.Stderr = w

		result, warned := ExpandEnv(tt.input)

		_ = w.Close()
		os.Stderr = oldStderr
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		stderrOutput := buf.String()

		if result != tt.expected {
			t.Errorf("ExpandEnv(%q) = %q, want %q", tt.input, result, tt.expected)
		}

		if warned != tt.warn {
			t.Errorf("ExpandEnv(%q) warned = %v, want %v", tt.input, warned, tt.warn)
		}

		if tt.warn {
			if !strings.Contains(stderrOutput, "Warning: environment variable") {
				t.Errorf("ExpandEnv(%q) expected warning in stderr, got none", tt.input)
			}
		} else {
			if stderrOutput != "" {
				t.Errorf("ExpandEnv(%q) unexpected warning in stderr: %s", tt.input, stderrOutput)
			}
		}
	}
}

func TestParseBool(t *testing.T) {
	tests := []struct {
		in        string
		wantValue bool
		wantOK    bool
	}{
		{"", false, false},
		{"   ", false, false},
		{"true", true, true},
		{"TRUE", true, true},
		{"True", true, true},
		{"t", true, true},
		{"T", true, true},
		{"1", true, true},
		{"yes", true, true},
		{"YES", true, true},
		{"y", true, true},
		{"on", true, true},
		{"On", true, true},
		{"true\n", true, true},
		{"  yes  ", true, true},
		{"false", false, true},
		{"FALSE", false, true},
		{"f", false, true},
		{"0", false, true},
		{"no", false, true},
		{"n", false, true},
		{"off", false, true},
		{"OFF\n", false, true},
		{"enabled", false, false},
		{"2", false, false},
		{"tru", false, false},
		{"yess", false, false},
	}
	for _, tt := range tests {
		gotValue, gotOK := ParseBool(tt.in)
		if gotValue != tt.wantValue || gotOK != tt.wantOK {
			t.Errorf("ParseBool(%q) = (%v, %v), want (%v, %v)", tt.in, gotValue, gotOK, tt.wantValue, tt.wantOK)
		}
	}
}

func TestParseBoolEnv(t *testing.T) {
	const key = "SCION_TEST_PARSE_BOOL_ENV"
	tests := []struct {
		name       string
		set        bool
		value      string
		defaultVal bool
		want       bool
	}{
		{"unset default false", false, "", false, false},
		{"unset default true", false, "", true, true},
		{"empty default true", true, "", true, true},
		{"whitespace default true", true, "  ", true, true},
		{"true default false", true, "true", false, true},
		{"yes default false", true, "Yes", false, true},
		{"1 default false", true, "1", false, true},
		{"false default true", true, "false", true, false},
		{"off default true", true, "off", true, false},
		{"0 default true", true, "0", true, false},
		{"garbage default true", true, "enabled", true, true},
		{"garbage default false", true, "enabled", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.set {
				t.Setenv(key, tt.value)
			} else {
				t.Setenv(key, "")
				_ = os.Unsetenv(key)
			}
			if got := ParseBoolEnv(key, tt.defaultVal); got != tt.want {
				t.Errorf("ParseBoolEnv(%q=%q, %v) = %v, want %v", key, tt.value, tt.defaultVal, got, tt.want)
			}
		})
	}
}

func TestLookupBoolEnv(t *testing.T) {
	const key = "SCION_TEST_LOOKUP_BOOL_ENV"
	t.Setenv(key, "")
	_ = os.Unsetenv(key)
	if v, ok := LookupBoolEnv(key); v || ok {
		t.Errorf("unset: got (%v, %v), want (false, false)", v, ok)
	}
	t.Setenv(key, "on")
	if v, ok := LookupBoolEnv(key); !v || !ok {
		t.Errorf("on: got (%v, %v), want (true, true)", v, ok)
	}
	t.Setenv(key, "nope")
	if v, ok := LookupBoolEnv(key); v || ok {
		t.Errorf("garbage: got (%v, %v), want (false, false)", v, ok)
	}
}
