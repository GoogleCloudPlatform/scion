/*
Copyright 2025 The Scion Authors.
*/

package procreap

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

func TestRunManaged_Success(t *testing.T) {
	cmd := exec.Command("true")
	if err := RunManaged(cmd); err != nil {
		t.Fatalf("RunManaged(true) = %v, want nil", err)
	}
}

func TestRunManaged_Failure(t *testing.T) {
	cmd := exec.Command("false")
	if err := RunManaged(cmd); err == nil {
		t.Fatal("RunManaged(false) = nil, want an error")
	}
}

func TestRunManaged_UnregistersAfterCompletion(t *testing.T) {
	cmd := exec.Command("true")
	if err := RunManaged(cmd); err != nil {
		t.Fatalf("RunManaged(true) = %v, want nil", err)
	}
	if isManagedPID(cmd.Process.Pid) {
		t.Fatal("pid should be unregistered once RunManaged returns")
	}
}

func TestCombinedOutputManaged(t *testing.T) {
	cmd := exec.Command("sh", "-c", "echo out; echo err >&2")
	out, err := CombinedOutputManaged(cmd)
	if err != nil {
		t.Fatalf("CombinedOutputManaged = %v", err)
	}
	if !strings.Contains(string(out), "out") || !strings.Contains(string(out), "err") {
		t.Fatalf("expected combined stdout+stderr, got %q", out)
	}
}

func TestCombinedOutputManaged_RejectsPresetStdout(t *testing.T) {
	cmd := exec.Command("true")
	cmd.Stdout = &strings.Builder{}
	if _, err := CombinedOutputManaged(cmd); err == nil {
		t.Fatal("expected error when Stdout is already set")
	}
}

func TestOutputManaged(t *testing.T) {
	cmd := exec.Command("echo", "-n", "hello")
	out, err := OutputManaged(cmd)
	if err != nil {
		t.Fatalf("OutputManaged = %v", err)
	}
	if string(out) != "hello" {
		t.Fatalf("OutputManaged output = %q, want %q", out, "hello")
	}
}

func TestOutputManaged_CapturesStderrOnExitError(t *testing.T) {
	cmd := exec.Command("sh", "-c", "echo boom >&2; exit 1")
	_, err := OutputManaged(cmd)
	if err == nil {
		t.Fatal("expected error from non-zero exit")
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected *exec.ExitError, got %T: %v", err, err)
	}
	if !strings.Contains(string(exitErr.Stderr), "boom") {
		t.Fatalf("expected captured stderr to contain 'boom', got %q", exitErr.Stderr)
	}
}
