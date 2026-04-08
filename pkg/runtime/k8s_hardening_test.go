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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

// --- Stage 2.1: Sync retry behavior ---

func TestSyncWithRetry_SucceedsOnFirstAttempt(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()
	calls := 0
	err := rt.syncWithRetry(context.Background(), func() error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Errorf("expected 1 call, got %d", calls)
	}
}

func TestSyncWithRetry_RetriesOnTransientError(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()
	calls := 0
	err := rt.syncWithRetry(context.Background(), func() error {
		calls++
		if calls < 3 {
			return fmt.Errorf("stream error: connection reset by peer")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 3 {
		t.Errorf("expected 3 calls, got %d", calls)
	}
}

func TestSyncWithRetry_NoRetryOnPermanentError(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()
	calls := 0
	err := rt.syncWithRetry(context.Background(), func() error {
		calls++
		return fmt.Errorf("permission denied: you do not have access")
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if calls != 1 {
		t.Errorf("expected 1 call (no retry on permanent error), got %d", calls)
	}
}

func TestSyncWithRetry_MaxRetriesExceeded(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()
	calls := 0
	err := rt.syncWithRetry(context.Background(), func() error {
		calls++
		return fmt.Errorf("connection reset by peer")
	})
	if err == nil {
		t.Fatal("expected error after max retries")
	}
	// 1 initial + 3 retries = 4 total
	if calls != 4 {
		t.Errorf("expected 4 calls, got %d", calls)
	}
}

func TestSyncWithRetry_RespectsContextCancellation(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	// Cancel immediately to prevent any backoff waits
	cancel()
	err := rt.syncWithRetry(ctx, func() error {
		calls++
		return fmt.Errorf("connection reset by peer")
	})
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
}

func TestIsSyncTransientError(t *testing.T) {
	tests := []struct {
		err       string
		transient bool
	}{
		{"connection reset by peer", true},
		{"stream error: broken pipe", true},
		{"unexpected EOF", true},
		{"i/o timeout", true},
		{"TLS handshake failure", true},
		{"use of closed network connection", true},
		{"permission denied", false},
		{"pod not found", false},
		{"", false},
	}
	for _, tt := range tests {
		got := isSyncTransientError(fmt.Errorf("%s", tt.err))
		if got != tt.transient {
			t.Errorf("isSyncTransientError(%q) = %v, want %v", tt.err, got, tt.transient)
		}
	}
}

// --- Stage 2.2: Pod spec hardening ---

func TestBuildPod_SecurityContext_FSGroup(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	config := RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
	}

	pod, err := rt.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod failed: %v", err)
	}

	if pod.Spec.SecurityContext == nil {
		t.Fatal("expected SecurityContext to be set")
	}
	if pod.Spec.SecurityContext.FSGroup == nil {
		t.Fatal("expected FSGroup to be set")
	}
	if pod.Spec.SecurityContext.RunAsNonRoot == nil || !*pod.Spec.SecurityContext.RunAsNonRoot {
		t.Fatal("expected RunAsNonRoot=true to be set")
	}
	if pod.Spec.SecurityContext.SeccompProfile == nil {
		t.Fatal("expected SeccompProfile to be set")
	}
	if pod.Spec.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("expected SeccompProfile RuntimeDefault, got %q", pod.Spec.SecurityContext.SeccompProfile.Type)
	}
}

func TestBuildPod_ContainerSecurityContextRestrictedDefaults(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	config := RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
	}

	pod, err := rt.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod failed: %v", err)
	}

	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("expected exactly one container, got %d", len(pod.Spec.Containers))
	}
	securityContext := pod.Spec.Containers[0].SecurityContext
	if securityContext == nil {
		t.Fatal("expected container SecurityContext to be set")
	}
	if securityContext.AllowPrivilegeEscalation == nil || *securityContext.AllowPrivilegeEscalation {
		t.Fatal("expected AllowPrivilegeEscalation=false to be set")
	}
	if securityContext.Capabilities == nil {
		t.Fatal("expected container capabilities to be set")
	}
	if len(securityContext.Capabilities.Drop) != 1 || securityContext.Capabilities.Drop[0] != corev1.Capability("ALL") {
		t.Fatalf("expected capabilities.drop=[ALL], got %v", securityContext.Capabilities.Drop)
	}
}

