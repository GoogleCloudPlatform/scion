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
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
)

// testGitToken is a test value standing in for a project's git token.
const testGitToken = "test-git-token-value-9d3e"

// gitTokenSecret is the resolved secret a project's GITHUB_TOKEN secret
// becomes at dispatch.
func gitTokenSecret() api.ResolvedSecret {
	return api.ResolvedSecret{Name: "GITHUB_TOKEN", Type: "environment", Target: "GITHUB_TOKEN", Value: testGitToken, Source: "project"}
}

func envEntry(envs []corev1.EnvVar, name string) *corev1.EnvVar {
	for i := range envs {
		if envs[i].Name == name {
			return &envs[i]
		}
	}
	return nil
}

// assertTokenNotInPodSpec fails if the token appears anywhere in the pod
// spec: a container's command, args or plain env value.
func assertTokenNotInPodSpec(t *testing.T, pod *corev1.Pod) {
	t.Helper()
	data, err := json.Marshal(pod)
	require.NoError(t, err)
	assert.NotContains(t, string(data), testGitToken, "the git token must not be in the pod spec")
}

// assertInitGitTokenLikeAgent checks that the init container reads
// GITHUB_TOKEN through the same secretKeyRef as the agent container.
func assertInitGitTokenLikeAgent(t *testing.T, pod *corev1.Pod, wantKey string) {
	t.Helper()
	require.Len(t, pod.Spec.InitContainers, 1)
	agent := envEntry(pod.Spec.Containers[0].Env, provision.GitTokenEnv)
	require.NotNil(t, agent, "agent container must have GITHUB_TOKEN")
	require.NotNil(t, agent.ValueFrom)
	require.NotNil(t, agent.ValueFrom.SecretKeyRef)
	assert.Empty(t, agent.Value)

	init := envEntry(pod.Spec.InitContainers[0].Env, provision.GitTokenEnv)
	require.NotNil(t, init, "cloning init container must have GITHUB_TOKEN")
	assert.Empty(t, init.Value, "no plain value")
	require.NotNil(t, init.ValueFrom)
	require.NotNil(t, init.ValueFrom.SecretKeyRef)
	assert.Equal(t, *agent.ValueFrom.SecretKeyRef, *init.ValueFrom.SecretKeyRef)
	assert.Equal(t, "scion-agent-"+pod.Name, init.ValueFrom.SecretKeyRef.Name)
	assert.Equal(t, wantKey, init.ValueFrom.SecretKeyRef.Key)
	assertTokenNotInPodSpec(t, pod)
}

func assertInitHasNoGitToken(t *testing.T, pod *corev1.Pod) {
	t.Helper()
	for _, ic := range pod.Spec.InitContainers {
		assert.Nil(t, envEntry(ic.Env, provision.GitTokenEnv), "init container %s must not get GITHUB_TOKEN", ic.Name)
	}
	assertTokenNotInPodSpec(t, pod)
}

// Shared-plain lock winner and worktree-per-agent (winner and loser, which
// both provision) get the agent's git token reference.
func TestBuildPod_NFSGitToken_CloningInitContainer(t *testing.T) {
	for name, cfg := range map[string]RunConfig{
		"shared-plain winner": nfsBaseConfig("gt-plain"),
		"worktree winner":     nfsWorktreeConfig("gt-wt"),
		"worktree loser": func() RunConfig {
			c := nfsWorktreeConfig("gt-wt-loser")
			c.nfsProvisionLockLost = true
			return c
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			cfg.ResolvedSecrets = []api.ResolvedSecret{gitTokenSecret()}
			pod, err := newNFSTestK8sRuntime().buildPod("default", cfg)
			require.NoError(t, err)
			assertInitGitTokenLikeAgent(t, pod, "GITHUB_TOKEN")
		})
	}
}

// The GKE hybrid secrets path references the same per-agent Secret for env
// secrets, so the init container gets the same reference there.
func TestBuildPod_NFSGitToken_GKEPath(t *testing.T) {
	r := newNFSTestK8sRuntime()
	r.GKEMode = true
	cfg := nfsBaseConfig("gt-gke")
	cfg.ResolvedSecrets = []api.ResolvedSecret{
		gitTokenSecret(),
		{Name: "api-key", Type: "environment", Target: "API_KEY", Ref: "projects/p/secrets/api-key", Source: "project"},
	}
	require.True(t, r.useGKESecretsPath(cfg))
	pod, err := r.buildPod("default", cfg)
	require.NoError(t, err)
	assertInitGitTokenLikeAgent(t, pod, "GITHUB_TOKEN")
}

