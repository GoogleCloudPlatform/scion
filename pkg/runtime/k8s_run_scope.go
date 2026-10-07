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
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	k8svalidation "k8s.io/apimachinery/pkg/util/validation"
)

// Run-scoped Kubernetes deletes (ptone/scion#2550 P2).
//
// The pod and its per-agent Secrets and SecretProviderClass have
// deterministic names (the pod name is the agent name), so a name alone
// cannot tell one run of an agent from the next. Every object Run creates
// carries the run's api.LabelRunID label. When a caller names a run, the
// code here deletes an object only when:
//   - its run label matches that run, or it has no run label (a legacy
//     object created before run IDs existed). An object labelled with
//     another run is never deleted; and
//   - the delete carries a UID precondition taken from the object as read.
//     This is the real guard: an object recreated under the same name
//     between the read and the delete has a new UID and survives. The label
//     is a filter on top of it.
//
// With no run, every path keeps its name-based behaviour.

// k8sRunMatches reports whether an object whose run label is objRun belongs
// to run runID under the broker's rule (filterDeleteCandidatesByRun): an
// exact match wins, an unlabelled legacy object matches, and an object of
// another run never does.
func k8sRunMatches(objRun, runID string) bool {
	return objRun == "" || objRun == runID
}

// errInvalidRunID is returned for a run ID that is not a valid label value.
var errInvalidRunID = errors.New("invalid run ID")

// ValidateRunID reports whether runID can be used as the scion.run_id label
// value (and so in a label selector, where a "," or "!" would add or negate
// terms). The error text is fixed and carries no part of runID.
func ValidateRunID(runID string) error {
	if len(k8svalidation.IsValidLabelValue(runID)) > 0 {
		return errInvalidRunID
	}
	return nil
}

// validateRunIDLabel is ValidateRunID with the offending value logged.
func validateRunIDLabel(runID string) error {
	if err := ValidateRunID(runID); err != nil {
		runtimeLog.Warn("Refusing a run ID that is not a valid label value",
			"run_id", runID, "errors", strings.Join(k8svalidation.IsValidLabelValue(runID), "; "))
		return err
	}
	return nil
}

// opaqueRunScopeError is a start error whose text is fixed (no namespace,
// object name or run ID: start errors can reach the hub verbatim, see
// classifyStartError) while errors.Is/As still reach the cause. The
// details are logged where it is built.
type opaqueRunScopeError struct {
	msg   string
	cause error
}

func (e *opaqueRunScopeError) Error() string { return e.msg }
func (e *opaqueRunScopeError) Unwrap() error { return e.cause }

// runConflictError logs reason with the identifying details at Warn and
// returns ErrRunConflict with only its fixed text, so errors.Is still
// matches and no namespace, object name or run ID reaches a client.
func runConflictError(reason string, logArgs ...any) error {
	runtimeLog.Warn(reason, logArgs...)
	return &opaqueRunScopeError{msg: ErrRunConflict.Error(), cause: ErrRunConflict}
}

// opaqueStartError logs msg with the identifying details and the cause,
// and returns an error with only msg as its text.
func opaqueStartError(msg string, cause error, logArgs ...any) error {
	runtimeLog.Warn(msg, append(logArgs, "error", cause)...)
	return &opaqueRunScopeError{msg: msg, cause: cause}
}

// k8sPodIsLive reports whether pod is Pending or Running and is not already
// being deleted. A live pod of another run must not be removed to make room
// for a new run.
//
// Phase Unknown (the node stopped reporting) counts as not live, as the
// name-based pre-clean has always treated it: such a pod is force-deleted.
// On a partitioned node its container may keep running beside the new
// pod's until the node returns. Counting Unknown as live would instead
// block every start of the agent until node-lifecycle garbage collection
// removes the pod, which can take many minutes.
func k8sPodIsLive(pod *corev1.Pod) bool {
	if pod.DeletionTimestamp != nil {
		return false
	}
	return pod.Status.Phase == corev1.PodPending || pod.Status.Phase == corev1.PodRunning
}

// k8sUIDPrecondition returns a delete precondition on uid.
func k8sUIDPrecondition(uid types.UID) *metav1.Preconditions {
	return &metav1.Preconditions{UID: &uid}
}

