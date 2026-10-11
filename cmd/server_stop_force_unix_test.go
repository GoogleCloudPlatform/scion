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

//go:build unix

package cmd

import (
	"bufio"
	"errors"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/daemon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startIgnoringInterruptChild starts a child that ignores SIGINT and blocks.
// The returned function reaps it; the child is SIGKILLed at test cleanup.
func startIgnoringInterruptChild(t *testing.T) (*exec.Cmd, func()) {
	t.Helper()
	cmd := exec.Command("sh", "-c", `trap '' INT; echo ready; while :; do sleep 1; done`)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())

	var once sync.Once
	reap := func() { once.Do(func() { _ = cmd.Wait() }) }
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		reap()
	})

	line, err := bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "ready\n", line)
	return cmd, reap
}

// stubServerProcessIdentity replaces the server identity check for a test.
func stubServerProcessIdentity(t *testing.T, check func(pid int) (bool, error)) {
	t.Helper()
	saved := serverProcessIdentity
	t.Cleanup(func() { serverProcessIdentity = saved })
	serverProcessIdentity = check
}

func shortenServerForceWaits(t *testing.T, grace, exitWait time.Duration) {
	t.Helper()
	savedGrace, savedWait := serverForceStopGrace, serverForceExitWait
	t.Cleanup(func() { serverForceStopGrace, serverForceExitWait = savedGrace, savedWait })
	serverForceStopGrace, serverForceExitWait = grace, exitWait
}

// TestServerStopForce_WaitsForKilledProcessToBeReaped: a process killed
// during the forced stop still answers liveness checks until it is reaped;
// the forced stop must wait for that rather than fail, then remove the PID
// file.
func TestServerStopForce_WaitsForKilledProcessToBeReaped(t *testing.T) {
	globalDir := t.TempDir()
	stubServerProcessIdentity(t, func(int) (bool, error) { return false, nil })
	shortenServerForceWaits(t, 200*time.Millisecond, 5*time.Second)
	child, reap := startIgnoringInterruptChild(t)
	pid := child.Process.Pid
	require.NoError(t, daemon.WritePIDComponent(serverDaemonComponent, globalDir, pid))

	// After the grace period, kill the child as the port kill would, but
	// reap it only later, leaving a window where it is killed yet unreaped.
	// The goroutine stops as soon as the test ends, and cleanup waits for it,
	// so it never signals a process group after the child is gone.
	done, finished := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(done); <-finished })
	go func() {
		defer close(finished)
		select {
		case <-done:
			return
		case <-time.After(400 * time.Millisecond):
		}
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		select {
		case <-done:
			return
		case <-time.After(500 * time.Millisecond):
		}
		reap()
	}()

	var err error
	out := captureStdout(t, func() {
		err = runServerStopForce(globalDir, true, pid, []int{unusedPort(t)})
	})
	require.NoError(t, err)
	assert.Contains(t, out, "Server stopped (forced).")
	assert.NoFileExists(t, daemon.GetPIDPathComponent(serverDaemonComponent, globalDir))
}

