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
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

func validCloudRunConfig() *CloudRunConfig {
	return &CloudRunConfig{
		ProjectID:      "my-project",
		Location:       "us-central1",
		ServiceAccount: "agent-sa@my-project.iam.gserviceaccount.com",
		Network:        "projects/my-project/global/networks/agents-vpc",
		Subnetwork:     "projects/my-project/regions/us-central1/subnetworks/agents-subnet",
	}
}

func findFinding(findings []ConversionFinding, code, path string) *ConversionFinding {
	for i := range findings {
		if findings[i].Code == code && findings[i].Path == path {
			return &findings[i]
		}
	}
	return nil
}

func TestProfileConversion_CloudRunMapped(t *testing.T) {
	vs := &VersionedSettings{
		Runtimes: map[string]V1RuntimeConfig{
			"cr": {
				Type:     "cloudrun",
				CloudRun: validCloudRunConfig(),
			},
		},
		Profiles: map[string]V1ProfileConfig{
			"prod": {
				Runtime:              "cr",
				DefaultHarnessConfig: "gemini",
				ImageRegistry:        "us-central1-docker.pkg.dev/my-project/scion",
				CloneDepth:           "full",
				Resources: &api.ResourceSpec{
					Requests: api.ResourceList{CPU: "2", Memory: "4Gi"},
				},
				Secrets: []api.RequiredSecret{{Key: "GEMINI_API_KEY", Type: "environment"}},
				HarnessOverrides: map[string]V1HarnessOverride{
					"gemini": {Image: "scion-gemini:v1"},
				},
			},
		},
	}

	got := ConvertProfilesToInstances(ProfileConversionInput{Settings: vs})
	if len(got.Instances) != 1 {
		t.Fatalf("instances = %+v, want 1", got.Instances)
	}
	inst := got.Instances[0]
	if inst.Key != "cloudrun-my-project-us-central1" || inst.Name != "cloudrun-my-project-us-central1" {
		t.Fatalf("instance key/name = %q/%q, want cloudrun-my-project-us-central1", inst.Key, inst.Name)
	}
	wantTarget := &V1RuntimeTargetConfig{
		Type:           RuntimeTargetTypeCloudRun,
		ProjectID:      "my-project",
		Region:         "us-central1",
		ServiceAccount: "agent-sa@my-project.iam.gserviceaccount.com",
		Network:        "projects/my-project/global/networks/agents-vpc",
		Subnetwork:     "projects/my-project/regions/us-central1/subnetworks/agents-subnet",
	}
	if !reflect.DeepEqual(inst.RuntimeTarget, wantTarget) {
		t.Fatalf("runtime_target = %+v, want %+v", inst.RuntimeTarget, wantTarget)
	}
	if inst.AgentDefaults == nil || inst.AgentDefaults.DefaultHarnessConfig != "gemini" ||
		inst.AgentDefaults.ImageRegistry != "us-central1-docker.pkg.dev/my-project/scion" ||
		inst.AgentDefaults.CloneDepth != "full" {
		t.Fatalf("agent_defaults = %+v", inst.AgentDefaults)
	}
	if wantMap := map[string]string{"prod": "cloudrun-my-project-us-central1"}; !reflect.DeepEqual(got.ProfileMap, wantMap) {
		t.Fatalf("profile map = %v, want %v (findings: %+v)", got.ProfileMap, wantMap, got.Findings)
	}
	if len(got.Findings) != 1 {
		t.Fatalf("findings = %+v, want only credentials_file operator-input notice", got.Findings)
	}
	assertFlatEquivalent(t, vs, "prod", inst)
}

func TestProfileConversion_CloudRunLocationToRegion(t *testing.T) {
	c1 := validCloudRunConfig()
	c1.Location = "europe-west1"
	c1.Subnetwork = ""
	c2 := validCloudRunConfig()
	c2.Location = "europe-west1"
	c2.Subnetwork = ""

	vs := &VersionedSettings{
		Runtimes: map[string]V1RuntimeConfig{
			"cr-a": {Type: "cloudrun", CloudRun: c1},
			"cr-b": {Type: "cloudrun", CloudRun: c2},
		},
		Profiles: map[string]V1ProfileConfig{
			"a": {Runtime: "cr-a"},
			"b": {Runtime: "cr-b"},
		},
	}

	got := ConvertProfilesToInstances(ProfileConversionInput{Settings: vs})
	if len(got.Instances) != 1 {
		t.Fatalf("instances = %+v, want both profiles grouped into 1 instance", got.Instances)
	}
	if got.Instances[0].Key != "cloudrun-my-project-europe-west1" {
		t.Fatalf("key = %q, want cloudrun-my-project-europe-west1", got.Instances[0].Key)
	}
	if got.Instances[0].RuntimeTarget.Region != "europe-west1" {
		t.Fatalf("region = %q, want europe-west1", got.Instances[0].RuntimeTarget.Region)
	}
	wantMap := map[string]string{
		"a": "cloudrun-my-project-europe-west1",
		"b": "cloudrun-my-project-europe-west1",
	}
	if !reflect.DeepEqual(got.ProfileMap, wantMap) {
		t.Fatalf("profile map = %v, want %v", got.ProfileMap, wantMap)
	}
}

