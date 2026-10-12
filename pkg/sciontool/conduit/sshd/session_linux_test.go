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

//go:build linux

package sshd

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// procGone reports whether pid has exited (no such process, or a zombie
// waiting to be reaped by its parent).
func procGone(pid int) bool {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return true
	}
	s := string(data)
	i := strings.LastIndexByte(s, ')')
	return i < 0 || i+2 >= len(s) || s[i+2] == 'Z'
}

// readPID waits for a pid file and returns its pid.
func readPID(t *testing.T, path string) int {
	t.Helper()
	var pid int
	waitFor(t, "pid file "+filepath.Base(path), func() bool {
		raw, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		n, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		pid = n
		return err == nil
	})
	return pid
}

// TestPTYDisconnectEndsShellAndForegroundJob: when the client closes a
// pty session while a foreground job runs, the login shell and the job
// both end.
func TestPTYDisconnectEndsShellAndForegroundJob(t *testing.T) {
	dir := t.TempDir()
	shPID := filepath.Join(dir, "sh-pid")
	jobPID := filepath.Join(dir, "job-pid")
	client := serve(t, testConfig(t), "scion")
	sess, in, _ := ptyShell(t, client, 24, 80)
	if _, err := io.WriteString(in, "echo $$ > "+shPID+"; sh -c 'echo $$ > "+jobPID+"; exec sleep 300'\n"); err != nil {
		t.Fatal(err)
	}
	sh, job := readPID(t, shPID), readPID(t, jobPID)
	_ = sess.Close()
	waitFor(t, "the login shell to end", func() bool { return procGone(sh) })
	waitFor(t, "the foreground job to end", func() bool { return procGone(job) })
}
