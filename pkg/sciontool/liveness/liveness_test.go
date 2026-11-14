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

package liveness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
)

const agentUID = 1000

// fakeProc is one fixture /proc/<pid> entry.
type fakeProc struct {
	pid, ppid int
	comm      string
	state     byte // 0 means 'S'
	uid       int
	ticks     uint64
	start     uint64
	cmdline   string
}

// fakeProcTree writes fixture /proc trees and samples them.
type fakeProcTree struct {
	t       *testing.T
	sampler *Sampler
}

func newFakeProcTree(t *testing.T, managed ...int) *fakeProcTree {
	m := map[int]bool{}
	for _, pid := range managed {
		m[pid] = true
	}
	s := NewSampler(Config{AgentUID: agentUID, IsManagedPID: func(pid int) bool { return m[pid] }})
	return &fakeProcTree{t: t, sampler: s}
}

// sample writes procs as a fresh /proc tree and samples it.
func (f *fakeProcTree) sample(procs ...fakeProc) *LiveWork {
	f.t.Helper()
	root := f.t.TempDir()
	for _, p := range procs {
		dir := filepath.Join(root, strconv.Itoa(p.pid))
		require.NoError(f.t, os.MkdirAll(dir, 0o755))
		st := p.state
		if st == 0 {
			st = 'S'
		}
		// Fields after comm: state ppid pgrp session tty_nr tpgid flags
		// minflt cminflt majflt cmajflt utime stime cutime cstime priority
		// nice num_threads itrealvalue starttime vsize rss.
		stat := fmt.Sprintf("%d (%s) %c %d 0 0 0 0 0 0 0 0 0 %d %d 0 0 20 0 1 0 %d 1000 100\n",
			p.pid, p.comm, st, p.ppid, p.ticks/2, p.ticks-p.ticks/2, p.start)
		require.NoError(f.t, os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644))
		status := fmt.Sprintf("Name:\t%s\nState:\t%c\nUid:\t%d\t%d\t%d\t%d\n", p.comm, st, p.uid, p.uid, p.uid, p.uid)
		require.NoError(f.t, os.WriteFile(filepath.Join(dir, "status"), []byte(status), 0o644))
		cmdline := p.cmdline
		if cmdline == "" {
			cmdline = p.comm
		}
		require.NoError(f.t, os.WriteFile(filepath.Join(dir, "cmdline"), []byte(cmdline+"\x00"), 0o644))
	}
	f.sampler.procRoot = root
	return f.sampler.sample()
}

// wrapperCmdline is the agent window's pane command as /proc shows it.
var wrapperCmdline = "sh\x00-c\x00claude --dangerously-skip-permissions; echo $? > " + state.HarnessExitCodeFile

