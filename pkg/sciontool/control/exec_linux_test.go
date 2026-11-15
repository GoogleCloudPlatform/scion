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

package control

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// These tests run real processes through the real executor (startExec)
// and the real handler: process groups, the timeout kill sequence, the
// output drain and the environment.

type realExec struct {
	h    *Handler
	dir  string // scratch directory, also the working directory
	next int
	mu   sync.Mutex
}

func newRealExec(t *testing.T, mutate ...func(*ExecOptions)) *realExec {
	t.Helper()
	dir := t.TempDir()
	o := ExecOptions{
		Env: func() ([]string, error) {
			return []string{"PATH=/usr/bin:/bin", "HOME=" + dir, "EXEC_TEST_ENV=present"}, nil
		},
		LaunchID: "launch-real",
		Workdir:  dir,
	}
	for _, m := range mutate {
		m(&o)
	}
	return &realExec{h: New(Options{KickTokenRefresh: func() {}, Exec: &o}), dir: dir}
}

func (r *realExec) req(argv []string, timeoutS int) agentcontrol.ExecStartRequest {
	r.mu.Lock()
	r.next++
	n := r.next
	r.mu.Unlock()
	return agentcontrol.ExecStartRequest{
		V: 1, RunID: uuidN(n), Argv: argv, TimeoutS: timeoutS,
		NotAfterUnixMS: time.Now().Add(agentcontrol.ExecNotAfter).UnixMilli(),
	}
}

func (r *realExec) start(t *testing.T, req agentcontrol.ExecStartRequest) (int, agentcontrol.ExecResponse, string) {
	t.Helper()
	return postExec(t, r.h, RouteExecStart, req)
}

// waitFinished polls exec-status until the run is finished.
func (r *realExec) waitFinished(t *testing.T, runID string, within time.Duration) (agentcontrol.ExecResponse, string) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		code, resp, raw := postExec(t, r.h, RouteExecStatus, agentcontrol.ExecStatusRequest{V: 1, RunID: runID})
		if code == http.StatusOK && resp.State == agentcontrol.ExecStateFinished {
			if _, err := agentcontrol.ParseExecStatusAnswer(code, []byte(raw)); err != nil {
				t.Fatalf("status answer fails the hub schema: %v", err)
			}
			return resp, raw
		}
		if code != http.StatusOK {
			t.Fatalf("status = %d %s", code, raw)
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s not finished within %s", runID, within)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (r *realExec) path(name string) string { return filepath.Join(r.dir, name) }

// readPID waits for a pid file written by a run.
func readPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		b, err := os.ReadFile(path)
		if err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no pid in %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// procGone reports whether pid no longer runs (absent or a zombie).
func procGone(pid int) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return true
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	return i < 0 || i+2 >= len(s) || s[i+2] == 'Z'
}