// perRunFilter decides whether a per-run object (k8s_run_names.go) that a
// selector returned may be deleted. kind is "Secret" or
// "SecretProviderClass".
type perRunFilter func(kind string, obj metav1.Object) bool

// deleteAgentSecretsBySelector deletes the per-agent Secrets and, in GKE
// mode, the SecretProviderClass of pod agentName that a list with selector
// returns, each with a UID precondition from the list:
//   - the fixed names (scion-agent-<name>, scion-auth-<name>, and the SPC
//     scion-agent-<name>), whatever their labels; and
//   - the per-run names of the run each object is labelled with
//     (k8sAgentObjectNames), when perRun is nil or accepts the object. A
//     per-run object with no run label, or whose name is not its own
//     run's name for this pod, is ignored.
//
// Objects with other names are ignored. A delete that fails with NotFound
// (already gone) or Conflict (recreated since the list) leaves the object
// alone. Other failures are passed to warn. Returns how many objects were
// deleted.
//
// Listing rather than reading by name keeps this within the
// create/list/delete permissions the runtime already needs.
func (r *KubernetesRuntime) deleteAgentSecretsBySelector(ctx context.Context, namespace, agentName, selector string, perRun perRunFilter, warn func(kind, name string, err error)) int {
	opts := metav1.ListOptions{LabelSelector: selector}
	fixed := k8sAgentObjectNames(agentName, "")
	selected := func(kind string, obj metav1.Object) bool {
		name := obj.GetName()
		if kind == "Secret" && (name == fixed.Secret || name == fixed.Auth) {
			return true
		}
		if kind == "SecretProviderClass" && name == fixed.SPC {
			return true
		}
		objRun := obj.GetLabels()[api.LabelRunID]
		if objRun == "" || !isPerRunObjectName(name) {
			return false
		}
		own := k8sAgentObjectNames(agentName, objRun)
		if kind == "Secret" && name != own.Secret && name != own.Auth {
			return false
		}
		if kind == "SecretProviderClass" && name != own.SPC {
			return false
		}
		return perRun == nil || perRun(kind, obj)
	}
	removed := 0
	deleted := func(kind, name string, err error) {
		switch {
		case err == nil:
			removed++
		case k8serrors.IsNotFound(err), k8serrors.IsConflict(err):
		default:
			warn(kind, name, err)
		}
	}

	secrets := r.Client.Clientset.CoreV1().Secrets(namespace)
	if list, err := secrets.List(ctx, opts); err != nil {
		warn("Secret", "", err)
	} else {
		for i := range list.Items {
			s := &list.Items[i]
			if !selected("Secret", s) {
				continue
			}
			deleted("Secret", s.Name, secrets.Delete(ctx, s.Name, metav1.DeleteOptions{
				Preconditions: k8sUIDPrecondition(s.UID),
			}))
		}
	}

	if r.GKEMode {
		spcs := r.Client.Dynamic().Resource(k8s.SecretProviderClassGVR).Namespace(namespace)
		if list, err := spcs.List(ctx, opts); err != nil {
			warn("SecretProviderClass", "", err)
		} else {
			for i := range list.Items {
				spc := &list.Items[i]
				if !selected("SecretProviderClass", spc) {
					continue
				}
				deleted("SecretProviderClass", spc.GetName(), spcs.Delete(ctx, spc.GetName(), metav1.DeleteOptions{
					Preconditions: k8sUIDPrecondition(spc.GetUID()),
				}))
			}
		}
	}
	return removed
}

// staleOtherRunFilter returns the perRunFilter of a sweep on behalf of run
// currentRun: it accepts a per-run object of another run only when it is
// stale (staleRunObject), and logs each one it accepts. Never one of
// currentRun.
func (r *KubernetesRuntime) staleOtherRunFilter(currentRun string) perRunFilter {
	return func(kind string, obj metav1.Object) bool {
		objRun := obj.GetLabels()[api.LabelRunID]
		if objRun == "" || objRun == currentRun || !r.staleRunObject(obj) {
			return false
		}
		runtimeLog.Info("Removing a stale per-run object of another run",
			"kind", kind, "name", obj.GetName(), "namespace", obj.GetNamespace(),
			"object_run_id", objRun, "run_id", currentRun,
			"created", obj.GetCreationTimestamp().UTC().Format(time.RFC3339),
			"deadline_offset", obj.GetAnnotations()[annotationStartDeadlineOffset])
		return true
	}
}

