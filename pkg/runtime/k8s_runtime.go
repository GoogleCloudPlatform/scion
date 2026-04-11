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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/gcp"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
	"golang.org/x/term"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
)

type KubernetesRuntime struct {
	Client            *k8s.Client
	DefaultNamespace  string
	GKEMode           bool // Enables GKE-specific features (SecretProviderClass CSI, GCS FUSE)
	GKEAutoDetected   bool // True when GKE was auto-detected (enables Autopilot tolerance only)
	ListAllNamespaces bool // When true, List() queries all namespaces for scion pods
}

func NewKubernetesRuntime(client *k8s.Client) *KubernetesRuntime {
	return &KubernetesRuntime{
		Client:           client,
		DefaultNamespace: "default",
	}
}

// isGKEScheduling returns true when GKE Autopilot scheduling tolerance
// should be applied — either via explicit GKEMode or auto-detection.
func (r *KubernetesRuntime) isGKEScheduling() bool {
	return r.GKEMode || r.GKEAutoDetected
}

func (r *KubernetesRuntime) Name() string {
	return "kubernetes"
}

func (r *KubernetesRuntime) ExecUser() string {
	return "scion"
}

var validExecUsername = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

func execTargetUsername(pod *corev1.Pod) string {
	if pod != nil {
		if u := strings.TrimSpace(pod.Annotations["scion.username"]); u != "" && validExecUsername.MatchString(u) {
			return u
		}
	}
	return "scion"
}

func shellQuote(arg string) string {
	return fmt.Sprintf("'%s'", strings.ReplaceAll(arg, "'", "'\"'\"'"))
}

func shellJoin(args []string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = shellQuote(arg)
	}
	return strings.Join(quoted, " ")
}

func buildExecCommandForUser(currentUser, targetUser string, cmd []string) []string {
	if targetUser == "" {
		targetUser = "scion"
	}
	if currentUser == "" || currentUser == targetUser || targetUser == "root" {
		return append([]string(nil), cmd...)
	}
	return []string{"su", "-", targetUser, "-c", shellJoin(cmd)}
}

func podRunsAsNonRoot(pod *corev1.Pod, containerName string) bool {
	if pod == nil {
		return false
	}
	if pod.Spec.SecurityContext != nil {
		if pod.Spec.SecurityContext.RunAsUser != nil {
			return *pod.Spec.SecurityContext.RunAsUser != 0
		}
		if pod.Spec.SecurityContext.RunAsNonRoot != nil && *pod.Spec.SecurityContext.RunAsNonRoot {
			return true
		}
	}
	for _, container := range pod.Spec.Containers {
		if container.Name != containerName {
			continue
		}
		if container.SecurityContext == nil {
			return false
		}
		if container.SecurityContext.RunAsUser != nil {
			return *container.SecurityContext.RunAsUser != 0
		}
		if container.SecurityContext.RunAsNonRoot != nil && *container.SecurityContext.RunAsNonRoot {
			return true
		}
		return false
	}
	return false
}

func (r *KubernetesRuntime) currentExecUser(ctx context.Context, namespace, podName string) (string, error) {
	out, err := r.execInPod(ctx, namespace, podName, []string{"id", "-un"})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func (r *KubernetesRuntime) commandForExec(ctx context.Context, namespace, podName string, cmd []string) ([]string, error) {
	pod, err := r.Client.Clientset.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	targetUser := execTargetUsername(pod)
	// Probe the live pod user at exec time instead of caching it because the
	// effective exec user is a property of the running container state.
	currentUser, err := r.currentExecUser(ctx, namespace, podName)
	if err == nil && currentUser != "" {
		return buildExecCommandForUser(currentUser, targetUser, cmd), nil
	}
	if podRunsAsNonRoot(pod, "agent") {
		return append([]string(nil), cmd...), nil
	}
	return buildExecCommandForUser("root", targetUser, cmd), nil
}

// resolveNamespace determines the namespace for a pod by looking up the
// scion.namespace annotation on the pod itself. Falls back to DefaultNamespace
// if the pod is not found or has no annotation.
func (r *KubernetesRuntime) resolveNamespace(ctx context.Context, podName string) string {
	// Try to find the pod in the default namespace first
	pod, err := r.Client.Clientset.CoreV1().Pods(r.DefaultNamespace).Get(ctx, podName, metav1.GetOptions{})
	if err == nil {
		if ns, ok := pod.Annotations["scion.namespace"]; ok && ns != "" {
			return ns
		}
		return r.DefaultNamespace
	}

	// If ListAllNamespaces is enabled, search across all namespaces
	if r.ListAllNamespaces {
		pods, err := r.Client.Clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("scion.name=%s", podName),
		})
		if err == nil {
			for _, p := range pods.Items {
				if p.Name == podName {
					return p.Namespace
				}
			}
		}
	}

	return r.DefaultNamespace
}

// parseResourceSafe parses a Kubernetes resource quantity string, returning a
// user-friendly error instead of panicking like resource.MustParse.
func parseResourceSafe(value, fieldName string) (resource.Quantity, error) {
	q, err := resource.ParseQuantity(value)
	if err != nil {
		return q, fmt.Errorf("invalid %s resource value %q: %w", fieldName, value, err)
	}
	return q, nil
}

// syncMaxRetries is the maximum number of retry attempts for sync operations.
const syncMaxRetries = 3

// syncWithRetry wraps a sync operation with exponential backoff retry for
// transient errors (connection resets, stream interruptions).
func (r *KubernetesRuntime) syncWithRetry(ctx context.Context, op func() error) error {
	var lastErr error
	for attempt := 0; attempt <= syncMaxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt-1)) * time.Second // 1s, 2s, 4s
			runtimeLog.Warn("Sync attempt failed, retrying",
				"attempt", attempt, "max_retries", syncMaxRetries,
				"backoff", backoff, "error", lastErr)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}
		lastErr = op()
		if lastErr == nil {
			return nil
		}
		// Only retry on transient errors
		if !isSyncTransientError(lastErr) {
			return lastErr
		}
	}
	return fmt.Errorf("sync failed after %d retries: %w", syncMaxRetries, lastErr)
}

// isSyncTransientError returns true if the error is likely transient and
// the sync operation should be retried.
func isSyncTransientError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	transientPatterns := []string{
		"connection reset",
		"connection refused",
		"broken pipe",
		"stream error",
		"EOF",
		"timeout",
		"i/o timeout",
		"TLS handshake",
		"use of closed network connection",
	}
	for _, pattern := range transientPatterns {
		if strings.Contains(strings.ToLower(msg), strings.ToLower(pattern)) {
			return true
		}
	}
	return false
}

func ensureAnnotations(annotations map[string]string) map[string]string {
	if annotations != nil {
		return annotations
	}
	return make(map[string]string)
}

