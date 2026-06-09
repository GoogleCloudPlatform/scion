/*
Copyright 2025 The Scion Authors.
*/

package commands

import (
	"errors"
	"os"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/handlers"
)

func intPtr(i int) *int { return &i }

func TestClassifyExit(t *testing.T) {
	tests := []struct {
		name              string
		supervisedCode    int
		supervisorErr     error
		harnessCode       *int
		limitsExceeded    bool
		requestedShutdown bool
		wantCode          int
		wantCrash         bool
		wantLimits        bool
		wantMsg           string
	}{
		{
			name:           "clean exit code 0",
			supervisedCode: 0,
			wantCode:       0,
			wantCrash:      false,
		},
		{
			name:           "harness file reports non-zero while supervised child is 0 -> crash",
			supervisedCode: 0,
			harnessCode:    intPtr(42),
			wantCode:       42,
			wantCrash:      true,
			wantMsg:        "Agent crashed with exit code 42",
		},
		{
			name:           "harness file reports 0 while supervised child is 0 -> clean",
			supervisedCode: 0,
			harnessCode:    intPtr(0),
			wantCode:       0,
			wantCrash:      false,
		},
		{
			name:           "no harness file, supervised child non-zero -> crash (SIGKILL fallback)",
			supervisedCode: 137,
			wantCode:       137,
			wantCrash:      true,
			wantMsg:        "Agent crashed with exit code 137",
		},
		{
			name:           "limits exceeded via flag",
			supervisedCode: 0,
			limitsExceeded: true,
			wantCode:       handlers.ExitCodeLimitsExceeded,
			wantLimits:     true,
			wantCrash:      false,
		},
		{
			name:           "limits exceeded via child exit code",
			supervisedCode: handlers.ExitCodeLimitsExceeded,
			wantCode:       handlers.ExitCodeLimitsExceeded,
			wantLimits:     true,
			wantCrash:      false,
		},
		{
			name:           "supervisor error with zero code -> crash code 1",
			supervisedCode: 0,
			supervisorErr:  errors.New("boom"),
			wantCode:       1,
			wantCrash:      true,
			wantMsg:        "Agent crashed (supervisor error: boom)",
		},
		{
			name:           "signal-killed without requested shutdown is crash",
			supervisedCode: -1,
			wantCode:       -1,
			wantCrash:      true,
			wantMsg:        "Agent crashed with exit code -1",
		},
		{
			name:              "signal-killed with requested shutdown is clean stop",
			supervisedCode:    -1,
			requestedShutdown: true,
			wantCode:          0,
			wantCrash:         false,
		},
		{
			name:              "requested shutdown with non-signal exit code is still crash",
			supervisedCode:    1,
			requestedShutdown: true,
			wantCode:          1,
			wantCrash:         true,
			wantMsg:           "Agent crashed with exit code 1",
		},
		{
			name:              "harness code -1 with requested shutdown is clean stop",
			supervisedCode:    0,
			harnessCode:       intPtr(-1),
			requestedShutdown: true,
			wantCode:          0,
			wantCrash:         false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyExit(tc.supervisedCode, tc.supervisorErr, tc.harnessCode, tc.limitsExceeded, tc.requestedShutdown)
			if got.exitCode != tc.wantCode {
				t.Errorf("exitCode = %d, want %d", got.exitCode, tc.wantCode)
			}
			if got.isCrash != tc.wantCrash {
				t.Errorf("isCrash = %v, want %v", got.isCrash, tc.wantCrash)
			}
			if got.limitsExceeded != tc.wantLimits {
				t.Errorf("limitsExceeded = %v, want %v", got.limitsExceeded, tc.wantLimits)
			}
			if tc.wantMsg != "" && got.message != tc.wantMsg {
				t.Errorf("message = %q, want %q", got.message, tc.wantMsg)
			}
			if tc.wantMsg == "" && got.message != "" {
				t.Errorf("message = %q, want empty", got.message)
			}
		})
	}
}

func TestReadHarnessExitCode(t *testing.T) {
	// Missing file -> nil.
	_ = os.Remove(state.HarnessExitCodeFile)
	if got := readHarnessExitCode(); got != nil {
		t.Errorf("expected nil for missing file, got %v", *got)
	}

	// Valid code.
	if err := os.WriteFile(state.HarnessExitCodeFile, []byte("137\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(state.HarnessExitCodeFile) })
	got := readHarnessExitCode()
	if got == nil || *got != 137 {
		t.Errorf("expected 137, got %v", got)
	}

	// Unparseable -> nil.
	if err := os.WriteFile(state.HarnessExitCodeFile, []byte("garbage"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := readHarnessExitCode(); got != nil {
		t.Errorf("expected nil for garbage, got %v", *got)
	}
}
