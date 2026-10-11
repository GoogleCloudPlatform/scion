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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
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
// that the pod was Preempted or Evicted, a tombstone entry for the agent
// (phase stopped or error, exit reason preempted or evicted) is kept for
// podTombstoneTTL so the hub can record the reason. Tombstones are returned
// only by a List() whose context carries WithVanishedPodReports (the broker
// heartbeat); every other caller sees live pods only.
//
// No tombstone is made:
//   - without such event evidence, so a pod removed by an explicit stop or
//     delete never reads as a disruption;
//   - for a pod a List() already reported as terminal (stopped or error) or
//     with a disruption reason: that state has been reported, and scion
//     itself may remove such a pod (a stop, or a start's pre-clean);
//   - when a newer pod for the agent is listed or created, or a start for
//     the agent began, after the List() snapshot that found the pod missing.
//
// Ordering against concurrent Run() calls uses a sequence number rather than
// the clock: a List() takes a snapshot of the sequence before listing pods,
// and a pod tracked or a start noted after that snapshot is never treated as
// vanished by that List() and suppresses its tombstones.
//
// Only a List() with the heartbeat marker finds vanished pods, looks up their
// Events and marks pods as reported; every other List() only remembers the
// pods it sees, so it never waits on lookups.
//
// The tracker is in-memory only and bounded: remembered pods, pending
// lookups and tombstones not refreshed within podTombstoneTTL are pruned. A
// List() performs at most podEventLookupsPerList Events lookups, each
// bounded by podEventLookupTimeout, and none that would run past its context
// deadline; further vanished pods are carried over to the next List().
//
// The tracker is lost on broker restart; a pod that vanishes while the
// broker is down is then not reported (the previous behaviour).
//
// Each tombstone entry has VanishedPodReport set (IsVanishedPodReport), so
// the heartbeat can drop it for an agent with a start in flight.

// podTombstoneTTL bounds how long a tombstone is reported and how long a
// remembered pod or pending lookup that no List() refreshes is kept.
const podTombstoneTTL = 5 * time.Minute

// podEventLookupTimeout bounds one Events list for a vanished pod.
const podEventLookupTimeout = 2 * time.Second

// podEventLookupsPerList caps the Events lookups one List() performs, so
// lookups stay well inside the heartbeat's listing deadline.
const podEventLookupsPerList = 3

// podEventWarnInterval rate-limits the warning logged for failed lookups
// other than Forbidden (which is logged once per process).
const podEventWarnInterval = 10 * time.Minute

// podEventReasonPreempted and podEventReasonEvicted are the Event reasons the
// scheduler (preemption) and the kubelet (node-pressure eviction) record on
// the pod they remove.
const (
	podEventReasonPreempted = "Preempted"
	podEventReasonEvicted   = "Evicted"
)

type vanishedPodReportsKey struct{}

// WithVanishedPodReports marks ctx so that a Kubernetes runtime List() made
// with it also returns tombstone entries for agent pods removed by a
// preemption or eviction before any List() reported them terminal. Only the
// broker heartbeat should use it.
func WithVanishedPodReports(ctx context.Context) context.Context {
	return context.WithValue(ctx, vanishedPodReportsKey{}, true)
}

func vanishedPodReportsRequested(ctx context.Context) bool {
	v, _ := ctx.Value(vanishedPodReportsKey{}).(bool)
	return v
}

// IsVanishedPodReport reports whether a is a tombstone entry: an agent pod
// already removed by a preemption or eviction, reported only to a List()
// made with WithVanishedPodReports.
func IsVanishedPodReport(a api.AgentInfo) bool {
	return a.VanishedPodReport
}

// trackedPod is a remembered agent pod.
type trackedPod struct {
	uid         types.UID
	namespace   string
	name        string
	agentKey    string
	labels      map[string]string
	base        api.AgentInfo // identity fields as List() reports them
	recoverable bool          // k8sPodWorkspaceRecoverable
	// reported is set once a List() reported the pod terminal or with a
	// disruption reason; such a pod never gets a tombstone.
	reported bool
	seen     time.Time
	seenSeq  uint64
	// vanishSnap is the snapshot of the List() that found the pod missing.
	vanishSnap uint64
}

