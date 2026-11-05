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
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
)

// The NFS mount check of waitForPodReady: a pod whose NFS volume cannot be
// mounted never leaves ContainerCreating (or PodInitializing), and its
// status carries no terminal reason, so without it the start waits for the
// whole pod-ready budget. The check reads the pod's Events, bounded, and
// fails the start with volume_mount_failed once a mount failure on one of
// the pod's NFS volumes has lasted mountFailGrace and recurred. It only
// reads, and it fails open: when Events cannot be read the wait goes on as
// it would without the check.

// Defaults of the NFS mount check. defaultMountFailGrace is how long a
// mount failure must have been seen before the start fails: kubelet
// retries a failed mount with a backoff from 500ms that doubles, so a
// transient failure normally clears within about 30s. The check reads
// Events at most once per defaultMountEventReadInterval, each read bounded
// by mountEventLookupTimeout and mountEventListLimit.
const (
	defaultMountFailGrace         = 60 * time.Second
	defaultMountEventReadInterval = 10 * time.Second
	mountEventLookupTimeout       = 2 * time.Second
	mountEventListLimit           = 100
)

// Pod Event reasons the NFS mount check acts on.
const (
	eventReasonFailedMount        = "FailedMount"
	eventReasonFailedAttachVolume = "FailedAttachVolume"
)

// Fixed remedies of a volume_mount_failed error, for an inline NFS volume
// and for a claim.
const (
	mountRemedyInline = "check that the server exports this path to the cluster nodes and is reachable from them"
	mountRemedyClaim  = "check the server and export of the claim's PersistentVolume, and that the server is reachable from the cluster nodes"
)

// mountSource is the configured share an NFS pod volume mounts: an inline
// NFS server path, or a claim. text describes it for errors.
type mountSource struct {
	inline bool
	text   string
}

// podMountWatch is the set of a start's pod volumes the NFS mount check
// watches: each pod volume name mapped to its configured share. A zero
// podMountWatch watches nothing, and the check then reads no Events.
type podMountWatch struct {
	sources map[string]mountSource
}

// newPodMountWatch returns the NFS volumes of pod, built for config: every
// inline NFS volume, and every volume of a claim that is one of the start's
// NFS share claims (nfsShareClaims). Any other volume is not watched.
func newPodMountWatch(pod *corev1.Pod, config RunConfig) podMountWatch {
	if pod == nil {
		return podMountWatch{}
	}
	claims := nfsShareClaims(config)
	w := podMountWatch{sources: make(map[string]mountSource)}
	for _, v := range pod.Spec.Volumes {
		switch {
		case v.NFS != nil:
			w.sources[v.Name] = mountSource{inline: true, text: fmt.Sprintf("NFS share %s:%s", v.NFS.Server, v.NFS.Path)}
		case v.PersistentVolumeClaim != nil && claims[v.PersistentVolumeClaim.ClaimName]:
			w.sources[v.Name] = mountSource{text: fmt.Sprintf("claim %q", v.PersistentVolumeClaim.ClaimName)}
		}
	}
	return w
}

// nfsShareClaims is the set of claims of a start's NFS shares: the
// workspace claim of an NFS workspace, the agent home claim, and the claim
// of shared dirs on an allowed backend (nfsSharedDirBackends). A
// gke-shared-volume claim, the per-dir claims of the local backend and any
// other backend's claim are not watched.
func nfsShareClaims(config RunConfig) map[string]bool {
	claims := make(map[string]bool)
	add := func(name string) {
		if name != "" {
			claims[name] = true
		}
	}
	if config.WorkspaceBackendName == "nfs" {
		add(config.NFSPVClaimName)
	}
	if config.HomeStorage != nil {
		add(config.HomeStorage.PVClaimName)
	}
	if sds := config.SharedDirStorage; sds != nil && nfsSharedDirBackends[projectfs.Backend(sds.Backend)] {
		add(sds.PVClaimName)
	}
	return claims
}

// nfsSharedDirBackends allows the shared-dir backends whose claim is an
// NFS share claim: nfs, and cloudrun-volume, which Kubernetes also mounts
// from the share's NFS-backed claim (see the shared-directory comment in
// KubernetesRuntime.buildPod).
var nfsSharedDirBackends = map[projectfs.Backend]bool{
	projectfs.BackendNFS:            true,
	projectfs.BackendCloudRunVolume: true,
}

// empty reports whether w watches no volume.
func (w podMountWatch) empty() bool {
	return len(w.sources) == 0
}

// Kubelet names a volume in a mount failure message in one of two forms:
// `... for volume "<name>" ...` (the pod volume name of an inline volume,
// the PersistentVolume name of a claim), and `unmounted volumes=[<a> <b>]`
// (pod volume names).
var (
	mountMsgVolumeRe    = regexp.MustCompile(`for volume "([^"]+)"`)
	mountMsgUnmountedRe = regexp.MustCompile(`unmounted volumes=\[([^\]]*)\]`)
)

