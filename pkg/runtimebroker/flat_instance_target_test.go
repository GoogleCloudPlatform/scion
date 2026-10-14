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

package runtimebroker

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// A flat Kubernetes instance's runtime_target scheduling defaults on the
// agent pod (.design/flat-runtime-brokers/contract-appendix-p3.md FC5,
// FC7): safe_to_evict reaches the pod annotation with an explicit false
// kept, an unset value adds nothing, and the priority class reaches the pod
// spec. The agent's own kubernetes block wins over both.

const safeToEvictAnnotation = "cluster-autoscaler.kubernetes.io/safe-to-evict"

// onlyPod returns the single agent pod the instance tried to create.
func (p *policyInstance) onlyPod(t *testing.T) *corev1.Pod {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.pods) != 1 {
		t.Fatalf("want exactly one agent pod, got %d", len(p.pods))
	}
	return p.pods[0]
}

// loadTargetInstances writes settings whose active profile sets its own
// safe_to_evict and priority class, and strictly loads the instances.
func loadTargetInstances(t *testing.T, instances string) []config.V1RuntimeBrokerInstanceConfig {
	t.Helper()
	writePolicyHome(t, `schema_version: "1"
active_profile: batch
runtimes:
  cluster:
    type: kubernetes
    priority_class_name: profile-priority
    safe_to_evict: true
profiles:
  batch:
    runtime: cluster
    safe_to_evict: true
server:
  broker:
    instances:
`+instances)
	got, err := config.LoadRuntimeBrokerInstances("")
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestFlatPolicy_SafeToEvictFalseOnPod: an instance's explicit
// safe_to_evict false is kept as false, not dropped as unset, and the agent
// pod carries the annotation "false".
func TestFlatPolicy_SafeToEvictFalseOnPod(t *testing.T) {
	inst := loadTargetInstances(t, `      - key: k8s-a
        name: k8s-a
        runtime_target:
          type: kubernetes
          namespace: agents
          safe_to_evict: false
`)
	flat := newPolicyInstance(t, &inst[0])
	if err := flat.start("evict-false"); err != nil {
		t.Fatal(err)
	}
	if got, ok := flat.onlyPod(t).Annotations[safeToEvictAnnotation]; !ok || got != "false" {
		t.Fatalf("pod annotation %s = %q (set %v), want \"false\" from the instance's safe_to_evict", safeToEvictAnnotation, got, ok)
	}
}

// TestFlatPolicy_SafeToEvictUnsetAddsNothing: an instance without
// safe_to_evict adds no annotation. Unset is not false, and the active
// profile's value never applies to a flat instance.
func TestFlatPolicy_SafeToEvictUnsetAddsNothing(t *testing.T) {
	inst := loadTargetInstances(t, `      - key: k8s-a
        name: k8s-a
        runtime_target:
          type: kubernetes
          namespace: agents
`)
	flat := newPolicyInstance(t, &inst[0])
	if err := flat.start("evict-unset"); err != nil {
		t.Fatal(err)
	}
	if got, ok := flat.onlyPod(t).Annotations[safeToEvictAnnotation]; ok {
		t.Fatalf("pod annotation %s = %q, want none for an unset safe_to_evict", safeToEvictAnnotation, got)
	}
}

// TestFlatPolicy_AgentSafeToEvictWins: the instance value is an overridable
// default. The agent's own (inline) kubernetes.safeToEvict true wins over
// the instance's false, so the pod has no annotation. A template's
// kubernetes block is not an agent setting (it is dropped from the job
// config), so the agent's inline config is the tier that outranks the
// instance.
func TestFlatPolicy_AgentSafeToEvictWins(t *testing.T) {
	inst := loadTargetInstances(t, `      - key: k8s-a
        name: k8s-a
        runtime_target:
          type: kubernetes
          namespace: agents
          safe_to_evict: false
`)
	flat := newPolicyInstance(t, &inst[0])
	safe := true
	if err := flat.startWith("evict-agent-true", func(o *api.StartOptions) {
		o.InlineConfig = &api.ScionConfig{Kubernetes: &api.KubernetesConfig{SafeToEvict: &safe}}
	}); err != nil {
		t.Fatal(err)
	}
	if got, ok := flat.onlyPod(t).Annotations[safeToEvictAnnotation]; ok {
		t.Fatalf("pod annotation %s = %q, want none: the agent's safeToEvict true outranks the instance's false", safeToEvictAnnotation, got)
	}
}

// TestFlatPolicy_PriorityClassOnPod: the instance's priority_class_name
// reaches the pod spec through the runtime built from its target, not the
// active profile's runtime entry; the agent's own priorityClassName wins
// over it.
func TestFlatPolicy_PriorityClassOnPod(t *testing.T) {
	inst := loadTargetInstances(t, `      - key: k8s-a
        name: k8s-a
        runtime_target:
          type: kubernetes
          namespace: agents
          priority_class_name: instance-priority
`)
	flat := newPolicyInstance(t, &inst[0])
	if err := flat.start("priority-instance"); err != nil {
		t.Fatal(err)
	}
	if got := flat.onlyPod(t).Spec.PriorityClassName; got != "instance-priority" {
		t.Fatalf("pod priorityClassName %q, want instance-priority", got)
	}

	own := newPolicyInstance(t, &inst[0])
	if err := own.startWith("priority-agent", func(o *api.StartOptions) {
		o.InlineConfig = &api.ScionConfig{Kubernetes: &api.KubernetesConfig{PriorityClassName: "agent-priority"}}
	}); err != nil {
		t.Fatal(err)
	}
	if got := own.onlyPod(t).Spec.PriorityClassName; got != "agent-priority" {
		t.Fatalf("pod priorityClassName %q, want agent-priority (the agent's own value wins)", got)
	}
}
