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
	"encoding/base64"
	"encoding/json"
	"os"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

func newTestK8sRuntime() (*KubernetesRuntime, *k8sfake.Clientset, *fake.FakeDynamicClient) {
	clientset := k8sfake.NewSimpleClientset()
	scheme := k8sruntime.NewScheme()
	dynClient := fake.NewSimpleDynamicClient(scheme)
	client := k8s.NewTestClient(dynClient, clientset)
	rt := NewKubernetesRuntime(client)
	return rt, clientset, dynClient
}

func TestBuildPod_FallbackSecrets_Environment(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	config := RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
		ResolvedSecrets: []api.ResolvedSecret{
			{Name: "API_KEY", Type: "environment", Target: "API_KEY", Value: "sk-123", Source: "user"},
			{Name: "DB_PASS", Type: "environment", Target: "DATABASE_PASSWORD", Value: "secret", Source: "project"},
		},
	}

	pod, _ := rt.buildPod("default", config)

	// Environment secrets should use secretKeyRef, not literal values
	for _, env := range pod.Spec.Containers[0].Env {
		if env.Name == "API_KEY" {
			if env.Value != "" {
				t.Errorf("API_KEY should not have a literal Value, got %q", env.Value)
			}
			if env.ValueFrom == nil || env.ValueFrom.SecretKeyRef == nil {
				t.Fatal("API_KEY should have ValueFrom.SecretKeyRef")
			}
			if env.ValueFrom.SecretKeyRef.Name != "scion-agent-test-agent" {
				t.Errorf("expected secret name scion-agent-test-agent, got %s", env.ValueFrom.SecretKeyRef.Name)
			}
			if env.ValueFrom.SecretKeyRef.Key != "API_KEY" {
				t.Errorf("expected key API_KEY, got %s", env.ValueFrom.SecretKeyRef.Key)
			}
		}
		if env.Name == "DATABASE_PASSWORD" {
			if env.Value != "" {
				t.Errorf("DATABASE_PASSWORD should not have a literal Value")
			}
			if env.ValueFrom == nil || env.ValueFrom.SecretKeyRef == nil {
				t.Fatal("DATABASE_PASSWORD should have ValueFrom.SecretKeyRef")
			}
			if env.ValueFrom.SecretKeyRef.Key != "DB_PASS" {
				t.Errorf("expected key DB_PASS, got %s", env.ValueFrom.SecretKeyRef.Key)
			}
		}
	}
}

func TestBuildPod_FallbackSecrets_File(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	config := RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
		ResolvedSecrets: []api.ResolvedSecret{
			{Name: "TLS_CERT", Type: "file", Target: "/etc/ssl/cert.pem", Value: "cert-data", Source: "user"},
			{Name: "SSH_KEY", Type: "file", Target: "~/.ssh/id_rsa", Value: "key-data", Source: "user"},
		},
	}

	pod, _ := rt.buildPod("default", config)

	// Should have agent-secrets volume
	foundVolume := false
	for _, v := range pod.Spec.Volumes {
		if v.Name == "agent-secrets" {
			foundVolume = true
			if v.Secret == nil || v.Secret.SecretName != "scion-agent-test-agent" {
				t.Errorf("expected Secret volume with name scion-agent-test-agent")
			}
		}
	}
	if !foundVolume {
		t.Fatal("expected agent-secrets volume")
	}

	// Check volume mounts for file secrets
	foundCert := false
	foundSSH := false
	for _, vm := range pod.Spec.Containers[0].VolumeMounts {
		if vm.Name == "agent-secrets" && vm.MountPath == "/etc/ssl/cert.pem" {
			foundCert = true
			if vm.SubPath != "TLS_CERT" {
				t.Errorf("expected SubPath TLS_CERT, got %s", vm.SubPath)
			}
			if !vm.ReadOnly {
				t.Error("expected ReadOnly mount")
			}
		}
		if vm.Name == "agent-secrets" && vm.MountPath == "/home/scion/.ssh/id_rsa" {
			foundSSH = true
			if vm.SubPath != "SSH_KEY" {
				t.Errorf("expected SubPath SSH_KEY, got %s", vm.SubPath)
			}
		}
	}
	if !foundCert {
		t.Error("expected volume mount for TLS_CERT at /etc/ssl/cert.pem")
	}
	if !foundSSH {
		t.Error("expected volume mount for SSH_KEY at /home/scion/.ssh/id_rsa (tilde expanded)")
	}
}