// podTombstone is the entry List() reports for a vanished, disrupted pod.
type podTombstone struct {
	namespace string
	labels    map[string]string
	info      api.AgentInfo
	created   time.Time
}

// startMark records when a start for an agent began.
type startMark struct {
	at  time.Time
	seq uint64
}

// podTracker holds the remembered pods, the pending Events lookups and the
// tombstones. The zero value is ready to use; all methods are safe for
// concurrent use.
type podTracker struct {
	mu         sync.Mutex
	seq        uint64
	pods       map[types.UID]*trackedPod
	pending    map[types.UID]*trackedPod // vanished, lookup not done yet
	tombstones map[string]*podTombstone  // by agentKey
	starts     map[string]startMark      // by namespace/agent name

	forbiddenLogged bool
	lastWarn        time.Time
}

func (t *podTracker) init() {
	if t.pods == nil {
		t.pods = make(map[types.UID]*trackedPod)
		t.pending = make(map[types.UID]*trackedPod)
		t.tombstones = make(map[string]*podTombstone)
		t.starts = make(map[string]startMark)
	}
}

// next returns a new sequence number. Caller holds t.mu.
func (t *podTracker) next() uint64 {
	t.seq++
	return t.seq
}

// snapshot returns the current sequence number; a List() takes it before it
// lists pods.
func (t *podTracker) snapshot() uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.seq
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
	for uid, p := range t.pending {
		if now.Sub(p.seen) > podTombstoneTTL {
			delete(t.pending, uid)
		}
	}
	for k, ts := range t.tombstones {
		if now.Sub(ts.created) > podTombstoneTTL {
			delete(t.tombstones, k)
		}
	}
	for k, m := range t.starts {
		if now.Sub(m.at) > podTombstoneTTL {
			delete(t.starts, k)
		}
	}
}

// track remembers pod (or refreshes it) and drops any tombstone for its
// agent. reported marks a pod a List() reported terminal or with a
// disruption reason; it is sticky. Caller holds t.mu.
func (t *podTracker) track(pod *corev1.Pod, now time.Time, reported bool) {
	if pod == nil || pod.UID == "" || pod.Labels["scion.name"] == "" {
		return
	}
	if prev, ok := t.pods[pod.UID]; ok && prev.reported {
		reported = true
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
		reported:    reported,
		seen:        now,
		seenSeq:     t.next(),
	}
	delete(t.pending, pod.UID)
	delete(t.tombstones, key)
}

// noteAgentStart records that a start for the agent named agentName in
// namespace began, and drops any tombstone for that agent.
func (r *KubernetesRuntime) noteAgentStart(namespace, agentName string) {
	t := &r.podTrack
	now := r.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.init()
	t.starts[startKey(namespace, agentName)] = startMark{at: now, seq: t.next()}
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
	t.track(pod, now, false)
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
		if got, ok := labels[k]; !ok || got != v {
			return false
		}
	}
	return true
}