// TestServerStopForce_StillRunningKeepsPIDFile: a process that survives the
// forced stop is reported with its PID and a next step, the command fails,
// and the PID file is kept.
func TestServerStopForce_StillRunningKeepsPIDFile(t *testing.T) {
	globalDir := t.TempDir()
	stubServerProcessIdentity(t, func(int) (bool, error) { return false, nil })
	shortenServerForceWaits(t, 200*time.Millisecond, 300*time.Millisecond)
	child, _ := startIgnoringInterruptChild(t)
	pid := child.Process.Pid
	require.NoError(t, daemon.WritePIDComponent(serverDaemonComponent, globalDir, pid))

	var err error
	captureStdout(t, func() {
		err = runServerStopForce(globalDir, true, pid, []int{unusedPort(t)})
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "still running after the forced stop")
	assert.Contains(t, err.Error(), "kill -9 "+strconv.Itoa(pid))

	got, readErr := daemon.ReadPIDComponent(serverDaemonComponent, globalDir)
	require.NoError(t, readErr, "PID file should be kept while the process runs")
	assert.Equal(t, pid, got)
}

// TestServerStopForce_KillsHungServerWithoutPort: a server that ignores the
// interrupt and holds no port is killed through its recorded PID once it is
// confirmed to be the server, and the PID file is removed after it exits.
func TestServerStopForce_KillsHungServerWithoutPort(t *testing.T) {
	globalDir := t.TempDir()
	shortenServerForceWaits(t, 200*time.Millisecond, 5*time.Second)
	child, reap := startIgnoringInterruptChild(t)
	pid := child.Process.Pid
	go reap()
	require.NoError(t, daemon.WritePIDComponent(serverDaemonComponent, globalDir, pid))

	var checked []int
	stubServerProcessIdentity(t, func(p int) (bool, error) {
		checked = append(checked, p)
		return p == pid, nil
	})

	var err error
	out := captureStdout(t, func() {
		err = runServerStopForce(globalDir, true, pid, []int{unusedPort(t)})
	})
	require.NoError(t, err)
	assert.Equal(t, []int{pid}, checked)
	assert.Contains(t, out, "Killed server process "+strconv.Itoa(pid))
	assert.Contains(t, out, "Server stopped (forced).")
	assert.NoFileExists(t, daemon.GetPIDPathComponent(serverDaemonComponent, globalDir))
}

// TestServerStopForce_UnconfirmedPIDNotKilled: a recorded process that fails
// the identity check, or cannot be checked, is not killed; the command fails
// with the manual kill hint and the PID file is kept.
func TestServerStopForce_UnconfirmedPIDNotKilled(t *testing.T) {
	checks := map[string]func(int) (bool, error){
		"not the server": func(int) (bool, error) { return false, nil },
		"unreadable":     func(int) (bool, error) { return false, errors.New("cannot read command line") },
	}
	for name, check := range checks {
		t.Run(name, func(t *testing.T) {
			globalDir := t.TempDir()
			shortenServerForceWaits(t, 200*time.Millisecond, 300*time.Millisecond)
			child, _ := startIgnoringInterruptChild(t)
			pid := child.Process.Pid
			require.NoError(t, daemon.WritePIDComponent(serverDaemonComponent, globalDir, pid))
			stubServerProcessIdentity(t, check)

			var err error
			out := captureStdout(t, func() {
				err = runServerStopForce(globalDir, true, pid, []int{unusedPort(t)})
			})
			require.Error(t, err)
			assert.NotContains(t, out, "Killed server process")
			assert.Contains(t, err.Error(), "still running after the forced stop")
			assert.Contains(t, err.Error(), "kill -9 "+strconv.Itoa(pid))
			require.NoError(t, child.Process.Signal(syscall.Signal(0)), "process must not be killed")

			got, readErr := daemon.ReadPIDComponent(serverDaemonComponent, globalDir)
			require.NoError(t, readErr, "PID file should be kept while the process runs")
			assert.Equal(t, pid, got)
		})
	}
}

// TestServerStopForce_NormalServerUnchanged: a server that exits on the
// interrupt stops gracefully; the identity check and direct kill are not used.
func TestServerStopForce_NormalServerUnchanged(t *testing.T) {
	globalDir := t.TempDir()
	shortenServerForceWaits(t, 5*time.Second, 2*time.Second)
	child := exec.Command("sleep", "30")
	require.NoError(t, child.Start())
	exited := make(chan struct{})
	go func() { _ = child.Wait(); close(exited) }()
	t.Cleanup(func() { _ = child.Process.Kill(); <-exited })
	pid := child.Process.Pid
	require.NoError(t, daemon.WritePIDComponent(serverDaemonComponent, globalDir, pid))
	stubServerProcessIdentity(t, func(int) (bool, error) {
		t.Error("identity check must not run for a server that exits on interrupt")
		return false, nil
	})

	var err error
	out := captureStdout(t, func() {
		err = runServerStopForce(globalDir, true, pid, []int{unusedPort(t)})
	})
	require.NoError(t, err)
	assert.NotContains(t, out, "Killed server process")
	assert.Contains(t, out, "Server stopped (forced).")
	assert.NoFileExists(t, daemon.GetPIDPathComponent(serverDaemonComponent, globalDir))
}

// TestForcedStopStillRunningError: the manual kill hint names a valid PID and
// is left out when no PID is known, since "kill -9 0" would signal the
// caller's whole process group.
func TestForcedStopStillRunningError(t *testing.T) {
	cause := errors.New("daemon process did not exit within 2s")

	err := forcedStopStillRunningError(4242, cause)
	require.ErrorIs(t, err, cause)
	assert.Contains(t, err.Error(), "PID 4242")
	assert.Contains(t, err.Error(), "kill -9 4242")

	for _, pid := range []int{0, -1} {
		err := forcedStopStillRunningError(pid, cause)
		require.ErrorIs(t, err, cause)
		assert.Contains(t, err.Error(), "still running after the forced stop")
		assert.NotContains(t, err.Error(), "kill -9", "pid %d", pid)
		assert.NotContains(t, err.Error(), "PID", "pid %d", pid)
	}
}
