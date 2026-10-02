/*
Copyright 2025 The Scion Authors.
*/

package commands

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

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

// TestReadHarnessExitCode_SymlinkRefused proves that a workload-planted
// symlink at the exit-code path can't make root's shutdown path read (and
// parse as an exit code) an unrelated file's contents: readHarnessExitCode
// must return nil without touching the symlink's target.
func TestReadHarnessExitCode_SymlinkRefused(t *testing.T) {
	target := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(target, []byte("42"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}

	_ = os.Remove(state.HarnessExitCodeFile)
	if err := os.Symlink(target, state.HarnessExitCodeFile); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(state.HarnessExitCodeFile) })

	if got := readHarnessExitCode(); got != nil {
		t.Errorf("expected nil for a symlinked exit-code path, got %v", *got)
	}

	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(data) != "42" {
		t.Errorf("symlink target was modified: %q", data)
	}
}

// TestReadHarnessExitCode_FIFODoesNotBlock proves that a workload-planted
// FIFO at the exit-code path can't make root's shutdown path hang forever
// waiting for a writer: readHarnessExitCode must return nil promptly
// (O_NONBLOCK on open, then a regular-file check before ever reading).
func TestReadHarnessExitCode_FIFODoesNotBlock(t *testing.T) {
	_ = os.Remove(state.HarnessExitCodeFile)
	if err := syscall.Mkfifo(state.HarnessExitCodeFile, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(state.HarnessExitCodeFile) })

	done := make(chan *int, 1)
	go func() { done <- readHarnessExitCode() }()

	select {
	case got := <-done:
		if got != nil {
			t.Errorf("expected nil for a FIFO exit-code path, got %v", *got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("readHarnessExitCode blocked on a FIFO with no writer")
	}
}

// TestReadHarnessExitCode_BoundedRead proves an oversized exit-code file is
// bounded by harnessExitCodeMaxBytes rather than read in full. The first
// harnessExitCodeMaxBytes bytes are all-digit and parse to 42 on their own;
// thousands of further digit bytes follow, which would overflow int (and
// so fail to parse) if the read weren't bounded. A bounded read must
// return 42; an unbounded one would return nil.
func TestReadHarnessExitCode_BoundedRead(t *testing.T) {
	_ = os.Remove(state.HarnessExitCodeFile)
	leading := strings.Repeat("0", harnessExitCodeMaxBytes-2) + "42"
	if len(leading) != harnessExitCodeMaxBytes {
		t.Fatalf("test setup: leading prefix is %d bytes, want %d", len(leading), harnessExitCodeMaxBytes)
	}
	oversized := leading + strings.Repeat("9", 1000)
	if _, err := strconv.Atoi(oversized); err == nil {
		t.Fatal("test setup: the full oversized content must not parse as an int (it needs to overflow)")
	}
	if err := os.WriteFile(state.HarnessExitCodeFile, []byte(oversized), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(state.HarnessExitCodeFile) })

	got := readHarnessExitCode()
	if got == nil || *got != 42 {
		t.Errorf("readHarnessExitCode() = %v, want 42 (derived from only the first %d bytes)", got, harnessExitCodeMaxBytes)
	}
}