func TestProfileConversion_CloudRunInstancesFieldRequired(t *testing.T) {
	t.Run("cloudrun-instances missing service_account and network", func(t *testing.T) {
		vs := &VersionedSettings{
			Runtimes: map[string]V1RuntimeConfig{
				"cri": {
					Type: "cloudrun-instances",
					CloudRunInstances: &V1CloudRunInstancesConfig{
						ProjectID: "my-project",
						Region:    "us-west1",
					},
				},
			},
			Profiles: map[string]V1ProfileConfig{
				"p": {Runtime: "cri"},
			},
		}
		got := ConvertProfilesToInstances(ProfileConversionInput{Settings: vs})
		if len(got.Instances) != 1 {
			t.Fatalf("instances = %+v, want proposed instance even when fields are missing", got.Instances)
		}
		inst := got.Instances[0]
		if inst.Key != "cloudrun-my-project-us-west1" || inst.RuntimeTarget.ProjectID != "my-project" || inst.RuntimeTarget.Region != "us-west1" {
			t.Fatalf("proposed instance = %+v", inst)
		}
		if len(got.ProfileMap) != 0 {
			t.Fatalf("profile map = %v, want empty for blocked profile", got.ProfileMap)
		}
		for _, path := range []string{
			"runtimes.cri.cloudrun.service_account",
			"runtimes.cri.cloudrun.network",
		} {
			f := findFinding(got.Findings, ConversionFlatTargetFieldRequired, path)
			if f == nil || f.Severity != ConversionSeverityBlocking || !reflect.DeepEqual(f.Profiles, []string{"p"}) {
				t.Fatalf("missing blocking flat_target_field_required at %s: %+v", path, got.Findings)
			}
		}
	})

	t.Run("cloudrun-instances missing project_id and region", func(t *testing.T) {
		vs := &VersionedSettings{
			Runtimes: map[string]V1RuntimeConfig{
				"cri": {Type: "cloudrun-instances", CloudRunInstances: &V1CloudRunInstancesConfig{}},
			},
			Profiles: map[string]V1ProfileConfig{"p": {Runtime: "cri"}},
		}
		got := ConvertProfilesToInstances(ProfileConversionInput{Settings: vs})
		if len(got.Instances) != 1 || len(got.ProfileMap) != 0 {
			t.Fatalf("instances = %+v, profile map = %v", got.Instances, got.ProfileMap)
		}
		for _, path := range []string{
			"runtimes.cri.cloudrun_instances.project_id",
			"runtimes.cri.cloudrun_instances.region",
			"runtimes.cri.cloudrun.service_account",
			"runtimes.cri.cloudrun.network",
		} {
			f := findFinding(got.Findings, ConversionFlatTargetFieldRequired, path)
			if f == nil || f.Severity != ConversionSeverityBlocking {
				t.Fatalf("missing blocking flat_target_field_required at %s: %+v", path, got.Findings)
			}
		}
	})

	t.Run("cloudrun missing each required field individually", func(t *testing.T) {
		for name, tc := range map[string]struct {
			mutate   func(*CloudRunConfig)
			wantPath string
		}{
			"missing project_id": {
				mutate:   func(c *CloudRunConfig) { c.ProjectID = "" },
				wantPath: "runtimes.cr.cloudrun.project_id",
			},
			"missing location": {
				mutate:   func(c *CloudRunConfig) { c.Location = "" },
				wantPath: "runtimes.cr.cloudrun.location",
			},
			"missing service_account": {
				mutate:   func(c *CloudRunConfig) { c.ServiceAccount = "" },
				wantPath: "runtimes.cr.cloudrun.service_account",
			},
			"missing network and subnetwork": {
				mutate:   func(c *CloudRunConfig) { c.Network = ""; c.Subnetwork = "" },
				wantPath: "runtimes.cr.cloudrun.network",
			},
		} {
			t.Run(name, func(t *testing.T) {
				cfg := validCloudRunConfig()
				tc.mutate(cfg)
				vs := &VersionedSettings{
					Runtimes: map[string]V1RuntimeConfig{"cr": {Type: "cloudrun", CloudRun: cfg}},
					Profiles: map[string]V1ProfileConfig{"p": {Runtime: "cr"}},
				}
				got := ConvertProfilesToInstances(ProfileConversionInput{Settings: vs})
				if len(got.Instances) != 1 {
					t.Fatalf("instances = %+v, want 1 proposed instance", got.Instances)
				}
				if len(got.ProfileMap) != 0 {
					t.Fatalf("profile map = %v, want empty", got.ProfileMap)
				}
				f := findFinding(got.Findings, ConversionFlatTargetFieldRequired, tc.wantPath)
				if f == nil || f.Severity != ConversionSeverityBlocking {
					t.Fatalf("want blocking flat_target_field_required at %s, got %+v", tc.wantPath, got.Findings)
				}
			})
		}
	})

	t.Run("subnetwork alone satisfies VPC requirement", func(t *testing.T) {
		cfg := validCloudRunConfig()
		cfg.Network = ""
		vs := &VersionedSettings{
			Runtimes: map[string]V1RuntimeConfig{"cr": {Type: "cloudrun", CloudRun: cfg}},
			Profiles: map[string]V1ProfileConfig{"p": {Runtime: "cr"}},
		}
		got := ConvertProfilesToInstances(ProfileConversionInput{Settings: vs})
		if got.ProfileMap["p"] != "cloudrun-my-project-us-central1" {
			t.Fatalf("profile map = %v, findings = %+v", got.ProfileMap, got.Findings)
		}
	})
}

