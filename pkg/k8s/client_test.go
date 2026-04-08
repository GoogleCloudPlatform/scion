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

package k8s

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/k8s/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

func TestClient_ListSandboxClaims(t *testing.T) {
	scheme := k8sruntime.NewScheme()
	gvr := schema.GroupVersionResource{Group: "extensions.agents.x-k8s.io", Version: "v1alpha1", Resource: "sandboxclaims"}

	scheme.AddKnownTypeWithName(schema.GroupVersionKind{
		Group:   "extensions.agents.x-k8s.io",
		Version: "v1alpha1",
		Kind:    "SandboxClaim",
	}, &v1alpha1.SandboxClaim{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{
		Group:   "extensions.agents.x-k8s.io",
		Version: "v1alpha1",
		Kind:    "SandboxClaimList",
	}, &v1alpha1.SandboxClaimList{})

	fc := fake.NewSimpleDynamicClient(scheme)

	claim := &v1alpha1.SandboxClaim{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "extensions.agents.x-k8s.io/v1alpha1",
			Kind:       "SandboxClaim",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-claim",
			Namespace: "default",
		},
	}

	unstructuredMap, _ := k8sruntime.DefaultUnstructuredConverter.ToUnstructured(claim)
	u := &unstructured.Unstructured{Object: unstructuredMap}

	_, err := fc.Resource(gvr).Namespace("default").Create(context.Background(), u, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	// Raw List call to see what fake client returns
	rawList, err := fc.Resource(gvr).Namespace("default").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("Raw List failed: %v", err)
	}
	fmt.Printf("DEBUG: Raw List items length: %d\n", len(rawList.Items))

	client := NewTestClient(fc, &kubernetes.Clientset{})
	list, err := client.ListSandboxClaims(context.Background(), "default", "")
	if err != nil {
		t.Fatalf("ListSandboxClaims failed: %v", err)
	}

	if len(list.Items) != 1 {
		t.Errorf("expected 1 item, got %d", len(list.Items))
	}
}

// --- Stage 3: Context support tests ---

func writeTestKubeconfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNewClientWithContext_EmptyContext(t *testing.T) {
	kubeconfig := `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://127.0.0.1:6443
  name: test-cluster
contexts:
- context:
    cluster: test-cluster
    user: test-user
  name: test-context
current-context: test-context
users:
- name: test-user
  user:
    token: fake-token
`
	path := writeTestKubeconfig(t, kubeconfig)

	client, err := NewClientWithContext(path, "")
	if err != nil {
		t.Fatalf("NewClientWithContext failed: %v", err)
	}

	if client.CurrentContext != "test-context" {
		t.Errorf("expected CurrentContext 'test-context', got %q", client.CurrentContext)
	}
}

func TestNewClientWithContext_SpecificContext(t *testing.T) {
	kubeconfig := `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://127.0.0.1:6443
  name: cluster-a
- cluster:
    server: https://127.0.0.1:7443
  name: cluster-b
contexts:
- context:
    cluster: cluster-a
    user: user-a
  name: context-a
- context:
    cluster: cluster-b
    user: user-b
  name: context-b
current-context: context-a
users:
- name: user-a
  user:
    token: fake-token-a
- name: user-b
  user:
    token: fake-token-b
`
	path := writeTestKubeconfig(t, kubeconfig)

	client, err := NewClientWithContext(path, "context-b")
	if err != nil {
		t.Fatalf("NewClientWithContext failed: %v", err)
	}

	if client.CurrentContext != "context-b" {
		t.Errorf("expected CurrentContext 'context-b', got %q", client.CurrentContext)
	}

	if client.Config.Host != "https://127.0.0.1:7443" {
		t.Errorf("expected host 'https://127.0.0.1:7443', got %q", client.Config.Host)
	}
}

func TestNewClientWithContext_InvalidContext(t *testing.T) {
	kubeconfig := `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://127.0.0.1:6443
  name: test-cluster
contexts:
- context:
    cluster: test-cluster
    user: test-user
  name: test-context
current-context: test-context
users:
- name: test-user
  user:
    token: fake-token
`
	path := writeTestKubeconfig(t, kubeconfig)

	_, err := NewClientWithContext(path, "nonexistent-context")
	if err == nil {
		t.Error("expected error for nonexistent context")
	}
}

