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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// Tests for the NFS mount check of waitForPodReady (k8s_mount_check.go;
// ptone/scion#7397).

const (
	mountTestPod    = "p"
	mountTestUID    = types.UID("uid-p-1")
	mountTestServer = "10.0.0.2"
	mountTestPath   = "/exports/scion/p1/workspace"
	mountTestClaim  = "scion-nfs"
)

// Kubelet-shaped mount failure messages. They carry planted markers, a
// kubelet path and mount options that must never reach an error.
const (
	msgInlineAccessDenied = `MountVolume.SetUp failed for volume "workspace" : mount failed: exit status 32
Mounting command: mount
Mounting arguments: -t nfs -o vers=4.1,sec=krb5,opt=planted-mount-option 10.0.0.2:/exports/scion/p1/workspace /var/lib/kubelet/pods/uid-p-1/volumes/kubernetes.io~nfs/workspace
Output: mount.nfs: access denied by server while mounting 10.0.0.2:/exports/scion/p1/workspace planted-marker-q9`
	msgClaimUnmounted = `Unable to attach or mount volumes: unmounted volumes=[workspace], unattached volumes=[kube-api-access-x7k2p workspace]: timed out waiting for the condition`
	msgSecretVolume   = `MountVolume.SetUp failed for volume "agent-secrets" : secret "agent-a1" not found`
	msgOtherUnmounted = `Unable to attach or mount volumes: unmounted volumes=[shared-dir-0], unattached volumes=[shared-dir-0]: timed out waiting for the condition`
)

// plantedLeaks are the parts of the messages above no error may carry.
var plantedLeaks = []string{"planted-mount-option", "planted-marker-q9", "/var/lib/kubelet", "sec=krb5", "vers=4.1", "Mounting arguments", "exit status 32"}

func inlineMountWatch() podMountWatch {
	return podMountWatch{sources: map[string]mountSource{
		"workspace": {inline: true, text: "NFS share " + mountTestServer + ":" + mountTestPath},
	}}
}

func claimMountWatch() podMountWatch {
	return podMountWatch{sources: map[string]mountSource{
		"workspace": {text: `claim "` + mountTestClaim + `"`},
	}}
}

// mountTestRuntime is a fast pod-ready runtime with the mount check's
// grace set and its Events read interval removed.
func mountTestRuntime(grace time.Duration) (*KubernetesRuntime, *fake.Clientset) {
	rt, clientset := fastPodReadyRuntime(false)
	rt.podReadyTimeout = 2 * time.Second
	rt.mountFailGrace = grace
	rt.mountEventReadInterval = time.Nanosecond
	return rt, clientset
}

// scriptMountPodGets is scriptPodGets for a pod with a UID.
func scriptMountPodGets(clientset *fake.Clientset, seq ...corev1.PodStatus) {
	var mu sync.Mutex
	i := 0
	clientset.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		st := seq[i]
		if i < len(seq)-1 {
			i++
		}
		return true, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: action.(k8stesting.GetAction).GetName(), Namespace: action.GetNamespace(), UID: mountTestUID},
			Status:     *st.DeepCopy(),
		}, nil
	})
}

// eventLister records the Events lists of a test.
type eventLister struct {
	mu      sync.Mutex
	actions []k8stesting.ListAction
}

func (l *eventLister) calls() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.actions)
}

// scriptMountEvents answers the n-th Events list (from 1) with events(n),
// or with err when err is non-nil.
func scriptMountEvents(clientset *fake.Clientset, err error, events func(n int) []corev1.Event) *eventLister {
	l := &eventLister{}
	clientset.PrependReactor("list", "events", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		l.mu.Lock()
		l.actions = append(l.actions, action.(k8stesting.ListAction))
		n := len(l.actions)
		l.mu.Unlock()
		if err != nil {
			return true, nil, err
		}
		return true, &corev1.EventList{Items: events(n)}, nil
	})
	return l
}

// podEvent is an Event on the test pod.
func podEvent(uid types.UID, reason, message string, count int32) corev1.Event {
	return corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "ev-" + reason, Namespace: "default"},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: mountTestPod, Namespace: "default", UID: uid},
		Reason:         reason,
		Message:        message,
		Type:           corev1.EventTypeWarning,
		Count:          count,
	}
}

// recurring returns events whose count grows with each list.
func recurring(reason, message string) func(n int) []corev1.Event {
	return func(n int) []corev1.Event {
		return []corev1.Event{podEvent(mountTestUID, reason, message, int32(n))}
	}
}