func TestProfileConversion_CloudRunDefaultSARequired(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*CloudRunConfig)
		wantPath string
		wantText string
	}{
		{
			name:     "compute default service account",
			mutate:   func(c *CloudRunConfig) { c.ServiceAccount = "123456789012-compute@developer.gserviceaccount.com" },
			wantPath: "runtimes.cr.cloudrun.service_account",
			wantText: "the project default service accounts are not allowed",
		},
		{
			name:     "compute default service account mixed case",
			mutate:   func(c *CloudRunConfig) { c.ServiceAccount = "123456789012-Compute@Developer.GServiceAccount.com" },
			wantPath: "runtimes.cr.cloudrun.service_account",
			wantText: "the project default service accounts are not allowed",
		},
		{
			name:     "app engine default service account",
			mutate:   func(c *CloudRunConfig) { c.ServiceAccount = "my-project@appspot.gserviceaccount.com" },
			wantPath: "runtimes.cr.cloudrun.service_account",
			wantText: "the project default service accounts are not allowed",
		},
		{
			name:     "app engine default service account mixed case",
			mutate:   func(c *CloudRunConfig) { c.ServiceAccount = "My-Project@AppSpot.GServiceAccount.com" },
			wantPath: "runtimes.cr.cloudrun.service_account",
			wantText: "the project default service accounts are not allowed",
		},
		{
			name:     "google service agent local part",
			mutate:   func(c *CloudRunConfig) { c.ServiceAccount = "service-123456@my-project.iam.gserviceaccount.com" },
			wantPath: "runtimes.cr.cloudrun.service_account",
			wantText: "service_account must be a user-managed service account (name@project.iam.gserviceaccount.com)",
		},
		{
			name:     "google service agent domain",
			mutate:   func(c *CloudRunConfig) { c.ServiceAccount = "agent-sa@gcp-sa-run.iam.gserviceaccount.com" },
			wantPath: "runtimes.cr.cloudrun.service_account",
			wantText: "service_account must be a user-managed service account (name@project.iam.gserviceaccount.com)",
		},
		{
			name:     "malformed service account",
			mutate:   func(c *CloudRunConfig) { c.ServiceAccount = "not-a-service-account" },
			wantPath: "runtimes.cr.cloudrun.service_account",
			wantText: "service_account must be a user-managed service account (name@project.iam.gserviceaccount.com)",
		},
		{
			name:     "domain-scoped project_id",
			mutate:   func(c *CloudRunConfig) { c.ProjectID = "example.com:my-proj" },
			wantPath: "runtimes.cr.cloudrun.project_id",
			wantText: "domain-scoped project IDs are not supported",
		},
		{
			name:     "invalid region",
			mutate:   func(c *CloudRunConfig) { c.Location = "INVALID_REGION" },
			wantPath: "runtimes.cr.cloudrun.location",
			wantText: "region must be a Cloud Run region",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validCloudRunConfig()
			tc.mutate(cfg)
			vs := &VersionedSettings{
				Runtimes: map[string]V1RuntimeConfig{"cr": {Type: "cloudrun", CloudRun: cfg}},
				Profiles: map[string]V1ProfileConfig{"p": {Runtime: "cr"}},
			}
			got := ConvertProfilesToInstances(ProfileConversionInput{Settings: vs})
			if len(got.Instances) != 1 {
				t.Fatalf("instances = %+v, want proposed instance", got.Instances)
			}
			if !runtimeBrokerInstanceKeyRe.MatchString(got.Instances[0].Key) {
				t.Fatalf("proposed instance key %q does not match key pattern", got.Instances[0].Key)
			}
			if len(got.ProfileMap) != 0 {
				t.Fatalf("profile map = %v, want empty", got.ProfileMap)
			}
			f := findFinding(got.Findings, ConversionFlatTargetFieldRequired, tc.wantPath)
			if f == nil || f.Severity != ConversionSeverityBlocking {
				t.Fatalf("want blocking flat_target_field_required at %s, got %+v", tc.wantPath, got.Findings)
			}
			if !strings.Contains(f.Message, tc.wantText) {
				t.Fatalf("message %q does not contain %q", f.Message, tc.wantText)
			}
		})
	}
}

