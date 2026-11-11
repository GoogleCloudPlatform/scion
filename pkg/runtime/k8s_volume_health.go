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
	"path/filepath"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	corev1 "k8s.io/api/core/v1"
)

// The NFS volume health check of Run: after the agent pod is running and
// its exec tunnel is ready, and before the startup gate lets the harness
// start, the runtime checks each NFS volume of the agent container from
// inside it. The mount check of waitForPodReady (k8s_mount_check.go) covers
// a volume the kubelet cannot mount; this check covers a volume that
// mounted but is not usable. It reads metadata only: the container's mount
// table, then a cd and test -r (and, for a read-write mount, test -w) on
// the mount directory. It writes nothing to the volume and runs nothing
// from it. A volume that faults on volumeHealthAttempts consecutive checks,
// with a fault of any kind on each, fails the start with volume_unhealthy
// and the reason of its last fault; a check that cannot run is logged and
// the start goes on.

// Bounds of the NFS volume health check. A volume must fail
// volumeHealthAttempts consecutive checks, defaultVolumeHealthInterval
// apart, before the start fails; a volume that passes one check is
// accepted. Each exec is bounded by projectfs.ShareVisibleTimeout, the
// bound of the broker's own share visibility check.
const (
	volumeHealthAttempts        = 3
	defaultVolumeHealthInterval = 5 * time.Second
)

// Reasons of a volume_unhealthy failure.
const (
	volumeReasonNotMounted   = "ShareNotMounted"
	volumeReasonReadOnly     = "ShareReadOnly"
	volumeReasonNotVisible   = "ShareNotVisible"
	volumeReasonNotReadable  = "ShareNotReadable"
	volumeReasonNotWritable  = "ShareNotWritable"
	volumeReasonUnresponsive = "ShareUnresponsive"
)

// volumeHealthRemedies are the fixed remedies of a volume_unhealthy error,
// by reason.
var volumeHealthRemedies = map[string]string{
	volumeReasonNotMounted:   "the volume is not mounted in the agent container; check the node's NFS client",
	volumeReasonReadOnly:     "the volume is mounted read-only; check the export's rw setting (or the PersistentVolume's mountOptions)",
	volumeReasonNotVisible:   "the volume's directory cannot be opened; check that the export path still exists on the server",
	volumeReasonNotReadable:  "the agent cannot read the volume's directory; check the export's permissions for the agent pod's uid and project_storage.nfs.gid",
	volumeReasonNotWritable:  "check that the export is writable by the agent pod's uid and project_storage.nfs.gid (root_squash/all_squash and directory group ownership)",
	volumeReasonUnresponsive: "the NFS server did not answer from the node; check the server and the network path from the nodes",
}

// volumeMountTableCommand reads the agent container's mount table. Reading
// /proc never touches an NFS mount, so it cannot hang on one.
var volumeMountTableCommand = []string{"cat", "/proc/self/mounts"}

// volumeProbeScript checks one mount directory, given as $1, with $2 "rw"
// or "ro". It prints exactly one word and always exits 0: cd, the read
// test and the write test are shell built-ins, so nothing on the volume is
// run, read or written.
const volumeProbeScript = `cd -- "$1" 2>/dev/null || { echo notvisible; exit 0; }
[ -r . ] || { echo notreadable; exit 0; }
if [ "$2" = rw ] && [ ! -w . ]; then echo notwritable; exit 0; fi
echo ok`

// volumeHealthTarget is one NFS mount of the agent container: the pod
// volume, where the container mounts it, whether it is read-only by
// configuration, and the configured share it mounts.
type volumeHealthTarget struct {
	volume    string
	mountPath string
	readOnly  bool
	source    mountSource
}

