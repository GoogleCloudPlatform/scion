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

package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// fakeVolumeExec scripts the execs of the NFS volume health check: the
// mount table read returns table (or tableErr), and each probe of a mount
// path returns probe(ctx, path, mode, n) for its n-th probe of that path.
// Any other command fails the test's exec.
type fakeVolumeExec struct {
	mu       sync.Mutex
	table    string
	tableErr error
	// tableDialErrors is how many table reads first fail with a transient
	// exec dial error before the table is returned.
	tableDialErrors int
	probe           func(ctx context.Context, path, mode string, n int) (string, error)
	tables          int
	probes          map[string]int
	modes           map[string][]string
	// deadlines records, for every exec, how long its context had left.
	deadlines []time.Duration
}

func newFakeVolumeExec(table string, probe func(ctx context.Context, path, mode string, n int) (string, error)) *fakeVolumeExec {
	return &fakeVolumeExec{table: table, probe: probe, probes: map[string]int{}, modes: map[string][]string{}}
}

func (f *fakeVolumeExec) exec(ctx context.Context, _, _ string, cmd []string) (string, error) {
	f.mu.Lock()
	if dl, ok := ctx.Deadline(); ok {
		f.deadlines = append(f.deadlines, time.Until(dl))
	} else {
		f.deadlines = append(f.deadlines, -1)
	}
	switch {
	case reflect.DeepEqual(cmd, volumeMountTableCommand):
		f.tables++
		if f.tableDialErrors > 0 {
			f.tableDialErrors--
			f.mu.Unlock()
			return "", errors.New("error dialing backend: No agent available")
		}
		table, err := f.table, f.tableErr
		f.mu.Unlock()
		return table, err
	case len(cmd) == 6 && cmd[0] == "sh" && cmd[1] == "-c" && cmd[2] == volumeProbeScript && cmd[3] == "sh":
		path, mode := cmd[4], cmd[5]
		f.probes[path]++
		n := f.probes[path]
		f.modes[path] = append(f.modes[path], mode)
		f.mu.Unlock()
		return f.probe(ctx, path, mode, n)
	}
	f.mu.Unlock()
	return "", fmt.Errorf("unexpected exec %q", cmd)
}

func (f *fakeVolumeExec) counts() (tables int, probes map[string]int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	probes = make(map[string]int, len(f.probes))
	for k, v := range f.probes {
		probes[k] = v
	}
	return f.tables, probes
}

const (
	testVolumeInlineShare = "NFS share 10.0.0.2:/exports/ws1"
	testVolumeClaimShare  = `claim "home-pvc"`
)

// testVolumeTable is a container mount table with both test targets
// mounted read-write over NFS.
const testVolumeTable = "overlay / overlay rw,relatime 0 0\n" +
	"proc /proc proc rw,nosuid,nodev,noexec 0 0\n" +
	"10.0.0.2:/exports/ws1 /workspace nfs4 rw,relatime,vers=4.1 0 0\n" +
	"10.0.0.2:/exports/home/a /home/scion nfs4 rw,relatime,vers=4.1 0 0\n"

func testVolumeTargets() []volumeHealthTarget {
	return []volumeHealthTarget{
		{volume: "workspace", mountPath: "/workspace", source: mountSource{inline: true, text: testVolumeInlineShare}},
		{volume: "home", mountPath: "/home/scion", source: mountSource{text: testVolumeClaimShare}},
	}
}

func okProbe(context.Context, string, string, int) (string, error) { return "ok", nil }

func newVolumeHealthRuntime(t *testing.T, f *fakeVolumeExec) *KubernetesRuntime {
	t.Helper()
	rt, _, _ := newTestK8sRuntime()
	rt.podExec = f.exec
	rt.volumeHealthInterval = time.Millisecond
	rt.volumeHealthProbeTimeout = 100 * time.Millisecond
	return rt
}

func checkTestVolumes(rt *KubernetesRuntime, targets []volumeHealthTarget) error {
	return rt.checkPodVolumeHealth(context.Background(), "default", "agent-pod", "agent", targets)
}

