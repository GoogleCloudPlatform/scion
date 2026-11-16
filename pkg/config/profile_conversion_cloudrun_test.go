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
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

func validCloudRunConfig() *CloudRunConfig {
	return &CloudRunConfig{
		ProjectID:           "my-project",
		Location:            "us-central1",
		ServiceAccount:      "agent-sa@my-project.iam.gserviceaccount.com",
		BlockServiceAccount: "no-role-block@my-project.iam.gserviceaccount.com",
		Network:             "projects/my-project/global/networks/agents-vpc",
		Subnetwork:          "projects/my-project/regions/us-central1/subnetworks/agents-subnet",
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
	crCfg := validCloudRunConfig()
	crCfg.AssignableServiceAccounts = []string{
		"worker-one@my-project.iam.gserviceaccount.com",
		"worker-two@my-project.iam.gserviceaccount.com",
	}
	vs := &VersionedSettings{
		Runtimes: map[string]V1RuntimeConfig{
			"cr": {
				Type:     "cloudrun",
				CloudRun: crCfg,
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
	if inst.Key != "cloudrun-us-central1" || inst.Name != "cloudrun-us-central1" {
		t.Fatalf("instance key/name = %q/%q, want cloudrun-us-central1", inst.Key, inst.Name)
	}
	wantTarget := &V1RuntimeTargetConfig{
		Type:                RuntimeTargetTypeCloudRun,
		ProjectID:           "my-project",
		Region:              "us-central1",
		ServiceAccount:      "agent-sa@my-project.iam.gserviceaccount.com",
		BlockServiceAccount: "no-role-block@my-project.iam.gserviceaccount.com",
		AssignableServiceAccounts: []string{
			"worker-one@my-project.iam.gserviceaccount.com",
			"worker-two@my-project.iam.gserviceaccount.com",
		},
		Network:    "projects/my-project/global/networks/agents-vpc",
		Subnetwork: "projects/my-project/regions/us-central1/subnetworks/agents-subnet",
	}
	if !reflect.DeepEqual(inst.RuntimeTarget, wantTarget) {
		t.Fatalf("runtime_target = %+v, want %+v", inst.RuntimeTarget, wantTarget)
	}
	if &inst.RuntimeTarget.AssignableServiceAccounts[0] == &crCfg.AssignableServiceAccounts[0] {
		t.Fatal("assignable_service_accounts shares memory with legacy CloudRunConfig")
	}
	if inst.AgentDefaults == nil || inst.AgentDefaults.DefaultHarnessConfig != "gemini" ||
		inst.AgentDefaults.ImageRegistry != "us-central1-docker.pkg.dev/my-project/scion" ||
		inst.AgentDefaults.CloneDepth != "full" {
		t.Fatalf("agent_defaults = %+v", inst.AgentDefaults)
	}
	if wantMap := map[string]string{"prod": "cloudrun-us-central1"}; !reflect.DeepEqual(got.ProfileMap, wantMap) {
		t.Fatalf("profile map = %v, want %v (findings: %+v)", got.ProfileMap, wantMap, got.Findings)
	}
	if len(got.Findings) != 1 {
		t.Fatalf("findings = %+v, want only credentials_file operator-input notice", got.Findings)
	}
	credPath := "server.broker.instances[0].runtime_target.credentials_file"
	if f := findFinding(got.Findings, ConversionFlatTargetOperatorInputRequired, credPath); f == nil || f.Severity != ConversionSeverityNotice {
		t.Fatalf("missing operator-input notice at %s: %+v", credPath, got.Findings)
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
	if got.Instances[0].Key != "cloudrun-europe-west1" {
		t.Fatalf("key = %q, want cloudrun-europe-west1", got.Instances[0].Key)
	}
	if got.Instances[0].RuntimeTarget.Region != "europe-west1" {
		t.Fatalf("region = %q, want europe-west1", got.Instances[0].RuntimeTarget.Region)
	}
	wantMap := map[string]string{
		"a": "cloudrun-europe-west1",
		"b": "cloudrun-europe-west1",
	}
	if !reflect.DeepEqual(got.ProfileMap, wantMap) {
		t.Fatalf("profile map = %v, want %v", got.ProfileMap, wantMap)
	}
}

func TestProfileConversion_CloudRunInstancesFieldRequired(t *testing.T) {
	t.Run("valid cloudrun-instances keeps ProfileMap and emits operator-input notices", func(t *testing.T) {
		vs := &VersionedSettings{
			Runtimes: map[string]V1RuntimeConfig{
				"cri": {
					Type: "cloudrun-instances",
					CloudRunInstances: &V1CloudRunInstancesConfig{
						ProjectID: "my-project",
						Region:    "us-west1",
					},
					// A stray cloudrun block on cloudrun-instances stays a non-folded notice.
					CloudRun: func() *CloudRunConfig {
						c := validCloudRunConfig()
						c.AssignableServiceAccounts = []string{"worker-one@my-project.iam.gserviceaccount.com"}
						return c
					}(),
				},
			},
			Profiles: map[string]V1ProfileConfig{
				"p": {Runtime: "cri"},
			},
		}
		got := ConvertProfilesToInstances(ProfileConversionInput{Settings: vs})
		if len(got.Instances) != 1 {
			t.Fatalf("instances = %+v, want 1 proposed instance", got.Instances)
		}
		inst := got.Instances[0]
		if inst.Key != "cloudrun-us-west1" || inst.RuntimeTarget.ProjectID != "my-project" || inst.RuntimeTarget.Region != "us-west1" {
			t.Fatalf("proposed instance = %+v", inst)
		}
		if inst.RuntimeTarget.ServiceAccount != "" || inst.RuntimeTarget.BlockServiceAccount != "" ||
			len(inst.RuntimeTarget.AssignableServiceAccounts) != 0 || inst.RuntimeTarget.Network != "" || inst.RuntimeTarget.Subnetwork != "" {
			t.Fatalf("cloudrun-instances must not fold runtimes.cri.cloudrun fields: %+v", inst.RuntimeTarget)
		}
		if wantMap := map[string]string{"p": "cloudrun-us-west1"}; !reflect.DeepEqual(got.ProfileMap, wantMap) {
			t.Fatalf("profile map = %v, want %v (findings: %+v)", got.ProfileMap, wantMap, got.Findings)
		}
		for _, f := range got.Findings {
			if f.Severity == ConversionSeverityBlocking {
				t.Fatalf("unexpected blocking finding on valid cloudrun-instances: %+v", f)
			}
		}
		for _, path := range []string{
			"server.broker.instances[0].runtime_target.block_service_account",
			"server.broker.instances[0].runtime_target.credentials_file",
			"server.broker.instances[0].runtime_target.service_account",
			"server.broker.instances[0].runtime_target.network",
		} {
			f := findFinding(got.Findings, ConversionFlatTargetOperatorInputRequired, path)
			if f == nil || f.Severity != ConversionSeverityNotice || !reflect.DeepEqual(f.Profiles, []string{"p"}) {
				t.Fatalf("missing notice flat_target_operator_input_required at %s: %+v", path, got.Findings)
			}
		}
		netFinding := findFinding(got.Findings, ConversionFlatTargetOperatorInputRequired, "server.broker.instances[0].runtime_target.network")
		if !strings.Contains(netFinding.Message, "network or subnetwork") {
			t.Fatalf("network operator-input message = %q, want mention of network or subnetwork", netFinding.Message)
		}
		if f := findFinding(got.Findings, ConversionFieldNotValidForTargetType, "runtimes.cri.cloudrun"); f == nil || f.Severity != ConversionSeverityNotice {
			t.Fatalf("want notice field_not_valid_for_target_type at runtimes.cri.cloudrun, got %+v", got.Findings)
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
		if len(got.Instances) != 1 || got.Instances[0].Key != "cloudrun" || len(got.ProfileMap) != 0 {
			t.Fatalf("instances = %+v, profile map = %v", got.Instances, got.ProfileMap)
		}
		for _, path := range []string{
			"runtimes.cri.cloudrun_instances.project_id",
			"runtimes.cri.cloudrun_instances.region",
		} {
			f := findFinding(got.Findings, ConversionFlatTargetFieldRequired, path)
			if f == nil || f.Severity != ConversionSeverityBlocking {
				t.Fatalf("missing blocking flat_target_field_required at %s: %+v", path, got.Findings)
			}
		}
		for _, badPath := range []string{
			"runtimes.cri.cloudrun.service_account",
			"runtimes.cri.cloudrun.network",
		} {
			if f := findFinding(got.Findings, ConversionFlatTargetFieldRequired, badPath); f != nil {
				t.Fatalf("cloudrun-instances must not emit flat_target_field_required at %s: %+v", badPath, f)
			}
		}
	})

	t.Run("cloudrun-instances never merges with sibling cloudrun in same project and region", func(t *testing.T) {
		// Even when the cloudrun entry also has empty service_account and network,
		// cloudrun-instances stays in its own proposed instance, keeps its ProfileMap
		// entry, and never inherits a sibling's service_account or blocking finding.
		vs := &VersionedSettings{
			Runtimes: map[string]V1RuntimeConfig{
				"cr-empty": {
					Type: "cloudrun",
					CloudRun: &CloudRunConfig{
						ProjectID: "my-project",
						Location:  "us-central1",
					},
				},
				"cri": {
					Type: "cloudrun-instances",
					CloudRunInstances: &V1CloudRunInstancesConfig{
						ProjectID: "my-project",
						Region:    "us-central1",
					},
				},
			},
			Profiles: map[string]V1ProfileConfig{
				"p-cr":  {Runtime: "cr-empty"},
				"p-cri": {Runtime: "cri"},
			},
		}
		got := ConvertProfilesToInstances(ProfileConversionInput{Settings: vs})
		if len(got.Instances) != 2 {
			t.Fatalf("instances = %+v, want 2 separate instances", got.Instances)
		}
		if _, blocked := got.ProfileMap["p-cr"]; blocked {
			t.Fatalf("p-cr should have no ProfileMap entry: %v", got.ProfileMap)
		}
		criKey, ok := got.ProfileMap["p-cri"]
		if !ok || criKey == "" {
			t.Fatalf("p-cri should keep its ProfileMap entry, got %v (findings: %+v)", got.ProfileMap, got.Findings)
		}

		// Also verify with a valid sibling cloudrun entry that p-cri never adopts its service_account.
		vsValid := &VersionedSettings{
			Runtimes: map[string]V1RuntimeConfig{
				"cr":  {Type: "cloudrun", CloudRun: validCloudRunConfig()},
				"cri": vs.Runtimes["cri"],
			},
			Profiles: map[string]V1ProfileConfig{
				"p-cr":  {Runtime: "cr"},
				"p-cri": {Runtime: "cri"},
			},
		}
		gotValid := ConvertProfilesToInstances(ProfileConversionInput{Settings: vsValid})
		if len(gotValid.Instances) != 2 {
			t.Fatalf("instances = %+v, want 2 separate instances", gotValid.Instances)
		}
		if gotValid.ProfileMap["p-cr"] == gotValid.ProfileMap["p-cri"] {
			t.Fatalf("p-cr and p-cri merged onto same instance: %v", gotValid.ProfileMap)
		}
		for _, inst := range gotValid.Instances {
			if inst.Key == gotValid.ProfileMap["p-cri"] && inst.RuntimeTarget.ServiceAccount != "" {
				t.Fatalf("cloudrun-instances instance inherited sibling service_account: %+v", inst.RuntimeTarget)
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
			"missing block_service_account": {
				mutate:   func(c *CloudRunConfig) { c.BlockServiceAccount = "" },
				wantPath: "runtimes.cr.cloudrun.block_service_account",
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
		if got.ProfileMap["p"] != "cloudrun-us-central1" {
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
		{
			name:     "block_service_account compute default",
			mutate:   func(c *CloudRunConfig) { c.BlockServiceAccount = "123456789012-compute@developer.gserviceaccount.com" },
			wantPath: "runtimes.cr.cloudrun.block_service_account",
			wantText: "the project default service accounts are not allowed",
		},
		{
			name:     "block_service_account upper case",
			mutate:   func(c *CloudRunConfig) { c.BlockServiceAccount = "No-Role@my-project.iam.gserviceaccount.com" },
			wantPath: "runtimes.cr.cloudrun.block_service_account",
			wantText: "block_service_account must be a user-managed service account",
		},
		{
			name:     "block_service_account equals service_account",
			mutate:   func(c *CloudRunConfig) { c.BlockServiceAccount = c.ServiceAccount },
			wantPath: "runtimes.cr.cloudrun.block_service_account",
			wantText: "block_service_account must differ from service_account",
		},
		{
			name: "assignable_service_accounts contains block_service_account",
			mutate: func(c *CloudRunConfig) {
				c.AssignableServiceAccounts = []string{
					"worker-one@my-project.iam.gserviceaccount.com",
					c.BlockServiceAccount,
				}
			},
			wantPath: "runtimes.cr.cloudrun.assignable_service_accounts[1]",
			wantText: "block_service_account must not appear in assignable_service_accounts",
		},
		{
			name: "assignable_service_accounts duplicate",
			mutate: func(c *CloudRunConfig) {
				c.AssignableServiceAccounts = []string{
					"worker-one@my-project.iam.gserviceaccount.com",
					"worker-one@my-project.iam.gserviceaccount.com",
				}
			},
			wantPath: "runtimes.cr.cloudrun.assignable_service_accounts[1]",
			wantText: "duplicate service account in assignable_service_accounts",
		},
		{
			name: "assignable_service_accounts compute default",
			mutate: func(c *CloudRunConfig) {
				c.AssignableServiceAccounts = []string{"123456789012-compute@developer.gserviceaccount.com"}
			},
			wantPath: "runtimes.cr.cloudrun.assignable_service_accounts[0]",
			wantText: "the project default service accounts are not allowed",
		},
		{
			name: "assignable_service_accounts empty entry",
			mutate: func(c *CloudRunConfig) {
				c.AssignableServiceAccounts = []string{""}
			},
			wantPath: "runtimes.cr.cloudrun.assignable_service_accounts[0]",
			wantText: "assignable_service_accounts[0] must be a user-managed service account",
		},
		{
			name: "assignable_service_accounts exceeds 64 cap",
			mutate: func(c *CloudRunConfig) {
				c.AssignableServiceAccounts = make([]string, 65)
				for i := range c.AssignableServiceAccounts {
					c.AssignableServiceAccounts[i] = fmt.Sprintf("worker-%02d@my-project.iam.gserviceaccount.com", i)
				}
			},
			wantPath: "runtimes.cr.cloudrun.assignable_service_accounts",
			wantText: "assignable_service_accounts accepts at most 64 entries",
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
		"p-cr":   "cloudrun-us-central1",
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
	if got.ProfileMap["p"] != "cloudrun-us-central1" {
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
		"differing block_service_account": {
			mutateB:   func(c *CloudRunConfig) { c.BlockServiceAccount = "other-block@my-project.iam.gserviceaccount.com" },
			wantField: "block_service_account",
		},
		"differing assignable_service_accounts": {
			mutateB: func(c *CloudRunConfig) {
				c.AssignableServiceAccounts = []string{"worker-b@my-project.iam.gserviceaccount.com"}
			},
			wantField: "assignable_service_accounts",
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
			cfgA.AssignableServiceAccounts = []string{"worker-a@my-project.iam.gserviceaccount.com"}
			cfgB := validCloudRunConfig()
			cfgB.AssignableServiceAccounts = []string{"worker-a@my-project.iam.gserviceaccount.com"}
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
				"alpha": "cloudrun-us-central1-alpha",
				"beta":  "cloudrun-us-central1-beta",
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
					TargetGroup:   "cloudrun-us-central1",
					Field:         tc.wantField,
					ChosenProfile: "alpha",
				}},
			})
			if len(merged.Instances) != 1 || merged.Instances[0].Key != "cloudrun-us-central1" {
				t.Fatalf("merged instances = %+v", merged.Instances)
			}
			if f := findFinding(merged.Findings, ConversionProfileConflictResolved, "profiles.*."+tc.wantField); f == nil {
				t.Fatalf("missing profile_conflict_resolved for %s: %+v", tc.wantField, merged.Findings)
			}

			// A template choice is refused because Cloud Run target fields are infrastructure fields.
			tmplRefused := ConvertProfilesToInstances(ProfileConversionInput{
				Settings: vs,
				Choices: []ConversionChoice{{
					TargetGroup:   "cloudrun-us-central1",
					Field:         tc.wantField,
					ChosenProfile: ConversionChoiceTemplatePrefix + "default",
				}},
			})
			if f := findFinding(tmplRefused.Findings, ConversionUnknownSettingsKey, "choices[0]"); f == nil || f.Severity != ConversionSeverityBlocking {
				t.Fatalf("want blocking refusal for template choice on %s, got %+v", tc.wantField, tmplRefused.Findings)
			}
		})
	}

	t.Run("same assignable_service_accounts in different order agrees without splitting", func(t *testing.T) {
		cfgA := validCloudRunConfig()
		cfgA.AssignableServiceAccounts = []string{
			"worker-two@my-project.iam.gserviceaccount.com",
			"worker-one@my-project.iam.gserviceaccount.com",
		}
		cfgB := validCloudRunConfig()
		cfgB.AssignableServiceAccounts = []string{
			"worker-one@my-project.iam.gserviceaccount.com",
			"worker-two@my-project.iam.gserviceaccount.com",
		}
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
		if len(got.Instances) != 1 || got.Instances[0].Key != "cloudrun-us-central1" {
			t.Fatalf("instances = %+v, want 1 merged instance", got.Instances)
		}
		if !reflect.DeepEqual(got.ProfileMap, map[string]string{"alpha": "cloudrun-us-central1", "beta": "cloudrun-us-central1"}) {
			t.Fatalf("profile map = %v", got.ProfileMap)
		}
	})
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
	// [0] cloudrun-us-east1
	// [1] cloudrun-us-west1
	// [2] docker
	wantMap := map[string]string{
		"p-east": "cloudrun-us-east1",
		"p-west": "cloudrun-us-west1",
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
		blockPath := fmt.Sprintf("server.broker.instances[%d].runtime_target.block_service_account", i)
		if f := findFinding(got.Findings, ConversionFlatTargetOperatorInputRequired, blockPath); f != nil {
			t.Fatalf("cloudrun instance must not get operator-input notice for block_service_account: %+v", f)
		}
	}
	for _, field := range []string{"credentials_file", "block_service_account"} {
		if f := findFinding(got.Findings, ConversionFlatTargetOperatorInputRequired, "server.broker.instances[2].runtime_target."+field); f != nil {
			t.Fatalf("docker instance must not get %s notice: %+v", field, f)
		}
	}
}

func TestProfileConversion_CloudRunKeyOmitsProjectID(t *testing.T) {
	cfgAlpha := &CloudRunConfig{
		ProjectID:                 "proj-alpha",
		Location:                  "us-central1",
		ServiceAccount:            "agent-sa@proj-alpha.iam.gserviceaccount.com",
		BlockServiceAccount:       "no-role-alpha@proj-alpha.iam.gserviceaccount.com",
		AssignableServiceAccounts: []string{"worker-alpha@proj-alpha.iam.gserviceaccount.com"},
		Network:                   "projects/proj-alpha/global/networks/agents-vpc",
	}
	cfgBeta := &CloudRunConfig{
		ProjectID:                 "proj-beta",
		Location:                  "us-central1",
		ServiceAccount:            "agent-sa@proj-beta.iam.gserviceaccount.com",
		BlockServiceAccount:       "no-role-beta@proj-beta.iam.gserviceaccount.com",
		AssignableServiceAccounts: []string{"worker-beta@proj-beta.iam.gserviceaccount.com"},
		Network:                   "projects/proj-beta/global/networks/agents-vpc",
	}

	vs := &VersionedSettings{
		Runtimes: map[string]V1RuntimeConfig{
			"cr-alpha": {Type: "cloudrun", CloudRun: cfgAlpha},
			"cr-beta":  {Type: "cloudrun", CloudRun: cfgBeta},
		},
		Profiles: map[string]V1ProfileConfig{
			"alpha": {Runtime: "cr-alpha"},
			"beta":  {Runtime: "cr-beta"},
		},
	}

	got := ConvertProfilesToInstances(ProfileConversionInput{Settings: vs})
	if len(got.Instances) != 2 {
		t.Fatalf("instances = %+v, want 2 instances for distinct projects in same region", got.Instances)
	}
	keyA, keyB := got.Instances[0].Key, got.Instances[1].Key
	if keyA == keyB {
		t.Fatalf("two projects in us-central1 got duplicate key %q", keyA)
	}
	suffixedRe := regexp.MustCompile(`^cloudrun-us-central1-[0-9a-f]{8}$`)
	secretTokens := []string{
		"proj-alpha", "proj-beta",
		"agent-sa@", "no-role-alpha", "no-role-beta", "worker-alpha", "worker-beta",
	}
	for _, inst := range got.Instances {
		if !suffixedRe.MatchString(inst.Key) {
			t.Fatalf("key %q does not match FC6 hashed cloudrun-us-central1-<8hex> pattern", inst.Key)
		}
		for _, tok := range secretTokens {
			if strings.Contains(inst.Key, tok) || strings.Contains(inst.Name, tok) {
				t.Fatalf("instance key/name %q/%q leaks %q", inst.Key, inst.Name, tok)
			}
		}
		presBytes, err := json.Marshal(PresentInstance(inst).API())
		if err != nil {
			t.Fatal(err)
		}
		for _, tok := range secretTokens {
			if strings.Contains(string(presBytes), tok) {
				t.Fatalf("presented instance JSON %s leaks %q", presBytes, tok)
			}
		}
	}
	if got.ProfileMap["alpha"] == "" || got.ProfileMap["beta"] == "" || got.ProfileMap["alpha"] == got.ProfileMap["beta"] {
		t.Fatalf("profile map = %v, want distinct mapped keys", got.ProfileMap)
	}
}

func TestProfileConversion_CloudRunIdentityKeysFoldAndProfileUnknown(t *testing.T) {
	t.Run("folds block_service_account and assignable_service_accounts including service_account in allow-list", func(t *testing.T) {
		cfg := validCloudRunConfig()
		cfg.BlockServiceAccount = "  no-role-block@my-project.iam.gserviceaccount.com  "
		cfg.AssignableServiceAccounts = []string{
			"  agent-sa@my-project.iam.gserviceaccount.com  ",
			"  worker-sa@my-project.iam.gserviceaccount.com  ",
		}
		vs := &VersionedSettings{
			Runtimes: map[string]V1RuntimeConfig{"cr": {Type: "cloudrun", CloudRun: cfg}},
			Profiles: map[string]V1ProfileConfig{"p": {Runtime: "cr"}},
		}
		got := ConvertProfilesToInstances(ProfileConversionInput{Settings: vs})
		if got.ProfileMap["p"] != "cloudrun-us-central1" {
			t.Fatalf("profile map = %v, findings = %+v", got.ProfileMap, got.Findings)
		}
		rt := got.Instances[0].RuntimeTarget
		if rt.BlockServiceAccount != "no-role-block@my-project.iam.gserviceaccount.com" {
			t.Fatalf("block_service_account = %q", rt.BlockServiceAccount)
		}
		wantAssignable := []string{
			"agent-sa@my-project.iam.gserviceaccount.com",
			"worker-sa@my-project.iam.gserviceaccount.com",
		}
		if !reflect.DeepEqual(rt.AssignableServiceAccounts, wantAssignable) {
			t.Fatalf("assignable_service_accounts = %v, want %v", rt.AssignableServiceAccounts, wantAssignable)
		}
		for _, f := range got.Findings {
			if f.Code == ConversionFieldNotValidForTargetType || f.Severity == ConversionSeverityBlocking {
				t.Fatalf("unexpected finding after folding Cloud Run identity keys: %+v", f)
			}
		}
	})

	// V1ProfileConfig has no CloudRun block; setting cloudrun under profiles.<p>
	// in raw settings is an unknown_settings_key blocking finding.
	t.Run("raw profile cloudrun block is unknown_settings_key", func(t *testing.T) {
		const rawYAML = `schema_version: "1"
runtimes:
  cr:
    type: cloudrun
    cloudrun:
      project_id: my-project
      location: us-central1
      service_account: agent-sa@my-project.iam.gserviceaccount.com
      block_service_account: no-role-block@my-project.iam.gserviceaccount.com
      network: projects/my-project/global/networks/agents-vpc
profiles:
  p:
    runtime: cr
    cloudrun:
      block_service_account: other-block@my-project.iam.gserviceaccount.com
`
		vs := &VersionedSettings{}
		if err := yamlv3.Unmarshal([]byte(rawYAML), vs); err != nil {
			t.Fatal(err)
		}
		got := ConvertProfilesToInstances(ProfileConversionInput{Settings: vs, RawSettings: []byte(rawYAML)})
		if len(got.ProfileMap) != 0 {
			t.Fatalf("profile map = %v, want empty", got.ProfileMap)
		}
		f := findFinding(got.Findings, ConversionUnknownSettingsKey, "profiles.p.cloudrun")
		if f == nil || f.Severity != ConversionSeverityBlocking {
			t.Fatalf("want blocking unknown_settings_key at profiles.p.cloudrun, got %+v", got.Findings)
		}
	})
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
      block_service_account: no-role-block@my-project.iam.gserviceaccount.com
      assignable_service_accounts:
        - worker-one@my-project.iam.gserviceaccount.com
        - worker-two@my-project.iam.gserviceaccount.com
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
	if wantMap := map[string]string{"prod": "cloudrun-us-central1"}; !reflect.DeepEqual(got.ProfileMap, wantMap) {
		t.Fatalf("profile map = %v, want %v (findings: %+v)", got.ProfileMap, wantMap, got.Findings)
	}

	// Without operator input, static validation flags only credentials_file as required.
	errs := ValidateRuntimeBrokerInstances(got.Instances)
	expectValidationPaths(t, errs, "runtime_target.credentials_file")
	if len(errs) != 1 {
		t.Fatalf("unfilled validation errors = %+v, want only credentials_file required", errs)
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