// volumeHealthTargets returns the agent container's mounts of the volumes
// in watch (the NFS volumes the mount check watches), in mount order. A
// volume mounted at several paths gives one target per path. A mount is
// read-only by configuration when the volumeMount, or the volume's NFS or
// claim source, is read-only.
func volumeHealthTargets(pod *corev1.Pod, watch podMountWatch) []volumeHealthTarget {
	if pod == nil || len(watch.sources) == 0 {
		return nil
	}
	volumes := make(map[string]corev1.Volume, len(pod.Spec.Volumes))
	for _, v := range pod.Spec.Volumes {
		volumes[v.Name] = v
	}
	var targets []volumeHealthTarget
	for _, c := range pod.Spec.Containers {
		if c.Name != agentContainerName {
			continue
		}
		for _, m := range c.VolumeMounts {
			src, ok := watch.sources[m.Name]
			if !ok {
				continue
			}
			targets = append(targets, volumeHealthTarget{
				volume:    m.Name,
				mountPath: filepath.Clean(m.MountPath),
				readOnly:  m.ReadOnly || volumeSourceReadOnly(volumes[m.Name]),
				source:    src,
			})
		}
	}
	return targets
}

// volumeSourceReadOnly reports whether v's NFS or claim source is
// read-only.
func volumeSourceReadOnly(v corev1.Volume) bool {
	switch {
	case v.NFS != nil:
		return v.NFS.ReadOnly
	case v.PersistentVolumeClaim != nil:
		return v.PersistentVolumeClaim.ReadOnly
	}
	return false
}

// volumeCheckError is a check that could not run: it says nothing about
// the volumes. class names what failed, never exec output.
type volumeCheckError struct {
	class string
}

func (e *volumeCheckError) Error() string {
	return "NFS volume health check could not run: " + e.class
}

// volumeHealthFailureError returns the volume_unhealthy *LaunchFailure for
// t. Its text names the volume, its configured share, the reason and a
// fixed remedy, never the mount path, mount options or exec output.
func volumeHealthFailureError(t volumeHealthTarget, reason string) error {
	return newLaunchFailure(api.LaunchCodeVolumeUnhealthy, api.LaunchStepContainerStart, reason,
		fmt.Errorf("volume %q from %s is mounted but not usable (%s) — %s",
			t.volume, t.source.text, reason, volumeHealthRemedies[reason]))
}

// checkPodVolumeHealth runs the NFS volume health check on targets in the
// agent container of podName. It returns nil when every target passes a
// check, the volume_unhealthy failure of the first target that failed
// volumeHealthAttempts consecutive checks, or ctx's error. A check that
// cannot run is logged once and returns nil.
func (r *KubernetesRuntime) checkPodVolumeHealth(ctx context.Context, namespace, podName, agentName string, targets []volumeHealthTarget) error {
	if len(targets) == 0 {
		return nil
	}
	interval := r.volumeHealthInterval
	if interval <= 0 {
		interval = defaultVolumeHealthInterval
	}
	timeout := r.volumeHealthProbeTimeout
	if timeout <= 0 {
		timeout = projectfs.ShareVisibleTimeout
	}
	// The wait between checks uses the runtime's exec clock, the one the
	// dial retries use, so tests can observe it.
	clock := r.execReadyClock
	if clock.now == nil || clock.sleep == nil {
		clock = realExecReadyClock()
	}
	pending := targets
	for attempt := 1; ; attempt++ {
		reasons, err := r.probeVolumes(ctx, namespace, podName, agentName, pending, timeout)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			var ce *volumeCheckError
			class := "unknown"
			if errors.As(err, &ce) {
				class = ce.class
			}
			runtimeLog().Warn("NFS volume health check could not run; the start continues",
				"agent", agentName, "volumes", volumeTargetNames(pending), "error_class", class)
			return nil
		}
		var failing []volumeHealthTarget
		var failingReasons []string
		for i, t := range pending {
			if reasons[i] != "" {
				failing = append(failing, t)
				failingReasons = append(failingReasons, reasons[i])
			}
		}
		if len(failing) == 0 {
			return nil
		}
		if attempt >= volumeHealthAttempts {
			t, reason := failing[0], failingReasons[0]
			runtimeLog().Error("NFS volume is mounted but not usable",
				"agent", agentName, "volume", t.volume, "share", t.source.text, "reason", reason)
			return volumeHealthFailureError(t, reason)
		}
		if err := clock.sleep(ctx, interval); err != nil {
			return err
		}
		pending = failing
	}
}