// idleTree is an agent container with nothing to do: sciontool init as
// PID 1, a managed sidecar service, the tmux server and client, the agent
// window's wrapper and harness, an MCP server and an LSP server under the
// harness, a sciontool hook, the shell window's login shell, and a
// root-owned process. tick adds CPU to the processes that are always
// busy-ish, or that must not count however busy they are.
func idleTree(tick uint64) []fakeProc {
	return []fakeProc{
		{pid: 1, ppid: 0, comm: "sciontool", uid: 0, ticks: 100 + tick*5, start: 1},
		{pid: 20, ppid: 1, comm: "sh", uid: agentUID, ticks: 1, start: 10},                              // managed: tmux start command
		{pid: 21, ppid: 20, comm: "tmux: client", uid: agentUID, ticks: 10 + tick*3, start: 11},         // managed subtree
		{pid: 30, ppid: 1, comm: "otel-collector", uid: agentUID, ticks: 50 + tick*4, start: 12},        // managed sidecar
		{pid: 31, ppid: 30, comm: "worker", uid: agentUID, ticks: 50 + tick*4, start: 13},               // child of the sidecar
		{pid: 40, ppid: 1, comm: "tmux: server", uid: agentUID, ticks: 100 + tick*2, start: 14},         // daemonized server
		{pid: 41, ppid: 40, comm: "sh", uid: agentUID, ticks: 1, start: 15, cmdline: wrapperCmdline},    // window-0 pane root
		{pid: 42, ppid: 41, comm: "claude", uid: agentUID, ticks: 1000 + tick*5, start: 16},             // harness root (TUI spinning)
		{pid: 43, ppid: 42, comm: "node", uid: agentUID, ticks: 200 + tick/20, start: 17},               // idle MCP server
		{pid: 44, ppid: 42, comm: "gopls", uid: agentUID, ticks: 300 + tick/30, start: 18},              // idle LSP server
		{pid: 45, ppid: 42, comm: "sciontool", uid: agentUID, ticks: 5 + tick, start: 19},               // a hook
		{pid: 50, ppid: 40, comm: "zsh", uid: agentUID, ticks: 3, start: 20, cmdline: "-zsh"},           // window-1 pane root
		{pid: 60, ppid: 1, comm: "dockerd-helper", uid: 0, ticks: 10 + tick*4, start: 21},               // not the agent UID
		{pid: 61, ppid: 42, comm: "defunct", uid: agentUID, state: 'Z', ticks: 400 + tick*4, start: 22}, // zombie
	}
}

var idleManaged = []int{20, 30}

func TestSample_IdleTreeIsNotWork(t *testing.T) {
	f := newFakeProcTree(t, idleManaged...)
	require.Nil(t, f.sample(idleTree(0)...), "the first sample only records the baseline")
	for i := uint64(1); i <= 3; i++ {
		assert.Nil(t, f.sample(idleTree(i*100)...), "sample %d", i)
	}
}

func TestSample_FirstSampleIsBaselineOnly(t *testing.T) {
	f := newFakeProcTree(t, idleManaged...)
	busy := fakeProc{pid: 70, ppid: 42, comm: "go", uid: agentUID, ticks: 5000, start: 30}
	assert.Nil(t, f.sample(append(idleTree(0), busy)...), "CPU used before sampling began is not counted")
	busy.ticks += 200
	assert.NotNil(t, f.sample(append(idleTree(100), busy)...))
}

func TestSample_CPUBusyToolChildCounts(t *testing.T) {
	f := newFakeProcTree(t, idleManaged...)
	shell := fakeProc{pid: 70, ppid: 42, comm: "bash", uid: agentUID, ticks: 2, start: 30}
	gotest := fakeProc{pid: 71, ppid: 70, comm: "go", uid: agentUID, ticks: 100, start: 31}
	require.Nil(t, f.sample(append(idleTree(0), shell, gotest)...))

	gotest.ticks += 450
	lw := f.sample(append(idleTree(100), shell, gotest)...)
	require.NotNil(t, lw)
	assert.Equal(t, KindProcess, lw.Kind)
	assert.True(t, lw.SuppressStall)
	assert.Equal(t, "go (pid 71, 4.5s cpu)", lw.Detail)

	// The job finished: the next sample is idle again.
	assert.Nil(t, f.sample(idleTree(200)...))
}

func TestSample_JobReparentedToPID1Counts(t *testing.T) {
	f := newFakeProcTree(t, idleManaged...)
	job := fakeProc{pid: 80, ppid: 1, comm: "make", uid: agentUID, ticks: 10, start: 40}
	require.Nil(t, f.sample(append(idleTree(0), job)...))
	job.ticks += 60
	lw := f.sample(append(idleTree(100), job)...)
	require.NotNil(t, lw)
	assert.Equal(t, "make (pid 80, 0.6s cpu)", lw.Detail)
}

func TestSample_ShellWindowCommandCounts(t *testing.T) {
	f := newFakeProcTree(t, idleManaged...)
	cmd := fakeProc{pid: 90, ppid: 50, comm: "cargo", uid: agentUID, ticks: 0, start: 50}
	require.Nil(t, f.sample(append(idleTree(0), cmd)...))
	cmd.ticks += 100
	assert.NotNil(t, f.sample(append(idleTree(100), cmd)...), "a command typed in the shell window is work")
}

