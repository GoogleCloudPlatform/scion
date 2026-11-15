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
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
)

// Tier 1 fixtures: the container booted at fixtureBoot and the tool call
// began callAt seconds later, so a process started at the call has
// starttime callTicks.
var fixtureBoot = time.Unix(1_800_000_000, 0)

const (
	callAt    = 1000 * time.Second
	callTicks = uint64(callAt / time.Second * clockTicksPerSecond)
)

// newToolCallTree returns a fixture tree whose agent is executing a tool
// call that began callAt after boot, sampled callAt+30s after boot.
func newToolCallTree(t *testing.T) *fakeProcTree {
	f := newFakeProcTree(t, idleManaged...)
	f.boot = fixtureBoot
	f.since = fixtureBoot.Add(callAt)
	f.now = f.since.Add(30 * time.Second)
	f.act = state.ActivityExecuting
	return f
}

// ioFile is a /proc/<pid>/io body.
func ioFile(rchar, wchar uint64) string {
	return "rchar: " + strconv.FormatUint(rchar, 10) + "\nwchar: " + strconv.FormatUint(wchar, 10) +
		"\nsyscr: 1\nsyscw: 1\nread_bytes: 0\nwrite_bytes: 0\ncancelled_write_bytes: 0\n"
}

// ghWatch is a low-CPU tool process (`gh run watch`) the harness spawned
// through a shell for the current tool call.
func ghWatch(rchar uint64) []fakeProc {
	return []fakeProc{
		{pid: 80, ppid: 42, comm: "bash", uid: agentUID, ticks: 1, start: callTicks + 10, io: ioFile(100, 10)},
		{pid: 81, ppid: 80, comm: "gh", uid: agentUID, ticks: 3, start: callTicks + 20, io: ioFile(rchar, 50)},
	}
}

func TestToolCall_LowCPUChildStartedAfterCallCounts(t *testing.T) {
	f := newToolCallTree(t)
	lw := f.sample(append(idleTree(0), ghWatch(1000)...)...)
	require.NotNil(t, lw, "a live tool-call process counts on the first sample: it needs no CPU")
	assert.Equal(t, KindToolCall, lw.Kind)
	assert.True(t, lw.SuppressStall)
	assert.Contains(t, lw.Detail, "bash (pid 80, tool call running 30s")

	f.now = f.now.Add(30 * time.Second)
	lw = f.sample(append(idleTree(100), ghWatch(2000)...)...)
	require.NotNil(t, lw)
	assert.Equal(t, KindToolCall, lw.Kind)
	assert.True(t, lw.SuppressStall)
}

func TestToolCall_ProcessesStartedBeforeTheCallDoNotCount(t *testing.T) {
	// The idle tree's MCP and LSP servers are harness children started
	// long before the call.
	f := newToolCallTree(t)
	for i := uint64(0); i < 3; i++ {
		assert.Nil(t, f.sample(idleTree(i*100)...), "sample %d", i)
		f.now = f.now.Add(30 * time.Second)
	}
}

func TestToolCall_StartSlack(t *testing.T) {
	tests := []struct {
		name  string
		start uint64
		want  bool
	}{
		{"1s before the call", callTicks - 100, true},
		{"2s before the call", callTicks - 200, true},
		{"3s before the call", callTicks - 300, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newToolCallTree(t)
			child := fakeProc{pid: 80, ppid: 42, comm: "curl", uid: agentUID, ticks: 1, start: tt.start}
			lw := f.sample(append(idleTree(0), child)...)
			assert.Equal(t, tt.want, lw != nil, "got %+v", lw)
		})
	}
}

func TestToolCall_OnlyWhileExecuting(t *testing.T) {
	for _, act := range []state.Activity{state.ActivityWorking, state.ActivityThinking, state.ActivityBlocked, ""} {
		t.Run(string(act), func(t *testing.T) {
			f := newToolCallTree(t)
			f.act = act
			assert.Nil(t, f.sample(append(idleTree(0), ghWatch(1000)...)...),
				"a low-CPU process is not work outside a tool call")
		})
	}
}