func waitGone(t *testing.T, pid int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !procGone(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("process %d still running", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func killLater(t *testing.T, pid int) {
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
}

func TestExecRealOutcomes(t *testing.T) {
	t.Parallel()
	r := newRealExec(t)
	cases := []struct {
		argv    []string
		outcome string
		code    int
		bytes   int64
	}{
		{[]string{"true"}, agentcontrol.ExecOutcomeOK, 0, 0},
		{[]string{"sh", "-c", "printf 12345; printf 678 >&2; exit 3"}, agentcontrol.ExecOutcomeNonzero, 3, 8},
		{[]string{"sh", "-c", "kill -USR1 $$"}, agentcontrol.ExecOutcomeNonzero, 128 + int(syscall.SIGUSR1), 0},
	}
	for _, c := range cases {
		req := r.req(c.argv, 30)
		if code, resp, _ := r.start(t, req); code != http.StatusAccepted || resp.State != agentcontrol.ExecStateRunning {
			t.Fatalf("%v: start = %d %+v", c.argv, code, resp)
		}
		resp, _ := r.waitFinished(t, req.RunID, 10*time.Second)
		if resp.Outcome != c.outcome || *resp.ExitCode != c.code || *resp.OutputBytes != c.bytes {
			t.Fatalf("%v: got %+v exit=%d bytes=%d", c.argv, resp, *resp.ExitCode, *resp.OutputBytes)
		}
	}
	// Not found: start_failed, no error text anywhere.
	req := r.req([]string{"/nonexistent/SENTINEL-not-found"}, 5)
	code, resp, raw := r.start(t, req)
	if code != http.StatusAccepted || resp.Outcome != agentcontrol.ExecOutcomeStartFailed || strings.Contains(raw, "SENTINEL") {
		t.Fatalf("not found = %d %s", code, raw)
	}
}

// TestExecRealEnvCwdStdin: the run sees the configured environment (with
// PWD), the working directory, and an empty stdin.
func TestExecRealEnvCwdStdin(t *testing.T) {
	t.Parallel()
	r := newRealExec(t)
	out := r.path("probe")
	req := r.req([]string{"sh", "-c", `{ echo "env=$EXEC_TEST_ENV"; echo "pwd=$(pwd)"; echo "PWD=$PWD"; echo "stdin=$(cat)"; } > ` + out}, 10)
	if code, _, raw := r.start(t, req); code != http.StatusAccepted {
		t.Fatalf("start = %d %s", code, raw)
	}
	if resp, _ := r.waitFinished(t, req.RunID, 10*time.Second); resp.Outcome != agentcontrol.ExecOutcomeOK {
		t.Fatalf("got %+v", resp)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("env=present\npwd=%s\nPWD=%s\nstdin=\n", r.dir, r.dir)
	if string(b) != want {
		t.Fatalf("probe = %q, want %q", b, want)
	}
}

// TestExecRealLargeOutputBoundedMemory: a 200 MiB producer is counted
// exactly while sciontool's memory stays bounded (SEC-P2).
func TestExecRealLargeOutputBoundedMemory(t *testing.T) {
	r := newRealExec(t)
	const size = 200 << 20
	base := rssBytes(t)
	req := r.req([]string{"head", "-c", strconv.Itoa(size), "/dev/zero"}, 120)
	if code, _, raw := r.start(t, req); code != http.StatusAccepted {
		t.Fatalf("start = %d %s", code, raw)
	}
	peak := base
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-time.After(5 * time.Millisecond):
				if v := rssBytes(t); v > peak {
					peak = v
				}
			case <-ctxDone(t):
				return
			}
			code, resp, _ := postExec(t, r.h, RouteExecStatus, agentcontrol.ExecStatusRequest{V: 1, RunID: req.RunID})
			if code != http.StatusOK || resp.State == agentcontrol.ExecStateFinished {
				return
			}
		}
	}()
	<-done
	resp, _ := r.waitFinished(t, req.RunID, time.Minute)
	if resp.Outcome != agentcontrol.ExecOutcomeOK || *resp.OutputBytes != size {
		t.Fatalf("got %+v bytes=%d, want ok %d", resp, *resp.OutputBytes, int64(size))
	}
	if grew := peak - base; grew > 48<<20 {
		t.Fatalf("RSS grew by %d MiB while draining 200 MiB", grew>>20)
	}
}

func ctxDone(t *testing.T) <-chan struct{} { return t.Context().Done() }

func rssBytes(t *testing.T) int64 {
	t.Helper()
	f, err := os.Open("/proc/self/status")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "VmRSS:"); ok {
			kb, _ := strconv.ParseInt(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(v), "kB")), 10, 64)
			return kb << 10
		}
	}
	t.Fatal("no VmRSS")
	return 0
}

// TestExecRealTimeoutKillsGroup: on timeout the whole process group dies,
// including an ordinary background child.
func TestExecRealTimeoutKillsGroup(t *testing.T) {
	t.Parallel()
	r := newRealExec(t)
	pidf := r.path("child.pid")
	req := r.req([]string{"sh", "-c", "sleep 300 & echo $! > " + pidf + "; wait"}, 1)
	if code, _, raw := r.start(t, req); code != http.StatusAccepted {
		t.Fatalf("start = %d %s", code, raw)
	}
	child := readPID(t, pidf)
	killLater(t, child)
	resp, _ := r.waitFinished(t, req.RunID, 15*time.Second)
	if resp.Outcome != agentcontrol.ExecOutcomeTimeout {
		t.Fatalf("got %+v, want timeout", resp)
	}
	waitGone(t, child, 2*time.Second)
}

