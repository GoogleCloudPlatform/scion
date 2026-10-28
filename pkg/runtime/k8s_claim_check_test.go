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
	"bytes"
	"context"
	"log/slog"
	"slices"
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
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// seedBoundClaims adds a Bound claim for each name to namespace, unless one
// already exists, so a Run test passes the claim check before the pod. It
// writes through the fake's tracker, so the clientset records no action.
func seedBoundClaims(t *testing.T, clientset *k8sfake.Clientset, namespace string, names ...string) {
	t.Helper()
	for _, name := range names {
		err := clientset.Tracker().Add(&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
		})
		if err != nil && !k8serrors.IsAlreadyExists(err) {
			t.Fatalf("seeding claim %s/%s: %v", namespace, name, err)
		}
	}
}

// seedConfigClaims seeds a Bound claim for every claim a start of cfg
// mounts (see seedBoundClaims).
func seedConfigClaims(t *testing.T, clientset *k8sfake.Clientset, namespace string, cfg RunConfig) {
	t.Helper()
	for name := range podClaimSources(cfg) {
		seedBoundClaims(t, clientset, namespace, name)
	}
}

// shortenClaimCheckWait sets the claim check's wait and poll interval for
// one test.
func shortenClaimCheckWait(t *testing.T, wait, interval time.Duration) {
	t.Helper()
	prevWait, prevInterval := claimCheckWait, claimCheckPollInterval
	claimCheckWait, claimCheckPollInterval = wait, interval
	t.Cleanup(func() { claimCheckWait, claimCheckPollInterval = prevWait, prevInterval })
}

func claimCheckPod(claims ...string) *corev1.Pod {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "default"}}
	for i, c := range claims {
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
			Name:         "vol-" + string(rune('a'+i)),
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: c}},
		})
	}
	// A volume without a claim is not checked.
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
	return pod
}

func createClaim(t *testing.T, clientset *k8sfake.Clientset, name string, uid types.UID, phase corev1.PersistentVolumeClaimPhase) {
	t.Helper()
	_, err := clientset.CoreV1().PersistentVolumeClaims("default").Create(context.Background(), &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: uid},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: phase},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
}

func createClaimEvent(t *testing.T, clientset *k8sfake.Clientset, name, claim string, uid types.UID, reason, message string, at time.Time) {
	t.Helper()
	_, err := clientset.CoreV1().Events("default").Create(context.Background(), &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: name, Namespace: "default"},
		InvolvedObject: corev1.ObjectReference{Kind: "PersistentVolumeClaim", Name: claim, Namespace: "default", UID: uid},
		Reason:         reason,
		Message:        message,
		LastTimestamp:  metav1.NewTime(at),
	}, metav1.CreateOptions{})
	require.NoError(t, err)
}

func actionVerbs(clientset *k8sfake.Clientset, resource string) []string {
	var verbs []string
	for _, a := range clientset.Actions() {
		if a.GetResource().Resource == resource {
			verbs = append(verbs, a.GetVerb())
		}
	}
	return verbs
}

func TestVerifyPodClaims_MissingClaimFails(t *testing.T) {
	shortenClaimCheckWait(t, time.Second, 10*time.Millisecond)
	rt, _, _ := newTestK8sRuntime()
	sources := podClaimSources(RunConfig{SharedDirStorage: sharedDirsOnClaim("nfs", "nfs-share", "projects/p1", "data")})

	start := time.Now()
	err := rt.verifyPodClaims(context.Background(), "default", claimCheckPod("nfs-share"), sources)
	require.Error(t, err)
	assert.Less(t, time.Since(start), 500*time.Millisecond, "a missing claim fails without waiting")
	assert.Contains(t, err.Error(), "claim default/nfs-share does not exist")
	assert.Contains(t, err.Error(), "server.project_storage nfs.shares[0].pv_name")
	assert.Contains(t, err.Error(), "Create the claim in namespace default")
}