func TestSample_Threshold(t *testing.T) {
	f := newFakeProcTree(t, idleManaged...)
	job := fakeProc{pid: 80, ppid: 42, comm: "rg", uid: agentUID, ticks: 0, start: 40}
	require.Nil(t, f.sample(append(idleTree(0), job)...))
	job.ticks += MinCPUTicks - 1
	assert.Nil(t, f.sample(append(idleTree(100), job)...), "below MinCPUTicks")
	job.ticks += MinCPUTicks
	assert.NotNil(t, f.sample(append(idleTree(200), job)...), "at MinCPUTicks")
}

func TestSample_NewProcessCountsFromStart(t *testing.T) {
	f := newFakeProcTree(t, idleManaged...)
	require.Nil(t, f.sample(idleTree(0)...))

	small := fakeProc{pid: 80, ppid: 42, comm: "ls", uid: agentUID, ticks: 2, start: 60}
	assert.Nil(t, f.sample(append(idleTree(100), small)...), "a new process with little CPU")

	born := fakeProc{pid: 81, ppid: 42, comm: "cc1", uid: agentUID, ticks: 80, start: 61}
	lw := f.sample(append(idleTree(200), born)...)
	require.NotNil(t, lw, "a process born between samples counts from its start")
	assert.Equal(t, "cc1 (pid 81, 0.8s cpu)", lw.Detail)
}

func TestSample_ReusedPIDCountsFromStart(t *testing.T) {
	f := newFakeProcTree(t, idleManaged...)
	old := fakeProc{pid: 80, ppid: 42, comm: "old", uid: agentUID, ticks: 500, start: 60}
	require.Nil(t, f.sample(append(idleTree(0), old)...))
	// Same PID, new process (different starttime) with fewer total ticks
	// than the old one had: its own ticks are what counts.
	reused := fakeProc{pid: 80, ppid: 42, comm: "new", uid: agentUID, ticks: 40, start: 99}
	lw := f.sample(append(idleTree(100), reused)...)
	require.NotNil(t, lw)
	assert.Equal(t, "new (pid 80, 0.4s cpu)", lw.Detail)
}

// tmux runs the pane command through its default shell. A shell that does
// not exec the command leaves a second wrapper between the pane root and
// the harness; the harness root is still excluded.
func TestSample_NestedWrapperExcludesHarnessRoot(t *testing.T) {
	outer := "/bin/bash\x00-c\x00" + wrapperCmdline
	tree := func(tick uint64) []fakeProc {
		return []fakeProc{
			{pid: 1, ppid: 0, comm: "sciontool", uid: 0, ticks: 1, start: 1},
			{pid: 40, ppid: 1, comm: "tmux: server", uid: agentUID, ticks: 100 + tick, start: 14},
			{pid: 41, ppid: 40, comm: "bash", uid: agentUID, ticks: 1, start: 15, cmdline: outer},
			{pid: 42, ppid: 41, comm: "sh", uid: agentUID, ticks: 1, start: 16, cmdline: wrapperCmdline},
			{pid: 43, ppid: 42, comm: "gemini", uid: agentUID, ticks: 1000 + tick*5, start: 17},
		}
	}
	f := newFakeProcTree(t)
	require.Nil(t, f.sample(tree(0)...))
	assert.Nil(t, f.sample(tree(100)...), "the harness TUI spinning is not work")

	tool := fakeProc{pid: 44, ppid: 43, comm: "npm", uid: agentUID, ticks: 0, start: 18}
	require.Nil(t, f.sample(append(tree(200), tool)...))
	tool.ticks += 300
	assert.NotNil(t, f.sample(append(tree(300), tool)...), "a child of the harness root counts")
}

