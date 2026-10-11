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
	shortenServerForceWaits(t, 200*time.Millisecond, 5*time.Second)
	child, reap := startIgnoringInterruptChild(t)
	pid := child.Process.Pid
	require.NoError(t, daemon.WritePIDComponent(serverDaemonComponent, globalDir, pid))

	// After the grace period, kill the child as the port kill would, but
	// reap it only later, leaving a window where it is killed yet unreaped.
	go func() {
		time.Sleep(400 * time.Millisecond)
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		time.Sleep(500 * time.Millisecond)
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