func requireVolumeUnhealthy(t *testing.T, err error, wantReason string) {
	t.Helper()
	var lf *LaunchFailure
	if !errors.As(err, &lf) {
		t.Fatalf("error = %v, want a *LaunchFailure", err)
	}
	if lf.Code != api.LaunchCodeVolumeUnhealthy || lf.Step != api.LaunchStepContainerStart || lf.Reason != wantReason {
		t.Fatalf("failure = %s/%s/%s, want %s/%s/%s", lf.Code, lf.Step, lf.Reason,
			api.LaunchCodeVolumeUnhealthy, api.LaunchStepContainerStart, wantReason)
	}
}

// TestVolumeHealth_HealthyShareStarts: healthy NFS volumes pass with one
// mount table read and exactly one probe per volume.
func TestVolumeHealth_HealthyShareStarts(t *testing.T) {
	f := newFakeVolumeExec(testVolumeTable, okProbe)
	if err := checkTestVolumes(newVolumeHealthRuntime(t, f), testVolumeTargets()); err != nil {
		t.Fatalf("check = %v, want nil", err)
	}
	tables, probes := f.counts()
	if tables != 1 || !reflect.DeepEqual(probes, map[string]int{"/workspace": 1, "/home/scion": 1}) {
		t.Errorf("execs: tables %d, probes %v; want 1 table read and one probe per volume", tables, probes)
	}
}

// TestVolumeHealth_UnhealthyShareFailsFast: a volume that is not writable
// on every check fails the start with volume_unhealthy; the healthy volume
// is not probed again.
func TestVolumeHealth_UnhealthyShareFailsFast(t *testing.T) {
	f := newFakeVolumeExec(testVolumeTable, func(_ context.Context, path, _ string, _ int) (string, error) {
		if path == "/workspace" {
			return "notwritable", nil
		}
		return "ok", nil
	})
	err := checkTestVolumes(newVolumeHealthRuntime(t, f), testVolumeTargets())
	requireVolumeUnhealthy(t, err, volumeReasonNotWritable)
	tables, probes := f.counts()
	// The threshold is pinned as a literal: exactly 3 checks, never more or fewer.
	if tables != 3 || probes["/workspace"] != 3 || probes["/home/scion"] != 1 {
		t.Errorf("execs: tables %d, probes %v; want 3 table reads, 3 workspace probes, 1 home probe", tables, probes)
	}
}

// TestVolumeHealthGoldenText pins the error text of every reason: the
// volume, its configured share, the reason and the fixed remedy, and
// nothing else (no mount path, options or exec output).
func TestVolumeHealthGoldenText(t *testing.T) {
	inline := volumeHealthTarget{volume: "workspace", mountPath: "/mnt/agent-ws", source: mountSource{inline: true, text: testVolumeInlineShare}}
	claim := volumeHealthTarget{volume: "home", mountPath: "/mnt/agent-home", source: mountSource{text: testVolumeClaimShare}}
	for _, tc := range []struct {
		target volumeHealthTarget
		reason string
		want   string
	}{
		{inline, volumeReasonNotWritable, `volume "workspace" from NFS share 10.0.0.2:/exports/ws1 is mounted but not usable (ShareNotWritable) — check that the export is writable by the agent pod's uid and project_storage.nfs.gid (root_squash/all_squash and directory group ownership)`},
		{inline, volumeReasonNotMounted, `volume "workspace" from NFS share 10.0.0.2:/exports/ws1 is mounted but not usable (ShareNotMounted) — the volume is not mounted in the agent container; check the node's NFS client`},
		{inline, volumeReasonReadOnly, `volume "workspace" from NFS share 10.0.0.2:/exports/ws1 is mounted but not usable (ShareReadOnly) — the volume is mounted read-only; check the export's rw setting (or the PersistentVolume's mountOptions)`},
		{claim, volumeReasonNotVisible, `volume "home" from claim "home-pvc" is mounted but not usable (ShareNotVisible) — the volume's directory cannot be opened; check that the export path still exists on the server`},
		{claim, volumeReasonNotReadable, `volume "home" from claim "home-pvc" is mounted but not usable (ShareNotReadable) — the agent cannot read the volume's directory; check the export's permissions for the agent pod's uid and project_storage.nfs.gid`},
		{claim, volumeReasonUnresponsive, `volume "home" from claim "home-pvc" is mounted but not usable (ShareUnresponsive) — the NFS server did not answer from the node; check the server and the network path from the nodes`},
	} {
		err := volumeHealthFailureError(tc.target, tc.reason)
		requireVolumeUnhealthy(t, err, tc.reason)
		var lf *LaunchFailure
		if !errors.As(err, &lf) {
			t.Fatalf("error = %v, want a *LaunchFailure", err)
		}
		if got := err.Error(); got != tc.want {
			t.Errorf("text = %q\nwant   %q", got, tc.want)
		}
		if strings.Contains(err.Error(), tc.target.mountPath) {
			t.Errorf("text names the mount path: %q", err.Error())
		}
		if lf.Err == nil {
			t.Error("LaunchFailure.Err is nil")
		}
	}
	if len(volumeHealthRemedies) != 6 {
		t.Errorf("remedies for %d reasons, want 6", len(volumeHealthRemedies))
	}
}