func (r *KubernetesRuntime) Run(ctx context.Context, config RunConfig) (string, error) {
	fmt.Printf("Starting agent '%s' on Kubernetes...\n", config.Name)
	namespace := r.DefaultNamespace
	if ns, ok := config.Labels["scion.namespace"]; ok {
		namespace = ns
	} else if ns, ok := config.Labels["namespace"]; ok {
		namespace = ns
	}

	if config.Name == "" {
		config.Name = fmt.Sprintf("scion-%d", time.Now().UnixNano())
	}

	// For non-git environments, Workspace might be empty but we might have it as a volume mount
	if config.Workspace == "" {
		for _, v := range config.Volumes {
			if v.Target == "/workspace" {
				config.Workspace = v.Source
				break
			}
		}
	}

	// Persist workspace path in annotations for later sync
	if config.Workspace != "" {
		config.Annotations = ensureAnnotations(config.Annotations)
		config.Annotations["scion.workspace"] = config.Workspace
	}

	if config.GitClone != nil {
		config.Annotations = ensureAnnotations(config.Annotations)
		config.Annotations["scion.git_clone"] = "true"
		config.Annotations["scion.git_clone_url"] = config.GitClone.URL
	}

	if config.HomeDir != "" {
		config.Annotations = ensureAnnotations(config.Annotations)
		config.Annotations["scion.homedir"] = config.HomeDir
		config.Annotations["scion.username"] = config.UnixUsername
	}

	// Persist the resolved namespace as an annotation for lifecycle operations
	config.Annotations = ensureAnnotations(config.Annotations)
	config.Annotations["scion.namespace"] = namespace

	// Pre-clean stale resources from a previous agent with the same name.
	// This handles cases where the agent was force-deleted from the hub
	// or the pod was evicted/GC'd by K8s without proper cleanup.
	r.cleanupAgentSecrets(ctx, namespace, config.Name)
	r.cleanupStalePod(ctx, namespace, config.Name)

	// Create K8s Secret or SecretProviderClass before the pod
	if len(config.ResolvedSecrets) > 0 {
		useGKEPath := r.GKEMode
		if useGKEPath {
			hasRef := false
			for _, s := range config.ResolvedSecrets {
				if s.Ref != "" {
					hasRef = true
					break
				}
			}
			useGKEPath = hasRef
		}

		if useGKEPath {
			if _, err := r.createSecretProviderClass(ctx, namespace, config.Name, config.ResolvedSecrets, config.Labels); err != nil {
				return "", fmt.Errorf("failed to create SecretProviderClass: %w", err)
			}
		} else {
			if _, err := r.createAgentSecret(ctx, namespace, config.Name, config.ResolvedSecrets, config.Labels); err != nil {
				return "", fmt.Errorf("failed to launch container: %w", err)
			}
		}
	}

	// Create K8s Secret for ResolvedAuth files (portable alternative to hostPath).
	if config.ResolvedAuth != nil && len(config.ResolvedAuth.Files) > 0 {
		if err := r.createAuthFileSecret(ctx, namespace, config.Name, config.ResolvedAuth.Files, config.Labels); err != nil {
			return "", fmt.Errorf("failed to create auth file secret: %w", err)
		}
	}

	// Create PVCs for shared directories (grove-scoped, reused across agents)
	if len(config.SharedDirs) > 0 {
		if err := r.createSharedDirPVCs(ctx, namespace, config); err != nil {
			return "", fmt.Errorf("failed to create shared dir PVCs: %w", err)
		}
	}

	pod, err := r.buildPod(namespace, config)
	if err != nil {
		return "", fmt.Errorf("failed to build pod spec: %w", err)
	}

	writeK8sRuntimeDebugFile(config, namespace, pod)

	runtimeLog.Info("Creating pod", "agent", config.Name, "namespace", namespace, "image", config.Image, "phase", "pod-create")
	fmt.Printf("  Provisioning pod '%s' in namespace '%s'...\n", config.Name, namespace)
	createdPod, err := r.Client.Clientset.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		// Clean up orphaned secrets on pod creation failure
		r.cleanupAgentSecrets(ctx, namespace, config.Name)
		return "", fmt.Errorf("failed to create pod: %w", err)
	}

	// Wait for Ready
	runtimeLog.Info("Waiting for pod ready", "agent", config.Name, "namespace", namespace, "phase", "wait-schedule")
	if err := r.waitForPodReady(ctx, namespace, createdPod.Name); err != nil {
		return createdPod.Name, err
	}

	if config.HomeDir != "" {
		destHome := fmt.Sprintf("/home/%s", config.UnixUsername)
		runtimeLog.Info("Syncing agent home", "agent", config.Name, "source", config.HomeDir, "dest", destHome, "phase", "home-sync")
		fmt.Printf("  Syncing agent home (%s -> %s)...\n", config.HomeDir, destHome)
		err = r.syncWithRetry(ctx, func() error {
			return r.syncToPod(ctx, namespace, createdPod.Name, config.HomeDir, destHome)
		})
		if err != nil {
			return createdPod.Name, fmt.Errorf("failed to sync home: %w", err)
		}
		// Fix ownership: tar extraction runs as root via K8s exec, so synced
		// files are owned by root. chown them to the scion user so the
		// privilege-dropped harness process can access its home directory.
		if _, err := r.execInPod(ctx, namespace, createdPod.Name, []string{"chown", "-R", fmt.Sprintf("%s:%s", config.UnixUsername, config.UnixUsername), destHome}); err != nil {
			runtimeLog.Debug("Failed to chown home directory (non-fatal)", "error", err)
		}
	}

	if config.Workspace != "" {
		runtimeLog.Info("Syncing workspace", "agent", config.Name, "source", config.Workspace, "phase", "workspace-sync")
		fmt.Printf("  Syncing workspace (%s -> /workspace)...\n", config.Workspace)
		err = r.syncWithRetry(ctx, func() error {
			return r.syncToPod(ctx, namespace, createdPod.Name, config.Workspace, "/workspace")
		})
		if err != nil {
			return createdPod.Name, fmt.Errorf("failed to sync workspace: %w", err)
		}
		// Fix workspace ownership for the scion user
		if _, err := r.execInPod(ctx, namespace, createdPod.Name, []string{"chown", "-R", fmt.Sprintf("%s:%s", config.UnixUsername, config.UnixUsername), "/workspace"}); err != nil {
			runtimeLog.Debug("Failed to chown workspace (non-fatal)", "error", err)
		}
	}

	// Signal the startup gate: all files are synced and ownership is fixed,
	// so it's safe to launch sciontool init → tmux → harness. The gate loop
	// in the pod command polls for this marker file (see buildPod for details).
	runtimeLog.Info("Signaling startup gate", "agent", config.Name, "phase", "startup-gate")
	if _, err := r.execInPod(ctx, namespace, createdPod.Name, []string{"touch", "/tmp/.scion-home-ready"}); err != nil {
		return createdPod.Name, fmt.Errorf("failed to signal startup gate: %w", err)
	}

	runtimeLog.Info("Agent started successfully", "agent", createdPod.Name, "namespace", namespace, "phase", "complete")
	fmt.Printf("Agent '%s' started successfully.\n", createdPod.Name)
	return createdPod.Name, nil
}

// writeK8sRuntimeDebugFile writes a kubectl-style representation of the pod
// spec to the runtime-exec-debug file for diagnostic purposes.
func writeK8sRuntimeDebugFile(config RunConfig, namespace string, pod *corev1.Pod) {
	if !config.Debug || config.HomeDir == "" {
		return
	}
	agentDir := filepath.Dir(config.HomeDir)
	debugPath := filepath.Join(agentDir, "runtime-exec-debug")

	podJSON, err := json.MarshalIndent(pod, "", "  ")
	if err != nil {
		runtimeLog.Debug("Failed to marshal pod spec for debug file", "error", err)
		return
	}

	content := fmt.Sprintf("# kubectl apply -f - <<'EOF'\n%s\n# EOF\n#\n# Equivalent:\n# kubectl apply -n %s -f <this-file's-json-content>\n", string(podJSON), namespace)

	if err := os.WriteFile(debugPath, []byte(content), 0644); err != nil {
		runtimeLog.Debug("Failed to write runtime debug file", "path", debugPath, "error", err)
	}
}

// createAgentSecret creates a K8s Secret containing all resolved secret values.
// Environment-type secrets are stored as individual keys; variable-type secrets
// are marshaled together as JSON under a "secrets.json" key. File-type secrets
// are stored as individual keys named by secret name.
// Returns the secret name, or empty string if no secrets need to be created.
func (r *KubernetesRuntime) createAgentSecret(ctx context.Context, namespace, agentName string, secrets []api.ResolvedSecret, labels map[string]string) (string, error) {
	if len(secrets) == 0 {
		return "", nil
	}

	secretName := fmt.Sprintf("scion-agent-%s", agentName)
	data := make(map[string][]byte)

	// Collect variable-type secrets for JSON aggregation
	varSecrets := make(map[string]string)

	for _, s := range secrets {
		switch s.Type {
		case "environment":
			data[s.Name] = []byte(s.Value)
		case "file":
			decoded, err := base64.StdEncoding.DecodeString(s.Value)
			if err != nil {
				decoded = []byte(s.Value)
			}
			data[s.Name] = decoded
		case "variable":
			varSecrets[s.Target] = s.Value
		}
	}

	if len(varSecrets) > 0 {
		jsonData, err := json.Marshal(varSecrets)
		if err != nil {
			return "", fmt.Errorf("failed to marshal variable secrets: %w", err)
		}
		data["secrets.json"] = jsonData
	}

	if len(data) == 0 {
		return "", nil
	}

	// Build labels for cleanup
	secretLabels := map[string]string{
		"scion.agent": agentName,
	}
	for k, v := range labels {
		if strings.HasPrefix(k, "scion.") {
			secretLabels[k] = v
		}
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: namespace,
			Labels:    secretLabels,
		},
		Data: data,
	}

	_, err := r.Client.Clientset.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{})
	if k8serrors.IsAlreadyExists(err) {
		// Delete the stale secret and retry
		_ = r.Client.Clientset.CoreV1().Secrets(namespace).Delete(ctx, secretName, metav1.DeleteOptions{})
		_, err = r.Client.Clientset.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{})
	}
	if err != nil {
		return "", fmt.Errorf("failed to create agent secret: %w", err)
	}

	return secretName, nil
}