var (
	// statusMountWaiting: scheduled, no init container, the main container
	// waiting for its volumes.
	statusMountWaiting = corev1.PodStatus{Phase: corev1.PodPending,
		Conditions:        []corev1.PodCondition{condScheduled(corev1.ConditionTrue, "")},
		ContainerStatuses: []corev1.ContainerStatus{mainWaiting("ContainerCreating", "")}}
	// statusMountWaitingInit: the same with an init container that has not
	// started (podLaunchStep reports init_containers).
	statusMountWaitingInit = corev1.PodStatus{Phase: corev1.PodPending,
		Conditions: []corev1.PodCondition{condScheduled(corev1.ConditionTrue, "")},
		InitContainerStatuses: []corev1.ContainerStatus{initState(corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}})},
		ContainerStatuses: []corev1.ContainerStatus{mainWaiting("PodInitializing", "")}}
)

// requireLaunchFailure asserts err is a *LaunchFailure with code, step and
// reason.
func requireLaunchFailure(t *testing.T, err error, code, step, reason string) {
	t.Helper()
	var lf *LaunchFailure
	require.ErrorAs(t, err, &lf, "err = %v", err)
	assert.Equal(t, code, lf.Code)
	assert.Equal(t, step, lf.Step)
	assert.Equal(t, reason, lf.Reason)
}

// launchFailureOf asserts err as requireLaunchFailure does and returns the
// *LaunchFailure, for tests that also check its text.
func launchFailureOf(t *testing.T, err error, code, step, reason string) *LaunchFailure {
	t.Helper()
	requireLaunchFailure(t, err, code, step, reason)
	var lf *LaunchFailure
	require.ErrorAs(t, err, &lf, "err = %v", err)
	return lf
}

// assertNoLeaks fails when text carries any planted part of a message.
func assertNoLeaks(t *testing.T, text string) {
	t.Helper()
	for _, leak := range plantedLeaks {
		assert.NotContains(t, text, leak)
	}
}

// T1: a lasting, recurring FailedMount on an inline NFS volume fails the
// start with volume_mount_failed in container_create, long before the
// pod-ready budget.
func TestWaitForPodReady_InlineNFSFailedMountFailsFast(t *testing.T) {
	rt, clientset := mountTestRuntime(20 * time.Millisecond)
	scriptMountPodGets(clientset, statusMountWaiting)
	scriptMountEvents(clientset, nil, recurring(eventReasonFailedMount, msgInlineAccessDenied))

	start := time.Now()
	err := rt.waitForPodReady(context.Background(), "default", mountTestPod, launchHooks{}, inlineMountWatch())
	lf := launchFailureOf(t, err, api.LaunchCodeVolumeMountFailed, api.LaunchStepContainerCreate, eventReasonFailedMount)
	assert.Less(t, time.Since(start), rt.podReadyTimeout, "must fail before the pod-ready budget")
	assert.Equal(t, `volume "workspace" of pod "p" cannot be mounted from NFS share 10.0.0.2:/exports/scion/p1/workspace (FailedMount: the server denied access to the export) — `+mountRemedyInline, lf.Error())
	assertNoLeaks(t, lf.Error())
}

// T1b: a pod with init containers waits for its volumes in the
// init_containers progress step; the failure is still reported in
// container_create (harness-lead ruling Q2).
func TestWaitForPodReady_MountFailureStepWithInitContainers(t *testing.T) {
	rt, clientset := mountTestRuntime(20 * time.Millisecond)
	scriptMountPodGets(clientset, statusMountWaitingInit)
	scriptMountEvents(clientset, nil, recurring(eventReasonFailedMount, msgInlineAccessDenied))
	rec := &progressRecorder{}

	err := rt.waitForPodReady(context.Background(), "default", mountTestPod, rec.hooks(), inlineMountWatch())
	requireLaunchFailure(t, err, api.LaunchCodeVolumeMountFailed, api.LaunchStepContainerCreate, eventReasonFailedMount)
	steps := rec.snapshot()
	require.NotEmpty(t, steps)
	assert.True(t, strings.HasPrefix(steps[len(steps)-1], api.LaunchStepInitContainers+":"),
		"last progress step %q, want init_containers", steps[len(steps)-1])
}

// T2: a claim-backed NFS volume is matched by kubelet's "unmounted
// volumes" form and fails the same way, with the claim's text and no hint:
// that form only says kubelet's wait timed out, whatever the cause.
func TestWaitForPodReady_ClaimNFSFailedMountFailsFast(t *testing.T) {
	rt, clientset := mountTestRuntime(20 * time.Millisecond)
	scriptMountPodGets(clientset, statusMountWaiting)
	scriptMountEvents(clientset, nil, recurring(eventReasonFailedMount, msgClaimUnmounted))

	err := rt.waitForPodReady(context.Background(), "default", mountTestPod, launchHooks{}, claimMountWatch())
	lf := launchFailureOf(t, err, api.LaunchCodeVolumeMountFailed, api.LaunchStepContainerCreate, eventReasonFailedMount)
	assert.Equal(t, `volume "workspace" of pod "p" cannot be mounted from claim "scion-nfs" (FailedMount) — `+mountRemedyClaim, lf.Error())
}