// TestVolumeHealth_TransientBelowThresholdDoesNotFail: faults on the first
// two checks and a pass on the third do not fail the start; the volume is
// probed exactly 3 times (the threshold is pinned as a literal).
func TestVolumeHealth_TransientBelowThresholdDoesNotFail(t *testing.T) {
	f := newFakeVolumeExec(testVolumeTable, func(_ context.Context, path, _ string, n int) (string, error) {
		if path == "/workspace" && n <= 2 {
			return "notwritable", nil
		}
		return "ok", nil
	})
	if err := checkTestVolumes(newVolumeHealthRuntime(t, f), testVolumeTargets()); err != nil {
		t.Fatalf("check = %v, want nil", err)
	}
	if tables, probes := f.counts(); tables != 3 || probes["/workspace"] != 3 {
		t.Errorf("execs: tables %d, workspace probes %d; want 3 and 3", tables, probes["/workspace"])
	}
}

// TestVolumeHealth_RecoveryResetsCount: a volume that passes one check is
// accepted and not probed again, so faults that are not consecutive never
// fail the start. Here the workspace faults on its 1st and 3rd probe and the
// home on its 2nd: re-probing accepted volumes would see a fault on every
// check and fail the start.
func TestVolumeHealth_RecoveryResetsCount(t *testing.T) {
	f := newFakeVolumeExec(testVolumeTable, func(_ context.Context, path, _ string, n int) (string, error) {
		switch {
		case path == "/workspace" && n != 2:
			return "notwritable", nil
		case path == "/home/scion" && n == 2:
			return "notwritable", nil
		}
		return "ok", nil
	})
	if err := checkTestVolumes(newVolumeHealthRuntime(t, f), testVolumeTargets()); err != nil {
		t.Fatalf("check = %v, want nil", err)
	}
	if _, probes := f.counts(); probes["/workspace"] != 2 || probes["/home/scion"] != 1 {
		t.Errorf("probes = %v, want workspace 2 (accepted at the second) and home 1", probes)
	}
}

// TestVolumeHealth_ProbeDeadlineIsUnresponsive: a probe cut off by its own
// timeout on every check is a share fault, ShareUnresponsive.
func TestVolumeHealth_ProbeDeadlineIsUnresponsive(t *testing.T) {
	f := newFakeVolumeExec(testVolumeTable, func(ctx context.Context, path, _ string, _ int) (string, error) {
		if path == "/home/scion" {
			<-ctx.Done()
			return "", ctx.Err()
		}
		return "ok", nil
	})
	rt := newVolumeHealthRuntime(t, f)
	rt.volumeHealthProbeTimeout = 20 * time.Millisecond
	err := checkTestVolumes(rt, testVolumeTargets())
	requireVolumeUnhealthy(t, err, volumeReasonUnresponsive)
	if !strings.Contains(err.Error(), `volume "home" from claim "home-pvc"`) {
		t.Errorf("error = %q, want the home volume", err)
	}
}

