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

// Package liveness detects live work in an agent container
// (ptone/scion#7855): a tool call whose process is still running (Tier 1),
// or a process doing CPU work that the agent is waiting on, such as a
// build, a test run or a background job (Tier 2). Neither emits activity
// events. sciontool init samples it on every Hub heartbeat, and the Hub
// keeps an agent with live work from being marked stalled or auto-suspended.
//
// The sampler only reads /proc. It never writes agent-info.json and never
// changes the agent's activity.
package liveness

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/procreap"
)

// Kind says what kind of live work was seen.
type Kind string

const (
	// KindToolCall (Tier 1) is a tool call in flight: the activity is
	// executing and a descendant of the harness started at or after the
	// activity began. It needs no CPU.
	KindToolCall Kind = "tool"
	// KindProcess (Tier 2) is a process outside the baseline set whose CPU
	// time advanced by at least MinCPUTicks since the previous sample.
	KindProcess Kind = "process"
	// KindAttached reports an interactive terminal client attached to the
	// tmux session (shell execution containers only; guards idle-suspend).
	KindAttached Kind = "attached"
)

// ToolCallStallCap is how long after a tool call began its live process
// keeps suppressing STALLED. Past it the sampler still reports the process,
// with SuppressStall false: the agent can be marked stalled, so a
// supervisor takes a look, but is still never auto-suspended.
const ToolCallStallCap = 60 * time.Minute

// ToolCallNoProgress is how long the tool call's process tree may go
// without progress before it stops suppressing STALLED, ahead of
// ToolCallStallCap. Progress is a change of its fingerprint: the set of
// processes, their total CPU time, or their total I/O (rchar+wchar, which
// counts every read and write syscall, sockets and pipes included). A tool
// blocked on a prompt, a dead socket or a sleep makes none.
const ToolCallNoProgress = 10 * time.Minute

// toolStartSlack is how long before the activity's timestamp a process may
// have started and still count as the tool call's: the hook that records
// the activity runs as the harness spawns the tool, and /proc/stat's boot
// time has one-second resolution.
const toolStartSlack = 2 * time.Second

// LiveWork is the live work one sample found. Its JSON shape is the
// heartbeat's "liveWork" field (store.LiveWork on the Hub).
type LiveWork struct {
	Kind Kind `json:"kind"`
	// SuppressStall reports whether this work keeps the agent from being
	// marked stalled. Work that does not still protects it from
	// auto-suspend.
	SuppressStall bool `json:"suppressStall,omitempty"`
	// Detail names the busiest process: its comm, pid and the CPU time it
	// used since the previous sample. Never the full command line.
	Detail string `json:"detail,omitempty"`
}

// MinCPUTicks is the CPU time, in clock ticks (USER_HZ, 1/100 s), a process
// must use between two samples to count as work: 300 ms per 30 s heartbeat,
// about 1% of one core. Idle MCP/LSP servers, dev servers and shells stay
// below it.
const MinCPUTicks = 30

// clockTicksPerSecond is USER_HZ, the unit of the /proc/<pid>/stat CPU
// times. The kernel fixes it at 100 for user space on every architecture
// Linux containers run on.
const clockTicksPerSecond = 100

// Config configures a Sampler.
type Config struct {
	// AgentUID is the UID the agent's processes run as. Processes owned by
	// any other UID are never work.
	AgentUID int
	// IsManagedPID reports whether a PID is a child process sciontool
	// started itself; its whole subtree is excluded. Defaults to
	// procreap.IsManagedPID.
	IsManagedPID func(pid int) bool
	// HarnessRootDepth is how many process levels below the agent window's
	// wrapper are the harness itself and never count as work: 1 (the
	// default, also used for any value below 1) when the harness command
	// is the TUI, 2 when it is a launcher that spawns the TUI as a child
	// (Codex). Capped at MaxHarnessRootDepth.
	HarnessRootDepth int
	// CheckAttached, when non-nil, reports whether an interactive terminal
	// client is attached to the container's tmux session. Scoped to shell
	// execution containers (ptone/scion#8812 §3.3).
	CheckAttached func() bool
}