func TestBuildPod_FallbackSecrets_Variable(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	config := RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
		ResolvedSecrets: []api.ResolvedSecret{
			{Name: "CONFIG", Type: "variable", Target: "config", Value: `{"key":"val"}`, Source: "user"},
		},
	}

	pod, _ := rt.buildPod("default", config)

	// Should have agent-secrets volume
	foundVolume := false
	for _, v := range pod.Spec.Volumes {
		if v.Name == "agent-secrets" {
			foundVolume = true
		}
	}
	if !foundVolume {
		t.Fatal("expected agent-secrets volume for variable secrets")
	}

	// Should have secrets.json mount
	foundMount := false
	for _, vm := range pod.Spec.Containers[0].VolumeMounts {
		if vm.Name == "agent-secrets" && vm.SubPath == "secrets.json" {
			foundMount = true
			expectedPath := "/home/scion/.scion/secrets.json"
			if vm.MountPath != expectedPath {
				t.Errorf("expected MountPath %s, got %s", expectedPath, vm.MountPath)
			}
		}
	}
	if !foundMount {
		t.Error("expected volume mount for secrets.json")
	}
}

func TestBuildPod_GKESecrets_Environment(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()
	rt.GKEMode = true

	config := RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
		ResolvedSecrets: []api.ResolvedSecret{
			{Name: "API_KEY", Type: "environment", Target: "API_KEY", Value: "sk-123", Source: "user", Ref: "projects/my-project/secrets/api-key"},
		},
	}

	pod, _ := rt.buildPod("default", config)

	// Should have secrets-store CSI volume
	foundCSI := false
	for _, v := range pod.Spec.Volumes {
		if v.Name == "secrets-store" {
			foundCSI = true
			if v.CSI == nil {
				t.Fatal("expected CSI volume source")
			}
			if v.CSI.Driver != "secrets-store.csi.x-k8s.io" {
				t.Errorf("expected CSI driver secrets-store.csi.x-k8s.io, got %s", v.CSI.Driver)
			}
			if v.CSI.VolumeAttributes["secretProviderClass"] != "scion-agent-test-agent" {
				t.Errorf("expected secretProviderClass scion-agent-test-agent, got %s", v.CSI.VolumeAttributes["secretProviderClass"])
			}
		}
	}
	if !foundCSI {
		t.Fatal("expected secrets-store CSI volume")
	}

	// Environment secrets should reference the -env K8s Secret
	envSecretName := "scion-agent-test-agent-env"
	foundEnv := false
	for _, env := range pod.Spec.Containers[0].Env {
		if env.Name == "API_KEY" {
			foundEnv = true
			if env.ValueFrom == nil || env.ValueFrom.SecretKeyRef == nil {
				t.Fatal("API_KEY should have ValueFrom.SecretKeyRef in GKE mode")
			}
			if env.ValueFrom.SecretKeyRef.Name != envSecretName {
				t.Errorf("expected secret ref to %s, got %s", envSecretName, env.ValueFrom.SecretKeyRef.Name)
			}
		}
	}
	if !foundEnv {
		t.Error("expected API_KEY env var in GKE mode")
	}

	// Should have /mnt/secrets-store mount
	foundMount := false
	for _, vm := range pod.Spec.Containers[0].VolumeMounts {
		if vm.Name == "secrets-store" && vm.MountPath == "/mnt/secrets-store" {
			foundMount = true
		}
	}
	if !foundMount {
		t.Error("expected /mnt/secrets-store volume mount")
	}
}