func TestToolCall_OffWithoutActivityTimeOrBootTime(t *testing.T) {
	f := newToolCallTree(t)
	f.since = time.Time{}
	assert.Nil(t, f.sample(append(idleTree(0), ghWatch(1000)...)...), "no activitySince (older sciontool record)")

	f = newToolCallTree(t)
	f.boot = time.Time{}
	assert.Nil(t, f.sample(append(idleTree(0), ghWatch(1000)...)...), "no /proc/stat btime")
}

func TestToolCall_OnlyHarnessDescendants(t *testing.T) {
	f := newToolCallTree(t)
	others := []fakeProc{
		// Typed in the shell window during the call.
		{pid: 90, ppid: 50, comm: "less", uid: agentUID, ticks: 1, start: callTicks + 10},
		// A job reparented to PID 1.
		{pid: 91, ppid: 1, comm: "sleep", uid: agentUID, ticks: 0, start: callTicks + 10},
		// A zombie child of the harness.
		{pid: 92, ppid: 42, comm: "defunct", uid: agentUID, state: 'Z', ticks: 0, start: callTicks + 10},
		// A hook the harness ran for the call, and its child.
		{pid: 93, ppid: 42, comm: "sciontool", uid: agentUID, ticks: 1, start: callTicks + 10},
		// Another UID under the harness.
		{pid: 94, ppid: 42, comm: "sudo", uid: 0, ticks: 1, start: callTicks + 10},
		// A process in a managed sidecar's subtree.
		{pid: 95, ppid: 30, comm: "exporter", uid: agentUID, ticks: 1, start: callTicks + 10},
	}
	assert.Nil(t, f.sample(append(idleTree(0), others...)...))
}

func TestToolCall_LauncherHarnessRootDepth(t *testing.T) {
	// Codex: wrapper 41 -> launcher node 42 -> native TUI 43. A tool
	// under the TUI counts; the TUI itself does not.
	f := newToolCallTree(t)
	f.sampler = NewSampler(Config{AgentUID: agentUID, HarnessRootDepth: 2,
		IsManagedPID: func(pid int) bool { return pid == 20 || pid == 30 }})
	tool := fakeProc{pid: 150, ppid: 43, comm: "cargo", uid: agentUID, ticks: 2, start: callTicks + 5}
	lw := f.sample(append(codexTree(0), tool)...)
	require.NotNil(t, lw)
	assert.Equal(t, KindToolCall, lw.Kind)
	assert.Contains(t, lw.Detail, "cargo (pid 150")

	// The native TUI started after the call (a restart) is still the
	// harness, not the call's process.
	f = newToolCallTree(t)
	f.sampler = NewSampler(Config{AgentUID: agentUID, HarnessRootDepth: 2,
		IsManagedPID: func(pid int) bool { return pid == 20 || pid == 30 }})
	tree := codexTree(0)
	for i := range tree {
		if tree[i].pid == 43 {
			tree[i].start = callTicks + 5
		}
	}
	assert.Nil(t, f.sample(tree...))
}

func TestToolCall_CapStopsSuppressingButStillReports(t *testing.T) {
	f := newToolCallTree(t)
	rchar := uint64(1000)
	// Progressing all along (I/O moves every sample).
	for f.now.Sub(f.since) < ToolCallStallCap {
		lw := f.sample(append(idleTree(0), ghWatch(rchar)...)...)
		require.NotNil(t, lw)
		require.True(t, lw.SuppressStall, "at %s", f.now.Sub(f.since))
		rchar += 100
		f.now = f.now.Add(30 * time.Second)
	}
	lw := f.sample(append(idleTree(0), ghWatch(rchar)...)...)
	require.NotNil(t, lw, "past the cap the live process is still reported")
	assert.Equal(t, KindToolCall, lw.Kind)
	assert.False(t, lw.SuppressStall, "past the cap it no longer suppresses STALLED")
	assert.Contains(t, lw.Detail, "past the cap")
}

