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
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
)

// Vanished-pod tombstones.
//
// Scheduler preemption and eviction delete the agent pod. The agent's own
// shutdown on SIGTERM takes seconds, so the pod object is often gone before
// the next List() (one per broker heartbeat) can see its DisruptionTarget
// condition, and the preempted/evicted reason is never reported. To keep
// that reason, the runtime remembers the agent pods it has seen (in List()
// or created in Run()). When a remembered pod is missing from a later List()
// in the same scope, the runtime lists the pod's Events once; if they show
// that the pod was Preempted or Evicted, List() reports a tombstone entry
// for the agent (phase stopped or error, exit reason preempted or evicted)
// for podTombstoneTTL, so the hub can record the reason. Without such event
// evidence nothing is reported: a pod removed by an explicit stop or delete
// never reads as a disruption.
//
// A tombstone never overrides a newer pod: it is dropped as soon as any pod
// for the same agent is listed or created, or a start for the agent begins.
//
// The tracker is in-memory only and bounded: entries not refreshed within
// podTombstoneTTL are pruned. It is lost on broker restart; a pod that
// vanishes while the broker is down is then not reported (the previous
// behaviour).

// podTombstoneTTL bounds how long a tombstone is reported and how long a
// remembered pod that no List() refreshes is kept.
const podTombstoneTTL = 5 * time.Minute

// podEventReasonPreempted and podEventReasonEvicted are the Event reasons the
// scheduler (preemption) and the kubelet (node-pressure eviction) record on
// the pod they remove.
const (
	podEventReasonPreempted = "Preempted"
	podEventReasonEvicted   = "Evicted"
)

// trackedPod is a remembered agent pod.
type trackedPod struct {
	uid         types.UID
	namespace   string
	name        string
	agentKey    string
	labels      map[string]string
	base        api.AgentInfo // identity fields as List() reports them
	recoverable bool          // k8sPodWorkspaceRecoverable
	seen        time.Time
}

// podTombstone is the entry List() reports for a vanished, disrupted pod.
type podTombstone struct {
	namespace string
	labels    map[string]string
	info      api.AgentInfo
	created   time.Time
}

// podTracker holds the remembered pods and the tombstones. The zero value is
// ready to use; all methods are safe for concurrent use.
type podTracker struct {
	mu         sync.Mutex
	pods       map[types.UID]*trackedPod
	tombstones map[string]*podTombstone // by agentKey
	// starts records, per namespace and agent name, when a start last
	// began, so a tombstone found by a List() that raced the start is
	// not added after it.
	starts map[string]time.Time
}

func (t *podTracker) init() {
	if t.pods == nil {
		t.pods = make(map[types.UID]*trackedPod)
		t.tombstones = make(map[string]*podTombstone)
		t.starts = make(map[string]time.Time)
	}
}

// podAgentKey identifies the agent a pod belongs to: namespace, scion.name
// and project ID.
func podAgentKey(namespace string, labels map[string]string) string {
	return namespace + "/" + labels["scion.name"] + "/" + projectkeys.ProjectIDFromLabels(labels)
}

func startKey(namespace, agentName string) string {
	return namespace + "/" + agentName
}

// prune drops entries older than podTombstoneTTL. Caller holds t.mu.
func (t *podTracker) prune(now time.Time) {
	for uid, p := range t.pods {
		if now.Sub(p.seen) > podTombstoneTTL {
			delete(t.pods, uid)
		}
	}
	for k, ts := range t.tombstones {
		if now.Sub(ts.created) > podTombstoneTTL {
			delete(t.tombstones, k)
		}
	}
	for k, at := range t.starts {
		if now.Sub(at) > podTombstoneTTL {
			delete(t.starts, k)
		}
	}
}

// track remembers pod (or refreshes it) and drops any tombstone for its
// agent. Caller holds t.mu.
func (t *podTracker) track(pod *corev1.Pod, now time.Time) {
	if pod == nil || pod.UID == "" || pod.Labels["scion.name"] == "" {
		return
	}
	key := podAgentKey(pod.Namespace, pod.Labels)
	t.pods[pod.UID] = &trackedPod{
		uid:         pod.UID,
		namespace:   pod.Namespace,
		name:        pod.Name,
		agentKey:    key,
		labels:      pod.Labels,
		base:        k8sPodBaseAgentInfo(pod),
		recoverable: k8sPodWorkspaceRecoverable(pod),
		seen:        now,
	}
	delete(t.tombstones, key)
}

// noteAgentStart records that a start for the agent named agentName in namespace
// began, and drops any tombstone for that agent.
func (r *KubernetesRuntime) noteAgentStart(namespace, agentName string) {
	t := &r.podTrack
	now := r.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.init()
	t.starts[startKey(namespace, agentName)] = now
	prefix := startKey(namespace, agentName) + "/"
	for k := range t.tombstones {
		if strings.HasPrefix(k, prefix) {
			delete(t.tombstones, k)
		}
	}
}

// trackCreatedPod remembers a pod Run() just created, so that a pod removed
// before any List() sees it is still noticed.
func (r *KubernetesRuntime) trackCreatedPod(pod *corev1.Pod) {
	t := &r.podTrack
	now := r.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.init()
	t.track(pod, now)
}