func TestBuildPod_GKESecrets_File(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()
	rt.GKEMode = true

	config := RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
		ResolvedSecrets: []api.ResolvedSecret{
			{Name: "TLS_CERT", Type: "file", Target: "/etc/ssl/cert.pem", Value: "cert-data", Source: "user", Ref: "projects/my-project/secrets/tls-cert"},
		},
	}

	pod, _ := rt.buildPod("default", config)

	// File secrets in GKE mode use CSI subPath mounts
	foundFileMount := false
	for _, vm := range pod.Spec.Containers[0].VolumeMounts {
		if vm.Name == "secrets-store" && vm.MountPath == "/etc/ssl/cert.pem" {
			foundFileMount = true
			if vm.SubPath != "TLS_CERT" {
				t.Errorf("expected SubPath TLS_CERT, got %s", vm.SubPath)
			}
			if !vm.ReadOnly {
				t.Error("expected ReadOnly mount for file secret")
			}
		}
	}
	if !foundFileMount {
		t.Error("expected CSI subPath mount for file secret at /etc/ssl/cert.pem")
	}
}

func TestBuildPod_GKEFallback_NoRefs(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()
	rt.GKEMode = true

	// GKE mode but no Ref fields → should fall back to K8s Secret path
	config := RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
		ResolvedSecrets: []api.ResolvedSecret{
			{Name: "API_KEY", Type: "environment", Target: "API_KEY", Value: "sk-123", Source: "user"},
		},
	}

	pod, _ := rt.buildPod("default", config)

	// Should NOT have secrets-store volume (GKE fallback)
	for _, v := range pod.Spec.Volumes {
		if v.Name == "secrets-store" {
			t.Error("should not have secrets-store volume when no Ref fields present")
		}
	}

	// Should use fallback secretKeyRef to scion-agent-test-agent
	for _, env := range pod.Spec.Containers[0].Env {
		if env.Name == "API_KEY" {
			if env.ValueFrom == nil || env.ValueFrom.SecretKeyRef == nil {
				t.Fatal("API_KEY should have ValueFrom.SecretKeyRef")
			}
			if env.ValueFrom.SecretKeyRef.Name != "scion-agent-test-agent" {
				t.Errorf("expected fallback secret name scion-agent-test-agent, got %s", env.ValueFrom.SecretKeyRef.Name)
			}
		}
	}
}

func TestBuildPod_NoSecrets(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	config := RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "scion",
	}

	pod, _ := rt.buildPod("default", config)

	// Should not have any secret-related volumes
	for _, v := range pod.Spec.Volumes {
		if v.Name == "agent-secrets" || v.Name == "secrets-store" {
			t.Errorf("should not have secret volume %s when no secrets configured", v.Name)
		}
	}

	// Should not have any secret-related env vars
	for _, env := range pod.Spec.Containers[0].Env {
		if env.Name == "GEMINI_API_KEY" || env.Name == "ANTHROPIC_API_KEY" || env.Name == "GOOGLE_API_KEY" {
			t.Errorf("should not have auth env var %s when no auth/secrets configured", env.Name)
		}
	}
}