// mountMsgNamedVolumes returns the volume names of form (a) in msg.
func mountMsgNamedVolumes(msg string) []string {
	var names []string
	for _, m := range mountMsgVolumeRe.FindAllStringSubmatch(msg, -1) {
		names = append(names, m[1])
	}
	return names
}

// mountMsgUnmountedVolumes returns the volume names of form (b) in msg.
func mountMsgUnmountedVolumes(msg string) []string {
	m := mountMsgUnmountedRe.FindStringSubmatch(msg)
	if m == nil {
		return nil
	}
	return strings.FieldsFunc(m[1], func(r rune) bool { return r == ' ' || r == ',' })
}

// match returns the first watched volume a mount failure message names,
// and whether it was named in form (a), the form that carries the mount's
// own output. A form (b) message only says that kubelet's wait for the
// pod's volumes timed out, whatever the cause.
func (w podMountWatch) match(msg string) (name string, namedForm bool, ok bool) {
	for _, n := range mountMsgNamedVolumes(msg) {
		if _, watched := w.sources[n]; watched {
			return n, true, true
		}
	}
	for _, n := range mountMsgUnmountedVolumes(msg) {
		if _, watched := w.sources[n]; watched {
			return n, false, true
		}
	}
	return "", false, false
}

// mountFailureHints maps fixed, case-insensitive substrings of a mount
// failure message to a fixed phrase. Only the phrase is ever reported,
// never the message. The first match wins.
var mountFailureHints = []struct {
	substrings []string
	hint       string
}{
	{[]string{"access denied"}, "the server denied access to the export"},
	{[]string{"no such file or directory"}, "the export path does not exist on the server"},
	{[]string{"connection refused", "no route to host"}, "the server could not be reached"},
	{[]string{"timed out", "deadline exceeded"}, "the server did not answer in time"},
}

// mountFailureHint returns the fixed phrase for msg, or "".
func mountFailureHint(msg string) string {
	lower := strings.ToLower(msg)
	for _, h := range mountFailureHints {
		for _, s := range h.substrings {
			if strings.Contains(lower, s) {
				return h.hint
			}
		}
	}
	return ""
}

// mountEvidence is what one Events read found about the watched volumes:
// the volume and reason of the newest matching event, the hint of the
// newest event that named that same volume in form (a) ("" when none
// did), and the summed occurrence count of all matching events.
type mountEvidence struct {
	volume string
	source mountSource
	reason string
	hint   string
	count  int64
}

// mountFailureError returns the volume_mount_failed *LaunchFailure for ev.
// Its text names the volume, the pod, the configured share, the Kubernetes
// reason and a fixed hint, never the event message: that can carry mount
// arguments and options.
func mountFailureError(podName string, ev mountEvidence) error {
	hint := ""
	if ev.hint != "" {
		hint = ": " + ev.hint
	}
	remedy := mountRemedyClaim
	if ev.source.inline {
		remedy = mountRemedyInline
	}
	return newLaunchFailure(api.LaunchCodeVolumeMountFailed, api.LaunchStepContainerCreate, ev.reason,
		fmt.Errorf("volume %q of pod %q cannot be mounted from %s (%s%s) — %s",
			ev.volume, podName, ev.source.text, ev.reason, hint, remedy))
}

// mountCheck is the NFS mount check's state for one waitForPodReady.
type mountCheck struct {
	watch    podMountWatch
	grace    time.Duration
	interval time.Duration

	// lastRead is when Events were last read (zero before the first read).
	lastRead time.Time
	// firstSeen is when matching evidence was first seen (zero when there
	// is none), and countAtFirst its occurrence count then.
	firstSeen    time.Time
	countAtFirst int64
}

// newMountCheck returns the check of watch, with the runtime's grace and
// read interval (the defaults when unset).
func (r *KubernetesRuntime) newMountCheck(watch podMountWatch) *mountCheck {
	mc := &mountCheck{watch: watch, grace: r.mountFailGrace, interval: r.mountEventReadInterval}
	if mc.grace <= 0 {
		mc.grace = defaultMountFailGrace
	}
	if mc.interval <= 0 {
		mc.interval = defaultMountEventReadInterval
	}
	return mc
}

// reset forgets the evidence seen so far.
func (mc *mountCheck) reset() {
	mc.firstSeen, mc.countAtFirst = time.Time{}, 0
}

// mountGateOpen reports whether pod may still be waiting for its volumes:
// it is scheduled and no container has started. Kubelet mounts every
// volume before any container starts, so a pod past that point is never
// failed on a mount event.
func mountGateOpen(pod *corev1.Pod) bool {
	scheduled := false
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionTrue {
			scheduled = true
		}
	}
	if !scheduled {
		return false
	}
	for _, ics := range pod.Status.InitContainerStatuses {
		if ics.State.Running != nil || ics.State.Terminated != nil {
			return false
		}
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != agentContainerName {
			continue
		}
		if cs.State.Waiting == nil {
			return false
		}
		if r := cs.State.Waiting.Reason; r != "ContainerCreating" && r != "PodInitializing" {
			return false
		}
	}
	return true
}