// MaxHarnessRootDepth caps Config.HarnessRootDepth: a deeper launcher
// chain would start excluding the tools the harness runs.
const MaxHarnessRootDepth = 4

// Sampler finds live work by comparing CPU times between successive walks
// of /proc. It is not safe for concurrent use; the heartbeat loop calls it
// from one goroutine.
type Sampler struct {
	agentUID      int
	isManaged     func(pid int) bool
	checkAttached func() bool
	rootDepth     int
	procRoot      string
	// prev holds each process's CPU time at the previous sample; nil until
	// the first sample, which only records the baseline.
	prev map[int]procSample
	// tool tracks the progress of the tool call Tier 1 last saw.
	tool toolProgress
	// boot caches the boot time from /proc/stat once it has been read; it
	// does not change while the container runs.
	boot time.Time
}

// toolProgress is the last fingerprint of a tool call's process tree and
// when it last changed.
type toolProgress struct {
	since     time.Time // the activity timestamp that identifies the call
	print     toolPrint
	changedAt time.Time
}

// toolPrint is the progress fingerprint of a tool call's process tree.
type toolPrint struct {
	pids  string // sorted pid:starttime list
	ticks uint64 // total utime+stime
	io    uint64 // total rchar+wchar; 0 when ioOK is false
	ioOK  bool   // every process's /proc/<pid>/io was readable
}

// procSample is what the next sample compares against. starttime tells a
// reused PID from the process that had it before.
type procSample struct {
	start uint64
	ticks uint64
}

// proc is one /proc/<pid> entry.
type proc struct {
	pid   int
	ppid  int
	state byte
	comm  string
	ticks uint64 // utime + stime
	start uint64 // starttime, clock ticks after boot
	uid   int    // real UID; -1 when unreadable
}

// NewSampler returns a sampler for the container's /proc.
func NewSampler(cfg Config) *Sampler {
	isManaged := cfg.IsManagedPID
	if isManaged == nil {
		isManaged = procreap.IsManagedPID
	}
	depth := min(max(cfg.HarnessRootDepth, 1), MaxHarnessRootDepth)
	return &Sampler{
		agentUID:      cfg.AgentUID,
		isManaged:     isManaged,
		checkAttached: cfg.CheckAttached,
		rootDepth:     depth,
		procRoot:      "/proc",
	}
}

// sample walks the /proc tree at s.procRoot once and returns the live work
// it shows, or nil. act and actSince are the agent's current activity and
// when it began (zero when unknown, which turns Tier 1 off).
//
// Tier 1 suppressing STALLED wins. Otherwise CPU work (Tier 2, never
// capped) does, so a CPU-busy build past the tool-call cap still keeps the
// agent from stalling. Otherwise a capped or non-progressing tool call is
// reported with SuppressStall false, which still guards auto-suspend.
//
// Every call records the CPU times the next call compares against; the
// first call only records them and reports no Tier 2 work, so CPU a process
// used before sampling started (an MCP server's start-up, say) is never
// counted.
func (s *Sampler) sample(now time.Time, act state.Activity, actSince time.Time) *LiveWork {
	procs := readProcs(s.procRoot)
	excluded, harness := s.baseline(procs)
	tool := s.toolCall(procs, excluded, harness, now, act, actSince)
	if tool != nil && tool.SuppressStall {
		s.recordCPU(procs)
		return tool
	}
	if work := s.cpuWork(procs, excluded); work != nil {
		return work
	}
	if tool != nil {
		return tool
	}
	if s.checkAttached != nil && s.checkAttached() {
		return &LiveWork{
			Kind:          KindAttached,
			SuppressStall: false,
			Detail:        "terminal attached",
		}
	}
	return nil
}

// recordCPU records every process's CPU time for the next sample's Tier 2
// comparison without evaluating it.
func (s *Sampler) recordCPU(procs map[int]*proc) {
	next := make(map[int]procSample, len(procs))
	for pid, p := range procs {
		next[pid] = procSample{start: p.start, ticks: p.ticks}
	}
	s.prev = next
}

