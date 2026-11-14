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
	"errors"
	"os/exec"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/procreap"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/suppgroups"
)

// execSupported: agent containers are Linux, where the executor below
// can wait for the main process without reaping it (waitid WNOWAIT).
const execSupported = true

// linuxExec is a started run.
type linuxExec struct {
	cmd     *exec.Cmd
	tok     *procreap.Token
	out     *countingDiscard
	spec    execSpec
	started time.Time
	// exited is closed when the main process has exited; it is not reaped
	// until wait calls cmd.Wait, so its pid (and so the process group id)
	// cannot be reused while the group is being signalled.
	exited chan struct{}
}

// startExec starts spec.path directly (no shell) in its own process group,
// as the run's user and supplementary groups, with stdin /dev/null and
// stdout and stderr drained into a counting discard sink.
func startExec(spec execSpec) (execProcess, error) {
	out := &countingDiscard{}
	cmd := &exec.Cmd{
		Path:   spec.path,
		Args:   spec.argv,
		Env:    spec.env,
		Dir:    spec.dir,
		Stdin:  nil, // /dev/null
		Stdout: out,
		Stderr: out,
		// A background child that still holds the output pipes cannot
		// delay completion past this.
		WaitDelay:   spec.waitDelay,
		SysProcAttr: &syscall.SysProcAttr{Setpgid: true},
	}
	if spec.drop {
		cmd.SysProcAttr.Credential = suppgroups.Credential(uint32(spec.uid), uint32(spec.gid))
	}
	p := &linuxExec{cmd: cmd, out: out, spec: spec, exited: make(chan struct{})}
	// sciontool init is PID 1 and reaps orphans; registering the main
	// process inside the gate (as the PTY code does) leaves it to wait, so
	// the reaper never reaps it while its group is being signalled.
	err := procreap.Gated(func() error {
		if err := cmd.Start(); err != nil {
			return err
		}
		p.tok = procreap.RegisterManagedPID(cmd.Process.Pid)
		return nil
	})
	if err != nil {
		return nil, errors.New("control: exec start failed")
	}
	p.started = time.Now()
	go p.watch()
	return p, nil
}

// watch closes exited once the main process has exited, without reaping
// it.
func (p *linuxExec) watch() {
	defer close(p.exited)
	pid := p.cmd.Process.Pid
	for {
		var info unix.Siginfo
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if !errors.Is(err, unix.EINTR) {
			return
		}
	}
}

// wait ends the run: on a normal exit it records the outcome at once and
// leaves the process group alone (backgrounded children survive); on
// timeout it sends SIGTERM to the group, then SIGKILL to the group
// killGrace later even if the main process already exited, and only then
// reaps the main process.
func (p *linuxExec) wait() execFinal {
	timer := time.NewTimer(p.spec.timeout)
	defer timer.Stop()
	pgid := p.cmd.Process.Pid
	timedOut := false
	var ended time.Time
	select {
	case <-p.exited:
		ended = time.Now()
	case <-timer.C:
		timedOut = true
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
		time.Sleep(p.spec.killGrace)
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		// A main process that left its group still dies: it is not reaped
		// yet, so its pid cannot name another process.
		_ = p.cmd.Process.Kill()
		<-p.exited
		ended = time.Now()
	}
	_ = p.cmd.Wait() // reaps; the exit status is read from ProcessState
	procreap.UnregisterManagedPID(pgid, p.tok)

	f := execFinal{
		exitCode:    exitCode(p.cmd.ProcessState),
		durationMS:  ended.Sub(p.started).Milliseconds(),
		outputBytes: p.out.n.Load(),
	}
	switch {
	case timedOut:
		f.outcome = agentcontrol.ExecOutcomeTimeout
	case f.exitCode == 0:
		f.outcome = agentcontrol.ExecOutcomeOK
	default:
		f.outcome = agentcontrol.ExecOutcomeNonzero
	}
	return f
}

// exitCode is the main process's exit status, or 128 plus the signal
// number when a signal ended it (the shell convention).
func exitCode(ps interface{ Sys() any }) int {
	if ps == nil {
		return -1
	}
	ws, ok := ps.Sys().(syscall.WaitStatus)
	if !ok {
		return -1
	}
	if ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ws.ExitStatus()
}