func TestVerifyPodClaims_BoundPasses(t *testing.T) {
	rt, clientset, _ := newTestK8sRuntime()
	createClaim(t, clientset, "c1", "uid-1", corev1.ClaimBound)
	clientset.ClearActions()

	require.NoError(t, rt.verifyPodClaims(context.Background(), "default", claimCheckPod("c1", "c1"), nil))
	assert.Equal(t, []string{"get"}, actionVerbs(clientset, "persistentvolumeclaims"), "one read per unique claim")
	assert.Empty(t, actionVerbs(clientset, "events"), "a Bound claim needs no events")
}

func TestVerifyPodClaims_ProvisioningFailedFails(t *testing.T) {
	shortenClaimCheckWait(t, 5*time.Second, 10*time.Millisecond)
	rt, clientset, _ := newTestK8sRuntime()
	createClaim(t, clientset, "c1", "uid-1", corev1.ClaimPending)
	now := time.Now()
	// An event of an earlier claim with the same name does not count.
	createClaimEvent(t, clientset, "old", "c1", "uid-old", claimEventProvisioningFailed, "old failure", now.Add(-time.Hour))
	createClaimEvent(t, clientset, "e1", "c1", "uid-1", claimEventProvisioningFailed, "storageclass \"x\" not found", now)

	start := time.Now()
	err := rt.verifyPodClaims(context.Background(), "default", claimCheckPod("c1"), map[string]string{"c1": "the per-dir claim of shared dir \"data\""})
	require.Error(t, err)
	assert.Less(t, time.Since(start), time.Second, "fails within one poll")
	assert.Contains(t, err.Error(), "claim default/c1 cannot be provisioned")
	assert.Contains(t, err.Error(), "shared dir \"data\"")
	assert.Contains(t, err.Error(), "ProvisioningFailed: storageclass \"x\" not found")
	assert.NotContains(t, err.Error(), "old failure")
}

// The newest of two ProvisioningFailed events of the claim is the one
// quoted, whether its time is in LastTimestamp or only in EventTime.
func TestVerifyPodClaims_ProvisioningFailedQuotesNewest(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name   string
		events []*corev1.Event
	}{
		{"last timestamp", []*corev1.Event{
			{Message: "newer failure", LastTimestamp: metav1.NewTime(now)},
			{Message: "older failure", LastTimestamp: metav1.NewTime(now.Add(-time.Minute))},
		}},
		{"event time", []*corev1.Event{
			{Message: "older failure", LastTimestamp: metav1.NewTime(now.Add(-time.Minute))},
			{Message: "newer failure", EventTime: metav1.NewMicroTime(now)},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shortenClaimCheckWait(t, 5*time.Second, 10*time.Millisecond)
			rt, clientset, _ := newTestK8sRuntime()
			createClaim(t, clientset, "c1", "uid-1", corev1.ClaimPending)
			for i, ev := range tc.events {
				ev.ObjectMeta = metav1.ObjectMeta{Name: "e" + string(rune('a'+i)), Namespace: "default"}
				ev.InvolvedObject = corev1.ObjectReference{Kind: "PersistentVolumeClaim", Name: "c1", Namespace: "default", UID: "uid-1"}
				ev.Reason = claimEventProvisioningFailed
				_, err := clientset.CoreV1().Events("default").Create(context.Background(), ev, metav1.CreateOptions{})
				require.NoError(t, err)
			}

			err := rt.verifyPodClaims(context.Background(), "default", claimCheckPod("c1"), nil)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "ProvisioningFailed: newer failure")
			assert.NotContains(t, err.Error(), "older failure")
		})
	}
}