// TestVolumeHealth_CheckErrorFailsOpen: a check that cannot run (exec
// error, empty or garbled output) is logged once, by volume name and error
// class, and the start goes on without further checks.
func TestVolumeHealth_CheckErrorFailsOpen(t *testing.T) {
	for _, tc := range []struct {
		name      string
		table     string
		tableErr  error
		probe     func(context.Context, string, string, int) (string, error)
		wantClass string
		// prodBound runs the case with the production 10s exec bound and a
		// fake exec clock anchored at time.Now(), so the dial retry's
		// deadline guard is live; wantTables and wantSlept then pin the
		// retries it makes before giving up.
		prodBound  bool
		wantTables int
		wantSlept  []time.Duration
	}{
		{name: "table exec error", tableErr: errors.New("exec failed: unable to upgrade connection: container not found (stderr: secret-ish-output)"), probe: okProbe, wantClass: "mount_table_exec", wantTables: 1},
		// Dial errors at t=0s, 1s and 3s; the next retry (backoff 4s plus
		// the 5s margin) would reach the 10s bound, so the helper gives up.
		{name: "persistent dial error", tableErr: errors.New("error dialing backend: No agent available"), probe: okProbe, wantClass: "mount_table_exec",
			prodBound: true, wantTables: 3, wantSlept: []time.Duration{time.Second, 2 * time.Second}},
		{name: "empty table", probe: okProbe, wantClass: "mount_table_empty", wantTables: 1},
		{name: "garbled probe output", table: testVolumeTable, probe: func(context.Context, string, string, int) (string, error) { return "secret-ish-output", nil }, wantClass: "probe_output", wantTables: 1},
		{name: "probe exec error", table: testVolumeTable, probe: func(context.Context, string, string, int) (string, error) {
			return "", errors.New("exec failed: command terminated with exit code 1 (stderr: secret-ish-output)")
		}, wantClass: "probe_exec", wantTables: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := &lockedBuffer{}
			setRuntimeLogHandler(t, slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			f := newFakeVolumeExec(tc.table, tc.probe)
			f.tableErr = tc.tableErr
			var rt *KubernetesRuntime
			var clock *fakeExecReadyClock
			if tc.prodBound {
				clock = newFakeExecReadyClock(t)
				clock.virtualTime = time.Now()
				rt, _, _ = newTestK8sRuntime()
				rt.podExec = f.exec
				rt.execReadyClock = execReadyClock{now: clock.Now, sleep: clock.Sleep}
			} else {
				rt = newVolumeHealthRuntime(t, f)
			}
			if err := checkTestVolumes(rt, testVolumeTargets()); err != nil {
				t.Fatalf("check = %v, want nil (fail open)", err)
			}
			if tables, _ := f.counts(); tables != tc.wantTables {
				t.Errorf("table reads = %d, want %d", tables, tc.wantTables)
			}
			if clock != nil && !reflect.DeepEqual(clock.slept, tc.wantSlept) {
				t.Errorf("dial retry sleeps = %v, want %v", clock.slept, tc.wantSlept)
			}
			out := logs.String()
			if strings.Count(out, "NFS volume health check could not run") != 1 {
				t.Errorf("want one warning, logs:\n%s", out)
			}
			if !strings.Contains(out, "error_class="+tc.wantClass) || !strings.Contains(out, "workspace") {
				t.Errorf("warning lacks the class %q or the volume names:\n%s", tc.wantClass, out)
			}
			if strings.Contains(out, "secret-ish-output") || strings.Contains(out, "/home/scion") {
				t.Errorf("warning carries exec output or a mount path:\n%s", out)
			}
		})
	}
}

// TestVolumeHealth_DialErrorThenSuccessRunsCheck: a transient exec dial
// error is retried inside the exec's bound, so the check still runs and
// still fails an unusable volume instead of failing open.
func TestVolumeHealth_DialErrorThenSuccessRunsCheck(t *testing.T) {
	f := newFakeVolumeExec(testVolumeTable, func(_ context.Context, path, _ string, _ int) (string, error) {
		if path == "/workspace" {
			return "notwritable", nil
		}
		return "ok", nil
	})
	f.tableDialErrors = 1
	clock := newFakeExecReadyClock(t)
	rt, _, _ := newTestK8sRuntime()
	rt.podExec = f.exec
	rt.execReadyClock = execReadyClock{now: clock.Now, sleep: clock.Sleep}
	err := checkTestVolumes(rt, testVolumeTargets())
	requireVolumeUnhealthy(t, err, volumeReasonNotWritable)
	if tables, _ := f.counts(); tables != 4 {
		t.Errorf("table reads = %d, want 4 (one dial error retried, then 3 checks)", tables)
	}
}

