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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
)

func TestKubernetesResourceAbsent(t *testing.T) {
	rt, clientset, _ := newGKECleanupTestRuntime(t)
	ctx := context.Background()
	_, err := clientset.CoreV1().Secrets("ns").Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "ns", UID: "uid-s1"}}, metav1.CreateOptions{})
	require.NoError(t, err)
	now := metav1.Now()
	_, err = clientset.CoreV1().Pods("ns").Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "ns", UID: "uid-p1",
		DeletionTimestamp: &now, Finalizers: []string{"x"}}}, metav1.CreateOptions{})
	require.NoError(t, err)

	for name, tc := range map[string]struct {
		h       api.ResourceHandle
		absent  bool
		wantErr bool
	}{
		"present secret":           {api.ResourceHandle{Kind: api.ResourceKindSecret, Namespace: "ns", Name: "s1", UID: "uid-s1"}, false, false},
		"name reused by other UID": {api.ResourceHandle{Kind: api.ResourceKindSecret, Namespace: "ns", Name: "s1", UID: "uid-old"}, true, false},
		"secret not found":         {api.ResourceHandle{Kind: api.ResourceKindSecret, Namespace: "ns", Name: "gone", UID: "uid-gone"}, true, false},
		"terminating pod":          {api.ResourceHandle{Kind: api.ResourceKindPod, Namespace: "ns", Name: "p1", UID: "uid-p1"}, false, false},
		"pod not found":            {api.ResourceHandle{Kind: api.ResourceKindPod, Namespace: "ns", Name: "p2", UID: "uid-p2"}, true, false},
		"spc not found":            {api.ResourceHandle{Kind: api.ResourceKindSecretProviderClass, Namespace: "ns", Name: "spc", UID: "uid-spc"}, true, false},
		"empty namespace":          {api.ResourceHandle{Kind: api.ResourceKindSecret, Name: "s1", UID: "uid-s1"}, false, true},
		"empty name":               {api.ResourceHandle{Kind: api.ResourceKindSecret, Namespace: "ns", UID: "uid-s1"}, false, true},
		"unknown kind":             {api.ResourceHandle{Kind: "volume", Namespace: "ns", Name: "v", UID: "uid-v"}, false, true},
	} {
		t.Run(name, func(t *testing.T) {
			absent, err := rt.ResourceAbsent(ctx, tc.h)
			if tc.wantErr {
				require.Error(t, err)
				assert.False(t, absent)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.absent, absent)
		})
	}

	// A read failure (permission, timeout) is an error, never absence.
	clientset.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, errors.New("forbidden")
	})
	absent, err := rt.ResourceAbsent(ctx, api.ResourceHandle{Kind: api.ResourceKindSecret, Namespace: "ns", Name: "s1", UID: "uid-s1"})
	require.Error(t, err)
	assert.False(t, absent)
}

// TestKubernetesResourceAbsent_MissingCRDIsAnError: a SecretProviderClass
// check against a cluster without the CRD fails; it is never absence.
func TestKubernetesResourceAbsent_MissingCRDIsAnError(t *testing.T) {
	clientset := k8sfake.NewClientset()
	dyn := fake.NewSimpleDynamicClient(k8sruntime.NewScheme())
	dyn.PrependReactor("get", "secretproviderclasses", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, &k8sNotFoundNoDetails{}
	})
	rt := NewKubernetesRuntime(k8s.NewTestClient(dyn, clientset))
	absent, err := rt.ResourceAbsent(context.Background(), api.ResourceHandle{Kind: api.ResourceKindSecretProviderClass, Namespace: "ns", Name: "spc", UID: "u"})
	require.Error(t, err)
	assert.False(t, absent)
}

func TestKubernetesListOwnedResources(t *testing.T) {
	rt, clientset, _ := newGKECleanupTestRuntime(t)
	rt.DefaultNamespace = "default"
	ctx := context.Background()
	mine := withOwner(productionAgentLabels("worker", "p1"), "rb-a")
	mine[api.LabelAgentID] = "agent-1"
	seedLabelledSecret(t, rt, "scion-auth-mine", mine)
	seedLabelledSecret(t, rt, "scion-auth-theirs", withOwner(productionAgentLabels("worker", "p1"), "rb-b"))
	seedLabelledSecret(t, rt, "scion-auth-unlabelled", productionAgentLabels("worker", "p1"))
	spc := &unstructured.Unstructured{}
	spc.SetGroupVersionKind(schema.GroupVersionKind{Group: "secrets-store.csi.x-k8s.io", Version: "v1", Kind: "SecretProviderClass"})
	spc.SetName("scion-agent-mine")
	spc.SetNamespace("default")
	spc.SetLabels(mine)
	_, err := rt.Client.Dynamic().Resource(k8s.SecretProviderClassGVR).Namespace("default").Create(ctx, spc, metav1.CreateOptions{})
	require.NoError(t, err)

	got, err := rt.ListOwnedResources(ctx, "rb-a")
	require.NoError(t, err)
	names := map[string]string{}
	for _, o := range got {
		names[o.Handle.Name] = o.Handle.Kind
		assert.Equal(t, "agent-1", o.Labels[api.LabelAgentID])
	}
	assert.Equal(t, map[string]string{"scion-auth-mine": api.ResourceKindSecret, "scion-agent-mine": api.ResourceKindSecretProviderClass}, names,
		"only the instance's own children")

	for _, id := range []string{"", "not a label value!"} {
		clientset.ClearActions()
		_, err := rt.ListOwnedResources(ctx, id)
		require.Error(t, err, "id %q", id)
		assert.Empty(t, clientset.Actions(), "no list before the ID is validated")
	}
	clientset.PrependReactor("list", "secrets", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, errors.New("forbidden")
	})
	_, err = rt.ListOwnedResources(ctx, "rb-a")
	require.Error(t, err, "a list failure is an error, never an empty result")
}