func TestBuildPod_NodeSelector(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	config := RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
		Kubernetes: &api.KubernetesConfig{
			NodeSelector: map[string]string{
				"gpu":  "true",
				"zone": "us-central1-a",
			},
		},
	}

	pod, err := rt.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod failed: %v", err)
	}

	if len(pod.Spec.NodeSelector) != 2 {
		t.Errorf("expected 2 nodeSelector entries, got %d", len(pod.Spec.NodeSelector))
	}
	if pod.Spec.NodeSelector["gpu"] != "true" {
		t.Errorf("expected nodeSelector gpu=true, got %s", pod.Spec.NodeSelector["gpu"])
	}
}

func TestBuildPod_Tolerations(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	config := RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
		Kubernetes: &api.KubernetesConfig{
			Tolerations: []api.K8sToleration{
				{
					Key:      "dedicated",
					Operator: "Equal",
					Value:    "agents",
					Effect:   "NoSchedule",
				},
			},
		},
	}

	pod, err := rt.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod failed: %v", err)
	}

	if len(pod.Spec.Tolerations) != 1 {
		t.Fatalf("expected 1 toleration, got %d", len(pod.Spec.Tolerations))
	}
	if pod.Spec.Tolerations[0].Key != "dedicated" {
		t.Errorf("expected toleration key 'dedicated', got %s", pod.Spec.Tolerations[0].Key)
	}
	if pod.Spec.Tolerations[0].Value != "agents" {
		t.Errorf("expected toleration value 'agents', got %s", pod.Spec.Tolerations[0].Value)
	}
	if pod.Spec.Tolerations[0].Effect != corev1.TaintEffectNoSchedule {
		t.Errorf("expected effect NoSchedule, got %s", pod.Spec.Tolerations[0].Effect)
	}
}

func TestBuildPod_RuntimeClassName(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	config := RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
		Kubernetes: &api.KubernetesConfig{
			RuntimeClassName: "gvisor",
		},
	}

	pod, err := rt.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod failed: %v", err)
	}

	if pod.Spec.RuntimeClassName == nil {
		t.Fatal("expected RuntimeClassName to be set")
	}
	if *pod.Spec.RuntimeClassName != "gvisor" {
		t.Errorf("expected RuntimeClassName 'gvisor', got %s", *pod.Spec.RuntimeClassName)
	}
}

func TestBuildPod_EphemeralStorageLimits(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	config := RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
		Resources: &api.ResourceSpec{
			Disk: "10Gi",
		},
	}

	pod, err := rt.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod failed: %v", err)
	}

	res := pod.Spec.Containers[0].Resources

	// Disk should appear in both requests and limits
	if _, ok := res.Requests[corev1.ResourceEphemeralStorage]; !ok {
		t.Error("expected ephemeral-storage in requests")
	}
	if _, ok := res.Limits[corev1.ResourceEphemeralStorage]; !ok {
		t.Error("expected ephemeral-storage in limits")
	}

	reqVal := res.Requests[corev1.ResourceEphemeralStorage]
	limVal := res.Limits[corev1.ResourceEphemeralStorage]
	if reqVal.String() != limVal.String() {
		t.Errorf("expected requests (%s) == limits (%s) for ephemeral-storage", reqVal.String(), limVal.String())
	}
}

