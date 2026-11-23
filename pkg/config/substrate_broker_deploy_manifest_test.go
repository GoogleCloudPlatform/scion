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

package config

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func loadExpandedSubstrateManifestWith(t *testing.T, overrides map[string]string, relParts ...string) []byte {
	t.Helper()
	parts := append([]string{"..", ".."}, relParts...)
	raw, err := os.ReadFile(filepath.Join(parts...))
	require.NoError(t, err)
	return []byte(os.Expand(string(raw), func(name string) string {
		if v, ok := overrides[name]; ok {
			return v
		}
		return "placeholder"
	}))
}

func loadExpandedSubstrateManifest(t *testing.T, relParts ...string) []byte {
	t.Helper()
	return loadExpandedSubstrateManifestWith(t, nil, relParts...)
}

// TestDeploySubstrateBrokerYAML_SettingsHaveOnlySubstrateProfiles loads the
// settings.yaml embedded in the REAL deploy/substrate/broker.yaml (envsubst
// placeholders filled with a dummy value) on top of the embedded defaults,
// and requires every profile to resolve to a substrate runtime. Unlike
// substrate_broker_profiles_test.go's
// substrateBrokerProfilesOverride (a hand-maintained copy of the same
// stanza, which can drift from the manifest silently), this fails if the
// manifest's local/remote repoint is ever removed or edited incorrectly.
func TestDeploySubstrateBrokerYAML_SettingsHaveOnlySubstrateProfiles(t *testing.T) {
	expanded := loadExpandedSubstrateManifest(t, "deploy", "substrate", "broker.yaml")

	var settings string
	dec := yaml.NewDecoder(bytes.NewReader(expanded))
	for {
		var doc struct {
			Kind     string                `yaml:"kind"`
			Metadata struct{ Name string } `yaml:"metadata"`
			Data     map[string]string     `yaml:"data"`
		}
		if err := dec.Decode(&doc); errors.Is(err, io.EOF) {
			break
		} else {
			require.NoError(t, err)
		}
		if doc.Kind == "ConfigMap" && doc.Metadata.Name == "scion-substrate-broker-settings" {
			settings = doc.Data["settings.yaml"]
		}
	}
	require.NotEmpty(t, settings, "scion-substrate-broker-settings ConfigMap settings.yaml not found in broker.yaml")

	valErrors, err := ValidateSettings([]byte(settings), "1")
	require.NoError(t, err)
	assert.Empty(t, valErrors, "broker ConfigMap settings.yaml should validate against v1 schema, got: %v", valErrors)

	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, ".scion"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, ".scion", "settings.yaml"), []byte(settings), 0o644))

	vs, err := LoadVersionedSettings(filepath.Join(tmpDir, "no-project", ".scion"))
	require.NoError(t, err)
	require.NotEmpty(t, vs.Profiles)
	for name, p := range vs.Profiles {
		rt, ok := vs.Runtimes[p.Runtime]
		if !ok || rt.Type != "substrate" {
			t.Errorf("profile %q -> runtime %q (type %q), want a substrate runtime", name, p.Runtime, rt.Type)
		}
	}
	require.Equal(t, "substrate-prod", vs.Profiles["local"].Runtime)
	require.Equal(t, "substrate-prod", vs.Profiles["remote"].Runtime)

	sub := vs.Runtimes["substrate-prod"].Substrate
	require.NotNil(t, sub)
	assert.Empty(t, sub.SandboxClass)
	assert.Empty(t, sub.SandboxConfigName)
	assert.Equal(t, "https://atenet-router.placeholder.svc", sub.RouterEndpoint)
	assert.Equal(t, "/run/podidentity.podcert.ate.dev/servicedns-ca/trust-bundle.pem", sub.CAFile)
	assert.Equal(t, "/run/podidentity.podcert.ate.dev/credential-bundle.pem", sub.RouterClientCertFile)
	assert.Empty(t, sub.ClusterTrustBundle)
}