// TestK8sRun_ChildrenCarryCanonicalLabels: the Secrets Run creates carry
// the agent ID, owner, project and run labels from the run (the canonical
// child mapping), and the legacy leftover selector still matches them.
func TestK8sRun_ChildrenCarryCanonicalLabels(t *testing.T) {
	rt, clientset, _ := newTestK8sRuntime()
	var fx uidFixture
	fx.install(t, clientset)
	var mu sync.Mutex
	var created []map[string]string
	clientset.PrependReactor("create", "secrets", func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
		mu.Lock()
		created = append(created, a.(k8stesting.CreateAction).GetObject().(*corev1.Secret).Labels)
		mu.Unlock()
		return false, nil, nil
	})
	clientset.PrependReactor("create", "pods", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, errors.New("admission denied")
	})
	config := hookTestConfig("child-agent")
	config.Labels[api.LabelRuntimeBrokerID] = "rb-a"
	config.Labels[projectkeys.LabelProjectID] = "p1"
	config.Labels[api.LabelAgentID] = "agent-1"
	config.Labels[api.LabelRunID] = "run-1"
	config.ResolvedSecrets = envSecrets(1)
	config.ResolvedAuth = authFiles(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _ = rt.Run(ctx, config)
	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, created)
	for _, l := range created {
		for k, want := range map[string]string{api.LabelAgentID: "agent-1", api.LabelRuntimeBrokerID: "rb-a",
			projectkeys.LabelProjectID: "p1", api.LabelRunID: "run-1", "scion.name": "child-agent"} {
			assert.Equal(t, want, l[k], "label %s on %v", k, l)
		}
	}
	legacy := map[string]string{"scion.name": "child-agent", projectkeys.LabelProjectID: "p1"}
	for _, l := range created {
		for k, v := range legacy {
			assert.Equal(t, v, l[k], "the legacy leftover selector matches the child")
		}
	}
}

func TestLaunchChildLabels(t *testing.T) {
	got := launchChildLabels("worker", map[string]string{"scion.agent": "true", "scion.name": "worker", api.LabelAgentID: "agent-1", "other": "x"})
	assert.Equal(t, map[string]string{"scion.agent": "true", "scion.name": "worker", api.LabelAgentID: "agent-1"}, got)
	assert.Equal(t, map[string]string{"scion.agent": "worker"}, launchChildLabels("worker", nil), "no agent ID is ever derived")
}

// TestContainerResourceAbsent: docker and podman read "no such
// container/object" (both CLIs' wordings) as absent, success as present,
// and any other failure as an error.
func TestContainerResourceAbsent(t *testing.T) {
	samples := map[string]bool{ // stderr -> absent
		"Error: No such container: 0123456789ab":                                                           true, // docker
		"Error response from daemon: No such container: 0123456789ab":                                      true, // docker (older)
		"Error: No such object: 0123456789ab":                                                              true, // docker without --type
		"Error: no such container 0123456789ab":                                                            true, // podman
		`Error: no such object: "0123456789ab"`:                                                            true, // podman
		"Error: inspecting object: no container with name or ID \"0123456789ab\" found: no such container": true, // podman
	}
	for stderr, absent := range samples {
		for _, newRT := range []func(string) ResourceAbsenceChecker{
			func(cli string) ResourceAbsenceChecker { return &DockerRuntime{Command: cli} },
			func(cli string) ResourceAbsenceChecker { return &PodmanRuntime{Command: cli} },
		} {
			cli := writeInspectCLI(t, fmt.Sprintf("echo %q >&2; exit 1", stderr))
			got, err := newRT(cli).ResourceAbsent(context.Background(), api.ResourceHandle{Kind: api.ResourceKindContainer, UID: "0123456789ab"})
			require.NoError(t, err, stderr)
			assert.Equal(t, absent, got, stderr)
		}
	}
	present := writeInspectCLI(t, "echo 0123456789abcdef")
	got, err := (&DockerRuntime{Command: present}).ResourceAbsent(context.Background(), api.ResourceHandle{Kind: api.ResourceKindContainer, UID: "0123456789ab"})
	require.NoError(t, err)
	assert.False(t, got)
	broken := writeInspectCLI(t, "echo 'Cannot connect to the Docker daemon' >&2; exit 1")
	got, err = (&DockerRuntime{Command: broken}).ResourceAbsent(context.Background(), api.ResourceHandle{Kind: api.ResourceKindContainer, UID: "0123456789ab"})
	require.Error(t, err, "a daemon failure is never absence")
	assert.False(t, got)
}

func writeInspectCLI(t *testing.T, inspectBody string) string {
	t.Helper()
	cli := filepath.Join(t.TempDir(), "cli")
	require.NoError(t, os.WriteFile(cli, []byte("#!/bin/sh\ncase \"$1\" in\n  inspect) "+inspectBody+" ;;\nesac\n"), 0o755))
	return cli
}

// k8sNotFoundNoDetails is a 404 without object details, as returned for a
// resource type the server does not serve (a missing CRD).
type k8sNotFoundNoDetails struct{}

func (k8sNotFoundNoDetails) Error() string { return "the server could not find the requested resource" }
func (k8sNotFoundNoDetails) Status() metav1.Status {
	return metav1.Status{Status: metav1.StatusFailure, Code: 404, Reason: metav1.StatusReasonNotFound, Message: "the server could not find the requested resource"}
}