func TestProfileConversion_CloudRunSandboxStillBlocking(t *testing.T) {
	vs := &VersionedSettings{
		Runtimes: map[string]V1RuntimeConfig{
			"runs": {
				Type:            "cloudrun-sandbox",
				CloudRunSandbox: &V1CloudRunSandboxConfig{SandboxBin: "/usr/local/gcp/bin/sandbox"},
			},
			"dock-sandbox": {
				Type:            "docker",
				CloudRunSandbox: &V1CloudRunSandboxConfig{},
			},
			"cr-sandbox": {
				Type:            "cloudrun",
				CloudRun:        validCloudRunConfig(),
				CloudRunSandbox: &V1CloudRunSandboxConfig{},
			},
		},
		Profiles: map[string]V1ProfileConfig{
			"p-sandbox":         {Runtime: "runs"},
			"p-implicit":        {Runtime: "cloudrun-sandbox"},
			"p-docker-sandbox":  {Runtime: "dock-sandbox"},
			"p-cloudrun-sbxblk": {Runtime: "cr-sandbox"},
		},
	}

	got := ConvertProfilesToInstances(ProfileConversionInput{Settings: vs})
	if len(got.Instances) != 0 {
		t.Fatalf("instances = %+v, want none", got.Instances)
	}
	if len(got.ProfileMap) != 0 {
		t.Fatalf("profile map = %v, want empty", got.ProfileMap)
	}
	for _, path := range []string{
		"profiles.p-implicit.runtime",
		"runtimes.cr-sandbox.cloudrun_sandbox",
		"runtimes.dock-sandbox.cloudrun_sandbox",
		"runtimes.runs.type",
		"runtimes.runs.cloudrun_sandbox",
	} {
		f := findFinding(got.Findings, ConversionUnsupportedFlatTargetType, path)
		if f == nil || f.Severity != ConversionSeverityBlocking {
			t.Fatalf("missing blocking unsupported_flat_target_type at %s: %+v", path, got.Findings)
		}
	}
}

