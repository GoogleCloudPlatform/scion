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
	"strings"
	"syscall"
	"time"

	"github.com/ptone/scion-agent/pkg/api"
	"github.com/ptone/scion-agent/pkg/gcp"
	"github.com/ptone/scion-agent/pkg/k8s"
	"github.com/ptone/scion-agent/pkg/mutagen"
	"golang.org/x/term"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
)

type KubernetesRuntime struct {
	Client           *k8s.Client
	DefaultNamespace string
	SyncMode         string
	GKEMode          bool
}

func NewKubernetesRuntime(client *k8s.Client) *KubernetesRuntime {
	return &KubernetesRuntime{
		Client:           client,
		DefaultNamespace: "default",
		SyncMode:         "tar", // Default
	}
}

func (r *KubernetesRuntime) Name() string {
	return "kubernetes"
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
		if config.Annotations == nil {
			config.Annotations = make(map[string]string)
		}
		config.Annotations["scion.workspace"] = config.Workspace
	}

	if config.GitClone != nil {
		if config.Annotations == nil {
			config.Annotations = make(map[string]string)
		}
		config.Annotations["scion.git_clone"] = "true"
		config.Annotations["scion.git_clone_url"] = config.GitClone.URL
	}

	if config.HomeDir != "" {
		if config.Annotations == nil {
			config.Annotations = make(map[string]string)
		}
		config.Annotations["scion.homedir"] = config.HomeDir
		config.Annotations["scion.username"] = config.UnixUsername
	}

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
				return "", fmt.Errorf("failed to create agent secret: %w", err)
			}
		}
	}

	pod := r.buildPod(namespace, config)

	fmt.Printf("  Provisioning pod '%s' in namespace '%s'...\n", config.Name, namespace)
	createdPod, err := r.Client.Clientset.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		// Clean up orphaned secrets on pod creation failure
		r.cleanupAgentSecrets(ctx, namespace, config.Name)
		return "", fmt.Errorf("failed to create pod: %w", err)
	}

	// Wait for Ready
	if err := r.waitForPodReady(ctx, namespace, createdPod.Name); err != nil {
		return createdPod.Name, err
	}

	if config.HomeDir != "" {
		destHome := fmt.Sprintf("/home/%s", config.UnixUsername)
		fmt.Printf("  Syncing agent home (%s -> %s)...\n", config.HomeDir, destHome)
		err = r.syncToPod(ctx, namespace, createdPod.Name, config.HomeDir, destHome)
		if err != nil {
			return createdPod.Name, fmt.Errorf("failed to sync home: %w", err)
		}
	}

	if config.Workspace != "" {
		useMutagen := false
		if r.SyncMode == "mutagen" {
			if mutagen.CheckInstalled() {
				fmt.Println("  Initializing live sync session (Mutagen)...")
				if err := mutagen.StartDaemon(); err != nil {
					fmt.Printf("  Warning: failed to start mutagen daemon: %s. Falling back to snapshot sync.\n", err)
				} else {
					// Construct the Mutagen Kubernetes URL.
					// Format: kubernetes://<context>/<namespace>/<pod>/<container>:<path>
					remoteURL := fmt.Sprintf("kubernetes://%s/%s/%s/agent:/workspace",
						r.Client.CurrentContext, namespace, createdPod.Name)

					// Create Sync
					err = mutagen.CreateSync(
						"scion-"+createdPod.Name,
						config.Workspace,
						remoteURL,
						map[string]string{"scion-agent": createdPod.Name, "scion-path": "workspace"},
					)
					if err != nil {
						fmt.Printf("  Warning: failed to create mutagen sync: %s. Falling back to snapshot sync.\n", err)
					} else {
						fmt.Println("  Waiting for initial sync to complete...")
						if err := mutagen.WaitForSync("scion-"+createdPod.Name, 60*time.Second); err != nil {
							fmt.Printf("  Warning: mutagen sync timed out or failed: %s. Proceeding, but sync may be incomplete.\n", err)
						} else {
							fmt.Println("  Mutagen workspace sync active.")
							useMutagen = true
						}
					}

					// Also set up mutagen for home if configured
					if config.HomeDir != "" {
						homeSyncName := "scion-home-" + createdPod.Name
						destHome := fmt.Sprintf("/home/%s", config.UnixUsername)
						remoteHomeURL := fmt.Sprintf("kubernetes://%s/%s/%s/agent:%s",
							r.Client.CurrentContext, namespace, createdPod.Name, destHome)

						err = mutagen.CreateSync(
							homeSyncName,
							config.HomeDir,
							remoteHomeURL,
							map[string]string{"scion-agent": createdPod.Name, "scion-path": "home"},
						)
						if err != nil {
							fmt.Printf("  Warning: failed to create mutagen home sync: %s.\n", err)
						} else {
							fmt.Println("  Mutagen home sync active.")
						}
					}
				}
			} else {
				fmt.Println("  Warning: Sync mode is 'mutagen' but mutagen is not installed. Falling back to snapshot sync.")
			}
		}

		if !useMutagen {
			fmt.Printf("  Syncing workspace (%s -> /workspace)...\n", config.Workspace)
			err = r.syncToPod(ctx, namespace, createdPod.Name, config.Workspace, "/workspace")
			if err != nil {
				return createdPod.Name, fmt.Errorf("failed to sync workspace: %w", err)
			}
		}
	}

	fmt.Printf("Agent '%s' started successfully.\n", createdPod.Name)
	return createdPod.Name, nil
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
			data[s.Name] = []byte(s.Value)
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
	if err != nil {
		return "", fmt.Errorf("failed to create SecretProviderClass: %w", err)
	}

	return spcName, nil
}