// A container that does not clone gets no git token: the shared-plain
// wait-for-sentinel container, clone-per-agent and empty-per-agent (the
// agent container clones), and a project without clone settings.
func TestBuildPod_NFSGitToken_NonCloningInitContainers(t *testing.T) {
	for name, cfg := range map[string]RunConfig{
		"shared-plain loser": func() RunConfig {
			c := nfsBaseConfig("gt-loser")
			c.nfsProvisionLockLost = true
			return c
		}(),
		"clone-per-agent": nfsAgentDirConfig("gt-cpa"),
		"empty-per-agent": func() RunConfig {
			c := nfsAgentDirConfig("gt-epa")
			c.NFSAgentDirEmpty = true
			return c
		}(),
		"non-git": func() RunConfig {
			c := nfsBaseConfig("gt-nongit")
			c.GitCloneForInit = nil
			return c
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			cfg.ResolvedSecrets = []api.ResolvedSecret{gitTokenSecret()}
			pod, err := newNFSTestK8sRuntime().buildPod("default", cfg)
			require.NoError(t, err)
			require.NotEmpty(t, pod.Spec.InitContainers)
			assertInitHasNoGitToken(t, pod)
			// The agent container keeps its own reference.
			agent := envEntry(pod.Spec.Containers[0].Env, provision.GitTokenEnv)
			require.NotNil(t, agent)
			require.NotNil(t, agent.ValueFrom)
		})
	}
}

// Without a git token the init container env is unchanged.
func TestBuildPod_NFSGitToken_NoneConfigured(t *testing.T) {
	cfg := nfsBaseConfig("gt-none")
	pod, err := newNFSTestK8sRuntime().buildPod("default", cfg)
	require.NoError(t, err)
	assertInitHasNoGitToken(t, pod)
	assert.Nil(t, envEntry(pod.Spec.Containers[0].Env, provision.GitTokenEnv))
}

// A git token that arrives as a plain env value (a GitHub App token minted
// at dispatch, or the NoAuth fallback) is moved into the per-agent Secret;
// the agent container and the cloning init container both read it from
// there, and the pod spec carries no value.
func TestRun_NFSGitToken_PlainEnvDivertedToSecret(t *testing.T) {
	rt, clientset := newTransportTestRuntime(false)
	cfg := nfsBaseConfig("gt-run")
	cfg.Labels = map[string]string{"scion.agent": "true", "scion.name": "gt-run"}
	cfg.Env = []string{"FOO=bar", "GITHUB_TOKEN=" + testGitToken}
	// A resolved secret for the same variable loses to the env value, as
	// it does today in the agent container.
	other := gitTokenSecret()
	other.Value = "other-test-token-value"
	cfg.ResolvedSecrets = []api.ResolvedSecret{other}

	pod, secret := runUntilPodCreated(t, rt, clientset, cfg)
	require.NotNil(t, secret)
	assert.Equal(t, testGitToken, string(secret.Data[gitCredentialSecretKey]))
	_, kept := secret.Data["GITHUB_TOKEN"]
	assert.False(t, kept, "the overridden resolved secret is dropped")
	assertInitGitTokenLikeAgent(t, pod, gitCredentialSecretKey)
	var count int
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == provision.GitTokenEnv {
			count++
		}
	}
	assert.Equal(t, 1, count)
	data, err := json.Marshal(pod)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "other-test-token-value")
}

// With no git token configured anywhere (for example NoAuth with no
// project or user GITHUB_TOKEN), nothing is diverted: no Secret is
// created and no container gets GITHUB_TOKEN.
func TestRun_NFSGitToken_NoneConfiguredUnchanged(t *testing.T) {
	rt, clientset := newTransportTestRuntime(false)
	cfg := nfsBaseConfig("gt-run-none")
	cfg.Labels = map[string]string{"scion.agent": "true", "scion.name": "gt-run-none"}
	cfg.Env = []string{"FOO=bar"}

	pod, secret := runUntilPodCreated(t, rt, clientset, cfg)
	assert.Nil(t, secret, "no Secret without any secret or credential")
	assert.Nil(t, envEntry(pod.Spec.Containers[0].Env, provision.GitTokenEnv))
	for _, ic := range pod.Spec.InitContainers {
		assert.Nil(t, envEntry(ic.Env, provision.GitTokenEnv))
	}
}