// deleteRun is Delete for a ref that names a run. See the comment at the
// top of this file for the rules.
//
//   - The pod is gone: only the Secrets and SecretProviderClass labelled
//     with runID are deleted; nil is returned.
//   - The pod belongs to another run: nothing is deleted, and an error
//     wrapping ErrRunMismatch is returned.
//   - The pod belongs to runID, or is a legacy pod with no run label: its
//     Secrets and SecretProviderClass are deleted (for a legacy pod, those
//     of its start, see legacyAgentObjectSelector), then the pod, with a UID
//     precondition from the Get. If the pod was replaced between the Get and
//     the delete, the newer pod survives and ErrRunMismatch is returned, so
//     the caller leaves the newer run's files alone too.
//
// The pod is deleted with podDeleteOptions(pod), as on the name-based path:
// immediately (grace period 0), except an NFS-home pod, which is deleted
// with its own grace period. The UID precondition applies either way.
func (r *KubernetesRuntime) deleteRun(ctx context.Context, namespace, podName, runID string) error {
	if err := validateRunIDLabel(runID); err != nil {
		return err
	}
	warn := func(kind, name string, err error) {
		runtimeLog.Warn("Failed to delete per-agent object of a run",
			"kind", kind, "name", name, "agent", podName, "namespace", namespace, "run_id", runID, "error", err)
	}
	pods := r.Client.Clientset.CoreV1().Pods(namespace)
	pod, err := pods.Get(ctx, podName, metav1.GetOptions{})
	if k8serrors.IsNotFound(err) {
		r.deleteAgentSecretsBySelector(ctx, namespace, podName, api.LabelRunID+"="+runID, nil, warn)
		return nil
	}
	if err != nil {
		runtimeLog.Warn("Run-scoped delete could not read the pod",
			"pod", podName, "namespace", namespace, "run_id", runID, "error", err)
		return &opaqueRunScopeError{msg: "failed to read the agent pod", cause: err}
	}

	podRun := pod.Labels[api.LabelRunID]
	if !k8sRunMatches(podRun, runID) {
		runtimeLog.Info("Delete left a pod of another run untouched",
			"pod", podName, "namespace", namespace, "run_id", runID, "pod_run_id", podRun)
		return fmt.Errorf("pod %s/%s belongs to run %q, not %q: %w", namespace, podName, podRun, runID, ErrRunMismatch)
	}

	selector := api.LabelRunID + "=" + runID
	if podRun == "" {
		selector = legacyAgentObjectSelector(pod)
	}
	r.deleteAgentSecretsBySelector(ctx, namespace, podName, selector, nil, warn)

	// Immediate deletion, except an NFS-home pod, which is deleted with its
	// grace period (podDeleteOptions), as on the name-based path.
	opts := podDeleteOptions(pod)
	opts.Preconditions = k8sUIDPrecondition(pod.UID)
	err = pods.Delete(ctx, podName, opts)
	switch {
	case err == nil, k8serrors.IsNotFound(err):
		return nil
	case k8serrors.IsConflict(err):
		runtimeLog.Info("Delete left a pod recreated under the same name untouched",
			"pod", podName, "namespace", namespace, "run_id", runID, "uid", pod.UID)
		return fmt.Errorf("pod %s/%s was replaced before it could be deleted: %w", namespace, podName, ErrRunMismatch)
	default:
		return fmt.Errorf("failed to delete pod: %w", err)
	}
}

// legacyAgentObjectSelector selects the per-agent Secrets and
// SecretProviderClass of a legacy pod (one with no run label): those of the
// pod's own start when the pod carries a start ID, otherwise any that
// carry no run label. Objects of a labelled run are never selected.
func legacyAgentObjectSelector(pod *corev1.Pod) string {
	if startID := pod.Labels[labelStartID]; startID != "" {
		return labelStartID + "=" + startID + ",!" + api.LabelRunID
	}
	return "!" + api.LabelRunID
}