func boolPtr(b bool) *bool { return &b }

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

	// Delete K8s Secrets
	_ = r.Client.Clientset.CoreV1().Secrets(namespace).DeleteCollection(ctx,
		metav1.DeleteOptions{},
		metav1.ListOptions{LabelSelector: selector},
	)

	// Delete SecretProviderClasses if GKE mode
	if r.GKEMode {
		_ = r.Client.Dynamic().Resource(k8s.SecretProviderClassGVR).Namespace(namespace).DeleteCollection(ctx,
			metav1.DeleteOptions{},
			metav1.ListOptions{LabelSelector: selector},
		)
	}
}

func (r *KubernetesRuntime) buildPod(namespace string, config RunConfig) *corev1.Pod {
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
	cmd = []string{"tmux", "new-session", "-s", "scion", cmdLine}

	// Env Resolution
	envVars := []corev1.EnvVar{}
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
	} else if config.Auth.GeminiAPIKey != "" || config.Auth.AnthropicAPIKey != "" || config.Auth.GoogleAPIKey != "" {
		// Backward-compatible M1 auth fallback (only when no ResolvedSecrets)
		if config.Auth.GeminiAPIKey != "" {
			envVars = append(envVars, corev1.EnvVar{Name: "GEMINI_API_KEY", Value: config.Auth.GeminiAPIKey})
		}
		if config.Auth.AnthropicAPIKey != "" {
			envVars = append(envVars, corev1.EnvVar{Name: "ANTHROPIC_API_KEY", Value: config.Auth.AnthropicAPIKey})
		}
		if config.Auth.GoogleAPIKey != "" {
			envVars = append(envVars, corev1.EnvVar{Name: "GOOGLE_API_KEY", Value: config.Auth.GoogleAPIKey})
		}
	}

	// Pass host user UID/GID for container user synchronization
	envVars = append(envVars, corev1.EnvVar{Name: "SCION_HOST_UID", Value: fmt.Sprintf("%d", os.Getuid())})
	envVars = append(envVars, corev1.EnvVar{Name: "SCION_HOST_GID", Value: fmt.Sprintf("%d", os.Getgid())})

	// TODO: For Kubernetes, we should consider using PodSecurityContext with fsGroup
	// to handle volume permissions more natively instead of relying on sciontool
	// UID/GID adjustment.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        config.Name,
			Namespace:   namespace,
			Labels:      config.Labels,
			Annotations: config.Annotations,
		},
		Spec: corev1.PodSpec{
			// TODO: Set SecurityContext.FSGroup here to SCION_HOST_GID
			Containers: []corev1.Container{
				{
					Name:            "agent",
					Image:           config.Image,
					Command:         cmd,
					Env:             envVars,
					ImagePullPolicy: corev1.PullIfNotPresent,
					WorkingDir:      "/workspace",
					Stdin:           true,
					TTY:             true,
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

	// Apply resource requests/limits from the common resource spec.
	if config.Resources != nil {
		reqs := corev1.ResourceList{}
		limits := corev1.ResourceList{}
		if config.Resources.Requests.CPU != "" {
			reqs[corev1.ResourceCPU] = resource.MustParse(config.Resources.Requests.CPU)
		}
		if config.Resources.Requests.Memory != "" {
			reqs[corev1.ResourceMemory] = resource.MustParse(config.Resources.Requests.Memory)
		}
		if config.Resources.Limits.CPU != "" {
			limits[corev1.ResourceCPU] = resource.MustParse(config.Resources.Limits.CPU)
		}
		if config.Resources.Limits.Memory != "" {
			limits[corev1.ResourceMemory] = resource.MustParse(config.Resources.Limits.Memory)
		}
		if config.Resources.Disk != "" {
			reqs[corev1.ResourceEphemeralStorage] = resource.MustParse(config.Resources.Disk)
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
			res.Requests[corev1.ResourceName(k)] = resource.MustParse(v)
		}
		for k, v := range config.Kubernetes.Resources.Limits {
			res.Limits[corev1.ResourceName(k)] = resource.MustParse(v)
		}
	}

	// Process Volumes (specifically GCS)
	type gcsVolInfo struct {
		Source string `json:"source"`
		Target string `json:"target"`
		Bucket string `json:"bucket"`
		Prefix string `json:"prefix"`
	}
	var gcsVolumes []gcsVolInfo

	for i, v := range config.Volumes {
		if v.Type == "gcs" {
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

			if pod.Annotations == nil {
				pod.Annotations = make(map[string]string)
			}
			pod.Annotations["gke-gcsfuse/volumes"] = "true"

			gcsVolumes = append(gcsVolumes, gcsVolInfo{
				Source: v.Source,
				Target: v.Target,
				Bucket: v.Bucket,
				Prefix: v.Prefix,
			})
		}
	}

	if len(gcsVolumes) > 0 {
		if data, err := json.Marshal(gcsVolumes); err == nil {
			encoded := base64.StdEncoding.EncodeToString(data)
			if pod.Annotations == nil {
				pod.Annotations = make(map[string]string)
			}
			pod.Annotations["scion.gcs_volumes"] = encoded
		}
	}

	if config.Kubernetes != nil && config.Kubernetes.ServiceAccountName != "" {
		pod.Spec.ServiceAccountName = config.Kubernetes.ServiceAccountName
	}

	return pod
}

func (r *KubernetesRuntime) waitForPodReady(ctx context.Context, namespace, podName string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute) // GKE Autopilot can be slow
	defer cancel()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	lastStatus := ""

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
				if reason == "ImagePullBackOff" || reason == "ErrImagePull" || reason == "InvalidImageName" {
					return fmt.Errorf("pod failed to start: %s - %s", reason, containerStatus.State.Waiting.Message)
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
	// Terminate Mutagen Sync if exists
	if mutagen.CheckInstalled() {
		// We use the label selector we applied during creation
		_ = mutagen.TerminateSync(fmt.Sprintf("scion-agent=%s", id))
	}

	namespace := r.DefaultNamespace

	// Clean up agent secrets and SecretProviderClasses before deleting the pod
	r.cleanupAgentSecrets(ctx, namespace, id)

	// 'id' is the pod name
	// Use GracePeriodSeconds=0 for immediate termination since Delete is used
	// for force-removal (e.g. scion rm), not graceful shutdown.
	gracePeriod := int64(0)
	err := r.Client.Clientset.CoreV1().Pods(namespace).Delete(ctx, id, metav1.DeleteOptions{
		GracePeriodSeconds: &gracePeriod,
	})
	if err != nil {
		return fmt.Errorf("failed to delete pod: %w", err)
	}
	return nil
}

func (r *KubernetesRuntime) List(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
	namespace := r.DefaultNamespace

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
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			agentStatus = "ended"
		}

		// Try to get more detail from container status
		for _, cs := range p.Status.ContainerStatuses {
			if cs.Name == "agent" {
				if cs.State.Waiting != nil {
					status = fmt.Sprintf("%s (%s)", p.Status.Phase, cs.State.Waiting.Reason)
				} else if cs.State.Terminated != nil {
					status = fmt.Sprintf("%s (%s)", p.Status.Phase, cs.State.Terminated.Reason)
					if agentStatus == "" {
						agentStatus = "ended"
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
			GrovePath:       grovePath,
			Labels:          p.Labels,
			Annotations:     p.Annotations,
			ContainerStatus: status,
			Status:          agentStatus,
			Image:           p.Spec.Containers[0].Image,
			Runtime:         r.Name(),
		})
	}
	return agents, nil
}

func (r *KubernetesRuntime) GetLogs(ctx context.Context, id string) (string, error) {
	namespace := r.DefaultNamespace
	podName := id // id is now pod name

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
	namespace := r.DefaultNamespace
	podName := id

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

	option := &corev1.PodExecOptions{
		Container: "agent",
		Command:   []string{"tmux", "attach", "-t", "scion"},
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
	// K8s pulls images if not present, so we can assume it "exists" or will be pulled.
	// Implementing a strict check would require querying the node or registry which is complex here.
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

	if r.SyncMode == "mutagen" {
		if !mutagen.CheckInstalled() {
			return fmt.Errorf("mutagen not installed but sync mode is mutagen")
		}
		// Check if workspace sync exists
		syncName := "scion-" + agent.ContainerID
		if err := mutagen.WaitForSync(syncName, 1*time.Second); err != nil {
			// Try to recreate if missing
			fmt.Printf("Mutagen workspace sync not found for '%s'. Creating...\n", agent.ContainerID)
			if err := mutagen.StartDaemon(); err != nil {
				return fmt.Errorf("failed to start mutagen daemon: %w", err)
			}

			// Clean up any existing session for this agent to avoid name collisions
			_ = mutagen.TerminateSync("scion-agent=" + agent.ContainerID)

			remoteURL := fmt.Sprintf("kubernetes://%s/%s/%s/agent:/workspace",
				r.Client.CurrentContext, namespace, agent.ContainerID)

			err = mutagen.CreateSync(
				syncName,
				workspacePath,
				remoteURL,
				map[string]string{"scion-agent": agent.ContainerID, "scion-path": "workspace"},
			)
			if err != nil {
				return fmt.Errorf("failed to create mutagen workspace sync: %w", err)
			}
			fmt.Println("Mutagen workspace sync created.")
		} else {
			fmt.Println("Mutagen workspace sync is already active.")
		}

		// Also handle home dir sync if configured
		if homeDir != "" && username != "" {
			homeSyncName := "scion-home-" + agent.ContainerID
			if err := mutagen.WaitForSync(homeSyncName, 1*time.Second); err != nil {
				fmt.Printf("Mutagen home sync not found for '%s'. Creating...\n", agent.ContainerID)
				destHome := fmt.Sprintf("/home/%s", username)
				remoteURL := fmt.Sprintf("kubernetes://%s/%s/%s/agent:%s",
					r.Client.CurrentContext, namespace, agent.ContainerID, destHome)

				err = mutagen.CreateSync(
					homeSyncName,
					homeDir,
					remoteURL,
					map[string]string{"scion-agent": agent.ContainerID, "scion-path": "home"},
				)
				if err != nil {
					return fmt.Errorf("failed to create mutagen home sync: %w", err)
				}
				fmt.Println("Mutagen home sync created.")
			} else {
				fmt.Println("Mutagen home sync is already active.")
			}
		}

		return nil
	}

	// Default to tar sync (Snapshot)
	if direction == SyncUnspecified {
		return fmt.Errorf("direction (to or from) must be specified for tar sync. Example: scion sync to %s", agent.ContainerID)
	}

	if direction == SyncFrom {
		fmt.Printf("Syncing workspace (agent -> %s)...\n", workspacePath)
		if err := r.syncFromPod(ctx, namespace, agent.ContainerID, "/workspace", workspacePath); err != nil {
			return err
		}
		if homeDir != "" && username != "" {
			destHome := fmt.Sprintf("/home/%s", username)
			fmt.Printf("Syncing agent home (agent -> %s)...\n", homeDir)
			if err := r.syncFromPod(ctx, namespace, agent.ContainerID, destHome, homeDir); err != nil {
				return err
			}
		}
		return nil
	}

	fmt.Printf("Syncing workspace (%s -> agent)...\n", workspacePath)
	if err := r.syncToPod(ctx, namespace, agent.ContainerID, workspacePath, "/workspace"); err != nil {
		return err
	}
	if homeDir != "" && username != "" {
		destHome := fmt.Sprintf("/home/%s", username)
		fmt.Printf("Syncing agent home (%s -> agent)...\n", homeDir)
		if err := r.syncToPod(ctx, namespace, agent.ContainerID, homeDir, destHome); err != nil {
			return err
		}
	}
	return nil
}

func (r *KubernetesRuntime) Exec(ctx context.Context, id string, cmd []string) (string, error) {
	namespace := r.DefaultNamespace
	podName := id

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

// GetWorkspacePath returns the local workspace path for a Kubernetes pod.
// For K8s, this returns the workspace path stored in annotations when the pod was created.
func (r *KubernetesRuntime) GetWorkspacePath(ctx context.Context, id string) (string, error) {
	namespace := r.DefaultNamespace

	// Parse namespace from id if present (format: namespace/podname)
	if strings.Contains(id, "/") {
		parts := strings.SplitN(id, "/", 2)
		namespace = parts[0]
		id = parts[1]
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