// T3: a FailedMount on a volume that is not watched (a secret, a shared
// dir that is not on an NFS share), or on an earlier pod of the same name,
// keeps today's behaviour: the start waits for its budget.
func TestWaitForPodReady_NonNFSFailedMountKeepsWaiting(t *testing.T) {
	rt, clientset := mountTestRuntime(time.Millisecond)
	rt.podReadyTimeout = 300 * time.Millisecond
	scriptMountPodGets(clientset, statusMountWaiting)
	l := scriptMountEvents(clientset, nil, func(n int) []corev1.Event {
		return []corev1.Event{
			podEvent(mountTestUID, eventReasonFailedMount, msgSecretVolume, int32(n)),
			podEvent(mountTestUID, eventReasonFailedMount, msgOtherUnmounted, int32(n)),
			podEvent("uid-earlier-pod", eventReasonFailedMount, msgInlineAccessDenied, int32(n)),
			podEvent(mountTestUID, "Scheduled", `for volume "workspace"`, int32(n)),
		}
	})

	err := rt.waitForPodReady(context.Background(), "default", mountTestPod, launchHooks{}, inlineMountWatch())
	requireLaunchFailure(t, err, api.LaunchCodeLaunchTimeout, api.LaunchStepContainerCreate, "")
	assert.Greater(t, l.calls(), 1, "events were read")
}

// T4: a mount failure that clears within the grace does not fail the
// start, even though it recurred.
func TestWaitForPodReady_TransientMountBelowThresholdDoesNotFail(t *testing.T) {
	rt, clientset := mountTestRuntime(time.Hour)
	seq := make([]corev1.PodStatus, 0, 21)
	for range 20 {
		seq = append(seq, statusMountWaiting)
	}
	seq = append(seq, statusRunning)
	scriptMountPodGets(clientset, seq...)
	l := scriptMountEvents(clientset, nil, recurring(eventReasonFailedMount, msgInlineAccessDenied))

	require.NoError(t, rt.waitForPodReady(context.Background(), "default", mountTestPod, launchHooks{}, inlineMountWatch()))
	assert.Greater(t, l.calls(), 2, "the failure was seen recurring")
}

// T5: a matching event that does not recur after it was first seen never
// fails the start, however long ago it was first seen.
func TestWaitForPodReady_StaleMountEventWithoutRecurrenceDoesNotFail(t *testing.T) {
	rt, clientset := mountTestRuntime(time.Millisecond)
	rt.podReadyTimeout = 300 * time.Millisecond
	scriptMountPodGets(clientset, statusMountWaiting)
	l := scriptMountEvents(clientset, nil, func(int) []corev1.Event {
		return []corev1.Event{podEvent(mountTestUID, eventReasonFailedMount, msgInlineAccessDenied, 3)}
	})

	err := rt.waitForPodReady(context.Background(), "default", mountTestPod, launchHooks{}, inlineMountWatch())
	requireLaunchFailure(t, err, api.LaunchCodeLaunchTimeout, api.LaunchStepContainerCreate, "")
	assert.Greater(t, l.calls(), 2, "events were read past the grace")
}

// T6: when Events cannot be read, the start waits as it would without the
// check (fail open), and that is logged once.
func TestWaitForPodReady_MountEventReadErrorFallsBackToWaiting(t *testing.T) {
	forbidden := k8serrors.NewForbidden(schema.GroupResource{Resource: "events"}, "", errors.New("no RBAC"))
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"forbidden", forbidden},
		{"other error", errors.New("connection reset")},
	} {
		t.Run(tc.name+"/pod later runs", func(t *testing.T) {
			rt, clientset := mountTestRuntime(time.Millisecond)
			seq := make([]corev1.PodStatus, 0, 21)
			for range 20 {
				seq = append(seq, statusMountWaiting)
			}
			seq = append(seq, statusRunning)
			scriptMountPodGets(clientset, seq...)
			l := scriptMountEvents(clientset, tc.err, nil)

			require.NoError(t, rt.waitForPodReady(context.Background(), "default", mountTestPod, launchHooks{}, inlineMountWatch()))
			assert.Greater(t, l.calls(), 1)
			assert.True(t, rt.mountEventsFallbackLogged.Load(), "the fallback is logged")
		})
		t.Run(tc.name+"/pod never runs", func(t *testing.T) {
			rt, clientset := mountTestRuntime(time.Millisecond)
			rt.podReadyTimeout = 300 * time.Millisecond
			scriptMountPodGets(clientset, statusMountWaiting)
			scriptMountEvents(clientset, tc.err, nil)

			err := rt.waitForPodReady(context.Background(), "default", mountTestPod, launchHooks{}, inlineMountWatch())
			requireLaunchFailure(t, err, api.LaunchCodeLaunchTimeout, api.LaunchStepContainerCreate, "")
		})
	}
}