func TestProfileConversion_CloudRunBlockOnDockerNotice(t *testing.T) {
	vs := &VersionedSettings{
		Runtimes: map[string]V1RuntimeConfig{
			"dock": {
				Type:              "docker",
				CloudRun:          validCloudRunConfig(),
				CloudRunInstances: &V1CloudRunInstancesConfig{ProjectID: "other-proj", Region: "us-east1"},
			},
			"k8s": {
				Type:              "kubernetes",
				Namespace:         "agents",
				CloudRun:          validCloudRunConfig(),
				CloudRunInstances: &V1CloudRunInstancesConfig{ProjectID: "other-proj", Region: "us-east1"},
			},
			"cr-with-k8s": {
				Type:              "cloudrun",
				Namespace:         "ignored-ns",
				CloudRun:          validCloudRunConfig(),
				CloudRunInstances: &V1CloudRunInstancesConfig{ProjectID: "ignored-proj", Region: "us-east1"},
			},
		},
		Profiles: map[string]V1ProfileConfig{
			"p-dock": {Runtime: "dock"},
			"p-k8s":  {Runtime: "k8s"},
			"p-cr":   {Runtime: "cr-with-k8s", SharedDirSize: "20Gi"},
		},
	}

	got := ConvertProfilesToInstances(ProfileConversionInput{Settings: vs})
	wantMap := map[string]string{
		"p-dock": "docker",
		"p-k8s":  "k8s-agents",
		"p-cr":   "cloudrun-my-project-us-central1",
	}
	if !reflect.DeepEqual(got.ProfileMap, wantMap) {
		t.Fatalf("profile map = %v, want %v (findings: %+v)", got.ProfileMap, wantMap, got.Findings)
	}

	for _, inst := range got.Instances {
		switch inst.RuntimeTarget.Type {
		case RuntimeTargetTypeDocker, RuntimeTargetTypeKubernetes:
			if len(cloudRunOnlyFieldsSet(inst.RuntimeTarget)) != 0 {
				t.Fatalf("%s instance carries Cloud Run fields: %+v", inst.Key, inst.RuntimeTarget)
			}
		case RuntimeTargetTypeCloudRun:
			if len(kubernetesOnlyFieldsSet(inst.RuntimeTarget)) != 0 {
				t.Fatalf("cloudrun instance carries Kubernetes fields: %+v", inst.RuntimeTarget)
			}
			if inst.RuntimeTarget.ProjectID != "my-project" || inst.RuntimeTarget.Region != "us-central1" {
				t.Fatalf("cloudrun instance picked up cloudrun_instances block: %+v", inst.RuntimeTarget)
			}
		}
	}

	for _, path := range []string{
		"runtimes.dock.cloudrun",
		"runtimes.dock.cloudrun_instances",
		"runtimes.k8s.cloudrun",
		"runtimes.k8s.cloudrun_instances",
		"runtimes.cr-with-k8s.cloudrun_instances",
		"runtimes.cr-with-k8s.namespace",
		"profiles.p-cr.shared_dir_size",
	} {
		f := findFinding(got.Findings, ConversionFieldNotValidForTargetType, path)
		if f == nil || f.Severity != ConversionSeverityNotice {
			t.Fatalf("missing notice field_not_valid_for_target_type at %s: %+v", path, got.Findings)
		}
	}
}

func TestProfileConversion_CloudRunNFSFieldsNoEffect(t *testing.T) {
	cfg := validCloudRunConfig()
	cfg.NFSServer = "10.0.0.2"
	cfg.NFSExport = "/vol1"

	vs := &VersionedSettings{
		Runtimes: map[string]V1RuntimeConfig{"cr": {Type: "cloudrun", CloudRun: cfg}},
		Profiles: map[string]V1ProfileConfig{"p": {Runtime: "cr"}},
	}

	got := ConvertProfilesToInstances(ProfileConversionInput{Settings: vs})
	if got.ProfileMap["p"] != "cloudrun-my-project-us-central1" {
		t.Fatalf("profile map = %v, findings = %+v", got.ProfileMap, got.Findings)
	}
	for _, path := range []string{
		"runtimes.cr.cloudrun.nfs_server",
		"runtimes.cr.cloudrun.nfs_export",
	} {
		f := findFinding(got.Findings, ConversionLegacyFieldNoEffect, path)
		if f == nil || f.Severity != ConversionSeverityNotice {
			t.Fatalf("missing notice legacy_field_no_effect at %s: %+v", path, got.Findings)
		}
	}
}