// A command line naming the exit-code file marks a wrapper only in the
// chain below tmux: a tool that happens to mention it still counts.
func TestSample_MarkerOutsidePaneChainCounts(t *testing.T) {
	f := newFakeProcTree(t, idleManaged...)
	tool := fakeProc{pid: 80, ppid: 42, comm: "cat", uid: agentUID, ticks: 0, start: 40,
		cmdline: "cat\x00" + state.HarnessExitCodeFile}
	require.Nil(t, f.sample(append(idleTree(0), tool)...))
	tool.ticks += 100
	assert.NotNil(t, f.sample(append(idleTree(100), tool)...))
}

func TestSample_RootlessAgentUID0(t *testing.T) {
	// Rootless containers run the agent as UID 0: PID 1 and the managed
	// sidecars still never count; an agent job does.
	s := NewSampler(Config{AgentUID: 0, IsManagedPID: func(pid int) bool { return pid == 30 }})
	f := &fakeProcTree{t: t, sampler: s}
	tree := func(tick uint64) []fakeProc {
		return []fakeProc{
			{pid: 1, ppid: 0, comm: "sciontool", uid: 0, ticks: 100 + tick, start: 1},
			{pid: 30, ppid: 1, comm: "sidecar", uid: 0, ticks: 10 + tick, start: 2},
			{pid: 80, ppid: 1, comm: "make", uid: 0, ticks: 10 + tick, start: 3},
		}
	}
	require.Nil(t, f.sample(tree(0)...))
	lw := f.sample(tree(100)...)
	require.NotNil(t, lw)
	assert.Equal(t, "make (pid 80, 1.0s cpu)", lw.Detail)
}

func TestSample_MissingProcRoot(t *testing.T) {
	s := NewSampler(Config{AgentUID: agentUID})
	s.procRoot = filepath.Join(t.TempDir(), "missing")
	assert.Nil(t, s.sample())
	assert.Nil(t, s.sample())
}

func TestParseStat(t *testing.T) {
	p, ok := parseStat("123 (tmux: server) S 1 123 123 0 -1 4194560 100 0 0 0 7 5 0 0 20 0 1 0 4242 1000 100\n")
	require.True(t, ok)
	assert.Equal(t, 123, p.pid)
	assert.Equal(t, 1, p.ppid)
	assert.Equal(t, byte('S'), p.state)
	assert.Equal(t, "tmux: server", p.comm)
	assert.Equal(t, uint64(12), p.ticks)
	assert.Equal(t, uint64(4242), p.start)

	// comm may contain spaces and parentheses.
	p, ok = parseStat("9 (a) b (c) R 2 0 0 0 0 0 0 0 0 0 1 2 0 0 20 0 1 0 77 0 0")
	require.True(t, ok)
	assert.Equal(t, "a) b (c", p.comm)
	assert.Equal(t, byte('R'), p.state)
	assert.Equal(t, uint64(3), p.ticks)

	for _, bad := range []string{"", "12 no-parens S 1", "x (c) S 1 0 0 0 0 0 0 0 0 0 1 2 0 0 20 0 1 0 77", "1 (c) S 1 2 3"} {
		_, ok := parseStat(bad)
		assert.False(t, ok, "%q", bad)
	}
}

func TestParseStatusUID(t *testing.T) {
	assert.Equal(t, 1000, parseStatusUID("Name:\tbash\nUid:\t1000\t1001\t1000\t1000\nGid:\t1000\n"))
	assert.Equal(t, -1, parseStatusUID("Name:\tbash\n"))
	assert.Equal(t, -1, parseStatusUID("Uid:\tx\n"))
}

// The JSON shape is the Hub's store.LiveWork.
func TestLiveWorkJSON(t *testing.T) {
	b, err := json.Marshal(LiveWork{Kind: KindProcess, SuppressStall: true, Detail: "go (pid 1, 1.0s cpu)"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"kind":"process","suppressStall":true,"detail":"go (pid 1, 1.0s cpu)"}`, string(b))
}