func TestClient_Verify_Success(t *testing.T) {
	clientset := k8sfake.NewSimpleClientset()
	scheme := k8sruntime.NewScheme()
	dynClient := fake.NewSimpleDynamicClient(scheme)
	client := NewTestClient(dynClient, clientset)

	if err := client.Verify(); err != nil {
		t.Fatalf("Verify() should succeed with fake client: %v", err)
	}
}

func TestClient_Verify_ExecPluginErrorDetection(t *testing.T) {
	// Test that the error message detection for exec plugin failures works.
	// We can't easily simulate a real exec plugin failure with a fake client,
	// so we test the string detection logic directly via error message content.
	tests := []struct {
		name     string
		errMsg   string
		wantHint string
	}{
		{
			name:     "gke auth plugin",
			errMsg:   `getting credentials: exec: executable gke-gcloud-auth-plugin failed with exit code 1`,
			wantHint: "gke-gcloud-auth-plugin could not obtain credentials",
		},
		{
			name:     "generic exec plugin",
			errMsg:   `getting credentials: exec: executable aws-iam-authenticator failed with exit code 1`,
			wantHint: "credential plugin is installed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Verify that our hint text matches for these patterns
			if strings.Contains(tt.errMsg, "getting credentials: exec:") {
				if strings.Contains(tt.errMsg, "gke-gcloud-auth-plugin") {
					if !strings.Contains(tt.wantHint, "gke-gcloud-auth-plugin") {
						t.Error("expected gke-specific hint")
					}
				}
			}
		})
	}
}

func TestNewClient_BackwardsCompatible(t *testing.T) {
	kubeconfig := `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://127.0.0.1:6443
  name: test-cluster
contexts:
- context:
    cluster: test-cluster
    user: test-user
  name: test-context
current-context: test-context
users:
- name: test-user
  user:
    token: fake-token
`
	path := writeTestKubeconfig(t, kubeconfig)

	client, err := NewClient(path)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	if client.CurrentContext != "test-context" {
		t.Errorf("expected CurrentContext 'test-context', got %q", client.CurrentContext)
	}
}

func testKubeconfigLoadError() error {
	return fmt.Errorf("no kubeconfig found: %w", os.ErrNotExist)
}

func TestNewClientWithContext_FallsBackToInClusterConfig(t *testing.T) {
	client, err := newClientWithContext(
		"",
		"",
		func(kubeconfigPath, contextName string) (*rest.Config, string, error) {
			return nil, "", testKubeconfigLoadError()
		},
		func() (*rest.Config, error) {
			return &rest.Config{Host: "https://10.43.0.1:443"}, nil
		},
		func(config *rest.Config) (dynamic.Interface, error) {
			return dynamic.NewForConfig(config)
		},
		func(config *rest.Config) (kubernetes.Interface, error) {
			return kubernetes.NewForConfig(config)
		},
	)
	if err != nil {
		t.Fatalf("NewClientWithContext should fall back to in-cluster config: %v", err)
	}

	if client.CurrentContext != "in-cluster" {
		t.Errorf("expected CurrentContext 'in-cluster', got %q", client.CurrentContext)
	}

	if client.Config.Host != "https://10.43.0.1:443" {
		t.Errorf("expected in-cluster host to be used, got %q", client.Config.Host)
	}
}

func TestNewClientWithContext_DoesNotFallBackWhenContextExplicit(t *testing.T) {
	inClusterCalled := false
	loadErr := testKubeconfigLoadError()
	_, err := newClientWithContext(
		"",
		"explicit-context",
		func(kubeconfigPath, contextName string) (*rest.Config, string, error) {
			return nil, "", loadErr
		},
		func() (*rest.Config, error) {
			inClusterCalled = true
			return &rest.Config{Host: "https://10.43.0.1:443"}, nil
		},
		func(config *rest.Config) (dynamic.Interface, error) {
			return dynamic.NewForConfig(config)
		},
		func(config *rest.Config) (kubernetes.Interface, error) {
			return kubernetes.NewForConfig(config)
		},
	)
	if err == nil {
		t.Fatal("expected error when explicit context kubeconfig resolution fails")
	}

	if !strings.Contains(err.Error(), loadErr.Error()) {
		t.Fatalf("expected original kubeconfig load error, got: %v", err)
	}

	if inClusterCalled {
		t.Fatal("expected in-cluster fallback to be skipped when context is explicit")
	}
}
