//go:build linux

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

package commands

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// useSelfAsInit points readInitPrivilege at this test process, which then
// plays the init process through the real /proc. It skips when the
// process cannot pass the legs a test does not target: it runs as root,
// it is not in the initial user namespace, or the kernel does not report
// mount IDs.
func useSelfAsInit(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("the test process runs as root, so it cannot play a non-root init")
	}
	if m, err := os.ReadFile("/proc/self/uid_map"); err != nil || !uidMapIsIdentity(m) {
		t.Skip("the test process is not in the initial user namespace")
	}
	restoreInitProcSeams(t)
	initProcStatusName = "self/status"
	initProcExeName = "self/exe"
	initProcInitPidNSName = "self/ns/pid"
	if p, err := openProcMount(); err != nil {
		t.Skipf("no usable /proc mount: %v", err)
	} else {
		p.close()
	}
}

func restoreInitProcSeams(t *testing.T) {
	t.Helper()
	for _, p := range []*string{&initProcRoot, &initProcStatusName, &initProcExeName, &initProcSelfExeName,
		&initProcInitPidNSName, &initProcSelfPidNSName, &initProcSelfUIDMapName} {
		orig := *p
		t.Cleanup(func() { *p = orig })
	}
	orig := statxMountID
	t.Cleanup(func() { statxMountID = orig })
}

func wantInitPrivilege(t *testing.T, want initPrivilege) {
	t.Helper()
	got, err := readInitPrivilege()
	if got != want {
		t.Errorf("readInitPrivilege = %d (err %v), want %d", got, err, want)
	}
	if want == initPrivUnknown && err == nil {
		t.Error("an unknown verdict carries no reason")
	}
}

// All legs pass for a non-root process playing init.
func TestReadInitPrivilege_AllLegsPass(t *testing.T) {
	useSelfAsInit(t)
	wantInitPrivilege(t, initPrivNonRoot)
}

