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

package runtimebroker

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

// TestExecMountChecker_IsMountpoint_NotMounted verifies that IsMountpoint
// returns false when mountpoint exits with code 1 (not a mountpoint).
func TestExecMountChecker_IsMountpoint_NotMounted(t *testing.T) {
	checker := NewExecMountChecker(nil)
	checker.runCommand = func(name string, args ...string) ([]byte, error) {
		if name != "mountpoint" {
			t.Fatalf("unexpected command: %s", name)
		}
		if len(args) != 2 || args[0] != "-q" || args[1] != "/mnt/nfs/ws1" {
			t.Fatalf("unexpected args: %v", args)
		}
		return nil, &exec.ExitError{}
	}

	mounted, err := checker.IsMountpoint("/mnt/nfs/ws1")
	if err != nil {
		t.Fatalf("IsMountpoint error: %v", err)
	}
	if mounted {
		t.Error("expected not mounted for exit code 1")
	}
}

// TestExecMountChecker_IsMountpoint_Mounted verifies that IsMountpoint
// returns true when mountpoint exits with code 0.
func TestExecMountChecker_IsMountpoint_Mounted(t *testing.T) {
	checker := NewExecMountChecker(nil)
	checker.runCommand = func(name string, args ...string) ([]byte, error) {
		return nil, nil // exit 0 = is a mountpoint
	}

	mounted, err := checker.IsMountpoint("/mnt/nfs/ws1")
	if err != nil {
		t.Fatalf("IsMountpoint error: %v", err)
	}
	if !mounted {
		t.Error("expected mounted for exit code 0")
	}
}

// TestExecMountChecker_Mount_Success verifies the mount command is constructed
// correctly with the right arguments.
func TestExecMountChecker_Mount_Success(t *testing.T) {
	checker := NewExecMountChecker(nil)
	var capturedName string
	var capturedArgs []string
	checker.runCommand = func(name string, args ...string) ([]byte, error) {
		capturedName = name
		capturedArgs = args
		return nil, nil
	}

	err := checker.Mount("10.0.0.2", "/scion-ws", "/mnt/nfs/ws1", "vers=3,hard")
	if err != nil {
		t.Fatalf("Mount error: %v", err)
	}

	if capturedName != "mount" {
		t.Errorf("expected mount command, got %s", capturedName)
	}

	// Expected args: -t nfs -o vers=3,hard 10.0.0.2:/scion-ws /mnt/nfs/ws1
	wantArgs := []string{"-t", "nfs", "-o", "vers=3,hard", "10.0.0.2:/scion-ws", "/mnt/nfs/ws1"}
	if len(capturedArgs) != len(wantArgs) {
		t.Fatalf("args len = %d, want %d: %v", len(capturedArgs), len(wantArgs), capturedArgs)
	}
	for i, want := range wantArgs {
		if capturedArgs[i] != want {
			t.Errorf("arg[%d] = %q, want %q", i, capturedArgs[i], want)
		}
	}
}

// TestExecMountChecker_Mount_Failure verifies mount failure is surfaced.
func TestExecMountChecker_Mount_Failure(t *testing.T) {
	checker := NewExecMountChecker(nil)
	checker.runCommand = func(name string, args ...string) ([]byte, error) {
		return []byte("mount: permission denied"), fmt.Errorf("exit status 32")
	}

	err := checker.Mount("10.0.0.2", "/scion-ws", "/mnt/nfs/ws1", "vers=3,hard")
	if err == nil {
		t.Fatal("expected error from mount failure")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("error should contain mount output, got: %v", err)
	}
}

// TestExecMountChecker_Unmount_Success verifies the umount command.
func TestExecMountChecker_Unmount_Success(t *testing.T) {
	checker := NewExecMountChecker(nil)
	var capturedName string
	var capturedArgs []string
	checker.runCommand = func(name string, args ...string) ([]byte, error) {
		capturedName = name
		capturedArgs = args
		return nil, nil
	}

	err := checker.Unmount("/mnt/nfs/ws1")
	if err != nil {
		t.Fatalf("Unmount error: %v", err)
	}

	if capturedName != "umount" {
		t.Errorf("expected umount command, got %s", capturedName)
	}
	if len(capturedArgs) != 1 || capturedArgs[0] != "/mnt/nfs/ws1" {
		t.Errorf("args = %v, want [/mnt/nfs/ws1]", capturedArgs)
	}
}

// TestExecMountChecker_Interface verifies ExecMountChecker satisfies the
// MountChecker interface at compile time.
func TestExecMountChecker_Interface(t *testing.T) {
	var _ MountChecker = (*ExecMountChecker)(nil)
}
