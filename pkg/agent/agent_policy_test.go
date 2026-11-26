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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeImageReference(t *testing.T) {
	for in, want := range map[string]string{
		"postgres":                            "docker.io/library/postgres",
		"postgres:18-alpine":                  "docker.io/library/postgres:18-alpine",
		"library/postgres:18":                 "docker.io/library/postgres:18",
		"someorg/tool:1":                      "docker.io/someorg/tool:1",
		"docker.io/postgres":                  "docker.io/library/postgres",
		"index.docker.io/someorg/tool":        "docker.io/someorg/tool",
		"Registry.Example.com/approved/img:1": "registry.example.com/approved/img:1",
		"localhost/img:dev":                   "localhost/img:dev",
		"localhost:5000/img":                  "localhost:5000/img",
		"ghcr.io/org/img@sha256:abc":          "ghcr.io/org/img@sha256:abc",
	} {
		assert.Equal(t, want, normalizeImageReference(in), in)
	}
}

func TestCheckAgentPolicyImage(t *testing.T) {
	p := &config.V1AgentPolicy{AllowedImagePrefixes: []string{
		"registry.example.com/approved/",
		"ghcr.io/org/agent",
		"scion-claude",
		"quay.example.com",
		"docker.io/hubns/",
		"otherns/",
	}}
	allowed := map[string]string{
		"registry.example.com/approved/scion-claude:latest": "registry.example.com/approved/scion-claude:latest",
		"registry.example.com/approved/sub/img@sha256:abc":  "registry.example.com/approved/sub/img@sha256:abc",
		"ghcr.io/org/agent":                "ghcr.io/org/agent",
		"ghcr.io/org/agent:v1":             "ghcr.io/org/agent:v1",
		"ghcr.io/org/agent/sub:v1":         "ghcr.io/org/agent/sub:v1",
		"ghcr.io/org/agent@sha256:abc":     "ghcr.io/org/agent@sha256:abc",
		"scion-claude:latest":              "docker.io/library/scion-claude:latest",
		"docker.io/library/scion-claude":   "docker.io/library/scion-claude",
		"quay.example.com/anything/at:all": "quay.example.com/anything/at:all",
		// Docker Hub namespace prefixes.
		"docker.io/hubns/agent:1": "docker.io/hubns/agent:1",
		"hubns/agent:1":           "docker.io/hubns/agent:1",
		"otherns/agent":           "docker.io/otherns/agent",
	}
	for in, want := range allowed {
		got, err := checkAgentPolicyImage(p, "a", in)
		if assert.NoError(t, err, in) {
			assert.Equal(t, want, got, in)
		}
	}
	refused := []string{
		"docker.io/library/postgres:18-alpine",
		"postgres:18-alpine",
		"registry.example.com/approved-evil/img",           // not under approved/
		"registry.example.com/approvedimg",                 // not under approved/
		"ghcr.io/org/agent-evil:v1",                        // boundary: agent vs agent-evil
		"ghcr.io/org/agentx",                               // boundary
		"scion-claude-evil:latest",                         // boundary on a short name
		"evil.example.com/registry.example.com/approved/x", // prefix only matches at the start
		"someorg/scion-claude",                             // docker.io/someorg/... is not docker.io/library/...
		"quay.example.com.evil.net/x",                      // host boundary
		"hubns-evil/agent",                                 // namespace boundary
		"docker.io/library/hubns",                          // a repository named hubns is not the hubns namespace
		"",
	}
	for _, in := range refused {
		_, err := checkAgentPolicyImage(p, "a", in)
		require.Error(t, err, in)
		assert.ErrorIs(t, err, ErrAgentPolicy, in)
		assert.Contains(t, err.Error(), "agent_policy.allowed_image_prefixes", in)
		// Error message must never leak the allowlist entries.
		if !strings.Contains(in, "registry.example.com/approved/") {
			assert.NotContains(t, err.Error(), "registry.example.com/approved/", in)
		}
		if !strings.Contains(in, "quay.example.com") {
			assert.NotContains(t, err.Error(), "quay.example.com", in)
		}
	}
	// Long requester image input is bounded in the error message.
	longImg := "evil.example.com/" + strings.Repeat("a", 200)
	_, err := checkAgentPolicyImage(p, "a", longImg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "...")
	assert.Less(t, len(err.Error()), 260)

	// Credentials embedded in a resolved image (e.g. from image_registry)
	// must be redacted before appearing in the public refusal message.
	_, err = checkAgentPolicyImage(p, "a", "user:s3cret@internal.example.com/team/scion-claude:latest")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrAgentPolicy)
	assert.NotContains(t, err.Error(), "s3cret")
	assert.NotContains(t, err.Error(), "user:")

	// No prefix list: the image is untouched.
	got, err := checkAgentPolicyImage(&config.V1AgentPolicy{}, "a", "postgres")
	require.NoError(t, err)
	assert.Equal(t, "postgres", got)
}