// preCleanForRun is Run's pre-clean when the new run carries a run ID. It
// removes what a previous run of the same agent name left behind, except
// anything belonging to another run that is still live:
//
//   - a pod of another run that is Pending or Running (and not being
//     deleted) makes Run fail with ErrRunConflict before anything is
//     deleted;
//   - otherwise the per-agent Secrets and SecretProviderClass not labelled
//     with the new run are deleted with UID preconditions, and the pod
//     (finished, legacy, or a retry of this same run), also with a UID
//     precondition (removePreviousPodForRun). A pod replaced between the
//     Get and the delete belongs to a concurrent start; Run then fails with
//     ErrRunConflict.
//
// NFS-home agents (GoogleCloudPlatform/scion#2534): Run holds the home start
// lock around this call. A previous NFS-home pod is deleted gracefully and
// waited for (removePreviousPodForRun), including one already terminating.
// For an NFS-home start (nfsHomeStart) the Secrets/SPC are deleted only
// after that, and a pod read failure or an unconfirmed stop carries
// errPreviousPodUnconfirmed. These are the rules of the name-based
// cleanupStalePod path.
//
// A failure to read the pod, or to delete it (other than NotFound), fails
// the start (retryable) rather than deleting without knowing whose pod
// holds the name or creating against a pod still there.
//
// A live legacy pod (no run label, created before run IDs) is deleted on
// purpose: during the upgrade window such a pod has no run identity to
// fence on, and the name-based pre-clean it replaces deleted it too.
//
// The Secret/SPC selection must stay a superset of every same-name object
// of another run with no live pod: replaceExistingAgentObject refuses any
// such object it meets later, so one skipped here would fail every retry
// of the start. It is therefore selected by the run label and the object
// name only, never by other labels (a project recreated under the same
// name gets a new project ID but the same object names).
func (r *KubernetesRuntime) preCleanForRun(ctx context.Context, namespace, podName, runID string, nfsHomeStart bool, hs *HomeStorageRealization) error {
	if err := validateRunIDLabel(runID); err != nil {
		return err
	}
	pods := r.Client.Clientset.CoreV1().Pods(namespace)
	pod, err := pods.Get(ctx, podName, metav1.GetOptions{})
	switch {
	case k8serrors.IsNotFound(err):
		pod = nil
	case err != nil:
		if nfsHomeStart {
			// An NFS-home start never proceeds past a pod it could not
			// read: it may be a previous pod still writing to the home.
			return opaqueStartError(errPreviousPodUnconfirmed.Error()+": failed to read the previous agent pod; retry later",
				fmt.Errorf("%w: %w", errPreviousPodUnconfirmed, err),
				"pod", podName, "namespace", namespace, "run_id", runID)
		}
		return opaqueStartError("failed to read the existing agent pod before start", err,
			"pod", podName, "namespace", namespace, "run_id", runID)
	}
	if pod != nil {
		if podRun := pod.Labels[api.LabelRunID]; podRun != "" && podRun != runID && k8sPodIsLive(pod) {
			return runConflictError("Start refused: a live pod of another run holds the agent name",
				"pod", podName, "namespace", namespace, "run_id", runID, "pod_run_id", podRun, "phase", pod.Status.Phase)
		}
	}

	warn := func(kind, name string, err error) {
		runtimeLog.Warn("Failed to delete stale per-agent object before start",
			"kind", kind, "name", name, "agent", podName, "namespace", namespace, "run_id", runID, "error", err)
	}
	// Every per-agent object Run creates carries scion.agent (see
	// createAgentSecret); "!=" also selects objects with no run label.
	//
	// Fixed-name objects of another run (or of none) are deleted, as
	// before: they share the names this start would otherwise use. A
	// per-run object of another run is deleted only when it belongs to the
	// previous pod's run (that pod is not live, or Run would have stopped
	// above, and is removed below), or when it is stale (the sweep,
	// staleOtherRunFilter). A per-run object of another run with no pod may
	// belong to a start of that run still before its pod, and is left
	// (ptone/scion#3101).
	prevPodRun := ""
	if pod != nil {
		prevPodRun = pod.Labels[api.LabelRunID]
	}
	stale := r.staleOtherRunFilter(runID)
	previousOrStale := func(kind string, obj metav1.Object) bool {
		if objRun := obj.GetLabels()[api.LabelRunID]; prevPodRun != "" && objRun == prevPodRun {
			return true
		}
		return stale(kind, obj)
	}
	deleteSecrets := func() {
		r.deleteAgentSecretsBySelector(ctx, namespace, podName, "scion.agent,"+api.LabelRunID+"!="+runID, previousOrStale, warn)
	}

	// An NFS-home start deletes the Secrets/SPC only after the previous pod
	// is confirmed stopped, so a pod still shutting down keeps the secrets
	// it mounted (as cleanupStalePod's caller does); other starts delete
	// them first, as before.
	if !nfsHomeStart {
		deleteSecrets()
	}
	if err := r.removePreviousPodForRun(ctx, namespace, podName, runID, pod, hs); err != nil {
		return err
	}
	if nfsHomeStart {
		deleteSecrets()
	}
	return nil
}

