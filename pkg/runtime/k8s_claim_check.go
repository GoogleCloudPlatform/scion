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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
)

// claimCheckWait is how long the claim check waits, in total for all of a
// pod's claims, for Pending claims to bind before it warns and lets the
// start continue. claimCheckPollInterval is how often it re-reads a claim
// meanwhile. Tests shorten both.
var (
	claimCheckWait         = 30 * time.Second
	claimCheckPollInterval = 2 * time.Second
)

// Event reasons the claim check acts on. ProvisioningFailed means the
// claim's volume cannot be provisioned; WaitForFirstConsumer means the
// claim's class binds it only once a pod using it is scheduled, so waiting
// before the pod exists cannot change it.
const (
	claimEventProvisioningFailed   = "ProvisioningFailed"
	claimEventWaitForFirstConsumer = "WaitForFirstConsumer"
)

// verifyPodClaims checks every PersistentVolumeClaim pod mounts before the
// pod is created, so a start that can never run fails with a reason instead
// of leaving the pod Pending:
//   - a missing claim fails at once, naming the setting that produced it
//     (sources maps a claim name to that description; see podClaimSources);
//   - a Bound claim passes;
//   - a claim whose newest event is ProvisioningFailed fails at once,
//     quoting it (an older ProvisioningFailed followed by a newer event
//     means the provisioner is retrying, so the claim counts as Pending);
//   - a claim with a WaitForFirstConsumer event passes (it binds when the
//     pod is scheduled);
//   - any other claim (Pending, Lost) is re-read every
//     claimCheckPollInterval until it binds or the check's one wait
//     budget (claimCheckWait, shared by all the pod's claims) is used up,
//     then the start continues with a warning.
//
// It only reads. When the runtime may not read events, it uses the claim
// phase alone; when it may not read claims, it skips the check. Each of
// these is logged once per runtime instance.
func (r *KubernetesRuntime) verifyPodClaims(ctx context.Context, namespace string, pod *corev1.Pod, sources map[string]string) error {
	deadline := time.Now().Add(claimCheckWait)
	seen := make(map[string]bool)
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim == nil || v.PersistentVolumeClaim.ClaimName == "" {
			continue
		}
		name := v.PersistentVolumeClaim.ClaimName
		if seen[name] {
			continue
		}
		seen[name] = true
		source, ok := sources[name]
		if !ok {
			source = fmt.Sprintf("mounted by pod volume %q", v.Name)
		}
		skip, err := r.verifyClaim(ctx, namespace, name, source, deadline)
		if err != nil {
			return err
		}
		if skip {
			return nil
		}
	}
	return nil
}

// verifyClaim checks one claim (see verifyPodClaims), waiting for it to
// bind until deadline at most. skip reports that claims cannot be read at
// all, so the rest of the check is skipped.
func (r *KubernetesRuntime) verifyClaim(ctx context.Context, namespace, name, source string, deadline time.Time) (skip bool, err error) {
	claims := r.Client.Clientset.CoreV1().PersistentVolumeClaims(namespace)
	var lastEvent *corev1.Event
	for {
		pvc, err := claims.Get(ctx, name, metav1.GetOptions{})
		switch {
		case k8serrors.IsNotFound(err):
			return false, fmt.Errorf("claim %s/%s does not exist; it is %s. Create the claim in namespace %s, or correct that setting",
				namespace, name, source, namespace)
		case k8serrors.IsForbidden(err):
			if r.claimReadFallbackLogged.CompareAndSwap(false, true) {
				runtimeLog().Warn("Cannot read PersistentVolumeClaims; the claim check before the pod is skipped (grant persistentvolumeclaims get to enable it)",
					"namespace", namespace, "error", err)
			}
			return true, nil
		case err != nil:
			return false, fmt.Errorf("checking claim %s/%s: %w", namespace, name, err)
		}
		if pvc.Status.Phase == corev1.ClaimBound {
			return false, nil
		}

		if ev := r.claimEvent(ctx, namespace, pvc); ev != nil {
			lastEvent = ev
			switch ev.Reason {
			case claimEventProvisioningFailed:
				return false, fmt.Errorf("claim %s/%s cannot be provisioned (it is %s): %s: %s",
					namespace, name, source, ev.Reason, ev.Message)
			case claimEventWaitForFirstConsumer:
				runtimeLog().Debug("Claim binds when the pod is scheduled; not waiting for it",
					"pvc", name, "namespace", namespace)
				return false, nil
			}
		}

		if !time.Now().Before(deadline) {
			args := []any{"pvc", name, "namespace", namespace, "phase", string(pvc.Status.Phase),
				"storage_class", claimStorageClass(pvc), "wait", claimCheckWait.String()}
			if lastEvent != nil {
				args = append(args, "last_event_reason", lastEvent.Reason, "last_event_message", lastEvent.Message)
			}
			runtimeLog().Warn("Claim is not Bound after the claim check wait; creating the pod anyway", args...)
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, fmt.Errorf("waiting for claim %s/%s to bind: %w", namespace, name, ctx.Err())
		case <-time.After(claimCheckPollInterval):
		}
	}
}