func TestBuildPod_SafeResourceParsing_InvalidValues(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	tests := []struct {
		name   string
		config RunConfig
	}{
		{
			name: "invalid CPU request",
			config: RunConfig{
				Name: "test", Image: "test:latest",
				Resources: &api.ResourceSpec{Requests: api.ResourceList{CPU: "not-a-cpu"}},
			},
		},
		{
			name: "invalid memory limit",
			config: RunConfig{
				Name: "test", Image: "test:latest",
				Resources: &api.ResourceSpec{Limits: api.ResourceList{Memory: "xyz"}},
			},
		},
		{
			name: "invalid disk",
			config: RunConfig{
				Name: "test", Image: "test:latest",
				Resources: &api.ResourceSpec{Disk: "bogus"},
			},
		},
		{
			name: "invalid k8s extended resource",
			config: RunConfig{
				Name: "test", Image: "test:latest",
				Kubernetes: &api.KubernetesConfig{
					Resources: &api.K8sResources{
						Limits: map[string]string{"nvidia.com/gpu": "not-a-number"},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := rt.buildPod("default", tt.config)
			if err == nil {
				t.Error("expected error for invalid resource value, got nil")
			}
		})
	}
}

// --- Stage 2.3: Image handling policy ---

func TestBuildPod_ImagePullPolicy(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	tests := []struct {
		policy   string
		expected corev1.PullPolicy
	}{
		{"Always", corev1.PullAlways},
		{"Never", corev1.PullNever},
		{"IfNotPresent", corev1.PullIfNotPresent},
		{"", corev1.PullIfNotPresent}, // default
	}

	for _, tt := range tests {
		t.Run("policy_"+tt.policy, func(t *testing.T) {
			config := RunConfig{
				Name:         "test-agent",
				Image:        "test:latest",
				UnixUsername: "scion",
			}
			if tt.policy != "" {
				config.Kubernetes = &api.KubernetesConfig{
					ImagePullPolicy: tt.policy,
				}
			}
			pod, err := rt.buildPod("default", config)
			if err != nil {
				t.Fatalf("buildPod failed: %v", err)
			}
			if pod.Spec.Containers[0].ImagePullPolicy != tt.expected {
				t.Errorf("expected pull policy %s, got %s", tt.expected, pod.Spec.Containers[0].ImagePullPolicy)
			}
		})
	}
}

func TestBuildPod_ImagePullPolicy_Invalid(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	config := RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
		Kubernetes: &api.KubernetesConfig{
			ImagePullPolicy: "InvalidPolicy",
		},
	}

	_, err := rt.buildPod("default", config)
	if err == nil {
		t.Error("expected error for invalid imagePullPolicy")
	}
}

func TestImageExists_Validation(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	tests := []struct {
		image   string
		wantErr bool
	}{
		{"valid:latest", false},
		{"gcr.io/project/image:tag", false},
		{"", true},
		{"image with spaces", true},
		{"image\twith\ttabs", true},
	}

	for _, tt := range tests {
		t.Run(tt.image, func(t *testing.T) {
			_, err := rt.ImageExists(context.Background(), tt.image)
			if (err != nil) != tt.wantErr {
				t.Errorf("ImageExists(%q) error = %v, wantErr %v", tt.image, err, tt.wantErr)
			}
		})
	}
}

// --- Stage 2.4: Multi-namespace operations ---

func TestList_AllNamespaces(t *testing.T) {
	clientset := k8sfake.NewSimpleClientset()

	// Create pods in different namespaces
	for _, ns := range []string{"default", "production", "staging"} {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("agent-%s", ns),
				Namespace: ns,
				Labels:    map[string]string{"scion.name": fmt.Sprintf("agent-%s", ns)},
				Annotations: map[string]string{
					"scion.namespace": ns,
				},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
			Spec:   corev1.PodSpec{Containers: []corev1.Container{{Image: "test:latest"}}},
		}
		_, err := clientset.CoreV1().Pods(ns).Create(context.Background(), pod, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("failed to create pod in %s: %v", ns, err)
		}
	}

	scheme := k8sruntime.NewScheme()
	dynClient := fake.NewSimpleDynamicClient(scheme)
	client := k8s.NewTestClient(dynClient, clientset)

	rt := NewKubernetesRuntime(client)
	rt.ListAllNamespaces = true

	agents, err := rt.List(context.Background(), nil)
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}

	if len(agents) != 3 {
		t.Errorf("expected 3 agents across namespaces, got %d", len(agents))
	}

	// Verify namespace metadata is populated
	for _, a := range agents {
		if a.Kubernetes == nil {
			t.Error("expected Kubernetes metadata on agent info")
			continue
		}
		if a.Kubernetes.Namespace == "" {
			t.Error("expected namespace in Kubernetes metadata")
		}
	}
}