// A ProvisioningFailed event followed by a newer event of the same claim
// means the provisioner is retrying: the claim counts as Pending, so the
// check waits, warns and lets the start continue.
func TestVerifyPodClaims_ProvisioningFailedThenNewerEventWaits(t *testing.T) {
	var buf bytes.Buffer
	setRuntimeLogHandler(t, slog.NewTextHandler(&buf, nil))
	shortenClaimCheckWait(t, 50*time.Millisecond, 10*time.Millisecond)
	rt, clientset, _ := newTestK8sRuntime()
	createClaim(t, clientset, "c1", "uid-1", corev1.ClaimPending)
	now := time.Now()
	createClaimEvent(t, clientset, "e1", "c1", "uid-1", claimEventProvisioningFailed, "rpc error: DeadlineExceeded", now.Add(-time.Minute))
	createClaimEvent(t, clientset, "e2", "c1", "uid-1", "Provisioning", "External provisioner is provisioning volume", now)

	start := time.Now()
	require.NoError(t, rt.verifyPodClaims(context.Background(), "default", claimCheckPod("c1"), nil))
	assert.GreaterOrEqual(t, time.Since(start), 50*time.Millisecond, "waits for the claim check wait")
	out := buf.String()
	assert.Contains(t, out, "level=WARN")
	assert.Contains(t, out, "creating the pod anyway")
	assert.Contains(t, out, "last_event_reason=Provisioning")
}

// A ProvisioningFailed event at the same time as another event of the
// claim is not fatal, whatever the list order (the fake client lists
// events by name): the claim counts as Pending, so the check waits, warns
// and lets the start continue.
func TestVerifyPodClaims_ProvisioningFailedTieWaits(t *testing.T) {
	cases := []struct {
		name       string
		failedName string
		otherName  string
		otherWhy   string
		wantReason string
	}{
		{"failed listed first", "a-failed", "b-other", "Provisioning", "last_event_reason=Provisioning"},
		{"failed listed last", "b-failed", "a-other", "Provisioning", "last_event_reason=Provisioning"},
		{"two failures", "a-failed", "b-other", claimEventProvisioningFailed, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			setRuntimeLogHandler(t, slog.NewTextHandler(&buf, nil))
			shortenClaimCheckWait(t, 50*time.Millisecond, 10*time.Millisecond)
			rt, clientset, _ := newTestK8sRuntime()
			createClaim(t, clientset, "c1", "uid-1", corev1.ClaimPending)
			at := time.Now().Truncate(time.Second)
			createClaimEvent(t, clientset, tc.failedName, "c1", "uid-1", claimEventProvisioningFailed, "rpc error: DeadlineExceeded", at)
			createClaimEvent(t, clientset, tc.otherName, "c1", "uid-1", tc.otherWhy, "External provisioner is provisioning volume", at)

			start := time.Now()
			require.NoError(t, rt.verifyPodClaims(context.Background(), "default", claimCheckPod("c1"), nil))
			assert.GreaterOrEqual(t, time.Since(start), 50*time.Millisecond, "waits for the claim check wait")
			out := buf.String()
			assert.Contains(t, out, "level=WARN")
			assert.Contains(t, out, "creating the pod anyway")
			if tc.wantReason != "" {
				assert.Contains(t, out, tc.wantReason)
			} else {
				assert.NotContains(t, out, "last_event_reason")
			}
		})
	}
}

func TestVerifyPodClaims_StaleProvisioningFailedIgnored(t *testing.T) {
	shortenClaimCheckWait(t, 30*time.Millisecond, 10*time.Millisecond)
	rt, clientset, _ := newTestK8sRuntime()
	createClaim(t, clientset, "c1", "uid-1", corev1.ClaimPending)
	createClaimEvent(t, clientset, "old", "c1", "uid-old", claimEventProvisioningFailed, "old failure", time.Now())

	require.NoError(t, rt.verifyPodClaims(context.Background(), "default", claimCheckPod("c1"), nil))
}

func TestVerifyPodClaims_PendingWithoutEventWarnsAndContinues(t *testing.T) {
	var buf bytes.Buffer
	setRuntimeLogHandler(t, slog.NewTextHandler(&buf, nil))
	shortenClaimCheckWait(t, 50*time.Millisecond, 10*time.Millisecond)
	rt, clientset, _ := newTestK8sRuntime()
	createClaim(t, clientset, "c1", "uid-1", corev1.ClaimPending)
	createClaimEvent(t, clientset, "e1", "c1", "uid-1", "ExternalProvisioning", "waiting for a volume", time.Now())
	clientset.ClearActions()

	start := time.Now()
	require.NoError(t, rt.verifyPodClaims(context.Background(), "default", claimCheckPod("c1"), nil))
	assert.GreaterOrEqual(t, time.Since(start), 50*time.Millisecond, "waits for the claim check wait")
	assert.Greater(t, len(actionVerbs(clientset, "persistentvolumeclaims")), 1, "re-reads the claim while waiting")
	out := buf.String()
	assert.Contains(t, out, "level=WARN")
	assert.Contains(t, out, "creating the pod anyway")
	assert.Contains(t, out, "pvc=c1")
	assert.Contains(t, out, "last_event_reason=ExternalProvisioning")
}

