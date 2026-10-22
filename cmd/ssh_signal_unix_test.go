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
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// installWaitingSSHClient installs a fake ssh as the only program on PATH.
// It writes its pid to the returned file and then waits 30s in /bin/sleep,
// which keeps the pid. With ignoreSignals it ignores SIGHUP and SIGTERM,
// so only a kill ends it.
func installWaitingSSHClient(t *testing.T, ignoreSignals bool) string {
	t.Helper()
	for _, p := range []string{"/bin/sh", "/bin/sleep"} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("needs %s", p)
		}
	}
	bin := t.TempDir()
	pidFile := filepath.Join(t.TempDir(), "pid")
	script := "#!/bin/sh\n"
	if ignoreSignals {
		script += "trap '' HUP TERM\n"
	}
	script += "echo $$ > '" + pidFile + ".tmp'\n" +
		"mv '" + pidFile + ".tmp' '" + pidFile + "'\n" +
		"exec /bin/sleep 30\n"
	require.NoError(t, os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0o755))
	t.Setenv("PATH", bin)
	return pidFile
}

// startSSHForSignal runs runSSH against a fake hub in the background and
// waits until the fake ssh has started. runSSH catches signals before it
// starts ssh, so from then on a signal to this process reaches runSSH
// instead of ending the test binary.
func startSSHForSignal(t *testing.T, pidFile string) <-chan int {
	t.Helper()
	hub := twoProjectHub(t, "tok")
	env, _, _ := newSSHTestEnv(t, t.TempDir(), hub)
	codes := make(chan int, 1)
	go func() { codes <- runSSH(context.Background(), env, []string{"a-b/c"}) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(pidFile); err == nil {
			return codes
		}
		select {
		case code := <-codes:
			t.Fatalf("scion ssh exited %d before ssh started", code)
		default:
		}
		require.True(t, time.Now().Before(deadline), "fake ssh did not start")
		time.Sleep(10 * time.Millisecond)
	}
}

// waitSSHCode returns runSSH's exit code, failing if it takes longer than
// within.
func waitSSHCode(t *testing.T, codes <-chan int, within time.Duration) int {
	t.Helper()
	select {
	case code := <-codes:
		return code
	case <-time.After(within):
		t.Fatalf("scion ssh did not exit within %s", within)
		return 0
	}
}

// A hangup or termination request sent to scion ssh reaches ssh, which
// ends promptly, and scion ssh exits 129 or 143.
func TestSSH_ParentSignalEndsSSH(t *testing.T) {
	for _, tc := range []struct {
		name string
		sig  syscall.Signal
		want int
	}{
		{"SIGHUP", syscall.SIGHUP, 129},
		{"SIGTERM", syscall.SIGTERM, 143},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := sshSignalGrace
			sshSignalGrace = time.Minute // the signal itself must end ssh
			t.Cleanup(func() { sshSignalGrace = old })
			pidFile := installWaitingSSHClient(t, false)
			codes := startSSHForSignal(t, pidFile)

			start := time.Now()
			require.NoError(t, syscall.Kill(os.Getpid(), tc.sig))
			assert.Equal(t, tc.want, waitSSHCode(t, codes, 5*time.Second))
			assert.Less(t, time.Since(start), 5*time.Second)
		})
	}
}

// An ssh that ignores the passed-on signal is killed after the grace
// period, and scion ssh still exits with the signal's status.
func TestSSH_ParentSignalKillsSSHAfterGrace(t *testing.T) {
	old := sshSignalGrace
	sshSignalGrace = 200 * time.Millisecond
	t.Cleanup(func() { sshSignalGrace = old })
	pidFile := installWaitingSSHClient(t, true)
	codes := startSSHForSignal(t, pidFile)

	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGTERM))
	assert.Equal(t, 143, waitSSHCode(t, codes, 5*time.Second))
}

// A terminal interrupt is left to ssh: scion ssh does not end ssh or exit
// on it, and a later termination request still ends both.
func TestSSH_InterruptLeftToSSH(t *testing.T) {
	old := sshSignalGrace
	sshSignalGrace = time.Minute
	t.Cleanup(func() { sshSignalGrace = old })
	pidFile := installWaitingSSHClient(t, false)
	codes := startSSHForSignal(t, pidFile)

	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGINT))
	select {
	case code := <-codes:
		t.Fatalf("scion ssh exited %d on an interrupt it should leave to ssh", code)
	case <-time.After(300 * time.Millisecond):
	}
	require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGTERM))
	assert.Equal(t, 143, waitSSHCode(t, codes, 5*time.Second))
}
