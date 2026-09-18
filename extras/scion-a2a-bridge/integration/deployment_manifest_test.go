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

package integration_test

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCloudRunManifestValidation(t *testing.T) {
	manifestPath := filepath.Join("..", "deploy", "cloudrun", "service.yaml")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read Cloud Run manifest: %v", err)
	}

	var root struct {
		APIVersion string `yaml:"apiVersion"`
		Kind       string `yaml:"kind"`
		Metadata   struct {
			Name        string            `yaml:"name"`
			Annotations map[string]string `yaml:"annotations"`
		} `yaml:"metadata"`
		Spec struct {
			Template struct {
				Metadata struct {
					Annotations map[string]string `yaml:"annotations"`
				} `yaml:"metadata"`
				Spec struct {
					Containers []struct {
						Ports []struct {
							Name          string `yaml:"name"`
							ContainerPort int    `yaml:"containerPort"`
						} `yaml:"ports"`
						Env []struct {
							Name      string `yaml:"name"`
							Value     string `yaml:"value"`
							ValueFrom *struct {
								SecretKeyRef struct {
									Name string `yaml:"name"`
									Key  string `yaml:"key"`
								} `yaml:"secretKeyRef"`
							} `yaml:"valueFrom"`
						} `yaml:"env"`
						StartupProbe *struct {
							HTTPGet struct {
								Path string `yaml:"path"`
								Port int    `yaml:"port"`
							} `yaml:"httpGet"`
						} `yaml:"startupProbe"`
						LivenessProbe *struct {
							HTTPGet struct {
								Path string `yaml:"path"`
								Port int    `yaml:"port"`
							} `yaml:"httpGet"`
						} `yaml:"livenessProbe"`
					} `yaml:"containers"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}

	if err := yaml.Unmarshal(data, &root); err != nil {
		t.Fatalf("unmarshal Cloud Run manifest: %v", err)
	}

	if root.Kind != "Service" {
		t.Errorf("kind = %s, want Service", root.Kind)
	}
	if root.Metadata.Name != "scion-a2a-bridge" {
		t.Errorf("name = %s, want scion-a2a-bridge", root.Metadata.Name)
	}

	minScaleStr := root.Spec.Template.Metadata.Annotations["autoscaling.knative.dev/minScale"]
	minScale, err := strconv.Atoi(minScaleStr)
	if err != nil || minScale < 2 {
		t.Errorf("minScale = %s (parsed %d), want >= 2 for HA multi-instance", minScaleStr, minScale)
	}

	if len(root.Spec.Template.Spec.Containers) == 0 {
		t.Fatal("no containers specified in manifest")
	}
	container := root.Spec.Template.Spec.Containers[0]

	var foundH2C bool
	for _, port := range container.Ports {
		if port.Name == "h2c" && port.ContainerPort == 8080 {
			foundH2C = true
			break
		}
	}
	if !foundH2C {
		t.Errorf("h2c port 8080 not found in container ports: %+v", container.Ports)
	}

	envMap := make(map[string]string)
	for _, env := range container.Env {
		envMap[env.Name] = env.Value
	}

	if envMap["PORT"] != "8080" {
		t.Errorf("PORT env = %q, want 8080", envMap["PORT"])
	}
	if envMap["MUX_PORTS"] != "true" {
		t.Errorf("MUX_PORTS env = %q, want true", envMap["MUX_PORTS"])
	}
	if envMap["GRPC_AUTH_MODE"] != "google_id_token" {
		t.Errorf("GRPC_AUTH_MODE env = %q, want google_id_token", envMap["GRPC_AUTH_MODE"])
	}
	if envMap["GRPC_AUTH_AUDIENCE"] == "" {
		t.Error("GRPC_AUTH_AUDIENCE must not be empty")
	}
	if envMap["GRPC_AUTH_SUBJECTS"] == "" {
		t.Error("GRPC_AUTH_SUBJECTS must not be empty")
	}

	if container.StartupProbe == nil || container.StartupProbe.HTTPGet.Path != "/healthz" {
		t.Error("startupProbe missing or does not query /healthz")
	}
	if container.LivenessProbe == nil || container.LivenessProbe.HTTPGet.Path != "/healthz" {
		t.Error("livenessProbe missing or does not query /healthz")
	}
}

func TestKubernetesManifestValidation(t *testing.T) {
	manifestPath := filepath.Join("..", "deploy", "kubernetes", "deployment.yaml")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read Kubernetes manifest: %v", err)
	}

	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var kinds []string
	var foundDeployment, foundService, foundIngress bool

	for {
		var doc struct {
			APIVersion string `yaml:"apiVersion"`
			Kind       string `yaml:"kind"`
			Metadata   struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
			Spec struct {
				Replicas *int `yaml:"replicas"`
				Strategy *struct {
					Type string `yaml:"type"`
				} `yaml:"strategy"`
				Ports []struct {
					Name        string `yaml:"name"`
					Port        int    `yaml:"port"`
					AppProtocol string `yaml:"appProtocol"`
				} `yaml:"ports"`
				Template struct {
					Spec struct {
						Containers []struct {
							Name  string `yaml:"name"`
							Ports []struct {
								ContainerPort int `yaml:"containerPort"`
							} `yaml:"ports"`
							Env []struct {
								Name      string `yaml:"name"`
								Value     string `yaml:"value"`
								ValueFrom *struct {
									SecretKeyRef struct {
										Name string `yaml:"name"`
										Key  string `yaml:"key"`
									} `yaml:"secretKeyRef"`
								} `yaml:"valueFrom"`
							} `yaml:"env"`
							ReadinessProbe *struct {
								HTTPGet struct {
									Path string `yaml:"path"`
								} `yaml:"httpGet"`
							} `yaml:"readinessProbe"`
							LivenessProbe *struct {
								HTTPGet struct {
									Path string `yaml:"path"`
								} `yaml:"httpGet"`
							} `yaml:"livenessProbe"`
						} `yaml:"containers"`
					} `yaml:"spec"`
				} `yaml:"template"`
			} `yaml:"spec"`
		}

		if err := decoder.Decode(&doc); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("decode doc: %v", err)
		}

		kinds = append(kinds, doc.Kind)
		switch doc.Kind {
		case "Deployment":
			foundDeployment = true
			if doc.Spec.Replicas == nil || *doc.Spec.Replicas < 2 {
				t.Errorf("Deployment replicas = %v, want >= 2", doc.Spec.Replicas)
			}
			if doc.Spec.Strategy == nil || doc.Spec.Strategy.Type != "RollingUpdate" {
				t.Errorf("Deployment strategy = %v, want RollingUpdate", doc.Spec.Strategy)
			}
			if len(doc.Spec.Template.Spec.Containers) == 0 {
				t.Fatal("Deployment has no containers")
			}
			c := doc.Spec.Template.Spec.Containers[0]
			var foundSecretRef bool
			for _, env := range c.Env {
				if env.Name == "DATABASE_URL" && env.ValueFrom != nil && env.ValueFrom.SecretKeyRef.Name == "a2a-postgres-secret" {
					foundSecretRef = true
					break
				}
			}
			if !foundSecretRef {
				t.Error("Deployment missing DATABASE_URL secretKeyRef from a2a-postgres-secret")
			}
			if c.ReadinessProbe == nil || c.ReadinessProbe.HTTPGet.Path != "/healthz" {
				t.Error("readinessProbe missing or does not query /healthz")
			}
			if c.LivenessProbe == nil || c.LivenessProbe.HTTPGet.Path != "/healthz" {
				t.Error("livenessProbe missing or does not query /healthz")
			}
		case "Service":
			foundService = true
			var foundH2CService bool
			for _, p := range doc.Spec.Ports {
				if p.Port == 8080 && p.AppProtocol == "kubernetes.io/h2c" {
					foundH2CService = true
					break
				}
			}
			if !foundH2CService {
				t.Errorf("Service missing h2c port 8080: %+v", doc.Spec.Ports)
			}
		case "Ingress":
			foundIngress = true
		}
	}

	if !foundDeployment || !foundService || !foundIngress {
		t.Fatalf("expected Deployment, Service, and Ingress in manifest; got kinds: %v", kinds)
	}
}