// The /proc root must be procfs: a fake tree on one ordinary mount, which
// passes every other leg (same mount ID throughout), gives unknown.
func TestReadInitPrivilege_FakeProcTree(t *testing.T) {
	useSelfAsInit(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for _, d := range []string{"1/ns", "self/ns"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		"1/status":     "Uid:\t1000\t1000\t1000\t1000\n",
		"self/uid_map": "         0          0 4294967295\n",
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for name, target := range map[string]string{"1/exe": exe, "self/exe": exe, "1/ns/pid": "pid:[1]", "self/ns/pid": "pid:[1]"} {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	initProcRoot = root
	initProcStatusName, initProcExeName, initProcInitPidNSName = "1/status", "1/exe", "1/ns/pid"
	wantInitPrivilege(t, initPrivUnknown)
}

// A file on another mount (as a bind over it would be) gives unknown.
func TestReadInitPrivilege_MountIDMismatch(t *testing.T) {
	useSelfAsInit(t)
	orig := statxMountID
	statxMountID = func(fd int, name string) (uint64, bool, error) {
		id, ok, err := orig(fd, name)
		if name == initProcSelfUIDMapName {
			id++
		}
		return id, ok, err
	}
	wantInitPrivilege(t, initPrivUnknown)
}

// A kernel that does not report mount IDs gives unknown.
func TestReadInitPrivilege_MountIDNotReported(t *testing.T) {
	useSelfAsInit(t)
	orig := statxMountID
	statxMountID = func(fd int, name string) (uint64, bool, error) {
		id, _, err := orig(fd, name)
		return id, false, err
	}
	wantInitPrivilege(t, initPrivUnknown)
}

// startSleeper starts another process, running sleep, and returns its
// pid; it is killed when the test ends.
func startSleeper(t *testing.T) int {
	t.Helper()
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep binary")
	}
	cmd := exec.Command(sleep, "60")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start sleep: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd.Process.Pid
}

// PID 1 must be this binary: another process's exe gives unknown.
func TestReadInitPrivilege_ExeMismatch(t *testing.T) {
	useSelfAsInit(t)
	initProcExeName = fmt.Sprintf("%d/exe", startSleeper(t))
	wantInitPrivilege(t, initPrivUnknown)
}

// doctor's own side of the comparison is read through the pinned /proc
// too: a self/exe that is another process's exe gives unknown.
func TestReadInitPrivilege_SelfExeMismatch(t *testing.T) {
	useSelfAsInit(t)
	initProcSelfExeName = fmt.Sprintf("%d/exe", startSleeper(t))
	wantInitPrivilege(t, initPrivUnknown)
}

// initPrivSleepEnv makes TestHelperInitPrivSleep sleep, so a test can run
// a second process of this test binary.
const initPrivSleepEnv = "SCION_TEST_INITPRIV_SLEEP"

func TestHelperInitPrivSleep(t *testing.T) {
	if os.Getenv(initPrivSleepEnv) != "1" {
		t.Skip("helper process only")
	}
	time.Sleep(60 * time.Second)
}

// Another process running the same binary passes: the comparison is of
// the links' targets, not of the procfs links themselves.
func TestReadInitPrivilege_SameBinaryOtherProcess(t *testing.T) {
	useSelfAsInit(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestHelperInitPrivSleep$")
	cmd.Env = append(os.Environ(), initPrivSleepEnv+"=1")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start a second test process: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	initProcExeName = fmt.Sprintf("%d/exe", cmd.Process.Pid)
	wantInitPrivilege(t, initPrivNonRoot)
}

// The exe link opened again after its target must be the same link as the
// one held: another inode (the link was replaced meanwhile) gives unknown.
func TestReadInitPrivilege_ExeLinkReplacedAfterTargetOpen(t *testing.T) {
	useSelfAsInit(t)
	initProcExeName = fmt.Sprintf("%d/exe", os.Getpid())
	orig := linkReopenStat
	t.Cleanup(func() { linkReopenStat = orig })
	calls := 0
	linkReopenStat = func(p *procMount, name string) (unix.Stat_t, error) {
		st, err := orig(p, name)
		if name == initProcExeName {
			calls++
			st.Ino++
		}
		return st, err
	}
	wantInitPrivilege(t, initPrivUnknown)
	if calls != 1 {
		t.Errorf("the exe link was opened again %d times, want 1", calls)
	}
}

// The device half of that comparison matters on its own: the same inode
// number on another device is another link, and gives unknown.
func TestReadInitPrivilege_ExeLinkOtherDeviceAfterTargetOpen(t *testing.T) {
	useSelfAsInit(t)
	initProcExeName = fmt.Sprintf("%d/exe", os.Getpid())
	orig := linkReopenStat
	t.Cleanup(func() { linkReopenStat = orig })
	calls := 0
	linkReopenStat = func(p *procMount, name string) (unix.Stat_t, error) {
		st, err := orig(p, name)
		if name == initProcExeName {
			calls++
			st.Dev++
		}
		return st, err
	}
	wantInitPrivilege(t, initPrivUnknown)
	if calls != 1 {
		t.Errorf("the exe link was opened again %d times, want 1", calls)
	}
}

// The exe link is pinned again after its target is opened: a link whose
// mount differs on that second open (a mount placed over it meanwhile)
// gives unknown.
func TestReadInitPrivilege_ExeLinkChangedAfterTargetOpen(t *testing.T) {
	useSelfAsInit(t)
	initProcExeName = fmt.Sprintf("%d/exe", os.Getpid())
	orig := statxMountID
	calls := 0
	statxMountID = func(fd int, name string) (uint64, bool, error) {
		id, ok, err := orig(fd, name)
		if name == initProcExeName {
			calls++
			if calls == 2 {
				id++
			}
		}
		return id, ok, err
	}
	wantInitPrivilege(t, initPrivUnknown)
	if calls < 2 {
		t.Errorf("the exe link was pinned %d times, want 2 (before and after the target open)", calls)
	}
}

// R1: a uid_map that is not the identity map gives unknown. A test cannot
// make procfs show another map, so a pinned procfs file with other
// content stands in for it (TestUIDMapIsIdentity covers the parser).
func TestReadInitPrivilege_UIDMapNotIdentity(t *testing.T) {
	useSelfAsInit(t)
	initProcSelfUIDMapName = "self/setgroups"
	wantInitPrivilege(t, initPrivUnknown)
}

// doctor and PID 1 must share the pid namespace.
func TestReadInitPrivilege_PidNSMismatch(t *testing.T) {
	useSelfAsInit(t)
	initProcInitPidNSName = "self/ns/user"
	wantInitPrivilege(t, initPrivUnknown)
}

// A missing procfs entry gives unknown.
func TestReadInitPrivilege_PidNSUnreadable(t *testing.T) {
	useSelfAsInit(t)
	initProcInitPidNSName = "self/ns/missing"
	wantInitPrivilege(t, initPrivUnknown)
}