func TestToolCall_CPUWorkPastTheCapStillSuppresses(t *testing.T) {
	// A CPU-busy build in a tool call longer than the cap is Tier 2 work,
	// which is never capped (acceptance criterion 1).
	f := newToolCallTree(t)
	f.now = f.since.Add(ToolCallStallCap + time.Minute)
	build := fakeProc{pid: 80, ppid: 42, comm: "go", uid: agentUID, ticks: 100, start: callTicks + 10}
	lw := f.sample(append(idleTree(0), build)...)
	require.NotNil(t, lw)
	assert.False(t, lw.SuppressStall, "first sample: no CPU baseline yet, only the capped call")

	build.ticks += 500
	f.now = f.now.Add(30 * time.Second)
	lw = f.sample(append(idleTree(100), build)...)
	require.NotNil(t, lw)
	assert.Equal(t, KindProcess, lw.Kind)
	assert.True(t, lw.SuppressStall)
}

func TestToolCall_TierOneKeepsTheCPUBaseline(t *testing.T) {
	// While Tier 1 wins, CPU times are still recorded, so a process's CPU
	// before the call ended is not counted again afterwards.
	f := newToolCallTree(t)
	bg := fakeProc{pid: 91, ppid: 1, comm: "watcher", uid: agentUID, ticks: 5000, start: 50}
	lw := f.sample(append(idleTree(0), append(ghWatch(1000), bg)...)...)
	require.NotNil(t, lw)
	require.Equal(t, KindToolCall, lw.Kind)

	f.act = state.ActivityWorking
	f.now = f.now.Add(30 * time.Second)
	assert.Nil(t, f.sample(append(idleTree(100), bg)...), "the background process used no CPU since the last sample")
}

// noProgressRun samples procs (fixed) every 30s from the call's start and
// returns the first offset at which SuppressStall turned false, or -1.
func noProgressRun(t *testing.T, f *fakeProcTree, procs func(i int) []fakeProc, limit time.Duration) time.Duration {
	t.Helper()
	f.now = f.since
	for i := 0; f.now.Sub(f.since) <= limit; i++ {
		lw := f.sample(append(idleTree(uint64(i)*100), procs(i)...)...)
		require.NotNil(t, lw, "sample %d", i)
		require.Equal(t, KindToolCall, lw.Kind)
		if !lw.SuppressStall {
			return f.now.Sub(f.since)
		}
		f.now = f.now.Add(30 * time.Second)
	}
	return -1
}

func TestToolCall_NoProgressWindow(t *testing.T) {
	stuck := func(int) []fakeProc { return ghWatch(1000) }
	f := newToolCallTree(t)
	assert.Equal(t, ToolCallNoProgress, noProgressRun(t, f, stuck, ToolCallStallCap),
		"an unchanged fingerprint stops suppressing after the window")

	// The detail says why.
	lw := f.sample(append(idleTree(0), ghWatch(1000)...)...)
	require.NotNil(t, lw)
	assert.Contains(t, lw.Detail, "no progress for 10m")
}

func TestToolCall_ProgressResetsTheWindow(t *testing.T) {
	tests := []struct {
		name  string
		procs func(i int) []fakeProc
	}{
		{"I/O", func(i int) []fakeProc { return ghWatch(1000 + uint64(i)) }},
		{"CPU", func(i int) []fakeProc {
			p := ghWatch(1000)
			p[1].ticks += uint64(i)
			return p
		}},
		{"PID set", func(i int) []fakeProc {
			// A short-lived child appears every few samples.
			p := ghWatch(1000)
			if i%5 == 0 {
				p = append(p, fakeProc{pid: 200 + i, ppid: 81, comm: "git", uid: agentUID, ticks: 0,
					start: callTicks + uint64(i)*3000, io: ioFile(0, 0)})
			}
			return p
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newToolCallTree(t)
			assert.Equal(t, ToolCallStallCap, noProgressRun(t, f, tt.procs, ToolCallStallCap),
				"a progressing call suppresses until the cap")
		})
	}
}