// createSecretProviderClass creates a SecretProviderClass CRD for GKE
// Secrets Store CSI driver integration. It maps GCP Secret Manager
// references to K8s-synced secrets for environment variable injection.
func (r *KubernetesRuntime) createSecretProviderClass(ctx context.Context, namespace, agentName string, secrets []api.ResolvedSecret, labels map[string]string) (string, error) {
	spcName := fmt.Sprintf("scion-agent-%s", agentName)
	envSecretName := fmt.Sprintf("scion-agent-%s-env", agentName)

	// Build the GCP SM secrets parameter as YAML
	type gcpSecretEntry struct {
		ResourceName string `json:"resourceName"`
		FileName     string `json:"fileName"`
	}
	var gcpSecrets []gcpSecretEntry
	for _, s := range secrets {
		if s.Ref == "" {
			continue
		}
		gcpSecrets = append(gcpSecrets, gcpSecretEntry{
			ResourceName: fmt.Sprintf("%s/versions/latest", s.Ref),
			FileName:     s.Name,
		})
	}

	if len(gcpSecrets) == 0 {
		return "", nil
	}

	secretsParam, err := json.Marshal(gcpSecrets)
	if err != nil {
		return "", fmt.Errorf("failed to marshal secrets parameter: %w", err)
	}

	// Build secretObjects for env-type secrets (synced to a K8s Secret)
	type secretObjectData struct {
		Key        string `json:"key"`
		ObjectName string `json:"objectName"`
	}
	type secretObject struct {
		SecretName string             `json:"secretName"`
		Type       string             `json:"type"`
		Data       []secretObjectData `json:"data"`
	}

	var envData []secretObjectData
	for _, s := range secrets {
		if s.Ref == "" || s.Type != "environment" {
			continue
		}
		envData = append(envData, secretObjectData{
			Key:        s.Name,
			ObjectName: s.Name,
		})
	}

	var secretObjects []secretObject
	if len(envData) > 0 {
		secretObjects = append(secretObjects, secretObject{
			SecretName: envSecretName,
			Type:       "Opaque",
			Data:       envData,
		})
	}

	// Build labels
	spcLabels := map[string]string{
		"scion.agent": agentName,
	}
	for k, v := range labels {
		if strings.HasPrefix(k, "scion.") {
			spcLabels[k] = v
		}
	}

	spec := map[string]interface{}{
		"provider": "gcp",
		"parameters": map[string]interface{}{
			"secrets": string(secretsParam),
		},
	}
	if len(secretObjects) > 0 {
		soJSON, _ := json.Marshal(secretObjects)
		var soSlice []interface{}
		json.Unmarshal(soJSON, &soSlice)
		spec["secretObjects"] = soSlice
	}

	spc := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "secrets-store.csi.x-k8s.io/v1",
			"kind":       "SecretProviderClass",
			"metadata": map[string]interface{}{
				"name":      spcName,
				"namespace": namespace,
				"labels":    toStringInterfaceMap(spcLabels),
			},
			"spec": spec,
		},
	}

	_, err = r.Client.Dynamic().Resource(k8s.SecretProviderClassGVR).Namespace(namespace).Create(ctx, spc, metav1.CreateOptions{})
	if k8serrors.IsAlreadyExists(err) {
		_ = r.Client.Dynamic().Resource(k8s.SecretProviderClassGVR).Namespace(namespace).Delete(ctx, spcName, metav1.DeleteOptions{})
		_, err = r.Client.Dynamic().Resource(k8s.SecretProviderClassGVR).Namespace(namespace).Create(ctx, spc, metav1.CreateOptions{})
	}
	if err != nil {
		return "", fmt.Errorf("failed to create SecretProviderClass: %w", err)
	}

	return spcName, nil
}

func boolPtr(b bool) *bool    { return &b }
func int64Ptr(v int64) *int64 { return &v }

// toStringInterfaceMap converts map[string]string to map[string]interface{} for unstructured objects.
func toStringInterfaceMap(m map[string]string) map[string]interface{} {
	result := make(map[string]interface{}, len(m))
	for k, v := range m {
		result[k] = v
	}
	return result
}

// cleanupAgentSecrets removes K8s Secrets and (if GKE mode) SecretProviderClasses
// associated with an agent, identified by the scion.agent label.
func (r *KubernetesRuntime) cleanupAgentSecrets(ctx context.Context, namespace, agentName string) {
	selector := fmt.Sprintf("scion.agent=%s", agentName)

	// Delete K8s Secrets by listing then deleting individually
	secretList, err := r.Client.Clientset.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err == nil {
		for _, s := range secretList.Items {
			_ = r.Client.Clientset.CoreV1().Secrets(namespace).Delete(ctx, s.Name, metav1.DeleteOptions{})
		}
	}

	// Delete SecretProviderClasses if GKE mode
	if r.GKEMode {
		spcList, err := r.Client.Dynamic().Resource(k8s.SecretProviderClassGVR).Namespace(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: selector,
		})
		if err == nil {
			for _, spc := range spcList.Items {
				_ = r.Client.Dynamic().Resource(k8s.SecretProviderClassGVR).Namespace(namespace).Delete(ctx, spc.GetName(), metav1.DeleteOptions{})
			}
		}
	}
}

// createAuthFileSecret creates a K8s Secret containing ResolvedAuth file contents
// so that auth files can be projected into pods via volume mounts instead of hostPath.
func (r *KubernetesRuntime) createAuthFileSecret(ctx context.Context, namespace, agentName string, files []api.FileMapping, labels map[string]string) error {
	secretName := fmt.Sprintf("scion-auth-%s", agentName)
	data := make(map[string][]byte)

	for i, f := range files {
		content, err := os.ReadFile(f.SourcePath)
		if err != nil {
			return fmt.Errorf("failed to read auth file %s: %w", f.SourcePath, err)
		}
		keyName := fmt.Sprintf("auth-file-%d", i)
		data[keyName] = content
	}

	secretLabels := map[string]string{
		"scion.agent": agentName,
	}
	for k, v := range labels {
		if strings.HasPrefix(k, "scion.") {
			secretLabels[k] = v
		}
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: namespace,
			Labels:    secretLabels,
		},
		Data: data,
	}

	_, err := r.Client.Clientset.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{})
	if k8serrors.IsAlreadyExists(err) {
		_ = r.Client.Clientset.CoreV1().Secrets(namespace).Delete(ctx, secretName, metav1.DeleteOptions{})
		_, err = r.Client.Clientset.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{})
	}
	if err != nil {
		return fmt.Errorf("failed to create auth secret: %w", err)
	}
	return nil
}

// sharedDirPVCName returns the deterministic PVC name for a grove shared directory.
// PVCs are grove-scoped (not agent-scoped), so multiple agents share the same PVC.
func sharedDirPVCName(groveName, dirName string) string {
	return fmt.Sprintf("scion-shared-%s-%s", groveName, dirName)
}

// defaultSharedDirSize is the default PVC size when not specified in settings.
const defaultSharedDirSize = "10Gi"

// createSharedDirPVCs ensures PVCs exist for all declared shared directories.
// PVCs are grove-scoped and persist across agent restarts. If a PVC already
// exists (from a previous agent in the same grove), it is reused.
func (r *KubernetesRuntime) createSharedDirPVCs(ctx context.Context, namespace string, config RunConfig) error {
	if len(config.SharedDirs) == 0 {
		return nil
	}

	groveName := config.Labels["scion.grove"]
	if groveName == "" {
		return fmt.Errorf("cannot create shared dir PVCs: missing scion.grove label")
	}

	storageClass := ""
	size := defaultSharedDirSize
	if config.Kubernetes != nil {
		if config.Kubernetes.SharedDirStorageClass != "" {
			storageClass = config.Kubernetes.SharedDirStorageClass
		}
		if config.Kubernetes.SharedDirSize != "" {
			size = config.Kubernetes.SharedDirSize
		}
	}

	storageQuantity, err := parseResourceSafe(size, "shared_dir_size")
	if err != nil {
		return err
	}

	for _, sd := range config.SharedDirs {
		pvcName := sharedDirPVCName(groveName, sd.Name)

		// Check if PVC already exists (grove-scoped, may have been created by another agent)
		_, err := r.Client.Clientset.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, pvcName, metav1.GetOptions{})
		if err == nil {
			runtimeLog.Info("Shared dir PVC already exists, reusing", "pvc", pvcName, "shared_dir", sd.Name)
			continue
		}

		accessMode := corev1.ReadWriteMany
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pvcName,
				Namespace: namespace,
				Labels: map[string]string{
					"scion.grove":      groveName,
					"scion.shared-dir": sd.Name,
				},
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{accessMode},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceStorage: storageQuantity,
					},
				},
			},
		}

		if storageClass != "" {
			pvc.Spec.StorageClassName = &storageClass
		}

		runtimeLog.Info("Creating shared dir PVC", "pvc", pvcName, "shared_dir", sd.Name, "size", size)
		if _, err := r.Client.Clientset.CoreV1().PersistentVolumeClaims(namespace).Create(ctx, pvc, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("failed to create shared dir PVC %q: %w", pvcName, err)
		}
	}

	return nil
}

// cleanupSharedDirPVCs removes PVCs for shared directories belonging to a grove.
// This is called during grove deletion, not agent deletion, since PVCs are grove-scoped.
func (r *KubernetesRuntime) cleanupSharedDirPVCs(ctx context.Context, namespace, groveName string) {
	selector := fmt.Sprintf("scion.grove=%s,scion.shared-dir", groveName)
	pvcList, err := r.Client.Clientset.CoreV1().PersistentVolumeClaims(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		runtimeLog.Warn("Failed to list shared dir PVCs for cleanup", "grove_id", groveName, "error", err)
		return
	}
	for _, pvc := range pvcList.Items {
		runtimeLog.Info("Deleting shared dir PVC", "pvc", pvc.Name, "grove_id", groveName)
		if err := r.Client.Clientset.CoreV1().PersistentVolumeClaims(namespace).Delete(ctx, pvc.Name, metav1.DeleteOptions{}); err != nil {
			runtimeLog.Warn("Failed to delete shared dir PVC", "pvc", pvc.Name, "error", err)
		}
	}
}