// The check has one wait budget for all of the pod's claims, not one per
// claim: two Pending claims together wait about one budget. The budget is
// wide so a loaded runner cannot push one shared budget past the bound,
// which stays below two budgets.
func TestVerifyPodClaims_OneWaitBudgetForAllClaims(t *testing.T) {
	var buf bytes.Buffer
	setRuntimeLogHandler(t, slog.NewTextHandler(&buf, nil))
	const budget = time.Second
	shortenClaimCheckWait(t, budget, 10*time.Millisecond)
	rt, clientset, _ := newTestK8sRuntime()
	createClaim(t, clientset, "c1", "uid-1", corev1.ClaimPending)
	createClaim(t, clientset, "c2", "uid-2", corev1.ClaimPending)

	start := time.Now()
	require.NoError(t, rt.verifyPodClaims(context.Background(), "default", claimCheckPod("c1", "c2"), nil))
	elapsed := time.Since(start)
	assert.GreaterOrEqual(t, elapsed, budget)
	assert.Less(t, elapsed, 2*budget-200*time.Millisecond, "two claims share one wait budget")
	out := buf.String()
	assert.Contains(t, out, "pvc=c1")
	assert.Contains(t, out, "pvc=c2")
	assert.Equal(t, 2, strings.Count(out, "creating the pod anyway"), "each Pending claim still warns")
}

// A Lost claim is treated like a Pending one: a warning, not a failure.
func TestVerifyPodClaims_LostWarnsAndContinues(t *testing.T) {
	shortenClaimCheckWait(t, 20*time.Millisecond, 10*time.Millisecond)
	rt, clientset, _ := newTestK8sRuntime()
	createClaim(t, clientset, "c1", "uid-1", corev1.ClaimLost)
	require.NoError(t, rt.verifyPodClaims(context.Background(), "default", claimCheckPod("c1"), nil))
}

func TestVerifyPodClaims_PendingThenBoundPasses(t *testing.T) {
	shortenClaimCheckWait(t, 5*time.Second, 10*time.Millisecond)
	rt, clientset, _ := newTestK8sRuntime()
	createClaim(t, clientset, "c1", "uid-1", corev1.ClaimPending)
	gets := 0
	clientset.PrependReactor("get", "persistentvolumeclaims", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		gets++
		if gets < 3 {
			return false, nil, nil
		}
		return true, &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "c1", Namespace: "default", UID: "uid-1"},
			Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
		}, nil
	})
	start := time.Now()
	require.NoError(t, rt.verifyPodClaims(context.Background(), "default", claimCheckPod("c1"), nil))
	assert.Less(t, time.Since(start), time.Second)
	assert.Equal(t, 3, gets)
}

func TestVerifyPodClaims_WaitForFirstConsumerPassesAtOnce(t *testing.T) {
	shortenClaimCheckWait(t, 5*time.Second, 10*time.Millisecond)
	rt, clientset, _ := newTestK8sRuntime()
	createClaim(t, clientset, "c1", "uid-1", corev1.ClaimPending)
	createClaimEvent(t, clientset, "e1", "c1", "uid-1", claimEventWaitForFirstConsumer, "waiting for first consumer to be created before binding", time.Now())

	start := time.Now()
	require.NoError(t, rt.verifyPodClaims(context.Background(), "default", claimCheckPod("c1"), nil))
	assert.Less(t, time.Since(start), time.Second, "no wait for a claim that binds at scheduling")
}