// T7: once a container has started the pod's volumes are mounted, so a
// recurring mount event never fails the start and Events are not read.
func TestWaitForPodReady_MountCheckGateClosedAfterProgress(t *testing.T) {
	rt, clientset := mountTestRuntime(time.Millisecond)
	rt.podReadyTimeout = 300 * time.Millisecond
	scriptMountPodGets(clientset, statusInitRunning)
	l := scriptMountEvents(clientset, nil, recurring(eventReasonFailedMount, msgInlineAccessDenied))

	err := rt.waitForPodReady(context.Background(), "default", mountTestPod, launchHooks{}, inlineMountWatch())
	requireLaunchFailure(t, err, api.LaunchCodeLaunchTimeout, api.LaunchStepInitContainers, "")
	assert.Equal(t, 0, l.calls(), "no Events read once a container started")
}

// T8: the Events read is bounded: none for a pod with no NFS volume, at
// most one per read interval, selected by the pod's kind, name and UID,
// with a Limit.
func TestWaitForPodReady_MountEventReadBounds(t *testing.T) {
	t.Run("no NFS volume reads nothing", func(t *testing.T) {
		rt, clientset := mountTestRuntime(time.Millisecond)
		rt.podReadyTimeout = 200 * time.Millisecond
		scriptMountPodGets(clientset, statusMountWaiting)
		l := scriptMountEvents(clientset, nil, recurring(eventReasonFailedMount, msgInlineAccessDenied))

		err := rt.waitForPodReady(context.Background(), "default", mountTestPod, launchHooks{}, podMountWatch{})
		requireLaunchFailure(t, err, api.LaunchCodeLaunchTimeout, api.LaunchStepContainerCreate, "")
		assert.Equal(t, 0, l.calls())
	})
	t.Run("one read per interval, selected and limited", func(t *testing.T) {
		rt, clientset := mountTestRuntime(time.Millisecond)
		rt.podReadyTimeout = 300 * time.Millisecond
		rt.mountEventReadInterval = time.Hour
		scriptMountPodGets(clientset, statusMountWaiting)
		l := scriptMountEvents(clientset, nil, recurring(eventReasonFailedMount, msgInlineAccessDenied))

		err := rt.waitForPodReady(context.Background(), "default", mountTestPod, launchHooks{}, inlineMountWatch())
		requireLaunchFailure(t, err, api.LaunchCodeLaunchTimeout, api.LaunchStepContainerCreate, "")
		require.Equal(t, 1, l.calls(), "one read in the interval")
		a := l.actions[0]
		assert.Equal(t, "default", a.GetNamespace())
		for key, want := range map[string]string{
			"involvedObject.kind": "Pod",
			"involvedObject.name": mountTestPod,
			"involvedObject.uid":  string(mountTestUID),
		} {
			got, ok := a.GetListRestrictions().Fields.RequiresExactMatch(key)
			assert.True(t, ok && got == want, "field selector %s = %q (set %v), want %q", key, got, ok, want)
		}
		impl, ok := a.(k8stesting.ListActionImpl)
		require.True(t, ok, "list action type %T", a)
		assert.Equal(t, int64(mountEventListLimit), impl.GetListOptions().Limit)
	})
}