func TestApplyAgentPolicyResources(t *testing.T) {
	p := &config.V1AgentPolicy{MaxResources: &config.V1AgentPolicyResources{CPU: "8", Memory: "8Gi"}}

	// Missing limits become the maximum; the input is not modified.
	in := &api.ResourceSpec{Requests: api.ResourceList{CPU: "1"}}
	out, err := applyAgentPolicyResources(p, "a", "docker", in, nil)
	require.NoError(t, err)
	assert.Equal(t, "8", out.Limits.CPU)
	assert.Equal(t, "8Gi", out.Limits.Memory)
	assert.Equal(t, "1", out.Requests.CPU)
	assert.Equal(t, "", out.Requests.Memory, "docker: no request is added")
	assert.Equal(t, "", in.Limits.CPU, "the input spec must not be modified")

	out, err = applyAgentPolicyResources(p, "a", "docker", nil, nil)
	require.NoError(t, err)
	assert.Equal(t, api.ResourceList{CPU: "8", Memory: "8Gi"}, out.Limits)

	// On Kubernetes a filled limit gets the default request, so the API
	// server does not default the request to the filled limit.
	out, err = applyAgentPolicyResources(p, "a", "kubernetes", nil, nil)
	require.NoError(t, err)
	assert.Equal(t, api.ResourceList{CPU: "250m", Memory: "512Mi"}, out.Requests)
	small := &config.V1AgentPolicy{MaxResources: &config.V1AgentPolicyResources{CPU: "100m"}}
	out, err = applyAgentPolicyResources(small, "a", "kubernetes", nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "100m", out.Requests.CPU, "the filled request never exceeds the maximum")

	// Values within the maximum pass unchanged.
	ok := &api.ResourceSpec{Limits: api.ResourceList{CPU: "8000m", Memory: "8192Mi"}, Requests: api.ResourceList{CPU: "2", Memory: "1Gi"}}
	out, err = applyAgentPolicyResources(p, "a", "docker", ok, nil)
	require.NoError(t, err)
	assert.Equal(t, *ok, *out)

	for name, spec := range map[string]*api.ResourceSpec{
		"limits.cpu":      {Limits: api.ResourceList{CPU: "12"}},
		"limits.memory":   {Limits: api.ResourceList{Memory: "64Gi"}},
		"requests.cpu":    {Requests: api.ResourceList{CPU: "9"}},
		"requests.memory": {Requests: api.ResourceList{Memory: "9Gi"}},
		"docker spelling": {Limits: api.ResourceList{Memory: "64g"}},
		"ambiguous m":     {Limits: api.ResourceList{Memory: "9000m"}}, // 9 GB to docker
		"unparsable":      {Limits: api.ResourceList{CPU: "lots"}},
		// Docker reads a 0 limit as unlimited.
		"zero cpu limit":        {Limits: api.ResourceList{CPU: "0"}},
		"zero memory limit":     {Limits: api.ResourceList{Memory: "0"}},
		"negative cpu limit":    {Limits: api.ResourceList{CPU: "-1"}},
		"negative memory limit": {Limits: api.ResourceList{Memory: "-8Gi"}},
		"sub-byte memory limit": {Limits: api.ResourceList{Memory: "0.1"}},
		"negative request":      {Requests: api.ResourceList{CPU: "-1"}},
	} {
		_, err := applyAgentPolicyResources(p, "a", "docker", spec, nil)
		require.Error(t, err, name)
		assert.ErrorIs(t, err, ErrAgentPolicy, name)
		assert.Contains(t, err.Error(), "agent_policy.max_resources", name)
	}

	// The Kubernetes resources map wins over the spec on Kubernetes, so it
	// is checked too, and its limit counts as a limit.
	k8s := &api.KubernetesConfig{Resources: &api.K8sResources{Limits: map[string]string{"cpu": "16"}}}
	_, err = applyAgentPolicyResources(p, "a", "kubernetes", nil, k8s)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kubernetes.resources.limits.cpu")
	k8s = &api.KubernetesConfig{Resources: &api.K8sResources{Limits: map[string]string{"cpu": "4"}}}
	out, err = applyAgentPolicyResources(p, "a", "kubernetes", nil, k8s)
	require.NoError(t, err)
	assert.Equal(t, "", out.Limits.CPU, "a kubernetes limit within the maximum is kept as the limit")
	assert.Equal(t, "8Gi", out.Limits.Memory)

	// No max_resources: the spec is returned as is.
	out, err = applyAgentPolicyResources(&config.V1AgentPolicy{}, "a", "docker", in, nil)
	require.NoError(t, err)
	assert.Same(t, in, out)
}