// volumeHealthPhase names the NFS volume health check's execs in the
// dial-retry logs.
const volumeHealthPhase = "volume-health"

// probeVolumes checks targets once and returns each target's fault reason
// ("" when it passed), indexed like targets, or a *volumeCheckError when
// the check could not run. Each exec goes through execInPodDialRetry, so a
// transient exec dial error is retried within the exec's bound rather than
// ending the check.
func (r *KubernetesRuntime) probeVolumes(ctx context.Context, namespace, podName, agentName string, targets []volumeHealthTarget, timeout time.Duration) ([]string, error) {
	tctx, cancel := context.WithTimeout(ctx, timeout)
	out, err := r.execInPodDialRetry(tctx, namespace, podName, volumeMountTableCommand, agentName, volumeHealthPhase)
	cancel()
	if err != nil {
		return nil, &volumeCheckError{class: "mount_table_exec"}
	}
	table, err := projectfs.ParseMountTable(strings.NewReader(out))
	if err != nil {
		return nil, &volumeCheckError{class: "mount_table_parse"}
	}
	// Every container has a root mount: a table without one was not read.
	if _, ok := table.LookupEntry("/"); !ok {
		return nil, &volumeCheckError{class: "mount_table_empty"}
	}
	reasons := make([]string, len(targets))
	for i, t := range targets {
		e, mounted := table.LookupEntry(t.mountPath)
		if !mounted {
			reasons[i] = volumeReasonNotMounted
			continue
		}
		if e.FSType != "nfs" && e.FSType != "nfs4" {
			// Not an NFS mount (a claim bound to another kind of volume):
			// not this check's to judge.
			continue
		}
		if !t.readOnly && hasMountOption(e.Options, "ro") {
			reasons[i] = volumeReasonReadOnly
			continue
		}
		reason, err := r.probeVolume(ctx, namespace, podName, agentName, t, timeout)
		if err != nil {
			return nil, err
		}
		reasons[i] = reason
	}
	return reasons, nil
}

// probeVolume runs volumeProbeScript on t's mount directory and returns
// its fault reason, "" when it passed. The timeout bounds the exec and any
// dial retries together. An exec cut off by that bound is
// ShareUnresponsive: a hung NFS mount is what the check looks for. Any
// other exec error (including a dial error that outlasts its retries), or
// output that is not exactly one known word, is a *volumeCheckError.
func (r *KubernetesRuntime) probeVolume(ctx context.Context, namespace, podName, agentName string, t volumeHealthTarget, timeout time.Duration) (string, error) {
	mode := "rw"
	if t.readOnly {
		mode = "ro"
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := r.execInPodDialRetry(tctx, namespace, podName, []string{"sh", "-c", volumeProbeScript, "sh", t.mountPath, mode}, agentName, volumeHealthPhase)
	if err != nil {
		if ctx.Err() == nil && errors.Is(tctx.Err(), context.DeadlineExceeded) {
			return volumeReasonUnresponsive, nil
		}
		return "", &volumeCheckError{class: "probe_exec"}
	}
	switch strings.TrimSpace(out) {
	case "ok":
		return "", nil
	case "notvisible":
		return volumeReasonNotVisible, nil
	case "notreadable":
		return volumeReasonNotReadable, nil
	case "notwritable":
		if t.readOnly {
			return "", &volumeCheckError{class: "probe_output"}
		}
		return volumeReasonNotWritable, nil
	}
	return "", &volumeCheckError{class: "probe_output"}
}

// hasMountOption reports whether the comma-separated mount options opts
// hold exactly opt.
func hasMountOption(opts, opt string) bool {
	for _, o := range strings.Split(opts, ",") {
		if o == opt {
			return true
		}
	}
	return false
}

// volumeTargetNames returns the volume names of targets, for logs.
func volumeTargetNames(targets []volumeHealthTarget) []string {
	names := make([]string, 0, len(targets))
	for _, t := range targets {
		names = append(names, t.volume)
	}
	return names
}
