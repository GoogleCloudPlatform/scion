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

// Package hostlimits checks kernel limits on a broker host that bound how
// many agents it can run. It only reads; it never changes a limit and never
// fails a caller.
//
// The inotify check exists because agents on a Docker or Podman broker run
// as the broker's host UID (SCION_HOST_UID, no user-namespace remap), so
// every agent's file watchers count against that one user's
// fs.inotify.max_user_instances and max_user_watches. Scion itself creates
// no inotify instances; the harness processes inside the agents do.
package hostlimits

import (
	"bufio"
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"runtime"
	"strconv"
	"strings"
)

const (
	// DefaultExpectedAgents is the agent count the inotify limits are
	// judged against when the caller has no better figure: the Hub's
	// seeded default for max_agents_per_broker. A broker does not see its
	// Hub-side limit at startup, so it uses this default.
	DefaultExpectedAgents = 100

	// The per-agent allowances are judgment constants from two data
	// points. A host on the kernel default max_user_instances=128 ran out
	// at about 150 agents: about 0.85 instances per agent, the user's own
	// processes included. One running Claude Code harness, measured,
	// held 1 inotify instance and 996 watches on a large repository.
	//
	// Instances: 1 per agent (the measured value and a ceiling of the
	// observed 0.85) plus a reserve of 128 (the kernel default) for the
	// user's processes that are not agents: 228 for 100 agents, so a host
	// on the default warns at the default capacity, as the observed host
	// would have.
	//
	// Watches did not run out on that host: 1024 per agent (the measured
	// 996, rounded up) plus a reserve of 8192 (the old kernel default):
	// 110592 for 100 agents. Recursive watchers on very large workspaces
	// can need more.

	// InotifyInstancesPerAgent is the inotify instance allowance per agent.
	InotifyInstancesPerAgent = 1
	// InotifyInstancesReserve is kept for the user's processes that are not
	// agents (for example the systemd user manager).
	InotifyInstancesReserve = 128

	// InotifyWatchesPerAgent is the inotify watch allowance per agent.
	InotifyWatchesPerAgent = 1024
	// InotifyWatchesReserve is kept for the user's processes that are not
	// agents.
	InotifyWatchesReserve = 8192

	// RecommendedInotifyInstances and RecommendedInotifyWatches are the
	// values the warning suggests.
	RecommendedInotifyInstances = 8192
	RecommendedInotifyWatches   = 524288

	// Paths inside the /proc file system (Reader.Proc).
	maxUserInstancesPath = "sys/fs/inotify/max_user_instances"
	maxUserWatchesPath   = "sys/fs/inotify/max_user_watches"

	maxUserInstancesKey = "fs.inotify.max_user_instances"
	maxUserWatchesKey   = "fs.inotify.max_user_watches"
)

// Doctor statuses, the values scion doctor uses.
const (
	StatusPass = "pass"
	StatusWarn = "warn"
	StatusSkip = "skip"
)

// Reader is the read-only system access the checks need. Tests replace
// both fields; DefaultReader reads the real system.
type Reader struct {
	// GOOS is the operating system; the checks apply only to "linux".
	GOOS string
	// Proc is the /proc file system: the limits are read from
	// sys/fs/inotify and the usage scan walks the process directories.
	// Nil makes every read fail.
	Proc fs.FS
}

// DefaultReader reads this host.
func DefaultReader() Reader {
	return Reader{GOOS: runtime.GOOS, Proc: os.DirFS("/proc")}
}

// MinInotifyInstances is the max_user_instances value below which the
// check warns for the given agent count.
func MinInotifyInstances(agents int) int64 {
	return int64(agents)*InotifyInstancesPerAgent + InotifyInstancesReserve
}

// MinInotifyWatches is the max_user_watches value below which the check
// warns for the given agent count.
func MinInotifyWatches(agents int) int64 {
	return int64(agents)*InotifyWatchesPerAgent + InotifyWatchesReserve
}

// Limit is one sysctl value; Known is false when it could not be read.
type Limit struct {
	Value int64
	Known bool
}

