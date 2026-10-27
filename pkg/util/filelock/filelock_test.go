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

package filelock

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAcquire_CreatesExcludesAndKeepsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	l, err := Acquire(path, 0600, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !l.Created {
		t.Error("Created = false for a new lock file")
	}
	if fi, err := os.Stat(path); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("lock file: %v %v", fi, err)
	}

	// A second Acquire on the same path, even in this process, waits for
	// its deadline and fails: the lock is not re-entrant.
	start := time.Now()
	if _, err := Acquire(path, 0600, 100*time.Millisecond); !errors.Is(err, ErrTimeout) {
		t.Fatalf("second Acquire: err = %v, want ErrTimeout", err)
	}
	if d := time.Since(start); d < 90*time.Millisecond || d > 5*time.Second {
		t.Errorf("waited %s, want about 100ms", d)
	}

	l.Unlock()
	l.Unlock() // idempotent
	l2, err := Acquire(path, 0600, time.Second)
	if err != nil {
		t.Fatalf("Acquire after Unlock: %v", err)
	}
	if l2.Created {
		t.Error("Created = true for an existing lock file")
	}
	l2.Unlock()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("lock file removed after Unlock: %v", err)
	}
}

func TestAcquire_WaitsForRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	l, err := Acquire(path, 0600, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		l.Unlock()
	}()
	l2, err := Acquire(path, 0600, 5*time.Second)
	if err != nil {
		t.Fatalf("Acquire while the holder releases: %v", err)
	}
	l2.Unlock()
}

func TestAcquire_RefusesSymlinkAndDirectory(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, nil, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.lock")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(link, 0600, 0); !errors.Is(err, ErrNotRegular) {
		t.Errorf("symlink: err = %v, want ErrNotRegular", err)
	}
	sub := filepath.Join(dir, "sub.lock")
	if err := os.Mkdir(sub, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(sub, 0600, 0); err == nil {
		t.Error("directory: Acquire succeeded")
	}
}

// An existing lock file this user may not write (another user's, or mode
// 0444) is opened read-only and still excludes other holders.
func TestAcquire_ReadOnlyLockFileStillLocks(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens any file for writing")
	}
	path := filepath.Join(t.TempDir(), "x.lock")
	if err := os.WriteFile(path, nil, 0444); err != nil {
		t.Fatal(err)
	}
	l, err := Acquire(path, 0600, time.Second)
	if err != nil {
		t.Fatalf("Acquire of a read-only lock file: %v", err)
	}
	defer l.Unlock()
	if l.Created {
		t.Error("Created = true for an existing lock file")
	}
	if _, err := Acquire(path, 0600, 50*time.Millisecond); !errors.Is(err, ErrTimeout) {
		t.Errorf("second Acquire: err = %v, want ErrTimeout (the read-only lock must exclude)", err)
	}
}

// helperEnv names the lock file a helper subprocess acquires.
const helperEnv = "SCION_FILELOCK_TEST_HELPER"

// TestHelperProcess is not a test: run as a subprocess with helperEnv set,
// it makes one Acquire attempt on that path and prints "locked" or
// "timeout". With mode "hold" it then waits until its stdin closes or it is
// killed, without unlocking. Without helperEnv it returns at once.
func TestHelperProcess(t *testing.T) {
	path := os.Getenv(helperEnv)
	if path == "" {
		return
	}
	l, err := Acquire(path, 0600, 0)
	switch {
	case errors.Is(err, ErrTimeout):
		fmt.Println("timeout")
		os.Exit(0)
	case err != nil:
		fmt.Println("error:", err)
		os.Exit(1)
	}
	fmt.Println("locked")
	if os.Getenv(helperEnv+"_MODE") == "hold" {
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0) // exits holding the lock: the kernel must release it
	}
	l.Unlock()
	os.Exit(0)
}

func helperCmd(path, mode string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$", "-test.count=1")
	cmd.Env = append(os.Environ(), helperEnv+"="+path, helperEnv+"_MODE="+mode)
	return cmd
}

func runHelper(t *testing.T, path string) string {
	t.Helper()
	out, err := helperCmd(path, "").Output()
	if err != nil {
		t.Fatalf("helper: %v: %s", err, out)
	}
	return strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
}

func TestAcquire_ExcludesSecondProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	l, err := Acquire(path, 0600, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got := runHelper(t, path); got != "timeout" {
		t.Fatalf("second process while held: %q, want timeout", got)
	}
	l.Unlock()
	// Control: once released, the other process gets the lock.
	if got := runHelper(t, path); got != "locked" {
		t.Fatalf("second process after Unlock: %q, want locked", got)
	}
}

func TestAcquire_ReleasedOnProcessExit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	cmd := helperCmd(path, "hold")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stdin.Close(); _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "locked" {
		t.Fatalf("helper: %q, %v", line, err)
	}
	if _, err := Acquire(path, 0600, 0); !errors.Is(err, ErrTimeout) {
		t.Fatalf("Acquire while another process holds the lock: %v, want ErrTimeout", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	l, err := Acquire(path, 0600, 5*time.Second)
	if err != nil {
		t.Fatalf("Acquire after the holder was killed: %v", err)
	}
	l.Unlock()
}

func TestAcquire_TimeoutWrapsErrTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	l, err := Acquire(path, 0600, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Acquire(path, 0600, 30*time.Millisecond)
	if !errors.Is(err, ErrTimeout) || !strings.Contains(err.Error(), path) {
		t.Fatalf("Acquire of a held lock: %v, want ErrTimeout naming the path", err)
	}
	l.Unlock()
	// Control: the same call succeeds once the lock is free.
	l, err = Acquire(path, 0600, 30*time.Millisecond)
	if err != nil {
		t.Fatalf("Acquire of a free lock: %v", err)
	}
	l.Unlock()
}

func TestLockFile_ExcludesAndUnlockCloses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	f1, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	l, err := LockFile(f1, 0)
	if err != nil {
		t.Fatalf("LockFile: %v", err)
	}
	f2, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f2.Close() }()
	if _, err := LockFile(f2, 20*time.Millisecond); !errors.Is(err, ErrTimeout) {
		t.Fatalf("LockFile on a second handle: %v, want ErrTimeout", err)
	}
	l.Unlock()
	if _, err := f1.Stat(); err == nil {
		t.Error("Unlock left the locked file open")
	}
	// Control: the second handle (still open after its failed attempt)
	// now locks.
	l2, err := LockFile(f2, 0)
	if err != nil {
		t.Fatalf("LockFile after Unlock: %v", err)
	}
	l2.Unlock()
}