// With events forbidden the check uses the claim phase only, and logs
// that once per runtime instance, not on every start.
func TestVerifyPodClaims_EventsForbiddenFallsBackToPhase(t *testing.T) {
	var buf bytes.Buffer
	setRuntimeLogHandler(t, slog.NewTextHandler(&buf, nil))
	shortenClaimCheckWait(t, 20*time.Millisecond, 10*time.Millisecond)
	rt, clientset, _ := newTestK8sRuntime()
	createClaim(t, clientset, "c1", "uid-1", corev1.ClaimPending)
	createClaim(t, clientset, "c2", "uid-2", corev1.ClaimBound)
	// Visible only through events, which the runtime may not read.
	createClaimEvent(t, clientset, "e1", "c1", "uid-1", claimEventProvisioningFailed, "hidden", time.Now())
	clientset.PrependReactor("list", "events", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, k8serrors.NewForbidden(schema.GroupResource{Resource: "events"}, "", nil)
	})

	for range 2 {
		require.NoError(t, rt.verifyPodClaims(context.Background(), "default", claimCheckPod("c1", "c2"), nil))
	}
	out := buf.String()
	assert.Equal(t, 1, strings.Count(out, "Cannot read events for claims"), "logged once per runtime instance")
	assert.Equal(t, 2, strings.Count(out, "creating the pod anyway"), "a Pending claim still warns on each start")

	// A Bound claim never reaches the events read.
	buf.Reset()
	rt2, clientset2, _ := newTestK8sRuntime()
	createClaim(t, clientset2, "c2", "uid-2", corev1.ClaimBound)
	require.NoError(t, rt2.verifyPodClaims(context.Background(), "default", claimCheckPod("c2"), nil))
	assert.Empty(t, actionVerbs(clientset2, "events"))
}

// A missing claim still fails with events forbidden.
func TestVerifyPodClaims_EventsForbiddenMissingClaimFails(t *testing.T) {
	rt, clientset, _ := newTestK8sRuntime()
	clientset.PrependReactor("list", "events", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, k8serrors.NewForbidden(schema.GroupResource{Resource: "events"}, "", nil)
	})
	err := rt.verifyPodClaims(context.Background(), "default", claimCheckPod("gone"), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "claim default/gone does not exist")
}

func TestVerifyPodClaims_ClaimReadForbiddenSkipsWithOneWarning(t *testing.T) {
	var buf bytes.Buffer
	setRuntimeLogHandler(t, slog.NewTextHandler(&buf, nil))
	rt, clientset, _ := newTestK8sRuntime()
	clientset.PrependReactor("get", "persistentvolumeclaims", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, k8serrors.NewForbidden(schema.GroupResource{Resource: "persistentvolumeclaims"}, "c1", nil)
	})
	for range 2 {
		require.NoError(t, rt.verifyPodClaims(context.Background(), "default", claimCheckPod("c1", "c2"), nil))
	}
	assert.Equal(t, 1, strings.Count(buf.String(), "Cannot read PersistentVolumeClaims"))
	assert.Equal(t, 2, len(actionVerbs(clientset, "persistentvolumeclaims")), "the rest of the check is skipped after a forbidden read")
}

func TestVerifyPodClaims_ClaimReadErrorReturned(t *testing.T) {
	rt, clientset, _ := newTestK8sRuntime()
	clientset.PrependReactor("get", "persistentvolumeclaims", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, k8serrors.NewServiceUnavailable("try later")
	})
	err := rt.verifyPodClaims(context.Background(), "default", claimCheckPod("c1"), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "checking claim default/c1")
}