// InotifyResult is the outcome of CheckInotify.
type InotifyResult struct {
	// Applicable is false off Linux, or when neither limit could be read.
	Applicable bool
	// Reason says why the check does not apply.
	Reason string
	// Agents is the expected agent count the limits are judged against.
	Agents    int
	Instances Limit
	Watches   Limit
}

// CheckInotify reads the per-user inotify limits and judges them against
// agents expected agents (DefaultExpectedAgents when agents is not
// positive). It never fails: a value that cannot be read is unknown, and
// with neither value known the check does not apply.
func CheckInotify(r Reader, agents int) InotifyResult {
	if agents <= 0 {
		agents = DefaultExpectedAgents
	}
	if r.GOOS != "linux" {
		return InotifyResult{Agents: agents, Reason: "not a Linux host"}
	}
	res := InotifyResult{
		Agents:    agents,
		Instances: readLimit(r, maxUserInstancesPath),
		Watches:   readLimit(r, maxUserWatchesPath),
	}
	if !res.Instances.Known && !res.Watches.Known {
		res.Reason = "/proc/sys/fs/inotify is not readable"
		return res
	}
	res.Applicable = true
	return res
}

func readLimit(r Reader, path string) Limit {
	if r.Proc == nil {
		return Limit{}
	}
	b, err := fs.ReadFile(r.Proc, path)
	if err != nil {
		return Limit{}
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return Limit{}
	}
	return Limit{Value: v, Known: true}
}

// InstancesLow reports whether max_user_instances is known and below
// MinInotifyInstances for the expected agent count.
func (res InotifyResult) InstancesLow() bool {
	return res.Applicable && res.Instances.Known && res.Instances.Value < MinInotifyInstances(res.Agents)
}

// WatchesLow reports whether max_user_watches is known and below
// MinInotifyWatches for the expected agent count.
func (res InotifyResult) WatchesLow() bool {
	return res.Applicable && res.Watches.Known && res.Watches.Value < MinInotifyWatches(res.Agents)
}

// lowParts describes each low limit.
func (res InotifyResult) lowParts() []string {
	var parts []string
	if res.InstancesLow() {
		parts = append(parts, fmt.Sprintf("%s is %d (want at least %d)", maxUserInstancesKey, res.Instances.Value, MinInotifyInstances(res.Agents)))
	}
	if res.WatchesLow() {
		parts = append(parts, fmt.Sprintf("%s is %d (want at least %d)", maxUserWatchesKey, res.Watches.Value, MinInotifyWatches(res.Agents)))
	}
	return parts
}

// Remediation is the fix for the low limits: the sysctl command and the
// file that keeps it across reboots. It is "" when no limit is low.
func (res InotifyResult) Remediation() string {
	var settings []string
	if res.InstancesLow() {
		settings = append(settings, fmt.Sprintf("%s=%d", maxUserInstancesKey, RecommendedInotifyInstances))
	}
	if res.WatchesLow() {
		settings = append(settings, fmt.Sprintf("%s=%d", maxUserWatchesKey, RecommendedInotifyWatches))
	}
	if len(settings) == 0 {
		return ""
	}
	return fmt.Sprintf("Raise the limits with 'sudo sysctl -w %s' and keep them across reboots in /etc/sysctl.d/99-scion-inotify.conf (one setting per line)",
		strings.Join(settings, " "))
}

// Warning is the broker startup warning, or "" when the limits are fine,
// unknown or the check does not apply.
func (res InotifyResult) Warning() string {
	parts := res.lowParts()
	if len(parts) == 0 {
		return ""
	}
	return fmt.Sprintf("inotify limits are low for %s, which run as this broker's user: %s; agents' file watchers can fail with \"Too many open files\". %s",
		res.agentsText(), strings.Join(parts, ", "), res.Remediation())
}

// agentsText names the agent count the limits were judged against, and
// says when it is the default capacity rather than a configured one.
func (res InotifyResult) agentsText() string {
	if res.Agents == DefaultExpectedAgents {
		return fmt.Sprintf("about %d agents (assuming the default capacity, max_agents_per_broker %d)", res.Agents, DefaultExpectedAgents)
	}
	return fmt.Sprintf("about %d agents", res.Agents)
}