func TestProfileConversion_CloudRunSplitBySA(t *testing.T) {
	for name, tc := range map[string]struct {
		mutateB   func(*CloudRunConfig)
		wantField string
	}{
		"differing service_account": {
			mutateB:   func(c *CloudRunConfig) { c.ServiceAccount = "other-sa@my-project.iam.gserviceaccount.com" },
			wantField: "service_account",
		},
		"differing network": {
			mutateB:   func(c *CloudRunConfig) { c.Network = "projects/my-project/global/networks/other-vpc" },
			wantField: "network",
		},
		"differing subnetwork": {
			mutateB: func(c *CloudRunConfig) {
				c.Subnetwork = "projects/my-project/regions/us-central1/subnetworks/other-subnet"
			},
			wantField: "subnetwork",
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfgA := validCloudRunConfig()
			cfgB := validCloudRunConfig()
			tc.mutateB(cfgB)

			vs := &VersionedSettings{
				Runtimes: map[string]V1RuntimeConfig{
					"cr-a": {Type: "cloudrun", CloudRun: cfgA},
					"cr-b": {Type: "cloudrun", CloudRun: cfgB},
				},
				Profiles: map[string]V1ProfileConfig{
					"alpha": {Runtime: "cr-a"},
					"beta":  {Runtime: "cr-b"},
				},
			}

			got := ConvertProfilesToInstances(ProfileConversionInput{Settings: vs})
			if len(got.Instances) != 2 {
				t.Fatalf("instances = %+v, want 2 split instances", got.Instances)
			}
			wantMap := map[string]string{
				"alpha": "cloudrun-my-project-us-central1-alpha",
				"beta":  "cloudrun-my-project-us-central1-beta",
			}
			if !reflect.DeepEqual(got.ProfileMap, wantMap) {
				t.Fatalf("profile map = %v, want %v", got.ProfileMap, wantMap)
			}
			split := findFinding(got.Findings, ConversionProfileConflict, "profiles.*")
			if split == nil || split.Severity != ConversionSeverityNotice || !strings.Contains(split.Message, tc.wantField) {
				t.Fatalf("want profile_conflict notice naming %s, got %+v", tc.wantField, got.Findings)
			}

			// A profile field choice consolidates the group onto one instance.
			merged := ConvertProfilesToInstances(ProfileConversionInput{
				Settings: vs,
				Choices: []ConversionChoice{{
					TargetGroup:   "cloudrun-my-project-us-central1",
					Field:         tc.wantField,
					ChosenProfile: "alpha",
				}},
			})
			if len(merged.Instances) != 1 || merged.Instances[0].Key != "cloudrun-my-project-us-central1" {
				t.Fatalf("merged instances = %+v", merged.Instances)
			}
			if f := findFinding(merged.Findings, ConversionProfileConflictResolved, "profiles.*."+tc.wantField); f == nil {
				t.Fatalf("missing profile_conflict_resolved for %s: %+v", tc.wantField, merged.Findings)
			}

			// A template choice is refused because Cloud Run target fields are infrastructure fields.
			tmplRefused := ConvertProfilesToInstances(ProfileConversionInput{
				Settings: vs,
				Choices: []ConversionChoice{{
					TargetGroup:   "cloudrun-my-project-us-central1",
					Field:         tc.wantField,
					ChosenProfile: ConversionChoiceTemplatePrefix + "default",
				}},
			})
			if f := findFinding(tmplRefused.Findings, ConversionUnknownSettingsKey, "choices[0]"); f == nil || f.Severity != ConversionSeverityBlocking {
				t.Fatalf("want blocking refusal for template choice on %s, got %+v", tc.wantField, tmplRefused.Findings)
			}
		})
	}
}

func TestProfileConversion_CloudRunCredentialsFileNotice(t *testing.T) {
	cfgEast := validCloudRunConfig()
	cfgEast.Location = "us-east1"
	cfgEast.Subnetwork = ""
	cfgWest := validCloudRunConfig()
	cfgWest.Location = "us-west1"
	cfgWest.Subnetwork = ""

	vs := &VersionedSettings{
		Runtimes: map[string]V1RuntimeConfig{
			"dock":    {Type: "docker"},
			"cr-east": {Type: "cloudrun", CloudRun: cfgEast},
			"cr-west": {Type: "cloudrun", CloudRun: cfgWest},
		},
		Profiles: map[string]V1ProfileConfig{
			"p-dock": {Runtime: "dock"},
			"p-east": {Runtime: "cr-east"},
			"p-west": {Runtime: "cr-west"},
		},
	}

	got := ConvertProfilesToInstances(ProfileConversionInput{Settings: vs})
	if len(got.Instances) != 3 {
		t.Fatalf("instances = %+v, want 3", got.Instances)
	}
	// Sorted by key:
	// [0] cloudrun-my-project-us-east1
	// [1] cloudrun-my-project-us-west1
	// [2] docker
	wantMap := map[string]string{
		"p-east": "cloudrun-my-project-us-east1",
		"p-west": "cloudrun-my-project-us-west1",
		"p-dock": "docker",
	}
	if !reflect.DeepEqual(got.ProfileMap, wantMap) {
		t.Fatalf("profile map = %v, want %v", got.ProfileMap, wantMap)
	}
	for i, prof := range []string{"p-east", "p-west"} {
		path := fmt.Sprintf("server.broker.instances[%d].runtime_target.credentials_file", i)
		f := findFinding(got.Findings, ConversionFlatTargetOperatorInputRequired, path)
		if f == nil || f.Severity != ConversionSeverityNotice || !reflect.DeepEqual(f.Profiles, []string{prof}) {
			t.Fatalf("want notice flat_target_operator_input_required at %s for %s, got %+v", path, prof, got.Findings)
		}
	}
	if f := findFinding(got.Findings, ConversionFlatTargetOperatorInputRequired, "server.broker.instances[2].runtime_target.credentials_file"); f != nil {
		t.Fatalf("docker instance must not get credentials_file notice: %+v", f)
	}
}

