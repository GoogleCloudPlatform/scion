/*
Copyright 2026 The Scion Authors.
*/

package commands

import (
	"os"
	"os/exec"
	"testing"
)

// stubHookExit replaces hookExit for the test and returns the recorded exit
// codes.
func stubHookExit(t *testing.T) *[]int {
	t.Helper()
	var codes []int
	old := hookExit
	hookExit = func(code int) { codes = append(codes, code) }
	t.Cleanup(func() { hookExit = old })
	return &codes
}

func TestRecoverHookPanic_ExitsOne(t *testing.T) {
	codes := stubHookExit(t)
	func() {
		defer recoverHookPanic()
		panic("synthetic hook panic")
	}()
	if len(*codes) != 1 || (*codes)[0] != 1 {
		t.Fatalf("exit codes = %v, want [1]", *codes)
	}
}

func TestRecoverHookPanic_NoPanicNoExit(t *testing.T) {
	codes := stubHookExit(t)
	func() {
		defer recoverHookPanic()
	}()
	if len(*codes) != 0 {
		t.Fatalf("exit codes = %v, want none", *codes)
	}
}

// The hook command's Run recovers a panic on its stdin path. A nil
// os.Stdin makes runHookFromStdin panic (Stat returns a nil FileInfo).
func TestHookCommand_PanicExitsOne(t *testing.T) {
	codes := stubHookExit(t)
	oldStdin := os.Stdin
	os.Stdin = nil
	t.Cleanup(func() { os.Stdin = oldStdin })

	hookCmd.Run(hookCmd, nil)

	if len(*codes) != 1 || (*codes)[0] != 1 {
		t.Fatalf("exit codes = %v, want [1]", *codes)
	}
}

// With the real exit, a recovered panic ends the process with code 1, not
// the code 2 of an unrecovered panic.
func TestRecoverHookPanic_ProcessExitCode(t *testing.T) {
	if os.Getenv("SCIONTOOL_HOOK_PANIC_CHILD") == "1" {
		defer recoverHookPanic()
		panic("synthetic hook panic")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestRecoverHookPanic_ProcessExitCode$")
	cmd.Env = append(os.Environ(), "SCIONTOOL_HOOK_PANIC_CHILD=1")
	err := cmd.Run()
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("child run: %v, want an exit error", err)
	}
	if code := exitErr.ExitCode(); code != 1 {
		t.Fatalf("child exit code = %d, want 1", code)
	}
}
