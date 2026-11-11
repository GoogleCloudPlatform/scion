//go:build unix

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

package daemon

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPIDFileName(t *testing.T) {
	assert.Equal(t, "server.pid", PIDFileName("server"))
	assert.Equal(t, "broker.pid", PIDFileName("broker"))
}

func TestLogFileName(t *testing.T) {
	assert.Equal(t, "server.log", LogFileName("server"))
	assert.Equal(t, "broker.log", LogFileName("broker"))
}

func TestComponentIsolation(t *testing.T) {
	dir := t.TempDir()

	// Write PIDs for different components
	err := WritePIDComponent("server", dir, 11111)
	require.NoError(t, err)
	err = WritePIDComponent("broker", dir, 22222)
	require.NoError(t, err)

	// Read back and verify isolation
	pid, err := ReadPIDComponent("server", dir)
	assert.NoError(t, err)
	assert.Equal(t, 11111, pid)

	pid, err = ReadPIDComponent("broker", dir)
	assert.NoError(t, err)
	assert.Equal(t, 22222, pid)

	// Verify file paths
	assert.FileExists(t, filepath.Join(dir, "server.pid"))
	assert.FileExists(t, filepath.Join(dir, "broker.pid"))

	// Remove one shouldn't affect the other
	err = RemovePIDComponent("server", dir)
	assert.NoError(t, err)

	_, err = ReadPIDComponent("server", dir)
	assert.Error(t, err)

	pid, err = ReadPIDComponent("broker", dir)
	assert.NoError(t, err)
	assert.Equal(t, 22222, pid)
}

func TestGetLogPathComponent(t *testing.T) {
	assert.Contains(t, GetLogPathComponent("server", "/tmp/test"), "server.log")
	assert.Contains(t, GetLogPathComponent("broker", "/tmp/test"), "broker.log")
}

func TestGetPIDPathComponent(t *testing.T) {
	assert.Contains(t, GetPIDPathComponent("server", "/tmp/test"), "server.pid")
	assert.Contains(t, GetPIDPathComponent("broker", "/tmp/test"), "broker.pid")
}

func TestStatusComponent_NoPIDFile(t *testing.T) {
	dir := t.TempDir()

	running, _, err := StatusComponent("server", dir)
	assert.False(t, running)
	assert.ErrorIs(t, err, ErrNotRunning)
}

func TestStatusComponent_StalePID(t *testing.T) {
	dir := t.TempDir()

	err := WritePIDComponent("server", dir, 99999999)
	require.NoError(t, err)

	running, _, err := StatusComponent("server", dir)
	assert.False(t, running)
	assert.ErrorIs(t, err, ErrNotRunning)
}

func TestWaitForExitComponent_AlreadyStopped(t *testing.T) {
	dir := t.TempDir()

	err := WaitForExitComponent("server", dir, 1*time.Second)
	assert.NoError(t, err)
}

func TestWaitForExitComponent_Timeout(t *testing.T) {
	dir := t.TempDir()

	err := WritePIDComponent("server", dir, os.Getpid())
	require.NoError(t, err)

	err = WaitForExitComponent("server", dir, 500*time.Millisecond)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "did not exit")

	_ = RemovePIDComponent("server", dir)
}

// --- Legacy API backward compatibility tests ---

func TestWaitForExit_AlreadyStopped(t *testing.T) {
	dir := t.TempDir()

	err := WaitForExit(dir, 1*time.Second)
	assert.NoError(t, err)
}

func TestWaitForExit_StalePIDFile(t *testing.T) {
	dir := t.TempDir()

	err := WritePID(dir, 99999999)
	require.NoError(t, err)

	err = WaitForExit(dir, 1*time.Second)
	assert.NoError(t, err)
}

func TestWaitForExit_Timeout(t *testing.T) {
	dir := t.TempDir()

	err := WritePID(dir, os.Getpid())
	require.NoError(t, err)

	err = WaitForExit(dir, 500*time.Millisecond)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "did not exit")

	_ = RemovePID(dir)
}

