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

package hostlimits

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"
)

// fakeReader returns a Linux Reader whose /proc serves files (paths
// relative to /proc); a path not in files is missing.
func fakeReader(files map[string]string) Reader {
	proc := fstest.MapFS{}
	for name, v := range files {
		proc[name] = &fstest.MapFile{Data: []byte(v)}
	}
	return Reader{GOOS: "linux", Proc: proc}
}

func limits(instances, watches string) map[string]string {
	m := map[string]string{}
	if instances != "" {
		m[maxUserInstancesPath] = instances + "\n"
	}
	if watches != "" {
		m[maxUserWatchesPath] = watches + "\n"
	}
	return m
}

func TestInotifyThresholds(t *testing.T) {
	if got := MinInotifyInstances(DefaultExpectedAgents); got != 228 {
		t.Errorf("MinInotifyInstances(default) = %d, want 228", got)
	}
	if got := MinInotifyWatches(DefaultExpectedAgents); got != 110592 {
		t.Errorf("MinInotifyWatches(default) = %d, want 110592", got)
	}
	if got := MinInotifyInstances(10); got != 138 {
		t.Errorf("MinInotifyInstances(10) = %d, want 138", got)
	}
}

func TestCheckInotify_Levels(t *testing.T) {
	cases := []struct {
		name                       string
		instances, watches         string
		instancesLow, watchesLow   bool
		wantStatus                 string
		wantWarning, wantMsgSubstr string
	}{
		{name: "kernel defaults", instances: "128", watches: "8192", instancesLow: true, watchesLow: true, wantStatus: StatusWarn,
			wantWarning: "fs.inotify.max_user_instances is 128 (want at least 228), fs.inotify.max_user_watches is 8192 (want at least 110592)"},
		{name: "instances one below", instances: "227", watches: "110592", instancesLow: true, wantStatus: StatusWarn,
			wantWarning: "fs.inotify.max_user_instances is 227 (want at least 228)"},
		{name: "watches one below", instances: "228", watches: "110591", watchesLow: true, wantStatus: StatusWarn,
			wantWarning: "fs.inotify.max_user_watches is 110591 (want at least 110592)"},
		{name: "both at threshold", instances: "228", watches: "110592", wantStatus: StatusPass,
			wantMsgSubstr: "fs.inotify.max_user_instances 228, fs.inotify.max_user_watches 110592"},
		{name: "both above", instances: "8192", watches: "524288", wantStatus: StatusPass,
			wantMsgSubstr: "fs.inotify.max_user_instances 8192, fs.inotify.max_user_watches 524288"},
		{name: "watches missing, instances low", instances: "128", instancesLow: true, wantStatus: StatusWarn,
			wantWarning: "fs.inotify.max_user_instances is 128 (want at least 228)"},
		{name: "instances missing, watches fine", watches: "524288", wantStatus: StatusPass,
			wantMsgSubstr: "fs.inotify.max_user_instances unknown, fs.inotify.max_user_watches 524288"},
		{name: "instances unparsable, watches low", instances: "lots", watches: "100", watchesLow: true, wantStatus: StatusWarn,
			wantWarning: "fs.inotify.max_user_watches is 100 (want at least 110592)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := CheckInotify(fakeReader(limits(c.instances, c.watches)), 0)
			if !res.Applicable {
				t.Fatalf("not applicable: %q", res.Reason)
			}
			if res.InstancesLow() != c.instancesLow || res.WatchesLow() != c.watchesLow {
				t.Fatalf("low = (%v, %v), want (%v, %v)", res.InstancesLow(), res.WatchesLow(), c.instancesLow, c.watchesLow)
			}
			status, msg, rem := res.Doctor()
			if status != c.wantStatus {
				t.Fatalf("doctor status = %q (%s), want %q", status, msg, c.wantStatus)
			}
			warning := res.Warning()
			if c.wantWarning == "" {
				if warning != "" || rem != "" {
					t.Fatalf("unexpected warning %q / remediation %q", warning, rem)
				}
				if !strings.Contains(msg, c.wantMsgSubstr) {
					t.Fatalf("doctor message %q lacks %q", msg, c.wantMsgSubstr)
				}
				return
			}
			if !strings.Contains(warning, c.wantWarning) || !strings.Contains(msg, c.wantWarning) {
				t.Fatalf("warning %q / doctor message %q lack %q", warning, msg, c.wantWarning)
			}
			if !strings.Contains(warning, "assuming the default capacity, max_agents_per_broker 100") {
				t.Fatalf("warning %q does not state the default capacity", warning)
			}
			if !strings.Contains(warning, rem) || !strings.Contains(rem, "sudo sysctl -w") || !strings.Contains(rem, "/etc/sysctl.d/") {
				t.Fatalf("remediation %q missing from warning %q, or lacks the sysctl fix", rem, warning)
			}
			if wantInst := c.instancesLow; strings.Contains(rem, "fs.inotify.max_user_instances=8192") != wantInst {
				t.Fatalf("remediation %q: instances fix present = %v, want %v", rem, !wantInst, wantInst)
			}
			if wantW := c.watchesLow; strings.Contains(rem, "fs.inotify.max_user_watches=524288") != wantW {
				t.Fatalf("remediation %q: watches fix present = %v, want %v", rem, !wantW, wantW)
			}
		})
	}
}

