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

package agent

import (
	"context"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// Tests for the built-in limits.cpu default against a larger CPU request
// (ptone/scion#3407), at the provisioning level.

const builtinResSettingsYAML = `schema_version: "1"
default_harness_config: test-harness
harness_configs:
  test-harness:
    harness: test-harness
`

const builtinResDisabledSettingsYAML = `schema_version: "1"
default_harness_config: test-harness
runtime:
  enforce_resource_defaults: false
harness_configs:
  test-harness:
    harness: test-harness
`

func provisionWithTemplate(t *testing.T, settingsYAML, templateJSON string) *api.ScionConfig {
	t.Helper()
	projectScionDir := hubDefaultsFixture(t, settingsYAML, templateJSON)
	_, _, cfg, err := ProvisionAgent(context.Background(), "res-agent", "limits-tpl", "", "test-harness",
		projectScionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("ProvisionAgent: %v", err)
	}
	if cfg == nil {
		t.Fatal("ProvisionAgent returned nil config")
	}
	return cfg
}

// A template requests.cpu above the built-in "2" raises the built-in limit to
// the request, so the pod is valid and Docker/Podman stay CPU-bounded.
func TestProvision_BuiltinCPULimit_RaisedToLargerRequest(t *testing.T) {
	cfg := provisionWithTemplate(t, builtinResSettingsYAML, `{
		"default_harness_config": "test-harness",
		"resources": {"requests": {"cpu": "4"}}
	}`)
	if cfg.Resources == nil {
		t.Fatal("Resources is nil")
	}
	if cfg.Resources.Limits.CPU != "4" {
		t.Errorf("limits.cpu = %q, want 4 (raised to requests.cpu)", cfg.Resources.Limits.CPU)
	}
	if cfg.Resources.Requests.CPU != "4" {
		t.Errorf("requests.cpu = %q, want 4", cfg.Resources.Requests.CPU)
	}
}

// A template requests.cpu below the built-in leaves the built-in "2".
func TestProvision_BuiltinCPULimit_SmallerRequestKeepsBuiltin(t *testing.T) {
	cfg := provisionWithTemplate(t, builtinResSettingsYAML, `{
		"default_harness_config": "test-harness",
		"resources": {"requests": {"cpu": "500m"}}
	}`)
	if cfg.Resources == nil || cfg.Resources.Limits.CPU != "2" {
		t.Errorf("limits.cpu = %+v, want 2", cfg.Resources)
	}
}

// A Kubernetes-only CPU request sets kubernetes.resources.limits.cpu and leaves
// the generic limit (used by Docker and Podman) at the built-in "2".
func TestProvision_BuiltinCPULimit_K8sRequestSetsK8sLimitOnly(t *testing.T) {
	cfg := provisionWithTemplate(t, builtinResSettingsYAML, `{
		"default_harness_config": "test-harness",
		"kubernetes": {"resources": {"requests": {"cpu": "6"}}}
	}`)
	if cfg.Resources == nil || cfg.Resources.Limits.CPU != "2" {
		t.Errorf("generic limits.cpu = %+v, want 2", cfg.Resources)
	}
	if cfg.Kubernetes == nil || cfg.Kubernetes.Resources == nil {
		t.Fatal("kubernetes.resources is nil")
	}
	if got := cfg.Kubernetes.Resources.Limits["cpu"]; got != "6" {
		t.Errorf("kubernetes.resources.limits.cpu = %q, want 6", got)
	}
	if got := cfg.Kubernetes.Resources.Requests["cpu"]; got != "6" {
		t.Errorf("kubernetes.resources.requests.cpu = %q, want 6", got)
	}
}

// An explicit limits.cpu from the template is never raised.
func TestProvision_BuiltinCPULimit_ExplicitLimitUntouched(t *testing.T) {
	cfg := provisionWithTemplate(t, builtinResSettingsYAML, `{
		"default_harness_config": "test-harness",
		"resources": {"requests": {"cpu": "4"}, "limits": {"cpu": "3"}},
		"kubernetes": {"resources": {"requests": {"cpu": "8"}}}
	}`)
	if cfg.Resources == nil || cfg.Resources.Limits.CPU != "3" {
		t.Errorf("limits.cpu = %+v, want explicit 3", cfg.Resources)
	}
	if cfg.Kubernetes != nil && cfg.Kubernetes.Resources != nil {
		if got, ok := cfg.Kubernetes.Resources.Limits["cpu"]; ok {
			t.Errorf("kubernetes.resources.limits.cpu = %q, want unset", got)
		}
	}
}

// With runtime.enforce_resource_defaults false, no CPU limit is added and
// nothing is raised.
func TestProvision_BuiltinCPULimit_DisabledAddsNothing(t *testing.T) {
	cfg := provisionWithTemplate(t, builtinResDisabledSettingsYAML, `{
		"default_harness_config": "test-harness",
		"resources": {"requests": {"cpu": "4"}},
		"kubernetes": {"resources": {"requests": {"cpu": "6"}}}
	}`)
	if cfg.Resources != nil && cfg.Resources.Limits.CPU != "" {
		t.Errorf("limits.cpu = %q, want unset", cfg.Resources.Limits.CPU)
	}
	if cfg.Kubernetes != nil && cfg.Kubernetes.Resources != nil {
		if got, ok := cfg.Kubernetes.Resources.Limits["cpu"]; ok {
			t.Errorf("kubernetes.resources.limits.cpu = %q, want unset", got)
		}
	}
}