// cpuWork is Tier 2: the busiest non-excluded process whose CPU time
// advanced by at least MinCPUTicks since the previous sample, or nil. It
// records the CPU times the next call compares against.
func (s *Sampler) cpuWork(procs map[int]*proc, excluded map[int]bool) *LiveWork {
	next := make(map[int]procSample, len(procs))
	var best *proc
	var bestDelta uint64
	for pid, p := range procs {
		next[pid] = procSample{start: p.start, ticks: p.ticks}
		if s.prev == nil || excluded[pid] {
			continue
		}
		// A process first seen now (new, or a reused PID) counts from its
		// start, so a job born between two samples is not missed.
		delta := p.ticks
		if q, ok := s.prev[pid]; ok && q.start == p.start {
			delta = 0
			if p.ticks > q.ticks {
				delta = p.ticks - q.ticks
			}
		}
		if delta < MinCPUTicks {
			continue
		}
		if best == nil || delta > bestDelta || (delta == bestDelta && pid < best.pid) {
			best, bestDelta = p, delta
		}
	}
	first := s.prev == nil
	s.prev = next
	if first || best == nil {
		return nil
	}
	return &LiveWork{
		Kind:          KindProcess,
		SuppressStall: true,
		Detail: fmt.Sprintf("%s (pid %d, %.1fs cpu)", best.comm, best.pid,
			float64(bestDelta)/clockTicksPerSecond),
	}
}

// toolCall is Tier 1: when act is executing, the processes descended from
// the harness that started at or after actSince (less toolStartSlack) are
// the tool call's. It reports them, and whether they still suppress STALLED
// (ToolCallStallCap, ToolCallNoProgress), or returns nil when there are
// none. It also tracks the tree's progress fingerprint across samples.
func (s *Sampler) toolCall(procs map[int]*proc, excluded, harness map[int]bool, now time.Time, act state.Activity, actSince time.Time) *LiveWork {
	if act != state.ActivityExecuting || actSince.IsZero() || len(harness) == 0 {
		s.tool = toolProgress{}
		return nil
	}
	if s.boot.IsZero() {
		s.boot = readBootTime(s.procRoot)
	}
	boot := s.boot
	if boot.IsZero() {
		s.tool = toolProgress{}
		return nil
	}
	minStart := uint64(0)
	if d := actSince.Add(-toolStartSlack).Sub(boot); d > 0 {
		minStart = uint64(d / (time.Second / clockTicksPerSecond))
	}

	var tree []*proc
	for pid, p := range procs {
		if excluded[pid] || p.start < minStart || !descendsFrom(procs, pid, harness) {
			continue
		}
		tree = append(tree, p)
	}
	if len(tree) == 0 {
		s.tool = toolProgress{}
		return nil
	}
	// The oldest process is the one the harness spawned for the call.
	slices.SortFunc(tree, func(a, b *proc) int {
		if a.start != b.start {
			return compareUint(a.start, b.start)
		}
		return a.pid - b.pid
	})

	fp := s.toolPrint(tree)
	if !s.tool.since.Equal(actSince) || s.tool.print != fp {
		s.tool = toolProgress{since: actSince, print: fp, changedAt: now}
	}
	running := now.Sub(actSince)
	stalled := now.Sub(s.tool.changedAt)
	work := &LiveWork{Kind: KindToolCall, SuppressStall: true}
	note := ""
	switch {
	case running >= ToolCallStallCap:
		work.SuppressStall = false
		note = ", past the cap"
	case stalled >= ToolCallNoProgress:
		work.SuppressStall = false
		note = fmt.Sprintf(", no progress for %s", stalled.Truncate(time.Second))
	}
	work.Detail = fmt.Sprintf("%s (pid %d, tool call running %s%s)", tree[0].comm, tree[0].pid,
		running.Truncate(time.Second), note)
	return work
}