// Doctor returns the scion doctor status, message and remediation.
func (res InotifyResult) Doctor() (status, message, remediation string) {
	if !res.Applicable {
		return StatusSkip, "Not applicable (" + res.Reason + ")", ""
	}
	if parts := res.lowParts(); len(parts) > 0 {
		return StatusWarn, fmt.Sprintf("Low for %s: %s", res.agentsText(), strings.Join(parts, ", ")), res.Remediation()
	}
	return StatusPass, fmt.Sprintf("%s %s, %s %s (enough for %s)",
		maxUserInstancesKey, limitText(res.Instances), maxUserWatchesKey, limitText(res.Watches), res.agentsText()), ""
}

func limitText(l Limit) string {
	if !l.Known {
		return "unknown"
	}
	return strconv.FormatInt(l.Value, 10)
}

// InotifyUsage is the inotify use of the processes of one effective UID,
// counted from /proc.
type InotifyUsage struct {
	Instances int
	Watches   int
	// Unreadable counts processes of the UID whose descriptors could not
	// be read.
	Unreadable int
}

// String renders the usage for doctor output. The counts are approximate:
// a descriptor shared by several processes (inherited or duplicated) is
// counted once for each, and unreadable processes are not counted.
func (u InotifyUsage) String() string {
	s := fmt.Sprintf("approximately %d instances, %d watches (a descriptor shared by several processes is counted once per process", u.Instances, u.Watches)
	if u.Unreadable > 0 {
		s += fmt.Sprintf("; %d processes not readable", u.Unreadable)
	}
	return s + ")"
}

// CountInotifyUsage counts the inotify instances and watches held by the
// processes whose effective UID is uid (the kernel charges an instance to
// its creator's effective UID). A descriptor is an inotify instance when
// its link reads "anon_inode:inotify"; each "inotify wd:" line of its
// fdinfo is one watch. It returns false when proc is nil or cannot be
// listed.
func CountInotifyUsage(proc fs.FS, uid int) (InotifyUsage, bool) {
	var u InotifyUsage
	if proc == nil {
		return u, false
	}
	entries, err := fs.ReadDir(proc, ".")
	if err != nil {
		return u, false
	}
	want := strconv.Itoa(uid)
	for _, e := range entries {
		pid := e.Name()
		if !isPID(pid) {
			continue
		}
		status, err := fs.ReadFile(proc, pid+"/status")
		if err != nil || effectiveUID(status) != want {
			continue
		}
		fds, err := fs.ReadDir(proc, pid+"/fd")
		if err != nil {
			u.Unreadable++
			continue
		}
		for _, fd := range fds {
			target, err := fs.ReadLink(proc, pid+"/fd/"+fd.Name())
			if err != nil || target != "anon_inode:inotify" {
				continue
			}
			u.Instances++
			info, err := fs.ReadFile(proc, pid+"/fdinfo/"+fd.Name())
			if err != nil {
				continue
			}
			u.Watches += countWatchLines(info)
		}
	}
	return u, true
}

// countWatchLines counts the lines of an fdinfo file that start with
// "inotify wd:".
func countWatchLines(info []byte) int {
	n := 0
	for _, line := range bytes.Split(info, []byte("\n")) {
		if bytes.HasPrefix(line, []byte("inotify wd:")) {
			n++
		}
	}
	return n
}

func isPID(name string) bool {
	if name == "" {
		return false
	}
	for _, c := range name {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// effectiveUID returns the second (effective) UID of the Uid line of a
// /proc/<pid>/status file (real, effective, saved, filesystem), or "" when
// there is no such field.
func effectiveUID(status []byte) string {
	sc := bufio.NewScanner(bytes.NewReader(status))
	for sc.Scan() {
		line := sc.Text()
		if rest, ok := strings.CutPrefix(line, "Uid:"); ok {
			if f := strings.Fields(rest); len(f) > 1 {
				return f[1]
			}
			return ""
		}
	}
	return ""
}
