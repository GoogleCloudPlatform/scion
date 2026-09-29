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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// TestBuildPod_HomeDir_RootUser verifies that every k8s pod-building site that
// derives the in-container home directory resolves the root user to /root,
// matching util.GetHomeDir, instead of the previously hardcoded /home/root.
func TestBuildPod_HomeDir_RootUser(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	config := RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "root",
		ResolvedSecrets: []api.ResolvedSecret{
			{Name: "SSH_KEY", Type: "file", Target: "~/.ssh/id_rsa", Value: "key-data", Source: "user"},
			{Name: "CONFIG", Type: "variable", Target: "config", Value: `{"key":"val"}`, Source: "user"},
			{Name: telemetryGCPCredentialsSecretName, Type: "file", Target: "~/.config/gcloud/creds.json", Value: "cred-data", Source: "user"},
		},
		ResolvedAuth: &api.ResolvedAuth{
			Method: "api-key",
			Files: []api.FileMapping{
				{SourcePath: "/host/path/to/cred.json", ContainerPath: "~/.config/gcloud/adc.json"},
			},
		},
	}

	pod, err := rt.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod failed: %v", err)
	}

	// HOME env var should be /root, not /home/root.
	foundHome := false
	for _, env := range pod.Spec.Containers[0].Env {
		if env.Name == "HOME" {
			foundHome = true
			if env.Value != "/root" {
				t.Errorf("HOME = %q, want /root", env.Value)
			}
		}
	}
	if !foundHome {
		t.Fatal("HOME not found in pod env")
	}

	// Tilde-expanded file secret mount should land under /root.
	foundSSH := false
	for _, vm := range pod.Spec.Containers[0].VolumeMounts {
		if vm.Name == "agent-secrets" && vm.SubPath == "SSH_KEY" {
			foundSSH = true
			if vm.MountPath != "/root/.ssh/id_rsa" {
				t.Errorf("SSH_KEY MountPath = %q, want /root/.ssh/id_rsa", vm.MountPath)
			}
		}
	}
	if !foundSSH {
		t.Error("expected volume mount for SSH_KEY")
	}

	// Variable secrets are staged under <home>/.scion/secrets.json.
	foundSecretsJSON := false
	for _, vm := range pod.Spec.Containers[0].VolumeMounts {
		if vm.Name == "agent-secrets" && vm.SubPath == "secrets.json" {
			foundSecretsJSON = true
			if vm.MountPath != "/root/.scion/secrets.json" {
				t.Errorf("secrets.json MountPath = %q, want /root/.scion/secrets.json", vm.MountPath)
			}
		}
	}
	if !foundSecretsJSON {
		t.Error("expected volume mount for secrets.json")
	}

	// ResolvedAuth file mount should also land under /root.
	foundAuthMount := false
	for _, vm := range pod.Spec.Containers[0].VolumeMounts {
		if vm.Name == "auth-files" && vm.MountPath == "/root/.config/gcloud/adc.json" {
			foundAuthMount = true
		}
	}
	if !foundAuthMount {
		t.Error("expected auth-files volume mount at /root/.config/gcloud/adc.json")
	}

	// GCP telemetry credential env var should point under /root.
	foundTelemetry := false
	for _, env := range pod.Spec.Containers[0].Env {
		if env.Name == telemetryGCPCredentialsEnvVar {
			foundTelemetry = true
			if env.Value != "/root/.config/gcloud/creds.json" {
				t.Errorf("%s = %q, want /root/.config/gcloud/creds.json", telemetryGCPCredentialsEnvVar, env.Value)
			}
		}
	}
	if !foundTelemetry {
		t.Error("expected telemetry credential env var")
	}
}

// TestBuildPod_HomeDir_NonRootUser is the non-root counterpart to
// TestBuildPod_HomeDir_RootUser: it verifies the same sites resolve to
// /home/<user> for an arbitrary non-root username (not just the common
// "scion" fixture used elsewhere), matching util.GetHomeDir.
func TestBuildPod_HomeDir_NonRootUser(t *testing.T) {
	rt, _, _ := newTestK8sRuntime()

	config := RunConfig{
		Name:         "test-agent",
		Image:        "test:latest",
		UnixUsername: "alice",
		ResolvedSecrets: []api.ResolvedSecret{
			{Name: "SSH_KEY", Type: "file", Target: "~/.ssh/id_rsa", Value: "key-data", Source: "user"},
			{Name: "CONFIG", Type: "variable", Target: "config", Value: `{"key":"val"}`, Source: "user"},
		},
		ResolvedAuth: &api.ResolvedAuth{
			Method: "api-key",
			Files: []api.FileMapping{
				{SourcePath: "/host/path/to/cred.json", ContainerPath: "~/.config/gcloud/adc.json"},
			},
		},
	}

	pod, err := rt.buildPod("default", config)
	if err != nil {
		t.Fatalf("buildPod failed: %v", err)
	}

	foundHome := false
	for _, env := range pod.Spec.Containers[0].Env {
		if env.Name == "HOME" {
			foundHome = true
			if env.Value != "/home/alice" {
				t.Errorf("HOME = %q, want /home/alice", env.Value)
			}
		}
	}
	if !foundHome {
		t.Fatal("HOME not found in pod env")
	}

	foundSSH := false
	for _, vm := range pod.Spec.Containers[0].VolumeMounts {
		if vm.Name == "agent-secrets" && vm.SubPath == "SSH_KEY" {
			foundSSH = true
			if vm.MountPath != "/home/alice/.ssh/id_rsa" {
				t.Errorf("SSH_KEY MountPath = %q, want /home/alice/.ssh/id_rsa", vm.MountPath)
			}
		}
	}
	if !foundSSH {
		t.Error("expected volume mount for SSH_KEY")
	}

	foundSecretsJSON := false
	for _, vm := range pod.Spec.Containers[0].VolumeMounts {
		if vm.Name == "agent-secrets" && vm.SubPath == "secrets.json" {
			foundSecretsJSON = true
			if vm.MountPath != "/home/alice/.scion/secrets.json" {
				t.Errorf("secrets.json MountPath = %q, want /home/alice/.scion/secrets.json", vm.MountPath)
			}
		}
	}
	if !foundSecretsJSON {
		t.Error("expected volume mount for secrets.json")
	}

	foundAuthMount := false
	for _, vm := range pod.Spec.Containers[0].VolumeMounts {
		if vm.Name == "auth-files" && vm.MountPath == "/home/alice/.config/gcloud/adc.json" {
			foundAuthMount = true
		}
	}
	if !foundAuthMount {
		t.Error("expected auth-files volume mount at /home/alice/.config/gcloud/adc.json")
	}
}