func (r *KubernetesRuntime) buildPod(namespace string, config RunConfig) (*corev1.Pod, error) {
	// Command Resolution
	var cmd []string
	var harnessArgs []string
	if config.Harness != nil {
		harnessArgs = config.Harness.GetCommand(config.Task, config.Resume, config.CommandArgs)
	} else {
		// Fallback if no harness (though RunConfig implies there should be one or defaults)
		harnessArgs = []string{"/bin/sh", "-c", "sleep infinity"}
	}

	var quotedArgs []string
	for _, a := range harnessArgs {
		if strings.ContainsAny(a, " \t\n\"'$") {
			quotedArgs = append(quotedArgs, fmt.Sprintf("%q", a))
		} else {
			quotedArgs = append(quotedArgs, a)
		}
	}
	cmdLine := strings.Join(quotedArgs, " ")
	// Create session with "agent" window running the harness, plus a "shell" window.
	tmuxCmd := fmt.Sprintf(
		"tmux new-session -d -s scion -n agent %s \\; new-window -t scion -n shell \\; select-window -t scion:agent \\; attach-session -t scion",
		cmdLine,
	)
	// --- K8s Startup Gate ---
	//
	// Unlike Docker/Podman where volumes are bind-mounted before the container
	// starts, K8s requires the pod to be running before we can exec into it to
	// sync files (home directory, workspace). This creates a chicken-and-egg
	// problem: the container process (sciontool init → tmux → harness) needs
	// dotfiles like .zshrc, .tmux.conf, and .gemini/settings.json to be
	// present at launch, but we can only copy them into a running container.
	//
	// Solution: the pod command starts with a lightweight gate loop that polls
	// for a marker file (/tmp/.scion-home-ready). The broker syncs home +
	// workspace, fixes ownership, then creates the marker. The gate detects
	// it and exec's the real entrypoint (sciontool init → tmux → harness)
	// with all files already in place.
	//
	// The real startup command is passed via the SCION_START_CMD env var to
	// avoid shell quoting issues with the tmux command string.
	//
	// Flow:
	//   1. Pod starts → gate loop (polling /tmp/.scion-home-ready)
	//   2. Broker syncs home dir → syncs workspace → chowns files
	//   3. Broker creates /tmp/.scion-home-ready via execInPod
	//   4. Gate detects marker → exec sciontool init -- sh -c "$SCION_START_CMD"
	//   5. sciontool init sets up user, drops privileges, launches tmux
	//
	gateCmd := `while [ ! -f /tmp/.scion-home-ready ]; do sleep 0.2; done; exec sciontool init -- sh -c "$SCION_START_CMD"`
	cmd = []string{"sh", "-c", gateCmd}

	// Env Resolution — match local runtimes by including harness env + telemetry env.
	envVars := []corev1.EnvVar{
		// The real startup command, consumed by the gate script above.
		{Name: "SCION_START_CMD", Value: tmuxCmd},
	}

	// Harness env (system prompt, agent name, etc.) — parity with buildCommonRunArgs.
	if config.Harness != nil {
		for k, v := range config.Harness.GetEnv(config.Name, config.HomeDir, config.UnixUsername) {
			if v != "" {
				envVars = append(envVars, corev1.EnvVar{Name: k, Value: v})
			}
		}
		if config.TelemetryEnabled {
			for k, v := range config.Harness.GetTelemetryEnv() {
				envVars = append(envVars, corev1.EnvVar{Name: k, Value: v})
			}
		}
	}

	for _, e := range config.Env {
		// Parse "KEY=VALUE"
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			envVars = append(envVars, corev1.EnvVar{Name: parts[0], Value: parts[1]})
		}
	}

	// Secret mounting: determine strategy and inject secrets
	var extraVolumes []corev1.Volume
	var extraVolumeMounts []corev1.VolumeMount

	if len(config.ResolvedSecrets) > 0 {
		// Check if we should use the GKE CSI path
		useGKEPath := r.GKEMode
		if useGKEPath {
			hasRef := false
			for _, s := range config.ResolvedSecrets {
				if s.Ref != "" {
					hasRef = true
					break
				}
			}
			useGKEPath = hasRef
		}

		agentSecretName := fmt.Sprintf("scion-agent-%s", config.Name)

		if useGKEPath {
			// GKE path: CSI volume for file-type secrets, secretKeyRef to -env secret for env vars
			spcName := fmt.Sprintf("scion-agent-%s", config.Name)
			envSecretName := fmt.Sprintf("scion-agent-%s-env", config.Name)

			// Add CSI volume (required for secretObjects sync)
			extraVolumes = append(extraVolumes, corev1.Volume{
				Name: "secrets-store",
				VolumeSource: corev1.VolumeSource{
					CSI: &corev1.CSIVolumeSource{
						Driver:   "secrets-store.csi.x-k8s.io",
						ReadOnly: boolPtr(true),
						VolumeAttributes: map[string]string{
							"secretProviderClass": spcName,
						},
					},
				},
			})
			extraVolumeMounts = append(extraVolumeMounts, corev1.VolumeMount{
				Name:      "secrets-store",
				MountPath: "/mnt/secrets-store",
				ReadOnly:  true,
			})

			for _, s := range config.ResolvedSecrets {
				switch s.Type {
				case "environment":
					envVars = append(envVars, corev1.EnvVar{
						Name: s.Target,
						ValueFrom: &corev1.EnvVarSource{
							SecretKeyRef: &corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{Name: envSecretName},
								Key:                  s.Name,
							},
						},
					})
				case "file":
					target := expandTildeTarget(s.Target, fmt.Sprintf("/home/%s", config.UnixUsername))
					extraVolumeMounts = append(extraVolumeMounts, corev1.VolumeMount{
						Name:      "secrets-store",
						MountPath: target,
						SubPath:   s.Name,
						ReadOnly:  true,
					})
				}
			}
		} else {
			// Fallback path: K8s Secret with secretKeyRef for env, volume subPath for files
			hasFileSecrets := false
			hasVariableSecrets := false
			for _, s := range config.ResolvedSecrets {
				switch s.Type {
				case "environment":
					envVars = append(envVars, corev1.EnvVar{
						Name: s.Target,
						ValueFrom: &corev1.EnvVarSource{
							SecretKeyRef: &corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{Name: agentSecretName},
								Key:                  s.Name,
							},
						},
					})
				case "file":
					hasFileSecrets = true
				case "variable":
					hasVariableSecrets = true
				}
			}

			if hasFileSecrets || hasVariableSecrets {
				extraVolumes = append(extraVolumes, corev1.Volume{
					Name: "agent-secrets",
					VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{
							SecretName: agentSecretName,
						},
					},
				})
			}

			for _, s := range config.ResolvedSecrets {
				if s.Type == "file" {
					target := expandTildeTarget(s.Target, fmt.Sprintf("/home/%s", config.UnixUsername))
					extraVolumeMounts = append(extraVolumeMounts, corev1.VolumeMount{
						Name:      "agent-secrets",
						MountPath: target,
						SubPath:   s.Name,
						ReadOnly:  true,
					})
				}
			}

			if hasVariableSecrets {
				scionDir := fmt.Sprintf("/home/%s/.scion", config.UnixUsername)
				extraVolumeMounts = append(extraVolumeMounts, corev1.VolumeMount{
					Name:      "agent-secrets",
					MountPath: scionDir + "/secrets.json",
					SubPath:   "secrets.json",
					ReadOnly:  true,
				})
			}
		}
	}

	// ResolvedAuth is always applied when present (composes with ResolvedSecrets).
	// Auth files are injected via a K8s Secret rather than hostPath for portability.
	if config.ResolvedAuth != nil {
		for k, v := range config.ResolvedAuth.EnvVars {
			envVars = append(envVars, corev1.EnvVar{Name: k, Value: v})
		}
		containerHome := fmt.Sprintf("/home/%s", config.UnixUsername)
		if len(config.ResolvedAuth.Files) > 0 {
			volName := "auth-files"
			extraVolumes = append(extraVolumes, corev1.Volume{
				Name: volName,
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: fmt.Sprintf("scion-auth-%s", config.Name),
					},
				},
			})
			for i, f := range config.ResolvedAuth.Files {
				target := expandTildeTarget(f.ContainerPath, containerHome)
				keyName := fmt.Sprintf("auth-file-%d", i)
				extraVolumeMounts = append(extraVolumeMounts, corev1.VolumeMount{
					Name:      volName,
					MountPath: target,
					SubPath:   keyName,
					ReadOnly:  true,
				})
			}
		}
	}

	// Inject GCP telemetry credential path if the well-known secret is present
	if credPath := findGCPTelemetryCredentialPath(config.ResolvedSecrets, fmt.Sprintf("/home/%s", config.UnixUsername)); credPath != "" {
		envVars = append(envVars, corev1.EnvVar{Name: telemetryGCPCredentialsEnvVar, Value: credPath})
	}

	containerHome := fmt.Sprintf("/home/%s", config.UnixUsername)

	// Pass host user UID/GID for container user synchronization
	envVars = append(envVars, corev1.EnvVar{Name: "SCION_HOST_UID", Value: fmt.Sprintf("%d", os.Getuid())})
	envVars = append(envVars, corev1.EnvVar{Name: "SCION_HOST_GID", Value: fmt.Sprintf("%d", os.Getgid())})
	envVars = append(envVars,
		corev1.EnvVar{Name: "HOME", Value: containerHome},
		corev1.EnvVar{Name: "USER", Value: config.UnixUsername},
		corev1.EnvVar{Name: "LOGNAME", Value: config.UnixUsername},
	)

	// Security context: run agent pods as the image's non-root scion user and
	// keep FSGroup aligned with the broker user so synced files remain writable.
	const containerUID int64 = 1000
	hostGID := int64(os.Getgid())
	runAsNonRoot := true
	allowPrivilegeEscalation := false
	podSecurityContext := &corev1.PodSecurityContext{
		FSGroup:      &hostGID,
		RunAsUser:    int64Ptr(containerUID),
		RunAsGroup:   int64Ptr(containerUID),
		RunAsNonRoot: &runAsNonRoot,
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
	}

	// Determine image pull policy
	pullPolicy := corev1.PullIfNotPresent
	if config.Kubernetes != nil && config.Kubernetes.ImagePullPolicy != "" {
		switch config.Kubernetes.ImagePullPolicy {
		case "Always":
			pullPolicy = corev1.PullAlways
		case "Never":
			pullPolicy = corev1.PullNever
		case "IfNotPresent":
			pullPolicy = corev1.PullIfNotPresent
		default:
			return nil, fmt.Errorf("invalid imagePullPolicy %q: must be Always, IfNotPresent, or Never", config.Kubernetes.ImagePullPolicy)
		}
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        config.Name,
			Namespace:   namespace,
			Labels:      config.Labels,
			Annotations: config.Annotations,
		},
		Spec: corev1.PodSpec{
			SecurityContext: podSecurityContext,
			Containers: []corev1.Container{
				{
					Name:            "agent",
					Image:           config.Image,
					Command:         cmd,
					Env:             envVars,
					ImagePullPolicy: pullPolicy,
					WorkingDir:      "/workspace",
					Stdin:           true,
					TTY:             true,
					SecurityContext: &corev1.SecurityContext{
						AllowPrivilegeEscalation: &allowPrivilegeEscalation,
						Capabilities: &corev1.Capabilities{
							Drop: []corev1.Capability{"ALL"},
						},
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "workspace", MountPath: "/workspace"},
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "workspace",
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{},
					},
				},
			},
			RestartPolicy: corev1.RestartPolicyNever,
		},
	}

	// Append secret volumes and mounts
	if len(extraVolumes) > 0 {
		pod.Spec.Volumes = append(pod.Spec.Volumes, extraVolumes...)
	}
	if len(extraVolumeMounts) > 0 {
		pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts, extraVolumeMounts...)
	}

	// Apply resource requests/limits from the common resource spec with safe parsing.
	// When no resources are specified, apply defaults so that GKE Autopilot
	// (and other environments) get predictable scheduling behavior.
	if config.Resources == nil {
		config.Resources = &api.ResourceSpec{
			Requests: api.ResourceList{CPU: "250m", Memory: "512Mi"},
			Limits:   api.ResourceList{CPU: "2", Memory: "4Gi"},
			Disk:     "10Gi",
		}
	}
	if config.Resources != nil {
		reqs := corev1.ResourceList{}
		limits := corev1.ResourceList{}
		if config.Resources.Requests.CPU != "" {
			q, err := parseResourceSafe(config.Resources.Requests.CPU, "requests.cpu")
			if err != nil {
				return nil, err
			}
			reqs[corev1.ResourceCPU] = q
		}
		if config.Resources.Requests.Memory != "" {
			q, err := parseResourceSafe(config.Resources.Requests.Memory, "requests.memory")
			if err != nil {
				return nil, err
			}
			reqs[corev1.ResourceMemory] = q
		}
		if config.Resources.Limits.CPU != "" {
			q, err := parseResourceSafe(config.Resources.Limits.CPU, "limits.cpu")
			if err != nil {
				return nil, err
			}
			limits[corev1.ResourceCPU] = q
		}
		if config.Resources.Limits.Memory != "" {
			q, err := parseResourceSafe(config.Resources.Limits.Memory, "limits.memory")
			if err != nil {
				return nil, err
			}
			limits[corev1.ResourceMemory] = q
		}
		if config.Resources.Disk != "" {
			q, err := parseResourceSafe(config.Resources.Disk, "disk (ephemeral-storage)")
			if err != nil {
				return nil, err
			}
			reqs[corev1.ResourceEphemeralStorage] = q
			limits[corev1.ResourceEphemeralStorage] = q
		}
		if len(reqs) > 0 || len(limits) > 0 {
			pod.Spec.Containers[0].Resources = corev1.ResourceRequirements{
				Requests: reqs,
				Limits:   limits,
			}
		}
	}

	// Merge Kubernetes-specific resources on top (supports extended resources like GPUs).
	if config.Kubernetes != nil && config.Kubernetes.Resources != nil {
		res := &pod.Spec.Containers[0].Resources
		if res.Requests == nil {
			res.Requests = corev1.ResourceList{}
		}
		if res.Limits == nil {
			res.Limits = corev1.ResourceList{}
		}
		for k, v := range config.Kubernetes.Resources.Requests {
			q, err := parseResourceSafe(v, fmt.Sprintf("kubernetes.resources.requests.%s", k))
			if err != nil {
				return nil, err
			}
			res.Requests[corev1.ResourceName(k)] = q
		}
		for k, v := range config.Kubernetes.Resources.Limits {
			q, err := parseResourceSafe(v, fmt.Sprintf("kubernetes.resources.limits.%s", k))
			if err != nil {
				return nil, err
			}
			res.Limits[corev1.ResourceName(k)] = q
		}
	}

	// Process shared directories — create PVC-backed volumes and mounts.
	// Build a set of shared dir targets so we can skip them in the regular volume loop.
	k8sContainerWorkspace := config.ContainerWorkspace
	if k8sContainerWorkspace == "" {
		k8sContainerWorkspace = "/workspace"
	}
	sharedDirTargets := make(map[string]bool, len(config.SharedDirs))
	for i, sd := range config.SharedDirs {
		target := fmt.Sprintf("/scion-volumes/%s", sd.Name)
		if sd.InWorkspace {
			target = fmt.Sprintf("%s/.scion-volumes/%s", k8sContainerWorkspace, sd.Name)
		}
		sharedDirTargets[target] = true

		groveName := config.Labels["scion.grove"]
		pvcName := sharedDirPVCName(groveName, sd.Name)
		volName := fmt.Sprintf("shared-dir-%d", i)

		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
			Name: volName,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: pvcName,
					ReadOnly:  sd.ReadOnly,
				},
			},
		})
		pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{
			Name:      volName,
			MountPath: target,
			ReadOnly:  sd.ReadOnly,
		})
	}

	// Process Volumes
	type gcsVolInfo struct {
		Source string `json:"source"`
		Target string `json:"target"`
		Bucket string `json:"bucket"`
		Prefix string `json:"prefix"`
	}
	var gcsVolumes []gcsVolInfo

	for i, v := range config.Volumes {
		switch v.Type {
		case "gcs":
			volName := fmt.Sprintf("gcs-vol-%d", i)
			attrs := map[string]string{
				"bucketName": v.Bucket,
			}
			if v.Mode != "" {
				attrs["mountOptions"] = v.Mode
			} else {
				attrs["mountOptions"] = "implicit-dirs"
			}

			pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
				Name: volName,
				VolumeSource: corev1.VolumeSource{
					CSI: &corev1.CSIVolumeSource{
						Driver:           "gcsfuse.csi.storage.gke.io",
						VolumeAttributes: attrs,
					},
				},
			})
			pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{
				Name:      volName,
				MountPath: v.Target,
				ReadOnly:  v.ReadOnly,
			})

			pod.Annotations = ensureAnnotations(pod.Annotations)
			pod.Annotations["gke-gcsfuse/volumes"] = "true"

			gcsVolumes = append(gcsVolumes, gcsVolInfo{
				Source: v.Source,
				Target: v.Target,
				Bucket: v.Bucket,
				Prefix: v.Prefix,
			})
		default:
			// Skip shared dir volumes — they are handled via PVCs above.
			if sharedDirTargets[v.Target] {
				continue
			}
			// Local/bind-mount volumes are not supported on Kubernetes.
			// Log explicitly rather than silently ignoring.
			volType := v.Type
			if volType == "" {
				volType = "local"
			}
			runtimeLog.Warn("Volume type not supported on Kubernetes runtime, skipping",
				"type", volType, "source", v.Source, "target", v.Target)
		}
	}

	if len(gcsVolumes) > 0 {
		if data, err := json.Marshal(gcsVolumes); err == nil {
			encoded := base64.StdEncoding.EncodeToString(data)
			pod.Annotations = ensureAnnotations(pod.Annotations)
			pod.Annotations["scion.gcs_volumes"] = encoded
		}
	}

	if config.Kubernetes != nil {
		if config.Kubernetes.ServiceAccountName != "" {
			pod.Spec.ServiceAccountName = config.Kubernetes.ServiceAccountName
		}
		if config.Kubernetes.RuntimeClassName != "" {
			pod.Spec.RuntimeClassName = &config.Kubernetes.RuntimeClassName
		}
		if len(config.Kubernetes.NodeSelector) > 0 {
			pod.Spec.NodeSelector = config.Kubernetes.NodeSelector
		}
		if len(config.Kubernetes.Tolerations) > 0 {
			for _, t := range config.Kubernetes.Tolerations {
				pod.Spec.Tolerations = append(pod.Spec.Tolerations, corev1.Toleration{
					Key:      t.Key,
					Operator: corev1.TolerationOperator(t.Operator),
					Value:    t.Value,
					Effect:   corev1.TaintEffect(t.Effect),
				})
			}
		}
	}

	return pod, nil
}