// toolPrint computes the progress fingerprint of a tool call's processes.
// When any /proc/<pid>/io is unreadable (a hardened runtime, or a process
// that just exited), I/O is left out and the PID set and CPU time remain.
func (s *Sampler) toolPrint(tree []*proc) toolPrint {
	var b strings.Builder
	fp := toolPrint{ioOK: true}
	for _, p := range tree {
		fmt.Fprintf(&b, "%d:%d,", p.pid, p.start)
		fp.ticks += p.ticks
		if fp.ioOK {
			n, ok := readIOChars(s.procRoot, p.pid)
			fp.io += n
			fp.ioOK = ok
		}
	}
	fp.pids = b.String()
	if !fp.ioOK {
		fp.io = 0
	}
	return fp
}

// descendsFrom reports whether an ancestor of pid is in set.
func descendsFrom(procs map[int]*proc, pid int, set map[int]bool) bool {
	// The depth bound guards against a parent cycle in a torn read.
	for depth := 0; depth < 64; depth++ {
		p, ok := procs[pid]
		if !ok || p.ppid <= 0 {
			return false
		}
		if set[p.ppid] {
			return true
		}
		pid = p.ppid
	}
	return false
}

func compareUint(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// baseline returns the PIDs that never count as work by themselves: PID 1,
// sciontool-managed subtrees, processes of other UIDs, zombies, tmux, tmux
// pane root processes, the harness wrapper and the harness root process,
// and sciontool invocations. Descendants of these (other than managed
// subtrees) are not excluded: a tool the harness runs counts, and so does a
// command typed in the shell window or a job reparented to PID 1. harness
// holds the harness root and its launcher levels (Config.HarnessRootDepth),
// whose descendants Tier 1 looks at.
func (s *Sampler) baseline(procs map[int]*proc) (excluded, harness map[int]bool) {
	children := make(map[int][]int, len(procs))
	for pid, p := range procs {
		children[p.ppid] = append(children[p.ppid], pid)
	}

	excluded = make(map[int]bool)
	harness = make(map[int]bool)
	for pid, p := range procs {
		switch {
		case pid == 1,
			p.state == 'Z' || p.state == 'X',
			p.uid != s.agentUID,
			isTmux(p.comm),
			p.comm == "sciontool",
			s.inManagedSubtree(procs, pid):
			excluded[pid] = true
		}
	}

	// Pane roots are the direct children of a tmux process: the agent
	// window's wrapper and the shell window's login shell. In the agent
	// window, tmux's default shell may run the wrapper `sh -c '<harness>;
	// echo $? > <exit-code file>'` as a child rather than exec it, so every
	// process in the chain whose command line names the exit-code file is
	// a wrapper, and a wrapper's other children are the harness root. A
	// harness launched through a launcher (Config.HarnessRootDepth > 1)
	// also has the launcher's descendants down to that depth excluded.
	// visited stops the walk on a parent cycle, which a torn read of /proc
	// during PID reuse can show: a process is walked at most once.
	visited := make(map[int]bool)
	var excludeHarness func(pid, depth int)
	excludeHarness = func(pid, depth int) {
		if visited[pid] {
			return
		}
		visited[pid] = true
		excluded[pid] = true
		harness[pid] = true
		if depth > 1 {
			for _, c := range children[pid] {
				excludeHarness(c, depth-1)
			}
		}
	}
	var excludeWrapper func(pid int)
	excludeWrapper = func(pid int) {
		if visited[pid] {
			return
		}
		visited[pid] = true
		excluded[pid] = true
		for _, c := range children[pid] {
			if hasHarnessMarker(s.procRoot, c) {
				excludeWrapper(c)
			} else {
				excludeHarness(c, s.rootDepth)
			}
		}
	}
	for pid, p := range procs {
		parent, ok := procs[p.ppid]
		if !ok || !isTmux(parent.comm) || isTmux(p.comm) {
			continue
		}
		excluded[pid] = true
		if hasHarnessMarker(s.procRoot, pid) {
			excludeWrapper(pid)
		}
	}
	return excluded, harness
}

// inManagedSubtree reports whether pid or any of its ancestors below PID 1
// is a sciontool-managed child process.
func (s *Sampler) inManagedSubtree(procs map[int]*proc, pid int) bool {
	// The depth bound guards against a parent cycle in a torn read.
	for depth := 0; pid > 1 && depth < 64; depth++ {
		if s.isManaged(pid) {
			return true
		}
		p, ok := procs[pid]
		if !ok {
			return false
		}
		pid = p.ppid
	}
	return false
}

// isTmux reports whether comm is a tmux server or client. tmux renames
// itself "tmux: server" and "tmux: client".
func isTmux(comm string) bool {
	return strings.HasPrefix(comm, "tmux")
}

// hasHarnessMarker reports whether the command line of pid names the
// harness exit-code file that only the agent window's wrapper writes.
func hasHarnessMarker(procRoot string, pid int) bool {
	b, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "cmdline"))
	return err == nil && strings.Contains(string(b), state.HarnessExitCodeFile)
}