func TestList_SingleNamespace(t *testing.T) {
	clientset := k8sfake.NewSimpleClientset()

	// Create pods in different namespaces
	for _, ns := range []string{"default", "other"} {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("agent-%s", ns),
				Namespace: ns,
				Labels:    map[string]string{"scion.name": fmt.Sprintf("agent-%s", ns)},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
			Spec:   corev1.PodSpec{Containers: []corev1.Container{{Image: "test:latest"}}},
		}
		_, _ = clientset.CoreV1().Pods(ns).Create(context.Background(), pod, metav1.CreateOptions{})
	}

	scheme := k8sruntime.NewScheme()
	dynClient := fake.NewSimpleDynamicClient(scheme)
	client := k8s.NewTestClient(dynClient, clientset)

	rt := NewKubernetesRuntime(client)
	// ListAllNamespaces is false by default

	agents, err := rt.List(context.Background(), nil)
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}

	// Should only see the pod in the default namespace
	if len(agents) != 1 {
		t.Errorf("expected 1 agent (default namespace only), got %d", len(agents))
	}
}

func TestResolveNamespace_FromAnnotation(t *testing.T) {
	clientset := k8sfake.NewSimpleClientset()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-agent",
			Namespace: "default",
			Labels:    map[string]string{"scion.name": "test-agent"},
			Annotations: map[string]string{
				"scion.namespace": "production",
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
		Spec:   corev1.PodSpec{Containers: []corev1.Container{{Image: "test:latest"}}},
	}
	_, _ = clientset.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{})

	scheme := k8sruntime.NewScheme()
	dynClient := fake.NewSimpleDynamicClient(scheme)
	client := k8s.NewTestClient(dynClient, clientset)

	rt := NewKubernetesRuntime(client)

	ns := rt.resolveNamespace(context.Background(), "test-agent")
	if ns != "production" {
		t.Errorf("expected namespace 'production', got %s", ns)
	}
}

func TestResolveNamespace_Default(t *testing.T) {
	clientset := k8sfake.NewSimpleClientset()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-agent",
			Namespace: "default",
			Labels:    map[string]string{"scion.name": "test-agent"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
		Spec:   corev1.PodSpec{Containers: []corev1.Container{{Image: "test:latest"}}},
	}
	_, _ = clientset.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{})

	scheme := k8sruntime.NewScheme()
	dynClient := fake.NewSimpleDynamicClient(scheme)
	client := k8s.NewTestClient(dynClient, clientset)

	rt := NewKubernetesRuntime(client)

	ns := rt.resolveNamespace(context.Background(), "test-agent")
	if ns != "default" {
		t.Errorf("expected namespace 'default', got %s", ns)
	}
}

func TestDelete_NamespaceSlashFormat(t *testing.T) {
	clientset := k8sfake.NewSimpleClientset()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-agent",
			Namespace: "production",
			Labels:    map[string]string{"scion.name": "test-agent"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
		Spec:   corev1.PodSpec{Containers: []corev1.Container{{Image: "test:latest"}}},
	}
	_, _ = clientset.CoreV1().Pods("production").Create(context.Background(), pod, metav1.CreateOptions{})

	scheme := k8sruntime.NewScheme()
	dynClient := fake.NewSimpleDynamicClient(scheme)
	client := k8s.NewTestClient(dynClient, clientset)

	rt := NewKubernetesRuntime(client)

	err := rt.Delete(context.Background(), "production/test-agent")
	if err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	// Verify pod was deleted
	_, err = clientset.CoreV1().Pods("production").Get(context.Background(), "test-agent", metav1.GetOptions{})
	if err == nil {
		t.Error("expected pod to be deleted")
	}
}

func TestNamespaceAnnotation_Persisted(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	config := RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
		Labels: map[string]string{
			"scion.namespace": "custom-ns",
		},
	}

	// The Run method would set the namespace annotation; verify via buildPod
	// that annotations flow through. The namespace annotation is set in Run()
	// before buildPod, so we simulate it here.
	config.Annotations = map[string]string{
		"scion.namespace": "custom-ns",
	}

	pod, err := rt.buildPod("custom-ns", config)
	if err != nil {
		t.Fatalf("buildPod failed: %v", err)
	}

	if pod.Annotations["scion.namespace"] != "custom-ns" {
		t.Errorf("expected scion.namespace annotation 'custom-ns', got %s", pod.Annotations["scion.namespace"])
	}
}