func TestCreateAgentSecret(t *testing.T) {
	rt, clientset, _ := newTestK8sRuntime()
	ctx := context.Background()

	secrets := []api.ResolvedSecret{
		{Name: "API_KEY", Type: "environment", Target: "API_KEY", Value: "sk-123", Source: "user"},
		{Name: "TLS_CERT", Type: "file", Target: "/etc/ssl/cert.pem", Value: base64.StdEncoding.EncodeToString([]byte("cert-content")), Source: "user"},
		{Name: "RAW_FILE", Type: "file", Target: "/etc/ssl/raw.pem", Value: "raw-content", Source: "user"},
		{Name: "CONFIG", Type: "variable", Target: "config", Value: `{"key":"val"}`, Source: "user"},
	}

	labels := map[string]string{
		"scion.name":  "test-agent",
		"scion.grove": "test-project",
		"app":         "other", // Non-scion label should not be copied
	}

	name, err := rt.createAgentSecret(ctx, "default", "test-agent", secrets, labels)
	if err != nil {
		t.Fatalf("createAgentSecret failed: %v", err)
	}
	if name != "scion-agent-test-agent" {
		t.Errorf("expected secret name scion-agent-test-agent, got %s", name)
	}

	// Verify the K8s Secret was created
	secret, err := clientset.CoreV1().Secrets("default").Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get created secret: %v", err)
	}

	// Check data keys
	if string(secret.Data["API_KEY"]) != "sk-123" {
		t.Errorf("expected API_KEY=sk-123, got %s", string(secret.Data["API_KEY"]))
	}
	if string(secret.Data["TLS_CERT"]) != "cert-content" {
		t.Errorf("expected decoded TLS_CERT=cert-content, got %s", string(secret.Data["TLS_CERT"]))
	}
	if string(secret.Data["RAW_FILE"]) != "raw-content" {
		t.Errorf("expected RAW_FILE=raw-content, got %s", string(secret.Data["RAW_FILE"]))
	}

	// Check secrets.json for variable secrets
	var vars map[string]string
	if err := json.Unmarshal(secret.Data["secrets.json"], &vars); err != nil {
		t.Fatalf("failed to unmarshal secrets.json: %v", err)
	}
	if vars["config"] != `{"key":"val"}` {
		t.Errorf("expected config value, got %q", vars["config"])
	}

	// Check labels
	if secret.Labels["scion.agent"] != "test-agent" {
		t.Errorf("expected scion.agent=test-agent label")
	}
	if secret.Labels["scion.name"] != "test-agent" {
		t.Errorf("expected scion.name label propagated")
	}
	if secret.Labels["scion.grove"] != "test-project" {
		t.Errorf("expected scion.grove label propagated")
	}
	if _, ok := secret.Labels["app"]; ok {
		t.Error("non-scion label should not be copied to secret")
	}
}

func TestCreateAgentSecret_NoSecrets(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()
	ctx := context.Background()

	name, err := rt.createAgentSecret(ctx, "default", "test-agent", nil, nil)
	if err != nil {
		t.Fatalf("createAgentSecret failed: %v", err)
	}
	if name != "" {
		t.Errorf("expected empty name for no secrets, got %s", name)
	}
}

func TestDeleteCleansUpSecrets(t *testing.T) {
	rt, clientset, _ := newTestK8sRuntime()
	ctx := context.Background()

	// Create a secret with the agent label
	secrets := []api.ResolvedSecret{
		{Name: "KEY", Type: "environment", Target: "KEY", Value: "val", Source: "user"},
	}
	labels := map[string]string{"scion.name": "test-agent"}
	_, err := rt.createAgentSecret(ctx, "default", "test-agent", secrets, labels)
	if err != nil {
		t.Fatalf("createAgentSecret failed: %v", err)
	}

	// Verify secret exists
	secretList, err := clientset.CoreV1().Secrets("default").List(ctx, metav1.ListOptions{
		LabelSelector: "scion.agent=test-agent",
	})
	if err != nil {
		t.Fatalf("failed to list secrets: %v", err)
	}
	if len(secretList.Items) != 1 {
		t.Fatalf("expected 1 secret before cleanup, got %d", len(secretList.Items))
	}

	// Run cleanup
	rt.cleanupAgentSecrets(ctx, "default", "test-agent")

	// Verify secret was deleted
	secretList, err = clientset.CoreV1().Secrets("default").List(ctx, metav1.ListOptions{
		LabelSelector: "scion.agent=test-agent",
	})
	if err != nil {
		t.Fatalf("failed to list secrets after cleanup: %v", err)
	}
	if len(secretList.Items) != 0 {
		t.Errorf("expected 0 secrets after cleanup, got %d", len(secretList.Items))
	}
}