// TestVolumeHealth_ProbeDialErrorThenFaultFails: a transient exec dial
// error on the first probe of a read-write mount is retried inside the
// probe's bound, so the probe still runs and the volume still fails on its
// fault: 1 dial error plus 3 checks, exactly 4 probes of that mount.
func TestVolumeHealth_ProbeDialErrorThenFaultFails(t *testing.T) {
	f := newFakeVolumeExec(testVolumeTable, func(_ context.Context, path, mode string, n int) (string, error) {
		if path != "/workspace" {
			return "ok", nil
		}
		if mode != "rw" {
			t.Errorf("workspace probe mode = %q, want rw", mode)
		}
		if n == 1 {
			return "", errors.New("error dialing backend: No agent available")
		}
		return "notwritable", nil
	})
	clock := newFakeExecReadyClock(t)
	rt, _, _ := newTestK8sRuntime()
	rt.podExec = f.exec
	rt.execReadyClock = execReadyClock{now: clock.Now, sleep: clock.Sleep}
	err := checkTestVolumes(rt, testVolumeTargets())
	requireVolumeUnhealthy(t, err, volumeReasonNotWritable)
	tables, probes := f.counts()
	if tables != 3 || probes["/workspace"] != 4 || probes["/home/scion"] != 1 {
		t.Errorf("execs: tables %d, probes %v; want 3 table reads, 4 workspace probes (1 dial error + 3 checks), 1 home probe", tables, probes)
	}
}

// TestVolumeHealth_IntervalAndBound: checks are 5 seconds apart, and every
// exec of the check is bounded at 10 seconds (projectfs.ShareVisibleTimeout),
// asserted through the injected clock and the exec contexts' deadlines.
func TestVolumeHealth_IntervalAndBound(t *testing.T) {
	f := newFakeVolumeExec(testVolumeTable, func(_ context.Context, path, _ string, _ int) (string, error) {
		if path == "/workspace" {
			return "notwritable", nil
		}
		return "ok", nil
	})
	clock := newFakeExecReadyClock(t)
	rt, _, _ := newTestK8sRuntime()
	rt.podExec = f.exec
	rt.execReadyClock = execReadyClock{now: clock.Now, sleep: clock.Sleep}
	err := checkTestVolumes(rt, testVolumeTargets())
	requireVolumeUnhealthy(t, err, volumeReasonNotWritable)
	if want := []time.Duration{5 * time.Second, 5 * time.Second}; !reflect.DeepEqual(clock.slept, want) {
		t.Errorf("sleeps between checks = %v, want %v", clock.slept, want)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.deadlines) == 0 {
		t.Fatal("no exec recorded")
	}
	for i, d := range f.deadlines {
		if d <= 9*time.Second || d > 10*time.Second {
			t.Errorf("exec %d had %v left, want a 10s bound", i, d)
		}
	}
}

// TestVolumeHealth_ReadOnlyMountFails: a volume the pod mounts read-write
// that the table shows read-only fails with ShareReadOnly, without a probe.
func TestVolumeHealth_ReadOnlyMountFails(t *testing.T) {
	table := strings.Replace(testVolumeTable, "/workspace nfs4 rw,", "/workspace nfs4 ro,", 1)
	f := newFakeVolumeExec(table, okProbe)
	err := checkTestVolumes(newVolumeHealthRuntime(t, f), testVolumeTargets())
	requireVolumeUnhealthy(t, err, volumeReasonReadOnly)
	if _, probes := f.counts(); probes["/workspace"] != 0 {
		t.Errorf("workspace probed %d times, want 0", probes["/workspace"])
	}
}