// On Docker and Podman the kubernetes.resources map is never read (only the
// k8s runtime applies it), so it must not stop a missing limit from being
// filled with the maximum: otherwise docker/podman run gets no --cpus or
// --memory and the container is unlimited.
func TestApplyAgentPolicyResources_KubernetesMapOnBindRuntimes(t *testing.T) {
	p := &config.V1AgentPolicy{MaxResources: &config.V1AgentPolicyResources{CPU: "8", Memory: "8Gi"}}
	k8s := &api.KubernetesConfig{Resources: &api.K8sResources{
		Limits:   map[string]string{"cpu": "1", "memory": "1Gi"},
		Requests: map[string]string{"cpu": "1", "memory": "1Gi"},
	}}
	for _, rt := range []string{"docker", "podman"} {
		t.Run(rt, func(t *testing.T) {
			out, err := applyAgentPolicyResources(p, "a", rt, nil, k8s)
			require.NoError(t, err)
			require.NotNil(t, out)
			assert.Equal(t, api.ResourceList{CPU: "8", Memory: "8Gi"}, out.Limits,
				"the limits the runtime reads must be filled")
		})
	}
	// The map is still checked against the maximum on every runtime.
	big := &api.KubernetesConfig{Resources: &api.K8sResources{Limits: map[string]string{"memory": "64Gi"}}}
	for _, rt := range []string{"docker", "podman", "container", "cloudrun"} {
		_, err := applyAgentPolicyResources(p, "a", rt, nil, big)
		require.Error(t, err, rt)
		assert.Contains(t, err.Error(), "kubernetes.resources.limits.memory", rt)
	}
}