func TestDeploySubstrateBrokerYAML_TLSWiringAndRBAC(t *testing.T) {
	expanded := loadExpandedSubstrateManifest(t, "deploy", "substrate", "broker.yaml")

	var foundNS, foundCTBRole, foundDeployment bool
	dec := yaml.NewDecoder(bytes.NewReader(expanded))
	for {
		var doc struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name   string            `yaml:"name"`
				Labels map[string]string `yaml:"labels"`
			} `yaml:"metadata"`
			Rules []struct {
				APIGroups     []string `yaml:"apiGroups"`
				Resources     []string `yaml:"resources"`
				ResourceNames []string `yaml:"resourceNames"`
				Verbs         []string `yaml:"verbs"`
			} `yaml:"rules"`
			Spec struct {
				Template struct {
					Spec struct {
						Containers []struct {
							Name         string `yaml:"name"`
							VolumeMounts []struct {
								Name      string `yaml:"name"`
								MountPath string `yaml:"mountPath"`
								SubPath   string `yaml:"subPath"`
								ReadOnly  bool   `yaml:"readOnly"`
							} `yaml:"volumeMounts"`
						} `yaml:"containers"`
						Volumes []struct {
							Name      string `yaml:"name"`
							Projected *struct {
								Sources []struct {
									ClusterTrustBundle *struct {
										SignerName    string `yaml:"signerName"`
										LabelSelector *struct {
											MatchLabels map[string]string `yaml:"matchLabels"`
										} `yaml:"labelSelector"`
										Path string `yaml:"path"`
									} `yaml:"clusterTrustBundle"`
									PodCertificate *struct {
										SignerName           string `yaml:"signerName"`
										KeyType              string `yaml:"keyType"`
										CredentialBundlePath string `yaml:"credentialBundlePath"`
									} `yaml:"podCertificate"`
								} `yaml:"sources"`
							} `yaml:"projected"`
						} `yaml:"volumes"`
					} `yaml:"spec"`
				} `yaml:"template"`
			} `yaml:"spec"`
		}
		if err := dec.Decode(&doc); errors.Is(err, io.EOF) {
			break
		} else {
			require.NoError(t, err)
		}
		switch doc.Kind {
		case "Namespace":
			foundNS = true
			assert.Equal(t, "true", doc.Metadata.Labels["ate.dev/api-access"], "broker Namespace must carry ate.dev/api-access=true for Substrate v1")
		case "ClusterRole":
			if doc.Metadata.Name == "scion-substrate-broker-ctb-reader" {
				foundCTBRole = true
				require.Len(t, doc.Rules, 1)
				assert.Equal(t, []string{"certificates.k8s.io"}, doc.Rules[0].APIGroups)
				assert.Equal(t, []string{"clustertrustbundles"}, doc.Rules[0].Resources)
				assert.Empty(t, doc.Rules[0].ResourceNames, "ClusterTrustBundle get/list RBAC must not pin resourceNames")
				assert.Equal(t, []string{"get", "list"}, doc.Rules[0].Verbs)
			}
		case "Deployment":
			if doc.Metadata.Name == "scion-substrate-broker" {
				foundDeployment = true
				var foundTLSMount bool
				for _, c := range doc.Spec.Template.Spec.Containers {
					for _, vm := range c.VolumeMounts {
						if vm.Name == "substrate-tls" {
							foundTLSMount = true
							assert.Equal(t, "/run/podidentity.podcert.ate.dev", vm.MountPath)
							assert.Empty(t, vm.SubPath, "substrate-tls must not use subPath so kubelet rotates certificates")
							assert.True(t, vm.ReadOnly)
						}
					}
				}
				assert.True(t, foundTLSMount, "Deployment must mount substrate-tls")

				var foundCTB, foundPodCert bool
				for _, v := range doc.Spec.Template.Spec.Volumes {
					if v.Name != "substrate-tls" || v.Projected == nil {
						continue
					}
					for _, src := range v.Projected.Sources {
						if src.ClusterTrustBundle != nil {
							foundCTB = true
							assert.Equal(t, "servicedns.podcert.ate.dev/identity", src.ClusterTrustBundle.SignerName)
							require.NotNil(t, src.ClusterTrustBundle.LabelSelector)
							assert.Equal(t, "live", src.ClusterTrustBundle.LabelSelector.MatchLabels["podcert.ate.dev/canarying"])
							assert.Equal(t, "servicedns-ca/trust-bundle.pem", src.ClusterTrustBundle.Path)
						}
						if src.PodCertificate != nil {
							foundPodCert = true
							assert.Equal(t, "podidentity.podcert.ate.dev/identity", src.PodCertificate.SignerName)
							assert.Equal(t, "ECDSAP256", src.PodCertificate.KeyType)
							assert.Equal(t, "credential-bundle.pem", src.PodCertificate.CredentialBundlePath)
						}
					}
				}
				assert.True(t, foundCTB, "substrate-tls projected volume must include clusterTrustBundle")
				assert.True(t, foundPodCert, "substrate-tls projected volume must include podCertificate")
			}
		}
	}
	assert.True(t, foundNS)
	assert.True(t, foundCTBRole)
	assert.True(t, foundDeployment)
}