// TestVolumeHealth_ConfiguredReadOnlyStartsHealthy: a volume that is
// read-only by configuration (the volumeMount, the NFS source or the claim
// source) is checked for presence and readability only. Its "ro" mount is
// expected, and it is never write-tested, even where test -w would fail.
func TestVolumeHealth_ConfiguredReadOnlyStartsHealthy(t *testing.T) {
	for _, tc := range []struct {
		name  string
		vol   corev1.Volume
		mount corev1.VolumeMount
		watch mountSource
	}{
		{"volumeMount readOnly",
			corev1.Volume{Name: "sd", VolumeSource: corev1.VolumeSource{NFS: &corev1.NFSVolumeSource{Server: "10.0.0.2", Path: "/exports/sd"}}},
			corev1.VolumeMount{Name: "sd", MountPath: "/scion-volumes/sd", ReadOnly: true},
			mountSource{inline: true, text: "NFS share 10.0.0.2:/exports/sd"}},
		{"NFS source readOnly",
			corev1.Volume{Name: "sd", VolumeSource: corev1.VolumeSource{NFS: &corev1.NFSVolumeSource{Server: "10.0.0.2", Path: "/exports/sd", ReadOnly: true}}},
			corev1.VolumeMount{Name: "sd", MountPath: "/scion-volumes/sd"},
			mountSource{inline: true, text: "NFS share 10.0.0.2:/exports/sd"}},
		{"claim source readOnly",
			corev1.Volume{Name: "sd", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "sd-pvc", ReadOnly: true}}},
			corev1.VolumeMount{Name: "sd", MountPath: "/scion-volumes/sd"},
			mountSource{text: `claim "sd-pvc"`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := &corev1.Pod{Spec: corev1.PodSpec{
				Volumes:    []corev1.Volume{tc.vol},
				Containers: []corev1.Container{{Name: agentContainerName, VolumeMounts: []corev1.VolumeMount{tc.mount}}},
			}}
			targets := volumeHealthTargets(pod, podMountWatch{sources: map[string]mountSource{"sd": tc.watch}})
			if len(targets) != 1 || !targets[0].readOnly {
				t.Fatalf("targets = %+v, want one read-only target", targets)
			}
			table := "overlay / overlay rw 0 0\n10.0.0.2:/exports/sd /scion-volumes/sd nfs4 ro,relatime 0 0\n"
			f := newFakeVolumeExec(table, func(_ context.Context, _, mode string, _ int) (string, error) {
				if mode == "rw" {
					return "notwritable", nil // test -w would fail on this volume
				}
				return "ok", nil
			})
			if err := checkTestVolumes(newVolumeHealthRuntime(t, f), targets); err != nil {
				t.Fatalf("check = %v, want nil", err)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if !reflect.DeepEqual(f.modes["/scion-volumes/sd"], []string{"ro"}) {
				t.Errorf("probe modes = %v, want one read-only probe", f.modes["/scion-volumes/sd"])
			}
		})
	}
}

// TestVolumeHealth_NonNFSVolumesUnaffected: only the NFS volumes the mount
// check watches are checked. emptyDir, Secret and other claims are never
// probed, and a watched claim whose mount is not NFS is skipped.
func TestVolumeHealth_NonNFSVolumesUnaffected(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{
		Volumes: []corev1.Volume{
			{Name: "workspace", VolumeSource: corev1.VolumeSource{NFS: &corev1.NFSVolumeSource{Server: "10.0.0.2", Path: "/exports/ws1"}}},
			{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			{Name: "local-sd", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "local-sd-pvc"}}},
			{Name: "creds", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "s"}}},
			{Name: "home", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "home-pvc"}}},
		},
		Containers: []corev1.Container{{Name: agentContainerName, VolumeMounts: []corev1.VolumeMount{
			{Name: "workspace", MountPath: "/workspace"},
			{Name: "tmp", MountPath: "/tmp"},
			{Name: "local-sd", MountPath: "/scion-volumes/local"},
			{Name: "creds", MountPath: "/run/creds", ReadOnly: true},
			{Name: "home", MountPath: "/home/scion"},
		}}},
	}}
	config := RunConfig{HomeStorage: &HomeStorageRealization{PVClaimName: "home-pvc"}}
	targets := volumeHealthTargets(pod, newPodMountWatch(pod, config))
	var paths []string
	for _, tg := range targets {
		paths = append(paths, tg.mountPath)
	}
	if !reflect.DeepEqual(paths, []string{"/workspace", "/home/scion"}) {
		t.Fatalf("target paths = %v, want only the NFS volumes", paths)
	}
	// The home claim is bound to a non-NFS volume here: skipped, not probed.
	table := "overlay / overlay rw 0 0\n" +
		"10.0.0.2:/exports/ws1 /workspace nfs4 rw 0 0\n" +
		"/dev/sdb /home/scion ext4 rw 0 0\n" +
		"tmpfs /tmp tmpfs rw 0 0\n"
	f := newFakeVolumeExec(table, okProbe)
	if err := checkTestVolumes(newVolumeHealthRuntime(t, f), targets); err != nil {
		t.Fatalf("check = %v, want nil", err)
	}
	if _, probes := f.counts(); !reflect.DeepEqual(probes, map[string]int{"/workspace": 1}) {
		t.Errorf("probes = %v, want only /workspace", probes)
	}
}