func TestCheckInotify_AgentCount(t *testing.T) {
	r := fakeReader(limits("200", "524288"))
	if res := CheckInotify(r, 0); !res.InstancesLow() || res.Agents != DefaultExpectedAgents {
		t.Fatalf("agents 0: low=%v agents=%d, want low at the default count", res.InstancesLow(), res.Agents)
	}
	if res := CheckInotify(r, -5); !res.InstancesLow() || res.Agents != DefaultExpectedAgents {
		t.Fatalf("agents -5: low=%v agents=%d, want low at the default count", res.InstancesLow(), res.Agents)
	}
	res := CheckInotify(r, 72) // 72*1+128 = 200: at the threshold
	if res.InstancesLow() || res.Warning() != "" {
		t.Fatalf("72 agents: instances 200 judged low: %q", res.Warning())
	}
	if _, msg, _ := res.Doctor(); !strings.Contains(msg, "enough for about 72 agents)") || strings.Contains(msg, "default capacity") {
		t.Fatalf("72 agents: doctor message %q", msg)
	}
}

func TestCheckInotify_NotApplicable(t *testing.T) {
	cases := []struct {
		name   string
		r      Reader
		reason string
	}{
		{name: "darwin", r: Reader{GOOS: "darwin", Proc: fakeReader(limits("128", "8192")).Proc}, reason: "not a Linux host"},
		{name: "both files missing", r: fakeReader(nil), reason: "/proc/sys/fs/inotify is not readable"},
		{name: "both unparsable", r: fakeReader(limits("x", "y")), reason: "/proc/sys/fs/inotify is not readable"},
		{name: "nil Proc", r: Reader{GOOS: "linux"}, reason: "/proc/sys/fs/inotify is not readable"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := CheckInotify(c.r, 0)
			if res.Applicable || res.Reason != c.reason {
				t.Fatalf("applicable=%v reason=%q, want false %q", res.Applicable, res.Reason, c.reason)
			}
			if res.Warning() != "" || res.InstancesLow() || res.WatchesLow() {
				t.Fatalf("not applicable but warns: %q", res.Warning())
			}
			status, msg, rem := res.Doctor()
			if status != StatusSkip || msg != "Not applicable ("+c.reason+")" || rem != "" {
				t.Fatalf("doctor = %q %q %q", status, msg, rem)
			}
		})
	}
}

// procStatus is a /proc/<pid>/status file with the given real and
// effective UIDs.
func procStatus(ruid, euid string) *fstest.MapFile {
	return &fstest.MapFile{Data: []byte("Name:\tnode\nState:\tS (sleeping)\nUid:\t" + ruid + "\t" + euid + "\t" + euid + "\t" + euid + "\n")}
}

func link(target string) *fstest.MapFile {
	return &fstest.MapFile{Data: []byte(target), Mode: fs.ModeSymlink}
}