// readBootTime returns the boot time from root/stat's btime line, or zero.
// /proc/<pid>/stat start times count clock ticks from it.
func readBootTime(root string) time.Time {
	f, err := os.Open(filepath.Join(root, "stat"))
	if err != nil {
		return time.Time{}
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		rest, ok := strings.CutPrefix(sc.Text(), "btime ")
		if !ok {
			continue
		}
		sec, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
		if err != nil || sec <= 0 {
			return time.Time{}
		}
		return time.Unix(sec, 0)
	}
	return time.Time{}
}

// readIOChars returns rchar+wchar from root/<pid>/io, and false when the
// file is unreadable or has neither field. Reading another user's io file
// needs ptrace access, which sciontool init (root) has.
func readIOChars(root string, pid int) (uint64, bool) {
	b, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "io"))
	if err != nil {
		return 0, false
	}
	var total uint64
	found := false
	for _, line := range strings.Split(string(b), "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok || (key != "rchar" && key != "wchar") {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSpace(val), 10, 64)
		if err != nil {
			return 0, false
		}
		total += n
		found = true
	}
	return total, found
}

// readProcs reads every /proc/<pid> entry under root. A process that exits
// mid-walk is skipped.
func readProcs(root string) map[int]*proc {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	procs := make(map[int]*proc, len(entries))
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		dir := filepath.Join(root, e.Name())
		stat, err := os.ReadFile(filepath.Join(dir, "stat"))
		if err != nil {
			continue
		}
		p, ok := parseStat(string(stat))
		if !ok || p.pid != pid {
			continue
		}
		p.uid = -1
		if status, err := os.ReadFile(filepath.Join(dir, "status")); err == nil {
			p.uid = parseStatusUID(string(status))
		}
		procs[pid] = p
	}
	return procs
}

// parseStat parses a /proc/<pid>/stat line. The comm field is
// parenthesized and may itself contain spaces or parentheses, so the fields
// after it are found from the last ')'.
func parseStat(stat string) (*proc, bool) {
	open := strings.IndexByte(stat, '(')
	end := strings.LastIndexByte(stat, ')')
	if open < 0 || end < open {
		return nil, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(stat[:open]))
	if err != nil {
		return nil, false
	}
	// Fields after comm, 0-based: state(0) ppid(1) ... utime(11) stime(12)
	// ... starttime(19).
	f := strings.Fields(stat[end+1:])
	if len(f) < 20 || len(f[0]) != 1 {
		return nil, false
	}
	ppid, err1 := strconv.Atoi(f[1])
	utime, err2 := strconv.ParseUint(f[11], 10, 64)
	stime, err3 := strconv.ParseUint(f[12], 10, 64)
	start, err4 := strconv.ParseUint(f[19], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
		return nil, false
	}
	return &proc{
		pid:   pid,
		ppid:  ppid,
		state: f[0][0],
		comm:  stat[open+1 : end],
		ticks: utime + stime,
		start: start,
	}, true
}

// parseStatusUID returns the real UID from a /proc/<pid>/status file, or -1.
func parseStatusUID(status string) int {
	for _, line := range strings.Split(status, "\n") {
		rest, ok := strings.CutPrefix(line, "Uid:")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			return -1
		}
		uid, err := strconv.Atoi(f[0])
		if err != nil {
			return -1
		}
		return uid
	}
	return -1
}