func (r *KubernetesRuntime) waitForPodReady(ctx context.Context, namespace, podName string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute) // GKE Autopilot can be slow
	defer cancel()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	lastStatus := ""
	autopilotWaitLogged := false

	fmt.Printf("Waiting for pod '%s' to be ready...\n", podName)
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for pod to be ready: %w", ctx.Err())
		case <-ticker.C:
			pod, err := r.Client.Clientset.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
			if err != nil {
				return err
			}

			// Check container statuses for more detail
			var containerStatus *corev1.ContainerStatus
			for _, cs := range pod.Status.ContainerStatuses {
				if cs.Name == "agent" {
					containerStatus = &cs
					break
				}
			}

			statusMsg := string(pod.Status.Phase)
			if containerStatus != nil && containerStatus.State.Waiting != nil {
				statusMsg = fmt.Sprintf("%s (%s)", pod.Status.Phase, containerStatus.State.Waiting.Reason)
			}

			if statusMsg != lastStatus {
				fmt.Printf("  Status: %s\n", statusMsg)
				lastStatus = statusMsg
			}

			// Check for terminal failure reasons in waiting state
			if containerStatus != nil && containerStatus.State.Waiting != nil {
				reason := containerStatus.State.Waiting.Reason
				message := containerStatus.State.Waiting.Message
				switch reason {
				case "ImagePullBackOff", "ErrImagePull":
					runtimeLog.Error("Image pull failed", "pod", podName, "reason", reason, "message", message, "phase", "image-pull")
					return fmt.Errorf("image pull failed for pod %q: %s — verify the image name and registry access (image pull policy: check kubernetes.imagePullPolicy)", podName, message)
				case "InvalidImageName":
					runtimeLog.Error("Invalid image name", "pod", podName, "message", message, "phase", "image-pull")
					return fmt.Errorf("invalid image name for pod %q: %s", podName, message)
				case "CreateContainerConfigError":
					runtimeLog.Error("Container config error", "pod", podName, "message", message, "phase", "container-config")
					return fmt.Errorf("container configuration error for pod %q: %s — check secret references and volume mounts", podName, message)
				case "CrashLoopBackOff":
					runtimeLog.Error("Container crash loop", "pod", podName, "message", message, "phase", "crash-loop")
					return fmt.Errorf("container is crash-looping in pod %q: %s — check container logs with 'scion logs'", podName, message)
				case "Unschedulable":
					if r.isGKEScheduling() {
						runtimeLog.Info("Pod unschedulable (GKE Autopilot will auto-provision nodes)", "pod", podName, "message", message, "phase", "scheduling")
					} else {
						runtimeLog.Error("Pod unschedulable", "pod", podName, "message", message, "phase", "scheduling")
						return fmt.Errorf("pod %q cannot be scheduled: %s — check node selectors, tolerations, and resource availability", podName, message)
					}
				}
			}

			// Check pod-level conditions for scheduling failures
			for _, cond := range pod.Status.Conditions {
				if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse && cond.Reason == "Unschedulable" {
					if r.isGKEScheduling() {
						// On GKE Autopilot, Unschedulable is transient — the cluster
						// will auto-provision nodes. Continue waiting instead of failing.
						if !autopilotWaitLogged {
							runtimeLog.Info("Pod unschedulable (GKE Autopilot will auto-provision nodes)", "pod", podName, "message", cond.Message, "phase", "scheduling")
							fmt.Printf("  Waiting for GKE Autopilot to provision node capacity...\n")
							autopilotWaitLogged = true
						}
					} else {
						runtimeLog.Error("Pod unschedulable", "pod", podName, "message", cond.Message, "phase", "scheduling")
						return fmt.Errorf("pod %q cannot be scheduled: %s — check node selectors, tolerations, and resource availability", podName, cond.Message)
					}
				}
			}

			if pod.Status.Phase == corev1.PodRunning {
				// Also ensure container is actually running
				if containerStatus != nil && containerStatus.State.Running != nil {
					return nil
				}
			}
			if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
				if containerStatus != nil && containerStatus.State.Terminated != nil {
					return fmt.Errorf("pod failed to start: %s - %s", containerStatus.State.Terminated.Reason, containerStatus.State.Terminated.Message)
				}
				return fmt.Errorf("pod terminated with status: %s", pod.Status.Phase)
			}
		}
	}
}