// TestExecRealTimeoutKillsTermIgnoringChildAfterLeaderExit: the leader
// exits on SIGTERM; a child that ignores SIGTERM is still killed by the
// group SIGKILL ExecKillGrace later, and the run is reported only after
// that (the leader stays unreaped until then).
func TestExecRealTimeoutKillsTermIgnoringChildAfterLeaderExit(t *testing.T) {
	t.Parallel()
	r := newRealExec(t)
	pidf := r.path("stubborn.pid")
	leaderf := r.path("leader.pid")
	script := `echo $$ > ` + leaderf + `; sh -c 'trap "" TERM; echo $$ > ` + pidf + `; while :; do sleep 1; done' & wait`
	req := r.req([]string{"sh", "-c", script}, 1)
	start := time.Now()
	if code, _, raw := r.start(t, req); code != http.StatusAccepted {
		t.Fatalf("start = %d %s", code, raw)
	}
	stubborn := readPID(t, pidf)
	leader := readPID(t, leaderf)
	killLater(t, stubborn)

	// After SIGTERM (at ~1s) the leader has exited but is not reaped: it
	// is a zombie, its pid (the group id) held; the stubborn child lives.
	time.Sleep(1500 * time.Millisecond)
	if !isZombie(leader) {
		t.Fatalf("leader %d should be an unreaped zombie during the kill grace", leader)
	}
	if procGone(stubborn) {
		t.Fatal("TERM-ignoring child died on SIGTERM; the test does not exercise the SIGKILL")
	}
	resp, _ := r.waitFinished(t, req.RunID, 15*time.Second)
	if resp.Outcome != agentcontrol.ExecOutcomeTimeout {
		t.Fatalf("got %+v, want timeout", resp)
	}
	if took := time.Since(start); took < time.Second+agentcontrol.ExecKillGrace {
		t.Fatalf("finished after %s, before the kill grace ended", took)
	}
	waitGone(t, stubborn, 2*time.Second)
	waitGone(t, leader, 2*time.Second)
}

func isZombie(pid int) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	return i >= 0 && i+2 < len(s) && s[i+2] == 'Z'
}

// TestExecRealNormalExitLeavesBackgroundChildren: a normal exit does not
// kill the group; a setsid child and a same-group child that both hold the
// output pipes survive, and completion comes within WaitDelay.
func TestExecRealNormalExitLeavesBackgroundChildren(t *testing.T) {
	t.Parallel()
	r := newRealExec(t)
	sidf, grpf := r.path("setsid.pid"), r.path("group.pid")
	script := "setsid sleep 60 & echo $! > " + sidf + "; sleep 60 & echo $! > " + grpf + "; exit 0"
	req := r.req([]string{"sh", "-c", script}, 30)
	start := time.Now()
	if code, _, raw := r.start(t, req); code != http.StatusAccepted {
		t.Fatalf("start = %d %s", code, raw)
	}
	sid, grp := readPID(t, sidf), readPID(t, grpf)
	killLater(t, sid)
	killLater(t, grp)
	resp, _ := r.waitFinished(t, req.RunID, 10*time.Second)
	if resp.Outcome != agentcontrol.ExecOutcomeOK {
		t.Fatalf("got %+v", resp)
	}
	if took := time.Since(start); took > agentcontrol.ExecWaitDelay+3*time.Second {
		t.Fatalf("completion took %s; WaitDelay should bound it", took)
	}
	time.Sleep(200 * time.Millisecond)
	if procGone(sid) || procGone(grp) {
		t.Fatalf("background children killed on normal exit (setsid gone=%t, group gone=%t)", procGone(sid), procGone(grp))
	}
}