func TestToolCall_UnreadableIOFallsBackToPIDsAndCPU(t *testing.T) {
	noIO := func(cpu func(i int) uint64) func(i int) []fakeProc {
		return func(i int) []fakeProc {
			p := ghWatch(0)
			for j := range p {
				p[j].io = ""
			}
			p[1].ticks = cpu(i)
			return p
		}
	}
	f := newToolCallTree(t)
	assert.Equal(t, ToolCallNoProgress, noProgressRun(t, f, noIO(func(int) uint64 { return 3 }), ToolCallStallCap),
		"without I/O, unchanged PIDs and CPU are no progress")

	f = newToolCallTree(t)
	assert.Equal(t, ToolCallStallCap, noProgressRun(t, f, noIO(func(i int) uint64 { return 3 + uint64(i) }), ToolCallStallCap),
		"without I/O, CPU still counts as progress")
}

func TestToolCall_NewCallResetsTheWindow(t *testing.T) {
	f := newToolCallTree(t)
	stuck := append(idleTree(0), ghWatch(1000)...)
	f.now = f.since.Add(9 * time.Minute)
	require.True(t, f.sample(stuck...).SuppressStall)
	f.now = f.since.Add(ToolCallNoProgress)
	require.True(t, f.sample(stuck...).SuppressStall, "the window starts at the first sample that saw the call")
	f.now = f.since.Add(9*time.Minute + ToolCallNoProgress)
	require.False(t, f.sample(stuck...).SuppressStall)

	// The next tool call (a new activity time) starts a fresh window and
	// cap, even with the same processes still around.
	f.since = f.now.Add(-time.Second)
	for i := range stuck {
		if stuck[i].pid == 80 || stuck[i].pid == 81 {
			stuck[i].start = uint64(f.since.Sub(fixtureBoot)/time.Second*clockTicksPerSecond) + 50
		}
	}
	lw := f.sample(stuck...)
	require.NotNil(t, lw)
	assert.True(t, lw.SuppressStall)
}

func TestToolCall_EndedCallStopsReporting(t *testing.T) {
	f := newToolCallTree(t)
	require.NotNil(t, f.sample(append(idleTree(0), ghWatch(1000)...)...))
	f.now = f.now.Add(30 * time.Second)
	assert.Nil(t, f.sample(idleTree(100)...), "the tool process exited")
}

func TestReadIOChars(t *testing.T) {
	root := t.TempDir()
	write := func(pid, body string) {
		require.NoError(t, os.MkdirAll(filepath.Join(root, pid), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(root, pid, "io"), []byte(body), 0o644))
	}
	write("10", ioFile(1234, 66))
	write("11", "syscr: 4\n")
	write("12", "rchar: x\nwchar: 1\n")

	n, ok := readIOChars(root, 10)
	assert.True(t, ok)
	assert.Equal(t, uint64(1300), n)
	_, ok = readIOChars(root, 11)
	assert.False(t, ok, "no rchar/wchar")
	_, ok = readIOChars(root, 12)
	assert.False(t, ok, "unparsable")
	_, ok = readIOChars(root, 13)
	assert.False(t, ok, "missing")
}

func TestReadBootTime(t *testing.T) {
	root := t.TempDir()
	assert.True(t, readBootTime(root).IsZero(), "missing stat")
	require.NoError(t, os.WriteFile(filepath.Join(root, "stat"), []byte("cpu 1 2\nbtime 1700000000\n"), 0o644))
	assert.Equal(t, time.Unix(1700000000, 0), readBootTime(root))
	require.NoError(t, os.WriteFile(filepath.Join(root, "stat"), []byte("cpu 1 2\nbtime nope\n"), 0o644))
	assert.True(t, readBootTime(root).IsZero())
}

// The boot time is read from /proc/stat once and cached: a later sample
// whose /proc has no stat file still dates the tool call's processes.
func TestToolCall_BootTimeIsCached(t *testing.T) {
	f := newToolCallTree(t)
	require.NotNil(t, f.sample(append(idleTree(0), ghWatch(1000)...)...))
	f.boot = time.Time{} // the next fixture writes no /proc/stat
	f.now = f.now.Add(30 * time.Second)
	lw := f.sample(append(idleTree(100), ghWatch(2000)...)...)
	require.NotNil(t, lw)
	assert.Equal(t, KindToolCall, lw.Kind)
}