func (r *KubernetesRuntime) syncToPod(ctx context.Context, namespace, podName, sourcePath, destPath string) error {
	fmt.Printf("  Preparing tar archive from %s...\n", sourcePath)
	tarCmd := exec.CommandContext(ctx, "tar", "-cz", "-C", sourcePath, ".")
	tarCmd.Env = append(os.Environ(), "COPYFILE_DISABLE=1")
	stdout, err := tarCmd.StdoutPipe()
	if err != nil {
		return err
	}

	if err := tarCmd.Start(); err != nil {
		return err
	}

	// Use sh -c to allow us to ignore certain exit codes if needed, or just to be more flexible.
	// We use -m to avoid utime errors on the mount point.
	remoteCmd := fmt.Sprintf("tar -xz -m --no-same-owner --no-same-permissions -C '%s'", destPath)
	cmd := []string{"sh", "-c", remoteCmd}

	req := r.Client.Clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec")

	option := &corev1.PodExecOptions{
		Command: cmd,
		Stdin:   true,
		Stdout:  true,
		Stderr:  true,
		TTY:     false,
	}

	req.VersionedParams(
		option,
		scheme.ParameterCodec,
	)

	executor, err := remotecommand.NewSPDYExecutor(r.Client.Config, "POST", req.URL())
	if err != nil {
		return err
	}

	fmt.Printf("  Streaming archive to pod '%s' (destination: %s)...\n", podName, destPath)
	var stderr bytes.Buffer
	// We stream to os.Stdout to see if there is any output from tar that helps debugging
	err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  stdout,
		Stdout: os.Stdout,
		Stderr: &stderr,
	})

	waitErr := tarCmd.Wait()

	if err != nil {
		// If tar exited with an error, it might be the permission error on .
		// which we want to ignore if the files were actually copied.
		// GNU tar exits with 2 for "fatal errors", which includes the permission error on .
		if strings.Contains(stderr.String(), "Cannot change mode") || strings.Contains(stderr.String(), "Cannot utime") {
			fmt.Printf("  Warning: tar reported permission issues on workspace root, but files may have been synced.\n")
		} else {
			return fmt.Errorf("stream failed: %w (remote stderr: %s)", err, stderr.String())
		}
	}

	if waitErr != nil {
		return fmt.Errorf("local tar failed: %w", waitErr)
	}

	fmt.Printf("  Sync to %s complete.\n", destPath)
	return nil
}

func (r *KubernetesRuntime) syncFromPod(ctx context.Context, namespace, podName, remotePath, localPath string) error {
	if err := os.MkdirAll(localPath, 0755); err != nil {
		return fmt.Errorf("failed to create local workspace directory: %w", err)
	}
	fmt.Printf("  Preparing remote tar archive from %s...\n", remotePath)

	remoteCmd := fmt.Sprintf("tar -cz -C '%s' .", remotePath)
	cmd := []string{"sh", "-c", remoteCmd}

	req := r.Client.Clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec")

	option := &corev1.PodExecOptions{
		Command: cmd,
		Stdin:   false,
		Stdout:  true,
		Stderr:  true,
		TTY:     false,
	}

	req.VersionedParams(
		option,
		scheme.ParameterCodec,
	)

	executor, err := remotecommand.NewSPDYExecutor(r.Client.Config, "POST", req.URL())
	if err != nil {
		return err
	}

	// Prepare local tar
	tarCmd := exec.CommandContext(ctx, "tar", "-xz", "-m", "-C", localPath)
	tarCmd.Env = append(os.Environ(), "COPYFILE_DISABLE=1")
	stdin, err := tarCmd.StdinPipe()
	if err != nil {
		return err
	}

	if err := tarCmd.Start(); err != nil {
		return err
	}

	fmt.Printf("  Streaming archive from pod '%s' (destination: %s)...\n", podName, localPath)
	var stderr bytes.Buffer
	err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: stdin,
		Stderr: &stderr,
	})

	// Close stdin to tell local tar that stream is finished
	stdin.Close()
	waitErr := tarCmd.Wait()

	if err != nil {
		return fmt.Errorf("stream failed: %w (remote stderr: %s)", err, stderr.String())
	}

	if waitErr != nil {
		return fmt.Errorf("local tar failed: %w", waitErr)
	}

	fmt.Printf("  Sync from %s complete.\n", remotePath)
	return nil
}