// TestExecRealReplayRunsOnce: sequential and concurrent duplicate starts of
// one run_id run the command once.
func TestExecRealReplayRunsOnce(t *testing.T) {
	t.Parallel()
	r := newRealExec(t)
	count := r.path("count")
	req := r.req([]string{"sh", "-c", "echo x >> " + count + "; sleep 1"}, 30)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); r.start(t, req) }()
	}
	wg.Wait()
	r.waitFinished(t, req.RunID, 10*time.Second)
	for i := 0; i < 4; i++ {
		if code, resp, _ := r.start(t, req); code != http.StatusOK || !resp.Replayed {
			t.Fatalf("replay = %d %+v", code, resp)
		}
	}
	b, err := os.ReadFile(count)
	if err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(b, []byte("x")); n != 1 {
		t.Fatalf("command ran %d times", n)
	}
}

// TestExecRealPrivilegeDropFailsClosed: with a drop required and no agent
// user, nothing runs (the marker is never written).
func TestExecRealPrivilegeDropFailsClosed(t *testing.T) {
	t.Parallel()
	r := newRealExec(t, func(o *ExecOptions) { o.RequirePrivilegeDrop = true })
	marker := r.path("marker")
	req := r.req([]string{"sh", "-c", "touch " + marker}, 5)
	code, resp, _ := r.start(t, req)
	if code != http.StatusAccepted || resp.Outcome != agentcontrol.ExecOutcomeStartFailed {
		t.Fatalf("got %d %+v, want start_failed", code, resp)
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("ran without the required privilege drop")
	}
}

// TestExecRealSentinelNeverLeaks: a secret in argv and in the command's
// stdout and stderr appears in no answer and no sciontool log line, for
// exit 0, non-zero, timeout and start failure.
func TestExecRealSentinelNeverLeaks(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "agent.log")
	log.SetLogPath(logPath)
	t.Cleanup(func() { log.SetLogPath(filepath.Join(os.TempDir(), "agent.log")) })
	log.SetDebug(true)
	t.Cleanup(func() { log.SetDebug(false) })

	const secret = "SENTINEL-7f3a-secret"
	r := newRealExec(t)
	argvs := [][]string{
		{"sh", "-c", "echo " + secret + "; echo " + secret + " >&2", secret},
		{"sh", "-c", "echo " + secret + " >&2; exit 4", secret},
		{"sh", "-c", "echo " + secret + "; sleep 30", secret},
		{"/nonexistent/" + secret, secret},
	}
	var answers []string
	for _, argv := range argvs {
		req := r.req(argv, 1)
		_, _, raw := r.start(t, req)
		answers = append(answers, raw)
		_, raw = r.waitFinished(t, req.RunID, 15*time.Second)
		answers = append(answers, raw)
		_, _, raw = r.start(t, req) // replay
		answers = append(answers, raw)
	}
	// Refusals, too.
	bad := r.req([]string{"sh", secret}, 0)
	_, _, raw := r.start(t, bad)
	answers = append(answers, raw)

	// Through the conduit RPC adapter as well.
	rpcReq := r.req([]string{"echo", secret}, 5)
	body := fmt.Sprintf(`{"v":1,"run_id":%q,"argv":["echo",%q],"timeout_s":5,"not_after_unix_ms":%d}`, rpcReq.RunID, secret, rpcReq.NotAfterUnixMS)
	resp := RPCHandler(r.h).HandleRPC(context.Background(), &conduitv1.RpcRequest{
		Method: http.MethodPost, Path: Prefix + RouteExecStart, Body: []byte(body),
		Headers: map[string]string{"Content-Type": "application/json"},
	})
	answers = append(answers, string(resp.GetBody()))
	r.waitFinished(t, rpcReq.RunID, 10*time.Second)

	for _, a := range answers {
		if strings.Contains(a, secret) {
			t.Fatalf("answer leaks the sentinel: %s", a)
		}
	}
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) == 0 || !strings.Contains(string(b), "exec-finish") {
		t.Fatalf("no exec log lines captured: %q", b)
	}
	if strings.Contains(string(b), secret) {
		t.Fatalf("sciontool log leaks the sentinel:\n%s", b)
	}
}