// reconcilePodTombstones updates the tracker with the pods one List() over
// listNamespace and labelFilter returned (reported marks the pods it
// reported terminal or with a disruption reason; snap is the sequence
// snapshot taken before the pods were listed). Only when ctx carries
// WithVanishedPodReports does it also find vanished pods, look up their
// Events and return the tombstone entries to report in addition to them.
func (r *KubernetesRuntime) reconcilePodTombstones(ctx context.Context, snap uint64, listNamespace string, labelFilter map[string]string, pods []corev1.Pod, reported map[types.UID]bool) []api.AgentInfo {
	t := &r.podTrack
	requested := vanishedPodReportsRequested(ctx)

	t.mu.Lock()
	t.init()
	t.prune(r.now())
	listed := make(map[types.UID]bool, len(pods))
	for i := range pods {
		listed[pods[i].UID] = true
		// Only the heartbeat's report counts as reporting a pod.
		t.track(&pods[i], r.now(), requested && reported[pods[i].UID])
	}
	if !requested {
		// Any other List() only remembers the pods it sees.
		t.mu.Unlock()
		return nil
	}
	for uid, p := range t.pods {
		// A pod tracked after the snapshot (created by a concurrent
		// start) cannot be in this listing; it has not vanished.
		if listed[uid] || p.seenSeq > snap || !listScopeMatches(listNamespace, labelFilter, p.namespace, p.labels) {
			continue
		}
		delete(t.pods, uid)
		if p.reported {
			continue
		}
		p.vanishSnap = snap
		t.pending[uid] = p
	}
	// At most podEventLookupsPerList lookups, oldest first; the rest wait
	// for the next List().
	lookups := make([]*trackedPod, 0, len(t.pending))
	for _, p := range t.pending {
		lookups = append(lookups, p)
	}
	sort.Slice(lookups, func(i, j int) bool { return lookups[i].seenSeq < lookups[j].seenSeq })
	if len(lookups) > podEventLookupsPerList {
		lookups = lookups[:podEventLookupsPerList]
	}
	for _, p := range lookups {
		delete(t.pending, p.uid)
	}
	t.mu.Unlock()

	type found struct {
		pod    *trackedPod
		reason state.ExitReason
		event  string
	}
	var disrupted []found
	var deferred []*trackedPod
	for i, p := range lookups {
		// Leave the rest for the next List() rather than run past the
		// caller's deadline.
		if dl, ok := ctx.Deadline(); ok && time.Until(dl) < podEventLookupTimeout {
			deferred = lookups[i:]
			break
		}
		if reason, ev := r.podDisruptionFromEvents(ctx, p); reason != "" {
			disrupted = append(disrupted, found{pod: p, reason: reason, event: ev})
		}
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	for _, p := range deferred {
		t.pending[p.uid] = p
	}
	now := r.now()
	for _, d := range disrupted {
		p := d.pod
		// A newer pod for the agent, or a start that began after the
		// snapshot that found the pod missing, wins over the tombstone.
		if t.hasPodFor(p.agentKey) {
			continue
		}
		if m, ok := t.starts[startKey(p.namespace, p.labels["scion.name"])]; ok && m.seq > p.vanishSnap {
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
		info.VanishedPodReport = true
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
// fails or times out).
func (r *KubernetesRuntime) podDisruptionFromEvents(ctx context.Context, p *trackedPod) (state.ExitReason, string) {
	if r.Client == nil || r.Client.Clientset == nil {
		return "", ""
	}
	lookupCtx, cancel := context.WithTimeout(ctx, podEventLookupTimeout)
	defer cancel()
	sel := fields.Set{
		"involvedObject.kind": "Pod",
		"involvedObject.name": p.name,
		"involvedObject.uid":  string(p.uid),
	}.AsSelector().String()
	events, err := r.Client.Clientset.CoreV1().Events(p.namespace).List(lookupCtx, metav1.ListOptions{FieldSelector: sel})
	if err != nil {
		r.warnEventLookupFailed(p, err)
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

// warnEventLookupFailed logs a failed Events lookup at Warn: a Forbidden
// error once per process (missing RBAC will not fix itself), any other error
// at most once per podEventWarnInterval.
func (r *KubernetesRuntime) warnEventLookupFailed(p *trackedPod, err error) {
	t := &r.podTrack
	t.mu.Lock()
	log := false
	if k8serrors.IsForbidden(err) {
		if !t.forbiddenLogged {
			t.forbiddenLogged = true
			log = true
		}
	} else if now := r.now(); t.lastWarn.IsZero() || now.Sub(t.lastWarn) >= podEventWarnInterval {
		t.lastWarn = now
		log = true
	}
	t.mu.Unlock()
	if log {
		runtimeLog.Warn("Failed to list events for a vanished agent pod; a preemption or eviction may not be reported",
			"pod", p.name, "namespace", p.namespace, "error", err)
	}
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