// Verify buildPod still works correctly with all existing features after Stage 2 changes
func TestBuildPod_FullConfig_Stage2(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	config := RunConfig{
		Name:             "full-test",
		Image:            "gcr.io/test/image:v1",
		UnixUsername:     "scion",
		Harness:          &EnvHarness{},
		TelemetryEnabled: true,
		Resources: &api.ResourceSpec{
			Requests: api.ResourceList{CPU: "500m", Memory: "1Gi"},
			Limits:   api.ResourceList{CPU: "2", Memory: "4Gi"},
			Disk:     "20Gi",
		},
		Kubernetes: &api.KubernetesConfig{
			RuntimeClassName:   "gvisor",
			ServiceAccountName: "agent-sa",
			ImagePullPolicy:    "Always",
			NodeSelector:       map[string]string{"pool": "agents"},
			Tolerations: []api.K8sToleration{
				{Key: "dedicated", Operator: "Equal", Value: "agents", Effect: "NoSchedule"},
			},
			Resources: &api.K8sResources{
				Limits: map[string]string{"nvidia.com/gpu": "1"},
			},
		},
	}

	pod, err := rt.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod failed: %v", err)
	}

	// Verify all Stage 2 features are applied
	if pod.Spec.SecurityContext == nil || pod.Spec.SecurityContext.FSGroup == nil {
		t.Error("expected FSGroup security context")
	}
	if pod.Spec.SecurityContext.RunAsNonRoot == nil || !*pod.Spec.SecurityContext.RunAsNonRoot {
		t.Error("expected RunAsNonRoot=true")
	}
	if pod.Spec.SecurityContext.SeccompProfile == nil || pod.Spec.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Error("expected SeccompProfile RuntimeDefault")
	}
	if pod.Spec.RuntimeClassName == nil || *pod.Spec.RuntimeClassName != "gvisor" {
		t.Error("expected RuntimeClassName gvisor")
	}
	if pod.Spec.ServiceAccountName != "agent-sa" {
		t.Error("expected ServiceAccountName agent-sa")
	}
	if pod.Spec.Containers[0].ImagePullPolicy != corev1.PullAlways {
		t.Error("expected PullAlways")
	}
	if pod.Spec.NodeSelector["pool"] != "agents" {
		t.Error("expected nodeSelector pool=agents")
	}
	if len(pod.Spec.Tolerations) != 1 {
		t.Errorf("expected 1 toleration, got %d", len(pod.Spec.Tolerations))
	}
	containerSecurityContext := pod.Spec.Containers[0].SecurityContext
	if containerSecurityContext == nil {
		t.Fatal("expected container security context")
	}
	if containerSecurityContext.AllowPrivilegeEscalation == nil || *containerSecurityContext.AllowPrivilegeEscalation {
		t.Error("expected AllowPrivilegeEscalation=false")
	}
	if containerSecurityContext.Capabilities == nil || len(containerSecurityContext.Capabilities.Drop) != 1 || containerSecurityContext.Capabilities.Drop[0] != corev1.Capability("ALL") {
		t.Errorf("expected capabilities.drop=[ALL], got %v", containerSecurityContext.Capabilities)
	}

	// Check resource values
	res := pod.Spec.Containers[0].Resources
	if res.Requests.Cpu().String() != "500m" {
		t.Errorf("expected CPU request 500m, got %s", res.Requests.Cpu().String())
	}
	if res.Limits.Cpu().String() != "2" {
		t.Errorf("expected CPU limit 2, got %s", res.Limits.Cpu().String())
	}
	if _, ok := res.Limits["nvidia.com/gpu"]; !ok {
		t.Error("expected GPU limit")
	}
	// Ephemeral storage in both requests and limits
	if _, ok := res.Requests[corev1.ResourceEphemeralStorage]; !ok {
		t.Error("expected ephemeral-storage request")
	}
	if _, ok := res.Limits[corev1.ResourceEphemeralStorage]; !ok {
		t.Error("expected ephemeral-storage limit")
	}
}

// Ensure existing tests still pass with signature change
func TestBuildPod_ReturnsError(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	config := RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
	}

	pod, err := rt.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod should succeed: %v", err)
	}
	if pod == nil {
		t.Fatal("expected non-nil pod")
	}
}
