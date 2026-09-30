/*
Copyright 2025 The Scion Authors.
*/

package procreap

import (
	"bytes"
	"errors"
	"os/exec"
)

// RunManaged is a drop-in replacement for cmd.Run() that registers the
// child's PID with the reaper (see reaper.go) for the lifetime of the call.
// Use this instead of cmd.Run() for any child process started while
// StartReaper is active, so the SIGCHLD reaper does not race Cmd.Wait to
// reap the same PID.
func RunManaged(cmd *exec.Cmd) error {
	var tok *Token
	err := Gated(func() error {
		if err := cmd.Start(); err != nil {
			return err
		}
		tok = RegisterManagedPID(cmd.Process.Pid)
		return nil
	})
	if err != nil {
		return err
	}
	pid := cmd.Process.Pid
	defer UnregisterManagedPID(pid, tok)
	return cmd.Wait()
}

// CombinedOutputManaged is a drop-in replacement for cmd.CombinedOutput()
// with the same reaper-safety as RunManaged.
func CombinedOutputManaged(cmd *exec.Cmd) ([]byte, error) {
	if cmd.Stdout != nil {
		return nil, errors.New("procreap: Stdout already set")
	}
	if cmd.Stderr != nil {
		return nil, errors.New("procreap: Stderr already set")
	}
	var b bytes.Buffer
	cmd.Stdout = &b
	cmd.Stderr = &b
	err := RunManaged(cmd)
	return b.Bytes(), err
}

// OutputManaged is a drop-in replacement for cmd.Output() with the same
// reaper-safety as RunManaged: it captures stdout, and — matching
// exec.Cmd.Output's behavior — attaches captured stderr to the returned
// *exec.ExitError when the caller has not already set cmd.Stderr.
func OutputManaged(cmd *exec.Cmd) ([]byte, error) {
	if cmd.Stdout != nil {
		return nil, errors.New("procreap: Stdout already set")
	}
	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	captureStderr := cmd.Stderr == nil
	var stderr bytes.Buffer
	if captureStderr {
		cmd.Stderr = &stderr
	}

	err := RunManaged(cmd)
	if err != nil && captureStderr {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitErr.Stderr = stderr.Bytes()
		}
	}
	return stdout.Bytes(), err
}