func TestVerifyPodClaims_ContextDoneWhileWaiting(t *testing.T) {
	shortenClaimCheckWait(t, 5*time.Second, 10*time.Millisecond)
	rt, clientset, _ := newTestK8sRuntime()
	createClaim(t, clientset, "c1", "uid-1", corev1.ClaimPending)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := rt.verifyPodClaims(ctx, "default", claimCheckPod("c1"), nil)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

// Every claim the pod mounts is checked: per-dir, the share claim, and a
// claim the check knows no setting for (for example one a template adds).
func TestVerifyPodClaims_CoversSharedDirShareAndAddedClaims(t *testing.T) {
	cfg := RunConfig{
		Labels:               map[string]string{"scion.project": "myproject"},
		SharedDirs:           []api.SharedDir{{Name: "data"}},
		WorkspaceBackendName: "nfs",
		NFSPVClaimName:       "nfs-share",
	}
	sources := podClaimSources(cfg)
	cases := []struct {
		missing string
		want    string
	}{
		{"scion-shared-myproject-data", `it is the per-dir claim of shared dir "data"`},
		{"nfs-share", "it is set by server.project_storage nfs.shares[0].pv_name (workspace)"},
		{"extra-claim", `it is mounted by pod volume "vol-c"`},
	}
	all := []string{"scion-shared-myproject-data", "nfs-share", "extra-claim"}
	for _, tc := range cases {
		t.Run(tc.missing, func(t *testing.T) {
			rt, clientset, _ := newTestK8sRuntime()
			for _, c := range all {
				if c != tc.missing {
					createClaim(t, clientset, c, types.UID("uid-"+c), corev1.ClaimBound)
				}
			}
			err := rt.verifyPodClaims(context.Background(), "default", claimCheckPod(all...), sources)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "claim default/"+tc.missing+" does not exist")
			assert.Contains(t, err.Error(), tc.want)
		})
	}
	t.Run("all present", func(t *testing.T) {
		rt, clientset, _ := newTestK8sRuntime()
		for _, c := range all {
			createClaim(t, clientset, c, types.UID("uid-"+c), corev1.ClaimBound)
		}
		require.NoError(t, rt.verifyPodClaims(context.Background(), "default", claimCheckPod(all...), sources))
	})
}

func TestPodClaimSources(t *testing.T) {
	labels := map[string]string{"scion.project": "myproject"}
	tests := []struct {
		name string
		cfg  RunConfig
		want map[string]string
	}{
		{"none", RunConfig{}, map[string]string{}},
		{"local per-dir", RunConfig{Labels: labels, SharedDirs: []api.SharedDir{{Name: "a"}}},
			map[string]string{"scion-shared-myproject-a": `the per-dir claim of shared dir "a"`}},
		{"nfs share for shared dirs and workspace", RunConfig{
			Labels: labels, SharedDirs: []api.SharedDir{{Name: "a"}},
			SharedDirStorage:     sharedDirsOnClaim("nfs", "share", "projects/p", "a"),
			WorkspaceBackendName: "nfs", NFSPVClaimName: "share",
		}, map[string]string{"share": "set by server.project_storage nfs.shares[0].pv_name"}},
		{"gke shared volume", RunConfig{
			SharedDirStorage: sharedDirsOnClaim("gke-shared-volume", "gke-claim", "projects/p", "a"),
		}, map[string]string{"gke-claim": "set by server.project_storage gke_shared_volume.pv_claim_name"}},
		{"agent home", RunConfig{HomeStorage: &HomeStorageRealization{PVClaimName: "home"}},
			map[string]string{"home": "set by server.project_storage nfs.shares[0].pv_name (agent home)"}},
		{"workspace claim ignored off nfs", RunConfig{NFSPVClaimName: "ws"}, map[string]string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, podClaimSources(tt.cfg))
		})
	}
}