// claimEvent returns the event about pvc the claim check acts on: the
// newest event when it is ProvisioningFailed and strictly newer than every
// other event of the claim, else the newest WaitForFirstConsumer event,
// else the newest event if it is not ProvisioningFailed (of events with
// the same newest time, one that is not), else nil. A ProvisioningFailed
// event with a newer event after it, or another event at the same time, is
// not returned for its reason: the provisioner may be retrying, and event
// times often have one-second resolution, so the claim counts as Pending
// whatever the list order. Events of an earlier claim with the same name
// are ignored by UID. When events cannot be read it returns nil, logging
// that once per runtime instance, so the check falls back to the claim
// phase.
func (r *KubernetesRuntime) claimEvent(ctx context.Context, namespace string, pvc *corev1.PersistentVolumeClaim) *corev1.Event {
	selector := fields.Set{
		"involvedObject.kind": "PersistentVolumeClaim",
		"involvedObject.name": pvc.Name,
	}.AsSelector().String()
	list, err := r.Client.Clientset.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{FieldSelector: selector})
	if err != nil {
		if r.claimEventsFallbackLogged.CompareAndSwap(false, true) {
			runtimeLog().Warn("Cannot read events for claims; the claim check uses the claim phase only (grant events list to see provisioning failures early)",
				"namespace", namespace, "error", err)
		}
		return nil
	}
	var newest, firstConsumer *corev1.Event
	// tied is whether another event has the same time as newest. On a tie,
	// newest is the event that is not ProvisioningFailed, if there is one.
	tied := false
	for i := range list.Items {
		ev := &list.Items[i]
		if ev.InvolvedObject.Kind != "PersistentVolumeClaim" || ev.InvolvedObject.Name != pvc.Name {
			continue
		}
		if ev.InvolvedObject.UID != "" && pvc.UID != "" && ev.InvolvedObject.UID != pvc.UID {
			continue
		}
		if ev.Reason == claimEventWaitForFirstConsumer {
			firstConsumer = newerEvent(firstConsumer, ev)
		}
		switch {
		case newest == nil || eventTime(ev).After(eventTime(newest)):
			newest, tied = ev, false
		case eventTime(ev).Equal(eventTime(newest)):
			tied = true
			if newest.Reason == claimEventProvisioningFailed {
				newest = ev
			}
		}
	}
	if newest != nil && newest.Reason == claimEventProvisioningFailed {
		if !tied {
			return newest
		}
		// Only ProvisioningFailed events share the newest time: the
		// claim counts as Pending, with no event to act on.
		newest = nil
	}
	if firstConsumer != nil {
		return firstConsumer
	}
	return newest
}

// newerEvent returns whichever of cur and ev was seen last (cur when ev is
// not newer).
func newerEvent(cur, ev *corev1.Event) *corev1.Event {
	if cur == nil || eventTime(ev).After(eventTime(cur)) {
		return ev
	}
	return cur
}

// eventTime is the most recent time recorded on ev.
func eventTime(ev *corev1.Event) time.Time {
	t := ev.LastTimestamp.Time
	if ev.EventTime.After(t) {
		t = ev.EventTime.Time
	}
	if t.IsZero() {
		t = ev.CreationTimestamp.Time
	}
	return t
}

// claimStorageClass is pvc's storage class name, or "" when unset.
func claimStorageClass(pvc *corev1.PersistentVolumeClaim) string {
	if pvc.Spec.StorageClassName == nil {
		return ""
	}
	return *pvc.Spec.StorageClassName
}

// podClaimSources maps each claim a start's pod may mount to the setting
// it comes from, for the claim check's errors. A claim used in several
// roles keeps the first description below.
func podClaimSources(config RunConfig) map[string]string {
	sources := make(map[string]string)
	add := func(name, source string) {
		if name == "" {
			return
		}
		if _, ok := sources[name]; !ok {
			sources[name] = source
		}
	}
	if sds := config.SharedDirStorage; sds.Shared() {
		switch sds.Backend {
		case "gke-shared-volume":
			add(sds.PVClaimName, "set by server.project_storage gke_shared_volume.pv_claim_name")
		default:
			add(sds.PVClaimName, "set by server.project_storage nfs.shares[0].pv_name")
		}
	}
	if config.WorkspaceBackendName == "nfs" {
		add(config.NFSPVClaimName, "set by server.project_storage nfs.shares[0].pv_name (workspace)")
	}
	if config.HomeStorage != nil {
		add(config.HomeStorage.PVClaimName, "set by server.project_storage nfs.shares[0].pv_name (agent home)")
	}
	if !config.SharedDirStorage.Shared() {
		if projectName := projectkeys.ProjectNameFromLabels(config.Labels); projectName != "" {
			for _, sd := range config.SharedDirs {
				add(sharedDirPVCName(projectName, sd.Name), fmt.Sprintf("the per-dir claim of shared dir %q", sd.Name))
			}
		}
	}
	return sources
}