// removePreviousPodForRun deletes pod (the previous pod holding the agent
// name, nil if none), already checked by preCleanForRun, with a UID
// precondition. A pod that is not an NFS-home pod is deleted immediately,
// as before. An NFS-home pod (isNFSHomePod) is never force-deleted: it is
// deleted with its own grace period (podDeleteOptions) and the start waits
// until its containers are confirmed stopped (waitForPodTermination), so the
// new pod never writes to the home while the old one still does. That also
// covers a pod that is already terminating: the graceful delete does not
// shorten its grace period, and the wait still applies. These are the
// rules cleanupStalePod applies on the name-based path (GoogleCloudPlatform/scion#2534).
func (r *KubernetesRuntime) removePreviousPodForRun(ctx context.Context, namespace, podName, runID string, pod *corev1.Pod, hs *HomeStorageRealization) error {
	if pod == nil {
		return nil
	}
	opts := podDeleteOptions(pod)
	opts.Preconditions = k8sUIDPrecondition(pod.UID)
	err := r.Client.Clientset.CoreV1().Pods(namespace).Delete(ctx, podName, opts)
	switch {
	case err == nil, k8serrors.IsNotFound(err):
	case k8serrors.IsConflict(err):
		return runConflictError("Start refused: the agent pod was recreated by another start",
			"pod", podName, "namespace", namespace, "run_id", runID)
	default:
		// Fail the start (retryable) rather than go on to a pod create
		// that would only report the old pod as still there; a Forbidden
		// or API error is surfaced as itself.
		if isNFSHomePod(pod) {
			return opaqueStartError(errPreviousPodUnconfirmed.Error()+": failed to delete the previous agent pod; retry later",
				fmt.Errorf("%w: %w", errPreviousPodUnconfirmed, err),
				"pod", podName, "namespace", namespace, "run_id", runID)
		}
		return opaqueStartError("failed to delete the stale agent pod before start", err,
			"pod", podName, "namespace", namespace, "run_id", runID)
	}
	if !isNFSHomePod(pod) {
		return nil
	}
	bound := nfsHomeTerminationBound(pod, hs)
	runtimeLog.Info("Waiting for the previous pod to stop", "pod", podName, "namespace", namespace,
		"run_id", runID, "bound", bound.String(), "phase", "home-wait")
	if err := r.waitForPodTermination(ctx, namespace, podName, pod.UID, bound); err != nil {
		// The visible text keeps the documented retryable code
		// (previous_pod_unconfirmed) without the pod name.
		msg := "the previous agent pod has not been confirmed stopped"
		if errors.Is(err, errPreviousPodUnconfirmed) {
			msg = errPreviousPodUnconfirmed.Error() + ": " + msg + "; retry later"
		}
		return opaqueStartError(msg, err, "pod", podName, "namespace", namespace, "run_id", runID)
	}
	return nil
}