func TestStatus_NoPIDFile(t *testing.T) {
	dir := t.TempDir()

	running, _, err := Status(dir)
	assert.False(t, running)
	assert.ErrorIs(t, err, ErrNotRunning)
}

func TestStatus_StalePID(t *testing.T) {
	dir := t.TempDir()

	err := WritePID(dir, 99999999)
	require.NoError(t, err)

	running, _, err := Status(dir)
	assert.False(t, running)
	assert.ErrorIs(t, err, ErrNotRunning)
}

func TestWriteReadPID(t *testing.T) {
	dir := t.TempDir()

	err := WritePID(dir, 12345)
	require.NoError(t, err)

	pid, err := ReadPID(dir)
	assert.NoError(t, err)
	assert.Equal(t, 12345, pid)
}

func TestRemovePID(t *testing.T) {
	dir := t.TempDir()

	err := WritePID(dir, 12345)
	require.NoError(t, err)

	err = RemovePID(dir)
	assert.NoError(t, err)

	_, err = ReadPID(dir)
	assert.Error(t, err)
}

func TestGetLogPath(t *testing.T) {
	path := GetLogPath("/tmp/test")
	assert.Contains(t, path, "broker.log")
}

func TestGetPIDPath(t *testing.T) {
	path := GetPIDPath("/tmp/test")
	assert.Contains(t, path, "broker.pid")
}

func TestArgsFileName(t *testing.T) {
	assert.Equal(t, "server-args.json", ArgsFileName("server"))
	assert.Equal(t, "broker-args.json", ArgsFileName("broker"))
}

func TestSaveLoadArgs(t *testing.T) {
	dir := t.TempDir()

	args := []string{"server", "start", "--foreground", "--hosted", "--enable-hub"}
	err := SaveArgs("server", dir, args)
	require.NoError(t, err)

	loaded, err := LoadArgs("server", dir)
	require.NoError(t, err)
	assert.Equal(t, args, loaded)

	// Verify file exists
	assert.FileExists(t, filepath.Join(dir, "server-args.json"))
}

func TestLoadArgs_NoFile(t *testing.T) {
	dir := t.TempDir()

	loaded, err := LoadArgs("server", dir)
	assert.NoError(t, err)
	assert.Nil(t, loaded)
}

func TestRemoveArgs(t *testing.T) {
	dir := t.TempDir()

	args := []string{"server", "start", "--foreground"}
	err := SaveArgs("server", dir, args)
	require.NoError(t, err)

	err = RemoveArgs("server", dir)
	assert.NoError(t, err)

	loaded, err := LoadArgs("server", dir)
	assert.NoError(t, err)
	assert.Nil(t, loaded)
}

func TestRemoveArgs_NoFile(t *testing.T) {
	dir := t.TempDir()

	err := RemoveArgs("server", dir)
	assert.NoError(t, err)
}

func TestSaveLoadArgs_ComponentIsolation(t *testing.T) {
	dir := t.TempDir()

	serverArgs := []string{"server", "start", "--foreground", "--enable-hub"}
	brokerArgs := []string{"server", "start", "--foreground", "--hosted", "--enable-runtime-broker"}

	err := SaveArgs("server", dir, serverArgs)
	require.NoError(t, err)
	err = SaveArgs("broker", dir, brokerArgs)
	require.NoError(t, err)

	loadedServer, err := LoadArgs("server", dir)
	require.NoError(t, err)
	assert.Equal(t, serverArgs, loadedServer)

	loadedBroker, err := LoadArgs("broker", dir)
	require.NoError(t, err)
	assert.Equal(t, brokerArgs, loadedBroker)
}

