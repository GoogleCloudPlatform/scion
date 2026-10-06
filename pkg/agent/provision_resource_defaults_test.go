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

// A settings default_resources.limits.cpu counts as a tier-set limit: a larger
// template requests.cpu does not raise it.
func TestProvision_BuiltinCPULimit_SettingsLimitNotRaised(t *testing.T) {
	cfg := provisionWithTemplate(t, `schema_version: "1"
default_harness_config: test-harness
default_resources:
  limits:
    cpu: "3"
harness_configs:
  test-harness:
    harness: test-harness
`, `{
		"default_harness_config": "test-harness",
		"resources": {"requests": {"cpu": "4"}}
	}`)
	if cfg.Resources == nil || cfg.Resources.Limits.CPU != "3" {
		t.Errorf("limits.cpu = %+v, want 3 from settings default_resources", cfg.Resources)
	}
	if cfg.Resources != nil && cfg.Resources.Requests.CPU != "4" {
		t.Errorf("requests.cpu = %q, want 4 from the template", cfg.Resources.Requests.CPU)
	}
}

// A settings default_resources.requests.cpu above the built-in, with no limit
// anywhere, raises the built-in limit to it.
func TestProvision_BuiltinCPULimit_SettingsRequestRaisesBuiltin(t *testing.T) {
	cfg := provisionWithTemplate(t, `schema_version: "1"
default_harness_config: test-harness
default_resources:
  requests:
    cpu: "4"
harness_configs:
  test-harness:
    harness: test-harness
`, `{
		"default_harness_config": "test-harness"
	}`)
	if cfg.Resources == nil || cfg.Resources.Limits.CPU != "4" {
		t.Errorf("limits.cpu = %+v, want 4 (built-in raised to settings requests.cpu)", cfg.Resources)
	}
}

// applyBuiltinResourceDefaults must replace cfg.Kubernetes with a copy when it
// raises the Kubernetes CPU limit, never write through a pointer the caller
// (or a template) still holds.
func TestApplyBuiltinResourceDefaults_DoesNotMutateHeldKubernetesConfig(t *testing.T) {
	heldRes := &api.K8sResources{
		Requests: map[string]string{"cpu": "6"},
		Limits:   map[string]string{"nvidia.com/gpu": "1"},
	}
	held := &api.KubernetesConfig{RuntimeClassName: "gvisor", Resources: heldRes}
	cfg := &api.ScionConfig{Kubernetes: held}

	applyBuiltinResourceDefaults(cfg)

	if cfg.Kubernetes == held {
		t.Fatal("cfg.Kubernetes is still the held pointer; want a copy")
	}
	if held.Resources != heldRes {
		t.Error("held KubernetesConfig.Resources pointer was replaced")
	}
	if _, ok := heldRes.Limits["cpu"]; ok || len(heldRes.Limits) != 1 || len(heldRes.Requests) != 1 {
		t.Errorf("held kubernetes resources mutated: %+v", heldRes)
	}
	if got := cfg.Kubernetes.Resources.Limits["cpu"]; got != "6" {
		t.Errorf("cfg kubernetes.resources.limits.cpu = %q, want 6", got)
	}
	if cfg.Kubernetes.RuntimeClassName != "gvisor" {
		t.Errorf("copy lost other Kubernetes fields: %+v", cfg.Kubernetes)
	}
	if cfg.Resources == nil || cfg.Resources.Limits.CPU != "2" {
		t.Errorf("generic limits.cpu = %+v, want 2", cfg.Resources)
	}
}

// Without a Kubernetes raise, cfg.Kubernetes stays the same pointer.
func TestApplyBuiltinResourceDefaults_NoRaiseKeepsKubernetesPointer(t *testing.T) {
	held := &api.KubernetesConfig{Resources: &api.K8sResources{Requests: map[string]string{"cpu": "1"}}}
	cfg := &api.ScionConfig{Kubernetes: held}

	applyBuiltinResourceDefaults(cfg)

	if cfg.Kubernetes != held {
		t.Error("cfg.Kubernetes replaced although nothing was raised")
	}
	if cfg.Resources == nil || cfg.Resources.Limits.CPU != "2" {
		t.Errorf("generic limits.cpu = %+v, want 2", cfg.Resources)
	}
}

// A nil Kubernetes block is left nil.
func TestApplyBuiltinResourceDefaults_NilKubernetes(t *testing.T) {
	cfg := &api.ScionConfig{Resources: &api.ResourceSpec{Requests: api.ResourceList{CPU: "4"}}}

	applyBuiltinResourceDefaults(cfg)

	if cfg.Kubernetes != nil {
		t.Errorf("cfg.Kubernetes = %+v, want nil", cfg.Kubernetes)
	}
	if cfg.Resources.Limits.CPU != "4" {
		t.Errorf("limits.cpu = %q, want 4", cfg.Resources.Limits.CPU)
	}
}
