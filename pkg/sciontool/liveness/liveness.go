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
// (ptone/scion#7855): a process doing CPU work that the agent is waiting on,
// such as a build, a test run or a background job, which emits no activity
// events. sciontool init samples it on every Hub heartbeat, and the Hub
// keeps an agent with live work from being marked stalled or auto-suspended.
//
// The sampler only reads /proc. It never writes agent-info.json and never
// changes the agent's activity.
package liveness

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/procreap"
)

// Kind says what kind of live work was seen.
type Kind string

// KindProcess is a process outside the baseline set whose CPU time advanced
// by at least MinCPUTicks since the previous sample.
const KindProcess Kind = "process"

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
}

// Sampler finds live work by comparing CPU times between successive walks
// of /proc. It is not safe for concurrent use; the heartbeat loop calls it
// from one goroutine.
type Sampler struct {
	agentUID  int
	isManaged func(pid int) bool
	procRoot  string
	// prev holds each process's CPU time at the previous sample; nil until
	// the first sample, which only records the baseline.
	prev map[int]procSample
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
	return &Sampler{agentUID: cfg.AgentUID, isManaged: isManaged, procRoot: "/proc"}
}

// sample walks the /proc tree at s.procRoot once and returns the live work
// it shows, or nil. Every call records the CPU times the next call compares
// against; the first call only records them and returns nil, so CPU a
// process used before sampling started (an MCP server's start-up, say) is
// never counted.
func (s *Sampler) sample() *LiveWork {
	procs := readProcs(s.procRoot)
	excluded := s.baseline(procs)

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

// baseline returns the PIDs that never count as work by themselves: PID 1,
// sciontool-managed subtrees, processes of other UIDs, zombies, tmux, tmux
// pane root processes, the harness wrapper and the harness root process,
// and sciontool invocations. Descendants of these (other than managed
// subtrees) are not excluded: a tool the harness runs counts, and so does a
// command typed in the shell window or a job reparented to PID 1.
func (s *Sampler) baseline(procs map[int]*proc) map[int]bool {
	children := make(map[int][]int, len(procs))
	for pid, p := range procs {
		children[p.ppid] = append(children[p.ppid], pid)
	}

	excluded := make(map[int]bool)
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
	// a wrapper, and a wrapper's other children are the harness root.
	var excludeWrapper func(pid int)
	excludeWrapper = func(pid int) {
		excluded[pid] = true
		for _, c := range children[pid] {
			if hasHarnessMarker(s.procRoot, c) {
				excludeWrapper(c)
			} else {
				excluded[c] = true
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
	return excluded
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