func TestLegacyDelegatesToBrokerComponent(t *testing.T) {
	dir := t.TempDir()

	// Write via legacy API
	err := WritePID(dir, 54321)
	require.NoError(t, err)

	// Read via component API with "broker"
	pid, err := ReadPIDComponent("broker", dir)
	assert.NoError(t, err)
	assert.Equal(t, 54321, pid)

	// Write via component API
	err = WritePIDComponent("broker", dir, 99999)
	require.NoError(t, err)

	// Read via legacy API
	pid, err = ReadPID(dir)
	assert.NoError(t, err)
	assert.Equal(t, 99999, pid)
}

// startSignalHelper starts a shell child that installs the given SIGINT trap
// and then blocks. It returns once the trap is in place, and returns a
// channel that is closed when the child has exited and been reaped. The child
// is always killed at test cleanup.
func startSignalHelper(t *testing.T, trap string) (*exec.Cmd, <-chan struct{}) {
	t.Helper()
	// "sleep & wait" lets a trapped SIGINT interrupt the wait at once.
	script := trap + `; sleep 60 & child=$!; echo ready; wait $child; kill $child 2>/dev/null; wait`
	cmd := exec.Command("sh", "-c", script)
	// Own process group so cleanup can kill the shell and its sleep together.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())

	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-exited
	})

	line, err := bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "ready\n", line)
	return cmd, exited
}

// TestStopComponent_WaitsForSlowExit: stop returns only after a process that
// takes a while to shut down has exited, and then removes the PID file.
func TestStopComponent_WaitsForSlowExit(t *testing.T) {
	dir := t.TempDir()
	const exitDelay = 700 * time.Millisecond
	cmd, exited := startSignalHelper(t, `trap 'sleep 0.7; kill $child 2>/dev/null; exit 0' INT`)
	require.NoError(t, WritePIDComponent("server", dir, cmd.Process.Pid))

	start := time.Now()
	err := StopComponentWithTimeout("server", dir, 10*time.Second)
	elapsed := time.Since(start)
	require.NoError(t, err)

	assert.GreaterOrEqual(t, elapsed, exitDelay, "stop returned before the process could have exited")
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("process was not reaped shortly after stop returned")
	}
	assert.NoFileExists(t, GetPIDPathComponent("server", dir), "PID file should be removed after exit")
}

// TestStopComponent_TimeoutKeepsPIDFile: a process that ignores the interrupt
// hits the bounded timeout, stop reports ErrStopTimeout, and the PID file is
// kept.
func TestStopComponent_TimeoutKeepsPIDFile(t *testing.T) {
	dir := t.TempDir()
	cmd, exited := startSignalHelper(t, `trap '' INT`)
	require.NoError(t, WritePIDComponent("server", dir, cmd.Process.Pid))

	const timeout = 300 * time.Millisecond
	start := time.Now()
	err := StopComponentWithTimeout("server", dir, timeout)
	elapsed := time.Since(start)

	require.ErrorIs(t, err, ErrStopTimeout)
	assert.Contains(t, err.Error(), strconv.Itoa(cmd.Process.Pid))
	assert.GreaterOrEqual(t, elapsed, timeout)
	assert.Less(t, elapsed, 5*time.Second, "timeout should be bounded")
	select {
	case <-exited:
		t.Fatal("process exited although it ignores SIGINT")
	default:
	}
	pid, err := ReadPIDComponent("server", dir)
	require.NoError(t, err, "PID file should be kept on timeout")
	assert.Equal(t, cmd.Process.Pid, pid)
}

// TestStopComponent_NotRunning: no PID file, or a stale one, is ErrNotRunning
// and leaves no PID file behind.
func TestStopComponent_NotRunning(t *testing.T) {
	dir := t.TempDir()
	assert.ErrorIs(t, StopComponentWithTimeout("server", dir, time.Second), ErrNotRunning)

	require.NoError(t, WritePIDComponent("server", dir, 99999999))
	assert.ErrorIs(t, StopComponentWithTimeout("server", dir, time.Second), ErrNotRunning)
	assert.NoFileExists(t, GetPIDPathComponent("server", dir))
}