func TestProfileConversion_CloudRunIdentityKeysBlocking(t *testing.T) {
	t.Run("runtime entry assignable_service_accounts", func(t *testing.T) {
		cfg := validCloudRunConfig()
		cfg.AssignableServiceAccounts = []string{"worker-sa@my-project.iam.gserviceaccount.com"}
		vs := &VersionedSettings{
			Runtimes: map[string]V1RuntimeConfig{"cr": {Type: "cloudrun", CloudRun: cfg}},
			Profiles: map[string]V1ProfileConfig{"p": {Runtime: "cr"}},
		}
		got := ConvertProfilesToInstances(ProfileConversionInput{Settings: vs})
		if len(got.ProfileMap) != 0 {
			t.Fatalf("profile map = %v, want empty", got.ProfileMap)
		}
		const wantPath = "runtimes.cr.cloudrun.assignable_service_accounts"
		const wantMsg = "runtimes.cr.cloudrun.assignable_service_accounts cannot be carried to a flat cloudrun instance yet (ptone/scion#7764); remove it from the legacy entry or wait for flat identity support"
		f := findFinding(got.Findings, ConversionFieldNotValidForTargetType, wantPath)
		if f == nil || f.Severity != ConversionSeverityBlocking || f.Message != wantMsg {
			t.Fatalf("got finding %+v, want blocking %q at %s (all findings: %+v)", f, wantMsg, wantPath, got.Findings)
		}
	})

	t.Run("runtime entry block_service_account", func(t *testing.T) {
		cfg := validCloudRunConfig()
		cfg.BlockServiceAccount = "block-sa@my-project.iam.gserviceaccount.com"
		vs := &VersionedSettings{
			Runtimes: map[string]V1RuntimeConfig{"cr": {Type: "cloudrun", CloudRun: cfg}},
			Profiles: map[string]V1ProfileConfig{"p": {Runtime: "cr"}},
		}
		got := ConvertProfilesToInstances(ProfileConversionInput{Settings: vs})
		if len(got.ProfileMap) != 0 {
			t.Fatalf("profile map = %v, want empty", got.ProfileMap)
		}
		const wantPath = "runtimes.cr.cloudrun.block_service_account"
		const wantMsg = "runtimes.cr.cloudrun.block_service_account cannot be carried to a flat cloudrun instance yet (ptone/scion#7764); remove it from the legacy entry or wait for flat identity support"
		f := findFinding(got.Findings, ConversionFieldNotValidForTargetType, wantPath)
		if f == nil || f.Severity != ConversionSeverityBlocking || f.Message != wantMsg {
			t.Fatalf("got finding %+v, want blocking %q at %s (all findings: %+v)", f, wantMsg, wantPath, got.Findings)
		}
	})

	// V1ProfileConfig has no CloudRun block; if a profile in raw settings sets
	// cloudrun.assignable_service_accounts, cloudrun.block_service_account, or
	// either identity key directly under profiles.<p>, conversion still blocks
	// with the profile path named and omits the ProfileMap entry.
	for name, tc := range map[string]struct {
		yaml     string
		wantCode string
		wantPath string
		wantMsg  string
	}{
		"profile cloudrun.assignable_service_accounts": {
			yaml: `schema_version: "1"
runtimes:
  cr:
    type: cloudrun
    cloudrun:
      project_id: my-project
      location: us-central1
      service_account: agent-sa@my-project.iam.gserviceaccount.com
      network: projects/my-project/global/networks/agents-vpc
profiles:
  p:
    runtime: cr
    cloudrun:
      assignable_service_accounts:
        - worker-sa@my-project.iam.gserviceaccount.com
`,
			wantCode: ConversionFieldNotValidForTargetType,
			wantPath: "profiles.p.cloudrun.assignable_service_accounts",
			wantMsg:  "profiles.p.cloudrun.assignable_service_accounts cannot be carried to a flat cloudrun instance yet (ptone/scion#7764); remove it from the legacy entry or wait for flat identity support",
		},
		"profile cloudrun.block_service_account": {
			yaml: `schema_version: "1"
runtimes:
  cr:
    type: cloudrun
    cloudrun:
      project_id: my-project
      location: us-central1
      service_account: agent-sa@my-project.iam.gserviceaccount.com
      network: projects/my-project/global/networks/agents-vpc
profiles:
  p:
    runtime: cr
    cloudrun:
      block_service_account: block-sa@my-project.iam.gserviceaccount.com
`,
			wantCode: ConversionFieldNotValidForTargetType,
			wantPath: "profiles.p.cloudrun.block_service_account",
			wantMsg:  "profiles.p.cloudrun.block_service_account cannot be carried to a flat cloudrun instance yet (ptone/scion#7764); remove it from the legacy entry or wait for flat identity support",
		},
	} {
		t.Run(name, func(t *testing.T) {
			vs := &VersionedSettings{}
			if err := yamlv3.Unmarshal([]byte(tc.yaml), vs); err != nil {
				t.Fatal(err)
			}
			got := ConvertProfilesToInstances(ProfileConversionInput{Settings: vs, RawSettings: []byte(tc.yaml)})
			if len(got.ProfileMap) != 0 {
				t.Fatalf("profile map = %v, want empty", got.ProfileMap)
			}
			f := findFinding(got.Findings, tc.wantCode, tc.wantPath)
			if f == nil || f.Severity != ConversionSeverityBlocking || f.Message != tc.wantMsg {
				t.Fatalf("got finding %+v, want blocking %s %q at %s (all findings: %+v)", f, tc.wantCode, tc.wantMsg, tc.wantPath, got.Findings)
			}
		})
	}
}