// Run checks the pod's claims before the pod-create checkpoint: a missing
// share claim fails the start, the checkpoint is never reached, and no
// pod is created.
func TestRun_ClaimCheckBeforePodCreateCheckpoint(t *testing.T) {
	rt, clientset := newTransportTestRuntime(false)
	cfg := nfsAgentDirConfig("cc-run")
	cfg.Labels = map[string]string{"scion.agent": "true", "scion.name": "cc-run"}
	var mu sync.Mutex
	var steps []string
	cfg.Checkpoint = func(ctx context.Context, step string) error {
		mu.Lock()
		defer mu.Unlock()
		steps = append(steps, step)
		return nil
	}

	_, err := rt.Run(context.Background(), cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "claim default/scion-workspaces does not exist")
	assert.Contains(t, err.Error(), "nfs.shares[0].pv_name")
	mu.Lock()
	assert.False(t, slices.Contains(steps, CheckpointStepPodCreate), "the claim check runs before the pod-create checkpoint: %v", steps)
	mu.Unlock()
	assert.NotContains(t, actionVerbs(clientset, "pods"), "create")

	// With the claim present the same start reaches the pod create.
	rt2, clientset2 := newTransportTestRuntime(false)
	steps = nil
	pod, _ := runUntilPodCreated(t, rt2, clientset2, cfg)
	require.NotNil(t, pod)
	mu.Lock()
	assert.True(t, slices.Contains(steps, CheckpointStepPodCreate))
	mu.Unlock()
}

// A start with a shared dir, no storage class and no existing claim is
// refused by Run before any pod create, with no claim written.
func TestRun_SharedDirNoClassNoClaimRefused(t *testing.T) {
	rt, clientset := newTransportTestRuntime(false)
	cfg := baseTransportConfig()
	cfg.Labels["scion.project"] = "myproject"
	cfg.Labels["scion.project_id"] = "proj-1"
	cfg.SharedDirs = []api.SharedDir{{Name: "cache"}}

	_, err := rt.Run(context.Background(), cfg)
	require.Error(t, err)
	assert.ErrorIs(t, err, errSharedDirClaimUnresolved)
	assert.NotContains(t, actionVerbs(clientset, "pods"), "create")
	assert.Empty(t, pvcWrites(clientset))
}

// pvcWrites lists the writes made to claims or volumes.
func pvcWrites(clientset *k8sfake.Clientset) []string {
	var out []string
	for _, a := range clientset.Actions() {
		r := a.GetResource().Resource
		if r != "persistentvolumeclaims" && r != "persistentvolumes" {
			continue
		}
		switch a.GetVerb() {
		case "get", "list", "watch":
		default:
			out = append(out, a.GetVerb()+" "+r)
		}
	}
	return out
}

// Upgrade: claims made before this change (a hand-made unlabelled per-dir
// claim bound to a static volume, and an existing share claim) keep
// working with today's settings (no storage class) and no object writes.
func TestRun_UpgradeExistingClaimsZeroWrites(t *testing.T) {
	empty := ""
	t.Run("hand-made per-dir claim", func(t *testing.T) {
		rt, clientset := newTransportTestRuntime(false)
		_, err := clientset.CoreV1().PersistentVolumeClaims("default").Create(context.Background(), &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "scion-shared-myproject-cache", Namespace: "default"},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
				StorageClassName: &empty,
				VolumeName:       "scion-shared-myproject-cache",
			},
			Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
		}, metav1.CreateOptions{})
		require.NoError(t, err)
		clientset.ClearActions()

		cfg := baseTransportConfig()
		cfg.Labels["scion.project"] = "myproject"
		cfg.Labels["scion.project_id"] = "proj-1"
		cfg.SharedDirs = []api.SharedDir{{Name: "cache"}}
		pod, _ := runUntilPodCreated(t, rt, clientset, cfg)
		require.NotNil(t, pod)
		assert.Empty(t, pvcWrites(clientset))
	})
	t.Run("existing share claim", func(t *testing.T) {
		rt, clientset := newTransportTestRuntime(false)
		createClaim(t, clientset, "hub-nfs", "uid-share", corev1.ClaimBound)
		clientset.ClearActions()

		cfg := baseTransportConfig()
		cfg.Labels["scion.project"] = "myproject"
		cfg.Labels["scion.project_id"] = "proj-1"
		cfg.SharedDirs = []api.SharedDir{{Name: "cache"}}
		cfg.SharedDirStorage = sharedDirsOnClaim("nfs", "hub-nfs", "projects/proj-1", "cache")
		pod, _ := runUntilPodCreated(t, rt, clientset, cfg)
		require.NotNil(t, pod)
		assert.Empty(t, pvcWrites(clientset))
	})
}