// TestVolumeHealthTargets: targets follow the agent container's mounts of
// watched volumes, one per mount path, with the read-only flag; other
// containers are ignored.
func TestVolumeHealthTargets(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{
		Volumes: []corev1.Volume{
			{Name: "workspace", VolumeSource: corev1.VolumeSource{NFS: &corev1.NFSVolumeSource{Server: "10.0.0.2", Path: "/exports/ws1"}}},
		},
		InitContainers: []corev1.Container{{Name: "provision", VolumeMounts: []corev1.VolumeMount{{Name: "workspace", MountPath: "/init-ws"}}}},
		Containers: []corev1.Container{
			{Name: "sidecar", VolumeMounts: []corev1.VolumeMount{{Name: "workspace", MountPath: "/sidecar-ws"}}},
			{Name: agentContainerName, VolumeMounts: []corev1.VolumeMount{
				{Name: "workspace", MountPath: "/workspace/"},
				{Name: "workspace", MountPath: "/workspace/.scion", SubPath: ".scion", ReadOnly: true},
			}},
		},
	}}
	src := mountSource{inline: true, text: testVolumeInlineShare}
	got := volumeHealthTargets(pod, podMountWatch{sources: map[string]mountSource{"workspace": src}})
	want := []volumeHealthTarget{
		{volume: "workspace", mountPath: "/workspace", source: src},
		{volume: "workspace", mountPath: "/workspace/.scion", readOnly: true, source: src},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("targets = %+v\nwant %+v", got, want)
	}
	if got := volumeHealthTargets(pod, podMountWatch{}); got != nil {
		t.Errorf("targets with an empty watch = %+v, want nil", got)
	}
	if got := volumeHealthTargets(nil, podMountWatch{sources: map[string]mountSource{"workspace": src}}); got != nil {
		t.Errorf("targets of a nil pod = %+v, want nil", got)
	}
}

// volumeHealthRunExec is a podExec for a Run of an NFS-home pod: it answers
// the mount table from the agent container's mounts in the created pod
// (each as NFS, read-only when the mount is), the probes with probe, and
// the home mode read with seed-over for this start. It records every
// command.
func volumeHealthRunExec(pods podReader, probe func(mode string) string, steps *[]string, mu *sync.Mutex) func(context.Context, string, string, []string) (string, error) {
	return func(_ context.Context, _, _ string, cmd []string) (string, error) {
		joined := strings.Join(cmd, " ")
		mu.Lock()
		*steps = append(*steps, joined)
		mu.Unlock()
		switch {
		case reflect.DeepEqual(cmd, volumeMountTableCommand):
			mounts, _, err := pods.agentMounts()
			if err != nil {
				return "", err
			}
			var b strings.Builder
			b.WriteString("overlay / overlay rw 0 0\n")
			for _, m := range mounts {
				opts := "rw"
				if m.ReadOnly {
					opts = "ro"
				}
				fmt.Fprintf(&b, "10.0.0.9:/exports/x %s nfs4 %s,relatime 0 0\n", filepath.Clean(m.MountPath), opts)
			}
			return b.String(), nil
		case len(cmd) == 6 && cmd[2] == volumeProbeScript:
			return probe(cmd[5]), nil
		case joined == "cat "+k8sHomeModeFile:
			_, labels, err := pods.agentMounts()
			if err != nil {
				return "", err
			}
			return `{"mode":"seed-over","start_id":"` + labels[labelStartID] + `"}`, nil
		}
		return "", nil
	}
}

// podReader reads the agent container's mounts and the labels of a pod in
// the fake clientset, for volumeHealthRunExec.
type podReader struct {
	get func() (*corev1.Pod, error)
}

func (p podReader) agentMounts() ([]corev1.VolumeMount, map[string]string, error) {
	pod, err := p.get()
	if err != nil {
		return nil, nil, err
	}
	for _, c := range pod.Spec.Containers {
		if c.Name == agentContainerName {
			return c.VolumeMounts, pod.Labels, nil
		}
	}
	return nil, pod.Labels, nil
}