// checkMounts runs the NFS mount check on one poll of pod. It returns the
// volume_mount_failed *LaunchFailure once matching evidence was first seen
// at least mc.grace ago and its occurrence count has grown since, else nil.
// It reads Events at most once per mc.interval, and only while the gate is
// open; any read failure counts as no evidence.
func (r *KubernetesRuntime) checkMounts(ctx context.Context, namespace string, mc *mountCheck, pod *corev1.Pod) error {
	if mc == nil || mc.watch.empty() {
		return nil
	}
	if !mountGateOpen(pod) {
		mc.reset()
		return nil
	}
	now := r.now()
	if !mc.lastRead.IsZero() && now.Sub(mc.lastRead) < mc.interval {
		return nil
	}
	mc.lastRead = now
	ev, ok := r.readMountEvidence(ctx, namespace, pod, mc.watch)
	if !ok {
		mc.reset()
		return nil
	}
	if mc.firstSeen.IsZero() {
		mc.firstSeen, mc.countAtFirst = now, ev.count
		return nil
	}
	if now.Sub(mc.firstSeen) < mc.grace || ev.count <= mc.countAtFirst {
		return nil
	}
	runtimeLog().Error("NFS volume cannot be mounted", "pod", pod.Name, "namespace", namespace,
		"volume", ev.volume, "reason", ev.reason, "phase", "volume-mount")
	return mountFailureError(pod.Name, ev)
}

// readMountEvidence lists pod's Events once, bounded, and returns the
// evidence about the watched volumes. ok is false when no matching event
// was found or the Events could not be read; a read failure is logged once
// per runtime instance.
func (r *KubernetesRuntime) readMountEvidence(ctx context.Context, namespace string, pod *corev1.Pod, watch podMountWatch) (ev mountEvidence, ok bool) {
	if r.Client == nil || r.Client.Clientset == nil {
		return mountEvidence{}, false
	}
	// Events are matched by the pod's UID, so an event of an earlier pod
	// of the same name never counts; with no UID there is nothing to match.
	if pod.UID == "" {
		return mountEvidence{}, false
	}
	lookupCtx, cancel := context.WithTimeout(ctx, mountEventLookupTimeout)
	defer cancel()
	set := fields.Set{
		"involvedObject.kind": "Pod",
		"involvedObject.name": pod.Name,
		"involvedObject.uid":  string(pod.UID),
	}
	list, err := r.Client.Clientset.CoreV1().Events(namespace).List(lookupCtx,
		metav1.ListOptions{FieldSelector: set.AsSelector().String(), Limit: mountEventListLimit})
	if err != nil {
		// The wait's own end is not a read failure: the wait reports it.
		if ctx.Err() == nil && r.mountEventsFallbackLogged.CompareAndSwap(false, true) {
			runtimeLog().Warn("Cannot read events for agent pods; an NFS volume that cannot be mounted is reported only when the start times out (grant events list to fail such starts early)",
				"namespace", namespace, "error", err)
		}
		return mountEvidence{}, false
	}
	// namedByVolume holds, per volume, the newest matching event that named
	// it in form (a); only the reported volume's own gives the hint.
	var newest *corev1.Event
	namedByVolume := make(map[string]*corev1.Event)
	for i := range list.Items {
		e := &list.Items[i]
		// Field selectors are not honoured everywhere (for example by
		// fake clients); match the pod here too.
		if e.InvolvedObject.Kind != "Pod" || e.InvolvedObject.Name != pod.Name {
			continue
		}
		if e.InvolvedObject.UID != pod.UID {
			continue
		}
		if e.Reason != eventReasonFailedMount && e.Reason != eventReasonFailedAttachVolume {
			continue
		}
		volume, named, matched := watch.match(e.Message)
		if !matched {
			continue
		}
		ev.count += eventCount(e)
		if newest == nil || eventTime(e).After(eventTime(newest)) {
			newest, ev.volume = e, volume
		}
		if cur := namedByVolume[volume]; named && (cur == nil || eventTime(e).After(eventTime(cur))) {
			namedByVolume[volume] = e
		}
	}
	if newest == nil {
		return mountEvidence{}, false
	}
	ev.source = watch.sources[ev.volume]
	ev.reason = newest.Reason
	if named := namedByVolume[ev.volume]; named != nil {
		ev.hint = mountFailureHint(named.Message)
	}
	return ev, true
}

// eventCount is how many times e occurred: the larger of its count and its
// series count, and at least 1.
func eventCount(e *corev1.Event) int64 {
	c := int64(e.Count)
	if e.Series != nil && int64(e.Series.Count) > c {
		c = int64(e.Series.Count)
	}
	if c < 1 {
		c = 1
	}
	return c
}