// Cloud Run and Apple container fall back from limit to request and apply
// platform defaults when both are unset, so missing limits are not filled
// with max_resources (avoiding Cloud Run CPU/memory band violations and
// silent upsizing of request-only agents). When both limit and request are
// unset and the runtime's default exceeds (or may exceed) max_resources — or
// on any unknown/unlisted runtime — the start is refused fail-closed.
// Substrate reads Limits only and opts into missing-limit filling.
func TestApplyAgentPolicyResources_NonUnlimitedRuntimesNoFill(t *testing.T) {
	p := &config.V1AgentPolicy{MaxResources: &config.V1AgentPolicyResources{CPU: "8", Memory: "32Gi"}}

	t.Run("cloudrun_default_cpu_no_memory_fill", func(t *testing.T) {
		in := &api.ResourceSpec{Limits: api.ResourceList{CPU: "2"}}
		out, err := applyAgentPolicyResources(p, "a", "cloudrun", in, nil)
		require.NoError(t, err)
		require.NotNil(t, out)
		assert.Equal(t, "2", out.Limits.CPU)
		assert.Equal(t, "", out.Limits.Memory, "missing memory limit must not be filled to 32Gi on cloudrun")
	})
	t.Run("cloudrun_nil_spec_stays_nil_when_defaults_within_max", func(t *testing.T) {
		out, err := applyAgentPolicyResources(p, "a", "cloudrun", nil, nil)
		require.NoError(t, err)
		assert.Nil(t, out, "nil spec must not become an empty non-nil spec when not filling")
	})
	t.Run("cloudrun_request_only_not_upsized", func(t *testing.T) {
		in := &api.ResourceSpec{Requests: api.ResourceList{CPU: "1", Memory: "2Gi"}}
		out, err := applyAgentPolicyResources(p, "a", "cloudrun", in, nil)
		require.NoError(t, err)
		require.NotNil(t, out)
		assert.Equal(t, "", out.Limits.CPU)
		assert.Equal(t, "", out.Limits.Memory, "request-only agent must not be upsized to ceiling on cloudrun")
		assert.Equal(t, "1", out.Requests.CPU)
		assert.Equal(t, "2Gi", out.Requests.Memory)
	})

	t.Run("container_no_fill", func(t *testing.T) {
		in := &api.ResourceSpec{Requests: api.ResourceList{CPU: "1", Memory: "2Gi"}}
		out, err := applyAgentPolicyResources(p, "a", "container", in, nil)
		require.NoError(t, err)
		require.NotNil(t, out)
		assert.Equal(t, "", out.Limits.CPU)
		assert.Equal(t, "", out.Limits.Memory)
		assert.Equal(t, "1", out.Requests.CPU)
		assert.Equal(t, "2Gi", out.Requests.Memory)
	})

	// Substrate reads Limits only and opts into missing-limit fill, including
	// when spec is nil.
	t.Run("substrate_fills_missing_limits", func(t *testing.T) {
		small := &config.V1AgentPolicy{MaxResources: &config.V1AgentPolicyResources{CPU: "1", Memory: "1Gi"}}
		out, err := applyAgentPolicyResources(small, "a", "substrate", &api.ResourceSpec{Limits: api.ResourceList{CPU: "1"}}, nil)
		require.NoError(t, err)
		require.NotNil(t, out)
		assert.Equal(t, "1", out.Limits.CPU)
		assert.Equal(t, "1Gi", out.Limits.Memory)

		out, err = applyAgentPolicyResources(small, "a", "substrate", nil, nil)
		require.NoError(t, err)
		require.NotNil(t, out)
		assert.Equal(t, api.ResourceList{CPU: "1", Memory: "1Gi"}, out.Limits)
	})

	// Unknown/unlisted runtimes fail closed when any capped resource is unset.
	for _, rt := range []string{"future-rt", "cloudrun-sandbox"} {
		t.Run(rt+"_unset_capped_resource_refused", func(t *testing.T) {
			_, err := applyAgentPolicyResources(p, "a", rt, &api.ResourceSpec{Limits: api.ResourceList{CPU: "2"}}, nil)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrAgentPolicy)
			assert.Contains(t, err.Error(), "resources.limits.memory")
			assert.Contains(t, err.Error(), "agent_policy.max_resources.memory")
		})
		t.Run(rt+"_explicit_within_max_allowed", func(t *testing.T) {
			in := &api.ResourceSpec{Requests: api.ResourceList{CPU: "1", Memory: "2Gi"}}
			out, err := applyAgentPolicyResources(p, "a", rt, in, nil)
			require.NoError(t, err)
			require.NotNil(t, out)
			assert.Equal(t, "", out.Limits.CPU)
			assert.Equal(t, "", out.Limits.Memory)
		})
	}

	// Unset memory when runtime default (2Gi on Cloud Run, 2G on Apple
	// container) exceeds max_resources.memory must be refused fail-closed.
	lowMem := &config.V1AgentPolicy{MaxResources: &config.V1AgentPolicyResources{CPU: "8", Memory: "1Gi"}}
	for _, rt := range []string{"cloudrun", "container"} {
		t.Run(rt+"_unset_memory_above_low_max_refused", func(t *testing.T) {
			_, err := applyAgentPolicyResources(lowMem, "a", rt, &api.ResourceSpec{Limits: api.ResourceList{CPU: "2"}}, nil)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrAgentPolicy)
			assert.Contains(t, err.Error(), "resources.limits.memory")
			assert.Contains(t, err.Error(), "agent_policy.max_resources.memory")
		})
		t.Run(rt+"_explicit_request_within_low_max_allowed", func(t *testing.T) {
			in := &api.ResourceSpec{Limits: api.ResourceList{CPU: "2"}, Requests: api.ResourceList{Memory: "512Mi"}}
			out, err := applyAgentPolicyResources(lowMem, "a", rt, in, nil)
			require.NoError(t, err)
			assert.Equal(t, "", out.Limits.Memory)
			assert.Equal(t, "512Mi", out.Requests.Memory)
		})
	}

	// Boundary: unset memory when runtime default equals max_resources.memory passes.
	_, err := applyAgentPolicyResources(
		&config.V1AgentPolicy{MaxResources: &config.V1AgentPolicyResources{CPU: "8", Memory: "2Gi"}},
		"a", "cloudrun", &api.ResourceSpec{Limits: api.ResourceList{CPU: "2"}}, nil,
	)
	require.NoError(t, err, "cloudrun default 2Gi equals 2Gi ceiling")
	_, err = applyAgentPolicyResources(
		&config.V1AgentPolicy{MaxResources: &config.V1AgentPolicyResources{CPU: "8", Memory: "2G"}},
		"a", "container", &api.ResourceSpec{Limits: api.ResourceList{CPU: "2"}}, nil,
	)
	require.NoError(t, err, "container default 2G equals 2G ceiling")

	// Unset CPU (e.g. runtime.enforce_resource_defaults=false) when runtime
	// default exceeds (Cloud Run: 2 vCPU) or may exceed (Apple container: CLI
	// default) max_resources.cpu must be refused fail-closed.
	lowCPU := &config.V1AgentPolicy{MaxResources: &config.V1AgentPolicyResources{CPU: "1", Memory: "8Gi"}}
	for _, rt := range []string{"cloudrun", "container"} {
		t.Run(rt+"_unset_cpu_refused", func(t *testing.T) {
			_, err := applyAgentPolicyResources(lowCPU, "a", rt, &api.ResourceSpec{}, nil)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrAgentPolicy)
			assert.Contains(t, err.Error(), "resources.limits.cpu")
			assert.Contains(t, err.Error(), "agent_policy.max_resources.cpu")
		})
	}

	// Explicit requests and limits above the maximum are still refused on
	// Cloud Run, Apple container, and unlisted runtimes.
	for _, rt := range []string{"cloudrun", "cloudrun-sandbox", "container", "future-rt"} {
		_, err := applyAgentPolicyResources(p, "a", rt, &api.ResourceSpec{Requests: api.ResourceList{Memory: "64Gi"}}, nil)
		require.Error(t, err, rt)
		assert.ErrorIs(t, err, ErrAgentPolicy, rt)
		assert.Contains(t, err.Error(), "agent_policy.max_resources.memory", rt)

		_, err = applyAgentPolicyResources(p, "a", rt, &api.ResourceSpec{Limits: api.ResourceList{CPU: "0"}}, nil)
		require.Error(t, err, rt)
		assert.ErrorIs(t, err, ErrAgentPolicy, rt)
	}
}