func (r *KubernetesRuntime) Stop(ctx context.Context, id string) error {
	return r.Delete(ctx, id)
}

func (r *KubernetesRuntime) Delete(ctx context.Context, id string) error {
	namespace := r.DefaultNamespace

	// Support namespace/pod format
	if strings.Contains(id, "/") {
		parts := strings.SplitN(id, "/", 2)
		namespace = parts[0]
		id = parts[1]
	} else {
		namespace = r.resolveNamespace(ctx, id)
	}

	// Clean up agent secrets and SecretProviderClasses before deleting the pod
	r.cleanupAgentSecrets(ctx, namespace, id)

	// 'id' is the pod name
	// Use GracePeriodSeconds=0 for immediate termination since Delete is used
	// for force-removal (e.g. scion rm), not graceful shutdown.
	gracePeriod := int64(0)
	err := r.Client.Clientset.CoreV1().Pods(namespace).Delete(ctx, id, metav1.DeleteOptions{
		GracePeriodSeconds: &gracePeriod,
	})
	if err != nil && !k8serrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete pod: %w", err)
	}
	return nil
}

// cleanupStalePod deletes an existing pod with the given name if it exists.
// This prevents "already exists" errors when recreating an agent.
func (r *KubernetesRuntime) cleanupStalePod(ctx context.Context, namespace, podName string) {
	gracePeriod := int64(0)
	err := r.Client.Clientset.CoreV1().Pods(namespace).Delete(ctx, podName, metav1.DeleteOptions{
		GracePeriodSeconds: &gracePeriod,
	})
	if err != nil && !k8serrors.IsNotFound(err) {
		runtimeLog.Debug("Failed to clean up stale pod", "pod", podName, "namespace", namespace, "error", err)
	}
}

func (r *KubernetesRuntime) List(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
	namespace := r.DefaultNamespace
	// When ListAllNamespaces is enabled, query across all namespaces
	if r.ListAllNamespaces {
		namespace = ""
	}

	var selector string
	if len(labelFilter) > 0 {
		var selectors []string
		for k, v := range labelFilter {
			selectors = append(selectors, fmt.Sprintf("%s=%s", k, v))
		}
		selector = strings.Join(selectors, ",")
	} else {
		// Default to filtering for scion agents if no specific filter is provided
		selector = "scion.name"
	}

	pods, err := r.Client.Clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return nil, err
	}

	var agents []api.AgentInfo
	for _, p := range pods.Items {
		// We already filtered by selector, but we still double check if scion.name is present
		// just in case the selector logic changes or is broader.
		if _, ok := p.Labels["scion.name"]; !ok {
			continue
		}

		status := string(p.Status.Phase)
		agentStatus := ""
		switch p.Status.Phase {
		case corev1.PodSucceeded:
			agentStatus = string(state.PhaseStopped)
		case corev1.PodFailed:
			agentStatus = string(state.PhaseError)
		case corev1.PodPending, corev1.PodRunning, corev1.PodUnknown:
			// Non-terminal pod phases are represented via ContainerStatus and
			// local agent-info state until the agent exits.
		}

		// Try to get more detail from container status
		for _, cs := range p.Status.ContainerStatuses {
			if cs.Name == "agent" {
				if cs.State.Waiting != nil {
					status = fmt.Sprintf("%s (%s)", p.Status.Phase, cs.State.Waiting.Reason)
				} else if cs.State.Terminated != nil {
					status = fmt.Sprintf("%s (%s)", p.Status.Phase, cs.State.Terminated.Reason)
					if agentStatus == "" {
						if cs.State.Terminated.ExitCode == 0 {
							agentStatus = string(state.PhaseStopped)
						} else {
							agentStatus = string(state.PhaseError)
						}
					}
				}
				break
			}
		}

		grovePath := p.Annotations["scion.grove_path"]
		if grovePath == "" {
			grovePath = p.Labels["scion.grove_path"]
		}

		agents = append(agents, api.AgentInfo{
			ContainerID:     p.Name, // Pod name serves as the container identifier
			Name:            p.Labels["scion.name"],
			Template:        p.Labels["scion.template"],
			Grove:           p.Labels["scion.grove"],
			GroveID:         p.Labels["scion.grove_id"],
			GrovePath:       grovePath,
			Labels:          p.Labels,
			Annotations:     p.Annotations,
			ContainerStatus: status,
			Phase:           agentStatus,
			Image:           p.Spec.Containers[0].Image,
			Runtime:         r.Name(),
			Kubernetes: &api.AgentK8sMetadata{
				Namespace: p.Namespace,
				PodName:   p.Name,
			},
		})
	}
	return agents, nil
}

func (r *KubernetesRuntime) GetLogs(ctx context.Context, id string) (string, error) {
	namespace := r.DefaultNamespace
	podName := id

	if strings.Contains(id, "/") {
		parts := strings.SplitN(id, "/", 2)
		namespace = parts[0]
		podName = parts[1]
	} else {
		namespace = r.resolveNamespace(ctx, podName)
	}

	req := r.Client.Clientset.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{})
	podLogs, err := req.Stream(ctx)
	if err != nil {
		return "", err
	}
	defer podLogs.Close()

	data, err := io.ReadAll(podLogs)
	if err != nil {
		return "", err
	}

	return string(data), nil
}

func (r *KubernetesRuntime) Attach(ctx context.Context, id string) error {
	podName := id
	namespace := r.DefaultNamespace

	if strings.Contains(id, "/") {
		parts := strings.SplitN(id, "/", 2)
		namespace = parts[0]
		podName = parts[1]
	} else {
		namespace = r.resolveNamespace(ctx, podName)
	}

	// Find pod first to check status
	agents, err := r.List(ctx, map[string]string{"scion.name": id})
	if err != nil {
		return fmt.Errorf("failed to list pods: %w", err)
	}

	var agent *api.AgentInfo
	for _, a := range agents {
		if a.ContainerID == id || a.Name == id {
			agent = &a
			break
		}
	}

	if agent == nil {
		return fmt.Errorf("agent '%s' pod not found. It may have been deleted.", id)
	}

	// For Kubernetes, we want to ensure it is in Running phase
	if !strings.EqualFold(agent.ContainerStatus, string(corev1.PodRunning)) {
		return fmt.Errorf("agent '%s' is not running (status: %s). Use 'scion start %s' to resume it.", id, agent.ContainerStatus, id)
	}

	fmt.Printf("Attaching to pod '%s' (use Ctrl-b d to detach)...\n", podName)

	req := r.Client.Clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec")

	execCmd, err := r.commandForExec(ctx, namespace, podName, []string{"tmux", "attach", "-t", "scion"})
	if err != nil {
		return err
	}

	option := &corev1.PodExecOptions{
		Container: "agent",
		Command:   execCmd,
		Stdin:     true,
		Stdout:    true,
		Stderr:    true,
		TTY:       true,
	}
	req.VersionedParams(option, scheme.ParameterCodec)
	realStdin := os.Stdin

	executor, err := remotecommand.NewSPDYExecutor(r.Client.Config, "POST", req.URL())
	if err != nil {
		return err
	}

	// Put the terminal into raw mode to support TUI interactions
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		oldState, err := term.MakeRaw(fd)
		if err != nil {
			return fmt.Errorf("failed to set raw mode: %w", err)
		}
		defer term.Restore(fd, oldState)
	}

	// Create a context that can be canceled by our detach sequence
	attachCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Setup terminal resizing support
	sizeQueue := &terminalSizeQueue{
		resizeChan: make(chan remotecommand.TerminalSize, 1),
	}

	// Initial size
	if w, h, err := term.GetSize(fd); err == nil {
		sizeQueue.resizeChan <- remotecommand.TerminalSize{Width: uint16(w), Height: uint16(h)}
	}

	// Monitor for resize signals (SIGWINCH)
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGWINCH)
	go func() {
		for {
			select {
			case <-sigChan:
				if w, h, err := term.GetSize(fd); err == nil {
					sizeQueue.resizeChan <- remotecommand.TerminalSize{Width: uint16(w), Height: uint16(h)}
				}
			case <-attachCtx.Done():
				return
			}
		}
	}()
	defer signal.Stop(sigChan)

	// Trigger a "resize dance" to force TUI redraw. Some TUIs only redraw
	// when they receive a SIGWINCH where the dimensions actually change.
	go func() {
		// Wait for the SPDY stream to be fully established
		time.Sleep(500 * time.Millisecond)
		if w, h, err := term.GetSize(fd); err == nil {
			// 1. Send slightly modified size
			sizeQueue.resizeChan <- remotecommand.TerminalSize{Width: uint16(w - 1), Height: uint16(h)}
			time.Sleep(100 * time.Millisecond)
			// 2. Restore original size
			sizeQueue.resizeChan <- remotecommand.TerminalSize{Width: uint16(w), Height: uint16(h)}
		}
	}()

	err = executor.StreamWithContext(attachCtx, remotecommand.StreamOptions{
		Stdin:             realStdin,
		Stdout:            os.Stdout,
		Stderr:            os.Stderr,
		Tty:               true,
		TerminalSizeQueue: sizeQueue,
	})

	if err != nil {
		// Suppress "context canceled" error when it's the result of a deliberate detach
		if errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "context canceled") {
			return nil
		}
		// Also ignore EOF which can happen on clean detach
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	return nil
}