// listScopeMatches reports whether a pod with namespace and labels is within
// the scope of a List() over listNamespace ("" for all namespaces) with
// labelFilter.
func listScopeMatches(listNamespace string, labelFilter map[string]string, namespace string, labels map[string]string) bool {
	if listNamespace != "" && namespace != listNamespace {
		return false
	}
	if len(labelFilter) == 0 {
		_, ok := labels["scion.name"]
		return ok
	}
	for k, v := range labelFilter {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// reconcilePodTombstones updates the tracker with the pods one List() over
// listNamespace and labelFilter returned and returns the tombstone entries
// that List() should report in addition to them.
func (r *KubernetesRuntime) reconcilePodTombstones(ctx context.Context, listNamespace string, labelFilter map[string]string, pods []corev1.Pod) []api.AgentInfo {
	t := &r.podTrack
	detectedAt := r.now()

	t.mu.Lock()
	t.init()
	t.prune(detectedAt)
	listed := make(map[types.UID]bool, len(pods))
	for i := range pods {
		listed[pods[i].UID] = true
		t.track(&pods[i], detectedAt)
	}
	var vanished []*trackedPod
	for uid, p := range t.pods {
		if listed[uid] || !listScopeMatches(listNamespace, labelFilter, p.namespace, p.labels) {
			continue
		}
		delete(t.pods, uid)
		vanished = append(vanished, p)
	}
	t.mu.Unlock()

	// One Events list per vanished pod, outside the lock.
	type found struct {
		pod    *trackedPod
		reason state.ExitReason
		event  string
	}
	var disrupted []found
	for _, p := range vanished {
		if reason, ev := r.podDisruptionFromEvents(ctx, p); reason != "" {
			disrupted = append(disrupted, found{pod: p, reason: reason, event: ev})
		}
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	now := r.now()
	for _, d := range disrupted {
		p := d.pod
		// A newer pod for the agent (listed now, or created by a start
		// since) or a start that began after the pod was found missing
		// wins over the tombstone.
		if t.hasPodFor(p.agentKey) {
			continue
		}
		if at, ok := t.starts[startKey(p.namespace, p.labels["scion.name"])]; ok && !at.Before(detectedAt) {
			continue
		}
		info := p.base
		info.Phase = string(state.PhaseError)
		if p.recoverable {
			info.Phase = string(state.PhaseStopped)
		}
		info.ExitReason = string(d.reason)
		info.ExitCode = nil
		info.Runtime = r.Name()
		info.ContainerStatus = fmt.Sprintf("deleted (%s)", d.event)
		t.tombstones[p.agentKey] = &podTombstone{
			namespace: p.namespace,
			labels:    p.labels,
			info:      info,
			created:   now,
		}
		runtimeLog.Info("Agent pod removed by a disruption before it was listed as terminal",
			"pod", p.name, "namespace", p.namespace, "exit_reason", d.reason)
	}

	var out []api.AgentInfo
	for key, ts := range t.tombstones {
		if t.hasPodFor(key) || !listScopeMatches(listNamespace, labelFilter, ts.namespace, ts.labels) {
			continue
		}
		out = append(out, ts.info)
	}
	return out
}

// hasPodFor reports whether a remembered pod belongs to agentKey. Caller
// holds t.mu.
func (t *podTracker) hasPodFor(agentKey string) bool {
	for _, p := range t.pods {
		if p.agentKey == agentKey {
			return true
		}
	}
	return false
}

// podDisruptionFromEvents lists the Events recorded on the vanished pod and
// returns preempted or evicted when one shows the pod was Preempted or
// Evicted, with the Event reason; "" otherwise (including when the list
// fails).
func (r *KubernetesRuntime) podDisruptionFromEvents(ctx context.Context, p *trackedPod) (state.ExitReason, string) {
	if r.Client == nil || r.Client.Clientset == nil {
		return "", ""
	}
	sel := fields.Set{
		"involvedObject.kind": "Pod",
		"involvedObject.name": p.name,
		"involvedObject.uid":  string(p.uid),
	}.AsSelector().String()
	events, err := r.Client.Clientset.CoreV1().Events(p.namespace).List(ctx, metav1.ListOptions{FieldSelector: sel})
	if err != nil {
		runtimeLog.Debug("Failed to list events for a vanished agent pod", "pod", p.name, "namespace", p.namespace, "error", err)
		return "", ""
	}
	var reason state.ExitReason
	var eventReason string
	for _, ev := range events.Items {
		// Field selectors are not honoured everywhere (for example by
		// fake clients); match the pod here too.
		if ev.InvolvedObject.UID != p.uid {
			continue
		}
		switch ev.Reason {
		case podEventReasonPreempted:
			return state.ExitReasonPreempted, ev.Reason
		case podEventReasonEvicted:
			reason, eventReason = state.ExitReasonEvicted, ev.Reason
		}
	}
	return reason, eventReason
}

// k8sPodBaseAgentInfo returns the identity fields List() reports for pod,
// without any status (phase, container status, exit fields) or the runtime
// name.
func k8sPodBaseAgentInfo(p *corev1.Pod) api.AgentInfo {
	projectPath := projectkeys.ProjectPathFromLabels(p.Annotations)
	if projectPath == "" {
		projectPath = projectkeys.ProjectPathFromLabels(p.Labels)
	}

	var agentImage string
	for _, c := range p.Spec.Containers {
		if c.Name == agentContainerName {
			agentImage = c.Image
			break
		}
	}

	return api.AgentInfo{
		ContainerID: p.Name, // Pod name serves as the container identifier
		RunID:       p.Labels[api.LabelRunID],
		Name:        p.Labels["scion.name"],
		Template:    p.Labels["scion.template"],
		Project:     projectkeys.ProjectNameFromLabels(p.Labels),
		ProjectID:   projectkeys.ProjectIDFromLabels(p.Labels),
		ProjectPath: projectPath,
		Labels:      p.Labels,
		Annotations: p.Annotations,
		Image:       agentImage,
		Kubernetes: &api.AgentK8sMetadata{
			Namespace: p.Namespace,
			PodName:   p.Name,
			UID:       string(p.UID),
		},
	}
}