// nfsHomeTerminationBound is how long a start waits for the previous
// NFS-home pod to stop: its own grace period (or the default) plus the
// configured termination wait (or the default).
func nfsHomeTerminationBound(pod *corev1.Pod, hs *HomeStorageRealization) time.Duration {
	wait := defaultHomeTerminationWaitSeconds
	if hs != nil && hs.TerminationWaitSeconds > 0 {
		wait = hs.TerminationWaitSeconds
	}
	grace := defaultHomeStopGraceSeconds
	if g := pod.Spec.TerminationGracePeriodSeconds; g != nil && *g > 0 {
		grace = int(*g)
	}
	return time.Duration(grace+wait) * time.Second
}

// replaceExistingAgentObject makes room for a per-agent Secret or
// SecretProviderClass whose create failed with AlreadyExists. kind is
// api.ResourceKindSecret or api.ResourceKindSecretProviderClass.
//
// Without a run (runID empty) the object is deleted by name, as before.
// With a run, the existing object is listed (field selector on its name)
// and:
//   - labelled with runID, or unlabelled (legacy): deleted with a UID
//     precondition;
//   - labelled with another run: left alone, and an error wrapping
//     ErrRunConflict is returned. Pre-clean already removed every stale
//     object of another run, so one that exists now was created by a
//     concurrent start. This holds only while pre-clean's selection covers
//     every same-name object of another run with no live pod (see
//     preCleanForRun); narrowing it by other labels would turn a stale
//     object into a conflict on every retry.
//
// A delete that fails with Conflict means the object was recreated since
// the list, also by a concurrent start: ErrRunConflict.
func (r *KubernetesRuntime) replaceExistingAgentObject(ctx context.Context, kind, namespace, name, runID string) error {
	type existing struct {
		run string
		uid types.UID
	}
	var del func(opts metav1.DeleteOptions) error
	var found []existing
	listOpts := metav1.ListOptions{FieldSelector: fields.OneTermEqualSelector("metadata.name", name).String()}

	switch kind {
	case api.ResourceKindSecret:
		secrets := r.Client.Clientset.CoreV1().Secrets(namespace)
		del = func(opts metav1.DeleteOptions) error { return secrets.Delete(ctx, name, opts) }
		if runID == "" {
			_ = del(metav1.DeleteOptions{})
			return nil
		}
		list, err := secrets.List(ctx, listOpts)
		if err != nil {
			return opaqueStartError("failed to list the existing agent Secret", err, "name", name, "namespace", namespace, "run_id", runID)
		}
		for _, s := range list.Items {
			if s.Name == name {
				found = append(found, existing{run: s.Labels[api.LabelRunID], uid: s.UID})
			}
		}
	case api.ResourceKindSecretProviderClass:
		spcs := r.Client.Dynamic().Resource(k8s.SecretProviderClassGVR).Namespace(namespace)
		del = func(opts metav1.DeleteOptions) error { return spcs.Delete(ctx, name, opts) }
		if runID == "" {
			_ = del(metav1.DeleteOptions{})
			return nil
		}
		list, err := spcs.List(ctx, listOpts)
		if err != nil {
			return opaqueStartError("failed to list the existing agent SecretProviderClass", err, "name", name, "namespace", namespace, "run_id", runID)
		}
		for _, spc := range list.Items {
			if spc.GetName() == name {
				found = append(found, existing{run: spc.GetLabels()[api.LabelRunID], uid: spc.GetUID()})
			}
		}
	default:
		return fmt.Errorf("unsupported resource kind %q", kind)
	}

	for _, obj := range found {
		if !k8sRunMatches(obj.run, runID) {
			return runConflictError("Start refused: an existing agent object belongs to another run",
				"kind", kind, "name", name, "namespace", namespace, "run_id", runID, "object_run_id", obj.run)
		}
		err := del(metav1.DeleteOptions{Preconditions: k8sUIDPrecondition(obj.uid)})
		switch {
		case err == nil, k8serrors.IsNotFound(err):
		case k8serrors.IsConflict(err):
			return runConflictError("Start refused: an existing agent object was recreated by another start",
				"kind", kind, "name", name, "namespace", namespace, "run_id", runID)
		default:
			return opaqueStartError("failed to delete the existing agent "+kind, err, "name", name, "namespace", namespace, "run_id", runID)
		}
	}
	return nil
}