// volumeHealthRunConfig is an NFS-home start config for Run tests.
func volumeHealthRunConfig(t *testing.T, name string) RunConfig {
	t.Helper()
	adc := filepath.Join(t.TempDir(), "adc.json")
	if err := os.WriteFile(adc, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := nfsHomeTestConfig(true)
	config.Name = name
	config.HomeStorage.AgentSlug = name
	config.Harness = &MockHarness{}
	config.ResolvedAuth.Files[0].SourcePath = adc
	config.HomeDir = t.TempDir()
	return config
}

func stepIndex(steps []string, prefix string) int {
	for i, s := range steps {
		if strings.HasPrefix(s, prefix) {
			return i
		}
	}
	return -1
}

// TestRun_VolumeHealthBeforeStartupGate: Run checks the NFS volumes after
// the exec tunnel is ready and before the home is prepared or the startup
// gate is signalled; an unusable volume stops the start with
// volume_unhealthy, with no home transfer and no startup gate.
func TestRun_VolumeHealthBeforeStartupGate(t *testing.T) {
	t.Setenv("SCION_K8S_NAMESPACE", "default")
	for _, tc := range []struct {
		name    string
		probe   func(mode string) string
		wantErr bool
	}{
		{"healthy", func(string) string { return "ok" }, false},
		{"not writable", func(mode string) string {
			if mode == "rw" {
				return "notwritable"
			}
			return "ok"
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt, clientset, _ := newTestK8sRuntime()
			rt.execProbe = func(context.Context, string, string) error { return nil }
			rt.volumeHealthInterval = time.Millisecond
			config := volumeHealthRunConfig(t, "vh-agent")
			var mu sync.Mutex
			var steps []string
			reader := podReader{get: func() (*corev1.Pod, error) {
				return clientset.CoreV1().Pods("default").Get(context.Background(), config.Name, metav1.GetOptions{})
			}}
			rt.podExec = volumeHealthRunExec(reader, tc.probe, &steps, &mu)
			rt.homeSync = func(context.Context, string, string, string, string, []string) error {
				mu.Lock()
				steps = append(steps, "home-sync")
				mu.Unlock()
				return nil
			}
			runErr := startFakeK8sPod(t, rt, clientset, config)
			mu.Lock()
			defer mu.Unlock()
			table := stepIndex(steps, "cat /proc/self/mounts")
			probe := stepIndex(steps, "sh -c "+volumeProbeScript)
			homeMode := stepIndex(steps, "cat "+k8sHomeModeFile)
			gate := stepIndex(steps, "touch /tmp/.scion-home-ready")
			if table < 0 || probe < table {
				t.Fatalf("volume check did not run: table %d, probe %d (steps %q)", table, probe, steps)
			}
			if tc.wantErr {
				requireVolumeUnhealthy(t, runErr, volumeReasonNotWritable)
				if gate >= 0 || homeMode >= 0 || stepIndex(steps, "home-sync") >= 0 {
					t.Errorf("start went on after the failed check (steps %q)", steps)
				}
				return
			}
			if runErr != nil {
				t.Fatalf("Run: %v (steps %q)", runErr, steps)
			}
			if homeMode < 0 || gate < 0 || probe > homeMode || probe > gate {
				t.Errorf("order: table %d, probe %d, home mode %d, gate %d (steps %q)", table, probe, homeMode, gate, steps)
			}
		})
	}
}

// TestRun_RestartRerunsVolumeHealth: a restart is a stop then a new Run,
// and each Run checks the NFS volumes again.
func TestRun_RestartRerunsVolumeHealth(t *testing.T) {
	t.Setenv("SCION_K8S_NAMESPACE", "default")
	rt, clientset, _ := newTestK8sRuntime()
	rt.execProbe = func(context.Context, string, string) error { return nil }
	rt.homeSync = func(context.Context, string, string, string, string, []string) error { return nil }
	config := volumeHealthRunConfig(t, "vh-restart")
	var mu sync.Mutex
	var steps []string
	reader := podReader{get: func() (*corev1.Pod, error) {
		return clientset.CoreV1().Pods("default").Get(context.Background(), config.Name, metav1.GetOptions{})
	}}
	rt.podExec = volumeHealthRunExec(reader, func(string) string { return "ok" }, &steps, &mu)

	countTables := func() int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, s := range steps {
			if s == "cat /proc/self/mounts" {
				n++
			}
		}
		return n
	}
	if err := startFakeK8sPod(t, rt, clientset, config); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if n := countTables(); n != 1 {
		t.Fatalf("table reads after the start = %d, want 1", n)
	}
	// Stop: the pod is gone before the next start.
	if err := clientset.CoreV1().Pods("default").Delete(context.Background(), config.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := startFakeK8sPod(t, rt, clientset, config); err != nil {
		t.Fatalf("restart Run: %v", err)
	}
	if n := countTables(); n != 2 {
		t.Errorf("table reads after the restart = %d, want 2", n)
	}
}