func TestDivertGitCredential(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		env := []string{"A=1"}
		secrets := []api.ResolvedSecret{gitTokenSecret()}
		gotEnv, gotSecrets := divertGitCredential(env, secrets)
		assert.Equal(t, env, gotEnv)
		assert.Equal(t, secrets, gotSecrets)
	})
	t.Run("empty value is not diverted", func(t *testing.T) {
		env := []string{"GITHUB_TOKEN=" + testGitToken, "GITHUB_TOKEN="}
		secrets := []api.ResolvedSecret{gitTokenSecret()}
		gotEnv, gotSecrets := divertGitCredential(env, secrets)
		assert.Equal(t, env, gotEnv)
		assert.Equal(t, secrets, gotSecrets)
	})
	t.Run("diverted, conflicts dropped, inputs untouched", func(t *testing.T) {
		env := []string{"GITHUB_TOKEN=first", "A=1", "GITHUB_TOKEN=" + testGitToken}
		secrets := []api.ResolvedSecret{
			gitTokenSecret(),
			{Name: gitCredentialSecretKey, Type: "file", Target: "/x", Value: "v"},
			{Name: "keep", Type: "environment", Target: "KEEP", Value: "k"},
		}
		envCopy := append([]string(nil), env...)
		secretsCopy := append([]api.ResolvedSecret(nil), secrets...)
		gotEnv, gotSecrets := divertGitCredential(env, secrets)
		assert.Equal(t, []string{"A=1"}, gotEnv)
		assert.Equal(t, []api.ResolvedSecret{
			{Name: "keep", Type: "environment", Target: "KEEP", Value: "k"},
			{Name: gitCredentialSecretKey, Type: "environment", Target: "GITHUB_TOKEN", Value: testGitToken, Source: "hub"},
		}, gotSecrets)
		assert.Equal(t, envCopy, env)
		assert.Equal(t, secretsCopy, secrets)
	})
}

// A plain GITHUB_TOKEN with no resolved secrets at all: the per-agent
// Secret is created for it alone, and the agent container and the cloning
// init container both read it via secretKeyRef.
func TestRun_NFSGitToken_PlainEnvOnlyCreatesSecret(t *testing.T) {
	rt, clientset := newTransportTestRuntime(false)
	cfg := nfsBaseConfig("gt-run-only")
	cfg.Labels = map[string]string{"scion.agent": "true", "scion.name": "gt-run-only"}
	cfg.Env = []string{"GITHUB_TOKEN=" + testGitToken}

	pod, secret := runUntilPodCreated(t, rt, clientset, cfg)
	require.NotNil(t, secret, "the per-agent Secret is created for the git token")
	assert.Equal(t, map[string][]byte{gitCredentialSecretKey: []byte(testGitToken)}, secret.Data)
	assertInitGitTokenLikeAgent(t, pod, gitCredentialSecretKey)
}

// GKE mode with a Secret Manager reference (the CSI path): the diverted
// token is stored in the per-agent Kubernetes Secret, both containers read
// it from there, and the SecretProviderClass does not reference it.
func TestRun_NFSGitToken_GKEPathDiverted(t *testing.T) {
	rt, clientset := newTransportTestRuntime(true)
	cfg := nfsBaseConfig("gt-run-gke")
	cfg.Labels = map[string]string{"scion.agent": "true", "scion.name": "gt-run-gke"}
	cfg.Env = []string{"GITHUB_TOKEN=" + testGitToken}
	cfg.ResolvedSecrets = []api.ResolvedSecret{
		{Name: "API_KEY", Type: "environment", Target: "API_KEY", Value: "test-api-value", Source: "user", Ref: "projects/p/secrets/api-key"},
	}

	var (
		mu     sync.Mutex
		spc    *unstructured.Unstructured
		spcErr error
	)
	clientset.PrependReactor("create", "pods", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		got, err := rt.Client.Dynamic().Resource(k8s.SecretProviderClassGVR).Namespace("default").Get(context.Background(), "scion-agent-gt-run-gke", metav1.GetOptions{})
		mu.Lock()
		spc, spcErr = got, err
		mu.Unlock()
		return false, nil, nil
	})

	pod, secret := runUntilPodCreated(t, rt, clientset, cfg)
	hasCSI := false
	for _, v := range pod.Spec.Volumes {
		if v.CSI != nil {
			hasCSI = true
		}
	}
	require.True(t, hasCSI, "expected the GKE CSI path")
	require.NotNil(t, secret)
	assert.Equal(t, testGitToken, string(secret.Data[gitCredentialSecretKey]))
	assertInitGitTokenLikeAgent(t, pod, gitCredentialSecretKey)

	mu.Lock()
	defer mu.Unlock()
	require.NoError(t, spcErr)
	params, _, _ := unstructured.NestedString(spc.Object, "spec", "parameters", "secrets")
	assert.False(t, strings.Contains(params, gitCredentialSecretKey) || strings.Contains(params, testGitToken),
		"the SecretProviderClass must not reference the git token")
}