// T9: the error text names only the volume, the pod, the configured share,
// the reason and a fixed hint; never the event message.
func TestMountFailureGoldenText(t *testing.T) {
	inline := mountSource{inline: true, text: "NFS share 10.0.0.2:/exports/scion/p1/workspace"}
	claim := mountSource{text: `claim "scion-nfs"`}
	for _, tc := range []struct {
		name    string
		source  mountSource
		reason  string
		message string
		named   bool // the message named the volume in form (a)
		want    string
	}{
		{"inline access denied", inline, eventReasonFailedMount, msgInlineAccessDenied, true,
			`volume "workspace" of pod "p" cannot be mounted from NFS share 10.0.0.2:/exports/scion/p1/workspace (FailedMount: the server denied access to the export) — ` + mountRemedyInline},
		{"inline missing path", inline, eventReasonFailedMount, "mount.nfs: mounting 10.0.0.2:/x failed, reason given by server: No such file or directory opt=planted-mount-option", true,
			`volume "workspace" of pod "p" cannot be mounted from NFS share 10.0.0.2:/exports/scion/p1/workspace (FailedMount: the export path does not exist on the server) — ` + mountRemedyInline},
		{"claim refused", claim, eventReasonFailedMount, "mount.nfs: Connection refused planted-marker-q9", true,
			`volume "workspace" of pod "p" cannot be mounted from claim "scion-nfs" (FailedMount: the server could not be reached) — ` + mountRemedyClaim},
		{"claim no route", claim, eventReasonFailedMount, "mount.nfs: No route to host", true,
			`volume "workspace" of pod "p" cannot be mounted from claim "scion-nfs" (FailedMount: the server could not be reached) — ` + mountRemedyClaim},
		{"claim, unmounted form only: no hint", claim, eventReasonFailedMount, msgClaimUnmounted, false,
			`volume "workspace" of pod "p" cannot be mounted from claim "scion-nfs" (FailedMount) — ` + mountRemedyClaim},
		{"timed out, named form", claim, eventReasonFailedMount, `MountVolume.SetUp failed for volume "workspace" : mount.nfs: Connection timed out`, true,
			`volume "workspace" of pod "p" cannot be mounted from claim "scion-nfs" (FailedMount: the server did not answer in time) — ` + mountRemedyClaim},
		{"attach, no hint", claim, eventReasonFailedAttachVolume, "AttachVolume.Attach failed for volume \"workspace\" : rpc error: code = Internal sec=krb5", true,
			`volume "workspace" of pod "p" cannot be mounted from claim "scion-nfs" (FailedAttachVolume) — ` + mountRemedyClaim},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hint := ""
			if tc.named {
				hint = mountFailureHint(tc.message)
			}
			ev := mountEvidence{volume: "workspace", source: tc.source, reason: tc.reason, hint: hint, count: 2}
			err := mountFailureError(mountTestPod, ev)
			lf := launchFailureOf(t, err, api.LaunchCodeVolumeMountFailed, api.LaunchStepContainerCreate, tc.reason)
			assert.Equal(t, tc.want, lf.Error())
			assertNoLeaks(t, lf.Error())
		})
	}
}

// T9b: the evidence read from Events carries the newest matching event's
// reason and hint and the summed count, and nothing of the message.
func TestReadMountEvidence(t *testing.T) {
	rt, clientset := mountTestRuntime(time.Millisecond)
	older := podEvent(mountTestUID, eventReasonFailedMount, msgClaimUnmounted, 2)
	older.LastTimestamp = metav1.NewTime(time.Unix(1000, 0))
	newer := podEvent(mountTestUID, eventReasonFailedMount, msgInlineAccessDenied, 0)
	newer.Series = &corev1.EventSeries{Count: 4}
	newer.LastTimestamp = metav1.NewTime(time.Unix(2000, 0))
	scriptMountEvents(clientset, nil, func(int) []corev1.Event { return []corev1.Event{older, newer} })
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: mountTestPod, UID: mountTestUID}}

	ev, ok := rt.readMountEvidence(context.Background(), "default", pod, inlineMountWatch())
	require.True(t, ok)
	assert.Equal(t, mountEvidence{volume: "workspace", source: inlineMountWatch().sources["workspace"],
		reason: eventReasonFailedMount, hint: "the server denied access to the export", count: 6}, ev)
}