func TestDeploySubstrateSettingsExampleYAML_ValidatesAndLoads(t *testing.T) {
	expanded := loadExpandedSubstrateManifest(t, "deploy", "substrate", "settings.example.yaml")

	valErrors, err := ValidateSettings(expanded, "1")
	require.NoError(t, err)
	assert.Empty(t, valErrors, "settings.example.yaml should validate against v1 schema, got: %v", valErrors)

	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, ".scion"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, ".scion", "settings.yaml"), expanded, 0o644))

	vs, err := LoadVersionedSettings(filepath.Join(tmpDir, "no-project", ".scion"))
	require.NoError(t, err)
	sub := vs.Runtimes["substrate-prod"].Substrate
	require.NotNil(t, sub)
	assert.Empty(t, sub.SandboxClass)
	assert.Empty(t, sub.SandboxConfigName)
	assert.Equal(t, "https://atenet-router.placeholder.svc", sub.RouterEndpoint)
	assert.Equal(t, "/run/podidentity.podcert.ate.dev/servicedns-ca/trust-bundle.pem", sub.CAFile)
	assert.Equal(t, "/run/podidentity.podcert.ate.dev/credential-bundle.pem", sub.RouterClientCertFile)
}

func TestDeploySubstrateClusterYAML_V1WorkerPoolAndRouterTLSNetworkPolicy(t *testing.T) {
	wpExpanded := loadExpandedSubstrateManifest(t, "deploy", "substrate", "cluster", "workerpool.yaml")
	var foundWorkerPool bool
	wpDec := yaml.NewDecoder(bytes.NewReader(wpExpanded))
	for {
		var doc struct {
			APIVersion string                 `yaml:"apiVersion"`
			Kind       string                 `yaml:"kind"`
			Spec       map[string]interface{} `yaml:"spec"`
		}
		if err := wpDec.Decode(&doc); errors.Is(err, io.EOF) {
			break
		} else {
			require.NoError(t, err)
		}
		if doc.Kind == "WorkerPool" {
			foundWorkerPool = true
			assert.Equal(t, "ate.dev/v1", doc.APIVersion)
			_, hasLegacySandboxClass := doc.Spec["sandboxClass"]
			assert.False(t, hasLegacySandboxClass, "v1 WorkerPool must use spec.sandboxClasses rather than sandboxClass")
			classes, ok := doc.Spec["sandboxClasses"].([]interface{})
			require.True(t, ok)
			require.NotEmpty(t, classes)
			first, ok := classes[0].(map[string]interface{})
			require.True(t, ok)
			assert.Equal(t, "gvisor", first["name"])
			cfgRef, ok := first["configRef"].(map[string]interface{})
			require.True(t, ok)
			assert.Equal(t, "placeholder", cfgRef["name"])
		}
	}
	assert.True(t, foundWorkerPool)

	npExpanded := loadExpandedSubstrateManifest(t, "deploy", "substrate", "cluster", "networkpolicy.yaml")
	var foundRouterPolicy bool
	npDec := yaml.NewDecoder(bytes.NewReader(npExpanded))
	for {
		var doc struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
			Spec struct {
				Ingress []struct {
					Ports []struct {
						Protocol string `yaml:"protocol"`
						Port     int    `yaml:"port"`
					} `yaml:"ports"`
				} `yaml:"ingress"`
			} `yaml:"spec"`
		}
		if err := npDec.Decode(&doc); errors.Is(err, io.EOF) {
			break
		} else {
			require.NoError(t, err)
		}
		if doc.Kind == "NetworkPolicy" && doc.Metadata.Name == "atenet-router-restrict-ingress" {
			foundRouterPolicy = true
			var ports []int
			for _, ing := range doc.Spec.Ingress {
				for _, p := range ing.Ports {
					assert.Equal(t, "TCP", p.Protocol)
					ports = append(ports, p.Port)
				}
			}
			assert.Equal(t, []int{8443, 8444}, ports, "atenet-router NetworkPolicy must permit only TLS ports 8443 and 8444")
		}
	}
	assert.True(t, foundRouterPolicy)
}