func TestCreateSecretProviderClass(t *testing.T) {
	clientset := k8sfake.NewSimpleClientset()
	scheme := k8sruntime.NewScheme()
	// Register the SPC GVR so the fake dynamic client can handle it
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "secrets-store.csi.x-k8s.io", Version: "v1", Kind: "SecretProviderClass"},
		&k8sruntime.Unknown{},
	)
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "secrets-store.csi.x-k8s.io", Version: "v1", Kind: "SecretProviderClassList"},
		&k8sruntime.Unknown{},
	)
	dynClient := fake.NewSimpleDynamicClient(scheme)
	client := k8s.NewTestClient(dynClient, clientset)
	rt := NewKubernetesRuntime(client)
	rt.GKEMode = true
	ctx := context.Background()

	secrets := []api.ResolvedSecret{
		{Name: "API_KEY", Type: "environment", Target: "API_KEY", Value: "sk-123", Source: "user", Ref: "projects/my-project/secrets/api-key"},
		{Name: "TLS_CERT", Type: "file", Target: "/etc/ssl/cert.pem", Value: "cert-data", Source: "user", Ref: "projects/my-project/secrets/tls-cert"},
		{Name: "LOCAL_ONLY", Type: "environment", Target: "LOCAL_ONLY", Value: "val", Source: "user"}, // No Ref, should be skipped
	}

	labels := map[string]string{
		"scion.name": "test-agent",
	}

	name, err := rt.createSecretProviderClass(ctx, "default", "test-agent", secrets, labels)
	if err != nil {
		t.Fatalf("createSecretProviderClass failed: %v", err)
	}
	if name != "scion-agent-test-agent" {
		t.Errorf("expected SPC name scion-agent-test-agent, got %s", name)
	}

	// Verify the SPC was created via dynamic client
	spc, err := dynClient.Resource(k8s.SecretProviderClassGVR).Namespace("default").Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get created SPC: %v", err)
	}

	// Check spec.provider
	spec, ok := spc.Object["spec"].(map[string]interface{})
	if !ok {
		t.Fatal("expected spec in SPC")
	}
	if spec["provider"] != "gcp" {
		t.Errorf("expected provider gcp, got %v", spec["provider"])
	}

	// Check that parameters.secrets contains the GCP SM paths
	params, ok := spec["parameters"].(map[string]interface{})
	if !ok {
		t.Fatal("expected parameters in spec")
	}
	secretsParam, ok := params["secrets"].(string)
	if !ok {
		t.Fatal("expected secrets parameter as string")
	}

	type gcpEntry struct {
		ResourceName string `json:"resourceName"`
		FileName     string `json:"fileName"`
	}
	var entries []gcpEntry
	if err := json.Unmarshal([]byte(secretsParam), &entries); err != nil {
		t.Fatalf("failed to parse secrets parameter: %v", err)
	}

	// Should have 2 entries (LOCAL_ONLY has no Ref, should be skipped)
	if len(entries) != 2 {
		t.Fatalf("expected 2 GCP secret entries, got %d", len(entries))
	}
	if entries[0].ResourceName != "projects/my-project/secrets/api-key/versions/latest" {
		t.Errorf("unexpected resource name: %s", entries[0].ResourceName)
	}
	if entries[0].FileName != "API_KEY" {
		t.Errorf("unexpected file name: %s", entries[0].FileName)
	}

	// Check labels
	labels2 := spc.GetLabels()
	if labels2["scion.agent"] != "test-agent" {
		t.Errorf("expected scion.agent label on SPC")
	}
}