func TestAgentPolicyError_FieldsAndUnwrap(t *testing.T) {
	err := error(NewAgentPolicyError("image", "agent_policy.allowed_image_prefixes", "image %q is not allowed", "x"))
	assert.ErrorIs(t, err, ErrAgentPolicy)
	var pe *AgentPolicyError
	require.True(t, errors.As(err, &pe))
	assert.Equal(t, "image", pe.Field)
	assert.Equal(t, "agent_policy.allowed_image_prefixes", pe.Setting)
	assert.Equal(t, err.Error(), pe.PublicMessage())
}

// --- Start integration: the policy is applied to the final RunConfig. ---

const agentPolicyRunSettings = `schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
agent_policy:
  allowed_image_prefixes: [test-image]
  max_resources:
    cpu: "8"
    memory: 8Gi
`

func startWithAgentPolicy(t *testing.T, settings string, inline *api.ScionConfig) (runtime.RunConfig, int, error) {
	t.Helper()
	f := newSharedDirStorageRunFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(f.globalScionDir, "settings.yaml"), []byte(settings), 0o644))
	var captured runtime.RunConfig
	ran, pulled := 0, []string{}
	mockRT := &runtime.MockRuntime{
		NameFunc: func() string { return "docker" },
		RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
			captured = cfg
			ran++
			return "mock-id", nil
		},
		ImageExistsFunc: func(ctx context.Context, image string) (bool, error) { return false, nil },
		PullImageFunc: func(ctx context.Context, image string) error {
			pulled = append(pulled, image)
			return nil
		},
	}
	opts := api.StartOptions{
		Name:         "policy-agent",
		ProjectPath:  f.projectScionDir,
		NoAuth:       true,
		InlineConfig: inline,
	}
	_, err := NewManager(mockRT).Start(context.Background(), opts)
	if errors.Is(err, ErrAgentPolicy) && strings.Contains(err.Error(), "allowed_image_prefixes") {
		assert.Empty(t, pulled, "a refused image must not be pulled")
	}
	return captured, ran, err
}