func TestCountInotifyUsage(t *testing.T) {
	proc := fstest.MapFS{
		// euid 1000: two inotify instances (3 + 1 watches) and a plain file.
		"10/status":   procStatus("1000", "1000"),
		"10/fd/3":     link("anon_inode:inotify"),
		"10/fd/4":     link("/tmp/log"),
		"10/fd/5":     link("anon_inode:inotify"),
		"10/fdinfo/3": {Data: []byte("pos:\t0\nflags:\t02004000\ninotify wd:1 ino:2\ninotify wd:2 ino:3\ninotify wd:3 ino:4\n")},
		"10/fdinfo/5": {Data: []byte("inotify wd:1 ino:9\n")},
		// euid 1000: an instance with no readable fdinfo still counts.
		"11/status": procStatus("1000", "1000"),
		"11/fd/7":   link("anon_inode:inotify"),
		// real uid 0, effective uid 1000: counted for 1000 (the kernel
		// charges the effective UID).
		"12/status":   procStatus("0", "1000"),
		"12/fd/3":     link("anon_inode:inotify"),
		"12/fdinfo/3": {Data: []byte("inotify wd:5 ino:2\ninotify wd:6 ino:3\n")},
		// real uid 1000, effective uid 0: counted for 0, not 1000.
		"20/status":   procStatus("1000", "0"),
		"20/fd/3":     link("anon_inode:inotify"),
		"20/fdinfo/3": {Data: []byte("inotify wd:1 ino:2\n")},
		// euid 1000 but the fd directory cannot be listed (a file, not a dir).
		"30/status": procStatus("1000", "1000"),
		"30/fd":     {Data: []byte("not a directory")},
		// not a process directory.
		"sys/fs/inotify/max_user_instances": {Data: []byte("128\n")},
		// a process with no status file.
		"40/fd/3": link("anon_inode:inotify"),
		// a status file with only one UID field.
		"50/status": {Data: []byte("Uid:\t1000\n")},
		"50/fd/3":   link("anon_inode:inotify"),
	}
	u, ok := CountInotifyUsage(proc, 1000)
	if !ok {
		t.Fatal("CountInotifyUsage: not ok")
	}
	if u != (InotifyUsage{Instances: 4, Watches: 6, Unreadable: 1}) {
		t.Fatalf("usage = %+v, want 4 instances, 6 watches, 1 unreadable", u)
	}
	if got, want := u.String(), "approximately 4 instances, 6 watches (a descriptor shared by several processes is counted once per process; 1 processes not readable)"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}

	u, ok = CountInotifyUsage(proc, 0)
	if !ok || u != (InotifyUsage{Instances: 1, Watches: 1}) {
		t.Fatalf("euid 0 usage = %+v ok=%v, want 1 instance, 1 watch", u, ok)
	}
	if got, want := u.String(), "approximately 1 instances, 1 watches (a descriptor shared by several processes is counted once per process)"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

// TestDefaultReaderAdapters exercises the real os.DirFS adapter: the limits
// are read through fs.ReadFile and a symlink target that is not a path
// ("anon_inode:inotify") comes back unchanged from fs.ReadLink.
func TestDefaultReaderAdapters(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sys/fs/inotify"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]string{"sys/fs/inotify/max_user_instances": "128\n", "sys/fs/inotify/max_user_watches": "524288\n", "7/status": "Uid:\t1000\t4242\t4242\t4242\n", "7/fdinfo/3": "inotify wd:1 ino:2\n"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "7/fd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("anon_inode:inotify", filepath.Join(root, "7/fd/3")); err != nil {
		t.Fatal(err)
	}
	r := Reader{GOOS: "linux", Proc: os.DirFS(root)}
	res := CheckInotify(r, 0)
	if !res.Applicable || res.Instances != (Limit{Value: 128, Known: true}) || res.Watches != (Limit{Value: 524288, Known: true}) {
		t.Fatalf("CheckInotify over os.DirFS = %+v", res)
	}
	u, ok := CountInotifyUsage(r.Proc, 4242)
	if !ok || u != (InotifyUsage{Instances: 1, Watches: 1}) {
		t.Fatalf("CountInotifyUsage over os.DirFS = %+v ok=%v, want 1 instance, 1 watch", u, ok)
	}
	if d := DefaultReader(); d.GOOS != runtime.GOOS || d.Proc == nil {
		t.Fatalf("DefaultReader = %+v", d)
	}
}

func TestCountInotifyUsage_NoProc(t *testing.T) {
	if _, ok := CountInotifyUsage(nil, 1000); ok {
		t.Fatal("nil proc: ok, want false")
	}
	if _, ok := CountInotifyUsage(errFS{}, 1000); ok {
		t.Fatal("unlistable proc: ok, want false")
	}
}

// errFS fails every open, like a /proc that cannot be listed.
type errFS struct{}

func (errFS) Open(string) (fs.File, error) { return nil, fs.ErrPermission }