// terminalSizeQueue implements remotecommand.TerminalSizeQueue
type terminalSizeQueue struct {
	resizeChan chan remotecommand.TerminalSize
}

func (t *terminalSizeQueue) Next() *remotecommand.TerminalSize {
	size, ok := <-t.resizeChan
	if !ok {
		return nil
	}
	return &size
}

func (r *KubernetesRuntime) ImageExists(ctx context.Context, image string) (bool, error) {
	// Validate image format before accepting it
	if image == "" {
		return false, fmt.Errorf("image name is empty")
	}
	if strings.ContainsAny(image, " \t\n") {
		return false, fmt.Errorf("image name %q contains whitespace", image)
	}
	// K8s pulls images if not present, so we can assume it "exists" or will be pulled.
	// Pull failures are caught during waitForPodReady with detailed error messages.
	return true, nil
}

func (r *KubernetesRuntime) PullImage(ctx context.Context, image string) error {
	// Not strictly needed as Pod creation handles pulling.
	return nil
}

func (r *KubernetesRuntime) Sync(ctx context.Context, id string, direction SyncDirection) error {
	// Find pod first
	agents, err := r.List(ctx, map[string]string{"scion.name": id})
	if err != nil {
		return fmt.Errorf("failed to list pods: %w", err)
	}

	var agent *api.AgentInfo
	for _, a := range agents {
		if a.ContainerID == id || a.Name == id {
			agent = &a
			break
		}
	}

	if agent == nil {
		return fmt.Errorf("agent '%s' pod not found", id)
	}

	// Check for GCS volumes
	if val, ok := agent.Annotations["scion.gcs_volumes"]; ok && val != "" {
		decoded, err := base64.StdEncoding.DecodeString(val)
		if err != nil {
			return fmt.Errorf("failed to decode gcs volume info: %w", err)
		}

		type gcsVolInfo struct {
			Source string `json:"source"`
			Target string `json:"target"`
			Bucket string `json:"bucket"`
			Prefix string `json:"prefix"`
		}
		var vols []gcsVolInfo
		if err := json.Unmarshal(decoded, &vols); err != nil {
			return fmt.Errorf("failed to parse gcs volume info: %w", err)
		}

		for _, v := range vols {
			if v.Source == "" {
				continue
			}
			if direction == SyncTo {
				if err := gcp.SyncToGCS(ctx, v.Source, v.Bucket, v.Prefix); err != nil {
					return fmt.Errorf("failed to sync to GCS: %w", err)
				}
			} else if direction == SyncFrom {
				if err := gcp.SyncFromGCS(ctx, v.Bucket, v.Prefix, v.Source); err != nil {
					return fmt.Errorf("failed to sync from GCS: %w", err)
				}
			} else {
				return fmt.Errorf("sync direction must be specified for GCS volumes")
			}
		}
		return nil
	}

	workspacePath := agent.Annotations["scion.workspace"]
	if workspacePath == "" {
		return fmt.Errorf("agent '%s' does not have a workspace path recorded", id)
	}

	homeDir := agent.Annotations["scion.homedir"]
	username := agent.Annotations["scion.username"]

	// Resolve namespace
	namespace := r.DefaultNamespace
	if ns, ok := agent.Labels["scion.namespace"]; ok {
		namespace = ns
	} else if ns, ok := agent.Labels["namespace"]; ok {
		namespace = ns
	}

	// Tar sync (Snapshot)
	if direction == SyncUnspecified {
		return fmt.Errorf("direction (to or from) must be specified for tar sync. Example: scion sync to %s", agent.ContainerID)
	}

	if direction == SyncFrom {
		fmt.Printf("Syncing workspace (agent -> %s)...\n", workspacePath)
		if err := r.syncWithRetry(ctx, func() error {
			return r.syncFromPod(ctx, namespace, agent.ContainerID, "/workspace", workspacePath)
		}); err != nil {
			return err
		}
		if homeDir != "" && username != "" {
			destHome := fmt.Sprintf("/home/%s", username)
			fmt.Printf("Syncing agent home (agent -> %s)...\n", homeDir)
			if err := r.syncWithRetry(ctx, func() error {
				return r.syncFromPod(ctx, namespace, agent.ContainerID, destHome, homeDir)
			}); err != nil {
				return err
			}
		}
		return nil
	}

	fmt.Printf("Syncing workspace (%s -> agent)...\n", workspacePath)
	if err := r.syncWithRetry(ctx, func() error {
		return r.syncToPod(ctx, namespace, agent.ContainerID, workspacePath, "/workspace")
	}); err != nil {
		return err
	}
	if homeDir != "" && username != "" {
		destHome := fmt.Sprintf("/home/%s", username)
		fmt.Printf("Syncing agent home (%s -> agent)...\n", homeDir)
		if err := r.syncWithRetry(ctx, func() error {
			return r.syncToPod(ctx, namespace, agent.ContainerID, homeDir, destHome)
		}); err != nil {
			return err
		}
	}
	return nil
}

func (r *KubernetesRuntime) Exec(ctx context.Context, id string, cmd []string) (string, error) {
	namespace := r.DefaultNamespace
	podName := id

	if strings.Contains(id, "/") {
		parts := strings.SplitN(id, "/", 2)
		namespace = parts[0]
		podName = parts[1]
	} else {
		namespace = r.resolveNamespace(ctx, podName)
	}

	req := r.Client.Clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec")

	execCmd, err := r.commandForExec(ctx, namespace, podName, cmd)
	if err != nil {
		return "", err
	}

	option := &corev1.PodExecOptions{
		Container: "agent",
		Command:   execCmd,
		Stdin:     false,
		Stdout:    true,
		Stderr:    true,
		TTY:       false,
	}

	req.VersionedParams(
		option,
		scheme.ParameterCodec,
	)

	executor, err := remotecommand.NewSPDYExecutor(r.Client.Config, "POST", req.URL())
	if err != nil {
		return "", err
	}

	var stdout, stderr bytes.Buffer
	err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	})

	if err != nil {
		return stdout.String(), fmt.Errorf("exec failed: %w (stderr: %s)", err, stderr.String())
	}

	return stdout.String(), nil
}

// execInPod runs a command in the pod's "agent" container as root (the default
// K8s exec user). This is used for administrative tasks like chown after syncing files.
func (r *KubernetesRuntime) execInPod(ctx context.Context, namespace, podName string, cmd []string) (string, error) {
	// Guard against fake/test clientsets where Config is nil (no real API server).
	if r.Client.Config == nil {
		return "", fmt.Errorf("K8s REST config not available (test environment)")
	}
	req := r.Client.Clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec")

	option := &corev1.PodExecOptions{
		Container: "agent",
		Command:   cmd,
		Stdin:     false,
		Stdout:    true,
		Stderr:    true,
		TTY:       false,
	}
	req.VersionedParams(option, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(r.Client.Config, "POST", req.URL())
	if err != nil {
		return "", err
	}

	var stdout, stderr bytes.Buffer
	err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		return stdout.String(), fmt.Errorf("exec failed: %w (stderr: %s)", err, stderr.String())
	}
	return stdout.String(), nil
}

// GetWorkspacePath returns the local workspace path for a Kubernetes pod.
// For K8s, this returns the workspace path stored in annotations when the pod was created.
func (r *KubernetesRuntime) GetWorkspacePath(ctx context.Context, id string) (string, error) {
	namespace := r.DefaultNamespace

	// Parse namespace from id if present (format: namespace/podname)
	if strings.Contains(id, "/") {
		parts := strings.SplitN(id, "/", 2)
		namespace = parts[0]
		id = parts[1]
	} else {
		namespace = r.resolveNamespace(ctx, id)
	}

	pod, err := r.Client.Clientset.CoreV1().Pods(namespace).Get(ctx, id, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to get pod: %w", err)
	}

	// Check annotations for workspace path
	if workspace, ok := pod.Annotations["scion.workspace"]; ok && workspace != "" {
		return workspace, nil
	}

	return "", fmt.Errorf("no workspace path found for pod %s", id)
}