func TestStart_AgentPolicyAppliedToFinalRunConfig(t *testing.T) {
	cfg, ran, err := startWithAgentPolicy(t, agentPolicyRunSettings, nil)
	require.NoError(t, err)
	require.Equal(t, 1, ran)
	assert.Equal(t, "docker.io/library/test-image:latest", cfg.Image)
	require.NotNil(t, cfg.Resources)
	// The built-in default limits.cpu (2) is within the maximum and kept;
	// the missing memory limit becomes the maximum.
	assert.Equal(t, "2", cfg.Resources.Limits.CPU)
	assert.Equal(t, "8Gi", cfg.Resources.Limits.Memory)
}

func TestStart_AgentPolicyRefusals(t *testing.T) {
	cases := map[string]struct {
		inline  *api.ScionConfig
		setting string
	}{
		"inline image": {&api.ScionConfig{Image: "docker.io/library/postgres:18-alpine"}, "agent_policy.allowed_image_prefixes"},
		"resources above max": {&api.ScionConfig{Resources: &api.ResourceSpec{Limits: api.ResourceList{CPU: "12", Memory: "64Gi"}}},
			"agent_policy.max_resources.cpu"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, ran, err := startWithAgentPolicy(t, agentPolicyRunSettings, tc.inline)
			require.Error(t, err)
			assert.Equal(t, 0, ran, "the container must not be created")
			assert.ErrorIs(t, err, ErrAgentPolicy)
			assert.Contains(t, err.Error(), tc.setting)
			var pe *AgentPolicyError
			assert.True(t, errors.As(err, &pe), "the broker must see a typed AgentPolicyError")
		})
	}
}

func TestStart_NoAgentPolicyUnchanged(t *testing.T) {
	cfg, ran, err := startWithAgentPolicy(t, `schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`, &api.ScionConfig{Image: "postgres:18-alpine"})
	require.NoError(t, err)
	require.Equal(t, 1, ran)
	assert.Equal(t, "postgres:18-alpine", cfg.Image)
}

func TestStart_InvalidAgentPolicyFailsClosed(t *testing.T) {
	_, ran, err := startWithAgentPolicy(t, `schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
agent_policy:
  max_resources:
    cpu: "0"
`, nil)
	require.Error(t, err)
	assert.Equal(t, 0, ran)
	assert.ErrorIs(t, err, ErrAgentPolicy)
}

func TestStart_AgentPolicyImageRefusalRedactsRegistryCredentials(t *testing.T) {
	_, ran, err := startWithAgentPolicy(t, `schema_version: "1"
active_profile: local
image_registry: "user:pw@internal.example.com/team"
profiles:
  local:
    runtime: docker
agent_policy:
  allowed_image_prefixes: [ok.example.com/]
`, nil)
	require.Error(t, err)
	assert.Equal(t, 0, ran)
	assert.ErrorIs(t, err, ErrAgentPolicy)
	var pe *AgentPolicyError
	require.True(t, errors.As(err, &pe))
	assert.NotContains(t, pe.PublicMessage(), "pw")
	assert.NotContains(t, pe.PublicMessage(), "user:")
}