func TestDeploySubstrateManifests_EmptySnapshotStorageValidatesAsEmptyString(t *testing.T) {
	emptyOverrides := map[string]string{"SNAPSHOT_STORAGE_URI": ""}

	t.Run("broker.yaml", func(t *testing.T) {
		expanded := loadExpandedSubstrateManifestWith(t, emptyOverrides, "deploy", "substrate", "broker.yaml")
		var settings string
		dec := yaml.NewDecoder(bytes.NewReader(expanded))
		for {
			var doc struct {
				Kind     string                `yaml:"kind"`
				Metadata struct{ Name string } `yaml:"metadata"`
				Data     map[string]string     `yaml:"data"`
			}
			if err := dec.Decode(&doc); errors.Is(err, io.EOF) {
				break
			} else {
				require.NoError(t, err)
			}
			if doc.Kind == "ConfigMap" && doc.Metadata.Name == "scion-substrate-broker-settings" {
				settings = doc.Data["settings.yaml"]
			}
		}
		require.NotEmpty(t, settings)

		valErrors, err := ValidateSettings([]byte(settings), "1")
		require.NoError(t, err)
		assert.Empty(t, valErrors, "empty SNAPSHOT_STORAGE_URI must serialize as empty string, not YAML null: %v", valErrors)

		tmpDir := t.TempDir()
		t.Setenv("HOME", tmpDir)
		require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, ".scion"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(tmpDir, ".scion", "settings.yaml"), []byte(settings), 0o644))

		vs, err := LoadVersionedSettings(filepath.Join(tmpDir, "no-project", ".scion"))
		require.NoError(t, err)
		require.NotNil(t, vs.Runtimes["substrate-prod"].Substrate)
		assert.Equal(t, "", vs.Runtimes["substrate-prod"].Substrate.SnapshotStorage)
	})

	t.Run("settings.example.yaml", func(t *testing.T) {
		expanded := loadExpandedSubstrateManifestWith(t, emptyOverrides, "deploy", "substrate", "settings.example.yaml")

		valErrors, err := ValidateSettings(expanded, "1")
		require.NoError(t, err)
		assert.Empty(t, valErrors, "empty SNAPSHOT_STORAGE_URI must serialize as empty string, not YAML null: %v", valErrors)

		tmpDir := t.TempDir()
		t.Setenv("HOME", tmpDir)
		require.NoError(t, os.MkdirAll(filepath.Join(tmpDir, ".scion"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(tmpDir, ".scion", "settings.yaml"), expanded, 0o644))

		vs, err := LoadVersionedSettings(filepath.Join(tmpDir, "no-project", ".scion"))
		require.NoError(t, err)
		require.NotNil(t, vs.Runtimes["substrate-prod"].Substrate)
		assert.Equal(t, "", vs.Runtimes["substrate-prod"].Substrate.SnapshotStorage)
	})
}