func TestCreateAgentSecret_AlreadyExists(t *testing.T) {
	rt, clientset, _ := newTestK8sRuntime()
	ctx := context.Background()

	secrets := []api.ResolvedSecret{
		{Name: "KEY", Type: "environment", Target: "KEY", Value: "old-val", Source: "user"},
	}
	labels := map[string]string{"scion.name": "test-agent"}

	// Create the secret the first time
	_, err := rt.createAgentSecret(ctx, "default", "test-agent", secrets, labels)
	if err != nil {
		t.Fatalf("first createAgentSecret failed: %v", err)
	}

	// Create again with updated value — should succeed via delete+recreate
	secrets[0].Value = "new-val"
	_, err = rt.createAgentSecret(ctx, "default", "test-agent", secrets, labels)
	if err != nil {
		t.Fatalf("second createAgentSecret should handle already-exists: %v", err)
	}

	// Verify the secret has the new value
	s, err := clientset.CoreV1().Secrets("default").Get(ctx, "scion-agent-test-agent", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get secret: %v", err)
	}
	if string(s.Data["KEY"]) != "new-val" {
		t.Errorf("expected new-val, got %s", string(s.Data["KEY"]))
	}
}

func TestCreateAuthFileSecret_AlreadyExists(t *testing.T) {
	rt, clientset, _ := newTestK8sRuntime()
	ctx := context.Background()

	// Create a temp file for the auth file source
	tmpFile := t.TempDir() + "/auth.json"
	if err := writeTestFile(tmpFile, "old-content"); err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}

	files := []api.FileMapping{{SourcePath: tmpFile, ContainerPath: "/home/scion/.auth"}}
	labels := map[string]string{"scion.name": "test-agent"}

	// Create the first time
	err := rt.createAuthFileSecret(ctx, "default", "test-agent", files, labels)
	if err != nil {
		t.Fatalf("first createAuthFileSecret failed: %v", err)
	}

	// Update the file content and create again
	if err := writeTestFile(tmpFile, "new-content"); err != nil {
		t.Fatalf("failed to update temp file: %v", err)
	}
	err = rt.createAuthFileSecret(ctx, "default", "test-agent", files, labels)
	if err != nil {
		t.Fatalf("second createAuthFileSecret should handle already-exists: %v", err)
	}

	// Verify the secret has the new content
	s, err := clientset.CoreV1().Secrets("default").Get(ctx, "scion-auth-test-agent", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get auth secret: %v", err)
	}
	if string(s.Data["auth-file-0"]) != "new-content" {
		t.Errorf("expected new-content, got %s", string(s.Data["auth-file-0"]))
	}
}

func TestDelete_PodNotFound_StillCleansSecrets(t *testing.T) {
	rt, clientset, _ := newTestK8sRuntime()
	ctx := context.Background()

	// Create a secret labeled for the agent, but no pod
	secrets := []api.ResolvedSecret{
		{Name: "KEY", Type: "environment", Target: "KEY", Value: "val", Source: "user"},
	}
	labels := map[string]string{"scion.name": "test-agent"}
	_, err := rt.createAgentSecret(ctx, "default", "test-agent", secrets, labels)
	if err != nil {
		t.Fatalf("createAgentSecret failed: %v", err)
	}

	// Delete should not error even though pod doesn't exist
	err = rt.Delete(ctx, "test-agent")
	if err != nil {
		t.Fatalf("Delete should succeed when pod is not found: %v", err)
	}

	// Verify secrets were still cleaned up
	secretList, err := clientset.CoreV1().Secrets("default").List(ctx, metav1.ListOptions{
		LabelSelector: "scion.agent=test-agent",
	})
	if err != nil {
		t.Fatalf("failed to list secrets: %v", err)
	}
	if len(secretList.Items) != 0 {
		t.Errorf("expected 0 secrets after delete, got %d", len(secretList.Items))
	}
}

func writeTestFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0644)
}

func TestCreateSecretProviderClass_NoRefs(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()
	rt.GKEMode = true
	ctx := context.Background()

	secrets := []api.ResolvedSecret{
		{Name: "API_KEY", Type: "environment", Target: "API_KEY", Value: "sk-123", Source: "user"},
	}

	name, err := rt.createSecretProviderClass(ctx, "default", "test-agent", secrets, nil)
	if err != nil {
		t.Fatalf("createSecretProviderClass failed: %v", err)
	}
	if name != "" {
		t.Errorf("expected empty name when no refs present, got %s", name)
	}
}