// T10: the watch holds the inline NFS volumes and the volumes of the
// start's NFS share claims, and nothing else.
func TestPodMountWatch(t *testing.T) {
	claimVol := func(name, claim string) corev1.Volume {
		return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim}}}
	}
	pod := &corev1.Pod{Spec: corev1.PodSpec{Volumes: []corev1.Volume{
		{Name: "workspace-git", VolumeSource: corev1.VolumeSource{NFS: &corev1.NFSVolumeSource{Server: mountTestServer, Path: "/exports/scion/p1/git"}}},
		{Name: "shared-dir-0", VolumeSource: corev1.VolumeSource{NFS: &corev1.NFSVolumeSource{Server: mountTestServer, Path: "/exports/scion/p1/shared-dirs/a"}}},
		claimVol("workspace", "ws-claim"),
		claimVol("agent-home", "home-claim"),
		claimVol("shared-dirs", "shared-claim"),
		claimVol("cr-shared-dirs", "cr-shared-claim"),
		claimVol("per-dir", "scion-p1-shared-b"),
		{Name: "agent-secrets", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "s"}}},
		{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}}}
	inlineOnly := map[string]mountSource{
		"workspace-git": {inline: true, text: "NFS share 10.0.0.2:/exports/scion/p1/git"},
		"shared-dir-0":  {inline: true, text: "NFS share 10.0.0.2:/exports/scion/p1/shared-dirs/a"},
	}
	with := func(extra map[string]mountSource) map[string]mountSource {
		m := map[string]mountSource{}
		for k, v := range inlineOnly {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	for _, tc := range []struct {
		name   string
		config RunConfig
		want   map[string]mountSource
	}{
		{"no NFS claims: inline only", RunConfig{}, inlineOnly},
		{"NFS workspace claim", RunConfig{WorkspaceBackendName: "nfs", NFSPVClaimName: "ws-claim"},
			with(map[string]mountSource{"workspace": {text: `claim "ws-claim"`}})},
		{"workspace claim of another backend is not NFS", RunConfig{WorkspaceBackendName: "local", NFSPVClaimName: "ws-claim"}, inlineOnly},
		{"home claim", RunConfig{HomeStorage: &HomeStorageRealization{PVClaimName: "home-claim"}},
			with(map[string]mountSource{"agent-home": {text: `claim "home-claim"`}})},
		{"NFS shared-dir claim", RunConfig{SharedDirStorage: &SharedDirRealization{Backend: "nfs", PVClaimName: "shared-claim"}},
			with(map[string]mountSource{"shared-dirs": {text: `claim "shared-claim"`}})},
		{"cloudrun-volume shared-dir claim is watched", RunConfig{SharedDirStorage: &SharedDirRealization{Backend: "cloudrun-volume", PVClaimName: "cr-shared-claim"}},
			with(map[string]mountSource{"cr-shared-dirs": {text: `claim "cr-shared-claim"`}})},
		{"gke-shared-volume claim is not NFS", RunConfig{SharedDirStorage: &SharedDirRealization{Backend: "gke-shared-volume", PVClaimName: "shared-claim"}}, inlineOnly},
		{"local shared-dir backend is not NFS", RunConfig{SharedDirStorage: &SharedDirRealization{Backend: "local", PVClaimName: "shared-claim"}}, inlineOnly},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, newPodMountWatch(pod, tc.config).sources)
		})
	}
	t.Run("pod with no NFS volume watches nothing", func(t *testing.T) {
		w := newPodMountWatch(&corev1.Pod{Spec: corev1.PodSpec{Volumes: []corev1.Volume{
			{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}}}, RunConfig{})
		assert.True(t, w.empty())
		assert.True(t, newPodMountWatch(nil, RunConfig{}).empty())
	})
}

// T11: the volume names of each kubelet message form, and nothing from a
// message in neither form.
func TestMountEventVolumeNames(t *testing.T) {
	for _, tc := range []struct {
		msg       string
		named     []string
		unmounted []string
	}{
		{msgInlineAccessDenied, []string{"workspace"}, nil},
		{msgClaimUnmounted, nil, []string{"workspace"}},
		{`MountVolume.MountDevice failed for volume "pv-nfs-1" : rpc error`, []string{"pv-nfs-1"}, nil},
		{`Unable to attach or mount volumes: unmounted volumes=[workspace workspace-git], unattached volumes=[]`, nil, []string{"workspace", "workspace-git"}},
		{`Unable to attach or mount volumes: unmounted volumes=[a,b]`, nil, []string{"a", "b"}},
		{`mount failed for volume workspace`, nil, nil},
		{`unmounted volumes=[`, nil, nil},
		{"", nil, nil},
	} {
		assert.Equal(t, tc.named, mountMsgNamedVolumes(tc.msg), "named form of %q", tc.msg)
		assert.Equal(t, tc.unmounted, mountMsgUnmountedVolumes(tc.msg), "unmounted form of %q", tc.msg)
	}
}

// T6b: a failed Events read is logged once while the wait is live, but an
// error caused by the wait's own end (its ctx done) is not a read failure
// and is not logged; either way it counts as no evidence.
func TestReadMountEvidence_FallbackLogOnlyForReadErrors(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: mountTestPod, UID: mountTestUID}}
	t.Run("wait ended", func(t *testing.T) {
		rt, clientset := mountTestRuntime(time.Millisecond)
		scriptMountEvents(clientset, context.Canceled, nil)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, ok := rt.readMountEvidence(ctx, "default", pod, inlineMountWatch())
		assert.False(t, ok)
		assert.False(t, rt.mountEventsFallbackLogged.Load(), "the wait's end is not logged as a read failure")
	})
	t.Run("live wait", func(t *testing.T) {
		rt, clientset := mountTestRuntime(time.Millisecond)
		scriptMountEvents(clientset, errors.New("connection reset"), nil)
		_, ok := rt.readMountEvidence(context.Background(), "default", pod, inlineMountWatch())
		assert.False(t, ok)
		assert.True(t, rt.mountEventsFallbackLogged.Load(), "a read failure on a live wait is logged")
	})
	t.Run("live wait, the read's own timeout", func(t *testing.T) {
		rt, clientset := mountTestRuntime(time.Millisecond)
		scriptMountEvents(clientset, context.DeadlineExceeded, nil)
		_, ok := rt.readMountEvidence(context.Background(), "default", pod, inlineMountWatch())
		assert.False(t, ok)
		assert.True(t, rt.mountEventsFallbackLogged.Load(), "the 2s read timeout on a live wait is a read failure, and is logged")
	})
}