func TestProfileConversion_CloudRunRoundTripWithCredentialsFile(t *testing.T) {
	const rawYAML = `schema_version: "1"
runtimes:
  cr:
    type: cloudrun
    cloudrun:
      project_id: my-project
      location: us-central1
      service_account: agent-sa@my-project.iam.gserviceaccount.com
      network: projects/my-project/global/networks/agents-vpc
      subnetwork: projects/my-project/regions/us-central1/subnetworks/agents-subnet
profiles:
  prod:
    runtime: cr
    default_harness_config: gemini
    clone_depth: full
`
	vs := &VersionedSettings{}
	if err := yamlv3.Unmarshal([]byte(rawYAML), vs); err != nil {
		t.Fatal(err)
	}
	got := ConvertProfilesToInstances(ProfileConversionInput{Settings: vs, RawSettings: []byte(rawYAML)})
	if len(got.Instances) != 1 {
		t.Fatalf("instances = %+v, want 1", got.Instances)
	}

	// Without credentials_file, static validation flags credentials_file as required.
	if errs := ValidateRuntimeBrokerInstances(got.Instances); len(errs) != 1 || errs[0].Path != "server.broker.instances[0].runtime_target.credentials_file" {
		t.Fatalf("unfilled validation errors = %+v, want credentials_file required", errs)
	}

	// Once the operator fills credentials_file in the proposed YAML, both
	// ValidateRuntimeBrokerInstances and strict LoadRuntimeBrokerInstances succeed.
	filled := append([]V1RuntimeBrokerInstanceConfig(nil), got.Instances...)
	rtCopy := *filled[0].RuntimeTarget
	rtCopy.CredentialsFile = "/etc/scion/cloudrun-credentials.json"
	filled[0].RuntimeTarget = &rtCopy

	if errs := ValidateRuntimeBrokerInstances(filled); len(errs) != 0 {
		t.Fatalf("ValidateRuntimeBrokerInstances(filled) = %+v, want none", errs)
	}

	var doc map[string]interface{}
	if err := yamlv3.Unmarshal([]byte(rawYAML), &doc); err != nil {
		t.Fatal(err)
	}
	instBytes, err := yamlv3.Marshal(filled)
	if err != nil {
		t.Fatal(err)
	}
	var instNode interface{}
	if err := yamlv3.Unmarshal(instBytes, &instNode); err != nil {
		t.Fatal(err)
	}
	doc["server"] = map[string]interface{}{
		"broker": map[string]interface{}{
			"instances": instNode,
		},
	}
	out, err := yamlv3.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}

	global := flatTestHome(t)
	writeFlatTestFile(t, filepath.Join(global, "settings.yaml"), string(out))
	loaded, err := LoadRuntimeBrokerInstances("")
	if err != nil {
		t.Fatalf("LoadRuntimeBrokerInstances: %v\n%s", err, out)
	}
	if !reflect.DeepEqual(loaded, filled) {
		t.Fatalf("strict load = %+v, want %+v", loaded, filled)
	}
}
