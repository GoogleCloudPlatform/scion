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

// TestBuildPod_HomeDir verifies that every k8s pod-building site that derives
// the in-container home directory resolves it via util.GetHomeDir — /root for
// the root user, /home/<user> otherwise — instead of the previously
// hardcoded /home/%s. It runs the same assertions for a root and a non-root
// user against both secret-mounting strategies: the fallback K8s-Secret path
// and the GKE CSI path (secrets-store, used when a secret carries a Ref).
func TestBuildPod_HomeDir(t *testing.T) {
	cases := []struct {
		name         string
		unixUsername string
		home         string
	}{
		{name: "root", unixUsername: "root", home: "/root"},
		{name: "alice", unixUsername: "alice", home: "/home/alice"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("fallback_secrets", func(t *testing.T) {
				rt, _, _ := newTestK8sRuntime()

				config := RunConfig{
					Name:         "test-agent",
					Image:        "test:latest",
					UnixUsername: tc.unixUsername,
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

				// HOME env var should match the expected home for this user.
				foundHome := false
				for _, env := range pod.Spec.Containers[0].Env {
					if env.Name == "HOME" {
						foundHome = true
						if env.Value != tc.home {
							t.Errorf("HOME = %q, want %q", env.Value, tc.home)
						}
					}
				}
				if !foundHome {
					t.Fatal("HOME not found in pod env")
				}

				// Files with targets under home are not mounted there; they
				// are staged under /run/scion and placed at the home path.
				assertNoMountsUnderHome(t, pod, tc.home)
				placements := rt.k8sHomeFilePlacements(config)
				assertPlacement(t, placements, tc.home+"/.ssh/id_rsa", "/run/scion/agent-secrets/SSH_KEY")
				// Variable secrets are placed at <home>/.scion/secrets.json.
				assertPlacement(t, placements, tc.home+"/.scion/secrets.json", "/run/scion/agent-secrets/secrets.json")
				// ResolvedAuth files are placed under home too.
				assertPlacement(t, placements, tc.home+"/.config/gcloud/adc.json", "/run/scion/auth-files/auth-file-0")
				assertStagingMount(t, pod, "agent-secrets")
				assertStagingMount(t, pod, "auth-files")

				// GCP telemetry credential env var should point under home.
				wantTelemetry := tc.home + "/.config/gcloud/creds.json"
				foundTelemetry := false
				for _, env := range pod.Spec.Containers[0].Env {
					if env.Name == telemetryGCPCredentialsEnvVar {
						foundTelemetry = true
						if env.Value != wantTelemetry {
							t.Errorf("%s = %q, want %q", telemetryGCPCredentialsEnvVar, env.Value, wantTelemetry)
						}
					}
				}
				if !foundTelemetry {
					t.Error("expected telemetry credential env var")
				}
			})

			// GKE CSI path: a resolved secret with a Ref routes file-type
			// secrets through the secrets-store CSI volume instead of the
			// agent-secrets K8s Secret. This covers the buildPod site at the
			// GKE CSI mount (k8s_runtime.go's useGKEPath "file" case).
			t.Run("gke_csi_file_secret", func(t *testing.T) {
				rt, _, _ := newTestK8sRuntime()
				rt.GKEMode = true

				config := RunConfig{
					Name:         "test-agent",
					Image:        "test:latest",
					UnixUsername: tc.unixUsername,
					ResolvedSecrets: []api.ResolvedSecret{
						{Name: "SSH_KEY", Type: "file", Target: "~/.ssh/id_rsa", Value: "key-data", Source: "user", Ref: "projects/p/secrets/s"},
					},
				}

				pod, err := rt.buildPod("default", config)
				if err != nil {
					t.Fatalf("buildPod failed: %v", err)
				}

				assertNoMountsUnderHome(t, pod, tc.home)
				assertStagingMount(t, pod, "secrets-store")
				assertPlacement(t, rt.k8sHomeFilePlacements(config), tc.home+"/.ssh/id_rsa", "/run/scion/secrets-store/SSH_KEY")
			})
		})
	}
}