// T9c: the hint comes only from the newest matching event that named its
// volume in form (a); a form (b) event, newest or alone, gives none.
func TestReadMountEvidence_HintFromNamedFormOnly(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: mountTestPod, UID: mountTestUID}}
	at := func(e corev1.Event, sec int64) corev1.Event {
		e.LastTimestamp = metav1.NewTime(time.Unix(sec, 0))
		return e
	}
	t.Run("unmounted form only", func(t *testing.T) {
		rt, clientset := mountTestRuntime(time.Millisecond)
		scriptMountEvents(clientset, nil, func(int) []corev1.Event {
			return []corev1.Event{at(podEvent(mountTestUID, eventReasonFailedMount, msgClaimUnmounted, 3), 1000)}
		})
		ev, ok := rt.readMountEvidence(context.Background(), "default", pod, claimMountWatch())
		require.True(t, ok)
		assert.Equal(t, "", ev.hint)
		assert.Equal(t, "workspace", ev.volume)
	})
	t.Run("an older named-form event keeps its hint", func(t *testing.T) {
		rt, clientset := mountTestRuntime(time.Millisecond)
		scriptMountEvents(clientset, nil, func(int) []corev1.Event {
			return []corev1.Event{
				at(podEvent(mountTestUID, eventReasonFailedMount, msgInlineAccessDenied, 2), 1000),
				at(podEvent(mountTestUID, eventReasonFailedMount, msgClaimUnmounted, 1), 2000),
			}
		})
		ev, ok := rt.readMountEvidence(context.Background(), "default", pod, inlineMountWatch())
		require.True(t, ok)
		assert.Equal(t, "the server denied access to the export", ev.hint)
		assert.Equal(t, int64(3), ev.count)
	})
	t.Run("another volume's named-form event gives no hint", func(t *testing.T) {
		rt, clientset := mountTestRuntime(time.Millisecond)
		watch := podMountWatch{sources: map[string]mountSource{
			"workspace":  inlineMountWatch().sources["workspace"],
			"agent-home": {text: `claim "home-claim"`},
		}}
		scriptMountEvents(clientset, nil, func(int) []corev1.Event {
			return []corev1.Event{
				at(podEvent(mountTestUID, eventReasonFailedMount, msgInlineAccessDenied, 2), 1000),
				at(podEvent(mountTestUID, eventReasonFailedMount, `Unable to attach or mount volumes: unmounted volumes=[agent-home workspace], unattached volumes=[]: timed out waiting for the condition`, 1), 2000),
			}
		})
		ev, ok := rt.readMountEvidence(context.Background(), "default", pod, watch)
		require.True(t, ok)
		assert.Equal(t, "agent-home", ev.volume)
		assert.Equal(t, "", ev.hint, "workspace's access-denied cause must not be reported for agent-home")
	})
}

// TestReadMountEvidence_RequiresPodUID: events are matched by the pod's
// UID only, so a pod with no UID yields no evidence and no read.
func TestReadMountEvidence_RequiresPodUID(t *testing.T) {
	rt, clientset := mountTestRuntime(time.Millisecond)
	l := scriptMountEvents(clientset, nil, func(int) []corev1.Event {
		return []corev1.Event{podEvent("", eventReasonFailedMount, msgInlineAccessDenied, 5)}
	})
	_, ok := rt.readMountEvidence(context.Background(), "default",
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: mountTestPod}}, inlineMountWatch())
	assert.False(t, ok)
	assert.Equal(t, 0, l.calls())
}

// TestMountGateOpen: the gate is open only while the pod is scheduled and
// no container has started.
func TestMountGateOpen(t *testing.T) {
	scheduled := []corev1.PodCondition{condScheduled(corev1.ConditionTrue, "")}
	sidecarRunning := corev1.ContainerStatus{Name: "istio-proxy", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}
	for _, tc := range []struct {
		name   string
		status corev1.PodStatus
		want   bool
	}{
		{"not scheduled", corev1.PodStatus{Conditions: []corev1.PodCondition{condScheduled(corev1.ConditionFalse, corev1.PodReasonUnschedulable)}}, false},
		{"no scheduled condition", corev1.PodStatus{}, false},
		{"scheduled, no container status yet", corev1.PodStatus{Conditions: scheduled}, true},
		{"main ContainerCreating", corev1.PodStatus{Conditions: scheduled, ContainerStatuses: []corev1.ContainerStatus{mainWaiting("ContainerCreating", "")}}, true},
		{"main PodInitializing", corev1.PodStatus{Conditions: scheduled, ContainerStatuses: []corev1.ContainerStatus{mainWaiting("PodInitializing", "")}}, true},
		{"main CrashLoopBackOff", corev1.PodStatus{Conditions: scheduled, ContainerStatuses: []corev1.ContainerStatus{mainWaiting("CrashLoopBackOff", "")}}, false},
		{"main Running", corev1.PodStatus{Conditions: scheduled, ContainerStatuses: []corev1.ContainerStatus{mainRunning()}}, false},
		{"another container's status is ignored", corev1.PodStatus{Conditions: scheduled, ContainerStatuses: []corev1.ContainerStatus{sidecarRunning, mainWaiting("ContainerCreating", "")}}, true},
		{"init Waiting", corev1.PodStatus{Conditions: scheduled, InitContainerStatuses: []corev1.ContainerStatus{initState(corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}})}}, true},
		{"init Running", corev1.PodStatus{Conditions: scheduled, InitContainerStatuses: []corev1.ContainerStatus{initState(corev1.ContainerState{Running: &corev1.ContainerStateRunning{}})}}, false},
		{"init Terminated", corev1.PodStatus{Conditions: scheduled, InitContainerStatuses: []corev1.ContainerStatus{initState(corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Completed"}})}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, mountGateOpen(&corev1.Pod{Status: tc.status}))
		})
	}
}

// TestCheckMounts_ResetsEvidence: evidence that disappears, or a gate that
// closes, forgets the evidence seen so far, so a failure that comes back
// starts its grace and recurrence over instead of failing at once.
func TestCheckMounts_ResetsEvidence(t *testing.T) {
	waiting := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: mountTestPod, UID: mountTestUID}, Status: statusMountWaiting}
	initRunning := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: mountTestPod, UID: mountTestUID}, Status: statusInitRunning}
	// steppingRuntime has a clock that moves 1s per read of it, a grace of
	// 1ns, and events counted per list call by counts (0 means none).
	steppingRuntime := func(counts ...int32) *KubernetesRuntime {
		rt, clientset := mountTestRuntime(time.Nanosecond)
		var mu sync.Mutex
		clock := time.Unix(10000, 0)
		rt.nowFn = func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			clock = clock.Add(time.Second)
			return clock
		}
		scriptMountEvents(clientset, nil, func(n int) []corev1.Event {
			if n > len(counts) || counts[n-1] == 0 {
				return nil
			}
			return []corev1.Event{podEvent(mountTestUID, eventReasonFailedMount, msgInlineAccessDenied, counts[n-1])}
		})
		return rt
	}
	t.Run("evidence disappears", func(t *testing.T) {
		rt := steppingRuntime(1, 0, 5, 6)
		mc := rt.newMountCheck(inlineMountWatch())
		ctx := context.Background()
		require.NoError(t, rt.checkMounts(ctx, "default", mc, waiting), "first seen")
		require.NoError(t, rt.checkMounts(ctx, "default", mc, waiting), "gone: reset")
		require.NoError(t, rt.checkMounts(ctx, "default", mc, waiting), "back with a higher count: seen anew, no immediate failure")
		requireLaunchFailure(t, rt.checkMounts(ctx, "default", mc, waiting),
			api.LaunchCodeVolumeMountFailed, api.LaunchStepContainerCreate, eventReasonFailedMount)
	})
	t.Run("gate closes", func(t *testing.T) {
		rt := steppingRuntime(1, 5, 6)
		mc := rt.newMountCheck(inlineMountWatch())
		ctx := context.Background()
		require.NoError(t, rt.checkMounts(ctx, "default", mc, waiting), "first seen")
		require.NoError(t, rt.checkMounts(ctx, "default", mc, initRunning), "gate closed: reset, no read")
		require.NoError(t, rt.checkMounts(ctx, "default", mc, waiting), "open again with a higher count: seen anew")
		requireLaunchFailure(t, rt.checkMounts(ctx, "default", mc, waiting),
			api.LaunchCodeVolumeMountFailed, api.LaunchStepContainerCreate, eventReasonFailedMount)
	})
}
